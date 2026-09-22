//go:build integration

package postgres

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"autodeploy/internal/deployment"
)

// insertJobNoConfig mirrors insertJobAt but deliberately skips provisioning
// an environment_runtime_configs row, so tests can exercise migration
// 000014's claim-time freeze against a genuinely never-configured
// environment. environment_runtime_configs rows are permanently retained
// (no-delete trigger), so this is the only way to get one missing: never
// insert it in the first place.
func (f leaseFixture) insertJobNoConfig(t *testing.T, serverID, environmentID, key string, priority int16) int64 {
	t.Helper()
	ctx := context.Background()
	deploymentID := "deployment-" + key
	var createdAt time.Time
	if err := f.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&createdAt); err != nil {
		t.Fatal(err)
	}
	dep, err := deployment.New(deployment.Identity{DeploymentID: deployment.ID(deploymentID), ProjectID: "project", EnvironmentID: environmentID, ServerID: serverID, CommitSHA: "0123456789abcdef0123456789abcdef01234567"}, createdAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := NewDeploymentRepository(f.pool).Create(ctx, dep); err != nil {
		t.Fatal(err)
	}
	digest := make([]byte, 32)
	var jobID int64
	err = f.pool.QueryRow(ctx, `INSERT INTO agent_jobs(schema_version,job_kind,owner_id,project_id,environment_id,server_id,deployment_id,repository_id,full_ref,commit_sha,priority,available_at,idempotency_key,payload_digest,state) VALUES (1,'deployment_execution',$1,'project',$2,$3,$4,201,'refs/heads/main','0123456789abcdef0123456789abcdef01234567',$5,clock_timestamp(),$6,$7,'queued') RETURNING id`,
		f.ownerID, environmentID, serverID, deploymentID, priority, key, digest).Scan(&jobID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.pool.Exec(ctx, `INSERT INTO agent_job_events(job_id,sequence,event_type) VALUES ($1,1,'enqueued')`, jobID); err != nil {
		t.Fatal(err)
	}
	return jobID
}

// insertSecretHeadWithActiveVersion inserts a secret head and one active
// version scoped to environmentID, bound to variableName on that
// environment via environment_runtime_bindings, and returns the active
// version's id -- the exact value a correct freeze must land in
// frozen_secret_versions.
func (f leaseFixture) insertSecretHeadWithActiveVersion(t *testing.T, headID, environmentID, variableName, deliveryScope, versionID string, versionNumber int) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `INSERT INTO secret_heads(id, owner_id, project_id, environment_id, purpose, delivery_scope, revision) VALUES ($1,$2,'project',$3,'runtime',$4,0)`, headID, f.ownerID, environmentID, deliveryScope); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO secret_versions(id, secret_id, owner_id, project_id, environment_id, version_number, state, suite, aad_version, kek_id, ciphertext, wrapped_dek)
		VALUES ($1,$2,$3,'project',$4,$5,'active','AES-256-GCM-RANDOMNONCE-v1','v1','kek-1', decode(repeat('ab',30),'hex'), decode(repeat('cd',60),'hex'))`,
		versionID, headID, f.ownerID, environmentID, versionNumber); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE secret_heads SET current_version_id=$2, revision=revision+1 WHERE id=$1`, headID, versionID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO environment_runtime_bindings(environment_id, variable_name, binding_kind, secret_head_id, delivery_scope) VALUES ($1,$2,'secret',$3,$4)`, environmentID, variableName, headID, deliveryScope); err != nil {
		t.Fatal(err)
	}
}

// rotateSecretVersion supersedes headID's current active version and
// activates a fresh one, mirroring SecretRepository.Replace's own
// state/current_version_id transitions closely enough for this test's
// purposes without pulling in the crypto/KEK plumbing that package uses.
func (f leaseFixture) rotateSecretVersion(t *testing.T, headID, environmentID, newVersionID string, newVersionNumber int) {
	t.Helper()
	ctx := context.Background()
	// A single transaction, matching SecretRepository.Replace's own
	// statement order: secret_versions_current_state_after_update is a
	// DEFERRED constraint trigger, so superseding the old version before
	// current_version_id is repointed is only legal because both happen
	// before commit -- issued as separate autocommitted statements (one
	// pool.Exec each), the old version's state change would commit while
	// still current and fail immediately.
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var current string
	if err := tx.QueryRow(ctx, `SELECT current_version_id FROM secret_heads WHERE id=$1`, headID).Scan(&current); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE secret_versions SET state='superseded' WHERE id=$1`, current); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO secret_versions(id, secret_id, owner_id, project_id, environment_id, version_number, state, suite, aad_version, kek_id, ciphertext, wrapped_dek)
		VALUES ($1,$2,$3,'project',$4,$5,'active','AES-256-GCM-RANDOMNONCE-v1','v1','kek-1', decode(repeat('ef',30),'hex'), decode(repeat('01',60),'hex'))`,
		newVersionID, headID, f.ownerID, environmentID, newVersionNumber); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE secret_heads SET current_version_id=$2, revision=revision+1 WHERE id=$1`, headID, newVersionID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// forceRetry mirrors the "expire and requeue" fixture pattern used by
