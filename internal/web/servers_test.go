package web

import (
	"context"
	"errors"
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

	markErr    error
	markEnv    string
	markReason string
	markCalls  int
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
func (s *serverAdminStub) MarkReconciled(_ context.Context, _ auth.Principal, environmentID, reason string) error {
	s.markCalls++
	s.markEnv, s.markReason = environmentID, reason
	return s.markErr
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
	for _, target := range []string{"/servers", "/servers/register", "/servers/enrollment", "/servers/revoke", "/environments/mark-reconciled"} {
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
		{http.MethodPost, "/environments/mark-reconciled", url.Values{"csrf": {"x"}, "environment_id": {"environment_a"}, "reason": {"r"}}},
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
	if admin.registerCalls != 0 || admin.issueCalls != 0 || admin.revokeCalls != 0 || admin.markCalls != 0 {
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

func TestEnvironmentsMarkReconciledRoute(t *testing.T) {
	h, admin, p := serversHandler(t)
	csrf := serversCSRF(t, h, p)
	ok := serversRequest(http.MethodPost, "/environments/mark-reconciled", url.Values{"csrf": {csrf}, "environment_id": {"environment_a"}, "reason": {"agent lost, manually verified clean"}}, p, h)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, ok)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/servers" || admin.markCalls != 1 {
		t.Fatalf("mark-reconciled = %d location=%q calls=%d", rr.Code, rr.Header().Get("Location"), admin.markCalls)
	}
	if admin.markEnv != "environment_a" || admin.markReason != "agent lost, manually verified clean" {
		t.Fatal("mark-reconciled did not forward environment/reason")
	}

	admin.markErr = errors.New("denied")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, serversRequest(http.MethodPost, "/environments/mark-reconciled", url.Values{"csrf": {csrf}, "environment_id": {"environment_a"}, "reason": {"r"}}, p, h))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("denied mark-reconciled = %d", rr.Code)
	}

	// A malformed environment_id is rejected before the repository call.
	admin.markErr = nil
	admin.markCalls = 0
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, serversRequest(http.MethodPost, "/environments/mark-reconciled", url.Values{"csrf": {csrf}, "environment_id": {""}, "reason": {"r"}}, p, h))
	if rr.Code != http.StatusBadRequest || admin.markCalls != 0 {
		t.Fatalf("empty environment_id = %d calls=%d", rr.Code, admin.markCalls)
	}
}

func TestServersListRendersAndEscapesNamesAndRepositoryDenialIsGeneric(t *testing.T) {
	h, admin, p := serversHandler(t)
	admin.servers = []postgres.ServerSummary{
		{ID: "server_abc", Name: "<script>alert(1)</script>", Status: server.StatusActive, CreatedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), ActiveCredentialVersion: 1, Enrollment: postgres.EnrollmentConsumed},
	}
	admin.environments = []postgres.ReconcilingEnvironment{
		{EnvironmentID: "environment_a", AttemptID: 5, JobID: 9, ServerID: "server_abc", Since: time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)},
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
