package web

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"autodeploy/internal/auth"
	"autodeploy/internal/githubapp"
)

type githubSetupStoreStub struct {
	principal                           auth.Principal
	installation                        githubapp.Installation
	created, reserved, completed, bound int
	handoff                             [32]byte
}

func (s *githubSetupStoreStub) CreateSetupState(_ context.Context, in githubapp.SetupStateInput) error {
	s.created++
	s.principal = in.Principal
	return nil
}
func (*githubSetupStoreStub) BeginCallback(context.Context, [32]byte) error  { return nil }
func (*githubSetupStoreStub) CancelCallback(context.Context, [32]byte) error { return nil }
func (s *githubSetupStoreStub) ReserveCallback(_ context.Context, _, handoff [32]byte, in githubapp.Installation) error {
	if s.reserved != 0 {
		return errors.New("replayed")
	}
	s.reserved++
	s.handoff, s.installation = handoff, in
	return nil
}
func (s *githubSetupStoreStub) PendingInstallation(_ context.Context, digest [32]byte, p auth.Principal) (githubapp.Installation, error) {
	if digest != s.handoff || p != s.principal {
		return githubapp.Installation{}, errors.New("invalid")
	}
	return s.installation, nil
}
func (s *githubSetupStoreStub) CompleteHandoff(_ context.Context, digest [32]byte, p auth.Principal, _ githubapp.Installation, _ []githubapp.Repository) error {
	if digest != s.handoff || p != s.principal {
		return errors.New("invalid")
	}
	s.completed++
	return nil
}
func (s *githubSetupStoreStub) BindRepository(_ context.Context, p auth.Principal, project string, repository int64, _ string) error {
	if p != s.principal || project != "project_opaque" || repository != 42 {
		return errors.New("invalid")
	}
	s.bound++
	return nil
}
func (*githubSetupStoreStub) DisableInstallation(context.Context, auth.Principal, int64, string) error {
	return nil
}
func (*githubSetupStoreStub) ReconcileInstallation(context.Context, auth.Principal, githubapp.Installation, []githubapp.Repository, string) error {
	return nil
}

type githubProviderStub struct {
	app          githubapp.App
	installation githubapp.Installation
	repos        []githubapp.Repository
}

func (s githubProviderStub) GetApp(context.Context) (githubapp.App, error) { return s.app, nil }
func (s githubProviderStub) GetInstallation(context.Context, int64) (githubapp.Installation, error) {
	return s.installation, nil
}
func (s githubProviderStub) ListInstallationRepositories(context.Context, int64) ([]githubapp.Repository, error) {
	return s.repos, nil
}
func (githubProviderStub) MintDiscoveryToken(context.Context, int64) (githubapp.Token, error) {
	return githubapp.Token{}, errors.New("unexpected")
}
func (githubProviderStub) MintCheckoutToken(context.Context, int64, []int64) (githubapp.Token, error) {
	return githubapp.Token{}, errors.New("unexpected")
}
func (githubProviderStub) RevokeToken(context.Context, string) error { return nil }

func githubHandler(t *testing.T) (*Handler, *githubSetupStoreStub, auth.Principal) {
	t.Helper()
	p := auth.Principal{OwnerID: "owner", UserID: "user", SessionID: "session", AuthRevision: 1, Role: auth.RoleOwner}
	store := &githubSetupStoreStub{principal: p}
	owner := githubapp.Owner{AccountID: 9, Type: "User"}
	provider := githubProviderStub{app: githubapp.App{ID: 7, ClientID: "client", Slug: "app", Owner: owner, Permissions: map[string]string{"contents": "read", "metadata": "read"}, Events: []string{"push"}}, installation: githubapp.Installation{ID: 11, Account: owner}, repos: []githubapp.Repository{{ID: 42, OwnerID: 9, Name: "repo", FullName: "owner/repo"}}}
	setup, err := githubapp.NewSetupService(store, provider, githubapp.AppIdentity{AppID: 7, ClientID: "client", Slug: "app"}, owner, bytes.NewReader(bytes.Repeat([]byte{4}, 512)))
	if err != nil {
		t.Fatal(err)
	}
	repo := &repositoryStub{principal: p}
	h := testHandler(t, repo)
	h.config.GitHubSetup, h.config.GitHubInstallationURL = setup, "https://github.com/apps/app/installations/new"
	return h, store, p
}

