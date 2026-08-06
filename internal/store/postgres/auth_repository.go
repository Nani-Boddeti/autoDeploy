package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"autodeploy/internal/auth"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AuthRepository persists the administrator account and digest-only sessions.
type AuthRepository struct{ pool *pgxpool.Pool }

func NewAuthRepository(pool *pgxpool.Pool) *AuthRepository { return &AuthRepository{pool: pool} }

type BootstrapInput struct {
	OwnerID, UserID, Username, AuditID string
	Verifier                           auth.PasswordVerifier
	Policy                             auth.Argon2Policy
}

type LoginUser struct {
	ID, OwnerID  string
	Verifier     auth.PasswordVerifier
	AuthRevision uint64
	Active       bool
}

type Session struct {
	ID, UserID, OwnerID                                     string
	AuthRevision                                            uint64
	CreatedAt, LastSeenAt, IdleExpiresAt, AbsoluteExpiresAt time.Time
}

// CompleteLoginInput carries the already verified, known-user login state into
// the single persistence transaction that mints authority. Callers must first
// reserve throttles, verify the password, and supply all pair/username aliases
// that may be cleared on success.
type CompleteLoginInput struct {
	SessionID, AuditID, SessionLimitAuditID string
	Token                                   auth.SessionToken
	User                                    LoginUser
	Replacement                             *auth.PasswordVerifier
	ThrottleIdentities                      []ThrottleIdentity
}

type ThrottleKind string

const (
	ThrottlePair           ThrottleKind = "pair"
	ThrottleIP             ThrottleKind = "ip"
	ThrottleUsername       ThrottleKind = "username"
	ThrottleInvalidForward ThrottleKind = "invalid_forward"
)

// ThrottleIdentity contains only a keyed, domain-separated digest; callers must
// never provide a submitted username or address to persistence.
type ThrottleIdentity struct {
	Kind       ThrottleKind
	KeyVersion string
	Digest     [32]byte
	Aliases    []ThrottleAlias
	// Every future known-user pair reservation must set RecoveryUserID; an
	// unknown-user pair reservation may remain untagged.
	RecoveryUserID string
}
type ThrottleAlias struct {
	KeyVersion string
	Digest     [32]byte
}
type ThrottleReservation struct {
	BlockedUntil time.Time
	Allowed      bool
}

const maxThrottleAliases = 4

type usernameThrottleRow struct {
	keyVersion string
	digest     [32]byte
	failures   int
	blocked    *time.Time
	updated    time.Time
}

// reserveUsernameThrottle deliberately has no fixed window: backoff state
// must survive a clock-window transition and is bounded in its own table.
func reserveUsernameThrottle(ctx context.Context, tx pgx.Tx, now time.Time, identity ThrottleIdentity, maxRows int) (ThrottleReservation, error) {
	candidates := append([]ThrottleAlias{{KeyVersion: identity.KeyVersion, Digest: identity.Digest}}, identity.Aliases...)
	rows := make([]usernameThrottleRow, 0, len(candidates))
	primaryFound := false
	for index, candidate := range candidates {
		var row usernameThrottleRow
		row.keyVersion, row.digest = candidate.KeyVersion, candidate.Digest
		err := tx.QueryRow(ctx, `SELECT failures,blocked_until,updated_at FROM auth_username_throttles WHERE key_version=$1 AND identifier_digest=$2 FOR UPDATE`, candidate.KeyVersion, candidate.Digest[:]).Scan(&row.failures, &row.blocked, &row.updated)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return ThrottleReservation{}, fmt.Errorf("lock username throttle: %w", err)
		}
		if index == 0 {
			primaryFound = true
		}
		// Inactive state has no continuing authority. A live block is never
		// reset, even when it crosses the 15 minute inactivity boundary.
		if !row.updated.After(now.Add(-15*time.Minute)) && (row.blocked == nil || !row.blocked.After(now)) {
			row.failures, row.blocked = 0, nil
		}
		rows = append(rows, row)
	}
	keyVersion, digest := identity.KeyVersion, identity.Digest
	if !primaryFound && len(rows) == 0 {
		var count int
		if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM auth_throttle_buckets) + (SELECT count(*) FROM auth_username_throttles)`).Scan(&count); err != nil {
			return ThrottleReservation{}, fmt.Errorf("count throttle rows: %w", err)
		}
		if count >= maxRows-4 {
			keyVersion, digest = "overflow", [32]byte{}
			rows = rows[:0]
			if count >= maxRows {
				var overflowExists bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM auth_username_throttles WHERE key_version=$1 AND identifier_digest=$2)`, keyVersion, digest[:]).Scan(&overflowExists); err != nil {
					return ThrottleReservation{}, fmt.Errorf("check username overflow: %w", err)
				}
				if !overflowExists {
					return ThrottleReservation{Allowed: false}, nil
				}
			}
			var row usernameThrottleRow
			row.keyVersion, row.digest = keyVersion, digest
			err := tx.QueryRow(ctx, `SELECT failures,blocked_until,updated_at FROM auth_username_throttles WHERE key_version=$1 AND identifier_digest=$2 FOR UPDATE`, keyVersion, digest[:]).Scan(&row.failures, &row.blocked, &row.updated)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return ThrottleReservation{}, fmt.Errorf("lock username overflow: %w", err)
			}
			if err == nil {
				if !row.updated.After(now.Add(-15*time.Minute)) && (row.blocked == nil || !row.blocked.After(now)) {
					row.failures, row.blocked = 0, nil
				}
				rows = append(rows, row)
			}
		}
	}
	failures := 0
	var liveBlock *time.Time
	for _, row := range rows {
		failures += row.failures
		if failures > 15 {
			failures = 15
		}
		if row.blocked != nil && row.blocked.After(now) && (liveBlock == nil || row.blocked.After(*liveBlock)) {
			v := *row.blocked
			liveBlock = &v
		}
	}
	// Retained aliases are merged before a denial too, so rotation never
	// leaves split authority. Overflow is shared and is never deleted here.
	if keyVersion != "overflow" {
		for _, row := range rows {
			if row.keyVersion == identity.KeyVersion && row.digest == identity.Digest {
				continue
			}
			if _, err := tx.Exec(ctx, `DELETE FROM auth_username_throttles WHERE key_version=$1 AND identifier_digest=$2`, row.keyVersion, row.digest[:]); err != nil {
				return ThrottleReservation{}, fmt.Errorf("delete username alias: %w", err)
			}
		}
	}
	if liveBlock != nil {
		if keyVersion != "overflow" && (!primaryFound || len(rows) > 1) {
			if _, err := tx.Exec(ctx, `INSERT INTO auth_username_throttles(key_version,identifier_digest,failures,blocked_until,updated_at) VALUES($1,$2,$3,$4,clock_timestamp()) ON CONFLICT(key_version,identifier_digest) DO UPDATE SET failures=EXCLUDED.failures,blocked_until=EXCLUDED.blocked_until,updated_at=EXCLUDED.updated_at`, keyVersion, digest[:], failures, liveBlock); err != nil {
				return ThrottleReservation{}, fmt.Errorf("persist username merge: %w", err)
			}
		}
		return ThrottleReservation{Allowed: false, BlockedUntil: *liveBlock}, nil
	}
	failures++
	if failures > 15 {
		failures = 15
	}
	var block *time.Time
	if failures >= 10 {
		delay := 30 * time.Second * time.Duration(1<<min(failures-10, 5))
		if delay > 15*time.Minute {
			delay = 15 * time.Minute
		}
		value := now.Add(delay)
		block = &value
	}
	if _, err := tx.Exec(ctx, `INSERT INTO auth_username_throttles(key_version,identifier_digest,failures,blocked_until,updated_at) VALUES($1,$2,$3,$4,clock_timestamp()) ON CONFLICT(key_version,identifier_digest) DO UPDATE SET failures=EXCLUDED.failures,blocked_until=EXCLUDED.blocked_until,updated_at=EXCLUDED.updated_at`, keyVersion, digest[:], failures, block); err != nil {
		return ThrottleReservation{}, fmt.Errorf("reserve username throttle: %w", err)
	}
	if block != nil {
		return ThrottleReservation{Allowed: true, BlockedUntil: *block}, nil
	}
	return ThrottleReservation{Allowed: true}, nil
}

