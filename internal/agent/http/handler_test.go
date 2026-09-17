package agenthttp

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"autodeploy/internal/queue"
	"autodeploy/internal/server"
	"autodeploy/internal/store/postgres"
)

// --- fakes -------------------------------------------------------------

type fakeEnroller struct {
	calls   int
	version int
	err     error
	gotID   string
	gotEnr  [32]byte
	gotCred [32]byte
}

func (f *fakeEnroller) Enroll(_ context.Context, serverID string, enrollmentDigest, credentialDigest [32]byte) (int, error) {
	f.calls++
	f.gotID, f.gotEnr, f.gotCred = serverID, enrollmentDigest, credentialDigest
	return f.version, f.err
}

type fakeAuthenticator struct {
	calls int
	err   error
}

func (f *fakeAuthenticator) Authenticate(_ context.Context, _ string, _ [32]byte) error {
	f.calls++
	return f.err
}

type fakeHeartbeater struct {
	calls int
	err   error
}

func (f *fakeHeartbeater) Heartbeat(_ context.Context, _ string, _ [32]byte) error {
	f.calls++
	return f.err
}

type fakeClaimer struct {
	calls  int
	leases []queue.ClaimedLease
	err    error
}

func (f *fakeClaimer) Claim(_ context.Context, _ string, _ [32]byte, _ int) ([]queue.ClaimedLease, error) {
	f.calls++
	return f.leases, f.err
}

type fakeRenewer struct {
	result queue.RenewalResult
	err    error
}

func (f *fakeRenewer) Renew(_ context.Context, _ string, _ [32]byte, _, _ int64, _ int) (queue.RenewalResult, error) {
	return f.result, f.err
}

type fakeCompleter struct {
	err  error
	seen struct {
		outcome  queue.CompletionOutcome
		failure  queue.FailureClass
		opID     string
		digest   [32]byte
		attempt  int64
		fence    int64
		callSeen bool
	}
}

func (f *fakeCompleter) Complete(_ context.Context, _ string, _ [32]byte, attemptID, fence int64, outcome queue.CompletionOutcome, failureClass queue.FailureClass, operationID string, resultDigest [32]byte) error {
	f.seen.callSeen = true
	f.seen.outcome, f.seen.failure, f.seen.opID, f.seen.digest, f.seen.attempt, f.seen.fence = outcome, failureClass, operationID, resultDigest, attemptID, fence
	return f.err
}

type fakeCanceller struct{ err error }

func (f *fakeCanceller) AcknowledgeCancellation(_ context.Context, _ string, _ [32]byte, _, _ int64, _ string, _ [32]byte) error {
	return f.err
}

type fakeReconciler struct{ err error }

func (f *fakeReconciler) Reconcile(_ context.Context, _ string, _ [32]byte, _ string, _, _ int64, _ [32]byte) error {
	return f.err
}

// --- helpers -------------------------------------------------------------

func testCredential(t *testing.T) (server.CredentialToken, [32]byte) {
	t.Helper()
	tok, err := server.NewCredentialToken(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	d, err := server.CredentialTokenDigest(tok)
	if err != nil {
		t.Fatal(err)
	}
	return tok, d
}

func testEnrollmentToken(t *testing.T) (server.EnrollmentToken, [32]byte) {
	t.Helper()
	tok, err := server.NewEnrollmentToken(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	d, err := server.EnrollmentTokenDigest(tok)
	if err != nil {
		t.Fatal(err)
	}
	return tok, d
}

func hexDigest(d [32]byte) string { return hex.EncodeToString(d[:]) }

func newRequest(t *testing.T, path string, body string, auth, serverID string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "https://agent.example.test"+path, strings.NewReader(body))
	r.RemoteAddr = "192.0.2.10:443"
	r.Header.Set("Content-Type", "application/json")
	if auth != "" {
		r.Header.Set(authHeaderName, auth)
	}
	if serverID != "" {
		r.Header.Set(serverIDHeader, serverID)
	}
	return r
}

func do(h Handler, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func decodeBody(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatalf("decode response %q: %v", w.Body.String(), err)
	}
}

// --- routing / method / content-type / size / decode tests ---------------

func TestRouteMethodAndContentTypeContracts(t *testing.T) {
	h := Handler{}
	r := newRequestTLS(newRequest(t, pathHeartbeat, "{}", "", ""))
	r.Method = http.MethodGet
	if w := do(h, r); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: status=%d", w.Code)
	}

	r = newRequestTLS(newRequest(t, "/agent/v1/unknown", "{}", "", ""))
	if w := do(h, r); w.Code != http.StatusNotFound {
		t.Fatalf("unknown path: status=%d", w.Code)
	}

	r = newRequestTLS(newRequest(t, pathHeartbeat, "{}", "", ""))
	r.Header.Set("Content-Type", "text/plain")
	if w := do(h, r); w.Code != http.StatusBadRequest {
		t.Fatalf("wrong content-type: status=%d", w.Code)
	}

	r = newRequestTLS(newRequest(t, pathHeartbeat, "{}", "", ""))
	r.URL.RawQuery = "foo=bar"
	if w := do(h, r); w.Code != http.StatusNotFound {
		t.Fatalf("query string present: status=%d", w.Code)
	}
}

