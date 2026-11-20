//go:build integration

package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const ovCommit = "0123456789abcdef0123456789abcdef01234567"

func ovExec(t *testing.T, pool *pgxpool.Pool, q string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// ovBind points the owner's binding of project at (env, server).
func ovBind(t *testing.T, pool *pgxpool.Pool, owner, project, env, server string) {
	t.Helper()
	ovExec(t, pool, `UPDATE github_project_bindings SET deploy_ref='refs/heads/main', environment_id=$3, server_id=$4 WHERE owner_id=$1 AND project_id=$2`, owner, project, env, server)
}

func ovRuntimeConfig(t *testing.T, pool *pgxpool.Pool, env, strategy string) {
	t.Helper()
	ovExec(t, pool, `INSERT INTO environment_runtime_configs(environment_id, container_port, container_scheme, release_strategy, drain_seconds, cpu_millis, memory_bytes, restart_policy, health_policy_revision, health_gate_host) VALUES ($1,8080,'http',$2,30,500,268435456,'unless-stopped',1,'app.internal') ON CONFLICT (environment_id) DO UPDATE SET release_strategy=EXCLUDED.release_strategy`, env, strategy)
}

// ovRelease creates a deployment and release for env and drives it to state
// (candidate or ready).
func ovRelease(t *testing.T, pool *pgxpool.Pool, env, unique, state string) string {
	t.Helper()
	dep, rel := "ov-dep-"+unique, "ov-rel-"+unique
	ovExec(t, pool, `INSERT INTO deployments(id, project_id, environment_id, server_id, commit_sha, schema_version, status, created_at, updated_at, revision) VALUES ($1,'ov-proj',$2,'server',$3,1,'healthy',clock_timestamp(),clock_timestamp(),1)`, dep, env, ovCommit)
	ovExec(t, pool, `INSERT INTO releases(id, deployment_id, environment_id, image_digest, config_digest, commit_sha, strategy, state) VALUES ($1,$2,$3,decode(repeat('07',32),'hex'),decode(repeat('07',32),'hex'),$4,'blue_green','candidate')`, rel, dep, env, ovCommit)
	if state == "ready" {
		ovExec(t, pool, `UPDATE releases SET state='ready' WHERE id=$1`, rel)
	}
	return rel
}

// ovPointRoute creates the route head for env with hostname and points it at
// the active release.
// When owner != "" the release's deployment gets a deployment_execution job
// of that owner (release-level ownership).
func ovPointRoute(t *testing.T, ctx context.Context, pool *pgxpool.Pool, owner, env, hostname, unique string) string {
	t.Helper()
	ovExec(t, pool, `INSERT INTO route_heads(environment_id, hostname, route_revision) VALUES ($1,$2,0)`, env, hostname)
	rel := insertActiveRelease(t, ctx, pool, env, unique)
	if owner != "" {
		ovOwnDeployment(t, pool, owner, env, "deploy-"+unique)
	}
	ovExec(t, pool, `UPDATE route_heads SET active_release_id=$2, route_revision=route_revision+1, updated_at=clock_timestamp() WHERE environment_id=$1`, env, rel)
	return rel
}

// ovOwnDeployment gives an existing deployment a deployment_execution job of owner.
func ovOwnDeployment(t *testing.T, pool *pgxpool.Pool, owner, env, depID string) {
	t.Helper()
	ovExec(t, pool, `INSERT INTO agent_jobs(schema_version,job_kind,owner_id,project_id,environment_id,server_id,deployment_id,repository_id,full_ref,commit_sha,priority,available_at,idempotency_key,payload_digest,state)
		VALUES (1,'deployment_execution',$1,'ov-proj',$2,'server',$3,201,'refs/heads/main',$4,0,clock_timestamp(),$3,decode(repeat('00',32),'hex'),'succeeded')`, owner, env, depID, ovCommit)
	ovExec(t, pool, `INSERT INTO agent_job_events(job_id,sequence,event_type) SELECT id,1,'enqueued' FROM agent_jobs WHERE deployment_id=$1`, depID)
}

// ovFailedAttempt gives jobID a failed attempt number n (fence n) with class.
func ovFailedAttempt(t *testing.T, pool *pgxpool.Pool, jobID int64, env string, n int, class string, fenceOpt ...int) int64 {
	t.Helper()
	fence := n
	if len(fenceOpt) > 0 {
		fence = fenceOpt[0]
	}
	ctx := context.Background()
	ovExec(t, pool, `INSERT INTO environment_slots(environment_id) VALUES ($1) ON CONFLICT DO NOTHING`, env)
	ovExec(t, pool, `UPDATE environment_slots SET fence=$2::bigint WHERE environment_id=$1`, env, fence)
	ovExec(t, pool, `UPDATE agent_jobs SET state='leased', attempt_count=$2::int WHERE id=$1`, jobID, n)
	var id int64
	if err := pool.QueryRow(ctx, `WITH t AS (SELECT clock_timestamp() + interval '90 seconds' AS d)
		INSERT INTO agent_job_attempts(job_id, attempt_number, server_id, environment_id, fence, state, lease_expires_at, authority_expires_at)
		SELECT $1,$2::smallint,'server',$3,$4::bigint,'leased',t.d,t.d FROM t RETURNING id`, jobID, n, env, fence).Scan(&id); err != nil {
		t.Fatal(err)
	}
	ovExec(t, pool, `UPDATE agent_job_attempts SET state='failed', failure_class=$2, result_operation_id='op', result_digest=decode(repeat('00',32),'hex'), closed_at=clock_timestamp() WHERE id=$1`, id, class)
	ovExec(t, pool, `UPDATE agent_jobs SET state='retry_wait', available_at=clock_timestamp()+interval '1 minute' WHERE id=$1`, jobID)
	return id
}

var ovSeq int64

func ovChunk(t *testing.T, pool *pgxpool.Pool, attemptID int64, stream, body string) {
	t.Helper()
	ovSeq++
	ovExec(t, pool, `INSERT INTO deployment_log_chunks(attempt_id, stream, writer_epoch, seq, body) VALUES ($1,$2,1,$3,$4)`, attemptID, stream, ovSeq, body)
}

func ovRow(t *testing.T, rows []EnvironmentOverview, env string) EnvironmentOverview {
	t.Helper()
	for _, r := range rows {
		if r.EnvironmentID == env {
			return r
		}
	}
	t.Fatalf("no overview row for %q in %+v", env, rows)
	return EnvironmentOverview{}
}

func TestEnvironmentOverviewEmpty(t *testing.T) {
	f := newOpsFixture(t, 160)
	resetWebhookJobs(t, f.pool)
	ovExec(t, f.pool, `DELETE FROM github_project_bindings WHERE owner_id=$1`, opsOwnerB)
	rows, err := f.repo.ListEnvironmentOverview(context.Background(), f.principalB)
	if err != nil || rows == nil || len(rows) != 0 {
		t.Fatalf("rows = %v, %v", rows, err)
	}
}

func TestEnvironmentOverviewBoundOnlyEnvironmentHasNoRouteConfigFields(t *testing.T) {
	f := newOpsFixture(t, 162)
	ctx := context.Background()
	resetWebhookJobs(t, f.pool)
	ovBind(t, f.pool, f.ownerID, "project", "env-bound", "server")
	// Rows for the same id exist (another owner's history) but A has no job.
	ovRuntimeConfig(t, f.pool, "env-bound", "blue_green")
	ovPointRoute(t, ctx, f.pool, "", "env-bound", "bound.example.com", "bound")

	rows, err := f.repo.ListEnvironmentOverview(ctx, f.principal)
	if err != nil {
		t.Fatal(err)
	}
	r := ovRow(t, rows, "env-bound")
	if r.Owned || !r.Bound || !r.BindingActive || r.ProjectID != "project" || r.DeployRef != "refs/heads/main" || r.ServerID != "server" {
		t.Fatalf("bound row = %+v", r)
	}
	if r.Hostname != "" || r.RouteRevision != 0 || r.ActiveReleaseID != "" || r.ActiveCommit != "" || !r.ActiveSince.IsZero() ||
		r.Strategy != "" || r.ReconciliationRequired || r.ReconcilingAttemptID != 0 || r.RollbackTargetReleaseID != "" || r.Latest != nil {
		t.Fatalf("bound-only row leaks route/config fields: %+v", r)
	}
}

func TestEnvironmentOverviewCrossOwnerCollision(t *testing.T) {
	f := newOpsFixture(t, 164)
	ctx := context.Background()
	resetWebhookJobs(t, f.pool)
	ovExec(t, f.pool, `INSERT INTO servers(id, owner_id, name, protocol_version, status) VALUES ('server-b', $1, 'b-box', 1, 'active') ON CONFLICT (id) DO NOTHING`, opsOwnerB)
	// Owner B fully owns env-x: jobs, route head, config, active release.
	seedOpsDeployment(t, f.pool, opsOwnerB, "project", "env-x", "b-x-dep", time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	ovRuntimeConfig(t, f.pool, "env-x", "blue_green")
	activeB := ovPointRoute(t, ctx, f.pool, opsOwnerB, "env-x", "b-secret.example.com", "x")
	ovRelease(t, f.pool, "env-x", "x-ready", "ready")
	// B's slot for env-x is flagged for reconciliation by one of B's attempts.
	var bJob int64
	if err := f.pool.QueryRow(ctx, `SELECT id FROM agent_jobs WHERE deployment_id='b-x-dep'`).Scan(&bJob); err != nil {
		t.Fatal(err)
	}
	bAttempt := ovFailedAttempt(t, f.pool, bJob, "env-x", 1, "transient")
	asReplica(t, f.pool, func(tx pgx.Tx) {
		if _, err := tx.Exec(ctx, `UPDATE environment_slots SET reconciliation_required=true, reconciliation_attempt_id=$1 WHERE environment_id='env-x'`, bAttempt); err != nil {
			t.Fatal(err)
		}
	})
	// Owner A only has a binding on env-x, aimed at B's server id.
	ovBind(t, f.pool, f.ownerID, "project", "env-x", "server-b")

	rows, err := f.repo.ListEnvironmentOverview(ctx, f.principal)
	if err != nil {
		t.Fatal(err)
	}
	a := ovRow(t, rows, "env-x")
	if a.Owned || !a.Bound {
		t.Fatalf("A row = %+v", a)
	}
	if a.Hostname != "" || a.RouteRevision != 0 || a.ActiveReleaseID != "" || a.ActiveCommit != "" || !a.ActiveSince.IsZero() ||
		a.Strategy != "" || a.ReconciliationRequired || a.ReconcilingAttemptID != 0 ||
		a.RollbackTargetReleaseID != "" || a.RollbackTargetCommit != "" || a.Latest != nil || a.LatestFailureClass != "" || a.LatestFailureReason != "" {
		t.Fatalf("CROSS-OWNER LEAK in A's row: %+v", a)
	}
	if a.ServerID != "" || a.ServerName != "" || a.ServerStatus != "" || !a.LastHeartbeatAt.IsZero() {
		t.Fatalf("B's server shown to A: %+v", a)
	}
	// Positive control: B sees everything on env-x.
	brows, err := f.repo.ListEnvironmentOverview(ctx, f.principalB)
	if err != nil {
		t.Fatal(err)
	}
	b := ovRow(t, brows, "env-x")
	if !b.ReconciliationRequired || b.ReconcilingAttemptID != bAttempt {
		t.Fatalf("B slot fields = %+v", b)
	}
	if !b.Owned || b.Hostname != "b-secret.example.com" || b.ActiveReleaseID != activeB || b.Strategy != "blue_green" || b.ActiveCommit != ovCommit || b.RollbackTargetReleaseID != "" {
		t.Fatalf("B row = %+v", b)
	}
}

func TestEnvironmentOverviewOwnedEnvironmentAllFields(t *testing.T) {
	f := newOpsFixture(t, 166)
	ctx := context.Background()
	attemptID := f.leaseJob(t, 1, 600) // owner A, environment "environment", runtime config stop_then_start
	f.insertRouteHead(t, "app.example.com")
	rel := insertActiveRelease(t, ctx, f.pool, f.envID, "full")
	ovOwnDeployment(t, f.pool, f.ownerID, f.envID, "deploy-full")
	ovExec(t, f.pool, `UPDATE route_heads SET active_release_id=$2, route_revision=route_revision+1, updated_at=clock_timestamp() WHERE environment_id=$1`, f.envID, rel)
	asReplica(t, f.pool, func(tx pgx.Tx) {
		if _, err := tx.Exec(ctx, `UPDATE environment_slots SET reconciliation_required=true, reconciliation_attempt_id=$2 WHERE environment_id=$1`, f.envID, attemptID); err != nil {
			t.Fatal(err)
		}
	})
	ovExec(t, f.pool, `UPDATE servers SET last_heartbeat_at=clock_timestamp() WHERE id='server'`)

	rows, err := f.repo.ListEnvironmentOverview(ctx, f.principal)
	if err != nil {
		t.Fatal(err)
	}
	r := ovRow(t, rows, f.envID)
	if !r.Owned || !r.Bound || r.ProjectID != "project" || r.ServerID != "server" || r.ServerName != "server" || r.ServerStatus != "active" || r.LastHeartbeatAt.IsZero() {
		t.Fatalf("identity/server = %+v", r)
	}
	if r.Hostname != "app.example.com" || r.RouteRevision != 1 || r.ActiveReleaseID != rel || r.ActiveCommit != ovCommit || r.ActiveSince.IsZero() ||
		r.Strategy != "stop_then_start" || !r.ReconciliationRequired || r.ReconcilingAttemptID != attemptID {
		t.Fatalf("route/config/reconciliation = %+v", r)
	}
	if r.Latest == nil || r.Latest.DeploymentID != "deploy-full" || r.Latest.JobState != "succeeded" {
		t.Fatalf("latest = %+v", r.Latest)
	}
	if r.RollbackTargetReleaseID != "" {
		t.Fatalf("no rollback candidate expected: %+v", r)
	}
}

func TestEnvironmentOverviewLatestDeploymentIsNewestByCreatedAtThenID(t *testing.T) {
	f := newOpsFixture(t, 168)
	ctx := context.Background()
	resetWebhookJobs(t, f.pool)
	at := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	seedOpsDeployment(t, f.pool, f.ownerID, "project", "env-l", "l-old", at.Add(-time.Hour))
	seedOpsDeployment(t, f.pool, f.ownerID, "project", "env-l", "l-a", at)
	seedOpsDeployment(t, f.pool, f.ownerID, "project", "env-l", "l-b", at) // same created_at, larger id wins
	rows, err := f.repo.ListEnvironmentOverview(ctx, f.principal)
	if err != nil {
		t.Fatal(err)
	}
	if r := ovRow(t, rows, "env-l"); r.Latest == nil || r.Latest.DeploymentID != "l-b" {
		t.Fatalf("latest = %+v", r.Latest)
	}
}

func TestEnvironmentOverviewRevokedSessionDenied(t *testing.T) {
	f := newOpsFixture(t, 172)
	ovExec(t, f.pool, `UPDATE admin_sessions SET revoked_at=clock_timestamp(), revoked_reason='logout' WHERE id='ops-sess-b'`)
	if _, err := f.repo.ListEnvironmentOverview(context.Background(), f.principalB); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("err = %v", err)
	}
	var nilRepo *OperationsReadRepository
	if _, err := nilRepo.ListEnvironmentOverview(context.Background(), f.principalB); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("nil repo err = %v", err)
	}
}

