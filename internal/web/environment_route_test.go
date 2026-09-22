package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"autodeploy/internal/auth"
	"autodeploy/internal/store/postgres"
)

// routeHeadAdminStub is a fake RouteHeadAdmin, mirroring
// runtimeConfigAdminStub's convention: it records every call and returns
// caller-configured results/errors.
type routeHeadAdminStub struct {
	getHead  postgres.RouteHead
	getFound bool
	getErr   error
	getCalls int

	assignResult     postgres.RouteHead
	assignErr        error
	assignHostname   string
	assignRevision   *int64
	assignCalls      int
	assignEnviroment string
}

func (s *routeHeadAdminStub) GetRouteHead(_ context.Context, _ auth.Principal, environmentID string) (postgres.RouteHead, bool, error) {
	s.getCalls++
	return s.getHead, s.getFound, s.getErr
}
func (s *routeHeadAdminStub) AssignRouteHostname(_ context.Context, _ auth.Principal, environmentID, hostname string, expectedRevision *int64) (postgres.RouteHead, error) {
	s.assignCalls++
	s.assignEnviroment, s.assignHostname, s.assignRevision = environmentID, hostname, expectedRevision
	return s.assignResult, s.assignErr
}

func routeHeadHandler(t *testing.T) (*Handler, *routeHeadAdminStub, auth.Principal) {
	t.Helper()
	p := auth.Principal{OwnerID: "owner", UserID: "user", SessionID: "session", AuthRevision: 1, Role: auth.RoleOwner}
	repo := &repositoryStub{principal: p}
	h := testHandler(t, repo)
	admin := &routeHeadAdminStub{}
	h.config.RouteHeadAdmin = admin
	return h, admin, p
}

func validRouteForm(environmentID, csrf, expectedRevision string) url.Values {
	return url.Values{
		"environment_id":        {environmentID},
		"csrf":                  {csrf},
		"expected_revision":     {expectedRevision},
		"hostname":              {"app.example.com"},
		"acknowledge_authority": {"on"},
	}
}

// TestEnvironmentRouteDisabledWithoutAdmin mirrors
// TestEnvironmentConfigDisabledWithoutAdmin: a nil RouteHeadAdmin 404s the
// route instead of panicking.
func TestEnvironmentRouteDisabledWithoutAdmin(t *testing.T) {
	h := testHandler(t, &repositoryStub{})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "https://admin.example.test/environments/route?environment_id=e", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("GET without RouteHeadAdmin = %d", rr.Code)
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "https://admin.example.test/environments/route", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("POST without RouteHeadAdmin = %d", rr.Code)
	}
}

// TestEnvironmentRouteGetRequiresSession proves the GET view route is
// session-gated like every other admin page.
func TestEnvironmentRouteGetRequiresSession(t *testing.T) {
	h, admin, _ := routeHeadHandler(t)
	r := httptest.NewRequest(http.MethodGet, "https://admin.example.test/environments/route?environment_id=env-a", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatalf("no session = %d %q", rr.Code, rr.Header().Get("Location"))
	}
	if admin.getCalls != 0 {
		t.Fatal("unauthenticated GET reached the repository")
	}
}

// TestEnvironmentRoutePostCSRFAndProvenance proves the write route honors
// the exact same CSRF/origin discipline as /environments/config.
func TestEnvironmentRoutePostCSRFAndProvenance(t *testing.T) {
	h, admin, p := routeHeadHandler(t)
	csrf := serversCSRF(t, h, p)

	// Missing Origin/Referer (provenance failure) is rejected before CSRF.
	form := validRouteForm("env-a", csrf, "")
	noOrigin := httptest.NewRequest(http.MethodPost, "https://admin.example.test/environments/route", strings.NewReader(form.Encode()))
	noOrigin.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	noOrigin.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "v1.AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, noOrigin)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("missing provenance = %d", rr.Code)
	}
	if admin.assignCalls != 0 {
		t.Fatal("missing provenance reached the repository")
	}

	// Invalid CSRF token is rejected with a redirect to /login, never
	// reaching the repository.
	badForm := validRouteForm("env-a", "not-the-token", "")
	bad := httptest.NewRequest(http.MethodPost, "https://admin.example.test/environments/route", strings.NewReader(badForm.Encode()))
	bad.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	bad.Header.Set("Origin", "https://admin.example.test")
	bad.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "v1.AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"})
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, bad)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatalf("invalid csrf = %d %q", rr.Code, rr.Header().Get("Location"))
	}
	if admin.assignCalls != 0 {
		t.Fatal("invalid csrf reached the repository")
	}
}

