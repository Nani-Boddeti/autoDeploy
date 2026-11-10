package agentclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"autodeploy/internal/agent/state"
)

type reportCall struct {
	attempt, fence int64
	op             string
	res            ActivationResult
}

// fakeReportClient answers per attempt from a script; the last entry repeats.
type fakeReportClient struct {
	mu     sync.Mutex
	script map[int64][]error
	calls  []reportCall
	hook   func(attempt int64) // runs inside the call
}

func (f *fakeReportClient) ReportActivationResult(ctx context.Context, attempt, fence int64, op string, r ActivationResult) error {
	f.mu.Lock()
	f.calls = append(f.calls, reportCall{attempt, fence, op, r})
	n := 0
	for _, c := range f.calls {
		if c.attempt == attempt {
			n++
		}
	}
	s := f.script[attempt]
	var err error
	if len(s) > 0 {
		err = s[min(n-1, len(s)-1)]
	}
	h := f.hook
	f.mu.Unlock()
	if h != nil {
		h(attempt)
	}
	return err
}

func (f *fakeReportClient) count(attempt int64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.attempt == attempt {
			n++
		}
	}
	return n
}

func (f *fakeReportClient) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func newActStore(t *testing.T, root string) state.ActivationStore {
	t.Helper()
	s, err := state.New(root)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Activations()
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func pending(id int64) state.PendingActivation {
	return state.PendingActivation{
		AttemptID: id, Fence: 3, EnvironmentID: "env_1", OperationID: "42",
		RouteDigest: [32]byte{1}, ImageDigest: [32]byte{2}, RouteRevision: 5,
		Phase: state.PhaseAuthorized,
	}
}

func readySucceeded(id int64) state.PendingActivation {
	p := pending(id)
	p.Phase, p.Succeeded, p.ObservedRouteDigest = state.PhaseResultReady, true, [32]byte{9, 8, 7}
	return p
}

func readyFailed(id int64) state.PendingActivation {
	p := pending(id)
	p.Phase, p.FailureSummary = state.PhaseResultReady, "probe failed: é tls: bad certificate"
	return p
}

func save(t *testing.T, a state.ActivationStore, ps ...state.PendingActivation) {
	t.Helper()
	for _, p := range ps {
		if err := a.SaveActivation(p); err != nil {
			t.Fatal(err)
		}
	}
}

type harness struct {
	rep    *ActivationReporter
	client *fakeReportClient
	logs   *bytes.Buffer
	mu     sync.Mutex
	clock  time.Time
	waits  []time.Duration
}

// newHarness builds a reporter with a fake clock whose waits elapse instantly.
func newHarness(t *testing.T, store activationRecordStore, script map[int64][]error) *harness {
	t.Helper()
	h := &harness{client: &fakeReportClient{script: script}, logs: &bytes.Buffer{}, clock: time.Now()}
	h.rep = &ActivationReporter{
		Client: h.client, Store: store, Logger: log.New(&lockedWriter{w: h.logs, mu: &h.mu}, "", 0),
		MinBackoff: 5 * time.Second, MaxBackoff: 60 * time.Second,
		now:    func() time.Time { h.mu.Lock(); defer h.mu.Unlock(); return h.clock },
		jitter: func(m time.Duration) time.Duration { return m }, // always the ceiling
	}
	return h
}

type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func (h *harness) logText() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.logs.String()
}

// runUntil runs the reporter, advancing the fake clock on every wait, until
// done() reports true (checked after each wait) or a wait cap is hit.
func (h *harness) runUntil(t *testing.T, done func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.rep.after = func(d time.Duration) <-chan time.Time {
		h.mu.Lock()
		h.waits = append(h.waits, d)
		h.clock = h.clock.Add(d)
		n := len(h.waits)
		h.mu.Unlock()
		if n > 200 {
			t.Error("wait cap exceeded")
			cancel()
		}
		if done() {
			cancel()
		}
		ch := make(chan time.Time, 1)
		ch <- time.Time{}
		return ch
	}
	finished := make(chan error, 1)
	go func() { finished <- h.rep.Run(ctx) }()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not finish")
	}
}

