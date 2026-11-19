package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agenthttp "autodeploy/internal/agent/http"
	githubhttp "autodeploy/internal/github/http"
	"autodeploy/internal/metrics"
	"autodeploy/internal/store/postgres"
)

func statusHandler(code int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) })
}

func scrape(t *testing.T, m *metricsSet) string {
	t.Helper()
	var b bytes.Buffer
	if err := m.registry.WriteText(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestStatusRecorderDefaultsTo200WhenWriteHeaderNeverCalled(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statusRecorder{ResponseWriter: rec}
	_, _ = sw.Write([]byte("hi"))
	if sw.statusCode() != 200 {
		t.Fatalf("status = %d", sw.statusCode())
	}
	if (&statusRecorder{ResponseWriter: httptest.NewRecorder()}).statusCode() != 200 {
		t.Fatal("untouched writer must default to 200")
	}
}

func TestStatusRecorderFirstWriteHeaderWins(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statusRecorder{ResponseWriter: rec}
	sw.WriteHeader(404)
	sw.WriteHeader(500)
	if sw.statusCode() != 404 || rec.Code != 404 {
		t.Fatalf("status = %d recorder = %d", sw.statusCode(), rec.Code)
	}
}

func TestStatusRecorderInformationalHeadersAreNotFinal(t *testing.T) {
	sw := &statusRecorder{ResponseWriter: httptest.NewRecorder()}
	sw.WriteHeader(http.StatusEarlyHints)
	sw.WriteHeader(http.StatusCreated)
	if sw.statusCode() != 201 {
		t.Fatalf("status = %d, want the final 201", sw.statusCode())
	}
}

func TestStatusRecorderUnwrapKeepsResponseController(t *testing.T) {
	rec := httptest.NewRecorder()
	var w http.ResponseWriter = &statusRecorder{ResponseWriter: rec}
	u, ok := w.(interface{ Unwrap() http.ResponseWriter })
	if !ok || u.Unwrap() != rec {
		t.Fatal("Unwrap must expose the underlying writer")
	}
	if err := http.NewResponseController(w).Flush(); err != nil {
		t.Fatalf("ResponseController cannot reach the Flusher through the wrapper: %v", err)
	}
	if !rec.Flushed {
		t.Fatal("flush did not reach the underlying writer")
	}
}

func TestComposeHTTPHandlerCountsPerSurfaceAndClass(t *testing.T) {
	m := newMetricsSet("test", nil, controlplaneLogger(io.Discard, nil))
	composed := composeHTTPHandler(statusHandler(302), statusHandler(204), statusHandler(503), m.requests)
	for _, c := range []struct{ path string }{
		{githubhttp.Path}, {githubhttp.Path},
		{agenthttp.Prefix + "enroll"},
		{"/login"}, {"/other"},
	} {
		composed.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "http://example.test"+c.path, nil))
	}
	out := scrape(t, m)
	for _, want := range []string{
		`autodeploy_http_requests_total{surface="webhook",code="2xx"} 2`,
		`autodeploy_http_requests_total{surface="agent",code="5xx"} 1`,
		`autodeploy_http_requests_total{surface="web",code="3xx"} 2`,
		`autodeploy_http_requests_total{surface="web",code="2xx"} 0`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestComposeHTTPHandlerCountsWebWhenOtherSurfacesDisabled(t *testing.T) {
	m := newMetricsSet("test", nil, controlplaneLogger(io.Discard, nil))
	composed := composeHTTPHandler(http.NotFoundHandler(), nil, nil, m.requests)
	composed.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.test/x", nil))
	if out := scrape(t, m); !strings.Contains(out, `{surface="web",code="4xx"} 1`+"\n") {
		t.Fatalf("web 404 not counted:\n%s", out)
	}
}

func TestComposeHTTPHandlerDefaultStatusCountsAs2xx(t *testing.T) {
	m := newMetricsSet("test", nil, controlplaneLogger(io.Discard, nil))
	composed := composeHTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("x")) }), nil, nil, m.requests)
	composed.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.test/x", nil))
	if out := scrape(t, m); !strings.Contains(out, `{surface="web",code="2xx"} 1`+"\n") {
		t.Fatalf("implicit 200 not counted:\n%s", out)
	}
}

