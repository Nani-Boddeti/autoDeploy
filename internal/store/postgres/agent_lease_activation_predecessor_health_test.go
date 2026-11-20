//go:build integration

package postgres

import (
	"context"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"autodeploy/internal/healthgate"
	"autodeploy/internal/queue"
)

// insertPredecessorExecution freezes the predecessor attempt's execution row
// with caller-chosen, deliberately non-default values (forced SQL).
func (f leaseFixture) insertPredecessorExecution(t *testing.T, attemptID int64) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO agent_job_attempt_execution(attempt_id, container_port, container_scheme, release_strategy, drain_seconds, health_policy_revision, cpu_millis, memory_bytes, restart_policy, environment_runtime_config_revision, health_gate_path, health_gate_host, health_success_threshold, health_interval_seconds, health_timeout_seconds, health_deadline_seconds, health_tcp_port)
		VALUES ($1,3000,'https','stop_then_start',30,1,500,268435456,'unless-stopped',0,'/old-ready','old.example.com',4,6,5,70,9100)`, attemptID); err != nil {
		t.Fatal(err)
	}
}

func wantOldBlock() *queue.PredecessorHealth {
	return &queue.PredecessorHealth{
		ContainerPort: 3000, ContainerScheme: "https", HealthPolicyRevision: 1,
		HealthGatePath: "/old-ready", HealthGateHost: "old.example.com",
		HealthPolicy: healthgate.Policy{SuccessThreshold: 4, IntervalSeconds: 6, TimeoutSeconds: 5, DeadlineSeconds: 70, TCPPort: 9100},
	}
}

// phWorld: an active predecessor whose frozen row (3000/https//old-ready) is
// unlike both the new candidate's frozen row (8080/http//app.internal, from
// insertCandidateRelease) and the environment's live runtime config, which is
// then edited to yet another path/port/policy.
func newPredecessorHealthWorld(t *testing.T, seed byte, freezePredecessor bool) (f leaseFixture, repo *AgentLeaseRepository, cred [32]byte, attemptID int64) {
	t.Helper()
	f = newLeaseFixture(t, seed)
	cred = f.withCredential(t, seed+50)
	f.insertRouteHead(t, "app.example.com")
	predAttempt := f.insertActiveReleaseAndPointRouteHead(t, "predecessor-"+releaseSuffix(t))
	if freezePredecessor {
		f.insertPredecessorExecution(t, predAttempt)
	}
	attemptID = f.leaseJob(t, 1, 90)
	f.advanceDeploymentToHealthChecking(t)
	f.insertCandidateRelease(t, attemptID, "candidate-"+releaseSuffix(t))
	if _, err := f.pool.Exec(context.Background(), `UPDATE environment_runtime_configs SET container_port=7777, health_gate_path='/live-changed', health_gate_host='live.example.com', health_success_threshold=9, revision=revision+1 WHERE environment_id=$1`, f.envID); err != nil {
		t.Fatal(err)
	}
	repo = NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	return f, repo, cred, attemptID
}

func TestActivatePredecessorHealthKeyCaseFrozenValuesAndReplay(t *testing.T) {
	f, repo, cred, attemptID := newPredecessorHealthWorld(t, 110, true)
	ctx := context.Background()
	img := digest(5)

	got, err := repo.ActivateWithPredecessorHealth(ctx, f.serverID, cred, attemptID, 1, img)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.PredecessorHealth, wantOldBlock()) {
		t.Fatalf("block = %+v, want %+v", got.PredecessorHealth, wantOldBlock())
	}
	// Replay (same digest) returns the identical block; a plain replay none.
	replay, err := repo.ActivateWithPredecessorHealth(ctx, f.serverID, cred, attemptID, 1, img)
	if err != nil {
		t.Fatal(err)
	}
	if replay.OperationID != got.OperationID || !reflect.DeepEqual(replay.PredecessorHealth, got.PredecessorHealth) {
		t.Fatalf("replay differs: %+v vs %+v", replay, got)
	}
	plain, err := repo.Activate(ctx, f.serverID, cred, attemptID, 1, img)
	if err != nil {
		t.Fatal(err)
	}
	if plain.PredecessorHealth != nil {
		t.Fatalf("plain Activate leaked a block: %+v", plain.PredecessorHealth)
	}
	var ops int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM release_operations WHERE attempt_id=$1`, attemptID).Scan(&ops); err != nil || ops != 1 {
		t.Fatalf("ops=%d err=%v, want exactly one operation", ops, err)
	}
}

func TestActivatePredecessorHealthNotRequestedIsNil(t *testing.T) {
	f, repo, cred, attemptID := newPredecessorHealthWorld(t, 111, true)
	got, err := repo.Activate(context.Background(), f.serverID, cred, attemptID, 1, digest(5))
	if err != nil || got.PredecessorHealth != nil {
		t.Fatalf("err=%v block=%+v, want nil", err, got.PredecessorHealth)
	}
}

func TestActivatePredecessorHealthNoPredecessorIsNil(t *testing.T) {
	f := newLeaseFixture(t, 112)
	cred := f.withCredential(t, 162)
	attemptID := f.leaseJob(t, 1, 90)
	f.advanceDeploymentToHealthChecking(t)
	f.insertRouteHead(t, "app.example.com")
	f.insertCandidateRelease(t, attemptID, "candidate-"+releaseSuffix(t))
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	got, err := repo.ActivateWithPredecessorHealth(context.Background(), f.serverID, cred, attemptID, 1, digest(5))
	if err != nil || got.PredecessorHealth != nil || got.PredecessorReleaseID != "" {
		t.Fatalf("err=%v result=%+v, want no predecessor and no block", err, got)
	}
}

