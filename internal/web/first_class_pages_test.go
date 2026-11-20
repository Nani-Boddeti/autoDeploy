package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"autodeploy/internal/auth"
	"autodeploy/internal/store/postgres"
)

// 22-3h: Runtime config, Routes and Logs are first-class pages (no typed IDs)
// and Deployments gains filter selects.

const stripOpen = `<nav aria-label="Environments" class="strip">`

// fcHandler is overviewHandler plus the config and route admin stubs.
func fcHandler(t *testing.T, rows []postgres.EnvironmentOverview) (*Handler, *overviewStub, *runtimeConfigAdminStub, *routeHeadAdminStub, auth.Principal) {
	t.Helper()
	h, ops, _, p := overviewHandler(t, rows)
	cfg, route := &runtimeConfigAdminStub{}, &routeHeadAdminStub{}
	h.config.RuntimeConfigAdmin, h.config.RouteHeadAdmin = cfg, route
	return h, ops, cfg, route, p
}

func manyOwned(n int) []postgres.EnvironmentOverview {
	rows := make([]postgres.EnvironmentOverview, 0, n)
	for i := n; i >= 1; i-- { // reverse order: the page must sort by id
		rows = append(rows, postgres.EnvironmentOverview{EnvironmentID: fmt.Sprintf("env-%02d", i), ProjectID: "proj", Owned: true})
	}
	return rows
}

func strip(t *testing.T, page string) string {
	t.Helper()
	i := strings.Index(page, stripOpen)
	if i < 0 {
		t.Fatalf("page has no environment strip")
	}
	rest := page[i:]
	j := strings.Index(rest, "</nav>")
	if j < 0 {
		t.Fatal("strip is not closed")
	}
	return rest[:j]
}

func TestEnvironmentPagesWithoutIDSelectFirstOwnedAndShowStrip(t *testing.T) {
	for _, pg := range []struct{ path, formAction string }{
		{"/environments/config", `action="/environments/config"`},
		{"/environments/route", `action="/environments/route"`},
	} {
		t.Run(pg.path, func(t *testing.T) {
			h, _, _, _, p := fcHandler(t, fourRows())
			rr := opsGet(h, p, http.MethodGet, pg.path)
			if rr.Code != http.StatusOK {
				t.Fatalf("no-id GET = %d", rr.Code)
			}
			body := rr.Body.String()
			contains(t, body, pg.formAction, `name="environment_id" value="env-blocked"`)
			s := strip(t, body)
			// Sorted by id; the first owned environment is the current one.
			want := []string{
				`<a href="` + pg.path + `?environment_id=env-blocked" aria-current="true">`,
				`<a href="` + pg.path + `?environment_id=env-failed">`,
				`<a href="` + pg.path + `?environment_id=env-live">`,
			}
			last := -1
			for _, w := range want {
				i := strings.Index(s, w)
				if i < 0 || i < last {
					t.Fatalf("strip lacks or misorders %q:\n%s", w, s)
				}
				last = i
			}
			if n := strings.Count(s, "aria-current"); n != 1 {
				t.Fatalf("strip has %d aria-current, want 1", n)
			}
			// env-new is bound but not owned: the config and route stores deny it.
			lacks(t, s, "env-new")
			if strings.Contains(s, "<select") {
				t.Fatal("strip renders the select fallback for 3 environments")
			}
		})
	}
}

func TestEnvironmentPagesWithIDStillShowStripWithCurrent(t *testing.T) {
	for _, path := range []string{"/environments/config", "/environments/route"} {
		h, _, _, _, p := fcHandler(t, fourRows())
		body := opsGet(h, p, http.MethodGet, path+"?environment_id=env-live").Body.String()
		s := strip(t, body)
		contains(t, s, `<a href="`+path+`?environment_id=env-live" aria-current="true">`, `environment_id=env-blocked`)
		lacks(t, s, "env-new")
		contains(t, body, `name="environment_id" value="env-live"`)
	}
}