func TestRegistryUsesOnlyAllowedLabelNames(t *testing.T) {
	m := newMetricsSet("test", nil, controlplaneLogger(io.Discard, nil))
	descs := m.registry.Descriptors()
	if len(descs) < 10 {
		t.Fatalf("expected the full metric set, got %d descriptors", len(descs))
	}
	for _, d := range descs {
		if !strings.HasPrefix(d.Name, "autodeploy_") {
			t.Errorf("%s lacks the autodeploy_ prefix", d.Name)
		}
		for _, l := range d.Labels {
			if !metrics.LabelAllowed(l) {
				t.Errorf("%s uses disallowed label %q", d.Name, l)
			}
		}
	}
}

func TestBuildVersion(t *testing.T) {
	if got := buildVersion(nil, false); got != "unknown" {
		t.Fatal(got)
	}
	rev := "0123456789abcdef0123456789abcdef01234567"
	if got := buildVersion(&debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: rev}}}, true); got != rev[:12] {
		t.Fatal(got)
	}
	if got := buildVersion(&debug.BuildInfo{Main: debug.Module{Version: "v1.2.3"}}, true); got != "v1.2.3" {
		t.Fatal(got)
	}
	if got := buildVersion(&debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, true); got != "unknown" {
		t.Fatal(got)
	}
	if got := buildVersion(&debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abc"}}}, true); got != "abc" {
		t.Fatal(got)
	}
}

func fakeSnapshot() postgres.MetricsSnapshot {
	return postgres.MetricsSnapshot{
		AgentJobs:                          map[string]int64{"queued": 2, "leased": 1},
		Releases:                           map[string]int64{"active": 1},
		WebhookJobs:                        map[string]int64{"pending": 0},
		HeartbeatAgeSeconds:                map[string]float64{"srv-1": 4.5},
		EnvironmentsReconciliationRequired: 1,
		ReleaseOperationsOpen:              3,
	}
}

