package web

import (
	"bytes"
	"context"
	"html"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"autodeploy/internal/auth"
	"autodeploy/internal/buildsecret"
	"autodeploy/internal/healthgate"
	"autodeploy/internal/server"
	"autodeploy/internal/store/postgres"
)

// TestInformationParity is the 22-3b guard for owner rule R4 (information
// parity): every fact, form field and link the current UI renders must
// survive the redesign. Each entry renders one current page/state and pins
// DATA values (ids, digests, timestamps, pill values), form attributes
// (action=, name=, hidden values) and navigational hrefs. It never pins prose
// or headings, so the redesign may reword freely but may not drop information.
//
// Later 22-3 slices that move content between pages flip the entry that owns
// the moved fragments in the same commit (the comments name the slice).
// Fragments are exact substrings of the rendered HTML.
func TestInformationParity(t *testing.T) {
	entries := parityEntries()
	for _, e := range entries {
		t.Run(e.name, func(t *testing.T) {
			if len(e.mustContain) == 0 {
				t.Fatalf("parity entry %q pins no fragments", e.name)
			}
			page := e.render(t)
			for _, frag := range e.mustContain {
				if !strings.Contains(page, frag) {
					t.Errorf("parity entry %q lost fragment %q", e.name, frag)
				}
			}
		})
	}
}

// parityEntry is one rendered page state plus the fragments it must keep.
type parityEntry struct {
	name        string
	render      func(t *testing.T) string
	mustContain []string
}

// parityEntries lists every current page/state; ui_dump_test.go reuses it.
func parityEntries() []parityEntry {
	return []parityEntry{
		{"landing", parityLanding, landingFragments()},
		// 22-3g: the GitHub forms moved from the landing entry to /github; the
		// reconciling rows and resolve forms moved from servers to /reconciliation.
		{"github", parityGitHub, githubFragments()},
		{"reconciliation", parityReconciliation, reconciliationFragments()},
		{"projects", parityProjects, projectsFragments()},
		{"deployments", parityDeployments, deploymentsFragments()},
		{"deployment detail leased", parityDetailLeased, detailLeasedFragments()},
		{"deployment detail cancel requested", parityDetailCancelRequested, detailCancelRequestedFragments()},
		{"deployment detail failed", parityDetailFailed, detailFailedFragments()},
		// State flags are facts (R4) even though they render as pill text.
		// 22-3i may reword them but must keep a stable marker and flip these pins.
		{"deployment detail rolled back", parityDetailRolledBack, []string{`data-fact="rolled-back"`, `>rolled_back<`, `dep-rb1`}},
		{"deployment detail failed with reason", parityDetailFailedReason, []string{`class="failure"`, `Temporary failure`, `Image HEALTHCHECK reported unhealthy`, `Last probe:`, `status=200; tcp ok`, `Activation:`, `route switch aborted`, `Health check deadline exceeded`, `dep-fr1`}},
		{"deployment logs", parityLogs, logsFragments()},
		{"servers", parityServers, serversFragments()},
		{"environment config exists", parityConfigExists, configExistsFragments()},
		{"environment config new", parityConfigNew, configNewFragments()},
		{"environment route exists", parityRouteExists, routeExistsFragments()},
		{"environment route new", parityRouteNew, routeNewFragments()},
		{"enrollment token", parityEnrollmentToken, enrollmentFragments()},
		{"github complete", parityGitHubComplete, []string{`action="/github/setup/complete"`, `name="csrf" value="csrf-complete"`}},
		{"github continue", parityGitHubContinue, []string{`href="/github/setup/complete"`}},
		// 22-3f: the overview board and the environments page (new pages; the landing entry above is unchanged).
		{"overview board", parityOverviewBoard, overviewBoardFragments()},
		{"environments page", parityEnvironmentsPage, environmentsPageFragments()},
		// 22-3h: no-id entry pages (strip, picker) and the deployments filter form.
		{"environment config no id", parityConfigNoID, environmentNoIDFragments("/environments/config")},
		{"environment route no id", parityRouteNoID, environmentNoIDFragments("/environments/route")},
		{"deployment logs no id", parityLogsNoID, logsNoIDFragments()},
		{"deployments filter form", parityDeploymentsFilter, deploymentsFilterFragments()},
	}
}

// ---- helpers ----

// hrefAttr renders a URL as it appears inside an href="..." attribute (the
// templates HTML-escape the query separators).
func hrefAttr(u string) string { return `href="` + html.EscapeString(u) + `"` }

func pageGet(t *testing.T, h *Handler, p auth.Principal, target string) string {
	t.Helper()
	rr := opsGet(h, p, http.MethodGet, target)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET %s = %d", target, rr.Code)
	}
	return rr.Body.String()
}

// parityNav builds a fully populated nav (all dependencies set) for pages that
// are rendered straight from their template.
func parityNav(t *testing.T, active string) navView {
	t.Helper()
	h, p := navTestHandler(t)
	return h.navFor(context.Background(), active, p)
}

