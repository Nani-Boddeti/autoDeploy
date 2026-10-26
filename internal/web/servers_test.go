package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"autodeploy/internal/auth"
	"autodeploy/internal/server"
	"autodeploy/internal/store/postgres"
)

// serverAdminStub is a fake ServerAdmin: it records every call it receives
// and returns caller-configured results/errors, mirroring repositoryStub and
// githubSetupStoreStub above.
type serverAdminStub struct {
	servers      []postgres.ServerSummary
	environments []postgres.ReconcilingEnvironment
	listErr      error
	envListErr   error

	registerErr    error
	registerInput  postgres.RegisterServerInput
	registerServer server.Server
	registerExpiry time.Time
	registerCalls  int

	issueErr      error
	issueServerID string
	issueDigest   [32]byte
	issueExpiry   time.Time
	issueCalls    int

	revokeErr    error
	revokeServer string
	revokeReason string
	revokeCalls  int

	resolveErr     error
	resolveEnv     string
	resolveOutcome string
	resolveReason  string
	resolveCalls   int
}

func (s *serverAdminStub) ListServers(context.Context, auth.Principal) ([]postgres.ServerSummary, error) {
	return s.servers, s.listErr
}
func (s *serverAdminStub) RegisterServer(_ context.Context, _ auth.Principal, in postgres.RegisterServerInput) (server.Server, time.Time, error) {
	s.registerCalls++
	s.registerInput = in
	if s.registerErr != nil {
		return server.Server{}, time.Time{}, s.registerErr
	}
	result := s.registerServer
	if result.ID == "" {
		result.ID = in.ServerID
	}
	return result, s.registerExpiry, nil
}
func (s *serverAdminStub) IssueEnrollment(_ context.Context, _ auth.Principal, serverID string, digest [32]byte, _ int) (time.Time, error) {
	s.issueCalls++
	s.issueServerID, s.issueDigest = serverID, digest
	if s.issueErr != nil {
		return time.Time{}, s.issueErr
	}
	return s.issueExpiry, nil
}
func (s *serverAdminStub) RevokeServer(_ context.Context, _ auth.Principal, serverID, reason string) error {
	s.revokeCalls++
	s.revokeServer, s.revokeReason = serverID, reason
	return s.revokeErr
}
func (s *serverAdminStub) ListReconcilingEnvironments(context.Context, auth.Principal) ([]postgres.ReconcilingEnvironment, error) {
	return s.environments, s.envListErr
}
func (s *serverAdminStub) ResolveActivation(_ context.Context, _ auth.Principal, environmentID, outcome, reason string) error {
	s.resolveCalls++
	s.resolveEnv, s.resolveOutcome, s.resolveReason = environmentID, outcome, reason
	return s.resolveErr
}

func serversHandler(t *testing.T) (*Handler, *serverAdminStub, auth.Principal) {
	t.Helper()
	p := auth.Principal{OwnerID: "owner", UserID: "user", SessionID: "session", AuthRevision: 1, Role: auth.RoleOwner}
	repo := &repositoryStub{principal: p}
	h := testHandler(t, repo)
	admin := &serverAdminStub{}
	h.config.ServerAdmin = admin
	return h, admin, p
}

func serversRequest(method, target string, form url.Values, p auth.Principal, h *Handler) *http.Request {
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	r := httptest.NewRequest(method, "https://admin.example.test"+target, body)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", "https://admin.example.test")
	}
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "v1.AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"})
	return r
}

func serversCSRF(t *testing.T, h *Handler, p auth.Principal) string {
	t.Helper()
	v, err := auth.NewSessionCSRF(h.config.CSRFKeys, p.SessionID, h.config.PublicOrigin)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// TestServersDisabledWithoutAdmin verifies the nil-disables convention
// shared with GitHubSetup: with no ServerAdmin configured, every /servers
// and /environments route 404s instead of panicking on a nil interface call.
func TestServersDisabledWithoutAdmin(t *testing.T) {
	h := testHandler(t, &repositoryStub{})
	for _, target := range []string{"/servers", "/servers/register", "/servers/enrollment", "/servers/revoke", "/environments/resolve"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "https://admin.example.test"+target, nil))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("%s without ServerAdmin = %d", target, rr.Code)
		}
	}
}

