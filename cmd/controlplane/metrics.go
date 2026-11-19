package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"autodeploy/internal/errclass"
	"autodeploy/internal/logging"
	"autodeploy/internal/metrics"
	"autodeploy/internal/store/postgres"
)

const (
	// metricsCacheTTL is how long one DB snapshot serves every scrape.
	metricsCacheTTL = 15 * time.Second
	// metricsFailureTTL is how long a failed snapshot is reused before retrying.
	metricsFailureTTL = 5 * time.Second
	// metricsWarnEvery rate-limits the snapshot-failure warning.
	metricsWarnEvery = time.Minute
)

// metricsFetchTimeout is the backstop around the snapshot query; the query
// also carries its own, shorter server-side statement_timeout. A var so tests
// can shorten it.
var metricsFetchTimeout = 3 * time.Second

// metricsSet owns the control plane's registry and the counters other
// components feed.
type metricsSet struct {
	registry      *metrics.Registry
	requests      *metrics.Counter
	sweepRuns     *metrics.Counter
	leasesExpired *metrics.Counter
	purgeRuns     *metrics.Counter
	cache         *metrics.Cache[postgres.MetricsSnapshot]
}

// newMetricsSet registers every control-plane metric. snapshot may be nil
// only in tests that do not scrape the DB gauges; it is then reported as down.
func newMetricsSet(version string, snapshot func(context.Context) (postgres.MetricsSnapshot, error), logger *slog.Logger) *metricsSet {
	r := metrics.NewRegistry()
	m := &metricsSet{registry: r}
	r.NewGaugeFunc("autodeploy_build_info", "Build information; the value is always 1.", []string{"version"}, func() []metrics.Sample {
		return []metrics.Sample{{LabelValues: []string{version}, Value: 1}}
	})
	m.requests = r.NewCounter("autodeploy_http_requests_total", "HTTP requests by surface and status class.", "surface", "code")
	for _, surface := range []string{surfaceWeb, surfaceAgent, surfaceWebhook} {
		for _, class := range []string{"2xx", "3xx", "4xx", "5xx"} {
			m.requests.Touch(surface, class)
		}
	}
	m.sweepRuns = r.NewCounter("autodeploy_lease_sweep_runs_total", "Lease sweeper ticks by result.", "result")
	m.leasesExpired = r.NewCounter("autodeploy_leases_expired_total", "Agent lease attempts recovered by the sweeper.")
	m.purgeRuns = r.NewCounter("autodeploy_log_purge_runs_total", "Deployment log purge runs by result.", "result")
	for _, result := range []string{"ok", "error"} {
		m.sweepRuns.Touch(result)
		m.purgeRuns.Touch(result)
	}
	m.leasesExpired.Touch()

	if snapshot == nil {
		snapshot = func(context.Context) (postgres.MetricsSnapshot, error) {
			return postgres.MetricsSnapshot{}, errors.New("no snapshot source")
		}
	}
	m.cache = metrics.NewCache(metricsCacheTTL, metricsFailureTTL, metricsFetchTimeout, snapshot, newRateLimitedWarn(logger, metricsWarnEvery, time.Now))

	// Every DB-derived gauge comes from ONE cache read per scrape (a gauge
	// set), so db_up and the gauges always agree. On failure only
	// autodeploy_db_up is exported (as 0): serving the last good numbers would
	// make a dead database look healthy.
	const (
		nDBUp, nJobs, nWebhooks, nReleases = "autodeploy_db_up", "autodeploy_agent_jobs", "autodeploy_webhook_jobs", "autodeploy_releases"
		nHeartbeat, nRecon, nOps           = "autodeploy_agent_heartbeat_age_seconds", "autodeploy_environments_reconciliation_required", "autodeploy_release_operations_open"
	)
	r.NewGaugeSet([]metrics.GaugeDesc{
		{Name: nDBUp, Help: "1 if the last metrics snapshot query succeeded, else 0."},
		{Name: nJobs, Help: "Agent jobs by state.", Labels: []string{"state"}},
		{Name: nWebhooks, Help: "Webhook deliveries awaiting processing (state=pending).", Labels: []string{"state"}},
		{Name: nReleases, Help: "Releases by state.", Labels: []string{"state"}},
		{Name: nHeartbeat, Help: "Seconds since the last heartbeat of each active server that has heartbeated.", Labels: []string{"server_id"}},
		{Name: nRecon, Help: "Environments flagged for operator reconciliation."},
		{Name: nOps, Help: "Release operations that are not yet closed."},
	}, func() map[string][]metrics.Sample {
		s, ok := m.cache.Get()
		if !ok {
			return map[string][]metrics.Sample{nDBUp: {{Value: 0}}}
		}
		states := func(in map[string]int64) []metrics.Sample {
			var out []metrics.Sample
			for state, n := range in {
				out = append(out, metrics.Sample{LabelValues: []string{state}, Value: float64(n)})
			}
			return out
		}
		var ages []metrics.Sample
		for id, age := range s.HeartbeatAgeSeconds {
			ages = append(ages, metrics.Sample{LabelValues: []string{id}, Value: age})
		}
		return map[string][]metrics.Sample{
			nDBUp:      {{Value: 1}},
			nJobs:      states(s.AgentJobs),
			nWebhooks:  states(s.WebhookJobs),
			nReleases:  states(s.Releases),
			nHeartbeat: ages,
			nRecon:     {{Value: float64(s.EnvironmentsReconciliationRequired)}},
			nOps:       {{Value: float64(s.ReleaseOperationsOpen)}},
		}
	})
	return m
}

