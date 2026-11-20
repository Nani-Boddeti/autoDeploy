package web

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"autodeploy/internal/store/postgres"
)

// enrollmentPage renders the token page through the real handler method with
// the test handler's fixed clock (2026-08-10T00:00:00Z).
func enrollmentPage(t *testing.T, serverID, token string, expires time.Time) string {
	t.Helper()
	h, _, p := serversHandler(t)
	return renderTo(t, func(w http.ResponseWriter) {
		h.renderEnrollmentToken(w, httptest.NewRequest(http.MethodPost, "https://admin.example.test/servers/enrollment", nil), p, serverID, token, expires)
	})
}

func TestEnrollmentTokenPage(t *testing.T) {
	const token = "tok-UNIQUE-0123456789abcdef"
	expires := time.Date(2026, 8, 10, 0, 14, 30, 0, time.UTC)
	page := enrollmentPage(t, "srv_9", token, expires)
	contains(t, page,
		`<pre class="token">`+token+`</pre>`,
		`srv_9`, `2026-08-10T00:14:30Z`, `valid for 15 minutes`,
		`read -rs -p &#39;enrollment token: &#39; T; echo`,
		`agent enroll \`,
		`--base-url https://admin.example.test --server-id srv_9 --state-dir /var/lib/autodeploy/agent`,
		`href="/servers"`, hrefAttr("/"),
	)
	if n := strings.Count(page, token); n != 1 {
		t.Fatalf("token appears %d times, want exactly 1", n)
	}
	if strings.Contains(page, "<script") || strings.Contains(page, "clipboard") {
		t.Fatal("enrollment page uses script or a copy button")
	}
	// An expired or unknown expiry hides the minutes sentence instead of showing 0.
	lacks(t, enrollmentPage(t, "srv_9", token, time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)), `valid for`)
}

func TestEnrollmentCommandNeverCarriesToken(t *testing.T) {
	cmd := enrollCommand("https://cp.example.test", "srv_1")
	for _, want := range []string{"read -rs", `printf '%s\n' "$T"`, "sudo -u autodeploy-agent", "agent enroll", "--base-url https://cp.example.test", "--server-id srv_1", "--state-dir /var/lib/autodeploy/agent", "unset T"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("command lacks %q:\n%s", want, cmd)
		}
	}
}

func TestGitHubFlowStepIndicators(t *testing.T) {
	nav := func() navView {
		h, p := navTestHandler(t)
		return h.navFor(context.Background(), "github", p)
	}
	var cont, complete bytes.Buffer
	if err := githubContinueTemplate.Execute(&cont, githubContinueView{Nav: nav()}); err != nil {
		t.Fatal(err)
	}
	if err := githubCompleteTemplate.Execute(&complete, githubCompleteView{Nav: nav(), CSRF: "c"}); err != nil {
		t.Fatal(err)
	}
	contains(t, cont.String(), `<li aria-current="step"><strong>Install App</strong></li><li>Confirm</li><li>Bind repository</li>`, `<a class="btn" href="/github/setup/complete">Continue</a>`)
	contains(t, complete.String(), `<li class="done">Install App</li><li aria-current="step"><strong>Confirm</strong></li><li>Bind repository</li>`, `<form method="post" action="/github/setup/complete"><input type="hidden" name="csrf" value="c"><button type="submit">Confirm</button></form>`)
	for name, page := range map[string]string{"continue": cont.String(), "complete": complete.String()} {
		if n := strings.Count(page, `aria-current="step"`); n != 1 {
			t.Errorf("%s has %d current steps, want 1", name, n)
		}
	}
}

func TestServersZeroShowsRegisterFormAndHostHint(t *testing.T) {
	h, _, p := serversHandler(t)
	page := pageGet(t, h, p, "/servers")
	contains(t, page, `action="/servers/register"`, `name="name"`, `<pre class="command">read -rs`, `--server-id &lt;SERVER_ID&gt;`, `--base-url https://admin.example.test`)
	lacks(t, page, `<table`, `<th>Enrollment</th>`)
}

func TestServersWithServersHaveNoHint(t *testing.T) {
	h, admin, p := serversHandler(t)
	admin.servers = []postgres.ServerSummary{okServer()}
	page := pageGet(t, h, p, "/servers")
	contains(t, page, `<table`, `action="/servers/register"`)
	lacks(t, page, `<pre class="command">`)
}

func TestProjectsAndDeploymentsZeroShowChecklist(t *testing.T) {
	for _, target := range []string{"/projects", "/deployments"} {
		h, _, p := operationsHandler(t)
		page := pageGet(t, h, p, target)
		contains(t, page, `class="checklist"`, hrefAttr("/github"), hrefAttr("/servers"))
		lacks(t, page, `class="empty"`)
	}
}

