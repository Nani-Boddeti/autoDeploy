//go:build integration

package migrations

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

const insertManualIntent = `INSERT INTO manual_deployment_intents(owner_id, actor_user_id, binding_project_id, repository_id, full_ref, commit_sha, request_nonce, deployment_id)
	VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`

type manualIntentFixture struct {
	ownerID, userID, projectID, deploymentID string
	repoID                                   int64
}

// newManualIntentFixture creates owner, user, installation, repository,
// binding and one deployment inside tx. Ids are random so reruns against a
// shared database never collide.
func newManualIntentFixture(t *testing.T, ctx context.Context, tx pgx.Tx) manualIntentFixture {
	t.Helper()
	suffix := randomSuffix(t)
	f := manualIntentFixture{
		ownerID: "owner-mi-" + suffix, userID: "user-mi-" + suffix, projectID: "project-mi-" + suffix,
		deploymentID: "dep-mi-" + suffix,
		repoID:       8_000_000_000 + int64(randomBytes(t, 1)[0])*1_000_000 + int64(randomBytes(t, 2)[0])*1000 + int64(randomBytes(t, 1)[0]),
	}
	instID := f.repoID + 1
	mustExec(t, ctx, tx, `INSERT INTO owners(id) VALUES ($1)`, f.ownerID)
	mustExec(t, ctx, tx, `INSERT INTO users(id,owner_id,username,role,password_phc,password_policy_revision) VALUES ($1,$2,$3,'owner',$4,1)`, f.userID, f.ownerID, "mi"+suffix, strings.Repeat("x", 68))
	mustExec(t, ctx, tx, `INSERT INTO github_installations(installation_id,owner_id,account_id,account_type) VALUES ($1,$2,$1,'User')`, instID, f.ownerID)
	mustExec(t, ctx, tx, `INSERT INTO github_repositories(repository_id,installation_id,owner_id,github_owner_id,name,full_name,clone_url) VALUES ($1,$2,$3,$2,'r','o/r','https://github.com/o/r.git')`, f.repoID, instID, f.ownerID)
	mustExec(t, ctx, tx, `INSERT INTO github_project_bindings(owner_id,project_id,repository_id,installation_id) VALUES ($1,$2,$3,$4)`, f.ownerID, f.projectID, f.repoID, instID)
	mustExec(t, ctx, tx, `INSERT INTO deployments(id,project_id,environment_id,server_id,commit_sha,schema_version,status,created_at,updated_at,revision) VALUES ($1,$2,'env','srv',$3,1,'queued',now(),now(),1)`, f.deploymentID, f.projectID, strings.Repeat("a", 40))
	return f
}

func TestManualDeploymentIntentsConstraints(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f := newManualIntentFixture(t, ctx, tx)
	sha := strings.Repeat("a", 40)
	ref := "refs/heads/main"
	nonce := strings.Repeat("n", 16)

	for name, bad := range map[string]string{
		"uppercase sha": strings.Repeat("A", 40), "short sha": strings.Repeat("a", 39),
		"long sha": strings.Repeat("a", 41), "zero sha": strings.Repeat("0", 40), "empty sha": "",
	} {
		mustFail(t, ctx, tx, name+" accepted", insertManualIntent, f.ownerID, f.userID, f.projectID, f.repoID, ref, bad, strings.Repeat("b", 16), f.deploymentID)
	}
	for name, bad := range map[string]string{
		"tag ref": "refs/tags/v1", "bare branch": "main", "double dot": "refs/heads/a..b",
		"space": "refs/heads/a b", "trailing slash": "refs/heads/a/", "lock suffix": "refs/heads/a.lock",
		"too short": "refs/heads/", "dot component": "refs/heads/.x", "empty": "",
	} {
		mustFail(t, ctx, tx, name+" ref accepted", insertManualIntent, f.ownerID, f.userID, f.projectID, f.repoID, bad, sha, strings.Repeat("c", 16), f.deploymentID)
	}
	for name, bad := range map[string]string{
		"15-byte nonce": strings.Repeat("n", 15), "129-byte nonce": strings.Repeat("n", 129),
		"nonce with space": strings.Repeat("n", 15) + " ", "nonce with slash": strings.Repeat("n", 15) + "/",
		"nonce with newline": strings.Repeat("n", 15) + "\n", "empty nonce": "",
	} {
		mustFail(t, ctx, tx, name+" accepted", insertManualIntent, f.ownerID, f.userID, f.projectID, f.repoID, ref, sha, bad, f.deploymentID)
	}
	mustFail(t, ctx, tx, "repository_id 0 accepted", insertManualIntent, f.ownerID, f.userID, f.projectID, 0, ref, sha, strings.Repeat("d", 16), f.deploymentID)
	// Every rejection above ran against a row that is otherwise valid and
	// unique (the baseline is inserted only now), so each failure is its own.
	if _, err := tx.Exec(ctx, insertManualIntent, f.ownerID, f.userID, f.projectID, f.repoID, ref, sha, nonce, f.deploymentID); err != nil {
		t.Fatalf("valid intent rejected: %v", err)
	}
	dep2 := "dep-mi-dup-" + randomSuffix(t)
	mustExec(t, ctx, tx, `INSERT INTO deployments(id,project_id,environment_id,server_id,commit_sha,schema_version,status,created_at,updated_at,revision) VALUES ($1,$2,'env','srv',$3,1,'queued',now(),now(),1)`, dep2, f.projectID, sha)
	// Fresh deployment, same (owner,nonce): only UNIQUE(owner_id,request_nonce) can fire.
	mustFail(t, ctx, tx, "duplicate (owner,nonce) accepted", insertManualIntent, f.ownerID, f.userID, f.projectID, f.repoID, ref, sha, nonce, dep2)
	// Fresh nonce, same deployment: only UNIQUE(deployment_id) can fire.
	mustFail(t, ctx, tx, "duplicate deployment_id accepted under a new nonce", insertManualIntent, f.ownerID, f.userID, f.projectID, f.repoID, ref, sha, strings.Repeat("m", 16), f.deploymentID)
}

