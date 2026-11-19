//go:build integration

package postgres

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"autodeploy/internal/deployment"
	"autodeploy/internal/queue"

	"github.com/jackc/pgx/v5/pgconn"
)

// ADR 0005 stop-then-start auto-finalize (slice 22-D, control plane): a
// not_switched activation-result report finalizes the operation as a definite
// failure ONLY when every condition holds; every other report keeps today's
// durable reconciliation block.

const notSwitchedSummary = "gate failed: candidate unhealthy; predecessor restarted and healthy"

func (a activatedFixture) reportNotSwitched(ctx context.Context, summary string) error {
	return a.repo.ReportActivationResultWithOutcome(ctx, a.serverID, a.cred, a.attemptID, 1, a.operationID, false, [32]byte{}, summary, queue.ActivationOutcomeNotSwitched)
}

// force runs one statement with triggers disabled (replica role, local to the
// transaction) to build a state no legal path reaches, so each negative case
// can break exactly ONE finalize condition.
func (a activatedFixture) force(t *testing.T, stmt string, args ...any) {
	t.Helper()
	ctx := context.Background()
	tx, err := a.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, stmt, args...); err != nil {
		t.Fatalf("force %q: %v", stmt, err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// assertNotSwitchedFinalized is the whole positive post-state.
func (a activatedFixture) assertNotSwitchedFinalized(t *testing.T) {
	t.Helper()
	state, closedAt, summary := a.operation(t)
	if state != "failed" || closedAt == nil || !strings.HasPrefix(summary, "not_switched: ") {
		t.Fatalf("operation state=%s closedAt=%v summary=%q, want failed with a not_switched summary", state, closedAt, summary)
	}
	if got := a.deploymentStatus(t); got != deployment.StatusFailed {
		t.Fatalf("deployment=%s, want failed", got)
	}
	if attemptState, attemptClosed, failureClass, resultOp, _ := a.attemptRow(t, a.attemptID); attemptState != "expired" || attemptClosed == nil || failureClass != nil || resultOp != nil {
		t.Fatalf("attempt state=%s closedAt=%v failureClass=%v resultOp=%v, want closed expired", attemptState, attemptClosed, failureClass, resultOp)
	}
	fence, current, reconAttempt, reconciling := a.slotRow(t)
	if fence != 1 || current != nil || reconAttempt != nil || reconciling {
		t.Fatalf("slot fence=%d current=%v reconAttempt=%v reconciling=%v, want free and NOT flagged", fence, current, reconAttempt, reconciling)
	}
	if jobState, _, _, _ := a.jobRow(t); jobState != "failed" {
		t.Fatalf("job state=%s, want failed", jobState)
	}
	revision, active, routeReconciling := a.routeHead(t)
	if revision != 1 || routeReconciling || (a.predecessorID != "" && (active == nil || *active != a.predecessorID)) {
		t.Fatalf("route head revision=%d active=%v reconciling=%v, want 1 %s false (untouched)", revision, active, routeReconciling, a.predecessorID)
	}
	if st, _ := a.releaseState(t, a.candidateID); st != "ready" {
		t.Fatalf("candidate=%s, want ready (untouched)", st)
	}
	if a.predecessorID != "" {
		if st, _ := a.releaseState(t, a.predecessorID); st != "active" {
			t.Fatalf("predecessor=%s, want still active", st)
		}
	}
	if n := a.activatedRouteEvents(t); n != 0 {
		t.Fatalf("activated route events=%d, want 0", n)
	}
	want := "activation_authorized,activation_reconciliation_required,activation_not_activated,failed"
	if got := strings.Join(a.jobEvents(t), ","); got != want {
		t.Fatalf("job events=%s, want %s", got, want)
	}
}

// assertFellBackToBlock is today's block for a not_switched report that did
// not qualify: the operation is reconciliation_required with the report's own
// text, the attempt/slot/job are held and flagged, the deployment and the
// releases are untouched. The route head's revision/active release are NOT
// asserted (some cases move them on purpose).
func (a activatedFixture) assertFellBackToBlock(t *testing.T, wantSummary string) {
	t.Helper()
	state, closedAt, summary := a.operation(t)
	if state != "reconciliation_required" || closedAt == nil || summary != wantSummary {
		t.Fatalf("operation state=%s closedAt=%v summary=%q, want reconciliation_required %q", state, closedAt, summary, wantSummary)
	}
	if attemptState, attemptClosed, _, _, _ := a.attemptRow(t, a.attemptID); attemptState != "leased" || attemptClosed != nil {
		t.Fatalf("attempt state=%s closedAt=%v, want untouched leased", attemptState, attemptClosed)
	}
	if _, current, reconAttempt, reconciling := a.slotRow(t); current == nil || *current != a.attemptID || !reconciling || reconAttempt == nil || *reconAttempt != a.attemptID {
		t.Fatalf("slot current=%v reconciling=%v reconAttempt=%v, want held and flagged", current, reconciling, reconAttempt)
	}
	if jobState, _, _, _ := a.jobRow(t); jobState != "leased" {
		t.Fatalf("job state=%s, want leased", jobState)
	}
	if _, _, flagged := a.routeHead(t); !flagged {
		t.Fatal("route head must be flagged")
	}
	if got := strings.Join(a.jobEvents(t), ","); got != "activation_authorized,activation_reconciliation_required" {
		t.Fatalf("job events=%s, want authorized then one block event", got)
	}
	if st, _ := a.releaseState(t, a.candidateID); st != "ready" && st != "retained" {
		t.Fatalf("candidate=%s", st)
	}
}

func TestNotSwitchedFinalizesDefiniteFailure(t *testing.T) {
	ctx := context.Background()
	a := newActivatedFixture(t, 40, true)
	a.assertStillAuthorized(t, 1, a.predecessorID)
	if err := a.reportNotSwitched(ctx, notSwitchedSummary); err != nil {
		t.Fatalf("not_switched report: %v", err)
	}
	a.assertNotSwitchedFinalized(t)

	// The environment is NOT blocked: a new attempt activates against the
	// untouched predecessor.
	_, attempt2 := a.secondAttempt(t, 2, "after-not-switched")
	result, err := a.repo.Activate(ctx, a.serverID, a.cred, attempt2, 2, digest(88))
	if err != nil {
		t.Fatalf("Activate after a finalized not_switched: %v", err)
	}
	if result.OperationID == "" || result.RouteDigest == "" {
		t.Fatalf("result=%+v, want a full authorization", result)
	}
}

func TestNotSwitchedIgnoresLapsedAuthority(t *testing.T) {
	ctx := context.Background()
	a := newActivatedFixture(t, 41, true)
	if _, err := a.pool.Exec(ctx, `UPDATE agent_job_attempts SET lease_expires_at=now()-interval '2 seconds', authority_expires_at=now()-interval '2 seconds' WHERE id=$1`, a.attemptID); err != nil {
		t.Fatal(err)
	}
	if err := a.reportNotSwitched(ctx, notSwitchedSummary); err != nil {
		t.Fatalf("not_switched report with lapsed authority: %v", err)
	}
	a.assertNotSwitchedFinalized(t)
}

// A plain failure without an outcome is exactly today's block, even when every
// not_switched condition would hold.
func TestPlainFailureStillBlocksWhenEveryConditionHolds(t *testing.T) {
	a := newActivatedFixture(t, 42, true)
	if err := a.report(context.Background(), false, notSwitchedSummary); err != nil {
		t.Fatal(err)
	}
	a.assertBlocked(t, 1, a.predecessorID, notSwitchedSummary)
}

func TestNotSwitchedInvalidOutcomeShapesWriteNothing(t *testing.T) {
	ctx := context.Background()
	a := newActivatedFixture(t, 43, true)
	for name, call := range map[string]func() error{
		"unknown outcome": func() error {
			return a.repo.ReportActivationResultWithOutcome(ctx, a.serverID, a.cred, a.attemptID, 1, a.operationID, false, [32]byte{}, "x", "switched")
		},
		"not_switched with succeeded=true": func() error {
			return a.repo.ReportActivationResultWithOutcome(ctx, a.serverID, a.cred, a.attemptID, 1, a.operationID, true, a.routeDigest, "", queue.ActivationOutcomeNotSwitched)
		},
	} {
		if err := call(); !errors.Is(err, ErrAgentLeaseInvalidInput) {
			t.Fatalf("%s: err=%v, want ErrAgentLeaseInvalidInput", name, err)
		}
		a.assertNothingRecorded(t)
	}
}

// Each case breaks exactly ONE condition of the finalize and must produce
// today's block. A mutation that removes the matching check makes its case
// finalize instead (verified by hand, see the slice report).
func TestNotSwitchedWithAnUnmetConditionFallsBackToBlock(t *testing.T) {
	ctx := context.Background()
	type tc struct {
		name     string
		strategy string
		noPred   bool
		mutate   func(a activatedFixture)
	}
	cases := []tc{
		{name: "blue_green strategy", strategy: "blue_green"},
		{name: "first deployment has no predecessor", noPred: true},
		{name: "rollback job", mutate: func(a activatedFixture) {
			a.force(t, `UPDATE agent_jobs SET job_kind='rollback_execution', rollback_target_release_id=$2 WHERE id=$1`, a.jobID, a.predecessorID)
		}},
		{name: "deployment not activating", mutate: func(a activatedFixture) {
			// Rewind the aggregate CONSISTENTLY (status, revision and its event
			// trail agree) to health_checking, a state from which failed is a
			// legal transition: only the explicit condition can refuse it.
			a.force(t, `WITH d AS (SELECT deployment_id AS id FROM agent_jobs WHERE id=$1),
				del AS (DELETE FROM deployment_events e USING d WHERE e.deployment_id=d.id AND e.to_status='activating' RETURNING 1)
				UPDATE deployments SET status='health_checking', revision=revision-1, updated_at=(SELECT max(e.occurred_at) FROM deployment_events e, d WHERE e.deployment_id=d.id AND e.to_status<>'activating') WHERE id=(SELECT id FROM d)`, a.jobID)
		}},
		{name: "route revision moved", mutate: func(a activatedFixture) {
			a.force(t, `UPDATE route_heads SET route_revision=2 WHERE environment_id=$1`, a.envID)
		}},
		{name: "route head active release moved", mutate: func(a activatedFixture) {
			a.force(t, `UPDATE route_heads SET active_release_id=$2 WHERE environment_id=$1`, a.envID, a.candidateID)
		}},
		{name: "route head already flagged", mutate: func(a activatedFixture) { a.setRouteHeadFlag(t, true) }},
		{name: "predecessor release not active", mutate: func(a activatedFixture) {
			a.force(t, `UPDATE releases SET state='retained' WHERE id=$1`, a.predecessorID)
		}},
		{name: "candidate release not ready", mutate: func(a activatedFixture) {
			a.force(t, `UPDATE releases SET state='retained' WHERE id=$1`, a.candidateID)
		}},
		{name: "job cancel requested", mutate: func(a activatedFixture) {
			a.force(t, `UPDATE agent_jobs SET cancel_requested_at=clock_timestamp() WHERE id=$1`, a.jobID)
		}},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			strategy := c.strategy
			if strategy == "" {
				strategy = "stop_then_start"
			}
			a := newActivatedFixtureWithStrategy(t, byte(44+i), !c.noPred, strategy)
			if c.mutate != nil {
				c.mutate(a)
			}
			if err := a.reportNotSwitched(ctx, notSwitchedSummary); err != nil {
				t.Fatalf("err=%v, want today's block answer (nil)", err)
			}
			a.assertFellBackToBlock(t, notSwitchedSummary)
			if got := a.deploymentStatusRaw(t); got == "failed" {
				t.Fatal("deployment must not be finalized")
			}
		})
	}
}