// ReserveThrottle conservatively records an attempt before password work. A
// caller crash therefore counts as a failure. It aggregates retained aliases.
func (r *AuthRepository) ReserveThrottle(ctx context.Context, identities []ThrottleIdentity, maxRows int) (ThrottleReservation, error) {
	if err := validateThrottleIdentities(identities, maxRows); err != nil {
		return ThrottleReservation{}, err
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return ThrottleReservation{}, fmt.Errorf("begin throttle reservation: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// The universal credential-flow order begins with throttle admission. This
	// lock serializes cap decisions before a reservation can acquire a user FK
	// lock for a first-time known-user pair.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(731492845631694123)`); err != nil {
		return ThrottleReservation{}, fmt.Errorf("lock throttle admission: %w", err)
	}
	var configuredMaxRows *int
	if err = tx.QueryRow(ctx, `SELECT max_rows FROM auth_throttle_state WHERE id=1 FOR UPDATE`).Scan(&configuredMaxRows); err != nil {
		return ThrottleReservation{}, fmt.Errorf("lock throttle state: %w", err)
	}
	if configuredMaxRows == nil {
		if _, err = tx.Exec(ctx, `UPDATE auth_throttle_state SET max_rows=$1 WHERE id=1 AND max_rows IS NULL`, maxRows); err != nil {
			return ThrottleReservation{}, fmt.Errorf("configure throttle cap: %w", err)
		}
		configuredMaxRows = &maxRows
	}
	if *configuredMaxRows != maxRows {
		return ThrottleReservation{}, errors.New("throttle max rows differs from durable configuration")
	}
	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return ThrottleReservation{}, fmt.Errorf("read throttle clock: %w", err)
	}
	window := now.UTC().Truncate(15 * time.Minute)
	var blocked time.Time
	for _, identity := range identities {
		if identity.Kind == ThrottleUsername {
			reservation, err := reserveUsernameThrottle(ctx, tx, now, identity, maxRows)
			if err != nil {
				return ThrottleReservation{}, err
			}
			if !reservation.Allowed {
				return reservation, tx.Commit(ctx)
			}
			if reservation.BlockedUntil.After(blocked) {
				blocked = reservation.BlockedUntil
			}
			continue
		}
		var attempts, failures int
		var activeAttempts, activeFailures int
		primaryExists := false
		aliasesToMigrate := make([]ThrottleAlias, 0, len(identity.Aliases))
		var activeBlocked *time.Time
		// Preserve an existing tag even when this reservation is made before the
		// username is known. A different known user for the same opaque pair
		// digest is an invariant violation, never a reassignment.
		resolvedRecoveryUserID := pairRecoveryUserID(identity, string(identity.Kind))
		for index, candidate := range append([]ThrottleAlias{{KeyVersion: identity.KeyVersion, Digest: identity.Digest}}, identity.Aliases...) {
			var a, f int
			var b *time.Time
			var recoveryUserID *string
			err = tx.QueryRow(ctx, `SELECT attempts,failures,blocked_until,recovery_user_id FROM auth_throttle_buckets WHERE kind=$1 AND key_version=$2 AND identifier_digest=$3 AND window_started_at=$4 FOR UPDATE`, identity.Kind, candidate.KeyVersion, candidate.Digest[:], window).Scan(&a, &f, &b, &recoveryUserID)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return ThrottleReservation{}, fmt.Errorf("lock throttle bucket: %w", err)
			}
			if err == nil {
				if identity.Kind == ThrottlePair && recoveryUserID != nil {
					if resolvedRecoveryUserID != nil && *recoveryUserID != *resolvedRecoveryUserID {
						return ThrottleReservation{}, errors.New("pair throttle recovery user collision")
					}
					v := *recoveryUserID
					resolvedRecoveryUserID = &v
				}
				if index == 0 {
					primaryExists = true
				}
				attempts += a
				failures += f
				activeAttempts += a
				activeFailures += f
				if index > 0 {
					aliasesToMigrate = append(aliasesToMigrate, candidate)
				}
				if b != nil && b.After(blocked) {
					blocked = *b
				}
				if b != nil && (activeBlocked == nil || b.After(*activeBlocked)) {
					v := *b
					activeBlocked = &v
				}
			}
			// Username backoff survives a fixed-window boundary.  Retaining the
			// latest blocked row also supplies the exponent after a block expires;
			// bounded cleanup never removes an unexpired block.
			if identity.Kind == ThrottleUsername {
				var historicalFailures int
				var historicalBlock *time.Time
				historyErr := tx.QueryRow(ctx, `SELECT failures,blocked_until FROM auth_throttle_buckets WHERE kind=$1 AND key_version=$2 AND identifier_digest=$3 AND blocked_until IS NOT NULL ORDER BY blocked_until DESC LIMIT 1 FOR UPDATE`, identity.Kind, candidate.KeyVersion, candidate.Digest[:]).Scan(&historicalFailures, &historicalBlock)
				if historyErr != nil && !errors.Is(historyErr, pgx.ErrNoRows) {
					return ThrottleReservation{}, fmt.Errorf("lock username backoff: %w", historyErr)
				}
				if historyErr == nil {
					if historicalFailures > failures {
						failures = historicalFailures
					}
					if historicalBlock != nil && historicalBlock.After(blocked) {
						blocked = *historicalBlock
					}
				}
			}
		}
		// Key rotation must not create a fresh allowance.  Move every live
		// retained alias into the active key while holding the installation
		// admission lock, then remove the source rows in this transaction.
		// The merged row occupies at most one slot, so cardinality cannot grow.
		if len(aliasesToMigrate) > 0 {
			for _, alias := range aliasesToMigrate {
				if _, err = tx.Exec(ctx, `DELETE FROM auth_throttle_buckets WHERE kind=$1 AND key_version=$2 AND identifier_digest=$3 AND window_started_at=$4`, identity.Kind, alias.KeyVersion, alias.Digest[:], window); err != nil {
					return ThrottleReservation{}, fmt.Errorf("delete migrated throttle alias: %w", err)
				}
			}
			if _, err = tx.Exec(ctx, `INSERT INTO auth_throttle_buckets(kind,key_version,identifier_digest,window_started_at,attempts,failures,blocked_until,recovery_user_id,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,clock_timestamp()) ON CONFLICT(kind,key_version,identifier_digest,window_started_at) DO UPDATE SET attempts=EXCLUDED.attempts,failures=EXCLUDED.failures,blocked_until=CASE WHEN auth_throttle_buckets.blocked_until IS NULL THEN EXCLUDED.blocked_until WHEN EXCLUDED.blocked_until IS NULL THEN auth_throttle_buckets.blocked_until ELSE GREATEST(auth_throttle_buckets.blocked_until,EXCLUDED.blocked_until) END,recovery_user_id=COALESCE(auth_throttle_buckets.recovery_user_id,EXCLUDED.recovery_user_id),updated_at=clock_timestamp()`, identity.Kind, identity.KeyVersion, identity.Digest[:], window, activeAttempts, activeFailures, activeBlocked, resolvedRecoveryUserID); err != nil {
				return ThrottleReservation{}, fmt.Errorf("migrate throttle aliases: %w", err)
			}
			primaryExists = true
			attempts, failures = activeAttempts, activeFailures
			if identity.Kind == ThrottleUsername && blocked.After(now) {
				// Keep an unexpired historical username block authoritative even
				// though only active-window counters are migrated.
				return ThrottleReservation{Allowed: false, BlockedUntil: blocked}, tx.Commit(ctx)
			}
		}
		if blocked.After(now) {
			return ThrottleReservation{Allowed: false, BlockedUntil: blocked}, tx.Commit(ctx)
		}
		var count int
		if err = tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM auth_throttle_buckets) + (SELECT count(*) FROM auth_username_throttles)`).Scan(&count); err != nil {
			return ThrottleReservation{}, fmt.Errorf("count throttle buckets: %w", err)
		}
		kind := string(identity.Kind)
		digest := identity.Digest
		keyVersion := identity.KeyVersion
		recoveryUserID := resolvedRecoveryUserID
		// Keep four installation-wide slots available for per-kind overflow.
		// Delayed cleanup can therefore only fold an unseen identity into an
		// already bounded shared bucket; it cannot increase cardinality.
		if !primaryExists && count >= maxRows-4 {
			kind += "_overflow"
			keyVersion = "overflow"
			digest = [32]byte{}
			recoveryUserID = nil
			if count >= maxRows {
				var overflowExists bool
				if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM auth_throttle_buckets WHERE kind=$1 AND key_version=$2 AND identifier_digest=$3 AND window_started_at=$4)`, kind, keyVersion, digest[:], window).Scan(&overflowExists); err != nil {
					return ThrottleReservation{}, fmt.Errorf("check throttle overflow bucket: %w", err)
				}
				if !overflowExists {
					return ThrottleReservation{Allowed: false}, tx.Commit(ctx)
				}
			}
		}
		// Once capacity routes an identity to overflow, its shared counter is
		// itself authoritative. Read it under the admission lock before
		// deciding, rather than treating overflow as write-only storage.
		if kind != string(identity.Kind) {
			var overflowAttempts, overflowFailures int
			var overflowBlocked *time.Time
			err = tx.QueryRow(ctx, `SELECT attempts,failures,blocked_until FROM auth_throttle_buckets WHERE kind=$1 AND key_version=$2 AND identifier_digest=$3 AND window_started_at=$4 FOR UPDATE`, kind, keyVersion, digest[:], window).Scan(&overflowAttempts, &overflowFailures, &overflowBlocked)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return ThrottleReservation{}, fmt.Errorf("lock throttle overflow: %w", err)
			}
			if err == nil {
				attempts, failures = overflowAttempts, overflowFailures
				if overflowBlocked != nil && overflowBlocked.After(blocked) {
					blocked = *overflowBlocked
				}
			}
			if blocked.After(now) {
				return ThrottleReservation{Allowed: false, BlockedUntil: blocked}, tx.Commit(ctx)
			}
		}
		limit := 20
		if identity.Kind == ThrottlePair {
			limit = 5
		}
		if identity.Kind != ThrottleUsername && attempts >= limit {
			return ThrottleReservation{Allowed: false, BlockedUntil: blocked}, tx.Commit(ctx)
		}
		var nextBlock *time.Time
		if identity.Kind == ThrottleUsername && failures >= 9 {
			delay := 30 * time.Second * time.Duration(1<<min(failures-9, 9))
			if delay > 15*time.Minute {
				delay = 15 * time.Minute
			}
			v := now.Add(delay)
			nextBlock, blocked = &v, v
		}
		if _, err = tx.Exec(ctx, `INSERT INTO auth_throttle_buckets(kind,key_version,identifier_digest,window_started_at,attempts,failures,blocked_until,recovery_user_id,updated_at) VALUES($1,$2,$3,$4,1,1,$5,$6,clock_timestamp()) ON CONFLICT(kind,key_version,identifier_digest,window_started_at) DO UPDATE SET attempts=auth_throttle_buckets.attempts+1,failures=auth_throttle_buckets.failures+1,blocked_until=COALESCE($5,auth_throttle_buckets.blocked_until),recovery_user_id=COALESCE(auth_throttle_buckets.recovery_user_id,EXCLUDED.recovery_user_id),updated_at=clock_timestamp()`, kind, keyVersion, digest[:], window, nextBlock, recoveryUserID); err != nil {
			return ThrottleReservation{}, fmt.Errorf("reserve throttle bucket: %w", err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return ThrottleReservation{}, fmt.Errorf("commit throttle reservation: %w", err)
	}
	return ThrottleReservation{Allowed: true, BlockedUntil: blocked}, nil
}

