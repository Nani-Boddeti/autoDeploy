package web

import (
	"bytes"
	"context"
	"errors"
	"html"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"autodeploy/internal/auth"
	secretcrypto "autodeploy/internal/crypto"
	"autodeploy/internal/githubapp"
	"autodeploy/internal/store/postgres"
)

const (
	testCreateState = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ" // 43 chars
	testCreateCode  = "code_ABC-123"
	oldCSP          = "default-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; style-src 'self'; script-src 'self'; connect-src 'self'"
)

type manifestStoreFake struct {
	createErr, consumeErr, insertErr error
	created, consumed, inserted      int
	createdDigest, consumedDigest    [32]byte
	insertInput                      postgres.ManifestRegistrationInput
	consumePrincipal                 auth.Principal
	priorApp                         bool
	priorErr                         error
	consumeCtxErr, insertCtxErr      error
	order                            *[]string
}

func (s *manifestStoreFake) CreateManifestState(_ context.Context, d [32]byte, _ auth.Principal, ttl time.Duration) error {
	s.created++
	s.createdDigest = d
	if ttl != 10*time.Minute {
		return errors.New("unexpected ttl")
	}
	return s.createErr
}

func (s *manifestStoreFake) ConsumeManifestStateForCreate(ctx context.Context, d [32]byte, p auth.Principal) error {
	s.consumed++
	s.consumedDigest, s.consumePrincipal, s.consumeCtxErr = d, p, ctx.Err()
	if s.order != nil {
		*s.order = append(*s.order, "consume")
	}
	return s.consumeErr
}

func (s *manifestStoreFake) PriorAppRegistered(context.Context) (bool, error) {
	return s.priorApp, s.priorErr
}

func (s *manifestStoreFake) InsertRegistration(ctx context.Context, in postgres.ManifestRegistrationInput, _ secretcrypto.KeyProvider) (postgres.ManifestRegistration, error) {
	s.inserted++
	s.insertInput, s.insertCtxErr = in, ctx.Err()
	if s.order != nil {
		*s.order = append(*s.order, "insert")
	}
	return postgres.ManifestRegistration{}, s.insertErr
}

type converterFake struct {
	result   githubapp.ManifestConversion
	err      error
	calls    int
	gotCode  string
	deadline time.Time
	hasDL    bool
	ctxErr   error
}

func (c *converterFake) ConvertManifestCode(ctx context.Context, code string) (githubapp.ManifestConversion, error) {
	c.calls++
	c.gotCode = code
	c.deadline, c.hasDL = ctx.Deadline()
	c.ctxErr = ctx.Err()
	return c.result, c.err
}

type kekFake struct{}

func (kekFake) Active() (string, []byte, error) { return "kek1", bytes.Repeat([]byte{7}, 32), nil }
func (kekFake) Resolve(string) ([]byte, error)  { return bytes.Repeat([]byte{7}, 32), nil }

type createFixture struct {
	t        *testing.T
	h        *Handler
	p        auth.Principal
	store    *manifestStoreFake
	conv     *converterFake
	restarts int
	logs     *bytes.Buffer
	order    []string
	rr       *httptest.ResponseRecorder
	// bodyAtRestart is the response body already written when RequestRestart ran.
	bodyAtRestart string
}

func newCreateFixture(t *testing.T) *createFixture {
	t.Helper()
	f := &createFixture{t: t}
	var base *Handler
	base, _, f.p = githubHandler(t)
	f.h = base
	f.h.config.GitHubSetup, f.h.config.GitHubInstallationURL = nil, ""
	f.store = &manifestStoreFake{order: &f.order}
	f.conv = &converterFake{result: githubapp.ManifestConversion{
		AppID: 4242, Slug: "autodeploy-x", ClientID: "Iv1.client", HTMLURL: "https://github.com/apps/autodeploy-x",
		OwnerID: 9, OwnerLogin: "owner", OwnerType: "User",
		PEM: []byte("PEM-SECRET-MATERIAL"), WebhookSecret: []byte("WEBHOOK-SECRET-MATERIAL"),
	}}
	f.logs = &bytes.Buffer{}
	f.h.config.Logger = slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f.h.config.GitHubAppSource = GitHubAppSourceNone
	f.h.config.GitHubManifestStore = f.store
	f.h.config.GitHubKEKProvider = func() (secretcrypto.KeyProvider, error) { return kekFake{}, nil }
	f.h.config.GitHubManifestConverter = f.conv
	f.h.config.RequestRestart = func() {
		f.restarts++
		f.order = append(f.order, "restart")
		if f.rr != nil {
			f.bodyAtRestart = f.rr.Body.String()
		}
	}
	return f
}

func (f *createFixture) serve(r *http.Request) *httptest.ResponseRecorder {
	f.rr = httptest.NewRecorder()
	f.h.ServeHTTP(f.rr, r)
	return f.rr
}

func (f *createFixture) createForm(mutate func(url.Values)) url.Values {
	v := url.Values{"csrf": {githubCSRF(f.t, f.h, f.p)}, "account_type": {"personal"}, "org_login": {""}, "app_name": {"AutoDeploy test"}}
	if mutate != nil {
		mutate(v)
	}
	return v
}

func (f *createFixture) completeForm() url.Values {
	return url.Values{"csrf": {githubCSRF(f.t, f.h, f.p)}}
}

