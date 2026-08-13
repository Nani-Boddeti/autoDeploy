package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"autodeploy/internal/auth"
	"autodeploy/internal/store/postgres"
)

type repositoryStub struct {
	user        postgres.LoginUser
	reserve     int
	completed   bool
	principal   auth.Principal
	pingErr     error
	findErr     error
	reserveErr  error
	blocked     bool
	revokeErr   error
	identities  []postgres.ThrottleIdentity
	completeErr error
	events      []string
	revokes     int
}

func (s *repositoryStub) Ping(context.Context) error { return s.pingErr }
func (s *repositoryStub) FindLoginUserByCanonicalUsername(_ context.Context, username string) (postgres.LoginUser, error) {
	s.events = append(s.events, "lookup")
	if s.findErr != nil {
		return postgres.LoginUser{}, s.findErr
	}
	if username != "administrator" {
		return postgres.LoginUser{}, postgres.ErrNotFound
	}
	return s.user, nil
}
func (s *repositoryStub) ReserveThrottle(_ context.Context, identities []postgres.ThrottleIdentity, _ int) (postgres.ThrottleReservation, error) {
	s.reserve++
	s.events = append(s.events, "reserve")
	s.identities = identities
	if len(identities) != 3 && len(identities) != 1 {
		return postgres.ThrottleReservation{}, errors.New("wrong identities")
	}
	if s.reserveErr != nil {
		return postgres.ThrottleReservation{}, s.reserveErr
	}
	if s.blocked {
		return postgres.ThrottleReservation{Allowed: false}, nil
	}
	return postgres.ThrottleReservation{Allowed: true}, nil
}
func (s *repositoryStub) CompleteLogin(_ context.Context, input postgres.CompleteLoginInput) (postgres.Session, error) {
	s.events = append(s.events, "complete")
	if s.completeErr != nil {
		return postgres.Session{}, s.completeErr
	}
	if len(input.ThrottleIdentities) != 3 || input.ThrottleIdentities[0].RecoveryUserID != s.user.ID {
		return postgres.Session{}, errors.New("missing recovery identity")
	}
	s.completed = true
	return postgres.Session{}, nil
}
func (s *repositoryStub) ValidateSession(_ context.Context, token auth.SessionToken) (auth.Principal, error) {
	if token.String() == "v1.AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE" && s.principal.Validate() == nil {
		return s.principal, nil
	}
	return auth.Principal{}, postgres.ErrUnauthenticated
}
func (s *repositoryStub) RevokeSession(_ context.Context, _ auth.SessionToken, _ auth.Principal, _ string) error {
	s.revokes++
	return s.revokeErr
}