func execTemplate(t *testing.T, run func(*bytes.Buffer) error) string {
	t.Helper()
	var b bytes.Buffer
	if err := run(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

var (
	pt1 = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	pt2 = time.Date(2026, 3, 5, 6, 7, 8, 0, time.UTC)
	pt3 = time.Date(2026, 3, 6, 7, 8, 9, 0, time.UTC)
	pt4 = time.Date(2026, 3, 7, 8, 9, 10, 0, time.UTC)
	pt5 = time.Date(2026, 3, 8, 9, 10, 11, 0, time.UTC)
	pt6 = time.Date(2026, 3, 9, 10, 11, 12, 0, time.UTC)
)

// ts renders a time as the pages do (UTC RFC 3339).
func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

const (
	paritySHA   = "0123456789abcdef0123456789abcdef01234567"
	paritySHA12 = "0123456789ab"
)

// ---- landing ----

func parityLanding(t *testing.T) string {
	return execTemplate(t, func(b *bytes.Buffer) error {
		return landingTemplate.Execute(b, landingView{Nav: parityNav(t, "home"), CSRF: "csrf-landing"})
	})
}

func landingFragments() []string {
	return []string{
		// Overview keeps these; the GitHub forms moved to /github in 22-3g.
		`action="/logout"`,
		hrefAttr("/servers"),
	}
}

// ---- github ----

func parityGitHub(t *testing.T) string {
	return execTemplate(t, func(b *bytes.Buffer) error {
		return githubTemplate.Execute(b, githubView{
			Nav: parityNav(t, "github"), CSRF: "csrf-github", ShowBind: true,
			Repositories: []postgres.BindableRepository{{RepositoryID: 201, FullName: "owner/demo", BoundProject: "demo"}, {RepositoryID: 202, FullName: "owner/other"}},
			Servers:      []githubServer{{ID: "server_1", Name: "acer"}},
		})
	})
}

func githubFragments() []string {
	return []string{
		// Moved verbatim from the landing entry (22-3g).
		`action="/github/setup"`,
		`action="/github/installations/reconcile"`,
		`name="installation_id"`,
		`action="/github/bindings"`,
		`name="repository_id"`, `value="201"`, `owner/demo`, `bound to demo`, `value="202"`, `owner/other`,
		`name="project_id"`,
		`name="ref"`, `value="refs/heads/main"`,
		`name="environment_id"`,
		`name="server_id"`, `value="server_1"`, `acer (server_1)`,
		`name="csrf" value="csrf-github"`,
	}
}

// ---- projects ----

func parityProjects(t *testing.T) string {
	h, _, _, p := manualHandler(t)
	h.config.OperationsReader.(*operationsStub).projects = []postgres.ProjectSummary{
		{
			ProjectID: "proj-par", HasBinding: true, BindingActive: true, RepositoryFullName: "owner/par-repo", RepositoryDefaultRef: "refs/heads/release",
			EnvironmentIDs: []string{"env-one", "env-two"},
			LatestDeployment: &postgres.DeploymentSummary{
				DeploymentID: "dep-latest", Status: "healthy", CommitSHA: paritySHA, CreatedAt: pt1,
			},
		},
		{ProjectID: "proj-bare"},
	}
	return pageGet(t, h, p, "/projects")
}

func projectsFragments() []string {
	return []string{
		hrefAttr("/deployments?project_id=proj-par"), `>proj-par<`, `>proj-bare<`, hrefAttr("/deployments?project_id=proj-bare"),
		`owner/par-repo`, `refs/heads/release`, `>active<`, `no binding`,
		`env-one`, hrefAttr("/environments/config?environment_id=env-one"), hrefAttr("/environments/route?environment_id=env-one"),
		`env-two`, hrefAttr("/environments/config?environment_id=env-two"), hrefAttr("/environments/route?environment_id=env-two"),
		`>healthy<`, paritySHA12, ts(pt1), hrefAttr("/deployments/detail?id=dep-latest"),
		// Manual deploy form.
		`action="/deployments/manual"`,
		`name="csrf" value="`,
		`name="project_id" value="proj-par"`,
		`name="request_nonce" value="`,
		`name="mode" value="head"`, `name="mode" value="sha"`,
		`name="commit_sha"`,
	}
}

// ---- deployments ----

func parityDeployments(t *testing.T) string {
	h, stub, p := operationsHandler(t)
	next := postgres.DeploymentCursor{CreatedAt: pt2, DeploymentID: "dep-cursor"}
	stub.list = postgres.DeploymentList{
		Items: []postgres.DeploymentSummary{
			{DeploymentID: "dep-row1", ProjectID: "proj-par", EnvironmentID: "env-par", ServerID: "srv-row1", CommitSHA: paritySHA, Status: "building", JobState: "leased", CancelRequested: true, CreatedAt: pt1, UpdatedAt: pt2},
			{DeploymentID: "dep-row2", ProjectID: "proj-par", EnvironmentID: "env-par", ServerID: "srv-row2", CommitSHA: "fedcba9876543210fedcba9876543210fedcba98", Status: "failed", JobState: "dead", CreatedAt: pt3, UpdatedAt: pt4},
		},
		Next: &next,
	}
	return pageGet(t, h, p, "/deployments?project_id=proj-par&environment_id=env-par")
}

func deploymentsFragments() []string {
	older := deploymentsHref(postgres.DeploymentFilter{ProjectID: "proj-par", EnvironmentID: "env-par"}, encodeDeploymentCursor(postgres.DeploymentCursor{CreatedAt: pt2, DeploymentID: "dep-cursor"}))
	return []string{
		// Rows.
		hrefAttr("/deployments/detail?id=dep-row1"), `>dep-row1<`, hrefAttr("/deployments/detail?id=dep-row2"), `>dep-row2<`,
		`>proj-par<`, `>env-par<`, `>srv-row1<`, `>srv-row2<`,
		paritySHA12, `fedcba987654`,
		`>building<`, `>leased<`, `cancel requested`, `>failed<`, `>dead<`,
		ts(pt1), ts(pt2), ts(pt3), ts(pt4),
		// Filter state and pagination.
		hrefAttr("/deployments"),
		hrefAttr(older),
	}
}

// ---- deployment detail ----

// parityDetailLeased is a running deployment: leased job, active release,
// refresh, cancel form, logs link. Numbers are distinct so each pins one cell.
func parityDetailLeased(t *testing.T) string {
	h, stub, _, p := cancelHandler(t, "leased")
	h.config.DeploymentLogReader = &logStub{}
	a77 := int64(77)
	stub.detail = postgres.DeploymentDetail{
		Deployment:       postgres.DeploymentSummary{DeploymentID: "dep-par1", ProjectID: "proj-par", EnvironmentID: "env-par", ServerID: "srv-par", CommitSHA: "0123456789abcdef0123", Status: "building", JobID: 31, JobState: "leased", CreatedAt: pt1, UpdatedAt: pt2},
		StatusEvents:     []postgres.DeploymentStatusEvent{{Revision: 17, FromStatus: "queued", ToStatus: "building", OccurredAt: pt3}},
		Job:              postgres.JobView{ID: 31, State: "leased", AttemptCount: 4, CreatedAt: pt4, UpdatedAt: pt5},
		JobEvents:        []postgres.JobEventView{{Sequence: 23, EventType: "leased", AttemptID: &a77, OccurredAt: pt6}},
		Attempts:         []postgres.AttemptView{{ID: 77, AttemptNumber: 6, State: "started", Fence: 41, ReleaseStrategy: "recreate", CreatedAt: pt1}},
		Release:          &postgres.ReleaseView{ID: "rel-par1", State: "ready", Strategy: "recreate", CommitSHA: "fedcba9876543210fedc", ImageDigest: "abcdef0123456789abcdef", CreatedAt: pt2, UpdatedAt: pt3},
		RuntimeInstances: []postgres.RuntimeInstanceView{{ID: 91, AttemptID: 77, Fence: 42, ObservedState: "running", CreatedAt: pt4}},
		ReleaseIsActive:  true,
	}
	return pageGet(t, h, p, "/deployments/detail?id=dep-par1")
}

func detailLeasedFragments() []string {
	return []string{
		`http-equiv="refresh"`,
		// Live-route flag (ReleaseIsActive): stable marker, wording may change.
		`data-fact="live-route"`,
		// Summary.
		`dep-par1`, `>proj-par<`, `>env-par<`, `>srv-par<`, `0123456789ab`, `>building<`, ts(pt1), ts(pt2),
		hrefAttr("/environments/config?environment_id=env-par"), hrefAttr("/environments/route?environment_id=env-par"),
		hrefAttr("/deployments/logs?id=dep-par1"), hrefAttr("/deployments?project_id=proj-par"),
		// Status timeline.
		`>17<`, `>queued<`, ts(pt3),
		// Job and events.
		`>31<`, `>leased<`, `>4<`, ts(pt4), ts(pt5), `>23<`, `>77<`, ts(pt6),
		// Attempts.
		`>6<`, `>started<`, `>41<`, `>recreate<`, `>open<`,
		// Release.
		`>rel-par1<`, `>ready<`, `fedcba9876543210fedc`, `abcdef012345`,
		// Runtime instances.
		`>91<`, `>42<`, `>running<`,
		// Cancel form.
		`action="/deployments/cancel"`, `name="deployment_id" value="dep-par1"`, `name="csrf" value="`, `name="confirm" value="cancel"`,
	}
}

// parityDetailRolledBack is a terminal deployment whose release no longer
// serves after a rollback: the rolled-back flag must be shown.
func parityDetailRolledBack(t *testing.T) string {
	h, stub, _, p := cancelHandler(t, "succeeded")
	stub.detail = postgres.DeploymentDetail{
		Deployment: postgres.DeploymentSummary{DeploymentID: "dep-rb1", ProjectID: "proj-par", EnvironmentID: "env-par", ServerID: "srv-par", CommitSHA: paritySHA, Status: "rolled_back", JobID: 33, JobState: "succeeded", CreatedAt: pt1, UpdatedAt: pt2},
		Job:        postgres.JobView{ID: 33, State: "succeeded", AttemptCount: 1, CreatedAt: pt1, UpdatedAt: pt2},
	}
	return pageGet(t, h, p, "/deployments/detail?id=dep-rb1")
}

// parityDetailFailedReason is a failed deployment with a health-gate reason,
// the latest failed attempt's probe and an activation summary.
func parityDetailFailedReason(t *testing.T) string {
	h, stub, _, p := cancelHandler(t, "dead")
	stub.detail = postgres.DeploymentDetail{
		Deployment:        postgres.DeploymentSummary{DeploymentID: "dep-fr1", ProjectID: "proj-par", EnvironmentID: "env-par", ServerID: "srv-par", CommitSHA: paritySHA, Status: "failed", JobID: 34, JobState: "dead", CreatedAt: pt1, UpdatedAt: pt2, FailureClass: "transient", FailureReason: "container_unhealthy"},
		Job:               postgres.JobView{ID: 34, State: "dead", AttemptCount: 2, CreatedAt: pt1, UpdatedAt: pt2},
		Attempts:          []postgres.AttemptView{{ID: 90, AttemptNumber: 1, State: "failed", FailureClass: "transient", Fence: 5, FailureReason: "deadline_exceeded", FailureProbe: "status=200; tcp ok", CreatedAt: pt1}},
		ActivationSummary: "route switch aborted",
	}
	return pageGet(t, h, p, "/deployments/detail?id=dep-fr1")
}

// parityDetailCancelRequested is a leased job whose cancellation is already
// requested: the cancel form is hidden and the request time plus the list
// pill are shown instead.
func parityDetailCancelRequested(t *testing.T) string {
	h, stub, _, p := cancelHandler(t, "leased")
	req := pt5
	stub.detail = sampleDetail("leased", false)
	stub.detail.Deployment.CancelRequested = true
	stub.detail.Job.CancelRequestedAt = &req
	return pageGet(t, h, p, "/deployments/detail?id=dep-1")
}

func detailCancelRequestedFragments() []string {
	return []string{ts(pt5), `cancel requested`}
}

// parityDetailFailed is a terminal failed deployment: attempts with failure
// classes, retained release with digest, retention cleanup, predecessor drain,
// runtime instances, and both the Retry and Roll back forms.
func parityDetailFailed(t *testing.T) string {
	h, _, _, p := manualHandler(t)
	h.config.Rollback = &rollbackStoreStub{eligible: true}
	h.config.DeploymentLogReader = &logStub{}
	stub := h.config.OperationsReader.(*operationsStub)
	stub.projects = []postgres.ProjectSummary{{ProjectID: "proj-fail", HasBinding: true, BindingActive: true, RepositoryDefaultRef: "refs/heads/main", BindingEnvironmentID: "env-fail"}}
	a81, a82 := int64(81), int64(82)
	closed1, closed2, done := pt3, pt4, pt6
	stub.detail = postgres.DeploymentDetail{
		Deployment: postgres.DeploymentSummary{DeploymentID: "dep-fail", ProjectID: "proj-fail", EnvironmentID: "env-fail", ServerID: "srv-fail", CommitSHA: paritySHA, Status: "failed", JobID: 52, JobState: "dead", CreatedAt: pt1, UpdatedAt: pt2},
		StatusEvents: []postgres.DeploymentStatusEvent{
			{Revision: 19, FromStatus: "building", ToStatus: "failed", OccurredAt: pt5},
			{Revision: 18, FromStatus: "queued", ToStatus: "building", OccurredAt: pt6},
		},
		Job: postgres.JobView{ID: 52, State: "dead", AttemptCount: 9, CreatedAt: pt1, UpdatedAt: pt2},
		JobEvents: []postgres.JobEventView{
			{Sequence: 33, EventType: "failed", AttemptID: &a82, OccurredAt: pt3},
			{Sequence: 32, EventType: "retry_scheduled", AttemptID: &a81, OccurredAt: pt4},
		},
		Attempts: []postgres.AttemptView{
			{ID: 82, AttemptNumber: 7, State: "failed", FailureClass: "health_gate_failed", Fence: 61, ReleaseStrategy: "blue_green", CreatedAt: pt5, ClosedAt: &closed2},
			{ID: 81, AttemptNumber: 8, State: "failed", FailureClass: "image_build_failed", Fence: 62, ReleaseStrategy: "stop_then_start", CreatedAt: pt1, ClosedAt: &closed1},
		},
		Release:          &postgres.ReleaseView{ID: "rel-fail", State: "retained", Strategy: "blue_green", CommitSHA: "aaaabbbbccccddddeeee", ImageDigest: "0123abcd4567ef890123abcd4567ef89", CreatedAt: pt2, UpdatedAt: pt3},
		RuntimeInstances: []postgres.RuntimeInstanceView{{ID: 93, AttemptID: 82, Fence: 63, ObservedState: "stopped", CreatedAt: pt5}},
		PredecessorDrain: &postgres.DrainResultView{Outcome: postgres.DrainOutcomeStopFailed, Summary: "stop timed out after 30s", PredecessorReleaseID: "rel-prev", ReportedAt: pt5},
		ReleaseCleanup:   &postgres.ReleaseCleanup{ID: 3, Result: "failed", ErrorClass: "engine", Summary: "daemon unreachable", Attempts: 3, CompletedAt: &done},
	}
	return pageGet(t, h, p, "/deployments/detail?id=dep-fail")
}

func detailFailedFragments() []string {
	return []string{
		// Summary: terminal job, so no refresh.
		`dep-fail`, `>proj-fail<`, `>env-fail<`, `>srv-fail<`, paritySHA12, `>failed<`, `>dead<`, ts(pt1), ts(pt2),
		hrefAttr("/environments/config?environment_id=env-fail"), hrefAttr("/environments/route?environment_id=env-fail"),
		hrefAttr("/deployments/logs?id=dep-fail"), hrefAttr("/deployments?project_id=proj-fail"),
		// Predecessor drain.
		`stop_failed`, `stop timed out after 30s`,
		// Status timeline (revision, from, to, time).
		`>19<`, `>18<`, `>building<`, `>queued<`, ts(pt5), ts(pt6),
		// Job.
		`>52<`, `>9<`, `>33<`, `>32<`, `>failed<`, `>retry_scheduled<`, `>82<`, `>81<`, ts(pt3), ts(pt4),
		// Attempts: failure class is the key diagnostic.
		`>7<`, `>8<`, `health_gate_failed`, `image_build_failed`, `>61<`, `>62<`, `>blue_green<`, `>stop_then_start<`,
		// Release with digest and retention cleanup.
		`>rel-fail<`, `>retained<`, `aaaabbbbccccddddeeee`, `0123abcd4567`, `engine: daemon unreachable (attempt 3)`,
		// Runtime instances.
		`>93<`, `>63<`, `>stopped<`,
		// Retry form (five fields).
		`action="/deployments/manual"`, `name="project_id" value="proj-fail"`, `name="mode" value="sha"`, `name="commit_sha" value="` + paritySHA + `"`,
		// Rollback form (four fields) and the shared hidden fields.
		`action="/deployments/rollback"`, `name="release_id" value="rel-fail"`, `name="confirm" value="rollback"`,
		`name="csrf" value="`, `name="request_nonce" value="`,
	}
}

// ---- logs ----

func parityLogs(t *testing.T) string {
	h, ls, _, p := logsHandler(t)
	at := func(l postgres.DeploymentLogLine, offset time.Duration) postgres.DeploymentLogLine {
		l.CreatedAt = t0.Add(offset)
		return l
	}
	next := int64(500)
	ls.page = postgres.DeploymentLogPage{
		Items: []postgres.DeploymentLogLine{
			at(line(1, 13, "build", "step one FROM base"), 1*time.Second),
			at(line(2, 13, "phase", "phase fetching entered"), 2*time.Second),
			at(line(3, 13, "health", "probe ok"), 3*time.Second),
			at(line(4, 14, "activation", "route switched"), 4*time.Second),
			at(line(5, 14, "drain", "predecessor stopped"), 5*time.Second),
			at(line(6, 14, "system", "worker note"), 6*time.Second),
			at(line(7, 14, "system", postgres.LogTruncationMarker), 7*time.Second),
			at(line(8, 14, "system", agentDropMarkerPrefix+"12 lines]"), 8*time.Second),
		},
		Next: &next,
	}
	return pageGet(t, h, p, "/deployments/logs?id=dep-1&after=40")
}

func logsFragments() []string {
	return []string{
		// Navigation: detail, start (after>0), newer (Next cursor).
		hrefAttr("/deployments/detail?id=dep-1"),
		hrefAttr("/deployments/logs?id=dep-1"),
		hrefAttr("/deployments/logs?after=500&id=dep-1"),
		// Lines: stream label, body and timestamp per line.
		`>build<`, `step one FROM base`, `datetime="2026-11-11T10:00:01.123Z"`, `10:00:01.123`,
		`>phase<`, `phase fetching entered`, `10:00:02.123`,
		`>health<`, `probe ok`, `10:00:03.123`,
		`>activation<`, `route switched`, `10:00:04.123`,
		`>drain<`, `predecessor stopped`, `10:00:05.123`,
		`>system<`, `worker note`, `10:00:06.123`,
		// Truncation and dropped-lines markers keep their marker flag.
		`class="marker"`, postgres.LogTruncationMarker, agentDropMarkerPrefix + `12 lines]`, `10:00:07.123`, `10:00:08.123`,
		// Attempt numbers group the lines (distinct so the bare digits are unambiguous).
		`13`, `14`,
	}
}

// ---- servers ----

func parityServers(t *testing.T) string {
	h, admin, p := serversHandler(t)
	admin.servers = []postgres.ServerSummary{
		{ID: "srv_alpha", Name: "alpha-host", Status: server.StatusActive, CreatedAt: pt1, LastHeartbeatAt: pt2, ActiveCredentialVersion: 3, Enrollment: postgres.EnrollmentConsumed},
		{ID: "srv_beta", Name: "beta-host", Status: server.StatusRevoked, CreatedAt: pt3, Enrollment: postgres.EnrollmentPending},
	}
	admin.environments = []postgres.ReconcilingEnvironment{
		{EnvironmentID: "env-reconcile", AttemptID: 55, JobID: 66, ServerID: "srv_alpha", Since: pt4, Activation: "activated"},
	}
	return pageGet(t, h, p, "/servers")
}

func serversFragments() []string {
	return []string{
		// Server table and forms: stay on /servers.
		`action="/servers/register"`, `name="name"`,
		`>srv_alpha<`, `>alpha-host<`, `>active<`, ts(pt1), ts(pt2), `>3<`, `>consumed<`,
		`>srv_beta<`, `>beta-host<`, `>revoked<`, ts(pt3), `>pending<`,
		`action="/servers/enrollment"`, `action="/servers/reenroll"`, `action="/servers/revoke"`,
		`name="server_id" value="srv_alpha"`, `name="server_id" value="srv_beta"`,
		`name="reason"`, `name="confirm" value="reenroll"`, `name="confirm" value="revoke"`,
		`name="csrf" value="`,
		// Reconciling rows and resolve forms moved to /reconciliation in 22-3g; the
		// environment-ID GET form was removed there (owner-approved: the env strip replaces it).
	}
}

// ---- reconciliation ----

func parityReconciliation(t *testing.T) string {
	h, admin, p := serversHandler(t)
	admin.environments = []postgres.ReconcilingEnvironment{
		{EnvironmentID: "env-reconcile", AttemptID: 55, JobID: 66, ServerID: "srv_alpha", Since: pt4, Activation: "activated"},
	}
	return pageGet(t, h, p, "/reconciliation")
}

func reconciliationFragments() []string {
	return []string{
		// Moved from the servers entry (22-3g).
		`env-reconcile`, hrefAttr("/environments/config?environment_id=env-reconcile"), hrefAttr("/environments/route?environment_id=env-reconcile"),
		`>55<`, `>66<`, `>activated<`, ts(pt4),
		`action="/environments/resolve"`,
		`name="environment_id" value="env-reconcile"`, `name="attempt_id" value="55"`,
		`name="outcome"`, `value="activated"`, `value="not_activated"`, `value="no_activation"`,
		`maxlength="` + strconv.Itoa(maxResolveReasonBytes) + `"`,
		`name="return_to" value="/reconciliation"`,
	}
}

// ---- environment config ----

func configStub(t *testing.T) (*Handler, *runtimeConfigAdminStub, auth.Principal) {
	h, _, p := serversHandler(t)
	stub := &runtimeConfigAdminStub{}
	h.config.RuntimeConfigAdmin = stub
	return h, stub, p
}

func healthBoundFragments() []string {
	return []string{
		`min="` + strconv.Itoa(healthgate.MinSuccessThreshold) + `" max="` + strconv.Itoa(healthgate.MaxSuccessThreshold) + `"`,
		`min="` + strconv.Itoa(healthgate.MinIntervalSeconds) + `" max="` + strconv.Itoa(healthgate.MaxIntervalSeconds) + `"`,
		`min="` + strconv.Itoa(healthgate.MinTimeoutSeconds) + `" max="` + strconv.Itoa(healthgate.MaxTimeoutSeconds) + `"`,
		`min="` + strconv.Itoa(healthgate.MinDeadlineSeconds) + `" max="` + strconv.Itoa(healthgate.MaxDeadlineSeconds) + `"`,
		`min="` + strconv.Itoa(healthgate.MinTCPPort) + `" max="` + strconv.Itoa(healthgate.MaxTCPPort) + `"`,
	}
}

// configFormNames are the 17 posted fields of the config form; the add and
// remove binding forms carry their own.
var configFormNames = []string{
	"environment_id", "csrf", "expected_revision", "container_port", "container_scheme", "release_strategy",
	"drain_seconds", "cpu_millis", "memory_bytes", "restart_policy", "health_gate_path", "health_gate_host",
	"health_success_threshold", "health_interval_seconds", "health_timeout_seconds", "health_deadline_seconds", "health_tcp_port",
}

func nameAttrs(names ...string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, `name="`+n+`"`)
	}
	return out
}