// FinalizeThrottle clears only supplied credential-failure buckets after a successful login.
func (r *AuthRepository) FinalizeThrottle(ctx context.Context, successful bool, identities []ThrottleIdentity) error {
	if err := validateThrottleIdentities(identities, 8); err != nil {
		return err
	}
	if !successful {
		return nil
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin throttle finalization: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(731492845631694123)`); err != nil {
		return fmt.Errorf("lock throttle finalization: %w", err)
	}
	for _, identity := range identities {
		if identity.Kind != ThrottlePair && identity.Kind != ThrottleUsername {
			continue
		}
		for _, candidate := range append([]ThrottleAlias{{KeyVersion: identity.KeyVersion, Digest: identity.Digest}}, identity.Aliases...) {
			if identity.Kind == ThrottleUsername {
				if _, err = tx.Exec(ctx, `DELETE FROM auth_username_throttles WHERE key_version=$1 AND identifier_digest=$2 AND key_version <> 'overflow'`, candidate.KeyVersion, candidate.Digest[:]); err != nil {
					return fmt.Errorf("clear username throttle: %w", err)
				}
				continue
			}
			if _, err = tx.Exec(ctx, `DELETE FROM auth_throttle_buckets WHERE kind=$1 AND key_version=$2 AND identifier_digest=$3`, identity.Kind, candidate.KeyVersion, candidate.Digest[:]); err != nil {
				return fmt.Errorf("clear throttle bucket: %w", err)
			}
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit throttle finalization: %w", err)
	}
	return nil
}

func (r *AuthRepository) CleanupThrottle(ctx context.Context, limit int) (int64, error) {
	if limit < 1 || limit > 1000 {
		return 0, errors.New("invalid throttle cleanup limit")
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin throttle cleanup: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(731492845631694123)`); err != nil {
		return 0, fmt.Errorf("lock throttle cleanup: %w", err)
	}
	var deleted int64
	tag, err := tx.Exec(ctx, `DELETE FROM auth_throttle_buckets WHERE ctid IN (SELECT ctid FROM auth_throttle_buckets WHERE window_started_at < clock_timestamp()-interval '15 minutes' AND (blocked_until IS NULL OR blocked_until <= clock_timestamp()) ORDER BY window_started_at LIMIT $1)`, limit)
	if err != nil {
		return 0, fmt.Errorf("cleanup throttle buckets: %w", err)
	}
	deleted = tag.RowsAffected()
	if deleted < int64(limit) {
		tag, err = tx.Exec(ctx, `DELETE FROM auth_username_throttles WHERE ctid IN (SELECT ctid FROM auth_username_throttles WHERE updated_at < clock_timestamp()-interval '15 minutes' AND (blocked_until IS NULL OR blocked_until <= clock_timestamp()) ORDER BY updated_at LIMIT $1)`, int64(limit)-deleted)
		if err != nil {
			return 0, fmt.Errorf("cleanup username throttles: %w", err)
		}
		deleted += tag.RowsAffected()
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit throttle cleanup: %w", err)
	}
	return deleted, nil
}

