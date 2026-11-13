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
	for _, want := range []string{"Older deployments", "href=\"" + strings.ReplaceAll(wantOlder, "&", "&amp;") + "\"", "cancel requested", "/deployments/detail?id=dep-1", "<code>0123456789ab</code>", `aria-current="page">Deployments`, "Traffic activation is disabled on this control plane."} {
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
	for _, want := range []string{"Traffic activation is disabled on this control plane.", "<code>abcdef012345</code>", "recreate", "Status timeline", "running"} {
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
	for _, want := range []string{"o/r", "refs/heads/main", "active", "a (<a href=\"/environments/config?environment_id=a\">config</a>", `href="/environments/route?environment_id=b"`, "<code>0123456789ab</code>", "/deployments/detail?id=dep-1", "/deployments?project_id=proj", "no binding", "none yet", `aria-current="page">Projects`} {
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

// ---- POST /deployments/cancel (step 15d) ----

type cancellerStub struct {
	err    error
	calls  int
	gotJob int64
	gotOwn string
}

func (s *cancellerStub) RequestCancellation(_ context.Context, p auth.Principal, jobID int64) error {
	s.calls++
	s.gotJob, s.gotOwn = jobID, p.OwnerID
	return s.err
}

func cancelHandler(t *testing.T, jobState string) (*Handler, *operationsStub, *cancellerStub, auth.Principal) {
	t.Helper()
	h, stub, p := operationsHandler(t)
	stub.detail = sampleDetail(jobState, false)
	c := &cancellerStub{}
	h.config.DeploymentCanceller = c
	return h, stub, c, p
}

func cancelForm(t *testing.T, h *Handler, p auth.Principal) url.Values {
	t.Helper()
	return url.Values{"csrf": {serversCSRF(t, h, p)}, "deployment_id": {"dep-1"}, "confirm": {"cancel"}}
}

func cancelPost(h *Handler, p auth.Principal, target string, form url.Values, mutate func(*http.Request)) *httptest.ResponseRecorder {
	r := serversRequest(http.MethodPost, target, form, p, h)
	if mutate != nil {
		mutate(r)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	return rr
}

func TestCancelDisabledWithoutCancellerOrReader(t *testing.T) {
	h, _, p := operationsHandler(t) // reader only
	if rr := cancelPost(h, p, "/deployments/cancel", url.Values{"x": {"y"}}, nil); rr.Code != http.StatusNotFound {
		t.Fatalf("no canceller = %d", rr.Code)
	}
	h2, _, _ := serversHandler(t)
	h2.config.DeploymentCanceller = &cancellerStub{}
	if rr := cancelPost(h2, p, "/deployments/cancel", url.Values{"x": {"y"}}, nil); rr.Code != http.StatusNotFound {
		t.Fatalf("no reader = %d", rr.Code)
	}
}

func TestCancelMethodSessionCSRFOriginQuery(t *testing.T) {
	h, _, c, p := cancelHandler(t, "queued")
	rr := opsGet(h, p, http.MethodGet, "/deployments/cancel")
	if rr.Code != http.StatusMethodNotAllowed || rr.Header().Get("Allow") != "POST" {
		t.Fatalf("GET = %d allow=%q", rr.Code, rr.Header().Get("Allow"))
	}
	form := cancelForm(t, h, p)

	// No session cookie: redirect to /login.
	rr = cancelPost(h, p, "/deployments/cancel", form, func(r *http.Request) { r.Header.Del("Cookie") })
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatalf("no session = %d %q", rr.Code, rr.Header().Get("Location"))
	}
	// Bad CSRF: redirect like revoke; missing CSRF field: 400.
	bad := cancelForm(t, h, p)
	bad.Set("csrf", "bogus")
	if rr = cancelPost(h, p, "/deployments/cancel", bad, nil); rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatalf("bad csrf = %d", rr.Code)
	}
	missing := cancelForm(t, h, p)
	missing.Del("csrf")
	if rr = cancelPost(h, p, "/deployments/cancel", missing, nil); rr.Code != http.StatusBadRequest {
		t.Fatalf("missing csrf = %d", rr.Code)
	}
	// Bad origin / no provenance.
	if rr = cancelPost(h, p, "/deployments/cancel", form, func(r *http.Request) { r.Header.Set("Origin", "https://evil.example.test") }); rr.Code != http.StatusBadRequest {
		t.Fatalf("bad origin = %d", rr.Code)
	}
	// Missing/wrong confirm and extra fields (including a client job id).
	noConfirm := cancelForm(t, h, p)
	noConfirm.Del("confirm")
	wrongConfirm := cancelForm(t, h, p)
	wrongConfirm.Set("confirm", "yes")
	extra := cancelForm(t, h, p)
	extra.Set("job_id", "999")
	for name, f := range map[string]url.Values{"no confirm": noConfirm, "wrong confirm": wrongConfirm, "client job_id": extra} {
		if rr = cancelPost(h, p, "/deployments/cancel", f, nil); rr.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d", name, rr.Code)
		}
	}
	// Query string: 404.
	if rr = cancelPost(h, p, "/deployments/cancel?id=dep-1", form, nil); rr.Code != http.StatusNotFound {
		t.Fatalf("query string = %d", rr.Code)
	}
	if c.calls != 0 {
		t.Fatalf("canceller called %d times on rejected requests", c.calls)
	}
}

func TestCancelUnknownDeploymentNeverCallsCanceller(t *testing.T) {
	for _, e := range []error{postgres.ErrOperationsNotFound, postgres.ErrOperationsInvalidInput} {
		h, stub, c, p := cancelHandler(t, "queued")
		stub.err = e
		rr := cancelPost(h, p, "/deployments/cancel", cancelForm(t, h, p), nil)
		if rr.Code != http.StatusNotFound || c.calls != 0 {
			t.Fatalf("reader err %v = %d calls=%d", e, rr.Code, c.calls)
		}
	}
}

func TestCancelUsesServerSideJobIDAndRedirects(t *testing.T) {
	h, stub, c, p := cancelHandler(t, "leased") // sampleDetail job id is 3
	rr := cancelPost(h, p, "/deployments/cancel", cancelForm(t, h, p), nil)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/deployments/detail?id=dep-1" {
		t.Fatalf("success = %d %q", rr.Code, rr.Header().Get("Location"))
	}
	if c.calls != 1 || c.gotJob != 3 || c.gotOwn != "owner" || stub.gotID != "dep-1" {
		t.Fatalf("calls=%d job=%d owner=%q looked up=%q", c.calls, c.gotJob, c.gotOwn, stub.gotID)
	}
}

