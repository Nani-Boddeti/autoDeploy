//go:build integration

package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"autodeploy/internal/deployment"

	"github.com/jackc/pgx/v5/pgconn"
)

// assertReconciliationRecorded asserts what every deterministic-failure case
// must leave committed, whatever else differs per case: the operation closed
// as reconciliation_required with evidence naming the discrepancy, the slot
// flagged WITH this attempt's id, exactly one
// 'activation_reconciliation_required' job event, and nothing that resolves
// the ambiguity (attempt/job/slot occupancy untouched, no 'activated' route
// event, no terminal job event).
func (a activatedFixture) assertReconciliationRecorded(t *testing.T, wantEvidence string, wantLateReports int) {
	t.Helper()
	state, closedAt, summary := a.operation(t)
	if state != "reconciliation_required" || closedAt == nil {
		t.Fatalf("operation state=%s closedAt=%v, want reconciliation_required closed", state, closedAt)
	}
	if !strings.Contains(summary, wantEvidence) {
		t.Fatalf("evidence=%q, want it to name %q", summary, wantEvidence)
	}
	fence, current, reconAttempt, reconciling := a.slotRow(t)
	if fence != 1 || current == nil || *current != a.attemptID || !reconciling || reconAttempt == nil || *reconAttempt != a.attemptID {
		t.Fatalf("slot fence=%d current=%v reconciling=%v reconAttempt=%v, want flagged with attempt %d", fence, current, reconciling, reconAttempt, a.attemptID)
	}
	if attemptState, attemptClosed, _, resultOp, _ := a.attemptRow(t, a.attemptID); attemptState != "leased" || attemptClosed != nil || resultOp != nil {
		t.Fatalf("attempt state=%s closedAt=%v resultOp=%v, want untouched leased", attemptState, attemptClosed, resultOp)
	}
	if jobState, _, _, _ := a.jobRow(t); jobState != "leased" {
		t.Fatalf("job state=%s, want leased", jobState)
	}
	if n := a.activatedRouteEvents(t); n != 0 {
		t.Fatalf("activated route events=%d, want 0", n)
	}
	blocks, lates := 0, 0
	for _, e := range a.jobEvents(t) {
		switch e {
		case "activation_reconciliation_required":
			blocks++
		case "activation_late_report":
			lates++
		case "activation_authorized":
		default:
			t.Fatalf("unexpected job event %q in %v", e, a.jobEvents(t))
		}
	}
	if blocks != 1 {
		t.Fatalf("activation_reconciliation_required events=%d, want exactly 1 (%v)", blocks, a.jobEvents(t))
	}
	if lates != wantLateReports {
		t.Fatalf("activation_late_report events=%d, want %d (%v)", lates, wantLateReports, a.jobEvents(t))
	}
}

// withTriggerDisabled runs fn with one trigger of one table disabled and
// always re-enables it. These tests run serially (-p 1 and no t.Parallel in
// the package), so the brief window cannot affect another test.
func withTriggerDisabled(t *testing.T, a activatedFixture, table, trigger string, fn func()) {
	t.Helper()
	ctx := context.Background()
	if _, err := a.pool.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s DISABLE TRIGGER %s`, table, trigger)); err != nil {
		t.Fatalf("disable %s.%s: %v", table, trigger, err)
	}
	defer func() {
		if _, err := a.pool.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s ENABLE TRIGGER %s`, table, trigger)); err != nil {
			t.Fatalf("re-enable %s.%s: %v", table, trigger, err)
		}
	}()
	fn()
}

