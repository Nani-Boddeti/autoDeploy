package web

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"autodeploy/internal/auth"
	"autodeploy/internal/store/postgres"
)

const setupHost = "https://admin.example.test"

type setupStub struct {
	open      bool
	openErr   error
	bootErr   error
	calls     int
	digest    [32]byte
	input     postgres.BootstrapInput
	wantToken auth.SetupToken
	liveCalls int
	liveErr   error
}

func (s *setupStub) SetupCodeLive(_ context.Context, digest [32]byte) (bool, error) {
	s.liveCalls++
	want, err := auth.SetupTokenDigest(s.wantToken)
	return err == nil && digest == want, s.liveErr
}
func (s *setupStub) SetupAvailable(context.Context) (bool, error) { return s.open, s.openErr }
func (s *setupStub) BootstrapWithSetupToken(_ context.Context, digest [32]byte, in postgres.BootstrapInput) error {
	s.calls++
	s.digest, s.input = digest, in
	if s.bootErr != nil {
		return s.bootErr
	}
	want, err := auth.SetupTokenDigest(s.wantToken)
	if err != nil || digest != want {
		return postgres.ErrSetupCodeInvalid
	}
	return nil
}

type setupHarness struct {
	hashes *atomic.Int32
	h      *Handler
	repo   *repositoryStub
	store  *setupStub
	logs   *bytes.Buffer
	code   auth.SetupToken
}

func newSetupHarness(t *testing.T) *setupHarness {
	t.Helper()
	repo := &repositoryStub{}
	h := testHandler(t, repo)
	code, err := auth.NewSetupToken(bytes.NewReader(bytes.Repeat([]byte{42}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	// Distinct identifiers need non-repeating randomness (the shared test
	// handler's reader repeats one byte).
	stream := make([]byte, 1<<14)
	for i := range stream {
		stream[i] = byte(i * 7)
	}
	h.config.Random = bytes.NewReader(stream)
	var hashCalls atomic.Int32
	store := &setupStub{open: true, wantToken: code}
	logs := &bytes.Buffer{}
	h.config.Setup = store
	h.hashPassword = func(password string, policy auth.Argon2Policy, r io.Reader) (auth.PasswordVerifier, error) {
		hashCalls.Add(1)
		return auth.HashPassword(password, policy, r)
	}
	h.config.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &setupHarness{hashes: &hashCalls, h: h, repo: repo, store: store, logs: logs, code: code}
}

// form fetches /setup and returns the cookie and CSRF a browser would post back.
func (s *setupHarness) form(t *testing.T) (*http.Cookie, string) {
	t.Helper()
	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, setupHost+"/setup", nil)
	r.RemoteAddr = "192.0.2.10:1234"
	s.h.ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /setup = %d", rr.Code)
	}
	var cookie *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == loginCookieName {
			cookie = c
		}
	}
	if cookie == nil || !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatal("setup page must set the strict __Host- envelope cookie")
	}
	envelope, err := auth.ParseLoginEnvelope(s.h.config.CSRFKeys, cookie.Value, s.h.config.Now(), s.h.config.PublicOrigin)
	if err != nil {
		t.Fatal(err)
	}
	csrf, err := auth.NewLoginCSRF(s.h.config.CSRFKeys, envelope.Nonce, s.h.config.PublicOrigin)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rr.Body.String(), csrf) {
		t.Fatal("rendered form does not carry the CSRF token")
	}
	return cookie, csrf
}

type setupPost struct {
	code, username, password, confirm string
	csrf                              string
	cookie                            *http.Cookie
	origin                            string // "" means the valid origin; "-" means none
}

func (s *setupHarness) post(p setupPost) *httptest.ResponseRecorder {
	values := url.Values{"code": {p.code}, "username": {p.username}, "password": {p.password}, "confirm": {p.confirm}, "csrf": {p.csrf}}
	r := httptest.NewRequest(http.MethodPost, setupHost+"/setup", strings.NewReader(values.Encode()))
	r.RemoteAddr = "192.0.2.10:1234"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	switch p.origin {
	case "":
		r.Header.Set("Origin", setupHost)
	case "-":
	default:
		r.Header.Set("Origin", p.origin)
	}
	if p.cookie != nil {
		r.AddCookie(p.cookie)
	}
	rr := httptest.NewRecorder()
	s.h.ServeHTTP(rr, r)
	return rr
}

const setupPassword = "correct horse battery staple"