func TestServersRoutesRequireSessionAndCorrectMethod(t *testing.T) {
	h, admin, p := serversHandler(t)
	cases := []struct {
		method, target string
		form           url.Values
	}{
		{http.MethodGet, "/servers", nil},
		{http.MethodPost, "/servers/register", url.Values{"csrf": {"x"}, "name": {"host-1"}}},
		{http.MethodPost, "/servers/enrollment", url.Values{"csrf": {"x"}, "server_id": {"server_a"}}},
		{http.MethodPost, "/servers/revoke", url.Values{"csrf": {"x"}, "server_id": {"server_a"}, "reason": {"r"}, "confirm": {"revoke"}}},
		{http.MethodPost, "/environments/resolve", url.Values{"csrf": {"x"}, "environment_id": {"environment_a"}, "outcome": {"no_activation"}, "reason": {"r"}}},
	}
	for _, tc := range cases {
		// No session cookie at all -> redirected to /login, never mutated.
		r := httptest.NewRequest(tc.method, "https://admin.example.test"+tc.target, strings.NewReader(func() string {
			if tc.form != nil {
				return tc.form.Encode()
			}
			return ""
		}()))
		if tc.form != nil {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.Header.Set("Origin", "https://admin.example.test")
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		if tc.method == http.MethodGet {
			if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
				t.Fatalf("%s without session = %d %q", tc.target, rr.Code, rr.Header().Get("Location"))
			}
		} else if rr.Code != http.StatusBadRequest && rr.Code != http.StatusSeeOther {
			t.Fatalf("%s without session unexpected = %d", tc.target, rr.Code)
		}

		// Wrong method on every route is rejected.
		wrongMethod := http.MethodPost
		if tc.method == http.MethodPost {
			wrongMethod = http.MethodGet
		}
		wr := httptest.NewRequest(wrongMethod, "https://admin.example.test"+tc.target, nil)
		wrr := httptest.NewRecorder()
		h.ServeHTTP(wrr, wr)
		if wrr.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s wrong method = %d", tc.target, wrr.Code)
		}
	}
	if admin.registerCalls != 0 || admin.issueCalls != 0 || admin.revokeCalls != 0 || admin.resolveCalls != 0 {
		t.Fatal("unauthenticated/wrong-method requests reached the repository")
	}
	_ = p
}

func TestServersRegisterCSRFAndProvenance(t *testing.T) {
	h, admin, p := serversHandler(t)
	csrf := serversCSRF(t, h, p)

	// Missing Origin/Referer (provenance failure) is rejected before CSRF is
	// even checked.
	noOrigin := httptest.NewRequest(http.MethodPost, "https://admin.example.test/servers/register", strings.NewReader(url.Values{"csrf": {csrf}, "name": {"host-1"}}.Encode()))
	noOrigin.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	noOrigin.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "v1.AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, noOrigin)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("missing provenance = %d", rr.Code)
	}

	// Invalid CSRF is rejected with a redirect to /login (same convention as
	// /logout, /github/bindings, etc: a bad session CSRF looks identical to
	// "no session").
	bad := serversRequest(http.MethodPost, "/servers/register", url.Values{"csrf": {"not-the-token"}, "name": {"host-1"}}, p, h)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, bad)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatalf("invalid csrf = %d %q", rr.Code, rr.Header().Get("Location"))
	}
	if admin.registerCalls != 0 {
		t.Fatal("invalid csrf reached the repository")
	}
}

