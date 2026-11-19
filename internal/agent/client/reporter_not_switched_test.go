package agentclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"autodeploy/internal/agent/state"
)

// Slice 22-D reporter behaviour: the not_switched outcome, and the durable
// strip-and-retry-once fallback when an older control plane answers 400.

func readyNotSwitched(id int64) state.PendingActivation {
	p := pending(id)
	p.ReleaseID, p.BackendRuntimeInstanceID = "rel_new", "rt_new"
	p.PredecessorReleaseID, p.PredecessorRuntimeInstanceID, p.DrainSeconds = "rel_old", "rt_old", 30
	p.PredecessorStopSent = true
	p.PreStopRouteDigest = [32]byte{0x11}
	p.Phase, p.FailureSummary, p.NotSwitched = state.PhaseResultReady, "candidate failed health gate", true
	return p
}

// saveNotSwitched writes the record the way the executor does: authorized
// first (with the stop intent), then the result.
func saveNotSwitched(t *testing.T, a state.ActivationStore, id int64) {
	t.Helper()
	auth := readyNotSwitched(id)
	auth.Phase, auth.FailureSummary, auth.NotSwitched = state.PhaseAuthorized, "", false
	save(t, a, auth, readyNotSwitched(id))
}

// realBadRequest is the error the real HTTP client returns for the server's
// 400 invalid_request answer (mapStatus), not a hand-rolled stand-in.
func realBadRequest() error {
	c := &Client{logger: slog.New(slog.DiscardHandler)}
	return c.mapStatus(http.StatusBadRequest, []byte(`{"error":"invalid_request"}`))
}

func outcomes(h *harness, attempt int64) []bool {
	h.client.mu.Lock()
	defer h.client.mu.Unlock()
	var out []bool
	for _, c := range h.client.calls {
		if c.attempt == attempt {
			out = append(out, c.res.NotSwitched())
		}
	}
	return out
}

func TestReporterSendsNotSwitchedAndDeletesOn200(t *testing.T) {
	a := newActStore(t, t.TempDir())
	saveNotSwitched(t, a, 1)
	h := newHarness(t, a, nil)
	h.runUntil(t, func() bool { return len(remaining(t, a)) == 0 })
	if got := outcomes(h, 1); len(got) != 1 || !got[0] {
		t.Fatalf("outcomes = %v, want one call carrying not_switched", got)
	}
}

func TestReporterBadRequestStripsOutcomeDurablyAndRetriesOnce(t *testing.T) {
	a := newActStore(t, t.TempDir())
	saveNotSwitched(t, a, 1)
	h := newHarness(t, a, map[int64][]error{1: {realBadRequest(), nil}})
	// At the moment of the retry the record on disk must already be plain.
	var onDiskDuringRetry state.PendingActivation
	h.client.hook = func(int64) {
		if h.client.total() == 2 {
			onDiskDuringRetry, _, _ = a.LoadActivation(1)
		}
	}
	h.runUntil(t, func() bool { return len(remaining(t, a)) == 0 })
	got := outcomes(h, 1)
	if len(got) != 2 || !got[0] || got[1] {
		t.Fatalf("outcomes = %v, want [not_switched, plain]", got)
	}
	if onDiskDuringRetry.AttemptID != 1 || onDiskDuringRetry.NotSwitched {
		t.Fatalf("record at retry time = %+v, want the durable downgrade first", onDiskDuringRetry)
	}
	if strings.Contains(h.logText(), "candidate failed") {
		t.Fatal("failure summary leaked into logs")
	}
}

// A 400 on the stripped retry is handled as a 400 is today: keep the record,
// retry slowly, and never loop inside a pass or resend the outcome.
func TestReporterBadRequestOnStrippedRetryDoesNotLoop(t *testing.T) {
	a := newActStore(t, t.TempDir())
	saveNotSwitched(t, a, 1)
	h := newHarness(t, a, map[int64][]error{1: {realBadRequest()}}) // every answer is 400
	h.runUntil(t, func() bool { return len(h.waits) >= 4 })
	got := outcomes(h, 1)
	if len(got) < 5 || !got[0] {
		t.Fatalf("outcomes = %v", got)
	}
	for i, n := range got[1:] {
		if n {
			t.Fatalf("call %d resent the outcome after the strip: %v", i+1, got)
		}
	}
	recs := remaining(t, a)
	if len(recs) != 1 || recs[0].NotSwitched {
		t.Fatalf("record must stay, downgraded: %+v", recs)
	}
	if countLogs(h.logText(), "server rejected the report body") == 0 {
		t.Fatal("the second 400 must be handled as today (logged, kept)")
	}
}

