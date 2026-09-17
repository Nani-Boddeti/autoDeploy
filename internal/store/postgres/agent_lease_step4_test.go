//go:build integration

package postgres

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"autodeploy/internal/deployment"
	"autodeploy/internal/queue"
)

// assignDeployment transitions f's backing deployment queued->assigned using
// the real DeploymentRepository, mirroring what a real first Claim would
// already have done before any attempt reaches completion/cancellation/
// expiry. f.leaseJob bypasses Claim for test setup, so tests that exercise a
// terminal deployment transition (failed/cancelled) must call this first or
// the deployment aggregate's own transition table (queued cannot go directly
// to failed) rejects it.
func (f leaseFixture) assignDeployment(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	var deploymentID string
	if err := f.pool.QueryRow(ctx, `SELECT deployment_id FROM agent_jobs WHERE id=$1`, f.jobID).Scan(&deploymentID); err != nil {
		t.Fatal(err)
	}
	repo := NewDeploymentRepository(f.pool)
	dep, err := repo.GetByID(ctx, deployment.ID(deploymentID))
	if err != nil {
		t.Fatal(err)
	}
	var now time.Time
	if err := f.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	next, err := dep.Transition(deployment.StatusAssigned, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Save(ctx, next); err != nil {
		t.Fatal(err)
	}
}

// advanceToAttempt drives f's job through n real leased->retry_wait->leased
// cycles so it legitimately reaches attempt_number/attempt_count n (the
// agent_jobs update trigger only allows attempt_count to change by exactly
// one, and only when entering 'leased', so it cannot be set directly by
// test SQL). Every attempt before the last is closed 'expired' and the job
// cycled back through retry_wait; the last attempt is left leased, with
// lease/authority deadlines finalLeaseSeconds out, for the caller to act on.
func (f leaseFixture) advanceToAttempt(t *testing.T, n int, fence int64, finalLeaseSeconds int) int64 {
	t.Helper()
	ctx := context.Background()
	var attemptID int64
	for i := 1; i <= n; i++ {
		seconds := 90
		if i == n {
			seconds = finalLeaseSeconds
		}
		attemptID = f.leaseJob(t, fence, seconds)
		if i < n {
			if _, err := f.pool.Exec(ctx, `UPDATE agent_job_attempts SET state='expired', closed_at=clock_timestamp() WHERE id=$1`, attemptID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.pool.Exec(ctx, `UPDATE agent_jobs SET state='retry_wait', available_at=clock_timestamp() WHERE id=$1`, f.jobID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.pool.Exec(ctx, `UPDATE environment_slots SET current_attempt_id=NULL WHERE environment_id=$1`, f.envID); err != nil {
				t.Fatal(err)
			}
		}
	}
	return attemptID
}

func (f leaseFixture) deploymentStatus(t *testing.T) deployment.Status {
	t.Helper()
	ctx := context.Background()
	var deploymentID, status string
	if err := f.pool.QueryRow(ctx, `SELECT deployment_id FROM agent_jobs WHERE id=$1`, f.jobID).Scan(&deploymentID); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT status FROM deployments WHERE id=$1`, deploymentID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return deployment.Status(status)
}

func (f leaseFixture) attemptRow(t *testing.T, attemptID int64) (state string, closedAt *time.Time, failureClass, resultOperationID *string, resultDigest []byte) {
	t.Helper()
	if err := f.pool.QueryRow(context.Background(), `SELECT state, closed_at, failure_class, result_operation_id, result_digest FROM agent_job_attempts WHERE id=$1`, attemptID).
		Scan(&state, &closedAt, &failureClass, &resultOperationID, &resultDigest); err != nil {
		t.Fatal(err)
	}
	return
}

func (f leaseFixture) jobRow(t *testing.T) (state string, availableAt time.Time, cancelRequestedAt *time.Time, attemptCount int) {
	t.Helper()
	if err := f.pool.QueryRow(context.Background(), `SELECT state, available_at, cancel_requested_at, attempt_count FROM agent_jobs WHERE id=$1`, f.jobID).
		Scan(&state, &availableAt, &cancelRequestedAt, &attemptCount); err != nil {
		t.Fatal(err)
	}
	return
}

func (f leaseFixture) slotRow(t *testing.T) (fence int64, currentAttemptID, reconciliationAttemptID *int64, reconciling bool) {
	t.Helper()
	if err := f.pool.QueryRow(context.Background(), `SELECT fence, current_attempt_id, reconciliation_attempt_id, reconciliation_required FROM environment_slots WHERE environment_id=$1`, f.envID).
		Scan(&fence, &currentAttemptID, &reconciliationAttemptID, &reconciling); err != nil {
		t.Fatal(err)
	}
	return
}

func TestAgentLeaseCompleteSucceeded(t *testing.T) {
	f := newLeaseFixture(t, 150)
	d := f.withCredential(t, 200)
	attemptID := f.leaseJob(t, 1, 90)
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	ctx := context.Background()
	resultDigest := digest(9)

	if err := repo.Complete(ctx, f.serverID, d, attemptID, 1, queue.CompletionSucceeded, "", "op-1", resultDigest); err != nil {
		t.Fatal(err)
	}

	state, closedAt, failureClass, resultOperationID, storedDigest := f.attemptRow(t, attemptID)
	if state != "succeeded" || closedAt == nil || failureClass != nil || resultOperationID == nil || *resultOperationID != "op-1" || [32]byte(storedDigest) != resultDigest {
		t.Fatalf("attempt state=%s closedAt=%v failureClass=%v resultOperationID=%v", state, closedAt, failureClass, resultOperationID)
	}
	jobState, _, _, _ := f.jobRow(t)
	if jobState != "succeeded" {
		t.Fatalf("jobState=%s", jobState)
	}
	_, currentAttemptID, _, _ := f.slotRow(t)
	if currentAttemptID != nil {
		t.Fatalf("slot still occupied by %v", *currentAttemptID)
	}
	// Success leaves the deployment untouched (the execution slice owns
	// fetching onward); the fixture never advanced it past queued.
	if got := f.deploymentStatus(t); got != deployment.StatusQueued {
		t.Fatalf("deployment status=%s, want unchanged queued", got)
	}
	var eventType string
	if err := f.pool.QueryRow(ctx, `SELECT event_type FROM agent_job_events WHERE job_id=$1 AND attempt_id=$2`, f.jobID, attemptID).Scan(&eventType); err != nil {
		t.Fatal(err)
	}
	if eventType != "succeeded" {
		t.Fatalf("eventType=%s", eventType)
	}
}

func TestAgentLeaseCompleteFailedTransientRetryWait(t *testing.T) {
	f := newLeaseFixture(t, 151)
	d := f.withCredential(t, 201)
	attemptID := f.leaseJob(t, 1, 90)
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	ctx := context.Background()

	var before time.Time
	if err := f.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := repo.Complete(ctx, f.serverID, d, attemptID, 1, queue.CompletionFailed, queue.FailureClassTransient, "op-2", digest(10)); err != nil {
		t.Fatal(err)
	}
	var after time.Time
	if err := f.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&after); err != nil {
		t.Fatal(err)
	}

	state, _, failureClass, _, _ := f.attemptRow(t, attemptID)
	if state != "failed" || failureClass == nil || *failureClass != "transient" {
		t.Fatalf("state=%s failureClass=%v", state, failureClass)
	}
	jobState, availableAt, _, attemptCount := f.jobRow(t)
	if jobState != "retry_wait" || attemptCount != 1 {
		t.Fatalf("jobState=%s attemptCount=%d", jobState, attemptCount)
	}
	// attempt_number for the first attempt is 1, so the jitter window is
	// uniform(0, 5s]: available_at must fall in [before, before+5s], with
	// slack up to after+5s for query latency.
	if availableAt.Before(before) || availableAt.After(after.Add(5*time.Second)) {
		t.Fatalf("available_at=%s not within [%s,%s]", availableAt, before, after.Add(5*time.Second))
	}
}

func TestAgentLeaseCompleteFailedTransientExhaustedDead(t *testing.T) {
	f := newLeaseFixture(t, 152)
	d := f.withCredential(t, 202)
	f.assignDeployment(t)
	attemptID := f.advanceToAttempt(t, 5, 1, 90)
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	ctx := context.Background()

	if err := repo.Complete(ctx, f.serverID, d, attemptID, 1, queue.CompletionFailed, queue.FailureClassTransient, "op-3", digest(11)); err != nil {
		t.Fatal(err)
	}
	jobState, _, _, attemptCount := f.jobRow(t)
	if jobState != "dead" || attemptCount != 5 {
		t.Fatalf("jobState=%s attemptCount=%d", jobState, attemptCount)
	}
	if got := f.deploymentStatus(t); got != deployment.StatusFailed {
		t.Fatalf("deployment status=%s, want failed", got)
	}
	var eventType string
	if err := f.pool.QueryRow(ctx, `SELECT event_type FROM agent_job_events WHERE job_id=$1 AND event_type='dead'`, f.jobID).Scan(&eventType); err != nil {
		t.Fatal(err)
	}
}

func TestAgentLeaseCompleteFailedNonRetryableFailsJobAndDeployment(t *testing.T) {
	f := newLeaseFixture(t, 153)
	d := f.withCredential(t, 203)
	f.assignDeployment(t)
	attemptID := f.leaseJob(t, 1, 90)
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	ctx := context.Background()

	if err := repo.Complete(ctx, f.serverID, d, attemptID, 1, queue.CompletionFailed, queue.FailureClassPermanentConfiguration, "op-4", digest(12)); err != nil {
		t.Fatal(err)
	}
	jobState, _, _, _ := f.jobRow(t)
	if jobState != "failed" {
		t.Fatalf("jobState=%s, want failed (non-retryable, no attempts consumed)", jobState)
	}
	if got := f.deploymentStatus(t); got != deployment.StatusFailed {
		t.Fatalf("deployment status=%s, want failed", got)
	}
}

func TestAgentLeaseCompleteReplayIdempotentEvenAfterAuthorityExpiry(t *testing.T) {
	f := newLeaseFixture(t, 154)
	d := f.withCredential(t, 204)
	// A 1-second lease: by the time the replay call below runs, real wall
	// time has moved past authority_expires_at. A closed attempt's
	// agent_job_attempts row is immutable (the update trigger forbids any
	// further write to it), so this cannot be simulated by mutating the
	// closed row afterward -- the lease genuinely has to have been short.
	attemptID := f.leaseJob(t, 1, 1)
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	ctx := context.Background()
	resultDigest := digest(13)

	if err := repo.Complete(ctx, f.serverID, d, attemptID, 1, queue.CompletionSucceeded, "", "op-5", resultDigest); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	// Replay must still succeed: a closed attempt's replay branch never
	// re-checks authority_expires_at, only the stored result.
	if err := repo.Complete(ctx, f.serverID, d, attemptID, 1, queue.CompletionSucceeded, "", "op-5", resultDigest); err != nil {
		t.Fatalf("replay rejected: %v", err)
	}
	// No further mutation: still succeeded, still one succeeded event.
	state, _, _, _, _ := f.attemptRow(t, attemptID)
	if state != "succeeded" {
		t.Fatalf("state=%s", state)
	}
	var events int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM agent_job_events WHERE job_id=$1 AND event_type='succeeded'`, f.jobID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Fatalf("events=%d, want exactly one succeeded event despite replay", events)
	}
}

func TestAgentLeaseCompleteReplayMismatchIsIntegrityConflict(t *testing.T) {
	f := newLeaseFixture(t, 155)
	d := f.withCredential(t, 205)
	attemptID := f.leaseJob(t, 1, 90)
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	ctx := context.Background()

	if err := repo.Complete(ctx, f.serverID, d, attemptID, 1, queue.CompletionSucceeded, "", "op-6", digest(14)); err != nil {
		t.Fatal(err)
	}
	if err := repo.Complete(ctx, f.serverID, d, attemptID, 1, queue.CompletionSucceeded, "", "op-6", digest(99)); !errors.Is(err, ErrAgentLeaseIntegrityConflict) {
		t.Fatalf("digest mismatch err=%v", err)
	}
	if err := repo.Complete(ctx, f.serverID, d, attemptID, 1, queue.CompletionFailed, queue.FailureClassTransient, "op-6", digest(14)); !errors.Is(err, ErrAgentLeaseIntegrityConflict) {
		t.Fatalf("outcome mismatch err=%v", err)
	}
	state, _, _, _, _ := f.attemptRow(t, attemptID)
	if state != "succeeded" {
		t.Fatalf("state=%s, mismatch must not mutate", state)
	}
}

func TestAgentLeaseCompleteLateFromExpiredAttemptIsStale(t *testing.T) {
	f := newLeaseFixture(t, 156)
	d := f.withCredential(t, 206)
	attemptID := f.leaseJob(t, 1, 90)
	if _, err := f.pool.Exec(context.Background(), `UPDATE agent_job_attempts SET state='expired', closed_at=clock_timestamp() WHERE id=$1`, attemptID); err != nil {
		t.Fatal(err)
	}
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	if err := repo.Complete(context.Background(), f.serverID, d, attemptID, 1, queue.CompletionSucceeded, "", "op-7", digest(15)); !errors.Is(err, ErrAgentLeaseStale) {
		t.Fatalf("err=%v", err)
	}
	state, _, _, _, _ := f.attemptRow(t, attemptID)
	if state != "expired" {
		t.Fatalf("state=%s, must remain expired", state)
	}
}

func TestAgentLeaseCompleteStaleFenceRejectedSlotStaysReconciling(t *testing.T) {
	f := newLeaseFixture(t, 157)
	d := f.withCredential(t, 207)
	attemptID := f.leaseJob(t, 1, 90)
	// Simulate an expiry sweep that already advanced the fence and marked
	// reconciliation required, then a late completion for the stale fence
	// arrives.
	if _, err := f.pool.Exec(context.Background(), `UPDATE agent_job_attempts SET state='expired', closed_at=clock_timestamp() WHERE id=$1`, attemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(context.Background(), `UPDATE environment_slots SET fence=fence+1, current_attempt_id=NULL, reconciliation_required=true, reconciliation_attempt_id=$2 WHERE environment_id=$1`, f.envID, attemptID); err != nil {
		t.Fatal(err)
	}
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	if err := repo.Complete(context.Background(), f.serverID, d, attemptID, 1, queue.CompletionSucceeded, "", "op-8", digest(16)); !errors.Is(err, ErrAgentLeaseStale) {
		t.Fatalf("err=%v", err)
	}
	_, _, reconciliationAttemptID, reconciling := f.slotRow(t)
	if !reconciling || reconciliationAttemptID == nil || *reconciliationAttemptID != attemptID {
		t.Fatalf("slot reconciliation state disturbed: reconciling=%v attempt=%v", reconciling, reconciliationAttemptID)
	}
}

func TestAgentLeaseCompleteInjectedFailureRollsBackEveryStage(t *testing.T) {
	stages := []string{"attempt_close", "slot_free", "job_close", "job_event"}
	for i, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			f := newLeaseFixture(t, byte(160+i))
			d := f.withCredential(t, byte(210+i))
			attemptID := f.leaseJob(t, 1, 90)
			repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
			injected := errors.New("injected failure at " + stage)
			repo.failAfter = func(s string) error {
				if s == stage {
					return injected
				}
				return nil
			}
			if err := repo.Complete(context.Background(), f.serverID, d, attemptID, 1, queue.CompletionSucceeded, "", "op-fail", digest(20)); !errors.Is(err, injected) {
				t.Fatalf("stage=%s err=%v", stage, err)
			}
			state, closedAt, _, resultOperationID, _ := f.attemptRow(t, attemptID)
			if state != "leased" || closedAt != nil || resultOperationID != nil {
				t.Fatalf("stage=%s attempt mutated despite rollback: state=%s closedAt=%v", stage, state, closedAt)
			}
			_, currentAttemptID, _, _ := f.slotRow(t)
			if currentAttemptID == nil || *currentAttemptID != attemptID {
				t.Fatalf("stage=%s slot occupant mutated despite rollback", stage)
			}
			jobState, _, _, _ := f.jobRow(t)
			if jobState != "leased" {
				t.Fatalf("stage=%s jobState=%s, want leased", stage, jobState)
			}
		})
	}
}