func TestEnvironmentPagesUnknownIDKeepsTodaysResponse(t *testing.T) {
	// The store denies an unowned or unknown id (an error); the page answers 400
	// with the same plain body, with or without an overview reader, and never
	// echoes the strip or the id.
	for _, path := range []string{"/environments/config", "/environments/route"} {
		h, _, cfg, route, p := fcHandler(t, fourRows())
		cfg.getErr, route.getErr = postgres.ErrServerAuthorizationDenied, postgres.ErrServerAuthorizationDenied
		with := opsGet(h, p, http.MethodGet, path+"?environment_id=env-new")
		h2, _, p2 := serversHandler(t)
		h2.config.RuntimeConfigAdmin = &runtimeConfigAdminStub{getErr: postgres.ErrServerAuthorizationDenied}
		h2.config.RouteHeadAdmin = &routeHeadAdminStub{getErr: postgres.ErrServerAuthorizationDenied}
		without := opsGet(h2, p2, http.MethodGet, path+"?environment_id=env-new")
		if with.Code != http.StatusBadRequest || without.Code != with.Code || with.Body.String() != without.Body.String() {
			t.Fatalf("%s unowned: with=%d %q without=%d %q", path, with.Code, with.Body.String(), without.Code, without.Body.String())
		}
		if strings.Contains(with.Body.String(), "strip") || strings.Contains(with.Body.String(), "env-new") {
			t.Fatalf("%s denial leaks page content: %q", path, with.Body.String())
		}
	}
}

func TestEnvironmentPagesStillRejectMalformedQueries(t *testing.T) {
	for _, path := range []string{"/environments/config", "/environments/route"} {
		h, _, _, _, p := fcHandler(t, fourRows())
		for _, target := range []string{
			path + "?environment_id=", path + "?environment_id=a&environment_id=b", path + "?environment_id=env-live&x=1", path + "?x=1",
		} {
			if rr := opsGet(h, p, http.MethodGet, target); rr.Code != http.StatusBadRequest {
				t.Errorf("%s = %d, want 400", target, rr.Code)
			}
		}
	}
}

func TestEnvironmentPagesZeroOwnedShowChecklistAndDeployForm(t *testing.T) {
	for _, path := range []string{"/environments/config", "/environments/route"} {
		// Only a bound, never-deployed environment: not owned.
		boundOnly := []postgres.EnvironmentOverview{fourRows()[1]}
		h, _, _, _, p := fcHandler(t, boundOnly)
		body := opsGet(h, p, http.MethodGet, path).Body.String()
		contains(t, body, `id="setup"`, `after its first deployment`, `env-new`,
			`action="/deployments/manual"`, `name="project_id" value="proj-new"`, `name="return_to" value="/environments"`)
		lacks(t, body, `action="/environments/config"`, `action="/environments/route"`, stripOpen)

		// Nothing at all: the checklist still renders (never text-only).
		h, _, _, _, p = fcHandler(t, nil)
		body = opsGet(h, p, http.MethodGet, path).Body.String()
		contains(t, body, `id="setup"`, `after its first deployment`)
		lacks(t, body, `action="/deployments/manual"`)
	}
}

func TestEnvironmentPagesUnownedRowsNeverOfferDeployWhenManualDeployOff(t *testing.T) {
	h, _, _, _, p := fcHandler(t, []postgres.EnvironmentOverview{fourRows()[1]})
	h.config.ManualDeploy = nil
	body := opsGet(h, p, http.MethodGet, "/environments/route").Body.String()
	contains(t, body, `id="setup"`)
	lacks(t, body, `action="/deployments/manual"`)
}

func TestEnvironmentPagesNewEnvironmentRendersPrefilledCreateForm(t *testing.T) {
	h, _, _, _, p := fcHandler(t, fourRows())
	body := opsGet(h, p, http.MethodGet, "/environments/config").Body.String()
	contains(t, body, `name="expected_revision" value=""`, `value="http" selected`, `value="stop_then_start" selected`,
		`value="unless-stopped" selected`, `value="/"`, `Create configuration`)
	// The explanation sits inside the form region, never as a lone paragraph.
	if strings.Contains(body, "<p>No configuration exists yet") {
		t.Fatal("standalone text-only paragraph remains on the config page")
	}
	body = opsGet(h, p, http.MethodGet, "/environments/route").Body.String()
	contains(t, body, `name="expected_revision" value=""`, `name="hostname"`, `name="acknowledge_authority"`, `Assign hostname`)
	if strings.Contains(body, "<p>No route exists yet") {
		t.Fatal("standalone text-only paragraph remains on the route page")
	}
}

