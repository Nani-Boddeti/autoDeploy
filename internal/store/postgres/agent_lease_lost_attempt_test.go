//go:build integration

package postgres

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"

	"autodeploy/internal/deployment"
	"autodeploy/internal/queue"
)

// Slice 23-G (owner decision D2): a restarted agent that lost an attempt at or
// before building calls Complete(failed, transient) with the recorded
// attempt/fence. That is an ordinary completion, not expiry: the attempt
// closes failed, the job retries under the normal limits, the deployment is
// untouched on retry_wait, and the slot is released with no reconciliation
// flag and no fence advance. The request shape below is exactly the one
// internal/agent/client's lost-attempt recovery sends.
func lostAttemptRequest(attemptID, fence int64) (operationID string, resultDigest [32]byte) {
	return fmt.Sprintf("op-%d-%d", attemptID, fence),
		sha256.Sum256([]byte(fmt.Sprintf("%d|%d|%t|%s", attemptID, fence, false, queue.FailureClassTransient)))
}

func TestCompleteFailedTransientForLostAttemptClosesAsOrdinaryFailure(t *testing.T) {
	for i, target := range []deployment.Status{deployment.StatusAssigned, deployment.StatusFetching, deployment.StatusBuilding} {
		t.Run(string(target), func(t *testing.T) {
			f := newLeaseFixture(t, byte(110+i*4))
			repo, d, attemptID, fence := f.claimAt(t, byte(180+i*4), target)
			opID, digestBytes := lostAttemptRequest(attemptID, fence)
			ctx := context.Background()

			if err := repo.Complete(ctx, f.serverID, d, attemptID, fence, queue.CompletionFailed, queue.FailureClassTransient, opID, digestBytes); err != nil {
				t.Fatal(err)
			}
			state, closedAt, failureClass, resultOp, _ := f.attemptRow(t, attemptID)
			if state != "failed" || closedAt == nil || failureClass == nil || *failureClass != "transient" || resultOp == nil || *resultOp != opID {
				t.Fatalf("attempt state=%s closedAt=%v failureClass=%v op=%v", state, closedAt, failureClass, resultOp)
			}
			// Complete is not expiry: no fence advance, no reconciliation flag.
			slotFence, current, reconAttempt, flagged := f.slotRow(t)
			if slotFence != fence || current != nil || flagged || reconAttempt != nil {
				t.Fatalf("slot fence=%d current=%v flagged=%v reconAttempt=%v, want fence %d, free, NOT flagged", slotFence, current, flagged, reconAttempt, fence)
			}
			if jobState, _, _, count := f.jobRow(t); jobState != "retry_wait" || count != 1 {
				t.Fatalf("job=%s attempt_count=%d, want retry_wait/1", jobState, count)
			}
			if got := f.deploymentStatus(t); got != target {
				t.Fatalf("deployment=%s, want unchanged %s on retry_wait", got, target)
			}
			ev := f.jobEventTypes(t)
			if n := len(ev); n < 1 || ev[n-1] != "retry_scheduled" {
				t.Fatalf("events=%v, want ...retry_scheduled", ev)
			}
			// Restarted twice quickly: the identical request replays cleanly.
			if err := repo.Complete(ctx, f.serverID, d, attemptID, fence, queue.CompletionFailed, queue.FailureClassTransient, opID, digestBytes); err != nil {
				t.Fatalf("identical replay: %v", err)
			}
		})
	}
}

// Once the attempt's authority lapsed the sweeper owns it; the late Complete
// is refused as stale, which the agent treats as a definitive refusal.
func TestCompleteFailedTransientForLapsedLostAttemptIsStale(t *testing.T) {
	f := newLeaseFixture(t, 130)
	repo, d, attemptID, fence := f.claimAt(t, 200, deployment.StatusBuilding)
	f.lapseAttempt(t, attemptID)
	opID, digestBytes := lostAttemptRequest(attemptID, fence)
	err := repo.Complete(context.Background(), f.serverID, d, attemptID, fence, queue.CompletionFailed, queue.FailureClassTransient, opID, digestBytes)
	if !errors.Is(err, ErrAgentLeaseStale) {
		t.Fatalf("Complete on a lapsed attempt = %v, want ErrAgentLeaseStale", err)
	}
}

// A Complete(failed) that lands after the operator requested cancellation must
// close the job as cancelled, not retry_wait/dead: Claim never picks up a job
// with cancel_requested_at set, so a retry_wait would strand it forever. The
// attempt itself is still recorded as failed with its class (that is what the
// agent reported); the slot is freed exactly as Complete always does.
func TestCompleteFailedAfterCancelRequestedCancelsJob(t *testing.T) {
	classes := []queue.FailureClass{queue.FailureClassTransient, queue.FailureClassPermanentConfiguration}
	targets := []deployment.Status{deployment.StatusAssigned, deployment.StatusFetching, deployment.StatusBuilding}
	for i, target := range targets {
		for j, class := range classes {
			t.Run(string(target)+"/"+string(class), func(t *testing.T) {
				f := newLeaseFixture(t, byte(140+i*4+j*2))
				repo, d, attemptID, fence := f.claimAt(t, byte(190+i*4+j*2), target)
				ctx := context.Background()
				if err := repo.RequestCancellation(ctx, f.principal, f.jobID); err != nil {
					t.Fatal(err)
				}
				opID, digestBytes := lostAttemptRequest(attemptID, fence)
				if err := repo.Complete(ctx, f.serverID, d, attemptID, fence, queue.CompletionFailed, class, opID, digestBytes); err != nil {
					t.Fatal(err)
				}
				state, closedAt, failureClass, _, _ := f.attemptRow(t, attemptID)
				if state != "failed" || closedAt == nil || failureClass == nil || *failureClass != string(class) {
					t.Fatalf("attempt state=%s failureClass=%v", state, failureClass)
				}
				if jobState, _, _, _ := f.jobRow(t); jobState != "cancelled" {
					t.Fatalf("job=%s, want cancelled", jobState)
				}
				if got := f.deploymentStatus(t); got != deployment.StatusCancelled {
					t.Fatalf("deployment=%s, want cancelled", got)
				}
				slotFence, current, reconAttempt, flagged := f.slotRow(t)
				if slotFence != fence || current != nil || flagged || reconAttempt != nil {
					t.Fatalf("slot fence=%d current=%v flagged=%v reconAttempt=%v, want fence %d, free, NOT flagged", slotFence, current, flagged, reconAttempt, fence)
				}
				ev := f.jobEventTypes(t)
				if n := len(ev); n < 1 || ev[n-1] != "cancelled" {
					t.Fatalf("events=%v, want ...cancelled", ev)
				}
				for _, e := range ev {
					if e == "retry_scheduled" || e == "dead" {
						t.Fatalf("events=%v must not schedule a retry", ev)
					}
				}
				if err := repo.Complete(ctx, f.serverID, d, attemptID, fence, queue.CompletionFailed, class, opID, digestBytes); err != nil {
					t.Fatalf("identical replay: %v", err)
				}
				f.insertJob(t, f.serverID, f.envID, "after-cancel-"+string(target), 0)
				leases, err := repo.Claim(ctx, f.serverID, d, 5)
				if err != nil {
					t.Fatal(err)
				}
				for _, l := range leases {
					if l.JobID == f.jobID {
						t.Fatal("cancelled job was claimed again")
					}
				}
			})
		}
	}
}