// installCommitTimeFailure creates a REAL deferred constraint trigger, in the
// live database, that raises when THIS operation is updated to 'succeeded'.
// PostgreSQL defers it to COMMIT, so the failure surfaces from Commit with
// SQLSTATE P0001 exactly as a production deferred trigger would. It is
// test-only schema (no production deferred trigger can be made to fail from
// inside finalizeActivationSucceeded, which holds every lock they read), and
// it is dropped on cleanup. It does not fire for the later
// reconciliation_required write.
func (a activatedFixture) installCommitTimeFailure(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	name := "test_activation_commit_failure_" + a.operationID
	drop := func() {
		_, _ = a.pool.Exec(ctx, `DROP TRIGGER IF EXISTS `+name+` ON release_operations`)
		_, _ = a.pool.Exec(ctx, `DROP FUNCTION IF EXISTS `+name+`()`)
	}
	drop()
	if _, err := a.pool.Exec(ctx, `CREATE FUNCTION `+name+`() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
		IF NEW.state = 'succeeded' THEN RAISE EXCEPTION 'test deferred trigger refuses operation %', NEW.id; END IF;
		RETURN NULL; END $$`); err != nil {
		t.Fatalf("create test trigger function: %v", err)
	}
	t.Cleanup(drop)
	if _, err := a.pool.Exec(ctx, `CREATE CONSTRAINT TRIGGER `+name+` AFTER UPDATE ON release_operations DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.id = `+a.operationID+`) EXECUTE FUNCTION `+name+`()`); err != nil {
		t.Fatalf("create test deferred trigger: %v", err)
	}
}

func (a activatedFixture) setDeploymentStatus(t *testing.T, to deployment.Status) {
	t.Helper()
	ctx := context.Background()
	var deploymentID string
	if err := a.pool.QueryRow(ctx, `SELECT deployment_id FROM agent_jobs WHERE id=$1`, a.jobID).Scan(&deploymentID); err != nil {
		t.Fatal(err)
	}
	repo := NewDeploymentRepository(a.pool)
	dep, err := repo.GetByID(ctx, deployment.ID(deploymentID))
	if err != nil {
		t.Fatal(err)
	}
	var now = dep.UpdatedAt()
	next, err := dep.Transition(to, now)
	if err != nil {
		t.Fatalf("transition to %s: %v", to, err)
	}
	if err := repo.Save(ctx, next); err != nil {
		t.Fatalf("save %s: %v", to, err)
	}
}

