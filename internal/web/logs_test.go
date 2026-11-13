package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"autodeploy/internal/agent/logship"
	"autodeploy/internal/auth"
	"autodeploy/internal/store/postgres"
)

type logStub struct {
	page      postgres.DeploymentLogPage
	err       error
	gotID     string
	gotAfter  int64
	gotLimit  int
	gotOwner  string
	callCount int
}

func (s *logStub) ListDeploymentLogs(_ context.Context, p auth.Principal, id string, after int64, limit int) (postgres.DeploymentLogPage, error) {
	s.callCount++
	s.gotOwner, s.gotID, s.gotAfter, s.gotLimit = p.OwnerID, id, after, limit
	if s.err != nil {
		return postgres.DeploymentLogPage{}, s.err
	}
	return s.page, nil
}

func logsHandler(t *testing.T) (*Handler, *logStub, *operationsStub, auth.Principal) {
	t.Helper()
	h, ops, p := operationsHandler(t)
	ops.detail = sampleDetail("leased", false)
	ls := &logStub{}
	h.config.DeploymentLogReader = ls
	return h, ls, ops, p
}

func line(id int64, attempt int, stream, body string) postgres.DeploymentLogLine {
	return postgres.DeploymentLogLine{ID: id, AttemptID: 100 + id, AttemptNumber: attempt, Stream: stream, Seq: id, Body: body, CreatedAt: t0}
}

func i64(n int64) *int64 { return &n }

func TestLogsDisabledWithoutReaderAndNoLink(t *testing.T) {
	h, _, ops, p := logsHandler(t)
	h.config.DeploymentLogReader = nil
	if rr := opsGet(h, p, http.MethodGet, "/deployments/logs?id=dep-1"); rr.Code != http.StatusNotFound {
		t.Fatalf("nil reader = %d", rr.Code)
	}
	ops.detail = sampleDetail("leased", false)
	rr := opsGet(h, p, http.MethodGet, "/deployments/detail?id=dep-1")
	if rr.Code != 200 || strings.Contains(rr.Body.String(), "/deployments/logs") {
		t.Fatalf("detail without reader: %d link present", rr.Code)
	}
}

func TestLogsLinkOnDetail(t *testing.T) {
	h, _, _, p := logsHandler(t)
	rr := opsGet(h, p, http.MethodGet, "/deployments/detail?id=dep-1")
	if !strings.Contains(rr.Body.String(), `<a href="/deployments/logs?id=dep-1">Logs</a>`) {
		t.Fatalf("missing logs link: %s", rr.Body.String())
	}
}

func TestLogsAuthAndMethods(t *testing.T) {
	h, ls, _, p := logsHandler(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "https://admin.example.test/deployments/logs?id=dep-1", nil))
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatalf("unauthenticated = %d %q", rr.Code, rr.Header().Get("Location"))
	}
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rr := opsGet(h, p, m, "/deployments/logs?id=dep-1")
		if rr.Code != http.StatusMethodNotAllowed || rr.Header().Get("Allow") != "GET, HEAD" {
			t.Errorf("%s = %d allow=%q", m, rr.Code, rr.Header().Get("Allow"))
		}
	}
	if rr := opsGet(h, p, http.MethodHead, "/deployments/logs?id=dep-1"); rr.Code != 200 {
		t.Errorf("HEAD = %d", rr.Code)
	}
	ls.err = postgres.ErrServerAuthorizationDenied
	rr = opsGet(h, p, http.MethodGet, "/deployments/logs?id=dep-1")
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatalf("denied = %d %q", rr.Code, rr.Header().Get("Location"))
	}
}

