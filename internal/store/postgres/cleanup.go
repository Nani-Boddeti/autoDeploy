package postgres

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	"autodeploy/internal/github"
	"autodeploy/internal/runtime/dockerid"

	"github.com/jackc/pgx/v5"
)

// Cleanup results the agent may report (migration 000026).
const (
	CleanupResultRemoved = "removed"
	CleanupResultFailed  = "failed"
	CleanupResultSkipped = "skipped"

	CleanupErrorEngine   = "engine"
	CleanupErrorIdentity = "identity"
	CleanupErrorStop     = "stop"
	CleanupErrorOther    = "other"

	maxCleanupClaimLimit = 16
)

// CleanupPolicy is the retention policy (ADR 0005 "Retention and drain policy",
// owner decision B-D5). The zero value is invalid; use DefaultCleanupPolicy.
type CleanupPolicy struct {
	// KeepRetained is how many of an environment's newest retained releases
	// (by retention time = the release row's updated_at at active -> retained)
	// are always kept, whatever their age.
	KeepRetained int
	// MaxRetainedAge makes a retained release eligible once it has been
	// retained this long, even inside the newest KeepRetained.
	MaxRetainedAge time.Duration
	// RetryBackoff and MaxAttempts bound how often a release left in
	// 'removing' by a failed or skipped cleanup is handed out again.
	RetryBackoff time.Duration
	MaxAttempts  int
}

// DefaultCleanupPolicy: 2 retained releases, 7 days, retry hourly, 5 tries.
func DefaultCleanupPolicy() CleanupPolicy {
	return CleanupPolicy{KeepRetained: 2, MaxRetainedAge: 7 * 24 * time.Hour, RetryBackoff: time.Hour, MaxAttempts: 5}
}

func (p CleanupPolicy) valid() bool {
	return p.KeepRetained >= 1 && p.MaxRetainedAge > 0 && p.RetryBackoff > 0 && p.MaxAttempts > 0
}

// SetCleanupPolicy replaces the retention policy (tests and wiring).
func (r *AgentLeaseRepository) SetCleanupPolicy(p CleanupPolicy) { r.cleanupPolicy = &p }

func (r *AgentLeaseRepository) policy() CleanupPolicy {
	if r.cleanupPolicy != nil {
		return *r.cleanupPolicy
	}
	return DefaultCleanupPolicy()
}

// CleanupClaim is one release the agent may now remove. Every field is
// server-derived: the agent only locates the container by these exact values.
type CleanupClaim struct {
	CleanupID         int64
	ReleaseID         string
	EnvironmentID     string
	RuntimeInstanceID string
	ImageDigest       string // hex, 64 characters
}

func validCleanupResult(result string) bool {
	return result == CleanupResultRemoved || result == CleanupResultFailed || result == CleanupResultSkipped
}

func validCleanupErrorClass(class string) bool {
	switch class {
	case CleanupErrorEngine, CleanupErrorIdentity, CleanupErrorStop, CleanupErrorOther:
		return true
	}
	return false
}

// cleanupCandidate is one release a claim may hand out.
type cleanupCandidate struct {
	releaseID, environmentID string
	imageDigest              []byte
	// openCleanupID is set when the release already has an open cleanup that
	// is simply handed out again (the earlier answer or report was lost).
	openCleanupID int64
	// retry is a 'removing' release whose last cleanup failed and whose
	// backoff elapsed: it gets a new cleanup row, no state change.
	retry bool
}

// cleanupCandidateEnvironments is the non-locking peek that only decides which
// environment slots to lock: environments of serverID with a retained release
// that could be eligible by count or age, or a removing release with a cleanup.
// Everything is re-evaluated under the locks.
const cleanupCandidateEnvironments = `
SELECT DISTINCT environment_id FROM (
  SELECT r.environment_id FROM releases r JOIN agent_jobs j ON j.deployment_id=r.deployment_id AND j.job_kind='deployment_execution'
   WHERE r.state='retained' AND j.server_id=$1
  UNION
  SELECT c.environment_id FROM release_cleanups c WHERE c.server_id=$1 AND c.completed_at IS NULL
  UNION
  SELECT c.environment_id FROM release_cleanups c JOIN releases r ON r.id=c.release_id WHERE c.server_id=$1 AND r.state='removing'
) e ORDER BY environment_id LIMIT $2`

