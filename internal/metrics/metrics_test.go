package metrics_test

import (
	"bytes"
	"math"
	"strings"
	"sync"
	"testing"

	"autodeploy/internal/metrics"
)

func render(t *testing.T, r *metrics.Registry) string {
	t.Helper()
	var b bytes.Buffer
	if err := r.WriteText(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestWriteTextGolden(t *testing.T) {
	r := metrics.NewRegistry()
	req := r.NewCounter("autodeploy_http_requests_total", "Requests by surface.\nSecond \\ line.", "surface", "code")
	req.Inc("web", "5xx")
	req.Add(2, "agent", "2xx")
	req.Inc("agent", "2xx")
	r.NewGaugeFunc("autodeploy_build_info", "Build.", []string{"version"}, func() []metrics.Sample {
		return []metrics.Sample{{LabelValues: []string{"a\"b\\c\nd"}, Value: 1}}
	})
	r.NewGaugeFunc("autodeploy_agent_heartbeat_age_seconds", "Age.", []string{"server_id"}, func() []metrics.Sample {
		return []metrics.Sample{{LabelValues: []string{"s2"}, Value: 1.5}, {LabelValues: []string{"s1"}, Value: math.Inf(1)}}
	})
	want := `# HELP autodeploy_agent_heartbeat_age_seconds Age.
# TYPE autodeploy_agent_heartbeat_age_seconds gauge
autodeploy_agent_heartbeat_age_seconds{server_id="s1"} +Inf
autodeploy_agent_heartbeat_age_seconds{server_id="s2"} 1.5
# HELP autodeploy_build_info Build.
# TYPE autodeploy_build_info gauge
autodeploy_build_info{version="a\"b\\c\nd"} 1
# HELP autodeploy_http_requests_total Requests by surface.\nSecond \\ line.
# TYPE autodeploy_http_requests_total counter
autodeploy_http_requests_total{surface="agent",code="2xx"} 3
autodeploy_http_requests_total{surface="web",code="5xx"} 1
`
	if got := render(t, r); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestUnlabelledAndEmptyFamilies(t *testing.T) {
	r := metrics.NewRegistry()
	c := r.NewCounter("autodeploy_x_total", "X.")
	c.Add(1000000)
	r.NewGaugeFunc("autodeploy_empty", "E.", []string{"state"}, func() []metrics.Sample { return nil })
	got := render(t, r)
	if !strings.Contains(got, "autodeploy_x_total 1000000\n") || strings.Contains(got, "autodeploy_empty") {
		t.Fatalf("unexpected output:\n%s", got)
	}
}

func TestTouchPublishesZeroSeries(t *testing.T) {
	r := metrics.NewRegistry()
	c := r.NewCounter("autodeploy_t_total", "T.", "result")
	c.Touch("ok")
	if got := render(t, r); !strings.Contains(got, `autodeploy_t_total{result="ok"} 0`) {
		t.Fatalf("missing zero series:\n%s", got)
	}
}

func TestConcurrentInc(t *testing.T) {
	r := metrics.NewRegistry()
	c := r.NewCounter("autodeploy_c_total", "C.", "result")
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				c.Inc("ok")
				if j%50 == 0 {
					_ = r.WriteText(&bytes.Buffer{})
				}
			}
		}()
	}
	wg.Wait()
	if got := render(t, r); !strings.Contains(got, `autodeploy_c_total{result="ok"} 10000`) {
		t.Fatalf("lost increments:\n%s", got)
	}
}

func mustPanic(t *testing.T, name string, f func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatalf("%s: expected panic", name)
		}
	}()
	f()
}

func TestRegistrationRules(t *testing.T) {
	r := metrics.NewRegistry()
	mustPanic(t, "disallowed label", func() { r.NewCounter("autodeploy_a_total", "A.", "owner") })
	mustPanic(t, "bad name", func() { r.NewCounter("1bad", "A.") })
	mustPanic(t, "gauge disallowed label", func() {
		r.NewGaugeFunc("autodeploy_g", "G.", []string{"hostname"}, func() []metrics.Sample { return nil })
	})
	r.NewCounter("autodeploy_dup_total", "D.")
	mustPanic(t, "duplicate", func() { r.NewCounter("autodeploy_dup_total", "D.") })
	c := r.NewCounter("autodeploy_arity_total", "A.", "result")
	mustPanic(t, "arity", func() { c.Inc("ok", "extra") })
}

func TestAllowedLabels(t *testing.T) {
	for _, n := range []string{"surface", "code", "result", "state", "server_id", "version"} {
		if !metrics.LabelAllowed(n) {
			t.Fatalf("%s must be allowed", n)
		}
	}
	for _, n := range []string{"owner", "project", "environment", "host", "repo"} {
		if metrics.LabelAllowed(n) {
			t.Fatalf("%s must be denied", n)
		}
	}
}

func TestGaugeSetCollectsOncePerWriteText(t *testing.T) {
	r := metrics.NewRegistry()
	var calls int
	r.NewGaugeSet([]metrics.GaugeDesc{
		{Name: "autodeploy_s_a", Help: "A.", Labels: []string{"state"}},
		{Name: "autodeploy_s_b", Help: "B."},
	}, func() map[string][]metrics.Sample {
		calls++
		return map[string][]metrics.Sample{
			"autodeploy_s_a": {{LabelValues: []string{"x"}, Value: float64(calls)}},
			"autodeploy_s_b": {{Value: float64(calls)}},
		}
	})
	r.NewGaugeFunc("autodeploy_m", "M.", nil, func() []metrics.Sample { return []metrics.Sample{{Value: 1}} })
	got := render(t, r)
	if calls != 1 || !strings.Contains(got, `autodeploy_s_a{state="x"} 1`) || !strings.Contains(got, "autodeploy_s_b 1\n") {
		t.Fatalf("calls=%d\n%s", calls, got)
	}
	if strings.Index(got, "autodeploy_m ") > strings.Index(got, "autodeploy_s_a") || strings.Index(got, "autodeploy_s_a") > strings.Index(got, "autodeploy_s_b") {
		t.Fatalf("families must stay globally sorted:\n%s", got)
	}
	mustPanic(t, "set dup", func() { r.NewGaugeSet([]metrics.GaugeDesc{{Name: "autodeploy_m", Help: "x"}}, nil) })
	mustPanic(t, "set label", func() {
		r.NewGaugeSet([]metrics.GaugeDesc{{Name: "autodeploy_z", Help: "x", Labels: []string{"owner"}}}, nil)
	})
}