func TestCancelErrorMapping(t *testing.T) {
	cases := []struct {
		err  error
		code int
		body string
	}{
		{postgres.ErrAgentJobTerminal, http.StatusConflict, "this deployment has already finished and cannot be cancelled\n"},
		{postgres.ErrAgentJobActivating, http.StatusConflict, "this deployment is activating and can no longer be cancelled\n"},
		{postgres.ErrServerAuthorizationDenied, http.StatusBadRequest, "bad request\n"},
		{fmt.Errorf("lock agent job for cancellation: %w", errors.New("secret detail")), http.StatusInternalServerError, "the deployment could not be cancelled; retry\n"},
	}
	for _, tc := range cases {
		h, _, c, p := cancelHandler(t, "queued")
		c.err = tc.err
		rr := cancelPost(h, p, "/deployments/cancel", cancelForm(t, h, p), nil)
		if rr.Code != tc.code || rr.Body.String() != tc.body {
			t.Errorf("%v = %d %q", tc.err, rr.Code, rr.Body.String())
		}
	}
}

func TestDetailCancelButtonVisibility(t *testing.T) {
	const form = `action="/deployments/cancel"`
	req := t0
	cases := []struct {
		name      string
		state     string
		requested bool
		status    string
		canceller bool
		want      bool
	}{
		{"queued", "queued", false, "", true, true},
		{"retry_wait", "retry_wait", false, "", true, true},
		{"leased", "leased", false, "", true, true},
		{"leased activating", "leased", false, "activating", true, false},
		{"cancel already requested", "leased", true, "", true, false},
		{"succeeded", "succeeded", false, "", true, false},
		{"failed", "failed", false, "", true, false},
		{"cancelled", "cancelled", true, "", true, false},
		{"dead", "dead", false, "", true, false},
		{"no canceller", "queued", false, "", false, false},
	}
	for _, tc := range cases {
		h, stub, c, p := cancelHandler(t, tc.state)
		if !tc.canceller {
			h.config.DeploymentCanceller = nil
		}
		_ = c
		if tc.requested {
			stub.detail.Job.CancelRequestedAt = &req
		}
		if tc.status != "" {
			stub.detail.Deployment.Status = tc.status
		}
		rr := opsGet(h, p, http.MethodGet, "/deployments/detail?id=dep-1")
		body := rr.Body.String()
		if rr.Code != http.StatusOK || strings.Contains(body, form) != tc.want {
			t.Errorf("%s: code=%d form=%v want %v", tc.name, rr.Code, strings.Contains(body, form), tc.want)
		}
		if tc.want && !strings.Contains(body, `value="`+serversCSRF(t, h, p)+`"`) {
			t.Errorf("%s: CSRF token missing", tc.name)
		}
		if strings.Contains(body, "cancellation requested") != tc.requested {
			t.Errorf("%s: cancellation-requested note mismatch", tc.name)
		}
	}
}