func TestEnvironmentPagesManyEnvironmentsAddSelectFallback(t *testing.T) {
	for _, path := range []string{"/environments/config", "/environments/route"} {
		h, _, _, _, p := fcHandler(t, manyOwned(13))
		body := opsGet(h, p, http.MethodGet, path).Body.String()
		contains(t, body, `<form method="get" class="strip-form">`, `<select name="environment_id"`, `<option value="env-01" selected>env-01</option>`, `<option value="env-13">env-13</option>`)

		h, _, _, _, p = fcHandler(t, manyOwned(12))
		if body := opsGet(h, p, http.MethodGet, path).Body.String(); strings.Contains(body, `class="strip-form"`) {
			t.Fatalf("%s: 12 environments must not add the select fallback", path)
		}
	}
}

func TestEnvironmentPagePostRejectionRerenderShowsStrip(t *testing.T) {
	h, _, _, _, p := fcHandler(t, fourRows())
	form := validConfigForm("env-live", serversCSRF(t, h, p), "")
	form.Set("container_port", "70000")
	rr := httptest200(t, h, serversRequest(http.MethodPost, "/environments/config", form, p, h))
	contains(t, rr, stripOpen, `<a href="/environments/config?environment_id=env-live" aria-current="true">`, `value="70000"`)

	rform := validRouteForm("env-live", serversCSRF(t, h, p), "")
	rform.Set("hostname", "not a hostname!")
	rr = httptest200(t, h, serversRequest(http.MethodPost, "/environments/route", rform, p, h))
	contains(t, rr, stripOpen, `<a href="/environments/route?environment_id=env-live" aria-current="true">`)
}

func httptest200(t *testing.T, h *Handler, r *http.Request) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s %s = %d: %s", r.Method, r.URL.Path, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func TestEnvironmentPagesWithoutOverviewReaderKeepOldBehaviour(t *testing.T) {
	for _, path := range []string{"/environments/config", "/environments/route"} {
		h, _, p := serversHandler(t)
		h.config.RuntimeConfigAdmin = &runtimeConfigAdminStub{}
		h.config.RouteHeadAdmin = &routeHeadAdminStub{}
		if rr := opsGet(h, p, http.MethodGet, path); rr.Code != http.StatusBadRequest {
			t.Errorf("%s without id and without overview reader = %d, want 400", path, rr.Code)
		}
		body := opsGet(h, p, http.MethodGet, path+"?environment_id=env-x").Body.String()
		lacks(t, body, `class="strip"`)
		contains(t, body, `name="environment_id" value="env-x"`)
		// An OperationsReader that lacks the overview read behaves the same.
		h.config.OperationsReader = &operationsStub{}
		if rr := opsGet(h, p, http.MethodGet, path); rr.Code != http.StatusBadRequest {
			t.Errorf("%s with a plain operations reader = %d, want 400", path, rr.Code)
		}
	}
}

func TestEnvironmentPagesOverviewFailureDegradesWithID(t *testing.T) {
	h, ops, _, _, p := fcHandler(t, fourRows())
	ops.err = fmt.Errorf("boom")
	body := opsGet(h, p, http.MethodGet, "/environments/config?environment_id=env-live").Body.String()
	contains(t, body, `name="environment_id" value="env-live"`)
	lacks(t, body, `class="strip"`)
	if rr := opsGet(h, p, http.MethodGet, "/environments/config"); rr.Code == http.StatusOK {
		t.Fatalf("no-id GET with a failing overview read = %d, want an error", rr.Code)
	}
}

// ---- logs ----

func logsPickerHandler(t *testing.T, items []postgres.DeploymentSummary) (*Handler, *logStub, *operationsStub, auth.Principal) {
	t.Helper()
	h, ls, ops, p := logsHandler(t)
	ops.list = postgres.DeploymentList{Items: items}
	return h, ls, ops, p
}

func nDeployments(n int) []postgres.DeploymentSummary {
	items := make([]postgres.DeploymentSummary, 0, n)
	for i := 1; i <= n; i++ {
		items = append(items, postgres.DeploymentSummary{
			DeploymentID: fmt.Sprintf("dep-%02d", i), ProjectID: "proj", EnvironmentID: "env-a",
			CommitSHA: "0123456789abcdef0123", Status: "healthy", CreatedAt: t0.Add(-time.Duration(i) * time.Minute),
		})
	}
	return items
}

