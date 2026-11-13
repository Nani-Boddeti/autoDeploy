//go:build integration

package postgres

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"autodeploy/internal/deployment"
	"autodeploy/internal/queue"
	"autodeploy/internal/runtime/dockerid"

	"github.com/jackc/pgx/v5"
)

const rollbackNonce = "rollback-nonce-0123456789"

// rollbackImageDigest is the write-once image digest every historic release of
// this file carries (decode(repeat('11',32))).
func rollbackImageDigest() [32]byte {
	var d [32]byte
	for i := range d {
		d[i] = 0x11
	}
	return d
}

// rollbackWorld: relA is retained (the rollback target), relB is active and
// the route head points at it. Both were activated by deployment jobs; the
// fixture's own queued job is cancelled so it is not an in-flight deployment.
type rollbackWorld struct {
	leaseFixture
	cred               [32]byte
	repo               *WebhookJobRepository
	lease              *AgentLeaseRepository
	relA, relB         string
	attemptA, attemptB int64
}

func (f leaseFixture) walkDeployment(t *testing.T, id string, statuses ...deployment.Status) {
	t.Helper()
	ctx := context.Background()
	repo := NewDeploymentRepository(f.pool)
	for _, target := range statuses {
		dep, err := repo.GetByID(ctx, deployment.ID(id))
		if err != nil {
			t.Fatal(err)
		}
		var now time.Time
		if err := f.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			t.Fatal(err)
		}
		next, err := dep.Transition(target, now)
		if err != nil {
			t.Fatalf("transition %s to %s: %v", id, target, err)
		}
		if err := repo.Save(ctx, next); err != nil {
			t.Fatalf("save %s %s: %v", id, target, err)
		}
	}
}

var healthyPath = []deployment.Status{deployment.StatusAssigned, deployment.StatusFetching, deployment.StatusBuilding, deployment.StatusStarting, deployment.StatusHealthChecking, deployment.StatusActivating, deployment.StatusHealthy}

// insertHistoricRelease gives releaseID a healthy deployment, the release row in
// state, and (activated) a succeeded activate operation by a deployment job
// plus its frozen execution row. It returns the activating attempt id (0 when
// not activated).
func (f leaseFixture) insertHistoricRelease(t *testing.T, releaseID, state, tag string, activated bool, strategy string, port, drain int) int64 {
	t.Helper()
	ctx := context.Background()
	deploymentID := "dep-" + releaseID
	f.insertPlainDeployment(t, deploymentID)
	f.walkDeployment(t, deploymentID, healthyPath...)
	if _, err := f.pool.Exec(ctx, `INSERT INTO releases(id, deployment_id, environment_id, image_digest, config_digest, commit_sha, strategy, state)
		VALUES ($1,$2,$3,decode(repeat('11',32),'hex'),decode(repeat('22',32),'hex'),'abcdefabcdefabcdefabcdefabcdefabcdefabcd','blue_green',$4)`, releaseID, deploymentID, f.envID, state); err != nil {
		t.Fatal(err)
	}
	if !activated {
		if _, err := f.pool.Exec(ctx, `INSERT INTO agent_jobs(schema_version,job_kind,owner_id,project_id,environment_id,server_id,deployment_id,repository_id,full_ref,commit_sha,priority,available_at,idempotency_key,payload_digest,state,attempt_count)
			VALUES (1,'deployment_execution',$1,'project',$2,$3,$4,201,'refs/heads/main','abcdefabcdefabcdefabcdefabcdefabcdefabcd',0,clock_timestamp(),$5,decode(repeat('00',32),'hex'),'succeeded',0)`, f.ownerID, f.envID, f.serverID, deploymentID, "hist-key-"+releaseID); err != nil {
			t.Fatal(err)
		}
		return 0
	}
	attemptID := f.insertSucceededActivation(t, releaseID, deploymentID, tag)
	if _, err := f.pool.Exec(ctx, `INSERT INTO agent_job_attempt_execution(attempt_id, container_port, container_scheme, release_strategy, drain_seconds, health_policy_revision, cpu_millis, memory_bytes, restart_policy, environment_runtime_config_revision, health_gate_path, health_gate_host)
		VALUES ($1,$2,'http',$3,$4,1,250,134217728,'unless-stopped',0,'/health','target.internal')`, attemptID, port, strategy, drain); err != nil {
		t.Fatal(err)
	}
	return attemptID
}

