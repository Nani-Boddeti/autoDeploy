package agenthttp

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"autodeploy/internal/queue"
	"autodeploy/internal/server"
	"autodeploy/internal/store/postgres"
)

type fakeActivator struct {
	result queue.ActivationResult
	err    error
	seen   struct {
		called      bool
		attemptID   int64
		fence       int64
		imageDigest [32]byte
	}
}

func (f *fakeActivator) Activate(_ context.Context, _ string, _ [32]byte, attemptID, fence int64, imageDigest [32]byte) (queue.ActivationResult, error) {
	f.seen.called = true
	f.seen.attemptID, f.seen.fence, f.seen.imageDigest = attemptID, fence, imageDigest
	return f.result, f.err
}

// TestHandleActivateGateDistinctFromTypedRefusals is the review-requested
// regression guard: the activationEnabled=false gate (a nil Activator) and
// the two typed activation refusals (candidate not ready / no route head)
// must never collapse into the same response. If mapError's cases for
// ErrAgentLeaseActivationCandidateNotReady/ErrAgentLeaseActivationNoRouteHead
// were ever removed (falling through to the default 503
// service_unavailable branch), this test's "candidate not ready" and
// "no route head" subtests would start asserting the same status/code the
// "gate disabled" subtest asserts, and the -- deliberately duplicated --
// explicit status/code checks below would fail rather than silently
// passing.
func TestHandleActivateGateDistinctFromTypedRefusals(t *testing.T) {
	tok, _ := server.NewCredentialToken(rand.Reader)
	imageDigest := hexDigest(digestFixture(1))
	body, _ := json.Marshal(activateRequest{AttemptID: 1, Fence: 1, ImageDigest: imageDigest})

	t.Run("gate disabled (nil Activator)", func(t *testing.T) {
		h := Handler{} // Activator left nil, matching activationEnabled=false wiring
		r := newRequestTLS(newRequest(t, pathActivate, string(body), bearerPrefix+string(tok), "server_1"))
		w := do(h, r)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusServiceUnavailable)
		}
		var got errorBody
		decodeBody(t, w, &got)
		if got.Error != "service_unavailable" {
			t.Fatalf("code=%s, want service_unavailable", got.Error)
		}
	})

	t.Run("candidate not ready", func(t *testing.T) {
		fa := &fakeActivator{err: postgres.ErrAgentLeaseActivationCandidateNotReady}
		h := Handler{Activator: fa}
		r := newRequestTLS(newRequest(t, pathActivate, string(body), bearerPrefix+string(tok), "server_1"))
		w := do(h, r)
		if w.Code != http.StatusConflict {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusConflict)
		}
		var got errorBody
		decodeBody(t, w, &got)
		if got.Error != "candidate_not_ready" {
			t.Fatalf("code=%s, want candidate_not_ready", got.Error)
		}
		if w.Code == http.StatusServiceUnavailable || got.Error == "service_unavailable" {
			t.Fatal("candidate_not_ready must not be indistinguishable from the disabled gate")
		}
	})

	t.Run("no route head", func(t *testing.T) {
		fa := &fakeActivator{err: postgres.ErrAgentLeaseActivationNoRouteHead}
		h := Handler{Activator: fa}
		r := newRequestTLS(newRequest(t, pathActivate, string(body), bearerPrefix+string(tok), "server_1"))
		w := do(h, r)
		if w.Code != http.StatusConflict {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusConflict)
		}
		var got errorBody
		decodeBody(t, w, &got)
		if got.Error != "no_route_head" {
			t.Fatalf("code=%s, want no_route_head", got.Error)
		}
		if w.Code == http.StatusServiceUnavailable || got.Error == "service_unavailable" {
			t.Fatal("no_route_head must not be indistinguishable from the disabled gate")
		}
	})
}

