//go:build integration

package postgres

import (
	"context"
	"encoding/hex"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"autodeploy/internal/deployment"

	"github.com/jackc/pgx/v5"
)

// Step D: the sweeper (expireOne) and revocation (revokeInFlightLeases) must
// not close an attempt that holds an UNRESOLVED activation. They record the
// block durably and leave everything else alone. Every test below drives the
// real Activate and the real sweeper / RevokeServer; nothing is forced except
// the lapse of time.

// rowJSON returns one row as JSON text so whole rows (every column, including
// updated_at) can be compared before and after.
func (a activatedFixture) rowJSON(t *testing.T, query string, args ...any) string {
	t.Helper()
	var out *string
	if err := a.pool.QueryRow(context.Background(), `SELECT row_to_json(x)::text FROM (`+query+`) x`, args...).Scan(&out); err != nil {
		t.Fatalf("row json %q: %v", query, err)
	}
	if out == nil {
		return "<none>"
	}
	return *out
}

type diversionSnapshot struct {
	attempt, slot, job, deployment, routeHead, operation string
	events                                               []string
}

func (a activatedFixture) snapshot(t *testing.T) diversionSnapshot {
	t.Helper()
	return diversionSnapshot{
		attempt:    a.rowJSON(t, `SELECT * FROM agent_job_attempts WHERE id=$1`, a.attemptID),
		slot:       a.rowJSON(t, `SELECT * FROM environment_slots WHERE environment_id=$1`, a.envID),
		job:        a.rowJSON(t, `SELECT * FROM agent_jobs WHERE id=$1`, a.jobID),
		deployment: a.rowJSON(t, `SELECT d.* FROM deployments d JOIN agent_jobs j ON j.deployment_id=d.id WHERE j.id=$1`, a.jobID),
		routeHead:  a.rowJSON(t, `SELECT * FROM route_heads WHERE environment_id=$1`, a.envID),
		operation:  a.rowJSON(t, `SELECT * FROM release_operations WHERE attempt_id=$1 AND kind='activate'`, a.attemptID),
		events:     a.jobEvents(t),
	}
}

// lapse moves the attempt's deadlines into the past (the only simulated thing).
func (a activatedFixture) lapse(t *testing.T, ago string) {
	t.Helper()
	if _, err := a.pool.Exec(context.Background(), `UPDATE agent_job_attempts SET lease_expires_at=now()-$2::interval, authority_expires_at=now()-$2::interval WHERE id=$1`, a.attemptID, ago); err != nil {
		t.Fatalf("lapse: %v", err)
	}
}