// newRequestTLS marks the request as arriving over TLS, matching the
// trusted-provenance check internal/github/http's handler also enforces.
func newRequestTLS(r *http.Request) *http.Request {
	r.TLS = &tls.ConnectionState{}
	return r
}

func TestUntrustedHTTPRejected(t *testing.T) {
	h := Handler{}
	r := httptest.NewRequest(http.MethodPost, "http://agent.example.test"+pathHeartbeat, strings.NewReader("{}"))
	r.RemoteAddr = "203.0.113.5:1234"
	r.Header.Set("Content-Type", "application/json")
	if w := do(h, r); w.Code != http.StatusNotFound {
		t.Fatalf("untrusted plaintext: status=%d", w.Code)
	}
}

func TestTrustedProxyHTTPSAccepted(t *testing.T) {
	h := Handler{TrustedProxyCIDRs: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}, Heartbeater: &fakeHeartbeater{}}
	tok, _ := server.NewCredentialToken(rand.Reader)
	body := heartbeatBody(1)
	r := httptest.NewRequest(http.MethodPost, "http://agent.example.test"+pathHeartbeat, strings.NewReader(body))
	r.RemoteAddr = "192.0.2.10:1234"
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(authHeaderName, bearerPrefix+string(tok))
	r.Header.Set(serverIDHeader, "server_1")
	if w := do(h, r); w.Code != http.StatusOK {
		t.Fatalf("trusted proxy https: status=%d body=%s", w.Code, w.Body.String())
	}
}

func heartbeatBody(protocolVersion int) string {
	b, _ := json.Marshal(heartbeatRequest{ProtocolVersion: protocolVersion, AgentVersion: "v1", OS: "linux", Arch: "amd64", CPUCount: 4, MemoryBytes: 1 << 30, DiskFreeBytes: 1 << 30, ExecutorAvailable: false})
	return string(b)
}

func TestBodySizeLimitRejected(t *testing.T) {
	h := Handler{Heartbeater: &fakeHeartbeater{}}
	tok, _ := server.NewCredentialToken(rand.Reader)
	oversized := strings.Repeat("a", MaxHeartbeatBodyBytes+1)
	body := `{"agent_version":"` + oversized + `"}`
	r := newRequestTLS(newRequest(t, pathHeartbeat, body, bearerPrefix+string(tok), "server_1"))
	if w := do(h, r); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: status=%d", w.Code)
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	h := Handler{Heartbeater: &fakeHeartbeater{}}
	tok, _ := server.NewCredentialToken(rand.Reader)
	body := `{"protocol_version":1,"agent_version":"v1","os":"linux","arch":"amd64","cpu_count":1,"memory_bytes":1,"disk_free_bytes":1,"executor_available":false,"bogus":true}`
	r := newRequestTLS(newRequest(t, pathHeartbeat, body, bearerPrefix+string(tok), "server_1"))
	if w := do(h, r); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown field: status=%d", w.Code)
	}
}

func TestTrailingDataRejected(t *testing.T) {
	h := Handler{Heartbeater: &fakeHeartbeater{}}
	tok, _ := server.NewCredentialToken(rand.Reader)
	body := heartbeatBody(1) + `{}`
	r := newRequestTLS(newRequest(t, pathHeartbeat, body, bearerPrefix+string(tok), "server_1"))
	if w := do(h, r); w.Code != http.StatusBadRequest {
		t.Fatalf("trailing data: status=%d", w.Code)
	}
}

