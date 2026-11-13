package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"autodeploy/internal/auth"
	"autodeploy/internal/deployment"
	"autodeploy/internal/github"
	"autodeploy/internal/queue"

	"github.com/jackc/pgx/v5"
)

// RollbackResult identifies the durable rollback deployment and job. Replayed is
// true when the nonce had already been committed.
type RollbackResult struct {
	DeploymentID deployment.ID
	JobID        int64
	Replayed     bool
}

// rollbackIDs mirrors manualIDs under its own domain so rollback, manual and
// push identities can never collide.
func rollbackIDs(ownerID, targetReleaseID, nonce string) (deployment.ID, string) {
	h := sha256.New()
	for _, value := range []string{"autodeploy:rollback:v1", ownerID, targetReleaseID, nonce} {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(value)))
		_, _ = h.Write(length[:])
		_, _ = h.Write([]byte(value))
	}
	seed := hex.EncodeToString(h.Sum(nil))
	return deployment.ID("rbd_" + seed[:40]), "rbj_" + seed[40:]
}

// rollbackTarget is everything one rollback needs, read from the target's own
// (non-rollback) deployment job and the live route head.
type rollbackTarget struct {
	releaseID, deploymentID                     string
	ownerID, projectID, environmentID, serverID string
	ref, commitSHA                              string
	repositoryID                                int64
	sourceReleaseID                             string
	winningAttemptID                            int64
}

// winningActivationAttempt returns the attempt whose container a release runs
// as: the newest SUCCEEDED activate operation of the release made by a
// non-rollback job. A rollback's own activate operation names the same release
// but restarts the original container, so it must not redefine the instance.
func winningActivationAttempt(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, releaseID string) (int64, bool, error) {
	var attemptID int64
	err := q.QueryRow(ctx, `SELECT ro.attempt_id FROM release_operations ro JOIN agent_jobs j ON j.id=ro.job_id AND j.rollback_target_release_id IS NULL WHERE ro.release_id=$1 AND ro.kind='activate' AND ro.state=$2 ORDER BY ro.id DESC LIMIT 1`, releaseID, activationStateSucceeded).Scan(&attemptID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("find winning activation attempt: %w", err)
	}
	return attemptID, true, nil
}