// revokeDirect runs the free function revokeInFlightLeases in its own
// transaction against the fixture's server. Used for repeat sweeps, since
// RevokeServer itself refuses an already revoked server.
func (a activatedFixture) revokeDirect(t *testing.T) error {
	t.Helper()
	ctx := context.Background()
	tx, err := a.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	if err = revokeInFlightLeases(ctx, tx, NewDeploymentRepository(a.pool), a.repo.inject, a.serverID, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// arm lapses the authority for the expireOne caller (revocation needs no
// lapse). It is separate from sweepVia so it can run BEFORE a before-snapshot.
func (a activatedFixture) arm(t *testing.T, caller string) {
	t.Helper()
	if caller == "expireOne" {
		a.lapse(t, "2 seconds")
	}
}

// sweepVia drives the named caller once and returns how many attempts the
// sweeper reports as closed (always 0 for revocation, which does not count).
func (a activatedFixture) sweepVia(t *testing.T, caller string) (int, error) {
	t.Helper()
	ctx := context.Background()
	switch caller {
	case "expireOne":
		return a.repo.ExpireBatch(ctx, 10)
	case "revoke":
		return 0, NewServerRepository(a.pool).RevokeServer(ctx, a.principal, a.serverID, "compromised host")
	}
	t.Fatalf("unknown caller %q", caller)
	return 0, nil
}

var sweepCallers = []string{"expireOne", "revoke"}

// assertDiverted asserts the FULL post-state of a diverted attempt against the
// snapshot taken before the sweep: every field, not a subset.
func (a activatedFixture) assertDiverted(t *testing.T, before diversionSnapshot) {
	t.Helper()
	after := a.snapshot(t)

	// Unchanged rows, whole-row equality (including updated_at).
	if after.attempt != before.attempt {
		t.Fatalf("attempt row changed:\nbefore %s\nafter  %s", before.attempt, after.attempt)
	}
	if after.job != before.job {
		t.Fatalf("job row changed:\nbefore %s\nafter  %s", before.job, after.job)
	}
	if after.deployment != before.deployment {
		t.Fatalf("deployment row changed:\nbefore %s\nafter  %s", before.deployment, after.deployment)
	}
	state, closedAt, _, _, _ := a.attemptRow(t, a.attemptID)
	if state != "leased" || closedAt != nil {
		t.Fatalf("attempt state=%s closedAt=%v, want leased/open", state, closedAt)
	}
	if jobState, _, _, count := a.jobRow(t); jobState != "leased" || count != 1 {
		t.Fatalf("job state=%s attempt_count=%d, want leased/1", jobState, count)
	}
	if got := a.deploymentStatus(t); got != deployment.StatusActivating {
		t.Fatalf("deployment=%s, want activating", got)
	}

	// Slot: flagged and STILL OCCUPIED, fence unchanged.
	fence, current, reconAttempt, flagged := a.slotRow(t)
	wantFence := int64(1)
	if fence != wantFence || current == nil || *current != a.attemptID || !flagged || reconAttempt == nil || *reconAttempt != a.attemptID {
		t.Fatalf("slot fence=%d current=%v flagged=%v reconciliation_attempt=%v, want fence 1, occupant %d, flagged, reconciliation_attempt %d", fence, current, flagged, reconAttempt, a.attemptID, a.attemptID)
	}

	// Operation: reconciliation_required WITH closed_at (check1) and the
	// sweeper's fixed summary.
	opState, opClosed, summary := a.operation(t)
	if opState != "reconciliation_required" || opClosed == nil || summary != activationBlockedByAuthorityEndSummary {
		t.Fatalf("operation state=%s closed_at=%v summary=%q, want reconciliation_required/closed/%q", opState, opClosed, summary, activationBlockedByAuthorityEndSummary)
	}

	// Route head flagged; revision and active release untouched.
	rev, _, headFlag := a.routeHead(t)
	if !headFlag {
		t.Fatal("route head not flagged")
	}
	if !strings.Contains(before.routeHead, `"reconciliation_required":false`) {
		t.Fatalf("setup: route head already flagged before the sweep: %s", before.routeHead)
	}
	if !strings.Contains(after.routeHead, `"route_revision":`+strconv.FormatInt(rev, 10)) || !strings.Contains(before.routeHead, `"route_revision":`+strconv.FormatInt(rev, 10)) {
		t.Fatalf("route revision moved:\nbefore %s\nafter  %s", before.routeHead, after.routeHead)
	}

	// Events: exactly one new event, the activation block; no expired/retry/dead.
	want := append(append([]string{}, before.events...), "activation_reconciliation_required")
	if !reflect.DeepEqual(after.events, want) {
		t.Fatalf("job events=%v, want %v", after.events, want)
	}
}

// newActivatedFixtureAtAttempt is newActivatedFixture for a job that is on its
// nth attempt (n >= 5 makes expiry dead-letter it, which is what transitions
// the deployment).
func newActivatedFixtureAtAttempt(t *testing.T, seed byte, attemptNumber int) activatedFixture {
	t.Helper()
	f := newLeaseFixture(t, seed)
	cred := f.withCredential(t, seed+50)
	attemptID := f.advanceToAttempt(t, attemptNumber, 1, 90)
	f.advanceDeploymentToHealthChecking(t)
	f.insertRouteHead(t, "app.example.com")
	candidateID := "candidate-" + releaseSuffix(t)
	f.insertCandidateRelease(t, attemptID, candidateID)
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	result, err := repo.Activate(context.Background(), f.serverID, cred, attemptID, 1, digest(seed))
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	raw, err := hex.DecodeString(strings.TrimPrefix(result.RouteDigest, "sha256:"))
	if err != nil || len(raw) != 32 {
		t.Fatalf("route digest %q: %v", result.RouteDigest, err)
	}
	return activatedFixture{leaseFixture: f, cred: cred, attemptID: attemptID, operationID: result.OperationID,
		routeDigest: [32]byte(raw), candidateID: candidateID, repo: repo}
}

// THE test the step exists for: a real Activate, the authority lapses (or the
// server is revoked), the REAL sweeper runs, and the activation is recorded
// durably while the attempt, fence, job and deployment are left alone.
func TestSweeperDivertsUnresolvedActivationAndRecordsIt(t *testing.T) {
	for i, caller := range sweepCallers {
		t.Run(caller, func(t *testing.T) {
			a := newActivatedFixture(t, byte(80+i), false)
			a.arm(t, caller)
			before := a.snapshot(t)
			processed, err := a.sweepVia(t, caller)
			if err != nil {
				t.Fatalf("sweep: %v", err)
			}
			// A diverted attempt was recorded, not closed, so it is not counted.
			if processed != 0 {
				t.Fatalf("sweeper reported %d closed attempts, want 0 (diverted, not closed)", processed)
			}
			a.assertDiverted(t, before)
		})
	}
	// A cancellation request must not defeat the diversion either: without it
	// the job would close as cancelled and the deployment transition.
	t.Run("expireOne/cancel requested", func(t *testing.T) {
		a := newActivatedFixture(t, 84, false)
		if _, err := a.pool.Exec(context.Background(), `UPDATE agent_jobs SET cancel_requested_at=clock_timestamp() WHERE id=$1`, a.jobID); err != nil {
			t.Fatal(err)
		}
		a.arm(t, "expireOne")
		before := a.snapshot(t)
		if _, err := a.sweepVia(t, "expireOne"); err != nil {
			t.Fatalf("sweep: %v", err)
		}
		a.assertDiverted(t, before)
	})
}

// D1: the SECOND sweep must be a no-op. After the first sweep the operation is
// reconciliation_required and the attempt is still leased with a lapsed
// deadline; a diversion keyed on switch_authorized would close the attempt
// here. The repeat calls bypass discovery (expireOne directly, and
// revokeInFlightLeases, which discovers by server) so the discovery exclusion
// cannot hide a defect in the diversion itself.
func TestSweeperSecondSweepIsNoOp(t *testing.T) {
	ctx := context.Background()
	for i, caller := range sweepCallers {
		t.Run(caller, func(t *testing.T) {
			a := newActivatedFixture(t, byte(86+i), false)
			a.arm(t, caller)
			before := a.snapshot(t)
			if _, err := a.sweepVia(t, caller); err != nil {
				t.Fatalf("first sweep: %v", err)
			}
			a.assertDiverted(t, before)
			afterFirst := a.snapshot(t)

			for round := 1; round <= 2; round++ {
				switch caller {
				case "expireOne":
					closed, err := a.repo.expireOne(ctx, a.attemptID)
					if err != nil || closed {
						t.Fatalf("repeat expireOne #%d = (%v, %v), want (false, nil)", round, closed, err)
					}
					if _, err := a.repo.ExpireBatch(ctx, 10); err != nil {
						t.Fatalf("repeat ExpireBatch #%d: %v", round, err)
					}
				case "revoke":
					if err := a.revokeDirect(t); err != nil {
						t.Fatalf("repeat revokeInFlightLeases #%d: %v", round, err)
					}
				}
				if got := a.snapshot(t); !reflect.DeepEqual(got, afterFirst) {
					t.Fatalf("repeat sweep #%d changed state:\nbefore %+v\nafter  %+v", round, afterFirst, got)
				}
			}
			// Spelled out, so a future change to the snapshot cannot hollow it.
			if state, closedAt, _, _, _ := a.attemptRow(t, a.attemptID); state != "leased" || closedAt != nil {
				t.Fatalf("attempt state=%s closedAt=%v, want still leased", state, closedAt)
			}
			if fence, current, _, flagged := a.slotRow(t); fence != 1 || current == nil || *current != a.attemptID || !flagged {
				t.Fatalf("slot fence=%d current=%v flagged=%v", fence, current, flagged)
			}
			if state, _, _ := a.operation(t); state != "reconciliation_required" {
				t.Fatalf("operation=%s, want reconciliation_required", state)
			}
			n := 0
			for _, e := range a.jobEvents(t) {
				if e == "activation_reconciliation_required" {
					n++
				}
			}
			if n != 1 {
				t.Fatalf("activation_reconciliation_required events=%d, want exactly 1", n)
			}
		})
	}
}

// The ambiguous tx2 path already leaves the operation reconciliation_required
// with the attempt leased: the sweeper must treat it identically (no-op), not
// close it. This is the production route to the same state.
func TestSweeperLeavesTx2RecordedAmbiguityAlone(t *testing.T) {
	ctx := context.Background()
	for i, caller := range sweepCallers {
		t.Run(caller, func(t *testing.T) {
			a := newActivatedFixture(t, byte(90+i), false)
			if err := a.report(ctx, false, "reload timed out"); err != nil {
				t.Fatal(err)
			}
			a.arm(t, caller)
			before := a.snapshot(t)
			// expireOne is called DIRECTLY (bypassing discovery, which already
			// excludes a recorded attempt) so only the diversion can leave the
			// attempt leased; the revoke caller has no discovery step.
			if caller == "expireOne" {
				if closed, err := a.repo.expireOne(ctx, a.attemptID); err != nil || closed {
					t.Fatalf("expireOne closed=%v err=%v, want not closed", closed, err)
				}
			} else if _, err := a.sweepVia(t, caller); err != nil {
				t.Fatalf("sweep: %v", err)
			}
			if after := a.snapshot(t); !reflect.DeepEqual(after, before) {
				t.Fatalf("sweep changed a tx2-recorded ambiguity:\nbefore %+v\nafter  %+v", before, after)
			}
			if state, _, _, _, _ := a.attemptRow(t, a.attemptID); state != "leased" {
				t.Fatalf("attempt state=%s, want leased", state)
			}
		})
	}
}

// assertClosedNormally asserts the ordinary close path ran: attempt expired,
// fence advanced, slot vacated and flagged, job dead-lettered at attempt 5,
// deployment transitioned to failed, expired+dead events.
func (a activatedFixture) assertClosedNormally(t *testing.T, wantOperation string) {
	t.Helper()
	state, closedAt, _, _, _ := a.attemptRow(t, a.attemptID)
	if state != "expired" || closedAt == nil {
		t.Fatalf("attempt state=%s closedAt=%v, want expired", state, closedAt)
	}
	fence, current, reconAttempt, flagged := a.slotRow(t)
	if fence != 2 || current != nil || !flagged || reconAttempt == nil || *reconAttempt != a.attemptID {
		t.Fatalf("slot fence=%d current=%v flagged=%v reconAttempt=%v, want fence 2, vacated, flagged", fence, current, flagged, reconAttempt)
	}
	if jobState, _, _, _ := a.jobRow(t); jobState != "dead" {
		t.Fatalf("job=%s, want dead", jobState)
	}
	if got := a.deploymentStatus(t); got != deployment.StatusFailed {
		t.Fatalf("deployment=%s, want failed", got)
	}
	events := strings.Join(a.jobEvents(t), ",")
	if !strings.Contains(events, "expired") || !strings.Contains(events, "dead") || strings.Contains(events, "activation_reconciliation_required") {
		t.Fatalf("job events=%s, want expired and dead and no activation block", events)
	}
	if wantOperation != "" {
		if state, _, _ := a.operation(t); state != wantOperation {
			t.Fatalf("operation=%s, want untouched %s", state, wantOperation)
		}
	}
	if _, _, headFlag := a.routeHead(t); headFlag {
		t.Fatal("route head was flagged by an ordinary close")
	}
}

// Over-reach guard: a RESOLVED activation (succeeded, and separately failed)
// must not stop the sweeper or revocation closing the attempt. A diversion
// that is too broad stops the sweeper closing anything.
func TestSweeperClosesAttemptWithResolvedActivation(t *testing.T) {
	for i, state := range []string{"succeeded", "failed"} {
		for j, caller := range sweepCallers {
			t.Run(state+"/"+caller, func(t *testing.T) {
				a := newActivatedFixtureAtAttempt(t, byte(100+i*2+j), queueMaxAttemptsForTest)
				// Forced with the attempt still leased (a real tx2 closes it).
				a.forceOperationState(t, state)
				a.arm(t, caller)
				if _, err := a.sweepVia(t, caller); err != nil {
					t.Fatalf("sweep: %v", err)
				}
				a.assertClosedNormally(t, state)
			})
		}
	}
}

// An attempt with no activation at all keeps closing normally.
func TestSweeperClosesAttemptWithNoActivation(t *testing.T) {
	for j, caller := range sweepCallers {
		t.Run(caller, func(t *testing.T) {
			f := newLeaseFixture(t, byte(110+j))
			f.withCredential(t, byte(160+j))
			attemptID := f.advanceToAttempt(t, queueMaxAttemptsForTest, 1, 90)
			f.advanceDeploymentToHealthChecking(t)
			a := activatedFixture{leaseFixture: f, attemptID: attemptID, repo: NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))}
			a.arm(t, caller)
			if _, err := a.sweepVia(t, caller); err != nil {
				t.Fatalf("sweep: %v", err)
			}
			state, _, _, _, _ := a.attemptRow(t, attemptID)
			fence, current, _, flagged := a.slotRow(t)
			if state != "expired" || fence != 2 || current != nil || !flagged {
				t.Fatalf("attempt=%s fence=%d current=%v flagged=%v, want expired/2/vacated/flagged", state, fence, current, flagged)
			}
			if jobState, _, _, _ := a.jobRow(t); jobState != "dead" {
				t.Fatalf("job=%s, want dead", jobState)
			}
			if got := a.deploymentStatus(t); got != deployment.StatusFailed {
				t.Fatalf("deployment=%s, want failed", got)
			}
		})
	}
}