func (r *AuthRepository) Bootstrap(ctx context.Context, in BootstrapInput) error {
	if err := validateBootstrap(in); err != nil {
		return err
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin auth bootstrap: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var owner *string
	if err = tx.QueryRow(ctx, `SELECT owner_id FROM installation_auth WHERE id=1 FOR UPDATE`).Scan(&owner); err != nil {
		return fmt.Errorf("lock installation authentication: %w", err)
	}
	if owner != nil {
		return ErrAlreadyBootstrapped
	}
	if _, err = tx.Exec(ctx, `INSERT INTO owners(id) VALUES($1)`, in.OwnerID); err != nil {
		return fmt.Errorf("insert owner: %w", err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO users(id,owner_id,username,role,password_phc,password_policy_revision) VALUES($1,$2,$3,'owner',$4,$5)`, in.UserID, in.OwnerID, in.Username, in.Verifier.PHC, int64(in.Verifier.PolicyRevision)); err != nil {
		return fmt.Errorf("insert administrator: %w", err)
	}
	if _, err = tx.Exec(ctx, `UPDATE installation_auth SET owner_id=$1,password_policy_revision=$2,password_memory_kib=$3,password_iterations=$4,password_lanes=$5,bootstrapped_at=clock_timestamp() WHERE id=1 AND owner_id IS NULL`, in.OwnerID, int64(in.Policy.Revision), int32(in.Policy.MemoryKiB), int32(in.Policy.Iterations), int16(in.Policy.Lanes)); err != nil {
		return fmt.Errorf("complete bootstrap: %w", err)
	}
	if err = insertAudit(ctx, tx, in.AuditID, in.UserID, in.OwnerID, "bootstrap", "success"); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit auth bootstrap: %w", err)
	}
	return nil
}

func (r *AuthRepository) LoadPasswordPolicy(ctx context.Context) (auth.Argon2Policy, error) {
	var p auth.Argon2Policy
	var rev int64
	var mem, iter int32
	var lanes int16
	err := r.pool.QueryRow(ctx, `SELECT password_policy_revision,password_memory_kib,password_iterations,password_lanes FROM installation_auth WHERE id=1 AND owner_id IS NOT NULL`).Scan(&rev, &mem, &iter, &lanes)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ErrNotFound
	}
	if err != nil {
		return p, fmt.Errorf("load password policy: %w", err)
	}
	p = auth.Argon2Policy{Revision: uint64(rev), MemoryKiB: uint32(mem), Iterations: uint32(iter), Lanes: uint8(lanes)}
	if err := p.Validate(); err != nil {
		return auth.Argon2Policy{}, fmt.Errorf("invalid stored password policy: %w", err)
	}
	return p, nil
}

func (r *AuthRepository) FindLoginUserByCanonicalUsername(ctx context.Context, username string) (LoginUser, error) {
	if canonical, err := auth.CanonicalUsername(username); err != nil || canonical != username {
		return LoginUser{}, errors.New("invalid canonical username")
	}
	var v LoginUser
	var rev, policyRev int64
	var status string
	err := r.pool.QueryRow(ctx, `SELECT id,owner_id,password_phc,password_policy_revision,auth_revision,status FROM users WHERE username=$1`, username).Scan(&v.ID, &v.OwnerID, &v.Verifier.PHC, &policyRev, &rev, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return LoginUser{}, ErrNotFound
	}
	if err != nil {
		return LoginUser{}, fmt.Errorf("load login user: %w", err)
	}
	v.Verifier.PolicyRevision = uint64(policyRev)
	v.AuthRevision = uint64(rev)
	v.Active = status == "active"
	if err := auth.ValidatePasswordVerifier(v.Verifier); err != nil {
		return LoginUser{}, fmt.Errorf("invalid stored password verifier: %w", err)
	}
	return v, nil
}

// CompleteLogin creates a session and clears only successful credential
// throttles in one transaction. It takes throttle admission, user, then
// session locks so a reset cannot mint or preserve stale authority.
func (r *AuthRepository) CompleteLogin(ctx context.Context, in CompleteLoginInput) (Session, error) {
	if err := validateCompleteLogin(in); err != nil {
		return Session{}, err
	}
	digest, err := auth.SessionTokenDigest(in.Token)
	if err != nil {
		return Session{}, ErrUnauthenticated
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Session{}, fmt.Errorf("begin complete login: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(731492845631694123)`); err != nil {
		return Session{}, fmt.Errorf("lock complete login throttle admission: %w", err)
	}
	var owner, status, phc string
	var current, storedPolicyRev, durablePolicyRev int64
	var durableMemory, durableIterations int32
	var durableLanes int16
	err = tx.QueryRow(ctx, `SELECT u.owner_id,u.status,u.auth_revision,u.password_phc,u.password_policy_revision,i.password_policy_revision,i.password_memory_kib,i.password_iterations,i.password_lanes FROM users u JOIN installation_auth i ON i.id=1 WHERE u.id=$1 FOR UPDATE OF u`, in.User.ID).Scan(&owner, &status, &current, &phc, &storedPolicyRev, &durablePolicyRev, &durableMemory, &durableIterations, &durableLanes)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrUnauthenticated
	}
	if err != nil {
		return Session{}, fmt.Errorf("lock login user: %w", err)
	}
	if owner != in.User.OwnerID || status != "active" || current != int64(in.User.AuthRevision) || phc != in.User.Verifier.PHC || storedPolicyRev != int64(in.User.Verifier.PolicyRevision) {
		return Session{}, ErrUnauthenticated
	}
	durablePolicy := auth.Argon2Policy{Revision: uint64(durablePolicyRev), MemoryKiB: uint32(durableMemory), Iterations: uint32(durableIterations), Lanes: uint8(durableLanes)}
	if err := durablePolicy.Validate(); err != nil {
		return Session{}, fmt.Errorf("invalid stored password policy: %w", err)
	}
	if storedPolicyRev > durablePolicyRev {
		return Session{}, ErrUnauthenticated
	}
	if in.Replacement != nil {
		if auth.ValidatePasswordVerifierForPolicy(in.User.Verifier, durablePolicy) == nil {
			return Session{}, errors.New("password rehash is not required")
		}
		if err := auth.ValidatePasswordVerifierForPolicy(*in.Replacement, durablePolicy); err != nil {
			return Session{}, fmt.Errorf("password verifier policy is not current: %w", err)
		}
		if _, err = tx.Exec(ctx, `UPDATE users SET password_phc=$2,password_policy_revision=$3,updated_at=clock_timestamp() WHERE id=$1`, in.User.ID, in.Replacement.PHC, int64(in.Replacement.PolicyRevision)); err != nil {
			return Session{}, fmt.Errorf("rehash password: %w", err)
		}
	}
	var s Session
	var revision int64
	err = tx.QueryRow(ctx, `WITH now_value AS (SELECT clock_timestamp() AS v) INSERT INTO admin_sessions(id,token_digest,user_id,owner_id,auth_revision,created_at,last_seen_at,idle_expires_at,absolute_expires_at) SELECT $1,$2,$3,$4,$5,v,v,v+interval '30 minutes',v+interval '8 hours' FROM now_value RETURNING created_at,last_seen_at,idle_expires_at,absolute_expires_at,auth_revision`, in.SessionID, digest[:], in.User.ID, owner, current).Scan(&s.CreatedAt, &s.LastSeenAt, &s.IdleExpiresAt, &s.AbsoluteExpiresAt, &revision)
	if err != nil {
		return Session{}, fmt.Errorf("insert login session: %w", err)
	}
	s.ID, s.UserID, s.OwnerID, s.AuthRevision = in.SessionID, in.User.ID, owner, uint64(revision)
	rows, err := tx.Query(ctx, `WITH excessive AS (SELECT id FROM admin_sessions WHERE user_id=$1 AND revoked_at IS NULL ORDER BY created_at DESC,id DESC OFFSET 10 FOR UPDATE) UPDATE admin_sessions SET revoked_at=clock_timestamp(),revoked_reason='session_limit' WHERE id IN (SELECT id FROM excessive) RETURNING id`, in.User.ID)
	if err != nil {
		return Session{}, fmt.Errorf("limit login sessions: %w", err)
	}
	var revokedIDs []string
	for rows.Next() {
		var revokedID string
		if err = rows.Scan(&revokedID); err != nil {
			rows.Close()
			return Session{}, fmt.Errorf("read limited login session: %w", err)
		}
		revokedIDs = append(revokedIDs, revokedID)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return Session{}, fmt.Errorf("read limited login sessions: %w", err)
	}
	rows.Close()
	if len(revokedIDs) > 1 {
		return Session{}, errors.New("session limit revoked more than one session")
	}
	if len(revokedIDs) == 1 {
		if err = insertAudit(ctx, tx, in.SessionLimitAuditID, in.User.ID, owner, "session_revoked", "success"); err != nil {
			return Session{}, err
		}
	}
	if err = insertAudit(ctx, tx, in.AuditID, in.User.ID, owner, "login_success", "success"); err != nil {
		return Session{}, err
	}
	for _, identity := range in.ThrottleIdentities {
		if identity.Kind != ThrottlePair && identity.Kind != ThrottleUsername {
			continue
		}
		for _, candidate := range append([]ThrottleAlias{{KeyVersion: identity.KeyVersion, Digest: identity.Digest}}, identity.Aliases...) {
			if identity.Kind == ThrottlePair {
				if _, err = tx.Exec(ctx, `DELETE FROM auth_throttle_buckets WHERE kind='pair' AND key_version=$1 AND identifier_digest=$2 AND (recovery_user_id=$3 OR recovery_user_id IS NULL)`, candidate.KeyVersion, candidate.Digest[:], in.User.ID); err != nil {
					return Session{}, fmt.Errorf("clear successful pair throttle: %w", err)
				}
				continue
			}
			if _, err = tx.Exec(ctx, `DELETE FROM auth_username_throttles WHERE key_version=$1 AND identifier_digest=$2 AND key_version <> 'overflow'`, candidate.KeyVersion, candidate.Digest[:]); err != nil {
				return Session{}, fmt.Errorf("clear successful username throttle: %w", err)
			}
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return Session{}, fmt.Errorf("commit complete login: %w", err)
	}
	return s, nil
}

func validateCompleteLogin(in CompleteLoginInput) error {
	if err := validID(in.SessionID); err != nil {
		return err
	}
	if err := validID(in.AuditID); err != nil {
		return err
	}
	if err := validID(in.SessionLimitAuditID); err != nil || in.SessionLimitAuditID == in.AuditID {
		return errors.New("invalid distinct session-limit audit identifier")
	}
	if err := userInputValid(in.User); err != nil {
		return err
	}
	if err := validateThrottleIdentities(in.ThrottleIdentities, 8); err != nil {
		return err
	}
	if len(in.ThrottleIdentities) != 3 {
		return errors.New("invalid successful login throttle identities")
	}
	var pairs, usernames, ips int
	for _, identity := range in.ThrottleIdentities {
		switch identity.Kind {
		case ThrottlePair:
			pairs++
			if identity.RecoveryUserID != in.User.ID {
				return errors.New("pair throttle recovery user does not match login user")
			}
		case ThrottleUsername:
			usernames++
		case ThrottleIP:
			ips++
		default:
			return errors.New("invalid successful login throttle identity")
		}
	}
	if pairs != 1 || usernames != 1 || ips != 1 {
		return errors.New("invalid successful login throttle identities")
	}
	if in.Replacement != nil {
		return auth.ValidatePasswordVerifier(*in.Replacement)
	}
	return nil
}

// CreateSession locks the user before observing its revision, so a reset cannot create stale authority.
func (r *AuthRepository) CreateSession(ctx context.Context, id string, token auth.SessionToken, user LoginUser, replacement *auth.PasswordVerifier, auditID, sessionLimitAuditID string) (Session, error) {
	if err := validID(id); err != nil {
		return Session{}, err
	}
	if err := validID(auditID); err != nil {
		return Session{}, err
	}
	if err := validID(sessionLimitAuditID); err != nil || sessionLimitAuditID == auditID {
		return Session{}, errors.New("invalid distinct session-limit audit identifier")
	}
	if err := userInputValid(user); err != nil {
		return Session{}, err
	}
	digest, err := auth.SessionTokenDigest(token)
	if err != nil {
		return Session{}, ErrUnauthenticated
	}
	if replacement != nil {
		if err := auth.ValidatePasswordVerifier(*replacement); err != nil {
			return Session{}, err
		}
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Session{}, fmt.Errorf("begin create session: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var owner, status, phc string
	var current, policyRev, durablePolicyRev int64
	var durableMemory, durableIterations int32
	var durableLanes int16
	if err = tx.QueryRow(ctx, `SELECT u.owner_id,u.status,u.auth_revision,u.password_phc,u.password_policy_revision,i.password_policy_revision,i.password_memory_kib,i.password_iterations,i.password_lanes FROM users u JOIN installation_auth i ON i.id=1 WHERE u.id=$1 FOR UPDATE OF u`, user.ID).Scan(&owner, &status, &current, &phc, &policyRev, &durablePolicyRev, &durableMemory, &durableIterations, &durableLanes); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Session{}, ErrUnauthenticated
		}
		return Session{}, fmt.Errorf("lock session user: %w", err)
	}
	if owner != user.OwnerID || status != "active" || current != int64(user.AuthRevision) {
		return Session{}, ErrUnauthenticated
	}
	if replacement != nil {
		durablePolicy := auth.Argon2Policy{Revision: uint64(durablePolicyRev), MemoryKiB: uint32(durableMemory), Iterations: uint32(durableIterations), Lanes: uint8(durableLanes)}
		if err := auth.ValidatePasswordVerifierForPolicy(*replacement, durablePolicy); err != nil {
			return Session{}, fmt.Errorf("password verifier policy is not current: %w", err)
		}
		if _, err = tx.Exec(ctx, `UPDATE users SET password_phc=$2,password_policy_revision=$3,updated_at=clock_timestamp() WHERE id=$1`, user.ID, replacement.PHC, int64(replacement.PolicyRevision)); err != nil {
			return Session{}, fmt.Errorf("rehash password: %w", err)
		}
	}
	var s Session
	var revision int64
	err = tx.QueryRow(ctx, `WITH now_value AS (SELECT clock_timestamp() AS v) INSERT INTO admin_sessions(id,token_digest,user_id,owner_id,auth_revision,created_at,last_seen_at,idle_expires_at,absolute_expires_at) SELECT $1,$2,$3,$4,$5,v,v,v+interval '30 minutes',v+interval '8 hours' FROM now_value RETURNING created_at,last_seen_at,idle_expires_at,absolute_expires_at,auth_revision`, id, digest[:], user.ID, owner, current).Scan(&s.CreatedAt, &s.LastSeenAt, &s.IdleExpiresAt, &s.AbsoluteExpiresAt, &revision)
	if err != nil {
		return Session{}, fmt.Errorf("insert session: %w", err)
	}
	s.ID = id
	s.UserID = user.ID
	s.OwnerID = owner
	s.AuthRevision = uint64(revision)
	rows, err := tx.Query(ctx, `WITH excessive AS (SELECT id FROM admin_sessions WHERE user_id=$1 AND revoked_at IS NULL ORDER BY created_at DESC,id DESC OFFSET 10) UPDATE admin_sessions SET revoked_at=clock_timestamp(),revoked_reason='session_limit' WHERE id IN (SELECT id FROM excessive) RETURNING id`, user.ID)
	if err != nil {
		return Session{}, fmt.Errorf("limit sessions: %w", err)
	}
	var revokedIDs []string
	for rows.Next() {
		var revokedID string
		if err = rows.Scan(&revokedID); err != nil {
			rows.Close()
			return Session{}, fmt.Errorf("read limited session: %w", err)
		}
		revokedIDs = append(revokedIDs, revokedID)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return Session{}, fmt.Errorf("read limited sessions: %w", err)
	}
	rows.Close()
	if len(revokedIDs) > 1 {
		return Session{}, errors.New("session limit revoked more than one session")
	}
	if len(revokedIDs) == 1 {
		if err = insertAudit(ctx, tx, sessionLimitAuditID, user.ID, owner, "session_revoked", "success"); err != nil {
			return Session{}, err
		}
	}
	if err = insertAudit(ctx, tx, auditID, user.ID, owner, "login_success", "success"); err != nil {
		return Session{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Session{}, fmt.Errorf("commit create session: %w", err)
	}
	return s, nil
}

func (r *AuthRepository) ValidateSession(ctx context.Context, token auth.SessionToken) (auth.Principal, error) {
	digest, err := auth.SessionTokenDigest(token)
	if err != nil {
		return auth.Principal{}, ErrUnauthenticated
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return auth.Principal{}, fmt.Errorf("begin validate session: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// Establish the user first without a lock, then acquire locks in the
	// documented user -> session order.  The locked session query below
	// rechecks the digest and all authority fields before any touch commits.
	var userID string
	if err = tx.QueryRow(ctx, `SELECT user_id FROM admin_sessions WHERE token_digest=$1`, digest[:]).Scan(&userID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return auth.Principal{}, ErrUnauthenticated
		}
		return auth.Principal{}, fmt.Errorf("identify session user: %w", err)
	}
	if _, err = tx.Exec(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, userID); err != nil {
		return auth.Principal{}, fmt.Errorf("lock session user: %w", err)
	}
	var p auth.Principal
	var sessionRevision, currentRevision int64
	var last, idle, absolute time.Time
	err = tx.QueryRow(ctx, `SELECT s.id,s.user_id,s.owner_id,s.auth_revision,s.last_seen_at,s.idle_expires_at,s.absolute_expires_at,u.auth_revision FROM admin_sessions s JOIN users u ON u.id=s.user_id WHERE s.token_digest=$1 AND s.user_id=$2 AND s.revoked_at IS NULL AND u.status='active' AND s.owner_id=u.owner_id FOR UPDATE OF s`, digest[:], userID).Scan(&p.SessionID, &p.UserID, &p.OwnerID, &sessionRevision, &last, &idle, &absolute, &currentRevision)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return auth.Principal{}, ErrUnauthenticated
		}
		return auth.Principal{}, fmt.Errorf("load session: %w", err)
	}
	if sessionRevision != currentRevision {
		return auth.Principal{}, ErrUnauthenticated
	}
	p.Role = auth.RoleOwner
	p.AuthRevision = uint64(sessionRevision)
	var valid bool
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp() < $1 AND clock_timestamp() < $2`, idle, absolute).Scan(&valid); err != nil {
		return auth.Principal{}, fmt.Errorf("validate session time: %w", err)
	}
	if !valid {
		return auth.Principal{}, ErrUnauthenticated
	}
	if _, err = tx.Exec(ctx, `UPDATE admin_sessions SET last_seen_at=clock_timestamp(),idle_expires_at=LEAST(absolute_expires_at,clock_timestamp()+interval '30 minutes') WHERE id=$1 AND last_seen_at <= clock_timestamp()-interval '5 minutes'`, p.SessionID); err != nil {
		return auth.Principal{}, fmt.Errorf("touch session: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return auth.Principal{}, fmt.Errorf("commit validate session: %w", err)
	}
	return p, nil
}

func (r *AuthRepository) RevokeSession(ctx context.Context, token auth.SessionToken, principal auth.Principal, auditID string) error {
	if err := validID(auditID); err != nil {
		return err
	}
	if err := principal.Validate(); err != nil {
		return ErrUnauthenticated
	}
	digest, err := auth.SessionTokenDigest(token)
	if err != nil {
		return ErrUnauthenticated
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin revoke session: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var identifiedUser string
	if err = tx.QueryRow(ctx, `SELECT user_id FROM admin_sessions WHERE token_digest=$1`, digest[:]).Scan(&identifiedUser); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUnauthenticated
		}
		return fmt.Errorf("identify revoke user: %w", err)
	}
	if _, err = tx.Exec(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, identifiedUser); err != nil {
		return fmt.Errorf("lock revoke user: %w", err)
	}
	var user, owner, status string
	var revision int64
	var revoked *time.Time
	if err = tx.QueryRow(ctx, `SELECT s.user_id,s.owner_id,u.status,u.auth_revision,s.revoked_at FROM admin_sessions s JOIN users u ON u.id=s.user_id WHERE s.token_digest=$1 AND s.user_id=$2 FOR UPDATE OF s`, digest[:], identifiedUser).Scan(&user, &owner, &status, &revision, &revoked); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUnauthenticated
		}
		return fmt.Errorf("lock revoke session: %w", err)
	}
	if status != "active" || user != principal.UserID || owner != principal.OwnerID || revision != int64(principal.AuthRevision) || principal.SessionID == "" {
		return ErrUnauthenticated
	}
	var sessionID string
	if err = tx.QueryRow(ctx, `SELECT id FROM admin_sessions WHERE token_digest=$1`, digest[:]).Scan(&sessionID); err != nil || sessionID != principal.SessionID {
		return ErrUnauthenticated
	}
	if revoked != nil {
		return ErrSessionRevoked
	}
	if _, err = tx.Exec(ctx, `UPDATE admin_sessions SET revoked_at=clock_timestamp(),revoked_reason='logout' WHERE token_digest=$1`, digest[:]); err != nil {
		return fmt.Errorf("revoke session: %w", err)
	}
	if err = insertAudit(ctx, tx, auditID, user, owner, "logout", "success"); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit revoke session: %w", err)
	}
	return nil
}

// CleanupSessions removes a bounded batch of expired or already-revoked
// sessions. Authorization never depends on this housekeeping path.
func (r *AuthRepository) CleanupSessions(ctx context.Context, limit int) (int64, error) {
	if limit < 1 || limit > 1000 {
		return 0, errors.New("invalid session cleanup limit")
	}
	tag, err := r.pool.Exec(ctx, `DELETE FROM admin_sessions WHERE ctid IN (
		SELECT ctid FROM admin_sessions
		WHERE revoked_at IS NOT NULL OR idle_expires_at <= clock_timestamp() OR absolute_expires_at <= clock_timestamp()
		ORDER BY created_at LIMIT $1
	)`, limit)
	if err != nil {
		return 0, fmt.Errorf("cleanup sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}

func (r *AuthRepository) ResetPassword(ctx context.Context, userID string, verifier auth.PasswordVerifier, auditID string, recoveryDigests [][32]byte) error {
	if err := validID(userID); err != nil {
		return err
	}
	if err := validID(auditID); err != nil {
		return err
	}
	if err := auth.ValidatePasswordVerifier(verifier); err != nil {
		return err
	}
	// Recovery receives the current digest plus up to four retained aliases
	// for each of the pair and username buckets: at most ten distinct digests.
	if len(recoveryDigests) > 10 {
		return errors.New("too many recovery throttle digests")
	}
	seenRecovery := make(map[[32]byte]struct{}, len(recoveryDigests))
	for _, digest := range recoveryDigests {
		if digest == ([32]byte{}) {
			return errors.New("invalid zero recovery throttle digest")
		}
		if _, exists := seenRecovery[digest]; exists {
			return errors.New("duplicate recovery throttle digest")
		}
		seenRecovery[digest] = struct{}{}
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin password reset: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// The universal credential-flow order is throttle admission -> user ->
	// session. A first-time tagged pair reservation acquires the user FK lock
	// after throttle admission, so reset must acquire the advisory lock first.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(731492845631694123)`); err != nil {
		return fmt.Errorf("lock reset throttle admission: %w", err)
	}
	var owner string
	var revision, policyRev int64
	var memory, iterations int32
	var lanes int16
	if err = tx.QueryRow(ctx, `SELECT u.owner_id,u.auth_revision,i.password_policy_revision,i.password_memory_kib,i.password_iterations,i.password_lanes FROM users u JOIN installation_auth i ON i.id=1 WHERE u.id=$1 FOR UPDATE OF u`, userID).Scan(&owner, &revision, &policyRev, &memory, &iterations, &lanes); err != nil {
		return fmt.Errorf("lock reset user: %w", err)
	}
	if revision == math.MaxInt64 {
		return ErrRevisionOverflow
	}
	if err := auth.ValidatePasswordVerifierForPolicy(verifier, auth.Argon2Policy{Revision: uint64(policyRev), MemoryKiB: uint32(memory), Iterations: uint32(iterations), Lanes: uint8(lanes)}); err != nil {
		return fmt.Errorf("password verifier policy is not current: %w", err)
	}
	if _, err = tx.Exec(ctx, `UPDATE users SET password_phc=$2,password_policy_revision=$3,auth_revision=auth_revision+1,updated_at=clock_timestamp() WHERE id=$1`, userID, verifier.PHC, int64(verifier.PolicyRevision)); err != nil {
		return fmt.Errorf("replace password: %w", err)
	}
	if _, err = tx.Exec(ctx, `UPDATE admin_sessions SET revoked_at=clock_timestamp(),revoked_reason='password_reset' WHERE user_id=$1 AND revoked_at IS NULL`, userID); err != nil {
		return fmt.Errorf("revoke reset sessions: %w", err)
	}
	if _, err = tx.Exec(ctx, `DELETE FROM auth_throttle_buckets WHERE kind='pair' AND recovery_user_id=$1`, userID); err != nil {
		return fmt.Errorf("clear tagged recovery pair throttles: %w", err)
	}
	for _, d := range recoveryDigests {
		if _, err = tx.Exec(ctx, `DELETE FROM auth_throttle_buckets WHERE kind='pair' AND identifier_digest=$1 AND recovery_user_id IS NULL`, d[:]); err != nil {
			return fmt.Errorf("clear recovery throttle: %w", err)
		}
		if _, err = tx.Exec(ctx, `DELETE FROM auth_username_throttles WHERE identifier_digest=$1 AND key_version <> 'overflow'`, d[:]); err != nil {
			return fmt.Errorf("clear recovery username throttle: %w", err)
		}
	}
	if err = insertAudit(ctx, tx, auditID, userID, owner, "password_reset", "success"); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit password reset: %w", err)
	}
	return nil
}