func TestManualDeploymentIntentsNonceBoundariesAndIdempotencyScope(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f := newManualIntentFixture(t, ctx, tx)
	sha := strings.Repeat("b", 40)
	// Same nonce under a different owner is a different key.
	g := newManualIntentFixture(t, ctx, tx)
	for i, nonce := range []string{strings.Repeat("A", 128), "abcDEF012345_-xyz"} {
		dep := f.deploymentID
		if i == 1 {
			// Second row needs its own deployment (deployment_id is UNIQUE).
			dep = "dep-mi-extra-" + randomSuffix(t)
			mustExec(t, ctx, tx, `INSERT INTO deployments(id,project_id,environment_id,server_id,commit_sha,schema_version,status,created_at,updated_at,revision) VALUES ($1,$2,'env','srv',$3,1,'queued',now(),now(),1)`, dep, f.projectID, sha)
		}
		if _, err := tx.Exec(ctx, insertManualIntent, f.ownerID, f.userID, f.projectID, f.repoID, "refs/heads/main", sha, nonce, dep); err != nil {
			t.Fatalf("nonce %q rejected: %v", nonce, err)
		}
	}
	if _, err := tx.Exec(ctx, insertManualIntent, g.ownerID, g.userID, g.projectID, g.repoID, "refs/heads/main", sha, "abcDEF012345_-xyz", g.deploymentID); err != nil {
		t.Fatalf("same nonce under another owner rejected: %v", err)
	}
}

func TestManualDeploymentIntentsForeignKeys(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f := newManualIntentFixture(t, ctx, tx)
	other := newManualIntentFixture(t, ctx, tx)
	sha, ref, nonce := strings.Repeat("c", 40), "refs/heads/main", strings.Repeat("f", 16)

	mustFail(t, ctx, tx, "nonexistent binding accepted", insertManualIntent, f.ownerID, f.userID, "no-such-project", f.repoID, ref, sha, nonce, f.deploymentID)
	mustFail(t, ctx, tx, "another owner's binding accepted", insertManualIntent, f.ownerID, f.userID, other.projectID, f.repoID, ref, sha, nonce, f.deploymentID)
	mustFail(t, ctx, tx, "repository not bound to the owner accepted", insertManualIntent, f.ownerID, f.userID, f.projectID, other.repoID, ref, sha, nonce, f.deploymentID)
	mustFail(t, ctx, tx, "actor from another owner accepted", insertManualIntent, f.ownerID, other.userID, f.projectID, f.repoID, ref, sha, nonce, f.deploymentID)
	mustFail(t, ctx, tx, "nonexistent actor accepted", insertManualIntent, f.ownerID, "no-such-user", f.projectID, f.repoID, ref, sha, nonce, f.deploymentID)

	// The deployment FK is deferred: a dangling deployment_id is accepted by
	// the INSERT and rejected at commit. Prove it with a real commit attempt
	// on a rolled-back-safe path using SET CONSTRAINTS IMMEDIATE.
	mustExec(t, ctx, tx, `SAVEPOINT dangling`)
	if _, err := tx.Exec(ctx, insertManualIntent, f.ownerID, f.userID, f.projectID, f.repoID, ref, sha, nonce, "no-such-deployment"); err != nil {
		t.Fatalf("deferred FK should not fire at insert: %v", err)
	}
	if _, err := tx.Exec(ctx, `SET CONSTRAINTS ALL IMMEDIATE`); err == nil {
		t.Fatal("dangling deployment_id survived constraint check")
	}
}

func TestManualDeploymentIntentsImmutable(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f := newManualIntentFixture(t, ctx, tx)
	if _, err := tx.Exec(ctx, insertManualIntent, f.ownerID, f.userID, f.projectID, f.repoID, "refs/heads/main", strings.Repeat("d", 40), strings.Repeat("g", 16), f.deploymentID); err != nil {
		t.Fatal(err)
	}
	mustRaiseTrigger(t, ctx, tx, "manual deployment intents are immutable", `UPDATE manual_deployment_intents SET commit_sha=$1 WHERE owner_id=$2`, strings.Repeat("e", 40), f.ownerID)
	mustRaiseTrigger(t, ctx, tx, "manual deployment intents are immutable", `DELETE FROM manual_deployment_intents WHERE owner_id=$1`, f.ownerID)
}

func TestManualDeploymentIntentsShape(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE tablename='manual_deployment_intents' AND indexname='manual_deployment_intents_owner_created_idx' AND indexdef LIKE '%(owner_id, created_at DESC)%'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("owner/created_at DESC index missing: n=%d err=%v", n, err)
	}
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE conrelid='manual_deployment_intents'::regclass AND contype='f'`).Scan(&n); err != nil || n != 4 {
		t.Fatalf("expected 4 foreign keys (user, binding, repository, deployment): n=%d err=%v", n, err)
	}
}
