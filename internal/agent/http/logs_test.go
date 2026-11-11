package agenthttp

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"autodeploy/internal/server"
	"autodeploy/internal/store/postgres"
)

type fakeLogAppender struct {
	calls                      int
	err                        error
	res                        postgres.AppendLogResult
	gotServer                  string
	gotAttempt, gotFence, gotE int64
	gotChunks                  []postgres.LogChunkInput
}

func (f *fakeLogAppender) AppendLogChunks(_ context.Context, serverID string, _ [32]byte, attemptID, fence, epoch int64, chunks []postgres.LogChunkInput) (postgres.AppendLogResult, error) {
	f.calls++
	f.gotServer, f.gotAttempt, f.gotFence, f.gotE, f.gotChunks = serverID, attemptID, fence, epoch, chunks
	return f.res, f.err
}

func logsBody(chunks ...logChunkRequest) string {
	b, _ := json.Marshal(logsRequest{AttemptID: 4, Fence: 2, WriterEpoch: 9, Chunks: chunks})
	return string(b)
}

func doLogs(t *testing.T, h Handler, body string) (int, string) {
	t.Helper()
	tok, _ := server.NewCredentialToken(rand.Reader)
	w := do(h, newRequestTLS(newRequest(t, pathLogs, body, bearerPrefix+string(tok), "server_1")))
	return w.Code, w.Body.String()
}

func chunk(stream string, seq int64, body string) logChunkRequest {
	return logChunkRequest{Stream: stream, Seq: seq, Body: body}
}

func TestLogsGoodRequestForwardsAndAcks(t *testing.T) {
	f := &fakeLogAppender{res: postgres.AppendLogResult{Accepted: 1, Duplicates: 1, Marker: true}}
	code, body := doLogs(t, Handler{LogAppender: f}, logsBody(chunk("build", 1, "a"), chunk("phase", 2, "b")))
	if code != http.StatusOK || body != `{"accepted":1,"duplicates":1,"conflicts":0,"dropped":0,"marker":true}` {
		t.Fatalf("code=%d body=%s", code, body)
	}
	if f.gotServer != "server_1" || f.gotAttempt != 4 || f.gotFence != 2 || f.gotE != 9 || len(f.gotChunks) != 2 || f.gotChunks[1].Stream != "phase" {
		t.Fatalf("forwarded %+v", f)
	}
}

func TestLogsDenialsAreIndistinguishable(t *testing.T) {
	var first string
	for i, sentinel := range []error{postgres.ErrLogAttemptNotFound, postgres.ErrLogServerMismatch, postgres.ErrLogFenceMismatch, postgres.ErrLogWindowClosed} {
		err := fmt.Errorf("%w: %w", postgres.ErrServerAuthorizationDenied, sentinel)
		code, body := doLogs(t, Handler{LogAppender: &fakeLogAppender{err: err}}, logsBody(chunk("build", 1, "a")))
		if code != http.StatusUnauthorized || strings.Contains(body, "fence") || strings.Contains(body, "window") {
			t.Fatalf("%v: code=%d body=%s", sentinel, code, body)
		}
		if i == 0 {
			first = body
		} else if body != first {
			t.Fatalf("body differs: %s vs %s", body, first)
		}
	}
}

func TestLogsStoreErrorMapping(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"invalid": {postgres.ErrAgentLeaseInvalidInput, http.StatusBadRequest},
		"other":   {errors.New("boom"), http.StatusServiceUnavailable},
	} {
		code, _ := doLogs(t, Handler{LogAppender: &fakeLogAppender{err: tc.err}}, logsBody(chunk("build", 1, "a")))
		if code != tc.want {
			t.Fatalf("%s: code=%d", name, code)
		}
	}
}