func TestAgentJobRequestCancellationQueuedIsImmediate(t *testing.T) {
	f := newLeaseFixture(t, 170)
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	if err := repo.RequestCancellation(context.Background(), f.principal, f.jobID); err != nil {
		t.Fatal(err)
	}
	jobState, _, cancelRequestedAt, _ := f.jobRow(t)
	if jobState != "cancelled" || cancelRequestedAt == nil {
		t.Fatalf("jobState=%s cancelRequestedAt=%v", jobState, cancelRequestedAt)
	}
	if got := f.deploymentStatus(t); got != deployment.StatusCancelled {
		t.Fatalf("deployment status=%s, want cancelled", got)
	}
	// Idempotent repeat.
	if err := repo.RequestCancellation(context.Background(), f.principal, f.jobID); err != nil {
		t.Fatalf("repeat cancellation: %v", err)
	}
}

func TestAgentJobRequestCancellationLeasedCapsDeadlinesInSameTx(t *testing.T) {
	f := newLeaseFixture(t, 171)
	attemptID := f.leaseJob(t, 1, 90)
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	ctx := context.Background()

	var before time.Time
	if err := f.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := repo.RequestCancellation(ctx, f.principal, f.jobID); err != nil {
		t.Fatal(err)
	}
	jobState, _, cancelRequestedAt, _ := f.jobRow(t)
	if jobState != "leased" || cancelRequestedAt == nil {
		t.Fatalf("jobState=%s cancelRequestedAt=%v", jobState, cancelRequestedAt)
	}
	var leaseExpiresAt, authorityExpiresAt time.Time
	var cancelDeadlineAt *time.Time
	if err := f.pool.QueryRow(ctx, `SELECT lease_expires_at, authority_expires_at, cancel_deadline_at FROM agent_job_attempts WHERE id=$1`, attemptID).Scan(&leaseExpiresAt, &authorityExpiresAt, &cancelDeadlineAt); err != nil {
		t.Fatal(err)
	}
	if cancelDeadlineAt == nil || !leaseExpiresAt.Equal(*cancelDeadlineAt) || !authorityExpiresAt.Equal(*cancelDeadlineAt) {
		t.Fatalf("deadlines not capped together in one tx: lease=%s authority=%s cap=%v", leaseExpiresAt, authorityExpiresAt, cancelDeadlineAt)
	}
	if cancelDeadlineAt.After(before.Add(61 * time.Second)) {
		t.Fatalf("cap %s too far past now+60s (%s)", cancelDeadlineAt, before.Add(60*time.Second))
	}

	// Renew must see cancel_requested and the capped deadline, never able to
	// extend past it.
	renewal, err := repo.Renew(ctx, f.serverID, f.withCredential(t, 220), attemptID, 1, 1)
	if err != nil {
		// withCredential inserts a fresh credential every call; the
		// existing one from leaseJob's fixture setup has none yet, so this
		// is the attempt's only credential and Renew must accept it.
		t.Fatal(err)
	}
	if !renewal.CancelRequested || !renewal.AuthorityExpiresAt.Equal(*cancelDeadlineAt) {
		t.Fatalf("renewal=%+v, want cancel-requested capped at %s", renewal, cancelDeadlineAt)
	}

	// A second cancellation request is idempotent.
	if err := repo.RequestCancellation(ctx, f.principal, f.jobID); err != nil {
		t.Fatalf("repeat cancellation: %v", err)
	}
}

