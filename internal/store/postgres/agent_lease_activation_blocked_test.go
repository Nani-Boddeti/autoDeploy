//go:build integration

package postgres

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"

	"autodeploy/internal/queue"

	"github.com/jackc/pgx/v5/pgconn"
)

// secondAttempt leases a SECOND job on a's environment at the given fence and
// readies its deployment and candidate release, so it can call Activate while
// a's operation sits in whatever state the test left it. Only DB state is
// touched; a's own attempt is not closed, because the INSERT/index path under
// test does not consult it.
func (a activatedFixture) secondAttempt(t *testing.T, fence int64, key string) (f2 leaseFixture, attemptID int64) {
	t.Helper()
	f2 = a.leaseFixture
	f2.jobID = a.insertJob(t, a.serverID, a.envID, key, 0)
	attemptID = f2.leaseJob(t, fence, 90)
	f2.advanceDeploymentToHealthChecking(t)
	f2.insertCandidateRelease(t, attemptID, "candidate2-"+releaseSuffix(t))
	return f2, attemptID
}

// secondAttemptWithoutRelease is secondAttempt minus the candidate release
// row. Without the early environment-scoped refusal such an Activate would die
// later with a corrupt-state error at the candidate lookup, so a typed refusal
// can only come from a check that runs BEFORE that lookup.
func (a activatedFixture) secondAttemptWithoutRelease(t *testing.T, fence int64, key string) (f2 leaseFixture, attemptID int64) {
	t.Helper()
	f2 = a.leaseFixture
	f2.jobID = a.insertJob(t, a.serverID, a.envID, key, 0)
	attemptID = f2.leaseJob(t, fence, 90)
	f2.advanceDeploymentToHealthChecking(t)
	return f2, attemptID
}

func (a activatedFixture) setRouteHeadFlag(t *testing.T, flagged bool) {
	t.Helper()
	if !flagged {
		// Fixture routes start with the flag clear; the route-head trigger
		// rejects a no-op update, so there is nothing to do.
		if _, _, now := a.routeHead(t); now {
			t.Fatal("route head flag unexpectedly set; clearing it is not supported here")
		}
		return
	}
	if _, err := a.pool.Exec(context.Background(), `UPDATE route_heads SET reconciliation_required=$2, updated_at=clock_timestamp() WHERE environment_id=$1`, a.envID, flagged); err != nil {
		t.Fatalf("force route head flag=%v: %v", flagged, err)
	}
}

func (a activatedFixture) forceOperationState(t *testing.T, state string) {
	t.Helper()
	if state == "failed" {
		// failed is only reachable from reconciliation_required (migration 000021).
		a.forceOperationState(t, "reconciliation_required")
	}
	set := `state=$2, closed_at=clock_timestamp(), outcome_summary='forced for test'`
	if state == "failed" {
		set = `state=$2` // closed_at is immutable once set
	}
	if _, err := a.pool.Exec(context.Background(),
		`UPDATE release_operations SET `+set+` WHERE attempt_id=$1 AND kind='activate'`,
		a.attemptID, state); err != nil {
		t.Fatalf("force operation to %s: %v", state, err)
	}
}

