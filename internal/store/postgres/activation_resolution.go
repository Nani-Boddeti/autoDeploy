package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"autodeploy/internal/auth"
	"autodeploy/internal/deployment"
	"autodeploy/internal/github"

	"github.com/jackc/pgx/v5"
)

// Step 10c-2b step G1: the operator's DECLARED resolution of a blocked
// environment. The operator states what really happened to the activation
// (activated / not_activated) or that no activation was involved
// (no_activation, today's MarkReconciled behaviour). There is no default: the
// declaration is the only thing that may clear an activation-caused block.
const (
	ActivationOutcomeActivated    = "activated"
	ActivationOutcomeNotActivated = "not_activated"
	ActivationOutcomeNoActivation = "no_activation"
)

// ErrAgentLeaseOutcomeMismatch reports a declared outcome that contradicts the
// kind of block: an activation-caused block (an unresolved activate operation
// exists for the slot's pending attempt) declared no_activation, or a block
// no activation caused declared activated / not_activated. Non-retryable.
var ErrAgentLeaseOutcomeMismatch = errors.New("agent lease declared outcome does not match the kind of block")

func validActivationOutcome(outcome string) bool {
	switch outcome {
	case ActivationOutcomeActivated, ActivationOutcomeNotActivated, ActivationOutcomeNoActivation:
		return true
	}
	return false
}

// clearPlainReconciliation is the tail MarkReconciled and ResolveActivation
// (no_activation) share: clear the slot flag, write the audit row and the
// 'reconciled' event. The caller holds the admin session, slot and job locks
// and commits. reason is stored verbatim in the audit row.
func (r *AgentLeaseRepository) clearPlainReconciliation(ctx context.Context, tx pgx.Tx, principal auth.Principal, environmentID string, jobID int64, attemptID *int64, reason string) error {
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return fmt.Errorf("environment mark-reconciled canonical time: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE environment_slots SET reconciliation_required=false, reconciliation_attempt_id=NULL, updated_at=$2 WHERE environment_id=$1`, environmentID, now); err != nil {
		return fmt.Errorf("clear environment slot reconciliation: %w", err)
	}
	if err := r.inject("slot_reconcile_clear"); err != nil {
		return err
	}

	actor := administratorActor(principal)
	if err := insertServerAuditEvent(ctx, tx, actor, "environment_mark_reconciled", nil, nil, "success", reason); err != nil {
		return err
	}
	if err := r.inject("audit_insert"); err != nil {
		return err
	}

	if err := appendAgentJobEvent(ctx, tx, jobID, attemptID, "reconciled"); err != nil {
		return err
	}
	return r.inject("job_event")
}

// declaredSummary appends " | declared <outcome>" to the operation's existing
// evidence. Evidence is NEVER replaced or cut: when the marker does not fit in
// the 2048-byte column the summary is returned unchanged (the audit row still
// carries the declaration and the operator's reason). The marker is ASCII, so
// no rune can be split.
func declaredSummary(existing, outcome string) string {
	marker := lateReportSeparator + "declared " + outcome
	if existing == "" {
		marker = "declared " + outcome
	}
	if len(existing)+len(marker) > maxOutcomeSummaryBytes {
		return existing
	}
	return existing + marker
}

// resolvedActivationOperation is the operation row a resolution acts on.
type resolvedActivationOperation struct {
	id, attemptID, fence    int64
	releaseID, state        string
	routeDigest             []byte
	outcomeSummary          string
	expectedRouteRevision   int64
	expectedActiveReleaseID *string
}

