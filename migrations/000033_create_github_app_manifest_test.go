//go:build integration

package migrations

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// failsConstraint asserts that sql is rejected by exactly the named
// constraint (SQLSTATE and constraint name), inside a savepoint. Naming the
// constraint is what keeps these tests from passing because some older
// constraint happened to reject the row.
func failsConstraint(t *testing.T, ctx context.Context, tx pgx.Tx, code, constraint, sql string, args ...any) {
	t.Helper()
	savepointCounter++
	sp := "sp_mf_" + strings.Repeat("x", savepointCounter)
	mustExec(t, ctx, tx, "SAVEPOINT "+sp)
	_, err := tx.Exec(ctx, sql, args...)
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		t.Fatalf("expected %s on %s, statement succeeded: %s", code, constraint, sql)
	case !errors.As(err, &pgErr):
		t.Fatalf("expected PgError, got %v", err)
	case pgErr.Code != code || pgErr.ConstraintName != constraint:
		t.Fatalf("expected %s on %q, got %s on %q (%s): %s", code, constraint, pgErr.Code, pgErr.ConstraintName, pgErr.Message, sql)
	}
	mustExec(t, ctx, tx, "ROLLBACK TO SAVEPOINT "+sp)
}

type manifestFixture struct{ ownerID, userID string }

func newManifestFixture(t *testing.T, ctx context.Context, tx pgx.Tx) manifestFixture {
	t.Helper()
	f := manifestFixture{ownerID: "mo-" + randomSuffix(t), userID: "mu-" + randomSuffix(t)}
	mustExec(t, ctx, tx, `INSERT INTO owners(id) VALUES ($1)`, f.ownerID)
	mustExec(t, ctx, tx, `INSERT INTO users(id,owner_id,username,role,password_phc,password_policy_revision) VALUES($1,$2,$3,'owner',$4,1)`, f.userID, f.ownerID, "mf-"+randomSuffix(t), strings.Repeat("x", 68))
	return f
}

const insertManifestState = `INSERT INTO github_manifest_states(digest,owner_id,user_id,session_id,auth_revision,created_at,expires_at,consumed_at)
  VALUES ($1,$2,$3,'sess-1',1,$4::timestamptz,$5::timestamptz,$6::timestamptz)`

// base time keeps every row valid unless one clause is deliberately violated.
const (
	t0       = `2030-01-01T00:00:00Z`
	t10m     = `2030-01-01T00:10:00Z`
	tBefore  = `2029-12-31T23:59:00Z`
	t2h      = `2030-01-01T02:00:00Z`
	t5m      = `2030-01-01T00:05:00Z`
	t20m     = `2030-01-01T00:20:00Z`
	tInvalid = `2030-01-01T00:11:00Z`
)

func TestManifestStateConstraints(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f := newManifestFixture(t, ctx, tx)
	d := func() []byte { return randomBytes(t, 32) }

	mustExec(t, ctx, tx, insertManifestState, d(), f.ownerID, f.userID, t0, t10m, nil) // control: valid row
	failsConstraint(t, ctx, tx, "23514", "github_manifest_states_digest_len", insertManifestState, randomBytes(t, 31), f.ownerID, f.userID, t0, t10m, nil)
	failsConstraint(t, ctx, tx, "23514", "github_manifest_states_expiry_after_issue", insertManifestState, d(), f.ownerID, f.userID, t0, t0, nil)
	failsConstraint(t, ctx, tx, "23514", "github_manifest_states_ttl_cap", insertManifestState, d(), f.ownerID, f.userID, t0, t2h, nil)
	failsConstraint(t, ctx, tx, "23514", "github_manifest_states_consumed_not_before_issue", insertManifestState, d(), f.ownerID, f.userID, t0, t10m, tBefore)
	failsConstraint(t, ctx, tx, "23514", "github_manifest_states_consumed_before_expiry", insertManifestState, d(), f.ownerID, f.userID, t0, t10m, tInvalid)
	// user must belong to the owner (composite foreign key).
	failsConstraint(t, ctx, tx, "23503", "github_manifest_states_user_id_owner_id_fkey", insertManifestState, d(), f.ownerID, "no-such-user", t0, t10m, nil)
}