func validID(id string) error {
	if len(id) == 0 || len(id) > 128 {
		return errors.New("invalid identifier")
	}
	return nil
}
func validateThrottleIdentities(identities []ThrottleIdentity, maxRows int) error {
	if len(identities) == 0 || len(identities) > 4 || maxRows < 8 || maxRows > 100000 {
		return errors.New("invalid throttle identities")
	}
	seen := make(map[string]struct{}, len(identities))
	for _, identity := range identities {
		if identity.Kind != ThrottlePair && identity.Kind != ThrottleIP && identity.Kind != ThrottleUsername && identity.Kind != ThrottleInvalidForward {
			return errors.New("invalid throttle kind")
		}
		if !validThrottleKeyVersion(identity.KeyVersion) || identity.KeyVersion == "overflow" || len(identity.Aliases) > maxThrottleAliases {
			return errors.New("invalid throttle key version")
		}
		if identity.Digest == ([32]byte{}) {
			return errors.New("invalid zero throttle digest")
		}
		if identity.RecoveryUserID != "" && (identity.Kind != ThrottlePair || validID(identity.RecoveryUserID) != nil) {
			return errors.New("invalid throttle recovery user")
		}
		key := string(identity.Kind) + "/" + identity.KeyVersion + string(identity.Digest[:])
		if _, ok := seen[key]; ok {
			return errors.New("duplicate throttle identity")
		}
		seen[key] = struct{}{}
		for _, alias := range identity.Aliases {
			if !validThrottleKeyVersion(alias.KeyVersion) || alias.KeyVersion == "overflow" || alias.Digest == ([32]byte{}) {
				return errors.New("invalid throttle alias")
			}
			aliasKey := string(identity.Kind) + "/" + alias.KeyVersion + string(alias.Digest[:])
			if _, ok := seen[aliasKey]; ok {
				return errors.New("duplicate throttle alias")
			}
			seen[aliasKey] = struct{}{}
		}
	}
	return nil
}