func TestLogsWithoutIDRendersNewestDeploymentAndPicker(t *testing.T) {
	h, ls, ops, p := logsPickerHandler(t, nDeployments(25))
	ls.page = postgres.DeploymentLogPage{Items: []postgres.DeploymentLogLine{line(1, 1, "build", "newest log body")}}
	rr := opsGet(h, p, http.MethodGet, "/deployments/logs")
	if rr.Code != http.StatusOK {
		t.Fatalf("no-id logs = %d", rr.Code)
	}
	body := rr.Body.String()
	if ls.gotID != "dep-01" || ls.gotAfter != 0 {
		t.Fatalf("logs read for %q after %d, want newest dep-01 from the start", ls.gotID, ls.gotAfter)
	}
	if ops.gotPage.Limit == 0 || ops.gotPage.Limit > 20 {
		t.Fatalf("deployment list limit = %d, want 1..20", ops.gotPage.Limit)
	}
	contains(t, body, `newest log body`, `<form method="get" action="/deployments/logs" class="picker">`, `<select name="id"`,
		`<option value="dep-01" selected>`, `<option value="dep-20">`)
	lacks(t, body, `value="dep-21"`)
	if n := strings.Count(body, "<option"); n != 20 {
		t.Fatalf("picker has %d options, want 20", n)
	}
	contains(t, body, `env-a`, `0123456789ab`, `healthy`)
}

func TestLogsPickerSelectsCurrentAndAddsMissingOne(t *testing.T) {
	h, _, _, p := logsPickerHandler(t, nDeployments(5))
	body := opsGet(h, p, http.MethodGet, "/deployments/logs?id=dep-03").Body.String()
	contains(t, body, `<option value="dep-03" selected>`)
	if n := strings.Count(body, " selected>"); n != 1 {
		t.Fatalf("%d selected options, want 1", n)
	}
	// An id outside the newest 20 still shows as the selected option.
	body = opsGet(h, p, http.MethodGet, "/deployments/logs?id=dep-old").Body.String()
	contains(t, body, `<option value="dep-old" selected>dep-old</option>`)
}

func TestLogsAfterWithoutIDIsBadRequest(t *testing.T) {
	h, ls, _, p := logsPickerHandler(t, nDeployments(2))
	for _, target := range []string{"/deployments/logs?after=5", "/deployments/logs?after=", "/deployments/logs?after=x"} {
		if rr := opsGet(h, p, http.MethodGet, target); rr.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", target, rr.Code)
		}
	}
	if ls.callCount != 0 {
		t.Fatalf("logs were read %d times for a rejected request", ls.callCount)
	}
}

func TestLogsUnknownParamsStillRejected(t *testing.T) {
	h, _, _, p := logsPickerHandler(t, nDeployments(2))
	for _, target := range []string{"/deployments/logs?x=1", "/deployments/logs?id=dep-01&x=1", "/deployments/logs?environment_id=e"} {
		if rr := opsGet(h, p, http.MethodGet, target); rr.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", target, rr.Code)
		}
	}
}

func TestLogsZeroDeploymentsShowsChecklist(t *testing.T) {
	h, ls, _, p := logsPickerHandler(t, nil)
	rr := opsGet(h, p, http.MethodGet, "/deployments/logs")
	if rr.Code != http.StatusOK {
		t.Fatalf("empty logs = %d", rr.Code)
	}
	contains(t, rr.Body.String(), `id="setup"`)
	lacks(t, rr.Body.String(), `<select name="id"`)
	if ls.callCount != 0 {
		t.Fatal("logs read with no deployment")
	}
}

// TestLogsNoIDDoesNotAutoRefresh: the no-id page re-resolves to the newest
// deployment on every load, so it must not auto-refresh (a newer deployment
// would replace the one being watched); it links to the explicit-id page instead.
func TestLogsNoIDDoesNotAutoRefresh(t *testing.T) {
	h, ls, ops, p := logsPickerHandler(t, nDeployments(1))
	ops.detail = sampleDetail("leased", false)
	ls.page = postgres.DeploymentLogPage{Items: []postgres.DeploymentLogLine{line(1, 1, "build", "x")}}
	body := opsGet(h, p, http.MethodGet, "/deployments/logs").Body.String()
	lacks(t, body, `http-equiv="refresh"`)
	contains(t, body, `href="/deployments/logs?id=dep-01"`)
	// The explicit-id page keeps the tail refresh.
	if body := opsGet(h, p, http.MethodGet, "/deployments/logs?id=dep-01").Body.String(); !strings.Contains(body, `http-equiv="refresh" content="3"`) {
		t.Fatal("explicit-id page lost its refresh on a non-terminal job")
	}
	ops.detail = sampleDetail("succeeded", false)
	body = opsGet(h, p, http.MethodGet, "/deployments/logs").Body.String()
	lacks(t, body, `http-equiv="refresh"`, `Follow it live`)
}