func (a activatedFixture) deploymentStatusRaw(t *testing.T) string {
	t.Helper()
	var s string
	if err := a.pool.QueryRow(context.Background(), `SELECT d.status FROM deployments d JOIN agent_jobs j ON j.deployment_id=d.id WHERE j.id=$1`, a.jobID).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestNotSwitchedNonOccupantIsStaleAndWritesNothing(t *testing.T) {
	ctx := context.Background()
	for i, how := range occupancyBreaks {
		t.Run(how, func(t *testing.T) {
			a := newActivatedFixture(t, byte(60+i), true)
			a.breakOccupancy(t, how)
			if err := a.reportNotSwitched(ctx, notSwitchedSummary); !errors.Is(err, ErrAgentLeaseStale) {
				t.Fatalf("err=%v, want ErrAgentLeaseStale", err)
			}
			if state, _, _ := a.operation(t); state != "switch_authorized" {
				t.Fatalf("operation=%s, want untouched", state)
			}
		})
	}
}

func TestNotSwitchedReplays(t *testing.T) {
	ctx := context.Background()
	t.Run("finalized identical report is an idempotent success", func(t *testing.T) {
		a := newActivatedFixture(t, 70, true)
		if err := a.reportNotSwitched(ctx, notSwitchedSummary); err != nil {
			t.Fatal(err)
		}
		before := a.jobEvents(t)
		if err := a.reportNotSwitched(ctx, notSwitchedSummary); err != nil {
			t.Fatalf("identical replay err=%v, want nil", err)
		}
		a.assertNotSwitchedFinalized(t)
		if got := a.jobEvents(t); strings.Join(got, ",") != strings.Join(before, ",") {
			t.Fatalf("replay wrote events: %v -> %v", before, got)
		}
	})
	t.Run("finalized different text is an integrity conflict", func(t *testing.T) {
		a := newActivatedFixture(t, 71, true)
		if err := a.reportNotSwitched(ctx, notSwitchedSummary); err != nil {
			t.Fatal(err)
		}
		if err := a.reportNotSwitched(ctx, "something else"); !errors.Is(err, ErrAgentLeaseIntegrityConflict) {
			t.Fatalf("err=%v, want ErrAgentLeaseIntegrityConflict", err)
		}
		a.assertNotSwitchedFinalized(t)
	})
	t.Run("finalized plain failure replay is an integrity conflict", func(t *testing.T) {
		a := newActivatedFixture(t, 72, true)
		if err := a.reportNotSwitched(ctx, notSwitchedSummary); err != nil {
			t.Fatal(err)
		}
		if err := a.report(ctx, false, notSwitchedSummary); !errors.Is(err, ErrAgentLeaseIntegrityConflict) {
			t.Fatalf("err=%v, want ErrAgentLeaseIntegrityConflict", err)
		}
	})
	t.Run("finalized success report is refused", func(t *testing.T) {
		a := newActivatedFixture(t, 73, true)
		if err := a.reportNotSwitched(ctx, notSwitchedSummary); err != nil {
			t.Fatal(err)
		}
		if err := a.report(ctx, true, ""); err == nil {
			t.Fatal("a success report against a finalized failure must be refused")
		}
		a.assertNotSwitchedFinalized(t)
	})
	t.Run("fallback block replay is the original block replay", func(t *testing.T) {
		a := newActivatedFixtureWithStrategy(t, 74, true, "blue_green")
		if err := a.reportNotSwitched(ctx, notSwitchedSummary); err != nil {
			t.Fatal(err)
		}
		before := a.jobEvents(t)
		if err := a.reportNotSwitched(ctx, notSwitchedSummary); err != nil {
			t.Fatalf("replay of the report that created the block err=%v, want nil", err)
		}
		a.assertFellBackToBlock(t, notSwitchedSummary)
		if got := a.jobEvents(t); strings.Join(got, ",") != strings.Join(before, ",") {
			t.Fatalf("replay wrote events: %v -> %v", before, got)
		}
	})
	t.Run("maximum-length summary finalizes and replays", func(t *testing.T) {
		a := newActivatedFixture(t, 75, true)
		long := strings.Repeat("é", 1024) // 2048 bytes; the store keeps 512 (prefix included)
		if err := a.reportNotSwitched(ctx, long); err != nil {
			t.Fatalf("max-length summary: %v", err)
		}
		if _, _, summary := a.operation(t); len(summary) != 512 || !strings.HasPrefix(summary, "not_switched: ") {
			t.Fatalf("summary len=%d prefix ok=%v", len(summary), strings.HasPrefix(summary, "not_switched: "))
		}
		if err := a.reportNotSwitched(ctx, long); err != nil {
			t.Fatalf("replay of a max-length summary err=%v, want nil", err)
		}
	})
}

// recordNotSwitchedStages runs one successful not_switched report and returns
// the stages the repository reached.
func recordNotSwitchedStages(t *testing.T, seed byte) []string {
	t.Helper()
	a := newActivatedFixture(t, seed, true)
	var stages []string
	a.repo.failAfter = func(stage string) error {
		stages = append(stages, stage)
		return nil
	}
	if err := a.reportNotSwitched(context.Background(), notSwitchedSummary); err != nil {
		t.Fatal(err)
	}
	a.assertNotSwitchedFinalized(t)
	return stages
}

// boundedNotSwitchedCtx bounds a report that can reach the fresh-transaction
// block (notSwitchedBlockFresh). That fallback re-takes the slot lock, so if
// the abandoned transaction were not rolled back first
// (agent_lease_repository.go: `_ = tx.Rollback(ctx)` before
// recordFailureBlockInFreshTransaction) the report would wait on its own lock
// forever; the bound turns that regression into a fast, explicit failure.
func boundedNotSwitchedCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// failIfDeadline names the probable cause when a bounded report ran out of time.
func failIfDeadline(t *testing.T, err error) {
	t.Helper()
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the report did not finish within its bound (%v): the abandoned finalize transaction is probably still holding the slot lock for the fresh-transaction block", err)
	}
}

