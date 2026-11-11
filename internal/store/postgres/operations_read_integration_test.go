//go:build integration

package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"autodeploy/internal/auth"

	"github.com/jackc/pgx/v5/pgxpool"
)

const opsOwnerB = "ops-owner-b"

type opsFixture struct {
	leaseFixture
	repo       *OperationsReadRepository
	principalB auth.Principal
}

// newOpsFixture builds owner A (the lease fixture: binding for "project",
// environment "environment", one queued deployment) and owner B with an
// overlapping project/environment id but its own installation/repository.
func newOpsFixture(t *testing.T, seed byte) opsFixture {
	t.Helper()
	f := newLeaseFixture(t, seed)
	ctx := context.Background()
	for _, q := range []string{
		`INSERT INTO owners(id) VALUES ('` + opsOwnerB + `') ON CONFLICT DO NOTHING`,
		`INSERT INTO github_installations(installation_id, owner_id, account_id, account_type) VALUES (102, '` + opsOwnerB + `', 10, 'User')`,
		`INSERT INTO github_repositories(repository_id, installation_id, owner_id, github_owner_id, name, full_name, clone_url) VALUES (202, 102, '` + opsOwnerB + `', 10, 'repo-b', 'owner-b/repo-b', 'https://github.com/owner-b/repo-b.git')`,
		`INSERT INTO github_project_bindings(owner_id, project_id, repository_id, installation_id) VALUES ('` + opsOwnerB + `', 'project', 202, 102)`,
	} {
		if _, err := f.pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`INSERT INTO users(id, owner_id, username, role, password_phc, password_policy_revision) VALUES ('ops-user-b','` + opsOwnerB + `','ops-user-b','owner',repeat('x',70),1)`,
		`INSERT INTO admin_sessions(id, token_digest, user_id, owner_id, auth_revision, last_seen_at, idle_expires_at, absolute_expires_at)
		 VALUES ('ops-sess-b', decode(repeat('5b',32),'hex'), 'ops-user-b', '` + opsOwnerB + `', 1, clock_timestamp(), clock_timestamp()+interval '1 hour', clock_timestamp()+interval '2 hours')`,
	} {
		if _, err := f.pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	pb := auth.Principal{UserID: "ops-user-b", OwnerID: opsOwnerB, Role: auth.RoleOwner, AuthRevision: 1, SessionID: "ops-sess-b"}
	return opsFixture{leaseFixture: f, repo: NewOperationsReadRepository(f.pool), principalB: pb}
}

// seedOpsDeployment inserts a deployment and (when owner != "") its owning
// job. Raw SQL so created_at can be pinned (equal timestamps for keyset tests).
func seedOpsDeployment(t *testing.T, pool *pgxpool.Pool, owner, project, env, depID string, createdAt time.Time) int64 {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO deployments(id, project_id, environment_id, server_id, commit_sha, schema_version, status, created_at, updated_at, revision)
		VALUES ($1,$2,$3,'server','0123456789abcdef0123456789abcdef01234567',1,'queued',$4,$4,1)`, depID, project, env, createdAt); err != nil {
		t.Fatal(err)
	}
	if owner == "" {
		return 0
	}
	var jobID int64
	if err := pool.QueryRow(ctx, `INSERT INTO agent_jobs(schema_version,job_kind,owner_id,project_id,environment_id,server_id,deployment_id,repository_id,full_ref,commit_sha,priority,available_at,idempotency_key,payload_digest,state)
		VALUES (1,'deployment_execution',$1,$2,$3,'server',$4,201,'refs/heads/main','0123456789abcdef0123456789abcdef01234567',0,clock_timestamp(),$4,decode(repeat('00',32),'hex'),'queued') RETURNING id`,
		owner, project, env, depID).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO agent_job_events(job_id,sequence,event_type) VALUES ($1,1,'enqueued')`, jobID); err != nil {
		t.Fatal(err)
	}
	return jobID
}