func withHandoff(r *http.Request, value string) *http.Request {
	r.AddCookie(&http.Cookie{Name: createHandoffCookieName, Value: value})
	return r
}

func goodHandoff() string { return testCreateState + "." + testCreateCode }

func (f *createFixture) postComplete(form url.Values, cookie string) *httptest.ResponseRecorder {
	r := githubRequest(http.MethodPost, "/github/create/complete", form, f.p, f.h)
	if cookie != "" {
		withHandoff(r, cookie)
	}
	return f.serve(r)
}

func clearsHandoff(rr *httptest.ResponseRecorder) bool {
	for _, c := range rr.Result().Cookies() {
		if c.Name == createHandoffCookieName && c.MaxAge < 0 && c.Value == "" {
			return true
		}
	}
	return false
}

func TestGitHubCreateRefusals(t *testing.T) {
	t.Run("source not none is 404 on every route", func(t *testing.T) {
		for _, source := range []string{GitHubAppSourceEnv, GitHubAppSourceDatabase} {
			f := newCreateFixture(t)
			f.h.config.GitHubAppSource = source
			for _, r := range []*http.Request{
				githubRequest(http.MethodPost, "/github/create", f.createForm(nil), f.p, f.h),
				githubRequest(http.MethodGet, "/github/create/callback?code="+testCreateCode+"&state="+testCreateState, nil, f.p, f.h),
				withHandoff(githubRequest(http.MethodGet, "/github/create/complete", nil, f.p, f.h), goodHandoff()),
				withHandoff(githubRequest(http.MethodPost, "/github/create/complete", f.completeForm(), f.p, f.h), goodHandoff()),
			} {
				if rr := f.serve(r); rr.Code != http.StatusNotFound {
					t.Errorf("%s %s source=%s = %d, want 404", r.Method, r.URL.Path, source, rr.Code)
				}
			}
			if f.store.created+f.store.consumed != 0 || f.conv.calls != 0 {
				t.Error("store or converter touched while source != none")
			}
		}
	})
	t.Run("missing dependency is 404", func(t *testing.T) {
		f := newCreateFixture(t)
		f.h.config.RequestRestart = nil
		if rr := f.serve(githubRequest(http.MethodPost, "/github/create", f.createForm(nil), f.p, f.h)); rr.Code != http.StatusNotFound {
			t.Fatalf("= %d", rr.Code)
		}
	})
	t.Run("create: no session, bad csrf, provenance, extra or missing fields", func(t *testing.T) {
		f := newCreateFixture(t)
		noSession := githubRequest(http.MethodPost, "/github/create", f.createForm(nil), f.p, f.h)
		noSession.Header.Del("Cookie")
		if rr := f.serve(noSession); rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
			t.Errorf("no session = %d %s", rr.Code, rr.Header().Get("Location"))
		}
		bad := f.createForm(func(v url.Values) { v.Set("csrf", "nope") })
		if rr := f.serve(githubRequest(http.MethodPost, "/github/create", bad, f.p, f.h)); rr.Header().Get("Location") != "/login" {
			t.Errorf("bad csrf = %d", rr.Code)
		}
		wrongOrigin := githubRequest(http.MethodPost, "/github/create", f.createForm(nil), f.p, f.h)
		wrongOrigin.Header.Set("Origin", "https://evil.example.test")
		if rr := f.serve(wrongOrigin); rr.Code != http.StatusBadRequest {
			t.Errorf("provenance = %d", rr.Code)
		}
		for name, mutate := range map[string]func(url.Values){
			"extra":   func(v url.Values) { v.Set("extra", "1") },
			"missing": func(v url.Values) { v.Del("org_login") },
			"dup":     func(v url.Values) { v.Add("app_name", "second") },
		} {
			if rr := f.serve(githubRequest(http.MethodPost, "/github/create", f.createForm(mutate), f.p, f.h)); rr.Code != http.StatusBadRequest {
				t.Errorf("%s fields = %d, want 400", name, rr.Code)
			}
		}
		if f.store.created != 0 {
			t.Fatal("state created by a refused request")
		}
	})
	t.Run("callback strict query", func(t *testing.T) {
		f := newCreateFixture(t)
		for name, target := range map[string]string{
			"extra param": "/github/create/callback?code=" + testCreateCode + "&state=" + testCreateState + "&x=1",
			"no state":    "/github/create/callback?code=" + testCreateCode,
			"no code":     "/github/create/callback?state=" + testCreateState,
			"short state": "/github/create/callback?code=" + testCreateCode + "&state=short",
			"bad code":    "/github/create/callback?code=a.b&state=" + testCreateState,
			"long code":   "/github/create/callback?code=" + strings.Repeat("a", 129) + "&state=" + testCreateState,
			"dup code":    "/github/create/callback?code=a&code=b&state=" + testCreateState,
			"bad state":   "/github/create/callback?code=" + testCreateCode + "&state=" + strings.Repeat("!", 43),
			"no query":    "/github/create/callback",
		} {
			rr := f.serve(githubRequest(http.MethodGet, target, nil, f.p, f.h))
			if rr.Code != http.StatusBadRequest {
				t.Errorf("%s = %d, want 400", name, rr.Code)
			}
			if len(rr.Result().Cookies()) != 0 {
				t.Errorf("%s set a cookie", name)
			}
		}
	})
	t.Run("complete: auth, csrf, provenance, fields, cookie", func(t *testing.T) {
		f := newCreateFixture(t)
		noSession := withHandoff(githubRequest(http.MethodPost, "/github/create/complete", f.completeForm(), f.p, f.h), goodHandoff())
		noSession.Header.Del("Cookie")
		withHandoff(noSession, goodHandoff())
		if rr := f.serve(noSession); rr.Header().Get("Location") != "/login" {
			t.Errorf("no session = %d", rr.Code)
		}
		if rr := f.postComplete(url.Values{"csrf": {"bad"}}, goodHandoff()); rr.Header().Get("Location") != "/login" {
			t.Errorf("bad csrf = %d", rr.Code)
		}
		wrongOrigin := withHandoff(githubRequest(http.MethodPost, "/github/create/complete", f.completeForm(), f.p, f.h), goodHandoff())
		wrongOrigin.Header.Set("Origin", "https://evil.example.test")
		if rr := f.serve(wrongOrigin); rr.Code != http.StatusBadRequest {
			t.Errorf("provenance = %d", rr.Code)
		}
		extra := f.completeForm()
		extra.Set("code", "x")
		if rr := f.postComplete(extra, goodHandoff()); rr.Code != http.StatusBadRequest {
			t.Errorf("extra field = %d", rr.Code)
		}
		for name, cookie := range map[string]string{"missing": "", "no dot": testCreateState, "bad code": testCreateState + ".a.b", "short state": "short." + testCreateCode} {
			rr := f.postComplete(f.completeForm(), cookie)
			if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), msgCreateExpired) {
				t.Errorf("%s cookie = %d", name, rr.Code)
			}
			if !clearsHandoff(rr) {
				t.Errorf("%s cookie not cleared", name)
			}
		}
		if f.store.consumed != 0 || f.conv.calls != 0 {
			t.Fatal("a refused request consumed state or called GitHub")
		}
	})
	t.Run("complete GET without session or cookie", func(t *testing.T) {
		f := newCreateFixture(t)
		r := githubRequest(http.MethodGet, "/github/create/complete", nil, f.p, f.h)
		r.Header.Del("Cookie")
		if rr := f.serve(withHandoff(r, goodHandoff())); rr.Header().Get("Location") != "/login" {
			t.Errorf("no session = %d", rr.Code)
		}
		if rr := f.serve(githubRequest(http.MethodGet, "/github/create/complete", nil, f.p, f.h)); rr.Code != http.StatusBadRequest {
			t.Errorf("no cookie = %d", rr.Code)
		}
	})
}