func TestHandleActivateRequestShapes(t *testing.T) {
	tok, _ := server.NewCredentialToken(rand.Reader)

	t.Run("missing attempt_id rejected before Activator is called", func(t *testing.T) {
		fa := &fakeActivator{}
		h := Handler{Activator: fa}
		body, _ := json.Marshal(activateRequest{Fence: 1, ImageDigest: hexDigest(digestFixture(2))})
		r := newRequestTLS(newRequest(t, pathActivate, string(body), bearerPrefix+string(tok), "server_1"))
		w := do(h, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
		}
		if fa.seen.called {
			t.Fatal("Activate must not be called on invalid input")
		}
	})

	t.Run("malformed image digest rejected", func(t *testing.T) {
		fa := &fakeActivator{}
		h := Handler{Activator: fa}
		body, _ := json.Marshal(activateRequest{AttemptID: 1, Fence: 1, ImageDigest: "not-hex"})
		r := newRequestTLS(newRequest(t, pathActivate, string(body), bearerPrefix+string(tok), "server_1"))
		w := do(h, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
		}
		if fa.seen.called {
			t.Fatal("Activate must not be called on invalid input")
		}
	})

	t.Run("valid request accepted and response round-trips", func(t *testing.T) {
		imgDigest := digestFixture(3)
		fa := &fakeActivator{result: queue.ActivationResult{
			OperationID:              "op-1",
			RouteRevision:            2,
			Hostname:                 "app.example.com",
			EntrypointName:           "websecure",
			CertResolverName:         "letsencrypt-production",
			BackendRuntimeInstanceID: "42",
			RouteDigest:              "sha256:" + hexDigest(digestFixture(4)),
		}}
		h := Handler{Activator: fa}
		body, _ := json.Marshal(activateRequest{AttemptID: 42, Fence: 3, ImageDigest: hexDigest(imgDigest)})
		r := newRequestTLS(newRequest(t, pathActivate, string(body), bearerPrefix+string(tok), "server_1"))
		w := do(h, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		if !fa.seen.called || fa.seen.attemptID != 42 || fa.seen.fence != 3 || fa.seen.imageDigest != imgDigest {
			t.Fatalf("Activate not called with expected args: %+v", fa.seen)
		}
		var got activateResponse
		decodeBody(t, w, &got)
		if got.OperationID != "op-1" || got.RouteRevision != 2 || got.Hostname != "app.example.com" ||
			got.EntrypointName != "websecure" || got.CertResolverName != "letsencrypt-production" ||
			got.BackendRuntimeInstanceID != "42" || got.RouteDigest == "" {
			t.Fatalf("unexpected response: %+v", got)
		}
	})
}

// digestFixture builds a deterministic, distinguishable 32-byte digest for
// test fixtures -- this package has no existing package-level digest helper
// (unlike internal/store/postgres's own test-only digest()), so this file
// defines its own.
func digestFixture(b byte) [32]byte { var d [32]byte; d[0] = b; return d }

// TestHandleActivateEnvironmentBlockedIsNonRetryable pins the wire contract
// for an environment holding an unresolved activation: 409 with the dedicated
// code, never the retryable 503 service_unavailable the agent client retries
// (a raw 23505 used to fall through to that default). The wrapped case proves
// errors.Is matching, not identity.
func TestHandleActivateEnvironmentBlockedIsNonRetryable(t *testing.T) {
	tok, _ := server.NewCredentialToken(rand.Reader)
	body, _ := json.Marshal(activateRequest{AttemptID: 1, Fence: 1, ImageDigest: hexDigest(digestFixture(1))})
	for name, err := range map[string]error{
		"bare":    postgres.ErrAgentLeaseActivationEnvironmentBlocked,
		"wrapped": fmt.Errorf("activate: %w", postgres.ErrAgentLeaseActivationEnvironmentBlocked),
	} {
		t.Run(name, func(t *testing.T) {
			h := Handler{Activator: &fakeActivator{err: err}}
			w := do(h, newRequestTLS(newRequest(t, pathActivate, string(body), bearerPrefix+string(tok), "server_1")))
			if w.Code != http.StatusConflict {
				t.Fatalf("status=%d, want %d", w.Code, http.StatusConflict)
			}
			var got errorBody
			decodeBody(t, w, &got)
			if got.Error != "environment_activation_blocked" {
				t.Fatalf("code=%q, want environment_activation_blocked", got.Error)
			}
		})
	}
}