func TestOperationsReadEmptyDatabase(t *testing.T) {
	f := newOpsFixture(t, 150)
	resetWebhookJobs(t, f.pool)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `DELETE FROM github_project_bindings WHERE owner_id=$1`, opsOwnerB); err != nil {
		t.Fatal(err)
	}
	nobody := f.principalB // live session, no data

	projects, err := f.repo.ListProjects(ctx, nobody)
	if err != nil || projects == nil || len(projects) != 0 {
		t.Fatalf("projects = %v, %v", projects, err)
	}
	list, err := f.repo.ListDeployments(ctx, nobody, DeploymentFilter{}, DeploymentPage{})
	if err != nil || list.Items == nil || len(list.Items) != 0 || list.Next != nil {
		t.Fatalf("deployments = %+v, %v", list, err)
	}
	if _, err = f.repo.GetDeploymentDetail(ctx, nobody, "missing"); !errors.Is(err, ErrOperationsNotFound) {
		t.Fatalf("detail = %v", err)
	}
	// Bound owner with no jobs: binding-only project, no latest deployment.
	projects, err = f.repo.ListProjects(ctx, f.principal)
	if err != nil || len(projects) != 1 || !projects[0].HasBinding || projects[0].LatestDeployment != nil || fmt.Sprint(projects[0].EnvironmentIDs) != "[environment]" {
		t.Fatalf("binding-only projects = %+v, %v", projects, err)
	}
}

func TestOperationsReadOwnerIsolation(t *testing.T) {
	f := newOpsFixture(t, 152)
	ctx := context.Background()
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	seedOpsDeployment(t, f.pool, f.ownerID, "project", "environment", "a-dep-1", at)
	seedOpsDeployment(t, f.pool, opsOwnerB, "project", "environment", "b-dep-1", at)
	seedOpsDeployment(t, f.pool, opsOwnerB, "project", "env-b-only", "b-dep-2", at.Add(time.Minute))
	seedOpsDeployment(t, f.pool, "", "project", "environment", "orphan-dep", at.Add(time.Hour)) // no job: must not leak

	for _, tc := range []struct {
		name string
		p    auth.Principal
		want []string
	}{
		{"A", f.principal, []string{"a-dep-1"}},
		{"B", f.principalB, []string{"b-dep-2", "b-dep-1"}},
	} {
		list, err := f.repo.ListDeployments(ctx, tc.p, DeploymentFilter{}, DeploymentPage{Limit: 50})
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, it := range list.Items {
			if strings.HasPrefix(it.DeploymentID, "lease-deployment") {
				if tc.name != "A" {
					t.Fatalf("B sees A's fixture deployment")
				}
				continue
			}
			got = append(got, it.DeploymentID)
		}
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Fatalf("%s sees %v, want %v", tc.name, got, tc.want)
		}
	}

	// Detail: other owner's, unknown and orphan ids give the identical error.
	_, errForeign := f.repo.GetDeploymentDetail(ctx, f.principal, "b-dep-1")
	_, errUnknown := f.repo.GetDeploymentDetail(ctx, f.principal, "nope")
	_, errOrphan := f.repo.GetDeploymentDetail(ctx, f.principal, "orphan-dep")
	_, errForeign2 := f.repo.GetDeploymentDetail(ctx, f.principalB, "a-dep-1")
	for _, err := range []error{errForeign, errUnknown, errOrphan, errForeign2} {
		if err != ErrOperationsNotFound {
			t.Fatalf("err = %v, want the single sentinel", err)
		}
	}
	if d, err := f.repo.GetDeploymentDetail(ctx, f.principalB, "b-dep-1"); err != nil || d.Deployment.DeploymentID != "b-dep-1" {
		t.Fatalf("own detail = %+v, %v", d, err)
	}

	// Projects: same project id, each owner sees only its own repo/envs.
	pa, err := f.repo.ListProjects(ctx, f.principal)
	if err != nil || len(pa) != 1 {
		t.Fatalf("A projects = %+v, %v", pa, err)
	}
	pb, err := f.repo.ListProjects(ctx, f.principalB)
	if err != nil || len(pb) != 1 {
		t.Fatalf("B projects = %+v, %v", pb, err)
	}
	if pb[0].RepositoryFullName != "owner-b/repo-b" || pa[0].RepositoryFullName == "owner-b/repo-b" || pa[0].RepositoryFullName == "" {
		t.Fatalf("repo identity A=%q B=%q", pa[0].RepositoryFullName, pb[0].RepositoryFullName)
	}
	if fmt.Sprint(pb[0].EnvironmentIDs) != "[env-b-only environment]" || fmt.Sprint(pa[0].EnvironmentIDs) != "[environment]" {
		t.Fatalf("envs A=%v B=%v", pa[0].EnvironmentIDs, pb[0].EnvironmentIDs)
	}
	if pb[0].LatestDeployment == nil || pb[0].LatestDeployment.DeploymentID != "b-dep-2" || pb[0].LatestDeployment.JobState != "queued" {
		t.Fatalf("B latest = %+v", pb[0].LatestDeployment)
	}
	if pa[0].LatestDeployment == nil || pa[0].LatestDeployment.DeploymentID != "a-dep-1" {
		// a-dep-1 (2026-09-01) is newer than the fixture's 2026-08-13 job.
		t.Fatalf("A latest = %+v", pa[0].LatestDeployment)
	}
	// A project known only through jobs (no binding) is still listed.
	seedOpsDeployment(t, f.pool, f.ownerID, "jobs-only-project", "environment", "a-dep-jobs-only", at)
	pa, _ = f.repo.ListProjects(ctx, f.principal)
	if len(pa) != 2 || pa[0].ProjectID != "jobs-only-project" || pa[0].HasBinding || pa[1].ProjectID != "project" {
		t.Fatalf("A projects after jobs-only = %+v", pa)
	}
}