func newRollbackWorld(t *testing.T, seed byte) rollbackWorld {
	t.Helper()
	f := newLeaseFixture(t, seed)
	ctx := context.Background()
	// The fixture's own queued deployment would count as in flight.
	f.walkDeployment(t, "lease-deployment-"+t.Name(), deployment.StatusCancelled)
	if _, err := f.pool.Exec(ctx, `UPDATE agent_jobs SET state='cancelled' WHERE id=$1`, f.jobID); err != nil {
		t.Fatal(err)
	}
	w := rollbackWorld{leaseFixture: f, cred: f.withCredential(t, seed+50)}
	w.repo = NewWebhookJobRepository(f.pool, NewDeploymentRepository(f.pool))
	w.lease = NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	f.insertRouteHead(t, "app.example.com")
	suffix := releaseSuffix(t)
	w.relA, w.relB = "rb-a-"+suffix, "rb-b-"+suffix
	w.attemptA = f.insertHistoricRelease(t, w.relA, "retained", "A", true, "blue_green", 9090, 5)
	w.attemptB = f.insertHistoricRelease(t, w.relB, "active", "B", true, "blue_green", 8080, 5)
	if _, err := f.pool.Exec(ctx, `UPDATE route_heads SET active_release_id=$2, route_revision=route_revision+1, updated_at=clock_timestamp() WHERE environment_id=$1`, f.envID, w.relB); err != nil {
		t.Fatal(err)
	}
	return w
}

func (w rollbackWorld) intentCount(t *testing.T) (intents, rollbackJobs, deployments int) {
	t.Helper()
	if err := w.pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM rollback_intents),(SELECT count(*) FROM agent_jobs WHERE job_kind='rollback_execution'),(SELECT count(*) FROM deployments)`).Scan(&intents, &rollbackJobs, &deployments); err != nil {
		t.Fatal(err)
	}
	return
}

func TestRollbackEnqueueWritesIntentDeploymentJobFromTarget(t *testing.T) {
	w := newRollbackWorld(t, 120)
	ctx := context.Background()
	res, err := w.repo.EnqueueRollback(ctx, w.principal, w.relA, rollbackNonce)
	if err != nil || res.Replayed || res.JobID == 0 || !strings.HasPrefix(string(res.DeploymentID), "rbd_") {
		t.Fatalf("result=%+v err=%v", res, err)
	}
	var kind, target, env, srv, sha, ref, status string
	var repoID int64
	if err = w.pool.QueryRow(ctx, `SELECT j.job_kind,j.rollback_target_release_id,j.environment_id,j.server_id,j.commit_sha,j.full_ref,j.repository_id,d.status FROM agent_jobs j JOIN deployments d ON d.id=j.deployment_id WHERE j.deployment_id=$1`, string(res.DeploymentID)).Scan(&kind, &target, &env, &srv, &sha, &ref, &repoID, &status); err != nil {
		t.Fatal(err)
	}
	if kind != "rollback_execution" || target != w.relA || env != w.envID || srv != w.serverID || sha != "abcdefabcdefabcdefabcdefabcdefabcdefabcd" || ref != "refs/heads/main" || repoID != 201 || status != "queued" {
		t.Fatalf("persisted kind=%s target=%s env=%s srv=%s sha=%s ref=%s repo=%d status=%s", kind, target, env, srv, sha, ref, repoID, status)
	}
	var source, actor string
	if err = w.pool.QueryRow(ctx, `SELECT source_release_id, actor_user_id FROM rollback_intents WHERE deployment_id=$1`, string(res.DeploymentID)).Scan(&source, &actor); err != nil {
		t.Fatal(err)
	}
	if source != w.relB || actor != w.principal.UserID {
		t.Fatalf("intent source=%s actor=%s", source, actor)
	}
	var events int
	if err = w.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM agent_job_events WHERE job_id=$1)+(SELECT count(*) FROM deployment_events WHERE deployment_id=$2)`, res.JobID, string(res.DeploymentID)).Scan(&events); err != nil || events != 2 {
		t.Fatalf("events=%d err=%v", events, err)
	}
	if ok, err := w.repo.RollbackEligible(ctx, w.principal, w.relA); err != nil || ok {
		t.Fatalf("eligible with a rollback in flight: %v %v", ok, err)
	}
}

