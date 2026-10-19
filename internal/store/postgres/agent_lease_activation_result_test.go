//go:build integration

package postgres

import (
	"context"
	"encoding/hex"
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

// activatedFixture is an attempt that has passed tx1 (Activate): operation
// switch_authorized, deployment activating, candidate release ready.
type activatedFixture struct {
	leaseFixture
	cred          [32]byte
	attemptID     int64
	operationID   string
	routeDigest   [32]byte
	candidateID   string
	predecessorID string // empty on a first deployment
	repo          *AgentLeaseRepository
}

// releaseSuffix makes t.Name() safe inside a release id (subtest names
// contain "/" and spaces, which the route document validator rejects).
func releaseSuffix(t *testing.T) string {
	return strings.NewReplacer("/", "-", " ", "-", "_", "-").Replace(t.Name())
}

func newActivatedFixture(t *testing.T, seed byte, withPredecessor bool) activatedFixture {
	t.Helper()
	f := newLeaseFixture(t, seed)
	cred := f.withCredential(t, seed+50)
	attemptID := f.leaseJob(t, 1, 90)
	f.advanceDeploymentToHealthChecking(t)
	f.insertRouteHead(t, "app.example.com")
	predecessorID := ""
	if withPredecessor {
		predecessorID = "predecessor-" + releaseSuffix(t)
		f.insertActiveReleaseAndPointRouteHead(t, predecessorID)
	}
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
		routeDigest: [32]byte(raw), candidateID: candidateID, predecessorID: predecessorID, repo: repo}
}

func (a activatedFixture) report(ctx context.Context, succeeded bool, summary string) error {
	if succeeded {
		return a.repo.ReportActivationResult(ctx, a.serverID, a.cred, a.attemptID, 1, a.operationID, true, a.routeDigest, "")
	}
	return a.repo.ReportActivationResult(ctx, a.serverID, a.cred, a.attemptID, 1, a.operationID, false, [32]byte{}, summary)
}

func (a activatedFixture) routeHead(t *testing.T) (revision int64, activeReleaseID *string, reconciling bool) {
	t.Helper()
	if err := a.pool.QueryRow(context.Background(), `SELECT route_revision, active_release_id, reconciliation_required FROM route_heads WHERE environment_id=$1`, a.envID).Scan(&revision, &activeReleaseID, &reconciling); err != nil {
		t.Fatal(err)
	}
	return
}

func (a activatedFixture) operation(t *testing.T) (state string, closedAt *time.Time, summary string) {
	t.Helper()
	if err := a.pool.QueryRow(context.Background(), `SELECT state, closed_at, outcome_summary FROM release_operations WHERE attempt_id=$1 AND kind='activate'`, a.attemptID).Scan(&state, &closedAt, &summary); err != nil {
		t.Fatal(err)
	}
	return
}

func (a activatedFixture) jobEvents(t *testing.T) []string {
	t.Helper()
	rows, err := a.pool.Query(context.Background(), `SELECT event_type FROM agent_job_events WHERE job_id=$1 AND attempt_id=$2 ORDER BY sequence`, a.jobID, a.attemptID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var events []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			t.Fatal(err)
		}
		events = append(events, e)
	}
	return events
}

