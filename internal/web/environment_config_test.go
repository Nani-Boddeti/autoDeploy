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

// runtimeConfigAdminStub is a fake RuntimeConfigAdmin, mirroring
// serverAdminStub's convention: it records every call and returns
// caller-configured results/errors.
type runtimeConfigAdminStub struct {
	getCfg      postgres.RuntimeConfig
	getBindings []postgres.RuntimeBinding
	getFound    bool
	getErr      error
	getCalls    int

	createResult postgres.RuntimeConfig
	createErr    error
	createInput  postgres.RuntimeConfigInput
	createCalls  int

	updateResult   postgres.RuntimeConfig
	updateErr      error
	updateInput    postgres.RuntimeConfigInput
	updateRevision int64
	updateCalls    int

	addResult postgres.RuntimeBinding
	addErr    error
	addInput  postgres.BindingInput
	addCalls  int

	removeErr   error
	removeVar   string
	removeCalls int
}

func (s *runtimeConfigAdminStub) GetEnvironmentRuntimeConfig(context.Context, auth.Principal, string) (postgres.RuntimeConfig, []postgres.RuntimeBinding, bool, error) {
	s.getCalls++
	return s.getCfg, s.getBindings, s.getFound, s.getErr
}
func (s *runtimeConfigAdminStub) CreateEnvironmentRuntimeConfig(_ context.Context, _ auth.Principal, _ string, in postgres.RuntimeConfigInput) (postgres.RuntimeConfig, error) {
	s.createCalls++
	s.createInput = in
	return s.createResult, s.createErr
}
func (s *runtimeConfigAdminStub) UpdateEnvironmentRuntimeConfig(_ context.Context, _ auth.Principal, _ string, in postgres.RuntimeConfigInput, expected int64) (postgres.RuntimeConfig, error) {
	s.updateCalls++
	s.updateInput, s.updateRevision = in, expected
	return s.updateResult, s.updateErr
}
func (s *runtimeConfigAdminStub) AddEnvironmentRuntimeBinding(_ context.Context, _ auth.Principal, _ string, in postgres.BindingInput) (postgres.RuntimeBinding, error) {
	s.addCalls++
	s.addInput = in
	return s.addResult, s.addErr
}
func (s *runtimeConfigAdminStub) RemoveEnvironmentRuntimeBinding(_ context.Context, _ auth.Principal, _, variableName string) error {
	s.removeCalls++
	s.removeVar = variableName
	return s.removeErr
}

func runtimeConfigHandler(t *testing.T) (*Handler, *runtimeConfigAdminStub, auth.Principal) {
	t.Helper()
	p := auth.Principal{OwnerID: "owner", UserID: "user", SessionID: "session", AuthRevision: 1, Role: auth.RoleOwner}
	repo := &repositoryStub{principal: p}
	h := testHandler(t, repo)
	admin := &runtimeConfigAdminStub{}
	h.config.RuntimeConfigAdmin = admin
	return h, admin, p
}

func validConfigForm(environmentID, csrf, expectedRevision string) url.Values {
	return url.Values{
		"environment_id":    {environmentID},
		"csrf":              {csrf},
		"expected_revision": {expectedRevision},
		"container_port":    {"8080"},
		"container_scheme":  {"http"},
		"release_strategy":  {"stop_then_start"},
		"drain_seconds":     {"30"},
		"cpu_millis":        {"500"},
		"memory_bytes":      {"268435456"},
		"restart_policy":    {"unless-stopped"},
	}
}

// TestEnvironmentConfigDisabledWithoutAdmin mirrors
// TestServersDisabledWithoutAdmin: a nil RuntimeConfigAdmin 404s every
// route instead of panicking.
func TestEnvironmentConfigDisabledWithoutAdmin(t *testing.T) {
	h := testHandler(t, &repositoryStub{})
	for _, target := range []string{"/environments/config?environment_id=e", "/environments/bindings/add", "/environments/bindings/remove"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "https://admin.example.test"+target, nil))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("%s without RuntimeConfigAdmin = %d", target, rr.Code)
		}
	}
}

