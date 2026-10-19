//go:build integration

package postgres

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"autodeploy/internal/deployment"

	"github.com/jackc/pgx/v5/pgconn"
)

// installBlockCommitFailureOnce creates a REAL deferred constraint trigger that
// raises at COMMIT the FIRST time an update moves THIS operation to
// 'reconciliation_required' and passes every later time (a non-transactional
// sequence counts the attempts). It makes "the block transaction's own COMMIT
// fails deterministically, but a fresh transaction can still write the block"
// reproducible. Test-only schema, dropped on cleanup.
func (a activatedFixture) installBlockCommitFailureOnce(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	name := "test_block_commit_once_" + a.operationID
	drop := func() {
		_, _ = a.pool.Exec(ctx, `DROP TRIGGER IF EXISTS `+name+` ON release_operations`)
		_, _ = a.pool.Exec(ctx, `DROP FUNCTION IF EXISTS `+name+`()`)
		_, _ = a.pool.Exec(ctx, `DROP SEQUENCE IF EXISTS `+name+`_seq`)
	}
	drop()
	if _, err := a.pool.Exec(ctx, `CREATE SEQUENCE `+name+`_seq`); err != nil {
		t.Fatalf("create sequence: %v", err)
	}
	t.Cleanup(drop)
	if _, err := a.pool.Exec(ctx, `CREATE FUNCTION `+name+`() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
		IF NEW.state = 'reconciliation_required' AND nextval('`+name+`_seq') = 1 THEN RAISE EXCEPTION 'test block commit refused once for operation %', NEW.id; END IF;
		RETURN NULL; END $$`); err != nil {
		t.Fatalf("create function: %v", err)
	}
	if _, err := a.pool.Exec(ctx, `CREATE CONSTRAINT TRIGGER `+name+` AFTER UPDATE ON release_operations DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.id = `+a.operationID+`) EXECUTE FUNCTION `+name+`()`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
}

// insertOtherAttempt adds a second, closed attempt for the same job and
// returns its id, so a slot can be pointed at an occupant that is not the
// reporting attempt.
func (a activatedFixture) insertOtherAttempt(t *testing.T) int64 {
	t.Helper()
	var id int64
	withTriggerDisabled(t, a, "agent_job_attempts", "agent_job_attempts_validate_insert", func() {
		if err := a.pool.QueryRow(context.Background(), `INSERT INTO agent_job_attempts(job_id, attempt_number, server_id, environment_id, fence, state, lease_expires_at, authority_expires_at, closed_at)
			VALUES ($1, 2, $2, $3, 1, 'expired', now(), now(), now()) RETURNING id`, a.jobID, a.serverID, a.envID).Scan(&id); err != nil {
			t.Fatalf("insert other attempt: %v", err)
		}
	})
	return id
}

// breakOccupancy puts the fixture into a state where the operation is still
// switch_authorized but the attempt is NOT the leased occupant of the slot at
// this fence, in one specific way. Each is a clause of the occupant check that
// no earlier lookup decides.
func (a activatedFixture) breakOccupancy(t *testing.T, how string) {
	t.Helper()
	ctx := context.Background()
	switch how {
	case "attempt not leased":
		if _, err := a.pool.Exec(ctx, `UPDATE agent_job_attempts SET state='expired', closed_at=clock_timestamp() WHERE id=$1`, a.attemptID); err != nil {
			t.Fatal(err)
		}
	case "slot has no occupant":
		if _, err := a.pool.Exec(ctx, `UPDATE environment_slots SET current_attempt_id=NULL WHERE environment_id=$1`, a.envID); err != nil {
			t.Fatal(err)
		}
	case "slot occupied by another attempt":
		other := a.insertOtherAttempt(t)
		if _, err := a.pool.Exec(ctx, `UPDATE environment_slots SET current_attempt_id=$2 WHERE environment_id=$1`, a.envID, other); err != nil {
			t.Fatal(err)
		}
	case "slot fence moved":
		if _, err := a.pool.Exec(ctx, `UPDATE environment_slots SET fence=2 WHERE environment_id=$1`, a.envID); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown occupancy break %q", how)
	}
}

// assertNothingRecorded asserts the operation is still open and unflagged, no
// block event exists and no finalization happened.
func (a activatedFixture) assertNothingRecorded(t *testing.T) {
	t.Helper()
	if state, _, _ := a.operation(t); state != "switch_authorized" {
		t.Fatalf("operation state=%s, want switch_authorized (nothing recorded)", state)
	}
	if _, _, reconciling := a.routeHead(t); reconciling {
		t.Fatal("route head must not be flagged")
	}
	if _, _, _, reconciling := a.slotRow(t); reconciling {
		t.Fatal("slot must not be flagged")
	}
	if events := a.jobEvents(t); len(events) != 1 || events[0] != "activation_authorized" {
		t.Fatalf("job events=%v, want only activation_authorized", events)
	}
	if n := a.activatedRouteEvents(t); n != 0 {
		t.Fatalf("activated route events=%d, want 0", n)
	}
}

var occupancyBreaks = []string{"attempt not leased", "slot has no occupant", "slot occupied by another attempt", "slot fence moved"}

// TestReportActivationResultRefusesNonOccupant: with the operation open but
// the attempt not the leased occupant of the slot at this fence, a report is
// refused as stale and writes nothing -- for each clause of the occupant
// check separately (the existing "fence bumped" test covers only the fence).
func TestReportActivationResultRefusesNonOccupant(t *testing.T) {
	ctx := context.Background()
	for i, how := range occupancyBreaks {
		for _, succeeded := range []bool{true, false} {
			name := how + "/failure report"
			if succeeded {
				name = how + "/success report"
			}
			t.Run(name, func(t *testing.T) {
				a := newActivatedFixture(t, byte(100+i), true)
				a.breakOccupancy(t, how)
				if err := a.report(ctx, succeeded, "reload failed"); !errors.Is(err, ErrAgentLeaseStale) {
					t.Fatalf("err=%v, want ErrAgentLeaseStale", err)
				}
				if state, _, _ := a.operation(t); state != "switch_authorized" {
					t.Fatalf("operation state=%s, want switch_authorized", state)
				}
				if _, _, routeReconciling := a.routeHead(t); routeReconciling {
					t.Fatal("route head must not be flagged")
				}
				if events := a.jobEvents(t); len(events) != 1 {
					t.Fatalf("job events=%v, want only activation_authorized", events)
				}
				if n := a.activatedRouteEvents(t); n != 0 {
					t.Fatalf("activated route events=%d, want 0", n)
				}
			})
		}
	}
}

// TestBlockAfterCommitFailureRefusesNonOccupant: the same occupant clauses in
// the fresh-transaction path (each in isolation): nothing is recorded and the
// original error is what the caller gets.
func TestBlockAfterCommitFailureRefusesNonOccupant(t *testing.T) {
	ctx := context.Background()
	cause := errors.New("original commit failure")
	for i, how := range occupancyBreaks {
		t.Run(how, func(t *testing.T) {
			a := newActivatedFixture(t, byte(110+i), true)
			a.breakOccupancy(t, how)
			if err := a.repo.blockAfterCommitFailure(ctx, a.serverID, a.attemptID, 1, mustParseInt(t, a.operationID), cause, a.successReportInput()); err != cause {
				t.Fatalf("err=%v, want the original error unchanged", err)
			}
			a.assertNothingRecorded(t)
		})
	}
}

// TestBlockAfterFinalizeFailureRollbackToSavepointFailureIsRetryable: a failed
// ROLLBACK TO SAVEPOINT means no block was written; the failure must be the
// retryable rollback error with the deterministic cause as TEXT ONLY. Wrapping
// the cause (%w) would make the caller see a terminal typed error with nothing
// durable recorded.
func TestBlockAfterFinalizeFailureRollbackToSavepointFailureIsRetryable(t *testing.T) {
	bg := context.Background()
	cause := fmt.Errorf("%w: injected deterministic finalize failure", ErrAgentLeaseCorruptState)
	call := func(t *testing.T, a activatedFixture, callCtx context.Context) error {
		t.Helper()
		tx, err := a.pool.Begin(bg)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(bg) //nolint:errcheck
		return a.repo.blockAfterFinalizeFailure(callCtx, tx, a.serverID, a.jobID, a.attemptID, 1, mustParseInt(t, a.operationID), a.envID, cause, a.successReportInput())
	}
	check := func(t *testing.T, a activatedFixture, err error) {
		t.Helper()
		if err == nil || errors.Is(err, cause) || errors.Is(err, ErrAgentLeaseCorruptState) {
			t.Fatalf("err=%v must not wrap the deterministic cause", err)
		}
		if !errors.Is(err, ErrActivationBlockUnrecorded) {
			t.Fatalf("err=%v, want ErrActivationBlockUnrecorded", err)
		}
		if classified := classifyActivationResultError(err); errors.Is(classified, ErrAgentLeaseConstraintViolation) || errors.Is(classified, ErrAgentLeaseCorruptState) {
			t.Fatalf("classified=%v must stay retryable", classified)
		}
		if !strings.Contains(err.Error(), cause.Error()) {
			t.Fatalf("err=%q must carry the cause as text", err)
		}
		a.assertNothingRecorded(t)
	}
	t.Run("cancelled context", func(t *testing.T) {
		a := newActivatedFixture(t, 120, true)
		cancelled, cancel := context.WithCancel(bg)
		cancel()
		err := call(t, a, cancelled)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v, want the rollback's own context.Canceled", err)
		}
		check(t, a, err)
	})
	t.Run("no savepoint", func(t *testing.T) {
		a := newActivatedFixture(t, 121, true)
		err := call(t, a, bg)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "3B001" {
			t.Fatalf("err=%v, want the rollback's own 3B001", err)
		}
		check(t, a, err)
	})
}