func parityConfigExists(t *testing.T) string {
	h, stub, p := configStub(t)
	stub.getFound = true
	stub.getCfg = postgres.RuntimeConfig{
		EnvironmentID: "env-cfg",
		RuntimeConfigInput: postgres.RuntimeConfigInput{
			ContainerPort: 8081, ContainerScheme: "https", ReleaseStrategy: "blue_green", DrainSeconds: 45, CPUMillis: 750,
			MemoryBytes: 536870912, RestartPolicy: "on-failure", HealthGatePath: "/ready", HealthGateHost: "svc.internal",
			HealthPolicy: healthgate.Policy{SuccessThreshold: 4, IntervalSeconds: 5, TimeoutSeconds: 6, DeadlineSeconds: 90, TCPPort: 8443},
		},
		HealthPolicyRevision: 2, Revision: 12, UpdatedAt: pt1,
	}
	stub.getBindings = []postgres.RuntimeBinding{
		{VariableName: "API_TOKEN", BindingKind: "secret", DeliveryScope: "runtime", SecretHeadID: "head_runtime", SecretVersionNumber: 2, CreatedAt: pt2},
		{VariableName: "BUILD_SECRET", BindingKind: "secret", DeliveryScope: "build", SecretHeadID: "head_build", SecretVersionNumber: 5, CreatedAt: pt3},
		{VariableName: "PLAIN_FLAG", BindingKind: "literal_nonsecret", DeliveryScope: "runtime", LiteralValue: "literal-on", CreatedAt: pt4},
	}
	return pageGet(t, h, p, "/environments/config?environment_id=env-cfg")
}