// TestAgentLeaseClaimFirstAttemptAssignsDeploymentSecondDoesNotRetransition:
// it expires attemptID, moves the job back to retry_wait with an immediately
// available_at, and frees the environment slot, so a second repo.Claim call
// can lease attempt N+1 of the same job.
func (f leaseFixture) forceRetry(t *testing.T, jobID, attemptID int64, environmentID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `UPDATE agent_job_attempts SET state='expired', closed_at=clock_timestamp() WHERE id=$1`, attemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE agent_jobs SET state='retry_wait', available_at=clock_timestamp() WHERE id=$1`, jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE environment_slots SET current_attempt_id=NULL WHERE environment_id=$1`, environmentID); err != nil {
		t.Fatal(err)
	}
}

// TestAgentLeaseClaimFreezesExecutionConfigAndSecretBindings proves the
// fresh-resolve (attempt 1) path end to end: the claimed lease's execution
// fields match environment_runtime_configs, its one secret binding resolves
// to the head's current active version, agent_job_attempt_execution and
// frozen_secret_versions rows are durably written, and a 'candidate' release
// row with a NULL image_digest exists exactly once.
func TestAgentLeaseClaimFreezesExecutionConfigAndSecretBindings(t *testing.T) {
	f := newLeaseFixture(t, 170)
	d := f.withCredential(t, 240)
	f.insertSecretHeadWithActiveVersion(t, "secret-head-"+t.Name(), f.envID, "APP_TOKEN", "runtime", "secret-version-"+t.Name()+"-v1", 1)
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	ctx := context.Background()

	leases, err := repo.Claim(ctx, f.serverID, d, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 {
		t.Fatalf("leases=%+v", leases)
	}
	lease := leases[0]
	if lease.ContainerPort != 8080 || lease.ContainerScheme != "http" || lease.ReleaseStrategy != "stop_then_start" ||
		lease.DrainSeconds != 30 || lease.HealthPolicyRevision != 1 || lease.CPUMillis != 500 ||
		lease.MemoryBytes != 268435456 || lease.RestartPolicy != "unless-stopped" {
		t.Fatalf("lease execution fields=%+v", lease)
	}
	if len(lease.SecretBindings) != 1 || lease.SecretBindings[0].VariableName != "APP_TOKEN" ||
		lease.SecretBindings[0].DeliveryScope != "runtime" || lease.SecretBindings[0].SecretVersionID != "secret-version-"+t.Name()+"-v1" {
		t.Fatalf("secret bindings=%+v", lease.SecretBindings)
	}
	if lease.ReleaseID == "" {
		t.Fatal("expected a non-empty release id")
	}

	var containerPort int
	var configRevision int64
	if err := f.pool.QueryRow(ctx, `SELECT container_port, environment_runtime_config_revision FROM agent_job_attempt_execution WHERE attempt_id=$1`, lease.AttemptID).Scan(&containerPort, &configRevision); err != nil {
		t.Fatal(err)
	}
	if containerPort != 8080 || configRevision != 0 {
		t.Fatalf("containerPort=%d configRevision=%d", containerPort, configRevision)
	}

	var frozenVersionID, frozenVariable, frozenScope string
	if err := f.pool.QueryRow(ctx, `SELECT secret_version_id, variable_name, delivery_scope FROM frozen_secret_versions WHERE attempt_id=$1`, lease.AttemptID).Scan(&frozenVersionID, &frozenVariable, &frozenScope); err != nil {
		t.Fatal(err)
	}
	if frozenVersionID != "secret-version-"+t.Name()+"-v1" || frozenVariable != "APP_TOKEN" || frozenScope != "runtime" {
		t.Fatalf("frozen=%s/%s/%s", frozenVersionID, frozenVariable, frozenScope)
	}

	var releaseCount int
	var state string
	var imageDigest []byte
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM releases WHERE deployment_id=$1`, lease.DeploymentID).Scan(&releaseCount); err != nil {
		t.Fatal(err)
	}
	if releaseCount != 1 {
		t.Fatalf("release rows=%d", releaseCount)
	}
	if err := f.pool.QueryRow(ctx, `SELECT state, image_digest FROM releases WHERE id=$1`, lease.ReleaseID).Scan(&state, &imageDigest); err != nil {
		t.Fatal(err)
	}
	if state != "candidate" || imageDigest != nil {
		t.Fatalf("state=%s imageDigest=%v", state, imageDigest)
	}
}

// TestAgentLeaseClaimMissingConfigFailsOnlyThatCandidate proves a candidate
// whose environment has no environment_runtime_configs row fails only
// itself: a second, properly configured environment on the same server
// still claims successfully in the same Claim call.
func TestAgentLeaseClaimMissingConfigFailsOnlyThatCandidate(t *testing.T) {
	f := newLeaseFixture(t, 171)
	d := f.withCredential(t, 241)
	unconfiguredEnv := "environment-no-config-" + t.Name()
	unconfiguredJob := f.insertJobNoConfig(t, f.serverID, unconfiguredEnv, "no-config-"+t.Name(), 0)

	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	leases, err := repo.Claim(context.Background(), f.serverID, d, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].EnvironmentID != f.envID {
		t.Fatalf("leases=%+v", leases)
	}

	var state string
	var attemptCount int
	if err := f.pool.QueryRow(context.Background(), `SELECT state, attempt_count FROM agent_jobs WHERE id=$1`, unconfiguredJob).Scan(&state, &attemptCount); err != nil {
		t.Fatal(err)
	}
	if state != "queued" || attemptCount != 0 {
		t.Fatalf("unconfigured candidate mutated: state=%s attemptCount=%d", state, attemptCount)
	}
}

// TestAgentLeaseClaimMissingActiveSecretVersionFailsOnlyThatCandidate proves
// a candidate with a secret binding whose head has no active version (here,
// current_version_id is NULL) fails only itself, while another environment
// on the same server still claims.
func TestAgentLeaseClaimMissingActiveSecretVersionFailsOnlyThatCandidate(t *testing.T) {
	f := newLeaseFixture(t, 172)
	d := f.withCredential(t, 242)
	brokenEnv := "environment-broken-secret-" + t.Name()
	brokenJob := f.insertJob(t, f.serverID, brokenEnv, "broken-secret-"+t.Name(), 0)
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO secret_heads(id, owner_id, project_id, environment_id, purpose, delivery_scope, revision) VALUES ($1,$2,'project',$3,'runtime','runtime',0)`,
		"secret-head-broken-"+t.Name(), f.ownerID, brokenEnv); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO environment_runtime_bindings(environment_id, variable_name, binding_kind, secret_head_id, delivery_scope) VALUES ($1,'APP_TOKEN','secret',$2,'runtime')`,
		brokenEnv, "secret-head-broken-"+t.Name()); err != nil {
		t.Fatal(err)
	}

	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	leases, err := repo.Claim(context.Background(), f.serverID, d, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].EnvironmentID != f.envID {
		t.Fatalf("leases=%+v", leases)
	}

	var state string
	var attemptCount int
	if err := f.pool.QueryRow(context.Background(), `SELECT state, attempt_count FROM agent_jobs WHERE id=$1`, brokenJob).Scan(&state, &attemptCount); err != nil {
		t.Fatal(err)
	}
	if state != "queued" || attemptCount != 0 {
		t.Fatalf("broken-secret candidate mutated: state=%s attemptCount=%d", state, attemptCount)
	}
}

// TestAgentLeaseClaimRetryFreezesSameSecretVersionAfterRotation is the
// ADR 0003 retry-rule proof: attempt 2 of the same job must freeze the exact
// same secret version attempt 1 froze, even though the bound secret was
// rotated to a new active version in between. Re-resolving would silently
// adopt the rotated version, which the ADR forbids.
func TestAgentLeaseClaimRetryFreezesSameSecretVersionAfterRotation(t *testing.T) {
	f := newLeaseFixture(t, 173)
	d := f.withCredential(t, 243)
	headID := "secret-head-" + t.Name()
	firstVersion := "secret-version-" + t.Name() + "-v1"
	f.insertSecretHeadWithActiveVersion(t, headID, f.envID, "APP_TOKEN", "runtime", firstVersion, 1)
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	ctx := context.Background()

	first, err := repo.Claim(ctx, f.serverID, d, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || len(first[0].SecretBindings) != 1 || first[0].SecretBindings[0].SecretVersionID != firstVersion {
		t.Fatalf("first=%+v", first)
	}

	rotatedVersion := "secret-version-" + t.Name() + "-v2"
	f.rotateSecretVersion(t, headID, f.envID, rotatedVersion, 2)

	f.forceRetry(t, f.jobID, first[0].AttemptID, f.envID)

	second, err := repo.Claim(ctx, f.serverID, d, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || second[0].AttemptNumber != 2 {
		t.Fatalf("second=%+v", second)
	}
	if len(second[0].SecretBindings) != 1 || second[0].SecretBindings[0].SecretVersionID != firstVersion {
		t.Fatalf("attempt 2 froze %+v, want the pre-rotation version %s", second[0].SecretBindings, firstVersion)
	}
	if second[0].ReleaseID != first[0].ReleaseID {
		t.Fatalf("release id changed across retry: first=%s second=%s", first[0].ReleaseID, second[0].ReleaseID)
	}

	var frozenVersionID string
	if err := f.pool.QueryRow(ctx, `SELECT secret_version_id FROM frozen_secret_versions WHERE attempt_id=$1`, second[0].AttemptID).Scan(&frozenVersionID); err != nil {
		t.Fatal(err)
	}
	if frozenVersionID != firstVersion {
		t.Fatalf("frozen_secret_versions for attempt 2 = %s, want %s", frozenVersionID, firstVersion)
	}
}

// TestAgentLeaseClaimRetryDoesNotObserveConfigChange is the companion
// proof for environment_runtime_configs: an administrator's config edit
// between attempt 1 and attempt 2 of the same job must never affect attempt
// 2's frozen agent_job_attempt_execution row or the ClaimedLease it returns.
func TestAgentLeaseClaimRetryDoesNotObserveConfigChange(t *testing.T) {
	f := newLeaseFixture(t, 174)
	d := f.withCredential(t, 244)
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	ctx := context.Background()

	first, err := repo.Claim(ctx, f.serverID, d, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].ContainerPort != 8080 {
		t.Fatalf("first=%+v", first)
	}

	if first[0].HealthGatePath != "/" || first[0].HealthGateHost != "app.internal" {
		t.Fatalf("first health gate path/host = %q/%q, want defaults from insertRuntimeConfig", first[0].HealthGatePath, first[0].HealthGateHost)
	}

	// An administrator's config CAS update, exactly the single-statement
	// shape Step 3d's admin page will issue: bump container_port, the
	// health-gate path/Host, and revision together.
	if _, err := f.pool.Exec(ctx, `UPDATE environment_runtime_configs SET container_port=9090, health_gate_path='/changed', health_gate_host='changed.internal', revision=revision+1, updated_at=clock_timestamp() WHERE environment_id=$1 AND revision=0`, f.envID); err != nil {
		t.Fatal(err)
	}

	f.forceRetry(t, f.jobID, first[0].AttemptID, f.envID)

	second, err := repo.Claim(ctx, f.serverID, d, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || second[0].AttemptNumber != 2 {
		t.Fatalf("second=%+v", second)
	}
	if second[0].ContainerPort != 8080 {
		t.Fatalf("attempt 2 observed the post-attempt-1 config change: containerPort=%d, want 8080", second[0].ContainerPort)
	}
	if second[0].HealthGatePath != "/" || second[0].HealthGateHost != "app.internal" {
		t.Fatalf("attempt 2 observed the post-attempt-1 health gate path/host change: path=%q host=%q, want attempt 1's frozen /,app.internal", second[0].HealthGatePath, second[0].HealthGateHost)
	}

	var containerPort int
	var configRevision int64
	var healthGatePath, healthGateHost string
	if err := f.pool.QueryRow(ctx, `SELECT container_port, environment_runtime_config_revision, health_gate_path, health_gate_host FROM agent_job_attempt_execution WHERE attempt_id=$1`, second[0].AttemptID).Scan(&containerPort, &configRevision, &healthGatePath, &healthGateHost); err != nil {
		t.Fatal(err)
	}
	if containerPort != 8080 || configRevision != 0 {
		t.Fatalf("attempt 2 execution row containerPort=%d configRevision=%d, want 8080/0 (copied from attempt 1)", containerPort, configRevision)
	}
	if healthGatePath != "/" || healthGateHost != "app.internal" {
		t.Fatalf("attempt 2 execution row health gate path/host = %q/%q, want attempt 1's frozen /,app.internal (copied forward, not re-read)", healthGatePath, healthGateHost)
	}
}

// TestAgentLeaseClaimReleaseCreatedOnceReusedAcrossRetries proves the
// releases.UNIQUE(deployment_id) contract end to end through Claim: exactly
// one release row exists for the deployment after two attempts, and both
// attempts' ClaimedLease report the same release id.
func TestAgentLeaseClaimReleaseCreatedOnceReusedAcrossRetries(t *testing.T) {
	f := newLeaseFixture(t, 175)
	d := f.withCredential(t, 245)
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	ctx := context.Background()

	first, err := repo.Claim(ctx, f.serverID, d, 5)
	if err != nil {
		t.Fatal(err)
	}
	f.forceRetry(t, f.jobID, first[0].AttemptID, f.envID)
	second, err := repo.Claim(ctx, f.serverID, d, 5)
	if err != nil {
		t.Fatal(err)
	}
	if first[0].ReleaseID == "" || first[0].ReleaseID != second[0].ReleaseID {
		t.Fatalf("first=%s second=%s", first[0].ReleaseID, second[0].ReleaseID)
	}
	var releaseCount int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM releases WHERE deployment_id=$1`, first[0].DeploymentID).Scan(&releaseCount); err != nil {
		t.Fatal(err)
	}
	if releaseCount != 1 {
		t.Fatalf("release rows=%d, want 1", releaseCount)
	}
}

