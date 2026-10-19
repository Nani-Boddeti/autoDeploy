//go:build integration

package postgres

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"autodeploy/internal/deployment"
)

// Step 10c-2b step E: a report that reaches tx2 after the operation is already
// reconciliation_required is recorded as evidence and finalizes nothing. Every
// scenario below is built from real calls (Activate, a real ambiguous
// ReportActivationResult or a real sweeper diversion). The only SQL forced is
// stated at each use.

// lateSnapshot is the whole-row state a late report must leave alone. The
// operation row is compared WITHOUT outcome_summary (the one column a late
// report may change); outcome_summary and the events are asserted separately.
type lateSnapshot struct {
	diversionSnapshot
	operationExceptSummary string
	candidate, predecessor string
	activatedRouteEvents   int
	closedAt               string
}

func (a activatedFixture) lateSnapshot(t *testing.T) lateSnapshot {
	t.Helper()
	s := lateSnapshot{
		diversionSnapshot:      a.snapshot(t),
		operationExceptSummary: a.rowJSON(t, `SELECT (to_jsonb(o) - 'outcome_summary')::text AS op FROM release_operations o WHERE attempt_id=$1 AND kind='activate'`, a.attemptID),
		activatedRouteEvents:   a.activatedRouteEvents(t),
	}
	s.candidate, _ = a.releaseState(t, a.candidateID)
	if a.predecessorID != "" {
		s.predecessor, _ = a.releaseState(t, a.predecessorID)
	}
	_, closedAt, _ := a.operation(t)
	if closedAt != nil {
		s.closedAt = closedAt.String()
	}
	return s
}

// assertNothingFinalized asserts that, relative to before, nothing but the
// operation's outcome_summary and the event log moved: the operation is still
// reconciliation_required with the SAME closed_at, the attempt is still the
// leased occupant, the deployment is still activating, the releases and route
// head are byte-identical, and no activated route event exists.
func (a activatedFixture) assertNothingFinalized(t *testing.T, before lateSnapshot) lateSnapshot {
	t.Helper()
	after := a.lateSnapshot(t)
	if after.operationExceptSummary != before.operationExceptSummary {
		t.Fatalf("operation row changed beyond outcome_summary:\nbefore %s\nafter  %s", before.operationExceptSummary, after.operationExceptSummary)
	}
	if after.closedAt != before.closedAt || after.closedAt == "" {
		t.Fatalf("closed_at %q -> %q, want unchanged and set", before.closedAt, after.closedAt)
	}
	for name, pair := range map[string][2]string{
		"attempt": {before.attempt, after.attempt}, "slot": {before.slot, after.slot}, "job": {before.job, after.job},
		"deployment": {before.deployment, after.deployment}, "route head": {before.routeHead, after.routeHead},
		"candidate release": {before.candidate, after.candidate}, "predecessor release": {before.predecessor, after.predecessor},
	} {
		if pair[0] != pair[1] {
			t.Fatalf("%s changed:\nbefore %s\nafter  %s", name, pair[0], pair[1])
		}
	}
	if state, _, _ := a.operation(t); state != "reconciliation_required" {
		t.Fatalf("operation state=%s, want reconciliation_required", state)
	}
	if st, _, _, _, _ := a.attemptRow(t, a.attemptID); st != "leased" {
		t.Fatalf("attempt=%s, want leased", st)
	}
	if got := a.deploymentStatus(t); got != deployment.StatusActivating {
		t.Fatalf("deployment=%s, want activating", got)
	}
	if after.candidate != "ready" || (a.predecessorID != "" && after.predecessor != "active") {
		t.Fatalf("releases candidate=%s predecessor=%s, want ready/active", after.candidate, after.predecessor)
	}
	if after.activatedRouteEvents != 0 {
		t.Fatalf("activated route events=%d, want 0", after.activatedRouteEvents)
	}
	return after
}

func countEvents(events []string, kind string) int {
	n := 0
	for _, e := range events {
		if e == kind {
			n++
		}
	}
	return n
}

