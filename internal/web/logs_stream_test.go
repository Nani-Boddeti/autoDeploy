package web

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"autodeploy/internal/auth"
	"autodeploy/internal/store/postgres"
)

// streamStub is a concurrency-safe DeploymentLogReader for the SSE tests.
type streamStub struct {
	mu     sync.Mutex
	lines  []postgres.DeploymentLogLine
	err    error
	afters []int64
}

func (s *streamStub) ListDeploymentLogs(_ context.Context, _ auth.Principal, _ string, after int64, _ int) (postgres.DeploymentLogPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.afters = append(s.afters, after)
	if s.err != nil {
		return postgres.DeploymentLogPage{}, s.err
	}
	var page postgres.DeploymentLogPage
	for _, l := range s.lines {
		if l.ID > after {
			page.Items = append(page.Items, l)
		}
	}
	return page, nil
}

func (s *streamStub) push(lines ...postgres.DeploymentLogLine) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines = append(s.lines, lines...)
}

func (s *streamStub) setErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func (s *streamStub) firstAfter() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.afters) == 0 {
		return -1
	}
	return s.afters[0]
}

// streamOps overrides GetDeploymentDetail with a race-free job state.
type streamOps struct {
	*operationsStub
	mu    sync.Mutex
	state string
}

func (s *streamOps) GetDeploymentDetail(context.Context, auth.Principal, string) (postgres.DeploymentDetail, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return sampleDetail(s.state, false), nil
}

func (s *streamOps) setState(state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = state
}

type streamEnv struct {
	h   *Handler
	ls  *streamStub
	ops *streamOps
	p   auth.Principal
}

func newStreamEnv(t *testing.T) *streamEnv {
	t.Helper()
	h, _, p := serversHandler(t)
	env := &streamEnv{h: h, ls: &streamStub{}, ops: &streamOps{operationsStub: &operationsStub{}, state: "leased"}, p: p}
	h.config.OperationsReader = env.ops
	h.config.DeploymentLogReader = env.ls
	h.streamTick = 10 * time.Millisecond
	h.streamHeartbeat = time.Hour
	h.streamMaxDuration = time.Minute
	h.streamTerminalCheck = 20 * time.Millisecond
	return env
}

func (e *streamEnv) start(t *testing.T, mutate func(*httptest.Server)) *httptest.Server {
	t.Helper()
	ts := httptest.NewUnstartedServer(e.h)
	if mutate != nil {
		mutate(ts)
	}
	ts.Start()
	t.Cleanup(ts.Close)
	return ts
}

type sseEvent struct {
	ID, Event, Data string
	Comment         bool
	DataLines       int
}

type sseStream struct {
	resp   *http.Response
	events chan sseEvent
	cancel context.CancelFunc
}

func openStream(t *testing.T, ts *httptest.Server, target string, headers map[string]string) (*sseStream, int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+target, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "v1.AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"})
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		cancel()
		t.Fatalf("open %s: %v", target, err)
	}
	s := &sseStream{resp: resp, events: make(chan sseEvent, 256), cancel: cancel}
	t.Cleanup(s.close)
	if resp.StatusCode != http.StatusOK {
		return s, resp.StatusCode
	}
	go func() {
		defer close(s.events)
		sc := bufio.NewScanner(resp.Body)
		var ev sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if ev.ID != "" || ev.Event != "" || ev.DataLines > 0 || ev.Comment {
					s.events <- ev
				}
				ev = sseEvent{}
			case strings.HasPrefix(line, ":"):
				ev.Comment = true
			case strings.HasPrefix(line, "id: "):
				ev.ID = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				ev.Event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.Data = strings.TrimPrefix(line, "data: ")
				ev.DataLines++
			}
		}
	}()
	return s, resp.StatusCode
}

func (s *sseStream) close() {
	s.cancel()
	_ = s.resp.Body.Close()
}

// next returns the next non-comment event, or fails after the timeout.
func (s *sseStream) next(t *testing.T, timeout time.Duration) sseEvent {
	t.Helper()
	for {
		ev, ok := s.nextAny(t, timeout)
		if !ok {
			t.Fatal("stream closed before the next event")
		}
		if !ev.Comment {
			return ev
		}
	}
}

func (s *sseStream) nextAny(t *testing.T, timeout time.Duration) (sseEvent, bool) {
	t.Helper()
	select {
	case ev, ok := <-s.events:
		return ev, ok
	case <-time.After(timeout):
		t.Fatalf("no event within %s", timeout)
		return sseEvent{}, false
	}
}

