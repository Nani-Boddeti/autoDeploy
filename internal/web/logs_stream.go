package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"autodeploy/internal/auth"
	"autodeploy/internal/store/postgres"
)

// Live log streaming (22-3k): GET /deployments/logs/stream is a Server-Sent
// Events feed of one deployment's log chunks. It is progressive enhancement for
// /deployments/logs: the page keeps its <noscript> meta refresh for browsers
// without JavaScript.
//
// Safety properties:
//   - every tick calls ListDeploymentLogs, which re-validates the live session,
//     so a revoked or expired session ends the stream within one tick;
//   - the http.Server WriteTimeout is never changed: each write extends its own
//     short deadline through http.ResponseController;
//   - concurrency is capped per session and per process, and every stream is
//     bounded by streamMaxDuration (the client reconnects with Last-Event-ID);
//   - line bodies and error text never reach the process log.
//
// Late-commit property: the cursor is the chunk id. Appends serialize per
// attempt, but a late append to an already-closed attempt can commit with an id
// lower than lines already streamed for a newer attempt, so the stream cursor
// skips it. The client therefore reloads once on `end` reason "done"; the
// reloaded terminal page renders from its own start cursor (and carries no
// script, so there is no reload loop) and shows any such late row.
const (
	defaultStreamTick          = 2 * time.Second
	defaultStreamHeartbeat     = 15 * time.Second
	defaultStreamMaxDuration   = 15 * time.Minute
	defaultStreamTerminalCheck = 5 * time.Second
	streamWriteDeadline        = 10 * time.Second
	streamPagesPerTick         = 10
	defaultStreamPerSession    = 3
	defaultStreamTotal         = 16
	streamRetryAfterSeconds    = "5"
	streamRetryMillis          = "3000"
)

// streamLimiter counts live streams, process-wide and per session. The zero
// value is ready to use.
type streamLimiter struct {
	mu         sync.Mutex
	total      int
	perSession map[string]int
}

// acquire reserves a slot; it reports false at either cap.
func (l *streamLimiter) acquire(sessionID string, maxPerSession, maxTotal int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.total >= maxTotal || l.perSession[sessionID] >= maxPerSession {
		return false
	}
	if l.perSession == nil {
		l.perSession = map[string]int{}
	}
	l.total++
	l.perSession[sessionID]++
	return true
}

func (l *streamLimiter) release(sessionID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.total--
	if l.perSession[sessionID] <= 1 {
		delete(l.perSession, sessionID)
		return
	}
	l.perSession[sessionID]--
}

// activeStreams is the number of streams currently open (tests and diagnostics).
func (h *Handler) activeStreams() int {
	h.streams.mu.Lock()
	defer h.streams.mu.Unlock()
	return h.streams.total
}

// streamTiming resolves the injectable intervals; zero fields use production values.
type streamTiming struct {
	tick, heartbeat, maxDuration, terminalCheck time.Duration
	perSession, total                           int
}

func (h *Handler) streamTiming() streamTiming {
	pick := func(v, def time.Duration) time.Duration {
		if v > 0 {
			return v
		}
		return def
	}
	pickInt := func(v, def int) int {
		if v > 0 {
			return v
		}
		return def
	}
	return streamTiming{
		tick:          pick(h.streamTick, defaultStreamTick),
		heartbeat:     pick(h.streamHeartbeat, defaultStreamHeartbeat),
		maxDuration:   pick(h.streamMaxDuration, defaultStreamMaxDuration),
		terminalCheck: pick(h.streamTerminalCheck, defaultStreamTerminalCheck),
		perSession:    pickInt(h.streamMaxPerSession, defaultStreamPerSession),
		total:         pickInt(h.streamMaxTotal, defaultStreamTotal),
	}
}

// streamLineData is the JSON payload of a `line` event. It carries the same
// fields the server-rendered row shows; the body is stored already redacted.
type streamLineData struct {
	Attempt int    `json:"attempt"`
	Stream  string `json:"stream"`
	Label   string `json:"label"`
	Clock   string `json:"clock"`
	Stamp   string `json:"stamp"`
	Body    string `json:"body"`
	Marker  bool   `json:"marker"`
}

// deploymentLogStream serves GET /deployments/logs/stream?id=&after=.
func (h *Handler) deploymentLogStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	_, p, ok := h.session(r.Context(), r)
	if !ok {
		redirect(w, "/login")
		return
	}
	q := r.URL.Query()
	if !onlyParams(q, "id", "after") {
		http.NotFound(w, r)
		return
	}
	id, single := singleParam(q, "id")
	if _, hasID := q["id"]; !hasID || !single || !validOperationsID(id) {
		http.NotFound(w, r)
		return
	}
	rawAfter, single := singleParam(q, "after")
	if !single {
		badRequest(w)
		return
	}
	var after int64
	if _, hasAfter := q["after"]; hasAfter {
		n, valid := parseChunkCursor(rawAfter)
		if !valid {
			badRequest(w)
			return
		}
		after = n
	}
	// EventSource resends the last event id on reconnect; the larger valid
	// cursor wins, and a malformed header is ignored rather than rejected.
	if n, valid := parseChunkCursor(r.Header.Get("Last-Event-ID")); valid && n > after {
		after = n
	}

	timing := h.streamTiming()
	if !h.streams.acquire(p.SessionID, timing.perSession, timing.total) {
		w.Header().Set("Retry-After", streamRetryAfterSeconds)
		plain(w, http.StatusTooManyRequests, "too many live log streams; retry shortly\n")
		return
	}
	defer h.streams.release(p.SessionID)

	rc := http.NewResponseController(w)
	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	s := &logStream{h: h, w: w, rc: rc, p: p, id: id, after: after, timing: timing, lastWrite: time.Now()}
	// Reconnect delay hint, flushed so the client sees headers at once.
	if s.write("retry: "+streamRetryMillis+"\n\n") != nil {
		return
	}
	s.run(r.Context())
}