// cleanupEligibleRetained lists the eligible retained releases of one
// environment, oldest retention first. It must run under the environment slot
// lock (and the route head FOR SHARE). The rank is computed over ALL retained
// releases of the environment, so protected releases still occupy a "kept"
// position.
//
// A release is NEVER eligible when it is the target of a queued, leased or
// retrying job (an in-flight rollback), when a release operation of it is not
// terminal (an unresolved activation), or when its environment slot is
// occupied or reconciling or its route head is flagged.
const cleanupEligibleRetained = `
SELECT id, image_digest FROM (
  SELECT r.id, r.image_digest, r.updated_at,
         row_number() OVER (ORDER BY r.updated_at DESC, r.id DESC) AS rn
    FROM releases r
   WHERE r.environment_id=$1 AND r.state='retained'
) ranked
JOIN LATERAL (SELECT j.server_id FROM releases rr JOIN agent_jobs j ON j.deployment_id=rr.deployment_id AND j.job_kind='deployment_execution' WHERE rr.id=ranked.id) own ON own.server_id=$2
WHERE (ranked.rn > $3 OR ranked.updated_at <= clock_timestamp() - make_interval(secs => $4))
  AND ranked.image_digest IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM agent_jobs aj WHERE aj.rollback_target_release_id=ranked.id AND aj.state IN ('queued','leased','retry_wait'))
  AND NOT EXISTS (SELECT 1 FROM release_operations ro WHERE ro.release_id=ranked.id AND ro.state NOT IN ('succeeded','failed'))
  AND EXISTS (SELECT 1 FROM environment_slots s WHERE s.environment_id=$1 AND s.current_attempt_id IS NULL AND NOT s.reconciliation_required)
  AND EXISTS (SELECT 1 FROM route_heads h WHERE h.environment_id=$1 AND NOT h.reconciliation_required)
ORDER BY ranked.updated_at, ranked.id`

