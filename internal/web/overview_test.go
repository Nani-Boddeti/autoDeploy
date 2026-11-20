package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"autodeploy/internal/auth"
	"autodeploy/internal/store/postgres"
)

// overviewStub is an OperationsReader that also implements EnvironmentOverviewReader.
type overviewStub struct {
	*operationsStub
	rows []postgres.EnvironmentOverview
	err  error
}

func (s *overviewStub) ListEnvironmentOverview(_ context.Context, p auth.Principal) ([]postgres.EnvironmentOverview, error) {
	s.gotOwner = p.OwnerID
	if s.err != nil {
		return nil, s.err
	}
	return s.rows, nil
}

type repoListerStub struct {
	repos []postgres.BindableRepository
	err   error
}

func (s repoListerStub) ListBindableRepositories(context.Context, auth.Principal) ([]postgres.BindableRepository, error) {
	return s.repos, s.err
}

// overviewNow is the handler's fixed clock (see testHandler).
var overviewNow = time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)

// overviewHandler wires every optional dependency the board actions need.
func overviewHandler(t *testing.T, rows []postgres.EnvironmentOverview) (*Handler, *overviewStub, *serverAdminStub, auth.Principal) {
	t.Helper()
	h, admin, p := serversHandler(t)
	ops := &overviewStub{operationsStub: &operationsStub{}, rows: rows}
	h.config.OperationsReader = ops
	h.config.ManualDeploy, h.config.CommitResolver = &manualStoreStub{ref: "refs/heads/main"}, &resolverStub{}
	h.config.Rollback = &rollbackStoreStub{eligible: true}
	h.config.DeploymentLogReader = &logStub{}
	h.config.ActivationEnabled = true
	return h, ops, admin, p
}

func okServer() postgres.ServerSummary {
	return postgres.ServerSummary{ID: "srv-1", Name: "acer", Status: "active", LastHeartbeatAt: overviewNow.Add(-30 * time.Second), ActiveCredentialVersion: 3}
}

// fourRows: owned+deployed+rollbackable, bound-only, blocked, failed.
func fourRows() []postgres.EnvironmentOverview {
	return []postgres.EnvironmentOverview{
		{
			EnvironmentID: "env-live", ProjectID: "proj-live", Bound: true, BindingActive: true, DeployRef: "refs/heads/main",
			ServerID: "srv-1", ServerName: "acer", ServerStatus: "active", LastHeartbeatAt: overviewNow.Add(-30 * time.Second),
			Owned: true, Hostname: "live.example.test", RouteRevision: 7, Strategy: "recreate",
			ActiveReleaseID: "rel-live", ActiveCommit: "0123456789abcdef0123", ActiveSince: overviewNow.Add(-3 * time.Hour),
			Latest:                  &postgres.DeploymentSummary{DeploymentID: "dep-live", ProjectID: "proj-live", EnvironmentID: "env-live", Status: "healthy", CommitSHA: "0123456789abcdef0123", CreatedAt: overviewNow.Add(-3 * time.Hour)},
			RollbackTargetReleaseID: "rel-prev", RollbackTargetCommit: "fedcba9876543210fedc",
		},
		{
			EnvironmentID: "env-new", ProjectID: "proj-new", Bound: true, BindingActive: true, DeployRef: "refs/heads/main",
			ServerID: "srv-1", ServerName: "acer", ServerStatus: "active", LastHeartbeatAt: overviewNow.Add(-30 * time.Second),
		},
		{
			EnvironmentID: "env-blocked", ProjectID: "proj-blocked", Bound: true, BindingActive: true,
			ServerID: "srv-1", ServerName: "acer", ServerStatus: "active", LastHeartbeatAt: overviewNow.Add(-30 * time.Second),
			Owned: true, ReconciliationRequired: true, ReconcilingAttemptID: 42,
			Latest: &postgres.DeploymentSummary{DeploymentID: "dep-blocked", Status: "activating", CommitSHA: "aaaaaaaaaaaaaaaaaaaa", CreatedAt: overviewNow.Add(-time.Hour)},
		},
		{
			EnvironmentID: "env-failed", ProjectID: "proj-failed", Bound: true, BindingActive: true,
			ServerID: "srv-1", ServerName: "acer", ServerStatus: "active", LastHeartbeatAt: overviewNow.Add(-30 * time.Second),
			Owned:              true,
			Latest:             &postgres.DeploymentSummary{DeploymentID: "dep-failed", Status: "failed", CommitSHA: "bbbbbbbbbbbbbbbbbbbb", CreatedAt: overviewNow.Add(-2 * time.Hour)},
			LatestFailureClass: "permanent_configuration", LatestFailureReason: "container_unhealthy",
		},
	}
}

