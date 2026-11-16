//go:build integration

package postgres

import (
	"context"
	"testing"

	"autodeploy/internal/healthgate"
	"autodeploy/internal/queue"
)

type frozenPolicyRow struct {
	threshold, interval, timeout, deadline int
	tcp                                    *int
}

func readFrozenPolicyRow(t *testing.T, f leaseFixture, attemptID int64) frozenPolicyRow {
	t.Helper()
	var r frozenPolicyRow
	if err := f.pool.QueryRow(context.Background(), `SELECT health_success_threshold, health_interval_seconds, health_timeout_seconds, health_deadline_seconds, health_tcp_port FROM agent_job_attempt_execution WHERE attempt_id=$1`, attemptID).
		Scan(&r.threshold, &r.interval, &r.timeout, &r.deadline, &r.tcp); err != nil {
		t.Fatal(err)
	}
	return r
}

func loadFrozenSnapshot(t *testing.T, f leaseFixture, attemptID int64) frozenExecutionSnapshot {
	t.Helper()
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	cfg, err := loadAttemptExecution(ctx, tx, attemptID)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// TestAgentLeaseClaimFreezesDefaultHealthPolicy proves a live row that never
// set a policy freezes 3/2/2/60/NULL and the frozen read returns the same.
func TestAgentLeaseClaimFreezesDefaultHealthPolicy(t *testing.T) {
	f := newLeaseFixture(t, 180)
	d := f.withCredential(t, 250)
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	leases, err := repo.Claim(context.Background(), f.serverID, d, 5)
	if err != nil || len(leases) != 1 {
		t.Fatalf("claim: %+v err=%v", leases, err)
	}
	r := readFrozenPolicyRow(t, f, leases[0].AttemptID)
	if r.threshold != 3 || r.interval != 2 || r.timeout != 2 || r.deadline != 60 || r.tcp != nil {
		t.Fatalf("frozen default policy = %+v", r)
	}
	if got := loadFrozenSnapshot(t, f, leases[0].AttemptID).HealthPolicy; got != healthgate.DefaultPolicy() {
		t.Fatalf("frozen read policy = %+v, want defaults", got)
	}
}

// TestAgentLeaseClaimFreezesTunedHealthPolicyAndCopiesItForward proves the
// live values are copied into the frozen row, read back by the frozen read,
// recorded in the release digest, and that a retry keeps attempt 1's policy
// after the administrator changes the live row.
func TestAgentLeaseClaimFreezesTunedHealthPolicyAndCopiesItForward(t *testing.T) {
	f := newLeaseFixture(t, 181)
	d := f.withCredential(t, 251)
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `UPDATE environment_runtime_configs SET health_success_threshold=5, health_interval_seconds=4, health_timeout_seconds=3, health_deadline_seconds=120, health_tcp_port=8081, revision=revision+1, updated_at=clock_timestamp() WHERE environment_id=$1`, f.envID); err != nil {
		t.Fatal(err)
	}

	// A non-default policy is leased only to an agent that opted in (22-1c).
	first, err := repo.ClaimWithCapabilities(ctx, f.serverID, d, 5, queue.ClaimCapabilities{HealthPolicy: true})
	if err != nil || len(first) != 1 {
		t.Fatalf("first claim: %+v err=%v", first, err)
	}
	want := tunedPolicy()
	r := readFrozenPolicyRow(t, f, first[0].AttemptID)
	if r.threshold != 5 || r.interval != 4 || r.timeout != 3 || r.deadline != 120 || r.tcp == nil || *r.tcp != 8081 {
		t.Fatalf("frozen tuned policy = %+v", r)
	}
	snap := loadFrozenSnapshot(t, f, first[0].AttemptID)
	if snap.HealthPolicy != want {
		t.Fatalf("frozen read policy = %+v, want %+v", snap.HealthPolicy, want)
	}
	var stored []byte
	if err := f.pool.QueryRow(ctx, `SELECT config_digest FROM releases WHERE id=$1`, first[0].ReleaseID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if digest := computeConfigDigest(snap, nil, nil); string(stored) != string(digest[:]) {
		t.Fatal("release config_digest does not include the tuned policy")
	}
	def := snap
	def.HealthPolicy = healthgate.DefaultPolicy()
	if defDigest := computeConfigDigest(def, nil, nil); string(stored) == string(defDigest[:]) {
		t.Fatal("tuned release digest equals the default-policy digest")
	}

	// Admin edits the live policy; attempt 2 must keep attempt 1's values.
	if _, err := f.pool.Exec(ctx, `UPDATE environment_runtime_configs SET health_success_threshold=1, health_interval_seconds=1, health_timeout_seconds=1, health_deadline_seconds=10, health_tcp_port=NULL, revision=revision+1, updated_at=clock_timestamp() WHERE environment_id=$1`, f.envID); err != nil {
		t.Fatal(err)
	}
	f.forceRetry(t, f.jobID, first[0].AttemptID, f.envID)
	second, err := repo.ClaimWithCapabilities(ctx, f.serverID, d, 5, queue.ClaimCapabilities{HealthPolicy: true})
	if err != nil || len(second) != 1 || second[0].AttemptNumber != 2 {
		t.Fatalf("second claim: %+v err=%v", second, err)
	}
	if got := loadFrozenSnapshot(t, f, second[0].AttemptID).HealthPolicy; got != want {
		t.Fatalf("attempt 2 frozen policy = %+v, want attempt 1's %+v", got, want)
	}
}
