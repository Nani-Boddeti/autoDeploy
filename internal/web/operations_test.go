package web

import (
	"context"
	"encoding/base64"
	"errors"
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

// operationsStub returns errors shaped like OperationsReadRepository: the
// sentinels bare (not-found, denied, invalid input) and everything else wrapped.
type operationsStub struct {
	projects   []postgres.ProjectSummary
	list       postgres.DeploymentList
	detail     postgres.DeploymentDetail
	err        error
	gotFilter  postgres.DeploymentFilter
	gotPage    postgres.DeploymentPage
	gotID      string
	gotOwner   string
	listCalled int
}

func (s *operationsStub) ListProjects(_ context.Context, p auth.Principal) ([]postgres.ProjectSummary, error) {
	s.gotOwner = p.OwnerID
	if s.err != nil {
		return nil, s.err
	}
	return s.projects, nil
}

func (s *operationsStub) ListDeployments(_ context.Context, p auth.Principal, f postgres.DeploymentFilter, pg postgres.DeploymentPage) (postgres.DeploymentList, error) {
	s.listCalled++
	s.gotOwner, s.gotFilter, s.gotPage = p.OwnerID, f, pg
	if s.err != nil {
		return postgres.DeploymentList{}, s.err
	}
	return s.list, nil
}

func (s *operationsStub) GetDeploymentDetail(_ context.Context, p auth.Principal, id string) (postgres.DeploymentDetail, error) {
	s.gotOwner, s.gotID = p.OwnerID, id
	if s.err != nil {
		return postgres.DeploymentDetail{}, s.err
	}
	return s.detail, nil
}

func operationsHandler(t *testing.T) (*Handler, *operationsStub, auth.Principal) {
	t.Helper()
	h, _, p := serversHandler(t)
	stub := &operationsStub{}
	h.config.OperationsReader = stub
	return h, stub, p
}

func opsGet(h *Handler, p auth.Principal, method, target string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, serversRequest(method, target, nil, p, h))
	return rr
}

var t0 = time.Date(2026, 11, 11, 10, 0, 0, 123456000, time.UTC)

func sampleDetail(jobState string, active bool) postgres.DeploymentDetail {
	attempt := int64(7)
	closed := t0.Add(time.Minute)
	return postgres.DeploymentDetail{
		Deployment:       postgres.DeploymentSummary{DeploymentID: "dep-1", ProjectID: "proj", EnvironmentID: "env", ServerID: "srv", CommitSHA: "0123456789abcdef0123", Status: "building", JobID: 3, JobState: jobState, CreatedAt: t0, UpdatedAt: t0},
		StatusEvents:     []postgres.DeploymentStatusEvent{{Revision: 2, FromStatus: "queued", ToStatus: "building", OccurredAt: t0}},
		Job:              postgres.JobView{ID: 3, State: jobState, AttemptCount: 1, CreatedAt: t0, UpdatedAt: t0},
		JobEvents:        []postgres.JobEventView{{Sequence: 1, EventType: "leased", AttemptID: &attempt, OccurredAt: t0}},
		Attempts:         []postgres.AttemptView{{ID: 7, AttemptNumber: 1, State: "started", FailureClass: "", Fence: 4, ReleaseStrategy: "recreate", CreatedAt: t0, ClosedAt: &closed}},
		Release:          &postgres.ReleaseView{ID: "rel-1", State: "ready", Strategy: "recreate", CommitSHA: "0123456789abcdef0123", ImageDigest: "abcdef0123456789abcdef", CreatedAt: t0, UpdatedAt: t0},
		RuntimeInstances: []postgres.RuntimeInstanceView{{ID: 9, AttemptID: 7, Fence: 4, ObservedState: "running", CreatedAt: t0}},
		ReleaseIsActive:  active,
	}
}

var operationsRoutes = []string{"/projects", "/deployments", "/deployments/detail?id=dep-1"}