func contains(t *testing.T, page string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(page, w) {
			t.Errorf("page lacks %q", w)
		}
	}
}

func lacks(t *testing.T, page string, unwanted ...string) {
	t.Helper()
	for _, w := range unwanted {
		if strings.Contains(page, w) {
			t.Errorf("page unexpectedly contains %q", w)
		}
	}
}

// rowFragment returns the board row of one environment (an <li class="board-row"> block).
func rowFragment(t *testing.T, page, env string) string {
	t.Helper()
	i := strings.Index(page, `data-env="`+env+`"`)
	if i < 0 {
		t.Fatalf("no row for %s", env)
	}
	rest := page[i:]
	if j := strings.Index(rest[1:], `data-env="`); j >= 0 {
		rest = rest[:j+1]
	}
	return rest
}

func TestOverviewBoardRendersAllFields(t *testing.T) {
	h, _, admin, p := overviewHandler(t, fourRows())
	admin.servers = []postgres.ServerSummary{okServer()}
	page := pageGet(t, h, p, "/")

	live := rowFragment(t, page, "env-live")
	contains(t, live,
		"env-live", "proj-live",
		hrefAttr("/environments/config?environment_id=env-live"), hrefAttr("/environments/route?environment_id=env-live"),
		hrefAttr("/deployments?environment_id=env-live"), hrefAttr("/deployments/logs?id=dep-live"),
		hrefAttr("https://live.example.test"), "live.example.test",
		"acer", `class="pill ok"`,
		"0123456789ab", "3h ago",
		`>healthy<`, hrefAttr("/deployments/detail?id=dep-live"),
	)
	lacks(t, live, "Not deployed yet", `class="reason"`)

	fresh := rowFragment(t, page, "env-new")
	contains(t, fresh, "env-new", "Not deployed yet", "acer")
	lacks(t, fresh, "/deployments/logs?id=")

	blocked := rowFragment(t, page, "env-blocked")
	contains(t, blocked, "blocked", "reconcil")

	failed := rowFragment(t, page, "env-failed")
	contains(t, failed, `>failed<`, `class="reason"`, "Configuration problem", "Image HEALTHCHECK reported unhealthy")
	lacks(t, live, "Configuration problem")
}

func TestOverviewFailureReasonOnlyForFailedStatus(t *testing.T) {
	rows := []postgres.EnvironmentOverview{{
		EnvironmentID: "env-x", ProjectID: "p", Owned: true,
		Latest:             &postgres.DeploymentSummary{DeploymentID: "d", Status: "healthy", CommitSHA: "cc"},
		LatestFailureClass: "transient", LatestFailureReason: "deadline_exceeded",
	}}
	h, _, _, p := overviewHandler(t, rows)
	page := pageGet(t, h, p, "/")
	lacks(t, page, "Temporary failure", "Health check deadline exceeded")
}

func TestOverviewUnknownFailureValuesRenderRawEscaped(t *testing.T) {
	rows := []postgres.EnvironmentOverview{{
		EnvironmentID: "env-x", ProjectID: "p", Owned: true,
		Latest:             &postgres.DeploymentSummary{DeploymentID: "d", Status: "failed", CommitSHA: "cc"},
		LatestFailureClass: "new_class", LatestFailureReason: "<b>odd</b>",
	}}
	h, _, _, p := overviewHandler(t, rows)
	page := pageGet(t, h, p, "/")
	contains(t, page, "new_class", "&lt;b&gt;odd&lt;/b&gt;")
	lacks(t, page, "<b>odd</b>")
}