func TestServersRegisterInvalidNameRejected(t *testing.T) {
	h, admin, p := serversHandler(t)
	csrf := serversCSRF(t, h, p)
	for _, name := range []string{"", strings.Repeat("a", 129), " leading space", "trailing space ", "control\x00char"} {
		r := serversRequest(http.MethodPost, "/servers/register", url.Values{"csrf": {csrf}, "name": {name}}, p, h)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("invalid name %q = %d", name, rr.Code)
		}
	}
	if admin.registerCalls != 0 {
		t.Fatal("invalid name reached the repository")
	}
}

// TestServersRegisterShowsTokenOnceAndNeverLeaksIt is the core enrollment-
// token safety test: the response is 200 (not a redirect, so the token is
// never placed in a Location header or URL), the body contains the raw
// token exactly once, and Cache-Control is no-store.
func TestServersRegisterShowsTokenOnceAndNeverLeaksIt(t *testing.T) {
	h, admin, p := serversHandler(t)
	admin.registerServer = server.Server{ID: "server_abc", OwnerID: p.OwnerID, Name: "host-1", ProtocolVersion: server.ProtocolVersion, Status: server.StatusActive, CreatedAt: time.Now()}
	admin.registerExpiry = time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	csrf := serversCSRF(t, h, p)
	r := serversRequest(http.MethodPost, "/servers/register", url.Values{"csrf": {csrf}, "name": {"host-1"}}, p, h)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("register = %d", rr.Code)
	}
	if rr.Header().Get("Location") != "" {
		t.Fatal("token response carried a Location header")
	}
	if rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("token response Cache-Control = %q", rr.Header().Get("Cache-Control"))
	}
	body := rr.Body.String()
	if admin.registerInput.ServerID == "" || !strings.Contains(body, "server_abc") {
		t.Fatal("response did not display the registered server")
	}
	tokenPrefix := "enr.v1."
	first := strings.Index(body, tokenPrefix)
	if first < 0 {
		t.Fatal("enrollment token was not displayed")
	}
	if strings.Contains(body[first+1:], tokenPrefix) {
		t.Fatal("enrollment token displayed more than once")
	}
	if admin.registerInput.EnrollmentTTLSeconds != enrollmentTTLSeconds || admin.registerInput.ProtocolVersion != server.ProtocolVersion {
		t.Fatal("register did not request the expected enrollment TTL/protocol")
	}
	// The digest reaching the repository must never equal an all-zero value
	// (i.e. a real digest was computed, not skipped), and must never itself
	// decode back to the raw token trivially -- it is a SHA-256 output.
	if admin.registerInput.EnrollmentDigest == ([32]byte{}) {
		t.Fatal("enrollment digest was not computed")
	}
}

func TestServersReissueEnrollmentShowsTokenOnce(t *testing.T) {
	h, admin, p := serversHandler(t)
	admin.issueExpiry = time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	csrf := serversCSRF(t, h, p)
	r := serversRequest(http.MethodPost, "/servers/enrollment", url.Values{"csrf": {csrf}, "server_id": {"server_abc"}}, p, h)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusOK || rr.Header().Get("Location") != "" {
		t.Fatalf("reissue = %d location=%q", rr.Code, rr.Header().Get("Location"))
	}
	if admin.issueServerID != "server_abc" {
		t.Fatalf("reissue targeted %q", admin.issueServerID)
	}
	if !strings.Contains(rr.Body.String(), "enr.v1.") {
		t.Fatal("reissue did not display a token")
	}

	// Repository denial (e.g. server already enrolled) renders the generic
	// error page, not a leak of which check failed.
	admin.issueErr = postgres.ErrServerAuthorizationDenied
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, serversRequest(http.MethodPost, "/servers/enrollment", url.Values{"csrf": {csrf}, "server_id": {"server_abc"}}, p, h))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("denied reissue = %d", rr.Code)
	}

	// A malformed server_id is rejected before the repository is called.
	admin.issueErr = nil
	admin.issueCalls = 0
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, serversRequest(http.MethodPost, "/servers/enrollment", url.Values{"csrf": {csrf}, "server_id": {""}}, p, h))
	if rr.Code != http.StatusBadRequest || admin.issueCalls != 0 {
		t.Fatalf("empty server_id = %d calls=%d", rr.Code, admin.issueCalls)
	}
}

