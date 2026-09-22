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

	"autodeploy/internal/healthgate"
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

// TestRunHealthGatePropagatesProbeErrorImmediately proves the fix for the
// independent-review finding: unlike an ordinary probe failure (which
// newHTTPProbe always reports as (false, nil), see
// TestHTTPProbeConnectionRefusedIsFailureNotError), a non-nil probe error
// -- the shape newHTTPProbe now returns when its own request-target does
// not match the configured path -- must abort the gate loop immediately,
// not be silently swallowed and retried until the 60s deadline.
func TestRunHealthGatePropagatesProbeErrorImmediately(t *testing.T) {
	wantErr := errors.New("probe target mismatch")
	var calls int32
	probe := func(context.Context) (bool, error) {
		atomic.AddInt32(&calls, 1)
		return false, wantErr
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	_, err := runHealthGate(ctx, probe, alwaysRunning, testInterval, testTimeout, 3)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("took %v, want an immediate abort on the first probe error, not a wait for the deadline", elapsed)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("probe called %d times, want exactly 1 (abort on first error, no retry)", got)
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

// TestHealthGateTargetValidateRejectsHeaderInjectionAndUserinfoHost proves
// Slice 10 Step 0's defense-in-depth re-validation (validate now delegates
// to internal/healthgate, mirroring migration 000017's CHECKs exactly,
// rather than the old bare "non-empty"/"starts with /" shape check): a
// userinfo-bearing host, an embedded path, an IPv6-bracket host, an
// out-of-range port suffix, and a path carrying a header-injection-shaped
// control character or '#' are all rejected, even though the old, looser
// checks would have accepted every one of them.
func TestHealthGateTargetValidateRejectsHeaderInjectionAndUserinfoHost(t *testing.T) {
	cases := map[string]HealthGateTarget{
		"userinfo host":                            {Host: "user@host", Path: "/p", Port: 80, Scheme: "http"},
		"embedded path in host":                    {Host: "host/path", Path: "/p", Port: 80, Scheme: "http"},
		"ipv6 bracket host":                        {Host: "[::1]", Path: "/p", Port: 80, Scheme: "http"},
		"host port out of range":                   {Host: "host:70000", Path: "/p", Port: 80, Scheme: "http"},
		"path with CR/LF (header injection shape)": {Host: "h", Path: "/p\r\nX-Injected: 1", Port: 80, Scheme: "http"},
		"path with double leading slash":           {Host: "h", Path: "//evil", Port: 80, Scheme: "http"},
		"path with fragment delimiter":             {Host: "h", Path: "/a#b", Port: 80, Scheme: "http"},
	}
	for name, tg := range cases {
		if err := tg.validate(); err == nil {
			t.Fatalf("%s: target %+v validated, want rejection", name, tg)
		}
	}
}

// TestHTTPProbeSplitsQueryStringFromPath proves the url.URL-based request
// construction (replacing the old target.Scheme+"://"+target.Host+target.Path
// string concatenation) correctly separates a '?'-introduced query string
// from the request path, both landing on the server exactly as configured.
func TestHTTPProbeSplitsQueryStringFromPath(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	ip, target := newHTTPProbeTarget(srv, "/healthz?ready=1")
	probe := newHTTPProbe(ip, target)
	if _, err := probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if gotPath != "/healthz" {
		t.Fatalf("path = %q, want /healthz", gotPath)
	}
	if gotQuery != "ready=1" {
		t.Fatalf("query = %q, want ready=1", gotQuery)
	}
}

// TestHTTPProbeRequestTargetIsByteIdenticalToConfiguredPath is the
// independent-review regression test: for every accepted path shape
// (including ones that are already percent-encoded, or that carry
// characters a naive re-encoding would mangle), the exact bytes the server
// reads off the wire as the request-target (net/http's r.RequestURI,
// populated from the raw request line before any decoding) must equal
// target.Path exactly. Before the fix, url.URL{Path: target.Path} silently
// re-encoded "/health%2Fcheck" into "/health%252Fcheck" and similar.
func TestHTTPProbeRequestTargetIsByteIdenticalToConfiguredPath(t *testing.T) {
	cases := []string{
		"/",
		"/healthz",
		"/health%2Fcheck",
		"/a;b",
		"/p?x=1&y=%20",
		"/a:b@c",
		"/foo?",  // empty query: net/url ForceQuery must keep the bare '?'
		"/a?b?c", // second '?' belongs to the raw query
	}
	for _, path := range cases {
		t.Run(path, func(t *testing.T) {
			if !healthgate.ValidPath(path) {
				t.Fatalf("test bug: %q is not accepted by healthgate.ValidPath", path)
			}
			var gotRequestURI string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotRequestURI = r.RequestURI
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()
			ip, target := newHTTPProbeTarget(srv, path)
			probe := newHTTPProbe(ip, target)
			ok, err := probe(context.Background())
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			if !ok {
				t.Fatal("want success for 200")
			}
			if gotRequestURI != path {
				t.Fatalf("server observed request-target %q, want the configured path %q byte-identically", gotRequestURI, path)
			}
		})
	}
}