func TestEnvironmentOverviewOrderingAndCap(t *testing.T) {
	f := newOpsFixture(t, 174)
	ctx := context.Background()
	resetWebhookJobs(t, f.pool)
	ovBind(t, f.pool, f.ownerID, "project", "env-m", "server")
	seedOpsDeployment(t, f.pool, f.ownerID, "project", "env-m", "o-1", time.Now().UTC()) // binding AND job: one row, binding preferred
	seedOpsDeployment(t, f.pool, f.ownerID, "project", "env-a", "o-2", time.Now().UTC())
	seedOpsDeployment(t, f.pool, f.ownerID, "project", "env-z", "o-3", time.Now().UTC())
	rows, err := f.repo.ListEnvironmentOverview(ctx, f.principal)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range rows {
		ids = append(ids, r.EnvironmentID)
	}
	if fmt.Sprint(ids) != "[env-a env-m env-z]" {
		t.Fatalf("ids = %v", ids)
	}
	if m := ovRow(t, rows, "env-m"); !m.Bound || !m.Owned {
		t.Fatalf("env-m = %+v", m)
	}

	ovExec(t, f.pool, `INSERT INTO deployments(id, project_id, environment_id, server_id, commit_sha, schema_version, status, created_at, updated_at, revision)
		SELECT 'cap-dep-'||g, 'project', 'cap-env-'||lpad(g::text,4,'0'), 'server', $1, 1, 'queued', clock_timestamp(), clock_timestamp(), 1 FROM generate_series(1,$2) g`, ovCommit, MaxEnvironmentsListed+5)
	ovExec(t, f.pool, `INSERT INTO agent_jobs(schema_version,job_kind,owner_id,project_id,environment_id,server_id,deployment_id,repository_id,full_ref,commit_sha,priority,available_at,idempotency_key,payload_digest,state)
		SELECT 1,'deployment_execution',$1,'project','cap-env-'||lpad(g::text,4,'0'),'server','cap-dep-'||g,201,'refs/heads/main',$2,0,clock_timestamp(),'cap-dep-'||g,decode(repeat('00',32),'hex'),'queued' FROM generate_series(1,$3) g`, f.ownerID, ovCommit, MaxEnvironmentsListed+5)
	ovExec(t, f.pool, `INSERT INTO agent_job_events(job_id,sequence,event_type) SELECT id,1,'enqueued' FROM agent_jobs WHERE deployment_id LIKE 'cap-dep-%'`)
	rows, err = f.repo.ListEnvironmentOverview(ctx, f.principal)
	if err != nil || len(rows) != MaxEnvironmentsListed {
		t.Fatalf("cap: len=%d err=%v", len(rows), err)
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].EnvironmentID <= rows[i-1].EnvironmentID {
			t.Fatalf("not ordered at %d: %q after %q", i, rows[i].EnvironmentID, rows[i-1].EnvironmentID)
		}
	}
}