func TestGitHubCreateHandoffPageAndCSP(t *testing.T) {
	f := newCreateFixture(t)
	rr := f.serve(githubRequest(http.MethodPost, "/github/create", f.createForm(func(v url.Values) {
		v.Set("app_name", `Quote "x" <b> & co`)
	}), f.p, f.h))
	if rr.Code != http.StatusOK {
		t.Fatalf("= %d: %.200s", rr.Code, rr.Body.String())
	}
	if f.store.created != 1 {
		t.Fatal("no state stored")
	}
	body := rr.Body.String()
	wantCSP := strings.Replace(oldCSP, "form-action 'self'", "form-action 'self' https://github.com", 1)
	if got := rr.Header().Get("Content-Security-Policy"); got != wantCSP {
		t.Fatalf("handoff CSP = %q", got)
	}
	m := regexp.MustCompile(`<form method="post" action="([^"]+)"><input type="hidden" name="manifest" value="([^"]*)"><button type="submit">Continue to GitHub</button></form>`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no handoff form: %s", body)
	}
	action := html.UnescapeString(m[1])
	if !strings.HasPrefix(action, "https://github.com/settings/apps/new?state=") {
		t.Fatalf("action = %s", action)
	}
	state := strings.TrimPrefix(action, "https://github.com/settings/apps/new?state=")
	digest, _ := githubapp.SetupDigest(state)
	if digest != f.store.createdDigest {
		t.Fatal("stored digest is not the digest of the state sent to GitHub")
	}
	want, err := githubapp.BuildManifest(githubapp.ManifestInput{Origin: "https://admin.example.test", AccountType: "personal", Name: `Quote "x" <b> & co`}, state)
	if err != nil {
		t.Fatal(err)
	}
	if got := html.UnescapeString(m[2]); got != string(want.JSON) {
		t.Fatalf("manifest field = %s, want %s", got, want.JSON)
	}
	if strings.Contains(m[2], `<b>`) || strings.Contains(m[2], `"x"`) {
		t.Fatal("manifest value is not HTML-escaped")
	}
	if strings.Count(body, "<script") != 0 || strings.Contains(body, " style=") {
		t.Fatal("inline script or style")
	}
}