func testHandler(t *testing.T, repository *repositoryStub) *Handler {
	t.Helper()
	policy := auth.Argon2Policy{Revision: 1, MemoryKiB: 19 * 1024, Iterations: 2, Lanes: 1}
	dummy, err := auth.BuildDummyPasswordVerifier(policy)
	if err != nil {
		t.Fatal(err)
	}
	keys := auth.CSRFKeyRing{ActiveVersion: "v1", Keys: map[string][]byte{"v1": bytes.Repeat([]byte{1}, 32)}}
	throttle := auth.UsernameThrottleKeyRing{ActiveVersion: "v1", Keys: map[string][]byte{"v1": bytes.Repeat([]byte{2}, 32)}}
	h, err := New(repository, Config{PublicOrigin: "https://admin.example.test", CSRFKeys: keys, UsernameThrottleKeys: throttle, AuthThrottleMaxRows: 8, PasswordPolicy: policy, DummyPassword: dummy, Now: func() time.Time { return time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC) }, Random: bytes.NewReader(bytes.Repeat([]byte{3}, 512))})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestRoutesAndLoginBoundary(t *testing.T) {
	policy := auth.Argon2Policy{Revision: 1, MemoryKiB: 19 * 1024, Iterations: 2, Lanes: 1}
	verifier, err := auth.HashPassword("correct horse battery staple", policy, bytes.NewReader(bytes.Repeat([]byte{5}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	repo := &repositoryStub{user: postgres.LoginUser{ID: "user-1", OwnerID: "owner-1", Verifier: verifier, AuthRevision: 1, Active: true}}
	h := testHandler(t, repo)
	request := httptest.NewRequest(http.MethodGet, "https://admin.example.test/login", nil)
	request.RemoteAddr = "192.0.2.10:1234"
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "name=\"csrf\"") || response.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("unexpected login response: %d", response.Code)
	}
	var loginCookie *http.Cookie
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == loginCookieName {
			loginCookie = cookie
		}
	}
	if loginCookie == nil || !loginCookie.Secure || !loginCookie.HttpOnly || loginCookie.SameSite != http.SameSiteStrictMode {
		t.Fatal("missing strict login cookie")
	}
	envelope, err := h.loginEnvelope(httptest.NewRequest(http.MethodGet, "https://admin.example.test/login", nil))
	_ = envelope
	_ = err
	parsed, err := auth.ParseLoginEnvelope(h.config.CSRFKeys, loginCookie.Value, h.config.Now(), h.config.PublicOrigin)
	if err != nil {
		t.Fatal(err)
	}
	csrf, err := auth.NewLoginCSRF(h.config.CSRFKeys, parsed.Nonce, h.config.PublicOrigin)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"username": {"administrator"}, "password": {"correct horse battery staple"}, "csrf": {csrf}}
	post := httptest.NewRequest(http.MethodPost, "https://admin.example.test/login", strings.NewReader(form.Encode()))
	post.RemoteAddr, post.Header = "192.0.2.10:1234", http.Header{"Content-Type": {"application/x-www-form-urlencoded"}, "Origin": {"https://admin.example.test"}}
	post.AddCookie(loginCookie)
	response = httptest.NewRecorder()
	h.ServeHTTP(response, post)
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/" || !repo.completed || repo.reserve == 0 {
		t.Fatalf("login did not complete safely: %#v", response.Result())
	}
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == sessionCookieName && (!cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode) {
			t.Fatal("invalid session cookie")
		}
	}
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		r := httptest.NewRequest(method, "https://admin.example.test/livez", nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s livez = %d", method, rr.Code)
		}
	}
}

func TestRejectsMalformedInputAndUntrustedForwarding(t *testing.T) {
	h := testHandler(t, &repositoryStub{})
	for _, target := range []string{"/login?a=b", "/unknown", "/login/"} {
		r := httptest.NewRequest(http.MethodGet, "https://admin.example.test"+target, nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("%s = %d", target, rr.Code)
		}
	}
	r := httptest.NewRequest(http.MethodPost, "https://admin.example.test/login", strings.NewReader("username=a"))
	r.RemoteAddr = "192.0.2.10:1234"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://admin.example.test")
	r.Header.Set("X-Forwarded-For", "198.51.100.2")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("untrusted forwarding = %d", rr.Code)
	}
	if got, err := h.clientIP(&http.Request{RemoteAddr: "192.0.2.1:80", Header: http.Header{"X-Forwarded-For": {"198.51.100.1"}}}); err != nil || got.String() != "192.0.2.1" {
		t.Fatal("untrusted forwarding header was not ignored")
	}
	if _, err := h.clientIP(&http.Request{RemoteAddr: "bad"}); !errors.Is(err, errBadRequest) {
		t.Fatal("bad peer accepted")
	}
}

func TestTrustedProxyChainAndReadiness(t *testing.T) {
	repo := &repositoryStub{}
	h := testHandler(t, repo)
	h.config.TrustedProxyCIDRs = []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}
	r := httptest.NewRequest(http.MethodGet, "https://admin.example.test/login", nil)
	r.RemoteAddr = "192.0.2.3:443"
	r.Header.Set("X-Forwarded-For", "198.51.100.7,192.0.2.2")
	if got, err := h.clientIP(r); err != nil || got.String() != "198.51.100.7" {
		t.Fatalf("proxy identity = %v, %v", got, err)
	}
	for _, want := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		repo.pingErr = nil
		if want != http.StatusOK {
			repo.pingErr = errors.New("down")
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "https://admin.example.test/readyz", nil))
		if rr.Code != want || strings.Contains(rr.Body.String(), "down") {
			t.Fatalf("ready = %d %q", rr.Code, rr.Body.String())
		}
	}
}

