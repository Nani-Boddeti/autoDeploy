//go:build integration

package web

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"autodeploy/internal/auth"
	"autodeploy/internal/store/postgres"
	"autodeploy/migrations"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestSetupFlowAgainstRealPostgres drives /setup through the real
// AuthRepository (durable throttle, real consume+bootstrap transaction) and
// then signs in with the created administrator.
func TestSetupFlowAgainstRealPostgres(t *testing.T) {
	dsn := os.Getenv("AUTODEPLOY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AUTODEPLOY_TEST_DATABASE_URL is required for PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err = migrations.Apply(ctx, conn); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close(ctx)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, statement := range []string{
		`TRUNCATE installation_setup_tokens, auth_audit_events, admin_sessions, auth_throttle_buckets, auth_username_throttles, auth_throttle_state, users, installation_auth, owners RESTART IDENTITY CASCADE`,
		`INSERT INTO installation_auth(id) VALUES(1)`,
		`INSERT INTO auth_throttle_state(id) VALUES(1)`,
	} {
		if _, err = pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	repo := postgres.NewAuthRepository(pool)
	h := testHandler(t, &repositoryStub{})
	h.repository = repo
	h.config.Setup = repo
	h.config.Random = randomStream()

	code, err := auth.NewSetupToken(nil)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := auth.SetupTokenDigest(code)
	if err != nil {
		t.Fatal(err)
	}
	if err = repo.IssueSetupToken(ctx, digest, time.Hour); err != nil {
		t.Fatal(err)
	}

	cookieAndCSRF := func(path string) (*http.Cookie, string) {
		rr := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, setupHost+path, nil)
		r.RemoteAddr = "192.0.2.10:1234"
		h.ServeHTTP(rr, r)
		var cookie *http.Cookie
		for _, c := range rr.Result().Cookies() {
			if c.Name == loginCookieName {
				cookie = c
			}
		}
		if rr.Code != http.StatusOK || cookie == nil {
			t.Fatalf("GET %s = %d", path, rr.Code)
		}
		envelope, parseErr := auth.ParseLoginEnvelope(h.config.CSRFKeys, cookie.Value, h.config.Now(), h.config.PublicOrigin)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		csrf, csrfErr := auth.NewLoginCSRF(h.config.CSRFKeys, envelope.Nonce, h.config.PublicOrigin)
		if csrfErr != nil {
			t.Fatal(csrfErr)
		}
		return cookie, csrf
	}
	post := func(path string, values url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, setupHost+path, strings.NewReader(values.Encode()))
		r.RemoteAddr = "192.0.2.10:1234"
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", setupHost)
		r.AddCookie(cookie)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		return rr
	}

	cookie, csrf := cookieAndCSRF("/setup")
	wrong := post("/setup", url.Values{"code": {"setup.v1." + strings.Repeat("D", 43)}, "username": {"admin"}, "password": {setupPassword}, "confirm": {setupPassword}, "csrf": {csrf}}, cookie)
	if wrong.Code != http.StatusForbidden {
		t.Fatalf("wrong code = %d", wrong.Code)
	}
	ok := post("/setup", url.Values{"code": {code.String()}, "username": {"Admin"}, "password": {setupPassword}, "confirm": {setupPassword}, "csrf": {csrf}}, cookie)
	if ok.Code != http.StatusSeeOther || ok.Header().Get("Location") != "/login?setup=done" {
		t.Fatalf("setup = %d %q", ok.Code, ok.Header().Get("Location"))
	}
	// Route is gone, and the code cannot be replayed.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, setupHost+"/setup", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("GET /setup after bootstrap = %d", rr.Code)
	}
	// The created administrator signs in through the normal login path.
	cookie, csrf = cookieAndCSRF("/login")
	login := post("/login", url.Values{"username": {"admin"}, "password": {setupPassword}, "csrf": {csrf}}, cookie)
	sessionSet := false
	for _, c := range login.Result().Cookies() {
		sessionSet = sessionSet || c.Name == sessionCookieName && c.Value != ""
	}
	if login.Code != http.StatusSeeOther || login.Header().Get("Location") != "/" || !sessionSet {
		t.Fatalf("login = %d %q session=%v", login.Code, login.Header().Get("Location"), sessionSet)
	}
}

func randomStream() *bytes.Reader {
	stream := make([]byte, 1<<16)
	for i := range stream {
		stream[i] = byte(i*7 + i/251)
	}
	return bytes.NewReader(stream)
}

