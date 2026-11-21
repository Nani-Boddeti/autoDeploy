//go:build integration

package postgres

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"autodeploy/internal/deployment"
)

// blockSecondAttempt resolves attempt A (not_activated), claims attempt B and
// blocks it through the ordinary expiry path. It returns a fixture view whose
// attemptID is B.
func blockSecondAttempt(t *testing.T, seed byte) (first, second activatedFixture) {
	t.Helper()
	ctx := context.Background()
	a := newActivatedFixture(t, seed, true)
	a.blockViaSweeperDiversion(t)
	if err := a.resolve(ActivationOutcomeNotActivated); err != nil {
		t.Fatal(err)
	}
	a.insertJob(t, a.serverID, a.envID, "attempt-b", 0)
	leases, err := a.repo.Claim(ctx, a.serverID, a.cred, 5)
	if err != nil || len(leases) != 1 {
		t.Fatalf("claim B: leases=%d err=%v", len(leases), err)
	}
	b := a
	b.attemptID = leases[0].AttemptID
	b.jobID = leases[0].JobID
	b.advanceDeploymentTo(t, deployment.StatusStarting) // 23-F: only a host-mutating phase blocks
	b.lapse(t, "2 seconds")
	if closed, err := b.repo.expireOne(ctx, b.attemptID); err != nil || !closed {
		t.Fatalf("expireOne B closed=%v err=%v", closed, err)
	}
	return a, b
}

func slotPendingAttempt(t *testing.T, a activatedFixture) int64 {
	t.Helper()
	_, _, rec, flagged := a.slotRow(t)
	if !flagged || rec == nil {
		t.Fatalf("setup: slot must be blocked with a pending attempt (flagged=%v rec=%v)", flagged, rec)
	}
	return *rec
}

func (a activatedFixture) resolveFor(attemptID int64, outcome string) error {
	return a.repo.ResolveActivation(context.Background(), a.principal, a.envID, attemptID, outcome, resolveReason)
}