func TestRouteSpecificFormsAndProvenanceAreStrict(t *testing.T) {
	h := testHandler(t, &repositoryStub{})
	for _, body := range []string{"csrf=x", "csrf=x&csrf=y", "csrf=x&username=y"} {
		r := httptest.NewRequest(http.MethodPost, "https://admin.example.test/logout", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if strings.Contains(body, "csrf=x") {
			r.Header.Set("Origin", "https://admin.example.test")
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		if body == "csrf=x" {
			if rr.Code == http.StatusBadRequest {
				t.Fatalf("csrf-only logout form rejected")
			}
		} else if rr.Code != http.StatusBadRequest {
			t.Fatalf("logout form %q = %d", body, rr.Code)
		}
	}
	for _, headers := range []http.Header{
		{"Origin": {"https://admin.example.test/"}},
		{"Origin": {"https://admin.example.test:443"}},
		{"Origin": {""}, "Referer": {"https://admin.example.test/login"}},
		{"Referer": {"https://user@admin.example.test/login"}},
	} {
		if h.provenance(&http.Request{Header: headers}) == nil {
			t.Fatalf("accepted non-canonical provenance: %#v", headers)
		}
	}
}

func TestThrottleRotationAndMalformedUsernameAreSeparated(t *testing.T) {
	h := testHandler(t, &repositoryStub{})
	h.config.CSRFKeys = auth.CSRFKeyRing{ActiveVersion: "v2", Keys: map[string][]byte{"v1": bytes.Repeat([]byte{1}, 32), "v2": bytes.Repeat([]byte{3}, 32)}}
	legal, err := h.identities("192.0.2.1", "administrator", "", false)
	if err != nil {
		t.Fatal(err)
	}
	malformed, err := h.identities("192.0.2.1", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(legal[2].Aliases) != 1 || legal[2].Aliases[0].KeyVersion != "v1" {
		t.Fatal("IP rotation alias missing")
	}
	if legal[0].Digest == malformed[0].Digest || legal[1].Digest == malformed[1].Digest {
		t.Fatal("malformed identity collides with legal identity")
	}
}

func TestLoginEnvelopeReuseAndExpiry(t *testing.T) {
	repo := &repositoryStub{}
	h := testHandler(t, repo)
	first := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "https://admin.example.test/login", nil)
	h.ServeHTTP(first, request)
	var cookie *http.Cookie
	for _, c := range first.Result().Cookies() {
		if c.Name == loginCookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("missing login cookie")
	}
	reused := httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "https://admin.example.test/login", nil)
	request.AddCookie(cookie)
	h.ServeHTTP(reused, request)
	for _, c := range reused.Result().Cookies() {
		if c.Name == loginCookieName {
			t.Fatal("valid login envelope was replaced")
		}
	}
	h.config.Now = func() time.Time { return time.Date(2026, 8, 10, 0, 11, 0, 0, time.UTC) }
	expired := httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "https://admin.example.test/login", nil)
	request.AddCookie(cookie)
	h.ServeHTTP(expired, request)
	found := false
	for _, c := range expired.Result().Cookies() {
		if c.Name == loginCookieName {
			found = true
		}
	}
	if !found {
		t.Fatal("expired login envelope was reused")
	}
}

func TestProxyAmbiguityZoneAndInvalidForwardRotation(t *testing.T) {
	repo := &repositoryStub{}
	h := testHandler(t, repo)
	h.config.CSRFKeys = auth.CSRFKeyRing{ActiveVersion: "v2", Keys: map[string][]byte{"v1": bytes.Repeat([]byte{1}, 32), "v2": bytes.Repeat([]byte{3}, 32)}}
	h.config.TrustedProxyCIDRs = []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}
	for _, xff := range []string{"192.0.2.2", "fe80::1%eth0"} {
		r := &http.Request{RemoteAddr: "192.0.2.3:443", Header: http.Header{"X-Forwarded-For": {xff}}}
		if _, err := h.clientIP(r); !errors.Is(err, errBadRequest) {
			t.Fatalf("accepted invalid proxy chain %q", xff)
		}
		h.reserveInvalidForward(context.Background(), r)
	}
	if repo.reserve != 2 {
		t.Fatalf("invalid forwards not reserved: %d", repo.reserve)
	}
	if len(repo.identities) != 1 || len(repo.identities[0].Aliases) != 1 || repo.identities[0].Aliases[0].KeyVersion != "v1" {
		t.Fatal("invalid-forward rotation alias missing")
	}
}

