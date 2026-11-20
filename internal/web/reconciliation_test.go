package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"autodeploy/internal/auth"
	"autodeploy/internal/store/postgres"
)

func blockedEnvs() []postgres.ReconcilingEnvironment {
	return []postgres.ReconcilingEnvironment{
		{EnvironmentID: "env-b1", AttemptID: 55, JobID: 66, ServerID: "srv-1", Since: pt4, Activation: "activated"},
		{EnvironmentID: "env-b2", AttemptID: 56, JobID: 67, ServerID: "srv-1", Since: pt5},
	}
}

func TestReconciliationPageListsBlockedWithResolveForms(t *testing.T) {
	h, _, admin, p := overviewHandler(t, fourRows())
	admin.environments = blockedEnvs()
	page := pageGet(t, h, p, "/reconciliation")
	contains(t, page,
		`env-b1`, `env-b2`, hrefAttr("/environments/config?environment_id=env-b1"), hrefAttr("/environments/route?environment_id=env-b2"),
		`>55<`, `>66<`, `>activated<`, ts(pt4), `<td>none</td>`,
		`name="environment_id" value="env-b1"`, `name="attempt_id" value="56"`,
		`<select name="outcome" required autocomplete="off"><option value="">Choose outcome</option>`,
		`value="activated"`, `value="not_activated"`, `value="no_activation"`,
		`<input name="reason" required autocomplete="off" maxlength="`+strconv.Itoa(maxResolveReasonBytes)+`">`,
		`name="csrf" value="`+serversCSRF(t, h, p)+`"`,
		`href="/reconciliation" aria-current="page"`,
	)
	if got := strings.Count(page, `action="/environments/resolve"`); got != 2 {
		t.Fatalf("resolve forms = %d, want 2", got)
	}
	if got := strings.Count(page, `name="return_to" value="/reconciliation"`); got != 2 {
		t.Fatalf("return_to fields = %d, want 2", got)
	}
	if strings.Contains(page, "selected") {
		t.Fatal("an outcome option is preselected")
	}
	// Blocked state replaces the activation list.
	lacks(t, page, `env-live`)
}

func TestReconciliationEmptyShowsActivationStateOfEveryEnvironment(t *testing.T) {
	h, _, _, p := overviewHandler(t, fourRows())
	page := pageGet(t, h, p, "/reconciliation")
	contains(t, page, `env-live`, `proj-live`, `live.example.test`, `env-new`, `env-blocked`, `env-failed`,
		hrefAttr("/environments/config?environment_id=env-live"), hrefAttr("/environments/route?environment_id=env-live"),
		`class="pill ok"`, `Not deployed yet`)
	lacks(t, page, `action="/environments/resolve"`)
	// The blocked flag renders as text on the blocked environment's row.
	row := page[strings.Index(page, `data-env="env-blocked"`):]
	row = row[:strings.Index(row, "</tr>")]
	contains(t, row, `blocked`)
	live := page[strings.Index(page, `data-env="env-live"`):]
	live = live[:strings.Index(live, "</tr>")]
	lacks(t, live, `>blocked<`)
}

func TestReconciliationEmptyWithoutOverviewReaderShowsServers(t *testing.T) {
	h, admin, p := serversHandler(t)
	admin.servers = []postgres.ServerSummary{okServer()}
	h.config.OperationsReader = &operationsStub{} // no overview read
	page := pageGet(t, h, p, "/reconciliation")
	contains(t, page, `acer`, `class="pill ok"`, hrefAttr("/servers"), `<th>Last heartbeat</th>`)
	lacks(t, page, `action="/environments/resolve"`)
	h.config.OperationsReader = nil
	contains(t, pageGet(t, h, p, "/reconciliation"), `acer`, hrefAttr("/servers"))
}

func TestReconciliationEmptyWithNoServersStillHasStructure(t *testing.T) {
	h, _, p := serversHandler(t)
	page := pageGet(t, h, p, "/reconciliation")
	contains(t, page, `class="checklist"`, hrefAttr("/servers"))
}

func TestReconciliationOverviewFailureDegrades(t *testing.T) {
	h, ops, _, p := overviewHandler(t, fourRows())
	ops.err = errors.New("db down")
	page := pageGet(t, h, p, "/reconciliation")
	contains(t, page, `role="note"`)
}

func TestReconciliationRouteAvailability(t *testing.T) {
	h, _, _, p := overviewHandler(t, nil)
	if rr := opsGet(h, p, http.MethodGet, "/reconciliation?x=1"); rr.Code != http.StatusNotFound {
		t.Fatalf("query = %d", rr.Code)
	}
	if rr := opsGet(h, p, http.MethodPost, "/reconciliation"); rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d", rr.Code)
	}
	h.config.ServerAdmin = nil
	if rr := opsGet(h, p, http.MethodGet, "/reconciliation"); rr.Code != http.StatusNotFound {
		t.Fatalf("no ServerAdmin = %d", rr.Code)
	}
	h2, _, _, _ := overviewHandler(t, nil)
	rr := httptest.NewRecorder()
	h2.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "https://admin.example.test/reconciliation", nil))
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatalf("no session = %d %q", rr.Code, rr.Header().Get("Location"))
	}
}