func (a activatedFixture) activateOperationCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := a.pool.QueryRow(context.Background(), `SELECT count(*) FROM release_operations WHERE environment_id=$1 AND kind='activate'`, a.envID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A second Activate in an environment that holds an unresolved operation
// (state forced in SQL, route-head flag clear) must get the typed sentinel for
// BOTH open states and leave nothing behind. This is a forced-state rollback
// test: since the environment-scoped early return, the refusal comes from that
// read, NOT from the unique index (see
// TestActivateInsertBackstopRefusesIndependentlyOfEarlyReturn for the index).
func TestActivateForcedUnresolvedOperationRefusesAndLeavesNothingBehind(t *testing.T) {
	for i, state := range []string{"switch_authorized", "reconciliation_required"} {
		t.Run(state, func(t *testing.T) {
			a := newActivatedFixture(t, byte(190+i), false)
			if state != "switch_authorized" {
				// Forced in SQL, WITHOUT the route_heads/slot reconciliation
				// flags a real tx2 also sets.
				a.forceOperationState(t, state)
			}
			f2, attempt2 := a.secondAttempt(t, 2, "second-"+state)
			_, err := a.repo.Activate(context.Background(), a.serverID, a.cred, attempt2, 2, digest(77))
			if !errors.Is(err, ErrAgentLeaseActivationEnvironmentBlocked) {
				t.Fatalf("Activate err = %v, want ErrAgentLeaseActivationEnvironmentBlocked", err)
			}
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) {
				t.Fatalf("raw pg error escaped: %v", err)
			}
			if n := a.activateOperationCount(t); n != 1 {
				t.Fatalf("activate operations=%d, want 1 (rolled back)", n)
			}
			if st, _ := f2.releaseState(t, "candidate2-"+releaseSuffix(t)); st != "candidate" {
				t.Fatalf("second candidate=%s, want untouched candidate", st)
			}
			if got := f2.deploymentStatus(t); got != "health_checking" {
				t.Fatalf("second deployment=%s, want health_checking (rolled back)", got)
			}
		})
	}
}

// The rule is "this environment holds an unresolved activate operation",
// enforced by an environment-scoped read BEFORE any deployment write and
// independent of the route-head flag. The second attempt has NO candidate
// release row, so any path that gets past the refusal point dies with a
// corrupt-state error at the candidate lookup instead of the typed sentinel:
// that is what lets this test tell the early return from the INSERT backstop
// (both return the same sentinel with the same rollback). The matrix covers
// both open states with the flag clear (the state the sweeper gap really
// produces until step D) and set.
func TestActivateEnvironmentRefusalIsIndependentOfRouteHeadFlag(t *testing.T) {
	ctx := context.Background()
	seed := byte(220)
	for _, state := range []string{"switch_authorized", "reconciliation_required"} {
		for _, flagged := range []bool{false, true} {
			seed++
			t.Run(fmt.Sprintf("%s/flag_%v", state, flagged), func(t *testing.T) {
				a := newActivatedFixture(t, seed, false)
				if state != "switch_authorized" {
					a.forceOperationState(t, state)
				}
				a.setRouteHeadFlag(t, flagged)
				f2, attempt2 := a.secondAttemptWithoutRelease(t, 2, "second-matrix")
				_, err := a.repo.Activate(ctx, a.serverID, a.cred, attempt2, 2, digest(77))
				if !errors.Is(err, ErrAgentLeaseActivationEnvironmentBlocked) {
					t.Fatalf("Activate err = %v, want ErrAgentLeaseActivationEnvironmentBlocked", err)
				}
				if got := f2.deploymentStatus(t); got != "health_checking" {
					t.Fatalf("second deployment=%s, want health_checking (no write before the refusal)", got)
				}
				if n := a.activateOperationCount(t); n != 1 {
					t.Fatalf("activate operations=%d, want 1", n)
				}
			})
		}
	}
}

// The INSERT's unique-index violation is defence in depth behind the early
// return and must refuse on its own. The early read is made to see nothing: the
// stranded operation is resolved (succeeded) when Activate starts, and a rival
// unresolved operation is committed from outside at the "deployment" stage,
// i.e. after the early read and before the INSERT (the late arrival the early
// read cannot see). Removing the 23505 mapping turns this into a raw
// "insert activation operation" error.
func TestActivateInsertBackstopRefusesIndependentlyOfEarlyReturn(t *testing.T) {
	ctx := context.Background()
	a := newActivatedFixture(t, 230, false)
	if err := a.report(ctx, true, ""); err != nil {
		t.Fatal(err)
	}
	_, attempt2 := a.secondAttempt(t, 2, "second-backstop")
	fired := false
	a.repo.failAfter = func(stage string) error {
		if stage != "deployment" || fired {
			return nil
		}
		fired = true
		_, err := a.pool.Exec(ctx, `INSERT INTO release_operations(release_id, job_id, attempt_id, fence, kind, digest, state, environment_id, expected_route_revision, route_digest)
			SELECT release_id, job_id, attempt_id, fence, kind, digest, 'switch_authorized', environment_id, expected_route_revision, route_digest FROM release_operations WHERE attempt_id=$1 AND kind='activate'`, a.attemptID)
		return err
	}
	_, err := a.repo.Activate(ctx, a.serverID, a.cred, attempt2, 2, digest(78))
	if !fired {
		t.Fatal("rival insert hook never fired: the early read refused, so the INSERT was not exercised")
	}
	if !errors.Is(err, ErrAgentLeaseActivationEnvironmentBlocked) {
		t.Fatalf("Activate err = %v, want ErrAgentLeaseActivationEnvironmentBlocked from the INSERT backstop", err)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		t.Fatalf("raw pg error escaped: %v", err)
	}
	if n := a.activateOperationCount(t); n != 2 {
		t.Fatalf("activate operations=%d, want 2 (original + rival only; Activate's insert rolled back)", n)
	}
}