func TestLogoutFailureDoesNotClearCookieAndSessionRejectionRedirects(t *testing.T) {
	principal := auth.Principal{UserID: "user-1", OwnerID: "owner-1", Role: auth.RoleOwner, AuthRevision: 1, SessionID: "session-1"}
	repo := &repositoryStub{principal: principal, revokeErr: errors.New("unavailable")}
	h := testHandler(t, repo)
	csrf, err := auth.NewSessionCSRF(h.config.CSRFKeys, principal.SessionID, h.config.PublicOrigin)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "https://admin.example.test/logout", strings.NewReader(url.Values{"csrf": {csrf}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://admin.example.test")
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "v1.AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatal("revoke failure did not fail closed")
	}
	for _, cookie := range rr.Result().Cookies() {
		if cookie.Name == sessionCookieName && cookie.MaxAge < 0 {
			t.Fatal("cookie cleared after failed revocation")
		}
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "https://admin.example.test/", nil))
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatal("missing session did not redirect")
	}
}

func TestDuplicateAndOversizedLoginFormsAreRejected(t *testing.T) {
	h := testHandler(t, &repositoryStub{})
	for _, body := range []string{"username=a&username=b&password=x&csrf=x", "username=" + strings.Repeat("a", maxBodyBytes+1)} {
		r := httptest.NewRequest(http.MethodPost, "https://admin.example.test/login", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", "https://admin.example.test")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("malformed login form = %d", rr.Code)
		}
	}
}

