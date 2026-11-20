package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"autodeploy/internal/auth"
	"autodeploy/internal/store/postgres"
)

// githubPageHandler is a handler with GitHub setup, an operations reader and a
// server admin, so every part of /github can render.
func githubPageHandler(t *testing.T) (*Handler, *operationsStub, *serverAdminStub, auth.Principal) {
	t.Helper()
	h, _, p := githubHandler(t)
	admin := &serverAdminStub{servers: []postgres.ServerSummary{{ID: "server_1", Name: "acer", Status: "active"}}}
	ops := &operationsStub{}
	h.config.ServerAdmin, h.config.OperationsReader = admin, ops
	h.config.RepositoryLister = repoListerStub{repos: []postgres.BindableRepository{{RepositoryID: 201, FullName: "owner/demo", BoundProject: "demo"}, {RepositoryID: 202, FullName: "owner/other"}}}
	return h, ops, admin, p
}

func TestGitHubPageRendersFormsAndBindings(t *testing.T) {
	h, ops, _, p := githubPageHandler(t)
	ops.projects = []postgres.ProjectSummary{
		{ProjectID: "demo", HasBinding: true, BindingActive: true, RepositoryFullName: "owner/demo", RepositoryDefaultRef: "refs/heads/main", BindingEnvironmentID: "env-demo", BindingServerID: "server_1"},
		{ProjectID: "old", HasBinding: true, RepositoryFullName: "owner/old", RepositoryDefaultRef: "refs/heads/dev", BindingEnvironmentID: "env-old", BindingServerID: "server_9"},
		{ProjectID: "unbound"},
	}
	page := pageGet(t, h, p, "/github")
	contains(t, page,
		`<form method="post" action="/github/setup"><input type="hidden" name="csrf" value="`+serversCSRF(t, h, p)+`">`,
		`action="/github/installations/reconcile"`, `name="installation_id"`,
		`<form method="post" action="/github/bindings">`,
		`<option value="201">owner/demo (bound to demo)</option>`, `<option value="server_1">acer (server_1)</option>`,
		`name="project_id"`, `name="ref"`, `value="refs/heads/main"`, `name="environment_id"`, `name="repository_id"`, `name="server_id"`,
		`<th>Project</th>`, `<th>Repository</th>`, `<th>Deploy ref</th>`, `<th>Environment</th>`, `<th>Server</th>`, `<th>Binding</th>`,
		`>demo<`, `>owner/demo<`, `>env-demo<`, `>server_1<`, `>active<`,
		`>old<`, `>owner/old<`, `>refs/heads/dev<`, `>env-old<`, `>server_9<`, `>inactive<`,
	)
	lacks(t, page, `>unbound<`)
	if !strings.Contains(page, `href="/github" aria-current="page"`) {
		t.Fatal("GitHub nav item is not current")
	}
	form := page[strings.Index(page, `action="/github/bindings"`):]
	form = form[:strings.Index(form, "</form>")]
	if got := strings.Count(form, `name="`); got != 6 {
		t.Fatalf("binding form has %d named fields, want 6", got)
	}
}

func TestGitHubPageEmptyIsNotTextOnly(t *testing.T) {
	h, _, _, p := githubPageHandler(t)
	h.config.RepositoryLister = nil
	page := pageGet(t, h, p, "/github")
	contains(t, page, `action="/github/setup"`, `action="/github/installations/reconcile"`, `action="/github/bindings"`, `class="empty"`)
	lacks(t, page, `<th>Project</th>`)
}

func TestGitHubPageWithoutOperationsReaderOrSetup(t *testing.T) {
	h, _, _, p := githubPageHandler(t)
	h.config.OperationsReader = nil
	page := pageGet(t, h, p, "/github")
	contains(t, page, `action="/github/setup"`, `action="/github/bindings"`)
	lacks(t, page, `<th>Deploy ref</th>`)
	h.config.GitHubSetup = nil
	page = pageGet(t, h, p, "/github")
	contains(t, page, `action="/github/setup"`, `action="/github/installations/reconcile"`)
	lacks(t, page, `action="/github/bindings"`)
}

func TestGitHubPageBindingsReadFailureStillRendersForms(t *testing.T) {
	h, ops, _, p := githubPageHandler(t)
	ops.err = errors.New("db down")
	page := pageGet(t, h, p, "/github")
	contains(t, page, `action="/github/setup"`, `action="/github/bindings"`, `role="note"`)
}

func TestGitHubPageRejectsQueryAndOtherMethods(t *testing.T) {
	h, _, _, p := githubPageHandler(t)
	if rr := opsGet(h, p, http.MethodGet, "/github?x=1"); rr.Code != http.StatusNotFound {
		t.Fatalf("query = %d", rr.Code)
	}
	if rr := opsGet(h, p, http.MethodPost, "/github"); rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d", rr.Code)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "https://admin.example.test/github", nil))
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatalf("no session = %d %q", rr.Code, rr.Header().Get("Location"))
	}
}

func TestOverviewNoLongerHostsGitHubForms(t *testing.T) {
	h, _, _, p := overviewHandler(t, fourRows())
	page := pageGet(t, h, p, "/")
	lacks(t, page, `action="/github/setup"`, `action="/github/installations/reconcile"`, `action="/github/bindings"`, `id="github"`)
	// A fresh install links the checklist steps to /github and /servers.
	h2, _, _, p2 := overviewHandler(t, nil)
	page2 := pageGet(t, h2, p2, "/")
	contains(t, page2, hrefAttr("/github"), hrefAttr("/servers"))
	lacks(t, page2, `/#github`)
}

func TestGitHubPostsRedirectToGitHubPage(t *testing.T) {
	h, _, p := githubHandler(t)
	csrf := githubCSRF(t, h, p)
	for name, c := range map[string]struct {
		target string
		form   url.Values
	}{
		"bind":      {"/github/bindings", url.Values{"csrf": {csrf}, "project_id": {"project_opaque"}, "repository_id": {"42"}, "ref": {"refs/heads/main"}, "environment_id": {"environment_opaque"}, "server_id": {"server_opaque"}}},
		"reconcile": {"/github/installations/reconcile", url.Values{"csrf": {csrf}, "installation_id": {"11"}}},
	} {
		rr := cancelPost(h, p, c.target, c.form, nil)
		if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/github" {
			t.Errorf("%s = %d %q, want redirect to /github", name, rr.Code, rr.Header().Get("Location"))
		}
	}
}