func (s *sseStream) expectEnd(t *testing.T, reason string) {
	t.Helper()
	ev := s.next(t, 5*time.Second)
	if ev.Event != "end" || ev.Data != `{"reason":"`+reason+`"}` {
		t.Fatalf("event = %+v, want end %s", ev, reason)
	}
}

type streamLineJSON struct {
	Attempt int    `json:"attempt"`
	Stream  string `json:"stream"`
	Label   string `json:"label"`
	Clock   string `json:"clock"`
	Stamp   string `json:"stamp"`
	Body    string `json:"body"`
	Marker  bool   `json:"marker"`
}

func decodeLine(t *testing.T, ev sseEvent) streamLineJSON {
	t.Helper()
	if ev.Event != "line" {
		t.Fatalf("event = %+v, want line", ev)
	}
	var l streamLineJSON
	if err := json.Unmarshal([]byte(ev.Data), &l); err != nil {
		t.Fatalf("data %q: %v", ev.Data, err)
	}
	return l
}

func waitStreams(t *testing.T, h *Handler, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if h.activeStreams() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("active streams = %d, want %d", h.activeStreams(), want)
}

const streamPath = "/deployments/logs/stream?id=dep-1"

func TestStreamLinesInOrderWithIDsAndJSON(t *testing.T) {
	e := newStreamEnv(t)
	e.ls.push(line(1, 1, "build", "first"), line(2, 1, "phase", "second"), line(3, 2, "system", postgres.LogTruncationMarker))
	ts := e.start(t, nil)
	s, code := openStream(t, ts, streamPath, nil)
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	h := s.resp.Header
	if ct := h.Get("Content-Type"); ct != "text/event-stream" || h.Get("Cache-Control") != "no-store" || h.Get("X-Accel-Buffering") != "no" {
		t.Fatalf("headers: %v", h)
	}
	for i, want := range []struct {
		id, stream, body string
		attempt          int
		marker           bool
	}{{"1", "build", "first", 1, false}, {"2", "phase", "second", 1, false}, {"3", "system", postgres.LogTruncationMarker, 2, true}} {
		ev := s.next(t, 3*time.Second)
		l := decodeLine(t, ev)
		if ev.ID != want.id || l.Stream != want.stream || l.Label != want.stream || l.Body != want.body || l.Attempt != want.attempt || l.Marker != want.marker {
			t.Fatalf("event %d = %+v %+v", i, ev, l)
		}
		if l.Clock != "10:00:00.123" || l.Stamp != "2026-11-11T10:00:00.123Z" {
			t.Fatalf("time fields = %q %q", l.Clock, l.Stamp)
		}
	}
	e.ls.push(line(4, 2, "evil<x>", "late"))
	ev := s.next(t, 3*time.Second)
	if l := decodeLine(t, ev); ev.ID != "4" || l.Stream != "other" {
		t.Fatalf("late line = %+v %+v", ev, l)
	}
}

func TestStreamResumeCursor(t *testing.T) {
	cases := []struct {
		name, target, lastEventID string
		want                      int64
	}{
		{"query only", streamPath + "&after=1", "", 1},
		{"header only", streamPath, "2", 2},
		{"header wins", streamPath + "&after=1", "2", 2},
		{"query wins", streamPath + "&after=3", "1", 3},
		{"invalid header ignored", streamPath + "&after=1", "abc", 1},
		{"negative header ignored", streamPath, "-5", 0},
		{"leading zero header ignored", streamPath, "02", 0},
		{"no cursor", streamPath, "", 0},
	}
	for _, c := range cases {
		e := newStreamEnv(t)
		e.ls.push(line(1, 1, "build", "a"), line(2, 1, "build", "b"), line(3, 1, "build", "c"), line(4, 1, "build", "d"))
		ts := e.start(t, nil)
		headers := map[string]string{}
		if c.lastEventID != "" {
			headers["Last-Event-ID"] = c.lastEventID
		}
		s, code := openStream(t, ts, c.target, headers)
		if code != 200 {
			t.Fatalf("%s: status %d", c.name, code)
		}
		ev := s.next(t, 3*time.Second)
		if ev.ID != strconv.FormatInt(c.want+1, 10) {
			t.Errorf("%s: first id %s, want %d", c.name, ev.ID, c.want+1)
		}
		if got := e.ls.firstAfter(); got != c.want {
			t.Errorf("%s: first store cursor %d, want %d", c.name, got, c.want)
		}
	}
}