func (a activatedFixture) lateReport(ctx context.Context, kind string) error {
	switch kind {
	case "success matching digest":
		return a.report(ctx, true, "")
	case "success wrong digest":
		wrong := a.routeDigest
		wrong[3] ^= 0xff
		return a.repo.ReportActivationResult(ctx, a.serverID, a.cred, a.attemptID, 1, a.operationID, true, wrong, "")
	case "different failure":
		return a.report(ctx, false, "late failure: reload\x00 exited\n137")
	}
	panic("unknown late report kind " + kind)
}

// assertLateReportRecorded runs the late report twice and asserts the
// evidence landed exactly once, nothing was finalized, and the replay changed
// nothing and returned the same answer.
func assertLateReportRecorded(t *testing.T, a activatedFixture, kind, wantInSummary string) {
	t.Helper()
	ctx := context.Background()
	_, _, baseSummary := a.operation(t)
	before := a.lateSnapshot(t)

	if err := a.lateReport(ctx, kind); !errors.Is(err, ErrAgentLeaseActivationLateReportRecorded) {
		t.Fatalf("late report err=%v, want ErrAgentLeaseActivationLateReportRecorded", err)
	}
	afterFirst := a.assertNothingFinalized(t, before)
	_, _, summary := a.operation(t)
	if !strings.HasPrefix(summary, baseSummary+" | late report #") {
		t.Fatalf("summary=%q does not APPEND to the original %q", summary, baseSummary)
	}
	if !strings.Contains(summary, wantInSummary) {
		t.Fatalf("summary=%q lacks evidence %q", summary, wantInSummary)
	}
	if strings.ContainsAny(summary, "\x00\n") {
		t.Fatalf("summary %q kept a control character", summary)
	}
	wantEvents := append(append([]string{}, before.events...), "activation_late_report")
	if !reflect.DeepEqual(afterFirst.events, wantEvents) {
		t.Fatalf("events=%v, want %v", afterFirst.events, wantEvents)
	}
	if n := countEvents(afterFirst.events, "activation_late_report"); n != 1 {
		t.Fatalf("late report events=%d, want exactly 1", n)
	}

	// Replay: identical report, same answer, ZERO writes (events, summary and
	// every row unchanged).
	if err := a.lateReport(ctx, kind); !errors.Is(err, ErrAgentLeaseActivationLateReportRecorded) {
		t.Fatalf("replayed late report err=%v, want the same sentinel", err)
	}
	afterReplay := a.assertNothingFinalized(t, afterFirst)
	if !reflect.DeepEqual(afterReplay.events, afterFirst.events) {
		t.Fatalf("replay changed events: %v -> %v", afterFirst.events, afterReplay.events)
	}
	if _, _, again := a.operation(t); again != summary {
		t.Fatalf("replay changed the summary:\nfirst  %q\nreplay %q", summary, again)
	}
}

func TestLateReportAfterAmbiguousTx2RecordsEvidence(t *testing.T) {
	for i, tc := range []struct{ kind, wantInSummary string }{
		{"success matching digest", "matches authorized digest: true"},
		{"success wrong digest", "matches authorized digest: false"},
		{"different failure", "late failure: reload  exited 137"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			a := newActivatedFixture(t, byte(100+i), true)
			// REAL ambiguous tx2 puts the operation at reconciliation_required
			// and flags the route head.
			if err := a.report(context.Background(), false, "reload timed out"); err != nil {
				t.Fatalf("ambiguous tx2: %v", err)
			}
			a.assertBlocked(t, 1, a.predecessorID, "reload timed out")
			assertLateReportRecorded(t, a, tc.kind, tc.wantInSummary)
		})
	}
}

// The other real path: the sweeper's diversion records the block and leaves
// the attempt leased, then the agent's report finally arrives. (Revocation is
// not driven: a revoked server cannot authenticate, so no report can follow.)
func TestLateReportAfterSweeperDiversionRecordsEvidence(t *testing.T) {
	for i, kind := range []string{"success matching digest", "success wrong digest", "different failure"} {
		t.Run(kind, func(t *testing.T) {
			a := newActivatedFixture(t, byte(105+i), true)
			a.arm(t, "expireOne")
			if n, err := a.sweepVia(t, "expireOne"); err != nil || n != 0 {
				t.Fatalf("sweep n=%d err=%v, want diverted (0 closed)", n, err)
			}
			if state, _, summary := a.operation(t); state != "reconciliation_required" || summary != activationBlockedByAuthorityEndSummary {
				t.Fatalf("diversion state=%s summary=%q", state, summary)
			}
			if st, _, _, _, _ := a.attemptRow(t, a.attemptID); st != "leased" {
				t.Fatalf("attempt=%s, want leased after diversion", st)
			}
			want := map[string]string{"success matching digest": "matches authorized digest: true", "success wrong digest": "matches authorized digest: false", "different failure": "late failure: reload  exited 137"}[kind]
			assertLateReportRecorded(t, a, kind, want)
		})
	}
}