func TestOverviewActionFormsCarryExactFields(t *testing.T) {
	h, _, admin, p := overviewHandler(t, fourRows())
	admin.servers = []postgres.ServerSummary{okServer()}
	page := pageGet(t, h, p, "/")
	csrf := serversCSRF(t, h, p)

	live := rowFragment(t, page, "env-live")
	contains(t, live,
		`action="/deployments/manual"`, `name="csrf" value="`+csrf+`"`, `name="project_id" value="proj-live"`, `name="request_nonce" value="`,
		`name="mode" value="head"`, `name="commit_sha" value=""`, `name="return_to" value="/"`,
		`action="/deployments/rollback"`, `name="release_id" value="rel-prev"`, `type="checkbox" name="confirm" value="rollback" required`,
	)
	lacks(t, live, `action="/environments/resolve"`)

	blocked := rowFragment(t, page, "env-blocked")
	contains(t, blocked,
		`action="/environments/resolve"`, `name="environment_id" value="env-blocked"`, `name="attempt_id" value="42"`,
		`name="outcome"`, `value="activated"`, `value="not_activated"`, `value="no_activation"`, `name="reason"`, `name="return_to" value="/"`,
	)
	lacks(t, blocked, `action="/deployments/rollback"`)

	// Nonces are fresh per form.
	seen := map[string]bool{}
	for _, part := range strings.Split(page, `name="request_nonce" value="`)[1:] {
		n := part[:strings.Index(part, `"`)]
		if len(n) < 16 || seen[n] {
			t.Fatalf("nonce %q reused or too short", n)
		}
		seen[n] = true
	}
	if len(seen) < 3 {
		t.Fatalf("expected a nonce per deploy/rollback form, got %d", len(seen))
	}
}

func TestOverviewFormsOnlyWhenAllowed(t *testing.T) {
	// No deploy deps: no Deploy form. No Rollback dep: no Roll back form. No ServerAdmin: no Resolve.
	h, _, _, p := overviewHandler(t, fourRows())
	h.config.CommitResolver = nil
	h.config.Rollback = nil
	h.config.ServerAdmin = nil
	page := pageGet(t, h, p, "/")
	lacks(t, page, `action="/deployments/manual"`, `action="/deployments/rollback"`, `action="/environments/resolve"`)

	// Inactive binding or unbound row: no Deploy form.
	rows := []postgres.EnvironmentOverview{
		{EnvironmentID: "env-inactive", ProjectID: "p1", Bound: true, BindingActive: false},
		{EnvironmentID: "env-unbound", ProjectID: "p2", Owned: true},
	}
	h2, _, _, p2 := overviewHandler(t, rows)
	lacks(t, pageGet(t, h2, p2, "/"), `action="/deployments/manual"`)

	// Rollback offer follows the store target only.
	rows = fourRows()
	rows[0].RollbackTargetReleaseID = ""
	h3, _, _, p3 := overviewHandler(t, rows)
	lacks(t, pageGet(t, h3, p3, "/"), `action="/deployments/rollback"`)
}