// TestReportActivationResultDeterministicFinalizeFailuresBlock: once a success
// report has passed the fence/occupant checks, every deterministic failure of
// the finalize sequence must commit a reconciliation block (operation closed,
// slot flagged with the attempt id, one event, evidence naming the
// discrepancy) AND still return the typed non-retryable error. Rolling back
// would leave the operation open with no flag while the host may already have
// switched.
func TestReportActivationResultDeterministicFinalizeFailuresBlock(t *testing.T) {
	ctx := context.Background()

	t.Run("candidate release not ready", func(t *testing.T) {
		a := newActivatedFixture(t, 150, true)
		if _, err := a.pool.Exec(ctx, `UPDATE releases SET state='failed' WHERE id=$1`, a.candidateID); err != nil {
			t.Fatalf("fail candidate: %v", err)
		}
		err := a.report(ctx, true, "")
		if !errors.Is(err, ErrAgentLeaseCorruptState) {
			t.Fatalf("err=%v, want ErrAgentLeaseCorruptState", err)
		}
		a.assertReconciliationRecorded(t, fmt.Sprintf(`candidate release %s is "failed"`, a.candidateID), 0)
		// Everything finalize wrote before it noticed was rolled back.
		if got := a.deploymentStatus(t); got != deployment.StatusActivating {
			t.Fatalf("deployment=%s, want activating", got)
		}
		if st, _ := a.releaseState(t, a.predecessorID); st != "active" {
			t.Fatalf("predecessor=%s, want active", st)
		}
		revision, active, routeReconciling := a.routeHead(t)
		if revision != 1 || active == nil || *active != a.predecessorID || !routeReconciling {
			t.Fatalf("route head revision=%d active=%v reconciling=%v, want 1 %s true", revision, active, routeReconciling, a.predecessorID)
		}
	})

	t.Run("predecessor release not active", func(t *testing.T) {
		a := newActivatedFixture(t, 151, true)
		// The deferred reference trigger would reject this state at the
		// setup statement's own commit (the head still names the
		// predecessor), so it is disabled for that one statement.
		withTriggerDisabled(t, a, "releases", "releases_active_reference_after_update", func() {
			if _, err := a.pool.Exec(ctx, `UPDATE releases SET state='retained' WHERE id=$1`, a.predecessorID); err != nil {
				t.Fatalf("retain predecessor: %v", err)
			}
		})
		err := a.report(ctx, true, "")
		if !errors.Is(err, ErrAgentLeaseCorruptState) {
			t.Fatalf("err=%v, want ErrAgentLeaseCorruptState", err)
		}
		a.assertReconciliationRecorded(t, fmt.Sprintf(`predecessor release %s is "retained"`, a.predecessorID), 0)
		if st, _ := a.releaseState(t, a.candidateID); st != "ready" {
			t.Fatalf("candidate=%s, want ready", st)
		}
		if got := a.deploymentStatus(t); got != deployment.StatusActivating {
			t.Fatalf("deployment=%s, want activating", got)
		}
	})

	t.Run("deployment already terminal", func(t *testing.T) {
		a := newActivatedFixture(t, 152, true)
		a.setDeploymentStatus(t, deployment.StatusFailed)
		err := a.report(ctx, true, "")
		if !errors.Is(err, ErrAgentLeaseCorruptState) || !errors.Is(err, deployment.ErrTerminalState) {
			t.Fatalf("err=%v, want ErrAgentLeaseCorruptState wrapping ErrTerminalState", err)
		}
		a.assertReconciliationRecorded(t, "transition deployment to healthy", 0)
		if got := a.deploymentStatus(t); got != deployment.StatusFailed {
			t.Fatalf("deployment=%s, want the pre-existing failed untouched", got)
		}
	})

	t.Run("route head missing", func(t *testing.T) {
		a := newActivatedFixture(t, 153, false)
		withTriggerDisabled(t, a, "route_heads", "route_heads_no_delete", func() {
			if _, err := a.pool.Exec(ctx, `DELETE FROM route_heads WHERE environment_id=$1`, a.envID); err != nil {
				t.Fatalf("delete route head: %v", err)
			}
		})
		err := a.report(ctx, true, "")
		if !errors.Is(err, ErrAgentLeaseCorruptState) {
			t.Fatalf("err=%v, want ErrAgentLeaseCorruptState", err)
		}
		a.assertReconciliationRecorded(t, fmt.Sprintf("environment %s has no route head", a.envID), 0)
		var heads int
		if err := a.pool.QueryRow(ctx, `SELECT count(*) FROM route_heads WHERE environment_id=$1`, a.envID).Scan(&heads); err != nil || heads != 0 {
			t.Fatalf("route heads=%d err=%v, want none", heads, err)
		}
	})

	// A deterministic error raised AFTER finalize already wrote the
	// deployment, both releases, the route head, the route event: the
	// savepoint rollback must undo ALL of it while the block survives.
	t.Run("late error undoes earlier writes", func(t *testing.T) {
		a := newActivatedFixture(t, 154, true)
		a.repo.failAfter = func(stage string) error {
			if stage == "route_head" {
				return fmt.Errorf("%w: injected after the route head moved", ErrAgentLeaseCorruptState)
			}
			return nil
		}
		err := a.report(ctx, true, "")
		if !errors.Is(err, ErrAgentLeaseCorruptState) {
			t.Fatalf("err=%v, want ErrAgentLeaseCorruptState", err)
		}
		a.assertBlocked(t, 1, a.predecessorID, "injected after the route head moved")
		if _, current, _, _ := a.slotRow(t); current == nil {
			t.Fatal("slot must stay occupied")
		}
	})
}