// TestEnsureReleaseRejectsConfigDigestMismatch exercises ensureRelease
// directly (it is package-internal, and migration 000014's release identity
// trigger makes config_digest immutable once stored -- see the trigger's
// "release identity is immutable" check on config_digest -- so this
// disagreement can only be produced by feeding ensureRelease mismatched
// inputs directly, never by mutating a stored row through SQL). This proves
// the corrupt-state guard fires instead of silently accepting or
// overwriting a disagreeing digest, and that a repeated call with the
// originally stored digest continues to succeed and reuse the same row.
func TestEnsureReleaseRejectsConfigDigestMismatch(t *testing.T) {
	f := newLeaseFixture(t, 176)
	ctx := context.Background()

	var createdAt time.Time
	if err := f.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&createdAt); err != nil {
		t.Fatal(err)
	}
	depID := "deployment-ensure-release-" + t.Name()
	dep, err := deployment.New(deployment.Identity{DeploymentID: deployment.ID(depID), ProjectID: "project", EnvironmentID: f.envID, ServerID: f.serverID, CommitSHA: "0123456789abcdef0123456789abcdef01234567"}, createdAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := NewDeploymentRepository(f.pool).Create(ctx, dep); err != nil {
		t.Fatal(err)
	}

	digestA := [32]byte{1, 2, 3}
	digestB := [32]byte{9, 9, 9}

	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	releaseID, err := ensureRelease(ctx, tx, depID, f.envID, "0123456789abcdef0123456789abcdef01234567", "stop_then_start", digestA)
	if err != nil {
		t.Fatal(err)
	}
	if releaseID != depID {
		t.Fatalf("releaseID=%s, want deployment id %s", releaseID, depID)
	}

	if _, err := ensureRelease(ctx, tx, depID, f.envID, "0123456789abcdef0123456789abcdef01234567", "stop_then_start", digestB); !errors.Is(err, ErrAgentLeaseCorruptState) {
		t.Fatalf("mismatched digest err=%v, want ErrAgentLeaseCorruptState", err)
	}

	reusedID, err := ensureRelease(ctx, tx, depID, f.envID, "0123456789abcdef0123456789abcdef01234567", "stop_then_start", digestA)
	if err != nil {
		t.Fatalf("reuse with matching digest: %v", err)
	}
	if reusedID != releaseID {
		t.Fatalf("reusedID=%s, want %s", reusedID, releaseID)
	}

	var releaseCount int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM releases WHERE deployment_id=$1`, depID).Scan(&releaseCount); err != nil {
		t.Fatal(err)
	}
	if releaseCount != 1 {
		t.Fatalf("release rows=%d, want 1", releaseCount)
	}
}

// TestAgentLeaseClaimInjectedFailureRollsBackReleaseStage extends the
// existing per-stage rollback matrix with the new "release" injection point:
// a failure there must roll back the whole transaction, including the
// attempt/execution/frozen-secret-version rows already written earlier in
// this same tryClaim call.
func TestAgentLeaseClaimInjectedFailureRollsBackReleaseStage(t *testing.T) {
	f := newLeaseFixture(t, 177)
	d := f.withCredential(t, 246)
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	injected := errors.New("injected failure at release")
	repo.failAfter = func(s string) error {
		if s == "release" {
			return injected
		}
		return nil
	}
	if _, err := repo.Claim(context.Background(), f.serverID, d, 5); !errors.Is(err, injected) {
		t.Fatalf("err=%v", err)
	}
	assertJobUntouched(t, f)
	var releaseCount int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM releases WHERE deployment_id=$1`, "lease-deployment-"+t.Name()).Scan(&releaseCount); err != nil {
		t.Fatal(err)
	}
	if releaseCount != 0 {
		t.Fatalf("release rows despite rollback=%d", releaseCount)
	}
}