func TestOverviewChecklistStates(t *testing.T) {
	// Zero environments: checklist is the main content, all three steps to do.
	h, _, admin, p := overviewHandler(t, nil)
	page := pageGet(t, h, p, "/")
	contains(t, page, `class="checklist"`, `id="setup"`)
	if n := strings.Count(page, `class="step-state">To do<`); n != 3 {
		t.Fatalf("zero state: %d undone steps, want 3", n)
	}
	lacks(t, page, `class="checklist compact"`)
	// R3: no text-only empty sections; the checklist covers servers and deployments.
	lacks(t, page, `class="empty"`, `id="recent-title"`, `id="servers-title"`)

	// Partial: server registered and a repository connected, nothing bound; envs exist -> compact.
	h2, ops2, admin2, p2 := overviewHandler(t, []postgres.EnvironmentOverview{{EnvironmentID: "e", ProjectID: "p", Owned: true}})
	_ = ops2
	admin2.servers = []postgres.ServerSummary{okServer()}
	h2.config.RepositoryLister = repoListerStub{repos: []postgres.BindableRepository{{RepositoryID: 1, FullName: "o/r"}}}
	page2 := pageGet(t, h2, p2, "/")
	contains(t, page2, `class="checklist compact"`)
	if d, u := strings.Count(page2, `class="step-state">Done<`), strings.Count(page2, `class="step-state">To do<`); d != 2 || u != 1 {
		t.Fatalf("partial: done=%d todo=%d, want 2/1", d, u)
	}

	// All done: checklist hidden.
	rows := fourRows()
	h3, _, admin3, p3 := overviewHandler(t, rows)
	admin3.servers = []postgres.ServerSummary{okServer()}
	h3.config.RepositoryLister = repoListerStub{repos: []postgres.BindableRepository{{RepositoryID: 1, FullName: "o/r"}}}
	lacks(t, pageGet(t, h3, p3, "/"), `class="checklist`)
	_ = admin
}

func TestOverviewWithoutOverviewReaderStillRenders(t *testing.T) {
	h, admin, p := serversHandler(t)
	admin.servers = []postgres.ServerSummary{okServer()}
	h.config.OperationsReader = &operationsStub{}
	page := pageGet(t, h, p, "/")
	contains(t, page, `class="checklist"`, "acer", hrefAttr("/github"))
	lacks(t, page, `class="board"`)

	h.config.OperationsReader = nil
	contains(t, pageGet(t, h, p, "/"), `class="checklist"`)
}

func TestOverviewPrimaryReadFailureDegrades(t *testing.T) {
	h, ops, _, p := overviewHandler(t, nil)
	ops.err = errors.New("boom secret detail")
	rr := opsGet(h, p, http.MethodGet, "/")
	body := rr.Body.String()
	if rr.Code != http.StatusOK || strings.Contains(body, "secret") {
		t.Fatalf("primary failure = %d", rr.Code)
	}
	contains(t, body, "Environments could not be loaded.", hrefAttr("/github"), `class="checklist"`)
	lacks(t, body, `class="board"`)

	// /environments has nothing else to show: it keeps the operationsFailure response.
	if rr := opsGet(h, p, http.MethodGet, "/environments"); rr.Code != http.StatusInternalServerError {
		t.Fatalf("/environments failure = %d", rr.Code)
	}
}

func TestOverviewPrimaryReadDeniedRedirectsToLogin(t *testing.T) {
	h, ops, _, p := overviewHandler(t, nil)
	ops.err = postgres.ErrServerAuthorizationDenied
	rr := opsGet(h, p, http.MethodGet, "/")
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatalf("denied = %d %q", rr.Code, rr.Header().Get("Location"))
	}
}

func TestOverviewOptionalReadsDegrade(t *testing.T) {
	h, ops, admin, p := overviewHandler(t, fourRows())
	admin.listErr = errors.New("nope")
	ops.operationsStub.err = errors.New("nope")
	rr := opsGet(h, p, http.MethodGet, "/")
	if rr.Code != http.StatusOK {
		t.Fatalf("degraded = %d", rr.Code)
	}
	contains(t, rr.Body.String(), "Servers could not be loaded", "Recent deployments could not be loaded")
}