func pairRecoveryUserID(identity ThrottleIdentity, kind string) *string {
	if kind != string(ThrottlePair) || identity.RecoveryUserID == "" {
		return nil
	}
	return &identity.RecoveryUserID
}

func validThrottleKeyVersion(version string) bool {
	if len(version) == 0 || len(version) > 32 {
		return false
	}
	for i := range version {
		c := version[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func validateBootstrap(in BootstrapInput) error {
	for _, id := range []string{in.OwnerID, in.UserID, in.AuditID} {
		if err := validID(id); err != nil {
			return err
		}
	}
	canonical, err := auth.CanonicalUsername(in.Username)
	if err != nil || canonical != in.Username {
		return errors.New("invalid canonical username")
	}
	if err = in.Policy.Validate(); err != nil {
		return err
	}
	if err = auth.ValidatePasswordVerifierForPolicy(in.Verifier, in.Policy); err != nil {
		return err
	}
	return nil
}
func userInputValid(u LoginUser) error {
	if validID(u.ID) != nil || validID(u.OwnerID) != nil || u.AuthRevision == 0 {
		return errors.New("invalid login user")
	}
	return nil
}
func insertAudit(ctx context.Context, tx pgx.Tx, id, actor, owner, action, result string) error {
	if _, err := tx.Exec(ctx, `INSERT INTO auth_audit_events(id,actor_user_id,owner_id,action,result) VALUES($1,$2,$3,$4,$5)`, id, actor, owner, action, result); err != nil {
		return fmt.Errorf("append auth audit: %w", err)
	}
	return nil
}