// ResolveActivation is the operator path (administrator session, owner
// scoped, NO server credential: a revoked server can never authenticate, so
// for a revoke-diverted block this is the only way out) that clears a blocked
// environment by a declared outcome. MarkReconciled is unchanged and keeps
// refusing activation-caused blocks.
//
// Lock order (ADR 0004 + 10c-2a amendment): admin session/user -> environment
// slot -> agent job -> attempt -> release operation -> deployment -> route head
// -> releases ascending (activated only) -> writes. The clock is read once,
// AFTER every row lock is held, so the bare `now` in slot/job/release/head
// updated_at cannot trail a concurrent writer. job_id and owner_id of an
// attempt are immutable, so the non-locking peek that finds the job is safe.
//
// Rule (both directions, under the locks): the block is activation-caused iff
// an unresolved activate operation exists for the slot's pending attempt.
// Every failure rolls back and writes NOTHING, including
// ErrAgentLeaseActivationRouteMoved: the environment is already blocked, so
// no new block is recorded (never blockAfterFinalizeFailure).
//
// Idempotency: once resolved the slot no longer names the attempt, so a
// repeat is answered by confirmResolvedOutcome.
func (r *AgentLeaseRepository) ResolveActivation(ctx context.Context, principal auth.Principal, environmentID, outcome, reason string) error {
	if r == nil || r.pool == nil || principal.Validate() != nil || !github.ValidOpaqueID(environmentID) {
		return ErrServerAuthorizationDenied
	}
	if !validActivationOutcome(outcome) {
		return ErrAgentLeaseInvalidInput
	}
	// The audit table has no outcome column (and a CHECK on the action), so the
	// outcome is encoded in the reason; the 512-byte bound applies to the whole.
	auditReason := "outcome=" + outcome + "; " + reason
	if !validReason(auditReason) {
		return ErrServerAuthorizationDenied
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin activation resolution: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if err = lockAdministratorSession(ctx, tx, principal); err != nil {
		return err
	}

	var pendingAttemptID, slotCurrentAttemptID *int64
	if err = tx.QueryRow(ctx, `SELECT reconciliation_attempt_id, current_attempt_id FROM environment_slots WHERE environment_id=$1 FOR UPDATE`, environmentID).Scan(&pendingAttemptID, &slotCurrentAttemptID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrServerAuthorizationDenied
		}
		return fmt.Errorf("lock environment slot for activation resolution: %w", err)
	}
	if pendingAttemptID == nil {
		if err = confirmResolvedOutcome(ctx, tx, principal, environmentID, outcome); err != nil {
			return err
		}
		if err = tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit activation resolution replay: %w", err)
		}
		return nil
	}
	attemptID := *pendingAttemptID

	// Owner check on the immutable job_id/owner_id BEFORE any further lock, so
	// a foreign owner gets the generic denial and never queues on the job.
	var jobID int64
	var jobOwnerID string
	if err = tx.QueryRow(ctx, `SELECT a.job_id, j.owner_id FROM agent_job_attempts a JOIN agent_jobs j ON j.id = a.job_id WHERE a.id=$1`, attemptID).Scan(&jobID, &jobOwnerID); err != nil {
		return fmt.Errorf("locate agent lease reconciliation attempt job: %w", err)
	}
	if jobOwnerID != principal.OwnerID {
		return ErrServerAuthorizationDenied
	}

	var jobState, deploymentID string
	if err = tx.QueryRow(ctx, `SELECT state, deployment_id FROM agent_jobs WHERE id=$1 FOR UPDATE`, jobID).Scan(&jobState, &deploymentID); err != nil {
		return fmt.Errorf("lock agent job for activation resolution: %w", err)
	}
	var attemptState, attemptEnvironmentID string
	var attemptFence int64
	var attemptClosedAt *time.Time
	if err = tx.QueryRow(ctx, `SELECT state, environment_id, fence, closed_at FROM agent_job_attempts WHERE id=$1 FOR UPDATE`, attemptID).Scan(&attemptState, &attemptEnvironmentID, &attemptFence, &attemptClosedAt); err != nil {
		return fmt.Errorf("lock agent job attempt for activation resolution: %w", err)
	}

	// The unresolved activate operation of the ENVIRONMENT (at most one: the
	// 000021 unique index), not of the attempt, so an operation that belongs to
	// another attempt than the flag is seen and refused rather than ignored.
	var op resolvedActivationOperation
	var hasOp bool
	err = tx.QueryRow(ctx, `SELECT id, attempt_id, fence, release_id, state, route_digest, outcome_summary, expected_route_revision, expected_active_release_id FROM release_operations WHERE environment_id=$1 AND `+unresolvedActivationSQL+` FOR UPDATE`, environmentID).
		Scan(&op.id, &op.attemptID, &op.fence, &op.releaseID, &op.state, &op.routeDigest, &op.outcomeSummary, &op.expectedRouteRevision, &op.expectedActiveReleaseID)
	switch {
	case err == nil:
		hasOp = true
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return fmt.Errorf("lock unresolved activation operation for resolution: %w", err)
	}
	if hasOp && op.attemptID != attemptID {
		return fmt.Errorf("%w: environment %s is flagged by attempt %d but its unresolved activation belongs to attempt %d", ErrAgentLeaseCorruptState, environmentID, attemptID, op.attemptID)
	}

	if !hasOp {
		if outcome != ActivationOutcomeNoActivation {
			return ErrAgentLeaseOutcomeMismatch
		}
		// A pending attempt that is still the leased occupant, with no
		// unresolved operation, is not a block any close produced. Clearing
		// the flag would leave a leased attempt holding the slot, invisible to
		// the sweeper and the list (the invisible wedge). MarkReconciled is
		// unchanged; this guard is specific to the declared path.
		if attemptState == "leased" && attemptClosedAt == nil {
			return fmt.Errorf("%w: attempt %d is still leased with no unresolved activation at environment %s", ErrAgentLeaseCorruptState, attemptID, environmentID)
		}
		if err = r.clearPlainReconciliation(ctx, tx, principal, environmentID, jobID, pendingAttemptID, auditReason); err != nil {
			return err
		}
		if err = tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit activation resolution: %w", err)
		}
		return nil
	}
	if outcome == ActivationOutcomeNoActivation {
		return ErrAgentLeaseOutcomeMismatch
	}
	// A switch_authorized operation is NOT resolvable: the host switch may
	// still be in flight. The sweeper or tx2 must first move it to
	// reconciliation_required.
	if op.state != "reconciliation_required" {
		return ErrAgentLeaseActivationUnresolved
	}
	// The shape every activation block leaves. Anything else is not a state
	// this resolution may act on (the helper's own writes would also refuse it).
	switch {
	case attemptState != "leased" || attemptClosedAt != nil:
		return fmt.Errorf("%w: attempt %d is %q (closed_at %v) at activation resolution", ErrAgentLeaseCorruptState, attemptID, attemptState, attemptClosedAt)
	case attemptEnvironmentID != environmentID || attemptFence != op.fence:
		return fmt.Errorf("%w: attempt %d does not match environment %s / operation %d fence", ErrAgentLeaseCorruptState, attemptID, environmentID, op.id)
	case jobState != "leased":
		return fmt.Errorf("%w: job %d is %q at activation resolution", ErrAgentLeaseCorruptState, jobID, jobState)
	case slotCurrentAttemptID == nil || *slotCurrentAttemptID != attemptID:
		return fmt.Errorf("%w: slot %s is not held by attempt %d at activation resolution", ErrAgentLeaseCorruptState, environmentID, attemptID)
	}
	if len(op.routeDigest) != 32 {
		return fmt.Errorf("%w: activation operation %d has a malformed route digest", ErrAgentLeaseCorruptState, op.id)
	}

	// Deployment (6) -> route head (6b) -> releases ascending (activated only),
	// all locked BEFORE the clock is read. The helper re-locks them, which is a
	// no-op for this transaction.
	var deploymentStatus string
	if err = tx.QueryRow(ctx, `SELECT status FROM deployments WHERE id=$1 FOR UPDATE`, deploymentID).Scan(&deploymentStatus); err != nil {
		return fmt.Errorf("lock deployment for activation resolution: %w", err)
	}
	if deploymentStatus != string(deployment.StatusActivating) {
		return fmt.Errorf("%w: deployment %s is %q at activation resolution", ErrAgentLeaseCorruptState, deploymentID, deploymentStatus)
	}
	headExists, headFlagged := true, false
	if err = tx.QueryRow(ctx, `SELECT reconciliation_required FROM route_heads WHERE environment_id=$1 FOR UPDATE`, environmentID).Scan(&headFlagged); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("lock route head for activation resolution: %w", err)
		}
		headExists = false
	}
	if outcome == ActivationOutcomeActivated {
		lockIDs := []string{op.releaseID}
		if op.expectedActiveReleaseID != nil {
			lockIDs = append(lockIDs, *op.expectedActiveReleaseID)
		}
		if _, err = tx.Exec(ctx, `SELECT 1 FROM releases WHERE id = ANY($1) ORDER BY id FOR UPDATE`, lockIDs); err != nil {
			return fmt.Errorf("lock releases for activation resolution: %w", err)
		}
	}
	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return fmt.Errorf("activation resolution canonical time: %w", err)
	}

	if outcome == ActivationOutcomeActivated {
		err = r.finalizeActivationSucceeded(ctx, tx, activationFinalizeInput{
			jobID:                   jobID,
			attemptID:               attemptID,
			operationID:             op.id,
			environmentID:           environmentID,
			deploymentID:            deploymentID,
			candidateReleaseID:      op.releaseID,
			expectedRouteRevision:   op.expectedRouteRevision,
			expectedActiveReleaseID: op.expectedActiveReleaseID,
			resultDigest:            [32]byte(op.routeDigest),
			summary:                 declaredSummary(op.outcomeSummary, outcome),
			now:                     now,
			fromState:               "reconciliation_required",
		})
	} else {
		err = r.resolveNotActivated(ctx, tx, jobID, attemptID, op.id, environmentID, deploymentID, headExists && headFlagged, declaredSummary(op.outcomeSummary, outcome), now)
	}
	if err != nil {
		return err
	}

	if err = insertServerAuditEvent(ctx, tx, administratorActor(principal), "environment_mark_reconciled", nil, nil, "success", auditReason); err != nil {
		return err
	}
	if err = r.inject("audit_insert"); err != nil {
		return err
	}
	if err = appendAgentJobEvent(ctx, tx, jobID, &attemptID, "reconciled"); err != nil {
		return err
	}
	if err = r.inject("job_event"); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit activation resolution: %w", err)
	}
	return nil
}