func (s *setupHarness) validPost(t *testing.T) setupPost {
	cookie, csrf := s.form(t)
	return setupPost{code: s.code.String(), username: "Admin", password: setupPassword, confirm: setupPassword, csrf: csrf, cookie: cookie}
}

func TestSetupSuccessRedirectsToLoginAndBootstraps(t *testing.T) {
	s := newSetupHarness(t)
	rr := s.post(s.validPost(t))
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login?setup=done" {
		t.Fatalf("status=%d location=%q body=%q", rr.Code, rr.Header().Get("Location"), rr.Body.String())
	}
	if s.store.calls != 1 || s.store.input.Username != "admin" || s.store.input.Policy != s.h.config.PasswordPolicy {
		t.Fatalf("calls=%d input=%+v", s.store.calls, s.store.input)
	}
	if ok, err := auth.VerifyPassword(setupPassword, s.store.input.Verifier, s.h.config.PasswordPolicy); err != nil || !ok.Valid {
		t.Fatalf("stored verifier does not verify the password: %v", err)
	}
	if s.store.input.OwnerID == s.store.input.UserID || s.store.input.UserID == s.store.input.AuditID {
		t.Fatal("identifiers not distinct")
	}
	// No session is created by setup: the operator signs in normally.
	for _, c := range rr.Result().Cookies() {
		if c.Name == sessionCookieName {
			t.Fatal("setup must not mint a session")
		}
	}
	login := httptest.NewRecorder()
	s.store.open = false
	s.h.ServeHTTP(login, httptest.NewRequest(http.MethodGet, setupHost+"/login?setup=done", nil))
	if login.Code != http.StatusOK || !strings.Contains(login.Body.String(), "Administrator created") || strings.Contains(login.Body.String(), "First run?") {
		t.Fatalf("login after setup = %d %q", login.Code, login.Body.String())
	}
}

func TestSetupIsNotFoundOnceAdministratorExists(t *testing.T) {
	s := newSetupHarness(t)
	post := s.validPost(t)
	s.store.open = false
	unknown := httptest.NewRecorder()
	s.h.ServeHTTP(unknown, httptest.NewRequest(http.MethodGet, setupHost+"/no-such-path", nil))
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete} {
		rr := httptest.NewRecorder()
		s.h.ServeHTTP(rr, httptest.NewRequest(method, setupHost+"/setup", nil))
		if rr.Code != http.StatusNotFound || (method != http.MethodHead && rr.Body.String() != unknown.Body.String()) {
			t.Fatalf("%s /setup = %d %q, want the unknown-path 404 %q", method, rr.Code, rr.Body.String(), unknown.Body.String())
		}
	}
	if rr := s.post(post); rr.Code != http.StatusNotFound || s.store.calls != 0 {
		t.Fatalf("POST /setup after bootstrap = %d, bootstrap calls=%d", rr.Code, s.store.calls)
	}
	if s.repo.reserve != 0 {
		t.Fatal("a closed /setup must not touch the throttle")
	}
}

func TestSetupDisabledWithoutStoreIsNotFound(t *testing.T) {
	h := testHandler(t, &repositoryStub{})
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(method, setupHost+"/setup", nil))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("%s /setup without a store = %d", method, rr.Code)
		}
	}
}

func TestSetupStoreFailureIsUnavailableNotNotFound(t *testing.T) {
	s := newSetupHarness(t)
	s.store.openErr = errors.New("database down")
	rr := httptest.NewRecorder()
	s.h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, setupHost+"/setup", nil))
	if rr.Code != http.StatusServiceUnavailable || strings.Contains(rr.Body.String(), "database") {
		t.Fatalf("GET = %d %q", rr.Code, rr.Body.String())
	}
}