func TestAgentJobRequestCancellationDuringActivatingRejected(t *testing.T) {
	f := newLeaseFixture(t, 172)
	f.leaseJob(t, 1, 90)
	f.assignDeployment(t)
	ctx := context.Background()
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	deploymentRepo := NewDeploymentRepository(f.pool)
	var deploymentID string
	if err := f.pool.QueryRow(ctx, `SELECT deployment_id FROM agent_jobs WHERE id=$1`, f.jobID).Scan(&deploymentID); err != nil {
		t.Fatal(err)
	}
	dep, err := deploymentRepo.GetByID(ctx, deployment.ID(deploymentID))
	if err != nil {
		t.Fatal(err)
	}
	var now time.Time
	if err := f.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	for _, s := range []deployment.Status{deployment.StatusFetching, deployment.StatusBuilding, deployment.StatusStarting, deployment.StatusHealthChecking, deployment.StatusActivating} {
		dep, err = dep.Transition(s, now)
		if err != nil {
			t.Fatal(err)
		}
		if err = deploymentRepo.Save(ctx, dep); err != nil {
			t.Fatal(err)
		}
	}

	if err := repo.RequestCancellation(ctx, f.principal, f.jobID); !errors.Is(err, ErrAgentJobActivating) {
		t.Fatalf("err=%v", err)
	}
	jobState, _, cancelRequestedAt, _ := f.jobRow(t)
	if jobState != "leased" || cancelRequestedAt != nil {
		t.Fatalf("job mutated despite activating rejection: state=%s cancelRequestedAt=%v", jobState, cancelRequestedAt)
	}
}