// The same attempt re-calling Activate: a live authorization replays exactly;
// an unresolved-but-no-longer-authorized operation gets the typed refusal,
// never a success-shaped authorization or a bare stale error; a resolved one
// keeps today's stale behaviour (the refusal must not over-reach).
func TestActivateReplayByOperationState(t *testing.T) {
	ctx := context.Background()

	t.Run("switch_authorized replays full authorization unchanged", func(t *testing.T) {
		f := newLeaseFixture(t, 196)
		cred := f.withCredential(t, 246)
		attemptID := f.leaseJob(t, 1, 90)
		f.advanceDeploymentToHealthChecking(t)
		f.insertRouteHead(t, "app.example.com")
		f.insertCandidateRelease(t, attemptID, "candidate-"+releaseSuffix(t))
		repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
		first, err := repo.Activate(ctx, f.serverID, cred, attemptID, 1, digest(196))
		if err != nil {
			t.Fatal(err)
		}
		second, err := repo.Activate(ctx, f.serverID, cred, attemptID, 1, digest(196))
		if err != nil {
			t.Fatalf("replay: %v", err)
		}
		if second != first || second.OperationID == "" || second.RouteDigest == "" || second.Hostname != "app.example.com" {
			t.Fatalf("replay = %+v, want identical to first %+v", second, first)
		}
	})

	t.Run("reconciliation_required is refused with the typed error", func(t *testing.T) {
		a := newActivatedFixture(t, 197, false)
		// Real ambiguous tx2 outcome: operation reconciliation_required, attempt
		// still leased, so the call reaches the replay branch.
		if err := a.report(ctx, false, "reload timed out"); err != nil {
			t.Fatal(err)
		}
		got, err := a.repo.Activate(ctx, a.serverID, a.cred, a.attemptID, 1, digest(197))
		if !errors.Is(err, ErrAgentLeaseActivationEnvironmentBlocked) {
			t.Fatalf("err = %v, want ErrAgentLeaseActivationEnvironmentBlocked", err)
		}
		if errors.Is(err, ErrAgentLeaseStale) {
			t.Fatalf("err = %v must not be the bare stale error", err)
		}
		if got != (queue.ActivationResult{}) {
			t.Fatalf("refusal returned a non-zero authorization: %+v", got)
		}
	})

	for i, state := range []string{"succeeded", "failed"} {
		t.Run(state+" stays stale", func(t *testing.T) {
			a := newActivatedFixture(t, byte(198+i), false)
			// Forced with the attempt still leased so the call reaches the
			// replay branch (a real tx2 closes the attempt first).
			a.forceOperationState(t, state)
			_, err := a.repo.Activate(ctx, a.serverID, a.cred, a.attemptID, 1, digest(byte(198+i)))
			if !errors.Is(err, ErrAgentLeaseStale) || errors.Is(err, ErrAgentLeaseActivationEnvironmentBlocked) {
				t.Fatalf("err = %v, want ErrAgentLeaseStale only", err)
			}
		})
	}
}