func configExistsFragments() []string {
	dockerfileID, _ := buildsecret.ID([]string{"BUILD_SECRET"}, "BUILD_SECRET")
	frags := []string{
		// Config form.
		`action="/environments/config"`, `name="environment_id" value="env-cfg"`, `name="expected_revision" value="12"`,
		`value="8081"`, `value="https" selected`, `value="blue_green" selected`, `value="45"`, `value="750"`, `value="536870912"`,
		`value="on-failure" selected`, `value="/ready"`, `value="svc.internal"`,
		`value="4"`, `value="5"`, `value="6"`, `value="90"`, `value="8443"`,
		// Options offered.
		`value="http"`, `value="stop_then_start"`, `value="unless-stopped"`,
		// Bindings table.
		`>API_TOKEN<`, `>BUILD_SECRET<`, `>PLAIN_FLAG<`, `>secret<`, `>literal_nonsecret<`, `>runtime<`, `>build<`,
		`secret head head_runtime (version 2)`, `secret head head_build (version 5)`, `Dockerfile id: ` + dockerfileID, `literal-on`,
		ts(pt2), ts(pt3), ts(pt4),
		// Remove and add binding forms.
		`action="/environments/bindings/remove"`, `name="variable_name" value="API_TOKEN"`, `name="variable_name" value="PLAIN_FLAG"`,
		`action="/environments/bindings/add"`, `value="secret"`, `value="literal_nonsecret"`, `value="runtime"`, `value="build"`,
		// Links.
		hrefAttr("/environments/route?environment_id=env-cfg"),
		`name="csrf" value="`,
	}
	frags = append(frags, nameAttrs(configFormNames...)...)
	frags = append(frags, nameAttrs("variable_name", "binding_kind", "delivery_scope", "secret_head_id", "literal_value")...)
	return append(frags, healthBoundFragments()...)
}