func remaining(t *testing.T, a state.ActivationStore) []state.PendingActivation {
	t.Helper()
	l, err := a.ListActivations()
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestReporterSuccessDeletes(t *testing.T) {
	a := newActStore(t, t.TempDir())
	save(t, a, readySucceeded(1))
	h := newHarness(t, a, nil)
	h.runUntil(t, func() bool { return len(remaining(t, a)) == 0 })
	if h.client.count(1) != 1 || len(remaining(t, a)) != 0 {
		t.Fatalf("calls=%d remaining=%d", h.client.count(1), len(remaining(t, a)))
	}
}

func TestReporterTerminalAnswersDelete(t *testing.T) {
	for _, c := range []string{ConflictLateReportRecorded, ConflictRouteMoved, ConflictIntegrity} {
		t.Run(c, func(t *testing.T) {
			a := newActStore(t, t.TempDir())
			save(t, a, readyFailed(1))
			h := newHarness(t, a, map[int64][]error{1: {fmt.Errorf("wrapped: %w", &ConflictError{Code: c})}})
			h.runUntil(t, func() bool { return len(remaining(t, a)) == 0 })
			if h.client.count(1) != 1 || len(remaining(t, a)) != 0 {
				t.Fatalf("calls=%d remaining=%d", h.client.count(1), len(remaining(t, a)))
			}
			if strings.Contains(h.logText(), "probe failed") {
				t.Fatal("failure summary leaked into logs")
			}
		})
	}
}

func TestReporterNonDecisiveConflictsKeepRecord(t *testing.T) {
	answers := map[string]error{"stale_lease": ErrStaleLease, "empty": &ConflictError{}, "novel": &ConflictError{Code: "brand_new"}}
	for _, c := range serverConflictCodes {
		if c != ConflictLateReportRecorded && c != ConflictRouteMoved && c != ConflictIntegrity {
			answers[c] = &ConflictError{Code: c}
		}
	}
	for name, ans := range answers {
		t.Run(name, func(t *testing.T) {
			a := newActStore(t, t.TempDir())
			save(t, a, readyFailed(1))
			h := newHarness(t, a, map[int64][]error{1: {fmt.Errorf("wrapped: %w", ans)}})
			h.runUntil(t, func() bool { return h.client.count(1) >= 4 })
			if len(remaining(t, a)) != 1 {
				t.Fatal("record deleted on a non-decisive answer")
			}
			for _, w := range h.waits {
				if w != 60*time.Second {
					t.Fatalf("waited %v, want MaxBackoff", w)
				}
			}
			if strings.Contains(h.logText(), "probe failed") {
				t.Fatal("failure summary leaked into logs")
			}
		})
	}
}

func TestReporterRetryableAnswersKeepRecord(t *testing.T) {
	cases := map[string]struct {
		err   error
		fixed bool
	}{
		"invalid_request": {ErrInvalidRequest, true},
		"unauthorized":    {ErrUnauthorized, true},
		"unavailable":     {ErrServiceUnavailable, false},
		"transport":       {ErrTransportFailure, false},
		"malformed":       {ErrMalformedResponse, false},
		"unknown":         {errors.New("boom"), false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			a := newActStore(t, t.TempDir())
			save(t, a, readyFailed(1))
			h := newHarness(t, a, map[int64][]error{1: {tc.err}})
			h.runUntil(t, func() bool { return h.client.count(1) >= 4 })
			if len(remaining(t, a)) != 1 {
				t.Fatal("record was deleted without a terminal answer")
			}
			if h.client.count(1) < 4 {
				t.Fatalf("not retried: %d", h.client.count(1))
			}
			if tc.fixed {
				for _, w := range h.waits {
					if w != 60*time.Second {
						t.Fatalf("fixed retry waited %v", w)
					}
				}
			}
		})
	}
}

