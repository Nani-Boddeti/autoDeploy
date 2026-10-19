package postgres

import (
	"bytes"
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

	if err = r.resolveDeclared(ctx, tx, declaredResolution{
		environmentID:        environmentID,
		jobID:                jobID,
		attemptID:            attemptID,
		slotCurrentAttemptID: slotCurrentAttemptID,
		outcome:              outcome,
		plainAttemptRefusal: func(att lockedAttempt) error {
			if att.state == "leased" && att.closedAt == nil {
				return fmt.Errorf("%w: attempt %d is still leased with no unresolved activation at environment %s", ErrAgentLeaseCorruptState, attemptID, environmentID)
			}
			return nil
		},
		clearPlain: func() error {
			return r.clearPlainReconciliation(ctx, tx, principal, environmentID, jobID, pendingAttemptID, auditReason)
		},
		audit: func() error {
			if err := insertServerAuditEvent(ctx, tx, administratorActor(principal), "environment_mark_reconciled", nil, nil, "success", auditReason); err != nil {
				return err
			}
			return r.inject("audit_insert")
		},
	}); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit activation resolution: %w", err)
	}
	return nil
}

// lockedAttempt is the attempt row a resolution holds locked.
type lockedAttempt struct {
	serverID, environmentID, state string
	fence                          int64
	closedAt                       *time.Time
}

// declaredResolution is what the two entry points (the operator's
// ResolveActivation and the agent's ReconcileActivation) hand to the ONE
// shared implementation. The caller has authenticated, locked the slot (and
// whatever precedes it) and resolved the pending attempt; resolveDeclared
// takes every lock from the job down and performs the resolution. The entry
// points differ only in these fields.
type declaredResolution struct {
	environmentID        string
	jobID, attemptID     int64
	slotCurrentAttemptID *int64
	outcome              string
	// authorize, when set, vets the locked attempt before anything else is
	// read (the agent's server / environment / fence match).
	authorize func(lockedAttempt) error
	// outcomeDigest, when set, must equal the operation's route digest for an
	// activated declaration (the agent's presented digest); mismatch is an
	// integrity conflict, nothing written.
	outcomeDigest *[32]byte
	// plainAttemptRefusal vets the attempt of a no_activation declaration
	// (no unresolved operation): nil, or the typed refusal for an attempt that
	// is still the leased occupant.
	plainAttemptRefusal func(lockedAttempt) error
	// clearPlain is the no_activation tail (flag clear + event; the operator
	// adds its audit row there).
	clearPlain func() error
	// audit, when set, runs after the resolution writes and before the
	// 'reconciled' event (operator only; the agent writes no audit row).
	audit func() error
}