func TestPublicLoginFailuresHaveOneResponse(t *testing.T) {
	policy := auth.Argon2Policy{Revision: 1, MemoryKiB: 19 * 1024, Iterations: 2, Lanes: 1}
	verifier, err := auth.HashPassword("correct horse battery staple", policy, bytes.NewReader(bytes.Repeat([]byte{5}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	cases := []*repositoryStub{
		{},
		{user: postgres.LoginUser{ID: "user-1", OwnerID: "owner-1", Verifier: verifier, AuthRevision: 1, Active: true}},
		{user: postgres.LoginUser{ID: "user-1", OwnerID: "owner-1", Verifier: verifier, AuthRevision: 1, Active: false}},
		{blocked: true},
		{findErr: errors.New("repository failure")},
	}
	var baseline string
	for index, repo := range cases {
		h := testHandler(t, repo)
		get := httptest.NewRecorder()
		h.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "https://admin.example.test/login", nil))
		var cookie *http.Cookie
		for _, c := range get.Result().Cookies() {
			if c.Name == loginCookieName {
				cookie = c
			}
		}
		if cookie == nil {
			t.Fatal("missing cookie")
		}
		envelope, parseErr := auth.ParseLoginEnvelope(h.config.CSRFKeys, cookie.Value, h.config.Now(), h.config.PublicOrigin)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		csrf, tokenErr := auth.NewLoginCSRF(h.config.CSRFKeys, envelope.Nonce, h.config.PublicOrigin)
		if tokenErr != nil {
			t.Fatal(tokenErr)
		}
		form := url.Values{"username": {"administrator"}, "password": {"wrong password long enough"}, "csrf": {csrf}}
		r := httptest.NewRequest(http.MethodPost, "https://admin.example.test/login", strings.NewReader(form.Encode()))
		r.RemoteAddr = "192.0.2.10:1234"
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", "https://admin.example.test")
		r.AddCookie(cookie)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		got := fmt.Sprintf("%d|%s|%s", rr.Code, rr.Header().Get("Location"), rr.Body.String())
		if index == 0 {
			baseline = got
		} else if got != baseline {
			t.Fatalf("public login failure differs: %q != %q", got, baseline)
		}
	}
}

func TestLoginCSRFExpiryFixationAndPersistenceFailures(t *testing.T) {
	policy := auth.Argon2Policy{Revision: 1, MemoryKiB: 19 * 1024, Iterations: 2, Lanes: 1}
	verifier, err := auth.HashPassword("correct horse battery staple", policy, bytes.NewReader(bytes.Repeat([]byte{5}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name         string
		csrf         func(*Handler, auth.LoginEnvelope) string
		repo         *repositoryStub
		wantComplete bool
	}{
		{"bad csrf", func(*Handler, auth.LoginEnvelope) string { return "bad" }, &repositoryStub{user: postgres.LoginUser{ID: "user-1", OwnerID: "owner-1", Verifier: verifier, AuthRevision: 1, Active: true}}, false},
		{"reservation error", func(h *Handler, e auth.LoginEnvelope) string {
			v, _ := auth.NewLoginCSRF(h.config.CSRFKeys, e.Nonce, h.config.PublicOrigin)
			return v
		}, &repositoryStub{reserveErr: errors.New("down"), user: postgres.LoginUser{ID: "user-1", OwnerID: "owner-1", Verifier: verifier, AuthRevision: 1, Active: true}}, false},
		{"complete error", func(h *Handler, e auth.LoginEnvelope) string {
			v, _ := auth.NewLoginCSRF(h.config.CSRFKeys, e.Nonce, h.config.PublicOrigin)
			return v
		}, &repositoryStub{completeErr: errors.New("down"), user: postgres.LoginUser{ID: "user-1", OwnerID: "owner-1", Verifier: verifier, AuthRevision: 1, Active: true}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := testHandler(t, tc.repo)
			get := httptest.NewRecorder()
			h.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "https://admin.example.test/login", nil))
			var login *http.Cookie
			for _, c := range get.Result().Cookies() {
				if c.Name == loginCookieName {
					login = c
				}
			}
			e, parseErr := auth.ParseLoginEnvelope(h.config.CSRFKeys, login.Value, h.config.Now(), h.config.PublicOrigin)
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			form := url.Values{"username": {"administrator"}, "password": {"correct horse battery staple"}, "csrf": {tc.csrf(h, e)}}
			r := httptest.NewRequest(http.MethodPost, "https://admin.example.test/login", strings.NewReader(form.Encode()))
			r.RemoteAddr = "192.0.2.10:1234"
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.Header.Set("Origin", "https://admin.example.test")
			r.AddCookie(login)
			r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "v1.AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"})
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, r)
			if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" || tc.repo.completed {
				t.Fatal("failed login minted authority")
			}
			for _, c := range rr.Result().Cookies() {
				if c.Name == sessionCookieName {
					t.Fatal("failed login set session cookie")
				}
			}
		})
	}
	// A successful login must overwrite any attacker-supplied session cookie.
	repo := &repositoryStub{user: postgres.LoginUser{ID: "user-1", OwnerID: "owner-1", Verifier: verifier, AuthRevision: 1, Active: true}}
	h := testHandler(t, repo)
	get := httptest.NewRecorder()
	h.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "https://admin.example.test/login", nil))
	var login *http.Cookie
	for _, c := range get.Result().Cookies() {
		if c.Name == loginCookieName {
			login = c
		}
	}
	e, _ := auth.ParseLoginEnvelope(h.config.CSRFKeys, login.Value, h.config.Now(), h.config.PublicOrigin)
	csrf, _ := auth.NewLoginCSRF(h.config.CSRFKeys, e.Nonce, h.config.PublicOrigin)
	r := httptest.NewRequest(http.MethodPost, "https://admin.example.test/login", strings.NewReader(url.Values{"username": {"administrator"}, "password": {"correct horse battery staple"}, "csrf": {csrf}}.Encode()))
	r.RemoteAddr = "192.0.2.10:1234"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://admin.example.test")
	r.AddCookie(login)
	fixed := "v1.AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: fixed})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	found := false
	for _, c := range rr.Result().Cookies() {
		if c.Name == sessionCookieName {
			found = true
			if c.Value == fixed {
				t.Fatal("session fixation preserved")
			}
		}
	}
	if !found || !repo.completed {
		t.Fatal("successful login did not mint replacement session")
	}
}