// --- auth header strictness ----------------------------------------------

func TestAuthHeaderStrictness(t *testing.T) {
	tok, _ := server.NewCredentialToken(rand.Reader)
	enrTok, _ := server.NewEnrollmentToken(rand.Reader)
	body := heartbeatBody(1)

	cases := []struct {
		name string
		fn   func(r *http.Request)
	}{
		{"missing", func(r *http.Request) {}},
		{"duplicate", func(r *http.Request) {
			r.Header.Set(authHeaderName, bearerPrefix+string(tok))
			r.Header.Add(authHeaderName, bearerPrefix+string(tok))
		}},
		{"lowercase scheme", func(r *http.Request) { r.Header.Set(authHeaderName, "bearer "+string(tok)) }},
		{"wrong prefix (enrollment as bearer)", func(r *http.Request) { r.Header.Set(authHeaderName, bearerPrefix+string(enrTok)) }},
		{"malformed token", func(r *http.Request) { r.Header.Set(authHeaderName, bearerPrefix+"not-a-token") }},
		{"no bearer prefix", func(r *http.Request) { r.Header.Set(authHeaderName, string(tok)) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := Handler{Heartbeater: &fakeHeartbeater{}}
			r := newRequestTLS(newRequest(t, pathHeartbeat, body, "", "server_1"))
			tc.fn(r)
			if w := do(h, r); w.Code != http.StatusUnauthorized {
				t.Fatalf("%s: status=%d", tc.name, w.Code)
			}
		})
	}

	t.Run("token in query string", func(t *testing.T) {
		h := Handler{Heartbeater: &fakeHeartbeater{}}
		r := newRequestTLS(newRequest(t, pathHeartbeat, body, bearerPrefix+string(tok), "server_1"))
		r.URL.RawQuery = "access_token=" + string(tok)
		if w := do(h, r); w.Code != http.StatusNotFound {
			t.Fatalf("token in query: status=%d", w.Code)
		}
	})

	t.Run("missing server id header", func(t *testing.T) {
		h := Handler{Heartbeater: &fakeHeartbeater{}}
		r := newRequestTLS(newRequest(t, pathHeartbeat, body, bearerPrefix+string(tok), ""))
		if w := do(h, r); w.Code != http.StatusUnauthorized {
			t.Fatalf("missing server id: status=%d", w.Code)
		}
	})

	t.Run("valid accepted", func(t *testing.T) {
		fh := &fakeHeartbeater{}
		h := Handler{Heartbeater: fh}
		r := newRequestTLS(newRequest(t, pathHeartbeat, body, bearerPrefix+string(tok), "server_1"))
		if w := do(h, r); w.Code != http.StatusOK || fh.calls != 1 {
			t.Fatalf("valid: status=%d calls=%d", w.Code, fh.calls)
		}
	})
}

// --- error mapping ---------------------------------------------------------

func TestErrorMapping(t *testing.T) {
	wrap := func(err error) error { return fmt.Errorf("wrapped: %w", err) }
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"auth denied", postgres.ErrServerAuthorizationDenied, http.StatusUnauthorized, "unauthorized"},
		{"wrapped auth denied", wrap(postgres.ErrServerAuthorizationDenied), http.StatusUnauthorized, "unauthorized"},
		{"stale lease", postgres.ErrAgentLeaseStale, http.StatusConflict, "stale_lease"},
		{"integrity conflict", postgres.ErrAgentLeaseIntegrityConflict, http.StatusConflict, "integrity_conflict"},
		{"invalid input", postgres.ErrAgentLeaseInvalidInput, http.StatusBadRequest, "invalid_request"},
		{"infra error", errors.New("boom secret db dsn"), http.StatusServiceUnavailable, "service_unavailable"},
	}
	tok, _ := server.NewCredentialToken(rand.Reader)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := Handler{Renewer: &fakeRenewer{err: tc.err}}
			body, _ := json.Marshal(renewRequest{AttemptID: 1, Fence: 1, Sequence: 0})
			r := newRequestTLS(newRequest(t, pathRenew, string(body), bearerPrefix+string(tok), "server_1"))
			w := do(h, r)
			if w.Code != tc.status {
				t.Fatalf("%s: status=%d want=%d", tc.name, w.Code, tc.status)
			}
			if strings.Contains(w.Body.String(), "boom secret db dsn") {
				t.Fatalf("%s: error text echoed: %s", tc.name, w.Body.String())
			}
			var body2 errorBody
			decodeBody(t, w, &body2)
			if body2.Error != tc.code {
				t.Fatalf("%s: code=%s want=%s", tc.name, body2.Error, tc.code)
			}
		})
	}
}

