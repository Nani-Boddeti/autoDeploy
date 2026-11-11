package web

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"autodeploy/internal/store/postgres"
)

const wantCSP = "default-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; style-src 'self'"

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
	h.config.OperationsReader = &operationsStub{detail: sampleDetail("leased", true)}
	h.config.DeploymentLogReader = &logStub{page: postgres.DeploymentLogPage{Items: []postgres.DeploymentLogLine{line(1, 1, "build", "hi")}}}
	pages := map[string]string{}
	login := httptest.NewRecorder()
	h.ServeHTTP(login, httptest.NewRequest(http.MethodGet, "https://admin.example.test/login", nil))
	if login.Code != http.StatusOK {
		t.Fatalf("/login = %d", login.Code)
	}
	pages["/login"] = login.Body.String()
	for _, target := range []string{"/", "/servers", "/environments/route?environment_id=env-a", "/environments/config?environment_id=env-a", "/projects", "/deployments", "/deployments/detail?id=dep-1", "/deployments/logs?id=dep-1"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, serversRequest(http.MethodGet, target, nil, p, h))
		if rr.Code != http.StatusOK {
			t.Fatalf("%s = %d", target, rr.Code)
		}
		pages[target] = rr.Body.String()
	}
	var buf bytes.Buffer
	if err := enrollmentTokenTemplate.Execute(&buf, struct{ ServerID, Token, ExpiresAt string }{"s", "tok", "later"}); err != nil {
		t.Fatal(err)
	}
	pages["enrollment"] = buf.String()
	buf.Reset()
	if err := githubCompleteTemplate.Execute(&buf, struct{ CSRF string }{"c"}); err != nil {
		t.Fatal(err)
	}
	pages["github-complete"] = buf.String()
	for name, body := range pages {
		lower := strings.ToLower(body)
		if !strings.Contains(body, `<link rel="stylesheet" href="/static/app.css">`) {
			t.Errorf("%s does not link the stylesheet", name)
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