func parityConfigNew(t *testing.T) string {
	h, _, p := configStub(t)
	return pageGet(t, h, p, "/environments/config?environment_id=env-new")
}

func configNewFragments() []string {
	d := healthgate.DefaultPolicy()
	frags := []string{
		`action="/environments/config"`, `name="environment_id" value="env-new"`, `name="expected_revision" value=""`,
		// Create-form defaults.
		`value="http" selected`, `value="stop_then_start" selected`, `value="unless-stopped" selected`, `value="/"`,
		`value="` + strconv.Itoa(d.SuccessThreshold) + `"`, `value="` + strconv.Itoa(d.IntervalSeconds) + `"`,
		`value="` + strconv.Itoa(d.TimeoutSeconds) + `"`, `value="` + strconv.Itoa(d.DeadlineSeconds) + `"`,
		`action="/environments/bindings/add"`,
		hrefAttr("/environments/route?environment_id=env-new"),
		`name="csrf" value="`,
	}
	frags = append(frags, nameAttrs(configFormNames...)...)
	frags = append(frags, nameAttrs("variable_name", "binding_kind", "delivery_scope", "secret_head_id", "literal_value")...)
	return append(frags, healthBoundFragments()...)
}

// ---- environment route ----

func parityRouteExists(t *testing.T) string {
	h, _, p := routeReleasesHandler(t)
	admin := h.config.RouteHeadAdmin.(*routeHeadAdminStub)
	admin.getHead = postgres.RouteHead{EnvironmentID: "env-a", Hostname: "app.example.com", RouteRevision: 9, ActiveReleaseID: "rel-active", ReconciliationRequired: true, CreatedAt: pt1, UpdatedAt: pt2}
	return routeReleasesGet(t, h, p).Body.String()
}

