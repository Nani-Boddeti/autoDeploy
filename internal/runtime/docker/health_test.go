package docker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Short durations for the gate loop's own tests: runHealthGate is
// parameterized by interval/probeTimeout precisely so these can run in
// milliseconds instead of the real 2s/2s/60s ADR numbers while exercising
// identical loop logic.
const (
	testInterval = 5 * time.Millisecond
	testTimeout  = 20 * time.Millisecond
)

func alwaysRunning(context.Context) (ContainerStatus, error) {
	return ContainerStatus{State: ContainerStateRunning}, nil
}

func fixedProbe(ok bool) HealthProbeFunc {
	return func(context.Context) (bool, error) { return ok, nil }
}

func TestRunHealthGateThreeConsecutiveSuccesses(t *testing.T) {
	var calls int32
	probe := func(context.Context) (bool, error) {
		atomic.AddInt32(&calls, 1)
		return true, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := runHealthGate(ctx, probe, alwaysRunning, testInterval, testTimeout, 3)
	if err != nil {
		t.Fatalf("runHealthGate: %v", err)
	}
	if !result.Success {
		t.Fatalf("result = %+v, want success", result)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("probe called %d times, want exactly 3", got)
	}
}

func TestRunHealthGateResetsConsecutiveCountOnFailure(t *testing.T) {
	// Sequence: success, success, FAILURE, success, success, success.
	// A gate that does not reset on failure would report success after
	// the 5th call (2 + 3); the correct behavior requires all 6.
	seq := []bool{true, true, false, true, true, true}
	var i int
	probe := func(context.Context) (bool, error) {
		ok := seq[i]
		i++
		return ok, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := runHealthGate(ctx, probe, alwaysRunning, testInterval, testTimeout, 3)
	if err != nil {
		t.Fatalf("runHealthGate: %v", err)
	}
	if !result.Success {
		t.Fatalf("result = %+v, want eventual success", result)
	}
	if i != len(seq) {
		t.Fatalf("probe called %d times, want exactly %d (proves the reset, not just eventual success)", i, len(seq))
	}
}

func TestRunHealthGateDeadlineExpiry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	result, err := runHealthGate(ctx, fixedProbe(false), alwaysRunning, testInterval, testTimeout, 3)
	if err != nil {
		t.Fatalf("runHealthGate: %v", err)
	}
	if result.Success {
		t.Fatal("want failure on deadline expiry")
	}
	if result.Reason != HealthGateFailureDeadlineExceeded {
		t.Fatalf("reason = %q, want deadline_exceeded", result.Reason)
	}
}

func TestRunHealthGateNonSuccessNeverSucceeds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	result, err := runHealthGate(ctx, fixedProbe(false), alwaysRunning, testInterval, testTimeout, 3)
	if err != nil {
		t.Fatalf("runHealthGate: %v", err)
	}
	if result.Success {
		t.Fatal("an always-failing probe must never report gate success")
	}
}

func TestRunHealthGateContainerExitedMidGate(t *testing.T) {
	var calls int32
	state := func(context.Context) (ContainerStatus, error) {
		n := atomic.AddInt32(&calls, 1)
		if n >= 2 {
			return ContainerStatus{State: ContainerStateNotRunning, ExitCode: 1}, nil
		}
		return ContainerStatus{State: ContainerStateRunning}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := runHealthGate(ctx, fixedProbe(true), state, testInterval, testTimeout, 3)
	if err != nil {
		t.Fatalf("runHealthGate: %v", err)
	}
	if result.Success {
		t.Fatal("a container that stops running mid-gate must fail the gate, even with successful probes")
	}
	if result.Reason != HealthGateFailureContainerNotRunning {
		t.Fatalf("reason = %q, want container_not_running", result.Reason)
	}
}

func TestRunHealthGateContainerNeverRunningFailsImmediately(t *testing.T) {
	notRunning := func(context.Context) (ContainerStatus, error) {
		return ContainerStatus{State: ContainerStateNotRunning, ExitCode: 1}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	result, err := runHealthGate(ctx, fixedProbe(true), notRunning, testInterval, testTimeout, 3)
	if err != nil {
		t.Fatalf("runHealthGate: %v", err)
	}
	if result.Success {
		t.Fatal("want immediate failure when the container is never observed running")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("took %v, want an immediate failure not a wait for the deadline", elapsed)
	}
}

func TestRunHealthGateStatePropagatesError(t *testing.T) {
	wantErr := errors.New("inspect failed")
	state := func(context.Context) (ContainerStatus, error) { return ContainerStatus{}, wantErr }
	_, err := runHealthGate(context.Background(), fixedProbe(true), state, testInterval, testTimeout, 3)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
}

// --- HTTP probe (newHTTPProbe) tests, over a real httptest server ---

func newHTTPProbeTarget(srv *httptest.Server, path string) (string, HealthGateTarget) {
	u := srv.Listener.Addr().String() // host:port
	host, portStr, _ := strings.Cut(u, ":")
	port, _ := strconv.Atoi(portStr)
	return host, HealthGateTarget{Host: "example.internal", Path: path, Port: port, Scheme: "http"}
}

func TestHTTPProbeAccepts2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	ip, target := newHTTPProbeTarget(srv, "/healthz")
	probe := newHTTPProbe(ip, target)
	ok, err := probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !ok {
		t.Fatal("want success for 200")
	}
}

func TestHTTPProbeRejectsNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	ip, target := newHTTPProbeTarget(srv, "/healthz")
	probe := newHTTPProbe(ip, target)
	ok, err := probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if ok {
		t.Fatal("want failure for 500")
	}
}