func TestRollbackEnqueueEligibility(t *testing.T) {
	ctx := context.Background()
	cases := map[string]struct {
		seed   byte
		mutate func(t *testing.T, w rollbackWorld) string // returns the target release id
		want   error
	}{
		"active target": {121, func(t *testing.T, w rollbackWorld) string { return w.relB }, ErrRollbackNotEligible},
		"removed target": {122, func(t *testing.T, w rollbackWorld) string {
			id := "rb-removed-" + releaseSuffix(t)
			w.insertHistoricRelease(t, id, "removed", "R", true, "blue_green", 9090, 5)
			return id
		}, ErrRollbackNotEligible},
		"candidate target": {123, func(t *testing.T, w rollbackWorld) string {
			id := "rb-cand-" + releaseSuffix(t)
			w.insertHistoricRelease(t, id, "candidate", "C", true, "blue_green", 9090, 5)
			return id
		}, ErrRollbackNotEligible},
		"never activated": {124, func(t *testing.T, w rollbackWorld) string {
			id := "rb-never-" + releaseSuffix(t)
			w.insertHistoricRelease(t, id, "retained", "N", false, "blue_green", 9090, 5)
			return id
		}, ErrRollbackNotEligible},
		"other server": {125, func(t *testing.T, w rollbackWorld) string {
			w.insertServer(t, "server2")
			g := w.leaseFixture
			g.serverID = "server2"
			id := "rb-srv2-" + releaseSuffix(t)
			g.insertHistoricRelease(t, id, "retained", "S", true, "blue_green", 9090, 5)
			return id
		}, ErrRollbackNotEligible},
		"unknown release": {126, func(t *testing.T, w rollbackWorld) string { return "no-such-release" }, ErrRollbackNotFound},
		"slot reconciling": {127, func(t *testing.T, w rollbackWorld) string {
			if _, err := w.pool.Exec(ctx, `UPDATE environment_slots SET reconciliation_required=true, reconciliation_attempt_id=$2 WHERE environment_id=$1`, w.envID, w.attemptB); err != nil {
				t.Fatal(err)
			}
			return w.relA
		}, ErrRollbackNotEligible},
		"in flight deployment": {128, func(t *testing.T, w rollbackWorld) string {
			w.insertJobAt(t, w.serverID, w.envID, "rb-inflight-"+releaseSuffix(t), 0, "clock_timestamp()")
			return w.relA
		}, ErrRollbackInFlight},
		"head flagged": {129, func(t *testing.T, w rollbackWorld) string {
			if _, err := w.pool.Exec(ctx, `UPDATE route_heads SET reconciliation_required=true WHERE environment_id=$1`, w.envID); err != nil {
				t.Fatal(err)
			}
			return w.relA
		}, ErrRollbackNotEligible},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			w := newRollbackWorld(t, c.seed)
			target := c.mutate(t, w)
			_, err := w.repo.EnqueueRollback(ctx, w.principal, target, rollbackNonce)
			if !errors.Is(err, c.want) {
				t.Fatalf("err=%v, want %v", err, c.want)
			}
			if i, j, _ := w.intentCount(t); i != 0 || j != 0 {
				t.Fatalf("refused rollback wrote intents=%d jobs=%d", i, j)
			}
			if ok, rerr := w.repo.RollbackEligible(ctx, w.principal, target); rerr != nil || ok {
				t.Fatalf("RollbackEligible=%v err=%v for a refused target", ok, rerr)
			}
		})
	}
}