func TestSetupPostRequiresProvenanceAndCSRF(t *testing.T) {
	s := newSetupHarness(t)
	good := s.validPost(t)
	cases := map[string]func(p *setupPost){
		"bad origin":      func(p *setupPost) { p.origin = "https://evil.example.test" },
		"no origin":       func(p *setupPost) { p.origin = "-" },
		"no cookie":       func(p *setupPost) { p.cookie = nil },
		"wrong csrf":      func(p *setupPost) { p.csrf = strings.Repeat("A", len(p.csrf)) },
		"missing csrf":    func(p *setupPost) { p.csrf = "" },
		"login nonce bad": func(p *setupPost) { p.cookie = &http.Cookie{Name: loginCookieName, Value: "garbage"} },
	}
	for name, mutate := range cases {
		p := good
		mutate(&p)
		rr := s.post(p)
		if rr.Code != http.StatusBadRequest && rr.Code != http.StatusSeeOther {
			t.Errorf("%s: status %d", name, rr.Code)
		}
		if rr.Header().Get("Location") == "/login?setup=done" {
			t.Errorf("%s: accepted", name)
		}
	}
	if s.store.calls != 0 {
		t.Fatalf("a request failing provenance or CSRF reached the store %d times", s.store.calls)
	}
	// Wrong content type and unknown extra fields are refused before any work.
	r := httptest.NewRequest(http.MethodPost, setupHost+"/setup", strings.NewReader("code=x"))
	r.Header.Set("Content-Type", "text/plain")
	r.Header.Set("Origin", setupHost)
	rr := httptest.NewRecorder()
	s.h.ServeHTTP(rr, r)
	if rr.Code != http.StatusBadRequest || s.repo.reserve != 0 {
		t.Fatalf("bad content type = %d reserve=%d", rr.Code, s.repo.reserve)
	}
}

func TestSetupThrottleEngages(t *testing.T) {
	s := newSetupHarness(t)
	p := s.validPost(t)
	s.repo.blocked = true
	rr := s.post(p)
	if rr.Code != http.StatusTooManyRequests || rr.Header().Get("Retry-After") == "" {
		t.Fatalf("blocked = %d", rr.Code)
	}
	if s.store.calls != 0 {
		t.Fatal("a throttled attempt reached the bootstrap transaction")
	}
	if s.repo.reserve != 1 || len(s.repo.identities) != 1 {
		t.Fatalf("reserve=%d identities=%d", s.repo.reserve, len(s.repo.identities))
	}
	// Every counted attempt reserves first, whatever the outcome.
	s.repo.blocked = false
	s.store.bootErr = nil
	p.code = "setup.v1." + strings.Repeat("A", 43)
	before := s.repo.reserve
	s.post(p)
	if s.repo.reserve != before+1 {
		t.Fatal("wrong-code attempt did not reserve a throttle slot")
	}
	// A throttle store failure is not a bypass.
	s.repo.reserveErr = errors.New("db")
	calls := s.store.calls
	if rr = s.post(p); rr.Code < 500 || s.store.calls != calls {
		t.Fatalf("throttle failure = %d calls=%d", rr.Code, s.store.calls)
	}
}

func TestSetupWrongMalformedExpiredAndConsumedCodesShareOneResponse(t *testing.T) {
	s := newSetupHarness(t)
	var baseline string
	for index, code := range []string{
		"setup.v1." + strings.Repeat("B", 43), // well-formed, unknown
		"not-a-code",
		"",
		"enr.v1." + strings.Repeat("B", 43),
		s.code.String(), // well-formed; the store reports it expired/consumed
	} {
		s.store.wantToken = "" // store refuses everything
		p := s.validPost(t)
		p.code = code
		rr := s.post(p)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("code %q: status %d", code, rr.Code)
		}
		got := rr.Body.String()
		got = got[strings.Index(got, "<main>"):]
		got = strings.ReplaceAll(got, p.csrf, "CSRF")
		if index == 0 {
			baseline = got
			if !strings.Contains(got, "not valid or has expired") {
				t.Fatalf("missing uniform message: %s", got)
			}
		} else if got != baseline {
			t.Fatalf("code %q answered differently:\n%s\n---\n%s", code, got, baseline)
		}
	}
	if s.store.calls != 0 || s.hashes.Load() != 0 {
		t.Fatalf("a bad code must never reach the hasher or the bootstrap transaction: bootstrap=%d hashes=%d", s.store.calls, s.hashes.Load())
	}
}

func TestSetupFieldValidationRefusesBeforeBootstrap(t *testing.T) {
	s := newSetupHarness(t)
	base := s.validPost(t)
	for name, mutate := range map[string]func(p *setupPost){
		"mismatch":       func(p *setupPost) { p.confirm = setupPassword + "x" },
		"short password": func(p *setupPost) { p.password, p.confirm = "short", "short" },
		"bad username":   func(p *setupPost) { p.username = "a b" },
		"empty username": func(p *setupPost) { p.username = "" },
	} {
		p := base
		mutate(&p)
		rr := s.post(p)
		if rr.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status %d", name, rr.Code)
		}
	}
	if s.store.calls != 0 {
		t.Fatalf("invalid fields reached the store %d times", s.store.calls)
	}
}

