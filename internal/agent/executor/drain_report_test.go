package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	agentclient "autodeploy/internal/agent/client"
	"autodeploy/internal/agent/state"
)

func TestDrainSuccessReportsStoppedThenDeletes(t *testing.T) {
	h := newDrainHarness(t)
	h.arm(5, 30)
	h.advance(30 * time.Second)
	h.d.pass(context.Background())
	if len(h.reporter.calls) != 1 {
		t.Fatalf("reports = %v", h.reporter.calls)
	}
	got := h.reporter.calls[0]
	if got.attempt != 5 || got.predRelease != drainTestOldRel || got.predRuntime != drainTestOldRT || got.outcome != "stopped" || got.summary != "predecessor stopped" {
		t.Fatalf("report = %+v", got)
	}
	if _, ok := h.get(5); ok {
		t.Fatal("acknowledged success record not deleted")
	}
}

func TestDrainFailureOutcomeIsReportedAndKeptAsEvidence(t *testing.T) {
	h := newDrainHarness(t)
	h.arm(5, 30)
	h.advance(30 * time.Second)
	h.digest = routeDigestString([32]byte{9}) // route moved
	h.d.pass(context.Background())
	if len(h.reporter.calls) != 1 || h.reporter.calls[0].outcome != "route_moved" {
		t.Fatalf("reports = %v", h.reporter.calls)
	}
	rec, ok := h.get(5)
	if !ok || rec.Phase != state.DrainStopFailed || rec.Outcome != "route_moved" || !rec.Reported || rec.Retryable || rec.FailureSummary != drainSummaryRouteChanged || rec.ResultSummary != "" {
		t.Fatalf("record = %+v ok=%v", rec, ok)
	}
	// Acknowledged evidence is never re-reported.
	h.advance(time.Hour)
	h.mono = h.mono.Add(time.Hour)
	h.d.pass(context.Background())
	if h.reporter.count() != 1 {
		t.Fatalf("re-reported an acknowledged record: %d", h.reporter.count())
	}
}

func TestDrainTransientReportErrorsKeepRecordThenSucceed(t *testing.T) {
	h := newDrainHarness(t)
	h.reporter.errs = []error{agentclient.ErrServiceUnavailable, agentclient.ErrTransportFailure}
	h.arm(5, 30)
	h.advance(30 * time.Second)
	h.d.pass(context.Background())
	rec, ok := h.get(5)
	if !ok || rec.Phase != state.DrainReportPending || rec.Outcome != "stopped" || rec.Reported {
		t.Fatalf("record after transient error = %+v ok=%v", rec, ok)
	}
	// Not due again before the backoff.
	h.d.pass(context.Background())
	if h.reporter.count() != 1 {
		t.Fatalf("retried before the backoff: %d", h.reporter.count())
	}
	h.mono = h.mono.Add(31 * time.Second)
	h.d.pass(context.Background())
	if h.reporter.count() != 2 {
		t.Fatalf("calls=%d, want 2", h.reporter.count())
	}
	h.mono = h.mono.Add(61 * time.Second)
	h.d.pass(context.Background())
	if _, ok := h.get(5); ok || h.reporter.count() != 3 {
		t.Fatalf("not delivered/deleted: calls=%d", h.reporter.count())
	}
	if h.docker.count("stop") != 1 {
		t.Fatalf("stop called %d times; reporting must never re-stop", h.docker.count("stop"))
	}
	if !strings.Contains(h.logs.String(), "not delivered yet") {
		t.Fatalf("no loud log: %s", h.logs)
	}
}

// Crash between making the outcome durable and sending it: a fresh drainer
// re-reports from the record alone, with no docker work.
func TestDrainCrashBeforeReportReReports(t *testing.T) {
	h := newDrainHarness(t)
	rec := h.arm(5, 30)
	rec.Phase = state.DrainReportPending
	rec.Outcome = "gone"
	rec.ResultSummary = "predecessor container vanished before the stop"
	if err := h.store.SaveDrain(rec); err != nil {
		t.Fatal(err)
	}
	d2 := h.newDrainer()
	d2.pass(context.Background())
	if len(h.reporter.calls) != 1 || h.reporter.calls[0].outcome != "gone" {
		t.Fatalf("reports = %v", h.reporter.calls)
	}
	h.assertNoDockerCalls()
	if _, ok := h.get(5); ok {
		t.Fatal("record not deleted after the re-report")
	}
}

func TestDrainTerminalConflictAcknowledges(t *testing.T) {
	for _, tc := range []struct {
		name     string
		digest   bool // route moved -> failure outcome
		wantKept bool
	}{{"success outcome deleted", false, false}, {"failure outcome kept reported", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			h := newDrainHarness(t)
			h.reporter.errs = []error{&agentclient.ConflictError{Code: agentclient.ConflictIntegrity}}
			h.arm(5, 30)
			h.advance(30 * time.Second)
			if tc.digest {
				h.digest = routeDigestString([32]byte{9})
			}
			h.d.pass(context.Background())
			rec, ok := h.get(5)
			if ok != tc.wantKept || (ok && !rec.Reported) {
				t.Fatalf("record = %+v ok=%v", rec, ok)
			}
			if h.reporter.count() != 1 {
				t.Fatalf("calls=%d", h.reporter.count())
			}
		})
	}
}

func TestDrainReportSummaryIsSanitized(t *testing.T) {
	if got := resultSummary("a\nb\x7fc\x00d"); got != "a b c d" {
		t.Fatalf("got %q", got)
	}
	if got := resultSummary("  \n"); got != "no detail" {
		t.Fatalf("got %q", got)
	}
	long := resultSummary(strings.Repeat("é", 2000))
	if len(long) > 2048 {
		t.Fatalf("len=%d", len(long))
	}
}

func TestDrainerRequiresReporter(t *testing.T) {
	h := newDrainHarness(t)
	d := h.newDrainer()
	d.Reporter = nil
	if err := d.Validate(); err == nil {
		t.Fatal("nil reporter accepted")
	}
}

func TestDrainReporterErrorWithCancelledContextKeepsRecord(t *testing.T) {
	h := newDrainHarness(t)
	h.arm(5, 30)
	h.advance(30 * time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	h.d.Reporter = cancelingReporter{cancel}
	h.d.pass(ctx)
	if rec, ok := h.get(5); !ok || rec.Phase != state.DrainReportPending {
		t.Fatalf("record = %+v ok=%v", rec, ok)
	}
}

type cancelingReporter struct{ cancel func() }

func (c cancelingReporter) ReportDrainResult(context.Context, int64, string, string, string, string) error {
	c.cancel()
	return errors.New("canceled")
}