func (a activatedFixture) activatedRouteEvents(t *testing.T) int {
	t.Helper()
	var n int
	if err := a.pool.QueryRow(context.Background(), `SELECT count(*) FROM route_events WHERE environment_id=$1 AND event_kind='activated'`, a.envID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// assertStillAuthorized asserts nothing tx2 writes has happened: the state
// left by Activate alone.
func (a activatedFixture) assertStillAuthorized(t *testing.T, wantRevision int64, wantActive string) {
	t.Helper()
	if state, _, _ := a.operation(t); state != "switch_authorized" {
		t.Fatalf("operation state=%s, want switch_authorized", state)
	}
	if state, _, _, _, _ := a.attemptRow(t, a.attemptID); state != "leased" {
		t.Fatalf("attempt state=%s, want leased", state)
	}
	if _, current, _, reconciling := a.slotRow(t); current == nil || *current != a.attemptID || reconciling {
		t.Fatalf("slot current=%v reconciling=%v, want occupied and unblocked", current, reconciling)
	}
	if state, _, _, _ := a.jobRow(t); state != "leased" {
		t.Fatalf("job state=%s, want leased", state)
	}
	if got := a.deploymentStatus(t); got != deployment.StatusActivating {
		t.Fatalf("deployment=%s, want activating", got)
	}
	revision, active, reconciling := a.routeHead(t)
	got := ""
	if active != nil {
		got = *active
	}
	if revision != wantRevision || got != wantActive || reconciling {
		t.Fatalf("route head revision=%d active=%q reconciling=%v, want %d %q false", revision, got, reconciling, wantRevision, wantActive)
	}
	if n := a.activatedRouteEvents(t); n != 0 {
		t.Fatalf("activated route events=%d, want 0", n)
	}
}

func (a activatedFixture) assertFinalized(t *testing.T, wantRevision int64) {
	t.Helper()
	ctx := context.Background()
	if state, closedAt, summary := a.operation(t); state != "succeeded" || closedAt == nil || summary == "" {
		t.Fatalf("operation state=%s closedAt=%v summary=%q", state, closedAt, summary)
	}
	if state, _ := a.releaseState(t, a.candidateID); state != "active" {
		t.Fatalf("candidate state=%s, want active", state)
	}
	if a.predecessorID != "" {
		if state, _ := a.releaseState(t, a.predecessorID); state != "retained" {
			t.Fatalf("predecessor state=%s, want retained", state)
		}
	}
	revision, active, reconciling := a.routeHead(t)
	if revision != wantRevision || active == nil || *active != a.candidateID || reconciling {
		t.Fatalf("route head revision=%d active=%v reconciling=%v, want %d %s false", revision, active, reconciling, wantRevision, a.candidateID)
	}
	var eventRevision int64
	var eventRelease *string
	if err := a.pool.QueryRow(ctx, `SELECT route_revision, active_release_id FROM route_events WHERE environment_id=$1 AND event_kind='activated'`, a.envID).Scan(&eventRevision, &eventRelease); err != nil {
		t.Fatalf("exactly one activated route event expected: %v", err)
	}
	if eventRevision != wantRevision || eventRelease == nil || *eventRelease != a.candidateID {
		t.Fatalf("route event revision=%d release=%v", eventRevision, eventRelease)
	}
	if got := a.deploymentStatus(t); got != deployment.StatusHealthy {
		t.Fatalf("deployment=%s, want healthy", got)
	}
	state, closedAt, _, resultOp, resultDigest := a.attemptRow(t, a.attemptID)
	if state != "succeeded" || closedAt == nil || resultOp == nil || *resultOp != a.operationID || len(resultDigest) != 32 {
		t.Fatalf("attempt state=%s closedAt=%v resultOp=%v", state, closedAt, resultOp)
	}
	fence, current, _, reconcilingSlot := a.slotRow(t)
	if fence != 1 || current != nil || reconcilingSlot {
		t.Fatalf("slot fence=%d current=%v reconciling=%v, want 1 nil false", fence, current, reconcilingSlot)
	}
	if jobState, _, _, _ := a.jobRow(t); jobState != "succeeded" {
		t.Fatalf("job state=%s, want succeeded", jobState)
	}
	events := a.jobEvents(t)
	want := []string{"activation_authorized", "activated", "succeeded"}
	if len(events) < 3 || strings.Join(events[len(events)-3:], ",") != strings.Join(want, ",") {
		t.Fatalf("job events=%v, want suffix %v", events, want)
	}
}

func TestReportActivationResultSuccessWithPredecessor(t *testing.T) {
	a := newActivatedFixture(t, 180, true)
	a.assertStillAuthorized(t, 1, a.predecessorID)
	if err := a.report(context.Background(), true, ""); err != nil {
		t.Fatalf("ReportActivationResult: %v", err)
	}
	a.assertFinalized(t, 2)
}

func TestReportActivationResultSuccessFirstDeployment(t *testing.T) {
	a := newActivatedFixture(t, 181, false)
	a.assertStillAuthorized(t, 0, "")
	if err := a.report(context.Background(), true, ""); err != nil {
		t.Fatalf("ReportActivationResult: %v", err)
	}
	a.assertFinalized(t, 1)
}

// TestReportActivationResultIgnoresLapsedAuthority pins the deliberate
// omission of the authority_expires_at check: ADR 0005 says an in-flight
// syscall cannot be preempted, so an authorized activation whose lease
// lapsed mid-syscall must still finalize.
func TestReportActivationResultIgnoresLapsedAuthority(t *testing.T) {
	a := newActivatedFixture(t, 182, true)
	// One shared timestamp: two separate clock_timestamp() calls can differ by
	// microseconds and trip CHECK (authority_expires_at <= lease_expires_at),
	// which made this test fail intermittently.
	if _, err := a.pool.Exec(context.Background(), `UPDATE agent_job_attempts SET lease_expires_at=t.lapsed, authority_expires_at=t.lapsed FROM (SELECT clock_timestamp() - interval '1 minute' AS lapsed) t WHERE id=$1`, a.attemptID); err != nil {
		t.Fatalf("lapse authority: %v", err)
	}
	if err := a.report(context.Background(), true, ""); err != nil {
		t.Fatalf("lapsed-authority activation must still finalize: %v", err)
	}
	a.assertFinalized(t, 2)
}

// assertBlocked asserts the durable ambiguous end state: operation
// reconciliation_required with the expected evidence, both reconciliation
// flags set, and everything else exactly as Activate left it -- attempt
// leased, slot occupied at the unchanged fence, job untouched, deployment
// still activating, route head revision/active release unchanged.
func (a activatedFixture) assertBlocked(t *testing.T, wantRevision int64, wantActive, wantSummaryContains string) {
	t.Helper()
	a.assertBlockedWithLate(t, wantRevision, wantActive, wantSummaryContains, 0)
}

// assertBlockedWithLate is assertBlocked after wantLate late reports: the
// flags, rows and the single block event are unchanged; only
// activation_late_report events may follow it, exactly wantLate of them.
func (a activatedFixture) assertBlockedWithLate(t *testing.T, wantRevision int64, wantActive, wantSummaryContains string, wantLate int) {
	t.Helper()
	state, closedAt, summary := a.operation(t)
	if state != "reconciliation_required" || closedAt == nil {
		t.Fatalf("operation state=%s closedAt=%v", state, closedAt)
	}
	if !strings.Contains(summary, wantSummaryContains) {
		t.Fatalf("summary=%q, want it to contain %q", summary, wantSummaryContains)
	}
	if attemptState, attemptClosed, _, resultOp, _ := a.attemptRow(t, a.attemptID); attemptState != "leased" || attemptClosed != nil || resultOp != nil {
		t.Fatalf("attempt state=%s closedAt=%v resultOp=%v, want untouched leased", attemptState, attemptClosed, resultOp)
	}
	fence, current, reconAttempt, reconciling := a.slotRow(t)
	if fence != 1 || current == nil || *current != a.attemptID || !reconciling || reconAttempt == nil || *reconAttempt != a.attemptID {
		t.Fatalf("slot fence=%d current=%v reconciling=%v reconAttempt=%v", fence, current, reconciling, reconAttempt)
	}
	if jobState, _, cancelRequestedAt, attemptCount := a.jobRow(t); jobState != "leased" || cancelRequestedAt != nil || attemptCount != 1 {
		t.Fatalf("job state=%s cancel=%v attempts=%d, want untouched", jobState, cancelRequestedAt, attemptCount)
	}
	if got := a.deploymentStatus(t); got != deployment.StatusActivating {
		t.Fatalf("deployment=%s, want activating", got)
	}
	revision, active, routeReconciling := a.routeHead(t)
	got := ""
	if active != nil {
		got = *active
	}
	if revision != wantRevision || got != wantActive || !routeReconciling {
		t.Fatalf("route head revision=%d active=%q reconciling=%v, want %d %q true", revision, got, routeReconciling, wantRevision, wantActive)
	}
	if st, _ := a.releaseState(t, a.candidateID); st != "ready" {
		t.Fatalf("candidate=%s, want ready", st)
	}
	if a.predecessorID != "" {
		if st, _ := a.releaseState(t, a.predecessorID); st != "active" {
			t.Fatalf("predecessor=%s, want active", st)
		}
	}
	if n := a.activatedRouteEvents(t); n != 0 {
		t.Fatalf("activated route events=%d, want 0", n)
	}
	events := a.jobEvents(t)
	blocks, lates, last := 0, 0, ""
	for _, e := range events {
		switch e {
		case "activation_reconciliation_required":
			blocks++
		case "activation_late_report":
			lates++
		}
		if e != "activation_late_report" {
			last = e
		}
	}
	if blocks != 1 || lates != wantLate || last != "activation_reconciliation_required" {
		t.Fatalf("job events=%v, want one block event (last non-late) and %d late reports", events, wantLate)
	}
	for _, e := range events {
		if e == "failed" || e == "dead" || e == "retry_scheduled" || e == "succeeded" || e == "activated" {
			t.Fatalf("unexpected terminal event %s in %v", e, events)
		}
	}
}

func TestReportActivationResultFailureBlocksEnvironment(t *testing.T) {
	ctx := context.Background()
	a := newActivatedFixture(t, 183, true)
	if err := a.report(ctx, false, "reload\x00 timed out\nafter 30s"); err != nil {
		t.Fatalf("ReportActivationResult failure: %v", err)
	}
	a.assertBlocked(t, 1, a.predecessorID, "reload  timed out after 30s")
	if _, _, summary := a.operation(t); summary != "reload  timed out after 30s" {
		t.Fatalf("summary=%q not sanitized", summary)
	}

	// Identical failure replay is idempotent (an agent whose response was
	// lost can tell "already recorded" from "lost lease"); it must not add
	// a second event.
	eventsBefore := len(a.jobEvents(t))
	if err := a.report(ctx, false, "reload\x00 timed out\nafter 30s"); err != nil {
		t.Fatalf("identical failure replay err=%v, want nil", err)
	}
	if got := len(a.jobEvents(t)); got != eventsBefore {
		t.Fatalf("replay appended events: %d -> %d", eventsBefore, got)
	}
	// Any other report against the blocked operation is a LATE report
	// (10c-2b step E): recorded as evidence, answered with the typed
	// non-retryable sentinel, never finalized. Full coverage of that path is in
	// agent_lease_activation_late_report_test.go.
	if err := a.report(ctx, false, "a different story"); !errors.Is(err, ErrAgentLeaseActivationLateReportRecorded) {
		t.Fatalf("different failure err=%v, want ErrAgentLeaseActivationLateReportRecorded", err)
	}
	if err := a.report(ctx, true, ""); !errors.Is(err, ErrAgentLeaseActivationLateReportRecorded) {
		t.Fatalf("success after failure err=%v, want ErrAgentLeaseActivationLateReportRecorded", err)
	}
	// Flags, rows and the single block event are unchanged by the late reports.
	a.assertBlockedWithLate(t, 1, a.predecessorID, "reload  timed out after 30s", 2)
	if _, _, summary := a.operation(t); !strings.HasPrefix(summary, "reload  timed out after 30s | late report #") || !strings.Contains(summary, "a different story") {
		t.Fatalf("summary=%q", summary)
	}
}

// TestReportActivationResultBlankFailureSummary runs against the real
// release_operations CHECK (octet_length <= 2048, no control characters, no
// minimum length): an empty or whitespace-only summary must be stored as a
// non-empty placeholder, and control characters must never reach the column.
func TestReportActivationResultBlankFailureSummary(t *testing.T) {
	for i, summary := range []string{"", "   \t\n  ", "\x00\x01\x7f"} {
		a := newActivatedFixture(t, byte(200+i), false)
		if err := a.report(context.Background(), false, summary); err != nil {
			t.Fatalf("summary %q: %v", summary, err)
		}
		a.assertBlocked(t, 0, "", emptyFailureSummary)
		if _, _, got := a.operation(t); got != emptyFailureSummary {
			t.Fatalf("summary %q stored as %q", summary, got)
		}
	}
}

func TestReportActivationResultRefusals(t *testing.T) {
	ctx := context.Background()
	// What this proves: a report carrying a fence other than the operation's
	// is refused with ErrAgentLeaseStale and writes nothing. It does NOT guard
	// the `attemptFence != fence` line in reportActivationResult: the operation
	// lookup is keyed by (attempt_id, fence), so it finds no row first, and
	// validate_release_operation_insert forces an operation's fence to equal
	// its attempt's, so no state exists in which that line is the deciding
	// check. It stays as unreachable defence in depth.
	t.Run("a stale fence is refused by the operation lookup", func(t *testing.T) {
		a := newActivatedFixture(t, 184, true)
		err := a.repo.ReportActivationResult(ctx, a.serverID, a.cred, a.attemptID, 2, a.operationID, true, a.routeDigest, "")
		if !errors.Is(err, ErrAgentLeaseStale) {
			t.Fatalf("err=%v, want ErrAgentLeaseStale", err)
		}
		a.assertStillAuthorized(t, 1, a.predecessorID)
	})
	t.Run("fence bumped elsewhere", func(t *testing.T) {
		a := newActivatedFixture(t, 185, true)
		if _, err := a.pool.Exec(ctx, `UPDATE environment_slots SET fence=2 WHERE environment_id=$1`, a.envID); err != nil {
			t.Fatal(err)
		}
		if err := a.report(ctx, true, ""); !errors.Is(err, ErrAgentLeaseStale) {
			t.Fatalf("err=%v, want ErrAgentLeaseStale", err)
		}
		// Nothing was written: the attempt is still the (now stale) leased
		// occupant, the operation is still open, nothing is flagged.
		if state, _, _ := a.operation(t); state != "switch_authorized" {
			t.Fatalf("operation state=%s, want switch_authorized", state)
		}
		if st, _, _, _, _ := a.attemptRow(t, a.attemptID); st != "leased" {
			t.Fatalf("attempt=%s, want leased", st)
		}
		if fence, current, _, reconciling := a.slotRow(t); fence != 2 || current == nil || *current != a.attemptID || reconciling {
			t.Fatalf("slot fence=%d current=%v reconciling=%v", fence, current, reconciling)
		}
		if got := a.deploymentStatus(t); got != deployment.StatusActivating {
			t.Fatalf("deployment=%s, want activating", got)
		}
		if _, _, routeReconciling := a.routeHead(t); routeReconciling {
			t.Fatal("route head must not be flagged by a stale report")
		}
	})
	t.Run("stale expected_route_revision is ambiguous and blocks durably", func(t *testing.T) {
		a := newActivatedFixture(t, 186, true)
		if _, err := a.pool.Exec(ctx, `UPDATE route_heads SET route_revision=route_revision+1, updated_at=clock_timestamp() WHERE environment_id=$1`, a.envID); err != nil {
			t.Fatal(err)
		}
		if err := a.report(ctx, true, ""); !errors.Is(err, ErrAgentLeaseActivationRouteMoved) {
			t.Fatalf("err=%v, want ErrAgentLeaseActivationRouteMoved", err)
		}
		a.assertBlocked(t, 2, a.predecessorID, "route head moved since authorization: expected revision 1")
	})
	t.Run("conflicting route digest is ambiguous and blocks durably", func(t *testing.T) {
		a := newActivatedFixture(t, 187, true)
		wrong := a.routeDigest
		wrong[0] ^= 0xff
		err := a.repo.ReportActivationResult(ctx, a.serverID, a.cred, a.attemptID, 1, a.operationID, true, wrong, "")
		if !errors.Is(err, ErrAgentLeaseIntegrityConflict) {
			t.Fatalf("err=%v, want ErrAgentLeaseIntegrityConflict", err)
		}
		a.assertBlocked(t, 1, a.predecessorID, "route digest mismatch: expected sha256:"+hex.EncodeToString(a.routeDigest[:]))
		if _, _, summary := a.operation(t); !strings.Contains(summary, "observed sha256:"+hex.EncodeToString(wrong[:])) {
			t.Fatalf("summary %q lacks the observed digest", summary)
		}
	})
	t.Run("wrong operation id", func(t *testing.T) {
		a := newActivatedFixture(t, 188, true)
		other, _ := strconv.ParseInt(a.operationID, 10, 64)
		err := a.repo.ReportActivationResult(ctx, a.serverID, a.cred, a.attemptID, 1, strconv.FormatInt(other+1000, 10), true, a.routeDigest, "")
		if !errors.Is(err, ErrAgentLeaseIntegrityConflict) {
			t.Fatalf("err=%v, want ErrAgentLeaseIntegrityConflict", err)
		}
		a.assertStillAuthorized(t, 1, a.predecessorID)
	})
	t.Run("non-numeric operation id", func(t *testing.T) {
		a := newActivatedFixture(t, 189, true)
		err := a.repo.ReportActivationResult(ctx, a.serverID, a.cred, a.attemptID, 1, "op-x", true, a.routeDigest, "")
		if !errors.Is(err, ErrAgentLeaseInvalidInput) {
			t.Fatalf("err=%v, want ErrAgentLeaseInvalidInput", err)
		}
	})
	t.Run("never authorized", func(t *testing.T) {
		f := newLeaseFixture(t, 190)
		cred := f.withCredential(t, 240)
		attemptID := f.leaseJob(t, 1, 90)
		repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
		if err := repo.ReportActivationResult(ctx, f.serverID, cred, attemptID, 1, "1", true, digest(1), ""); !errors.Is(err, ErrAgentLeaseStale) {
			t.Fatalf("err=%v, want ErrAgentLeaseStale", err)
		}
		var operations int
		if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM release_operations WHERE attempt_id=$1`, attemptID).Scan(&operations); err != nil {
			t.Fatal(err)
		}
		if operations != 0 {
			t.Fatalf("operations=%d, want none created", operations)
		}
		if st, _, _, _, _ := f.attemptRow(t, attemptID); st != "leased" {
			t.Fatalf("attempt=%s, want leased", st)
		}
		if _, current, _, reconciling := f.slotRow(t); current == nil || *current != attemptID || reconciling {
			t.Fatalf("slot current=%v reconciling=%v, want occupied and unblocked", current, reconciling)
		}
		if state, _, _, _ := f.jobRow(t); state != "leased" {
			t.Fatalf("job=%s, want leased", state)
		}
	})
	t.Run("wrong server", func(t *testing.T) {
		a := newActivatedFixture(t, 191, true)
		// A fully valid second server with its OWN active credential, so the
		// call passes lockActiveServerCredential and is denied by the
		// attempt-ownership check itself, not earlier at the credential.
		a.insertServer(t, "other-server")
		otherCred := digest(77)
		if _, err := a.pool.Exec(ctx, `INSERT INTO server_credentials(server_id,version,digest,status) VALUES ('other-server',1,$1,'active')`, otherCred[:]); err != nil {
			t.Fatal(err)
		}
		err := a.repo.ReportActivationResult(ctx, "other-server", otherCred, a.attemptID, 1, a.operationID, true, a.routeDigest, "")
		if !errors.Is(err, ErrServerAuthorizationDenied) {
			t.Fatalf("err=%v, want ErrServerAuthorizationDenied", err)
		}
		a.assertStillAuthorized(t, 1, a.predecessorID)
	})
}

func TestReportActivationResultReplayAndConflicts(t *testing.T) {
	ctx := context.Background()
	a := newActivatedFixture(t, 192, true)
	if err := a.report(ctx, true, ""); err != nil {
		t.Fatal(err)
	}
	if err := a.report(ctx, true, ""); err != nil {
		t.Fatalf("identical replay err=%v, want nil", err)
	}
	a.assertFinalized(t, 2) // exactly one revision bump, one activated event
	if n := strings.Count(strings.Join(a.jobEvents(t), ","), "activated"); n != 1 {
		t.Fatalf("activated job events=%d, want 1 (events=%v)", n, a.jobEvents(t))
	}

	wrong := a.routeDigest
	wrong[5] ^= 0xff
	if err := a.repo.ReportActivationResult(ctx, a.serverID, a.cred, a.attemptID, 1, a.operationID, true, wrong, ""); !errors.Is(err, ErrAgentLeaseIntegrityConflict) {
		t.Fatalf("conflicting digest err=%v, want ErrAgentLeaseIntegrityConflict", err)
	}
	if err := a.report(ctx, false, "late failure"); !errors.Is(err, ErrAgentLeaseIntegrityConflict) {
		t.Fatalf("failure after success err=%v, want ErrAgentLeaseIntegrityConflict", err)
	}
	a.assertFinalized(t, 2)
}

// TestCompleteRefusedWhileActivationOpen proves the Complete guard: the slot
// must NOT be freed and the job NOT closed.
func TestCompleteRefusedWhileActivationOpen(t *testing.T) {
	a := newActivatedFixture(t, 193, true)
	err := a.repo.Complete(context.Background(), a.serverID, a.cred, a.attemptID, 1, queue.CompletionSucceeded, "", "op-complete", digest(9))
	if !errors.Is(err, ErrAgentLeaseActivationOpen) {
		t.Fatalf("err=%v, want ErrAgentLeaseActivationOpen", err)
	}
	a.assertStillAuthorized(t, 1, a.predecessorID)
	err = a.repo.Complete(context.Background(), a.serverID, a.cred, a.attemptID, 1, queue.CompletionFailed, queue.FailureClassPermanentConfiguration, "op-complete", digest(9))
	if !errors.Is(err, ErrAgentLeaseActivationOpen) {
		t.Fatalf("failed-outcome err=%v, want ErrAgentLeaseActivationOpen", err)
	}
	a.assertStillAuthorized(t, 1, a.predecessorID)
}

// TestCompleteRefusedAfterAmbiguousReport: once a failure/ambiguous report
// has moved the operation to reconciliation_required the attempt is still
// leased, so Complete of either outcome must still be refused -- otherwise it
// would free the slot (leaving the deployment stuck at activating) or fail
// the deployment, both blind resolutions of an ambiguous switch (ADR 0005).
func TestCompleteRefusedAfterAmbiguousReport(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		outcome queue.CompletionOutcome
		class   queue.FailureClass
	}{
		{"succeeded", queue.CompletionSucceeded, ""},
		{"failed", queue.CompletionFailed, queue.FailureClassPermanentConfiguration},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newActivatedFixture(t, 210, true)
			if err := a.report(ctx, false, "reload timed out"); err != nil {
				t.Fatal(err)
			}
			err := a.repo.Complete(ctx, a.serverID, a.cred, a.attemptID, 1, tc.outcome, tc.class, "op-complete", digest(9))
			if !errors.Is(err, ErrAgentLeaseActivationOpen) {
				t.Fatalf("err=%v, want ErrAgentLeaseActivationOpen", err)
			}
			a.assertBlocked(t, 1, a.predecessorID, "reload timed out")
		})
	}
}

// TestCompleteAfterActivationResultUsesExistingReplayBranch proves a Complete
// arriving after a successful tx2 is not caught by the new guard: it reaches
// the unchanged attempt-already-closed replay-or-conflict branch.
func TestCompleteAfterActivationResultUsesExistingReplayBranch(t *testing.T) {
	ctx := context.Background()
	a := newActivatedFixture(t, 194, true)
	if err := a.report(ctx, true, ""); err != nil {
		t.Fatal(err)
	}
	// Exact replay of the stored result (operation id + route digest).
	if err := a.repo.Complete(ctx, a.serverID, a.cred, a.attemptID, 1, queue.CompletionSucceeded, "", a.operationID, a.routeDigest); err != nil {
		t.Fatalf("replay Complete err=%v, want nil", err)
	}
	if err := a.repo.Complete(ctx, a.serverID, a.cred, a.attemptID, 1, queue.CompletionSucceeded, "", a.operationID, digest(9)); !errors.Is(err, ErrAgentLeaseIntegrityConflict) {
		t.Fatalf("different digest err=%v, want ErrAgentLeaseIntegrityConflict", err)
	}
	if err := a.repo.Complete(ctx, a.serverID, a.cred, a.attemptID, 1, queue.CompletionFailed, queue.FailureClassPermanentConfiguration, a.operationID, a.routeDigest); !errors.Is(err, ErrAgentLeaseIntegrityConflict) {
		t.Fatalf("failed outcome err=%v, want ErrAgentLeaseIntegrityConflict", err)
	}
	a.assertFinalized(t, 2)
}

// TestReportActivationResultIsAtomic fails EVERY inject stage of both paths,
// one fresh fixture per stage, and proves nothing earlier in the transaction
// survived. The stage list is recorded from a real run rather than written by
// hand, so a stage added later is covered automatically; the expected core
// stages are asserted so the recording itself cannot silently be empty.
//
// What survives differs by error kind, because "deterministic" is a NEGATIVE
// definition (anything not known to be transient):
//   - success path, plain/unknown-shaped error: an error after the occupant
//     check must BLOCK (reconciliation recorded, every finalize write undone),
//     never leave the operation silently open;
//   - success path, KNOWN-TRANSIENT error (serialization failure): rolls back
//     whole with no block, so the retry can still succeed;
//   - failure path (the block write itself failing): rolls back whole; the
//     error is returned for retry.
//
// preOccupantStages are the read/lock statements before the occupant check:
// nothing durable is written there, so even an unrecognized error stays
// retryable instead of blocking.
var preOccupantStages = map[string]bool{"attempt_peek": true, "slot_lock": true, "job_lock": true, "attempt_lock": true, "operation_lock": true}

func TestReportActivationResultIsAtomic(t *testing.T) {
	ctx := context.Background()
	record := func(seed byte, succeeded bool) []string {
		a := newActivatedFixture(t, seed, true)
		var stages []string
		a.repo.failAfter = func(stage string) error {
			stages = append(stages, stage)
			return nil
		}
		if err := a.report(ctx, succeeded, "reload timed out"); err != nil {
			t.Fatal(err)
		}
		return stages
	}
	paths := []struct {
		name      string
		succeeded bool
		mustSee   []string
	}{
		{"success", true, []string{"deployment", "release_active", "route_head", "operation_close", "attempt_close", "slot_free", "job_close", "job_event"}},
		{"failure", false, []string{"operation_close", "slot_reconcile", "job_event"}},
	}
	for pi, path := range paths {
		stages := record(byte(215+pi), path.succeeded)
		seen := map[string]bool{}
		for _, stage := range stages {
			seen[stage] = true
		}
		for _, want := range path.mustSee {
			if !seen[want] {
				t.Fatalf("%s path never reached stage %q (recorded %v)", path.name, want, stages)
			}
		}
		for index := range stages {
			stage := stages[index]
			// The success path is run with both an unknown-shaped and a
			// known-transient injected error; the failure path only once.
			kinds := []struct {
				name     string
				injected error
				blocks   bool
			}{{"plain", errors.New("injected"), path.succeeded && !preOccupantStages[stage]}}
			if path.succeeded {
				kinds = append(kinds, struct {
					name     string
					injected error
					blocks   bool
				}{"transient", &pgconn.PgError{Code: "40001"}, false})
			}
			for _, kind := range kinds {
				t.Run(path.name+"/"+kind.name+"/"+stage+"/"+strconv.Itoa(index), func(t *testing.T) {
					a := newActivatedFixture(t, byte(220+pi), true)
					boom := kind.injected
					// Fail the nth occurrence of this stage name (a stage such as
					// "deployment" is reached from several call sites).
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
					if err := a.report(ctx, path.succeeded, "reload timed out"); !errors.Is(err, boom) {
						t.Fatalf("err=%v, want injected failure", err)
					}
					if kind.blocks {
						// Every finalize write is undone and the block survives.
						a.assertBlocked(t, 1, a.predecessorID, "injected")
						return
					}
					a.assertStillAuthorized(t, 1, a.predecessorID)
					if st, _ := a.releaseState(t, a.candidateID); st != "ready" {
						t.Fatalf("candidate=%s, want ready", st)
					}
					if st, _ := a.releaseState(t, a.predecessorID); st != "active" {
						t.Fatalf("predecessor=%s, want active", st)
					}
					if events := a.jobEvents(t); len(events) != 1 || events[0] != "activation_authorized" {
						t.Fatalf("job events=%v, want only activation_authorized", events)
					}
				})
			}
		}
	}
}

// holdSlotAndRace holds environment_slots' row in a third transaction,
// launches the calls one at a time -- confirming via pg_stat_activity that
// each is genuinely blocked on the slot row before the next starts, so
// Postgres's FIFO row-lock queue gives a deterministic order -- then
// releases the holder and returns when every call has finished. It never
// sleeps to "give goroutines a head start".
func holdSlotAndRace(t *testing.T, f leaseFixture, calls ...func(ctx context.Context)) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	holder, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Deferred so a failed waiter poll (e.g. a removed FOR UPDATE) cannot
	// leak the lock and hang the still-blocked goroutines.
	defer holder.Rollback(ctx) //nolint:errcheck
	if _, err := holder.Exec(ctx, `SELECT 1 FROM environment_slots WHERE environment_id=$1 FOR UPDATE`, f.envID); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i, call := range calls {
		wg.Add(1)
		go func(call func(ctx context.Context)) {
			defer wg.Done()
			call(ctx)
		}(call)
		waitForLockWaiters(t, ctx, f.pool, "environment_slots", i+1)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
}

// TestReportActivationResultGenuinelyRacesItself: two identical success
// reports both blocked on the slot row; exactly one finalizes, the other
// observes the committed result as an idempotent replay.
func TestReportActivationResultGenuinelyRacesItself(t *testing.T) {
	a := newActivatedFixture(t, 196, true)
	var errs [2]error
	call := func(i int) func(ctx context.Context) {
		return func(ctx context.Context) { errs[i] = a.report(ctx, true, "") }
	}
	holdSlotAndRace(t, a.leaseFixture, call(0), call(1))
	for i, err := range errs {
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	a.assertFinalized(t, 2)
	if n := strings.Count(strings.Join(a.jobEvents(t), ","), "activated"); n != 1 {
		t.Fatalf("activated job events=%d, want exactly one finalization", n)
	}
}

// TestReportActivationResultGenuinelyRacesConflictingReport: a success and a
// failure report race for the same operation; exactly one wins and the
// loser is refused, never both accepted.
func TestReportActivationResultGenuinelyRacesConflictingReport(t *testing.T) {
	a := newActivatedFixture(t, 197, true)
	var successErr, failureErr error
	holdSlotAndRace(t, a.leaseFixture,
		func(ctx context.Context) { successErr = a.report(ctx, true, "") },
		func(ctx context.Context) { failureErr = a.report(ctx, false, "reload failed") },
	)
	// FIFO lock queue: the success report was queued first and wins.
	if successErr != nil {
		t.Fatalf("first-queued success err=%v", successErr)
	}
	if !errors.Is(failureErr, ErrAgentLeaseIntegrityConflict) {
		t.Fatalf("losing failure report err=%v, want ErrAgentLeaseIntegrityConflict", failureErr)
	}
	a.assertFinalized(t, 2)
}

// TestCompleteGenuinelyRacesOpenActivation: Complete and tx2 both blocked on
// the slot row with Complete first in the queue. The guard must refuse
// Complete; tx2 then finalizes; the slot is freed and the job closed exactly
// once, by tx2.
func TestCompleteGenuinelyRacesOpenActivation(t *testing.T) {
	a := newActivatedFixture(t, 198, true)
	var completeErr, reportErr error
	holdSlotAndRace(t, a.leaseFixture,
		func(ctx context.Context) {
			completeErr = a.repo.Complete(ctx, a.serverID, a.cred, a.attemptID, 1, queue.CompletionSucceeded, "", "op-racing-complete", digest(9))
		},
		func(ctx context.Context) { reportErr = a.report(ctx, true, "") },
	)
	if !errors.Is(completeErr, ErrAgentLeaseActivationOpen) {
		t.Fatalf("racing Complete err=%v, want ErrAgentLeaseActivationOpen", completeErr)
	}
	if reportErr != nil {
		t.Fatalf("tx2 err=%v", reportErr)
	}
	a.assertFinalized(t, 2)
}

// TestCompleteGenuinelyRacesOpenActivationTx2First is the ordering that
// discriminates the guard's position: tx2 queued first on the slot row,
// Complete second. The operation is switch_authorized when Complete starts
// but already succeeded when Complete gets its locks, so Complete must take
// the existing closed-attempt replay branch (nil for the exact stored
// result), NOT ErrAgentLeaseActivationOpen. A guard hoisted above the locks
// would read the stale open state and wrongly refuse.
func TestCompleteGenuinelyRacesOpenActivationTx2First(t *testing.T) {
	a := newActivatedFixture(t, 199, true)
	var completeErr, reportErr error
	holdSlotAndRace(t, a.leaseFixture,
		func(ctx context.Context) { reportErr = a.report(ctx, true, "") },
		func(ctx context.Context) {
			completeErr = a.repo.Complete(ctx, a.serverID, a.cred, a.attemptID, 1, queue.CompletionSucceeded, "", a.operationID, a.routeDigest)
		},
	)
	if reportErr != nil {
		t.Fatalf("tx2 err=%v", reportErr)
	}
	if completeErr != nil {
		t.Fatalf("Complete queued after tx2 err=%v, want the replay branch (nil)", completeErr)
	}
	a.assertFinalized(t, 2)
}

// TestReportActivationResultToleratesRouteHeadUpdatedAtAhead: a concurrent
// writer may leave route_heads.updated_at later than this transaction's
// clock reading; validate_route_head_update's monotonicity rule must not
// trip on either path (updated_at is written as GREATEST(updated_at, now)).
func TestReportActivationResultToleratesRouteHeadUpdatedAtAhead(t *testing.T) {
	ctx := context.Background()
	for _, succeeded := range []bool{true, false} {
		name := map[bool]string{true: "success", false: "failure"}[succeeded]
		t.Run(name, func(t *testing.T) {
			a := newActivatedFixture(t, byte(230+map[bool]int{true: 1, false: 0}[succeeded]), true)
			// Flag-only updates (allowed at an unchanged revision) push
			// updated_at an hour ahead, then clear the flag again.
			if _, err := a.pool.Exec(ctx, `UPDATE route_heads SET reconciliation_required=true, updated_at=clock_timestamp() + interval '1 hour' WHERE environment_id=$1`, a.envID); err != nil {
				t.Fatal(err)
			}
			if _, err := a.pool.Exec(ctx, `UPDATE route_heads SET reconciliation_required=false WHERE environment_id=$1`, a.envID); err != nil {
				t.Fatal(err)
			}
			if err := a.report(ctx, succeeded, "reload timed out"); err != nil {
				t.Fatalf("report with updated_at ahead: %v", err)
			}
		})
	}
}