// TestHTTPProbeRefusesRedirects proves redirects are OFF: a 3xx must count
// as a failed probe, not be transparently followed to whatever 2xx it
// points at.
func TestHTTPProbeRefusesRedirects(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirector.Close()

	ip, tg := newHTTPProbeTarget(redirector, "/healthz")
	probe := newHTTPProbe(ip, tg)
	ok, err := probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if ok {
		t.Fatal("a 3xx redirect must be rejected, not followed to a 2xx")
	}
}

// TestHTTPProbeBoundsResponseBody proves an oversized body does not hang
// or exhaust memory: the probe still completes and evaluates status, having
// discarded the body up to the bound.
func TestHTTPProbeBoundsResponseBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		buf := make([]byte, 64*1024)
		for i := 0; i < 64; i++ { // 4 MiB, well past healthGateMaxBodyBytes (1 MiB)
			w.Write(buf)
		}
	}))
	defer srv.Close()
	ip, target := newHTTPProbeTarget(srv, "/healthz")
	probe := newHTTPProbe(ip, target)

	done := make(chan struct{})
	var ok bool
	var err error
	go func() {
		ok, err = probe(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("probe did not return promptly against an oversized body")
	}
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !ok {
		t.Fatal("want success (status was 200 regardless of body size)")
	}
}

func TestHTTPProbeSendsConfiguredHostHeader(t *testing.T) {
	var gotHost string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	ip, target := newHTTPProbeTarget(srv, "/healthz")
	target.Host = "configured.example.internal"
	probe := newHTTPProbe(ip, target)
	if _, err := probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if gotHost != "configured.example.internal" {
		t.Fatalf("Host header = %q, want configured.example.internal", gotHost)
	}
}

func TestHTTPProbeConnectionRefusedIsFailureNotError(t *testing.T) {
	// A closed port: the probe must report (false, nil) so the gate loop
	// treats it as a normal failed attempt (resetting the consecutive
	// count) rather than propagating a hard error that would abort the
	// whole gate on one blip.
	target := HealthGateTarget{Host: "example.internal", Path: "/", Port: 1, Scheme: "http"}
	probe := newHTTPProbe("127.0.0.1", target)
	ok, err := probe(context.Background())
	if err != nil {
		t.Fatalf("probe returned an error, want (false, nil): %v", err)
	}
	if ok {
		t.Fatal("want failure for a refused connection")
	}
}

func TestHealthGateTargetValidateRejectsBadInput(t *testing.T) {
	base := HealthGateTarget{Host: "h", Path: "/p", Port: 80, Scheme: "http"}
	cases := []HealthGateTarget{
		{Path: "/p", Port: 80, Scheme: "http"},           // empty host
		{Host: "h", Port: 80, Scheme: "http"},            // empty path
		{Host: "h", Path: "p", Port: 80, Scheme: "http"}, // relative path
		{Host: "h", Path: "/p", Port: 0, Scheme: "http"}, // bad port
		{Host: "h", Path: "/p", Port: 80, Scheme: "ftp"}, // bad scheme
	}
	if err := base.validate(); err != nil {
		t.Fatalf("base target should validate: %v", err)
	}
	for _, c := range cases {
		if err := c.validate(); err == nil {
			t.Fatalf("case %+v validated, want rejection", c)
		}
	}
}