func TestReporterOutageThenRecovery(t *testing.T) {
	a := newActStore(t, t.TempDir())
	save(t, a, readySucceeded(1))
	script := []error{}
	for i := 0; i < 8; i++ {
		script = append(script, ErrTransportFailure)
	}
	script = append(script, nil)
	h := newHarness(t, a, map[int64][]error{1: script})
	h.runUntil(t, func() bool { return len(remaining(t, a)) == 0 })
	if h.client.count(1) != 9 || len(remaining(t, a)) != 0 {
		t.Fatalf("calls=%d", h.client.count(1))
	}
	// Jitter pinned to the ceiling: 10s, 20s, 40s, then capped at 60s.
	want := []time.Duration{10, 20, 40, 60, 60, 60, 60, 60}
	for i, w := range want {
		if h.waits[i] != w*time.Second {
			t.Fatalf("wait %d = %v want %v (all %v)", i, h.waits[i], w*time.Second, h.waits)
		}
	}
}

func TestReporterJitterBounds(t *testing.T) {
	a := newActStore(t, t.TempDir())
	save(t, a, readyFailed(1))
	h := newHarness(t, a, map[int64][]error{1: {ErrTransportFailure}})
	h.rep.jitter = func(time.Duration) time.Duration { return 0 }
	h.runUntil(t, func() bool { return h.client.count(1) >= 5 })
	for _, w := range h.waits[:4] {
		if w != 5*time.Second {
			t.Fatalf("zero jitter must wait MinBackoff, got %v", w)
		}
	}
	// Default jitter stays within [Min, Max].
	h2 := newHarness(t, a, map[int64][]error{1: {ErrTransportFailure}})
	h2.rep.jitter = nil
	h2.runUntil(t, func() bool { return h2.client.count(1) >= 30 })
	for _, w := range h2.waits {
		if w < 5*time.Second || w > 60*time.Second {
			t.Fatalf("wait %v out of bounds", w)
		}
	}
}

func TestReporterRestartDeliversFromDisk(t *testing.T) {
	root := t.TempDir()
	first := newActStore(t, root)
	save(t, first, readySucceeded(7))
	h1 := newHarness(t, first, map[int64][]error{7: {ErrServiceUnavailable}})
	h1.runUntil(t, func() bool { return h1.client.count(7) >= 2 })
	if len(remaining(t, first)) != 1 {
		t.Fatal("record lost during outage")
	}
	// "Restart": new store instance over the same dir, new reporter.
	second := newActStore(t, root)
	h2 := newHarness(t, second, nil)
	h2.runUntil(t, func() bool { return len(remaining(t, second)) == 0 })
	if h2.client.count(7) != 1 || len(remaining(t, newActStore(t, root))) != 0 {
		t.Fatal("restarted reporter did not deliver and delete")
	}
}

func TestReporterNeverSendsAuthorized(t *testing.T) {
	a := newActStore(t, t.TempDir())
	save(t, a, pending(1), readySucceeded(2))
	h := newHarness(t, a, nil)
	h.runUntil(t, func() bool { return h.client.count(2) >= 1 && len(h.waits) >= 3 })
	if h.client.count(1) != 0 {
		t.Fatal("authorized record was reported")
	}
	l := remaining(t, a)
	if len(l) != 1 || l[0].AttemptID != 1 || l[0].Phase != state.PhaseAuthorized {
		t.Fatalf("authorized record disturbed: %+v", l)
	}
}