// TestReportActivationResultBlockWriteFailureIsRetryableNotTerminal: every
// path whose block write fails returns a retryable error (never a terminal
// typed one) and leaves the operation open for the retry -- including a
// DETERMINISTIC PgError from the block write itself, which classify would
// otherwise turn into a terminal 409 with nothing durable recorded.
func TestReportActivationResultBlockWriteFailureIsRetryableNotTerminal(t *testing.T) {
	ctx := context.Background()
	deterministic := &pgconn.PgError{Code: "23514", ConstraintName: "injected_check"}
	scenarios := []struct {
		name      string
		succeeded bool
		setup     func(t *testing.T, a activatedFixture)
		// failOn is the occurrence (1-based) of operation_close that fails.
		failOn int
	}{
		{"failure report", false, func(*testing.T, activatedFixture) {}, 1},
		{"digest mismatch", true, func(*testing.T, activatedFixture) {}, 1},
		{"finalize failure", true, func(t *testing.T, a activatedFixture) {
			if _, err := a.pool.Exec(ctx, `UPDATE releases SET state='failed' WHERE id=$1`, a.candidateID); err != nil {
				t.Fatal(err)
			}
		}, 1},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			a := newActivatedFixture(t, 130, true)
			sc.setup(t, a)
			closes := 0
			a.repo.failAfter = func(stage string) error {
				if stage != "operation_close" {
					return nil
				}
				if closes++; closes == sc.failOn {
					return deterministic
				}
				return nil
			}
			var err error
			if sc.name == "digest mismatch" {
				wrong := a.routeDigest
				wrong[0] ^= 0xff
				err = a.repo.ReportActivationResult(ctx, a.serverID, a.cred, a.attemptID, 1, a.operationID, true, wrong, "")
			} else {
				err = a.report(ctx, sc.succeeded, "reload failed")
			}
			if err == nil || !errors.Is(err, deterministic) {
				t.Fatalf("err=%v, want the block write's own failure", err)
			}
			if errors.Is(err, ErrAgentLeaseConstraintViolation) || errors.Is(err, ErrAgentLeaseCorruptState) || errors.Is(err, ErrAgentLeaseIntegrityConflict) {
				t.Fatalf("err=%v must be retryable, not terminal", err)
			}
			a.assertNothingRecorded(t)
		})
	}
}