func TestOperationsDisabledWithoutReader(t *testing.T) {
	h, _, p := serversHandler(t)
	for _, target := range operationsRoutes {
		if rr := opsGet(h, p, http.MethodGet, target); rr.Code != http.StatusNotFound {
			t.Fatalf("%s without reader = %d", target, rr.Code)
		}
	}
}

func TestOperationsRequireSessionAndMethods(t *testing.T) {
	h, _, p := operationsHandler(t)
	for _, target := range operationsRoutes {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "https://admin.example.test"+target, nil))
		if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
			t.Errorf("%s unauthenticated = %d %q", target, rr.Code, rr.Header().Get("Location"))
		}
		for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
			rr := opsGet(h, p, m, target)
			if rr.Code != http.StatusMethodNotAllowed || rr.Header().Get("Allow") != "GET, HEAD" {
				t.Errorf("%s %s = %d allow=%q", m, target, rr.Code, rr.Header().Get("Allow"))
			}
		}
	}
}

func TestOperationsHeadIsAllowed(t *testing.T) {
	h, stub, p := operationsHandler(t)
	stub.detail = sampleDetail("leased", false)
	for _, target := range operationsRoutes {
		rr := opsGet(h, p, http.MethodHead, target)
		if rr.Code != http.StatusOK {
			t.Errorf("HEAD %s = %d", target, rr.Code)
		}
	}
}

func TestOperationsQueryAllowlist(t *testing.T) {
	h, _, p := operationsHandler(t)
	for _, target := range []string{
		"/projects?x=1", "/projects?", "/deployments?x=1", "/deployments?project_id=a&x=1",
		"/deployments/detail?id=dep-1&x=1", "/deployments/detail?x=1", "/deployments/detail", "/deployments/detail?id=",
		"/deployments/detail?id=a&id=b", "/deployments/detail?id=a%20b",
	} {
		if rr := opsGet(h, p, http.MethodGet, target); rr.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", target, rr.Code)
		}
	}
	for _, target := range []string{"/deployments?project_id=a&project_id=b", "/deployments?after=!!!", "/deployments?after=", "/deployments?project_id=" + strings.Repeat("a", 129)} {
		if rr := opsGet(h, p, http.MethodGet, target); rr.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", target, rr.Code)
		}
	}
}

func TestDeploymentCursorRoundTripAndMalformed(t *testing.T) {
	in := postgres.DeploymentCursor{CreatedAt: t0, DeploymentID: "dep-1"}
	out, ok := decodeDeploymentCursor(encodeDeploymentCursor(in))
	if !ok || !out.CreatedAt.Equal(t0) || out.DeploymentID != "dep-1" {
		t.Fatalf("round trip = %+v %v", out, ok)
	}
	nonUTC := time.Date(2026, 1, 1, 0, 0, 0, 0, time.FixedZone("x", 3600))
	if c, ok := decodeDeploymentCursor(encodeDeploymentCursor(postgres.DeploymentCursor{CreatedAt: nonUTC, DeploymentID: "a"})); !ok || !c.CreatedAt.Equal(nonUTC) {
		t.Fatal("non-UTC input must be normalised to UTC and round-trip")
	}
	enc := func(raw string) string { return encodeOpaque(raw) }
	bad := []string{
		"", "!!", "AAAA", strings.Repeat("A", 600),
		enc("2026-11-11T10:00:00Z"),            // no id
		enc("2026-11-11T10:00:00Z|"),           // empty id
		enc("not-a-time|dep"),                  // bad time
		enc("2026-11-11T10:00:00+01:00|dep"),   // not UTC canonical
		enc("2026-11-11T10:00:00.100Z|a b"),    // space in id
		enc("2026-11-11T10:00:00Z|" + "a\x00"), // control
		enc("2026-11-11T10:00:00.50Z|dep"),     // non-canonical fraction? (Format would give .5Z)
		encodeDeploymentCursor(in) + "=",       // padding rejected
	}
	for _, s := range bad {
		if _, ok := decodeDeploymentCursor(s); ok {
			t.Errorf("cursor %q accepted", s)
		}
	}
}