func TestLogsNavKeyAndItem(t *testing.T) {
	h, _, _, p := logsPickerHandler(t, nDeployments(1))
	body := opsGet(h, p, http.MethodGet, "/deployments/logs?id=dep-01").Body.String()
	contains(t, body, `<a href="/deployments/logs" aria-current="page">Logs</a>`)
	lacks(t, body, `<a href="/deployments" aria-current="page">`)
}

// ---- deployments filters ----

func filterHandler(t *testing.T) (*Handler, *operationsStub, auth.Principal) {
	t.Helper()
	h, stub, p := operationsHandler(t)
	stub.projects = []postgres.ProjectSummary{
		{ProjectID: "proj-b", EnvironmentIDs: []string{"env-2", "env-1"}},
		{ProjectID: "proj-a", EnvironmentIDs: []string{"env-1", "env-3"}},
	}
	return h, stub, p
}

func TestDeploymentsFilterFormOptionsAndPreselection(t *testing.T) {
	h, _, p := filterHandler(t)
	body := opsGet(h, p, http.MethodGet, "/deployments").Body.String()
	contains(t, body, `<form method="get" action="/deployments" class="filter-form">`,
		`<select name="project_id"`, `<select name="environment_id"`,
		`<option value="" selected>All projects</option>`, `<option value="" selected>All environments</option>`,
		`<option value="proj-a">proj-a</option>`, `<option value="proj-b">proj-b</option>`,
		`<option value="env-1">env-1</option>`, `<option value="env-2">env-2</option>`, `<option value="env-3">env-3</option>`, `Apply`)
	if n := strings.Count(body, `<option value="env-1"`); n != 1 {
		t.Fatalf("env-1 listed %d times, want deduplicated", n)
	}
	body = opsGet(h, p, http.MethodGet, "/deployments?project_id=proj-b&environment_id=env-2").Body.String()
	contains(t, body, `<option value="proj-b" selected>proj-b</option>`, `<option value="env-2" selected>env-2</option>`,
		`<option value="">All projects</option>`, `Clear filters`)
	// A filter value unknown to the project list is kept, not silently dropped.
	body = opsGet(h, p, http.MethodGet, "/deployments?environment_id=env-gone").Body.String()
	contains(t, body, `<option value="env-gone" selected>env-gone</option>`)
}

func TestDeploymentsFilterFormDegradesWhenProjectsFail(t *testing.T) {
	h, stub, p := filterHandler(t)
	stub.projects = nil
	body := opsGet(h, p, http.MethodGet, "/deployments?project_id=proj-x").Body.String()
	contains(t, body, `<option value="proj-x" selected>proj-x</option>`, `<select name="environment_id"`)
}

func TestDeploymentsFilterFormRoundTrip(t *testing.T) {
	h, stub, p := filterHandler(t)
	// What the form submits (empty "All" values included) must be accepted.
	rr := opsGet(h, p, http.MethodGet, "/deployments?"+url.Values{"project_id": {"proj-a"}, "environment_id": {""}}.Encode())
	if rr.Code != http.StatusOK || stub.gotFilter.ProjectID != "proj-a" || stub.gotFilter.EnvironmentID != "" {
		t.Fatalf("round trip = %d filter %+v", rr.Code, stub.gotFilter)
	}
	rr = opsGet(h, p, http.MethodGet, "/deployments?project_id=&environment_id=")
	if rr.Code != http.StatusOK || strings.Contains(rr.Body.String(), "Clear filters") {
		t.Fatalf("all-empty submit = %d", rr.Code)
	}
}