func TestOverviewAttentionItems(t *testing.T) {
	h, _, admin, p := overviewHandler(t, fourRows())
	admin.servers = []postgres.ServerSummary{
		okServer(),
		{ID: "srv-stale", Name: "stale-box", Status: "active", LastHeartbeatAt: overviewNow.Add(-10 * time.Minute)},
		{ID: "srv-gone", Name: "gone-box", Status: "revoked", LastHeartbeatAt: overviewNow.Add(-time.Hour)},
	}
	h.config.ActivationEnabled = false
	page := pageGet(t, h, p, "/")
	attention := page[strings.Index(page, `class="attention"`):]
	attention = attention[:strings.Index(attention, "</section>")]
	contains(t, attention,
		"env-blocked", hrefAttr("/reconciliation"),
		"stale-box", "gone-box", "revoked",
		"env-failed", "Configuration problem",
		"Traffic activation is disabled",
	)
	lacks(t, attention, ">acer<")

	// Healthy fleet: no attention section.
	h2, _, admin2, p2 := overviewHandler(t, fourRows()[:2])
	admin2.servers = []postgres.ServerSummary{okServer()}
	lacks(t, pageGet(t, h2, p2, "/"), `class="attention"`)
}

func TestOverviewHeartbeatStates(t *testing.T) {
	rows := []postgres.EnvironmentOverview{
		{EnvironmentID: "e-fresh", ProjectID: "p", ServerID: "s-a", ServerName: "a", ServerStatus: "active", LastHeartbeatAt: overviewNow.Add(-time.Minute)},
		{EnvironmentID: "e-old", ProjectID: "p", ServerID: "s-b", ServerName: "b", ServerStatus: "active", LastHeartbeatAt: overviewNow.Add(-5 * time.Minute)},
		{EnvironmentID: "e-never", ProjectID: "p", ServerID: "s-c", ServerName: "c", ServerStatus: "active"},
	}
	h, _, _, p := overviewHandler(t, rows)
	page := pageGet(t, h, p, "/")
	contains(t, rowFragment(t, page, "e-fresh"), `class="pill ok"`)
	contains(t, rowFragment(t, page, "e-old"), `class="pill warn"`)
	contains(t, rowFragment(t, page, "e-never"), `class="pill warn"`, "never")
}

func TestOverviewRecentDeploymentsAndServers(t *testing.T) {
	h, ops, admin, p := overviewHandler(t, nil)
	ops.list = postgres.DeploymentList{Items: []postgres.DeploymentSummary{
		{DeploymentID: "dep-r1", ProjectID: "pr", EnvironmentID: "er", Status: "failed", CommitSHA: "dddddddddddddddddddd", CreatedAt: overviewNow.Add(-time.Hour), FailureClass: "transient", FailureReason: "deadline_exceeded"},
		{DeploymentID: "dep-r2", ProjectID: "pr", EnvironmentID: "er", Status: "healthy", CommitSHA: "eeeeeeeeeeeeeeeeeeee", CreatedAt: overviewNow.Add(-2 * time.Hour), FailureClass: "transient"},
	}}
	admin.servers = []postgres.ServerSummary{okServer()}
	page := pageGet(t, h, p, "/")
	if ops.operationsStub.gotPage.Limit != 10 {
		t.Fatalf("recent limit = %d, want 10", ops.operationsStub.gotPage.Limit)
	}
	contains(t, page, hrefAttr("/deployments/detail?id=dep-r1"), hrefAttr("/deployments/detail?id=dep-r2"), "Temporary failure", "Health check deadline exceeded", "srv-1", "acer", ">3<")
	if strings.Count(page, "Temporary failure") != 1 {
		t.Fatal("failure class shown for a non-failed deployment")
	}
}

// ---- Environments page ----