func TestOperationsReadKeysetPagingAndFilters(t *testing.T) {
	f := newOpsFixture(t, 154)
	resetWebhookJobs(t, f.pool)
	ctx := context.Background()
	tie := time.Date(2026, 9, 2, 12, 0, 0, 123456000, time.UTC)
	var want []string
	// 7 rows share one created_at (ties broken by id desc), 2 are newer, 1 is older.
	for i := 0; i < 7; i++ {
		seedOpsDeployment(t, f.pool, f.ownerID, "project", "environment", fmt.Sprintf("tie-%d", i), tie)
	}
	seedOpsDeployment(t, f.pool, f.ownerID, "other-project", "env-2", "newer-1", tie.Add(time.Second))
	seedOpsDeployment(t, f.pool, f.ownerID, "project", "env-2", "newer-2", tie.Add(2*time.Second))
	seedOpsDeployment(t, f.pool, f.ownerID, "project", "environment", "older", tie.Add(-time.Hour))
	want = []string{"newer-2", "newer-1", "tie-6", "tie-5", "tie-4", "tie-3", "tie-2", "tie-1", "tie-0", "older"}

	var got []string
	var after *DeploymentCursor
	pages := 0
	for {
		list, err := f.repo.ListDeployments(ctx, f.principal, DeploymentFilter{}, DeploymentPage{Limit: 3, After: after})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, it := range list.Items {
			got = append(got, it.DeploymentID)
		}
		if pages == 1 { // a newer row inserted mid-paging must not disturb later pages
			seedOpsDeployment(t, f.pool, f.ownerID, "project", "environment", "inserted-mid", tie.Add(time.Hour))
		}
		if list.Next == nil {
			break
		}
		if len(list.Items) != 3 {
			t.Fatalf("non-final page has %d items", len(list.Items))
		}
		after = list.Next
	}
	if fmt.Sprint(got) != fmt.Sprint(want) || pages != 4 {
		t.Fatalf("paged = %v (pages %d), want %v", got, pages, want)
	}

	// Exactly-full final page has no Next.
	list, err := f.repo.ListDeployments(ctx, f.principal, DeploymentFilter{}, DeploymentPage{Limit: 11})
	if err != nil || len(list.Items) != 11 || list.Next != nil {
		t.Fatalf("exact page = %d items, next=%v, err=%v", len(list.Items), list.Next, err)
	}
	// Default limit applies when 0; max limit accepted.
	if _, err = f.repo.ListDeployments(ctx, f.principal, DeploymentFilter{}, DeploymentPage{Limit: MaxDeploymentPageLimit}); err != nil {
		t.Fatal(err)
	}

	// Filters.
	byProject, _ := f.repo.ListDeployments(ctx, f.principal, DeploymentFilter{ProjectID: "other-project"}, DeploymentPage{})
	if len(byProject.Items) != 1 || byProject.Items[0].DeploymentID != "newer-1" {
		t.Fatalf("project filter = %+v", byProject.Items)
	}
	byEnv, _ := f.repo.ListDeployments(ctx, f.principal, DeploymentFilter{EnvironmentID: "env-2"}, DeploymentPage{})
	if len(byEnv.Items) != 2 {
		t.Fatalf("env filter = %+v", byEnv.Items)
	}
	both, _ := f.repo.ListDeployments(ctx, f.principal, DeploymentFilter{ProjectID: "project", EnvironmentID: "env-2"}, DeploymentPage{})
	if len(both.Items) != 1 || both.Items[0].DeploymentID != "newer-2" {
		t.Fatalf("combined filter = %+v", both.Items)
	}
	none, err := f.repo.ListDeployments(ctx, f.principal, DeploymentFilter{ProjectID: "absent"}, DeploymentPage{})
	if err != nil || len(none.Items) != 0 {
		t.Fatalf("absent filter = %+v, %v", none, err)
	}
}