// ClaimCleanups hands the calling server at most limit releases to remove
// (step 20E, owner decisions B-D5/B-D6). The server is the credential's, never
// a request field.
//
// The fence is database state: in ONE READ COMMITTED transaction, in the ADR 0004
// amendment 2026-11-13 ("Retention cleanup") order -- server credential (SHARE,
// position 1), the candidate environments' slots (FOR UPDATE, position 3, by
// environment id ascending), their route heads (FOR SHARE, 6b), the chosen
// release rows (FOR UPDATE, 6b, by id ascending), then the append-only insert
// (8) -- it re-evaluates eligibility, moves each release retained -> removing
// and inserts its release_cleanups row. Because EnqueueRollback, Activate and
// the activation finalizers all take the same slot first, a rollback enqueue
// and a claim racing on one release serialize: either the rollback commits
// first (its queued job protects the release) or the claim does (the release is
// 'removing', which EnqueueRollback refuses).
//
// Already-open cleanups of this server are handed out again unchanged (their
// report or answer was lost; removal is idempotent). A release left 'removing'
// by a failed or skipped cleanup gets a new cleanup row once RetryBackoff has
// passed, at most MaxAttempts times in all.
func (r *AgentLeaseRepository) ClaimCleanups(ctx context.Context, serverID string, presentedDigest [32]byte, limit int) ([]CleanupClaim, error) {
	if r == nil || r.pool == nil || !github.ValidOpaqueID(serverID) || limit <= 0 || limit > maxCleanupClaimLimit {
		return nil, ErrAgentLeaseInvalidInput
	}
	policy := r.policy()
	if !policy.valid() {
		return nil, ErrAgentLeaseInvalidInput
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("begin cleanup claim: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err = lockActiveServerCredential(ctx, tx, serverID, presentedDigest); err != nil {
		return nil, err
	}
	environments, err := queryStrings(ctx, tx, cleanupCandidateEnvironments, serverID, limit)
	if err != nil {
		return nil, fmt.Errorf("list cleanup environments: %w", err)
	}
	// Slots ascending (position 3). Other transactions hold at most one slot
	// at a time, so ascending acquisition cannot cycle.
	for _, environmentID := range environments {
		if err = tx.QueryRow(ctx, `SELECT 1 FROM environment_slots WHERE environment_id=$1 FOR UPDATE`, environmentID).Scan(new(int)); err != nil {
			return nil, fmt.Errorf("lock cleanup environment slot: %w", err)
		}
	}
	if len(environments) > 0 {
		// Route heads (6b) FOR SHARE, ascending: a flagged head vetoes the environment.
		rows, qerr := tx.Query(ctx, `SELECT environment_id FROM route_heads WHERE environment_id = ANY($1) ORDER BY environment_id FOR SHARE`, environments)
		if qerr != nil {
			return nil, fmt.Errorf("lock cleanup route heads: %w", qerr)
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return nil, fmt.Errorf("lock cleanup route heads: %w", err)
		}
	}

	var candidates []cleanupCandidate
	for _, environmentID := range environments {
		found, ferr := cleanupCandidatesUnderLock(ctx, tx, serverID, environmentID, policy)
		if ferr != nil {
			return nil, ferr
		}
		candidates = append(candidates, found...)
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].releaseID < candidates[j].releaseID })
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	if len(candidates) == 0 {
		if err = tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("commit empty cleanup claim: %w", err)
		}
		return nil, nil
	}

	// Release rows (6b) FOR UPDATE, ascending; the state is re-checked.
	ids := make([]string, len(candidates))
	for i, c := range candidates {
		ids[i] = c.releaseID
	}
	states := map[string]string{}
	rows, err := tx.Query(ctx, `SELECT id, state FROM releases WHERE id = ANY($1) ORDER BY id FOR UPDATE`, ids)
	if err != nil {
		return nil, fmt.Errorf("lock cleanup releases: %w", err)
	}
	for rows.Next() {
		var id, state string
		if err = rows.Scan(&id, &state); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan cleanup release: %w", err)
		}
		states[id] = state
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("lock cleanup releases: %w", err)
	}

	claims := make([]CleanupClaim, 0, len(candidates))
	for _, c := range candidates {
		want := "retained"
		if c.openCleanupID != 0 || c.retry {
			want = "removing"
		}
		if states[c.releaseID] != want {
			continue // changed between the eligibility read and the row lock: definite skip
		}
		attemptID, ok, werr := winningActivationAttempt(ctx, tx, c.releaseID)
		if werr != nil {
			return nil, werr
		}
		if !ok {
			continue // no known container identity: never hand out a guess
		}
		cleanupID := c.openCleanupID
		if cleanupID == 0 {
			if !c.retry {
				if _, err = tx.Exec(ctx, `UPDATE releases SET state='removing', updated_at=clock_timestamp() WHERE id=$1 AND state='retained'`, c.releaseID); err != nil {
					return nil, classifyActivationResultError(fmt.Errorf("release retained->removing: %w", err))
				}
			}
			if err = tx.QueryRow(ctx, `INSERT INTO release_cleanups(release_id, server_id, environment_id) VALUES ($1,$2,$3) RETURNING id`, c.releaseID, serverID, c.environmentID).Scan(&cleanupID); err != nil {
				return nil, classifyActivationResultError(fmt.Errorf("insert release cleanup: %w", err))
			}
		}
		claims = append(claims, CleanupClaim{
			CleanupID: cleanupID, ReleaseID: c.releaseID, EnvironmentID: c.environmentID,
			RuntimeInstanceID: dockerid.AttemptRuntimeInstanceID(attemptID), ImageDigest: hex.EncodeToString(c.imageDigest),
		})
	}
	if err = r.inject("cleanup_claim"); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, classifyActivationResultError(fmt.Errorf("commit cleanup claim: %w", err))
	}
	return claims, nil
}