// resolveDeclared is the shared post-slot-lock core: lock the job, attempt,
// operation, deployment, route head and releases in the documented order, read
// the clock, pre-verify under the locks, then run the activated /
// not_activated writes and append 'reconciled'. The caller commits. Every
// failure returns an error and the caller rolls back: no new block is ever
// recorded (never blockAfterFinalizeFailure).
func (r *AgentLeaseRepository) resolveDeclared(ctx context.Context, tx pgx.Tx, d declaredResolution) error {
	environmentID, attemptID, jobID, outcome := d.environmentID, d.attemptID, d.jobID, d.outcome
	var err error
	var jobState, deploymentID string
	if err = tx.QueryRow(ctx, `SELECT state, deployment_id FROM agent_jobs WHERE id=$1 FOR UPDATE`, jobID).Scan(&jobState, &deploymentID); err != nil {
		return fmt.Errorf("lock agent job for activation resolution: %w", err)
	}
	var att lockedAttempt
	if err = tx.QueryRow(ctx, `SELECT server_id, state, environment_id, fence, closed_at FROM agent_job_attempts WHERE id=$1 FOR UPDATE`, attemptID).Scan(&att.serverID, &att.state, &att.environmentID, &att.fence, &att.closedAt); err != nil {
		return fmt.Errorf("lock agent job attempt for activation resolution: %w", err)
	}
	if d.authorize != nil {
		if err = d.authorize(att); err != nil {
			return err
		}
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
		// the sweeper and the list (the invisible wedge). The entry point
		// decides the typed refusal.
		if err = d.plainAttemptRefusal(att); err != nil {
			return err
		}
		return d.clearPlain()
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
	case att.state != "leased" || att.closedAt != nil:
		return fmt.Errorf("%w: attempt %d is %q (closed_at %v) at activation resolution", ErrAgentLeaseCorruptState, attemptID, att.state, att.closedAt)
	case att.environmentID != environmentID || att.fence != op.fence:
		return fmt.Errorf("%w: attempt %d does not match environment %s / operation %d fence", ErrAgentLeaseCorruptState, attemptID, environmentID, op.id)
	case jobState != "leased":
		return fmt.Errorf("%w: job %d is %q at activation resolution", ErrAgentLeaseCorruptState, jobID, jobState)
	case d.slotCurrentAttemptID == nil || *d.slotCurrentAttemptID != attemptID:
		return fmt.Errorf("%w: slot %s is not held by attempt %d at activation resolution", ErrAgentLeaseCorruptState, environmentID, attemptID)
	}
	if len(op.routeDigest) != 32 {
		return fmt.Errorf("%w: activation operation %d has a malformed route digest", ErrAgentLeaseCorruptState, op.id)
	}
	// The agent proves it observed this very activation by presenting the
	// digest the operation authorized (owner decision Q2).
	if outcome == ActivationOutcomeActivated && d.outcomeDigest != nil && !bytes.Equal(d.outcomeDigest[:], op.routeDigest) {
		return ErrAgentLeaseIntegrityConflict
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

	if d.audit != nil {
		if err = d.audit(); err != nil {
			return err
		}
	}
	if err = appendAgentJobEvent(ctx, tx, jobID, &attemptID, "reconciled"); err != nil {
		return err
	}
	return r.inject("job_event")
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
	return agreeResolvedOutcome(ctx, tx, attemptID, outcome, nil)
}

// A plain no_activation never clears an attempt whose activate operation is
// terminal (so a lost-response retry cannot hit a spurious conflict): the only
// writers of a terminal operation (finalizeActivationSucceeded,
// resolveNotActivated) close the attempt and free the slot in the same
// transaction, and the sweeper / revoke close only leased attempts.
// agreeResolvedOutcome judges a repeat declaration against the attempt's own
// activate operation (succeeded<->activated, failed<->not_activated, none<->
// no_activation); disagreement is an integrity conflict. When digest is set
// (the agent's presented digest) an activated repeat must also present the
// digest the operation authorized. Read-only.
func agreeResolvedOutcome(ctx context.Context, tx pgx.Tx, attemptID int64, outcome string, digest *[32]byte) error {
	var opState string
	var routeDigest []byte
	want := ActivationOutcomeNoActivation
	err := tx.QueryRow(ctx, `SELECT state, route_digest FROM release_operations WHERE attempt_id=$1 AND kind='activate' ORDER BY id DESC LIMIT 1`, attemptID).Scan(&opState, &routeDigest)
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
	if want == ActivationOutcomeActivated && digest != nil && !bytes.Equal(digest[:], routeDigest) {
		return ErrAgentLeaseIntegrityConflict
	}
	return nil
}

// ReconcileActivation is the agent path of the declared resolution: the server
// that held the blocked attempt declares what really happened (activated /
// not_activated) or that no activation was involved (no_activation, the
// semantics of Reconcile plus the outcome-kind checks in both directions).
// Authority is deliberately NOT checked (it has usually lapsed); the attempt's
// server, environment and fence, the operation's fence, the slot's pending
// flag and the operation's exact state are. A revoked server fails the
// credential lock: the operator path (ResolveActivation) is its only way out.
//
// Lock order: server credential (SHARE) -> environment slot -> agent job ->
// attempt -> release operation -> deployment -> route head -> releases
// ascending (activated only) -> writes; the clock is read after every lock.
// No server audit row (Reconcile writes none); events as ResolveActivation,
// 'reconciled' last. Any failure rolls back and writes NOTHING, no new block.
//
// Idempotency is anchored to the GIVEN attempt: once resolved the slot no
// longer names it, so a repeat is honoured only when the attempt is closed,
// a 'reconciled' event exists for it and the declaration agrees with its own
// operation (for activated, the digest too). Disagreement is an integrity
// conflict, no event is the generic denial; a repeat never writes.
func (r *AgentLeaseRepository) ReconcileActivation(ctx context.Context, serverID string, presentedDigest [32]byte, environmentID string, attemptID, fence int64, outcome string, outcomeDigest [32]byte) error {
	if r == nil || r.pool == nil || !github.ValidOpaqueID(serverID) || !github.ValidOpaqueID(environmentID) || attemptID <= 0 || fence <= 0 || !validActivationOutcome(outcome) {
		return ErrAgentLeaseInvalidInput
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin agent lease activation reconcile: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err = lockActiveServerCredential(ctx, tx, serverID, presentedDigest); err != nil {
		return err
	}
	var pendingAttemptID, slotCurrentAttemptID *int64
	if err = tx.QueryRow(ctx, `SELECT reconciliation_attempt_id, current_attempt_id FROM environment_slots WHERE environment_id=$1 FOR UPDATE`, environmentID).Scan(&pendingAttemptID, &slotCurrentAttemptID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrServerAuthorizationDenied
		}
		return fmt.Errorf("lock environment slot for activation reconcile: %w", err)
	}
	authorize := func(att lockedAttempt) error {
		if att.serverID != serverID || att.environmentID != environmentID || att.fence != fence {
			return ErrServerAuthorizationDenied
		}
		return nil
	}

	if pendingAttemptID == nil || *pendingAttemptID != attemptID {
		// Repeat: anchored to the explicit attempt (slot lock held, so the
		// flag cannot move). Slot -> attempt is a subsequence of the lock order.
		var att lockedAttempt
		var jobID int64
		if err = tx.QueryRow(ctx, `SELECT job_id, server_id, state, environment_id, fence, closed_at FROM agent_job_attempts WHERE id=$1 FOR UPDATE`, attemptID).Scan(&jobID, &att.serverID, &att.state, &att.environmentID, &att.fence, &att.closedAt); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrServerAuthorizationDenied
			}
			return fmt.Errorf("lock agent job attempt for activation reconcile: %w", err)
		}
		if err = authorize(att); err != nil {
			return err
		}
		if att.state == "leased" || att.closedAt == nil {
			return ErrServerAuthorizationDenied
		}
		var alreadyReconciled bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM agent_job_events WHERE job_id=$1 AND attempt_id=$2 AND event_type='reconciled')`, jobID, attemptID).Scan(&alreadyReconciled); err != nil {
			return fmt.Errorf("check prior agent lease reconciliation: %w", err)
		}
		if !alreadyReconciled {
			return ErrServerAuthorizationDenied
		}
		if err = agreeResolvedOutcome(ctx, tx, attemptID, outcome, &outcomeDigest); err != nil {
			return err
		}
		if err = tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit agent lease activation reconcile no-op: %w", err)
		}
		return nil
	}

	// Identity check on the immutable job_id/server_id/environment_id/fence
	// (validate_agent_job_attempt_update, migration 000013) BEFORE any further
	// lock, so a foreign server is denied and never queues on the job. The
	// in-lock authorize below repeats it as defence in depth.
	var jobID int64
	var peek lockedAttempt
	if err = tx.QueryRow(ctx, `SELECT job_id, server_id, environment_id, fence FROM agent_job_attempts WHERE id=$1`, attemptID).Scan(&jobID, &peek.serverID, &peek.environmentID, &peek.fence); err != nil {
		return fmt.Errorf("locate agent lease reconciliation attempt job: %w", err)
	}
	if err = authorize(peek); err != nil {
		return err
	}
	if err = r.resolveDeclared(ctx, tx, declaredResolution{
		environmentID:        environmentID,
		jobID:                jobID,
		attemptID:            attemptID,
		slotCurrentAttemptID: slotCurrentAttemptID,
		outcome:              outcome,
		authorize:            authorize,
		outcomeDigest:        &outcomeDigest,
		// Reconcile's own rule: only a closed attempt's flag is cleared.
		plainAttemptRefusal: func(att lockedAttempt) error {
			if att.state == "leased" || att.closedAt == nil {
				return ErrServerAuthorizationDenied
			}
			return nil
		},
		clearPlain: func() error {
			var now time.Time
			if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
				return fmt.Errorf("agent lease reconcile canonical time: %w", err)
			}
			if _, err := tx.Exec(ctx, `UPDATE environment_slots SET reconciliation_required=false, reconciliation_attempt_id=NULL, updated_at=$2 WHERE environment_id=$1`, environmentID, now); err != nil {
				return fmt.Errorf("clear environment slot reconciliation: %w", err)
			}
			if err := r.inject("slot_reconcile_clear"); err != nil {
				return err
			}
			if err := appendAgentJobEvent(ctx, tx, jobID, &attemptID, "reconciled"); err != nil {
				return err
			}
			return r.inject("job_event")
		},
	}); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit agent lease activation reconcile: %w", err)
	}
	return nil
}