func TestAgentJobRequestCancellationInjectedFailureRollsBack(t *testing.T) {
	stages := []string{"job_cancel_request", "attempt_cancel_cap", "job_event"}
	for i, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			f := newLeaseFixture(t, byte(180+i))
			f.leaseJob(t, 1, 90)
			repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
			injected := errors.New("injected failure at " + stage)
			repo.failAfter = func(s string) error {
				if s == stage {
					return injected
				}
				return nil
			}
			if err := repo.RequestCancellation(context.Background(), f.principal, f.jobID); !errors.Is(err, injected) {
				t.Fatalf("stage=%s err=%v", stage, err)
			}
			jobState, _, cancelRequestedAt, _ := f.jobRow(t)
			if jobState != "leased" || cancelRequestedAt != nil {
				t.Fatalf("stage=%s job mutated despite rollback: state=%s cancelRequestedAt=%v", stage, jobState, cancelRequestedAt)
			}
		})
	}
}

func TestAgentLeaseAcknowledgeCancellationClosesAttemptAndJob(t *testing.T) {
	f := newLeaseFixture(t, 190)
	d := f.withCredential(t, 240)
	attemptID := f.leaseJob(t, 1, 90)
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	ctx := context.Background()

	if err := repo.RequestCancellation(ctx, f.principal, f.jobID); err != nil {
		t.Fatal(err)
	}
	if err := repo.AcknowledgeCancellation(ctx, f.serverID, d, attemptID, 1, "ack-1", digest(30)); err != nil {
		t.Fatal(err)
	}
	state, _, failureClass, resultOperationID, _ := f.attemptRow(t, attemptID)
	if state != "cancelled" || failureClass != nil || resultOperationID == nil || *resultOperationID != "ack-1" {
		t.Fatalf("state=%s failureClass=%v resultOperationID=%v", state, failureClass, resultOperationID)
	}
	jobState, _, _, _ := f.jobRow(t)
	if jobState != "cancelled" {
		t.Fatalf("jobState=%s", jobState)
	}
	_, currentAttemptID, _, _ := f.slotRow(t)
	if currentAttemptID != nil {
		t.Fatalf("slot still occupied")
	}

	// Replay is idempotent.
	if err := repo.AcknowledgeCancellation(ctx, f.serverID, d, attemptID, 1, "ack-1", digest(30)); err != nil {
		t.Fatalf("replay: %v", err)
	}
	// Mismatch is an integrity conflict.
	if err := repo.AcknowledgeCancellation(ctx, f.serverID, d, attemptID, 1, "ack-1", digest(99)); !errors.Is(err, ErrAgentLeaseIntegrityConflict) {
		t.Fatalf("mismatch err=%v", err)
	}
}