// The core 21G hazard: a stale form for attempt A must not resolve the newer
// blocked attempt B, whatever outcome it carries.
func TestActivationResolveAnchorStaleFormRefused(t *testing.T) {
	a, b := blockSecondAttempt(t, 160)
	if got := slotPendingAttempt(t, b); got != b.attemptID {
		t.Fatalf("setup: pending=%d want B=%d", got, b.attemptID)
	}
	for _, outcome := range []string{ActivationOutcomeNoActivation, ActivationOutcomeNotActivated, ActivationOutcomeActivated} {
		before := b.resolveState(t)
		if err := b.resolveFor(a.attemptID, outcome); !errors.Is(err, ErrActivationResolveStale) {
			t.Fatalf("stale resolve(%s) err=%v, want ErrActivationResolveStale", outcome, err)
		}
		if after := b.resolveState(t); !reflect.DeepEqual(before, after) {
			t.Fatalf("stale resolve(%s) wrote state:\nbefore %+v\nafter  %+v", outcome, before, after)
		}
		if got := slotPendingAttempt(t, b); got != b.attemptID {
			t.Fatalf("slot pending=%d after stale resolve, want B=%d", got, b.attemptID)
		}
	}
	var audit, events int
	if err := b.pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM server_audit_events WHERE action='environment_mark_reconciled'), (SELECT count(*) FROM agent_job_events WHERE event_type='reconciled' AND attempt_id=$1)`, b.attemptID).Scan(&audit, &events); err != nil {
		t.Fatal(err)
	}
	if audit != 1 || events != 0 {
		t.Fatalf("audit rows=%d (want 1, only A's), reconciled events for B=%d (want 0)", audit, events)
	}
	// Positive control: the same setup resolves with B's own id.
	if err := b.resolveFor(b.attemptID, ActivationOutcomeNoActivation); err != nil {
		t.Fatalf("resolve with B's id: %v", err)
	}
	if _, _, rec, flagged := b.slotRow(t); rec != nil || flagged {
		t.Fatalf("slot after resolving B: rec=%v flagged=%v", rec, flagged)
	}
}

func TestActivationResolveAnchorRejectsNonPositiveAttemptID(t *testing.T) {
	a := plainBlock(t, 161)
	for _, id := range []int64{0, -1} {
		before := a.resolveState(t)
		if err := a.resolveFor(id, ActivationOutcomeNoActivation); !errors.Is(err, ErrAgentLeaseInvalidInput) {
			t.Fatalf("expected %d err=%v, want ErrAgentLeaseInvalidInput", id, err)
		}
		if after := a.resolveState(t); !reflect.DeepEqual(before, after) {
			t.Fatalf("expected %d wrote state", id)
		}
	}
}

// Replay stays anchored to the latest attempt (21G D1) and now also requires
// the form's attempt to be that attempt.
func TestActivationResolveAnchorReplay(t *testing.T) {
	t.Run("expected equals latest resolved attempt", func(t *testing.T) {
		a := newActivatedFixture(t, 162, true)
		a.blockViaSweeperDiversion(t)
		if err := a.resolveFor(a.attemptID, ActivationOutcomeNotActivated); err != nil {
			t.Fatal(err)
		}
		before := a.resolveState(t)
		if err := a.resolveFor(a.attemptID, ActivationOutcomeNotActivated); err != nil {
			t.Fatalf("same-outcome replay err=%v", err)
		}
		if err := a.resolveFor(a.attemptID, ActivationOutcomeActivated); !errors.Is(err, ErrAgentLeaseIntegrityConflict) {
			t.Fatalf("different-outcome replay err=%v, want integrity conflict", err)
		}
		if after := a.resolveState(t); !reflect.DeepEqual(before, after) {
			t.Fatal("replay wrote state")
		}
	})
	t.Run("expected differs from latest attempt", func(t *testing.T) {
		a := newActivatedFixture(t, 163, true)
		a.blockViaSweeperDiversion(t)
		if err := a.resolveFor(a.attemptID, ActivationOutcomeNotActivated); err != nil {
			t.Fatal(err)
		}
		before := a.resolveState(t)
		if err := a.resolveFor(a.attemptID+1000000, ActivationOutcomeNotActivated); !errors.Is(err, ErrServerAuthorizationDenied) {
			t.Fatalf("replay with a different attempt err=%v, want denied", err)
		}
		if after := a.resolveState(t); !reflect.DeepEqual(before, after) {
			t.Fatal("refused replay wrote state")
		}
	})
}

// A foreign owner gets the generic denial whatever attempt id it names: the
// stale check runs after the owner check, so it learns nothing about another
// owner's pending attempt.
func TestActivationResolveAnchorForeignOwnerWrongAttemptDenied(t *testing.T) {
	a := newActivatedFixture(t, 156, true)
	a.blockViaTx2Failure(t)
	other := secondOwnerPrincipal(t, a.pool, NewAuthRepository(a.pool), 157)
	before := a.resolveState(t)
	for _, outcome := range []string{ActivationOutcomeActivated, ActivationOutcomeNotActivated, ActivationOutcomeNoActivation} {
		if err := a.repo.ResolveActivation(context.Background(), other, a.envID, a.attemptID+1000, outcome, resolveReason); !errors.Is(err, ErrServerAuthorizationDenied) {
			t.Fatalf("foreign owner wrong attempt %s err=%v, want denied", outcome, err)
		}
	}
	if after := a.resolveState(t); !reflect.DeepEqual(before, after) {
		t.Fatal("foreign owner wrote state")
	}
}

// Replay path (nothing pending): a foreign owner naming the resolved attempt
// gets the generic denial, not the idempotent success the owner would get.
func TestActivationResolveAnchorReplayForeignOwnerDenied(t *testing.T) {
	a := newActivatedFixture(t, 158, true)
	a.blockViaSweeperDiversion(t)
	if err := a.resolve(ActivationOutcomeNotActivated); err != nil {
		t.Fatal(err)
	}
	other := secondOwnerPrincipal(t, a.pool, NewAuthRepository(a.pool), 159)
	before := a.resolveState(t)
	if err := a.repo.ResolveActivation(context.Background(), other, a.envID, a.attemptID, ActivationOutcomeNotActivated, resolveReason); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("foreign owner replay err=%v, want denied", err)
	}
	if err := a.resolve(ActivationOutcomeNotActivated); err != nil {
		t.Fatalf("owner replay err=%v, want idempotent success", err)
	}
	if after := a.resolveState(t); !reflect.DeepEqual(before, after) {
		t.Fatal("replay wrote state")
	}
}