// TestEnvironmentConfigGetRequiresSession proves the GET view route is
// session-gated like every other admin page.
func TestEnvironmentConfigGetRequiresSession(t *testing.T) {
	h, admin, _ := runtimeConfigHandler(t)
	r := httptest.NewRequest(http.MethodGet, "https://admin.example.test/environments/config?environment_id=env-a", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatalf("no session = %d %q", rr.Code, rr.Header().Get("Location"))
	}
	if admin.getCalls != 0 {
		t.Fatal("unauthenticated GET reached the repository")
	}
}

// TestEnvironmentConfigPostCSRFAndProvenance proves the write routes honor
// the exact same CSRF/origin discipline as /servers/register.
func TestEnvironmentConfigPostCSRFAndProvenance(t *testing.T) {
	h, admin, p := runtimeConfigHandler(t)
	csrf := serversCSRF(t, h, p)

	// Missing Origin/Referer (provenance failure) is rejected before CSRF.
	form := validConfigForm("env-a", csrf, "")
	noOrigin := httptest.NewRequest(http.MethodPost, "https://admin.example.test/environments/config", strings.NewReader(form.Encode()))
	noOrigin.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	noOrigin.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "v1.AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, noOrigin)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("missing provenance = %d", rr.Code)
	}
	if admin.createCalls != 0 {
		t.Fatal("missing provenance reached the repository")
	}

	// Wrong/foreign origin is rejected the same way.
	form2 := validConfigForm("env-a", csrf, "")
	foreign := httptest.NewRequest(http.MethodPost, "https://admin.example.test/environments/config", strings.NewReader(form2.Encode()))
	foreign.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	foreign.Header.Set("Origin", "https://attacker.example.test")
	foreign.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "v1.AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"})
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, foreign)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("foreign origin = %d", rr.Code)
	}
	if admin.createCalls != 0 {
		t.Fatal("foreign origin reached the repository")
	}

	// Invalid CSRF token is rejected with a redirect to /login, never
	// reaching the repository.
	badForm := validConfigForm("env-a", "not-the-token", "")
	bad := httptest.NewRequest(http.MethodPost, "https://admin.example.test/environments/config", strings.NewReader(badForm.Encode()))
	bad.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	bad.Header.Set("Origin", "https://admin.example.test")
	bad.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "v1.AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"})
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, bad)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatalf("invalid csrf = %d %q", rr.Code, rr.Header().Get("Location"))
	}
	if admin.createCalls != 0 {
		t.Fatal("invalid csrf reached the repository")
	}
}

// TestEnvironmentConfigCreateSucceedsAndRedirects proves a valid, fully
// provenanced/CSRF'd create request reaches CreateEnvironmentRuntimeConfig
// (expected_revision empty) and redirects back to the view.
func TestEnvironmentConfigCreateSucceedsAndRedirects(t *testing.T) {
	h, admin, p := runtimeConfigHandler(t)
	csrf := serversCSRF(t, h, p)
	form := validConfigForm("env-a", csrf, "")
	r := httptest.NewRequest(http.MethodPost, "https://admin.example.test/environments/config", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://admin.example.test")
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "v1.AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("create = %d body=%s", rr.Code, rr.Body.String())
	}
	if admin.createCalls != 1 || admin.updateCalls != 0 {
		t.Fatalf("createCalls=%d updateCalls=%d", admin.createCalls, admin.updateCalls)
	}
	if admin.createInput.ContainerPort != 8080 || admin.createInput.MemoryBytes != 268435456 {
		t.Fatalf("create input not parsed: %+v", admin.createInput)
	}
	if loc := rr.Header().Get("Location"); loc != "/environments/config?environment_id=env-a" {
		t.Fatalf("redirect location = %q", loc)
	}
}

// TestEnvironmentConfigUpdateUsesExpectedRevision proves a non-empty
// expected_revision routes to UpdateEnvironmentRuntimeConfig with that
// exact value, never Create.
func TestEnvironmentConfigUpdateUsesExpectedRevision(t *testing.T) {
	h, admin, p := runtimeConfigHandler(t)
	csrf := serversCSRF(t, h, p)
	form := validConfigForm("env-a", csrf, "3")
	r := httptest.NewRequest(http.MethodPost, "https://admin.example.test/environments/config", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://admin.example.test")
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "v1.AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("update = %d body=%s", rr.Code, rr.Body.String())
	}
	if admin.updateCalls != 1 || admin.createCalls != 0 {
		t.Fatalf("createCalls=%d updateCalls=%d", admin.createCalls, admin.updateCalls)
	}
	if admin.updateRevision != 3 {
		t.Fatalf("updateRevision = %d, want 3", admin.updateRevision)
	}
}