func TestGitHubCreateOrganizationAndValidation(t *testing.T) {
	f := newCreateFixture(t)
	rr := f.serve(githubRequest(http.MethodPost, "/github/create", f.createForm(func(v url.Values) {
		v.Set("account_type", "organization")
		v.Set("org_login", "my-org")
	}), f.p, f.h))
	if rr.Code != http.StatusOK || !strings.Contains(html.UnescapeString(rr.Body.String()), "https://github.com/organizations/my-org/settings/apps/new?state=") {
		t.Fatalf("org create = %d", rr.Code)
	}
	for name, mutate := range map[string]func(url.Values){
		"org without login": func(v url.Values) { v.Set("account_type", "organization") },
		"bad org login":     func(v url.Values) { v.Set("account_type", "organization"); v.Set("org_login", "-bad/org") },
		"bad type":          func(v url.Values) { v.Set("account_type", "team") },
		"empty name":        func(v url.Values) { v.Set("app_name", "   ") },
		"long name":         func(v url.Values) { v.Set("app_name", strings.Repeat("a", 35)) },
	} {
		before := f.store.created
		rr := f.serve(githubRequest(http.MethodPost, "/github/create", f.createForm(mutate), f.p, f.h))
		body := rr.Body.String()
		if rr.Code != http.StatusBadRequest || !strings.Contains(body, msgCreateInput) || !strings.Contains(body, `action="/github/create"`) {
			t.Errorf("%s = %d", name, rr.Code)
		}
		if f.store.created != before {
			t.Errorf("%s stored a state", name)
		}
		if rr.Header().Get("Content-Security-Policy") != oldCSP {
			t.Errorf("%s relaxed the CSP", name)
		}
	}
	// A personal account ignores a leftover organization login.
	rr = f.serve(githubRequest(http.MethodPost, "/github/create", f.createForm(func(v url.Values) { v.Set("org_login", "leftover") }), f.p, f.h))
	if rr.Code != http.StatusOK {
		t.Errorf("personal with leftover org = %d", rr.Code)
	}
}

func TestGitHubCreateStoreAndKEKFailures(t *testing.T) {
	f := newCreateFixture(t)
	f.store.createErr = postgres.ErrManifestAppExists
	if rr := f.serve(githubRequest(http.MethodPost, "/github/create", f.createForm(nil), f.p, f.h)); rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/github" {
		t.Errorf("app exists = %d %s", rr.Code, rr.Header().Get("Location"))
	}
	f.store.createErr = postgres.ErrManifestInputInvalid
	if rr := f.serve(githubRequest(http.MethodPost, "/github/create", f.createForm(nil), f.p, f.h)); rr.Code != http.StatusTooManyRequests || !strings.Contains(rr.Body.String(), msgCreateBusy) {
		t.Errorf("cap = %d", rr.Code)
	}
	f.store.createErr = errors.New("db down")
	if rr := f.serve(githubRequest(http.MethodPost, "/github/create", f.createForm(nil), f.p, f.h)); rr.Code != http.StatusInternalServerError {
		t.Errorf("db = %d", rr.Code)
	}
	f.store.createErr = nil
	before := f.store.created
	f.h.config.GitHubKEKProvider = func() (secretcrypto.KeyProvider, error) { return nil, errors.New("no kek") }
	rr := f.serve(githubRequest(http.MethodPost, "/github/create", f.createForm(nil), f.p, f.h))
	if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), msgCreateNoKEK) || f.store.created != before {
		t.Errorf("no KEK = %d, created %d", rr.Code, f.store.created)
	}
}

func TestGitHubCreateCallbackSetsStrictHandoffCookie(t *testing.T) {
	f := newCreateFixture(t)
	r := githubRequest(http.MethodGet, "/github/create/callback?code="+testCreateCode+"&state="+testCreateState, nil, f.p, f.h)
	r.Header.Del("Cookie") // cross-site: no session
	rr := f.serve(r)
	if rr.Code != http.StatusOK {
		t.Fatalf("= %d", rr.Code)
	}
	cookies := rr.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("%d cookies", len(cookies))
	}
	c := cookies[0]
	if c.Name != createHandoffCookieName || c.Value != goodHandoff() || c.Path != "/" || !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.MaxAge != 600 || c.Domain != "" {
		t.Fatalf("cookie = %+v", c)
	}
	body := rr.Body.String()
	contains(t, body, `<a class="btn" href="/github/create/complete">Continue</a>`, `aria-current="step"`)
	lacks(t, body, testCreateCode, testCreateState, "csrf")
	if rr.Header().Get("Content-Security-Policy") != oldCSP {
		t.Fatal("callback relaxed the CSP")
	}
	if f.store.consumed != 0 || f.conv.calls != 0 {
		t.Fatal("callback exchanged the code")
	}
}

func TestGitHubCreateConfirmPageUsesSessionCSRF(t *testing.T) {
	f := newCreateFixture(t)
	rr := f.serve(withHandoff(githubRequest(http.MethodGet, "/github/create/complete", nil, f.p, f.h), goodHandoff()))
	if rr.Code != http.StatusOK {
		t.Fatalf("= %d", rr.Code)
	}
	contains(t, rr.Body.String(), `<form method="post" action="/github/create/complete"><input type="hidden" name="csrf" value="`+githubCSRF(t, f.h, f.p)+`"><button type="submit">Finish setup</button></form>`)
	lacks(t, rr.Body.String(), testCreateCode, testCreateState)
}