func TestDeploymentsFilteredZeroKeepsFilterAndClear(t *testing.T) {
	h, _, p := operationsHandler(t)
	page := pageGet(t, h, p, "/deployments?project_id=nope")
	contains(t, page, `<form method="get" action="/deployments" class="filter-form">`, `No deployments match these filters.`, `href="/deployments">Clear filters</a>`)
	lacks(t, page, `class="checklist"`)
}

// TestDeploymentsEmptyChecklistReflectsBinding: with zero deployments but a
// bound project, the checklist's bind step is done.
func TestDeploymentsEmptyChecklistReflectsBinding(t *testing.T) {
	h, stub, p := operationsHandler(t)
	unbound := opsGet(h, p, http.MethodGet, "/deployments").Body.String()
	stub.projects = []postgres.ProjectSummary{{ProjectID: "proj-b", HasBinding: true, BindingActive: true}}
	bound := opsGet(h, p, http.MethodGet, "/deployments").Body.String()
	todo := func(s string) int { return strings.Count(s, `class="step-state">To do<`) }
	if !strings.Contains(unbound, `class="checklist`) || !strings.Contains(bound, `class="checklist`) {
		t.Fatal("empty deployments page lost its checklist")
	}
	if todo(bound) != todo(unbound)-1 {
		t.Fatalf("bound project: to-do steps = %d, want %d", todo(bound), todo(unbound)-1)
	}
}

func TestEnrollCommandRejectsHostileValues(t *testing.T) {
	for name, tc := range map[string]struct{ origin, id, want, lacks string }{
		"hostile server id":           {"https://cp.example.test", "srv; rm -rf /", "--server-id <SERVER_ID>", "rm -rf"},
		"command substitution origin": {"https://cp.$(id).example.test", "srv_1", "--base-url https://<CP_HOST>", "$(id)"},
		"quote in id":                 {"https://cp.example.test", "a'b", "--server-id <SERVER_ID>", "a'b"},
		"http origin":                 {"http://cp.example.test", "srv_1", "--base-url https://<CP_HOST>", "http://cp"},
		"origin with path":            {"https://cp.example.test/x", "srv_1", "--base-url https://<CP_HOST>", "/x "},
	} {
		cmd := enrollCommand(tc.origin, tc.id)
		if !strings.Contains(cmd, tc.want) || (tc.lacks != "" && strings.Contains(cmd, tc.lacks)) {
			t.Errorf("%s: command = %q", name, cmd)
		}
	}
	if cmd := enrollCommand("https://cp.example.test:8443", "srv_1"); !strings.Contains(cmd, "--base-url https://cp.example.test:8443 --server-id srv_1") {
		t.Errorf("valid values were altered: %q", cmd)
	}
}

func TestEnrollmentValidMinutesWording(t *testing.T) {
	now := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		left time.Duration
		want string
	}{{60 * time.Second, "valid for 1 minute)"}, {59 * time.Second, "valid for 1 minute)"}, {120 * time.Second, "valid for 2 minutes)"}} {
		page := enrollmentPage(t, "srv_9", "tok-UNIQUE-0123456789abcdef", now.Add(tc.left))
		if !strings.Contains(page, tc.want) {
			t.Errorf("%v: page lacks %q", tc.left, tc.want)
		}
	}
}

func TestChecklistOmitsServersLinkWithoutServerAdmin(t *testing.T) {
	for _, target := range []string{"/projects", "/deployments"} {
		h, _, p := operationsHandler(t)
		h.config.ServerAdmin = nil
		page := mainContent(t, pageGet(t, h, p, target))
		contains(t, page, `class="checklist"`, `Register a server`, hrefAttr("/github"))
		lacks(t, page, `href="/servers"`)
	}
}

func TestStylesheetPreRulesAreScoped(t *testing.T) {
	css := string(appCSS)
	for _, rule := range regexp.MustCompile(`(?m)^pre[^{]*\{[^}]*\}`).FindAllString(css, -1) {
		if !strings.HasPrefix(rule, "pre.token") && !strings.HasPrefix(rule, "pre.command") {
			t.Errorf("unscoped pre rule would restyle log lines: %s", rule)
		}
	}
	if regexp.MustCompile(`(?m)^pre\s*(,|\{)`).MatchString(css) {
		t.Error("bare pre rule found")
	}
	for _, want := range []string{".logs pre { min-width: 0; margin: 0; white-space: pre-wrap; overflow-wrap: anywhere; font: 0.88rem/1.45 var(--mono); }"} {
		if !strings.Contains(css, want) {
			t.Errorf("log line rule changed: %s", want)
		}
	}
}