// The agent stops between the 400 and the retry (the retry never lands): a new
// process finds the downgraded record and sends a plain failure.
func TestReporterRestartBetweenBadRequestAndRetry(t *testing.T) {
	root := t.TempDir()
	a := newActStore(t, root)
	saveNotSwitched(t, a, 1)
	h1 := newHarness(t, a, map[int64][]error{1: {realBadRequest()}})
	ctx, cancel := context.WithCancel(context.Background())
	h1.client.hook = func(int64) {
		if h1.client.total() == 2 {
			cancel() // shutdown during the stripped retry
		}
	}
	h1.rep.after = func(d time.Duration) <-chan time.Time { return make(chan time.Time) }
	done := make(chan error, 1)
	go func() { done <- h1.rep.Run(ctx) }()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v", err)
	}
	if got := outcomes(h1, 1); len(got) != 2 || !got[0] || got[1] {
		t.Fatalf("first process outcomes = %v", got)
	}

	// New process, same state dir.
	a2 := newActStore(t, root)
	rec, ok, err := a2.LoadActivation(1)
	if err != nil || !ok || rec.NotSwitched {
		t.Fatalf("record after restart = %+v ok=%v err=%v, want downgraded and present", rec, ok, err)
	}
	h2 := newHarness(t, a2, nil)
	h2.runUntil(t, func() bool { return len(remaining(t, a2)) == 0 })
	if got := outcomes(h2, 1); len(got) != 1 || got[0] {
		t.Fatalf("second process outcomes = %v, want one plain report", got)
	}
}

// failingDowngradeStore: the durable strip cannot be written.
type failingDowngradeStore struct {
	activationRecordStore
	mu    sync.Mutex
	tries int
}

func (s *failingDowngradeStore) DowngradeNotSwitched(int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tries++
	return errors.New("disk full")
}

// When the durable downgrade fails the report is not lost: this process still
// retries plain and never re-sends the outcome; a restarted process (record
// unchanged) simply repeats the strip, so there is no loop and no lost report.
func TestReporterDowngradePersistFailureStillStripsInMemory(t *testing.T) {
	root := t.TempDir()
	a := newActStore(t, root)
	saveNotSwitched(t, a, 1)
	st := &failingDowngradeStore{activationRecordStore: a}
	h := newHarness(t, st, map[int64][]error{1: {realBadRequest(), ErrServiceUnavailable, ErrServiceUnavailable}})
	h.runUntil(t, func() bool { return h.client.count(1) >= 3 })
	got := outcomes(h, 1)
	if len(got) < 3 || !got[0] {
		t.Fatalf("outcomes = %v", got)
	}
	for _, n := range got[1:] {
		if n {
			t.Fatalf("outcome resent after the strip: %v", got)
		}
	}
	if st.tries != 1 {
		t.Fatalf("downgrade attempts = %d, want 1", st.tries)
	}
	if !strings.Contains(h.logText(), "cannot persist the outcome downgrade") {
		t.Fatal("persist failure not logged")
	}
	// The record is untouched on disk and a new process strips again, then delivers.
	h2 := newHarness(t, newActStore(t, root), map[int64][]error{1: {realBadRequest(), nil}})
	h2.runUntil(t, func() bool { return len(remaining(t, newActStore(t, root))) == 0 })
	if got := outcomes(h2, 1); len(got) != 2 || !got[0] || got[1] {
		t.Fatalf("restarted outcomes = %v", got)
	}
}

// A local pre-send validation failure is ErrInvalidRequest too but says
// nothing about the server's version: no strip, no downgrade.
func TestReporterLocalInvalidRequestDoesNotStrip(t *testing.T) {
	a := newActStore(t, t.TempDir())
	saveNotSwitched(t, a, 1)
	h := newHarness(t, a, map[int64][]error{1: {fmt.Errorf("%w: activation result", ErrInvalidRequest)}})
	h.runUntil(t, func() bool { return h.client.count(1) >= 2 })
	for _, n := range outcomes(h, 1) {
		if !n {
			t.Fatal("a non-server 400 must not strip the outcome")
		}
	}
	if recs := remaining(t, a); len(recs) != 1 || !recs[0].NotSwitched {
		t.Fatalf("record = %+v", recs)
	}
}

