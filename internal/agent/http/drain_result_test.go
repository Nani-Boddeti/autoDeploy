package agenthttp

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"autodeploy/internal/server"
	"autodeploy/internal/store/postgres"
)

type fakeDrainRecorder struct {
	err                     error
	calls                   int
	attempt                 int64
	pred, rt, outcome, summ string
}

func (f *fakeDrainRecorder) RecordDrainResult(_ context.Context, _ string, _ [32]byte, attemptID int64, pred, rt, outcome, summary string) error {
	f.calls++
	f.attempt, f.pred, f.rt, f.outcome, f.summ = attemptID, pred, rt, outcome, summary
	return f.err
}

func drainBody(outcome string) string {
	return fmt.Sprintf(`{"attempt_id":7,"predecessor_release_id":"rel_old","predecessor_runtime_instance_id":"41","outcome":%q,"summary":"x"}`, outcome)
}

func postDrain(t *testing.T, h Handler, body string) (int, string) {
	t.Helper()
	tok, _ := server.NewCredentialToken(rand.Reader)
	w := do(h, newRequestTLS(newRequest(t, pathDrainResult, body, bearerPrefix+string(tok), "server_1")))
	var got errorBody
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	return w.Code, got.Error
}

func TestDrainResultAcceptsEveryOutcome(t *testing.T) {
	for _, o := range []string{"stopped", "already_stopped", "gone", "stop_failed", "route_moved", "identity_error"} {
		fr := &fakeDrainRecorder{}
		if code, _ := postDrain(t, Handler{DrainRecorder: fr}, drainBody(o)); code != http.StatusOK || fr.calls != 1 {
			t.Fatalf("%s: status=%d calls=%d", o, code, fr.calls)
		}
		if fr.attempt != 7 || fr.pred != "rel_old" || fr.rt != "41" || fr.outcome != o || fr.summ != "x" {
			t.Fatalf("%s: recorder saw %+v", o, fr)
		}
	}
}

func TestDrainResultRejectsMalformed(t *testing.T) {
	for name, body := range map[string]string{
		"unknown outcome":  drainBody("restarted"),
		"empty outcome":    drainBody(""),
		"zero attempt":     `{"attempt_id":0,"predecessor_release_id":"rel_old","predecessor_runtime_instance_id":"41","outcome":"stopped","summary":"x"}`,
		"missing release":  `{"attempt_id":7,"predecessor_runtime_instance_id":"41","outcome":"stopped","summary":"x"}`,
		"missing instance": `{"attempt_id":7,"predecessor_release_id":"rel_old","outcome":"stopped","summary":"x"}`,
		"oversize summary": fmt.Sprintf(`{"attempt_id":7,"predecessor_release_id":"rel_old","predecessor_runtime_instance_id":"41","outcome":"stopped","summary":%q}`, strings.Repeat("a", 2049)),
		"unknown field":    `{"attempt_id":7,"predecessor_release_id":"rel_old","predecessor_runtime_instance_id":"41","outcome":"stopped","summary":"x","fence":1}`,
	} {
		fr := &fakeDrainRecorder{}
		if code, msg := postDrain(t, Handler{DrainRecorder: fr}, body); code != http.StatusBadRequest || msg != "invalid_request" || fr.calls != 0 {
			t.Errorf("%s: status=%d code=%q calls=%d", name, code, msg, fr.calls)
		}
	}
}

func TestDrainResultErrorMapping(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		code int
		msg  string
	}{
		"conflict":  {postgres.ErrAgentLeaseIntegrityConflict, http.StatusConflict, "integrity_conflict"},
		"denied":    {postgres.ErrServerAuthorizationDenied, http.StatusUnauthorized, "unauthorized"},
		"invalid":   {postgres.ErrAgentLeaseInvalidInput, http.StatusBadRequest, "invalid_request"},
		"corrupt":   {postgres.ErrAgentLeaseCorruptState, http.StatusConflict, "corrupt_state"},
		"transient": {context.DeadlineExceeded, http.StatusServiceUnavailable, "service_unavailable"},
	} {
		fr := &fakeDrainRecorder{err: tc.err}
		if code, msg := postDrain(t, Handler{DrainRecorder: fr}, drainBody("stopped")); code != tc.code || msg != tc.msg {
			t.Errorf("%s: status=%d code=%q, want %d %q", name, code, msg, tc.code, tc.msg)
		}
	}
}

func TestDrainResultNilRecorderAndBadCredential(t *testing.T) {
	if code, _ := postDrain(t, Handler{}, drainBody("stopped")); code != http.StatusServiceUnavailable {
		t.Fatalf("nil recorder: status=%d, want 503", code)
	}
	fr := &fakeDrainRecorder{}
	w := do(Handler{DrainRecorder: fr}, newRequestTLS(newRequest(t, pathDrainResult, drainBody("stopped"), "Bearer nope", "server_1")))
	if w.Code != http.StatusUnauthorized || fr.calls != 0 {
		t.Fatalf("bad credential: status=%d calls=%d", w.Code, fr.calls)
	}
}