// resolveNotActivated is the write sequence for a declared not_activated, in
// lock order, every statement conditional (RowsAffected==1, else corrupt
// state). The caller holds every lock and has verified the shape. Releases are
// untouched (the candidate stays ready, as after Complete(failed)); the head
// loses only its flag (hostname, revision and active release are unchanged).
// Legality (live triggers): validate_agent_job_attempt_update allows
// leased -> expired, agent_job_attempts_check3 requires NULL failure_class and
// result for expired; validate_agent_job_update allows leased -> failed;
// validate_release_operation_update allows reconciliation_required -> failed
// with closed_at unchanged (COALESCE keeps it).
func (r *AgentLeaseRepository) resolveNotActivated(ctx context.Context, tx pgx.Tx, jobID, attemptID, operationID int64, environmentID, deploymentID string, headFlagged bool, summary string, now time.Time) error {
	if err := transitionDeployment(ctx, tx, r.deployments, deploymentID, deployment.StatusFailed, now, r.inject); err != nil {
		return err
	}
	if err := r.inject("deployment"); err != nil {
		return err
	}
	if headFlagged {
		tag, err := tx.Exec(ctx, `UPDATE route_heads SET reconciliation_required=false, updated_at=GREATEST(updated_at, $2) WHERE environment_id=$1 AND reconciliation_required`, environmentID, now)
		if err != nil {
			return fmt.Errorf("clear route head reconciliation flag: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("%w: route head for environment %s is not flagged at activation resolution", ErrAgentLeaseCorruptState, environmentID)
		}
		if err = r.inject("route_head_flag"); err != nil {
			return err
		}
	}
	tag, err := tx.Exec(ctx, `UPDATE release_operations SET state='failed', closed_at=COALESCE(closed_at, $2), outcome_summary=$3 WHERE id=$1 AND state='reconciliation_required'`, operationID, now, summary)
	if err != nil {
		return fmt.Errorf("close activation operation as failed: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: operation %d is not reconciliation_required at activation resolution", ErrAgentLeaseCorruptState, operationID)
	}
	if err = r.inject("operation_close"); err != nil {
		return err
	}
	tag, err = tx.Exec(ctx, `UPDATE agent_job_attempts SET state='expired', closed_at=$2 WHERE id=$1 AND state='leased'`, attemptID, now)
	if err != nil {
		return fmt.Errorf("close agent job attempt: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: attempt %d is not leased at activation resolution", ErrAgentLeaseCorruptState, attemptID)
	}
	if err = r.inject("attempt_close"); err != nil {
		return err
	}
	// Free the slot and clear the flag pair in ONE statement (the slot CHECK
	// ties the flag to its attempt id), only if this attempt owns both. The
	// fence is not advanced: the next Claim does, and the closed attempt
	// cannot act.
	tag, err = tx.Exec(ctx, `UPDATE environment_slots SET current_attempt_id=NULL, reconciliation_required=false, reconciliation_attempt_id=NULL, updated_at=$2 WHERE environment_id=$1 AND current_attempt_id=$3 AND reconciliation_attempt_id=$3`, environmentID, now, attemptID)
	if err != nil {
		return fmt.Errorf("free environment slot: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: slot %s is not held and flagged by attempt %d at activation resolution", ErrAgentLeaseCorruptState, environmentID, attemptID)
	}
	if err = r.inject("slot_free"); err != nil {
		return err
	}
	tag, err = tx.Exec(ctx, `UPDATE agent_jobs SET state='failed', updated_at=$2 WHERE id=$1 AND state='leased'`, jobID, now)
	if err != nil {
		return fmt.Errorf("close agent job: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: job %d is not leased at activation resolution", ErrAgentLeaseCorruptState, jobID)
	}
	if err = r.inject("job_close"); err != nil {
		return err
	}
	if err = appendAgentJobEvent(ctx, tx, jobID, &attemptID, "activation_not_activated"); err != nil {
		return err
	}
	return appendAgentJobEvent(ctx, tx, jobID, &attemptID, "failed")
}

// confirmResolvedOutcome answers a repeat call (the slot no longer names a
// pending attempt) without writing. A repeat is honoured ONLY when the
// environment's most recent attempt (highest id; attempt ids are a global
// identity sequence, and the slot lock is held) is one a resolution already
// reconciled: then a duplicate submit for that resolution is judged against
// it. Anything else is "nothing to reconcile" (the generic denial), so an old
// resolution in a healthy environment's history never reports success for a
// call that did nothing, and a duplicate for attempt A after attempt B was
// resolved is not judged against B. No event ordering is needed:
// agent_job_events has only a per-job sequence and an occurred_at that is not
// a total order across jobs. The attempt's activate operation, if any, must
// agree with the declared outcome (succeeded<->activated,
// failed<->not_activated, none<->no_activation); disagreement is an integrity
// conflict.
func confirmResolvedOutcome(ctx context.Context, tx pgx.Tx, principal auth.Principal, environmentID, outcome string) error {
	var attemptID int64
	if err := tx.QueryRow(ctx, `SELECT a.id FROM agent_job_attempts a JOIN agent_jobs j ON j.id=a.job_id WHERE a.id=(SELECT max(id) FROM agent_job_attempts WHERE environment_id=$1) AND j.owner_id=$2 AND EXISTS (SELECT 1 FROM agent_job_events e WHERE e.attempt_id=a.id AND e.event_type='reconciled')`, environmentID, principal.OwnerID).Scan(&attemptID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrServerAuthorizationDenied
		}
		return fmt.Errorf("find prior activation resolution: %w", err)
	}
	var opState string
	want := ActivationOutcomeNoActivation
	err := tx.QueryRow(ctx, `SELECT state FROM release_operations WHERE attempt_id=$1 AND kind='activate' ORDER BY id DESC LIMIT 1`, attemptID).Scan(&opState)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return fmt.Errorf("read resolved activation operation: %w", err)
	case opState == activationStateSucceeded:
		want = ActivationOutcomeActivated
	case opState == activationStateFailed:
		want = ActivationOutcomeNotActivated
	default:
		return fmt.Errorf("%w: attempt %d was reconciled but its activation is %q", ErrAgentLeaseCorruptState, attemptID, opState)
	}
	if outcome != want {
		return ErrAgentLeaseIntegrityConflict
	}
	return nil
}