// TestReportActivationResultFreshBlockCommitFailureIsRetryableNotTerminal: the
// fresh transaction's own COMMIT failing with a deterministic PgError (a real
// deferred trigger, refusing once) means nothing durable exists, so the error
// must stay retryable -- not be classified into the terminal constraint
// violation -- and the retry must then record the block.
func TestReportActivationResultFreshBlockCommitFailureIsRetryableNotTerminal(t *testing.T) {
	ctx := context.Background()
	a := newActivatedFixture(t, 143, true)
	a.installCommitTimeFailure(t)      // main transaction fails at COMMIT
	a.installBlockCommitFailureOnce(t) // the fresh block's COMMIT fails once too
	err := a.report(ctx, true, "")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "P0001" || !strings.Contains(pgErr.Message, "test block commit refused once") {
		t.Fatalf("err=%v, want the fresh transaction's own P0001", err)
	}
	if errors.Is(err, ErrAgentLeaseConstraintViolation) || !errors.Is(err, ErrActivationBlockUnrecorded) {
		t.Fatalf("err=%v must be retryable (ErrActivationBlockUnrecorded), not terminal", err)
	}
	a.assertStillAuthorized(t, 1, a.predecessorID)
	if err := a.report(ctx, true, ""); !errors.Is(err, ErrAgentLeaseConstraintViolation) {
		t.Fatalf("retry err=%v, want the typed error with the block recorded", err)
	}
	a.assertReconciliationRecorded(t, "test deferred trigger refuses operation", 0)
}

