package web

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"autodeploy/internal/auth"
	"autodeploy/internal/store/postgres"
)

// loginOnce performs GET+POST /login and returns the POST response.
func loginOnce(t *testing.T, h *Handler, password string) *httptest.ResponseRecorder {
	t.Helper()
	get := httptest.NewRecorder()
	h.ServeHTTP(get, httptest.NewRequest(http.MethodGet, setupHost+"/login", nil))
	var cookie *http.Cookie
	for _, c := range get.Result().Cookies() {
		if c.Name == loginCookieName {
			cookie = c
		}
	}
	env, err := auth.ParseLoginEnvelope(h.config.CSRFKeys, cookie.Value, h.config.Now(), h.config.PublicOrigin)
	if err != nil {
		t.Fatal(err)
	}
	csrf, _ := auth.NewLoginCSRF(h.config.CSRFKeys, env.Nonce, h.config.PublicOrigin)
	form := url.Values{"username": {"administrator"}, "password": {password}, "csrf": {csrf}}
	r := httptest.NewRequest(http.MethodPost, setupHost+"/login", strings.NewReader(form.Encode()))
	r.RemoteAddr = "192.0.2.10:1234"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", setupHost)
	r.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	return rr
}

// A control plane started before any administrator holds a calibrated policy
// P1. If the terminal CLI then bootstraps under a different calibrated policy
// P2, logins must verify against the durable P2 (no rehash demanded, no P1
// rewrite). Without lazy loading, the P2 verifier is flagged RehashNeeded
// against the stale in-memory P1 and gets rewritten to P1.
func TestLoginAdoptsDurablePolicyAfterTerminalBootstrap(t *testing.T) {
	p2 := auth.Argon2Policy{Revision: 1, MemoryKiB: 32 * 1024, Iterations: 3, Lanes: 1}
	verifier, err := auth.HashPassword("correct horse battery staple", p2, bytes.NewReader(bytes.Repeat([]byte{5}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	repo := &repositoryStub{user: postgres.LoginUser{ID: "user-1", OwnerID: "owner-1", Verifier: verifier, AuthRevision: 1, Active: true}}
	h := testHandler(t, repo) // started with calibrated P1 = 19MiB/2
	var loads atomic.Int32
	durable := false
	h.config.PolicyLoader = func(context.Context) (auth.Argon2Policy, error) {
		loads.Add(1)
		if !durable {
			return auth.Argon2Policy{}, postgres.ErrNotFound
		}
		return p2, nil
	}
	// Before the CLI bootstraps there is no durable policy: P1 stays in use.
	if got, _ := h.passwordPolicy(context.Background()); got == p2 {
		t.Fatal("adopted a policy before one existed")
	}
	durable = true // the CLI bootstrap commits P2
	rr := loginOnce(t, h, "correct horse battery staple")
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/" || !repo.completed {
		t.Fatalf("login = %d %q completed=%v", rr.Code, rr.Header().Get("Location"), repo.completed)
	}
	if repo.replacement != nil {
		t.Fatal("login rewrote the verifier under a stale in-memory policy")
	}
	// Once adopted it is cached: no further loads.
	before := loads.Load()
	loginOnce(t, h, "correct horse battery staple")
	if loads.Load() != before {
		t.Fatal("durable policy not cached after the first successful load")
	}
}

func TestPolicyLoaderErrorsKeepCalibratedPolicy(t *testing.T) {
	h := testHandler(t, &repositoryStub{})
	h.config.PolicyLoader = func(context.Context) (auth.Argon2Policy, error) { return auth.Argon2Policy{}, errors.New("db down") }
	want := h.config.PasswordPolicy
	if got, _ := h.passwordPolicy(context.Background()); got != want {
		t.Fatalf("policy = %+v", got)
	}
}

// After a /setup bootstrap the cached policy equals what was persisted.
func TestSetupCachesThePolicyItPersisted(t *testing.T) {
	s := newSetupHarness(t)
	s.h.config.PolicyLoader = func(context.Context) (auth.Argon2Policy, error) { return auth.Argon2Policy{}, postgres.ErrNotFound }
	rr := s.post(s.validPost(t))
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("setup = %d", rr.Code)
	}
	persisted := s.store.input.Policy
	if got, _ := s.h.passwordPolicy(context.Background()); got != persisted {
		t.Fatalf("cached %+v != persisted %+v", got, persisted)
	}
	s.h.config.PolicyLoader = func(context.Context) (auth.Argon2Policy, error) {
		t.Fatal("loader called after /setup persisted the policy")
		return auth.Argon2Policy{}, nil
	}
	s.h.passwordPolicy(context.Background())
}