func TestManifestStateTriggers(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f := newManifestFixture(t, ctx, tx)
	otherOwner := "mo2-" + randomSuffix(t)
	mustExec(t, ctx, tx, `INSERT INTO owners(id) VALUES ($1)`, otherOwner)
	digest := randomBytes(t, 32)
	mustExec(t, ctx, tx, insertManifestState, digest, f.ownerID, f.userID, t0, t10m, nil)

	// Each issuance column is individually immutable.
	for _, c := range []struct{ name, set string }{
		{"digest", `digest = $2`},
		{"owner_id", `owner_id = $2`},
		{"user_id", `user_id = $2`},
		{"session_id", `session_id = $2`},
		{"auth_revision", `auth_revision = 2`},
		{"created_at", `created_at = $2::timestamptz`},
	} {
		sql := `UPDATE github_manifest_states SET ` + c.set + ` WHERE digest=$1`
		var arg any
		switch c.name {
		case "digest":
			arg = randomBytes(t, 32)
		case "owner_id":
			arg = otherOwner
		case "user_id":
			arg = "other-user"
		case "session_id":
			arg = "sess-2"
		case "created_at":
			arg = tBefore
		}
		args := []any{digest}
		if arg != nil {
			args = append(args, arg)
		}
		mustRaiseTrigger(t, ctx, tx, "manifest state issuance is immutable", sql, args...)
	}
	// Lifetime may not grow (10m -> 20m is within the 1h cap, so only the trigger refuses).
	mustRaiseTrigger(t, ctx, tx, "manifest state lifetime cannot be extended", `UPDATE github_manifest_states SET expires_at=$2::timestamptz WHERE digest=$1`, digest, t20m)
	mustRaiseTrigger(t, ctx, tx, "manifest states are retained", `DELETE FROM github_manifest_states WHERE digest=$1`, digest)
	// Shortening (retiring) is allowed.
	mustExec(t, ctx, tx, `UPDATE github_manifest_states SET expires_at=$2::timestamptz WHERE digest=$1`, digest, t5m)
	// Consuming is allowed only with a current timestamp. Rows below are issued relative to now.
	const insertNow = `INSERT INTO github_manifest_states(digest,owner_id,user_id,session_id,auth_revision,created_at,expires_at)
	  VALUES ($1,$2,$3,'sess-now',1,clock_timestamp()-interval '2 minutes',clock_timestamp()+interval '10 minutes')`
	stale, ahead, fresh := randomBytes(t, 32), randomBytes(t, 32), randomBytes(t, 32)
	for _, d := range [][]byte{stale, ahead, fresh} {
		mustExec(t, ctx, tx, insertNow, d, f.ownerID, f.userID)
	}
	mustRaiseTrigger(t, ctx, tx, "manifest state consumed_at must be current", `UPDATE github_manifest_states SET consumed_at=clock_timestamp()-interval '1 minute' WHERE digest=$1`, stale)
	mustRaiseTrigger(t, ctx, tx, "manifest state consumed_at must be current", `UPDATE github_manifest_states SET consumed_at=clock_timestamp()+interval '1 minute' WHERE digest=$1`, ahead)
	mustExec(t, ctx, tx, `UPDATE github_manifest_states SET consumed_at=clock_timestamp() WHERE digest=$1`, fresh)
	// A consumed row is frozen even against a harmless shortening.
	mustRaiseTrigger(t, ctx, tx, "consumed manifest state cannot change", `UPDATE github_manifest_states SET expires_at=clock_timestamp()+interval '5 minutes' WHERE digest=$1`, fresh)
	mustRaiseTrigger(t, ctx, tx, "consumed manifest state cannot change", `UPDATE github_manifest_states SET consumed_at=clock_timestamp() WHERE digest=$1`, fresh)
}

const insertManifestApp = `INSERT INTO github_manifest_apps(singleton,id,app_id,slug,client_id,github_owner_id,github_owner_type,github_owner_login,html_url,private_key_envelope,webhook_secret_envelope,owner_id,created_by_user_id,private_key_kek_id,webhook_secret_kek_id)
  VALUES (COALESCE($1::boolean,true),$2,$3,'slug','client',7,COALESCE($4::text,'User'),'login','https://github.com/apps/slug',$5,$6,$7,$8,'kek-a','kek-b')`

