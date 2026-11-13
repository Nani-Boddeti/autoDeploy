//go:build integration

package postgres

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"autodeploy/internal/deployment"
	"autodeploy/internal/runtime/dockerid"
)

// forceNewActive makes the (already inserted, retained) release id "active" behind the rollback's back, as a
// deploy that activated between the request and the rollback's Activate would
// (forced SQL: the old active becomes retained, the head moves).
func (w rollbackWorld) forceNewActive(t *testing.T, id string) {
	t.Helper()
	ctx := context.Background()
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE releases SET state='retained' WHERE id=$1`, w.relB); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE releases SET state='active' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = w.pool.Exec(ctx, `UPDATE route_heads SET active_release_id=$2, route_revision=route_revision+1, updated_at=clock_timestamp() WHERE environment_id=$1`, w.envID, id); err != nil {
		t.Fatal(err)
	}
}

func (w rollbackWorld) enqueueAndClaimAtHealthChecking(t *testing.T) (RollbackResult, int64, int64) {
	t.Helper()
	res, lease := w.claimRollback(t)
	w.walkDeployment(t, lease.DeploymentID, deployment.StatusFetching, deployment.StatusBuilding, deployment.StatusStarting, deployment.StatusHealthChecking)
	return res, lease.AttemptID, lease.Fence
}

func TestRollbackActivateRefusesChangedSourceFresh(t *testing.T) {
	w := newRollbackWorld(t, 150)
	ctx := context.Background()
	relC := "rb-c-" + releaseSuffix(t)
	w.insertHistoricRelease(t, relC, "retained", "C", true, "blue_green", 8080, 5)
	res, err := w.repo.EnqueueRollback(ctx, w.principal, w.relA, rollbackNonce)
	if err != nil {
		t.Fatal(err)
	}
	w.forceNewActive(t, relC) // a deploy activated after the rollback was requested
	leases, err := w.lease.Claim(ctx, w.serverID, w.cred, 1)
	if err != nil || len(leases) != 1 {
		t.Fatalf("claim %d %v", len(leases), err)
	}
	l := leases[0]
	w.walkDeployment(t, l.DeploymentID, deployment.StatusFetching, deployment.StatusBuilding, deployment.StatusStarting, deployment.StatusHealthChecking)
	_, err = w.lease.Activate(ctx, w.serverID, w.cred, l.AttemptID, l.Fence, rollbackImageDigest())
	if !errors.Is(err, ErrRollbackSourceChanged) || !errors.Is(err, ErrAgentLeaseActivationCandidateNotReady) {
		t.Fatalf("err=%v, want ErrRollbackSourceChanged (a CandidateNotReady refusal)", err)
	}
	var active string
	var ops int
	if err = w.pool.QueryRow(ctx, `SELECT (SELECT active_release_id FROM route_heads WHERE environment_id=$1),(SELECT count(*) FROM release_operations WHERE attempt_id=$2)`, w.envID, l.AttemptID).Scan(&active, &ops); err != nil || active != relC || ops != 0 {
		t.Fatalf("active=%s ops=%d err=%v", active, ops, err)
	}
	if st, _ := w.releaseState(t, relC); st != "active" {
		t.Fatalf("new active release state=%s", st)
	}
	if got := w.deploymentStatusOf(t, "dep-"+relC); got != deployment.StatusHealthy {
		t.Fatalf("the new active release's deployment=%s, want untouched healthy", got)
	}
	if got := w.deploymentStatusOf(t, string(res.DeploymentID)); got != deployment.StatusHealthChecking {
		t.Fatalf("refused rollback deployment=%s, want health_checking (rolled back tx)", got)
	}
}

func TestRollbackActivateReplayRefusesChangedSource(t *testing.T) {
	w := newRollbackWorld(t, 151)
	ctx := context.Background()
	relC := "rb-c-" + releaseSuffix(t)
	w.insertHistoricRelease(t, relC, "retained", "C", true, "blue_green", 8080, 5)
	_, attemptID, fence := w.enqueueAndClaimAtHealthChecking(t)
	if _, err := w.lease.Activate(ctx, w.serverID, w.cred, attemptID, fence, rollbackImageDigest()); err != nil {
		t.Fatal(err)
	}
	w.forceNewActive(t, relC)
	if _, err := w.lease.Activate(ctx, w.serverID, w.cred, attemptID, fence, rollbackImageDigest()); !errors.Is(err, ErrRollbackSourceChanged) {
		t.Fatalf("replay err=%v, want ErrRollbackSourceChanged", err)
	}
}

func TestRollbackFinalizeRefusesChangedSource(t *testing.T) {
	w := newRollbackWorld(t, 152)
	ctx := context.Background()
	other := "rb-other-" + releaseSuffix(t)
	w.insertHistoricRelease(t, other, "retained", "O", true, "blue_green", 8080, 5)
	_, attemptID, fence := w.enqueueAndClaimAtHealthChecking(t)
	act, err := w.lease.Activate(ctx, w.serverID, w.cred, attemptID, fence, rollbackImageDigest())
	if err != nil {
		t.Fatal(err)
	}
	// Forced SQL: the recorded intent names another source than the head.
	tx, _ := w.pool.Begin(ctx)
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE rollback_intents SET source_release_id=$1`, other); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	raw, _ := hex.DecodeString(strings.TrimPrefix(act.RouteDigest, "sha256:"))
	if err = w.lease.ReportActivationResult(ctx, w.serverID, w.cred, attemptID, fence, act.OperationID, true, [32]byte(raw), ""); !errors.Is(err, ErrRollbackSourceChanged) {
		t.Fatalf("err=%v, want ErrRollbackSourceChanged", err)
	}
	if active, a, b := w.activeAndStates(t); active != w.relB || a != "retained" || b != "active" {
		t.Fatalf("finalize wrote: active=%s A=%s B=%s", active, a, b)
	}
}