func TestServersRevokeRequiresConfirmationAndReason(t *testing.T) {
	h, admin, p := serversHandler(t)
	csrf := serversCSRF(t, h, p)

	// Missing confirm field entirely: rejected by the strict field-count
	// check before the repository is ever called.
	noConfirm := serversRequest(http.MethodPost, "/servers/revoke", url.Values{"csrf": {csrf}, "server_id": {"server_abc"}, "reason": {"decommissioned"}}, p, h)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, noConfirm)
	if rr.Code != http.StatusBadRequest || admin.revokeCalls != 0 {
		t.Fatalf("missing confirm = %d calls=%d", rr.Code, admin.revokeCalls)
	}

	// Confirm present but not the required value is also rejected.
	wrongConfirm := serversRequest(http.MethodPost, "/servers/revoke", url.Values{"csrf": {csrf}, "server_id": {"server_abc"}, "reason": {"decommissioned"}, "confirm": {"yes"}}, p, h)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, wrongConfirm)
	if rr.Code != http.StatusBadRequest || admin.revokeCalls != 0 {
		t.Fatalf("wrong confirm value = %d calls=%d", rr.Code, admin.revokeCalls)
	}

	// Missing reason field: rejected the same way.
	noReason := serversRequest(http.MethodPost, "/servers/revoke", url.Values{"csrf": {csrf}, "server_id": {"server_abc"}, "confirm": {"revoke"}}, p, h)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, noReason)
	if rr.Code != http.StatusBadRequest || admin.revokeCalls != 0 {
		t.Fatalf("missing reason = %d calls=%d", rr.Code, admin.revokeCalls)
	}

	// A fully confirmed revoke reaches the repository and redirects (PRG) to
	// /servers on success.
	ok := serversRequest(http.MethodPost, "/servers/revoke", url.Values{"csrf": {csrf}, "server_id": {"server_abc"}, "reason": {"decommissioned"}, "confirm": {"revoke"}}, p, h)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, ok)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/servers" || admin.revokeCalls != 1 {
		t.Fatalf("confirmed revoke = %d location=%q calls=%d", rr.Code, rr.Header().Get("Location"), admin.revokeCalls)
	}
	if admin.revokeServer != "server_abc" || admin.revokeReason != "decommissioned" {
		t.Fatal("revoke did not forward server/reason")
	}

	// Repository denial (invalid reason, unknown/cross-owner server, etc.)
	// renders the generic error page.
	admin.revokeErr = postgres.ErrServerAuthorizationDenied
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, serversRequest(http.MethodPost, "/servers/revoke", url.Values{"csrf": {csrf}, "server_id": {"server_abc"}, "reason": {"decommissioned"}, "confirm": {"revoke"}}, p, h))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("denied revoke = %d", rr.Code)
	}
}

// resolveForm is a well-formed resolve body; tests override single keys.
func resolveForm(csrf string, override map[string][]string) url.Values {
	v := url.Values{"csrf": {csrf}, "environment_id": {"environment_a"}, "outcome": {postgres.ActivationOutcomeNoActivation}, "reason": {"agent lost, manually verified clean"}}
	for k, val := range override {
		if val == nil {
			delete(v, k)
		} else {
			v[k] = val
		}
	}
	return v
}

func postResolve(h *Handler, p auth.Principal, form url.Values) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, serversRequest(http.MethodPost, "/environments/resolve", form, p, h))
	return rr
}