func TestStreamEndsWhenJobTerminalAndDrained(t *testing.T) {
	e := newStreamEnv(t)
	e.ops.setState("succeeded")
	e.ls.push(line(1, 1, "build", "only"))
	s, _ := openStream(t, e.start(t, nil), streamPath, nil)
	if ev := s.next(t, 3*time.Second); ev.ID != "1" {
		t.Fatalf("first event %+v", ev)
	}
	s.expectEnd(t, "done")

	// Empty and terminal from the start: end immediately, no lines.
	e2 := newStreamEnv(t)
	e2.ops.setState("failed")
	s2, _ := openStream(t, e2.start(t, nil), streamPath, nil)
	s2.expectEnd(t, "done")
}

func TestStreamStaysOpenWhileJobRuns(t *testing.T) {
	e := newStreamEnv(t)
	s, _ := openStream(t, e.start(t, nil), streamPath, nil)
	select {
	case ev := <-s.events:
		t.Fatalf("unexpected event on a running job: %+v", ev)
	case <-time.After(300 * time.Millisecond):
	}
	e.ls.push(line(1, 1, "build", "x"))
	if ev := s.next(t, 3*time.Second); ev.ID != "1" {
		t.Fatalf("event %+v", ev)
	}
}

func TestStreamEndReasonsOnStoreErrors(t *testing.T) {
	// Session revoked mid-stream.
	e := newStreamEnv(t)
	e.ls.push(line(1, 1, "build", "x"))
	s, _ := openStream(t, e.start(t, nil), streamPath, nil)
	s.next(t, 3*time.Second)
	e.ls.setErr(postgres.ErrServerAuthorizationDenied)
	s.expectEnd(t, "session")

	// Foreign or unknown deployment at the start leaks no lines.
	for _, notFound := range []error{postgres.ErrOperationsNotFound, postgres.ErrOperationsInvalidInput} {
		e := newStreamEnv(t)
		e.ls.push(line(1, 1, "build", "secret-line"))
		e.ls.setErr(notFound)
		s, _ := openStream(t, e.start(t, nil), streamPath, nil)
		s.expectEnd(t, "notfound")
	}

	// Any other error is generic and never echoes the error text.
	e = newStreamEnv(t)
	e.ls.setErr(errors.New("dial tcp 10.0.0.1: password=hunter2"))
	s, _ = openStream(t, e.start(t, nil), streamPath, nil)
	ev := s.next(t, 3*time.Second)
	if ev.Event != "end" || ev.Data != `{"reason":"error"}` || strings.Contains(ev.Data, "hunter2") {
		t.Fatalf("event %+v", ev)
	}
}

func TestStreamPerSessionAndProcessCaps(t *testing.T) {
	e := newStreamEnv(t)
	ts := e.start(t, nil)
	var open []*sseStream
	for i := 0; i < 3; i++ {
		s, code := openStream(t, ts, streamPath, nil)
		if code != 200 {
			t.Fatalf("stream %d = %d", i, code)
		}
		open = append(open, s)
	}
	s4, code := openStream(t, ts, streamPath, nil)
	if code != http.StatusTooManyRequests || s4.resp.Header.Get("Retry-After") != "5" {
		t.Fatalf("4th stream = %d retry-after %q", code, s4.resp.Header.Get("Retry-After"))
	}
	if ct := s4.resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("429 carried streaming headers: %q", ct)
	}
	open[0].close()
	waitStreams(t, e.h, 2)
	if _, code := openStream(t, ts, streamPath, nil); code != 200 {
		t.Fatalf("stream after release = %d", code)
	}

	// Process-wide cap.
	e2 := newStreamEnv(t)
	e2.h.streamMaxPerSession = 100
	e2.h.streamMaxTotal = 2
	ts2 := e2.start(t, nil)
	for i := 0; i < 2; i++ {
		if _, code := openStream(t, ts2, streamPath, nil); code != 200 {
			t.Fatalf("stream %d = %d", i, code)
		}
	}
	s3, code := openStream(t, ts2, streamPath, nil)
	if code != http.StatusTooManyRequests || s3.resp.Header.Get("Retry-After") != "5" {
		t.Fatalf("process cap = %d", code)
	}
}