func TestLoginEnvelopeExpiryAndLogoutCrossSessionCSRF(t *testing.T) {
	principal := auth.Principal{UserID: "user-1", OwnerID: "owner-1", Role: auth.RoleOwner, AuthRevision: 1, SessionID: "session-1"}
	repo := &repositoryStub{principal: principal}
	h := testHandler(t, repo)
	get := httptest.NewRecorder()
	h.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "https://admin.example.test/login", nil))
	var login *http.Cookie
	for _, c := range get.Result().Cookies() {
		if c.Name == loginCookieName {
			login = c
		}
	}
	e, _ := auth.ParseLoginEnvelope(h.config.CSRFKeys, login.Value, h.config.Now(), h.config.PublicOrigin)
	csrf, _ := auth.NewLoginCSRF(h.config.CSRFKeys, e.Nonce, h.config.PublicOrigin)
	h.config.Now = func() time.Time { return time.Date(2026, 8, 10, 0, 11, 0, 0, time.UTC) }
	r := httptest.NewRequest(http.MethodPost, "https://admin.example.test/login", strings.NewReader(url.Values{"username": {"administrator"}, "password": {"correct horse battery staple"}, "csrf": {csrf}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://admin.example.test")
	r.AddCookie(login)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatal("expired login envelope accepted")
	}
	if repo.reserve != 0 || len(repo.events) != 0 || repo.completed {
		t.Fatalf("expired envelope reached authentication side effects: %#v", repo.events)
	}
	for _, cookie := range rr.Result().Cookies() {
		if cookie.Name == sessionCookieName {
			t.Fatal("expired envelope minted a session cookie")
		}
	}
	cross, _ := auth.NewSessionCSRF(h.config.CSRFKeys, "other-session", h.config.PublicOrigin)
	r = httptest.NewRequest(http.MethodPost, "https://admin.example.test/logout", strings.NewReader(url.Values{"csrf": {cross}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://admin.example.test")
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "v1.AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"})
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatal("cross-session logout CSRF accepted")
	}
	if repo.revokes != 0 {
		t.Fatal("cross-session CSRF reached session revocation")
	}
	for _, cookie := range rr.Result().Cookies() {
		if cookie.Name == sessionCookieName && cookie.MaxAge < 0 {
			t.Fatal("cross-session CSRF cleared the session cookie")
		}
	}
}

func TestThrottleReservationPrecedesKnownUserLookup(t *testing.T) {
	policy := auth.Argon2Policy{Revision: 1, MemoryKiB: 19 * 1024, Iterations: 2, Lanes: 1}
	verifier, err := auth.HashPassword("correct horse battery staple", policy, bytes.NewReader(bytes.Repeat([]byte{5}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	repo := &repositoryStub{user: postgres.LoginUser{ID: "user-1", OwnerID: "owner-1", Verifier: verifier, AuthRevision: 1, Active: true}}
	h := testHandler(t, repo)
	get := httptest.NewRecorder()
	h.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "https://admin.example.test/login", nil))
	var login *http.Cookie
	for _, c := range get.Result().Cookies() {
		if c.Name == loginCookieName {
			login = c
		}
	}
	e, _ := auth.ParseLoginEnvelope(h.config.CSRFKeys, login.Value, h.config.Now(), h.config.PublicOrigin)
	csrf, _ := auth.NewLoginCSRF(h.config.CSRFKeys, e.Nonce, h.config.PublicOrigin)
	r := httptest.NewRequest(http.MethodPost, "https://admin.example.test/login", strings.NewReader(url.Values{"username": {"administrator"}, "password": {"wrong password long enough"}, "csrf": {csrf}}.Encode()))
	r.RemoteAddr = "192.0.2.10:1234"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://admin.example.test")
	r.AddCookie(login)
	h.ServeHTTP(httptest.NewRecorder(), r)
	if len(repo.events) < 2 || repo.events[0] != "reserve" || repo.events[1] != "lookup" {
		t.Fatalf("wrong throttle/user order: %#v", repo.events)
	}
}