func TestSetupAlreadyBootstrappedRaceAnswersNotFound(t *testing.T) {
	s := newSetupHarness(t)
	s.store.bootErr = postgres.ErrAlreadyBootstrapped
	if rr := s.post(s.validPost(t)); rr.Code != http.StatusNotFound {
		t.Fatalf("race loser = %d", rr.Code)
	}
}

func TestSetupStoreErrorsAreSanitized(t *testing.T) {
	s := newSetupHarness(t)
	s.store.bootErr = errors.New("pq: secret-dsn leaked")
	rr := s.post(s.validPost(t))
	if rr.Code != http.StatusInternalServerError || strings.Contains(rr.Body.String(), "secret-dsn") {
		t.Fatalf("store error = %d %q", rr.Code, rr.Body.String())
	}
}

// TestSetupNeverLogsOrEchoesCodeOrPassword drives success and every failure
// path with a debug-level logger attached and asserts neither the setup code
// nor the password appears in any log line or any response (including the
// re-rendered form).
func TestSetupNeverLogsOrEchoesCodeOrPassword(t *testing.T) {
	s := newSetupHarness(t)
	const distinctPassword = "zebra-quartz-nebula-2026!"
	var responses strings.Builder
	record := func(rr *httptest.ResponseRecorder) {
		responses.WriteString(rr.Body.String())
		for key, values := range rr.Header() {
			responses.WriteString(key + strings.Join(values, ","))
		}
	}
	attempt := func(mutate func(p *setupPost)) {
		p := s.validPost(t)
		p.password, p.confirm = distinctPassword, distinctPassword
		mutate(&p)
		record(s.post(p))
	}
	attempt(func(p *setupPost) {})                                                 // success
	attempt(func(p *setupPost) { p.code = "setup.v1." + strings.Repeat("C", 43) }) // wrong code
	attempt(func(p *setupPost) { p.confirm = "different" })                        // mismatch
	attempt(func(p *setupPost) { p.origin = "https://evil.example.test" })         // provenance
	s.repo.blocked = true
	attempt(func(p *setupPost) {}) // throttled
	s.repo.blocked = false
	s.store.bootErr = errors.New("boom")
	attempt(func(p *setupPost) {}) // store failure
	for _, secret := range []string{s.code.String(), strings.TrimPrefix(s.code.String(), "setup.v1."), distinctPassword} {
		if strings.Contains(s.logs.String(), secret) {
			t.Fatalf("a secret appears in process logs: %q", secret[:4])
		}
	}
	// The form is allowed to re-render the username but never a secret.
	for _, secret := range []string{distinctPassword, strings.TrimPrefix(s.code.String(), "setup.v1.")} {
		if strings.Contains(responses.String(), secret) {
			t.Fatalf("a secret is echoed in a response: %q", secret[:4])
		}
	}
}

func TestLoginShowsSetupHintOnlyWhileSetupIsOpen(t *testing.T) {
	s := newSetupHarness(t)
	get := func(path string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		s.h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, setupHost+path, nil))
		return rr
	}
	if rr := get("/login"); !strings.Contains(rr.Body.String(), "First run?") || !strings.Contains(rr.Body.String(), "/setup") {
		t.Fatalf("hint missing while open: %q", rr.Body.String())
	}
	s.store.open = false
	if rr := get("/login"); strings.Contains(rr.Body.String(), "First run?") {
		t.Fatal("hint still shown after bootstrap")
	}
	s.store.open, s.store.openErr = true, errors.New("down")
	if rr := get("/login"); rr.Code != http.StatusOK || strings.Contains(rr.Body.String(), "First run?") {
		t.Fatalf("login must render without the hint when the check fails: %d", rr.Code)
	}
	// Only the exact success query is accepted on /login.
	for _, path := range []string{"/login?setup=other", "/login?setup=done&x=1", "/login?x=1"} {
		if rr := get(path); rr.Code != http.StatusNotFound {
			t.Fatalf("%s = %d", path, rr.Code)
		}
	}
}

func TestSetupBadCodeNeverCallsHasherButValidCodeDoes(t *testing.T) {
	s := newSetupHarness(t)
	p := s.validPost(t)
	p.code = "setup.v1." + strings.Repeat("E", 43)
	if rr := s.post(p); rr.Code != http.StatusForbidden || s.hashes.Load() != 0 || s.store.liveCalls != 1 {
		t.Fatalf("bad code: status=%d hashes=%d liveChecks=%d", rr.Code, s.hashes.Load(), s.store.liveCalls)
	}
	p.code = s.code.String()
	if rr := s.post(p); rr.Code != http.StatusSeeOther || s.hashes.Load() != 1 {
		t.Fatalf("valid code: status=%d hashes=%d", rr.Code, s.hashes.Load())
	}
}