func TestGitHubCreateCompleteSuccess(t *testing.T) {
	f := newCreateFixture(t)
	pemBuf, secBuf := f.conv.result.PEM, f.conv.result.WebhookSecret
	rr := f.postComplete(f.completeForm(), goodHandoff())
	if rr.Code != http.StatusOK {
		t.Fatalf("= %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	contains(t, body, `<meta http-equiv="refresh" content="10;url=/github">`, "Applying... AutoDeploy is restarting to load the App.", `aria-current="step"><strong>Restart`)
	if f.restarts != 1 {
		t.Fatalf("RequestRestart called %d times", f.restarts)
	}
	if f.bodyAtRestart != body || !strings.Contains(f.bodyAtRestart, "Applying") {
		t.Fatal("RequestRestart ran before the response body was written")
	}
	if !rr.Flushed {
		t.Fatal("response not flushed before restart")
	}
	if got := strings.Join(f.order, ","); got != "consume,insert,restart" {
		t.Fatalf("order = %s", got)
	}
	if !clearsHandoff(rr) {
		t.Fatal("handoff cookie not cleared")
	}
	digest, _ := githubapp.SetupDigest(testCreateState)
	if f.store.consumePrincipal != f.p || f.store.insertInput.Principal != f.p {
		t.Fatalf("consume principal = %+v, want session principal", f.store.consumePrincipal)
	}
	if f.store.consumedDigest != digest || f.store.insertInput.StateDigest != digest || f.conv.gotCode != testCreateCode {
		t.Fatal("digest or code not passed through")
	}
	in := f.store.insertInput
	if in.AppID != 4242 || in.Slug != "autodeploy-x" || in.ClientID != "Iv1.client" || in.GitHubOwnerID != 9 || in.GitHubOwnerType != "User" || in.GitHubOwnerLogin != "owner" || in.HTMLURL == "" || in.Principal != f.p {
		t.Fatalf("insert input = %+v", in)
	}
	if !f.conv.hasDL || time.Until(f.conv.deadline) > 61*time.Second || time.Until(f.conv.deadline) < 30*time.Second {
		t.Fatalf("converter deadline = %v (set %v)", f.conv.deadline, f.conv.hasDL)
	}
	for _, b := range [][]byte{pemBuf, secBuf} {
		if !bytes.Equal(b, make([]byte, len(b))) {
			t.Fatal("conversion secret buffer not cleared")
		}
	}
	if !strings.Contains(f.logs.String(), "github app created") || !strings.Contains(f.logs.String(), "app_id=4242") {
		t.Fatalf("log = %s", f.logs.String())
	}
	assertNoSecrets(t, f, body)
}

func assertNoSecrets(t *testing.T, f *createFixture, body string) {
	t.Helper()
	for _, secret := range []string{"PEM-SECRET-MATERIAL", "WEBHOOK-SECRET-MATERIAL", testCreateCode, testCreateState} {
		if strings.Contains(body, secret) {
			t.Errorf("response body contains %q", secret)
		}
		if strings.Contains(f.logs.String(), secret) {
			t.Errorf("log contains %q", secret)
		}
	}
}

func TestGitHubCreateConsumeErrorMapping(t *testing.T) {
	cases := []struct {
		err      error
		code     int
		contains string
	}{
		{postgres.ErrManifestStateConsumed, 400, msgCreateExpired},
		{postgres.ErrManifestStateExpired, 400, msgCreateExpired},
		{postgres.ErrManifestStateInvalid, 400, msgCreateExpired},
		{postgres.ErrManifestCreateInFlight, 409, "Wait 2 minutes, then start again."},
		{postgres.ErrManifestAppExists, 303, ""},
		{errors.New("pg: secret detail"), 503, msgCreateUnavail},
	}
	for _, tc := range cases {
		f := newCreateFixture(t)
		f.store.consumeErr = tc.err
		rr := f.postComplete(f.completeForm(), goodHandoff())
		if rr.Code != tc.code || !strings.Contains(rr.Body.String(), tc.contains) {
			t.Errorf("%v = %d %.120s", tc.err, rr.Code, rr.Body.String())
		}
		if tc.code == 303 && rr.Header().Get("Location") != "/github" {
			t.Errorf("exists redirect = %s", rr.Header().Get("Location"))
		}
		if !clearsHandoff(rr) {
			t.Errorf("%v: cookie not cleared", tc.err)
		}
		if f.conv.calls != 0 || f.restarts != 0 || f.store.inserted != 0 {
			t.Errorf("%v: went past consume", tc.err)
		}
		if strings.Contains(rr.Body.String()+f.logs.String(), "secret detail") {
			t.Errorf("%v: error text leaked", tc.err)
		}
	}
	f := newCreateFixture(t)
	f.store.consumeErr = postgres.ErrManifestCreateInFlight
	if body := f.postComplete(f.completeForm(), goodHandoff()).Body.String(); !strings.Contains(body, msgCreateInFlight) {
		t.Errorf("in-flight wording missing: %s", body)
	}
}

func TestGitHubCreateConvertErrorMapping(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		code     int
		contains string
		href     string
		fallback bool
	}{
		{"code rejected", githubapp.ErrManifestCodeRejected, 400, msgCreateExpired, "", false},
		{"rate limited", githubapp.ErrRateLimited, 429, "rate limiting", "", false},
		{"orphan with url", &githubapp.ManifestOrphanError{AppID: 77, HTMLURL: "https://github.com/apps/orphan"}, 502, msgCreateOrphan, "https://github.com/apps/orphan", false},
		{"orphan no url", &githubapp.ManifestOrphanError{AppID: 77}, 502, msgCreateOrphan, githubSettingsAppsURL, true},
		{"invalid response", githubapp.ErrInvalidResponse, 502, msgCreateOrphan, githubSettingsAppsURL, true},
		{"provider", githubapp.ErrProvider, 502, msgCreateOrphan, githubSettingsAppsURL, true},
		{"deadline", errors.Join(githubapp.ErrProvider, context.DeadlineExceeded), 502, msgCreateOrphan, githubSettingsAppsURL, true},
		{"unknown", errors.New("other"), 502, msgCreateOrphan, githubSettingsAppsURL, true},
	}
	for _, tc := range cases {
		f := newCreateFixture(t)
		f.conv.err = tc.err
		f.conv.result = githubapp.ManifestConversion{}
		rr := f.postComplete(f.completeForm(), goodHandoff())
		body := rr.Body.String()
		if rr.Code != tc.code || !strings.Contains(body, tc.contains) {
			t.Errorf("%s = %d %.160s", tc.name, rr.Code, body)
		}
		if tc.href != "" && !strings.Contains(body, `href="`+tc.href+`"`) {
			t.Errorf("%s lacks link %s", tc.name, tc.href)
		}
		if tc.fallback != strings.Contains(html.UnescapeString(body), "organization's Developer settings") {
			t.Errorf("%s organization note = %v", tc.name, !tc.fallback)
		}
		if tc.name == "orphan with url" && strings.Contains(body, githubSettingsAppsURL) {
			t.Errorf("fallback link shown although html_url is known")
		}
		if f.restarts != 0 || f.store.inserted != 0 || !clearsHandoff(rr) {
			t.Errorf("%s: restart/insert/cookie wrong", tc.name)
		}
		if strings.Contains(tc.name, "orphan") && !strings.Contains(f.logs.String(), "app_id=77") {
			t.Errorf("%s: app id not logged: %s", tc.name, f.logs.String())
		}
	}
}

func TestGitHubCreateInsertFailureShowsKnownHTMLURL(t *testing.T) {
	f := newCreateFixture(t)
	f.store.insertErr = errors.New("insert failed with PEM-SECRET-MATERIAL")
	pemBuf, secBuf := f.conv.result.PEM, f.conv.result.WebhookSecret
	rr := f.postComplete(f.completeForm(), goodHandoff())
	body := rr.Body.String()
	if rr.Code != http.StatusBadGateway || !strings.Contains(body, msgCreateOrphan) || !strings.Contains(body, `href="https://github.com/apps/autodeploy-x"`) {
		t.Fatalf("= %d %.200s", rr.Code, body)
	}
	if f.restarts != 0 || !clearsHandoff(rr) {
		t.Fatal("restart or cookie wrong")
	}
	for _, b := range [][]byte{pemBuf, secBuf} {
		if !bytes.Equal(b, make([]byte, len(b))) {
			t.Fatal("buffer not cleared on insert failure")
		}
	}
	if !strings.Contains(f.logs.String(), "app_id=4242") {
		t.Fatal("app id not logged")
	}
	assertNoSecrets(t, f, body)
	// KEK failure after a successful conversion is also an orphan with the URL.
	f = newCreateFixture(t)
	f.h.config.GitHubKEKProvider = func() (secretcrypto.KeyProvider, error) { return nil, errors.New("no kek") }
	rr = f.postComplete(f.completeForm(), goodHandoff())
	if rr.Code != http.StatusBadGateway || f.store.inserted != 0 || !strings.Contains(rr.Body.String(), `href="https://github.com/apps/autodeploy-x"`) {
		t.Fatalf("kek failure = %d", rr.Code)
	}
}

func TestOnlyCreateHandoffPageRelaxesCSP(t *testing.T) {
	f := newCreateFixture(t)
	for _, r := range []*http.Request{
		githubRequest(http.MethodGet, "/github", nil, f.p, f.h),
		githubRequest(http.MethodGet, "/github/create/callback?code="+testCreateCode+"&state="+testCreateState, nil, f.p, f.h),
		withHandoff(githubRequest(http.MethodGet, "/github/create/complete", nil, f.p, f.h), goodHandoff()),
		githubRequest(http.MethodGet, "/", nil, f.p, f.h),
	} {
		if got := f.serve(r).Header().Get("Content-Security-Policy"); got != oldCSP {
			t.Errorf("%s CSP = %q", r.URL.Path, got)
		}
	}
	for _, rr := range []*httptest.ResponseRecorder{
		f.postComplete(f.completeForm(), goodHandoff()),
		f.postComplete(f.completeForm(), ""),
	} {
		if got := rr.Header().Get("Content-Security-Policy"); got != oldCSP {
			t.Errorf("complete CSP = %q", got)
		}
	}
}

func TestGitHubPageStates(t *testing.T) {
	const note = "An App created in the browser is stored but ignored because the host configuration file sets the App."
	t.Run("none shows only the create form", func(t *testing.T) {
		f := newCreateFixture(t)
		page := f.serve(githubRequest(http.MethodGet, "/github", nil, f.p, f.h)).Body.String()
		contains(t, page, `action="/github/create"`, `name="account_type" value="personal" checked`, `name="account_type" value="organization"`,
			`name="org_login"`, `name="app_name" required maxlength="34" value="AutoDeploy admin.example.test"`, "GitHub will ask you to confirm", `>Create GitHub App<`)
		lacks(t, page, `/github/setup"`, `/github/installations/reconcile`, `/github/bindings`, "Install GitHub App", note)
		if n := strings.Count(page, "<form"); n != 2 { // create form + sign-out
			t.Errorf("%d forms, want create + sign-out", n)
		}
	})
	t.Run("env plus ignored shows the note", func(t *testing.T) {
		h, _, _, p := githubPageHandler(t)
		h.config.GitHubAppSource, h.config.GitHubDBAppIgnored = GitHubAppSourceEnv, true
		page := pageGet(t, h, p, "/github")
		contains(t, page, note, `action="/github/setup"`, `action="/github/bindings"`)
		lacks(t, page, `/github/create"`)
	})
	t.Run("env without ignored and database are unchanged", func(t *testing.T) {
		for _, source := range []string{GitHubAppSourceEnv, GitHubAppSourceDatabase} {
			h, _, _, p := githubPageHandler(t)
			h.config.GitHubAppSource = source
			page := pageGet(t, h, p, "/github")
			contains(t, page, `action="/github/setup"`, `action="/github/installations/reconcile"`, `action="/github/bindings"`)
			lacks(t, page, note, `/github/create"`)
		}
		h, _, _, p := githubPageHandler(t)
		h.config.GitHubAppSource, h.config.GitHubDBAppIgnored = GitHubAppSourceDatabase, true
		lacks(t, pageGet(t, h, p, "/github"), note)
	})
}

func TestGitHubCreateCookieNameIsDistinctHostPrefixed(t *testing.T) {
	if createHandoffCookieName == handoffCookieName || !strings.HasPrefix(createHandoffCookieName, "__Host-") {
		t.Fatalf("cookie name %q", createHandoffCookieName)
	}
	// A setup-flow cookie must not satisfy the create flow.
	f := newCreateFixture(t)
	r := githubRequest(http.MethodPost, "/github/create/complete", f.completeForm(), f.p, f.h)
	r.AddCookie(&http.Cookie{Name: handoffCookieName, Value: goodHandoff()})
	if rr := f.serve(r); rr.Code != http.StatusBadRequest || f.store.consumed != 0 {
		t.Fatalf("setup cookie accepted: %d", rr.Code)
	}
}

func TestGitHubCreateCancelledRequestDoesNotCancelWork(t *testing.T) {
	f := newCreateFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := withHandoff(githubRequest(http.MethodPost, "/github/create/complete", f.completeForm(), f.p, f.h), goodHandoff()).WithContext(ctx)
	rr := f.serve(r)
	if rr.Code != http.StatusOK || f.restarts != 1 {
		t.Fatalf("= %d restarts %d", rr.Code, f.restarts)
	}
	if f.store.consumeCtxErr != nil || f.conv.ctxErr != nil || f.store.insertCtxErr != nil {
		t.Fatalf("a cancelled request cancelled the work: consume=%v convert=%v insert=%v", f.store.consumeCtxErr, f.conv.ctxErr, f.store.insertCtxErr)
	}
}

func TestGitHubCreateShortBudgetRefusesConversion(t *testing.T) {
	f := newCreateFixture(t)
	f.h.createBudget = 5 * time.Second // below createMinConvertWindow
	rr := f.postComplete(f.completeForm(), goodHandoff())
	if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), msgCreateInFlight) {
		t.Fatalf("= %d %.200s", rr.Code, rr.Body.String())
	}
	if f.store.consumed != 1 || f.conv.calls != 0 || f.store.inserted != 0 || f.restarts != 0 || !clearsHandoff(rr) {
		t.Fatalf("consumed %d calls %d inserted %d restarts %d", f.store.consumed, f.conv.calls, f.store.inserted, f.restarts)
	}
}