// blockedRollback leaves a rollback's activate operation reconciliation_required
// (the agent reported a failure), the state an operator then resolves.
func (w rollbackWorld) blockedRollback(t *testing.T) (res RollbackResult, attemptID int64, deploymentID string) {
	t.Helper()
	ctx := context.Background()
	res, lease := w.claimRollback(t)
	w.walkDeployment(t, lease.DeploymentID, deployment.StatusFetching, deployment.StatusBuilding, deployment.StatusStarting, deployment.StatusHealthChecking)
	act, err := w.lease.Activate(ctx, w.serverID, w.cred, lease.AttemptID, lease.Fence, rollbackImageDigest())
	if err != nil {
		t.Fatal(err)
	}
	if err = w.lease.ReportActivationResult(ctx, w.serverID, w.cred, lease.AttemptID, lease.Fence, act.OperationID, false, [32]byte{}, "route switch failed"); err != nil {
		t.Fatalf("report failure: %v", err)
	}
	return res, lease.AttemptID, lease.DeploymentID
}

func TestRollbackOperatorDeclaredActivated(t *testing.T) {
	w := newRollbackWorld(t, 153)
	ctx := context.Background()
	res, attemptID, depID := w.blockedRollback(t)
	if err := w.lease.ResolveActivation(ctx, w.principal, w.envID, ActivationOutcomeActivated, resolveReason); err != nil {
		t.Fatalf("ResolveActivation: %v", err)
	}
	if active, a, b := w.activeAndStates(t); active != w.relA || a != "active" || b != "retained" {
		t.Fatalf("active=%s A=%s B=%s, want A active, B retained", active, a, b)
	}
	if got := w.deploymentStatusOf(t, "dep-"+w.relB); got != deployment.StatusRolledBack {
		t.Fatalf("source deployment=%s, want rolled_back", got)
	}
	if got := w.deploymentStatusOf(t, depID); got != deployment.StatusHealthy {
		t.Fatalf("rollback deployment=%s", got)
	}
	var jobState, opState string
	if err := w.pool.QueryRow(ctx, `SELECT (SELECT state FROM agent_jobs WHERE id=$1),(SELECT state FROM release_operations WHERE attempt_id=$2 AND kind='activate')`, res.JobID, attemptID).Scan(&jobState, &opState); err != nil || jobState != "succeeded" || opState != "succeeded" {
		t.Fatalf("job=%s op=%s err=%v", jobState, opState, err)
	}
}

func TestRollbackOperatorDeclaredNotActivatedChangesNothingElse(t *testing.T) {
	w := newRollbackWorld(t, 154)
	ctx := context.Background()
	res, _, depID := w.blockedRollback(t)
	if err := w.lease.ResolveActivation(ctx, w.principal, w.envID, ActivationOutcomeNotActivated, resolveReason); err != nil {
		t.Fatalf("ResolveActivation: %v", err)
	}
	if active, a, b := w.activeAndStates(t); active != w.relB || a != "retained" || b != "active" {
		t.Fatalf("active=%s A=%s B=%s, want unchanged", active, a, b)
	}
	if got := w.deploymentStatusOf(t, "dep-"+w.relB); got != deployment.StatusHealthy {
		t.Fatalf("source deployment=%s, want untouched", got)
	}
	if got := w.deploymentStatusOf(t, depID); got != deployment.StatusFailed {
		t.Fatalf("rollback deployment=%s, want failed", got)
	}
	var current *int64
	var jobState string
	if err := w.pool.QueryRow(ctx, `SELECT (SELECT current_attempt_id FROM environment_slots WHERE environment_id=$1),(SELECT state FROM agent_jobs WHERE id=$2)`, w.envID, res.JobID).Scan(&current, &jobState); err != nil || current != nil || jobState != "failed" {
		t.Fatalf("slot=%v job=%s err=%v", current, jobState, err)
	}
}

func TestRollbackDrainResultUsesTargetsOriginalInstance(t *testing.T) {
	w := newRollbackWorld(t, 155)
	ctx := context.Background()
	_, attemptID, fence := w.enqueueAndClaimAtHealthChecking(t)
	act, err := w.lease.Activate(ctx, w.serverID, w.cred, attemptID, fence, rollbackImageDigest())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := hex.DecodeString(strings.TrimPrefix(act.RouteDigest, "sha256:"))
	if err = w.lease.ReportActivationResult(ctx, w.serverID, w.cred, attemptID, fence, act.OperationID, true, [32]byte(raw), ""); err != nil {
		t.Fatal(err)
	}
	predInstance := dockerid.AttemptRuntimeInstanceID(w.attemptB)
	if err = w.lease.RecordDrainResult(ctx, w.serverID, w.cred, attemptID, w.relB, predInstance, "stopped", "predecessor stopped"); err != nil {
		t.Fatalf("drain result: %v", err)
	}
	if err = w.lease.RecordDrainResult(ctx, w.serverID, w.cred, attemptID, w.relB, dockerid.AttemptRuntimeInstanceID(attemptID), "stopped", "x"); !errors.Is(err, ErrAgentLeaseIntegrityConflict) {
		t.Fatalf("wrong predecessor instance err=%v", err)
	}
}

func TestRollbackClaimRefusesRevokedSecretWithoutAttempt(t *testing.T) {
	w := newRollbackWorld(t, 156)
	ctx := context.Background()
	w.insertSecretHeadWithActiveVersion(t, "rb-head-c-"+releaseSuffix(t), w.envID, "TOKEN", "runtime", "rb-cv1-"+releaseSuffix(t), 1)
	if _, err := w.pool.Exec(ctx, `INSERT INTO frozen_secret_versions(attempt_id, secret_version_id, variable_name, delivery_scope) VALUES ($1,$2,'TOKEN','runtime')`, w.attemptA, "rb-cv1-"+releaseSuffix(t)); err != nil {
		t.Fatal(err)
	}
	w.rotateSecretVersion(t, "rb-head-c-"+releaseSuffix(t), w.envID, "rb-cv2-"+releaseSuffix(t), 2)
	res, err := w.repo.EnqueueRollback(ctx, w.principal, w.relA, rollbackNonce)
	if err != nil {
		t.Fatalf("superseded version must enqueue: %v", err)
	}
	if _, err = w.pool.Exec(ctx, `UPDATE secret_versions SET state='revoked' WHERE id=$1`, "rb-cv1-"+releaseSuffix(t)); err != nil {
		t.Fatal(err)
	}
	leases, err := w.lease.Claim(ctx, w.serverID, w.cred, 1)
	if err != nil || len(leases) != 0 {
		t.Fatalf("claim leases=%d err=%v, want none", len(leases), err)
	}
	var attempts int
	var jobState, events string
	if err = w.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM agent_job_attempts WHERE job_id=$1),(SELECT state FROM agent_jobs WHERE id=$1),(SELECT string_agg(event_type,',' ORDER BY sequence) FROM agent_job_events WHERE job_id=$1)`, res.JobID).Scan(&attempts, &jobState, &events); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 || jobState != "cancelled" || events != "enqueued,cancelled" {
		t.Fatalf("attempts=%d job=%s events=%s", attempts, jobState, events)
	}
	if got := w.deploymentStatusOf(t, string(res.DeploymentID)); got != deployment.StatusCancelled {
		t.Fatalf("deployment=%s, want cancelled", got)
	}
	var current *int64
	if err = w.pool.QueryRow(ctx, `SELECT current_attempt_id FROM environment_slots WHERE environment_id=$1`, w.envID).Scan(&current); err != nil || current != nil {
		t.Fatalf("slot occupied: %v %v", current, err)
	}
}