func routeExistsFragments() []string {
	return []string{
		// Reconciliation flag is a fact (R4); 22-3 may reword it but must keep a stable marker.
		`reconciliation required`,
		// Hostname form.
		`action="/environments/route"`, `name="environment_id" value="env-a"`, `name="expected_revision" value="9"`,
		`name="hostname"`, `value="app.example.com"`, `name="acknowledge_authority"`, `name="csrf" value="`,
		// Head state.
		`rel-active`, ts(pt2),
		// Releases table with cleanup (sampleEnvReleases).
		`>rel-active<`, `>rel-retained<`, `>rel-removing<`, `>rel-removed<`, `>rel-failedcand<`, `>rel-skipped<`,
		`>active<`, `>retained<`, `>removing<`, `>removed<`, `>blue_green<`, `>stop_then_start<`,
		`2026-11-14T09:00:00Z`, `2026-11-14T08:00:00Z`, `2026-11-14T07:00:00Z`, `2026-11-14T06:00:00Z`, `2026-11-14T05:00:00Z`, `2026-11-14T04:00:00Z`,
		`>failed<`, `>skipped<`, `engine: daemon unreachable (attempt 3)`, `identity (attempt 1)`, `container removed; image kept`,
		hrefAttr("/environments/config?environment_id=env-a"),
	}
}