func TestAgentLeaseExpireBatchExpiredVsCancelledClosure(t *testing.T) {
	t.Run("expired", func(t *testing.T) {
		f := newLeaseFixture(t, 200)
		attemptID := f.leaseJob(t, 1, 1) // 1-second lease, already past by the time ExpireBatch runs
		time.Sleep(1100 * time.Millisecond)
		repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
		processed, err := repo.ExpireBatch(context.Background(), 10)
		if err != nil {
			t.Fatal(err)
		}
		if processed != 1 {
			t.Fatalf("processed=%d", processed)
		}
		state, closedAt, _, _, _ := f.attemptRow(t, attemptID)
		if state != "expired" || closedAt == nil {
			t.Fatalf("state=%s closedAt=%v", state, closedAt)
		}
		jobState, _, _, attemptCount := f.jobRow(t)
		if jobState != "retry_wait" || attemptCount != 1 {
			t.Fatalf("jobState=%s attemptCount=%d, want retry_wait/1 (expiry must not increment attempt_count)", jobState, attemptCount)
		}
		fence, currentAttemptID, reconciliationAttemptID, reconciling := f.slotRow(t)
		if fence != 2 || currentAttemptID != nil || !reconciling || reconciliationAttemptID == nil || *reconciliationAttemptID != attemptID {
			t.Fatalf("slot fence=%d current=%v reconciling=%v reconcileAttempt=%v", fence, currentAttemptID, reconciling, reconciliationAttemptID)
		}
	})

	t.Run("cancel requested closes as cancelled", func(t *testing.T) {
		f := newLeaseFixture(t, 201)
		attemptID := f.leaseJob(t, 1, 1)
		if _, err := f.pool.Exec(context.Background(), `UPDATE agent_jobs SET cancel_requested_at=clock_timestamp() WHERE id=$1`, f.jobID); err != nil {
			t.Fatal(err)
		}
		time.Sleep(1100 * time.Millisecond)
		repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
		processed, err := repo.ExpireBatch(context.Background(), 10)
		if err != nil {
			t.Fatal(err)
		}
		if processed != 1 {
			t.Fatalf("processed=%d", processed)
		}
		state, _, _, _, _ := f.attemptRow(t, attemptID)
		if state != "cancelled" {
			t.Fatalf("state=%s, want cancelled", state)
		}
		jobState, _, _, attemptCount := f.jobRow(t)
		if jobState != "cancelled" || attemptCount != 1 {
			t.Fatalf("jobState=%s attemptCount=%d", jobState, attemptCount)
		}
	})
}