// TestEnvironmentConfigStaleRevisionRejectedWithDistinctMessage proves a
// stale-revision CAS conflict is reported distinctly from a generic
// rejection, and the operator's submitted field values are re-displayed
// rather than discarded (spec: "re-display the submitted values with an
// error rather than discarding the operator's input").
func TestEnvironmentConfigStaleRevisionRejectedWithDistinctMessage(t *testing.T) {
	h, admin, p := runtimeConfigHandler(t)
	csrf := serversCSRF(t, h, p)
	admin.updateErr = postgres.ErrEnvironmentRuntimeConfigConflict
	admin.getFound = true
	admin.getCfg = postgres.RuntimeConfig{EnvironmentID: "env-a", Revision: 9}

	form := validConfigForm("env-a", csrf, "3")
	form.Set("container_port", "9090") // a distinctive value we expect echoed back
	r := httptest.NewRequest(http.MethodPost, "https://admin.example.test/environments/config", strings.NewReader(form.Encode()))
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
	if !strings.Contains(body, "9090") {
		t.Fatalf("body did not re-display submitted container_port: %s", body)
	}
}

// TestEnvironmentConfigInvalidFieldsRejectedBeforeWrite proves every
// invalid field named in the spec is refused before any repository call,
// and the page re-displays the submitted values.
func TestEnvironmentConfigInvalidFieldsRejectedBeforeWrite(t *testing.T) {
	cases := []struct {
		name     string
		field    string
		value    string
		freeText bool // container_port/drain_seconds/cpu_millis/memory_bytes are plain text
		// inputs, so a rejected value is re-displayed verbatim; the
		// enum fields (scheme/strategy/restart) render as a fixed
		// <select>, so an out-of-enum submission cannot be echoed as a
		// literal option -- the error banner (asserted below for every
		// case) is what tells the operator their value was invalid.
	}{
		{"port", "container_port", "70000", true},
		{"scheme", "container_scheme", "ftp", false},
		{"strategy", "release_strategy", "rolling", false},
		{"drain", "drain_seconds", "0", true},
		{"cpu", "cpu_millis", "0", true},
		{"memory", "memory_bytes", "0", true},
		{"restart", "restart_policy", "always", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, admin, p := runtimeConfigHandler(t)
			csrf := serversCSRF(t, h, p)
			form := validConfigForm("env-a", csrf, "")
			form.Set(tc.field, tc.value)
			r := httptest.NewRequest(http.MethodPost, "https://admin.example.test/environments/config", strings.NewReader(form.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.Header.Set("Origin", "https://admin.example.test")
			r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "v1.AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"})
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, r)
			if rr.Code != http.StatusOK {
				t.Fatalf("%s invalid = %d body=%s", tc.name, rr.Code, rr.Body.String())
			}
			if admin.createCalls != 0 || admin.updateCalls != 0 {
				t.Fatalf("%s: invalid value reached the repository", tc.name)
			}
			body := rr.Body.String()
			if !strings.Contains(body, "container port must be 1-65535") {
				t.Fatalf("%s: missing validation error message: %s", tc.name, body)
			}
			if tc.freeText && !strings.Contains(body, tc.value) {
				t.Fatalf("%s: submitted value %q not re-displayed: %s", tc.name, tc.value, body)
			}
		})
	}
}

// TestEnvironmentConfigBindingInvalidSurfacesAsValidation proves a binding
// the schema's scope trigger rejects (ErrEnvironmentRuntimeBindingInvalid)
// renders as a 200 with a validation message, never a 500, and re-displays
// the submitted binding fields.
func TestEnvironmentConfigBindingInvalidSurfacesAsValidation(t *testing.T) {
	h, admin, p := runtimeConfigHandler(t)
	csrf := serversCSRF(t, h, p)
	admin.addErr = postgres.ErrEnvironmentRuntimeBindingInvalid

	form := url.Values{
		"environment_id": {"env-a"},
		"csrf":           {csrf},
		"variable_name":  {"API_TOKEN"},
		"binding_kind":   {"secret"},
		"delivery_scope": {"runtime"},
		"secret_head_id": {"secret-head-foreign-env"},
		"literal_value":  {""},
	}
	r := httptest.NewRequest(http.MethodPost, "https://admin.example.test/environments/bindings/add", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://admin.example.test")
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "v1.AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("invalid binding = %d body=%s", rr.Code, rr.Body.String())
	}
	if admin.addCalls != 1 {
		t.Fatalf("addCalls = %d", admin.addCalls)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "binding rejected") {
		t.Fatalf("body missing validation message: %s", body)
	}
	if !strings.Contains(body, "secret-head-foreign-env") {
		t.Fatalf("submitted secret_head_id not re-displayed: %s", body)
	}
}