// frozenSecretRevoked reports whether any secret version frozen by attemptID has
// been revoked (owner decision D-R3).
func frozenSecretRevoked(ctx context.Context, tx pgx.Tx, attemptID int64) (bool, error) {
	var revoked bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM frozen_secret_versions f JOIN secret_versions sv ON sv.id=f.secret_version_id WHERE f.attempt_id=$1 AND sv.state='revoked')`, attemptID).Scan(&revoked); err != nil {
		return false, fmt.Errorf("check rollback secret versions: %w", err)
	}
	return revoked, nil
}

// evaluateRollback decides whether targetReleaseID may be rolled back to for
// ownerID. lock=true (the enqueue tx) takes FOR SHARE on the route head and the
// release rows, after the caller holds the environment slot; lock=false is the
// plain read behind the detail page. Every refusal is a typed error.
func evaluateRollback(ctx context.Context, tx pgx.Tx, ownerID, targetReleaseID string, lock bool) (rollbackTarget, error) {
	suffix := ""
	if lock {
		suffix = " FOR SHARE"
	}
	t := rollbackTarget{releaseID: targetReleaseID, ownerID: ownerID}
	err := tx.QueryRow(ctx, `SELECT r.deployment_id, j.project_id, j.environment_id, j.server_id, j.repository_id, j.full_ref, j.commit_sha FROM releases r JOIN agent_jobs j ON j.deployment_id=r.deployment_id AND j.job_kind='deployment_execution' WHERE r.id=$1 AND j.owner_id=$2`, targetReleaseID, ownerID).
		Scan(&t.deploymentID, &t.projectID, &t.environmentID, &t.serverID, &t.repositoryID, &t.ref, &t.commitSHA)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, ErrRollbackNotFound
	}
	if err != nil {
		return t, fmt.Errorf("read rollback target: %w", err)
	}

	// The slot (locked by the enqueue tx): a leased attempt is an in-flight
	// deployment, a reconciliation flag needs an operator first.
	var slotReconciling, slotOccupied bool
	if err = tx.QueryRow(ctx, `SELECT reconciliation_required, current_attempt_id IS NOT NULL FROM environment_slots WHERE environment_id=$1`, t.environmentID).Scan(&slotReconciling, &slotOccupied); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return t, ErrRollbackNotEligible
		}
		return t, fmt.Errorf("read rollback environment slot: %w", err)
	}
	if slotReconciling {
		return t, ErrRollbackNotEligible
	}
	if slotOccupied {
		return t, ErrRollbackInFlight
	}

	var active *string
	var headFlagged bool
	err = tx.QueryRow(ctx, `SELECT active_release_id, reconciliation_required FROM route_heads WHERE environment_id=$1`+suffix, t.environmentID).Scan(&active, &headFlagged)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, ErrRollbackNotEligible
	}
	if err != nil {
		return t, fmt.Errorf("read rollback route head: %w", err)
	}
	if active == nil || *active == "" || *active == targetReleaseID || headFlagged {
		return t, ErrRollbackNotEligible
	}
	t.sourceReleaseID = *active

	// Release rows ascending by id (ADR 0004 position 6b/7 rule).
	rows, err := tx.Query(ctx, `SELECT id, state, environment_id FROM releases WHERE id = ANY($1) ORDER BY id`+suffix, []string{targetReleaseID, t.sourceReleaseID})
	if err != nil {
		return t, fmt.Errorf("read rollback releases: %w", err)
	}
	type rel struct{ state, environmentID string }
	found := map[string]rel{}
	for rows.Next() {
		var id string
		var r rel
		if err = rows.Scan(&id, &r.state, &r.environmentID); err != nil {
			rows.Close()
			return t, fmt.Errorf("scan rollback release: %w", err)
		}
		found[id] = r
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return t, fmt.Errorf("read rollback releases: %w", err)
	}
	target, source := found[targetReleaseID], found[t.sourceReleaseID]
	if target.state != "retained" || source.state != "active" || target.environmentID != t.environmentID || source.environmentID != t.environmentID {
		return t, ErrRollbackNotEligible
	}
	var sourceServer string
	err = tx.QueryRow(ctx, `SELECT j.server_id FROM releases r JOIN agent_jobs j ON j.deployment_id=r.deployment_id AND j.job_kind='deployment_execution' WHERE r.id=$1`, t.sourceReleaseID).Scan(&sourceServer)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && sourceServer != t.serverID) {
		return t, ErrRollbackNotEligible
	}
	if err != nil {
		return t, fmt.Errorf("read rollback source server: %w", err)
	}

	attemptID, ok, err := winningActivationAttempt(ctx, tx, targetReleaseID)
	if err != nil {
		return t, err
	}
	if !ok {
		return t, ErrRollbackNotEligible
	}
	t.winningAttemptID = attemptID

	var hasConfig, inFlight bool
	revoked, err := frozenSecretRevoked(ctx, tx, attemptID)
	if err != nil {
		return t, err
	}
	if revoked {
		return t, ErrRollbackSecretRevoked
	}
	if err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM environment_runtime_configs WHERE environment_id=$1)`, t.environmentID).Scan(&hasConfig); err != nil {
		return t, fmt.Errorf("check rollback runtime config: %w", err)
	}
	if !hasConfig {
		return t, ErrRollbackNotEligible
	}
	activeStatuses := make([]string, 0, 8)
	for _, status := range deployment.ActiveStatuses() {
		activeStatuses = append(activeStatuses, string(status))
	}
	if err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM deployments d JOIN agent_jobs j ON j.deployment_id=d.id WHERE j.owner_id=$1 AND d.environment_id=$2 AND d.status = ANY($3))`, ownerID, t.environmentID, activeStatuses).Scan(&inFlight); err != nil {
		return t, fmt.Errorf("check rollback in-flight deployments: %w", err)
	}
	if inFlight {
		return t, ErrRollbackInFlight
	}
	return t, nil
}

// RollbackEligible is the plain, lock-free read behind the "Roll back to this
// release" form. It never authorizes anything: EnqueueRollback re-evaluates
// under locks. Unexpected database errors are returned; every typed refusal is
// simply false.
func (r *WebhookJobRepository) RollbackEligible(ctx context.Context, principal auth.Principal, targetReleaseID string) (bool, error) {
	if r == nil || r.pool == nil || principal.Validate() != nil || !github.ValidOpaqueID(targetReleaseID) {
		return false, nil
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return false, fmt.Errorf("begin rollback eligibility: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = requireLiveSessionRow(ctx, tx, principal); err != nil {
		return false, nil //nolint:nilerr // a dead session simply sees no form
	}
	_, err = evaluateRollback(ctx, tx, principal.OwnerID, targetReleaseID, false)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrRollbackNotFound), errors.Is(err, ErrRollbackNotEligible), errors.Is(err, ErrRollbackSecretRevoked), errors.Is(err, ErrRollbackInFlight):
		return false, nil
	default:
		return false, err
	}
}

// EnqueueRollback creates one rollback deployment and job returning the
// environment to the retained release targetReleaseID.
//
// Lock order (ADR 0004 amendment 2026-11-13, "Rollback"), one READ COMMITTED
// tx, no GitHub rows: administrator session + user (FOR UPDATE, position 1),
// environment slot (FOR UPDATE, position 3), then route head and the target
// and active release rows (FOR SHARE, 6b, releases ascending id), then the
// append-only inserts (8). The slot lock is what orders this against claim,
// activation, cancellation and a concurrent rollback of the same environment.
func (r *WebhookJobRepository) EnqueueRollback(ctx context.Context, principal auth.Principal, targetReleaseID, nonce string) (RollbackResult, error) {
	if r == nil || r.pool == nil || r.deployments == nil || principal.Validate() != nil {
		return RollbackResult{}, ErrRollbackUnauthorized
	}
	if !github.ValidOpaqueID(targetReleaseID) || !validManualNonce(nonce) {
		return RollbackResult{}, ErrRollbackInvalid
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return RollbackResult{}, fmt.Errorf("begin rollback: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if err = lockAdministratorSessionRow(ctx, tx, principal); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RollbackResult{}, ErrRollbackUnauthorized
		}
		return RollbackResult{}, fmt.Errorf("rollback session lock: %w", err)
	}
	// Non-locking peek: only to learn which slot to lock. Everything is
	// re-read under the locks by evaluateRollback.
	var environmentID string
	err = tx.QueryRow(ctx, `SELECT r.environment_id FROM releases r JOIN agent_jobs j ON j.deployment_id=r.deployment_id AND j.job_kind='deployment_execution' WHERE r.id=$1 AND j.owner_id=$2`, targetReleaseID, principal.OwnerID).Scan(&environmentID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Unknown or cross-owner: never distinguished.
		return RollbackResult{}, ErrRollbackNotFound
	}
	if err != nil {
		return RollbackResult{}, fmt.Errorf("rollback target peek: %w", err)
	}
	// Slot lock (position 3): orders this against claim, activation,
	// cancellation and a concurrent rollback of the same environment.
	if err = tx.QueryRow(ctx, `SELECT 1 FROM environment_slots WHERE environment_id=$1 FOR UPDATE`, environmentID).Scan(new(int)); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RollbackResult{}, ErrRollbackNotEligible
		}
		return RollbackResult{}, fmt.Errorf("lock rollback environment slot: %w", err)
	}
	if result, found, rerr := resultForRollbackNonce(ctx, tx, principal.OwnerID, nonce, targetReleaseID); rerr != nil || found {
		if rerr != nil {
			return RollbackResult{}, rerr
		}
		if err = tx.Commit(ctx); err != nil {
			return RollbackResult{}, fmt.Errorf("commit rollback replay: %w", err)
		}
		return result, nil
	}
	t, err := evaluateRollback(ctx, tx, principal.OwnerID, targetReleaseID, true)
	if err != nil {
		return RollbackResult{}, err
	}

	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return RollbackResult{}, fmt.Errorf("rollback canonical time: %w", err)
	}
	deploymentID, jobKey := rollbackIDs(principal.OwnerID, targetReleaseID, nonce)
	d, err := deployment.New(deployment.Identity{DeploymentID: deploymentID, ProjectID: t.projectID, EnvironmentID: t.environmentID, ServerID: t.serverID, CommitSHA: t.commitSHA}, now)
	if err != nil {
		return RollbackResult{}, fmt.Errorf("rollback build deployment: %w", err)
	}
	j, err := queue.NewRollbackExecutionJob(t.ownerID, t.projectID, t.environmentID, t.serverID, string(deploymentID), t.repositoryID, t.ref, t.commitSHA, targetReleaseID, jobKey, 0, now)
	if err != nil {
		return RollbackResult{}, fmt.Errorf("rollback build job: %w", err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO rollback_intents(owner_id,actor_user_id,source_release_id,target_release_id,request_nonce,deployment_id) VALUES($1,$2,$3,$4,$5,$6)`, principal.OwnerID, principal.UserID, t.sourceReleaseID, targetReleaseID, nonce, string(deploymentID)); err != nil {
		if isUniqueViolation(err) {
			return RollbackResult{}, ErrRollbackConflict
		}
		return RollbackResult{}, fmt.Errorf("insert rollback intent: %w", err)
	}
	if err = r.inject("rollback_intent"); err != nil {
		return RollbackResult{}, err
	}
	jobID, err := r.insertQueuedDeploymentTx(ctx, tx, d, j, ErrRollbackConflict)
	if err != nil {
		return RollbackResult{}, err
	}
	if err = r.inject("rollback_done"); err != nil {
		return RollbackResult{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return RollbackResult{}, fmt.Errorf("commit rollback: %w", err)
	}
	return RollbackResult{DeploymentID: deploymentID, JobID: jobID}, nil
}

// resultForRollbackNonce returns the committed result for (owner, nonce) when
// the stored target equals targetReleaseID, ErrRollbackConflict when it
// differs, and found=false when the nonce is unused.
func resultForRollbackNonce(ctx context.Context, tx pgx.Tx, ownerID, nonce, targetReleaseID string) (RollbackResult, bool, error) {
	var storedTarget, deploymentID string
	var jobID int64
	err := tx.QueryRow(ctx, `SELECT i.target_release_id,i.deployment_id,j.id FROM rollback_intents i JOIN agent_jobs j ON j.deployment_id=i.deployment_id AND j.job_kind='rollback_execution' WHERE i.owner_id=$1 AND i.request_nonce=$2`, ownerID, nonce).Scan(&storedTarget, &deploymentID, &jobID)
	if errors.Is(err, pgx.ErrNoRows) {
		return RollbackResult{}, false, nil
	}
	if err != nil {
		return RollbackResult{}, false, fmt.Errorf("resolve rollback intent: %w", err)
	}
	if storedTarget != targetReleaseID {
		return RollbackResult{}, false, ErrRollbackConflict
	}
	return RollbackResult{DeploymentID: deployment.ID(deploymentID), JobID: jobID, Replayed: true}, true, nil
}