// TestReportActivationResultCommitTimeFailureBlocksInFreshTransaction: a
// deferred trigger that fails at COMMIT (real, installed in the live database)
// discards the whole transaction; the block must still be recorded, in a fresh
// transaction, and the typed error returned.
func TestReportActivationResultCommitTimeFailureBlocksInFreshTransaction(t *testing.T) {
	ctx := context.Background()
	a := newActivatedFixture(t, 155, true)
	a.installCommitTimeFailure(t)
	err := a.report(ctx, true, "")
	if !errors.Is(err, ErrAgentLeaseConstraintViolation) {
		t.Fatalf("err=%v, want ErrAgentLeaseConstraintViolation", err)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "P0001" || !strings.Contains(pgErr.Message, "test deferred trigger refuses operation") {
		t.Fatalf("err=%v, want the deferred trigger's own P0001 raised at commit", err)
	}
	a.assertBlocked(t, 1, a.predecessorID, "activation finalize failed at commit")
	if _, _, summary := a.operation(t); !strings.Contains(summary, "test deferred trigger refuses operation "+a.operationID) {
		t.Fatalf("evidence %q does not name the deferred trigger's message", summary)
	}
	a.assertReconciliationRecorded(t, "test deferred trigger refuses operation", 0)

	// Replay of the same success report finds the blocked operation: it is
	// recorded once as late evidence (the host may have switched), and no
	// second block is written.
	if err := a.report(ctx, true, ""); !errors.Is(err, ErrAgentLeaseActivationLateReportRecorded) {
		t.Fatalf("replay err=%v, want ErrAgentLeaseActivationLateReportRecorded", err)
	}
	a.assertReconciliationRecorded(t, "test deferred trigger refuses operation", 1)
	_, _, afterFirst := a.operation(t)
	if strings.Count(afterFirst, "late report #") != 1 || countEvents(a.jobEvents(t), "activation_late_report") != 1 {
		t.Fatalf("summary=%q, want exactly one late entry and one late event", afterFirst)
	}
	// A second replay hits the fingerprint ledger: same answer, no write.
	if err := a.report(ctx, true, ""); !errors.Is(err, ErrAgentLeaseActivationLateReportRecorded) {
		t.Fatalf("second replay err=%v, want ErrAgentLeaseActivationLateReportRecorded", err)
	}
	if _, _, again := a.operation(t); again != afterFirst {
		t.Fatalf("second replay changed the summary:\n%q\n%q", afterFirst, again)
	}
	if n := countEvents(a.jobEvents(t), "activation_late_report"); n != 1 {
		t.Fatalf("late report events=%d after the second replay, want 1", n)
	}
}

// TestReportActivationResultCommitTimeBlockFailureIsRetryableNotTerminal: when
// the fresh transaction that writes the block fails, nothing durable exists.
// Returning the ORIGINAL typed terminal error would make the agent stop
// reporting with the operation silently open while the host has switched. The
// fresh transaction's failure must be returned instead (retryable), with the
// original cause as text only -- and the retry must then record the block.
func TestReportActivationResultCommitTimeBlockFailureIsRetryableNotTerminal(t *testing.T) {
	ctx := context.Background()
	for name, injected := range map[string]error{
		"plain error":                       errors.New("injected fresh-transaction failure"),
		"deterministic PgError":             &pgconn.PgError{Code: "23514", ConstraintName: "injected_check"},
		"read only transaction 25006":       &pgconn.PgError{Code: "25006"},
		"serialization failure 40001":       &pgconn.PgError{Code: "40001"},
		"deadlock 40P01":                    &pgconn.PgError{Code: "40P01"},
		"context deadline mid-fresh-tx":     fmt.Errorf("fresh: %w", context.DeadlineExceeded),
		"stale statement cache 0A000":       &pgconn.PgError{Code: "0A000", Routine: "RevalidateCachedQuery"},
		"io error 58030":                    &pgconn.PgError{Code: "58030"},
		"statement completion unknown":      &pgconn.PgError{Code: "40003"},
		"session timeout 57P05":             &pgconn.PgError{Code: "57P05"},
		"invalid statement name 26000":      &pgconn.PgError{Code: "26000"},
		"transaction timeout 25P04":         &pgconn.PgError{Code: "25P04"},
		"idle in transaction timeout 25P03": &pgconn.PgError{Code: "25P03"},
	} {
		t.Run(name, func(t *testing.T) {
			a := newActivatedFixture(t, 156, true)
			a.installCommitTimeFailure(t)
			closes := 0
			a.repo.failAfter = func(stage string) error {
				if stage != "operation_close" {
					return nil
				}
				// 1st: finalize's own operation close; 2nd: the fresh block's.
				if closes++; closes == 2 {
					return injected
				}
				return nil
			}
			err := a.report(ctx, true, "")
			if err == nil || !errors.Is(err, injected) {
				t.Fatalf("err=%v, want the fresh transaction's own failure %v", err, injected)
			}
			if errors.Is(err, ErrAgentLeaseConstraintViolation) || errors.Is(err, ErrAgentLeaseCorruptState) {
				t.Fatalf("err=%v must stay retryable, not the terminal typed error", err)
			}
			if !strings.Contains(err.Error(), "test deferred trigger refuses operation") {
				t.Fatalf("err=%q must carry the original cause as text", err)
			}
			a.assertStillAuthorized(t, 1, a.predecessorID)

			// The agent's retry (fresh transaction healthy again) records it.
			a.repo.failAfter = nil
			if err := a.report(ctx, true, ""); !errors.Is(err, ErrAgentLeaseConstraintViolation) {
				t.Fatalf("retry err=%v, want ErrAgentLeaseConstraintViolation", err)
			}
			a.assertReconciliationRecorded(t, "test deferred trigger refuses operation", 0)
		})
	}
}

// TestBlockAfterCommitFailureIsIdempotentAndTolerant drives the fresh-
// transaction path directly against operations a concurrent writer already
// closed or flagged: it must neither panic, nor double-record, nor mask the
// original error.
func TestBlockAfterCommitFailureIsIdempotentAndTolerant(t *testing.T) {
	ctx := context.Background()
	cause := errors.New("original commit failure")
	call := func(a activatedFixture) error {
		return a.repo.blockAfterCommitFailure(ctx, a.serverID, a.attemptID, 1, mustParseInt(t, a.operationID), cause, a.successReportInput())
	}

	t.Run("already reconciliation_required", func(t *testing.T) {
		a := newActivatedFixture(t, 157, true)
		if err := a.report(ctx, false, "reload timed out"); err != nil {
			t.Fatal(err)
		}
		before := a.jobEvents(t)
		// Already blocked: the report is recorded as late evidence, not lost.
		if err := call(a); !errors.Is(err, ErrAgentLeaseActivationLateReportRecorded) {
			t.Fatalf("err=%v, want ErrAgentLeaseActivationLateReportRecorded", err)
		}
		if after := a.jobEvents(t); len(after) != len(before)+1 {
			t.Fatalf("events %v -> %v, want exactly the late report event and no second block", before, after)
		}
		a.assertBlockedWithLate(t, 1, a.predecessorID, "reload timed out", 1)
	})

	// Another writer finalized this very operation: this report's intent is
	// satisfied, so the idempotent answer is nil (not the original error).
	t.Run("already succeeded", func(t *testing.T) {
		a := newActivatedFixture(t, 158, true)
		if err := a.report(ctx, true, ""); err != nil {
			t.Fatal(err)
		}
		if err := call(a); err != nil {
			t.Fatalf("err=%v, want nil: the operation already succeeded", err)
		}
		a.assertFinalized(t, 2)
	})

	t.Run("open op blocked once with head already flagged", func(t *testing.T) {
		a := newActivatedFixture(t, 159, true)
		if _, err := a.pool.Exec(ctx, `UPDATE route_heads SET reconciliation_required=true, updated_at=clock_timestamp() WHERE environment_id=$1`, a.envID); err != nil {
			t.Fatal(err)
		}
		if err := call(a); err != cause {
			t.Fatalf("err=%v, want the original error unchanged", err)
		}
		a.assertBlocked(t, 1, a.predecessorID, "activation finalize failed at commit: original commit failure")
		// The same report again finds the block this call created: recorded
		// once as late evidence, never a second block.
		if err := call(a); !errors.Is(err, ErrAgentLeaseActivationLateReportRecorded) {
			t.Fatalf("second call err=%v, want ErrAgentLeaseActivationLateReportRecorded", err)
		}
		a.assertReconciliationRecorded(t, "original commit failure", 1)
	})

	// Authentication happened in tx1; the block grants the agent nothing, so
	// a credential revoked between the report and the commit failure must NOT
	// stop the ambiguity from being recorded (fail safe).
	t.Run("revoked credential and server still record the block", func(t *testing.T) {
		a := newActivatedFixture(t, 160, true)
		if _, err := a.pool.Exec(ctx, `UPDATE server_credentials SET status='revoked', revoked_at=clock_timestamp() WHERE server_id=$1`, a.serverID); err != nil {
			t.Fatal(err)
		}
		if _, err := a.pool.Exec(ctx, `UPDATE servers SET status='revoked', revoked_at=clock_timestamp() WHERE id=$1`, a.serverID); err != nil {
			t.Fatal(err)
		}
		if err := call(a); err != cause {
			t.Fatalf("err=%v, want the original error unchanged", err)
		}
		a.assertReconciliationRecorded(t, "original commit failure", 0)
	})

	// Ownership is still enforced: another server's id cannot record a block
	// on this attempt.
	t.Run("wrong server id records nothing and fails retryably", func(t *testing.T) {
		a := newActivatedFixture(t, 163, true)
		err := a.repo.blockAfterCommitFailure(ctx, "someone-else", a.attemptID, 1, mustParseInt(t, a.operationID), cause, a.successReportInput())
		if err == nil || errors.Is(err, cause) || !errors.Is(err, ErrActivationBlockUnrecorded) {
			t.Fatalf("err=%v, want ErrActivationBlockUnrecorded without wrapping the cause", err)
		}
		a.assertStillAuthorized(t, 1, a.predecessorID)
	})
}

func mustParseInt(t *testing.T, s string) int64 {
	t.Helper()
	var v int64
	if _, err := fmt.Sscan(s, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TestReportActivationResultRetryableFailuresStillRollBack proves the fix did
// not over-apply: transient errors raised mid-finalize roll the whole
// transaction back and write NO block, so the agent's retry can still
// succeed. The retryable errors are injected at the repository's fault-
// injection point (a real serialization failure or deadlock cannot be made to
// occur deterministically at that statement); what is under test is the
// classification and that no block is written.
func TestReportActivationResultRetryableFailuresStillRollBack(t *testing.T) {
	ctx := context.Background()
	for name, injected := range map[string]error{
		"serialization failure 40001": &pgconn.PgError{Code: "40001"},
		"deadlock 40P01":              &pgconn.PgError{Code: "40P01"},
		"query cancelled 57014":       &pgconn.PgError{Code: "57014"},
		"connection class 08006":      &pgconn.PgError{Code: "08006"},
		"context deadline":            fmt.Errorf("finalize: %w", context.DeadlineExceeded),
	} {
		t.Run(name, func(t *testing.T) {
			a := newActivatedFixture(t, 161, true)
			a.repo.failAfter = func(stage string) error {
				if stage == "release_active" {
					return injected
				}
				return nil
			}
			err := a.report(ctx, true, "")
			if !errors.Is(err, injected) && !errors.As(err, new(*pgconn.PgError)) {
				t.Fatalf("err=%v, want the injected retryable error", err)
			}
			if errors.Is(err, ErrAgentLeaseConstraintViolation) || errors.Is(err, ErrAgentLeaseCorruptState) {
				t.Fatalf("err=%v must stay retryable, not typed terminal", err)
			}
			a.assertStillAuthorized(t, 1, a.predecessorID)
			if events := a.jobEvents(t); len(events) != 1 || events[0] != "activation_authorized" {
				t.Fatalf("job events=%v, want only activation_authorized (no block)", events)
			}
			// The retry then succeeds: nothing was poisoned.
			a.repo.failAfter = nil
			if err := a.report(ctx, true, ""); err != nil {
				t.Fatalf("retry: %v", err)
			}
			a.assertFinalized(t, 2)
		})
	}
}

// TestReportActivationResultFailureOnAlreadyFlaggedRouteHead: migration 000020
// exempts a flag-only route head update only when the flag actually changes,
// so blocking an environment whose head is already flagged must not issue the
// UPDATE (it would raise P0001).
func TestReportActivationResultFailureOnAlreadyFlaggedRouteHead(t *testing.T) {
	ctx := context.Background()
	a := newActivatedFixture(t, 162, true)
	if _, err := a.pool.Exec(ctx, `UPDATE route_heads SET reconciliation_required=true, updated_at=clock_timestamp() WHERE environment_id=$1`, a.envID); err != nil {
		t.Fatal(err)
	}
	if err := a.report(ctx, false, "reload timed out"); err != nil {
		t.Fatalf("report on an already-flagged head: %v", err)
	}
	a.assertBlocked(t, 1, a.predecessorID, "reload timed out")
}
