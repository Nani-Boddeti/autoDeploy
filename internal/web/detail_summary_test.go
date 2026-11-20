package web

import (
	"net/http"
	"strings"
	"testing"

	"autodeploy/internal/store/postgres"
)

// summaryPage renders the detail page of dep-1 with every optional dependency
// wired (retry, rollback, cancel, logs) after mutate adjusts the store detail.
func summaryPage(t *testing.T, mutate func(d *postgres.DeploymentDetail)) string {
	t.Helper()
	h, _, _, p := manualHandler(t)
	h.config.Rollback = &rollbackStoreStub{eligible: true}
	h.config.DeploymentCanceller = &cancellerStub{}
	h.config.DeploymentLogReader = &logStub{}
	stub := h.config.OperationsReader.(*operationsStub)
	stub.projects = []postgres.ProjectSummary{{ProjectID: "proj", HasBinding: true, BindingActive: true, RepositoryDefaultRef: "refs/heads/main", BindingEnvironmentID: "env"}}
	stub.detail = sampleDetail("succeeded", false)
	mutate(&stub.detail)
	return opsGet(h, p, http.MethodGet, "/deployments/detail?id=dep-1").Body.String()
}

// summaryOf returns the <section class="summary"> block of a detail page.
func summaryOf(t *testing.T, page string) string {
	t.Helper()
	i := strings.Index(page, `<section class="summary"`)
	if i < 0 {
		t.Fatalf("no summary section:\n%s", page)
	}
	rest := page[i:]
	j := strings.Index(rest, "</section>")
	if j < 0 {
		t.Fatal("summary section not closed")
	}
	return rest[:j]
}

func withFailure(d *postgres.DeploymentDetail, status string) {
	d.Deployment.Status = status
	d.Deployment.FailureClass, d.Deployment.FailureReason = "transient", "container_unhealthy"
	d.Attempts = []postgres.AttemptView{{ID: 7, AttemptNumber: 1, State: "failed", FailureClass: "transient", Fence: 4, FailureReason: "container_unhealthy", FailureProbe: "status=200; tcp ok", CreatedAt: t0}}
	d.ActivationSummary = "route switch aborted"
}

func TestSummaryFailureBlockForFailedDeployment(t *testing.T) {
	page := summaryPage(t, func(d *postgres.DeploymentDetail) { withFailure(d, "failed") })
	sum := summaryOf(t, page)
	contains(t, sum, `class="failure"`, "Temporary failure", "Image HEALTHCHECK reported unhealthy", "Last probe:", "status=200; tcp ok", "Activation:", "route switch aborted")
}

func TestSummaryFailureBlockEscapesEverything(t *testing.T) {
	page := summaryPage(t, func(d *postgres.DeploymentDetail) {
		withFailure(d, "failed")
		d.Deployment.FailureClass = `<script>c</script>"`
		d.Deployment.FailureReason = `<script>r</script>"`
		d.Attempts[0].FailureProbe = `<script>p</script>"`
		d.ActivationSummary = `<script>a</script>"`
	})
	sum := summaryOf(t, page)
	lacks(t, page, "<script>")
	contains(t, sum, "&lt;script&gt;c&lt;/script&gt;", "&lt;script&gt;r&lt;/script&gt;", "&lt;script&gt;p&lt;/script&gt;", "&lt;script&gt;a&lt;/script&gt;", "&#34;")
}

func TestSummaryNoFailureBlockForNonFailedStatuses(t *testing.T) {
	for _, status := range []string{"healthy", "rolled_back", "building"} {
		page := summaryPage(t, func(d *postgres.DeploymentDetail) { withFailure(d, status) })
		sum := summaryOf(t, page)
		lacks(t, sum, `class="failure"`, "Temporary failure", "Image HEALTHCHECK", "Last probe:", "Activation:", "status=200", "route switch aborted")
	}
}

func TestSummaryFailureBlockOmittedWhenNothingToShow(t *testing.T) {
	page := summaryPage(t, func(d *postgres.DeploymentDetail) { d.Deployment.Status = "failed" })
	lacks(t, summaryOf(t, page), `class="failure"`)
}

func TestSummaryUnknownClassAndReasonRenderRaw(t *testing.T) {
	page := summaryPage(t, func(d *postgres.DeploymentDetail) {
		withFailure(d, "failed")
		d.Deployment.FailureClass, d.Deployment.FailureReason = "new_class<b>", "new_reason&x"
	})
	contains(t, summaryOf(t, page), "new_class&lt;b&gt;", "new_reason&amp;x")
}

func TestSummaryStateFlagMarkers(t *testing.T) {
	const live, rolled = `data-fact="live-route"`, `data-fact="rolled-back"`
	cases := []struct {
		name         string
		status       string
		active       bool
		wantLive     bool
		wantRolledBk bool
	}{
		{"plain", "healthy", false, false, false},
		{"active", "healthy", true, true, false},
		{"rolled back", "rolled_back", false, false, true},
		{"rolled back but active", "rolled_back", true, true, false},
	}
	for _, c := range cases {
		page := summaryPage(t, func(d *postgres.DeploymentDetail) {
			d.Deployment.Status, d.ReleaseIsActive = c.status, c.active
		})
		sum := summaryOf(t, page)
		if got := strings.Contains(sum, live); got != c.wantLive {
			t.Errorf("%s: live marker = %v", c.name, got)
		}
		if got := strings.Contains(sum, rolled); got != c.wantRolledBk {
			t.Errorf("%s: rolled-back marker = %v", c.name, got)
		}
		if c.wantLive {
			contains(t, sum, "live on route")
		}
		if c.wantRolledBk {
			contains(t, sum, `<span class="pill neutral">rolled back</span>`)
		}
	}
}