func TestLogsQueryValidation(t *testing.T) {
	h, ls, _, p := logsHandler(t)
	for _, target := range []string{"/deployments/logs", "/deployments/logs?id=", "/deployments/logs?id=a&id=b", "/deployments/logs?id=dep-1&limit=5", "/deployments/logs?id=dep-1&x=1", "/deployments/logs?id=a%00b"} {
		if rr := opsGet(h, p, http.MethodGet, target); rr.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", target, rr.Code)
		}
	}
	for _, after := range []string{"", "-1", "+1", "abc", "1.5", "01", "9223372036854775808", "%201", "0x10"} {
		target := "/deployments/logs?id=dep-1&after=" + after
		if rr := opsGet(h, p, http.MethodGet, target); rr.Code != http.StatusBadRequest {
			t.Errorf("after=%q = %d, want 400", after, rr.Code)
		}
	}
	if rr := opsGet(h, p, http.MethodGet, "/deployments/logs?id=dep-1&after=1&after=2"); rr.Code != http.StatusBadRequest {
		t.Errorf("repeated after = %d", rr.Code)
	}
	if ls.callCount != 0 {
		t.Fatalf("reader called %d times for invalid input", ls.callCount)
	}
	if rr := opsGet(h, p, http.MethodGet, "/deployments/logs?id=dep-1&after=0"); rr.Code != 200 || ls.gotAfter != 0 || ls.gotID != "dep-1" || ls.gotLimit != 0 {
		t.Fatalf("after=0: %d %+v", rr.Code, ls)
	}
	if rr := opsGet(h, p, http.MethodGet, "/deployments/logs?id=dep-1&after=9223372036854775807"); rr.Code != 200 || ls.gotAfter != 9223372036854775807 {
		t.Fatalf("max after: %d %d", rr.Code, ls.gotAfter)
	}
}

func TestLogsNotFoundMatchesDetail(t *testing.T) {
	h, ls, ops, p := logsHandler(t)
	ops.err = postgres.ErrOperationsNotFound
	want := opsGet(h, p, http.MethodGet, "/deployments/detail?id=nope")
	for _, e := range []error{postgres.ErrOperationsNotFound, postgres.ErrOperationsInvalidInput} {
		ls.err = e
		got := opsGet(h, p, http.MethodGet, "/deployments/logs?id=nope")
		if got.Code != http.StatusNotFound || got.Body.String() != want.Body.String() {
			t.Errorf("%v: %d %q vs %q", e, got.Code, got.Body.String(), want.Body.String())
		}
	}
	ls.err = errors.New("boom secret detail")
	got := opsGet(h, p, http.MethodGet, "/deployments/logs?id=dep-1")
	if got.Code != http.StatusInternalServerError || strings.Contains(got.Body.String(), "boom") {
		t.Errorf("internal error = %d %q", got.Code, got.Body.String())
	}
}

func TestLogsGroupingStreamsAndMarkers(t *testing.T) {
	h, ls, _, p := logsHandler(t)
	ls.page = postgres.DeploymentLogPage{Items: []postgres.DeploymentLogLine{
		line(1, 1, "build", "step one"), line(2, 1, "phase", "p"), line(3, 1, "health", "h"),
		line(4, 2, "activation", "a"), line(5, 2, "drain", "d"), line(6, 2, "system", "s"),
		line(7, 2, "system", postgres.LogTruncationMarker), line(8, 2, "system", "[logs dropped: 12 lines]"),
		line(9, 2, "evil\"><x", "u"),
	}}
	body := opsGet(h, p, http.MethodGet, "/deployments/logs?id=dep-1").Body.String()
	if strings.Count(body, `<section class="attempt">`) != 2 || !strings.Contains(body, "<h2>Attempt 1</h2>") || !strings.Contains(body, "<h2>Attempt 2</h2>") {
		t.Fatalf("grouping wrong: %s", body)
	}
	if strings.Index(body, "Attempt 1") > strings.Index(body, "Attempt 2") {
		t.Fatal("attempts out of order")
	}
	for _, s := range []string{"build", "phase", "health", "activation", "drain", "system", "other"} {
		if !strings.Contains(body, `class="stream stream-`+s+`">`+s+`</span>`) {
			t.Errorf("missing stream class %s", s)
		}
	}
	if strings.Contains(body, "evil") || strings.Contains(body, "<x") {
		t.Error("unknown stream text leaked")
	}
	if strings.Count(body, `<li class="marker">`) != 2 {
		t.Errorf("marker rows = %d, want 2", strings.Count(body, `<li class="marker">`))
	}
	if !strings.Contains(body, `<time datetime="2026-11-11T10:00:00.123Z">10:00:00.123</time>`) {
		t.Error("time cell wrong")
	}
}

