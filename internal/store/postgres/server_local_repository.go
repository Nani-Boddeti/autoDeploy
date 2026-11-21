package postgres

import (
	"context"
	"errors"
	"fmt"

	"autodeploy/internal/github"
	"autodeploy/internal/server"

	"github.com/jackc/pgx/v5"
)

var (
	// ErrNoAdministrator reports that first-run setup has not created the owner yet.
	ErrNoAdministrator = errors.New("no administrator exists yet")
	// ErrOwnerInactive reports an owner whose administrator user is disabled.
	ErrOwnerInactive = errors.New("the administrator account is not active")
	// ErrServerNeedsReenroll reports a server whose enrollment token was consumed
	// but which has no active credential (enrolled once, credential since revoked).
	ErrServerNeedsReenroll = errors.New("server needs re-enrollment")
)

// LocalEnrollAuditReason marks the "register" audit event written by the
// host-trusted local path. It is the only difference from a UI registration
// row, and it is what lets the local path recognise (and only adopt) servers it
// registered itself.
const LocalEnrollAuditReason = "local-enroll"

// localEnrollLockKey is the pg_advisory_xact_lock key serializing every local
// enrollment ("ADENLOCA" in ASCII). Other advisory locks already in production
// code: the fixed keys 7314928456316941{22,23,24}, the ASCII keys "agnt_lp3",
// "agnt_ls2" and "agnt_rt3", and the hashtextextended-derived keys in
// manual_deploy_repository.go. This key does not equal any of the fixed or
// ASCII keys; a collision with a hash-derived key is possible only by
// astronomically unlikely chance and would merely serialize two unrelated
// short transactions.
const localEnrollLockKey int64 = 0x4144454e4c4f4341

// LocalOwner is the sole owner and its single 'owner'-role user (users.role is
// CHECK-constrained to 'owner'). The host-trusted local-enrollment path has no
// browser session, so it attributes audit rows to this user exactly as the UI
// attributes them to the signed-in administrator (actor_type 'administrator',
// actor_id = users.id).
type LocalOwner struct{ OwnerID, UserID string }

// LocalEnrollment is the outcome of EnrollLocalServer. Enrolled means the
// server already has an active credential and nothing was minted.
type LocalEnrollment struct {
	ServerID string
	Enrolled bool
}

func (r *ServerRepository) localOwner(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) (LocalOwner, error) {
	var ownerID *string
	if err := q.QueryRow(ctx, `SELECT owner_id FROM installation_auth WHERE id=1`).Scan(&ownerID); err != nil {
		return LocalOwner{}, fmt.Errorf("load local owner: %w", err)
	}
	if ownerID == nil {
		return LocalOwner{}, ErrNoAdministrator
	}
	o := LocalOwner{OwnerID: *ownerID}
	err := q.QueryRow(ctx, `SELECT id FROM users WHERE owner_id=$1 AND role='owner' AND status='active' ORDER BY created_at,id LIMIT 1`, o.OwnerID).Scan(&o.UserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return LocalOwner{}, ErrOwnerInactive
	}
	if err != nil {
		return LocalOwner{}, fmt.Errorf("load local owner: %w", err)
	}
	return o, nil
}

// LocalOwner returns the owner once an active administrator exists;
// ErrNoAdministrator before setup, ErrOwnerInactive when it is disabled.
func (r *ServerRepository) LocalOwner(ctx context.Context) (LocalOwner, error) {
	if r == nil || r.pool == nil {
		return LocalOwner{}, ErrServerAuthorizationDenied
	}
	return r.localOwner(ctx, r.pool)
}