// queueMaxAttemptsForTest is queue.MaxAttempts as an int literal the dead-letter
// tests above rely on (TestAgentLeaseExpireBatchRetryVsDeadAtFiveAttempts uses 5).
const queueMaxAttemptsForTest = 5

// D1b: diverted attempts stay leased with lapsed deadlines. Without the
// discovery exclusion they match every tick, sort first, and fill the LIMIT,
// so a genuinely expired lease behind them is never processed. The batch limit
// is 2 and two diverted attempts (older deadlines) sit in front of one real
// expired lease; the real one must still be closed within a few ticks, and the
// diverted ones must stay untouched across all of them.
func TestSweeperDivertedAttemptsDoNotStarveOtherExpiredLeases(t *testing.T) {
	ctx := context.Background()
	a := newActivatedFixture(t, 120, false)
	b := a.otherEnvironmentActivated(t, "environment-b", "app-b.example.com")

	// A plain expired lease on a third environment, with a LATER deadline than
	// the diverted pair so it sorts behind them.
	c := a.leaseFixture
	c.envID = "environment-c"
	c.jobID = c.insertJob(t, c.serverID, c.envID, "starvation-c", 0)
	cAttempt := c.leaseJob(t, 1, 90)

	a.lapse(t, "30 seconds")
	b.lapse(t, "30 seconds")
	if _, err := a.pool.Exec(ctx, `UPDATE agent_job_attempts SET lease_expires_at=now()-interval '1 second', authority_expires_at=now()-interval '1 second' WHERE id=$1`, cAttempt); err != nil {
		t.Fatal(err)
	}

	closedAt := -1
	for tick := 1; tick <= 4; tick++ {
		if _, err := a.repo.ExpireBatch(ctx, 2); err != nil {
			t.Fatalf("tick %d: %v", tick, err)
		}
		if state, _, _, _, _ := c.attemptRow(t, cAttempt); state == "expired" && closedAt < 0 {
			closedAt = tick
		}
	}
	if closedAt < 0 {
		t.Fatal("the genuinely expired lease was never processed: diverted attempts starve the batch")
	}
	if closedAt > 2 {
		t.Fatalf("expired lease processed only on tick %d, want <= 2 (tick 1 records the two diverted attempts)", closedAt)
	}
	// Diverted ones: recorded exactly once, then untouched by the later ticks.
	for name, f := range map[string]activatedFixture{"a": a, "b": b} {
		if state, _, _, _, _ := f.attemptRow(t, f.attemptID); state != "leased" {
			t.Fatalf("diverted attempt %s state=%s, want leased", name, state)
		}
		if st, _, _ := f.operation(t); st != "reconciliation_required" {
			t.Fatalf("diverted attempt %s operation=%s, want reconciliation_required", name, st)
		}
	}
}