// TestReportActivationResultBlockCommitFailureFallsBackToFreshTransaction: a
// deterministic COMMIT error of the block transaction itself (a real deferred
// trigger that refuses once) must not lose the block: each of the three
// commit sites falls back to a fresh transaction and still answers as it
// would have had the commit succeeded.
func TestReportActivationResultBlockCommitFailureFallsBackToFreshTransaction(t *testing.T) {
	ctx := context.Background()

	t.Run("failure report", func(t *testing.T) {
		a := newActivatedFixture(t, 140, true)
		a.installBlockCommitFailureOnce(t)
		if err := a.report(ctx, false, "reload timed out"); err != nil {
			t.Fatalf("err=%v, want nil: the failure was recorded by the fallback", err)
		}
		a.assertBlocked(t, 1, a.predecessorID, "reload timed out")
	})

	t.Run("digest mismatch", func(t *testing.T) {
		a := newActivatedFixture(t, 141, true)
		a.installBlockCommitFailureOnce(t)
		wrong := a.routeDigest
		wrong[0] ^= 0xff
		err := a.repo.ReportActivationResult(ctx, a.serverID, a.cred, a.attemptID, 1, a.operationID, true, wrong, "")
		if !errors.Is(err, ErrAgentLeaseIntegrityConflict) {
			t.Fatalf("err=%v, want the typed refusal ErrAgentLeaseIntegrityConflict", err)
		}
		a.assertBlocked(t, 1, a.predecessorID, "route digest mismatch: expected sha256:"+hex.EncodeToString(a.routeDigest[:]))
	})

	t.Run("finalize failure", func(t *testing.T) {
		a := newActivatedFixture(t, 142, true)
		if _, err := a.pool.Exec(ctx, `UPDATE releases SET state='failed' WHERE id=$1`, a.candidateID); err != nil {
			t.Fatal(err)
		}
		a.installBlockCommitFailureOnce(t)
		err := a.report(ctx, true, "")
		if !errors.Is(err, ErrAgentLeaseCorruptState) {
			t.Fatalf("err=%v, want the typed ErrAgentLeaseCorruptState", err)
		}
		a.assertReconciliationRecorded(t, fmt.Sprintf(`candidate release %s is "failed"`, a.candidateID), 0)
	})
}