func TestAgentLeaseExpireBatchRetryVsDeadAtFiveAttempts(t *testing.T) {
	f := newLeaseFixture(t, 202)
	f.assignDeployment(t)
	attemptID := f.advanceToAttempt(t, 5, 1, 1)
	time.Sleep(1100 * time.Millisecond)
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	processed, err := repo.ExpireBatch(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if processed != 1 {
		t.Fatalf("processed=%d", processed)
	}
	jobState, _, _, attemptCount := f.jobRow(t)
	if jobState != "dead" || attemptCount != 5 {
		t.Fatalf("jobState=%s attemptCount=%d", jobState, attemptCount)
	}
	if got := f.deploymentStatus(t); got != deployment.StatusFailed {
		t.Fatalf("deployment status=%s, want failed", got)
	}
	state, _, _, _, _ := f.attemptRow(t, attemptID)
	if state != "expired" {
		t.Fatalf("fifth attempt state=%s, want expired", state)
	}
}

func TestAgentLeaseExpireBatchConcurrentRunnersProcessOnce(t *testing.T) {
	f := newLeaseFixture(t, 203)
	attemptID := f.leaseJob(t, 1, 1)
	time.Sleep(1100 * time.Millisecond)
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	ctx := context.Background()

	const runners = 6
	var wg sync.WaitGroup
	totals := make([]int, runners)
	start := make(chan struct{})
	for i := 0; i < runners; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			n, err := repo.ExpireBatch(ctx, 10)
			if err != nil {
				t.Errorf("runner %d: %v", i, err)
				return
			}
			totals[i] = n
		}(i)
	}
	close(start)
	wg.Wait()

	total := 0
	for _, n := range totals {
		total += n
	}
	if total != 1 {
		t.Fatalf("expected exactly one runner to process the attempt, got total=%d", total)
	}
	state, _, _, _, _ := f.attemptRow(t, attemptID)
	if state != "expired" {
		t.Fatalf("state=%s", state)
	}
}