func encodeOpaque(raw string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func TestDeploymentsFiltersCursorAndOlderLink(t *testing.T) {
	h, stub, p := operationsHandler(t)
	next := postgres.DeploymentCursor{CreatedAt: t0, DeploymentID: "dep-9"}
	stub.list = postgres.DeploymentList{
		Items: []postgres.DeploymentSummary{{DeploymentID: "dep-1", ProjectID: "proj", EnvironmentID: "env", ServerID: "srv", CommitSHA: "0123456789abcdef0123", Status: "queued", JobID: 1, JobState: "queued", CancelRequested: true, CreatedAt: t0, UpdatedAt: t0}},
		Next:  &next,
	}
	after := encodeDeploymentCursor(postgres.DeploymentCursor{CreatedAt: t0.Add(-time.Hour), DeploymentID: "dep-5"})
	rr := opsGet(h, p, http.MethodGet, "/deployments?"+url.Values{"project_id": {"proj&x"}, "environment_id": {"env"}, "after": {after}}.Encode())
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d", rr.Code)
	}
	if stub.gotOwner != "owner" || stub.gotFilter != (postgres.DeploymentFilter{ProjectID: "proj&x", EnvironmentID: "env"}) {
		t.Fatalf("filter/owner = %+v %q", stub.gotFilter, stub.gotOwner)
	}
	if stub.gotPage.After == nil || stub.gotPage.After.DeploymentID != "dep-5" {
		t.Fatalf("page = %+v", stub.gotPage)
	}
	body := rr.Body.String()
	wantOlder := "/deployments?" + url.Values{"project_id": {"proj&x"}, "environment_id": {"env"}, "after": {encodeDeploymentCursor(next)}}.Encode()
	for _, want := range []string{"Older deployments", "href=\"" + strings.ReplaceAll(wantOlder, "&", "&amp;") + "\"", "cancel requested", "/deployments/detail?id=dep-1", "<code>0123456789ab</code>", `aria-current="page">Deployments`, "execution and traffic activation are disabled"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	// first page without cursor
	opsGet(h, p, http.MethodGet, "/deployments")
	if stub.gotPage.After != nil || stub.gotFilter != (postgres.DeploymentFilter{}) {
		t.Fatalf("first page = %+v %+v", stub.gotPage, stub.gotFilter)
	}
	// no Next -> no older link; empty state
	stub.list = postgres.DeploymentList{}
	if b := opsGet(h, p, http.MethodGet, "/deployments").Body.String(); strings.Contains(b, "Older deployments") || !strings.Contains(b, "No deployments to show") {
		t.Fatalf("empty page = %s", b)
	}
}

func TestMalformedCursorNeverReachesRepository(t *testing.T) {
	h, stub, p := operationsHandler(t)
	if rr := opsGet(h, p, http.MethodGet, "/deployments?after=garbage!"); rr.Code != http.StatusBadRequest || stub.listCalled != 0 {
		t.Fatalf("code=%d calls=%d", rr.Code, stub.listCalled)
	}
}

func TestOperationsErrorMapping(t *testing.T) {
	h, stub, p := operationsHandler(t)
	cases := []struct {
		err      error
		target   string
		wantCode int
		wantLoc  string
	}{
		{postgres.ErrOperationsNotFound, "/deployments/detail?id=dep-1", http.StatusNotFound, ""},
		{postgres.ErrOperationsInvalidInput, "/deployments/detail?id=dep-1", http.StatusNotFound, ""},
		{postgres.ErrServerAuthorizationDenied, "/deployments/detail?id=dep-1", http.StatusSeeOther, "/login"},
		{postgres.ErrServerAuthorizationDenied, "/deployments", http.StatusSeeOther, "/login"},
		{postgres.ErrServerAuthorizationDenied, "/projects", http.StatusSeeOther, "/login"},
		{postgres.ErrOperationsInvalidInput, "/deployments?project_id=x", http.StatusBadRequest, ""},
		{fmt.Errorf("list deployments: %w", errors.New("connection reset")), "/deployments", http.StatusInternalServerError, ""},
		{fmt.Errorf("list projects: %w", errors.New("boom")), "/projects", http.StatusInternalServerError, ""},
	}
	for _, c := range cases {
		stub.err = c.err
		rr := opsGet(h, p, http.MethodGet, c.target)
		if rr.Code != c.wantCode || rr.Header().Get("Location") != c.wantLoc {
			t.Errorf("%v on %s = %d %q", c.err, c.target, rr.Code, rr.Header().Get("Location"))
		}
		if strings.Contains(rr.Body.String(), "connection reset") || strings.Contains(rr.Body.String(), "boom") {
			t.Errorf("internal error text leaked on %s", c.target)
		}
	}
	// not-found body is the generic one regardless of cause.
	stub.err = postgres.ErrOperationsNotFound
	a := opsGet(h, p, http.MethodGet, "/deployments/detail?id=dep-1").Body.String()
	stub.err = postgres.ErrOperationsInvalidInput
	b := opsGet(h, p, http.MethodGet, "/deployments/detail?id=dep-2").Body.String()
	if a != b {
		t.Fatalf("not-found bodies differ: %q vs %q", a, b)
	}
}

func TestDetailRefreshOnlyWhileJobNonTerminal(t *testing.T) {
	h, stub, p := operationsHandler(t)
	const meta = `<meta http-equiv="refresh" content="5">`
	for _, state := range []string{"queued", "leased", "retry_wait"} {
		stub.detail = sampleDetail(state, false)
		if b := opsGet(h, p, http.MethodGet, "/deployments/detail?id=dep-1").Body.String(); !strings.Contains(b, meta) {
			t.Errorf("%s: no refresh", state)
		}
	}
	for _, state := range []string{"succeeded", "failed", "cancelled", "dead"} {
		stub.detail = sampleDetail(state, false)
		if b := opsGet(h, p, http.MethodGet, "/deployments/detail?id=dep-1").Body.String(); strings.Contains(b, "refresh") {
			t.Errorf("%s: refresh present", state)
		}
	}
	// refresh meta sits in <head>, before <body>.
	stub.detail = sampleDetail("leased", false)
	b := opsGet(h, p, http.MethodGet, "/deployments/detail?id=dep-1").Body.String()
	if strings.Index(b, meta) > strings.Index(b, "<body>") {
		t.Fatal("meta refresh not in head")
	}
	if stub.gotID != "dep-1" || stub.gotOwner != "owner" {
		t.Fatalf("id/owner = %q %q", stub.gotID, stub.gotOwner)
	}
}

func TestDetailLiveBadgeOnlyWhenActive(t *testing.T) {
	h, stub, p := operationsHandler(t)
	stub.detail = sampleDetail("succeeded", false)
	if b := opsGet(h, p, http.MethodGet, "/deployments/detail?id=dep-1").Body.String(); strings.Contains(b, "live on route") {
		t.Fatal("badge without active release")
	}
	stub.detail = sampleDetail("succeeded", true)
	b := opsGet(h, p, http.MethodGet, "/deployments/detail?id=dep-1").Body.String()
	if !strings.Contains(b, "live on route") {
		t.Fatal("badge missing for active release")
	}
	for _, want := range []string{"execution and traffic activation are disabled", "<code>abcdef012345</code>", "recreate", "Status timeline", "running"} {
		if !strings.Contains(b, want) {
			t.Errorf("detail missing %q", want)
		}
	}
	if strings.Contains(b, "abcdef0123456789abcdef") {
		t.Error("full image digest rendered")
	}
}

func TestOperationsPagesEscapeValues(t *testing.T) {
	h, stub, p := operationsHandler(t)
	evil := `<script>alert("x")</script>`
	stub.projects = []postgres.ProjectSummary{{ProjectID: evil, HasBinding: true, BindingActive: true, RepositoryFullName: evil, RepositoryDefaultRef: evil, EnvironmentIDs: []string{evil},
		LatestDeployment: &postgres.DeploymentSummary{DeploymentID: evil, Status: evil, CommitSHA: evil}}}
	stub.list = postgres.DeploymentList{Items: []postgres.DeploymentSummary{{DeploymentID: evil, ProjectID: evil, EnvironmentID: evil, ServerID: evil, CommitSHA: evil, Status: evil, JobState: evil}}}
	d := sampleDetail("leased", false)
	d.Deployment.DeploymentID, d.Deployment.ProjectID, d.Deployment.Status = evil, evil, evil
	d.Attempts[0].FailureClass = evil
	d.Release.ID = evil
	stub.detail = d
	for _, target := range operationsRoutes {
		rr := opsGet(h, p, http.MethodGet, target)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s = %d", target, rr.Code)
		}
		body := rr.Body.String()
		if strings.Contains(body, "<script") || strings.Contains(body, `alert("x")`) {
			t.Errorf("%s: unescaped value", target)
		}
		if !strings.Contains(body, "&lt;script&gt;") {
			t.Errorf("%s: escaped value not found", target)
		}
		// unknown state text never reaches a class attribute.
		if strings.Contains(body, `class="pill <`) || strings.Contains(body, `class="pill &`) {
			t.Errorf("%s: raw state in class", target)
		}
	}
}