func TestGitHubCreateStateInvalidAtCreateRedirectsToLogin(t *testing.T) {
	f := newCreateFixture(t)
	f.store.createErr = postgres.ErrManifestStateInvalid
	rr := f.serve(githubRequest(http.MethodPost, "/github/create", f.createForm(nil), f.p, f.h))
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatalf("= %d %s", rr.Code, rr.Header().Get("Location"))
	}
}

func TestGitHubCreateOrphanLinkIsRevalidated(t *testing.T) {
	for _, bad := range []string{"https://evil.example/apps/x", "https://github.com.evil.example/apps/x", "https://user@github.com/apps/x", "http://github.com/apps/x", "javascript:alert(1)"} {
		f := newCreateFixture(t)
		f.conv.err = &githubapp.ManifestOrphanError{AppID: 5, HTMLURL: bad}
		body := f.postComplete(f.completeForm(), goodHandoff()).Body.String()
		if strings.Contains(body, "evil") || strings.Contains(body, "user@") || strings.Contains(body, "javascript") || strings.Contains(body, `href="http://`) {
			t.Errorf("%s rendered as a link", bad)
		}
		if !strings.Contains(body, `href="`+githubSettingsAppsURL+`"`) {
			t.Errorf("%s: no fallback link", bad)
		}
	}
}

