//go:build integration

package migrations

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

const insertRollbackJob = `INSERT INTO agent_jobs
	(schema_version, job_kind, owner_id, project_id, environment_id, server_id, deployment_id, repository_id, full_ref, commit_sha, priority, available_at, idempotency_key, payload_digest, state, rollback_target_release_id)
	VALUES (1,$1,$2,'project-rb',$3,$4,$5,1,'refs/heads/main',$6,0,$7,$8,$9,'queued',$10)`

// newRollbackJobDeployment adds a deployment for a new job and returns its id.
func newRollbackJobDeployment(t *testing.T, ctx context.Context, tx pgx.Tx, f releaseFixture) string {
	t.Helper()
	id := "deploy-rbj-" + randomSuffix(t)
	mustExec(t, ctx, tx, `INSERT INTO deployments (id, project_id, environment_id, server_id, commit_sha, schema_version, status, created_at, updated_at, revision)
	      VALUES ($1,'project-rb',$2,$3,$4,1,'queued',now(),now(),1)`, id, f.environmentID, f.serverID, randomHexSHA(t))
	return id
}

func TestAgentJobsRollbackKindAndTargetCheck(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f := buildReleaseFixture(t, ctx, tx)
	target := insertRelease(t, ctx, tx, f, "retained")
	now := time.Now().UTC()
	insert := func(kind, key string, targetID any) error {
		_, err := tx.Exec(ctx, `SAVEPOINT rb`)
		if err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(ctx, insertRollbackJob, kind, f.ownerID, f.environmentID, f.serverID, newRollbackJobDeployment(t, ctx, tx, f), randomHexSHA(t), now, key+randomSuffix(t), randomBytes(t, 32), targetID)
		if err != nil {
			mustExec(t, ctx, tx, `ROLLBACK TO SAVEPOINT rb`)
		}
		return err
	}
	if err := insert("rollback_execution", "k-ok-", target); err != nil {
		t.Fatalf("valid rollback job rejected: %v", err)
	}
	if err := insert("deployment_execution", "k-dep-", nil); err != nil {
		t.Fatalf("deployment job without target rejected: %v", err)
	}
	for name, c := range map[string]struct {
		kind   string
		target any
	}{
		"rollback without target":          {"rollback_execution", nil},
		"deployment with a target":         {"deployment_execution", target},
		"unknown kind":                     {"retire_execution", nil},
		"unknown kind with target":         {"retire_execution", target},
		"rollback with a dangling target":  {"rollback_execution", "no-such-release"},
		"empty kind":                       {"", nil},
		"upper-case kind":                  {"ROLLBACK_EXECUTION", target},
		"rollback kind prefix with target": {"rollback", target},
	} {
		if err := insert(c.kind, "k-bad-", c.target); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestAgentJobsRollbackTargetIsImmutable(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f := buildReleaseFixture(t, ctx, tx)
	target, other := insertRelease(t, ctx, tx, f, "retained"), insertRelease(t, ctx, tx, f, "retained")
	var jobID int64
	if err := tx.QueryRow(ctx, insertRollbackJob+` RETURNING id`, "rollback_execution", f.ownerID, f.environmentID, f.serverID, newRollbackJobDeployment(t, ctx, tx, f), randomHexSHA(t), time.Now().UTC(), "k-imm-"+randomSuffix(t), randomBytes(t, 32), target).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	mustFail(t, ctx, tx, "rollback target changed", `UPDATE agent_jobs SET rollback_target_release_id=$2 WHERE id=$1`, jobID, other)
	mustFail(t, ctx, tx, "rollback target cleared", `UPDATE agent_jobs SET rollback_target_release_id=NULL WHERE id=$1`, jobID)
	mustFail(t, ctx, tx, "kind changed", `UPDATE agent_jobs SET job_kind='deployment_execution', rollback_target_release_id=NULL WHERE id=$1`, jobID)
	// The 000013 identity checks survive the replaced trigger function.
	mustFail(t, ctx, tx, "environment changed", `UPDATE agent_jobs SET environment_id='elsewhere' WHERE id=$1`, jobID)
	mustExec(t, ctx, tx, `UPDATE agent_jobs SET state='cancelled', updated_at=clock_timestamp() WHERE id=$1`, jobID)
	mustFail(t, ctx, tx, "terminal job updated", `UPDATE agent_jobs SET state='leased' WHERE id=$1`, jobID)
}

const insertRollbackIntent = `INSERT INTO rollback_intents(owner_id, actor_user_id, source_release_id, target_release_id, request_nonce, deployment_id) VALUES ($1,$2,$3,$4,$5,$6)`

func TestRollbackIntentsConstraints(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f := newManualIntentFixture(t, ctx, tx)
	other := newManualIntentFixture(t, ctx, tx)
	rf := buildReleaseFixture(t, ctx, tx)
	src, dst := insertRelease(t, ctx, tx, rf, "active"), insertRelease(t, ctx, tx, rf, "retained")
	nonce := strings.Repeat("r", 16)

	for name, bad := range map[string]string{
		"15-byte nonce": strings.Repeat("n", 15), "129-byte nonce": strings.Repeat("n", 129), "nonce with space": strings.Repeat("n", 15) + " ",
		"nonce with slash": strings.Repeat("n", 15) + "/", "nonce with newline": strings.Repeat("n", 15) + "\n", "empty nonce": "",
	} {
		mustFail(t, ctx, tx, name+" accepted", insertRollbackIntent, f.ownerID, f.userID, src, dst, bad, f.deploymentID)
	}
	mustFail(t, ctx, tx, "source equal to target accepted", insertRollbackIntent, f.ownerID, f.userID, src, src, nonce, f.deploymentID)
	mustFail(t, ctx, tx, "unknown source accepted", insertRollbackIntent, f.ownerID, f.userID, "no-such-release", dst, nonce, f.deploymentID)
	mustFail(t, ctx, tx, "unknown target accepted", insertRollbackIntent, f.ownerID, f.userID, src, "no-such-release", nonce, f.deploymentID)
	mustFail(t, ctx, tx, "actor from another owner accepted", insertRollbackIntent, f.ownerID, other.userID, src, dst, nonce, f.deploymentID)
	mustFail(t, ctx, tx, "nonexistent actor accepted", insertRollbackIntent, f.ownerID, "no-such-user", src, dst, nonce, f.deploymentID)
	if _, err := tx.Exec(ctx, insertRollbackIntent, f.ownerID, f.userID, src, dst, nonce, f.deploymentID); err != nil {
		t.Fatalf("valid intent rejected: %v", err)
	}
	dep2 := "dep-rb-dup-" + randomSuffix(t)
	mustExec(t, ctx, tx, `INSERT INTO deployments(id,project_id,environment_id,server_id,commit_sha,schema_version,status,created_at,updated_at,revision) VALUES ($1,'p','env','srv',$2,1,'queued',now(),now(),1)`, dep2, strings.Repeat("c", 40))
	mustFail(t, ctx, tx, "duplicate (owner,nonce) accepted", insertRollbackIntent, f.ownerID, f.userID, src, dst, nonce, dep2)
	mustFail(t, ctx, tx, "duplicate deployment_id accepted under a new nonce", insertRollbackIntent, f.ownerID, f.userID, src, dst, strings.Repeat("s", 16), f.deploymentID)
	// The same nonce under another owner is a different key.
	if _, err := tx.Exec(ctx, insertRollbackIntent, other.ownerID, other.userID, src, dst, nonce, other.deploymentID); err != nil {
		t.Fatalf("same nonce under another owner rejected: %v", err)
	}
	// 128-byte and mixed-alphabet nonces are valid.
	mustExec(t, ctx, tx, insertRollbackIntent, f.ownerID, f.userID, dst, src, strings.Repeat("A", 128), dep2)
}

func TestRollbackIntentsImmutable(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f := newManualIntentFixture(t, ctx, tx)
	rf := buildReleaseFixture(t, ctx, tx)
	src, dst := insertRelease(t, ctx, tx, rf, "active"), insertRelease(t, ctx, tx, rf, "retained")
	mustExec(t, ctx, tx, insertRollbackIntent, f.ownerID, f.userID, src, dst, strings.Repeat("i", 16), f.deploymentID)
	mustFail(t, ctx, tx, "intent updated", `UPDATE rollback_intents SET request_nonce=$1 WHERE deployment_id=$2`, strings.Repeat("j", 16), f.deploymentID)
	mustFail(t, ctx, tx, "intent deleted", `DELETE FROM rollback_intents WHERE deployment_id=$1`, f.deploymentID)
}
