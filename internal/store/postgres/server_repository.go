package postgres

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"
	"unicode/utf8"

	"autodeploy/internal/auth"
	"autodeploy/internal/github"
	"autodeploy/internal/server"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ServerRepository persists deployment-agent server identity, scoped bearer
// credentials, single-use enrollment tokens, and their audit trail. Every
// method commits exactly once with a deferred rollback; callers pass only
// digests, never raw tokens, so this repository never observes bearer
// material and cannot leak it into a query, error, or audit row.
type ServerRepository struct {
	pool      *pgxpool.Pool
	failAfter func(string) error // test-only deterministic rollback seam
}

func NewServerRepository(pool *pgxpool.Pool) *ServerRepository { return &ServerRepository{pool: pool} }

func (r *ServerRepository) inject(stage string) error {
	if r.failAfter == nil {
		return nil
	}
	return r.failAfter(stage)
}

// AuditActor identifies who caused a server audit event. It is validated
// against the same enum the servers_audit_events CHECK enforces.
type AuditActor struct {
	Type server.ActorType
	ID   string
}

func (a AuditActor) valid() bool {
	if len(a.ID) > 128 {
		return false
	}
	switch a.Type {
	case server.ActorAdministrator, server.ActorAgent, server.ActorSystem:
		return true
	default:
		return false
	}
}

// administratorActor derives the audit actor from an already-validated
// principal rather than accepting one from the caller, so an administrator
// can never spoof a different actor ID in the audit trail.
func administratorActor(principal auth.Principal) AuditActor {
	return AuditActor{Type: server.ActorAdministrator, ID: principal.UserID}
}

// lockAdministratorSession re-validates principal's session live, inside the
// current transaction, mirroring GitHubRepository.BindRepository /
// DisableInstallation: the web layer already validated the session once for
// this request, but a durable, multi-statement administrative mutation
// re-checks it under FOR UPDATE so a session revoked in the interim (or
// concurrently, by another request) cannot ride through on a stale
// principal.
func lockAdministratorSession(ctx context.Context, tx pgx.Tx, principal auth.Principal) error {
	if principal.Validate() != nil {
		return ErrServerAuthorizationDenied
	}
	var locked int
	err := tx.QueryRow(ctx, `SELECT 1 FROM admin_sessions s JOIN users u ON u.id=s.user_id AND u.owner_id=s.owner_id WHERE s.id=$1 AND s.user_id=$2 AND s.owner_id=$3 AND s.auth_revision=$4 AND u.auth_revision=$4 AND u.status='active' AND s.revoked_at IS NULL AND s.idle_expires_at>clock_timestamp() AND s.absolute_expires_at>clock_timestamp() FOR UPDATE OF s,u`, principal.SessionID, principal.UserID, principal.OwnerID, int64(principal.AuthRevision)).Scan(&locked)
	if err != nil || locked != 1 {
		return ErrServerAuthorizationDenied
	}
	return nil
}