func TestStreamCountersReleasedAfterManyOpenClose(t *testing.T) {
	e := newStreamEnv(t)
	e.ops.setState("failed") // each stream ends itself with done
	ts := e.start(t, nil)
	for i := 0; i < 25; i++ {
		s, code := openStream(t, ts, streamPath, nil)
		if code != 200 {
			t.Fatalf("iteration %d = %d", i, code)
		}
		s.expectEnd(t, "done")
		s.close()
	}
	// And abrupt client disconnects.
	e.ops.setState("leased")
	for i := 0; i < 25; i++ {
		s, code := openStream(t, ts, streamPath, nil)
		if code != 200 {
			t.Fatalf("disconnect iteration %d = %d", i, code)
		}
		s.close()
		waitStreams(t, e.h, 0)
	}
	waitStreams(t, e.h, 0)
	e.h.streams.mu.Lock()
	defer e.h.streams.mu.Unlock()
	if len(e.h.streams.perSession) != 0 {
		t.Fatalf("per-session map leaked: %v", e.h.streams.perSession)
	}
}

// TestStreamOutlivesServerWriteTimeout runs the server with a 2s WriteTimeout
// and proves the per-write deadline extension keeps a 5s stream alive.
func TestStreamOutlivesServerWriteTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("5s stream")
	}
	e := newStreamEnv(t)
	e.h.streamTick = 50 * time.Millisecond
	e.h.streamHeartbeat = 400 * time.Millisecond
	ts := e.start(t, func(ts *httptest.Server) { ts.Config.WriteTimeout = 2 * time.Second })
	s, _ := openStream(t, ts, streamPath, nil)
	start := time.Now()
	id := int64(0)
	var lateLines int
	for time.Since(start) < 5*time.Second {
		id++
		e.ls.push(line(id, 1, "build", "tick"))
		time.Sleep(500 * time.Millisecond)
	}
	// Drain what arrived; lines after the 4s mark prove the deadline moved.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-s.events:
			if !ok {
				t.Fatalf("stream closed by the server WriteTimeout after %d late lines", lateLines)
			}
			if ev.Event == "line" {
				if n, _ := strconv.ParseInt(ev.ID, 10, 64); n >= 9 {
					lateLines++
				}
			}
			if lateLines > 0 {
				return
			}
		case <-deadline:
			t.Fatal("no line received after the 4s mark")
		}
	}
}

func TestStreamHeartbeatAndRotation(t *testing.T) {
	e := newStreamEnv(t)
	e.h.streamHeartbeat = 40 * time.Millisecond
	s, _ := openStream(t, e.start(t, nil), streamPath, nil)
	ev, _ := s.nextAny(t, 3*time.Second)
	if !ev.Comment {
		// The retry preamble is also a comment-less block; skip once.
		ev, _ = s.nextAny(t, 3*time.Second)
	}
	if !ev.Comment {
		t.Fatalf("expected heartbeat comment, got %+v", ev)
	}

	e2 := newStreamEnv(t)
	e2.h.streamMaxDuration = 150 * time.Millisecond
	s2, _ := openStream(t, e2.start(t, nil), streamPath, nil)
	s2.expectEnd(t, "rotate")
}

func TestStreamBodyIsJSONEscapedOnOneDataLine(t *testing.T) {
	e := newStreamEnv(t)
	body := "line1\nline2 <script>alert(1)</script> &   end"
	e.ls.push(line(1, 1, "build", body))
	s, _ := openStream(t, e.start(t, nil), streamPath, nil)
	ev := s.next(t, 3*time.Second)
	if ev.DataLines != 1 || strings.ContainsAny(ev.Data, "\n\r") || strings.Contains(ev.Data, "<script>") {
		t.Fatalf("data not single-line escaped: %+v", ev)
	}
	if got := decodeLine(t, ev).Body; got != body {
		t.Fatalf("body round trip = %q", got)
	}
}

func TestStreamDisconnectStopsHandler(t *testing.T) {
	e := newStreamEnv(t)
	ts := e.start(t, nil)
	s, _ := openStream(t, ts, streamPath, nil)
	waitStreams(t, e.h, 1)
	start := time.Now()
	s.close()
	waitStreams(t, e.h, 0)
	if time.Since(start) > 2*time.Second {
		t.Fatalf("handler took %s to stop", time.Since(start))
	}
}