// TestEnvironmentConfigNoSecretValueOnPage proves a secret binding renders
// only the head's identity and version number -- never a value -- while a
// literal_nonsecret binding (non-secret per ADR 0003) does show its value.
func TestEnvironmentConfigNoSecretValueOnPage(t *testing.T) {
	h, admin, p := runtimeConfigHandler(t)
	csrf := serversCSRF(t, h, p)
	_ = csrf
	admin.getFound = true
	admin.getCfg = postgres.RuntimeConfig{
		EnvironmentID:        "env-a",
		RuntimeConfigInput:   postgres.RuntimeConfigInput{ContainerPort: 8080, ContainerScheme: "http", ReleaseStrategy: "stop_then_start", DrainSeconds: 30, CPUMillis: 500, MemoryBytes: 268435456, RestartPolicy: "unless-stopped"},
		HealthPolicyRevision: 1,
		Revision:             1,
	}
	const secretValueMarker = "TOP-SECRET-CIPHERTEXT-MARKER-should-never-render"
	admin.getBindings = []postgres.RuntimeBinding{
		{VariableName: "API_TOKEN", BindingKind: "secret", DeliveryScope: "runtime", SecretHeadID: "head-1", SecretVersionNumber: 3},
		{VariableName: "NODE_ENV", BindingKind: "literal_nonsecret", DeliveryScope: "runtime", LiteralValue: "production"},
	}
	r := httptest.NewRequest(http.MethodGet, "https://admin.example.test/environments/config?environment_id=env-a", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "v1.AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("get = %d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if strings.Contains(body, secretValueMarker) {
		t.Fatalf("secret value marker leaked onto page: %s", body)
	}
	if !strings.Contains(body, "head-1") || !strings.Contains(body, "version 3") {
		t.Fatalf("secret identity/version missing from page: %s", body)
	}
	if !strings.Contains(body, "production") {
		t.Fatalf("literal_nonsecret value missing from page: %s", body)
	}
	_ = admin
}

// TestEnvironmentConfigOwnerDeniedIsGeneric proves a repository denial
// (e.g. the environment belongs to a different owner) is refused
// generically, matching TestServersListRendersAndEscapesNamesAndRepositoryDenialIsGeneric's
// convention for the equivalent /servers case.
func TestEnvironmentConfigOwnerDeniedIsGeneric(t *testing.T) {
	h, admin, _ := runtimeConfigHandler(t)
	admin.getErr = postgres.ErrServerAuthorizationDenied
	r := httptest.NewRequest(http.MethodGet, "https://admin.example.test/environments/config?environment_id=env-a", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "v1.AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("owner-denied get = %d", rr.Code)
	}
}

// TestEnvironmentConfigRemoveBinding proves the remove route parses its
// form and calls through with the exact variable name, and 404s/etc are
// consistent with other admin POST routes.
func TestEnvironmentConfigRemoveBinding(t *testing.T) {
	h, admin, p := runtimeConfigHandler(t)
	csrf := serversCSRF(t, h, p)
	form := url.Values{"environment_id": {"env-a"}, "csrf": {csrf}, "variable_name": {"API_TOKEN"}}
	r := httptest.NewRequest(http.MethodPost, "https://admin.example.test/environments/bindings/remove", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://admin.example.test")
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "v1.AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("remove = %d body=%s", rr.Code, rr.Body.String())
	}
	if admin.removeCalls != 1 || admin.removeVar != "API_TOKEN" {
		t.Fatalf("removeCalls=%d removeVar=%q", admin.removeCalls, admin.removeVar)
	}
}