const (
	ovHealthFailed = "health gate: failed (reason=container_unhealthy class=transient)"
	ovHealthProbe  = "health gate: last probe: GET /healthz -> 503 Service Unavailable"
)

func TestFailureReasonClassAndProbeAcrossReadModels(t *testing.T) {
	f := newOpsFixture(t, 176)
	ctx := context.Background()
	resetWebhookJobs(t, f.pool)
	at := time.Date(2026, 9, 3, 9, 0, 0, 0, time.UTC)
	jobID := seedOpsDeployment(t, f.pool, f.ownerID, "project", f.envID, "fail-dep", at)
	a1 := ovFailedAttempt(t, f.pool, jobID, f.envID, 1, "permanent_configuration")
	ovChunk(t, f.pool, a1, "health", "health gate: failed (reason=deadline_exceeded)")
	a2 := ovFailedAttempt(t, f.pool, jobID, f.envID, 2, "transient")
	ovChunk(t, f.pool, a2, "build", "health gate: failed (reason=deadline_exceeded)") // wrong stream: ignored
	ovChunk(t, f.pool, a2, "health", "health gate: failed (reason=container_not_running)")
	ovChunk(t, f.pool, a2, "health", ovHealthFailed) // newest wins
	ovChunk(t, f.pool, a2, "health", ovHealthProbe)

	check := func(where string, d DeploymentSummary) {
		t.Helper()
		if d.FailureClass != "transient" || d.FailureReason != "container_unhealthy" {
			t.Fatalf("%s: class=%q reason=%q (latest failed attempt must win)", where, d.FailureClass, d.FailureReason)
		}
	}
	list, err := f.repo.ListDeployments(ctx, f.principal, DeploymentFilter{}, DeploymentPage{})
	if err != nil || len(list.Items) != 1 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	check("ListDeployments", list.Items[0])
	projects, err := f.repo.ListProjects(ctx, f.principal)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range projects {
		if p.ProjectID == "project" && p.LatestDeployment != nil {
			check("ListProjects", *p.LatestDeployment)
			goto detail
		}
	}
	t.Fatal("project latest deployment missing")
detail:
	d, err := f.repo.GetDeploymentDetail(ctx, f.principal, "fail-dep")
	if err != nil {
		t.Fatal(err)
	}
	check("GetDeploymentDetail", d.Deployment)
	if len(d.Attempts) != 2 {
		t.Fatalf("attempts = %+v", d.Attempts)
	}
	if got := d.Attempts[0]; got.AttemptNumber != 2 || got.FailureReason != "container_unhealthy" || got.FailureProbe != "GET /healthz -> 503 Service Unavailable" {
		t.Fatalf("attempt 2 = %+v", got)
	}
	if got := d.Attempts[1]; got.AttemptNumber != 1 || got.FailureReason != "deadline_exceeded" || got.FailureProbe != "" {
		t.Fatalf("attempt 1 = %+v", got)
	}
	rows, err := f.repo.ListEnvironmentOverview(ctx, f.principal)
	if err != nil {
		t.Fatal(err)
	}
	r := ovRow(t, rows, f.envID)
	if r.LatestFailureClass != "transient" || r.LatestFailureReason != "container_unhealthy" || r.Latest == nil {
		t.Fatalf("overview failure = %+v", r)
	}
	check("overview.Latest", *r.Latest)

	// Owner B cannot read A's failure detail.
	if _, err = f.repo.GetDeploymentDetail(ctx, f.principalB, "fail-dep"); !errors.Is(err, ErrOperationsNotFound) {
		t.Fatalf("foreign detail err = %v", err)
	}
}