// A predecessor without a frozen execution row fails closed: no block, and the
// activation itself still succeeds (replay too).
func TestActivatePredecessorHealthMissingFrozenRowIsNil(t *testing.T) {
	f, repo, cred, attemptID := newPredecessorHealthWorld(t, 113, false)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		got, err := repo.ActivateWithPredecessorHealth(ctx, f.serverID, cred, attemptID, 1, digest(5))
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if got.PredecessorHealth != nil || got.PredecessorReleaseID == "" {
			t.Fatalf("call %d: result=%+v, want predecessor named but no block", i, got)
		}
	}
}

// Rollback activation: the predecessor is the release being left (relB). Its
// block is relB's frozen values, not the rollback attempt's copied-from-target
// row nor the live config. After finalize the active release is the rolled-back-to
// relA, whose block (via its winning NON-rollback attempt) equals the values the
// rollback attempt copied from it.
func TestActivatePredecessorHealthRollback(t *testing.T) {
	w := newRollbackWorld(t, 134)
	ctx := context.Background()
	_, attemptID, fence := w.enqueueAndClaimAtHealthChecking(t)

	got, err := w.lease.ActivateWithPredecessorHealth(ctx, w.serverID, w.cred, attemptID, fence, rollbackImageDigest())
	if err != nil {
		t.Fatal(err)
	}
	wantB := &queue.PredecessorHealth{ContainerPort: 8080, ContainerScheme: "http", HealthPolicyRevision: 1, HealthGatePath: "/health", HealthGateHost: "target.internal",
		HealthPolicy: healthgate.Policy{SuccessThreshold: 3, IntervalSeconds: 2, TimeoutSeconds: 2, DeadlineSeconds: 60}}
	if got.PredecessorReleaseID != w.relB || !reflect.DeepEqual(got.PredecessorHealth, wantB) {
		t.Fatalf("predecessor %s block %+v, want %s %+v", got.PredecessorReleaseID, got.PredecessorHealth, w.relB, wantB)
	}
	replay, err := w.lease.ActivateWithPredecessorHealth(ctx, w.serverID, w.cred, attemptID, fence, rollbackImageDigest())
	if err != nil || !reflect.DeepEqual(replay.PredecessorHealth, wantB) {
		t.Fatalf("replay err=%v block=%+v", err, replay.PredecessorHealth)
	}

	raw, _ := hex.DecodeString(strings.TrimPrefix(got.RouteDigest, "sha256:"))
	if err = w.lease.ReportActivationResult(ctx, w.serverID, w.cred, attemptID, fence, got.OperationID, true, [32]byte(raw), ""); err != nil {
		t.Fatal(err)
	}
	if active, _, _ := w.activeAndStates(t); active != w.relA {
		t.Fatalf("active=%s, want %s", active, w.relA)
	}
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	block := predecessorHealthBlock(ctx, tx, w.relA)
	var copiedPort int
	var copiedPath, copiedHost string
	if err = tx.QueryRow(ctx, `SELECT container_port, health_gate_path, health_gate_host FROM agent_job_attempt_execution WHERE attempt_id=$1`, attemptID).Scan(&copiedPort, &copiedPath, &copiedHost); err != nil {
		t.Fatal(err)
	}
	if block == nil || block.ContainerPort != 9090 || block.ContainerPort != copiedPort || block.HealthGatePath != copiedPath || block.HealthGateHost != copiedHost {
		t.Fatalf("rolled-back-to release block %+v, want the target's values %d %s %s", block, copiedPort, copiedPath, copiedHost)
	}
}

// A real SQL error inside the block's reads must not poison the activation
// transaction: the savepoint absorbs it, Activate commits, and the result has
// no block (fail closed). Not parallel: it sets a package-level fault seam.
func TestActivatePredecessorHealthReadErrorKeepsActivationCommitted(t *testing.T) {
	f, repo, cred, attemptID := newPredecessorHealthWorld(t, 114, true)
	ctx := context.Background()
	predecessorHealthReadFault = func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT 1/0`)
		return err
	}
	defer func() { predecessorHealthReadFault = nil }()

	got, err := repo.ActivateWithPredecessorHealth(ctx, f.serverID, cred, attemptID, 1, digest(5))
	if err != nil {
		t.Fatalf("activate failed after a block read error: %v", err)
	}
	if got.PredecessorHealth != nil || got.PredecessorReleaseID == "" {
		t.Fatalf("result=%+v, want predecessor named, no block", got)
	}
	var ops int
	if err = f.pool.QueryRow(ctx, `SELECT count(*) FROM release_operations WHERE attempt_id=$1`, attemptID).Scan(&ops); err != nil || ops != 1 {
		t.Fatalf("ops=%d err=%v, want the activation committed", ops, err)
	}
	// Fault cleared: a replay returns the block again.
	predecessorHealthReadFault = nil
	replay, err := repo.ActivateWithPredecessorHealth(ctx, f.serverID, cred, attemptID, 1, digest(5))
	if err != nil || !reflect.DeepEqual(replay.PredecessorHealth, wantOldBlock()) {
		t.Fatalf("replay err=%v block=%+v", err, replay.PredecessorHealth)
	}
}