func TestRollbackEnqueueRefusesRevokedFrozenSecret(t *testing.T) {
	w := newRollbackWorld(t, 130)
	ctx := context.Background()
	// A frozen version of the target's winning attempt that has since been
	// rotated away (superseded: still allowed) and one that was revoked.
	w.insertSecretHeadWithActiveVersion(t, "rb-head-1-"+releaseSuffix(t), w.envID, "TOKEN", "runtime", "rb-v1-"+releaseSuffix(t), 1)
	if _, err := w.pool.Exec(ctx, `INSERT INTO frozen_secret_versions(attempt_id, secret_version_id, variable_name, delivery_scope) VALUES ($1,$2,'TOKEN','runtime')`, w.attemptA, "rb-v1-"+releaseSuffix(t)); err != nil {
		t.Fatal(err)
	}
	w.rotateSecretVersion(t, "rb-head-1-"+releaseSuffix(t), w.envID, "rb-v2-"+releaseSuffix(t), 2)
	if ok, err := w.repo.RollbackEligible(ctx, w.principal, w.relA); err != nil || !ok {
		t.Fatalf("superseded frozen version must stay eligible: %v %v", ok, err)
	}
	if _, err := w.pool.Exec(ctx, `UPDATE secret_versions SET state='revoked' WHERE id=$1`, "rb-v1-"+releaseSuffix(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.repo.EnqueueRollback(ctx, w.principal, w.relA, rollbackNonce); !errors.Is(err, ErrRollbackSecretRevoked) {
		t.Fatalf("err=%v, want ErrRollbackSecretRevoked", err)
	}
	if ok, err := w.repo.RollbackEligible(ctx, w.principal, w.relA); err != nil || ok {
		t.Fatalf("revoked: eligible=%v err=%v", ok, err)
	}
}

func TestRollbackEnqueueCrossOwnerIsNotFoundAndDeadSessionDenied(t *testing.T) {
	w := newRollbackWorld(t, 131)
	ctx := context.Background()
	tx, err := w.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = evaluateRollback(ctx, tx, "some-other-owner", w.relA, false); !errors.Is(err, ErrRollbackNotFound) {
		t.Fatalf("cross-owner err=%v, want ErrRollbackNotFound", err)
	}
	if _, err = evaluateRollback(ctx, tx, w.ownerID, w.relA, false); err != nil {
		t.Fatalf("own release err=%v", err)
	}
	bad := w.principal
	bad.OwnerID = "some-other-owner"
	if _, err = w.repo.EnqueueRollback(ctx, bad, w.relA, rollbackNonce); !errors.Is(err, ErrRollbackUnauthorized) {
		t.Fatalf("foreign principal err=%v, want ErrRollbackUnauthorized", err)
	}
	for name, in := range map[string][2]string{"short nonce": {w.relA, "short"}, "bad nonce": {w.relA, "nonce with spaces!!!"}, "empty release": {"", rollbackNonce}} {
		if _, err = w.repo.EnqueueRollback(ctx, w.principal, in[0], in[1]); !errors.Is(err, ErrRollbackInvalid) {
			t.Fatalf("%s err=%v", name, err)
		}
	}
}

func TestRollbackEnqueueNonceIdempotencyAndConflict(t *testing.T) {
	w := newRollbackWorld(t, 132)
	ctx := context.Background()
	other := "rb-other-" + releaseSuffix(t)
	w.insertHistoricRelease(t, other, "retained", "O", true, "blue_green", 9090, 5)
	first, err := w.repo.EnqueueRollback(ctx, w.principal, w.relA, rollbackNonce)
	if err != nil {
		t.Fatal(err)
	}
	again, err := w.repo.EnqueueRollback(ctx, w.principal, w.relA, rollbackNonce)
	if err != nil || !again.Replayed || again.DeploymentID != first.DeploymentID || again.JobID != first.JobID {
		t.Fatalf("replay=%+v err=%v", again, err)
	}
	if _, err = w.repo.EnqueueRollback(ctx, w.principal, other, rollbackNonce); !errors.Is(err, ErrRollbackConflict) {
		t.Fatalf("different target same nonce err=%v, want ErrRollbackConflict", err)
	}
	// A fresh nonce while the first is in flight is the in-flight refusal.
	if _, err = w.repo.EnqueueRollback(ctx, w.principal, w.relA, "rollback-nonce-other-0123"); !errors.Is(err, ErrRollbackInFlight) {
		t.Fatalf("second nonce err=%v, want ErrRollbackInFlight", err)
	}
	if i, j, _ := w.intentCount(t); i != 1 || j != 1 {
		t.Fatalf("intents=%d jobs=%d, want 1/1", i, j)
	}
}

// claimRollback enqueues a rollback to relA and claims it.
func (w rollbackWorld) claimRollback(t *testing.T) (RollbackResult, queue.ClaimedLease) {
	t.Helper()
	ctx := context.Background()
	res, err := w.repo.EnqueueRollback(ctx, w.principal, w.relA, rollbackNonce)
	if err != nil {
		t.Fatal(err)
	}
	leases, err := w.lease.Claim(ctx, w.serverID, w.cred, 1)
	if err != nil || len(leases) != 1 {
		t.Fatalf("claim leases=%d err=%v", len(leases), err)
	}
	return res, leases[0]
}

func TestRollbackClaimFreezesTargetConfigWithLiveStrategy(t *testing.T) {
	w := newRollbackWorld(t, 133)
	ctx := context.Background()
	// A live binding that the target never had: a rollback must not pick it up.
	w.insertSecretHeadWithActiveVersion(t, "rb-live-head-"+releaseSuffix(t), w.envID, "LIVE_ONLY", "runtime", "rb-live-v1-"+releaseSuffix(t), 1)
	if _, err := w.pool.Exec(ctx, `INSERT INTO frozen_literal_values(attempt_id, variable_name, delivery_scope, literal_value) VALUES ($1,'NODE_ENV','runtime','production')`, w.attemptA); err != nil {
		t.Fatal(err)
	}
	res, lease := w.claimRollback(t)
	if lease.DeploymentID != string(res.DeploymentID) || lease.ReleaseID != w.relA || lease.RollbackTargetReleaseID != w.relA {
		t.Fatalf("lease ids %+v", lease)
	}
	if want := dockerid.AttemptRuntimeInstanceID(w.attemptA); lease.RollbackTargetRuntimeInstanceID != want {
		t.Fatalf("target instance %q, want %q", lease.RollbackTargetRuntimeInstanceID, want)
	}
	// Frozen from the target (port 9090, health gate /health on target.internal)...
	if lease.ContainerPort != 9090 || lease.HealthGatePath != "/health" || lease.HealthGateHost != "target.internal" || lease.CPUMillis != 250 || lease.MemoryBytes != 134217728 {
		t.Fatalf("config not from target: %+v", lease)
	}
	// ...except strategy and drain, which are the live environment's (stop_then_start, 30).
	if lease.ReleaseStrategy != "stop_then_start" || lease.DrainSeconds != 30 {
		t.Fatalf("strategy=%s drain=%d, want live stop_then_start/30", lease.ReleaseStrategy, lease.DrainSeconds)
	}
	if len(lease.SecretBindings) != 0 || len(lease.LiteralBindings) != 1 || lease.LiteralBindings[0].VariableName != "NODE_ENV" {
		t.Fatalf("bindings not the target's: secrets=%v literals=%v", lease.SecretBindings, lease.LiteralBindings)
	}
	var strategy string
	var drain int
	if err := w.pool.QueryRow(ctx, `SELECT release_strategy, drain_seconds FROM agent_job_attempt_execution WHERE attempt_id=$1`, lease.AttemptID).Scan(&strategy, &drain); err != nil || strategy != "stop_then_start" || drain != 30 {
		t.Fatalf("stored execution %s/%d err=%v", strategy, drain, err)
	}
	var releases int
	if err := w.pool.QueryRow(ctx, `SELECT count(*) FROM releases WHERE deployment_id=$1`, lease.DeploymentID).Scan(&releases); err != nil || releases != 0 {
		t.Fatalf("a rollback must not create a release row: %d %v", releases, err)
	}
	if status := w.deploymentStatusOf(t, lease.DeploymentID); status != deployment.StatusAssigned {
		t.Fatalf("rollback deployment status=%s, want assigned", status)
	}
}

func (f leaseFixture) deploymentStatusOf(t *testing.T, id string) deployment.Status {
	t.Helper()
	var s string
	if err := f.pool.QueryRow(context.Background(), `SELECT status FROM deployments WHERE id=$1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return deployment.Status(s)
}

func (w rollbackWorld) activeAndStates(t *testing.T) (active string, a, b string) {
	t.Helper()
	ctx := context.Background()
	if err := w.pool.QueryRow(ctx, `SELECT active_release_id FROM route_heads WHERE environment_id=$1`, w.envID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	a, _ = w.releaseState(t, w.relA)
	b, _ = w.releaseState(t, w.relB)
	return
}

func TestRollbackActivateAndFinalizeWithRetainedCandidate(t *testing.T) {
	w := newRollbackWorld(t, 134)
	ctx := context.Background()
	res, lease := w.claimRollback(t)
	w.walkDeployment(t, lease.DeploymentID, deployment.StatusFetching, deployment.StatusBuilding, deployment.StatusStarting, deployment.StatusHealthChecking)

	// A digest that is not the stored one is refused and writes nothing.
	if _, err := w.lease.Activate(ctx, w.serverID, w.cred, lease.AttemptID, lease.Fence, digest(9)); !errors.Is(err, ErrAgentLeaseIntegrityConflict) {
		t.Fatalf("wrong digest err=%v", err)
	}
	act, err := w.lease.Activate(ctx, w.serverID, w.cred, lease.AttemptID, lease.Fence, rollbackImageDigest())
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if want := dockerid.AttemptRuntimeInstanceID(w.attemptA); act.BackendRuntimeInstanceID != want {
		t.Fatalf("backend %q, want the target's original instance %q", act.BackendRuntimeInstanceID, want)
	}
	if act.PredecessorReleaseID != w.relB || act.PredecessorRuntimeInstanceID != dockerid.AttemptRuntimeInstanceID(w.attemptB) {
		t.Fatalf("predecessor %q/%q", act.PredecessorReleaseID, act.PredecessorRuntimeInstanceID)
	}
	if st, _ := w.releaseState(t, w.relA); st != "retained" {
		t.Fatalf("target state after authorization=%s, want still retained (no candidate->ready)", st)
	}
	var kind string
	if err = w.pool.QueryRow(ctx, `SELECT kind FROM release_operations WHERE attempt_id=$1`, lease.AttemptID).Scan(&kind); err != nil || kind != "activate" {
		t.Fatalf("operation kind=%q err=%v", kind, err)
	}
	// Replay answers the same authorization (fresh and replay agree).
	replay, err := w.lease.Activate(ctx, w.serverID, w.cred, lease.AttemptID, lease.Fence, rollbackImageDigest())
	if err != nil || replay.BackendRuntimeInstanceID != act.BackendRuntimeInstanceID || replay.OperationID != act.OperationID || replay.PredecessorRuntimeInstanceID != act.PredecessorRuntimeInstanceID {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}

	raw, err := hex.DecodeString(strings.TrimPrefix(act.RouteDigest, "sha256:"))
	if err != nil || len(raw) != 32 {
		t.Fatal(err)
	}
	if err = w.lease.ReportActivationResult(ctx, w.serverID, w.cred, lease.AttemptID, lease.Fence, act.OperationID, true, [32]byte(raw), ""); err != nil {
		t.Fatalf("ReportActivationResult: %v", err)
	}
	active, a, b := w.activeAndStates(t)
	if active != w.relA || a != "active" || b != "retained" {
		t.Fatalf("active=%s A=%s B=%s, want A active, B retained", active, a, b)
	}
	if got := w.deploymentStatusOf(t, "dep-"+w.relB); got != deployment.StatusRolledBack {
		t.Fatalf("source deployment=%s, want rolled_back", got)
	}
	if got := w.deploymentStatusOf(t, lease.DeploymentID); got != deployment.StatusHealthy {
		t.Fatalf("rollback deployment=%s, want healthy", got)
	}
	if got := w.deploymentStatusOf(t, "dep-"+w.relA); got != deployment.StatusHealthy {
		t.Fatalf("target's own deployment=%s, want untouched healthy", got)
	}
	var jobState string
	var current *int64
	if err = w.pool.QueryRow(ctx, `SELECT (SELECT state FROM agent_jobs WHERE id=$1),(SELECT current_attempt_id FROM environment_slots WHERE environment_id=$2)`, res.JobID, w.envID).Scan(&jobState, &current); err != nil || jobState != "succeeded" || current != nil {
		t.Fatalf("job=%s slot=%v err=%v", jobState, current, err)
	}
	// The rollback's own succeeded operation must not redefine the target's
	// container: its instance stays the original attempt's.
	if got, found, err := winningActivationAttempt(ctx, w.pool, w.relA); err != nil || !found || got != w.attemptA {
		t.Fatalf("winning attempt after rollback = %d found=%v err=%v, want %d", got, found, err, w.attemptA)
	}
	// Rolling forward again: B is now retained and eligible.
	if ok, err := w.repo.RollbackEligible(ctx, w.principal, w.relB); err != nil || !ok {
		t.Fatalf("B after rollback: eligible=%v err=%v", ok, err)
	}
}

func TestRollbackActivateRefusesTargetNoLongerRetained(t *testing.T) {
	w := newRollbackWorld(t, 135)
	ctx := context.Background()
	_, lease := w.claimRollback(t)
	w.walkDeployment(t, lease.DeploymentID, deployment.StatusFetching, deployment.StatusBuilding, deployment.StatusStarting, deployment.StatusHealthChecking)
	// Forced SQL (triggers off for one tx via session_replication_role): the target leaves 'retained'
	// after the claim, as retention cleanup eventually will.
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	for _, q := range []string{`SET LOCAL session_replication_role = replica`, `UPDATE releases SET state='removed' WHERE id='` + w.relA + `'`} {
		if _, err = tx.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := w.lease.Activate(ctx, w.serverID, w.cred, lease.AttemptID, lease.Fence, rollbackImageDigest()); !errors.Is(err, ErrAgentLeaseActivationCandidateNotReady) {
		t.Fatalf("err=%v, want ErrAgentLeaseActivationCandidateNotReady", err)
	}
}

// Rollback vs a deploy's activation result for the same environment, both
// blocked on the environment slot row by a holder tx, then released together.
// Report always succeeds. The rollback either runs after it (source is the NEW
// active release, never the old one) or is refused as in flight.
func TestRollbackEnqueueGenuinelyRacesActivationResult(t *testing.T) {
	f := newLeaseFixture(t, 137)
	cred := f.withCredential(t, 187)
	f.insertRouteHead(t, "app.example.com")
	relA, relB := "rb-ra-"+releaseSuffix(t), "rb-rb-"+releaseSuffix(t)
	f.insertHistoricRelease(t, relA, "retained", "A", true, "blue_green", 9090, 5)
	f.insertHistoricRelease(t, relB, "active", "B", true, "blue_green", 8080, 5)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	if _, err := f.pool.Exec(ctx, `UPDATE route_heads SET active_release_id=$2, route_revision=route_revision+1, updated_at=clock_timestamp() WHERE environment_id=$1`, f.envID, relB); err != nil {
		t.Fatal(err)
	}
	// The fixture's own job (deployment C) is the deploy: lease, authorize.
	attemptC := f.leaseJob(t, 2, 90)
	f.advanceDeploymentToHealthChecking(t)
	candidateC := "rb-rc-" + releaseSuffix(t)
	f.insertCandidateRelease(t, attemptC, candidateC)
	lease := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	act, err := lease.Activate(ctx, f.serverID, cred, attemptC, 2, digest(5))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := hex.DecodeString(strings.TrimPrefix(act.RouteDigest, "sha256:"))
	repo := NewWebhookJobRepository(f.pool, NewDeploymentRepository(f.pool))

	holder, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx) //nolint:errcheck
	if _, err := holder.Exec(ctx, `SELECT 1 FROM environment_slots WHERE environment_id=$1 FOR UPDATE`, f.envID); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var reportErr, enqueueErr error
	var res RollbackResult
	wg.Add(2)
	go func() {
		defer wg.Done()
		reportErr = lease.ReportActivationResult(ctx, f.serverID, cred, attemptC, 2, act.OperationID, true, [32]byte(raw), "")
	}()
	go func() {
		defer wg.Done()
		res, enqueueErr = repo.EnqueueRollback(ctx, f.principal, relA, rollbackNonce)
	}()
	waitForLockWaiters(t, ctx, f.pool, "environment_slots", 2)
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()

	if reportErr != nil {
		t.Fatalf("activation result must always succeed: %v", reportErr)
	}
	var active string
	if err := f.pool.QueryRow(ctx, `SELECT active_release_id FROM route_heads WHERE environment_id=$1`, f.envID).Scan(&active); err != nil || active != candidateC {
		t.Fatalf("active=%q err=%v, want the deploy's release", active, err)
	}
	var actives int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM releases WHERE environment_id=$1 AND state='active'`, f.envID).Scan(&actives); err != nil || actives != 1 {
		t.Fatalf("active releases=%d err=%v", actives, err)
	}
	switch {
	case enqueueErr == nil:
		var source string
		if err := f.pool.QueryRow(ctx, `SELECT source_release_id FROM rollback_intents WHERE deployment_id=$1`, string(res.DeploymentID)).Scan(&source); err != nil || source != candidateC {
			t.Fatalf("rollback ran after the deploy but recorded source %q (err %v), want %q", source, err, candidateC)
		}
	case errors.Is(enqueueErr, ErrRollbackInFlight):
		if n := countRows(t, f.pool, `rollback_intents`); n != 0 {
			t.Fatalf("refused rollback left %d intents", n)
		}
	default:
		t.Fatalf("enqueue err=%v, want success or ErrRollbackInFlight", enqueueErr)
	}
}

func countRows(t *testing.T, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, table string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM `+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A rollback request racing a deploy's Activate (authorization): the deploy's
// deployment is non-terminal whichever side takes the slot first, so the
// rollback is always refused and Activate always succeeds.
func TestRollbackEnqueueGenuinelyRacesDeployActivate(t *testing.T) {
	f := newLeaseFixture(t, 138)
	cred := f.withCredential(t, 188)
	f.insertRouteHead(t, "app.example.com")
	relA, relB := "rb-qa-"+releaseSuffix(t), "rb-qb-"+releaseSuffix(t)
	f.insertHistoricRelease(t, relA, "retained", "A", true, "blue_green", 9090, 5)
	f.insertHistoricRelease(t, relB, "active", "B", true, "blue_green", 8080, 5)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	if _, err := f.pool.Exec(ctx, `UPDATE route_heads SET active_release_id=$2, route_revision=route_revision+1, updated_at=clock_timestamp() WHERE environment_id=$1`, f.envID, relB); err != nil {
		t.Fatal(err)
	}
	attemptC := f.leaseJob(t, 2, 90)
	f.advanceDeploymentToHealthChecking(t)
	f.insertCandidateRelease(t, attemptC, "rb-qc-"+releaseSuffix(t))
	lease := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	repo := NewWebhookJobRepository(f.pool, NewDeploymentRepository(f.pool))

	holder, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx) //nolint:errcheck
	if _, err := holder.Exec(ctx, `SELECT 1 FROM environment_slots WHERE environment_id=$1 FOR UPDATE`, f.envID); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var activateErr, enqueueErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, activateErr = lease.Activate(ctx, f.serverID, cred, attemptC, 2, digest(6))
	}()
	go func() {
		defer wg.Done()
		_, enqueueErr = repo.EnqueueRollback(ctx, f.principal, relA, rollbackNonce)
	}()
	waitForLockWaiters(t, ctx, f.pool, "environment_slots", 2)
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if activateErr != nil || !errors.Is(enqueueErr, ErrRollbackInFlight) {
		t.Fatalf("activateErr=%v enqueueErr=%v, want nil / ErrRollbackInFlight", activateErr, enqueueErr)
	}
	if n := countRows(t, f.pool, `rollback_intents`); n != 0 {
		t.Fatalf("intents=%d", n)
	}
}

// Double click: two concurrent requests with the same nonce create exactly one
// rollback; with different nonces exactly one wins and the other is refused.
func TestRollbackEnqueueDoubleClickConcurrentCalls(t *testing.T) {
	for name, nonces := range map[string][2]string{"same nonce": {rollbackNonce, rollbackNonce}, "different nonces": {rollbackNonce, "rollback-nonce-second-01"}} {
		t.Run(name, func(t *testing.T) {
			w := newRollbackWorld(t, 139)
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
			defer cancel()
			holder, err := w.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer holder.Rollback(ctx) //nolint:errcheck
			if _, err := holder.Exec(ctx, `SELECT 1 FROM environment_slots WHERE environment_id=$1 FOR UPDATE`, w.envID); err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			results := make([]RollbackResult, 2)
			errs := make([]error, 2)
			for i := range nonces {
				wg.Add(1)
				go func() {
					defer wg.Done()
					results[i], errs[i] = w.repo.EnqueueRollback(ctx, w.principal, w.relA, nonces[i])
				}()
			}
			// One call blocks on the slot; the other waits behind it on the
			// shared administrator session row (same principal).
			waitForLockWaiters(t, ctx, w.pool, "environment_slots", 1)
			waitForLockWaiters(t, ctx, w.pool, "admin_sessions", 1)
			if err := holder.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			wg.Wait()
			if i, j, _ := w.intentCount(t); i != 1 || j != 1 {
				t.Fatalf("intents=%d jobs=%d, want exactly 1/1 (errs %v %v)", i, j, errs[0], errs[1])
			}
			ok := 0
			for i := range errs {
				switch {
				case errs[i] == nil:
					ok++
				case name == "different nonces" && errors.Is(errs[i], ErrRollbackInFlight):
				default:
					t.Fatalf("call %d err=%v", i, errs[i])
				}
			}
			if name == "same nonce" {
				if ok != 2 || results[0].DeploymentID != results[1].DeploymentID || results[0].JobID != results[1].JobID || results[0].Replayed == results[1].Replayed {
					t.Fatalf("same nonce: ok=%d results=%+v", ok, results)
				}
			} else if ok != 1 {
				t.Fatalf("different nonces: %d succeeded, want 1", ok)
			}
		})
	}
}

// A rollback retry (attempt 2) copies attempt 1's frozen rows, including the
// live strategy/drain frozen at attempt 1, never re-reading the target or the
// live config.
func TestRollbackClaimRetryCopiesFirstAttempt(t *testing.T) {
	w := newRollbackWorld(t, 140)
	ctx := context.Background()
	res, first := w.claimRollback(t)
	w.forceRetry(t, res.JobID, first.AttemptID, w.envID)
	// The live config changes between the attempts: the retry must not see it.
	if _, err := w.pool.Exec(ctx, `UPDATE environment_runtime_configs SET release_strategy='blue_green', drain_seconds=99, revision=revision+1 WHERE environment_id=$1`, w.envID); err != nil {
		t.Fatal(err)
	}
	leases, err := w.lease.Claim(ctx, w.serverID, w.cred, 1)
	if err != nil || len(leases) != 1 {
		t.Fatalf("retry claim leases=%d err=%v", len(leases), err)
	}
	second := leases[0]
	if second.AttemptNumber != 2 || second.ReleaseID != w.relA || second.RollbackTargetReleaseID != w.relA || second.RollbackTargetRuntimeInstanceID != first.RollbackTargetRuntimeInstanceID {
		t.Fatalf("retry lease %+v", second)
	}
	if second.ReleaseStrategy != first.ReleaseStrategy || second.DrainSeconds != first.DrainSeconds || second.ContainerPort != 9090 {
		t.Fatalf("retry config %s/%d/%d, want attempt 1's %s/%d/9090", second.ReleaseStrategy, second.DrainSeconds, second.ContainerPort, first.ReleaseStrategy, first.DrainSeconds)
	}
}