func TestAgentLeaseExpireBatchInjectedFailureRollsBack(t *testing.T) {
	stages := []string{"attempt_close", "slot_reconcile", "job_close", "job_event"}
	for i, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			f := newLeaseFixture(t, byte(210+i))
			attemptID := f.leaseJob(t, 1, 1)
			time.Sleep(1100 * time.Millisecond)
			repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
			injected := errors.New("injected failure at " + stage)
			repo.failAfter = func(s string) error {
				if s == stage {
					return injected
				}
				return nil
			}
			if _, err := repo.ExpireBatch(context.Background(), 10); !errors.Is(err, injected) {
				t.Fatalf("stage=%s err=%v", stage, err)
			}
			state, closedAt, _, _, _ := f.attemptRow(t, attemptID)
			if state != "leased" || closedAt != nil {
				t.Fatalf("stage=%s attempt mutated despite rollback: state=%s", stage, state)
			}
			fence, currentAttemptID, _, reconciling := f.slotRow(t)
			if fence != 1 || currentAttemptID == nil || *currentAttemptID != attemptID || reconciling {
				t.Fatalf("stage=%s slot mutated despite rollback: fence=%d current=%v reconciling=%v", stage, fence, currentAttemptID, reconciling)
			}
		})
	}
}

func TestEnvironmentSlotReconciliationGateBlocksClaimUntilReconciled(t *testing.T) {
	f := newLeaseFixture(t, 220)
	d := f.withCredential(t, 250)
	attemptID := f.leaseJob(t, 1, 1)
	time.Sleep(1100 * time.Millisecond)
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	ctx := context.Background()

	if _, err := repo.ExpireBatch(ctx, 10); err != nil {
		t.Fatal(err)
	}
	// A second job queued on the same environment/server cannot be claimed
	// while reconciliation is pending.
	secondJobID := f.insertJob(t, f.serverID, f.envID, "second-"+t.Name(), 0)
	leases, err := repo.Claim(ctx, f.serverID, d, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 0 {
		t.Fatalf("claimed despite pending reconciliation: %+v", leases)
	}

	// Reconciliation from a different server is denied.
	f.insertServer(t, "other-server")
	otherDigest := digest(251)
	if _, err := f.pool.Exec(ctx, `INSERT INTO server_credentials(server_id,version,digest,status) VALUES ($1,1,$2,'active')`, "other-server", otherDigest[:]); err != nil {
		t.Fatal(err)
	}
	// Reconcile is verified against the attempt's own (immutable) fence, not
	// the environment's newly advanced fence: the attempt was leased at
	// fence 1, so that is what a genuine reconcile call must present.
	if err := repo.Reconcile(ctx, "other-server", otherDigest, f.envID, attemptID, 1, digest(3)); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("cross-server reconcile err=%v", err)
	}

	// Reconciliation from the owning server clears the gate.
	if err := repo.Reconcile(ctx, f.serverID, d, f.envID, attemptID, 1, digest(3)); err != nil {
		t.Fatal(err)
	}
	// Idempotent replay of the same reconcile.
	if err := repo.Reconcile(ctx, f.serverID, d, f.envID, attemptID, 1, digest(3)); err != nil {
		t.Fatalf("reconcile replay: %v", err)
	}
	leases, err = repo.Claim(ctx, f.serverID, d, 5)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, l := range leases {
		if l.JobID == secondJobID {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected second job claimable after reconcile, leases=%+v", leases)
	}
}

func TestAgentLeaseMarkReconciledCrossOwnerDeniedAndAudited(t *testing.T) {
	f := newLeaseFixture(t, 221)
	attemptID := f.leaseJob(t, 1, 1)
	time.Sleep(1100 * time.Millisecond)
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	ctx := context.Background()
	if _, err := repo.ExpireBatch(ctx, 10); err != nil {
		t.Fatal(err)
	}

	// A second owner/session, added additively via secondOwnerPrincipal
	// rather than webhookAuthority/newLeaseFixture: both of those go through
	// freshAuthRepository, whose TRUNCATE ... owners ... CASCADE would
	// destroy the expired attempt/slot state set up above (servers,
	// agent_job_attempts, and environment_slots all FK-chain from owners).
	otherPrincipal := secondOwnerPrincipal(t, f.pool, NewAuthRepository(f.pool), 222)
	if err := repo.MarkReconciled(ctx, otherPrincipal, f.envID, "cross-owner break-glass attempt"); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("cross-owner err=%v", err)
	}
	_, _, _, reconciling := f.slotRow(t)
	if !reconciling {
		t.Fatal("cross-owner mark-reconciled must not clear the flag")
	}

	if err := repo.MarkReconciled(ctx, f.principal, f.envID, "operator break-glass: agent unreachable"); err != nil {
		t.Fatal(err)
	}
	_, _, _, reconciling = f.slotRow(t)
	if reconciling {
		t.Fatal("owner mark-reconciled must clear the flag")
	}
	var auditAction string
	if err := f.pool.QueryRow(ctx, `SELECT action FROM server_audit_events WHERE action='environment_mark_reconciled' ORDER BY id DESC LIMIT 1`).Scan(&auditAction); err != nil {
		t.Fatal(err)
	}
	if auditAction != "environment_mark_reconciled" {
		t.Fatalf("auditAction=%s", auditAction)
	}
	var reconciledEvent int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM agent_job_events WHERE job_id=$1 AND attempt_id=$2 AND event_type='reconciled'`, f.jobID, attemptID).Scan(&reconciledEvent); err != nil {
		t.Fatal(err)
	}
	if reconciledEvent != 1 {
		t.Fatalf("reconciledEvent=%d", reconciledEvent)
	}
}

func TestServerRevokeServerClosesInFlightLease(t *testing.T) {
	f := newLeaseFixture(t, 230)
	d := f.withCredential(t, 60)
	f.assignDeployment(t)
	attemptID := f.leaseJob(t, 1, 90)
	serverRepo := NewServerRepository(f.pool)
	leaseRepo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	ctx := context.Background()

	if err := serverRepo.RevokeServer(ctx, f.principal, f.serverID, "compromised host"); err != nil {
		t.Fatal(err)
	}

	state, closedAt, _, _, _ := f.attemptRow(t, attemptID)
	if state != "expired" || closedAt == nil {
		t.Fatalf("state=%s closedAt=%v, want expired", state, closedAt)
	}
	fence, currentAttemptID, reconciliationAttemptID, reconciling := f.slotRow(t)
	if fence != 2 || currentAttemptID != nil || !reconciling || reconciliationAttemptID == nil || *reconciliationAttemptID != attemptID {
		t.Fatalf("slot fence=%d current=%v reconciling=%v reconcileAttempt=%v", fence, currentAttemptID, reconciling, reconciliationAttemptID)
	}
	jobState, _, _, attemptCount := f.jobRow(t)
	if jobState != "retry_wait" || attemptCount != 1 {
		t.Fatalf("jobState=%s attemptCount=%d", jobState, attemptCount)
	}

	// Renew and Complete are denied once the server/credential is revoked.
	if _, err := leaseRepo.Renew(ctx, f.serverID, d, attemptID, 2, 0); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("renew after revoke err=%v", err)
	}
	if err := leaseRepo.Complete(ctx, f.serverID, d, attemptID, 2, queue.CompletionSucceeded, "", "op-late", digest(40)); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("complete after revoke err=%v", err)
	}
}

func TestAgentLeaseSweeperSingleRunnerAdvisoryLock(t *testing.T) {
	f := newLeaseFixture(t, 240)
	attemptID := f.leaseJob(t, 1, 1)
	time.Sleep(1100 * time.Millisecond)
	leaseRepo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	sweeperA := NewAgentLeaseSweeper(f.pool, leaseRepo)
	sweeperB := NewAgentLeaseSweeper(f.pool, leaseRepo)
	ctx := context.Background()

	var wg sync.WaitGroup
	results := make([]int, 2)
	errs := make([]error, 2)
	start := make(chan struct{})
	run := func(i int, s *AgentLeaseSweeper) {
		defer wg.Done()
		<-start
		n, err := s.SweepOnce(ctx)
		results[i], errs[i] = n, err
	}
	wg.Add(2)
	go run(0, sweeperA)
	go run(1, sweeperB)
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("sweeper %d: %v", i, err)
		}
	}
	total := results[0] + results[1]
	if total != 1 {
		t.Fatalf("expected exactly one sweeper to process the attempt, got results=%v", results)
	}
	state, _, _, _, _ := f.attemptRow(t, attemptID)
	if state != "expired" {
		t.Fatalf("state=%s", state)
	}
}