func TestOperationsReadDetailAggregate(t *testing.T) {
	f := newOpsFixture(t, 156)
	resetWebhookJobs(t, f.pool)
	ctx := context.Background()
	at := time.Date(2026, 9, 3, 9, 0, 0, 0, time.UTC)
	jobID := seedOpsDeployment(t, f.pool, f.ownerID, "project", f.envID, "detail-dep", at)
	f.insertSlot(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := f.pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`UPDATE environment_slots SET fence=1 WHERE environment_id=$1`, f.envID)
	exec(`UPDATE agent_jobs SET state='leased', attempt_count=1 WHERE id=$1`, jobID)
	var attemptID int64
	if err := f.pool.QueryRow(ctx, `WITH t AS (SELECT clock_timestamp() + interval '90 seconds' AS d)
		INSERT INTO agent_job_attempts(job_id, attempt_number, server_id, environment_id, fence, state, lease_expires_at, authority_expires_at)
		SELECT $1, 1, $2, $3, 1, 'leased', t.d, t.d FROM t RETURNING id`, jobID, f.serverID, f.envID).Scan(&attemptID); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO releases(id, deployment_id, environment_id, config_digest, commit_sha, strategy, state)
		VALUES ('detail-rel','detail-dep',$1,decode(repeat('ab',32),'hex'),'0123456789abcdef0123456789abcdef01234567','stop_then_start','candidate')`, f.envID)
	exec(`INSERT INTO agent_job_attempt_execution(attempt_id, container_port, container_scheme, release_strategy, drain_seconds, health_policy_revision, cpu_millis, memory_bytes, restart_policy, environment_runtime_config_revision, health_gate_path, health_gate_host)
		VALUES ($1,8080,'http','stop_then_start',30,1,500,268435456,'unless-stopped',0,'/','app.internal')`, attemptID)
	exec(`INSERT INTO release_runtime_instances(release_id, job_id, attempt_id, fence, container_name, observed_state) VALUES ('detail-rel',$1,$2,1,'c-detail','running')`, jobID, attemptID)
	exec(`UPDATE agent_job_attempts SET state='failed', failure_class='transient', result_operation_id='op', result_digest=decode(repeat('00',32),'hex'), closed_at=clock_timestamp() WHERE id=$1`, attemptID)
	exec(`UPDATE agent_jobs SET state='retry_wait', available_at=clock_timestamp()+interval '1 minute' WHERE id=$1`, jobID)
	exec(`UPDATE releases SET image_digest=decode(repeat('0f',32),'hex') WHERE id='detail-rel'`)
	exec(`UPDATE releases SET state='ready' WHERE id='detail-rel'`)

	d, err := f.repo.GetDeploymentDetail(ctx, f.principal, "detail-dep")
	if err != nil {
		t.Fatal(err)
	}
	if d.Job.State != "retry_wait" || d.Job.AttemptCount != 1 || d.Job.EnvironmentID != f.envID || d.Deployment.JobID != jobID || d.Job.CancelRequestedAt != nil {
		t.Fatalf("job = %+v", d.Job)
	}
	if len(d.Attempts) != 1 || d.Attempts[0].State != "failed" || d.Attempts[0].FailureClass != "transient" || d.Attempts[0].Fence != 1 || d.Attempts[0].ReleaseStrategy != "stop_then_start" || d.Attempts[0].ClosedAt == nil {
		t.Fatalf("attempts = %+v", d.Attempts)
	}
	if d.Release == nil || d.Release.State != "ready" || d.Release.ImageDigest != strings.Repeat("0f", 32) || len(d.RuntimeInstances) != 1 || d.RuntimeInstances[0].ObservedState != "running" {
		t.Fatalf("release=%+v instances=%+v", d.Release, d.RuntimeInstances)
	}
	if d.RouteActiveReleaseID != "" || d.ReleaseIsActive {
		t.Fatalf("no route head yet: %q %v", d.RouteActiveReleaseID, d.ReleaseIsActive)
	}
	if len(d.JobEvents) != 1 || d.JobEvents[0].EventType != "enqueued" {
		t.Fatalf("job events = %+v", d.JobEvents)
	}

	// Route head pointing at this release -> active; pointing at another -> not.
	exec(`UPDATE releases SET state='active' WHERE id='detail-rel'`)
	exec(`INSERT INTO route_heads(environment_id, hostname) VALUES ($1,'detail.example.com')`, f.envID)
	exec(`UPDATE route_heads SET active_release_id='detail-rel', route_revision=1, updated_at=clock_timestamp() WHERE environment_id=$1`, f.envID)
	d, _ = f.repo.GetDeploymentDetail(ctx, f.principal, "detail-dep")
	if d.RouteActiveReleaseID != "detail-rel" || !d.ReleaseIsActive {
		t.Fatalf("active = %q %v", d.RouteActiveReleaseID, d.ReleaseIsActive)
	}
	swapActiveRelease(t, ctx, f, "other-dep", "other-rel")
	d, _ = f.repo.GetDeploymentDetail(ctx, f.principal, "detail-dep")
	if d.RouteActiveReleaseID != "other-rel" || d.ReleaseIsActive {
		t.Fatalf("superseded active = %q %v", d.RouteActiveReleaseID, d.ReleaseIsActive)
	}
	// A deployment with no release has Release nil and no instances.
	seedOpsDeployment(t, f.pool, f.ownerID, "project", f.envID, "bare-dep", at)
	d, err = f.repo.GetDeploymentDetail(ctx, f.principal, "bare-dep")
	if err != nil || d.Release != nil || len(d.RuntimeInstances) != 0 || len(d.Attempts) != 0 || d.ReleaseIsActive {
		t.Fatalf("bare detail = %+v, %v", d, err)
	}
}

// swapActiveRelease creates another deployment+release and, in one
// transaction (the route-head/active-release invariants are deferred), demotes
// the current active release to retained, activates the new one and points the
// route head at it.
func swapActiveRelease(t *testing.T, ctx context.Context, f opsFixture, depID, relID string) {
	t.Helper()
	seedOpsDeployment(t, f.pool, f.ownerID, "project", f.envID, depID, time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC))
	for _, q := range []string{
		`INSERT INTO releases(id, deployment_id, environment_id, image_digest, config_digest, commit_sha, strategy, state)
		 VALUES ('` + relID + `','` + depID + `','` + f.envID + `',decode(repeat('07',32),'hex'),decode(repeat('07',32),'hex'),'0123456789abcdef0123456789abcdef01234567','blue_green','candidate')`,
		`UPDATE releases SET state='ready' WHERE id='` + relID + `'`,
	} {
		if _, err := f.pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	for _, q := range []string{
		`UPDATE releases SET state='retained' WHERE environment_id='` + f.envID + `' AND state='active'`,
		`UPDATE releases SET state='active' WHERE id='` + relID + `'`,
		`UPDATE route_heads SET active_release_id='` + relID + `', route_revision=2, updated_at=clock_timestamp() WHERE environment_id='` + f.envID + `'`,
	} {
		if _, err = tx.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestOperationsReadDetailEventsBounded(t *testing.T) {
	f := newOpsFixture(t, 158)
	resetWebhookJobs(t, f.pool)
	ctx := context.Background()
	jobID := seedOpsDeployment(t, f.pool, f.ownerID, "project", "environment", "bounded-dep", time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC))
	if _, err := f.pool.Exec(ctx, `INSERT INTO agent_job_events(job_id,sequence,event_type) SELECT $1, g, 'reconciled' FROM generate_series(2,150) g`, jobID); err != nil {
		t.Fatal(err)
	}
	d, err := f.repo.GetDeploymentDetail(ctx, f.principal, "bounded-dep")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.JobEvents) != MaxDetailJobEvents || d.JobEvents[0].Sequence != 150 || d.JobEvents[MaxDetailJobEvents-1].Sequence != 51 {
		t.Fatalf("job events len=%d first=%d last=%d", len(d.JobEvents), d.JobEvents[0].Sequence, d.JobEvents[len(d.JobEvents)-1].Sequence)
	}
}

// TestOperationsReadTakesNoBlockingLocks holds an uncommitted writer on the
// owner's agent_jobs and deployments rows (a real UPDATE plus FOR UPDATE on
// every row) in another connection, and proves every read returns promptly
// and sees the last committed snapshot.
func TestOperationsReadTakesNoBlockingLocks(t *testing.T) {
	f := newOpsFixture(t, 160)
	resetWebhookJobs(t, f.pool)
	ctx := context.Background()
	jobID := seedOpsDeployment(t, f.pool, f.ownerID, "project", "environment", "locked-dep", time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC))

	writer, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(ctx) //nolint:errcheck
	if _, err = writer.Exec(ctx, `SELECT 1 FROM agent_jobs FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	if _, err = writer.Exec(ctx, `SELECT 1 FROM deployments FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	if _, err = writer.Exec(ctx, `UPDATE agent_jobs SET state='cancelled', updated_at=clock_timestamp() WHERE id=$1`, jobID); err != nil {
		t.Fatal(err)
	}
	// The writer also holds the session row lock the locking variant would need.
	if _, err = writer.Exec(ctx, `SELECT 1 FROM admin_sessions WHERE id=$1 FOR UPDATE`, f.principal.SessionID); err != nil {
		t.Fatal(err)
	}
	// Prove the writer really holds the row lock: a competing locker times out.
	probe, cancelProbe := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancelProbe()
	if _, err = f.pool.Exec(probe, `SELECT 1 FROM agent_jobs WHERE id=$1 FOR UPDATE`, jobID); err == nil {
		t.Fatal("control locker was not blocked; the writer holds no lock")
	}

	short, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	start := time.Now()
	list, err := f.repo.ListDeployments(short, f.principal, DeploymentFilter{}, DeploymentPage{})
	if err != nil || len(list.Items) != 1 || list.Items[0].JobState != "queued" {
		t.Fatalf("list under lock = %+v, %v", list.Items, err)
	}
	if _, err = f.repo.ListProjects(short, f.principal); err != nil {
		t.Fatalf("projects under lock: %v", err)
	}
	d, err := f.repo.GetDeploymentDetail(short, f.principal, "locked-dep")
	if err != nil || d.Job.State != "queued" {
		t.Fatalf("detail under lock = %+v, %v", d.Job, err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("reads took %v under a held writer", time.Since(start))
	}
}

func TestOperationsReadDeniesRevokedAndExpiredSessions(t *testing.T) {
	f := newOpsFixture(t, 162)
	resetWebhookJobs(t, f.pool)
	ctx := context.Background()
	seedOpsDeployment(t, f.pool, opsOwnerB, "project", "environment", "sess-dep", time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC))
	expectAll := func(label string, want error) {
		t.Helper()
		_, e1 := f.repo.ListProjects(ctx, f.principalB)
		_, e2 := f.repo.ListDeployments(ctx, f.principalB, DeploymentFilter{}, DeploymentPage{})
		_, e3 := f.repo.GetDeploymentDetail(ctx, f.principalB, "sess-dep")
		for i, e := range []error{e1, e2, e3} {
			if !errors.Is(e, want) || (want == nil && e != nil) {
				t.Fatalf("%s method %d err = %v, want %v", label, i, e, want)
			}
		}
	}
	expectAll("live", nil)
	reset := `UPDATE admin_sessions SET last_seen_at=clock_timestamp(), revoked_at=NULL, revoked_reason=NULL, idle_expires_at=clock_timestamp()+interval '1 hour', absolute_expires_at=clock_timestamp()+interval '2 hours' WHERE id='ops-sess-b'`
	for label, q := range map[string]string{
		"revoked":       `UPDATE admin_sessions SET revoked_at=clock_timestamp(), revoked_reason='logout' WHERE id='ops-sess-b'`,
		"idle expired":  `UPDATE admin_sessions SET created_at=clock_timestamp()-interval '3 hours', last_seen_at=clock_timestamp()-interval '2 hours', idle_expires_at=clock_timestamp()-interval '1 second', absolute_expires_at=clock_timestamp()-interval '1 second' WHERE id='ops-sess-b'`,
		"user disabled": `UPDATE users SET status='disabled' WHERE id='ops-user-b'`,
	} {
		if _, err := f.pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		expectAll(label, ErrServerAuthorizationDenied)
		if _, err := f.pool.Exec(ctx, reset); err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.Exec(ctx, `UPDATE users SET status='active' WHERE id='ops-user-b'`); err != nil {
			t.Fatal(err)
		}
	}
	expectAll("restored", nil)
	wrongRev := f.principalB
	wrongRev.AuthRevision = 2
	if _, err := f.repo.ListProjects(ctx, wrongRev); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("wrong revision = %v", err)
	}
}

func TestOperationsReadRouteHeadOwnerScoped(t *testing.T) {
	f := newOpsFixture(t, 164)
	resetWebhookJobs(t, f.pool)
	ctx := context.Background()
	at := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	seedOpsDeployment(t, f.pool, f.ownerID, "project", "environment", "share-a", at)
	seedOpsDeployment(t, f.pool, opsOwnerB, "project", "environment", "share-b", at)
	for _, q := range []string{
		`INSERT INTO releases(id, deployment_id, environment_id, image_digest, config_digest, commit_sha, strategy, state)
		 VALUES ('share-rel-a','share-a','environment',decode(repeat('07',32),'hex'),decode(repeat('07',32),'hex'),'0123456789abcdef0123456789abcdef01234567','blue_green','candidate')`,
		`UPDATE releases SET state='ready' WHERE id='share-rel-a'`,
		`UPDATE releases SET state='active' WHERE id='share-rel-a'`,
		`INSERT INTO route_heads(environment_id, hostname) VALUES ('environment','share.example.com')`,
		`UPDATE route_heads SET active_release_id='share-rel-a', route_revision=1, updated_at=clock_timestamp() WHERE environment_id='environment'`,
	} {
		if _, err := f.pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	a, err := f.repo.GetDeploymentDetail(ctx, f.principal, "share-a")
	if err != nil || a.RouteActiveReleaseID != "share-rel-a" || !a.ReleaseIsActive {
		t.Fatalf("A = %q %v %v", a.RouteActiveReleaseID, a.ReleaseIsActive, err)
	}
	b, err := f.repo.GetDeploymentDetail(ctx, f.principalB, "share-b")
	if err != nil || b.RouteActiveReleaseID != "" || b.ReleaseIsActive {
		t.Fatalf("B leaked A's active release: %q %v %v", b.RouteActiveReleaseID, b.ReleaseIsActive, err)
	}
}