// newRateLimitedWarn returns a callback that logs one fixed WARN line (the
// error class only, never its text) at most once per interval.
func newRateLimitedWarn(logger *slog.Logger, every time.Duration, now func() time.Time) func(error) {
	var mu sync.Mutex
	var last time.Time
	return func(err error) {
		mu.Lock()
		defer mu.Unlock()
		t := now()
		if !last.IsZero() && t.Sub(last) < every {
			return
		}
		last = t
		logger.Warn("metrics snapshot unavailable", logging.KeyComponent, "metrics", logging.KeyClass, errclass.Of(err))
	}
}

// OnSweepRun and OnPurgeRun adapt the store hooks to counters.
func (m *metricsSet) onSweepRun(expired int, ok bool) {
	m.sweepRuns.Inc(resultLabel(ok))
	if ok && expired > 0 {
		m.leasesExpired.Add(uint64(expired))
	}
}

func (m *metricsSet) onPurgeRun(ok bool) { m.purgeRuns.Inc(resultLabel(ok)) }

func resultLabel(ok bool) string {
	if ok {
		return "ok"
	}
	return "error"
}

// buildVersion is the short VCS revision when stamped, else the main module
// version, else "unknown".
func buildVersion(info *debug.BuildInfo, ok bool) string {
	if !ok || info == nil {
		return "unknown"
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" && s.Value != "" {
			return s.Value[:min(12, len(s.Value))]
		}
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	return "unknown"
}

// Surfaces for autodeploy_http_requests_total.
const (
	surfaceWeb     = "web"
	surfaceAgent   = "agent"
	surfaceWebhook = "webhook"
)

// statusRecorder captures the response status for metrics. It records only
// the first final WriteHeader (1xx informational headers are not final) and
// defaults to 200 when the handler never calls WriteHeader. It deliberately
// implements no optional interfaces itself: Unwrap lets http.ResponseController
// reach the real writer's Flusher/Hijacker.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 && (code >= 200 || code == http.StatusSwitchingProtocols) {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (s *statusRecorder) statusCode() int {
	if s.status == 0 {
		return http.StatusOK
	}
	return s.status
}

func statusClass(code int) string {
	switch {
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	case code >= 300:
		return "3xx"
	default:
		return "2xx"
	}
}

// instrument counts every response of next under surface. A nil counter
// returns next unchanged.
func instrument(surface string, requests *metrics.Counter, next http.Handler) http.Handler {
	if requests == nil || next == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w}
		defer func() {
			// A panicking handler aborts the response whatever was recorded.
			if p := recover(); p != nil {
				requests.Inc(surface, "5xx")
				panic(p)
			}
			requests.Inc(surface, statusClass(rec.statusCode()))
		}()
		next.ServeHTTP(rec, r)
	})
}

// newMetricsHandler serves only GET/HEAD /metrics: everything else is 404
// (other paths) or 405 (other methods on /metrics).
func newMetricsHandler(r *metrics.Registry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/metrics" {
			http.NotFound(w, req)
			return
		}
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", metrics.ContentType)
		if req.Method == http.MethodHead {
			return
		}
		_ = r.WriteText(w)
	})
}

// newMetricsServer shares the main server's timeouts, header limit and
// ErrorLog.
func newMetricsServer(address string, handler http.Handler, logger *slog.Logger) *http.Server {
	return newHTTPServer(address, handler, logger)
}

// startMetricsListener binds the metrics address synchronously, so a bind
// failure fails startup like the main listener. An empty address disables the
// listener (nil server, nil listener, nil error). The caller serves it.
func startMetricsListener(address string, handler http.Handler, logger *slog.Logger) (*http.Server, net.Listener, error) {
	if address == "" {
		return nil, nil, nil
	}
	ln, err := net.Listen("tcp", address)
	if err != nil {
		return nil, nil, err
	}
	return newMetricsServer(address, handler, logger), ln, nil
}

// serveMetrics serves the metrics listener until it stops. Unlike the main
// server, its failure must never end the control plane: a non-shutdown error
// is logged once (fixed message, error class only) and the goroutine exits.
// The returned channel closes when serving has stopped.
func serveMetrics(server *http.Server, listener net.Listener, logger *slog.Logger) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("metrics listener stopped", logging.KeyComponent, "metrics", logging.KeyClass, errclass.Of(err))
		}
	}()
	return done
}
