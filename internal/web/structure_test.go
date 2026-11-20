package web

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"autodeploy/internal/auth"
	"autodeploy/internal/store/postgres"
)

// structureFixture renders one page route in two states: an empty stub (no
// data behind it) and a populated one.
type structureFixture struct {
	empty, populated func(t *testing.T) string
}

// mainContent returns the markup between <main> and </main>.
func mainContent(t *testing.T, page string) string {
	t.Helper()
	start := strings.Index(page, "<main")
	end := strings.Index(page, "</main>")
	if start < 0 || end < start {
		t.Fatalf("page has no <main>: %.200s", page)
	}
	return page[start:end]
}

// textOnly reports whether the content carries none of the allowed shapes: a
// form, a table, the setup checklist, a facts grid (data), a list with links, or
// a preformatted block.
func textOnly(content string) bool {
	if strings.Contains(content, "<form") || strings.Contains(content, "<table") ||
		strings.Contains(content, `class="checklist"`) || strings.Contains(content, `<dl class="facts"`) || strings.Contains(content, "<pre") {
		return false
	}
	if (strings.Contains(content, "<ol") || strings.Contains(content, "<ul")) && strings.Contains(content, "href=") {
		return false
	}
	return true
}

func renderTo(t *testing.T, render func(w http.ResponseWriter)) string {
	t.Helper()
	rr := httptest.NewRecorder()
	render(rr)
	if rr.Code != http.StatusOK {
		t.Fatalf("render = %d", rr.Code)
	}
	return rr.Body.String()
}