func TestEnvironmentsPageRows(t *testing.T) {
	h, _, admin, p := overviewHandler(t, fourRows())
	admin.servers = []postgres.ServerSummary{okServer()}
	page := pageGet(t, h, p, "/environments")
	contains(t, page, `aria-current="page">Environments<`)
	live := rowFragment(t, page, "env-live")
	contains(t, live,
		"proj-live", "acer", hrefAttr("https://live.example.test"), "recreate", ">7<", "rel-live", "0123456789ab",
		`>healthy<`, hrefAttr("/environments/config?environment_id=env-live"), hrefAttr("/environments/route?environment_id=env-live"),
		hrefAttr("/deployments?environment_id=env-live"), hrefAttr("/deployments/logs?id=dep-live"),
		`action="/deployments/manual"`, `name="return_to" value="/environments"`, `action="/deployments/rollback"`, `name="release_id" value="rel-prev"`,
	)
	lacks(t, live, `name="return_to" value="/"`)
	contains(t, rowFragment(t, page, "env-new"), "Not deployed yet", `action="/deployments/manual"`)
	contains(t, rowFragment(t, page, "env-blocked"), `action="/environments/resolve"`, `name="attempt_id" value="42"`, "blocked", `name="return_to" value="/environments"`)
	contains(t, rowFragment(t, page, "env-failed"), "Configuration problem", "Image HEALTHCHECK reported unhealthy")
}

func TestEnvironmentsPageZeroShowsChecklist(t *testing.T) {
	h, _, _, p := overviewHandler(t, nil)
	contains(t, pageGet(t, h, p, "/environments"), `class="checklist"`)
}

func TestEnvironmentsPageNotFound(t *testing.T) {
	h, _, _, p := overviewHandler(t, fourRows())
	if rr := opsGet(h, p, http.MethodGet, "/environments?x=1"); rr.Code != http.StatusNotFound {
		t.Fatalf("query = %d", rr.Code)
	}
	h.config.OperationsReader = &operationsStub{}
	if rr := opsGet(h, p, http.MethodGet, "/environments"); rr.Code != http.StatusNotFound {
		t.Fatalf("nil reader = %d", rr.Code)
	}
	h.config.OperationsReader = nil
	if rr := opsGet(h, p, http.MethodGet, "/environments"); rr.Code != http.StatusNotFound {
		t.Fatalf("nil ops = %d", rr.Code)
	}
}

func TestEnvironmentsPageRejectsPost(t *testing.T) {
	h, _, _, p := overviewHandler(t, fourRows())
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, serversRequest(http.MethodPost, "/environments", url.Values{"x": {"y"}}, p, h))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d", rr.Code)
	}
}

// ---- return_to ----

func TestReturnToAllowlist(t *testing.T) {
	type post struct {
		name, target, wantDefault string
		form                      func(h *Handler, p auth.Principal) url.Values
	}
	posts := []post{
		{"manual", "/deployments/manual", "/deployments/detail?id=mnd_abc", func(h *Handler, p auth.Principal) url.Values {
			return manualForm(t, h, p)
		}},
		{"rollback", "/deployments/rollback", "/deployments/detail?id=rbd_abc", func(h *Handler, p auth.Principal) url.Values {
			return rollbackForm(t, h, p)
		}},
		{"resolve", "/environments/resolve", "/reconciliation", func(h *Handler, p auth.Principal) url.Values {
			return url.Values{"csrf": {serversCSRF(t, h, p)}, "environment_id": {"env-a"}, "attempt_id": {"5"}, "outcome": {"activated"}, "reason": {"checked by hand"}}
		}},
	}
	cases := []struct{ value, want string }{
		{"/", "/"},
		{"/environments", "/environments"},
		{"/servers", "/servers"},
		{"/reconciliation", "/reconciliation"},
		{"//evil.example", ""},
		{"https://evil.example", ""},
		{`/\evil`, ""},
		{"/environments?x=1", ""},
		{"/deployments/detail?id=x", ""},
		{"", ""},
		{"javascript:alert(1)", ""},
		{"/environments/", ""},
		{" /", ""},
		{"%2F%2Fevil.example", ""},
		{"/environments\t", ""},
		{"/environments\n", ""},
		{"/environments ", ""},
		{"／", ""},
	}
	for _, ps := range posts {
		for _, c := range cases {
			t.Run(ps.name+"/"+c.value, func(t *testing.T) {
				h, _, _, p := overviewHandler(t, nil)
				form := ps.form(h, p)
				form.Set("return_to", c.value)
				rr := cancelPost(h, p, ps.target, form, nil)
				if rr.Code != http.StatusSeeOther {
					t.Fatalf("status = %d body=%q", rr.Code, rr.Body.String())
				}
				want := c.want
				if want == "" {
					want = ps.wantDefault
				}
				if got := rr.Header().Get("Location"); got != want {
					t.Fatalf("Location = %q, want %q", got, want)
				}
			})
		}
		t.Run(ps.name+"/absent", func(t *testing.T) {
			h, _, _, p := overviewHandler(t, nil)
			rr := cancelPost(h, p, ps.target, ps.form(h, p), nil)
			if got := rr.Header().Get("Location"); rr.Code != http.StatusSeeOther || got != ps.wantDefault {
				t.Fatalf("no return_to: %d %q", rr.Code, got)
			}
		})
		t.Run(ps.name+"/repeated", func(t *testing.T) {
			h, _, _, p := overviewHandler(t, nil)
			form := ps.form(h, p)
			form["return_to"] = []string{"/", "/environments"}
			rr := cancelPost(h, p, ps.target, form, nil)
			if got := rr.Header().Get("Location"); rr.Code != http.StatusSeeOther || got != ps.wantDefault {
				t.Fatalf("repeated return_to: %d %q", rr.Code, got)
			}
		})
	}
}