// --- enroll ----------------------------------------------------------------

func TestEnrollNeverTransmitsRawCredential(t *testing.T) {
	fe := &fakeEnroller{version: 1}
	h := Handler{Enroller: fe}
	enrTok, _ := testEnrollmentToken(t)
	_, credDigest := testCredential(t)
	body, _ := json.Marshal(enrollRequest{ServerID: "server_1", EnrollmentToken: string(enrTok), CredentialDigest: hexDigest(credDigest)})
	r := newRequestTLS(newRequest(t, pathEnroll, string(body), "", ""))
	w := do(h, r)
	if w.Code != http.StatusOK {
		t.Fatalf("enroll: status=%d body=%s", w.Code, w.Body.String())
	}
	if fe.gotCred != credDigest {
		t.Fatal("credential digest not forwarded correctly")
	}
	var resp enrollResponse
	decodeBody(t, w, &resp)
	if resp.CredentialVersion != 1 {
		t.Fatalf("credential_version=%d", resp.CredentialVersion)
	}
	// The raw credential token must never appear anywhere in the request
	// body or response.
	if strings.Contains(string(body), "srv.v1.") {
		t.Fatal("raw credential token present in enroll request body")
	}
	if strings.Contains(w.Body.String(), "srv.v1.") {
		t.Fatal("raw credential token present in enroll response body")
	}
}

func TestEnrollRejectsRawCredentialToken(t *testing.T) {
	fe := &fakeEnroller{version: 1}
	h := Handler{Enroller: fe}
	enrTok, _ := testEnrollmentToken(t)
	rawCred, _ := server.NewCredentialToken(rand.Reader)
	body := `{"server_id":"server_1","enrollment_token":"` + string(enrTok) + `","credential_digest":"` + string(rawCred) + `"}`
	r := newRequestTLS(newRequest(t, pathEnroll, body, "", ""))
	w := do(h, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("raw credential token as digest: status=%d", w.Code)
	}
	if fe.calls != 0 {
		t.Fatal("Enroll must not be called on invalid input")
	}
}

// --- claim / executor_available gating -------------------------------------