func TestReporterCorruptFileDoesNotBlockGoodRecords(t *testing.T) {
	root := t.TempDir()
	a := newActStore(t, root)
	save(t, a, readySucceeded(1))
	bad := filepath.Join(root, "activations", "attempt-2.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, a, nil)
	h.rep.corruptLogEvery = time.Hour
	count := func() int { return strings.Count(h.logText(), `corrupt entry "attempt-2.json"`) }
	h.runUntil(t, func() bool { return len(h.waits) >= 3 }) // 3 passes, 5s apart
	if h.client.count(1) != 1 {
		t.Fatalf("good record calls=%d", h.client.count(1))
	}
	if b, err := os.ReadFile(bad); err != nil || string(b) != "{not json" {
		t.Fatal("corrupt file touched")
	}
	if count() != 1 {
		t.Fatalf("logged %d times within the hour, want 1", count())
	}
	h.mu.Lock()
	h.clock = h.clock.Add(time.Hour)
	h.mu.Unlock()
	h.waits = nil
	h.runUntil(t, func() bool { return len(h.waits) >= 1 })
	if count() != 2 {
		t.Fatalf("after an hour logged %d times, want 2", count())
	}
	// A vanished entry is forgotten, so it logs again if it reappears.
	if err := os.Remove(bad); err != nil {
		t.Fatal(err)
	}
	h.waits = nil
	h.runUntil(t, func() bool { return len(h.waits) >= 1 })
	if err := os.WriteFile(bad, []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	h.waits = nil
	h.runUntil(t, func() bool { return len(h.waits) >= 1 })
	if count() != 3 {
		t.Fatalf("reappeared entry logged %d times total, want 3", count())
	}
}

type scanFailStore struct {
	activationRecordStore
	deleted bool
}

func (s *scanFailStore) ScanActivations() ([]state.PendingActivation, []string, error) {
	return nil, nil, fmt.Errorf("%w: readdir", state.ErrCorruptState)
}

func (s *scanFailStore) DeleteActivation(int64) error { s.deleted = true; return nil }

func TestReporterDirectoryFailureFailsClosed(t *testing.T) {
	st := &scanFailStore{}
	h := newHarness(t, st, nil)
	h.runUntil(t, func() bool { return len(h.waits) >= 3 })
	if h.client.total() != 0 || st.deleted {
		t.Fatal("acted despite directory failure")
	}
	for _, w := range h.waits {
		if w != 60*time.Second {
			t.Fatalf("waited %v, want MaxBackoff", w)
		}
	}
	if !strings.Contains(h.logText(), "cannot scan") {
		t.Fatal("no loud log")
	}
}

func TestReporterOutageStopsPass(t *testing.T) {
	a := newActStore(t, t.TempDir())
	save(t, a, readyFailed(1), readyFailed(2), readyFailed(3))
	h := newHarness(t, a, nil)
	h.rep.CallTimeout = 20 * time.Millisecond
	calls := 0
	h.rep.Client = clientFunc(func(ctx context.Context) error {
		calls++
		<-ctx.Done()
		return ctx.Err()
	})
	h.runUntil(t, func() bool { return len(h.waits) >= 1 })
	if calls != 1 {
		t.Fatalf("calls in first pass = %d, want 1", calls)
	}
}

func TestReporterOutageDoesNotStarveOtherRecords(t *testing.T) {
	a := newActStore(t, t.TempDir())
	save(t, a, readySucceeded(1), readySucceeded(2), readySucceeded(3))
	script := map[int64][]error{1: {ErrTransportFailure}, 2: {ErrTransportFailure}, 3: {ErrTransportFailure}}
	h := newHarness(t, a, script)
	h.runUntil(t, func() bool { return h.client.total() >= 4 })
	h.client.mu.Lock()
	var order []int64
	for _, c := range h.client.calls[:4] {
		order = append(order, c.attempt)
	}
	h.client.mu.Unlock()
	// One call per pass, round-robin by last try.
	if fmt.Sprint(order) != "[1 2 3 1]" {
		t.Fatalf("call order %v", order)
	}
	// Records 2 and 3 were deferred without a charge: their own first failure
	// still waits the first-failure delay (10s); record 1's second waits 20s.
	want := []time.Duration{10, 10, 10, 20}
	for i, w := range want {
		if h.waits[i] != w*time.Second {
			t.Fatalf("waits %v, want %v", h.waits[:4], want)
		}
	}
}

func TestReporterFailingRecordDoesNotDelayAnother(t *testing.T) {
	a := newActStore(t, t.TempDir())
	save(t, a, readyFailed(1), readySucceeded(2))
	h := newHarness(t, a, map[int64][]error{1: {ErrInvalidRequest}})
	h.runUntil(t, func() bool { return len(remaining(t, a)) == 1 && h.client.count(1) >= 3 })
	if h.client.count(2) != 1 {
		t.Fatalf("healthy record sent %d times", h.client.count(2))
	}
	// Record 2 was delivered on the very first pass, before any wait.
	h.client.mu.Lock()
	defer h.client.mu.Unlock()
	seen2 := false
	for _, c := range h.client.calls {
		if c.attempt == 2 {
			seen2 = true
		}
	}
	if !seen2 || h.client.calls[1].attempt != 2 {
		t.Fatalf("second record not tried right after the first: %+v", h.client.calls[:2])
	}
}

type flakyDeleteStore struct {
	activationRecordStore
	mu    sync.Mutex
	fails int
}

func (s *flakyDeleteStore) DeleteActivation(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fails > 0 {
		s.fails--
		return errors.New("disk full")
	}
	return s.activationRecordStore.DeleteActivation(id)
}

func TestReporterDeleteFailureResendsThenDeletes(t *testing.T) {
	a := newActStore(t, t.TempDir())
	save(t, a, readySucceeded(1))
	st := &flakyDeleteStore{activationRecordStore: a, fails: 2}
	h := newHarness(t, st, nil)
	h.runUntil(t, func() bool { return len(remaining(t, a)) == 0 })
	if h.client.count(1) != 3 {
		t.Fatalf("expected 2 resends after failed deletes, calls=%d", h.client.count(1))
	}
	if !strings.Contains(h.logText(), "delete failed") {
		t.Fatal("delete failure not logged")
	}
}

func TestReporterKickTriggersImmediatePass(t *testing.T) {
	a := newActStore(t, t.TempDir())
	h := newHarness(t, a, nil)
	idle := make(chan struct{}, 8)
	h.rep.after = func(time.Duration) <-chan time.Time { idle <- struct{}{}; return make(chan time.Time) } // never fires
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.rep.Run(ctx) }()
	<-idle // first pass done, now waiting
	save(t, a, readySucceeded(1))
	h.rep.Kick()
	h.rep.Kick() // coalesces
	<-idle
	if h.client.count(1) != 1 || len(remaining(t, a)) != 0 {
		t.Fatalf("kick did not deliver: calls=%d", h.client.count(1))
	}
	start := time.Now()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) || time.Since(start) > 2*time.Second {
		t.Fatalf("cancel: %v after %v", err, time.Since(start))
	}
}