func TestLogsEscapesBody(t *testing.T) {
	h, ls, _, p := logsHandler(t)
	ls.page = postgres.DeploymentLogPage{Items: []postgres.DeploymentLogLine{line(1, 1, "build", "</pre><script>alert(1)</script>")}}
	body := opsGet(h, p, http.MethodGet, "/deployments/logs?id=dep-1").Body.String()
	if strings.Contains(body, "<script>") || strings.Count(body, "</pre>") != 1 || !strings.Contains(body, "&lt;/pre&gt;&lt;script&gt;") {
		t.Fatalf("body not escaped: %s", body)
	}
}

func TestLogsEmptyState(t *testing.T) {
	h, _, _, p := logsHandler(t)
	body := opsGet(h, p, http.MethodGet, "/deployments/logs?id=dep-1").Body.String()
	if !strings.Contains(body, "No log lines yet for this deployment.") {
		t.Fatal("empty state text missing")
	}
}

func TestLogsRefreshRule(t *testing.T) {
	const meta = `<meta http-equiv="refresh" content="3">`
	page := postgres.DeploymentLogPage{Items: []postgres.DeploymentLogLine{line(1, 1, "build", "x")}}
	cases := []struct {
		name    string
		state   string
		next    *int64
		noOps   bool
		opsErr  error
		refresh bool
	}{
		{"non-terminal last page", "leased", nil, false, nil, true},
		{"terminal last page", "succeeded", nil, false, nil, false},
		{"dead last page", "dead", nil, false, nil, false},
		{"non-terminal not last page", "leased", i64(1), false, nil, false},
		{"no operations reader", "leased", nil, true, nil, false},
		{"detail lookup fails", "leased", nil, false, errors.New("x"), false},
	}
	for _, c := range cases {
		h, ls, ops, p := logsHandler(t)
		ops.detail = sampleDetail(c.state, false)
		ops.err = c.opsErr
		if c.noOps {
			h.config.OperationsReader = nil
		}
		ls.page = page
		ls.page.Next = c.next
		body := opsGet(h, p, http.MethodGet, "/deployments/logs?id=dep-1&after=5").Body.String()
		if got := strings.Contains(body, meta); got != c.refresh {
			t.Errorf("%s: refresh=%v want %v", c.name, got, c.refresh)
		}
	}
}

func TestLogsNewerAndStartLinks(t *testing.T) {
	h, ls, _, p := logsHandler(t)
	ls.page = postgres.DeploymentLogPage{Items: []postgres.DeploymentLogLine{line(40, 1, "build", "x")}, Next: i64(40)}
	body := opsGet(h, p, http.MethodGet, "/deployments/logs?id=dep-1&after=20").Body.String()
	if !strings.Contains(body, `<a href="/deployments/logs?after=40&amp;id=dep-1">Newer lines</a>`) {
		t.Errorf("newer link wrong: %s", body)
	}
	if !strings.Contains(body, `<a href="/deployments/logs?id=dep-1">From the start</a>`) {
		t.Error("start link missing for after>0")
	}
	ls.page.Next = nil
	body = opsGet(h, p, http.MethodGet, "/deployments/logs?id=dep-1").Body.String()
	if strings.Contains(body, "Newer lines") || strings.Contains(body, "From the start") {
		t.Error("links present on first/last page")
	}
}

func TestAgentDropMarkerPrefixMatchesShipper(t *testing.T) {
	if agentDropMarkerPrefix != logship.DroppedMarkerPrefix {
		t.Fatalf("web marker prefix %q drifted from shipper %q", agentDropMarkerPrefix, logship.DroppedMarkerPrefix)
	}
}
