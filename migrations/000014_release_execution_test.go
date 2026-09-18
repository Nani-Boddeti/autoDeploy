//go:build integration

package migrations

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// connectAndApply opens a fresh connection, applies every migration twice
// (proving idempotent ordering on top of what TestApplyConcurrentAndChecksum
// already covers), and returns the connection.
func connectAndApply(t *testing.T) *pgx.Conn {
	t.Helper()
	dsn := os.Getenv("AUTODEPLOY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AUTODEPLOY_TEST_DATABASE_URL is required for PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(ctx) })
	if err := Apply(ctx, conn); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if err := Apply(ctx, conn); err != nil {
		t.Fatalf("second apply (idempotency): %v", err)
	}
	return conn
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(buf)
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	return buf
}

func randomHexSHA(t *testing.T) string {
	t.Helper()
	return hex.EncodeToString(randomBytes(t, 20))
}

// releaseFixture is the full chain of rows Step 1's tables reference:
// owner -> server -> environment_slot -> deployment -> agent_job (leased,
// attempt_count=1) -> agent_job_attempts (fence=1) -> secret_heads/versions.
type releaseFixture struct {
	ownerID       string
	serverID      string
	environmentID string
	deploymentID  string
	jobID         int64
	attemptID     int64
	secretHeadID  string
	secretVerID   string
}

func buildReleaseFixture(t *testing.T, ctx context.Context, tx pgx.Tx) releaseFixture {
	t.Helper()
	suffix := randomSuffix(t)
	f := releaseFixture{
		ownerID:       "owner-" + suffix,
		serverID:      "server-" + suffix,
		environmentID: "env-" + suffix,
		deploymentID:  "deploy-" + suffix,
	}
	exec := func(sql string, args ...any) {
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("fixture setup %q: %v", sql, err)
		}
	}
	exec(`INSERT INTO owners (id) VALUES ($1)`, f.ownerID)
	exec(`INSERT INTO servers (id, owner_id, name, protocol_version, status) VALUES ($1,$2,$3,1,'active')`, f.serverID, f.ownerID, "server-"+suffix)
	exec(`INSERT INTO environment_slots (environment_id) VALUES ($1)`, f.environmentID)
	now := time.Now().UTC()
	commitSHA := randomHexSHA(t)
	exec(`INSERT INTO deployments (id, project_id, environment_id, server_id, commit_sha, schema_version, status, created_at, updated_at, revision)
	      VALUES ($1,$2,$3,$4,$5,1,'queued',$6,$6,1)`, f.deploymentID, "project-"+suffix, f.environmentID, f.serverID, commitSHA, now)

	var jobID int64
	if err := tx.QueryRow(ctx, `INSERT INTO agent_jobs
	      (schema_version, job_kind, owner_id, project_id, environment_id, server_id, deployment_id, repository_id, full_ref, commit_sha, priority, available_at, idempotency_key, payload_digest, state)
	      VALUES (1,'deployment_execution',$1,$2,$3,$4,$5,1,'refs/heads/main',$6,0,$7,$8,$9,'queued') RETURNING id`,
		f.ownerID, "project-"+suffix, f.environmentID, f.serverID, f.deploymentID, commitSHA, now, "idem-"+suffix, randomBytes(t, 32)).Scan(&jobID); err != nil {
		t.Fatalf("insert agent_job: %v", err)
	}
	f.jobID = jobID

	exec(`UPDATE environment_slots SET fence = fence + 1 WHERE environment_id = $1`, f.environmentID)
	exec(`UPDATE agent_jobs SET state='leased', attempt_count=1 WHERE id=$1`, f.jobID)

	var attemptID int64
	if err := tx.QueryRow(ctx, `INSERT INTO agent_job_attempts
	      (job_id, attempt_number, server_id, environment_id, fence, state, lease_expires_at, authority_expires_at)
	      VALUES ($1,1,$2,$3,1,'leased',$4,$4) RETURNING id`,
		f.jobID, f.serverID, f.environmentID, now.Add(time.Hour)).Scan(&attemptID); err != nil {
		t.Fatalf("insert agent_job_attempt: %v", err)
	}
	f.attemptID = attemptID

	f.secretHeadID = "secret-" + suffix
	f.secretVerID = "secretver-" + suffix
	exec(`INSERT INTO secret_heads (id, owner_id, project_id, environment_id, purpose, delivery_scope) VALUES ($1,$2,$3,$4,'runtime','runtime')`,
		f.secretHeadID, f.ownerID, "project-"+suffix, f.environmentID)
	exec(`INSERT INTO secret_versions (id, secret_id, owner_id, project_id, environment_id, version_number, state, suite, aad_version, kek_id, ciphertext, wrapped_dek)
	      VALUES ($1,$2,$3,$4,$5,1,'active','AES-256-GCM-RANDOMNONCE-v1','v1','kek-test',$6,$7)`,
		f.secretVerID, f.secretHeadID, f.ownerID, "project-"+suffix, f.environmentID, randomBytes(t, 32), randomBytes(t, 60))
	exec(`UPDATE secret_heads SET current_version_id=$1, revision=1 WHERE id=$2`, f.secretVerID, f.secretHeadID)

	return f
}