func TestFailureReasonNotAllowlistedOrPurgedFallsBackToClass(t *testing.T) {
	f := newOpsFixture(t, 178)
	ctx := context.Background()
	resetWebhookJobs(t, f.pool)
	at := time.Date(2026, 9, 3, 9, 0, 0, 0, time.UTC)
	jobHostile := seedOpsDeployment(t, f.pool, f.ownerID, "project", "env-h", "hostile-dep", at)
	ah := ovFailedAttempt(t, f.pool, jobHostile, "env-h", 1, "transient")
	ovChunk(t, f.pool, ah, "health", "health gate: failed (reason=container_unhealthy)")
	ovChunk(t, f.pool, ah, "health", "health gate: failed (reason=disk_full password=hunter2)") // newest, hostile: no fallback to older
	ovChunk(t, f.pool, ah, "health", "health gate: last probe: ignored by reason")              // sanity: probe line is not a reason
	jobPurged := seedOpsDeployment(t, f.pool, f.ownerID, "project", "env-p", "purged-dep", at.Add(time.Minute))
	ovFailedAttempt(t, f.pool, jobPurged, "env-p", 1, "authorization") // no log lines at all

	list, err := f.repo.ListDeployments(ctx, f.principal, DeploymentFilter{}, DeploymentPage{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]DeploymentSummary{}
	for _, it := range list.Items {
		got[it.DeploymentID] = it
	}
	if h := got["hostile-dep"]; h.FailureClass != "transient" || h.FailureReason != "" {
		t.Fatalf("hostile = %+v", h)
	}
	if p := got["purged-dep"]; p.FailureClass != "authorization" || p.FailureReason != "" {
		t.Fatalf("purged = %+v", p)
	}
	d, err := f.repo.GetDeploymentDetail(ctx, f.principal, "hostile-dep")
	if err != nil || d.Attempts[0].FailureReason != "" || d.Attempts[0].FailureProbe != "ignored by reason" {
		t.Fatalf("hostile detail = %+v, %v", d.Attempts, err)
	}
	// A deployment without a failed attempt has no failure fields.
	seedOpsDeployment(t, f.pool, f.ownerID, "project", "env-ok", "ok-dep", at.Add(time.Hour))
	list, _ = f.repo.ListDeployments(ctx, f.principal, DeploymentFilter{EnvironmentID: "env-ok"}, DeploymentPage{})
	if len(list.Items) != 1 || list.Items[0].FailureClass != "" || list.Items[0].FailureReason != "" {
		t.Fatalf("ok = %+v", list.Items)
	}
}

func TestDeploymentDetailActivationSummary(t *testing.T) {
	f := newOpsFixture(t, 180)
	ctx := context.Background()
	resetWebhookJobs(t, f.pool)
	at := time.Date(2026, 9, 3, 9, 0, 0, 0, time.UTC)
	jobID := seedOpsDeployment(t, f.pool, f.ownerID, "project", f.envID, "act-dep", at)
	attemptID := ovFailedAttempt(t, f.pool, jobID, f.envID, 1, "transient")

	d, err := f.repo.GetDeploymentDetail(ctx, f.principal, "act-dep")
	if err != nil || d.ActivationSummary != "" {
		t.Fatalf("no release: %q, %v", d.ActivationSummary, err)
	}
	ovExec(t, f.pool, `INSERT INTO releases(id, deployment_id, environment_id, config_digest, commit_sha, strategy, state) VALUES ('act-rel','act-dep',$1,decode(repeat('ab',32),'hex'),$2,'stop_then_start','candidate')`, f.envID, ovCommit)
	d, err = f.repo.GetDeploymentDetail(ctx, f.principal, "act-dep")
	if err != nil || d.ActivationSummary != "" {
		t.Fatalf("release without operations: %q, %v", d.ActivationSummary, err)
	}
	asReplica(t, f.pool, func(tx pgx.Tx) {
		for i, s := range []string{"reason=deadline_exceeded last_probe=old", "reason=container_unhealthy last_probe=connection refused"} {
			if _, err := tx.Exec(ctx, `INSERT INTO release_operations(release_id, job_id, attempt_id, fence, kind, digest, state, outcome_summary, closed_at)
				VALUES ('act-rel',$1,$2,1,'prepare',decode(repeat($3::text,32),'hex'),'failed',$4,clock_timestamp())`, jobID, attemptID, fmt.Sprintf("0%d", i+1), s); err != nil {
				t.Fatal(err)
			}
		}
	})
	d, err = f.repo.GetDeploymentDetail(ctx, f.principal, "act-dep")
	if err != nil || d.ActivationSummary != "reason=container_unhealthy last_probe=connection refused" {
		t.Fatalf("summary = %q, %v", d.ActivationSummary, err)
	}
	if len(d.ActivationSummary) > 512 {
		t.Fatalf("summary too long: %d", len(d.ActivationSummary))
	}
	// Over-long summaries (the column allows 2048) are cut to 512 bytes on a rune boundary.
	asReplica(t, f.pool, func(tx pgx.Tx) {
		if _, err := tx.Exec(ctx, `INSERT INTO release_operations(release_id, job_id, attempt_id, fence, kind, digest, state, outcome_summary, closed_at)
			VALUES ('act-rel',$1,$2,1,'prepare',decode(repeat('03',32),'hex'),'failed',$3,clock_timestamp())`, jobID, attemptID, strings.Repeat("€", 400)); err != nil {
			t.Fatal(err)
		}
	})
	d, _ = f.repo.GetDeploymentDetail(ctx, f.principal, "act-dep")
	if len(d.ActivationSummary) != 510 {
		t.Fatalf("truncated summary = %d bytes", len(d.ActivationSummary))
	}
	// Another owner's deployment detail never carries it.
	if _, err = f.repo.GetDeploymentDetail(ctx, f.principalB, "act-dep"); !errors.Is(err, ErrOperationsNotFound) {
		t.Fatalf("foreign = %v", err)
	}
}

// TestEnvironmentOverviewExplainScale is a throwaway measurement helper:
// AUTODEPLOY_OVERVIEW_EXPLAIN=1 seeds 50 environments / 500 jobs and prints
// EXPLAIN (ANALYZE) of the overview statement.
func TestEnvironmentOverviewExplainScale(t *testing.T) {
	if os.Getenv("AUTODEPLOY_OVERVIEW_EXPLAIN") != "1" {
		t.Skip("set AUTODEPLOY_OVERVIEW_EXPLAIN=1")
	}
	f := newOpsFixture(t, 182)
	ctx := context.Background()
	resetWebhookJobs(t, f.pool)
	ovExec(t, f.pool, `INSERT INTO deployments(id, project_id, environment_id, server_id, commit_sha, schema_version, status, created_at, updated_at, revision)
		SELECT 'x-dep-'||g, 'project', 'x-env-'||lpad((g%50)::text,2,'0'), 'server', $1, 1, 'queued', clock_timestamp()+g*interval '1 second', clock_timestamp()+g*interval '1 second', 1 FROM generate_series(1,500) g`, ovCommit)
	ovExec(t, f.pool, `INSERT INTO agent_jobs(schema_version,job_kind,owner_id,project_id,environment_id,server_id,deployment_id,repository_id,full_ref,commit_sha,priority,available_at,idempotency_key,payload_digest,state)
		SELECT 1,'deployment_execution',$1,'project','x-env-'||lpad((g%50)::text,2,'0'),'server','x-dep-'||g,201,'refs/heads/main',$2,0,clock_timestamp(),'x-dep-'||g,decode(repeat('00',32),'hex'),'queued' FROM generate_series(1,500) g`, f.ownerID, ovCommit)
	ovExec(t, f.pool, `INSERT INTO agent_job_events(job_id,sequence,event_type) SELECT id,1,'enqueued' FROM agent_jobs WHERE deployment_id LIKE 'x-dep-%'`)
	for e := 0; e < 50; e++ {
		env := fmt.Sprintf("x-env-%02d", e)
		ovExec(t, f.pool, `INSERT INTO environment_slots(environment_id) VALUES ($1)`, env)
		ovRuntimeConfig(t, f.pool, env, "blue_green")
		ovPointRoute(t, ctx, f.pool, f.ownerID, env, fmt.Sprintf("x%02d.example.com", e), fmt.Sprintf("xs%02d", e))
	}
	// 500 failed attempts, each with a health line.
	ovExec(t, f.pool, `SELECT 1`)
	var jobs []int64
	rows, err := f.pool.Query(ctx, `SELECT id FROM agent_jobs WHERE deployment_id LIKE 'x-dep-%' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		jobs = append(jobs, id)
	}
	rows.Close()
	for i, id := range jobs {
		env := fmt.Sprintf("x-env-%02d", (i+1)%50)
		a := ovFailedAttempt(t, f.pool, id, env, 1, "transient", 1+i/50)
		ovChunk(t, f.pool, a, "health", ovHealthFailed)
	}
	ovExec(t, f.pool, `ANALYZE`)
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	er, err := tx.Query(ctx, `EXPLAIN (ANALYZE, BUFFERS) `+environmentOverviewSQL, f.ownerID, MaxEnvironmentsListed, inFlightStatuses())
	if err != nil {
		t.Fatal(err)
	}
	defer er.Close()
	for er.Next() {
		var line string
		if err = er.Scan(&line); err != nil {
			t.Fatal(err)
		}
		fmt.Println(line)
	}
}

func TestFailureOnlyWhenLatestAttemptFailed(t *testing.T) {
	f := newOpsFixture(t, 184)
	ctx := context.Background()
	resetWebhookJobs(t, f.pool)
	at := time.Date(2026, 9, 3, 9, 0, 0, 0, time.UTC)
	jobID := seedOpsDeployment(t, f.pool, f.ownerID, "project", f.envID, "retry-dep", at)
	a1 := ovFailedAttempt(t, f.pool, jobID, f.envID, 1, "transient")
	ovChunk(t, f.pool, a1, "health", ovHealthFailed)
	// Attempt 2 (fence 2) recovers.
	ovExec(t, f.pool, `UPDATE environment_slots SET fence=2 WHERE environment_id=$1`, f.envID)
	ovExec(t, f.pool, `UPDATE agent_jobs SET state='leased', attempt_count=2 WHERE id=$1`, jobID)
	var a2 int64
	if err := f.pool.QueryRow(ctx, `WITH t AS (SELECT clock_timestamp() + interval '90 seconds' AS d)
		INSERT INTO agent_job_attempts(job_id, attempt_number, server_id, environment_id, fence, state, lease_expires_at, authority_expires_at)
		SELECT $1,2,'server',$2,2,'leased',t.d,t.d FROM t RETURNING id`, jobID, f.envID).Scan(&a2); err != nil {
		t.Fatal(err)
	}
	ovExec(t, f.pool, `UPDATE agent_job_attempts SET state='succeeded', result_operation_id='op', result_digest=decode(repeat('00',32),'hex'), closed_at=clock_timestamp() WHERE id=$1`, a2)

	list, err := f.repo.ListDeployments(ctx, f.principal, DeploymentFilter{}, DeploymentPage{})
	if err != nil || len(list.Items) != 1 || list.Items[0].FailureClass != "" || list.Items[0].FailureReason != "" {
		t.Fatalf("list = %+v, %v", list.Items, err)
	}
	d, err := f.repo.GetDeploymentDetail(ctx, f.principal, "retry-dep")
	if err != nil || d.Deployment.FailureClass != "" || d.Deployment.FailureReason != "" {
		t.Fatalf("detail = %+v, %v", d.Deployment, err)
	}
	// Per-attempt values are unchanged: attempt 1 keeps its reason.
	if len(d.Attempts) != 2 || d.Attempts[1].FailureReason != "container_unhealthy" || d.Attempts[0].FailureReason != "" {
		t.Fatalf("attempts = %+v", d.Attempts)
	}
	rows, err := f.repo.ListEnvironmentOverview(ctx, f.principal)
	if err != nil {
		t.Fatal(err)
	}
	if r := ovRow(t, rows, f.envID); r.LatestFailureClass != "" || r.LatestFailureReason != "" || r.Latest.FailureClass != "" {
		t.Fatalf("overview = %+v", r)
	}
}