// The original block's own report replayed AFTER evidence was appended must
// still be an idempotent success (nil), write nothing, and not be mistaken
// for new evidence.
func TestOriginalFailureReplayStillIdempotentAfterLateEvidence(t *testing.T) {
	ctx := context.Background()
	a := newActivatedFixture(t, 108, false)
	if err := a.report(ctx, false, "reload timed out"); err != nil {
		t.Fatal(err)
	}
	if err := a.lateReport(ctx, "success matching digest"); !errors.Is(err, ErrAgentLeaseActivationLateReportRecorded) {
		t.Fatal(err)
	}
	before := a.lateSnapshot(t)
	_, _, summary := a.operation(t)
	if err := a.report(ctx, false, "reload timed out"); err != nil {
		t.Fatalf("original failure replay err=%v, want nil", err)
	}
	after := a.assertNothingFinalized(t, before)
	if _, _, again := a.operation(t); again != summary || !reflect.DeepEqual(after.events, before.events) {
		t.Fatalf("replay wrote: summary %q -> %q events %v -> %v", summary, again, before.events, after.events)
	}
}

// Over-reach guard, the critical non-vacuity test: against a
// switch_authorized operation the NORMAL tx2 path runs and finalizes. A late
// report branch that was too broad would turn every successful activation
// into "evidence recorded".
func TestReportAgainstSwitchAuthorizedStillFinalizes(t *testing.T) {
	ctx := context.Background()
	a := newActivatedFixture(t, 109, true)
	if err := a.report(ctx, true, ""); err != nil {
		t.Fatalf("normal success report err=%v, want nil", err)
	}
	a.assertFinalized(t, 2)
	if n := countEvents(a.jobEvents(t), "activation_late_report"); n != 0 {
		t.Fatalf("late report events=%d, want 0 on the normal path", n)
	}
	if _, _, summary := a.operation(t); strings.Contains(summary, "late report") {
		t.Fatalf("summary=%q recorded as late evidence", summary)
	}
	// And a failure against a fresh switch_authorized blocks via the normal
	// path (a block, not a late report).
	b := newActivatedFixture(t, 110, false)
	if err := b.report(ctx, false, "boom"); err != nil {
		t.Fatalf("normal failure report err=%v, want nil", err)
	}
	b.assertBlocked(t, 0, "", "boom")
	if n := countEvents(b.jobEvents(t), "activation_late_report"); n != 0 {
		t.Fatalf("late report events=%d, want 0 on the normal path", n)
	}
}

// succeeded regression guard: behaviour unchanged by the new late-report arm.
func TestLateReportDoesNotAlterSucceededArm(t *testing.T) {
	ctx := context.Background()
	a := newActivatedFixture(t, 111, true)
	if err := a.report(ctx, true, ""); err != nil {
		t.Fatal(err)
	}
	if err := a.report(ctx, true, ""); err != nil {
		t.Fatalf("identical success replay err=%v, want nil", err)
	}
	if err := a.report(ctx, false, "late failure"); !errors.Is(err, ErrAgentLeaseIntegrityConflict) {
		t.Fatalf("failure after success err=%v, want ErrAgentLeaseIntegrityConflict", err)
	}
	if n := countEvents(a.jobEvents(t), "activation_late_report"); n != 0 {
		t.Fatalf("late report events=%d against a succeeded operation, want 0", n)
	}
	a.assertFinalized(t, 2)
}