func TestEnvironmentsResolveForwardsEachOutcome(t *testing.T) {
	for _, outcome := range []string{postgres.ActivationOutcomeActivated, postgres.ActivationOutcomeNotActivated, postgres.ActivationOutcomeNoActivation} {
		t.Run(outcome, func(t *testing.T) {
			h, admin, p := serversHandler(t)
			csrf := serversCSRF(t, h, p)
			rr := postResolve(h, p, resolveForm(csrf, map[string][]string{"outcome": {outcome}}))
			if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/servers" || admin.resolveCalls != 1 {
				t.Fatalf("resolve = %d location=%q calls=%d", rr.Code, rr.Header().Get("Location"), admin.resolveCalls)
			}
			if admin.resolveEnv != "environment_a" || admin.resolveOutcome != outcome || admin.resolveReason != "agent lost, manually verified clean" {
				t.Fatalf("resolve forwarded env=%q outcome=%q reason=%q", admin.resolveEnv, admin.resolveOutcome, admin.resolveReason)
			}
		})
	}
}

func TestEnvironmentsResolveRefusesMalformedBodiesWithoutRepositoryCall(t *testing.T) {
	h, admin, p := serversHandler(t)
	csrf := serversCSRF(t, h, p)
	cases := map[string]url.Values{
		"no outcome key":        resolveForm(csrf, map[string][]string{"outcome": nil}),
		"extra key":             resolveForm(csrf, map[string][]string{"extra": {"x"}}),
		"duplicate outcome":     resolveForm(csrf, map[string][]string{"outcome": {"no_activation", "no_activation"}}),
		"duplicate reason":      resolveForm(csrf, map[string][]string{"reason": {"a", "b"}}),
		"duplicate environment": resolveForm(csrf, map[string][]string{"environment_id": {"environment_a", "environment_a"}}),
		"empty outcome":         resolveForm(csrf, map[string][]string{"outcome": {""}}),
		"unknown outcome":       resolveForm(csrf, map[string][]string{"outcome": {"reconciled"}}),
		"outcome wrong case":    resolveForm(csrf, map[string][]string{"outcome": {"Activated"}}),
		"outcome padded":        resolveForm(csrf, map[string][]string{"outcome": {"activated "}}),
		"empty environment":     resolveForm(csrf, map[string][]string{"environment_id": {""}}),
		"empty reason":          resolveForm(csrf, map[string][]string{"reason": {""}}),
		"spaces-only reason":    resolveForm(csrf, map[string][]string{"reason": {"   "}}),
		"tab-only reason":       resolveForm(csrf, map[string][]string{"reason": {"\t"}}),
	}
	for name, form := range cases {
		t.Run(name, func(t *testing.T) {
			rr := postResolve(h, p, form)
			if rr.Code != http.StatusBadRequest || admin.resolveCalls != 0 {
				t.Fatalf("%s = %d calls=%d, want 400 and no repository call", name, rr.Code, admin.resolveCalls)
			}
		})
	}
}

// A non-blank reason is forwarded exactly as posted, spaces included.
func TestEnvironmentsResolveForwardsReasonUntrimmed(t *testing.T) {
	h, admin, p := serversHandler(t)
	rr := postResolve(h, p, resolveForm(serversCSRF(t, h, p), map[string][]string{"reason": {"  padded reason  "}}))
	if rr.Code != http.StatusSeeOther || admin.resolveCalls != 1 || admin.resolveReason != "  padded reason  " {
		t.Fatalf("padded reason = %d calls=%d forwarded=%q", rr.Code, admin.resolveCalls, admin.resolveReason)
	}
}

