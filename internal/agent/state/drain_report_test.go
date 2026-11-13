package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func reportPending(id int64, outcome string) PendingDrain {
	p := drainRec(id)
	p.Phase, p.Outcome, p.ResultSummary = DrainReportPending, outcome, "summary"
	return p
}

// An old-shape file (written before step 20B: no outcome, result_summary or
// reported keys) still loads, and a new pending record writes none of them.
func TestDrainOldShapeLoadsAndPendingWritesNoNewKeys(t *testing.T) {
	root, d := newDrains(t)
	if err := d.SaveDrain(drainRec(1)); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "drains", "attempt-1.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{`"outcome"`, `"result_summary"`, `"reported"`} {
		if strings.Contains(string(raw), k) {
			t.Fatalf("pending record carries %s: %s", k, raw)
		}
	}
	got, ok, err := d.LoadDrain(1)
	if err != nil || !ok || got.Outcome != "" || got.Reported {
		t.Fatalf("got %+v ok=%v err=%v", got, ok, err)
	}
}

func TestDrainReportLifecycleRoundTripAndMonotone(t *testing.T) {
	_, d := newDrains(t)
	if err := d.SaveDrain(drainRec(1)); err != nil {
		t.Fatal(err)
	}
	rp := reportPending(1, DrainOutcomeRouteMoved)
	if err := d.SaveDrain(rp); err != nil {
		t.Fatal(err)
	}
	got, _, err := d.LoadDrain(1)
	if err != nil || got.Phase != DrainReportPending || got.Outcome != DrainOutcomeRouteMoved || got.ResultSummary != "summary" {
		t.Fatalf("got %+v err=%v", got, err)
	}
	// Cannot fall back to pending/retryable, nor change the outcome or text.
	if err := d.SaveDrain(drainRec(1)); !errors.Is(err, ErrDrainRegressed) {
		t.Fatalf("back to pending: %v", err)
	}
	other := reportPending(1, DrainOutcomeStopFailed)
	if err := d.SaveDrain(other); !errors.Is(err, ErrDrainRegressed) {
		t.Fatalf("outcome change: %v", err)
	}
	text := reportPending(1, DrainOutcomeRouteMoved)
	text.ResultSummary = "changed"
	if err := d.SaveDrain(text); !errors.Is(err, ErrDrainRegressed) {
		t.Fatalf("text change: %v", err)
	}
	// Acknowledge: failure evidence, Reported.
	ack := drainRec(1)
	ack.Phase, ack.Outcome, ack.FailureSummary, ack.Reported = DrainStopFailed, DrainOutcomeRouteMoved, "summary", true
	if err := d.SaveDrain(ack); err != nil {
		t.Fatal(err)
	}
	if err := d.SaveDrain(rp); !errors.Is(err, ErrDrainRegressed) {
		t.Fatalf("report_pending after ack: %v", err)
	}
	// Re-arming never rewinds.
	if err := d.ArmDrain(drainRec(1)); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := d.LoadDrain(1); !got.Reported || got.Phase != DrainStopFailed {
		t.Fatalf("re-arm rewound: %+v", got)
	}
}

func TestDrainReportValidation(t *testing.T) {
	_, d := newDrains(t)
	mut := func(f func(*PendingDrain)) PendingDrain { p := reportPending(1, DrainOutcomeStopped); f(&p); return p }
	for name, p := range map[string]PendingDrain{
		"no outcome":         mut(func(p *PendingDrain) { p.Outcome = "" }),
		"unknown outcome":    mut(func(p *PendingDrain) { p.Outcome = "restarted" }),
		"empty summary":      mut(func(p *PendingDrain) { p.ResultSummary = "" }),
		"oversize summary":   mut(func(p *PendingDrain) { p.ResultSummary = strings.Repeat("a", 2049) }),
		"reported pending":   mut(func(p *PendingDrain) { p.Reported = true }),
		"retryable pending":  mut(func(p *PendingDrain) { p.Retryable = true }),
		"failure summary":    mut(func(p *PendingDrain) { p.FailureSummary = "x" }),
		"outcome on pending": func() PendingDrain { p := drainRec(1); p.Outcome = DrainOutcomeStopped; return p }(),
		"summary on pending": func() PendingDrain { p := drainRec(1); p.ResultSummary = "x"; return p }(),
		"reported no outcome": func() PendingDrain {
			p := drainRec(1)
			p.Phase, p.FailureSummary, p.Reported = DrainStopFailed, "x", true
			return p
		}(),
		"success outcome acked": func() PendingDrain {
			p := drainRec(1)
			p.Phase, p.FailureSummary, p.Outcome, p.Reported = DrainStopFailed, "x", DrainOutcomeStopped, true
			return p
		}(),
		"unreported outcome": func() PendingDrain {
			p := drainRec(1)
			p.Phase, p.FailureSummary, p.Outcome = DrainStopFailed, "x", DrainOutcomeStopFailed
			return p
		}(),
	} {
		if err := d.SaveDrain(p); !errors.Is(err, ErrInvalidDrain) {
			t.Errorf("%s: err=%v, want ErrInvalidDrain", name, err)
		}
	}
}