func TestGitHubCreateFailureLogsFixedCategory(t *testing.T) {
	cases := []struct {
		name     string
		set      func(*createFixture)
		category string
	}{
		{"store", func(f *createFixture) { f.store.consumeErr = errors.New("SECRET-DB-TEXT") }, "category=store"},
		{"state", func(f *createFixture) { f.store.consumeErr = postgres.ErrManifestStateConsumed }, "category=state"},
		{"in_flight", func(f *createFixture) { f.store.consumeErr = postgres.ErrManifestCreateInFlight }, "category=in_flight"},
		{"app_exists", func(f *createFixture) { f.store.consumeErr = postgres.ErrManifestAppExists }, "category=app_exists"},
		{"kek", func(f *createFixture) {
			f.h.config.GitHubKEKProvider = func() (secretcrypto.KeyProvider, error) { return nil, errors.New("SECRET-KEK-TEXT") }
		}, "category=kek"},
		{"insert_conflict", func(f *createFixture) {
			f.store.insertErr = errors.Join(postgres.ErrManifestAppExists, errors.New("SECRET-INSERT-TEXT"))
		}, "category=app_exists"},
		{"insert store", func(f *createFixture) { f.store.insertErr = errors.New("SECRET-INSERT-TEXT") }, "category=store"},
	}
	for _, tc := range cases {
		f := newCreateFixture(t)
		tc.set(f)
		f.postComplete(f.completeForm(), goodHandoff())
		logs := f.logs.String()
		if !strings.Contains(logs, tc.category) || strings.Contains(logs, "SECRET-") {
			t.Errorf("%s: logs = %s", tc.name, logs)
		}
	}
}