// A previous activation that is succeeded or failed must not wedge the
// environment: a new attempt activates normally. This is the over-reach guard.
func TestActivateNewAttemptAfterResolvedOperation(t *testing.T) {
	ctx := context.Background()
	for i, state := range []string{"succeeded", "failed"} {
		t.Run(state, func(t *testing.T) {
			a := newActivatedFixture(t, byte(200+i), false)
			if state == "succeeded" {
				if err := a.report(ctx, true, ""); err != nil {
					t.Fatal(err)
				}
			} else {
				a.forceOperationState(t, state)
			}
			if got, _, _ := a.operation(t); got != state {
				t.Fatalf("previous operation=%s, want %s", got, state)
			}
			_, attempt2 := a.secondAttempt(t, 2, "second-after-"+state)
			result, err := a.repo.Activate(ctx, a.serverID, a.cred, attempt2, 2, digest(88))
			if err != nil {
				t.Fatalf("Activate after %s operation: %v", state, err)
			}
			if result.OperationID == "" || result.RouteDigest == "" {
				t.Fatalf("result=%+v, want a full authorization", result)
			}
			if n := a.activateOperationCount(t); n != 2 {
				t.Fatalf("activate operations=%d, want 2", n)
			}
			if _, err := strconv.ParseInt(result.OperationID, 10, 64); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// The mapping is by constraint name, not SQLSTATE. Real 23505s from the real
// database: a duplicate on release_operations' OTHER unique constraint, and a
// duplicate against the open-activation index's name.
func TestIsUniqueViolationOfDistinguishesConstraints(t *testing.T) {
	ctx := context.Background()
	a := newActivatedFixture(t, 202, false)
	// Resolve the operation first, so the duplicate below is not also an open
	// activation and can only trip the OTHER unique constraint.
	if err := a.report(ctx, true, ""); err != nil {
		t.Fatal(err)
	}

	// Duplicate primary key: the only other unique constraint on
	// release_operations. The existing row is succeeded, so the copy (state
	// switch_authorized, open) can only collide on the primary key.
	_, otherErr := a.pool.Exec(ctx, `INSERT INTO release_operations(id, release_id, job_id, attempt_id, fence, kind, digest, state, environment_id, expected_route_revision, route_digest)
		OVERRIDING SYSTEM VALUE
		SELECT id, release_id, job_id, attempt_id, fence, kind, digest, 'switch_authorized', environment_id, expected_route_revision, route_digest FROM release_operations WHERE attempt_id=$1`, a.attemptID)
	var pgErr *pgconn.PgError
	if !errors.As(otherErr, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("setup: want a 23505 from the duplicate insert, got %v", otherErr)
	}
	t.Logf("other violation constraint=%q", pgErr.ConstraintName)
	if pgErr.ConstraintName == releaseOperationsOpenActivationIndex {
		t.Fatal("setup: duplicate unexpectedly hit the open-activation index; need a different constraint")
	}
	if !isUniqueViolation(otherErr) {
		t.Fatal("setup: isUniqueViolation must still match any 23505")
	}
	if isUniqueViolationOf(otherErr, releaseOperationsOpenActivationIndex) {
		t.Fatalf("a different unique constraint (%s) was classified as the open-activation index", pgErr.ConstraintName)
	}
	if !isUniqueViolationOf(otherErr, pgErr.ConstraintName) {
		t.Fatal("isUniqueViolationOf must match the constraint that actually fired")
	}
}

// strandedByRealCalls drives the production path to the stranded state with
// no forced operation or flag state: a real Activate, a real ambiguous
// ReportActivationResult (tx2 sets BOTH the operation and the route-head
// flag), the stranded attempt's authority lapsing and being closed by the
// real sweeper (ExpireBatch), and the administrator clearing the SLOT flag
// with the real MarkReconciled (which touches only the slot). What is left is
// exactly what a later Activate meets: route_heads.reconciliation_required
// set, the old operation reconciliation_required, and a free slot.
//
// Only the lapse of time is simulated (the attempt's deadlines are moved into
// the past); no state the code under test reads is written directly.
func (a activatedFixture) strandedByRealCalls(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if err := a.report(ctx, false, "reload timed out"); err != nil {
		t.Fatalf("ambiguous report: %v", err)
	}
	// Non-vacuity of the setup: tx2 really produced both facts.
	if state, _, _ := a.operation(t); state != "reconciliation_required" {
		t.Fatalf("operation=%s, want reconciliation_required", state)
	}
	if _, _, flagged := a.routeHead(t); !flagged {
		t.Fatal("route head flag not set by tx2")
	}
	if _, err := a.pool.Exec(ctx, `UPDATE agent_job_attempts SET lease_expires_at=now()-interval '2 seconds', authority_expires_at=now()-interval '2 seconds' WHERE id=$1`, a.attemptID); err != nil {
		t.Fatalf("lapse attempt deadlines: %v", err)
	}
	// ExpireBatch is global: in a shared database it may also expire other
	// tests' lapsed attempts, so assert THIS attempt was closed rather than
	// that exactly one was.
	if _, err := a.repo.ExpireBatch(ctx, 10); err != nil {
		t.Fatalf("ExpireBatch: %v", err)
	}
	a.requireAttemptExpired(t)
	if err := a.repo.MarkReconciled(ctx, a.principal, a.envID, "operator break-glass: verified clean"); err != nil {
		t.Fatalf("MarkReconciled: %v", err)
	}
	if _, current, _, slotFlag := a.slotRow(t); current != nil || slotFlag {
		t.Fatalf("slot current=%v flagged=%v, want free and clear", current, slotFlag)
	}
	if _, _, flagged := a.routeHead(t); !flagged {
		t.Fatal("route head flag must still be set (MarkReconciled clears only the slot)")
	}
}

func (a activatedFixture) requireAttemptExpired(t *testing.T) {
	t.Helper()
	var state string
	if err := a.pool.QueryRow(context.Background(), `SELECT state FROM agent_job_attempts WHERE id=$1`, a.attemptID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "expired" {
		t.Fatalf("stranded attempt state=%s, want expired (closed by the real sweeper)", state)
	}
}

// requireStrandedPrecondition asserts the full triple the end-to-end proofs
// rely on, immediately before the final Activate. It is the explicit coupling
// to step D/E/G: if a later step changes what the sweeper or MarkReconciled
// leave behind, this fails loudly instead of the proof silently re-routing
// through the INSERT backstop.
func (a activatedFixture) requireStrandedPrecondition(t *testing.T) {
	t.Helper()
	if state, _, _ := a.operation(t); !unresolvedActivation(state) {
		t.Fatalf("stranded operation state=%s, want unresolved", state)
	}
	if _, _, flagged := a.routeHead(t); !flagged {
		t.Fatal("route_heads.reconciliation_required must still be set")
	}
	if _, current, _, slotFlag := a.slotRow(t); current != nil || slotFlag {
		t.Fatalf("slot current=%v flagged=%v, want free and clear", current, slotFlag)
	}
}

// THE test the step exists for. The old gate answered ErrAgentLeaseStale here
// because the route head flag stops Activate before the INSERT is reached.
func TestActivateStrandedEnvironmentIsBlockedEndToEnd(t *testing.T) {
	ctx := context.Background()
	a := newActivatedFixture(t, 210, false)
	a.strandedByRealCalls(t)

	// Full triple BEFORE the second attempt is set up: leasing it occupies the
	// slot, which is part of the triple.
	a.requireStrandedPrecondition(t)
	f2, attempt2 := a.secondAttempt(t, 2, "second-stranded")
	got, err := a.repo.Activate(ctx, a.serverID, a.cred, attempt2, 2, digest(99))
	if !errors.Is(err, ErrAgentLeaseActivationEnvironmentBlocked) {
		t.Fatalf("Activate err = %v, want ErrAgentLeaseActivationEnvironmentBlocked", err)
	}
	if errors.Is(err, ErrAgentLeaseStale) {
		t.Fatalf("err = %v must not be stale", err)
	}
	if got != (queue.ActivationResult{}) {
		t.Fatalf("refusal returned a non-zero authorization: %+v", got)
	}
	if n := a.activateOperationCount(t); n != 1 {
		t.Fatalf("activate operations=%d, want 1 (nothing written)", n)
	}
	if st := f2.deploymentStatus(t); st != "health_checking" {
		t.Fatalf("second deployment=%s, want untouched health_checking", st)
	}
}

// The rule is "an unresolved activate operation exists", not "the flag is
// set". Flag set with NO unresolved operation (a state production does not
// currently produce, so it is forced) must keep today's stale answer.
func TestActivateRouteHeadFlagWithoutUnresolvedOperationStaysStale(t *testing.T) {
	ctx := context.Background()
	for i, state := range []string{"succeeded", "failed"} {
		t.Run(state, func(t *testing.T) {
			a := newActivatedFixture(t, byte(212+i), false)
			if state == "succeeded" {
				if err := a.report(ctx, true, ""); err != nil {
					t.Fatal(err)
				}
			} else {
				a.forceOperationState(t, state)
			}
			if _, err := a.pool.Exec(ctx, `UPDATE route_heads SET reconciliation_required=true, updated_at=clock_timestamp() WHERE environment_id=$1`, a.envID); err != nil {
				t.Fatalf("force route head flag: %v", err)
			}
			if _, _, flagged := a.routeHead(t); !flagged {
				t.Fatal("flag not set")
			}
			_, attempt2 := a.secondAttempt(t, 2, "second-flag-only-"+state)
			_, err := a.repo.Activate(ctx, a.serverID, a.cred, attempt2, 2, digest(98))
			if !errors.Is(err, ErrAgentLeaseStale) || errors.Is(err, ErrAgentLeaseActivationEnvironmentBlocked) {
				t.Fatalf("Activate err = %v, want ErrAgentLeaseStale only", err)
			}
		})
	}
}

// Reachable TODAY through the real sweeper: it closes an attempt whose
// operation is still switch_authorized (the step D gap), leaving the operation
// unresolved with the route-head flag CLEAR and the slot flagged. The admin's
// MarkReconciled then frees the slot. A new attempt without a candidate release
// row must still get the typed refusal, not a corrupt-state error.
//
// STEP D WILL REMOVE THIS SCENARIO (the sweeper will divert instead of
// closing). When it lands, re-point this test at whatever then strands an
// operation with the flag clear, or delete it with the other step-D coupling.
func TestActivateSweeperStrandedSwitchAuthorizedFlagClearIsBlocked(t *testing.T) {
	ctx := context.Background()
	a := newActivatedFixture(t, 240, false)
	if _, err := a.pool.Exec(ctx, `UPDATE agent_job_attempts SET lease_expires_at=now()-interval '2 seconds', authority_expires_at=now()-interval '2 seconds' WHERE id=$1`, a.attemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.repo.ExpireBatch(ctx, 10); err != nil {
		t.Fatalf("ExpireBatch: %v", err)
	}
	a.requireAttemptExpired(t)
	if err := a.repo.MarkReconciled(ctx, a.principal, a.envID, "operator break-glass: verified clean"); err != nil {
		t.Fatalf("MarkReconciled: %v", err)
	}
	// Precondition (step-D coupling): operation still switch_authorized, flag
	// clear, slot free and clear.
	if state, _, _ := a.operation(t); state != "switch_authorized" {
		t.Fatalf("operation=%s, want switch_authorized (sweeper gap)", state)
	}
	if _, _, flagged := a.routeHead(t); flagged {
		t.Fatal("route head flag unexpectedly set")
	}
	if _, current, _, slotFlag := a.slotRow(t); current != nil || slotFlag {
		t.Fatalf("slot current=%v flagged=%v, want free and clear", current, slotFlag)
	}
	_, attempt2 := a.secondAttemptWithoutRelease(t, 2, "second-sweeper-gap")
	_, err := a.repo.Activate(ctx, a.serverID, a.cred, attempt2, 2, digest(100))
	if !errors.Is(err, ErrAgentLeaseActivationEnvironmentBlocked) {
		t.Fatalf("Activate err = %v, want ErrAgentLeaseActivationEnvironmentBlocked", err)
	}
}