type logStream struct {
	h         *Handler
	w         http.ResponseWriter
	rc        *http.ResponseController
	p         auth.Principal
	id        string
	after     int64
	timing    streamTiming
	lastWrite time.Time
	lastCheck time.Time
}

// write sends one chunk with a fresh write deadline and flushes it. The
// server-wide WriteTimeout (20s) would otherwise cut every stream; extending
// the deadline per write keeps it bounded to a stuck client instead.
func (s *logStream) write(chunk string) error {
	_ = s.rc.SetWriteDeadline(time.Now().Add(streamWriteDeadline))
	if _, err := io.WriteString(s.w, chunk); err != nil {
		return err
	}
	if err := s.rc.Flush(); err != nil {
		return err
	}
	s.lastWrite = time.Now()
	return nil
}

func (s *logStream) end(reason string) {
	_ = s.write("event: end\ndata: {\"reason\":\"" + reason + "\"}\n\n")
}

// run polls until the stream ends: client gone, rotation, terminal job, or an error.
func (s *logStream) run(ctx context.Context) {
	rotate := time.NewTimer(s.timing.maxDuration)
	defer rotate.Stop()
	tick := time.NewTicker(s.timing.tick)
	defer tick.Stop()
	for {
		if !s.poll(ctx) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-rotate.C:
			s.end("rotate")
			return
		case <-tick.C:
		}
	}
}

// poll runs one tick and reports whether the stream continues.
func (s *logStream) poll(ctx context.Context) bool {
	got, reason, err := s.drain(ctx)
	if err != nil {
		return false // write failed or client gone
	}
	if reason != "" {
		s.end(reason)
		return false
	}
	if got {
		return true
	}
	// Nothing new: check whether the job can still produce lines. Rate-limited
	// so an idle running job costs one detail read per terminalCheck.
	if now := time.Now(); s.lastCheck.IsZero() || now.Sub(s.lastCheck) >= s.timing.terminalCheck {
		s.lastCheck = now
		if s.jobTerminal(ctx) {
			// A chunk may have committed between the empty read and the state
			// read; one more drain makes "done" mean nothing was left behind.
			got, reason, err = s.drain(ctx)
			if err != nil {
				return false
			}
			if reason != "" {
				s.end(reason)
				return false
			}
			if !got {
				s.end("done")
				return false
			}
			return true
		}
	}
	if time.Since(s.lastWrite) >= s.timing.heartbeat {
		if s.write(": ping\n\n") != nil {
			return false
		}
	}
	return ctx.Err() == nil
}

// jobTerminal reports whether the deployment's job is in a terminal state. It is
// best effort: any failure means "not known terminal", and the next list call
// reports an auth or not-found problem itself.
func (s *logStream) jobTerminal(ctx context.Context) bool {
	d, err := s.h.config.OperationsReader.GetDeploymentDetail(ctx, s.p, s.id)
	return err == nil && terminalJobStates[d.Job.State]
}

// drain reads and sends every available line (bounded pages per tick). It
// returns whether any line was sent, an end reason when the stream must stop,
// and err when the client is gone or a write failed.
func (s *logStream) drain(ctx context.Context) (got bool, reason string, err error) {
	for pages := 0; pages < streamPagesPerTick; pages++ {
		page, lerr := s.h.config.DeploymentLogReader.ListDeploymentLogs(ctx, s.p, s.id, s.after, 0)
		if lerr != nil {
			if ctx.Err() != nil {
				return got, "", ctx.Err()
			}
			return got, s.reasonFor(lerr), nil
		}
		for _, l := range page.Items {
			if werr := s.writeLine(l); werr != nil {
				return got, "", werr
			}
			s.after = l.ID
			got = true
		}
		if page.Next == nil || len(page.Items) == 0 {
			break
		}
	}
	return got, "", nil
}

// reasonFor maps a store error to a fixed end reason; error text is never sent.
func (s *logStream) reasonFor(err error) string {
	switch {
	case errors.Is(err, postgres.ErrServerAuthorizationDenied):
		return "session"
	case errors.Is(err, postgres.ErrOperationsNotFound), errors.Is(err, postgres.ErrOperationsInvalidInput):
		return "notfound"
	}
	s.h.logger().Debug("log stream: store read failed", "deployment_id", s.id)
	return "error"
}

func (s *logStream) writeLine(l postgres.DeploymentLogLine) error {
	data, err := json.Marshal(streamLineData{
		Attempt: l.AttemptNumber,
		Stream:  logStreamClass(l.Stream),
		Label:   logStreamClass(l.Stream),
		Clock:   l.CreatedAt.UTC().Format("15:04:05.000"),
		Stamp:   l.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		Body:    l.Body,
		Marker:  isLogMarker(l.Stream, l.Body),
	})
	if err != nil {
		return err
	}
	// json.Marshal escapes newlines, so the payload is always one data line.
	return s.write("id: " + strconv.FormatInt(l.ID, 10) + "\nevent: line\ndata: " + string(data) + "\n\n")
}