func TestEnvironmentsResolveReasonBoundIsInBytes(t *testing.T) {
	h, admin, p := serversHandler(t)
	csrf := serversCSRF(t, h, p)
	// Together with the longest outcome prefix the maximum reason is exactly
	// the repository's 512-byte audit bound.
	if got := len("outcome="+postgres.ActivationOutcomeNotActivated+"; ") + maxResolveReasonBytes; got != 512 {
		t.Fatalf("longest prefix + max reason = %d bytes, want 512", got)
	}
	atMax := strings.Repeat("a", maxResolveReasonBytes)
	if rr := postResolve(h, p, resolveForm(csrf, map[string][]string{"reason": {atMax}})); rr.Code != http.StatusSeeOther || admin.resolveCalls != 1 {
		t.Fatalf("reason at max = %d calls=%d", rr.Code, admin.resolveCalls)
	}
	// One byte over.
	admin.resolveCalls = 0
	if rr := postResolve(h, p, resolveForm(csrf, map[string][]string{"reason": {atMax + "a"}})); rr.Code != http.StatusBadRequest || admin.resolveCalls != 0 {
		t.Fatalf("reason one byte over = %d calls=%d", rr.Code, admin.resolveCalls)
	}
	// Multibyte: fewer characters than the maximum but more bytes. Two-byte
	// runes, so max/2+1 of them is max+1 bytes (max is odd) while
	// being well under max characters.
	multi := strings.Repeat("é", maxResolveReasonBytes/2+1)
	if len([]rune(multi)) >= maxResolveReasonBytes || len(multi) != maxResolveReasonBytes+1 {
		t.Fatalf("setup: %d runes %d bytes", len([]rune(multi)), len(multi))
	}
	if rr := postResolve(h, p, resolveForm(csrf, map[string][]string{"reason": {multi}})); rr.Code != http.StatusBadRequest || admin.resolveCalls != 0 {
		t.Fatalf("multibyte reason over the byte bound = %d calls=%d", rr.Code, admin.resolveCalls)
	}
	// Multibyte exactly at the bound is accepted.
	exact := strings.Repeat("é", maxResolveReasonBytes/2) + "a"
	if len(exact) != maxResolveReasonBytes {
		t.Fatalf("setup: %d bytes", len(exact))
	}
	if rr := postResolve(h, p, resolveForm(csrf, map[string][]string{"reason": {exact}})); rr.Code != http.StatusSeeOther || admin.resolveCalls != 1 {
		t.Fatalf("multibyte reason at the byte bound = %d calls=%d", rr.Code, admin.resolveCalls)
	}
}

