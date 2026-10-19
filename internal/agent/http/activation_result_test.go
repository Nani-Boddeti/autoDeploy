package agenthttp

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"autodeploy/internal/server"
	"autodeploy/internal/store/postgres"
)

type fakeActivationReporter struct {
	err  error
	seen struct {
		called      bool
		attemptID   int64
		fence       int64
		operationID string
		succeeded   bool
		digest      [32]byte
		summary     string
	}
}

func (f *fakeActivationReporter) ReportActivationResult(_ context.Context, _ string, _ [32]byte, attemptID, fence int64, operationID string, succeeded bool, observedRouteDigest [32]byte, failureSummary string) error {
	f.seen.called = true
	f.seen.attemptID, f.seen.fence, f.seen.operationID = attemptID, fence, operationID
	f.seen.succeeded, f.seen.digest, f.seen.summary = succeeded, observedRouteDigest, failureSummary
	return f.err
}

// TestHandleActivationResultGateDistinctFromTypedRefusals guards against
// the 10c-1 review's defect class: a typed refusal falling through mapError
// to the generic, agent-retryable 503 that the nil-dependency gate also
// returns. Every status/code is asserted explicitly and the 409s are
// compared against the gate's own response.
func TestHandleActivationResultGateDistinctFromTypedRefusals(t *testing.T) {
	tok, _ := server.NewCredentialToken(rand.Reader)
	body, _ := json.Marshal(activationResultRequest{AttemptID: 1, Fence: 1, OperationID: "7", Succeeded: boolPtr(true), ObservedRouteDigest: hexDigest(digestFixture(1))})

	t.Run("gate disabled (nil ActivationReporter)", func(t *testing.T) {
		w := do(Handler{}, newRequestTLS(newRequest(t, pathActivationResult, string(body), bearerPrefix+string(tok), "server_1")))
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusServiceUnavailable)
		}
		var got errorBody
		decodeBody(t, w, &got)
		if got.Error != "service_unavailable" {
			t.Fatalf("code=%s, want service_unavailable", got.Error)
		}
	})

	cases := []struct {
		name string
		err  error
		code string
	}{
		{"stale", postgres.ErrAgentLeaseStale, "stale_lease"},
		{"integrity conflict", postgres.ErrAgentLeaseIntegrityConflict, "integrity_conflict"},
		{"activation open", postgres.ErrAgentLeaseActivationOpen, "activation_open"},
		{"route moved", postgres.ErrAgentLeaseActivationRouteMoved, "route_moved"},
		{"late report recorded", postgres.ErrAgentLeaseActivationLateReportRecorded, "late_report_recorded"},
		{"late report recorded wrapped", fmt.Errorf("report: %w", postgres.ErrAgentLeaseActivationLateReportRecorded), "late_report_recorded"},
		{"corrupt state", postgres.ErrAgentLeaseCorruptState, "corrupt_state"},
		{"constraint violation", postgres.ErrAgentLeaseConstraintViolation, "constraint_violation"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := Handler{ActivationReporter: &fakeActivationReporter{err: tc.err}}
			w := do(h, newRequestTLS(newRequest(t, pathActivationResult, string(body), bearerPrefix+string(tok), "server_1")))
			if w.Code != http.StatusConflict {
				t.Fatalf("status=%d, want %d", w.Code, http.StatusConflict)
			}
			var got errorBody
			decodeBody(t, w, &got)
			if got.Error != tc.code {
				t.Fatalf("code=%s, want %s", got.Error, tc.code)
			}
			if got.Error == "service_unavailable" {
				t.Fatal("typed refusal must not be indistinguishable from the disabled gate")
			}
		})
	}

	t.Run("unauthenticated is 401 before the gate", func(t *testing.T) {
		w := do(Handler{}, newRequestTLS(newRequest(t, pathActivationResult, string(body), "", "")))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusUnauthorized)
		}
	})
}

// TestHandleCompleteActivationOpenIsNotRetryable proves Complete's new
// refusal is a 409, not the retryable generic 503.
func TestHandleCompleteActivationOpenIsNotRetryable(t *testing.T) {
	tok, _ := server.NewCredentialToken(rand.Reader)
	body, _ := json.Marshal(completeRequest{AttemptID: 1, Fence: 1, Outcome: "succeeded", OperationID: "op", ResultDigest: hexDigest(digestFixture(1))})
	h := Handler{Completer: &fakeCompleter{err: postgres.ErrAgentLeaseActivationOpen}}
	w := do(h, newRequestTLS(newRequest(t, pathComplete, string(body), bearerPrefix+string(tok), "server_1")))
	if w.Code != http.StatusConflict {
		t.Fatalf("status=%d, want %d", w.Code, http.StatusConflict)
	}
	var got errorBody
	decodeBody(t, w, &got)
	if got.Error != "activation_open" {
		t.Fatalf("code=%s, want activation_open", got.Error)
	}
}

func boolPtr(v bool) *bool { return &v }

// TestHandleActivationResultMissingSucceededIsRejected: an absent "succeeded"
// key must be a 400, never a declared failure that would durably block the
// environment. Raw JSON is sent because a marshalled struct with the pointer
// field set can never omit the key.
func TestHandleActivationResultMissingSucceededIsRejected(t *testing.T) {
	tok, _ := server.NewCredentialToken(rand.Reader)
	for name, body := range map[string]string{
		"no succeeded key":      `{"attempt_id":1,"fence":1,"operation_id":"7"}`,
		"null succeeded":        `{"attempt_id":1,"fence":1,"operation_id":"7","succeeded":null}`,
		"summary, no succeeded": `{"attempt_id":1,"fence":1,"operation_id":"7","failure_summary":"reload timed out"}`,
	} {
		t.Run(name, func(t *testing.T) {
			fr := &fakeActivationReporter{}
			w := do(Handler{ActivationReporter: fr}, newRequestTLS(newRequest(t, pathActivationResult, body, bearerPrefix+string(tok), "server_1")))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
			}
			if fr.seen.called {
				t.Fatal("repository must not be called when succeeded is absent")
			}
		})
	}
}

