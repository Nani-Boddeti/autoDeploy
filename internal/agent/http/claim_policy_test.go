package agenthttp

import (
	"context"
	"crypto/rand"
	"net/http"
	"strings"
	"testing"

	"autodeploy/internal/healthgate"
	"autodeploy/internal/queue"
	"autodeploy/internal/server"
)

// capClaimer is a fakeClaimer that also serves the health_policy opt-in.
type capClaimer struct {
	fakeClaimer
	capCalls int
	gotCaps  queue.ClaimCapabilities
}

func (f *capClaimer) ClaimWithCapabilities(_ context.Context, _ string, _ [32]byte, _ int, caps queue.ClaimCapabilities) ([]queue.ClaimedLease, error) {
	f.capCalls++
	f.gotCaps = caps
	return f.leases, f.err
}

func claimBody(t *testing.T, h Handler, body string) (int, string) {
	t.Helper()
	tok, _ := server.NewCredentialToken(rand.Reader)
	w := do(h, newRequestTLS(newRequest(t, pathClaim, body, bearerPrefix+string(tok), "server_1")))
	return w.Code, w.Body.String()
}

func TestClaimWithHealthPolicyCapabilityCarriesFrozenPolicy(t *testing.T) {
	def := claimGoldenLease()
	def.HealthPolicy = healthgate.DefaultPolicy()
	tuned := claimGoldenLease()
	tuned.JobID = 7
	tuned.HealthPolicy = healthgate.Policy{SuccessThreshold: 5, IntervalSeconds: 4, TimeoutSeconds: 3, DeadlineSeconds: 120, TCPPort: 8081}
	fc := &capClaimer{fakeClaimer: fakeClaimer{leases: []queue.ClaimedLease{def, tuned}}}
	h := Handler{Authenticator: &fakeAuthenticator{}, Claimer: fc}
	code, body := claimBody(t, h, `{"limit":1,"executor_available":true,"capabilities":["health_policy"]}`)
	if code != http.StatusOK || fc.capCalls != 1 || fc.calls != 0 || !fc.gotCaps.HealthPolicy {
		t.Fatalf("status=%d capCalls=%d plainCalls=%d caps=%+v", code, fc.capCalls, fc.calls, fc.gotCaps)
	}
	// Every lease carries all five fields; an unset tcp_port is an explicit null.
	if n := strings.Count(body, `"health_policy":{`); n != 2 {
		t.Fatalf("health_policy objects = %d in %s", n, body)
	}
	for _, want := range []string{
		`"health_policy":{"success_threshold":3,"interval_seconds":2,"timeout_seconds":2,"deadline_seconds":60,"tcp_port":null}`,
		`"health_policy":{"success_threshold":5,"interval_seconds":4,"timeout_seconds":3,"deadline_seconds":120,"tcp_port":8081}`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s in %s", want, body)
		}
	}
}

func TestClaimWithoutCapabilityNeverEmitsPolicyEvenIfLeaseHasOne(t *testing.T) {
	l := claimGoldenLease()
	l.HealthPolicy = healthgate.Policy{SuccessThreshold: 5, IntervalSeconds: 4, TimeoutSeconds: 3, DeadlineSeconds: 120, TCPPort: 8081}
	fc := &capClaimer{fakeClaimer: fakeClaimer{leases: []queue.ClaimedLease{l}}}
	code, body := claimBody(t, Handler{Authenticator: &fakeAuthenticator{}, Claimer: fc}, `{"limit":1,"executor_available":true}`)
	if code != http.StatusOK || body != claimGoldenBody || fc.calls != 1 || fc.capCalls != 0 {
		t.Fatalf("status=%d plain=%d cap=%d body=%s", code, fc.calls, fc.capCalls, body)
	}
}

func TestClaimCapabilityValidationAndGating(t *testing.T) {
	for name, body := range map[string]string{
		"unknown":   `{"limit":1,"executor_available":true,"capabilities":["bogus"]}`,
		"too many":  `{"limit":1,"executor_available":true,"capabilities":["health_policy","health_policy","health_policy","health_policy","health_policy"]}`,
		"not array": `{"limit":1,"executor_available":true,"capabilities":"health_policy"}`,
	} {
		fc := &capClaimer{}
		code, resp := claimBody(t, Handler{Authenticator: &fakeAuthenticator{}, Claimer: fc}, body)
		if code != http.StatusBadRequest || !strings.Contains(resp, "invalid_request") || fc.calls+fc.capCalls != 0 {
			t.Errorf("%s: status=%d body=%s calls=%d/%d", name, code, resp, fc.calls, fc.capCalls)
		}
	}
	// executor_available=false still never reaches the claimer.
	fc := &capClaimer{fakeClaimer: fakeClaimer{leases: []queue.ClaimedLease{claimGoldenLease()}}}
	code, resp := claimBody(t, Handler{Authenticator: &fakeAuthenticator{}, Claimer: fc}, `{"limit":1,"executor_available":false,"capabilities":["health_policy"]}`)
	if code != http.StatusOK || resp != `{"leases":[]}` || fc.calls+fc.capCalls != 0 {
		t.Fatalf("status=%d body=%s", code, resp)
	}
}

// A claimer without the extension (older store wiring) is still served: the
// plain Claim is the non-opted-in path, which never yields a non-default policy.
func TestClaimWithCapabilityFallsBackToPlainClaimerWithDefaultPolicy(t *testing.T) {
	fc := &fakeClaimer{leases: []queue.ClaimedLease{claimGoldenLease()}}
	code, body := claimBody(t, Handler{Authenticator: &fakeAuthenticator{}, Claimer: fc}, `{"limit":1,"executor_available":true,"capabilities":["health_policy"]}`)
	if code != http.StatusOK || fc.calls != 1 || !strings.Contains(body, `"health_policy":{"success_threshold":3,"interval_seconds":2,"timeout_seconds":2,"deadline_seconds":60,"tcp_port":null}`) {
		t.Fatalf("status=%d calls=%d body=%s", code, fc.calls, body)
	}
}