func TestEnvironmentsResolveErrorMapping(t *testing.T) {
	noActivation := map[string][]string{"outcome": {postgres.ActivationOutcomeNoActivation}}
	notActivated := map[string][]string{"outcome": {postgres.ActivationOutcomeNotActivated}}
	activated := map[string][]string{"outcome": {postgres.ActivationOutcomeActivated}}
	cases := []struct {
		name     string
		err      error
		override map[string][]string
		status   int
		body     string
	}{
		{"mismatch no_activation declared", postgres.ErrAgentLeaseOutcomeMismatch, noActivation, http.StatusConflict, "this block was caused by an activation; declare activated or not_activated\n"},
		{"mismatch not_activated declared", postgres.ErrAgentLeaseOutcomeMismatch, notActivated, http.StatusConflict, "this block was not caused by an activation; declare no_activation\n"},
		{"mismatch activated declared", fmt.Errorf("x: %w", postgres.ErrAgentLeaseOutcomeMismatch), activated, http.StatusConflict, "this block was not caused by an activation; declare no_activation\n"},
		{"unresolved", postgres.ErrAgentLeaseActivationUnresolved, activated, http.StatusConflict, "the activation switch is still authorized and cannot be resolved yet; retry once it requires reconciliation\n"},
		{"route moved", fmt.Errorf("x: %w", postgres.ErrAgentLeaseActivationRouteMoved), activated, http.StatusConflict, "the route moved since the activation was authorized, so activated is impossible; declare not_activated\n"},
		{"integrity conflict", postgres.ErrAgentLeaseIntegrityConflict, notActivated, http.StatusConflict, "this environment was already resolved with a different outcome\n"},
		{"corrupt state", postgres.ErrAgentLeaseCorruptState, activated, http.StatusInternalServerError, "environment is in an inconsistent state; it cannot be resolved from this form\n"},
		{"wrapped corrupt state leaks no detail", fmt.Errorf("%w: attempt 77 secret-detail", postgres.ErrAgentLeaseCorruptState), activated, http.StatusInternalServerError, "environment is in an inconsistent state; it cannot be resolved from this form\n"},
		{"corrupt state joined with mismatch", errors.Join(postgres.ErrAgentLeaseOutcomeMismatch, fmt.Errorf("%w: d", postgres.ErrAgentLeaseCorruptState)), activated, http.StatusInternalServerError, "environment is in an inconsistent state; it cannot be resolved from this form\n"},
		{"denied", postgres.ErrServerAuthorizationDenied, activated, http.StatusBadRequest, "bad request\n"},
		{"wrapped denial", fmt.Errorf("lock: %w", postgres.ErrServerAuthorizationDenied), activated, http.StatusBadRequest, "bad request\n"},
		{"wrapped invalid input", fmt.Errorf("x: %w", postgres.ErrAgentLeaseInvalidInput), activated, http.StatusBadRequest, "bad request\n"},
		{"invalid input", postgres.ErrAgentLeaseInvalidInput, activated, http.StatusBadRequest, "bad request\n"},
		{"unexpected failure", errors.New("db down: password=hunter2"), activated, http.StatusInternalServerError, "the environment could not be resolved; retry\n"},
		{"cancelled context", context.Canceled, activated, http.StatusInternalServerError, "the environment could not be resolved; retry\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, admin, p := serversHandler(t)
			admin.resolveErr = tc.err
			rr := postResolve(h, p, resolveForm(serversCSRF(t, h, p), tc.override))
			if rr.Code != tc.status || rr.Body.String() != tc.body {
				t.Fatalf("%s = %d %q, want %d %q", tc.name, rr.Code, rr.Body.String(), tc.status, tc.body)
			}
			if admin.resolveCalls != 1 {
				t.Fatalf("repository calls = %d, want 1", admin.resolveCalls)
			}
			if strings.Contains(rr.Body.String(), "db down") || strings.Contains(rr.Body.String(), "hunter2") {
				t.Fatalf("body leaks the error: %q", rr.Body.String())
			}
		})
	}
}

func TestEnvironmentsResolveRequiresSessionCSRFAndProvenance(t *testing.T) {
	h, admin, p := serversHandler(t)
	rr := postResolve(h, p, resolveForm("not-a-csrf-token", nil))
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" || admin.resolveCalls != 0 {
		t.Fatalf("bad csrf = %d location=%q calls=%d", rr.Code, rr.Header().Get("Location"), admin.resolveCalls)
	}
	r := serversRequest(http.MethodPost, "/environments/resolve", resolveForm(serversCSRF(t, h, p), nil), p, h)
	r.Header.Set("Origin", "https://evil.example.test")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusBadRequest || admin.resolveCalls != 0 {
		t.Fatalf("foreign origin = %d calls=%d", rr.Code, admin.resolveCalls)
	}
}

// The old path is no longer routed: the form posts only to /environments/resolve.
func TestEnvironmentsMarkReconciledPathIsGone(t *testing.T) {
	h, admin, p := serversHandler(t)
	rr := postResolve(h, p, resolveForm(serversCSRF(t, h, p), nil))
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("setup: resolve = %d", rr.Code)
	}
	admin.resolveCalls = 0
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, serversRequest(http.MethodPost, "/environments/mark-reconciled", resolveForm(serversCSRF(t, h, p), nil), p, h))
	if rr.Code != http.StatusNotFound || admin.resolveCalls != 0 {
		t.Fatalf("old path = %d calls=%d, want 404 and no call", rr.Code, admin.resolveCalls)
	}
}