func TestCollectorEmitsSnapshotGauges(t *testing.T) {
	m := newMetricsSet("v9", func(context.Context) (postgres.MetricsSnapshot, error) { return fakeSnapshot(), nil }, controlplaneLogger(io.Discard, nil))
	out := scrape(t, m)
	for _, want := range []string{
		`autodeploy_build_info{version="v9"} 1`,
		`autodeploy_db_up 1`,
		`autodeploy_agent_jobs{state="queued"} 2`,
		`autodeploy_agent_heartbeat_age_seconds{server_id="srv-1"} 4.5`,
		`autodeploy_environments_reconciliation_required 1`,
		`autodeploy_release_operations_open 3`,
		`autodeploy_webhook_jobs{state="pending"} 0`,
		`autodeploy_releases{state="active"} 1`,
		`autodeploy_lease_sweep_runs_total{result="ok"} 0`,
		`autodeploy_log_purge_runs_total{result="error"} 0`,
		`autodeploy_leases_expired_total 0`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestCollectorHonoursCacheAndSharesInFlightQuery(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	m := newMetricsSet("v", func(context.Context) (postgres.MetricsSnapshot, error) {
		calls.Add(1)
		<-release
		return fakeSnapshot(), nil
	}, controlplaneLogger(io.Discard, nil))
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = scrape(t, m) }()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	_ = scrape(t, m)
	if calls.Load() != 1 {
		t.Fatalf("snapshot queries = %d, want 1", calls.Load())
	}
}

func TestCollectorTimeoutEmitsDBDownWithoutDBGauges(t *testing.T) {
	prev := metricsFetchTimeout
	metricsFetchTimeout = 100 * time.Millisecond
	t.Cleanup(func() { metricsFetchTimeout = prev })
	var logs bytes.Buffer
	m := newMetricsSet("v", func(ctx context.Context) (postgres.MetricsSnapshot, error) {
		<-ctx.Done()
		return postgres.MetricsSnapshot{}, ctx.Err()
	}, controlplaneLogger(&logs, nil))
	start := time.Now()
	out := scrape(t, m)
	if time.Since(start) > 3*time.Second {
		t.Fatal("scrape hung")
	}
	if !strings.Contains(out, "autodeploy_db_up 0\n") {
		t.Fatalf("no db_up 0:\n%s", out)
	}
	for _, gone := range []string{"autodeploy_agent_jobs", "autodeploy_agent_heartbeat_age_seconds", "autodeploy_environments_reconciliation_required", "autodeploy_release_operations_open", "autodeploy_webhook_jobs", "autodeploy_releases{"} {
		if strings.Contains(out, gone) {
			t.Fatalf("stale DB gauge %s exported while the DB is down", gone)
		}
	}
	if !strings.Contains(out, "autodeploy_build_info") || !strings.Contains(out, "autodeploy_http_requests_total") {
		t.Fatal("process-local metrics must survive a DB outage")
	}
	if !strings.Contains(logs.String(), "deadline_exceeded") || strings.Count(logs.String(), "\n") != 1 {
		t.Fatalf("want exactly one fixed warn line with the error class, got %q", logs.String())
	}
}

func TestSnapshotFailureWarningIsRateLimited(t *testing.T) {
	var logs bytes.Buffer
	now := time.Unix(1000, 0)
	w := newRateLimitedWarn(controlplaneLogger(&logs, nil), time.Minute, func() time.Time { return now })
	boom := errors.New("secret postgres://user:pw@host/db")
	w(boom)
	w(boom)
	now = now.Add(61 * time.Second)
	w(boom)
	if n := strings.Count(logs.String(), "\n"); n != 2 {
		t.Fatalf("warn lines = %d, want 2", n)
	}
	if strings.Contains(logs.String(), "secret") || strings.Contains(logs.String(), "pw@") {
		t.Fatal("error text leaked into the log")
	}
}

func TestMetricsHandlerServesOnlyMetrics(t *testing.T) {
	m := newMetricsSet("v", func(context.Context) (postgres.MetricsSnapshot, error) { return fakeSnapshot(), nil }, controlplaneLogger(io.Discard, nil))
	h := newMetricsHandler(m.registry)
	for _, c := range []struct {
		method, path string
		code         int
	}{
		{http.MethodGet, "/metrics", 200},
		{http.MethodHead, "/metrics", 200},
		{http.MethodPost, "/metrics", 405},
		{http.MethodDelete, "/metrics", 405},
		{http.MethodGet, "/", 404},
		{http.MethodGet, "/metrics/", 404},
		{http.MethodGet, "/login", 404},
		{http.MethodGet, "/agent/v1/enroll", 404},
		{http.MethodPost, "/other", 404},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(c.method, "http://127.0.0.1:9101"+c.path, nil))
		if rec.Code != c.code {
			t.Fatalf("%s %s = %d, want %d", c.method, c.path, rec.Code, c.code)
		}
		if c.code == 405 && rec.Header().Get("Allow") == "" {
			t.Fatal("405 needs an Allow header")
		}
		if c.code == 200 && rec.Header().Get("Content-Type") != metrics.ContentType {
			t.Fatalf("content type %q", rec.Header().Get("Content-Type"))
		}
		if c.method == http.MethodHead && rec.Body.Len() != 0 {
			t.Fatal("HEAD must not carry a body")
		}
	}
}

func TestNewMetricsServerReusesProductionLimitsAndErrorLog(t *testing.T) {
	s := newMetricsServer("127.0.0.1:9101", http.NotFoundHandler(), controlplaneLogger(io.Discard, nil))
	if s.ReadHeaderTimeout != readHeaderTimeout || s.ReadTimeout != readTimeout || s.WriteTimeout != writeTimeout || s.IdleTimeout != idleTimeout || s.MaxHeaderBytes != maxHeaderBytes || s.ErrorLog == nil {
		t.Fatal("metrics server must share the main server's limits and ErrorLog")
	}
}

func TestListenMetricsDisabledWhenUnset(t *testing.T) {
	srv, ln, err := startMetricsListener("", http.NotFoundHandler(), controlplaneLogger(io.Discard, nil))
	if err != nil || srv != nil || ln != nil {
		t.Fatalf("unset address must not start a listener: %v %v %v", srv, ln, err)
	}
}