// failed: NOT a state any production path reaches before step G, so the
// operation is forced to failed IN SQL (reconciliation_required -> failed is
// legal per migration 000021). What is proven is the dispatch arm only.
func TestLateReportAgainstFailedOperationIsTypedNotCorrupt(t *testing.T) {
	ctx := context.Background()
	for i, kind := range []string{"success matching digest", "different failure"} {
		t.Run(kind, func(t *testing.T) {
			a := newActivatedFixture(t, byte(112+i), true)
			if err := a.report(ctx, false, "reload timed out"); err != nil {
				t.Fatal(err)
			}
			if _, err := a.pool.Exec(ctx, `UPDATE release_operations SET state='failed' WHERE attempt_id=$1 AND kind='activate' AND state='reconciliation_required'`, a.attemptID); err != nil {
				t.Fatalf("force failed: %v", err)
			}
			before := a.snapshot(t)
			err := a.lateReport(ctx, kind)
			if !errors.Is(err, ErrAgentLeaseIntegrityConflict) {
				t.Fatalf("err=%v, want ErrAgentLeaseIntegrityConflict", err)
			}
			if errors.Is(err, ErrAgentLeaseCorruptState) {
				t.Fatalf("a failed operation must not be reported as corrupt state: %v", err)
			}
			if after := a.snapshot(t); !reflect.DeepEqual(after, before) {
				t.Fatalf("a report against a failed operation wrote something:\nbefore %+v\nafter  %+v", before, after)
			}
		})
	}
}