func TestSetupHashConcurrencyIsBounded(t *testing.T) {
	s := newSetupHarness(t)
	p := s.validPost(t)
	for range setupHashSlots {
		s.h.setupSlots <- struct{}{}
	}
	rr := s.post(p)
	if rr.Code != http.StatusServiceUnavailable || rr.Header().Get("Retry-After") == "" || s.hashes.Load() != 0 || s.store.calls != 0 {
		t.Fatalf("busy = %d retry=%q hashes=%d", rr.Code, rr.Header().Get("Retry-After"), s.hashes.Load())
	}
	<-s.h.setupSlots
	if rr = s.post(p); rr.Code != http.StatusSeeOther {
		t.Fatalf("after a slot frees = %d", rr.Code)
	}
	if len(s.h.setupSlots) != 1 {
		t.Fatalf("slot leaked: %d held", len(s.h.setupSlots))
	}
}

// Throttling is per client IP in a namespace of its own: the identity must not
// be a username/pair bucket (which a real login user could share).
func TestSetupThrottleIdentityIsIPOnlyAndNamespaced(t *testing.T) {
	s := newSetupHarness(t)
	a, err := s.h.setupIdentities("192.0.2.10")
	if err != nil || len(a) != 1 || a[0].Kind != postgres.ThrottleIP {
		t.Fatalf("identities=%+v err=%v", a, err)
	}
	b, _ := s.h.setupIdentities("192.0.2.11")
	login, err := s.h.identities("192.0.2.10", "setup-code", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if a[0].Digest == b[0].Digest {
		t.Fatal("different IPs share a setup bucket")
	}
	for _, id := range login {
		if id.Digest == a[0].Digest {
			t.Fatalf("setup bucket collides with a login %s bucket", id.Kind)
		}
	}
}

// After bootstrap, GET /setup is byte-identical to an unknown path: status,
// every header except Date, and body.
func TestSetupNotFoundIsIndistinguishableFromUnknownPath(t *testing.T) {
	s := newSetupHarness(t)
	s.store.open = false
	get := func(path string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		s.h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, setupHost+path, nil))
		return rr
	}
	setup, unknown := get("/setup"), get("/definitely-not-a-route")
	if setup.Code != unknown.Code || setup.Body.String() != unknown.Body.String() {
		t.Fatalf("status/body differ: %d %q vs %d %q", setup.Code, setup.Body.String(), unknown.Code, unknown.Body.String())
	}
	for _, h := range []http.Header{setup.Header(), unknown.Header()} {
		h.Del("Date")
	}
	if len(setup.Header()) != len(unknown.Header()) {
		t.Fatalf("headers differ: %v vs %v", setup.Header(), unknown.Header())
	}
	for key, values := range unknown.Header() {
		if strings.Join(values, ",") != strings.Join(setup.Header()[key], ",") {
			t.Fatalf("header %s differs: %v vs %v", key, setup.Header()[key], values)
		}
	}
}

// The setup=done query exists for the GET page only. Every other method on
// /login?setup=done keeps the pre-change behavior: the raw-query allowlist
// answers 404 before any handler runs.
func TestLoginSetupDoneQueryIsGetOnly(t *testing.T) {
	s := newSetupHarness(t)
	for _, method := range []string{http.MethodPost, http.MethodHead, http.MethodPut, http.MethodDelete} {
		r := httptest.NewRequest(method, setupHost+"/login?setup=done", strings.NewReader("username=a&password=b&csrf=c"))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", setupHost)
		rr := httptest.NewRecorder()
		s.h.ServeHTTP(rr, r)
		ref := httptest.NewRecorder()
		s.h.ServeHTTP(ref, httptest.NewRequest(method, setupHost+"/login?x=1", strings.NewReader("")))
		if rr.Code != http.StatusNotFound || rr.Code != ref.Code || rr.Body.String() != ref.Body.String() || s.repo.reserve != 0 {
			t.Fatalf("%s /login?setup=done = %d (reference %d), reserve=%d", method, rr.Code, ref.Code, s.repo.reserve)
		}
	}
}