func TestManifestAppConstraintsAndTriggers(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f := newManifestFixture(t, ctx, tx)
	env := func() []byte { return randomBytes(t, 120) }
	ins := func(id string, appID int64, singleton any, ownerType any, key, secret []byte) (string, []any) {
		return insertManifestApp, []any{singleton, id, appID, ownerType, key, secret, f.ownerID, f.userID}
	}
	run := func(constraint, code string, id string, appID int64, singleton any, ownerType any, key, secret []byte) {
		sql, args := ins(id, appID, singleton, ownerType, key, secret)
		failsConstraint(t, ctx, tx, code, constraint, sql, args...)
	}
	// Each rejection varies exactly one thing from a valid row with a fresh id/app_id.
	run("github_manifest_apps_app_id_positive", "23514", "a1", 0, nil, nil, env(), env())
	run("github_manifest_apps_owner_type", "23514", "a2", 2, nil, "Bot", env(), env())
	run("github_manifest_apps_key_envelope_len", "23514", "a3", 3, nil, nil, randomBytes(t, 63), env())
	run("github_manifest_apps_secret_envelope_len", "23514", "a4", 4, nil, nil, env(), randomBytes(t, 63))
	run("github_manifest_apps_singleton", "23514", "a5", 5, false, nil, env(), env())

	sql, args := ins("ok-1", 10, nil, nil, env(), env())
	mustExec(t, ctx, tx, sql, args...)
	// A second App with a different id and app_id collides only on the singleton key.
	run("github_manifest_apps_pkey", "23505", "ok-2", 11, nil, nil, env(), env())

	mustRaiseTrigger(t, ctx, tx, "manifest GitHub App is immutable", `UPDATE github_manifest_apps SET slug='other'`)
	mustRaiseTrigger(t, ctx, tx, "manifest GitHub App is immutable", `DELETE FROM github_manifest_apps`)
}

func TestManifestAuditActions(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f := newManifestFixture(t, ctx, tx)
	// 000005's original actions stay accepted (no migration 000006-000032 altered this constraint).
	for _, action := range []string{"setup_started", "callback_verified", "setup_completed", "repository_bound", "installation_disabled", "repository_disabled", "app_create_started", "app_created"} {
		mustExec(t, ctx, tx, `INSERT INTO github_audit_events(id,owner_id,actor_user_id,action,result) VALUES ($1,$2,$3,$4,'success')`, "m-"+action+randomSuffix(t), f.ownerID, f.userID, action)
	}
	failsConstraint(t, ctx, tx, "23514", "github_audit_events_action_check", `INSERT INTO github_audit_events(id,owner_id,actor_user_id,action,result) VALUES ($1,$2,$3,'app_deleted','success')`, "m-bad"+randomSuffix(t), f.ownerID, f.userID)
}

func TestManifestRuntimeGrants(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	table := func(name string, want map[string]bool) {
		for privilege, expected := range want {
			var got bool
			if err := conn.QueryRow(ctx, `SELECT has_table_privilege('autodeploy_app',$1,$2)`, "public."+name, privilege).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != expected {
				t.Fatalf("%s %s = %v, want %v", name, privilege, got, expected)
			}
		}
	}
	table("github_manifest_states", map[string]bool{"SELECT": true, "INSERT": true, "UPDATE": false, "DELETE": false, "TRUNCATE": false, "TRIGGER": false})
	table("github_manifest_apps", map[string]bool{"SELECT": true, "INSERT": true, "UPDATE": false, "DELETE": false, "TRUNCATE": false, "TRIGGER": false})
	for column, want := range map[string]bool{"consumed_at": true, "expires_at": true, "digest": false, "owner_id": false, "user_id": false, "session_id": false, "auth_revision": false, "created_at": false} {
		var got bool
		if err := conn.QueryRow(ctx, `SELECT has_column_privilege('autodeploy_app','public.github_manifest_states',$1,'UPDATE')`, column).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("column UPDATE on github_manifest_states.%s = %v, want %v", column, got, want)
		}
	}
}