func TestSummaryEmptySectionsCollapseIntoFacts(t *testing.T) {
	page := summaryPage(t, func(d *postgres.DeploymentDetail) {
		d.StatusEvents, d.JobEvents, d.Attempts, d.Release, d.RuntimeInstances = nil, nil, nil, nil, nil
	})
	sum := summaryOf(t, page)
	contains(t, sum, "not yet", "not started (job not claimed)", "none recorded")
	lacks(t, page, `class="empty"`, "<h2>Status timeline</h2>", "<h2>Job events</h2>", "<h2>Attempts</h2>", "<h2>Release</h2>", "<h2>Runtime instances</h2>", "<table")
	for _, want := range []string{"Release", "Attempts", "Runtime instances", "Status events", "Job events"} {
		contains(t, sum, "<dt>"+want+"</dt>")
	}
}

func TestSummaryPopulatedSectionsInOrder(t *testing.T) {
	page := summaryPage(t, func(d *postgres.DeploymentDetail) {})
	last := strings.Index(page, "</section>")
	for _, h := range []string{"<h2>Status timeline</h2>", "<h2>Job events</h2>", "<h2>Attempts</h2>", "<h2>Release</h2>", "<h2>Runtime instances</h2>"} {
		i := strings.Index(page, h)
		if i < last {
			t.Fatalf("%s missing or before the summary (%d < %d)", h, i, last)
		}
		last = i
	}
	lacks(t, summaryOf(t, page), "not yet", "not started", "none recorded")
}

func TestAttemptReasonAndProbeOnlyForFailedAttempts(t *testing.T) {
	page := summaryPage(t, func(d *postgres.DeploymentDetail) {
		d.Attempts = []postgres.AttemptView{
			{ID: 2, AttemptNumber: 2, State: "failed", FailureClass: "transient", FailureReason: "deadline_exceeded", FailureProbe: "PROBE-FAILED", Fence: 5, CreatedAt: t0},
			{ID: 1, AttemptNumber: 1, State: "started", FailureReason: "container_unhealthy", FailureProbe: "PROBE-OPEN", Fence: 4, CreatedAt: t0},
		}
	})
	contains(t, page, "<th>Reason</th>", "<th>Last probe</th>", "Health check deadline exceeded", "PROBE-FAILED")
	lacks(t, page, "PROBE-OPEN", "Image HEALTHCHECK reported unhealthy")
}

func TestSummaryActionsRow(t *testing.T) {
	page := summaryPage(t, func(d *postgres.DeploymentDetail) {
		d.Deployment.Status, d.Job.State = "failed", "leased"
		d.Release.State = "retained"
		d.Deployment.CommitSHA = "0123456789abcdef0123"
	})
	sum := summaryOf(t, page)
	i := strings.Index(sum, `class="actions"`)
	if i < 0 {
		t.Fatalf("no actions row:\n%s", sum)
	}
	acts := sum[i:]
	form := func(action string) string {
		k := strings.Index(acts, `action="`+action+`"`)
		if k < 0 {
			t.Fatalf("no %s form", action)
		}
		f := acts[k:]
		return f[:strings.Index(f, "</form>")]
	}
	retry, rollback, cancel := form("/deployments/manual"), form("/deployments/rollback"), form("/deployments/cancel")
	if n := strings.Count(retry, "<input"); n != 5 {
		t.Errorf("retry inputs = %d", n)
	}
	contains(t, retry, `name="csrf"`, `name="project_id" value="proj"`, `name="request_nonce"`, `name="mode" value="sha"`, `name="commit_sha"`, "Retry this commit")
	lacks(t, retry, "danger")
	contains(t, rollback, `name="csrf"`, `name="release_id" value="rel-1"`, `name="request_nonce"`, `name="confirm" value="rollback" required`, `<button type="submit" class="btn danger">Roll back to this release</button>`)
	contains(t, cancel, `name="deployment_id" value="dep-1"`, `name="csrf"`, `name="confirm" value="cancel" required`, `<button type="submit" class="btn danger">Cancel deployment</button>`)
	contains(t, acts, hrefAttr("/deployments/logs?id=dep-1"))
}

func TestSummaryRefreshMetaUnchanged(t *testing.T) {
	const meta = `<meta http-equiv="refresh" content="5">`
	if page := summaryPage(t, func(d *postgres.DeploymentDetail) { d.Job.State = "leased" }); !strings.Contains(page, meta) || !strings.Contains(page, "refreshes every 5 seconds") {
		t.Error("no refresh while non-terminal")
	}
	if page := summaryPage(t, func(d *postgres.DeploymentDetail) { d.Job.State = "succeeded" }); strings.Contains(page, "refresh") {
		t.Error("refresh present for terminal job")
	}
}

func TestLatestFailedProbeOnlyFromNewestAttempt(t *testing.T) {
	failed := postgres.AttemptView{ID: 1, AttemptNumber: 1, State: "failed", FailureProbe: "status=503"}
	if got := latestFailedProbe([]postgres.AttemptView{failed}); got != "status=503" {
		t.Fatalf("single failed attempt: got %q", got)
	}
	later := postgres.AttemptView{ID: 2, AttemptNumber: 2, State: "cancelled"}
	if got := latestFailedProbe([]postgres.AttemptView{later, failed}); got != "" {
		t.Fatalf("newest attempt not failed: got %q, want empty", got)
	}
	newestFailed := postgres.AttemptView{ID: 3, AttemptNumber: 3, State: "failed", FailureProbe: "tcp ok"}
	if got := latestFailedProbe([]postgres.AttemptView{failed, newestFailed, later}); got != "tcp ok" {
		t.Fatalf("newest failed: got %q", got)
	}
	if got := latestFailedProbe(nil); got != "" {
		t.Fatalf("no attempts: got %q", got)
	}
}