func githubRequest(method, target string, form url.Values, p auth.Principal, h *Handler) *http.Request {
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

func githubCSRF(t *testing.T, h *Handler, p auth.Principal) string {
	t.Helper()
	v, err := auth.NewSessionCSRF(h.config.CSRFKeys, p.SessionID, h.config.PublicOrigin)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestGitHubSetupRoutesKeepCallbackUnauthorised(t *testing.T) {
	h, store, p := githubHandler(t)
	r := githubRequest(http.MethodPost, "/github/setup", url.Values{"csrf": {githubCSRF(t, h, p)}}, p, h)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusSeeOther || store.created != 1 || !strings.HasPrefix(rr.Header().Get("Location"), "https://github.com/apps/app/installations/new?state=") {
		t.Fatalf("setup: code=%d location=%q created=%d", rr.Code, rr.Header().Get("Location"), store.created)
	}
	state, _ := url.Parse(rr.Header().Get("Location"))
	callback := "/github/setup/callback?state=" + state.Query().Get("state") + "&installation_id=11&setup_action=install"
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "https://admin.example.test"+callback, nil))
	if rr.Code != http.StatusSeeOther || store.reserved != 1 || store.completed != 0 || rr.Header().Get("Location") != "/github/setup/complete" {
		t.Fatalf("callback side effect: %d reserve=%d complete=%d", rr.Code, store.reserved, store.completed)
	}
	var handoff *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == handoffCookieName {
			handoff = c
		}
	}
	if handoff == nil || !handoff.Secure || !handoff.HttpOnly || handoff.SameSite != http.SameSiteStrictMode {
		t.Fatal("missing strict handoff cookie")
	}
	// Replays and invalid callbacks have a generic failure and never complete setup.
	bad := httptest.NewRecorder()
	h.ServeHTTP(bad, httptest.NewRequest(http.MethodGet, "https://admin.example.test"+callback, nil))
	if bad.Code != http.StatusBadRequest || store.completed != 0 {
		t.Fatalf("callback replay: %d complete=%d", bad.Code, store.completed)
	}
}

func TestGitHubHandoffAndBindingRequireSessionCSRFAndAreOneUse(t *testing.T) {
	h, store, p := githubHandler(t)
	state, _ := githubapp.NewCapability(bytes.NewReader(bytes.Repeat([]byte{8}, 32)))
	sd, _ := githubapp.SetupDigest(state)
	handoff, _ := githubapp.NewCapability(bytes.NewReader(bytes.Repeat([]byte{9}, 32)))
	hd, _ := githubapp.HandoffDigest(handoff)
	store.handoff, store.installation = hd, githubapp.Installation{ID: 11, Account: githubapp.Owner{AccountID: 9, Type: "User"}}
	_ = sd
	get := githubRequest(http.MethodGet, "/github/setup/complete", nil, p, h)
	get.AddCookie(&http.Cookie{Name: handoffCookieName, Value: handoff})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, get)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "name=\"csrf\"") || rr.Header().Get("Cache-Control") == "" {
		t.Fatalf("handoff get=%d", rr.Code)
	}
	post := githubRequest(http.MethodPost, "/github/setup/complete", url.Values{"csrf": {githubCSRF(t, h, p)}}, p, h)
	post.AddCookie(&http.Cookie{Name: handoffCookieName, Value: handoff})
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, post)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/" || store.completed != 1 {
		t.Fatalf("complete=%d count=%d", rr.Code, store.completed)
	}
	bind := githubRequest(http.MethodPost, "/github/bindings", url.Values{"csrf": {githubCSRF(t, h, p)}, "project_id": {"project_opaque"}, "repository_id": {"42"}}, p, h)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, bind)
	if rr.Code != http.StatusSeeOther || store.bound != 1 {
		t.Fatalf("bind=%d count=%d", rr.Code, store.bound)
	}
	invalid := githubRequest(http.MethodPost, "/github/bindings", url.Values{"csrf": {githubCSRF(t, h, p)}, "project_id": {"project_opaque"}, "repository_id": {"042"}}, p, h)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, invalid)
	if rr.Code != http.StatusBadRequest || store.bound != 1 {
		t.Fatalf("noncanonical bind=%d", rr.Code)
	}
}

func TestGitHubReconcileRequiresSessionCSRF(t *testing.T) {
	h, _, p := githubHandler(t)
	valid := githubRequest(http.MethodPost, "/github/installations/reconcile", url.Values{"csrf": {githubCSRF(t, h, p)}, "installation_id": {"11"}}, p, h)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, valid)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/" {
		t.Fatalf("reconcile code=%d location=%q", rr.Code, rr.Header().Get("Location"))
	}
	invalid := githubRequest(http.MethodPost, "/github/installations/reconcile", url.Values{"installation_id": {"11"}}, p, h)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, invalid)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("csrf reconciliation code=%d", rr.Code)
	}
}