func parityRouteNew(t *testing.T) string {
	h, _, p := routeHeadHandler(t)
	return opsGet(h, p, http.MethodGet, "/environments/route?environment_id=env-new").Body.String()
}

func routeNewFragments() []string {
	return []string{
		`action="/environments/route"`, `name="environment_id" value="env-new"`, `name="expected_revision" value=""`,
		`name="hostname"`, `name="acknowledge_authority"`, `name="csrf" value="`,
		hrefAttr("/environments/config?environment_id=env-new"),
	}
}

// ---- flow-only pages ----

func parityEnrollmentToken(t *testing.T) string {
	return execTemplate(t, func(b *bytes.Buffer) error {
		return enrollmentTokenTemplate.Execute(b, enrollmentView{Nav: parityNav(t, "servers"), ServerID: "srv_enroll", Token: "tok-enroll-secret", ExpiresAt: "2026-03-04T05:06:07Z"})
	})
}

func enrollmentFragments() []string {
	return []string{`srv_enroll`, `tok-enroll-secret`, `2026-03-04T05:06:07Z`}
}

func parityGitHubComplete(t *testing.T) string {
	return execTemplate(t, func(b *bytes.Buffer) error {
		return githubCompleteTemplate.Execute(b, githubCompleteView{Nav: parityNav(t, "github"), CSRF: "csrf-complete"})
	})
}