func TestClaimSkipsRepositoryWhenExecutorUnavailable(t *testing.T) {
	fa := &fakeAuthenticator{}
	fc := &fakeClaimer{leases: []queue.ClaimedLease{{JobID: 1}}}
	h := Handler{Authenticator: fa, Claimer: fc}
	tok, _ := server.NewCredentialToken(rand.Reader)
	body, _ := json.Marshal(claimRequest{Limit: 1, ExecutorAvailable: false})
	r := newRequestTLS(newRequest(t, pathClaim, string(body), bearerPrefix+string(tok), "server_1"))
	w := do(h, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	if fa.calls != 1 {
		t.Fatalf("authenticate calls=%d, want 1", fa.calls)
	}
	if fc.calls != 0 {
		t.Fatalf("claim calls=%d, want 0", fc.calls)
	}
	var resp claimResponse
	decodeBody(t, w, &resp)
	if len(resp.Leases) != 0 {
		t.Fatalf("leases=%d, want 0", len(resp.Leases))
	}
}

func TestClaimCallsRepositoryWhenExecutorAvailable(t *testing.T) {
	fa := &fakeAuthenticator{}
	now := time.Now()
	fc := &fakeClaimer{leases: []queue.ClaimedLease{{JobID: 1, AttemptID: 2, Fence: 3, EnvironmentID: "env", DeploymentID: "dep", RepositoryID: 9, Ref: "refs/heads/main", CommitSHA: "abc", LeaseExpiresAt: now, AuthorityExpiresAt: now}}}
	h := Handler{Authenticator: fa, Claimer: fc}
	tok, _ := server.NewCredentialToken(rand.Reader)
	body, _ := json.Marshal(claimRequest{Limit: 1, ExecutorAvailable: true})
	r := newRequestTLS(newRequest(t, pathClaim, string(body), bearerPrefix+string(tok), "server_1"))
	w := do(h, r)
	if w.Code != http.StatusOK || fc.calls != 1 {
		t.Fatalf("status=%d claim calls=%d", w.Code, fc.calls)
	}
	var resp claimResponse
	decodeBody(t, w, &resp)
	if len(resp.Leases) != 1 || resp.Leases[0].JobID != 1 {
		t.Fatalf("leases=%+v", resp.Leases)
	}
	if _, err := time.Parse(time.RFC3339Nano, resp.Leases[0].LeaseExpiresAt); err != nil {
		t.Fatalf("lease_expires_at not RFC3339Nano: %v", err)
	}
}

func TestClaimRejectsLimitOtherThanOne(t *testing.T) {
	fa := &fakeAuthenticator{}
	fc := &fakeClaimer{}
	h := Handler{Authenticator: fa, Claimer: fc}
	tok, _ := server.NewCredentialToken(rand.Reader)
	body, _ := json.Marshal(claimRequest{Limit: 2, ExecutorAvailable: true})
	r := newRequestTLS(newRequest(t, pathClaim, string(body), bearerPrefix+string(tok), "server_1"))
	w := do(h, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", w.Code)
	}
	if fa.calls != 0 || fc.calls != 0 {
		t.Fatalf("auth calls=%d claim calls=%d, want 0/0", fa.calls, fc.calls)
	}
}

// --- no secret leakage in responses -----------------------------------------

func TestNoTokenOrDigestInResponses(t *testing.T) {
	tok, digest := testCredential(t)
	h := Handler{Renewer: &fakeRenewer{result: queue.RenewalResult{LeaseExpiresAt: time.Now(), AuthorityExpiresAt: time.Now()}}}
	body, _ := json.Marshal(renewRequest{AttemptID: 1, Fence: 1, Sequence: 0})
	r := newRequestTLS(newRequest(t, pathRenew, string(body), bearerPrefix+string(tok), "server_1"))
	w := do(h, r)
	if strings.Contains(w.Body.String(), string(tok)) {
		t.Fatal("raw token present in response")
	}
	if strings.Contains(w.Body.String(), hexDigest(digest)) {
		t.Fatal("digest present in response")
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("missing no-store cache header")
	}
}

// --- deterministic complete request parsing ---------------------------------

func TestCompleteRequestShapes(t *testing.T) {
	tok, _ := server.NewCredentialToken(rand.Reader)
	var resultDigest [32]byte
	copy(resultDigest[:], bytes.Repeat([]byte{7}, 32))

	t.Run("succeeded with failure class rejected", func(t *testing.T) {
		fc := &fakeCompleter{}
		h := Handler{Completer: fc}
		body, _ := json.Marshal(completeRequest{AttemptID: 1, Fence: 1, Outcome: "succeeded", FailureClass: "transient", OperationID: "op1", ResultDigest: hexDigest(resultDigest)})
		r := newRequestTLS(newRequest(t, pathComplete, string(body), bearerPrefix+string(tok), "server_1"))
		w := do(h, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d", w.Code)
		}
		if fc.seen.callSeen {
			t.Fatal("Complete must not be called on invalid input")
		}
	})

	t.Run("failed without failure class rejected", func(t *testing.T) {
		fc := &fakeCompleter{}
		h := Handler{Completer: fc}
		body, _ := json.Marshal(completeRequest{AttemptID: 1, Fence: 1, Outcome: "failed", OperationID: "op1", ResultDigest: hexDigest(resultDigest)})
		r := newRequestTLS(newRequest(t, pathComplete, string(body), bearerPrefix+string(tok), "server_1"))
		w := do(h, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d", w.Code)
		}
	})

	t.Run("valid succeeded accepted", func(t *testing.T) {
		fc := &fakeCompleter{}
		h := Handler{Completer: fc}
		body, _ := json.Marshal(completeRequest{AttemptID: 1, Fence: 1, Outcome: "succeeded", OperationID: "op1", ResultDigest: hexDigest(resultDigest)})
		r := newRequestTLS(newRequest(t, pathComplete, string(body), bearerPrefix+string(tok), "server_1"))
		w := do(h, r)
		if w.Code != http.StatusOK || !fc.seen.callSeen || fc.seen.outcome != queue.CompletionSucceeded {
			t.Fatalf("status=%d seen=%+v", w.Code, fc.seen)
		}
	})
}