func TestReconciliationListErrorIsGeneric(t *testing.T) {
	h, _, admin, p := overviewHandler(t, nil)
	admin.envListErr = errors.New("db down")
	if rr := opsGet(h, p, http.MethodGet, "/reconciliation"); rr.Code != http.StatusBadRequest {
		t.Fatalf("env list error = %d", rr.Code)
	}
}

func TestResolveDefaultRedirectIsReconciliation(t *testing.T) {
	h, _, p := serversHandler(t)
	rr := postResolve(h, p, resolveForm(serversCSRF(t, h, p), nil))
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/reconciliation" {
		t.Fatalf("resolve = %d %q", rr.Code, rr.Header().Get("Location"))
	}
	form := resolveForm(serversCSRF(t, h, p), nil)
	form.Set("return_to", "/reconciliation")
	if rr = postResolve(h, p, form); rr.Header().Get("Location") != "/reconciliation" {
		t.Fatalf("return_to honoured = %q", rr.Header().Get("Location"))
	}
	form.Set("return_to", "//evil.example")
	if rr = postResolve(h, p, form); rr.Header().Get("Location") != "/reconciliation" {
		t.Fatalf("hostile return_to = %q", rr.Header().Get("Location"))
	}
}

func TestOverviewAndEnvironmentsLinkBlockedToReconciliation(t *testing.T) {
	h, _, _, p := overviewHandler(t, fourRows())
	page := pageGet(t, h, p, "/")
	attention := page[strings.Index(page, `class="attention"`):]
	attention = attention[:strings.Index(attention, "</section>")]
	contains(t, attention, "env-blocked", hrefAttr("/reconciliation"))
	// The inline resolve form keeps its own page.
	contains(t, rowFragment(t, page, "env-blocked"), `name="return_to" value="/"`)
	envs := pageGet(t, h, p, "/environments")
	contains(t, rowFragment(t, envs, "env-blocked"), `name="return_to" value="/environments"`)
}

func TestServersPageDropsReconcilingTableAndShowsBanner(t *testing.T) {
	h, admin, p := serversHandler(t)
	admin.servers = []postgres.ServerSummary{okServer()}
	page := pageGet(t, h, p, "/servers")
	lacks(t, page, `action="/environments/resolve"`, `<form method="get" action="/environments/config">`, `need reconciliation`, `needs reconciliation`)
	admin.environments = blockedEnvs()[:1]
	page = pageGet(t, h, p, "/servers")
	contains(t, page, `1 environment needs reconciliation`, hrefAttr("/reconciliation"))
	lacks(t, page, `action="/environments/resolve"`, `env-b1`)
	admin.environments = blockedEnvs()
	contains(t, pageGet(t, h, p, "/servers"), `2 environments need reconciliation`)
}

func TestNavReconciliationItemAndBadge(t *testing.T) {
	h, admin, p := serversHandler(t)
	// No blocked environments: link present, no badge.
	page := pageGet(t, h, p, "/servers")
	contains(t, page, hrefAttr("/reconciliation"))
	lacks(t, page, `class="badge"`)
	// Blocked environments: text badge with an accessible label, on every page.
	admin.environments = blockedEnvs()
	for _, target := range []string{"/servers", "/reconciliation", "/"} {
		page = pageGet(t, h, p, target)
		contains(t, page, `<span class="badge" role="img" aria-label="2 blocked">2</span>`)
	}
	// Without ServerAdmin the item and the badge are gone.
	h.config.ServerAdmin = nil
	page = pageGet(t, h, p, "/")
	lacks(t, page, hrefAttr("/reconciliation"), `class="badge"`)
}

func TestNavBadgeFetchFailureRendersPageWithoutBadge(t *testing.T) {
	h, admin, p := serversHandler(t)
	admin.environments = blockedEnvs()
	admin.envListErr = errors.New("db down")
	h.config.OperationsReader = &overviewStub{operationsStub: &operationsStub{}}
	page := pageGet(t, h, p, "/")
	contains(t, page, hrefAttr("/reconciliation"))
	lacks(t, page, `class="badge"`)
}

func TestNavBadgeIsNotRefetchedWhereTheHandlerHasTheList(t *testing.T) {
	h, _, p := serversHandler(t)
	counting := &countingAdmin{serverAdminStub: &serverAdminStub{environments: blockedEnvs()}}
	h.config.ServerAdmin = counting
	for _, target := range []string{"/servers", "/reconciliation"} {
		counting.listEnvCalls = 0
		pageGet(t, h, p, target)
		if counting.listEnvCalls != 1 {
			t.Errorf("%s listed reconciling environments %d times, want 1", target, counting.listEnvCalls)
		}
	}
}

type countingAdmin struct {
	*serverAdminStub
	listEnvCalls int
}

func (c *countingAdmin) ListReconcilingEnvironments(ctx context.Context, p auth.Principal) ([]postgres.ReconcilingEnvironment, error) {
	c.listEnvCalls++
	return c.serverAdminStub.ListReconcilingEnvironments(ctx, p)
}