func TestNotSwitchedIsAtomic(t *testing.T) {
	stages := recordNotSwitchedStages(t, 80)
	seen := map[string]bool{}
	for _, s := range stages {
		seen[s] = true
	}
	for _, want := range []string{"not_switched_savepoint", "not_switched_checks", "operation_close", "slot_reconcile", "deployment", "attempt_close", "slot_free", "job_close"} {
		if !seen[want] {
			t.Fatalf("not_switched path never reached %q (recorded %v)", want, stages)
		}
	}
	failNth := func(a activatedFixture, stage string, index int, boom error) {
		occurrence := 0
		for _, earlier := range stages[:index] {
			if earlier == stage {
				occurrence++
			}
		}
		seenSoFar := 0
		a.repo.failAfter = func(s string) error {
			if s != stage {
				return nil
			}
			seenSoFar++
			if seenSoFar-1 == occurrence {
				return boom
			}
			return nil
		}
	}
	for index, stage := range stages {
		if preOccupantStages[stage] {
			continue
		}
		t.Run("deterministic/"+stage+"/"+strconv.Itoa(index), func(t *testing.T) {
			ctx := boundedNotSwitchedCtx(t)
			a := newActivatedFixture(t, 81, true)
			failNth(a, stage, index, errors.New("injected"))
			// Today's block answer: the failure is recorded and nothing of the
			// finalize survives.
			err := a.reportNotSwitched(ctx, notSwitchedSummary)
			failIfDeadline(t, err)
			if err != nil {
				t.Fatalf("err=%v, want nil (block recorded)", err)
			}
			a.assertFellBackToBlock(t, notSwitchedSummary)
			if got := a.deploymentStatus(t); got != deployment.StatusActivating {
				t.Fatalf("deployment=%s, want activating", got)
			}
			if rev, active, _ := a.routeHead(t); rev != 1 || active == nil || *active != a.predecessorID {
				t.Fatalf("route head revision=%d active=%v moved", rev, active)
			}
		})
		t.Run("transient/"+stage+"/"+strconv.Itoa(index), func(t *testing.T) {
			ctx := boundedNotSwitchedCtx(t)
			a := newActivatedFixture(t, 82, true)
			boom := &pgconn.PgError{Code: "40001"}
			failNth(a, stage, index, boom)
			err := a.reportNotSwitched(ctx, notSwitchedSummary)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "40001" {
				t.Fatalf("err=%v, want the retryable 40001", err)
			}
			a.assertNothingRecorded(t)
			a.repo.failAfter = nil
			if err := a.reportNotSwitched(ctx, notSwitchedSummary); err != nil {
				t.Fatalf("retry: %v", err)
			}
			a.assertNotSwitchedFinalized(t)
		})
	}
}