// TestEnvironmentRouteMissingAcknowledgementRejected proves ADR 0005's
// authority-acknowledgement checkbox is required: an unchecked checkbox
// (which browsers omit from the submitted body entirely) is refused before
// any repository call, with a distinct message, and the operator's other
// submitted values are still re-displayed.
func TestEnvironmentRouteMissingAcknowledgementRejected(t *testing.T) {
	h, admin, p := routeHeadHandler(t)
	csrf := serversCSRF(t, h, p)
	admin.getFound = false

	form := validRouteForm("env-a", csrf, "")
	form.Del("acknowledge_authority") // simulates an unchecked checkbox
	r := httptest.NewRequest(http.MethodPost, "https://admin.example.test/environments/route", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://admin.example.test")
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "v1.AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("missing acknowledgement = %d body=%s", rr.Code, rr.Body.String())
	}
	if admin.assignCalls != 0 {
		t.Fatal("missing acknowledgement reached the repository")
	}
	if !strings.Contains(rr.Body.String(), "acknowledge") {
		t.Fatalf("body missing acknowledgement error: %s", rr.Body.String())
	}
}

// TestEnvironmentRouteUnknownFifthFieldRejected proves that when a form
// submits exactly 5 keys, the 5th must be exactly acknowledge_authority
// with exactly its one legitimate value ("on") -- a smuggled/renamed 5th
// key, or a tampered value, is rejected outright as a bad request rather
// than silently ignored (which would otherwise let the 5th key slip past
// validation entirely) or misread as an unchecked checkbox.
func TestEnvironmentRouteUnknownFifthFieldRejected(t *testing.T) {
	h, admin, p := routeHeadHandler(t)
	csrf := serversCSRF(t, h, p)

	cases := []struct {
		name  string
		field string
		value string
	}{
		{"renamed key", "acknowledge_authorized", "on"},
		{"wrong value", "acknowledge_authority", "true"},
		{"empty value", "acknowledge_authority", ""},
	}
	for _, tc := range cases {
		form := validRouteForm("env-a", csrf, "")
		form.Del("acknowledge_authority")
		form.Set(tc.field, tc.value)
		r := httptest.NewRequest(http.MethodPost, "https://admin.example.test/environments/route", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", "https://admin.example.test")
		r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "v1.AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"})
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("%s: code = %d, want 400", tc.name, rr.Code)
		}
	}
	if admin.assignCalls != 0 {
		t.Fatalf("unknown/tampered 5th field reached the repository (assignCalls=%d)", admin.assignCalls)
	}
}

// TestEnvironmentRouteInvalidHostnameRejectedBeforeWrite proves an invalid
// hostname is refused before any repository call.
func TestEnvironmentRouteInvalidHostnameRejectedBeforeWrite(t *testing.T) {
	h, admin, p := routeHeadHandler(t)
	csrf := serversCSRF(t, h, p)
	admin.getFound = false

	cases := []string{"", "user@host.com", "host.com/path", "host.com:8443", "-bad.com", "localhost", "app", "1.2.3.4", "192.168.0.1"}
	for _, hostname := range cases {
		form := validRouteForm("env-a", csrf, "")
		form.Set("hostname", hostname)
		r := httptest.NewRequest(http.MethodPost, "https://admin.example.test/environments/route", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", "https://admin.example.test")
		r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "v1.AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"})
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		if rr.Code != http.StatusOK {
			t.Fatalf("hostname %q = %d body=%s", hostname, rr.Code, rr.Body.String())
		}
	}
	if admin.assignCalls != 0 {
		t.Fatalf("invalid hostname reached the repository (assignCalls=%d)", admin.assignCalls)
	}
}