// ---- parity entries and dump pages ----

func parityBoardHandler(t *testing.T) (*Handler, auth.Principal) {
	t.Helper()
	h, _, admin, p := overviewHandler(t, fourRows())
	admin.servers = []postgres.ServerSummary{okServer()}
	return h, p
}

func parityOverviewBoard(t *testing.T) string {
	h, p := parityBoardHandler(t)
	return pageGet(t, h, p, "/")
}

func parityEnvironmentsPage(t *testing.T) string {
	h, p := parityBoardHandler(t)
	return pageGet(t, h, p, "/environments")
}

// boardDataFragments are the facts and links both pages keep per environment.
func boardDataFragments() []string {
	return []string{
		`env-live`, `proj-live`, `acer`, `live.example.test`, hrefAttr("https://live.example.test"),
		`0123456789ab`, `rel-prev`, ts(overviewNow.Add(-3 * time.Hour)),
		hrefAttr("/environments/config?environment_id=env-live"), hrefAttr("/environments/route?environment_id=env-live"),
		hrefAttr("/deployments?environment_id=env-live"), hrefAttr("/deployments/logs?id=dep-live"), hrefAttr("/deployments/detail?id=dep-live"),
		`>healthy<`, `>failed<`, `env-new`, `env-blocked`, `env-failed`,
		`action="/deployments/manual"`, `name="project_id" value="proj-live"`, `name="request_nonce" value="`, `name="mode" value="head"`, `name="commit_sha" value=""`,
		`action="/deployments/rollback"`, `name="release_id" value="rel-prev"`, `name="confirm" value="rollback"`,
		`action="/environments/resolve"`, `name="environment_id" value="env-blocked"`, `name="attempt_id" value="42"`, `name="outcome"`, `name="reason"`,
		`name="csrf" value="`,
	}
}

func overviewBoardFragments() []string {
	return append(boardDataFragments(), `name="return_to" value="/"`, `action="/logout"`, hrefAttr("/environments"), hrefAttr("/servers"), `srv-1`)
}

func environmentsPageFragments() []string {
	return append(boardDataFragments(), `name="return_to" value="/environments"`, `recreate`, `rel-live`, `>7<`)
}