func TestServersListRendersAndEscapesNamesAndRepositoryDenialIsGeneric(t *testing.T) {
	h, admin, p := serversHandler(t)
	admin.servers = []postgres.ServerSummary{
		{ID: "server_abc", Name: "<script>alert(1)</script>", Status: server.StatusActive, CreatedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), ActiveCredentialVersion: 1, Enrollment: postgres.EnrollmentConsumed},
	}
	admin.environments = []postgres.ReconcilingEnvironment{
		{EnvironmentID: "environment_a", AttemptID: 5, JobID: 9, ServerID: "server_abc", Since: time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)},
		{EnvironmentID: "environment_b", AttemptID: 6, JobID: 10, ServerID: "server_abc", Since: time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC), Activation: "reconciliation_required"},
	}
	r := serversRequest(http.MethodGet, "/servers", nil, p, h)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("list = %d", rr.Code)
	}
	body := rr.Body.String()
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Fatal("server name was not HTML-escaped: XSS")
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Fatal("expected escaped server name in output")
	}
	if !strings.Contains(body, "server_abc") || !strings.Contains(body, "environment_a") {
		t.Fatal("list did not render server/environment identifiers")
	}
	if !strings.Contains(body, "name=\"csrf\"") {
		t.Fatal("list forms are missing CSRF tokens")
	}
	// The resolve form: a required select whose first option is the empty
	// "Choose outcome" and nothing is preselected, the three exact values,
	// the byte-derived maxlength, and the Activation column.
	if got := strings.Count(body, `action="/environments/resolve"`); got != 2 {
		t.Fatalf("resolve forms = %d, want one per reconciling environment", got)
	}
	if strings.Contains(body, "mark-reconciled") {
		t.Fatal("page still references the old mark-reconciled path")
	}
	if got := strings.Count(body, `<select name="outcome" required autocomplete="off"><option value="">Choose outcome</option>`); got != 2 {
		t.Fatalf("required outcome selects with an empty first option = %d, want 2", got)
	}
	if strings.Contains(body, "selected") {
		t.Fatal("an outcome option is preselected")
	}
	for _, value := range []string{`value="activated"`, `value="not_activated"`, `value="no_activation"`} {
		if got := strings.Count(body, "<option "+value+">"); got != 2 {
			t.Fatalf("option %s appears %d times, want 2", value, got)
		}
	}
	if want := fmt.Sprintf(`<input name="reason" required autocomplete="off" maxlength="%d">`, maxResolveReasonBytes); strings.Count(body, want) != 2 {
		t.Fatalf("resolve reason input with maxlength %d not rendered for both rows", maxResolveReasonBytes)
	}
	if !strings.Contains(body, "<th>Activation</th>") || !strings.Contains(body, "<td>none</td>") || !strings.Contains(body, "<td>reconciliation_required</td>") {
		t.Fatal("Activation column missing or does not render none / the activation state")
	}
	if !strings.Contains(body, ">Resolve</button>") {
		t.Fatal("resolve button missing")
	}

	// A repository read failure renders the generic error page, not a
	// distinguishable failure.
	admin.listErr = errors.New("db down")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, serversRequest(http.MethodGet, "/servers", nil, p, h))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("denied list = %d", rr.Code)
	}
	admin.listErr = nil
	admin.envListErr = errors.New("db down")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, serversRequest(http.MethodGet, "/servers", nil, p, h))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("denied environment list = %d", rr.Code)
	}
}

func TestLandingPageLinksToServers(t *testing.T) {
	principal := auth.Principal{UserID: "user-1", OwnerID: "owner-1", Role: auth.RoleOwner, AuthRevision: 1, SessionID: "session-1"}
	repo := &repositoryStub{principal: principal}
	h := testHandler(t, repo)
	r := httptest.NewRequest(http.MethodGet, "https://admin.example.test/", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "v1.AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `href="/servers"`) {
		t.Fatal("landing page is missing a link to /servers")
	}
}