// otherEnvironmentActivated builds a second, independent activated attempt on
// another environment of the same server (the fixture is single-environment).
func (a activatedFixture) otherEnvironmentActivated(t *testing.T, envID, hostname string) activatedFixture {
	t.Helper()
	f := a.leaseFixture
	f.envID = envID
	f.jobID = f.insertJob(t, f.serverID, envID, "other-"+envID, 0)
	attemptID := f.leaseJob(t, 1, 90)
	f.advanceDeploymentToHealthChecking(t)
	f.insertRouteHead(t, hostname)
	candidateID := "candidate-" + envID + "-" + releaseSuffix(t)
	f.insertCandidateRelease(t, attemptID, candidateID)
	result, err := a.repo.Activate(context.Background(), f.serverID, a.cred, attemptID, 1, digest(121))
	if err != nil {
		t.Fatalf("Activate on %s: %v", envID, err)
	}
	raw, err := hex.DecodeString(strings.TrimPrefix(result.RouteDigest, "sha256:"))
	if err != nil || len(raw) != 32 {
		t.Fatalf("route digest %q: %v", result.RouteDigest, err)
	}
	return activatedFixture{leaseFixture: f, cred: a.cred, attemptID: attemptID, operationID: result.OperationID,
		routeDigest: [32]byte(raw), candidateID: candidateID, repo: a.repo}
}