func integrationHandler(t *testing.T) (*Handler, *postgres.AuthRepository, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("AUTODEPLOY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AUTODEPLOY_TEST_DATABASE_URL is required for PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err = migrations.Apply(ctx, conn); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close(ctx)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, statement := range []string{
		`TRUNCATE installation_setup_tokens, auth_audit_events, admin_sessions, auth_throttle_buckets, auth_username_throttles, auth_throttle_state, users, installation_auth, owners RESTART IDENTITY CASCADE`,
		`INSERT INTO installation_auth(id) VALUES(1)`,
		`INSERT INTO auth_throttle_state(id) VALUES(1)`,
	} {
		if _, err = pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	repo := postgres.NewAuthRepository(pool)
	h := testHandler(t, &repositoryStub{})
	h.repository = repo
	h.config.Setup = repo
	h.config.Random = randomStream()
	return h, repo, pool
}

// roundTrip does GET then POST of a form page from remoteAddr and returns the POST response.
func roundTrip(t *testing.T, h *Handler, remoteAddr, path string, fields url.Values) *httptest.ResponseRecorder {
	t.Helper()
	get := httptest.NewRecorder()
	g := httptest.NewRequest(http.MethodGet, setupHost+path, nil)
	g.RemoteAddr = remoteAddr
	h.ServeHTTP(get, g)
	var cookie *http.Cookie
	for _, c := range get.Result().Cookies() {
		if c.Name == loginCookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatalf("GET %s from %s = %d without envelope", path, remoteAddr, get.Code)
	}
	env, err := auth.ParseLoginEnvelope(h.config.CSRFKeys, cookie.Value, h.config.Now(), h.config.PublicOrigin)
	if err != nil {
		t.Fatal(err)
	}
	csrf, _ := auth.NewLoginCSRF(h.config.CSRFKeys, env.Nonce, h.config.PublicOrigin)
	fields.Set("csrf", csrf)
	r := httptest.NewRequest(http.MethodPost, setupHost+path, strings.NewReader(fields.Encode()))
	r.RemoteAddr = remoteAddr
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", setupHost)
	r.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	return rr
}

// A flood of bad codes from one IP trips that IP's throttle but neither blocks
// a valid submit from another IP, nor throttles a login for a user literally
// named "setup-code" (the old global subject).
func TestSetupFloodFromOneIPDoesNotLockOutOperatorOrLoginUser(t *testing.T) {
	h, repo, _ := integrationHandler(t)
	ctx := context.Background()
	code, _ := auth.NewSetupToken(nil)
	digest, _ := auth.SetupTokenDigest(code)
	if err := repo.IssueSetupToken(ctx, digest, time.Hour); err != nil {
		t.Fatal(err)
	}
	const attacker, operator = "198.51.100.7:4000", "192.0.2.50:4000"
	bad := func() url.Values {
		return url.Values{"code": {"setup.v1." + strings.Repeat("F", 43)}, "username": {"setup-code"}, "password": {setupPassword}, "confirm": {setupPassword}}
	}
	throttled := false
	for i := 0; i < 200 && !throttled; i++ {
		throttled = roundTrip(t, h, attacker, "/setup", bad()).Code == http.StatusTooManyRequests
	}
	if !throttled {
		t.Fatal("the attacking IP was never throttled")
	}
	good := url.Values{"code": {code.String()}, "username": {"setup-code"}, "password": {setupPassword}, "confirm": {setupPassword}}
	if rr := roundTrip(t, h, operator, "/setup", good); rr.Code != http.StatusSeeOther {
		t.Fatalf("operator from another IP = %d", rr.Code)
	}
	// The attacker IP's /setup throttle must not leak into login for the user "setup-code".
	login := roundTrip(t, h, attacker, "/login", url.Values{"username": {"setup-code"}, "password": {setupPassword}})
	if login.Code != http.StatusSeeOther || login.Header().Get("Location") != "/" {
		t.Fatalf("login as setup-code after a /setup flood = %d %q", login.Code, login.Header().Get("Location"))
	}
}

// The control plane started before any administrator (calibrated P1). The
// terminal CLI path then persists P2; logins must verify against P2 and must
// not rewrite the stored verifier.
func TestLoginAfterTerminalBootstrapUsesDurablePolicy(t *testing.T) {
	h, repo, pool := integrationHandler(t)
	ctx := context.Background()
	h.config.PolicyLoader = repo.LoadPasswordPolicy
	p2 := auth.Argon2Policy{Revision: 1, MemoryKiB: 32 * 1024, Iterations: 3, Lanes: 1}
	if p2 == h.config.PasswordPolicy {
		t.Fatal("test needs distinct policies")
	}
	verifier, err := auth.HashPassword(setupPassword, p2, bytes.NewReader(bytes.Repeat([]byte{4}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	if err = repo.Bootstrap(ctx, postgres.BootstrapInput{OwnerID: "owner-p2", UserID: "user-p2", Username: "administrator", AuditID: "audit-p2", Verifier: verifier, Policy: p2}); err != nil {
		t.Fatal(err)
	}
	login := roundTrip(t, h, "192.0.2.10:1234", "/login", url.Values{"username": {"administrator"}, "password": {setupPassword}})
	if login.Code != http.StatusSeeOther || login.Header().Get("Location") != "/" {
		t.Fatalf("login = %d %q", login.Code, login.Header().Get("Location"))
	}
	var stored string
	if err = pool.QueryRow(ctx, `SELECT password_phc FROM users WHERE id='user-p2'`).Scan(&stored); err != nil || stored != verifier.PHC {
		t.Fatalf("stored verifier was rewritten under a stale policy: %v", err)
	}
}
