package web

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"autodeploy/internal/auth"
	"autodeploy/internal/store/postgres"
)

const wantCSP = "default-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; style-src 'self'; script-src 'self'; connect-src 'self'"

// logsScriptTag is the single script tag the CSP-bound pages may carry.
const logsScriptTag = `<script src="/static/logs.js" defer></script>`

func TestContentSecurityPolicyIsExact(t *testing.T) {
	h, _, _ := serversHandler(t)
	for _, target := range []string{"/login", "/static/app.css", "/nope"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "https://admin.example.test"+target, nil))
		if got := rr.Header().Get("Content-Security-Policy"); got != wantCSP {
			t.Fatalf("%s CSP = %q", target, got)
		}
	}
}

// TestReferrerPolicyKeepsSameOriginOrigin pins same-origin: with no-referrer,
// browsers send "Origin: null" on form POSTs and provenance rejects every
// login and admin form with 400.
func TestReferrerPolicyKeepsSameOriginOrigin(t *testing.T) {
	h, _, _ := serversHandler(t)
	for _, target := range []string{"/login", "/static/app.css", "/nope"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "https://admin.example.test"+target, nil))
		if got := rr.Header().Get("Referrer-Policy"); got != "same-origin" {
			t.Fatalf("%s Referrer-Policy = %q", target, got)
		}
	}
}

func TestStylesheetRoute(t *testing.T) {
	h, _, _ := serversHandler(t)
	get := func(method, target string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(method, "https://admin.example.test"+target, nil))
		return rr
	}
	rr := get(http.MethodGet, "/static/app.css") // no session cookie
	if rr.Code != http.StatusOK || rr.Header().Get("Content-Type") != "text/css; charset=utf-8" ||
		rr.Header().Get("X-Content-Type-Options") != "nosniff" || rr.Body.Len() == 0 {
		t.Fatalf("GET = %d %q len=%d", rr.Code, rr.Header().Get("Content-Type"), rr.Body.Len())
	}
	if head := get(http.MethodHead, "/static/app.css"); head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("HEAD = %d len=%d", head.Code, head.Body.Len())
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		if rr := get(method, "/static/app.css"); rr.Code != http.StatusMethodNotAllowed || rr.Header().Get("Allow") != "GET, HEAD" {
			t.Fatalf("%s = %d allow=%q", method, rr.Code, rr.Header().Get("Allow"))
		}
	}
	for _, target := range []string{"/static/app.css?v=1", "/static/app.css?", "/static/", "/static/app.css/", "/static/other.css"} {
		if rr := get(http.MethodGet, target); rr.Code != http.StatusNotFound {
			t.Fatalf("%s = %d, want 404", target, rr.Code)
		}
	}
}

func TestStylesheetHasNoExternalReferences(t *testing.T) {
	css := string(appCSS)
	for _, banned := range []string{"@import", "url(", "http://", "https://", "@font-face"} {
		if strings.Contains(css, banned) {
			t.Fatalf("stylesheet contains %q", banned)
		}
	}
}

// TestEveryPageUsesLayoutAndStaysCSPClean renders every page template and
// proves each links the stylesheet and contains nothing the CSP would block.
func TestEveryPageUsesLayoutAndStaysCSPClean(t *testing.T) {
	h, _, p := serversHandler(t)
	h.config.RouteHeadAdmin = &routeHeadAdminStub{}
	h.config.RuntimeConfigAdmin = &runtimeConfigAdminStub{}
	h.config.OperationsReader = &overviewStub{operationsStub: &operationsStub{detail: sampleDetail("leased", true)}, rows: fourRows()}
	h.config.DeploymentLogReader = &logStub{page: postgres.DeploymentLogPage{Items: []postgres.DeploymentLogLine{line(1, 1, "build", "hi")}}}
	pages := map[string]string{}
	login := httptest.NewRecorder()
	h.ServeHTTP(login, httptest.NewRequest(http.MethodGet, "https://admin.example.test/login", nil))
	if login.Code != http.StatusOK {
		t.Fatalf("/login = %d", login.Code)
	}
	pages["/login"] = login.Body.String()
	for _, target := range []string{"/", "/environments", "/servers", "/github", "/reconciliation", "/environments/route?environment_id=env-a", "/environments/config?environment_id=env-a", "/environments/route", "/environments/config", "/projects", "/deployments", "/deployments/detail?id=dep-1", "/deployments/logs?id=dep-1", "/deployments/logs"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, serversRequest(http.MethodGet, target, nil, p, h))
		if rr.Code != http.StatusOK {
			t.Fatalf("%s = %d", target, rr.Code)
		}
		pages[target] = rr.Body.String()
	}
	var buf bytes.Buffer
	if err := enrollmentTokenTemplate.Execute(&buf, enrollmentView{Nav: h.navFor(context.Background(), "servers", p), ServerID: "s", Token: "tok", ExpiresAt: "later"}); err != nil {
		t.Fatal(err)
	}
	pages["enrollment"] = buf.String()
	buf.Reset()
	if err := githubCompleteTemplate.Execute(&buf, githubCompleteView{Nav: h.navFor(context.Background(), "github", p), CSRF: "c"}); err != nil {
		t.Fatal(err)
	}
	pages["github-complete"] = buf.String()
	for name, body := range pages {
		lower := strings.ToLower(body)
		if !strings.Contains(body, `<link rel="stylesheet" href="/static/app.css">`) {
			t.Errorf("%s does not link the stylesheet", name)
		}
		// The one allowed script tag is the same-origin live-logs enhancement,
		// and only on the explicit-id logs page.
		if name == "/deployments/logs?id=dep-1" {
			if !strings.Contains(lower, logsScriptTag) {
				t.Errorf("%s lacks the live-logs script tag", name)
			}
			lower = strings.Replace(lower, logsScriptTag, "", 1)
		}
		for _, banned := range []string{"style=", "<style", "<script", "javascript:", "onclick=", "<link rel=\"stylesheet\" href=\"http"} {
			if strings.Contains(lower, banned) {
				t.Errorf("%s contains %q", name, banned)
			}
		}
		if name != "/login" && !strings.Contains(body, `<nav aria-label="Primary">`) {
			t.Errorf("%s is missing the shared nav", name)
		}
	}
}