// Attempt-shape rule: evidence is accepted only from the leased occupant at
// the operation's fence. The slot fence is bumped IN SQL (no production path
// does this to a blocked slot) to model "the attempt no longer owns the slot".
func TestLateReportRefusedWhenAttemptIsNotTheOccupant(t *testing.T) {
	ctx := context.Background()
	a := newActivatedFixture(t, 114, true)
	if err := a.report(ctx, false, "reload timed out"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.pool.Exec(ctx, `UPDATE environment_slots SET fence=2 WHERE environment_id=$1`, a.envID); err != nil {
		t.Fatal(err)
	}
	before := a.snapshot(t)
	if err := a.lateReport(ctx, "success matching digest"); !errors.Is(err, ErrAgentLeaseStale) {
		t.Fatalf("err=%v, want ErrAgentLeaseStale", err)
	}
	if after := a.snapshot(t); !reflect.DeepEqual(after, before) {
		t.Fatalf("a refused late report wrote something:\nbefore %+v\nafter  %+v", before, after)
	}
}

// Evidence and its event are one transaction: a failure after the summary
// UPDATE rolls the summary back, and the retry then records it exactly once.
func TestLateReportEvidenceAndEventAreAtomic(t *testing.T) {
	ctx := context.Background()
	// late_report_operation: after the summary UPDATE, before the event;
	// late_report_event: after both, before COMMIT.
	for i, stage := range []string{"late_report_operation", "late_report_event"} {
		t.Run(stage[len("late_report_"):], func(t *testing.T) {
			a := newActivatedFixture(t, byte(115+4*i), false)
			if err := a.report(ctx, false, "reload timed out"); err != nil {
				t.Fatal(err)
			}
			before := a.snapshot(t)
			_, _, baseSummary := a.operation(t)
			a.repo.failAfter = func(s string) error {
				if s == stage {
					return errors.New("injected after " + stage)
				}
				return nil
			}
			err := a.lateReport(ctx, "success matching digest")
			if err == nil || errors.Is(err, ErrAgentLeaseActivationLateReportRecorded) || !strings.Contains(err.Error(), "injected") {
				t.Fatalf("err=%v, want the injected (retryable) failure", err)
			}
			if _, _, got := a.operation(t); got != baseSummary {
				t.Fatalf("summary=%q after a failed attempt, want unchanged %q", got, baseSummary)
			}
			if after := a.snapshot(t); !reflect.DeepEqual(after, before) {
				t.Fatalf("a failed late report left a partial write:\nbefore %+v\nafter  %+v", before, after)
			}
			a.repo.failAfter = nil
			assertLateReportRecorded(t, a, "success matching digest", "matches authorized digest: true")
		})
	}
}

// Real-database bound: the column CHECK is octet_length <= 2048 and no
// control characters. Distinct late reports fill the column; every write must
// be accepted by the CHECK, valid UTF-8, never exceed the cap, and once the
// marker no longer fits nothing more is written and no more events appear.
func TestLateReportFillsColumnWithoutExceedingTheCheck(t *testing.T) {
	ctx := context.Background()
	a := newActivatedFixture(t, 116, false)
	// 170 three-byte runes = 510 bytes: the largest base the sanitizer allows.
	if err := a.report(ctx, false, strings.Repeat("€", 170)); err != nil {
		t.Fatal(err)
	}
	lateEvents := 0
	var last string
	for i := 0; i < 8; i++ {
		// 2-byte runes after an odd-length prefix keep cut points mid-rune.
		err := a.report(ctx, false, strconv.Itoa(i)+strings.Repeat("é", 250))
		if !errors.Is(err, ErrAgentLeaseActivationLateReportRecorded) {
			t.Fatalf("report %d err=%v", i, err)
		}
		_, _, summary := a.operation(t)
		if len(summary) > maxOutcomeSummaryBytes || !utf8.ValidString(summary) {
			t.Fatalf("report %d: len=%d valid=%v", i, len(summary), utf8.ValidString(summary))
		}
		now := countEvents(a.jobEvents(t), "activation_late_report")
		if summary == last && now != lateEvents {
			t.Fatalf("report %d changed events (%d -> %d) without changing the summary", i, lateEvents, now)
		}
		if summary != last && now != lateEvents+1 {
			t.Fatalf("report %d changed the summary but events %d -> %d", i, lateEvents, now)
		}
		last, lateEvents = summary, now
	}
	if lateEvents < 2 || lateEvents >= 8 {
		t.Fatalf("late events=%d, want the column to fill after a few and then stop", lateEvents)
	}
	// Replaying a report that fit is still a pure no-op once full.
	before := a.lateSnapshot(t)
	if err := a.report(ctx, false, "0"+strings.Repeat("é", 250)); !errors.Is(err, ErrAgentLeaseActivationLateReportRecorded) {
		t.Fatal(err)
	}
	a.assertNothingFinalized(t, before)
	if got := countEvents(a.jobEvents(t), "activation_late_report"); got != lateEvents {
		t.Fatalf("replay on a full column emitted an event: %d -> %d", lateEvents, got)
	}
}

// The success reserve, end to end: distinct failure reports fill the column
// until one is refused for room, and a later success report -- the decisive
// evidence -- is still recorded whole, with its event.
func TestLateSuccessReportSurvivesAFullFailureBudget(t *testing.T) {
	ctx := context.Background()
	a := newActivatedFixture(t, 117, false)
	if err := a.report(ctx, false, strings.Repeat("€", 170)); err != nil {
		t.Fatal(err)
	}
	lateEvents, full := 0, false
	for i := 0; i < 10 && !full; i++ {
		if err := a.report(ctx, false, strconv.Itoa(i)+strings.Repeat("é", 250)); !errors.Is(err, ErrAgentLeaseActivationLateReportRecorded) {
			t.Fatalf("failure report %d err=%v", i, err)
		}
		now := countEvents(a.jobEvents(t), "activation_late_report")
		full = now == lateEvents // refused for room: nothing written, no event
		lateEvents = now
	}
	if !full {
		t.Fatal("failure reports never ran out of room")
	}
	if err := a.report(ctx, true, ""); !errors.Is(err, ErrAgentLeaseActivationLateReportRecorded) {
		t.Fatalf("success report err=%v", err)
	}
	_, _, summary := a.operation(t)
	if !strings.Contains(summary, "agent reported success after the activation was blocked") || len(summary) > maxOutcomeSummaryBytes {
		t.Fatalf("success evidence missing or over the cap (len=%d): %q", len(summary), summary)
	}
	if got := countEvents(a.jobEvents(t), "activation_late_report"); got != lateEvents+1 {
		t.Fatalf("late events %d -> %d, want the success report's event added", lateEvents, got)
	}
}

func (a activatedFixture) successReportInput() activationReport {
	return activationReport{succeeded: true, digestMatches: true, observedRouteDigest: a.routeDigest}
}

func (a activatedFixture) failureReportInput(summary string) activationReport {
	return activationReport{failureSummary: failureOutcomeSummary(summary)}
}

// F1: tx2 rolled back (a deterministic error), released the slot lock, and a
// concurrent writer blocked the operation before the fresh transaction locked
// it. The report must not be lost: the fresh transaction records it as late
// evidence, with the fingerprint tx2's late path uses, so a retry is a replay.
// The interleaving is built deterministically by pre-blocking the operation
// with real paths and then calling each fresh-transaction entry point directly.
func TestFreshTransactionRecordsLateEvidenceWhenAlreadyBlocked(t *testing.T) {
	seed := byte(120)
	for _, preblock := range []string{"sweeper diversion", "failure report"} {
		for _, kind := range []string{"success", "failure"} {
			for _, entry := range []string{"blockAfterCommitFailure", "recordBlockInFreshTransaction"} {
				seed++
				t.Run(strings.Fields(preblock)[0]+"_"+kind+"_"+strings.TrimPrefix(entry, "block")[:5], func(t *testing.T) {
					ctx := context.Background()
					a := newActivatedFixture(t, seed, true)
					if preblock == "sweeper diversion" {
						a.arm(t, "expireOne")
						if n, err := a.sweepVia(t, "expireOne"); err != nil || n != 0 {
							t.Fatalf("sweep n=%d err=%v, want diverted", n, err)
						}
					} else if err := a.report(ctx, false, "reload timed out"); err != nil {
						t.Fatal(err)
					}
					_, _, original := a.operation(t)
					before := a.lateSnapshot(t)

					rep := a.successReportInput()
					retry := func() error { return a.report(ctx, true, "") }
					if kind == "failure" {
						rep = a.failureReportInput("late story")
						retry = func() error { return a.report(ctx, false, "late story") }
					}
					opID := mustParseInt(t, a.operationID)
					var err error
					var outcome activationBlockOutcome
					if entry == "blockAfterCommitFailure" {
						err = a.repo.blockAfterCommitFailure(ctx, a.serverID, a.attemptID, 1, opID, errors.New("commit failed"), rep)
					} else {
						outcome, err = a.repo.recordBlockInFreshTransaction(ctx, a.serverID, a.attemptID, 1, opID, "block evidence", rep)
						if err == nil && outcome != activationBlockLateRecorded {
							t.Fatalf("outcome=%v, want activationBlockLateRecorded", outcome)
						}
						if err == nil {
							err = ErrAgentLeaseActivationLateReportRecorded
						}
					}
					if !errors.Is(err, ErrAgentLeaseActivationLateReportRecorded) {
						t.Fatalf("err=%v, want ErrAgentLeaseActivationLateReportRecorded", err)
					}
					afterFirst := a.assertNothingFinalized(t, before)
					_, _, summary := a.operation(t)
					if !strings.HasPrefix(summary, original+" | late report #") || strings.Count(summary, "late report #") != 1 {
						t.Fatalf("summary=%q, want the original %q preserved plus one late entry", summary, original)
					}
					if n := countEvents(afterFirst.events, "activation_late_report"); n != 1 {
						t.Fatalf("late report events=%d, want exactly 1", n)
					}
					// Retry through tx2: a replay, byte-identical, no new event.
					if err := retry(); !errors.Is(err, ErrAgentLeaseActivationLateReportRecorded) {
						t.Fatalf("retry err=%v, want the same sentinel", err)
					}
					afterRetry := a.assertNothingFinalized(t, afterFirst)
					if _, _, again := a.operation(t); again != summary {
						t.Fatalf("retry changed the summary:\n%q\n%q", summary, again)
					}
					if n := countEvents(afterRetry.events, "activation_late_report"); n != 1 {
						t.Fatalf("late report events after retry=%d, want 1", n)
					}
				})
			}
		}
	}
}

// rr with a non-occupant shape: ErrAgentLeaseStale from every fresh-transaction
// entry point, and nothing is written.
func TestFreshTransactionRefusesLateEvidenceFromNonOccupant(t *testing.T) {
	ctx := context.Background()
	a := newActivatedFixture(t, 140, true)
	if err := a.report(ctx, false, "reload timed out"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.pool.Exec(ctx, `UPDATE environment_slots SET fence=2 WHERE environment_id=$1`, a.envID); err != nil {
		t.Fatal(err)
	}
	before := a.snapshot(t)
	opID := mustParseInt(t, a.operationID)
	if err := a.repo.blockAfterCommitFailure(ctx, a.serverID, a.attemptID, 1, opID, errors.New("commit failed"), a.successReportInput()); !errors.Is(err, ErrAgentLeaseStale) {
		t.Fatalf("err=%v, want ErrAgentLeaseStale", err)
	}
	if outcome, err := a.repo.recordBlockInFreshTransaction(ctx, a.serverID, a.attemptID, 1, opID, "block evidence", a.successReportInput()); err != nil || outcome != activationBlockStale {
		t.Fatalf("outcome=%v err=%v, want activationBlockStale", outcome, err)
	}
	if after := a.snapshot(t); !reflect.DeepEqual(after, before) {
		t.Fatalf("a refused late report wrote something:\nbefore %+v\nafter  %+v", before, after)
	}
}

// A failure report that created the block, retried through the fresh
// transaction after the slot moved, is still the durable original: answered
// like tx2's replay (recorded), not as a terminal stale lease.
func TestFreshTransactionOriginalBlockReplayBeatsTheOccupantCheck(t *testing.T) {
	ctx := context.Background()
	a := newActivatedFixture(t, 150, true)
	if err := a.report(ctx, false, "reload timed out"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.pool.Exec(ctx, `UPDATE environment_slots SET fence=2 WHERE environment_id=$1`, a.envID); err != nil {
		t.Fatal(err)
	}
	before := a.snapshot(t)
	outcome, err := a.repo.recordBlockInFreshTransaction(ctx, a.serverID, a.attemptID, 1, mustParseInt(t, a.operationID), "block evidence", a.failureReportInput("reload timed out"))
	if err != nil || outcome != activationBlockRecorded {
		t.Fatalf("outcome=%v err=%v, want activationBlockRecorded", outcome, err)
	}
	if after := a.snapshot(t); !reflect.DeepEqual(after, before) {
		t.Fatalf("the replay wrote something:\nbefore %+v\nafter  %+v", before, after)
	}
}

// A forced failure inside the fresh transaction's late arm is retryable
// (ErrActivationBlockUnrecorded, 503) and leaves nothing durable; a clean
// retry then records exactly one entry and one event.
func TestFreshTransactionLateArmFailureIsRetryableAndLeavesNothing(t *testing.T) {
	ctx := context.Background()
	for i, stage := range []string{"late_report_operation", "late_report_event", "commit"} {
		t.Run(stage, func(t *testing.T) {
			a := newActivatedFixture(t, byte(152+i), true)
			if err := a.report(ctx, false, "reload timed out"); err != nil {
				t.Fatal(err)
			}
			if stage == "commit" {
				// A real deferred trigger refuses the late write's COMMIT once.
				a.installBlockCommitFailureOnce(t)
			} else {
				a.repo.failAfter = func(s string) error {
					if s == stage {
						return errors.New("injected at " + stage)
					}
					return nil
				}
			}
			_, _, base := a.operation(t)
			before := a.snapshot(t)
			opID := mustParseInt(t, a.operationID)
			_, err := a.repo.recordBlockInFreshTransaction(ctx, a.serverID, a.attemptID, 1, opID, "block evidence", a.successReportInput())
			if !errors.Is(err, ErrActivationBlockUnrecorded) {
				t.Fatalf("err=%v, want ErrActivationBlockUnrecorded (retryable)", err)
			}
			if _, _, got := a.operation(t); got != base {
				t.Fatalf("summary changed after a failed attempt: %q -> %q", base, got)
			}
			if after := a.snapshot(t); !reflect.DeepEqual(after, before) {
				t.Fatalf("a failed late arm left a partial write:\nbefore %+v\nafter  %+v", before, after)
			}
			if n := countEvents(a.jobEvents(t), "activation_late_report"); n != 0 {
				t.Fatalf("late events=%d after a failed attempt, want 0", n)
			}
			a.repo.failAfter = nil
			outcome, err := a.repo.recordBlockInFreshTransaction(ctx, a.serverID, a.attemptID, 1, opID, "block evidence", a.successReportInput())
			if err != nil || outcome != activationBlockLateRecorded {
				t.Fatalf("retry outcome=%v err=%v, want activationBlockLateRecorded", outcome, err)
			}
			_, _, summary := a.operation(t)
			if !strings.HasPrefix(summary, base+" | late report #") || strings.Count(summary, "late report #") != 1 {
				t.Fatalf("summary=%q, want the original plus exactly one late entry", summary)
			}
			if n := countEvents(a.jobEvents(t), "activation_late_report"); n != 1 {
				t.Fatalf("late events=%d after the retry, want 1", n)
			}
		})
	}
}