func TestReporterCancelInterruptsHungCall(t *testing.T) {
	a := newActStore(t, t.TempDir())
	save(t, a, readySucceeded(1))
	h := newHarness(t, a, nil)
	h.rep.after = nil
	entered := make(chan struct{})
	blocked := &blockingClient{entered: entered}
	h.rep.Client = blocked
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.rep.Run(ctx) }()
	<-entered
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return on cancel")
	}
	if len(remaining(t, a)) != 1 {
		t.Fatal("record must survive shutdown")
	}
}

type blockingClient struct{ entered chan struct{} }

func (b *blockingClient) ReportActivationResult(ctx context.Context, _, _ int64, _ string, _ ActivationResult) error {
	close(b.entered)
	<-ctx.Done()
	return ctx.Err()
}

func TestReporterCallTimeout(t *testing.T) {
	a := newActStore(t, t.TempDir())
	save(t, a, readySucceeded(1))
	h := newHarness(t, a, nil)
	h.rep.CallTimeout = 20 * time.Millisecond
	calls := 0
	h.rep.Client = clientFunc(func(ctx context.Context) error {
		calls++
		if calls == 1 {
			<-ctx.Done() // hung request, released by the per-call timeout
			return ctx.Err()
		}
		return nil
	})
	h.runUntil(t, func() bool { return len(remaining(t, a)) == 0 })
	if calls != 2 {
		t.Fatalf("calls=%d", calls)
	}
}