// emptyDumpPages renders the zero-data states for the review dump.
func emptyDumpPages() map[string]func(t *testing.T) string {
	empty := func(target string) func(t *testing.T) string {
		return func(t *testing.T) string {
			h, _, _, p := overviewHandler(t, nil)
			return pageGet(t, h, p, target)
		}
	}
	return map[string]func(t *testing.T) string{
		"overview-empty":     empty("/"),
		"environments-empty": empty("/environments"),
		"github-empty": func(t *testing.T) string {
			h, _, _, p := githubPageHandler(t)
			h.config.RepositoryLister = nil
			return pageGet(t, h, p, "/github")
		},
		"reconciliation-empty": empty("/reconciliation"),
		// 22-3h: first-class pages with no owned environment or deployment.
		"environment-config-empty": func(t *testing.T) string {
			h, _, _, _, p := fcHandler(t, []postgres.EnvironmentOverview{fourRows()[1]})
			return pageGet(t, h, p, "/environments/config")
		},
		"environment-route-empty": func(t *testing.T) string {
			h, _, _, _, p := fcHandler(t, nil)
			return pageGet(t, h, p, "/environments/route")
		},
		"logs-empty": func(t *testing.T) string {
			h, _, _, p := logsPickerHandler(t, nil)
			return pageGet(t, h, p, "/deployments/logs")
		},
		"environment-config-many": func(t *testing.T) string {
			h, _, _, _, p := fcHandler(t, manyOwned(14))
			return pageGet(t, h, p, "/environments/config")
		},
		// 22-3j: remaining empty states and flow pages.
		"projects-empty": func(t *testing.T) string {
			h, _, p := operationsHandler(t)
			return pageGet(t, h, p, "/projects")
		},
		"deployments-empty": func(t *testing.T) string {
			h, _, p := operationsHandler(t)
			return pageGet(t, h, p, "/deployments")
		},
		"deployments-filtered-empty": func(t *testing.T) string {
			h, _, p := operationsHandler(t)
			return pageGet(t, h, p, "/deployments?project_id=nope")
		},
		"servers-empty": func(t *testing.T) string {
			h, _, p := serversHandler(t)
			return pageGet(t, h, p, "/servers")
		},
		"enrollment-token-issued": func(t *testing.T) string {
			return enrollmentPage(t, "srv_demo", "tok-demo-0123456789abcdef", time.Date(2026, 8, 10, 0, 15, 0, 0, time.UTC))
		},
		"github-continue-steps": func(t *testing.T) string { return parityGitHubContinue(t) },
		"github-complete-steps": func(t *testing.T) string { return parityGitHubComplete(t) },
		"reconciliation-empty-servers": func(t *testing.T) string {
			h, admin, p := serversHandler(t)
			admin.servers = []postgres.ServerSummary{okServer()}
			return pageGet(t, h, p, "/reconciliation")
		},
	}
}

// return_to must not loosen the strict field checks of the three POST handlers.
func TestReturnToKeepsStrictFieldChecks(t *testing.T) {
	forms := map[string]struct {
		target   string
		form     func(h *Handler, p auth.Principal) url.Values
		required string
	}{
		"manual":   {"/deployments/manual", func(h *Handler, p auth.Principal) url.Values { return manualForm(t, h, p) }, "project_id"},
		"rollback": {"/deployments/rollback", func(h *Handler, p auth.Principal) url.Values { return rollbackForm(t, h, p) }, "release_id"},
		"resolve": {"/environments/resolve", func(h *Handler, p auth.Principal) url.Values {
			return url.Values{"csrf": {serversCSRF(t, h, p)}, "environment_id": {"env-a"}, "attempt_id": {"5"}, "outcome": {"activated"}, "reason": {"by hand"}}
		}, "attempt_id"},
	}
	for name, f := range forms {
		t.Run(name+"/extra field", func(t *testing.T) {
			h, _, _, p := overviewHandler(t, nil)
			form := f.form(h, p)
			form.Set("return_to", "/")
			form.Set("surprise", "x")
			if rr := cancelPost(h, p, f.target, form, nil); rr.Code != http.StatusBadRequest {
				t.Fatalf("extra field = %d", rr.Code)
			}
		})
		t.Run(name+"/missing field", func(t *testing.T) {
			h, _, _, p := overviewHandler(t, nil)
			form := f.form(h, p)
			form.Set("return_to", "/")
			form.Del(f.required)
			if rr := cancelPost(h, p, f.target, form, nil); rr.Code != http.StatusBadRequest {
				t.Fatalf("missing field = %d", rr.Code)
			}
		})
	}
}