// If the finalize cannot be written AND the fall-back block cannot be written
// either, nothing durable exists and the answer must be retryable, never a
// terminal one (the lost-report class of bug).
func TestNotSwitchedFinalizeAndFallbackBlockBothFailingIsRetryable(t *testing.T) {
	ctx := boundedNotSwitchedCtx(t)
	a := newActivatedFixture(t, 83, true)
	a.repo.failAfter = func(stage string) error {
		if stage == "slot_reconcile" { // every block write fails, in the branch and in the fall-back
			return errors.New("injected block failure")
		}
		return nil
	}
	err := a.reportNotSwitched(ctx, notSwitchedSummary)
	failIfDeadline(t, err)
	if !errors.Is(err, ErrActivationBlockUnrecorded) {
		t.Fatalf("err=%v, want ErrActivationBlockUnrecorded (retryable)", err)
	}
	a.assertNothingRecorded(t)
	if got := a.deploymentStatus(t); got != deployment.StatusActivating {
		t.Fatalf("deployment=%s, want activating", got)
	}
	a.repo.failAfter = nil
	if err := a.reportNotSwitched(ctx, notSwitchedSummary); err != nil {
		t.Fatalf("retry: %v", err)
	}
	a.assertNotSwitchedFinalized(t)
}

// installFailedCommitRefusalOnce makes the COMMIT of the first transaction
// that moves THIS operation to 'failed' fail with a deferred trigger.
func (a activatedFixture) installFailedCommitRefusalOnce(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	name := "test_failed_commit_once_" + a.operationID
	drop := func() {
		_, _ = a.pool.Exec(ctx, `DROP TRIGGER IF EXISTS `+name+` ON release_operations`)
		_, _ = a.pool.Exec(ctx, `DROP FUNCTION IF EXISTS `+name+`()`)
		_, _ = a.pool.Exec(ctx, `DROP SEQUENCE IF EXISTS `+name+`_seq`)
	}
	drop()
	if _, err := a.pool.Exec(ctx, `CREATE SEQUENCE `+name+`_seq`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(drop)
	if _, err := a.pool.Exec(ctx, `CREATE FUNCTION `+name+`() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
		IF NEW.state = 'failed' AND nextval('`+name+`_seq') = 1 THEN RAISE EXCEPTION 'test failed-commit refused once for operation %', NEW.id; END IF;
		RETURN NULL; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.pool.Exec(ctx, `CREATE CONSTRAINT TRIGGER `+name+` AFTER UPDATE ON release_operations DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.id = `+a.operationID+`) EXECUTE FUNCTION `+name+`()`); err != nil {
		t.Fatal(err)
	}
}

// A deterministic COMMIT failure of the finalize transaction falls back to the
// existing fresh-transaction block (today's answer: nil, block recorded).
func TestNotSwitchedCommitFailureFallsBackToFreshTransactionBlock(t *testing.T) {
	a := newActivatedFixture(t, 84, true)
	a.installFailedCommitRefusalOnce(t)
	err := a.reportNotSwitched(boundedNotSwitchedCtx(t), notSwitchedSummary)
	failIfDeadline(t, err)
	if err != nil {
		t.Fatalf("err=%v, want nil: the failure was recorded by the fallback", err)
	}
	a.assertFellBackToBlock(t, notSwitchedSummary)
	if got := a.deploymentStatus(t); got != deployment.StatusActivating {
		t.Fatalf("deployment=%s, want activating", got)
	}
}

// --- genuine contention -------------------------------------------------

func (a activatedFixture) assertNotSwitchedOrBlockedConsistent(t *testing.T) (finalized bool) {
	t.Helper()
	state, _, _ := a.operation(t)
	switch state {
	case "failed":
		a.assertNotSwitchedFinalized(t)
		return true
	case "reconciliation_required":
		if _, _, flagged := a.routeHead(t); !flagged {
			t.Fatal("blocked operation without a flagged route head")
		}
		if _, current, _, reconciling := a.slotRow(t); current == nil || !reconciling {
			t.Fatal("blocked operation without a held, flagged slot")
		}
		if got := a.deploymentStatus(t); got != deployment.StatusActivating {
			t.Fatalf("blocked operation but deployment=%s", got)
		}
		return false
	}
	t.Fatalf("operation state=%s, want failed or reconciliation_required", state)
	return false
}

func TestNotSwitchedGenuinelyRacesComplete(t *testing.T) {
	for _, order := range []string{"tx2 first", "Complete first"} {
		t.Run(order, func(t *testing.T) {
			a := newActivatedFixture(t, 90, true)
			var completeErr, reportErr error
			tx2 := func(ctx context.Context) { reportErr = a.reportNotSwitched(ctx, notSwitchedSummary) }
			complete := func(ctx context.Context) {
				completeErr = a.repo.Complete(ctx, a.serverID, a.cred, a.attemptID, 1, queue.CompletionFailed, "permanent_configuration", "op-racing-complete", digest(9))
			}
			calls := []func(context.Context){complete, tx2}
			if order == "tx2 first" {
				calls = []func(context.Context){tx2, complete}
			}
			holdSlotAndRace(t, a.leaseFixture, calls...)
			if reportErr != nil {
				t.Fatalf("tx2 err=%v", reportErr)
			}
			if !a.assertNotSwitchedOrBlockedConsistent(t) {
				t.Fatal("tx2 reported success but the operation is not finalized")
			}
			if order == "Complete first" && !errors.Is(completeErr, ErrAgentLeaseActivationOpen) {
				t.Fatalf("Complete queued before tx2 err=%v, want ErrAgentLeaseActivationOpen", completeErr)
			}
			ev := a.jobEvents(t)
			if countEvents(ev, "activation_not_activated") != 1 || countEvents(ev, "failed") != 1 {
				t.Fatalf("events=%v, want exactly one finalization", ev)
			}
		})
	}
}

// pauseAt returns a failAfter hook that, at the first occurrence of stage,
// announces itself on entered and blocks until release is closed.
func pauseAt(stage string, entered chan<- struct{}, release <-chan struct{}) func(string) error {
	var once sync.Once
	return func(s string) error {
		if s == stage {
			once.Do(func() {
				close(entered)
				<-release
			})
		}
		return nil
	}
}

// The sweeper's per-attempt transaction takes its locks with SKIP LOCKED, so
// contention with it is "one holds the locks, the other runs inside that
// window": the first transaction is paused INSIDE its transaction (holding the
// slot lock) while the second runs.
func TestNotSwitchedGenuinelyRacesSweeper(t *testing.T) {
	lapse := func(a activatedFixture) {
		if _, err := a.pool.Exec(context.Background(), `UPDATE agent_job_attempts SET lease_expires_at=now()-interval '2 seconds', authority_expires_at=now()-interval '2 seconds' WHERE id=$1`, a.attemptID); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("tx2 first the sweeper skips the locked slot", func(t *testing.T) {
		a := newActivatedFixture(t, 91, true)
		lapse(a)
		sweeper := NewAgentLeaseRepository(a.pool, NewDeploymentRepository(a.pool))
		entered, release := make(chan struct{}), make(chan struct{})
		a.repo.failAfter = pauseAt("not_switched_checks", entered, release)
		done := make(chan error, 1)
		go func() { done <- a.reportNotSwitched(context.Background(), notSwitchedSummary) }()
		<-entered
		closed, err := sweeper.expireOne(context.Background(), a.attemptID)
		if err != nil || closed {
			close(release)
			t.Fatalf("sweeper during tx2: closed=%v err=%v, want a skip", closed, err)
		}
		close(release)
		if err := <-done; err != nil {
			t.Fatalf("tx2 err=%v", err)
		}
		a.assertNotSwitchedFinalized(t)
	})
	t.Run("sweeper first the report is late evidence", func(t *testing.T) {
		a := newActivatedFixture(t, 92, true)
		lapse(a)
		sweeper := NewAgentLeaseRepository(a.pool, NewDeploymentRepository(a.pool))
		entered, release := make(chan struct{}), make(chan struct{})
		sweeper.failAfter = pauseAt("operation_close", entered, release)
		sweepDone := make(chan error, 1)
		go func() { _, err := sweeper.expireOne(context.Background(), a.attemptID); sweepDone <- err }()
		<-entered
		reportDone := make(chan error, 1)
		go func() { reportDone <- a.reportNotSwitched(context.Background(), notSwitchedSummary) }()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		waitForLockWaiters(t, ctx, a.pool, "environment_slots", 1)
		close(release)
		if err := <-sweepDone; err != nil {
			t.Fatalf("sweeper err=%v", err)
		}
		if err := <-reportDone; !errors.Is(err, ErrAgentLeaseActivationLateReportRecorded) {
			t.Fatalf("report err=%v, want ErrAgentLeaseActivationLateReportRecorded", err)
		}
		if a.assertNotSwitchedOrBlockedConsistent(t) {
			t.Fatal("a report that lost to the sweeper's diversion must never finalize")
		}
	})
}

func TestNotSwitchedGenuinelyRacesCancellation(t *testing.T) {
	for _, order := range []string{"tx2 first", "cancel first"} {
		t.Run(order, func(t *testing.T) {
			a := newActivatedFixture(t, 92, true)
			var reportErr, cancelErr error
			tx2 := func(ctx context.Context) { reportErr = a.reportNotSwitched(ctx, notSwitchedSummary) }
			cancel := func(ctx context.Context) { cancelErr = a.repo.RequestCancellation(ctx, a.principal, a.jobID) }
			calls := []func(context.Context){cancel, tx2}
			if order == "tx2 first" {
				calls = []func(context.Context){tx2, cancel}
			}
			holdSlotAndRace(t, a.leaseFixture, calls...)
			if reportErr != nil {
				t.Fatalf("tx2 err=%v", reportErr)
			}
			if !a.assertNotSwitchedOrBlockedConsistent(t) {
				t.Fatal("tx2 reported success but the operation is not finalized")
			}
			// The deployment is activating when cancel runs first: refused. When
			// tx2 ran first the job is already failed: terminal.
			want := ErrAgentJobActivating
			if order == "tx2 first" {
				want = ErrAgentJobTerminal
			}
			if !errors.Is(cancelErr, want) {
				t.Fatalf("cancel err=%v, want %v", cancelErr, want)
			}
			if _, _, cancelRequestedAt, _ := a.jobRow(t); cancelRequestedAt != nil {
				t.Fatal("cancellation must not be recorded")
			}
		})
	}
}

func TestNotSwitchedGenuinelyRacesResolveActivation(t *testing.T) {
	for _, order := range []string{"tx2 first", "resolve first"} {
		t.Run(order, func(t *testing.T) {
			a := newActivatedFixture(t, 93, true)
			var reportErr, resolveErr error
			tx2 := func(ctx context.Context) { reportErr = a.reportNotSwitched(ctx, notSwitchedSummary) }
			resolve := func(ctx context.Context) {
				resolveErr = a.repo.ResolveActivation(ctx, a.principal, a.envID, a.attemptID, ActivationOutcomeNotActivated, resolveReason)
			}
			calls := []func(context.Context){resolve, tx2}
			if order == "tx2 first" {
				calls = []func(context.Context){tx2, resolve}
			}
			holdSlotAndRace(t, a.leaseFixture, calls...)
			if reportErr != nil {
				t.Fatalf("tx2 err=%v", reportErr)
			}
			// ResolveActivation can never act on a switch_authorized operation
			// and, once tx2 finalized, finds nothing to reconcile.
			if resolveErr == nil {
				t.Fatal("ResolveActivation must be refused in both orders")
			}
			if !a.assertNotSwitchedOrBlockedConsistent(t) {
				t.Fatal("tx2 reported success but the operation is not finalized")
			}
			if countEvents(a.jobEvents(t), "reconciled") != 0 {
				t.Fatalf("events=%v: a declared resolution must not have been written", a.jobEvents(t))
			}
		})
	}
}

// Two identical not_switched reports contend on the slot row: exactly one
// finalizes, the other is the idempotent replay.
func TestNotSwitchedGenuinelyRacesItself(t *testing.T) {
	a := newActivatedFixture(t, 94, true)
	var errs [2]error
	call := func(i int) func(ctx context.Context) {
		return func(ctx context.Context) { errs[i] = a.reportNotSwitched(ctx, notSwitchedSummary) }
	}
	holdSlotAndRace(t, a.leaseFixture, call(0), call(1))
	for i, err := range errs {
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	a.assertNotSwitchedFinalized(t)
}