// EnrollLocalServer is the host-trusted counterpart of RegisterServer +
// IssueEnrollment, as ONE transaction under pg_advisory_xact_lock so that
// concurrent callers serialize (exactly one server; the last committed digest
// wins). It finds the server to enroll, then registers or reissues:
//   - recordedID (agent-local.env) is authoritative: it must be an active server
//     of this owner (ErrServerAuthorizationDenied otherwise). An active
//     credential -> Enrolled, nothing minted. A consumed token without an
//     active credential -> ErrServerNeedsReenroll. Else the token is reissued.
//   - with no recorded id, only a server THIS path registered (audit
//     reason LocalEnrollAuditReason) with exactly this name, active, never
//     enrolled and with an unconsumed token is reused (crash recovery). A
//     UI-registered server is never adopted, whatever its name.
//   - otherwise newID is registered with the marker.
//
// Owner and audit actor come from the database (see LocalOwner).
func (r *ServerRepository) EnrollLocalServer(ctx context.Context, recordedID, name, newID string, digest [32]byte, ttlSeconds int) (LocalEnrollment, error) {
	if r == nil || r.pool == nil || (recordedID != "" && !github.ValidOpaqueID(recordedID)) || ttlSeconds <= 0 || ttlSeconds > 3600 {
		return LocalEnrollment{}, ErrServerAuthorizationDenied
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return LocalEnrollment{}, fmt.Errorf("begin local enrollment: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, localEnrollLockKey); err != nil {
		return LocalEnrollment{}, fmt.Errorf("lock local enrollment: %w", err)
	}
	owner, err := r.localOwner(ctx, tx)
	if err != nil {
		return LocalEnrollment{}, err
	}
	actor := AuditActor{Type: server.ActorAdministrator, ID: owner.UserID}

	const enrolled = `EXISTS(SELECT 1 FROM server_credentials c WHERE c.server_id=s.id AND c.status='active')`
	var existingID string
	if recordedID != "" {
		var hasCredential, consumed bool
		err = tx.QueryRow(ctx, `SELECT s.id,`+enrolled+`,coalesce(t.consumed_at IS NOT NULL,false) FROM servers s LEFT JOIN server_enrollment_tokens t ON t.server_id=s.id AND t.purpose='enroll' WHERE s.id=$1 AND s.owner_id=$2 AND s.status='active'`, recordedID, owner.OwnerID).Scan(&existingID, &hasCredential, &consumed)
		if errors.Is(err, pgx.ErrNoRows) {
			return LocalEnrollment{}, ErrServerAuthorizationDenied
		}
		if err != nil {
			return LocalEnrollment{}, fmt.Errorf("find local server: %w", err)
		}
		if hasCredential {
			return LocalEnrollment{ServerID: existingID, Enrolled: true}, nil
		}
		if consumed {
			return LocalEnrollment{}, ErrServerNeedsReenroll
		}
	} else {
		err = tx.QueryRow(ctx, `SELECT s.id FROM servers s JOIN server_enrollment_tokens t ON t.server_id=s.id AND t.purpose='enroll' WHERE s.owner_id=$1 AND s.name=$2 AND s.status='active' AND t.consumed_at IS NULL AND NOT `+enrolled+` AND EXISTS(SELECT 1 FROM server_audit_events e WHERE e.server_id=s.id AND e.action='register' AND e.reason=$3) ORDER BY s.created_at DESC,s.id LIMIT 1`, owner.OwnerID, name, LocalEnrollAuditReason).Scan(&existingID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return LocalEnrollment{}, fmt.Errorf("find local server: %w", err)
		}
	}

	result := LocalEnrollment{ServerID: existingID}
	if existingID != "" {
		if _, err = r.issueEnrollmentTx(ctx, tx, owner.OwnerID, actor, existingID, digest, ttlSeconds); err != nil {
			return LocalEnrollment{}, err
		}
	} else {
		in := RegisterServerInput{ServerID: newID, Name: name, ProtocolVersion: server.ProtocolVersion, EnrollmentDigest: digest, EnrollmentTTLSeconds: ttlSeconds}
		if !in.valid(owner.OwnerID) {
			return LocalEnrollment{}, ErrServerAuthorizationDenied
		}
		registered, _, regErr := r.registerServerTx(ctx, tx, owner.OwnerID, actor, LocalEnrollAuditReason, in)
		if regErr != nil {
			return LocalEnrollment{}, regErr
		}
		result.ServerID = registered.ID
	}
	if err = tx.Commit(ctx); err != nil {
		return LocalEnrollment{}, fmt.Errorf("commit local enrollment: %w", err)
	}
	return result, nil
}