// TestAgentLeaseClaimRacesAdministratorConfigUpdateNoDeadlock proves the ADR
// 0004 lock-hierarchy position "1b" placement: many concurrent Claim calls
// (each against its own fresh job/environment) racing many concurrent
// administrator-style config CAS updates against those same environments
// must never deadlock and must never pathologically block -- every
// goroutine completes, and every error, if any, is an expected CAS
// conflict (mismatched revision), never a Postgres deadlock (40P01) or lock
// timeout. Run with -race -count=20 so repeated runs sample many interleavings.
func TestAgentLeaseClaimRacesAdministratorConfigUpdateNoDeadlock(t *testing.T) {
	f := newLeaseFixture(t, 178)
	d := f.withCredential(t, 247)
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	ctx := context.Background()

	const environments = 6
	envIDs := make([]string, environments)
	for i := 0; i < environments; i++ {
		envIDs[i] = "environment-race-" + t.Name() + "-" + string(rune('a'+i))
		f.insertJob(t, f.serverID, envIDs[i], "race-"+t.Name()+"-"+string(rune('a'+i)), 0)
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	claimErrs := make([]error, environments)
	updateErrs := make([]error, environments)

	for i := 0; i < environments; i++ {
		i := i
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, err := repo.Claim(ctx, f.serverID, d, 10)
			claimErrs[i] = err
		}()
		go func() {
			defer wg.Done()
			<-start
			_, err := f.pool.Exec(ctx, `UPDATE environment_runtime_configs SET container_port=container_port+1, revision=revision+1, updated_at=clock_timestamp() WHERE environment_id=$1 AND revision=0`, envIDs[i])
			updateErrs[i] = err
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range claimErrs {
		if err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
	}
	for i, err := range updateErrs {
		if err != nil {
			t.Fatalf("admin update %d: %v", i, err)
		}
	}

	// Every environment's job must have been claimed exactly once (the
	// admin update targets only the config row, never the job/slot), and
	// every config update must have taken effect despite racing a claim.
	for i, envID := range envIDs {
		var attemptCount int
		if err := f.pool.QueryRow(ctx, `SELECT attempt_count FROM agent_jobs WHERE environment_id=$1`, envID).Scan(&attemptCount); err != nil {
			t.Fatal(err)
		}
		if attemptCount != 1 {
			t.Fatalf("environment %d (%s) attemptCount=%d, want 1", i, envID, attemptCount)
		}
		var port int
		if err := f.pool.QueryRow(ctx, `SELECT container_port FROM environment_runtime_configs WHERE environment_id=$1`, envID).Scan(&port); err != nil {
			t.Fatal(err)
		}
		if port != 8081 {
			t.Fatalf("environment %d (%s) container_port=%d, want 8081 (admin update applied)", i, envID, port)
		}
	}
}

// TestAgentLeaseClaimNoSecretValueInClaimedLease proves the ClaimedLease a
// Claim call returns carries only secret-version identifiers, never a
// value: the bound secret's ciphertext/wrapped_dek bytes this test itself
// inserted never appear anywhere reachable from the returned struct.
func TestAgentLeaseClaimNoSecretValueInClaimedLease(t *testing.T) {
	f := newLeaseFixture(t, 179)
	d := f.withCredential(t, 248)
	f.insertSecretHeadWithActiveVersion(t, "secret-head-"+t.Name(), f.envID, "APP_TOKEN", "runtime", "secret-version-"+t.Name(), 1)
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))

	leases, err := repo.Claim(context.Background(), f.serverID, d, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || len(leases[0].SecretBindings) != 1 {
		t.Fatalf("leases=%+v", leases)
	}
	b := leases[0].SecretBindings[0]
	if b.SecretVersionID == "" || b.VariableName == "" || b.DeliveryScope == "" {
		t.Fatalf("binding missing identifiers: %+v", b)
	}
	// The only fields on SecretBinding are these three identifiers -- there
	// is no field capable of carrying a value at all -- but assert the
	// version id string itself (the one value this test controls precisely)
	// never contains the hex-encoded ciphertext/wrapped_dek markers this
	// fixture inserted, guarding against a future field addition silently
	// concatenating a value in.
	if strings.Contains(b.SecretVersionID, "abababab") || strings.Contains(b.SecretVersionID, "cdcdcdcd") {
		t.Fatalf("secret version id leaks ciphertext material: %s", b.SecretVersionID)
	}
}

// insertLiteralBinding adds a literal_nonsecret binding, the kind migration
// 000015 gave a value column so it stops being a silent no-op.
func (f leaseFixture) insertLiteralBinding(t *testing.T, environmentID, variableName, deliveryScope, value string) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO environment_runtime_bindings(environment_id, variable_name, binding_kind, delivery_scope, literal_value) VALUES ($1,$2,'literal_nonsecret',$3,$4)`, environmentID, variableName, deliveryScope, value); err != nil {
		t.Fatal(err)
	}
}

func TestAgentLeaseClaimFreezesLiteralBinding(t *testing.T) {
	f := newLeaseFixture(t, 181)
	d := f.withCredential(t, 251)
	f.insertLiteralBinding(t, f.envID, "NODE_ENV", "runtime", "production")
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	ctx := context.Background()

	leases, err := repo.Claim(ctx, f.serverID, d, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 {
		t.Fatalf("leases=%+v", leases)
	}
	if len(leases[0].LiteralBindings) != 1 {
		t.Fatalf("literal bindings=%+v, want exactly one", leases[0].LiteralBindings)
	}
	got := leases[0].LiteralBindings[0]
	if got.VariableName != "NODE_ENV" || got.DeliveryScope != "runtime" || got.Value != "production" {
		t.Fatalf("literal binding=%+v", got)
	}

	var value, scope string
	if err := f.pool.QueryRow(ctx, `SELECT literal_value, delivery_scope FROM frozen_literal_values WHERE attempt_id=$1 AND variable_name='NODE_ENV'`, leases[0].AttemptID).Scan(&value, &scope); err != nil {
		t.Fatal(err)
	}
	if value != "production" || scope != "runtime" {
		t.Fatalf("frozen literal = %s/%s", value, scope)
	}
}

// TestAgentLeaseClaimRetryDoesNotObserveLiteralChange is the literal
// counterpart of the secret-rotation and config-change retry tests: an
// administrator editing a literal between attempts must not change what a
// retry executes, so one attempt stays one provable snapshot.
func TestAgentLeaseClaimRetryDoesNotObserveLiteralChange(t *testing.T) {
	f := newLeaseFixture(t, 182)
	d := f.withCredential(t, 252)
	f.insertLiteralBinding(t, f.envID, "FEATURE_FLAGS", "runtime", "alpha")
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	ctx := context.Background()

	first, err := repo.Claim(ctx, f.serverID, d, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || len(first[0].LiteralBindings) != 1 || first[0].LiteralBindings[0].Value != "alpha" {
		t.Fatalf("first=%+v", first)
	}

	if _, err := f.pool.Exec(ctx, `UPDATE environment_runtime_bindings SET literal_value='beta' WHERE environment_id=$1 AND variable_name='FEATURE_FLAGS'`, f.envID); err != nil {
		t.Fatal(err)
	}

	f.forceRetry(t, f.jobID, first[0].AttemptID, f.envID)

	second, err := repo.Claim(ctx, f.serverID, d, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || second[0].AttemptNumber != 2 {
		t.Fatalf("second=%+v", second)
	}
	if len(second[0].LiteralBindings) != 1 || second[0].LiteralBindings[0].Value != "alpha" {
		t.Fatalf("attempt 2 observed %+v, want the pre-edit value alpha", second[0].LiteralBindings)
	}
	var frozen string
	if err := f.pool.QueryRow(ctx, `SELECT literal_value FROM frozen_literal_values WHERE attempt_id=$1 AND variable_name='FEATURE_FLAGS'`, second[0].AttemptID).Scan(&frozen); err != nil {
		t.Fatal(err)
	}
	if frozen != "alpha" {
		t.Fatalf("frozen literal for attempt 2 = %s, want alpha", frozen)
	}
	if second[0].ReleaseID != first[0].ReleaseID {
		t.Fatalf("release id changed across retry: %s -> %s", first[0].ReleaseID, second[0].ReleaseID)
	}
}

func TestAgentLeaseClaimFreezesMixedSecretAndLiteralBindings(t *testing.T) {
	f := newLeaseFixture(t, 183)
	d := f.withCredential(t, 253)
	headID := "secret-head-" + t.Name()
	versionID := "secret-version-" + t.Name()
	f.insertSecretHeadWithActiveVersion(t, headID, f.envID, "APP_TOKEN", "runtime", versionID, 1)
	f.insertLiteralBinding(t, f.envID, "LOG_LEVEL", "runtime", "info")
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))

	leases, err := repo.Claim(context.Background(), f.serverID, d, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 {
		t.Fatalf("leases=%+v", leases)
	}
	if len(leases[0].SecretBindings) != 1 || leases[0].SecretBindings[0].SecretVersionID != versionID {
		t.Fatalf("secret bindings=%+v", leases[0].SecretBindings)
	}
	if len(leases[0].LiteralBindings) != 1 || leases[0].LiteralBindings[0].Value != "info" {
		t.Fatalf("literal bindings=%+v", leases[0].LiteralBindings)
	}
}

// TestAgentLeaseClaimLiteralValueChangesConfigDigest proves a literal is
// genuinely part of the release's config_digest: two environments identical
// except for one literal value must not produce the same digest, or a later
// slice could not tell two different frozen configurations apart.
func TestAgentLeaseClaimLiteralValueChangesConfigDigest(t *testing.T) {
	f := newLeaseFixture(t, 184)
	d := f.withCredential(t, 254)
	f.insertLiteralBinding(t, f.envID, "TUNABLE", "runtime", "one")

	otherEnv := f.envID + "-other"
	f.insertRuntimeConfig(t, otherEnv)
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO environment_slots(environment_id) VALUES ($1) ON CONFLICT DO NOTHING`, otherEnv); err != nil {
		t.Fatal(err)
	}
	f.insertLiteralBinding(t, otherEnv, "TUNABLE", "runtime", "two")
	otherJobID := f.insertJob(t, f.serverID, otherEnv, "job-other-"+t.Name(), 0)
	_ = otherJobID

	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	ctx := context.Background()
	leases, err := repo.Claim(ctx, f.serverID, d, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 2 {
		t.Fatalf("expected both environments claimed, got %d", len(leases))
	}
	digests := make(map[string]struct{})
	for _, l := range leases {
		var digest []byte
		if err := f.pool.QueryRow(ctx, `SELECT config_digest FROM releases WHERE id=$1`, l.ReleaseID).Scan(&digest); err != nil {
			t.Fatal(err)
		}
		digests[string(digest)] = struct{}{}
	}
	if len(digests) != 2 {
		t.Fatal("two environments differing only in one literal value produced the same config_digest")
	}
}