// Atomicity: a failure injected at each stage of the diverted path (both
// callers) leaves NOTHING committed -- every row identical to before -- and
// the stage must have really fired (non-vacuity). A clean retry then records
// the block, so the rollback did not poison the state.
func TestSweeperDiversionIsAtomicAtEveryStage(t *testing.T) {
	stages := []string{"operation_lock", "operation_close", "route_head_flag", "slot_reconcile", "job_event"}
	seed := byte(130)
	for _, caller := range sweepCallers {
		for _, stage := range stages {
			seed++
			t.Run(caller+"/"+stage, func(t *testing.T) {
				a := newActivatedFixture(t, seed, false)
				a.arm(t, caller)
				before := a.snapshot(t)
				fired := false
				hook := func(s string) error {
					if s == stage && !fired {
						fired = true
						return errors.New("injected failure at " + stage)
					}
					return nil
				}
				var err error
				switch caller {
				case "expireOne":
					a.repo.failAfter = hook
					_, err = a.repo.ExpireBatch(context.Background(), 10)
					a.repo.failAfter = nil
				case "revoke":
					servers := NewServerRepository(a.pool)
					servers.failAfter = hook
					err = servers.RevokeServer(context.Background(), a.principal, a.serverID, "compromised host")
				}
				if !fired {
					t.Fatalf("stage %q never fired: the diverted path does not reach it", stage)
				}
				if err == nil || !strings.Contains(err.Error(), "injected failure at "+stage) {
					t.Fatalf("err = %v, want the injected failure at %s", err, stage)
				}
				if after := a.snapshot(t); !reflect.DeepEqual(after, before) {
					t.Fatalf("partial commit after failure at %s:\nbefore %+v\nafter  %+v", stage, before, after)
				}
				// Recovery: the same call without the failure records the block.
				if _, err := a.sweepVia(t, caller); err != nil {
					t.Fatalf("retry sweep: %v", err)
				}
				a.assertDiverted(t, before)
			})
		}
	}
}