// validReason mirrors the server_audit_events.reason CHECK constraint:
// bounded length and no control characters. Validating here lets the
// repository fail cleanly before ever reaching the database.
func validReason(reason string) bool {
	if len(reason) > 512 || !utf8.ValidString(reason) {
		return false
	}
	for _, r := range reason {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// dummyCredentialDigest is compared against presented bearer material on an
// unknown-server or unknown-token lookup so Authenticate and Enroll always
// perform one constant-time comparison, never skip it outright. It can never
// equal a real stored digest (those are domain-separated SHA-256 outputs
// over caller randomness), so it never causes a false accept.
var dummyCredentialDigest = sha256.Sum256([]byte("autodeploy/server-repository/dummy-digest/v1"))

// RegisterServerInput constructs a brand-new server together with its first
// (and, at registration time, only) enrollment token. ServerID and Name must
// already be validated by the caller via server.Server.Validate with
// Status == server.StatusActive and zero timestamps; the owner comes from
// the calling principal, never from caller input, and the repository
// assigns CreatedAt from PostgreSQL time.
type RegisterServerInput struct {
	ServerID, Name       string
	ProtocolVersion      int
	EnrollmentDigest     [32]byte
	EnrollmentTTLSeconds int
}

func (in RegisterServerInput) valid(ownerID string) bool {
	s := server.Server{ID: in.ServerID, OwnerID: ownerID, Name: in.Name, ProtocolVersion: in.ProtocolVersion, Status: server.StatusActive, CreatedAt: time.Unix(1, 0)}
	return s.Validate() == nil && in.EnrollmentTTLSeconds > 0 && in.EnrollmentTTLSeconds <= 3600
}

// RegisterServer commits the new server row, its first enrollment token
// digest, and a "register" audit event in one transaction. The server is
// always owned by principal.OwnerID: an administrator can never register a
// server under any other owner.
func (r *ServerRepository) RegisterServer(ctx context.Context, principal auth.Principal, in RegisterServerInput) (server.Server, time.Time, error) {
	if r == nil || r.pool == nil || principal.Validate() != nil || !in.valid(principal.OwnerID) {
		return server.Server{}, time.Time{}, ErrServerAuthorizationDenied
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return server.Server{}, time.Time{}, fmt.Errorf("begin server register: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if err = lockAdministratorSession(ctx, tx, principal); err != nil {
		return server.Server{}, time.Time{}, err
	}
	actor := administratorActor(principal)

	var createdAt time.Time
	err = tx.QueryRow(ctx, `INSERT INTO servers(id,owner_id,name,protocol_version,status) VALUES($1,$2,$3,$4,'active') RETURNING created_at`, in.ServerID, principal.OwnerID, in.Name, in.ProtocolVersion).Scan(&createdAt)
	if err != nil {
		if isUniqueViolation(err) || isForeignKeyViolation(err) {
			return server.Server{}, time.Time{}, ErrServerAuthorizationDenied
		}
		return server.Server{}, time.Time{}, fmt.Errorf("insert server: %w", err)
	}
	if err = r.inject("server_insert"); err != nil {
		return server.Server{}, time.Time{}, err
	}

	var expiresAt time.Time
	err = tx.QueryRow(ctx, `INSERT INTO server_enrollment_tokens(server_id,digest,issued_at,expires_at) VALUES($1,$2,clock_timestamp(),clock_timestamp()+make_interval(secs=>$3)) RETURNING expires_at`, in.ServerID, in.EnrollmentDigest[:], in.EnrollmentTTLSeconds).Scan(&expiresAt)
	if err != nil {
		return server.Server{}, time.Time{}, fmt.Errorf("insert server enrollment token: %w", err)
	}
	if err = r.inject("enrollment_insert"); err != nil {
		return server.Server{}, time.Time{}, err
	}

	if err = insertServerAuditEvent(ctx, tx, actor, "register", &in.ServerID, nil, "success", ""); err != nil {
		return server.Server{}, time.Time{}, err
	}
	if err = r.inject("audit_insert"); err != nil {
		return server.Server{}, time.Time{}, err
	}

	if err = tx.Commit(ctx); err != nil {
		return server.Server{}, time.Time{}, fmt.Errorf("commit server register: %w", err)
	}
	return server.Server{ID: in.ServerID, OwnerID: principal.OwnerID, Name: in.Name, ProtocolVersion: in.ProtocolVersion, Status: server.StatusActive, CreatedAt: createdAt}, expiresAt, nil
}

// IssueEnrollment reissues an enrollment token for an existing, active,
// not-yet-enrolled server. Reissue always invalidates the prior unconsumed
// token by overwriting its digest and lifetime. A server whose token was
// already consumed (i.e. it has already enrolled) can never receive a new
// enrollment token: the update is rejected before any row changes.
func (r *ServerRepository) IssueEnrollment(ctx context.Context, principal auth.Principal, serverID string, digest [32]byte, ttlSeconds int) (time.Time, error) {
	if r == nil || r.pool == nil || principal.Validate() != nil || !github.ValidOpaqueID(serverID) || ttlSeconds <= 0 || ttlSeconds > 3600 {
		return time.Time{}, ErrServerAuthorizationDenied
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return time.Time{}, fmt.Errorf("begin server enrollment reissue: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if err = lockAdministratorSession(ctx, tx, principal); err != nil {
		return time.Time{}, err
	}
	actor := administratorActor(principal)

	// No failure audit row here: the server's own existence (under this
	// owner) is unconfirmed, and server_audit_events.server_id
	// FK-references servers(id), so auditing an unknown ID would itself
	// fail. A rolled-back transaction could not retain such a row anyway.
	// lockActiveServer's owner scope makes an owner-B server indistinguishable
	// from an unknown one to an owner-A principal.
	if err = lockActiveServer(ctx, tx, serverID, principal.OwnerID); err != nil {
		return time.Time{}, err
	}
	var consumedAt *time.Time
	if err = tx.QueryRow(ctx, `SELECT consumed_at FROM server_enrollment_tokens WHERE server_id=$1 FOR UPDATE`, serverID).Scan(&consumedAt); err != nil {
		return time.Time{}, ErrServerAuthorizationDenied
	}
	if consumedAt != nil {
		return time.Time{}, ErrServerAuthorizationDenied
	}
	var expiresAt time.Time
	if err = tx.QueryRow(ctx, `UPDATE server_enrollment_tokens SET digest=$2,issued_at=clock_timestamp(),expires_at=clock_timestamp()+make_interval(secs=>$3) WHERE server_id=$1 AND consumed_at IS NULL RETURNING expires_at`, serverID, digest[:], ttlSeconds).Scan(&expiresAt); err != nil {
		return time.Time{}, ErrServerAuthorizationDenied
	}
	if err = r.inject("enrollment_reissue"); err != nil {
		return time.Time{}, err
	}
	if err = insertServerAuditEvent(ctx, tx, actor, "enrollment_issued", &serverID, nil, "success", ""); err != nil {
		return time.Time{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return time.Time{}, fmt.Errorf("commit server enrollment reissue: %w", err)
	}
	return expiresAt, nil
}

// Enroll consumes a presented enrollment digest and, atomically, mints
// credential version 1 from the presented credential digest. It rejects any
// unknown server, expired token, already-consumed token, or digest mismatch
// with one generic error so a caller can never distinguish which check
// failed. PostgreSQL time (clock_timestamp()) is the sole TTL authority.
// Enroll is agent-initiated and authorized solely by possession of the
// enrollment token digest, so it takes no principal; the audit actor is
// always the agent itself.
func (r *ServerRepository) Enroll(ctx context.Context, serverID string, enrollmentDigest, credentialDigest [32]byte) (int, error) {
	if r == nil || r.pool == nil || !github.ValidOpaqueID(serverID) {
		return 0, ErrServerAuthorizationDenied
	}
	actor := AuditActor{Type: server.ActorAgent, ID: serverID}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, fmt.Errorf("begin server enroll: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Unlike lockActiveServer's early return, an unknown or inactive server
	// falls through here (foundServer=false) rather than returning
	// immediately: Enroll must still perform the constant-time digest
	// comparison below so an unknown server and a known server with a wrong
	// token cannot be told apart by whether a comparison happened at all.
	// The two paths still differ in round-trip count (one query fewer for an
	// unknown server) and, when a server is known, in one extra audit-write
	// query; that residual latency difference is accepted (see below).
	var status string
	foundServer := true
	if err = tx.QueryRow(ctx, `SELECT status FROM servers WHERE id=$1 FOR UPDATE`, serverID).Scan(&status); err != nil || status != "active" {
		foundServer = false
	}

	var storedDigest []byte
	var expiresAt time.Time
	var consumedAt *time.Time
	foundToken := false
	if foundServer {
		if err = tx.QueryRow(ctx, `SELECT digest,expires_at,consumed_at FROM server_enrollment_tokens WHERE server_id=$1 FOR UPDATE`, serverID).Scan(&storedDigest, &expiresAt, &consumedAt); err == nil {
			foundToken = true
		}
	}
	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return 0, fmt.Errorf("server enroll canonical time: %w", err)
	}
	var stored [32]byte
	if foundToken {
		copy(stored[:], storedDigest)
	} else {
		stored = dummyCredentialDigest
	}
	match := server.ConstantTimeEqualDigest(stored, enrollmentDigest)
	if !foundServer || !foundToken || consumedAt != nil || !now.Before(expiresAt) || !match {
		if foundServer {
			// Audit rows are kept for known servers only: server_id
			// FK-references servers(id), so auditing an unknown server would
			// itself fail, and the transaction rolls back regardless.
			_ = insertServerAuditEvent(ctx, tx, actor, "enrollment_rejected", &serverID, nil, "failure", "invalid enrollment token")
			if err = tx.Commit(ctx); err != nil {
				return 0, fmt.Errorf("commit server enrollment rejection: %w", err)
			}
		}
		return 0, ErrServerAuthorizationDenied
	}

	if _, err = tx.Exec(ctx, `UPDATE server_enrollment_tokens SET consumed_at=$2 WHERE server_id=$1 AND consumed_at IS NULL`, serverID, now); err != nil {
		return 0, fmt.Errorf("consume server enrollment token: %w", err)
	}
	if err = r.inject("enrollment_consume"); err != nil {
		return 0, err
	}

	const version = 1
	if _, err = tx.Exec(ctx, `INSERT INTO server_credentials(server_id,version,digest,status) VALUES($1,$2,$3,'active')`, serverID, version, credentialDigest[:]); err != nil {
		if isUniqueViolation(err) {
			return 0, ErrServerAuthorizationDenied
		}
		return 0, fmt.Errorf("insert server credential: %w", err)
	}
	if err = r.inject("credential_insert"); err != nil {
		return 0, err
	}

	if err = insertServerAuditEvent(ctx, tx, actor, "enrolled", &serverID, intPtr(version), "success", ""); err != nil {
		return 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit server enroll: %w", err)
	}
	return version, nil
}

// RotateCredential revokes the current active credential and inserts
// version+1 in one transaction, so no window exists where an agent is
// unauthenticatable or where two credentials are simultaneously active.
func (r *ServerRepository) RotateCredential(ctx context.Context, principal auth.Principal, serverID string, newDigest [32]byte) (int, error) {
	if r == nil || r.pool == nil || principal.Validate() != nil || !github.ValidOpaqueID(serverID) {
		return 0, ErrServerAuthorizationDenied
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, fmt.Errorf("begin server credential rotate: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if err = lockAdministratorSession(ctx, tx, principal); err != nil {
		return 0, err
	}
	actor := administratorActor(principal)

	if err = lockActiveServer(ctx, tx, serverID, principal.OwnerID); err != nil {
		return 0, err
	}
	var activeID int64
	var activeVersion int
	if err = tx.QueryRow(ctx, `SELECT id,version FROM server_credentials WHERE server_id=$1 AND status='active' FOR UPDATE`, serverID).Scan(&activeID, &activeVersion); err != nil {
		return 0, ErrServerAuthorizationDenied
	}
	if _, err = tx.Exec(ctx, `UPDATE server_credentials SET status='revoked',revoked_at=clock_timestamp() WHERE id=$1`, activeID); err != nil {
		return 0, fmt.Errorf("revoke server credential: %w", err)
	}
	if err = r.inject("credential_revoke"); err != nil {
		return 0, err
	}
	newVersion := activeVersion + 1
	if _, err = tx.Exec(ctx, `INSERT INTO server_credentials(server_id,version,digest,status) VALUES($1,$2,$3,'active')`, serverID, newVersion, newDigest[:]); err != nil {
		if isUniqueViolation(err) {
			return 0, ErrServerAuthorizationDenied
		}
		return 0, fmt.Errorf("insert rotated server credential: %w", err)
	}
	if err = r.inject("credential_rotate_insert"); err != nil {
		return 0, err
	}
	if err = insertServerAuditEvent(ctx, tx, actor, "credential_rotated", &serverID, intPtr(newVersion), "success", ""); err != nil {
		return 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit server credential rotate: %w", err)
	}
	return newVersion, nil
}

// RevokeServer revokes the server and its active credential (if any) in one
// transaction. Revocation stops all further mutation for this server
// immediately: Authenticate always fails afterward.
func (r *ServerRepository) RevokeServer(ctx context.Context, principal auth.Principal, serverID string, reason string) error {
	if r == nil || r.pool == nil || principal.Validate() != nil || !github.ValidOpaqueID(serverID) || !validReason(reason) {
		return ErrServerAuthorizationDenied
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin server revoke: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if err = lockAdministratorSession(ctx, tx, principal); err != nil {
		return err
	}
	actor := administratorActor(principal)

	if err = lockActiveServer(ctx, tx, serverID, principal.OwnerID); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE servers SET status='revoked',revoked_at=clock_timestamp() WHERE id=$1 AND owner_id=$2 AND status='active'`, serverID, principal.OwnerID)
	if err != nil {
		return fmt.Errorf("revoke server: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrServerAuthorizationDenied
	}
	if err = r.inject("server_revoke"); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE server_credentials SET status='revoked',revoked_at=clock_timestamp() WHERE server_id=$1 AND status='active'`, serverID); err != nil {
		return fmt.Errorf("revoke server credential: %w", err)
	}
	if err = r.inject("credential_revoke_on_server_revoke"); err != nil {
		return err
	}

	// ADR 0004 revocation paragraph: revocation (unlike rotation, Decision 3)
	// must not leave a leased attempt believing it still holds authority.
	// Lock order continues the hierarchy already begun by lockActiveServer
	// above: servers row first, then environment slots/jobs/attempts.
	var revokedAt time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&revokedAt); err != nil {
		return fmt.Errorf("server revoke canonical time: %w", err)
	}
	if err = revokeInFlightLeases(ctx, tx, NewDeploymentRepository(r.pool), r.inject, serverID, revokedAt); err != nil {
		return err
	}
	if err = r.inject("revoke_in_flight_leases"); err != nil {
		return err
	}

	if err = insertServerAuditEvent(ctx, tx, actor, "server_revoked", &serverID, nil, "success", reason); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit server revoke: %w", err)
	}
	return nil
}

// AuthenticatedServer is the identity Authenticate returns on success.
type AuthenticatedServer struct {
	ServerID, OwnerID string
	CredentialVersion int
}

// Authenticate verifies a presented credential digest against the server's
// currently active credential using a constant-time comparison. Any
// failure -- unknown server, revoked server, revoked/missing credential, or
// digest mismatch -- returns the identical generic error. It holds FOR
// SHARE on both the server and its active credential row until commit.
//
// A single query LEFT-joining servers to their active credential cannot
// carry that locking guarantee here: PostgreSQL rejects FOR SHARE/FOR UPDATE
// on the nullable side of an outer join (there is no row to lock when a
// server has no active credential), so the server and credential lookups
// stay two separate FOR SHARE queries. To close the timing gap that would
// otherwise exist between "no such row" and "row found, digest mismatch",
// the digest comparison always runs -- against a fixed dummy digest when
// either lookup misses -- instead of returning as soon as a lookup fails.
// The remaining difference in round-trip count between an unknown server
// (one query) and a known server (two queries) is an accepted residual.
func (r *ServerRepository) Authenticate(ctx context.Context, serverID string, presentedDigest [32]byte) (AuthenticatedServer, error) {
	if r == nil || r.pool == nil || !github.ValidOpaqueID(serverID) {
		return AuthenticatedServer{}, ErrServerAuthorizationDenied
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return AuthenticatedServer{}, fmt.Errorf("begin server authenticate: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var ownerID, status string
	foundServer := true
	if err = tx.QueryRow(ctx, `SELECT owner_id,status FROM servers WHERE id=$1 FOR SHARE`, serverID).Scan(&ownerID, &status); err != nil || status != "active" {
		foundServer = false
	}
	var storedDigest []byte
	var version int
	foundCredential := false
	if foundServer {
		if err = tx.QueryRow(ctx, `SELECT digest,version FROM server_credentials WHERE server_id=$1 AND status='active' FOR SHARE`, serverID).Scan(&storedDigest, &version); err == nil {
			foundCredential = true
		}
	}
	var stored [32]byte
	if foundCredential {
		copy(stored[:], storedDigest)
	} else {
		stored = dummyCredentialDigest
	}
	match := server.ConstantTimeEqualDigest(stored, presentedDigest)
	if !foundServer || !foundCredential || !match {
		return AuthenticatedServer{}, ErrServerAuthorizationDenied
	}
	if err = tx.Commit(ctx); err != nil {
		return AuthenticatedServer{}, fmt.Errorf("commit server authenticate: %w", err)
	}
	return AuthenticatedServer{ServerID: serverID, OwnerID: ownerID, CredentialVersion: version}, nil
}

// lockActiveServer locks the servers row scoped to ownerID (owner-of-lock-
// order first) and fails with the same generic error whether the server
// does not exist, is not active, or belongs to a different owner -- an
// administrator can never distinguish "unknown server" from "owner B's
// server" this way.
func lockActiveServer(ctx context.Context, tx pgx.Tx, serverID, ownerID string) error {
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM servers WHERE id=$1 AND owner_id=$2 FOR UPDATE`, serverID, ownerID).Scan(&status); err != nil || status != "active" {
		return ErrServerAuthorizationDenied
	}
	return nil
}

func insertServerAuditEvent(ctx context.Context, tx pgx.Tx, actor AuditActor, action string, serverID *string, credentialVersion *int, result, reason string) error {
	if _, err := tx.Exec(ctx, `INSERT INTO server_audit_events(actor_type,actor_id,action,server_id,credential_version,result,reason) VALUES($1,$2,$3,$4,$5,$6,$7)`, string(actor.Type), actor.ID, action, serverID, credentialVersion, result, reason); err != nil {
		return fmt.Errorf("insert server audit event: %w", err)
	}
	return nil
}

func intPtr(v int) *int { return &v }