// TestReportActivationResultFailureCommitFallbackFindsOperationAlreadyBlocked:
// a failure report whose own block COMMIT fails deterministically, while
// another writer's block queued on the slot lock (confirmed in pg_stat_activity)
// and wins it the moment this transaction rolls back. The fresh transaction
// finds the operation already reconciliation_required: the report is recorded
// as late evidence and answered with the late sentinel, not lost and not
// reported as stale.
func TestReportActivationResultFailureCommitFallbackFindsOperationAlreadyBlocked(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a := newActivatedFixture(t, 144, true)
	a.installBlockCommitFailureOnce(t)
	opID := mustParseInt(t, a.operationID)
	done := make(chan error, 1)
	started := false
	a.repo.failAfter = func(stage string) error {
		if stage != "operation_close" || started {
			return nil
		}
		started = true
		go func() {
			_, err := a.repo.recordBlockInFreshTransaction(ctx, a.serverID, a.attemptID, 1, opID, "other writer block", a.failureReportInput("other writer block"))
			done <- err
		}()
		waitForLockWaiters(t, ctx, a.pool, "FROM environment_slots%FOR UPDATE", 1)
		return nil
	}
	if err := a.report(ctx, false, "reload timed out"); !errors.Is(err, ErrAgentLeaseActivationLateReportRecorded) {
		t.Fatalf("err=%v, want ErrAgentLeaseActivationLateReportRecorded", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("other writer's block: %v", err)
	}
	a.assertBlockedWithLate(t, 1, a.predecessorID, "other writer block", 1)
	if _, _, summary := a.operation(t); !strings.HasPrefix(summary, "other writer block | late report #") || !strings.Contains(summary, "reload timed out") {
		t.Fatalf("summary=%q, want the other block followed by this report as late evidence", summary)
	}
}

// Same race as the failure-report test above, through refuseAmbiguous: a
// success report with the wrong digest whose ambiguity-block COMMIT fails
// while another writer's block wins the slot. The report is recorded as late
// evidence and answered with the late sentinel, not the ambiguity refusal.
func TestReportActivationResultAmbiguityCommitFallbackFindsOperationAlreadyBlocked(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a := newActivatedFixture(t, 145, true)
	a.installBlockCommitFailureOnce(t)
	opID := mustParseInt(t, a.operationID)
	done := make(chan error, 1)
	started := false
	a.repo.failAfter = func(stage string) error {
		if stage != "operation_close" || started {
			return nil
		}
		started = true
		go func() {
			_, err := a.repo.recordBlockInFreshTransaction(ctx, a.serverID, a.attemptID, 1, opID, "other writer block", a.failureReportInput("other writer block"))
			done <- err
		}()
		waitForLockWaiters(t, ctx, a.pool, "FROM environment_slots%FOR UPDATE", 1)
		return nil
	}
	wrong := a.routeDigest
	wrong[0] ^= 0xff
	err := a.repo.ReportActivationResult(ctx, a.serverID, a.cred, a.attemptID, 1, a.operationID, true, wrong, "")
	if !errors.Is(err, ErrAgentLeaseActivationLateReportRecorded) {
		t.Fatalf("err=%v, want ErrAgentLeaseActivationLateReportRecorded", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("other writer's block: %v", err)
	}
	a.assertBlockedWithLate(t, 1, a.predecessorID, "other writer block", 1)
	if _, _, summary := a.operation(t); !strings.Contains(summary, "matches authorized digest: false") {
		t.Fatalf("summary=%q, want the wrong-digest success recorded as late evidence", summary)
	}
}

// TestReportActivationResultFailuresAroundTheSavepointAreRouted covers the
// returns that sit before the savepoint exists (deployment lock, route head
// lock, SAVEPOINT) and the post-savepoint clock read. They are driven through
// the repository's fault-injection points, which fire right after the real
// statement succeeded: what is under test is the routing, not Postgres.
func TestReportActivationResultFailuresAroundTheSavepointAreRouted(t *testing.T) {
	ctx := context.Background()
	for _, stage := range []string{"deployment_lock", "route_head_lock", "savepoint", "finalize_clock"} {
		t.Run(stage+"/deterministic blocks", func(t *testing.T) {
			a := newActivatedFixture(t, 150, true)
			boom := fmt.Errorf("%w: injected at %s", ErrAgentLeaseCorruptState, stage)
			a.repo.failAfter = func(s string) error {
				if s == stage {
					return boom
				}
				return nil
			}
			err := a.report(ctx, true, "")
			if !errors.Is(err, ErrAgentLeaseCorruptState) {
				t.Fatalf("err=%v, want the typed error", err)
			}
			a.assertBlocked(t, 1, a.predecessorID, "injected at "+stage)
		})
		t.Run(stage+"/transient rolls back", func(t *testing.T) {
			a := newActivatedFixture(t, 151, true)
			a.repo.failAfter = func(s string) error {
				if s == stage {
					return &pgconn.PgError{Code: "40001"}
				}
				return nil
			}
			err := a.report(ctx, true, "")
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "40001" {
				t.Fatalf("err=%v, want the injected 40001", err)
			}
			a.assertNothingRecorded(t)
			a.repo.failAfter = nil
			if err := a.report(ctx, true, ""); err != nil {
				t.Fatalf("retry: %v", err)
			}
			a.assertFinalized(t, 2)
		})
	}
}

// TestReportActivationResultFinalizeSentinelsBlock: the deployment
// repository's sentinels cannot come from concurrency inside tx2 (the
// deployment row is locked) and so mean corruption; they must block and map to
// the terminal corrupt-state error, never stay retryable.
func TestReportActivationResultFinalizeSentinelsBlock(t *testing.T) {
	ctx := context.Background()
	for name, sentinel := range map[string]error{"revision conflict": ErrRevisionConflict, "not found": ErrNotFound, "revision overflow": ErrRevisionOverflow} {
		t.Run(name, func(t *testing.T) {
			a := newActivatedFixture(t, 152, true)
			a.repo.failAfter = func(stage string) error {
				if stage == "deployment" {
					return fmt.Errorf("injected: %w", sentinel)
				}
				return nil
			}
			err := a.report(ctx, true, "")
			if !errors.Is(err, ErrAgentLeaseCorruptState) || !errors.Is(err, sentinel) {
				t.Fatalf("err=%v, want ErrAgentLeaseCorruptState wrapping %v", err, sentinel)
			}
			a.assertBlocked(t, 1, a.predecessorID, "injected")
			if got := a.deploymentStatus(t); got != deployment.StatusActivating {
				t.Fatalf("deployment=%s, want activating", got)
			}
		})
	}
}

// TestBlockAfterCommitFailureLocksGenuinelyContend proves each row lock the
// fresh-transaction block takes is a real FOR UPDATE: a third transaction
// holds the row, the block is launched on a goroutine, and
// pg_stat_activity is polled until the block is ACTUALLY waiting on that
// statement (never a sleep). The holder then wins by changing the row, and
// the block -- re-reading the locked row after the wait -- must find the
// changed state and record nothing. With a plain SELECT in place of FOR UPDATE
// the block never waits (the poll times out); a stale read would instead
// block over the holder's decision.
//
// The property asserted is not a fixed winner but consistency with the
// winner: the holder's committed change stands and the loser's refusal is the
// no-op (the original cause), never a block layered on top of it.
func TestBlockAfterCommitFailureLocksGenuinelyContend(t *testing.T) {
	cause := errors.New("original commit failure")
	rows := []struct {
		table, lockSQL, winnerSQL string
		// holderWins: the holder's change makes the block a no-op; otherwise
		// the holder only delays it and the block must still be recorded.
		holderWins bool
	}{
		{"environment_slots", `SELECT 1 FROM environment_slots WHERE environment_id=$1 FOR UPDATE`,
			`UPDATE environment_slots SET current_attempt_id=NULL WHERE environment_id=$1`, true},
		{"agent_jobs", `SELECT 1 FROM agent_jobs WHERE id=$1 FOR UPDATE`, ``, false},
		{"agent_job_attempts", `SELECT 1 FROM agent_job_attempts WHERE id=$1 FOR UPDATE`,
			`UPDATE agent_job_attempts SET state='expired', closed_at=clock_timestamp() WHERE id=$1`, true},
		{"release_operations", `SELECT 1 FROM release_operations WHERE id=$1 FOR UPDATE`,
			`UPDATE release_operations SET state='reconciliation_required', closed_at=clock_timestamp(), outcome_summary='holder won' WHERE id=$1`, true},
	}
	for i, row := range rows {
		t.Run(row.table, func(t *testing.T) {
			a := newActivatedFixture(t, byte(160+i), true)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			var key any
			switch row.table {
			case "environment_slots":
				key = a.envID
			case "agent_jobs":
				key = a.jobID
			case "agent_job_attempts":
				key = a.attemptID
			default:
				key = mustParseInt(t, a.operationID)
			}
			holder, err := a.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			// Deferred so a failed poll (t.Fatal unwinds with runtime.Goexit)
			// cannot leak the lock and hang the blocked goroutine.
			defer holder.Rollback(ctx) //nolint:errcheck
			if _, err := holder.Exec(ctx, row.lockSQL, key); err != nil {
				t.Fatal(err)
			}
			var blockErr error
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				blockErr = a.repo.blockAfterCommitFailure(ctx, a.serverID, a.attemptID, 1, mustParseInt(t, a.operationID), cause, a.successReportInput())
			}()
			waitForLockWaiters(t, ctx, a.pool, "FROM "+row.table+"%FOR UPDATE", 1)
			// Still blocked: nothing may have been recorded yet.
			if state, _, _ := a.operation(t); state != "switch_authorized" {
				t.Fatalf("operation state=%s while the block is blocked, want switch_authorized", state)
			}
			if row.winnerSQL != "" {
				if _, err := holder.Exec(ctx, row.winnerSQL, key); err != nil {
					t.Fatal(err)
				}
			}
			if err := holder.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			wg.Wait()
			// A holder that already blocked the operation (release_operations
			// row) does not make the report a no-op: it is recorded as late
			// evidence. Every other winner leaves the original cause.
			wantErr := cause
			if row.table == "release_operations" {
				wantErr = ErrAgentLeaseActivationLateReportRecorded
			}
			if !errors.Is(blockErr, wantErr) {
				t.Fatalf("block err=%v, want %v (recorded or no-op alike)", blockErr, wantErr)
			}
			if row.holderWins {
				if _, _, _, reconciling := a.slotRow(t); reconciling {
					t.Fatal("block was layered on top of the holder's committed change")
				}
				if _, _, routeReconciling := a.routeHead(t); routeReconciling {
					t.Fatal("route head flagged on top of the holder's committed change")
				}
				wantEvents := 1
				if row.table == "release_operations" {
					wantEvents = 2 // authorization + the late report, never a block
					if _, _, summary := a.operation(t); !strings.HasPrefix(summary, "holder won | late report #") {
						t.Fatalf("summary=%q, want the holder's followed by the late entry", summary)
					}
				}
				if events := a.jobEvents(t); len(events) != wantEvents || countEvents(events, "activation_reconciliation_required") != 0 {
					t.Fatalf("job events=%v, want no block event", events)
				}
				return
			}
			a.assertReconciliationRecorded(t, "original commit failure", 0)
		})
	}
}

// TestBlockAfterCommitFailureGenuinelyRacesSuccessReport: the fresh-transaction
// block and a real success report for the same operation both queue on the
// slot row held by a third transaction (confirmed via pg_stat_activity, two
// backends waiting). Exactly one decides the operation; the loser observes the
// winner. Either order is valid -- the property is consistency, not a fixed
// winner.
func TestBlockAfterCommitFailureGenuinelyRacesSuccessReport(t *testing.T) {
	a := newActivatedFixture(t, 170, true)
	cause := errors.New("original commit failure")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	holder, err := a.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx) //nolint:errcheck
	if _, err := holder.Exec(ctx, `SELECT 1 FROM environment_slots WHERE environment_id=$1 FOR UPDATE`, a.envID); err != nil {
		t.Fatal(err)
	}
	var blockErr, reportErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		blockErr = a.repo.blockAfterCommitFailure(ctx, a.serverID, a.attemptID, 1, mustParseInt(t, a.operationID), cause, a.successReportInput())
	}()
	go func() {
		defer wg.Done()
		reportErr = a.report(ctx, true, "")
	}()
	waitForLockWaiters(t, ctx, a.pool, "FROM environment_slots%FOR UPDATE", 2)
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	switch {
	case blockErr == cause:
		// The block won. The success report is NOT stale: the host may have
		// switched, so it is evidence that must reach the operator. It is
		// recorded after the original block evidence, not instead of it.
		if !errors.Is(reportErr, ErrAgentLeaseActivationLateReportRecorded) {
			t.Fatalf("block won but the success report err=%v, want ErrAgentLeaseActivationLateReportRecorded", reportErr)
		}
		a.assertReconciliationRecorded(t, "original commit failure", 1)
		if _, _, summary := a.operation(t); !strings.Contains(summary, "late report #") {
			t.Fatalf("summary=%q lacks the late success report entry", summary)
		}
		if n := countEvents(a.jobEvents(t), "activation_late_report"); n != 1 {
			t.Fatalf("late report events=%d, want exactly 1", n)
		}
	case blockErr == nil:
		// The report won: the block saw the succeeded operation (idempotent nil).
		if reportErr != nil {
			t.Fatalf("report won but err=%v", reportErr)
		}
		a.assertFinalized(t, 2)
	default:
		t.Fatalf("block err=%v, want the cause (block won) or nil (report won)", blockErr)
	}
}