func TestLogsBodyCapIsRouteScoped(t *testing.T) {
	f := &fakeLogAppender{}
	h := Handler{LogAppender: f, ProgressReporter: &fakeProgressReporter{}}
	// 20 KiB valid body: accepted here (5 chunks x 4000 bytes).
	var cs []logChunkRequest
	for i := 1; i <= 5; i++ {
		cs = append(cs, chunk("build", int64(i), strings.Repeat("x", 4000)))
	}
	big := logsBody(cs...)
	if len(big) <= MaxBodyBytes {
		t.Fatalf("test body %d not above global cap", len(big))
	}
	if code, body := doLogs(t, h, big); code != http.StatusOK {
		t.Fatalf("20KiB logs: code=%d body=%s", code, body)
	}
	// The same size on another route is still rejected.
	tok, _ := server.NewCredentialToken(rand.Reader)
	pad := `{"attempt_id":1,"fence":1,"phase":"building","x":"` + strings.Repeat("a", len(big)) + `"}`
	if w := do(h, newRequestTLS(newRequest(t, pathProgress, pad, bearerPrefix+string(tok), "server_1"))); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("progress oversize: %d", w.Code)
	}
	// Above the route cap: 413 and the store is untouched.
	calls := f.calls
	over := `{"attempt_id":1,"fence":1,"writer_epoch":1,"chunks":[],"pad":"` + strings.Repeat("a", MaxLogBodyBytes) + `"}`
	if code, _ := doLogs(t, h, over); code != http.StatusRequestEntityTooLarge || f.calls != calls {
		t.Fatalf("over cap: code=%d", code)
	}
}

func TestLogsRejectsMalformedRequests(t *testing.T) {
	tooMany := make([]logChunkRequest, 33)
	for i := range tooMany {
		tooMany[i] = chunk("build", int64(i+1), "a")
	}
	cases := map[string]string{
		"unknown field":  `{"attempt_id":4,"fence":2,"writer_epoch":9,"chunks":[{"stream":"build","seq":1,"body":"a"}],"x":1}`,
		"unknown chunk":  `{"attempt_id":4,"fence":2,"writer_epoch":9,"chunks":[{"stream":"build","seq":1,"body":"a","x":1}]}`,
		"trailing data":  logsBody(chunk("build", 1, "a")) + `{}`,
		"bad stream":     logsBody(chunk("stdout", 1, "a")),
		"too many":       logsBody(tooMany...),
		"no chunks":      logsBody(),
		"zero seq":       logsBody(chunk("build", 0, "a")),
		"empty body":     logsBody(chunk("build", 1, "")),
		"chunk too long": logsBody(chunk("build", 1, strings.Repeat("a", 4097))),
		"zero fence":     `{"attempt_id":4,"fence":0,"writer_epoch":9,"chunks":[{"stream":"build","seq":1,"body":"a"}]}`,
		"zero epoch":     `{"attempt_id":4,"fence":2,"writer_epoch":0,"chunks":[{"stream":"build","seq":1,"body":"a"}]}`,
	}
	for name, body := range cases {
		f := &fakeLogAppender{}
		if code, _ := doLogs(t, Handler{LogAppender: f}, body); code != http.StatusBadRequest || f.calls != 0 {
			t.Fatalf("%s: code=%d calls=%d", name, code, f.calls)
		}
	}
}

func TestLogsSanitizesBeforeStore(t *testing.T) {
	f := &fakeLogAppender{res: postgres.AppendLogResult{Accepted: 2}}
	secret := "ghp_" + strings.Repeat("A", 36)
	code, body := doLogs(t, Handler{LogAppender: f}, logsBody(
		chunk("build", 1, "token "+secret+" nul\x00byte"),
		chunk("build", 2, "\x00\x01\x02"),
		chunk("build", 3, "ok"),
	))
	if code != http.StatusOK || len(f.gotChunks) != 2 {
		t.Fatalf("code=%d chunks=%+v", code, f.gotChunks)
	}
	if f.gotChunks[0].Body != "token [REDACTED] nulbyte" || f.gotChunks[1].Body != "ok" {
		t.Fatalf("bodies=%+v", f.gotChunks)
	}
	if !strings.Contains(body, `"dropped":1`) {
		t.Fatalf("body=%s", body)
	}
	// All chunks empty after sanitizing: 400, store untouched.
	f2 := &fakeLogAppender{}
	if code, body := doLogs(t, Handler{LogAppender: f2}, logsBody(chunk("build", 1, "\x00"))); code != http.StatusBadRequest || f2.calls != 0 {
		t.Fatalf("all-empty: code=%d calls=%d body=%s", code, f2.calls, body)
	}
}

func TestLogsAuthenticationAndDependency(t *testing.T) {
	f := &fakeLogAppender{}
	r := newRequestTLS(newRequest(t, pathLogs, logsBody(chunk("build", 1, "a")), "", ""))
	if w := do(Handler{LogAppender: f}, r); w.Code != http.StatusUnauthorized || f.calls != 0 {
		t.Fatalf("unauthenticated: %d", w.Code)
	}
	if code, _ := doLogs(t, Handler{}, logsBody(chunk("build", 1, "a"))); code != http.StatusServiceUnavailable {
		t.Fatalf("nil dependency: %d", code)
	}
}