func TestHandleActivationResultRequestShapes(t *testing.T) {
	tok, _ := server.NewCredentialToken(rand.Reader)
	good := hexDigest(digestFixture(2))
	rejected := []struct {
		name string
		req  activationResultRequest
	}{
		{"missing attempt_id", activationResultRequest{Fence: 1, OperationID: "7", Succeeded: boolPtr(true), ObservedRouteDigest: good}},
		{"missing fence", activationResultRequest{AttemptID: 1, OperationID: "7", Succeeded: boolPtr(true), ObservedRouteDigest: good}},
		{"missing operation_id", activationResultRequest{AttemptID: 1, Fence: 1, Succeeded: boolPtr(true), ObservedRouteDigest: good}},
		{"oversized operation_id", activationResultRequest{AttemptID: 1, Fence: 1, OperationID: string(make([]byte, 129)), Succeeded: boolPtr(true), ObservedRouteDigest: good}},
		{"success with malformed digest", activationResultRequest{AttemptID: 1, Fence: 1, OperationID: "7", Succeeded: boolPtr(true), ObservedRouteDigest: "not-hex"}},
		{"success with no digest", activationResultRequest{AttemptID: 1, Fence: 1, OperationID: "7", Succeeded: boolPtr(true)}},
		{"success carrying a failure summary", activationResultRequest{AttemptID: 1, Fence: 1, OperationID: "7", Succeeded: boolPtr(true), ObservedRouteDigest: good, FailureSummary: "x"}},
		{"failure carrying a digest", activationResultRequest{AttemptID: 1, Fence: 1, OperationID: "7", Succeeded: boolPtr(false), ObservedRouteDigest: good}},
		{"oversized failure summary", activationResultRequest{AttemptID: 1, Fence: 1, OperationID: "7", Succeeded: boolPtr(false), FailureSummary: string(make([]byte, maxActivationFailureSummaryBytes+1))}},
	}
	for _, tc := range rejected {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			fr := &fakeActivationReporter{}
			body, _ := json.Marshal(tc.req)
			w := do(Handler{ActivationReporter: fr}, newRequestTLS(newRequest(t, pathActivationResult, string(body), bearerPrefix+string(tok), "server_1")))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
			}
			if fr.seen.called {
				t.Fatal("repository must not be called on invalid input")
			}
		})
	}

	t.Run("valid success, sha256-prefixed digest accepted", func(t *testing.T) {
		fr := &fakeActivationReporter{}
		body, _ := json.Marshal(activationResultRequest{AttemptID: 42, Fence: 3, OperationID: "9", Succeeded: boolPtr(true), ObservedRouteDigest: "sha256:" + good})
		w := do(Handler{ActivationReporter: fr}, newRequestTLS(newRequest(t, pathActivationResult, string(body), bearerPrefix+string(tok), "server_1")))
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		if !fr.seen.called || fr.seen.attemptID != 42 || fr.seen.fence != 3 || fr.seen.operationID != "9" || !fr.seen.succeeded || fr.seen.digest != digestFixture(2) {
			t.Fatalf("unexpected call: %+v", fr.seen)
		}
	})

	t.Run("valid failure", func(t *testing.T) {
		fr := &fakeActivationReporter{}
		body, _ := json.Marshal(activationResultRequest{AttemptID: 42, Fence: 3, OperationID: "9", Succeeded: boolPtr(false), FailureSummary: "reload timed out"})
		w := do(Handler{ActivationReporter: fr}, newRequestTLS(newRequest(t, pathActivationResult, string(body), bearerPrefix+string(tok), "server_1")))
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		if !fr.seen.called || fr.seen.succeeded || fr.seen.summary != "reload timed out" {
			t.Fatalf("unexpected call: %+v", fr.seen)
		}
	})
}

// TestHandleActivationResultUnrecordedBlockIsAlwaysRetryable: an error that
// says the reconciliation block was NOT made durable must answer the
// retryable 503 whatever sentinel its inner error matches. mapError tests
// ErrActivationBlockUnrecorded first; with it second, an inner
// ErrAgentLeaseCorruptState/Stale would answer a terminal 409 and the agent
// would stop with nothing recorded.
func TestHandleActivationResultUnrecordedBlockIsAlwaysRetryable(t *testing.T) {
	tok, _ := server.NewCredentialToken(rand.Reader)
	body, _ := json.Marshal(activationResultRequest{AttemptID: 1, Fence: 1, OperationID: "7", Succeeded: boolPtr(true), ObservedRouteDigest: hexDigest(digestFixture(1))})
	for name, inner := range map[string]error{
		"corrupt state":        postgres.ErrAgentLeaseCorruptState,
		"stale":                postgres.ErrAgentLeaseStale,
		"constraint violation": postgres.ErrAgentLeaseConstraintViolation,
		"unauthorized":         postgres.ErrServerAuthorizationDenied,
		"integrity conflict":   postgres.ErrAgentLeaseIntegrityConflict,
	} {
		t.Run(name, func(t *testing.T) {
			err := fmt.Errorf("%w: %w", postgres.ErrActivationBlockUnrecorded, inner)
			h := Handler{ActivationReporter: &fakeActivationReporter{err: err}}
			w := do(h, newRequestTLS(newRequest(t, pathActivationResult, string(body), bearerPrefix+string(tok), "server_1")))
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("status=%d, want %d", w.Code, http.StatusServiceUnavailable)
			}
		})
	}
}