func parityGitHubContinue(t *testing.T) string {
	return execTemplate(t, func(b *bytes.Buffer) error {
		return githubContinueTemplate.Execute(b, githubContinueView{Nav: parityNav(t, "github")})
	})
}

// ---- 22-3h: first-class entry pages and filters ----

func parityConfigNoID(t *testing.T) string {
	h, _, _, _, p := fcHandler(t, fourRows())
	return pageGet(t, h, p, "/environments/config")
}

func parityRouteNoID(t *testing.T) string {
	h, _, _, _, p := fcHandler(t, fourRows())
	return pageGet(t, h, p, "/environments/route")
}

// environmentNoIDFragments pins the first owned environment (sorted by id),
// the strip over the owned environments only, and the page's own form.
func environmentNoIDFragments(path string) []string {
	return []string{
		`<nav aria-label="Environments" class="strip">`,
		`<a href="` + path + `?environment_id=env-blocked" aria-current="true">env-blocked</a>`,
		`<a href="` + path + `?environment_id=env-failed">env-failed</a>`,
		`<a href="` + path + `?environment_id=env-live">env-live</a>`,
		`action="` + path + `"`, `name="environment_id" value="env-blocked"`, `name="expected_revision" value=""`, `name="csrf" value="`,
	}
}

func parityLogsNoID(t *testing.T) string {
	h, ls, _, p := logsPickerHandler(t, nDeployments(3))
	ls.page = postgres.DeploymentLogPage{Items: []postgres.DeploymentLogLine{line(1, 1, "build", "newest build line")}}
	return pageGet(t, h, p, "/deployments/logs")
}

func logsNoIDFragments() []string {
	return []string{
		`<form method="get" action="/deployments/logs" class="picker">`, `<select name="id">`,
		`<option value="dep-01" selected>`, `<option value="dep-02">`, `<option value="dep-03">`,
		`>build<`, `newest build line`, hrefAttr("/deployments/detail?id=dep-01"),
	}
}

func parityDeploymentsFilter(t *testing.T) string {
	h, stub, p := filterHandler(t)
	stub.list = postgres.DeploymentList{Items: nDeployments(1)}
	return pageGet(t, h, p, "/deployments?project_id=proj-b")
}

func deploymentsFilterFragments() []string {
	return []string{
		`<form method="get" action="/deployments" class="filter-form">`, `<select name="project_id">`, `<select name="environment_id">`,
		`<option value="proj-b" selected>proj-b</option>`, `<option value="proj-a">proj-a</option>`,
		`<option value="env-1">env-1</option>`, `<option value="env-2">env-2</option>`, `<option value="env-3">env-3</option>`,
		hrefAttr("/deployments"), `>dep-01<`,
	}
}