func TestProjectsPageContentAndEmptyState(t *testing.T) {
	h, stub, p := operationsHandler(t)
	if b := opsGet(h, p, http.MethodGet, "/projects").Body.String(); !strings.Contains(b, "No projects are bound yet") {
		t.Fatalf("empty state missing: %s", b)
	}
	stub.projects = []postgres.ProjectSummary{
		{ProjectID: "proj", HasBinding: true, BindingActive: true, RepositoryFullName: "o/r", RepositoryDefaultRef: "refs/heads/main", EnvironmentIDs: []string{"a", "b"},
			LatestDeployment: &postgres.DeploymentSummary{DeploymentID: "dep-1", Status: "healthy", CommitSHA: "0123456789abcdef", CreatedAt: t0}},
		{ProjectID: "orphan"},
	}
	b := opsGet(h, p, http.MethodGet, "/projects").Body.String()
	for _, want := range []string{"o/r", "refs/heads/main", "active", "a, b", "<code>0123456789ab</code>", "/deployments/detail?id=dep-1", "/deployments?project_id=proj", "no binding", "none yet", `aria-current="page">Projects`} {
		if !strings.Contains(b, want) {
			t.Errorf("projects page missing %q", want)
		}
	}
}

func TestOperationsPagesStayCSPClean(t *testing.T) {
	h, stub, p := operationsHandler(t)
	stub.projects = []postgres.ProjectSummary{{ProjectID: "proj", LatestDeployment: &postgres.DeploymentSummary{DeploymentID: "d", Status: "queued"}}}
	next := postgres.DeploymentCursor{CreatedAt: t0, DeploymentID: "d"}
	stub.list = postgres.DeploymentList{Items: []postgres.DeploymentSummary{{DeploymentID: "d", Status: "queued", JobState: "queued"}}, Next: &next}
	stub.detail = sampleDetail("leased", true)
	for _, target := range operationsRoutes {
		rr := opsGet(h, p, http.MethodGet, target)
		if got := rr.Header().Get("Content-Security-Policy"); got != wantCSP {
			t.Errorf("%s CSP = %q", target, got)
		}
		if rr.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s Cache-Control = %q", target, rr.Header().Get("Cache-Control"))
		}
		body, lower := rr.Body.String(), strings.ToLower(rr.Body.String())
		if !strings.Contains(body, `<link rel="stylesheet" href="/static/app.css">`) || !strings.Contains(body, `<nav aria-label="Primary">`) {
			t.Errorf("%s missing layout", target)
		}
		for _, banned := range []string{"style=", "<style", "<script", "javascript:", "onclick=", "<form"} {
			if strings.Contains(lower, banned) {
				t.Errorf("%s contains %q", target, banned)
			}
		}
	}
}