type clientFunc func(ctx context.Context) error

func (f clientFunc) ReportActivationResult(ctx context.Context, _, _ int64, _ string, _ ActivationResult) error {
	return f(ctx)
}

func TestReporterStaleWarningOncePerRecord(t *testing.T) {
	a := newActStore(t, t.TempDir())
	save(t, a, readyFailed(1), readyFailed(2))
	h := newHarness(t, a, map[int64][]error{1: {ErrTransportFailure}, 2: {ErrTransportFailure}})
	h.rep.StaleAfter = 5 * time.Minute
	h.runUntil(t, func() bool { return h.client.total() >= 60 })
	for _, id := range []int{1, 2} {
		if n := strings.Count(h.logText(), fmt.Sprintf("WARNING attempt %d:", id)); n != 1 {
			t.Fatalf("attempt %d stale warnings = %d\n%s", id, n, h.logText())
		}
	}
}

func TestReporterNoStaleWarningWhenYoung(t *testing.T) {
	a := newActStore(t, t.TempDir())
	save(t, a, readyFailed(1))
	h := newHarness(t, a, map[int64][]error{1: {ErrTransportFailure}})
	h.runUntil(t, func() bool { return h.client.count(1) >= 3 })
	if strings.Contains(h.logText(), "WARNING") {
		t.Fatal("premature stale warning")
	}
}

func TestReporterSendsStoredResultVerbatim(t *testing.T) {
	a := newActStore(t, t.TempDir())
	ok, bad := readySucceeded(1), readyFailed(2)
	ok.Fence, bad.Fence, bad.OperationID = 11, 12, "77"
	save(t, a, ok, bad)
	h := newHarness(t, a, nil)
	h.runUntil(t, func() bool { return len(remaining(t, a)) == 0 })
	h.client.mu.Lock()
	defer h.client.mu.Unlock()
	for _, c := range h.client.calls {
		switch c.attempt {
		case 1:
			if c.fence != 11 || c.op != "42" || !c.res.succeeded || c.res.observedDigest != ok.ObservedRouteDigest || c.res.failureSummary != "" {
				t.Fatalf("success request: %+v", c)
			}
		case 2:
			if c.fence != 12 || c.op != "77" || c.res.succeeded || c.res.failureSummary != bad.FailureSummary || c.res.observedDigest != ([32]byte{}) {
				t.Fatalf("failure request: %+v", c)
			}
		}
	}
}

func TestReporterTerminalConflictThenDeleteFailureResends(t *testing.T) {
	a := newActStore(t, t.TempDir())
	save(t, a, readyFailed(1))
	st := &flakyDeleteStore{activationRecordStore: a, fails: 1}
	h := newHarness(t, st, map[int64][]error{1: {&ConflictError{Code: ConflictLateReportRecorded}}})
	h.runUntil(t, func() bool { return len(remaining(t, a)) == 0 })
	if h.client.count(1) != 2 {
		t.Fatalf("calls=%d, want re-send after failed delete", h.client.count(1))
	}
}

func TestReporterStaleWarnOnceOnFixedPath(t *testing.T) {
	a := newActStore(t, t.TempDir())
	save(t, a, readyFailed(1))
	h := newHarness(t, a, map[int64][]error{1: {&ConflictError{Code: ConflictOperationMismatch}}})
	h.rep.StaleAfter = 5 * time.Minute
	h.runUntil(t, func() bool { return h.client.count(1) >= 12 })
	if len(remaining(t, a)) != 1 {
		t.Fatal("operation_mismatch must keep the record")
	}
	if n := strings.Count(h.logText(), "WARNING attempt 1:"); n != 1 {
		t.Fatalf("warnings=%d", n)
	}
}