func TestGitHubCreatePriorAppRegistered(t *testing.T) {
	notice := html.EscapeString(msgCreatePriorApp)
	t.Run("GET hides the form", func(t *testing.T) {
		f := newCreateFixture(t)
		f.store.priorApp = true
		rr := f.serve(githubRequest(http.MethodGet, "/github", nil, f.p, f.h))
		body := rr.Body.String()
		if rr.Code != http.StatusOK || !strings.Contains(body, notice) {
			t.Fatalf("= %d %.200s", rr.Code, body)
		}
		lacks(t, body, `action="/github/create"`, `name="app_name"`)
		if strings.Contains(mainContent(t, body), "<form") || textOnly(mainContent(t, body)) {
			t.Fatal("blocked page has a form or is text only")
		}
	})
	t.Run("GET check error shows unavailable and logs a category", func(t *testing.T) {
		f := newCreateFixture(t)
		f.store.priorErr = errors.New("SECRET-DB-TEXT")
		rr := f.serve(githubRequest(http.MethodGet, "/github", nil, f.p, f.h))
		body := rr.Body.String()
		if rr.Code != http.StatusServiceUnavailable || !strings.Contains(body, msgCreateCheckFailed) || strings.Contains(body, `action="/github/create"`) {
			t.Fatalf("= %d", rr.Code)
		}
		if !strings.Contains(f.logs.String(), "category=prior_app_check") || strings.Contains(f.logs.String(), "SECRET-") {
			t.Fatalf("logs = %s", f.logs.String())
		}
	})
	t.Run("GET without a prior App still shows the form", func(t *testing.T) {
		f := newCreateFixture(t)
		if body := f.serve(githubRequest(http.MethodGet, "/github", nil, f.p, f.h)).Body.String(); !strings.Contains(body, `action="/github/create"`) {
			t.Fatal("form missing")
		}
	})
	t.Run("POST create", func(t *testing.T) {
		f := newCreateFixture(t)
		f.store.createErr = postgres.ErrManifestPriorAppRegistered
		rr := f.serve(githubRequest(http.MethodPost, "/github/create", f.createForm(nil), f.p, f.h))
		if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), notice) || !strings.Contains(f.logs.String(), "category=prior_app") {
			t.Fatalf("= %d logs %s", rr.Code, f.logs.String())
		}
		if rr.Header().Get("Content-Security-Policy") != oldCSP {
			t.Fatal("CSP relaxed")
		}
	})
	t.Run("POST complete consume", func(t *testing.T) {
		f := newCreateFixture(t)
		f.store.consumeErr = postgres.ErrManifestPriorAppRegistered
		rr := f.postComplete(f.completeForm(), goodHandoff())
		if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), notice) || !clearsHandoff(rr) || !strings.Contains(f.logs.String(), "category=prior_app") {
			t.Fatalf("= %d", rr.Code)
		}
		if f.conv.calls != 0 || f.restarts != 0 {
			t.Fatal("went past consume")
		}
	})
	t.Run("POST complete insert goes to the orphan page", func(t *testing.T) {
		f := newCreateFixture(t)
		f.store.insertErr = postgres.ErrManifestPriorAppRegistered
		rr := f.postComplete(f.completeForm(), goodHandoff())
		body := rr.Body.String()
		if rr.Code != http.StatusBadGateway || !strings.Contains(body, msgCreateOrphan) || !strings.Contains(body, `href="https://github.com/apps/autodeploy-x"`) || f.restarts != 0 {
			t.Fatalf("= %d", rr.Code)
		}
		if !strings.Contains(f.logs.String(), "category=prior_app") || !strings.Contains(f.logs.String(), "app_id=4242") {
			t.Fatalf("logs = %s", f.logs.String())
		}
	})
}