// Lock order, with REAL contention. The diversion takes the environment_slots
// lock first (ADR 0004 forward constraint): while another transaction holds
// the slot row, expireOne (SKIP LOCKED) must skip and write nothing, and
// RevokeServer (FOR UPDATE) must WAIT behind it and only then record the
// block. A diversion that took the operation row before the slot would not
// block here but could deadlock against tx2/Activate, which is why the order
// is asserted from the wait itself.
func TestSweeperDiversionTakesSlotLockFirst(t *testing.T) {
	ctx := context.Background()
	t.Run("expireOne skips a held slot", func(t *testing.T) {
		a := newActivatedFixture(t, 150, false)
		a.lapse(t, "2 seconds")
		before := a.snapshot(t)
		holder, err := a.pool.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer holder.Rollback(ctx) //nolint:errcheck
		if _, err = holder.Exec(ctx, `SELECT 1 FROM environment_slots WHERE environment_id=$1 FOR UPDATE`, a.envID); err != nil {
			t.Fatal(err)
		}
		// A bounded context: a regression to a blocking slot lock would
		// otherwise hang here forever, since the holder is this goroutine.
		boundedCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		closed, err := a.repo.expireOne(boundedCtx, a.attemptID)
		if err != nil || closed {
			t.Fatalf("expireOne with the slot held = (%v, %v), want (false, nil)", closed, err)
		}
		if after := a.snapshot(t); !reflect.DeepEqual(after, before) {
			t.Fatalf("expireOne wrote while the slot was held:\nbefore %+v\nafter  %+v", before, after)
		}
		holder.Rollback(ctx) //nolint:errcheck
		if _, err := a.repo.ExpireBatch(ctx, 10); err != nil {
			t.Fatal(err)
		}
		if state, _, _ := a.operation(t); state != "reconciliation_required" {
			t.Fatalf("operation=%s after the slot was released, want reconciliation_required", state)
		}
	})

	t.Run("revoke waits for the slot then diverts", func(t *testing.T) {
		a := newActivatedFixture(t, 151, false)
		before := a.snapshot(t)
		holder, err := a.pool.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer holder.Rollback(ctx) //nolint:errcheck
		if _, err = holder.Exec(ctx, `SELECT 1 FROM environment_slots WHERE environment_id=$1 FOR UPDATE`, a.envID); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var revokeErr error
		wg.Add(1)
		go func() {
			defer wg.Done()
			revokeErr = NewServerRepository(a.pool).RevokeServer(ctx, a.principal, a.serverID, "compromised host")
		}()
		// The revoker must be BLOCKED on the slot (a lock wait), not finished.
		deadline := time.Now().Add(10 * time.Second)
		for {
			var waiting int
			if err := a.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type='Lock' AND query ILIKE '%environment_slots%FOR UPDATE%' AND pid<>pg_backend_pid()`).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if waiting > 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("RevokeServer never blocked on the held slot lock")
			}
			time.Sleep(20 * time.Millisecond)
		}
		if state, _, _ := a.operation(t); state != "switch_authorized" {
			t.Fatalf("operation=%s while the revoker waits, want untouched switch_authorized", state)
		}
		holder.Rollback(ctx) //nolint:errcheck
		wg.Wait()
		if revokeErr != nil {
			t.Fatalf("RevokeServer: %v", revokeErr)
		}
		a.assertDiverted(t, before)
	})
}

// The operation row is locked AFTER the slot (position 5 vs 1), and the
// diversion waits for it while still holding the slot. Proven from the wait:
// with the operation row held by another transaction, the sweeping transaction
// blocks on release_operations, and at that moment the slot row lock is taken
// (a NOWAIT lock attempt from a third connection is refused). A diversion that
// read the operation before locking the slot would not hold the slot here.
func TestSweeperDiversionWaitsForOperationHoldingTheSlot(t *testing.T) {
	ctx := context.Background()
	for i, caller := range sweepCallers {
		t.Run(caller, func(t *testing.T) {
			a := newActivatedFixture(t, byte(160+i), false)
			a.arm(t, caller)
			before := a.snapshot(t)
			holder, err := a.pool.BeginTx(ctx, pgx.TxOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer holder.Rollback(ctx) //nolint:errcheck
			if _, err = holder.Exec(ctx, `SELECT 1 FROM release_operations WHERE attempt_id=$1 AND kind='activate' FOR UPDATE`, a.attemptID); err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			var sweepErr error
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, sweepErr = a.sweepVia(t, caller)
			}()
			deadline := time.Now().Add(10 * time.Second)
			for {
				var waiting int
				if err := a.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type='Lock' AND query ILIKE '%FROM release_operations WHERE attempt_id%FOR UPDATE%'`).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting > 0 {
					break
				}
				if time.Now().After(deadline) {
					holder.Rollback(ctx) //nolint:errcheck
					wg.Wait()
					t.Fatal("the sweeping transaction never waited on the operation row")
				}
				time.Sleep(20 * time.Millisecond)
			}
			probe, err := a.pool.BeginTx(ctx, pgx.TxOptions{})
			if err != nil {
				t.Fatal(err)
			}
			_, probeErr := probe.Exec(ctx, `SELECT 1 FROM environment_slots WHERE environment_id=$1 FOR UPDATE NOWAIT`, a.envID)
			probe.Rollback(ctx) //nolint:errcheck
			if probeErr == nil || !strings.Contains(probeErr.Error(), "55P03") {
				holder.Rollback(ctx) //nolint:errcheck
				wg.Wait()
				t.Fatalf("slot row was lockable while the sweeper waited on the operation (err=%v): the operation is not taken under the slot lock", probeErr)
			}
			holder.Rollback(ctx) //nolint:errcheck
			wg.Wait()
			if sweepErr != nil {
				t.Fatalf("sweep: %v", sweepErr)
			}
			a.assertDiverted(t, before)
		})
	}
}

