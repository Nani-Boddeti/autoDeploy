package agentclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"autodeploy/internal/healthgate"
)

const leaseHead = `{"job_id":1,"attempt_id":2,"attempt_number":1,"fence":3,"project_id":"p","environment_id":"e","deployment_id":"d","repository_id":1,"ref":"refs/heads/main","commit_sha":"abc","lease_expires_at":"2026-11-16T10:00:00Z","authority_expires_at":"2026-11-16T10:00:00Z","release_id":"rel","secret_bindings":[],"literal_bindings":[]`

func claimServer(t *testing.T, bodies *[]string, handle func(n int, body string, w http.ResponseWriter)) *httptest.Server {
	t.Helper()
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*bodies = append(*bodies, string(b))
		w.Header().Set("Content-Type", "application/json")
		handle(len(*bodies), string(b), w)
	}))
}

func TestClaimSendsHealthPolicyCapability(t *testing.T) {
	var bodies []string
	srv := claimServer(t, &bodies, func(_ int, _ string, w http.ResponseWriter) { _, _ = w.Write([]byte(`{"leases":[]}`)) })
	defer srv.Close()
	if _, err := newTestClient(t, srv, nil).Claim(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 1 || bodies[0] != `{"limit":1,"executor_available":true,"capabilities":["health_policy"]}` {
		t.Fatalf("bodies = %v", bodies)
	}
}

func TestClaimFallsBackOnceWithoutCapabilityAndUsesDefaultPolicy(t *testing.T) {
	var bodies []string
	srv := claimServer(t, &bodies, func(_ int, body string, w http.ResponseWriter) {
		if strings.Contains(body, "capabilities") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_request"}`))
			return
		}
		_, _ = w.Write([]byte(`{"leases":[` + leaseHead + `}]}`))
	})
	defer srv.Close()
	leases, err := newTestClient(t, srv, nil).Claim(context.Background(), true)
	if err != nil || len(leases) != 1 {
		t.Fatalf("leases=%v err=%v", leases, err)
	}
	if len(bodies) != 2 || bodies[1] != `{"limit":1,"executor_available":true}` {
		t.Fatalf("bodies = %v", bodies)
	}
	if leases[0].HealthPolicy != healthgate.DefaultPolicy() {
		t.Fatalf("policy = %+v, want defaults", leases[0].HealthPolicy)
	}
}

func TestClaimFallbackDoesNotLoopOrCoverOtherErrors(t *testing.T) {
	var bodies []string
	srv := claimServer(t, &bodies, func(_ int, _ string, w http.ResponseWriter) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_request"}`))
	})
	_, err := newTestClient(t, srv, nil).Claim(context.Background(), true)
	srv.Close()
	if !errors.Is(err, ErrInvalidRequest) || len(bodies) != 2 {
		t.Fatalf("err=%v requests=%d", err, len(bodies))
	}
	bodies = nil
	srv = claimServer(t, &bodies, func(_ int, _ string, w http.ResponseWriter) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	})
	_, err = newTestClient(t, srv, nil).Claim(context.Background(), true)
	srv.Close()
	if !errors.Is(err, ErrUnauthorized) || len(bodies) != 1 {
		t.Fatalf("err=%v requests=%d", err, len(bodies))
	}
}

func claimWithPolicyField(t *testing.T, field string) ([]Lease, error) {
	t.Helper()
	var bodies []string
	srv := claimServer(t, &bodies, func(_ int, _ string, w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"leases":[` + leaseHead + field + `}]}`))
	})
	defer srv.Close()
	return newTestClient(t, srv, nil).Claim(context.Background(), true)
}

func TestClaimDecodesHealthPolicyStrictly(t *testing.T) {
	leases, err := claimWithPolicyField(t, `,"health_policy":{"success_threshold":5,"interval_seconds":4,"timeout_seconds":3,"deadline_seconds":120,"tcp_port":8081}`)
	if err != nil || len(leases) != 1 {
		t.Fatalf("leases=%v err=%v", leases, err)
	}
	if want := (healthgate.Policy{SuccessThreshold: 5, IntervalSeconds: 4, TimeoutSeconds: 3, DeadlineSeconds: 120, TCPPort: 8081}); leases[0].HealthPolicy != want {
		t.Fatalf("policy = %+v, want %+v", leases[0].HealthPolicy, want)
	}
	leases, err = claimWithPolicyField(t, `,"health_policy":{"success_threshold":3,"interval_seconds":2,"timeout_seconds":2,"deadline_seconds":60,"tcp_port":null}`)
	if err != nil || leases[0].HealthPolicy != healthgate.DefaultPolicy() {
		t.Fatalf("null tcp: %+v err=%v", leases, err)
	}
	for name, field := range map[string]string{
		"unknown inner field": `,"health_policy":{"success_threshold":3,"interval_seconds":2,"timeout_seconds":2,"deadline_seconds":60,"tcp_port":null,"extra":1}`,
		"missing field":       `,"health_policy":{"success_threshold":3,"interval_seconds":2,"timeout_seconds":2,"tcp_port":null}`,
		"out of bounds":       `,"health_policy":{"success_threshold":0,"interval_seconds":2,"timeout_seconds":2,"deadline_seconds":60,"tcp_port":null}`,
		"cross rule":          `,"health_policy":{"success_threshold":3,"interval_seconds":1,"timeout_seconds":2,"deadline_seconds":60,"tcp_port":null}`,
		"bad port":            `,"health_policy":{"success_threshold":3,"interval_seconds":2,"timeout_seconds":2,"deadline_seconds":60,"tcp_port":70000}`,
	} {
		if _, err := claimWithPolicyField(t, field); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The agent declared the capability and the server accepted the request, so
// every lease must carry its frozen policy. A missing or null health_policy
// (a server bug, or a proxy stripping the field) is a malformed lease, never
// the default policy: running a tuned job with default settings would
// silently weaken its check. Defaults apply only after the legacy fallback.
func TestClaimWithCapabilityRefusesLeaseWithoutPolicy(t *testing.T) {
	for name, field := range map[string]string{"missing": ``, "null": `,"health_policy":null`} {
		if leases, err := claimWithPolicyField(t, field); !errors.Is(err, ErrTransportFailure) {
			t.Errorf("%s: leases=%+v err=%v, want malformed lease", name, leases, err)
		}
	}
}