// The delete set is unchanged for a not_switched report: terminal answers
// delete after ONE call with the outcome (no strip), everything else keeps.
func TestReporterNotSwitchedDeleteSetUnchanged(t *testing.T) {
	for _, c := range []string{ConflictLateReportRecorded, ConflictRouteMoved, ConflictIntegrity} {
		t.Run("deletes on "+c, func(t *testing.T) {
			a := newActStore(t, t.TempDir())
			saveNotSwitched(t, a, 1)
			h := newHarness(t, a, map[int64][]error{1: {&ConflictError{Code: c}}})
			h.runUntil(t, func() bool { return len(remaining(t, a)) == 0 })
			if got := outcomes(h, 1); len(got) != 1 || !got[0] {
				t.Fatalf("outcomes = %v, want exactly one outcome-carrying call", got)
			}
		})
	}
	keep := map[string]error{
		"stale_lease":        ErrStaleLease,
		"operation_mismatch": &ConflictError{Code: ConflictOperationMismatch},
		"unauthorized":       ErrUnauthorized,
		"unavailable":        ErrServiceUnavailable,
		"transport":          ErrTransportFailure,
	}
	for name, ans := range keep {
		t.Run("keeps on "+name, func(t *testing.T) {
			a := newActStore(t, t.TempDir())
			saveNotSwitched(t, a, 1)
			h := newHarness(t, a, map[int64][]error{1: {ans}})
			h.runUntil(t, func() bool { return h.client.count(1) >= 3 })
			for _, n := range outcomes(h, 1) {
				if !n {
					t.Fatalf("%s must keep the outcome (no strip) and retry", name)
				}
			}
			if recs := remaining(t, a); len(recs) != 1 || !recs[0].NotSwitched {
				t.Fatalf("record = %+v, want kept untouched", recs)
			}
		})
	}
}

// End to end through the real HTTP client and the real record store: an older
// control plane strict-decodes and rejects the unknown outcome field.
func TestReporterAgainstOlderControlPlane(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(b), `"outcome"`) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_request"}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	a := newActStore(t, t.TempDir())
	saveNotSwitched(t, a, 1)
	h := newHarness(t, a, nil)
	h.rep.Client = newTestClient(t, srv, nil)
	h.runUntil(t, func() bool { return len(remaining(t, a)) == 0 })
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 || !strings.Contains(bodies[0], `"outcome":"not_switched"`) || strings.Contains(bodies[1], "outcome") {
		t.Fatalf("bodies = %v", bodies)
	}
	if !strings.Contains(bodies[1], `"succeeded":false`) || !strings.Contains(bodies[1], `"failure_summary":"candidate failed health gate"`) {
		t.Fatalf("stripped retry is not the same plain failure: %s", bodies[1])
	}
}

func TestActivationResultWireOutcome(t *testing.T) {
	var got []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, string(b))
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := newTestClient(t, srv, nil)
	plain, _ := ActivationFailed("boom")
	ns, _ := ActivationNotSwitched("boom")
	for _, res := range []ActivationResult{plain, ns, ActivationSucceeded([32]byte{1})} {
		if err := c.ReportActivationResult(context.Background(), 1, 2, "9", res); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Contains(got[0], "outcome") || strings.Contains(got[2], "outcome") {
		t.Fatalf("plain and success must omit outcome: %v", got)
	}
	if want := `{"attempt_id":1,"fence":2,"operation_id":"9","succeeded":false,"failure_summary":"boom","outcome":"not_switched"}`; got[1] != want {
		t.Fatalf("not_switched body = %s, want %s", got[1], want)
	}
	if _, err := ActivationNotSwitched(""); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("empty summary: %v", err)
	}
}

// countingDowngradeStore counts DowngradeNotSwitched calls and delegates.
type countingDowngradeStore struct {
	activationRecordStore
	downgrades int
}

func (s *countingDowngradeStore) DowngradeNotSwitched(id int64) error {
	s.downgrades++
	return s.activationRecordStore.DowngradeNotSwitched(id)
}

// A PLAIN failure (no outcome) that gets a server 400 invalid_request is not
// an older-control-plane signal: it is sent exactly once in the pass, never
// downgraded or retried, and the record is kept per the ordinary 400 handling.
// Guards the res.NotSwitched() condition on the strip branch.
func TestReporterPlainFailureBadRequestIsSentOnceNoStrip(t *testing.T) {
	a := newActStore(t, t.TempDir())
	save(t, a, readyFailed(1))
	st := &countingDowngradeStore{activationRecordStore: a}
	h := newHarness(t, st, map[int64][]error{1: {realBadRequest()}})
	tr := &reportTrack{}
	if outage := h.rep.report(context.Background(), readyFailed(1), tr); outage {
		t.Fatal("a 400 is not an outage")
	}
	if n := h.client.count(1); n != 1 {
		t.Fatalf("calls = %d, want exactly one in this pass", n)
	}
	if got := outcomes(h, 1); len(got) != 1 || got[0] {
		t.Fatalf("outcomes = %v, want one plain report", got)
	}
	if st.downgrades != 0 || tr.stripped {
		t.Fatalf("downgrades = %d stripped = %v, want none", st.downgrades, tr.stripped)
	}
	if recs := remaining(t, a); len(recs) != 1 || recs[0].AttemptID != 1 {
		t.Fatalf("record must be kept: %+v", recs)
	}
	if countLogs(h.logText(), "server rejected the report body") != 1 || strings.Contains(h.logText(), "not_switched") {
		t.Fatalf("expected today's 400 handling only; logs: %s", h.logText())
	}
}