func TestDeploymentsAndProjectsReasonOnlyForFailed(t *testing.T) {
	h, stub, p := filterHandler(t)
	stub.list = postgres.DeploymentList{Items: []postgres.DeploymentSummary{
		{DeploymentID: "dep-bad", ProjectID: "proj-a", EnvironmentID: "env-1", Status: "failed", FailureClass: "permanent_configuration", FailureReason: "container_unhealthy", CreatedAt: t0, UpdatedAt: t0},
		{DeploymentID: "dep-ok", ProjectID: "proj-a", EnvironmentID: "env-1", Status: "healthy", FailureClass: "permanent_configuration", FailureReason: "container_unhealthy", CreatedAt: t0, UpdatedAt: t0},
	}}
	body := opsGet(h, p, http.MethodGet, "/deployments").Body.String()
	contains(t, body, `<th>Reason</th>`)
	if n := strings.Count(body, `class="reason"`); n != 1 {
		t.Fatalf("%d reason chips on the deployments page, want 1 (failed only)", n)
	}
	contains(t, body, "Configuration problem", "Image HEALTHCHECK reported unhealthy")

	stub.projects = []postgres.ProjectSummary{
		{ProjectID: "proj-a", LatestDeployment: &postgres.DeploymentSummary{DeploymentID: "dep-bad", Status: "failed", FailureClass: "transient", FailureReason: "deadline_exceeded", CreatedAt: t0}},
		{ProjectID: "proj-b", LatestDeployment: &postgres.DeploymentSummary{DeploymentID: "dep-ok", Status: "healthy", FailureClass: "transient", CreatedAt: t0}},
	}
	body = opsGet(h, p, http.MethodGet, "/projects").Body.String()
	if n := strings.Count(body, `class="reason"`); n != 1 {
		t.Fatalf("%d reason chips on the projects page, want 1", n)
	}
	contains(t, body, "Temporary failure", "Health check deadline exceeded")
}

// ---- nav ----

func TestNavFirstClassItemsFollowTheirDependencies(t *testing.T) {
	full, p := navTestHandler(t)
	full.config.RuntimeConfigAdmin, full.config.RouteHeadAdmin = &runtimeConfigAdminStub{}, &routeHeadAdminStub{}
	full.config.DeploymentLogReader = &logStub{}
	ordered := []string{"/", "/environments", "/deployments", "/deployments/logs", "/environments/config", "/environments/route", "/projects", "/github"}
	body := navPage(t, full, p, "/projects")
	last := -1
	for _, href := range ordered {
		i := strings.Index(body, `<a href="`+href+`"`)
		if i < 0 || i < last {
			t.Fatalf("nav lacks or misorders %q", href)
		}
		last = i
	}
	contains(t, body, `>Runtime config<`, `>Routes<`, `>Logs<`)

	for name, mutate := range map[string]func(h *Handler){
		"no config admin": func(h *Handler) { h.config.RuntimeConfigAdmin = nil },
		"no route admin":  func(h *Handler) { h.config.RouteHeadAdmin = nil },
		"no log reader":   func(h *Handler) { h.config.DeploymentLogReader = nil },
		"no overview":     func(h *Handler) { h.config.OperationsReader = &operationsStub{} },
		"no operations":   func(h *Handler) { h.config.OperationsReader = nil },
		"all deps nil": func(h *Handler) {
			h.config.RuntimeConfigAdmin, h.config.RouteHeadAdmin, h.config.DeploymentLogReader = nil, nil, nil
		},
	} {
		h, p := navTestHandler(t)
		h.config.RuntimeConfigAdmin, h.config.RouteHeadAdmin, h.config.DeploymentLogReader = &runtimeConfigAdminStub{}, &routeHeadAdminStub{}, &logStub{}
		mutate(h)
		b := navPage(t, h, p, "/")
		switch name {
		case "no config admin":
			lacks(t, b, `href="/environments/config"`)
			contains(t, b, `href="/environments/route"`, `href="/deployments/logs"`)
		case "no route admin":
			lacks(t, b, `href="/environments/route"`)
			contains(t, b, `href="/environments/config"`)
		case "no log reader":
			lacks(t, b, `href="/deployments/logs"`)
		case "no overview", "no operations":
			lacks(t, b, `href="/environments/config"`, `href="/environments/route"`)
			if name == "no operations" {
				lacks(t, b, `href="/deployments/logs"`)
			}
		default:
			lacks(t, b, `href="/environments/config"`, `href="/environments/route"`, `href="/deployments/logs"`)
		}
	}
}

func TestNavCurrentItemOnFirstClassPages(t *testing.T) {
	h, _, _, _, p := fcHandler(t, fourRows())
	for _, pg := range []struct{ target, current string }{
		{"/environments/config", "/environments/config"},
		{"/environments/config?environment_id=env-live", "/environments/config"},
		{"/environments/route", "/environments/route"},
		{"/environments/route?environment_id=env-live", "/environments/route"},
		{"/deployments/logs", "/deployments/logs"},
	} {
		body := navPage(t, h, p, pg.target)
		if !strings.Contains(body, `<a href="`+pg.current+`" aria-current="page">`) {
			t.Errorf("%s: current nav item is not %q", pg.target, pg.current)
		}
		if n := strings.Count(body, `aria-current="page"`); n != 1 {
			t.Errorf("%s has %d aria-current=page, want 1", pg.target, n)
		}
	}
}