func beginTx(t *testing.T, ctx context.Context, conn *pgx.Conn) pgx.Tx {
	t.Helper()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback(ctx) }) //nolint:errcheck
	return tx
}

// mustFail runs sql inside a SAVEPOINT and asserts it fails. Postgres aborts
// an entire transaction on the first error, so without the savepoint every
// later statement in the same tx would fail too -- not because it re-proved
// the invariant, but only because the transaction was already dead. Rolling
// back to the savepoint restores the transaction to a live, usable state so
// the next assertion in the same test genuinely exercises its own statement.
var savepointCounter int

func mustFail(t *testing.T, ctx context.Context, tx pgx.Tx, msg, sql string, args ...any) {
	t.Helper()
	savepointCounter++
	sp := fmt.Sprintf("sp_%d", savepointCounter)
	if _, err := tx.Exec(ctx, "SAVEPOINT "+sp); err != nil {
		t.Fatalf("create savepoint: %v", err)
	}
	if _, err := tx.Exec(ctx, sql, args...); err == nil {
		t.Fatal(msg)
	}
	if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT "+sp); err != nil {
		t.Fatalf("rollback to savepoint: %v", err)
	}
}

func TestEnvironmentRuntimeConfigCAS(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	envID := "env-cfg-" + randomSuffix(t)
	if _, err := tx.Exec(ctx, `INSERT INTO environment_runtime_configs
	      (environment_id, container_port, container_scheme, release_strategy, drain_seconds, cpu_millis, memory_bytes, restart_policy, health_policy_revision)
	      VALUES ($1,8080,'http','blue_green',30,500,268435456,'unless-stopped',1)`, envID); err != nil {
		t.Fatalf("insert config: %v", err)
	}

	// Correct-revision CAS update succeeds and advances revision.
	tag, err := tx.Exec(ctx, `UPDATE environment_runtime_configs SET container_port=8081, revision=revision+1 WHERE environment_id=$1 AND revision=0`, envID)
	if err != nil {
		t.Fatalf("cas update: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("expected 1 row affected, got %d", tag.RowsAffected())
	}

	// Stale-revision CAS update affects zero rows.
	tag, err = tx.Exec(ctx, `UPDATE environment_runtime_configs SET container_port=8082, revision=revision+1 WHERE environment_id=$1 AND revision=0`, envID)
	if err != nil {
		t.Fatalf("stale cas update: %v", err)
	}
	if tag.RowsAffected() != 0 {
		t.Fatalf("expected 0 rows affected for stale revision, got %d", tag.RowsAffected())
	}

	// A matched row that forgets to advance revision is rejected by the trigger.
	mustFail(t, ctx, tx, "expected rejection of update that does not advance revision",
		`UPDATE environment_runtime_configs SET container_port=8083 WHERE environment_id=$1 AND revision=1`, envID)

	// Identity is immutable.
	mustFail(t, ctx, tx, "expected rejection of environment_id change",
		`UPDATE environment_runtime_configs SET environment_id='other', revision=revision+1 WHERE environment_id=$1 AND revision=1`, envID)

	// Retained: delete is rejected.
	mustFail(t, ctx, tx, "expected delete rejection",
		`DELETE FROM environment_runtime_configs WHERE environment_id=$1`, envID)
}

func TestEnvironmentRuntimeBindingsCheck(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f := buildReleaseFixture(t, ctx, tx)

	// secret kind with a secret head: allowed.
	if _, err := tx.Exec(ctx, `INSERT INTO environment_runtime_bindings (environment_id, variable_name, binding_kind, secret_head_id, delivery_scope)
	      VALUES ($1,'DATABASE_URL','secret',$2,'runtime')`, f.environmentID, f.secretHeadID); err != nil {
		t.Fatalf("expected secret binding to succeed: %v", err)
	}

	// literal kind with no secret head: allowed.
	if _, err := tx.Exec(ctx, `INSERT INTO environment_runtime_bindings (environment_id, variable_name, binding_kind, secret_head_id, delivery_scope)
	      VALUES ($1,'PORT','literal_nonsecret',NULL,'runtime')`, f.environmentID); err != nil {
		t.Fatalf("expected literal binding to succeed: %v", err)
	}

	// secret kind with no secret head: rejected.
	mustFail(t, ctx, tx, "expected rejection of secret binding with no secret_head_id",
		`INSERT INTO environment_runtime_bindings (environment_id, variable_name, binding_kind, secret_head_id, delivery_scope)
	      VALUES ($1,'MISSING_HEAD','secret',NULL,'runtime')`, f.environmentID)

	// literal kind with a secret head set: rejected.
	mustFail(t, ctx, tx, "expected rejection of literal binding with a secret_head_id",
		`INSERT INTO environment_runtime_bindings (environment_id, variable_name, binding_kind, secret_head_id, delivery_scope)
	      VALUES ($1,'SPURIOUS_HEAD','literal_nonsecret',$2,'runtime')`, f.environmentID, f.secretHeadID)
}

func TestFrozenSecretVersionsWidenedPrimaryKeyAndAppendOnly(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f := buildReleaseFixture(t, ctx, tx)

	// Same attempt+secret version, two distinct variable names: both succeed
	// under the widened (attempt_id, secret_version_id, variable_name) PK.
	if _, err := tx.Exec(ctx, `INSERT INTO frozen_secret_versions (attempt_id, secret_version_id, variable_name, delivery_scope)
	      VALUES ($1,$2,'DATABASE_URL','runtime')`, f.attemptID, f.secretVerID); err != nil {
		t.Fatalf("insert first frozen version: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO frozen_secret_versions (attempt_id, secret_version_id, variable_name, delivery_scope)
	      VALUES ($1,$2,'DATABASE_URL_RO','build')`, f.attemptID, f.secretVerID); err != nil {
		t.Fatalf("insert second frozen version with distinct variable_name: %v", err)
	}
	// A duplicate (same attempt, secret version, and variable name) still
	// collides on the widened PK.
	mustFail(t, ctx, tx, "expected PK collision on duplicate (attempt_id, secret_version_id, variable_name)",
		`INSERT INTO frozen_secret_versions (attempt_id, secret_version_id, variable_name, delivery_scope)
	      VALUES ($1,$2,'DATABASE_URL','runtime')`, f.attemptID, f.secretVerID)

	mustFail(t, ctx, tx, "expected update rejection (append-only)",
		`UPDATE frozen_secret_versions SET delivery_scope='build' WHERE attempt_id=$1 AND secret_version_id=$2 AND variable_name='DATABASE_URL'`, f.attemptID, f.secretVerID)
	mustFail(t, ctx, tx, "expected delete rejection (append-only)",
		`DELETE FROM frozen_secret_versions WHERE attempt_id=$1 AND secret_version_id=$2 AND variable_name='DATABASE_URL'`, f.attemptID, f.secretVerID)
}

func TestAgentJobAttemptExecutionInsertOnce(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f := buildReleaseFixture(t, ctx, tx)

	if _, err := tx.Exec(ctx, `INSERT INTO agent_job_attempt_execution
	      (attempt_id, container_port, container_scheme, release_strategy, drain_seconds, health_policy_revision, cpu_millis, memory_bytes, restart_policy, environment_runtime_config_revision)
	      VALUES ($1,8080,'http','blue_green',30,1,500,268435456,'unless-stopped',0)`, f.attemptID); err != nil {
		t.Fatalf("insert execution row: %v", err)
	}

	mustFail(t, ctx, tx, "expected update rejection (insert-once)",
		`UPDATE agent_job_attempt_execution SET container_port=8081 WHERE attempt_id=$1`, f.attemptID)
	mustFail(t, ctx, tx, "expected delete rejection (insert-once)",
		`DELETE FROM agent_job_attempt_execution WHERE attempt_id=$1`, f.attemptID)
	mustFail(t, ctx, tx, "expected update rejection of drain_seconds (insert-once)",
		`UPDATE agent_job_attempt_execution SET drain_seconds=60 WHERE attempt_id=$1`, f.attemptID)
}

// TestAgentJobAttemptExecutionDrainSecondsBounds proves drain_seconds is
// frozen with the same CHECK bounds as environment_runtime_configs
// (1..300): out-of-range values are rejected at insert, and an in-range
// value is both accepted and immutable thereafter.
func TestAgentJobAttemptExecutionDrainSecondsBounds(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f := buildReleaseFixture(t, ctx, tx)

	mustFail(t, ctx, tx, "expected rejection of drain_seconds=0",
		`INSERT INTO agent_job_attempt_execution
	      (attempt_id, container_port, container_scheme, release_strategy, drain_seconds, health_policy_revision, cpu_millis, memory_bytes, restart_policy, environment_runtime_config_revision)
	      VALUES ($1,8080,'http','blue_green',0,1,500,268435456,'unless-stopped',0)`, f.attemptID)
	mustFail(t, ctx, tx, "expected rejection of drain_seconds=301",
		`INSERT INTO agent_job_attempt_execution
	      (attempt_id, container_port, container_scheme, release_strategy, drain_seconds, health_policy_revision, cpu_millis, memory_bytes, restart_policy, environment_runtime_config_revision)
	      VALUES ($1,8080,'http','blue_green',301,1,500,268435456,'unless-stopped',0)`, f.attemptID)

	if _, err := tx.Exec(ctx, `INSERT INTO agent_job_attempt_execution
	      (attempt_id, container_port, container_scheme, release_strategy, drain_seconds, health_policy_revision, cpu_millis, memory_bytes, restart_policy, environment_runtime_config_revision)
	      VALUES ($1,8080,'http','blue_green',300,1,500,268435456,'unless-stopped',0)`, f.attemptID); err != nil {
		t.Fatalf("insert execution row with drain_seconds=300: %v", err)
	}
	var drain int
	if err := tx.QueryRow(ctx, `SELECT drain_seconds FROM agent_job_attempt_execution WHERE attempt_id=$1`, f.attemptID).Scan(&drain); err != nil {
		t.Fatalf("read drain_seconds: %v", err)
	}
	if drain != 300 {
		t.Fatalf("expected drain_seconds=300, got %d", drain)
	}
}

// insertRelease creates a fresh deployment row (releases.deployment_id is
// UNIQUE, so a test exercising several releases against one fixture must not
// reuse the fixture's own deployment) and a release against it.
func insertRelease(t *testing.T, ctx context.Context, tx pgx.Tx, f releaseFixture, state string) string {
	t.Helper()
	suffix := randomSuffix(t)
	deploymentID := "deploy-" + suffix
	now := time.Now().UTC()
	if _, err := tx.Exec(ctx, `INSERT INTO deployments (id, project_id, environment_id, server_id, commit_sha, schema_version, status, created_at, updated_at, revision)
	      VALUES ($1,'project-x',$2,$3,$4,1,'queued',$5,$5,1)`,
		deploymentID, f.environmentID, f.serverID, randomHexSHA(t), now); err != nil {
		t.Fatalf("insert release deployment: %v", err)
	}
	releaseID := "release-" + suffix
	// image_digest is deliberately left NULL: a release is opened before its
	// build runs, so the digest is set later by a write-once update.
	if _, err := tx.Exec(ctx, `INSERT INTO releases (id, deployment_id, environment_id, config_digest, commit_sha, strategy, state)
	      VALUES ($1,$2,$3,$4,$5,'blue_green',$6)`,
		releaseID, deploymentID, f.environmentID, randomBytes(t, 32), randomHexSHA(t), state); err != nil {
		t.Fatalf("insert release: %v", err)
	}
	return releaseID
}

// setImageDigest performs the write-once image-digest set a real build does
// when it finishes, which a release must have before it may become ready.
func setImageDigest(t *testing.T, ctx context.Context, tx pgx.Tx, releaseID string) {
	t.Helper()
	if _, err := tx.Exec(ctx, `UPDATE releases SET image_digest=$2, updated_at=clock_timestamp() WHERE id=$1`, releaseID, randomBytes(t, 32)); err != nil {
		t.Fatalf("set image digest: %v", err)
	}
}

func TestReleaseStateTransitions(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f := buildReleaseFixture(t, ctx, tx)

	// Legal chain: candidate -> ready -> removing -> removed.
	releaseID := insertRelease(t, ctx, tx, f, "candidate")
	setImageDigest(t, ctx, tx, releaseID)
	if _, err := tx.Exec(ctx, `UPDATE releases SET state='ready' WHERE id=$1`, releaseID); err != nil {
		t.Fatalf("candidate->ready: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE releases SET state='removing' WHERE id=$1`, releaseID); err != nil {
		t.Fatalf("ready->removing: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE releases SET state='removed' WHERE id=$1`, releaseID); err != nil {
		t.Fatalf("removing->removed: %v", err)
	}
	// Terminal: removed cannot transition further.
	mustFail(t, ctx, tx, "expected rejection of transition out of removed",
		`UPDATE releases SET state='candidate' WHERE id=$1`, releaseID)

	// Legal: candidate -> failed directly.
	releaseID2 := insertRelease(t, ctx, tx, f, "candidate")
	if _, err := tx.Exec(ctx, `UPDATE releases SET state='failed' WHERE id=$1`, releaseID2); err != nil {
		t.Fatalf("candidate->failed: %v", err)
	}
	// Terminal: failed cannot transition further.
	mustFail(t, ctx, tx, "expected rejection of transition out of failed",
		`UPDATE releases SET state='removing' WHERE id=$1`, releaseID2)

	// Illegal: skipping straight from candidate to removed.
	releaseID3 := insertRelease(t, ctx, tx, f, "candidate")
	mustFail(t, ctx, tx, "expected rejection of candidate->removed skip",
		`UPDATE releases SET state='removed' WHERE id=$1`, releaseID3)

	// Illegal: ready back to candidate.
	releaseID4 := insertRelease(t, ctx, tx, f, "candidate")
	setImageDigest(t, ctx, tx, releaseID4)
	if _, err := tx.Exec(ctx, `UPDATE releases SET state='ready' WHERE id=$1`, releaseID4); err != nil {
		t.Fatalf("candidate->ready: %v", err)
	}
	mustFail(t, ctx, tx, "expected rejection of ready->candidate",
		`UPDATE releases SET state='candidate' WHERE id=$1`, releaseID4)

	// active/retained do not exist yet: rejected by the state CHECK itself.
	releaseID5 := insertRelease(t, ctx, tx, f, "candidate")
	mustFail(t, ctx, tx, "expected rejection of transition to 'active'",
		`UPDATE releases SET state='active' WHERE id=$1`, releaseID5)
	mustFail(t, ctx, tx, "expected rejection of transition to 'retained'",
		`UPDATE releases SET state='retained' WHERE id=$1`, releaseID5)

	// Identity is immutable.
	mustFail(t, ctx, tx, "expected rejection of environment_id change",
		`UPDATE releases SET environment_id='other', state='ready' WHERE id=$1`, releaseID5)

	// Retained: delete is rejected.
	mustFail(t, ctx, tx, "expected delete rejection",
		`DELETE FROM releases WHERE id=$1`, releaseID)
}

func TestReleaseImageDigestWriteOnceAndGatesReady(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f := buildReleaseFixture(t, ctx, tx)
	releaseID := insertRelease(t, ctx, tx, f, "candidate")

	// A release opened before its build has no digest and cannot become ready.
	mustFail(t, ctx, tx, "expected rejection of ready without an image digest",
		`UPDATE releases SET state='ready' WHERE id=$1`, releaseID)

	setImageDigest(t, ctx, tx, releaseID)

	// Set once, never again: neither a different digest nor clearing it.
	mustFail(t, ctx, tx, "expected rejection of a second image digest",
		`UPDATE releases SET image_digest=$2, updated_at=clock_timestamp() WHERE id=$1`, releaseID, randomBytes(t, 32))
	mustFail(t, ctx, tx, "expected rejection of clearing the image digest",
		`UPDATE releases SET image_digest=NULL, updated_at=clock_timestamp() WHERE id=$1`, releaseID)

	// With a digest present, ready is allowed.
	if _, err := tx.Exec(ctx, `UPDATE releases SET state='ready' WHERE id=$1`, releaseID); err != nil {
		t.Fatalf("candidate->ready with digest: %v", err)
	}

	// A release may still fail without ever producing an image.
	releaseID2 := insertRelease(t, ctx, tx, f, "candidate")
	if _, err := tx.Exec(ctx, `UPDATE releases SET state='failed' WHERE id=$1`, releaseID2); err != nil {
		t.Fatalf("candidate->failed without digest: %v", err)
	}
}

func TestReleaseRuntimeInstanceFenceGatedObservedState(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f := buildReleaseFixture(t, ctx, tx)
	releaseID := insertRelease(t, ctx, tx, f, "candidate")

	if _, err := tx.Exec(ctx, `INSERT INTO release_runtime_instances (release_id, job_id, attempt_id, fence, container_name, observed_state)
	      VALUES ($1,$2,$3,1,$4,'created')`, releaseID, f.jobID, f.attemptID, "autodeploy-"+releaseID); err != nil {
		t.Fatalf("insert runtime instance: %v", err)
	}

	// A write carrying the attempt's own fence changes observed_state.
	tag, err := tx.Exec(ctx, `UPDATE release_runtime_instances SET observed_state='running' WHERE attempt_id=$1 AND fence=1`, f.attemptID)
	if err != nil {
		t.Fatalf("fence-matched update: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("expected 1 row affected, got %d", tag.RowsAffected())
	}

	// A write carrying a stale/foreign fence matches nothing.
	tag, err = tx.Exec(ctx, `UPDATE release_runtime_instances SET observed_state='stopped' WHERE attempt_id=$1 AND fence=999`, f.attemptID)
	if err != nil {
		t.Fatalf("fence-mismatched update: %v", err)
	}
	if tag.RowsAffected() != 0 {
		t.Fatalf("expected 0 rows affected for mismatched fence, got %d", tag.RowsAffected())
	}

	// Any other column is immutable even with the correct fence.
	mustFail(t, ctx, tx, "expected rejection of container_name change",
		`UPDATE release_runtime_instances SET container_name='renamed', observed_state='running' WHERE attempt_id=$1 AND fence=1`, f.attemptID)

	// Retained: delete is rejected.
	mustFail(t, ctx, tx, "expected delete rejection",
		`DELETE FROM release_runtime_instances WHERE attempt_id=$1`, f.attemptID)
}

func TestReleaseOperationsAppendOnlyAndClose(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f := buildReleaseFixture(t, ctx, tx)
	releaseID := insertRelease(t, ctx, tx, f, "candidate")

	var opID int64
	if err := tx.QueryRow(ctx, `INSERT INTO release_operations (release_id, job_id, attempt_id, fence, kind, digest, state)
	      VALUES ($1,$2,$3,1,'prepare',$4,'started') RETURNING id`, releaseID, f.jobID, f.attemptID, randomBytes(t, 32)).Scan(&opID); err != nil {
		t.Fatalf("insert operation: %v", err)
	}

	// Close: only state/closed_at may change. clock_timestamp() is used
	// rather than now(), because now() is frozen at transaction start and
	// this whole test runs inside one transaction that began before
	// created_at (set by clock_timestamp() at INSERT) -- now() here would be
	// earlier than created_at and trip the closed_at >= created_at CHECK.
	if _, err := tx.Exec(ctx, `UPDATE release_operations SET state='succeeded', closed_at=clock_timestamp() WHERE id=$1`, opID); err != nil {
		t.Fatalf("close operation: %v", err)
	}

	// Already closed: fully frozen.
	mustFail(t, ctx, tx, "expected rejection of re-closing an already-closed operation",
		`UPDATE release_operations SET state='failed', closed_at=clock_timestamp() WHERE id=$1`, opID)

	// Never delete.
	mustFail(t, ctx, tx, "expected delete rejection (append-only)",
		`DELETE FROM release_operations WHERE id=$1`, opID)

	// A second, freshly opened operation cannot change an identity column
	// (kind) alongside its close.
	var opID2 int64
	if err := tx.QueryRow(ctx, `INSERT INTO release_operations (release_id, job_id, attempt_id, fence, kind, digest, state)
	      VALUES ($1,$2,$3,1,'prepare',$4,'started') RETURNING id`, releaseID, f.jobID, f.attemptID, randomBytes(t, 32)).Scan(&opID2); err != nil {
		t.Fatalf("insert second operation: %v", err)
	}
	mustFail(t, ctx, tx, "expected rejection of changing kind alongside close",
		`UPDATE release_operations SET kind='cleanup', state='succeeded', closed_at=clock_timestamp() WHERE id=$1`, opID2)

	// The sanitized outcome is written when the operation closes, because that
	// is when it is known.
	if _, err := tx.Exec(ctx, `UPDATE release_operations SET state='failed', outcome_summary='build failed: exit status 1', closed_at=clock_timestamp() WHERE id=$1`, opID2); err != nil {
		t.Fatalf("close with outcome summary: %v", err)
	}
	var summary string
	if err := tx.QueryRow(ctx, `SELECT outcome_summary FROM release_operations WHERE id=$1`, opID2).Scan(&summary); err != nil {
		t.Fatalf("read outcome summary: %v", err)
	}
	if summary != "build failed: exit status 1" {
		t.Fatalf("unexpected outcome summary %q", summary)
	}
	// Control characters are rejected by the CHECK even on close.
	var opID3 int64
	if err := tx.QueryRow(ctx, `INSERT INTO release_operations (release_id, job_id, attempt_id, fence, kind, digest, state)
	      VALUES ($1,$2,$3,1,'prepare',$4,'started') RETURNING id`, releaseID, f.jobID, f.attemptID, randomBytes(t, 32)).Scan(&opID3); err != nil {
		t.Fatalf("insert third operation: %v", err)
	}
	mustFail(t, ctx, tx, "expected rejection of control characters in outcome summary",
		`UPDATE release_operations SET state='failed', outcome_summary=$2, closed_at=clock_timestamp() WHERE id=$1`, opID3, "bad\x00summary")
}

// TestReleaseRuntimeInstanceInsertValidatesFenceAndJobID proves the
// BEFORE INSERT trigger rejects a fence or job_id that disagrees with the
// agent_job_attempts row actually named by attempt_id, instead of silently
// admitting a row whose later fence-gated UPDATE would just match zero rows.
func TestReleaseRuntimeInstanceInsertValidatesFenceAndJobID(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f := buildReleaseFixture(t, ctx, tx)
	f2 := buildReleaseFixture(t, ctx, tx)
	releaseID := insertRelease(t, ctx, tx, f, "candidate")

	// A fence that does not match the attempt's own fence (1) is rejected --
	// f2's job_id exists (satisfying the FK) but is a different job, so this
	// exercises the fence check specifically.
	mustFail(t, ctx, tx, "expected rejection of mismatched fence",
		`INSERT INTO release_runtime_instances (release_id, job_id, attempt_id, fence, container_name, observed_state)
	      VALUES ($1,$2,$3,999,$4,'created')`, releaseID, f.jobID, f.attemptID, "autodeploy-fence-mismatch")

	// A job_id that does not match the attempt's own job_id is rejected, even
	// though f2.jobID is a real, existing job (so this is not an FK failure).
	mustFail(t, ctx, tx, "expected rejection of mismatched job_id",
		`INSERT INTO release_runtime_instances (release_id, job_id, attempt_id, fence, container_name, observed_state)
	      VALUES ($1,$2,$3,1,$4,'created')`, releaseID, f2.jobID, f.attemptID, "autodeploy-jobid-mismatch")

	// The correct fence and job_id together are accepted.
	if _, err := tx.Exec(ctx, `INSERT INTO release_runtime_instances (release_id, job_id, attempt_id, fence, container_name, observed_state)
	      VALUES ($1,$2,$3,1,$4,'created')`, releaseID, f.jobID, f.attemptID, "autodeploy-correct"); err != nil {
		t.Fatalf("expected correctly fenced insert to succeed: %v", err)
	}
}

// TestReleaseOperationInsertValidatesFenceAndJobID mirrors
// TestReleaseRuntimeInstanceInsertValidatesFenceAndJobID for release_operations.
func TestReleaseOperationInsertValidatesFenceAndJobID(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f := buildReleaseFixture(t, ctx, tx)
	f2 := buildReleaseFixture(t, ctx, tx)
	releaseID := insertRelease(t, ctx, tx, f, "candidate")

	mustFail(t, ctx, tx, "expected rejection of mismatched fence",
		`INSERT INTO release_operations (release_id, job_id, attempt_id, fence, kind, digest, state)
	      VALUES ($1,$2,$3,999,'prepare',$4,'started')`, releaseID, f.jobID, f.attemptID, randomBytes(t, 32))

	mustFail(t, ctx, tx, "expected rejection of mismatched job_id",
		`INSERT INTO release_operations (release_id, job_id, attempt_id, fence, kind, digest, state)
	      VALUES ($1,$2,$3,1,'prepare',$4,'started')`, releaseID, f2.jobID, f.attemptID, randomBytes(t, 32))

	if _, err := tx.Exec(ctx, `INSERT INTO release_operations (release_id, job_id, attempt_id, fence, kind, digest, state)
	      VALUES ($1,$2,$3,1,'prepare',$4,'started')`, releaseID, f.jobID, f.attemptID, randomBytes(t, 32)); err != nil {
		t.Fatalf("expected correctly fenced insert to succeed: %v", err)
	}
}

// TestEnvironmentRuntimeBindingScopeTrigger proves the BEFORE INSERT OR
// UPDATE trigger enforces ADR 0003's cross-environment/cross-scope
// materialization boundary: a binding may only reference a secret head in
// its own environment, with a matching delivery_scope, and never a
// 'provider'-purpose head.
func TestEnvironmentRuntimeBindingScopeTrigger(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f := buildReleaseFixture(t, ctx, tx)     // secretHeadID: purpose=runtime, delivery_scope=runtime, environment_id=f.environmentID
	other := buildReleaseFixture(t, ctx, tx) // a distinct environment with its own secret head

	// Foreign-environment head: rejected.
	mustFail(t, ctx, tx, "expected rejection of a binding referencing a foreign-environment secret head",
		`INSERT INTO environment_runtime_bindings (environment_id, variable_name, binding_kind, secret_head_id, delivery_scope)
	      VALUES ($1,'FOREIGN_ENV','secret',$2,'runtime')`, f.environmentID, other.secretHeadID)

	// delivery_scope mismatch: the head is scoped 'runtime' but the binding
	// claims 'build'.
	mustFail(t, ctx, tx, "expected rejection of a delivery_scope mismatch",
		`INSERT INTO environment_runtime_bindings (environment_id, variable_name, binding_kind, secret_head_id, delivery_scope)
	      VALUES ($1,'SCOPE_MISMATCH','secret',$2,'build')`, f.environmentID, f.secretHeadID)

	// 'provider'-purpose head: never a deployable variable, even with
	// matching environment and delivery_scope.
	providerHeadID := "secret-provider-" + randomSuffix(t)
	providerVerID := "secretver-provider-" + randomSuffix(t)
	if _, err := tx.Exec(ctx, `INSERT INTO secret_heads (id, owner_id, project_id, environment_id, purpose, delivery_scope) VALUES ($1,$2,'project-x',$3,'provider','runtime')`,
		providerHeadID, f.ownerID, f.environmentID); err != nil {
		t.Fatalf("insert provider secret head: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO secret_versions (id, secret_id, owner_id, project_id, environment_id, version_number, state, suite, aad_version, kek_id, ciphertext, wrapped_dek)
	      VALUES ($1,$2,$3,'project-x',$4,1,'active','AES-256-GCM-RANDOMNONCE-v1','v1','kek-test',$5,$6)`,
		providerVerID, providerHeadID, f.ownerID, f.environmentID, randomBytes(t, 32), randomBytes(t, 60)); err != nil {
		t.Fatalf("insert provider secret version: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE secret_heads SET current_version_id=$1, revision=1 WHERE id=$2`, providerVerID, providerHeadID); err != nil {
		t.Fatalf("set provider secret head current version: %v", err)
	}
	mustFail(t, ctx, tx, "expected rejection of a provider-purpose secret head binding",
		`INSERT INTO environment_runtime_bindings (environment_id, variable_name, binding_kind, secret_head_id, delivery_scope)
	      VALUES ($1,'PROVIDER_HEAD','secret',$2,'runtime')`, f.environmentID, providerHeadID)

	// Correctly scoped: same environment, matching delivery_scope, non-provider purpose.
	if _, err := tx.Exec(ctx, `INSERT INTO environment_runtime_bindings (environment_id, variable_name, binding_kind, secret_head_id, delivery_scope)
	      VALUES ($1,'CORRECTLY_SCOPED','secret',$2,'runtime')`, f.environmentID, f.secretHeadID); err != nil {
		t.Fatalf("expected correctly scoped binding to succeed: %v", err)
	}

	// The same scope trigger applies to UPDATE, not just INSERT: retarget an
	// existing binding onto the foreign-environment head and expect rejection.
	mustFail(t, ctx, tx, "expected rejection of updating a binding onto a foreign-environment secret head",
		`UPDATE environment_runtime_bindings SET secret_head_id=$2 WHERE environment_id=$1 AND variable_name='CORRECTLY_SCOPED'`, f.environmentID, other.secretHeadID)
}