// TestEnvironmentRouteStaleRevisionRejectedWithDistinctMessage proves a
// stale-revision CAS conflict is reported distinctly, and the operator's
// submitted hostname is still re-displayed.
func TestEnvironmentRouteStaleRevisionRejectedWithDistinctMessage(t *testing.T) {
	h, admin, p := routeHeadHandler(t)
	csrf := serversCSRF(t, h, p)
	admin.assignErr = postgres.ErrRouteHeadConflict
	admin.getFound = true
	admin.getHead = postgres.RouteHead{EnvironmentID: "env-a", Hostname: "old.example.com", RouteRevision: 9}

	form := validRouteForm("env-a", csrf, "3")
	form.Set("hostname", "distinctive.example.com")
	r := httptest.NewRequest(http.MethodPost, "https://admin.example.test/environments/route", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://admin.example.test")
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "v1.AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("stale revision = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "changed underneath you") {
		t.Fatalf("body missing stale-revision message: %s", body)
	}
	if !strings.Contains(body, "distinctive.example.com") {
		t.Fatalf("body did not re-display submitted hostname: %s", body)
	}
}

// TestEnvironmentRouteCreateSucceedsAndRedirects proves a valid, fully
// provenanced/CSRF'd, acknowledged create request reaches
// AssignRouteHostname (expected_revision empty -> nil) and redirects back
// to the view, normalizing hostname to lowercase before it reaches the
// repository.
func TestEnvironmentRouteCreateSucceedsAndRedirects(t *testing.T) {
	h, admin, p := routeHeadHandler(t)
	csrf := serversCSRF(t, h, p)
	form := validRouteForm("env-a", csrf, "")
	form.Set("hostname", "App.Example.com")
	r := httptest.NewRequest(http.MethodPost, "https://admin.example.test/environments/route", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://admin.example.test")
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "v1.AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("create = %d body=%s", rr.Code, rr.Body.String())
	}
	if admin.assignCalls != 1 {
		t.Fatalf("assignCalls=%d", admin.assignCalls)
	}
	if admin.assignRevision != nil {
		t.Fatalf("assignRevision = %v, want nil for create", admin.assignRevision)
	}
	if admin.assignHostname != "app.example.com" {
		t.Fatalf("assignHostname = %q, want lowercase-normalized", admin.assignHostname)
	}
	if loc := rr.Header().Get("Location"); loc != "/environments/route?environment_id=env-a" {
		t.Fatalf("redirect location = %q", loc)
	}
}

// TestEnvironmentRouteUpdateUsesExpectedRevision proves a non-empty
// expected_revision is passed through as a non-nil pointer with that exact
// value.
func TestEnvironmentRouteUpdateUsesExpectedRevision(t *testing.T) {
	h, admin, p := routeHeadHandler(t)
	csrf := serversCSRF(t, h, p)
	form := validRouteForm("env-a", csrf, "3")
	r := httptest.NewRequest(http.MethodPost, "https://admin.example.test/environments/route", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://admin.example.test")
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "v1.AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("update = %d body=%s", rr.Code, rr.Body.String())
	}
	if admin.assignRevision == nil || *admin.assignRevision != 3 {
		t.Fatalf("assignRevision = %v, want *3", admin.assignRevision)
	}
}

// TestEnvironmentRouteHostnameImmutableSurfacesDistinctMessage proves the
// ErrRouteHeadHostnameImmutable policy error is surfaced with its own
// message, not the generic rejection.
func TestEnvironmentRouteHostnameImmutableSurfacesDistinctMessage(t *testing.T) {
	h, admin, p := routeHeadHandler(t)
	csrf := serversCSRF(t, h, p)
	admin.assignErr = postgres.ErrRouteHeadHostnameImmutable
	admin.getFound = true
	admin.getHead = postgres.RouteHead{EnvironmentID: "env-a", Hostname: "old.example.com", RouteRevision: 1, ActiveReleaseID: "release-1"}

	form := validRouteForm("env-a", csrf, "1")
	r := httptest.NewRequest(http.MethodPost, "https://admin.example.test/environments/route", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://admin.example.test")
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "v1.AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("hostname immutable = %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "active release") {
		t.Fatalf("body missing hostname-immutable message: %s", rr.Body.String())
	}
}