func structureFixtures() map[string]structureFixture {
	overview := func(target string) structureFixture {
		return structureFixture{
			empty: func(t *testing.T) string {
				h, _, _, p := overviewHandler(t, nil)
				return pageGet(t, h, p, target)
			},
			populated: func(t *testing.T) string {
				h, _, _, p := overviewHandler(t, fourRows())
				return pageGet(t, h, p, target)
			},
		}
	}
	firstClass := func(target string) structureFixture {
		return structureFixture{
			empty: func(t *testing.T) string {
				h, _, _, _, p := fcHandler(t, nil)
				return pageGet(t, h, p, target)
			},
			populated: func(t *testing.T) string {
				h, _, _, _, p := fcHandler(t, fourRows())
				return pageGet(t, h, p, target)
			},
		}
	}
	flowNav := func(t *testing.T) navView {
		h, p := navTestHandler(t)
		return h.navFor(context.Background(), "github", p)
	}
	enrollment := func(serverID, token string, expires time.Time) func(t *testing.T) string {
		return func(t *testing.T) string {
			h, _, p := serversHandler(t)
			return renderTo(t, func(w http.ResponseWriter) {
				h.renderEnrollmentToken(w, httptest.NewRequest(http.MethodPost, "https://admin.example.test/servers/enrollment", nil), p, serverID, token, expires)
			})
		}
	}
	continuePage := func(t *testing.T) string {
		var b bytes.Buffer
		if err := githubContinueTemplate.Execute(&b, githubContinueView{Nav: flowNav(t)}); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	completePage := func(t *testing.T) string {
		var b bytes.Buffer
		if err := githubCompleteTemplate.Execute(&b, githubCompleteView{Nav: flowNav(t), CSRF: "c"}); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	detail := func(populated bool) func(t *testing.T) string {
		return func(t *testing.T) string {
			h, ops, p := operationsHandler(t)
			ops.detail = sampleDetail("leased", true)
			if !populated {
				ops.detail.StatusEvents, ops.detail.JobEvents, ops.detail.Attempts = nil, nil, nil
				ops.detail.Release, ops.detail.RuntimeInstances = nil, nil
			}
			return pageGet(t, h, p, "/deployments/detail?id=dep-1")
		}
	}
	return map[string]structureFixture{
		"/":             overview("/"),
		"/environments": overview("/environments"),
		"/deployments": {
			empty: func(t *testing.T) string {
				h, _, p := operationsHandler(t)
				return pageGet(t, h, p, "/deployments")
			},
			populated: func(t *testing.T) string {
				h, ops, p := operationsHandler(t)
				ops.list = postgres.DeploymentList{Items: nDeployments(3)}
				return pageGet(t, h, p, "/deployments")
			},
		},
		"/deployments/logs": {
			empty: func(t *testing.T) string {
				h, _, _, p := logsPickerHandler(t, nil)
				return pageGet(t, h, p, "/deployments/logs")
			},
			populated: func(t *testing.T) string {
				h, ls, _, p := logsPickerHandler(t, nDeployments(3))
				ls.page = postgres.DeploymentLogPage{Items: []postgres.DeploymentLogLine{line(1, 1, "build", "hi")}}
				return pageGet(t, h, p, "/deployments/logs")
			},
		},
		"/environments/config": firstClass("/environments/config"),
		"/environments/route":  firstClass("/environments/route"),
		"/projects": {
			empty: func(t *testing.T) string {
				h, _, p := operationsHandler(t)
				return pageGet(t, h, p, "/projects")
			},
			populated: func(t *testing.T) string {
				h, ops, p := operationsHandler(t)
				ops.projects = []postgres.ProjectSummary{{ProjectID: "demo", HasBinding: true, BindingActive: true, RepositoryFullName: "owner/demo", RepositoryDefaultRef: "refs/heads/main"}}
				return pageGet(t, h, p, "/projects")
			},
		},
		"/github": {
			empty: func(t *testing.T) string {
				h, _, _, p := githubPageHandler(t)
				h.config.RepositoryLister = nil
				return pageGet(t, h, p, "/github")
			},
			populated: func(t *testing.T) string {
				h, ops, _, p := githubPageHandler(t)
				ops.projects = []postgres.ProjectSummary{{ProjectID: "demo", HasBinding: true, BindingActive: true, RepositoryFullName: "owner/demo", RepositoryDefaultRef: "refs/heads/main"}}
				return pageGet(t, h, p, "/github")
			},
		},
		"/servers": {
			empty: func(t *testing.T) string {
				h, _, p := serversHandler(t)
				return pageGet(t, h, p, "/servers")
			},
			populated: func(t *testing.T) string {
				h, admin, p := serversHandler(t)
				admin.servers = []postgres.ServerSummary{okServer()}
				return pageGet(t, h, p, "/servers")
			},
		},
		"/reconciliation": {
			empty: func(t *testing.T) string {
				h, _, _, p := overviewHandler(t, nil)
				return pageGet(t, h, p, "/reconciliation")
			},
			populated: func(t *testing.T) string {
				h, _, admin, p := overviewHandler(t, fourRows())
				admin.environments = blockedEnvs()
				return pageGet(t, h, p, "/reconciliation")
			},
		},
		"/deployments/detail":    {empty: detail(false), populated: detail(true)},
		"/servers/enrollment":    {empty: enrollment("", "", time.Time{}), populated: enrollment("srv_1", "tok-1", time.Date(2026, 8, 10, 0, 30, 0, 0, time.UTC))},
		"/github/setup/callback": {empty: continuePage, populated: continuePage},
		"/github/setup/complete": {empty: completePage, populated: completePage},
	}
}

// TestNoPageIsTextOnly pins owner rule R3: no page, in an empty or a populated
// state, renders text alone. The page list comes from navRoutes, so a new route
// needs a fixture here (the test fails until one exists).
func TestNoPageIsTextOnly(t *testing.T) {
	fixtures := structureFixtures()
	for _, route := range navRoutes {
		fixture, ok := fixtures[route.Path]
		if !ok {
			t.Errorf("navRoutes entry %s has no fixture in structureFixtures", route.Path)
			continue
		}
		for state, render := range map[string]func(t *testing.T) string{"empty": fixture.empty, "populated": fixture.populated} {
			t.Run(route.Path+" "+state, func(t *testing.T) {
				if content := mainContent(t, render(t)); textOnly(content) {
					t.Fatalf("%s (%s) renders text only inside <main>: %.400s", route.Path, state, content)
				}
			})
		}
	}
}

// routerPaths parses the path literals of every `case "/..."` in ServeHTTP's
// switch (web.go), so a new route cannot be added without this guard noticing.
func routerPaths(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile("web.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	start := strings.Index(text, "func (h *Handler) ServeHTTP(")
	if start < 0 {
		t.Fatal("ServeHTTP not found in web.go")
	}
	body := text[start:]
	if end := strings.Index(body, "\nfunc "); end > 0 {
		body = body[:end]
	}
	var paths []string
	for _, line := range regexp.MustCompile(`(?m)^\s*case ("/[^"]*"(, "/[^"]*")*):`).FindAllStringSubmatch(body, -1) {
		for _, lit := range regexp.MustCompile(`"(/[^"]*)"`).FindAllStringSubmatch(line[1], -1) {
			paths = append(paths, lit[1])
		}
	}
	if len(paths) < 20 {
		t.Fatalf("parsed only %d router paths; the switch layout changed", len(paths))
	}
	return paths
}

// exemptRoutes are router paths that are not operator pages, with the reason.
// Static assets are served before the switch (staticAssets) and never appear here.
var exemptRoutes = map[string]string{
	"/login":                          "sign-in page, outside the shell",
	"/logout":                         "POST only",
	"/livez":                          "liveness probe, plain text",
	"/readyz":                         "readiness probe, plain text",
	"/github/setup":                   "POST only (starts the GitHub App install)",
	"/github/bindings":                "POST only",
	"/github/installations/reconcile": "POST only",
	"/deployments/cancel":             "POST only",
	"/deployments/logs/stream":        "SSE feed for the logs page (22-3k), not a page",
	"/deployments/manual":             "POST only",
	"/deployments/rollback":           "POST only",
	"/servers/register":               "POST only (its result is the enrollment flow page)",
	"/servers/reenroll":               "POST only (its result is the enrollment flow page)",
	"/servers/revoke":                 "POST only",
	"/environments/resolve":           "POST only",
	"/environments/bindings/add":      "POST only",
	"/environments/bindings/remove":   "POST only",
}

func fullHandler(t *testing.T) (*Handler, auth.Principal) {
	t.Helper()
	h, _, _, _, p := fcHandler(t, fourRows())
	return h, p
}

func TestEveryGetRouteIsInNav(t *testing.T) {
	h, p := fullHandler(t)

	// 1. navItems and the nav-kind navRoutes agree, and every one is a link.
	nav := h.navFor(context.Background(), "home", p)
	links := map[string]bool{}
	for _, g := range nav.Groups {
		for _, item := range g.Items {
			links[item.Href] = true
		}
	}
	listed := map[string]navRoute{}
	for _, route := range navRoutes {
		listed[route.Path] = route
		if route.Kind == routeNav && !links[route.Path] {
			t.Errorf("navRoutes lists %s as a nav page but the sidebar has no link to it", route.Path)
		}
	}
	for _, def := range navItems {
		if r, ok := listed[def.Href]; !ok || r.Kind != routeNav {
			t.Errorf("navItems entry %s is not a nav-kind navRoutes entry", def.Href)
		}
	}

	// 2. The router serves every listed GET page (detail needs an id; the
	// callback needs a GitHub state and the complete page a hand-off cookie, so
	// those answer a non-200 here and are exercised by their own tests).
	for _, route := range navRoutes {
		if route.Method != "GET" || route.Kind == routeFlow {
			continue
		}
		target := route.Path
		if route.Path == "/deployments/detail" {
			target += "?id=dep-1"
		}
		if code := opsGet(h, p, http.MethodGet, target).Code; code != http.StatusOK && code != http.StatusSeeOther {
			t.Errorf("GET %s = %d, want 200 or 303", target, code)
		}
	}

	// 3. Every router path is a navRoutes entry or an explicit exemption, and an
	// exempt POST-only route must not answer an authenticated GET with a page.
	for _, path := range routerPaths(t) {
		if _, ok := listed[path]; ok {
			continue
		}
		if _, ok := exemptRoutes[path]; !ok {
			t.Errorf("router path %s is neither in navRoutes nor in exemptRoutes", path)
			continue
		}
		if strings.Contains(exemptRoutes[path], "POST only") {
			if code := opsGet(h, p, http.MethodGet, path).Code; code == http.StatusOK {
				t.Errorf("%s is exempt as POST only but answers GET with 200", path)
			}
		}
	}

	// 4. Unknown paths 404.
	for _, path := range []string{"/nope", "/deployments/other", "/environments/missing", "/servers/"} {
		if code := opsGet(h, p, http.MethodGet, path).Code; code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, code)
		}
	}
}