// startedActivationOnNewEnvironment leases an attempt on a new environment of
// the same server and SQL-inserts a `started` activate operation for it (a
// state no code path produces, but one release_operations legally holds: its
// closed_at is NULL, which is exactly what a closed_at-based discovery
// exclusion gets wrong). The attempt's authority is lapsed by `ago`.
func (a activatedFixture) startedActivationOnNewEnvironment(t *testing.T, envID, ago string) int64 {
	t.Helper()
	f := a.leaseFixture
	f.envID = envID
	f.jobID = f.insertJob(t, f.serverID, envID, "started-"+envID, 0)
	attemptID := f.leaseJob(t, 1, 90)
	f.advanceDeploymentToHealthChecking(t)
	candidateID := "candidate-" + envID + "-" + releaseSuffix(t)
	f.insertCandidateRelease(t, attemptID, candidateID)
	d := digest(7)
	if _, err := a.pool.Exec(context.Background(), `INSERT INTO release_operations(release_id, job_id, attempt_id, fence, kind, digest, state, environment_id, expected_route_revision, route_digest)
		VALUES ($1,$2,$3,1,'activate',$4,'started',$5,0,$4)`, candidateID, f.jobID, attemptID, d[:], envID); err != nil {
		t.Fatalf("insert started activate operation: %v", err)
	}
	if _, err := a.pool.Exec(context.Background(), `UPDATE agent_job_attempts SET lease_expires_at=now()-$2::interval, authority_expires_at=now()-$2::interval WHERE id=$1`, attemptID, ago); err != nil {
		t.Fatalf("lapse started attempt: %v", err)
	}
	return attemptID
}

// A `started` activate row is unresolved but has NO legal transition into
// reconciliation_required, so the diversion is a no-op for it. The discovery
// exclusion must therefore skip it (it is derived from "unresolved AND not the
// block's source state"); excluding on closed_at would leave it discoverable
// (closed_at IS NULL for started), and `limit` such attempts, sorting first on
// their old deadlines, would starve every plain expired lease forever.
func TestExpireBatchStartedActivationsDoNotStarvePlainExpiredLeases(t *testing.T) {
	ctx := context.Background()
	a := newActivatedFixture(t, 125, false)
	startedB := a.startedActivationOnNewEnvironment(t, "environment-started-b", "30 seconds")
	startedD := a.startedActivationOnNewEnvironment(t, "environment-started-d", "30 seconds")

	c := a.leaseFixture
	c.envID = "environment-started-c"
	c.jobID = c.insertJob(t, c.serverID, c.envID, "started-plain-c", 0)
	cAttempt := c.leaseJob(t, 1, 90)
	if _, err := a.pool.Exec(ctx, `UPDATE agent_job_attempts SET lease_expires_at=now()-interval '1 second', authority_expires_at=now()-interval '1 second' WHERE id=$1`, cAttempt); err != nil {
		t.Fatal(err)
	}

	// limit 2 == the number of started attempts that sort ahead of C.
	if _, err := a.repo.ExpireBatch(ctx, 2); err != nil {
		t.Fatalf("ExpireBatch: %v", err)
	}
	if state, _, _, _, _ := c.attemptRow(t, cAttempt); state != "expired" {
		t.Fatalf("plain expired lease state=%s after one tick, want expired: started activations starve the batch", state)
	}
	for _, id := range []int64{startedB, startedD} {
		if state, _, _, _, _ := c.attemptRow(t, id); state != "leased" {
			t.Fatalf("started-activation attempt %d state=%s, want left leased", id, state)
		}
	}
}