func TestMetricsListenerServesOverTCP(t *testing.T) {
	m := newMetricsSet("v", func(context.Context) (postgres.MetricsSnapshot, error) { return fakeSnapshot(), nil }, controlplaneLogger(io.Discard, nil))
	srv, ln, err := startMetricsListener("127.0.0.1:0", newMetricsHandler(m.registry), controlplaneLogger(io.Discard, nil))
	if err != nil {
		t.Skipf("cannot listen in this sandbox: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	base := "http://" + ln.Addr().String()
	resp, err := http.Get(base + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "autodeploy_db_up 1") {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}
	resp, err = http.Get(base + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, http.ErrServerClosed) {
		t.Fatal(err)
	}
}

func TestStartMetricsListenerFailsOnAddressInUse(t *testing.T) {
	_, ln, err := startMetricsListener("127.0.0.1:0", http.NotFoundHandler(), controlplaneLogger(io.Discard, nil))
	if err != nil {
		t.Skipf("cannot listen in this sandbox: %v", err)
	}
	defer ln.Close()
	if _, _, err := startMetricsListener(ln.Addr().String(), http.NotFoundHandler(), controlplaneLogger(io.Discard, nil)); err == nil {
		t.Fatal("a second bind on the same address must fail startup")
	}
}

func TestSingleWriteTextTriggersOneFetchAndGaugesAgree(t *testing.T) {
	var calls atomic.Int32
	m := newMetricsSet("v", func(context.Context) (postgres.MetricsSnapshot, error) {
		n := calls.Add(1)
		s := fakeSnapshot()
		s.ReleaseOperationsOpen = int64(n)
		s.EnvironmentsReconciliationRequired = int64(n)
		return s, nil
	}, controlplaneLogger(io.Discard, nil))
	// The clock jumps 1000s on every read, so the cache is expired at every possible Get: a gauge reading the cache
	// separately would observe a newer snapshot than db_up.
	var ticks atomic.Int64
	m.cache.SetClock(func() time.Time { return time.Unix(ticks.Add(1)*1000, 0) })
	out := scrape(t, m)
	if calls.Load() != 1 {
		t.Fatalf("fetches in one scrape = %d, want 1", calls.Load())
	}
	if !strings.Contains(out, "autodeploy_release_operations_open 1\n") || !strings.Contains(out, "autodeploy_environments_reconciliation_required 1\n") || !strings.Contains(out, "autodeploy_db_up 1\n") {
		t.Fatalf("gauges disagree:\n%s", out)
	}
}

func TestInstrumentPanicCounts5xxAndRepanicsSameValue(t *testing.T) {
	for name, value := range map[string]any{"abort": http.ErrAbortHandler, "plain": "boom"} {
		t.Run(name, func(t *testing.T) {
			m := newMetricsSet("v", nil, controlplaneLogger(io.Discard, nil))
			h := instrument(surfaceAgent, m.requests, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(value) }))
			var got any
			func() {
				defer func() { got = recover() }()
				h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.test/x", nil))
			}()
			if got != value {
				t.Fatalf("recovered %v, want the original panic value %v", got, value)
			}
			if out := scrape(t, m); !strings.Contains(out, `{surface="agent",code="5xx"} 1`+"\n") || strings.Contains(out, `{surface="agent",code="2xx"} 1`) {
				t.Fatalf("panic not counted as 5xx:\n%s", out)
			}
		})
	}
}

// A metrics listener that dies after startup is logged once and nothing else
// happens: the main server keeps serving, and shutdown of both completes.
func TestMetricsServeFailureDoesNotAffectMainServer(t *testing.T) {
	logger := func(w io.Writer) *slog.Logger { return controlplaneLogger(w, nil) }
	mainServer := newHTTPServer("", statusHandler(http.StatusNoContent), logger(io.Discard))
	mainLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen in this sandbox: %v", err)
	}
	mainErr := make(chan error, 1)
	go func() { mainErr <- mainServer.Serve(mainLn) }()

	var logs syncBuffer
	srv, ln, err := startMetricsListener("127.0.0.1:0", http.NotFoundHandler(), logger(io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	done := serveMetrics(srv, ln, logger(&logs))
	_ = ln.Close() // force Serve to fail with a non-ErrServerClosed error
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("serveMetrics did not return after its listener failed")
	}
	if got := logs.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, "metrics listener stopped") || !strings.Contains(got, "component=metrics") && !strings.Contains(got, `"component":"metrics"`) {
		t.Fatalf("want one fixed ERROR line, got %q", got)
	}
	select {
	case err := <-mainErr:
		t.Fatalf("main server stopped: %v", err)
	default:
	}
	resp, err := http.Get("http://" + mainLn.Addr().String() + "/")
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("main server no longer serves: %v", err)
	}
	resp.Body.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := errors.Join(mainServer.Shutdown(ctx), srv.Shutdown(ctx)); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if err := <-mainErr; !errors.Is(err, http.ErrServerClosed) {
		t.Fatal(err)
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }
