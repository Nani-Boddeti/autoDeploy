//go:build integration

package postgres

import (
	"context"
	"testing"

	"autodeploy/internal/healthgate"
	"autodeploy/internal/queue"
)

// claimWriteCounts snapshots every table a claim writes to, plus the slot
// fence, so a skipped claim can be proven to have written nothing.
type claimWriteCounts struct {
	attempts, executions, events, releases, deployments, frozenSecrets int
	fence                                                              int64
	jobState                                                           string
	attemptCount                                                       int
}

func (f leaseFixture) claimWrites(t *testing.T, jobID int64) claimWriteCounts {
	t.Helper()
	var c claimWriteCounts
	if err := f.pool.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM agent_job_attempts), (SELECT count(*) FROM agent_job_attempt_execution),
		(SELECT count(*) FROM agent_job_events), (SELECT count(*) FROM releases),
		(SELECT count(*) FROM deployment_events), (SELECT count(*) FROM frozen_secret_versions),
		COALESCE((SELECT fence FROM environment_slots WHERE environment_id=$2),-1),
		(SELECT state FROM agent_jobs WHERE id=$1), (SELECT attempt_count FROM agent_jobs WHERE id=$1)`, jobID, f.envID).
		Scan(&c.attempts, &c.executions, &c.events, &c.releases, &c.deployments, &c.frozenSecrets, &c.fence, &c.jobState, &c.attemptCount); err != nil {
		t.Fatal(err)
	}
	return c
}

func (f leaseFixture) setLiveTunedPolicy(t *testing.T) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `UPDATE environment_runtime_configs SET health_success_threshold=5, health_interval_seconds=4, health_timeout_seconds=3, health_deadline_seconds=120, health_tcp_port=8081, revision=revision+1, updated_at=clock_timestamp() WHERE environment_id=$1`, f.envID); err != nil {
		t.Fatal(err)
	}
}

func (f leaseFixture) setLiveDefaultPolicy(t *testing.T) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `UPDATE environment_runtime_configs SET health_success_threshold=3, health_interval_seconds=2, health_timeout_seconds=2, health_deadline_seconds=60, health_tcp_port=NULL, revision=revision+1, updated_at=clock_timestamp() WHERE environment_id=$1`, f.envID); err != nil {
		t.Fatal(err)
	}
}

var optedIn = queue.ClaimCapabilities{HealthPolicy: true}

func TestClaimNonDefaultPolicyIsNotLeasedToNonOptedInAgent(t *testing.T) {
	f := newLeaseFixture(t, 61)
	d := f.withCredential(t, 231)
	f.setLiveTunedPolicy(t)
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	ctx := context.Background()
	// Claim's discovery pre-creates the environment slot row (pre-existing,
	// idempotent, committed outside the claim transaction); run one skipped
	// claim first so that row is part of the baseline.
	if leases, err := repo.Claim(ctx, f.serverID, d, 5); err != nil || len(leases) != 0 {
		t.Fatalf("priming claim: %+v err=%v", leases, err)
	}
	before := f.claimWrites(t, f.jobID)

	for i := 0; i < 2; i++ { // twice: the skip must be repeatable, not a one-shot
		leases, err := repo.Claim(ctx, f.serverID, d, 5)
		if err != nil || len(leases) != 0 {
			t.Fatalf("non-opted claim: %+v err=%v", leases, err)
		}
		if after := f.claimWrites(t, f.jobID); after != before || after.jobState != "queued" {
			t.Fatalf("skipped claim wrote state: before=%+v after=%+v", before, after)
		}
	}
	leases, err := repo.ClaimWithCapabilities(ctx, f.serverID, d, 5, queue.ClaimCapabilities{})
	if err != nil || len(leases) != 0 {
		t.Fatalf("explicit non-opted claim: %+v err=%v", leases, err)
	}

	leases, err = repo.ClaimWithCapabilities(ctx, f.serverID, d, 5, optedIn)
	if err != nil || len(leases) != 1 {
		t.Fatalf("opted-in claim: %+v err=%v", leases, err)
	}
	if leases[0].HealthPolicy != tunedPolicy() {
		t.Fatalf("lease policy = %+v, want %+v", leases[0].HealthPolicy, tunedPolicy())
	}
	if got := loadFrozenSnapshot(t, f, leases[0].AttemptID).HealthPolicy; got != tunedPolicy() {
		t.Fatalf("frozen policy = %+v", got)
	}
}

func TestClaimDefaultPolicyIsLeasedToNonOptedInAgentAsBefore(t *testing.T) {
	f := newLeaseFixture(t, 63)
	d := f.withCredential(t, 233)
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	leases, err := repo.Claim(context.Background(), f.serverID, d, 5)
	if err != nil || len(leases) != 1 {
		t.Fatalf("claim: %+v err=%v", leases, err)
	}
	if leases[0].HealthPolicy != healthgate.DefaultPolicy() {
		t.Fatalf("lease policy = %+v, want defaults", leases[0].HealthPolicy)
	}
}

// A retry copies attempt 1's frozen policy forward, so the frozen value (not
// the live row) decides whether a non-opted-in agent may take it.
func TestClaimRetryUsesFrozenPolicyForTheOptInGuard(t *testing.T) {
	f := newLeaseFixture(t, 65)
	d := f.withCredential(t, 235)
	f.setLiveTunedPolicy(t)
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	ctx := context.Background()
	first, err := repo.ClaimWithCapabilities(ctx, f.serverID, d, 5, optedIn)
	if err != nil || len(first) != 1 {
		t.Fatalf("first claim: %+v err=%v", first, err)
	}
	f.setLiveDefaultPolicy(t) // live now default; attempt 1's frozen policy is not
	f.forceRetry(t, f.jobID, first[0].AttemptID, f.envID)
	before := f.claimWrites(t, f.jobID)

	leases, err := repo.Claim(ctx, f.serverID, d, 5)
	if err != nil || len(leases) != 0 {
		t.Fatalf("non-opted retry claim: %+v err=%v", leases, err)
	}
	if after := f.claimWrites(t, f.jobID); after != before || after.jobState != "retry_wait" {
		t.Fatalf("skipped retry wrote state: before=%+v after=%+v", before, after)
	}
	leases, err = repo.ClaimWithCapabilities(ctx, f.serverID, d, 5, optedIn)
	if err != nil || len(leases) != 1 || leases[0].AttemptNumber != 2 || leases[0].HealthPolicy != tunedPolicy() {
		t.Fatalf("opted-in retry claim: %+v err=%v", leases, err)
	}
}

// setFrozenPolicy rewrites a frozen row. The table is insert-once by trigger,
// so the fixture bypasses user triggers for this one test-only statement.
func (f leaseFixture) setFrozenPolicy(t *testing.T, attemptID int64) {
	t.Helper()
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE agent_job_attempt_execution SET health_success_threshold=5, health_interval_seconds=4, health_timeout_seconds=3, health_deadline_seconds=120, health_tcp_port=8081 WHERE attempt_id=$1`, attemptID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRollbackClaimNonDefaultTargetPolicyNeedsOptIn(t *testing.T) {
	w := newRollbackWorld(t, 67)
	ctx := context.Background()
	w.setFrozenPolicy(t, w.attemptA)
	res, err := w.repo.EnqueueRollback(ctx, w.principal, w.relA, rollbackNonce)
	if err != nil {
		t.Fatal(err)
	}
	before := w.claimWrites(t, res.JobID)

	leases, err := w.lease.Claim(ctx, w.serverID, w.cred, 1)
	if err != nil || len(leases) != 0 {
		t.Fatalf("non-opted rollback claim: %+v err=%v", leases, err)
	}
	if after := w.claimWrites(t, res.JobID); after != before || after.jobState != "queued" {
		t.Fatalf("skipped rollback claim wrote state: before=%+v after=%+v", before, after)
	}
	leases, err = w.lease.ClaimWithCapabilities(ctx, w.serverID, w.cred, 1, optedIn)
	if err != nil || len(leases) != 1 || leases[0].RollbackTargetReleaseID != w.relA {
		t.Fatalf("opted-in rollback claim: %+v err=%v", leases, err)
	}
	if leases[0].HealthPolicy != tunedPolicy() {
		t.Fatalf("rollback lease policy = %+v, want the target's %+v", leases[0].HealthPolicy, tunedPolicy())
	}
}

func TestRollbackClaimDefaultTargetPolicyLeasesToNonOptedInAgent(t *testing.T) {
	w := newRollbackWorld(t, 69)
	_, lease := w.claimRollback(t)
	if lease.HealthPolicy != healthgate.DefaultPolicy() {
		t.Fatalf("rollback lease policy = %+v, want defaults", lease.HealthPolicy)
	}
}