// markReconciledRefusalSnapshot returns what a refused MarkReconciled must leave
// untouched.
func (a activatedFixture) markReconciledRefusalSnapshot(t *testing.T) (slot string, events []string) {
	t.Helper()
	return a.rowJSON(t, `SELECT * FROM environment_slots WHERE environment_id=$1`, a.envID), a.jobEvents(t)
}

// MarkReconciled must refuse while an unresolved activate operation exists
// (switch_authorized AND reconciliation_required), leave the slot untouched,
// and report the typed error. reconciliation_required is reached through the
// real tx2 ambiguity path (which flags the slot with the attempt);
// switch_authorized is not reachable with a flagged slot since step D, so its
// legacy close is forced in SQL.
func TestMarkReconciledRefusesWhileActivationUnresolved(t *testing.T) {
	ctx := context.Background()
	for i, state := range []string{"reconciliation_required", "switch_authorized"} {
		t.Run(state, func(t *testing.T) {
			a := newActivatedFixture(t, byte(126+i), false)
			if state == "reconciliation_required" {
				if err := a.report(ctx, false, "reload timed out"); err != nil {
					t.Fatal(err)
				}
			} else {
				a.forceLegacySweeperClose(t)
			}
			if got, _, _ := a.operation(t); got != state {
				t.Fatalf("operation=%s, want %s", got, state)
			}
			if _, _, _, flagged := a.slotRow(t); !flagged {
				t.Fatal("setup: slot flag must be set so the refusal is the activation check, not the generic denial")
			}
			slotBefore, eventsBefore := a.markReconciledRefusalSnapshot(t)
			err := a.repo.MarkReconciled(ctx, a.principal, a.envID, "operator break-glass: verified clean")
			if !errors.Is(err, ErrAgentLeaseActivationUnresolved) {
				t.Fatalf("MarkReconciled err = %v, want ErrAgentLeaseActivationUnresolved", err)
			}
			if errors.Is(err, ErrServerAuthorizationDenied) {
				t.Fatalf("err = %v must be the typed refusal, not the generic denial", err)
			}
			slotAfter, eventsAfter := a.markReconciledRefusalSnapshot(t)
			if slotAfter != slotBefore || !reflect.DeepEqual(eventsAfter, eventsBefore) {
				t.Fatalf("refused MarkReconciled changed state:\nslot before %s\nslot after  %s\nevents before %v after %v", slotBefore, slotAfter, eventsBefore, eventsAfter)
			}
		})
	}
}

// No over-reach: MarkReconciled still works for a reconciliation block that
// no unresolved activation caused: an ordinary expiry close (no activation at
// all) and an activation that has RESOLVED (terminal operation, slot flagged).
func TestMarkReconciledStillClearsNonActivationBlock(t *testing.T) {
	ctx := context.Background()
	t.Run("no activation", func(t *testing.T) {
		f := newLeaseFixture(t, 130)
		attemptID := f.leaseJob(t, 1, 90)
		if _, err := f.pool.Exec(ctx, `UPDATE agent_job_attempts SET lease_expires_at=now()-interval '2 seconds', authority_expires_at=now()-interval '2 seconds' WHERE id=$1`, attemptID); err != nil {
			t.Fatal(err)
		}
		repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
		if closed, err := repo.expireOne(ctx, attemptID); err != nil || !closed {
			t.Fatalf("expireOne closed=%v err=%v, want the ordinary close", closed, err)
		}
		if _, _, _, flagged := f.slotRow(t); !flagged {
			t.Fatal("setup: ordinary close must flag the slot")
		}
		if err := repo.MarkReconciled(ctx, f.principal, f.envID, "operator break-glass: agent unreachable"); err != nil {
			t.Fatalf("MarkReconciled on a non-activation block: %v", err)
		}
		if _, _, _, flagged := f.slotRow(t); flagged {
			t.Fatal("slot flag must be cleared")
		}
	})
	for i, terminal := range []string{"succeeded", "failed"} {
		t.Run("resolved "+terminal, func(t *testing.T) {
			a := newActivatedFixture(t, byte(131+i), false)
			if terminal == "succeeded" {
				if err := a.report(ctx, true, ""); err != nil {
					t.Fatal(err)
				}
			} else {
				a.forceOperationState(t, terminal)
			}
			if _, err := a.pool.Exec(ctx, `UPDATE environment_slots SET reconciliation_required=true, reconciliation_attempt_id=$2 WHERE environment_id=$1`, a.envID, a.attemptID); err != nil {
				t.Fatalf("flag slot: %v", err)
			}
			if err := a.repo.MarkReconciled(ctx, a.principal, a.envID, "operator break-glass: verified clean"); err != nil {
				t.Fatalf("MarkReconciled with a %s activation: %v", terminal, err)
			}
			if _, _, _, flagged := a.slotRow(t); flagged {
				t.Fatal("slot flag must be cleared")
			}
		})
	}
}