// cleanupCandidatesUnderLock runs with the environment slot and route head
// locked. Order of preference inside one environment: open cleanups (redeliver),
// failed-removal retries, then fresh retained releases.
func cleanupCandidatesUnderLock(ctx context.Context, tx pgx.Tx, serverID, environmentID string, policy CleanupPolicy) ([]cleanupCandidate, error) {
	var out []cleanupCandidate
	// Slot and route head veto everything in the environment that is not an
	// already-open cleanup (which was claimed while the environment was quiet
	// and is idempotent to redo).
	var quiet bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM environment_slots s WHERE s.environment_id=$1 AND s.current_attempt_id IS NULL AND NOT s.reconciliation_required)
		AND EXISTS (SELECT 1 FROM route_heads h WHERE h.environment_id=$1 AND NOT h.reconciliation_required)`, environmentID).Scan(&quiet); err != nil {
		return nil, fmt.Errorf("read cleanup environment: %w", err)
	}

	rows, err := tx.Query(ctx, `SELECT c.id, c.release_id, r.image_digest FROM release_cleanups c JOIN releases r ON r.id=c.release_id
		WHERE c.server_id=$1 AND c.environment_id=$2 AND c.completed_at IS NULL AND r.state='removing' AND r.image_digest IS NOT NULL ORDER BY c.id`, serverID, environmentID)
	if err != nil {
		return nil, fmt.Errorf("read open cleanups: %w", err)
	}
	for rows.Next() {
		c := cleanupCandidate{environmentID: environmentID}
		if err = rows.Scan(&c.openCleanupID, &c.releaseID, &c.imageDigest); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan open cleanup: %w", err)
		}
		out = append(out, c)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("read open cleanups: %w", err)
	}
	if !quiet {
		return out, nil
	}

	// Failed or skipped last cleanup, backoff elapsed, attempts left.
	rows, err = tx.Query(ctx, `SELECT r.id, r.image_digest FROM releases r
		WHERE r.environment_id=$1 AND r.state='removing' AND r.image_digest IS NOT NULL
		  AND NOT EXISTS (SELECT 1 FROM release_cleanups o WHERE o.release_id=r.id AND o.completed_at IS NULL)
		  AND EXISTS (SELECT 1 FROM release_cleanups l WHERE l.release_id=r.id AND l.server_id=$2 AND l.id=(SELECT max(id) FROM release_cleanups WHERE release_id=r.id)
		              AND l.result IN ('failed','skipped') AND l.completed_at <= clock_timestamp() - make_interval(secs => $3))
		  AND (SELECT count(*) FROM release_cleanups WHERE release_id=r.id) < $4
		ORDER BY r.id`, environmentID, serverID, policy.RetryBackoff.Seconds(), policy.MaxAttempts)
	if err != nil {
		return nil, fmt.Errorf("read cleanup retries: %w", err)
	}
	for rows.Next() {
		c := cleanupCandidate{environmentID: environmentID, retry: true}
		if err = rows.Scan(&c.releaseID, &c.imageDigest); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan cleanup retry: %w", err)
		}
		out = append(out, c)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("read cleanup retries: %w", err)
	}

	rows, err = tx.Query(ctx, cleanupEligibleRetained, environmentID, serverID, policy.KeepRetained, policy.MaxRetainedAge.Seconds())
	if err != nil {
		return nil, fmt.Errorf("read eligible retained releases: %w", err)
	}
	for rows.Next() {
		c := cleanupCandidate{environmentID: environmentID}
		if err = rows.Scan(&c.releaseID, &c.imageDigest); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan eligible retained release: %w", err)
		}
		out = append(out, c)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("read eligible retained releases: %w", err)
	}
	return out, nil
}

func queryStrings(ctx context.Context, tx pgx.Tx, sql string, args ...any) ([]string, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err = rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// RecordCleanupResult records what the agent's removal of one claimed release
// ended in (step 20E). Credential-authenticated only; authority is DERIVED: the
// cleanup row must belong to the credential's server (any other or unknown id
// is ErrServerAuthorizationDenied, never distinguished).
//
// Lock order: server credential (SHARE, 1) -> the cleanup's environment slot
// (FOR UPDATE, 3) -> the release row (FOR UPDATE, 6b) -> the cleanup row
// (FOR UPDATE, 8's class, locked last). The slot lock serializes it with a
// claim that might retry the same release.
//
// Idempotent by cleanup id: a replay of the identical report is a no-op
// success; a different report for a completed cleanup is
// ErrAgentLeaseIntegrityConflict (first report wins; rows are write-once).
// Only result 'removed' moves the release removing -> removed; failed and
// skipped leave it 'removing', operator visible, retried by a later claim after
// the backoff. The server never marks a release removed on its own.
func (r *AgentLeaseRepository) RecordCleanupResult(ctx context.Context, serverID string, presentedDigest [32]byte, cleanupID int64, result, errorClass string, imageKept bool, summary string) error {
	if r == nil || r.pool == nil || !github.ValidOpaqueID(serverID) || cleanupID <= 0 || !validCleanupResult(result) ||
		(result == CleanupResultRemoved) != (errorClass == "") || (errorClass != "" && !validCleanupErrorClass(errorClass)) ||
		(imageKept && result != CleanupResultRemoved) || !validDrainSummary(summary) {
		return ErrAgentLeaseInvalidInput
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin cleanup result: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err = lockActiveServerCredential(ctx, tx, serverID, presentedDigest); err != nil {
		return err
	}
	var cleanupServerID, environmentID, releaseID string
	if err = tx.QueryRow(ctx, `SELECT server_id, environment_id, release_id FROM release_cleanups WHERE id=$1`, cleanupID).Scan(&cleanupServerID, &environmentID, &releaseID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrServerAuthorizationDenied
		}
		return fmt.Errorf("read cleanup: %w", err)
	}
	if cleanupServerID != serverID {
		return ErrServerAuthorizationDenied
	}
	if err = tx.QueryRow(ctx, `SELECT 1 FROM environment_slots WHERE environment_id=$1 FOR UPDATE`, environmentID).Scan(new(int)); err != nil {
		return fmt.Errorf("lock cleanup result slot: %w", err)
	}
	var releaseState string
	if err = tx.QueryRow(ctx, `SELECT state FROM releases WHERE id=$1 FOR UPDATE`, releaseID).Scan(&releaseState); err != nil {
		return fmt.Errorf("lock cleanup result release: %w", err)
	}
	var storedResult, storedClass, storedSummary *string
	var storedKept bool
	if err = tx.QueryRow(ctx, `SELECT result, error_class, outcome_summary, image_kept FROM release_cleanups WHERE id=$1 FOR UPDATE`, cleanupID).Scan(&storedResult, &storedClass, &storedSummary, &storedKept); err != nil {
		return fmt.Errorf("lock cleanup row: %w", err)
	}
	if storedResult != nil {
		same := *storedResult == result && derefString(storedClass) == errorClass && derefString(storedSummary) == summary && storedKept == imageKept
		if !same {
			return ErrAgentLeaseIntegrityConflict
		}
		if err = tx.Commit(ctx); err != nil {
			return classifyActivationResultError(fmt.Errorf("commit cleanup result replay: %w", err))
		}
		return nil
	}
	if releaseState != "removing" {
		return ErrAgentLeaseCorruptState
	}
	var class any
	if errorClass != "" {
		class = errorClass
	}
	if _, err = tx.Exec(ctx, `UPDATE release_cleanups SET result=$2, error_class=$3, image_kept=$4, outcome_summary=$5, completed_at=clock_timestamp() WHERE id=$1 AND result IS NULL`,
		cleanupID, result, class, imageKept, summary); err != nil {
		return classifyActivationResultError(fmt.Errorf("record cleanup result: %w", err))
	}
	if result == CleanupResultRemoved {
		if _, err = tx.Exec(ctx, `UPDATE releases SET state='removed', updated_at=clock_timestamp() WHERE id=$1 AND state='removing'`, releaseID); err != nil {
			return classifyActivationResultError(fmt.Errorf("release removing->removed: %w", err))
		}
	}
	if err = r.inject("cleanup_result"); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return classifyActivationResultError(fmt.Errorf("commit cleanup result: %w", err))
	}
	return nil
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ReleaseCleanup is the latest cleanup of a release, for the web detail page.
type ReleaseCleanup struct {
	ID          int64
	ClaimedAt   time.Time
	Result      string // empty while open
	ErrorClass  string
	ImageKept   bool
	Summary     string
	CompletedAt *time.Time
	Attempts    int
}

// readLatestCleanup returns the newest cleanup of releaseID (nil when the
// release was never claimed) and how many cleanups it has had. The caller has
// already established that the release belongs to the principal.
func readLatestCleanup(ctx context.Context, tx pgx.Tx, releaseID string) (*ReleaseCleanup, error) {
	var c ReleaseCleanup
	var result, class *string
	err := tx.QueryRow(ctx, `SELECT id, claimed_at, result, error_class, image_kept, outcome_summary, completed_at, (SELECT count(*) FROM release_cleanups WHERE release_id=$1)
FROM release_cleanups WHERE release_id=$1 ORDER BY id DESC LIMIT 1`, releaseID).
		Scan(&c.ID, &c.ClaimedAt, &result, &class, &c.ImageKept, &c.Summary, &c.CompletedAt, &c.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read release cleanup: %w", err)
	}
	c.Result, c.ErrorClass = derefString(result), derefString(class)
	return &c, nil
}