func TestStreamRouteGuards(t *testing.T) {
	e := newStreamEnv(t)
	h, p := e.h, e.p
	for _, target := range []string{
		"/deployments/logs/stream", "/deployments/logs/stream?id=", "/deployments/logs/stream?id=a&id=b",
		"/deployments/logs/stream?id=dep-1&x=1", "/deployments/logs/stream?id=dep-1&live=1", "/deployments/logs/stream?after=3",
		"/deployments/logs/stream?id=a%00b",
	} {
		if rr := opsGet(h, p, http.MethodGet, target); rr.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", target, rr.Code)
		}
	}
	for _, after := range []string{"", "-1", "+1", "abc", "01", "9223372036854775808"} {
		if rr := opsGet(h, p, http.MethodGet, streamPath+"&after="+after); rr.Code != http.StatusBadRequest {
			t.Errorf("after=%q = %d, want 400", after, rr.Code)
		}
	}
	if rr := opsGet(h, p, http.MethodGet, streamPath+"&after=1&after=2"); rr.Code != http.StatusBadRequest {
		t.Errorf("repeated after = %d", rr.Code)
	}
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodHead} {
		rr := opsGet(h, p, m, streamPath)
		if rr.Code != http.StatusMethodNotAllowed || rr.Header().Get("Allow") != "GET" {
			t.Errorf("%s = %d allow=%q", m, rr.Code, rr.Header().Get("Allow"))
		}
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "https://admin.example.test"+streamPath, nil))
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Errorf("no session = %d %q", rr.Code, rr.Header().Get("Location"))
	}
	h.config.DeploymentLogReader = nil
	if rr := opsGet(h, p, http.MethodGet, streamPath); rr.Code != http.StatusNotFound {
		t.Errorf("nil log reader = %d", rr.Code)
	}
	h.config.DeploymentLogReader = e.ls
	h.config.OperationsReader = nil
	if rr := opsGet(h, p, http.MethodGet, streamPath); rr.Code != http.StatusNotFound {
		t.Errorf("nil operations reader = %d", rr.Code)
	}
}

// ---- logs page wiring ----

func TestLogsPageStreamWiring(t *testing.T) {
	const (
		script   = `<script src="/static/logs.js" defer></script>`
		noscript = `<noscript><meta http-equiv="refresh" content="3"></noscript>`
	)
	page := postgres.DeploymentLogPage{Items: []postgres.DeploymentLogLine{line(7, 1, "build", "x")}}
	cases := []struct {
		name   string
		target string
		state  string
		next   *int64
		noOps  bool
		stream bool
	}{
		{"explicit id, live, last page", "/deployments/logs?id=dep-1&after=5", "leased", nil, false, true},
		{"terminal", "/deployments/logs?id=dep-1&after=5", "succeeded", nil, false, false},
		{"not last page", "/deployments/logs?id=dep-1&after=5", "leased", i64(7), false, false},
		{"no operations reader", "/deployments/logs?id=dep-1&after=5", "leased", nil, true, false},
		{"no id", "/deployments/logs", "leased", nil, false, false},
	}
	for _, c := range cases {
		h, ls, ops, p := logsPickerHandler(t, nDeployments(1))
		if c.target == "/deployments/logs?id=dep-1&after=5" {
			h, ls, ops, p = logsHandler(t)
		}
		ops.detail = sampleDetail(c.state, false)
		if c.noOps {
			h.config.OperationsReader = nil
		}
		ls.page = page
		ls.page.Next = c.next
		body := opsGet(h, p, http.MethodGet, c.target).Body.String()
		for _, frag := range []string{script, noscript, `data-stream-url=`} {
			if got := strings.Contains(body, frag); got != c.stream {
				t.Errorf("%s: contains %q = %v, want %v", c.name, frag, got, c.stream)
			}
		}
		if c.stream && !strings.Contains(body, `data-stream-url="/deployments/logs/stream?id=dep-1&amp;after=7"`) {
			t.Errorf("%s: stream url wrong: %s", c.name, body)
		}
		// The meta refresh must only ever appear inside <noscript>.
		if c.stream && strings.Count(body, `http-equiv="refresh"`) != 1 {
			t.Errorf("%s: refresh not unique", c.name)
		}
	}
}

func TestLogsPageStreamURLUsesRequestCursorWhenEmpty(t *testing.T) {
	h, _, _, p := logsHandler(t)
	body := opsGet(h, p, http.MethodGet, "/deployments/logs?id=dep-1&after=9").Body.String()
	if !strings.Contains(body, `data-stream-url="/deployments/logs/stream?id=dep-1&amp;after=9"`) {
		t.Fatalf("empty page lost the cursor: %s", body)
	}
}