func navTestHandler(t *testing.T) (*Handler, auth.Principal) {
	t.Helper()
	h, _, p := serversHandler(t)
	h.config.OperationsReader = &overviewStub{operationsStub: &operationsStub{detail: sampleDetail("leased", true)}}
	return h, p
}

func navPage(t *testing.T, h *Handler, p auth.Principal, target string) string {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, serversRequest(http.MethodGet, target, nil, p, h))
	if rr.Code != http.StatusOK {
		t.Fatalf("%s = %d", target, rr.Code)
	}
	return rr.Body.String()
}

func TestNavShowsAvailablePages(t *testing.T) {
	h, p := navTestHandler(t)
	pages := []struct{ target, currentHref string }{
		{"/", "/"},
		{"/environments", "/environments"},
		{"/deployments", "/deployments"},
		{"/deployments/detail?id=dep-1", "/deployments"},
		{"/projects", "/projects"},
		{"/servers", "/servers"},
		{"/github", "/github"},
		{"/reconciliation", "/reconciliation"},
	}
	for _, pg := range pages {
		body := navPage(t, h, p, pg.target)
		for _, href := range []string{"/", "/environments", "/deployments", "/projects", "/github", "/servers", "/reconciliation"} {
			if !strings.Contains(body, `href="`+href+`"`) {
				t.Errorf("%s nav lacks href %q", pg.target, href)
			}
		}
		if n := strings.Count(body, `aria-current="page"`); n != 1 {
			t.Errorf("%s has %d aria-current, want 1", pg.target, n)
		}
		if !strings.Contains(body, `href="`+pg.currentHref+`" aria-current="page"`) {
			t.Errorf("%s: current item is not %q", pg.target, pg.currentHref)
		}
	}

	noServers, p2 := navTestHandler(t)
	noServers.config.ServerAdmin = nil
	if body := navPage(t, noServers, p2, "/projects"); strings.Contains(body, `href="/servers"`) {
		t.Error("nav shows /servers with ServerAdmin nil")
	} else if strings.Contains(body, `href="/reconciliation"`) {
		t.Error("nav shows /reconciliation with ServerAdmin nil")
	}
	// An OperationsReader without the overview read omits Environments (its route 404s).
	noOverview, p4 := navTestHandler(t)
	noOverview.config.OperationsReader = &operationsStub{}
	if body := navPage(t, noOverview, p4, "/projects"); strings.Contains(body, `href="/environments"`) || !strings.Contains(body, `href="/projects"`) {
		t.Error("nav Environments item must follow the overview reader only")
	}
	noOps, p3 := navTestHandler(t)
	noOps.config.OperationsReader = nil
	body := navPage(t, noOps, p3, "/")
	for _, href := range []string{"/environments", "/projects", "/deployments"} {
		if strings.Contains(body, `href="`+href+`"`) {
			t.Errorf("nav shows %s with OperationsReader nil", href)
		}
	}
}

func TestNavRendersSignOutForm(t *testing.T) {
	h, p := navTestHandler(t)
	body := navPage(t, h, p, "/projects")
	if !strings.Contains(body, `<form method="post" action="/logout">`) {
		t.Fatal("sign-out form missing from nav")
	}
	if want := `name="csrf" value="` + serversCSRF(t, h, p) + `"`; !strings.Contains(body, want) {
		t.Fatalf("sign-out form lacks session csrf field %q", want)
	}
}

func TestLandingHasSingleSignOut(t *testing.T) {
	h, p := navTestHandler(t)
	if n := strings.Count(navPage(t, h, p, "/"), `action="/logout"`); n != 1 {
		t.Fatalf("landing has %d sign-out forms, want 1 (sidebar only)", n)
	}
}

func TestStylesheetGuards(t *testing.T) {
	css := string(appCSS)
	for _, banned := range []string{"url(", "@import", "background-clip", "backdrop-filter"} {
		if strings.Contains(css, banned) {
			t.Errorf("stylesheet contains %q", banned)
		}
	}
	// Side borders (physical or logical) may only be 0 or 1px; var(), keywords
	// (thin/medium/thick) and wider widths are refused (impeccable side-stripe ban).
	side := regexp.MustCompile(`border-(?:left|right|inline(?:-start|-end)?)(?:-width)?\s*:\s*([^;}]+)`)
	allowed := regexp.MustCompile(`^\s*(?:0(?:px)?|1px)(?:\s|$)`)
	for _, m := range side.FindAllStringSubmatch(css, -1) {
		if !allowed.MatchString(m[1]) {
			t.Errorf("side border wider than 1px: %q", m[0])
		}
	}
}
