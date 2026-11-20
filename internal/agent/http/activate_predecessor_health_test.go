package agenthttp

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"autodeploy/internal/healthgate"
	"autodeploy/internal/queue"
	"autodeploy/internal/server"
)

// phActivator implements both Activate and the 22-D2 extension, recording
// which one the handler called.
type phActivator struct {
	result queue.ActivationResult
	plain  int
	withPH int
}

func (f *phActivator) Activate(context.Context, string, [32]byte, int64, int64, [32]byte) (queue.ActivationResult, error) {
	f.plain++
	return f.result, nil
}

func (f *phActivator) ActivateWithPredecessorHealth(context.Context, string, [32]byte, int64, int64, [32]byte) (queue.ActivationResult, error) {
	f.withPH++
	return f.result, nil
}

func phResult() queue.ActivationResult {
	return queue.ActivationResult{
		OperationID: "7", RouteRevision: 3, Hostname: "app.example.com", EntrypointName: "websecure",
		CertResolverName: "letsencrypt-production", BackendRuntimeInstanceID: "43", RouteDigest: "sha256:ab",
		PredecessorReleaseID: "rel-1", PredecessorRuntimeInstanceID: "42",
		PredecessorHealth: &queue.PredecessorHealth{
			ContainerPort: 8081, ContainerScheme: "https", HealthPolicyRevision: 1,
			HealthGatePath: "/ready", HealthGateHost: "old.example.com",
			HealthPolicy: healthgate.Policy{SuccessThreshold: 2, IntervalSeconds: 5, TimeoutSeconds: 4, DeadlineSeconds: 50, TCPPort: 9000},
		},
	}
}

func postActivate(t *testing.T, h Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	tok, _ := server.NewCredentialToken(rand.Reader)
	return do(h, newRequestTLS(newRequest(t, pathActivate, body, bearerPrefix+string(tok), "server_1")))
}

func activateBody(extra string) string {
	return `{"attempt_id":43,"fence":3,"image_digest":"` + hexDigest(digestFixture(3)) + `"` + extra + `}`
}

// The non-opted response bytes are pinned: a caller that does not ask gets
// exactly the pre-22-D2 shape, even if the activator carries a block.
func TestActivateNonOptedBytesUnchanged(t *testing.T) {
	const golden = `{"operation_id":"7","route_revision":3,"hostname":"app.example.com","entrypoint_name":"websecure","cert_resolver_name":"letsencrypt-production","backend_runtime_instance_id":"43","route_digest":"sha256:ab","predecessor_release_id":"rel-1","predecessor_runtime_instance_id":"42"}`
	for _, extra := range []string{``, `,"predecessor_health":false`} {
		fa := &phActivator{result: phResult()}
		w := postActivate(t, Handler{Activator: fa}, activateBody(extra))
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d %s", w.Code, w.Body.String())
		}
		if got := strings.TrimSpace(w.Body.String()); got != golden {
			t.Fatalf("non-opted bytes changed (extra=%q):\n got %s\nwant %s", extra, got, golden)
		}
		if fa.withPH != 0 || fa.plain != 1 {
			t.Fatalf("non-opted request must use plain Activate: %+v", fa)
		}
	}
}

func TestActivateOptedEmitsFrozenBlock(t *testing.T) {
	const golden = `{"operation_id":"7","route_revision":3,"hostname":"app.example.com","entrypoint_name":"websecure","cert_resolver_name":"letsencrypt-production","backend_runtime_instance_id":"43","route_digest":"sha256:ab","predecessor_release_id":"rel-1","predecessor_runtime_instance_id":"42","predecessor_health":{"container_port":8081,"container_scheme":"https","health_policy_revision":1,"health_gate_path":"/ready","health_gate_host":"old.example.com","health_policy":{"success_threshold":2,"interval_seconds":5,"timeout_seconds":4,"deadline_seconds":50,"tcp_port":9000}}}`
	fa := &phActivator{result: phResult()}
	w := postActivate(t, Handler{Activator: fa}, activateBody(`,"predecessor_health":true`))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d %s", w.Code, w.Body.String())
	}
	if got := strings.TrimSpace(w.Body.String()); got != golden {
		t.Fatalf("opted bytes:\n got %s\nwant %s", got, golden)
	}
	if fa.withPH != 1 || fa.plain != 0 {
		t.Fatalf("opted request must use the extension: %+v", fa)
	}
}

func TestActivateOptedWithoutBlockOmitsKey(t *testing.T) {
	res := phResult()
	res.PredecessorHealth = nil
	w := postActivate(t, Handler{Activator: &phActivator{result: res}}, activateBody(`,"predecessor_health":true`))
	var raw map[string]json.RawMessage
	decodeBody(t, w, &raw)
	if _, ok := raw["predecessor_health"]; ok {
		t.Fatalf("absent block must be omitted: %s", w.Body.String())
	}
	// An Activator without the extension answers a plain activation.
	w = postActivate(t, Handler{Activator: &fakeActivator{result: phResult()}}, activateBody(`,"predecessor_health":true`))
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "predecessor_health") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestActivateStrictDecoderAcceptsFlagRejectsUnknown(t *testing.T) {
	fa := &phActivator{result: phResult()}
	if w := postActivate(t, Handler{Activator: fa}, activateBody(`,"predecessor_health":true`)); w.Code != http.StatusOK {
		t.Fatalf("flag rejected: %d", w.Code)
	}
	for _, extra := range []string{`,"predecessor_healthz":true`, `,"unknown":1`, `,"predecessor_health":"yes"`} {
		before := fa.plain + fa.withPH
		if w := postActivate(t, Handler{Activator: fa}, activateBody(extra)); w.Code != http.StatusBadRequest {
			t.Fatalf("extra %q: status=%d, want 400", extra, w.Code)
		}
		if fa.plain+fa.withPH != before {
			t.Fatalf("extra %q reached the activator", extra)
		}
	}
}