// ---- static asset ----

func TestLogsScriptServedAndFreeOfDangerousAPIs(t *testing.T) {
	h, _, _ := serversHandler(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "https://admin.example.test/static/logs.js", nil))
	if rr.Code != 200 || rr.Header().Get("Content-Type") != "text/javascript; charset=utf-8" || rr.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("served %d %v", rr.Code, rr.Header())
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "https://admin.example.test/static/logs.js?v=1", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("query on asset = %d", rr.Code)
	}
	src, err := os.ReadFile("static/logs.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range bannedScriptAPIs {
		if strings.Contains(string(src), banned) {
			t.Errorf("logs.js contains banned API %q", banned)
		}
	}
	if !strings.Contains(string(src), "'use strict'") {
		t.Error("logs.js must be strict mode")
	}
	if n := strings.Count(string(src), "\n"); n > 175 {
		t.Errorf("logs.js has %d lines, want <= 175", n)
	}
}

var bannedScriptAPIs = []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function", "Function(", `setTimeout("`, `setTimeout('`, "setInterval(\"", "setInterval('"}

func TestStreamProductionDefaults(t *testing.T) {
	h, _, _ := serversHandler(t)
	got := h.streamTiming()
	if got.tick != 2*time.Second || got.heartbeat != 15*time.Second || got.maxDuration != 15*time.Minute || got.perSession != 3 || got.total != 16 {
		t.Fatalf("defaults = %+v", got)
	}
}

// multiSessionRepo validates two distinct session tokens of the same owner.
type multiSessionRepo struct {
	*repositoryStub
	byToken map[string]auth.Principal
}

func (r *multiSessionRepo) ValidateSession(_ context.Context, token auth.SessionToken) (auth.Principal, error) {
	if p, ok := r.byToken[token.String()]; ok {
		return p, nil
	}
	return auth.Principal{}, postgres.ErrUnauthenticated
}

const (
	sessionCookieA = "v1.AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"
	sessionCookieB = "v1.AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI"
)

func TestStreamCapIsPerSessionNotPerOwner(t *testing.T) {
	e := newStreamEnv(t)
	pa, pb := e.p, e.p
	pb.SessionID = "session-b"
	e.h.repository = &multiSessionRepo{repositoryStub: &repositoryStub{}, byToken: map[string]auth.Principal{sessionCookieA: pa, sessionCookieB: pb}}
	ts := e.start(t, nil)
	cookie := func(v string) map[string]string {
		return map[string]string{"Cookie": sessionCookieName + "=" + v}
	}
	for _, c := range []string{sessionCookieA, sessionCookieB} {
		for i := 0; i < 3; i++ {
			if _, code := openStream(t, ts, streamPath, cookie(c)); code != 200 {
				t.Fatalf("session %s stream %d = %d", c[:8], i, code)
			}
		}
	}
	for _, c := range []string{sessionCookieA, sessionCookieB} {
		if _, code := openStream(t, ts, streamPath, cookie(c)); code != http.StatusTooManyRequests {
			t.Fatalf("4th stream for %s = %d, want 429", c[:8], code)
		}
	}
}

func TestLogsScriptReloadsOnlyOnDoneAndMessagesClosedErrors(t *testing.T) {
	src, err := os.ReadFile("static/logs.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(src)
	if strings.Count(js, "location.reload()") != 3 { // no-EventSource fallback, reload button, done
		t.Errorf("unexpected reload call count %d", strings.Count(js, "location.reload()"))
	}
	i := strings.Index(js, "if (reason === 'done') { window.location.reload(); }")
	if i < 0 {
		t.Fatal("reload on done missing")
	}
	// Every other reload is the fallback or the explicit button, never the stream end path.
	for _, frag := range []string{"retry.addEventListener('click', function () { window.location.reload(); })", "window.setTimeout(function () { window.location.reload(); }, 3000)"} {
		if !strings.Contains(js, frag) {
			t.Errorf("missing %q", frag)
		}
	}
	if !strings.Contains(js, "Live updates unavailable") || !strings.Contains(js, "finish('Ended', reason === 'session', false)") || strings.Contains(js, "finish('Ended', true)") {
		t.Error("sign-in link must be tied to the session end reason only")
	}
}
