//go:build integration

package migrations

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// failsWithCode runs sql in a savepoint and requires a PgError with exactly
// the SQLSTATE (and, when non-empty, message), so a refusal cannot pass for an
// unrelated reason (missing row, CHECK, wrong role).
func failsWithCode(t *testing.T, ctx context.Context, tx pgx.Tx, code, msg, sql string, args ...any) {
	t.Helper()
	savepointCounter++
	sp := "sp_rr_" + string(rune('a'+savepointCounter%26)) + strings.Repeat("x", savepointCounter/26)
	mustExec(t, ctx, tx, "SAVEPOINT "+sp)
	_, err := tx.Exec(ctx, sql, args...)
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		t.Fatalf("expected %s, statement succeeded: %s", code, sql)
	case !errors.As(err, &pgErr):
		t.Fatalf("expected PgError, got %v", err)
	case pgErr.Code != code || (msg != "" && pgErr.Message != msg):
		t.Fatalf("expected %s %q, got %s %q: %s", code, msg, pgErr.Code, pgErr.Message, sql)
	}
	mustExec(t, ctx, tx, "ROLLBACK TO SAVEPOINT "+sp)
}

func asAppRole(t *testing.T, ctx context.Context, tx pgx.Tx) {
	t.Helper()
	mustExec(t, ctx, tx, `SET LOCAL ROLE autodeploy_app`)
}

func TestRuntimeRoleDefinition(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	var login, super, createDB, createRole, repl, bypass bool
	if err := conn.QueryRow(ctx, `SELECT rolcanlogin, rolsuper, rolcreatedb, rolcreaterole, rolreplication, rolbypassrls FROM pg_roles WHERE rolname='autodeploy_app'`).
		Scan(&login, &super, &createDB, &createRole, &repl, &bypass); err != nil {
		t.Fatalf("runtime role missing: %v", err)
	}
	if super || createDB || createRole || repl || bypass {
		t.Fatalf("runtime role over-privileged: super=%v createdb=%v createrole=%v repl=%v bypassrls=%v", super, createDB, createRole, repl, bypass)
	}
	if login {
		t.Fatal("migration must create the role NOLOGIN; the installer sets LOGIN and the password")
	}
	var owned int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM pg_class c JOIN pg_roles r ON r.oid=c.relowner WHERE r.rolname='autodeploy_app'`).Scan(&owned); err != nil || owned != 0 {
		t.Fatalf("runtime role must own nothing: owned=%d err=%v", owned, err)
	}
	var missingDML, hasTruncate, hasTrigger int
	if err := conn.QueryRow(ctx, `
SELECT
  count(*) FILTER (WHERE tablename NOT IN ('schema_migrations', 'installation_setup_tokens', 'github_manifest_states', 'github_manifest_apps') AND NOT (has_table_privilege('autodeploy_app', format('public.%I', tablename), 'SELECT')
     AND has_table_privilege('autodeploy_app', format('public.%I', tablename), 'INSERT')
     AND has_table_privilege('autodeploy_app', format('public.%I', tablename), 'UPDATE')
     AND has_table_privilege('autodeploy_app', format('public.%I', tablename), 'DELETE'))),
  count(*) FILTER (WHERE has_table_privilege('autodeploy_app', format('public.%I', tablename), 'TRUNCATE')),
  count(*) FILTER (WHERE has_table_privilege('autodeploy_app', format('public.%I', tablename), 'TRIGGER'))
FROM pg_tables WHERE schemaname='public'`).Scan(&missingDML, &hasTruncate, &hasTrigger); err != nil {
		t.Fatal(err)
	}
	// installation_setup_tokens (000032) and the github_manifest_* tables (000033) are deliberately narrower (asserted in
	// TestSetupTokenRuntimeGrants / TestManifestRuntimeGrants), so they are exempt from the full-DML rule only.
	if missingDML != 0 || hasTruncate != 0 || hasTrigger != 0 {
		t.Fatalf("grants wrong: tables lacking DML=%d with TRUNCATE=%d with TRIGGER=%d", missingDML, hasTruncate, hasTrigger)
	}
	var seqMissing int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM pg_class WHERE relkind='S' AND relnamespace='public'::regnamespace
	  AND NOT (has_sequence_privilege('autodeploy_app', oid, 'USAGE') AND has_sequence_privilege('autodeploy_app', oid, 'SELECT'))`).Scan(&seqMissing); err != nil || seqMissing != 0 {
		t.Fatalf("sequences lacking USAGE/SELECT: %d err=%v", seqMissing, err)
	}
	var schemaCreate, ledgerWrite bool
	if err := conn.QueryRow(ctx, `SELECT has_schema_privilege('autodeploy_app','public','CREATE'),
	  has_table_privilege('autodeploy_app','schema_migrations','INSERT') OR has_table_privilege('autodeploy_app','schema_migrations','UPDATE') OR has_table_privilege('autodeploy_app','schema_migrations','DELETE')`).Scan(&schemaCreate, &ledgerWrite); err != nil {
		t.Fatal(err)
	}
	if schemaCreate || ledgerWrite {
		t.Fatalf("schema CREATE=%v ledger write=%v must both be false", schemaCreate, ledgerWrite)
	}
	// Default privileges: a table and sequence created later by the migrating
	// role are usable by the runtime role without another GRANT.
	tx := beginTx(t, ctx, conn)
	mustExec(t, ctx, tx, `CREATE TABLE zz_future_table (id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY, v TEXT)`)
	asAppRole(t, ctx, tx)
	mustExec(t, ctx, tx, `INSERT INTO zz_future_table(v) VALUES ('x')`)
	failsWithCode(t, ctx, tx, "42501", "", `TRUNCATE zz_future_table`)
}

func TestRuntimeRoleOperations(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f := buildReleaseFixture(t, ctx, tx) // as the owner: setup only
	asAppRole(t, ctx, tx)

	// Deployment-path write and read.
	mustExec(t, ctx, tx, `INSERT INTO owners(id) VALUES ($1)`, "app-"+randomSuffix(t))
	var state string
	if err := tx.QueryRow(ctx, `SELECT state FROM agent_jobs WHERE id=$1`, f.jobID).Scan(&state); err != nil || state != "leased" {
		t.Fatalf("select job as runtime role: state=%q err=%v", state, err)
	}
	mustExec(t, ctx, tx, `UPDATE agent_jobs SET state='cancelled' WHERE id=$1`, f.jobID) // trigger functions run under the invoker
	// FOR UPDATE row locks (needs UPDATE privilege) and an advisory lock.
	if _, err := tx.Exec(ctx, `SELECT id FROM agent_job_attempts WHERE id=$1 FOR UPDATE`, f.attemptID); err != nil {
		t.Fatalf("row lock: %v", err)
	}
	mustExec(t, ctx, tx, `SELECT pg_advisory_xact_lock(731492845631694199)`)

	// 000022 log-chunk path: append (identity sequence), reject delete, purge under the GUC.
	attempt := logAttemptBase + 4242
	mustExec(t, ctx, tx, insertChunk, attempt, "build", 1, 1, "line")
	mustExec(t, ctx, tx, `INSERT INTO deployment_log_attempt_totals(attempt_id, bytes, chunks) VALUES ($1,4,1)`, attempt)
	mustRaiseTrigger(t, ctx, tx, "deployment log rows may only be deleted by the retention purge", `DELETE FROM deployment_log_chunks WHERE attempt_id=$1`, attempt)
	mustExec(t, ctx, tx, `SELECT set_config('autodeploy.log_purge','on',true)`)
	mustExec(t, ctx, tx, `DELETE FROM deployment_log_chunks WHERE attempt_id=$1`, attempt)
	mustExec(t, ctx, tx, `DELETE FROM deployment_log_attempt_totals WHERE attempt_id=$1`, attempt)

	// Append-only tables still reject DELETE for the runtime role (trigger, not privilege).
	mustRaiseTrigger(t, ctx, tx, "agent job attempts are retained", `DELETE FROM agent_job_attempts WHERE id=$1`, f.attemptID)

	// Superuser-only escape hatches are refused.
	failsWithCode(t, ctx, tx, "42501", "", `TRUNCATE agent_jobs CASCADE`)
	failsWithCode(t, ctx, tx, "42501", "", `ALTER TABLE agent_jobs DISABLE TRIGGER ALL`)
	failsWithCode(t, ctx, tx, "42501", "", `ALTER TABLE agent_jobs DISABLE TRIGGER agent_jobs_validate_update`)
	failsWithCode(t, ctx, tx, "42501", "", `SET session_replication_role = replica`)
	failsWithCode(t, ctx, tx, "42501", "", `CREATE TABLE zz_nope (id INT)`)
	failsWithCode(t, ctx, tx, "42501", "", `INSERT INTO schema_migrations(name) VALUES ('x')`)
	failsWithCode(t, ctx, tx, "42501", "", `DROP TRIGGER agent_jobs_validate_update ON agent_jobs`)
	failsWithCode(t, ctx, tx, "42501", "", `COPY (SELECT 1) TO PROGRAM 'true'`)
}

func TestRetentionPurgeGate(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f := buildReleaseFixture(t, ctx, tx)
	old := time.Now().UTC().Add(-100 * 24 * time.Hour)

	// webhook_deliveries: processed and pending receipts, both 100 days old.
	mustExec(t, ctx, tx, `INSERT INTO github_app_registrations(id, app_id, client_id, slug, owner_account_id, owner_type) VALUES (1,9000001,'c','s',1,'User') ON CONFLICT DO NOTHING`)
	insDelivery := `INSERT INTO webhook_deliveries(app_registration_id, delivery_id, event_type, body_digest, verification_state, processing_state, installation_id, repository_id, github_owner_id, created_at, processed_at)
	  VALUES (1,$1,'push',$2,'verified',$3::text,1,1,1,$4::timestamptz,CASE WHEN $3::text='processed' THEN $4::timestamptz ELSE NULL END)`
	mustExec(t, ctx, tx, insDelivery, "proc-"+randomSuffix(t), randomBytes(t, 32), "processed", old)
	mustExec(t, ctx, tx, insDelivery, "pend-"+randomSuffix(t), randomBytes(t, 32), "pending", old)

	// An installation receipt with a lifecycle-repository child (must stay undeletable).
	var lifecycleID int64
	if err := tx.QueryRow(ctx, `INSERT INTO webhook_deliveries(app_registration_id, delivery_id, event_type, action, body_digest, verification_state, processing_state, installation_id, github_owner_id, created_at)
	  VALUES (1,$1,'installation','created',$2,'verified','pending',5,6,$3) RETURNING id`, "life-"+randomSuffix(t), randomBytes(t, 32), old).Scan(&lifecycleID); err != nil {
		t.Fatal(err)
	}
	mustExec(t, ctx, tx, `INSERT INTO webhook_lifecycle_repositories(delivery_id, repository_id) VALUES ($1,77)`, lifecycleID)
	mustExec(t, ctx, tx, `UPDATE webhook_deliveries SET processing_state='processed', processed_at=now() WHERE id=$1`, lifecycleID)

	// agent_job_events for the (non-terminal, leased) fixture job.
	mustExec(t, ctx, tx, `INSERT INTO agent_job_events(job_id, sequence, event_type, occurred_at) VALUES ($1,1,'enqueued',$2)`, f.jobID, old)

	// route_events.
	insertRouteHead(t, ctx, tx, f.environmentID, "r-"+randomSuffix(t)+".example.com")
	mustExec(t, ctx, tx, `INSERT INTO route_events(environment_id, event_kind, route_revision, hostname, created_at) VALUES ($1,'hostname_assigned',0,'h.example.com',$2)`, f.environmentID, old)

	newPrepareOp(t, ctx, tx, "succeeded") // a closed release_operations row the purge must never reach
	asAppRole(t, ctx, tx)
	const purged = `autodeploy.retention_purge`
	failsDel := func(msg, sql string, args ...any) { mustRaiseTrigger(t, ctx, tx, msg, sql, args...) }
	// Without the GUC: all reject.
	failsDel("webhook deliveries are retained", `DELETE FROM webhook_deliveries WHERE created_at < now() - interval '90 days'`)
	failsDel("agent job events are append-only", `DELETE FROM agent_job_events WHERE occurred_at < now() - interval '90 days'`)
	failsDel("route events are append-only", `DELETE FROM route_events WHERE created_at < now() - interval '90 days'`)
	// The log-purge GUC must not unlock them.
	mustExec(t, ctx, tx, `SELECT set_config('autodeploy.log_purge','on',true)`)
	failsDel("route events are append-only", `DELETE FROM route_events WHERE created_at < now() - interval '90 days'`)
	// Wrong value does not unlock.
	mustExec(t, ctx, tx, `SELECT set_config($1,'yes',true)`, purged)
	failsDel("route events are append-only", `DELETE FROM route_events WHERE created_at < now() - interval '90 days'`)

	mustExec(t, ctx, tx, `SELECT set_config($1,'on',true)`, purged)
	// Structural guards hold even with the GUC.
	// webhook_deliveries is never purged: the original trigger rejects even
	// old, processed rows under the retention GUC.
	failsDel("webhook deliveries are retained", `DELETE FROM webhook_deliveries WHERE processing_state <> 'pending' AND created_at < now() - interval '90 days'`)
	failsDel("webhook deliveries are retained", `DELETE FROM webhook_deliveries WHERE processing_state='pending'`)
	failsDel("webhook lifecycle repositories are immutable", `DELETE FROM webhook_lifecycle_repositories`)
	failsDel("events of non-terminal jobs cannot be purged", `DELETE FROM agent_job_events WHERE job_id=$1`, f.jobID)
	// UPDATE stays rejected, and audit/release tables are untouched by this path.
	failsWithCode(t, ctx, tx, "P0001", "route events are append-only", `UPDATE route_events SET route_revision=1 WHERE environment_id=$1`, f.environmentID)
	failsWithCode(t, ctx, tx, "P0001", "", `DELETE FROM release_operations`) // still rejected under the purge GUC

	var n int64
	del := func(sql string, args ...any) int64 {
		tag, err := tx.Exec(ctx, sql, args...)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return tag.RowsAffected()
	}
	if n = del(`DELETE FROM route_events WHERE created_at < now() - interval '90 days'`); n != 1 {
		t.Fatalf("route events purged = %d", n)
	}
	// Terminal job: its events become purgeable.
	mustExec(t, ctx, tx, `UPDATE agent_jobs SET state='cancelled' WHERE id=$1`, f.jobID)
	if n = del(`DELETE FROM agent_job_events WHERE occurred_at < now() - interval '90 days'`); n != 1 {
		t.Fatalf("job events purged = %d", n)
	}
}

// TestPurgeAndRuntimeIndexesUsable proves the planner can use each new index
// (enable_seqscan=off removes the tiny-table seq-scan preference; the
// natural-plan evidence on 100k seeded rows is in the 22-7 report).
func TestPurgeAndRuntimeIndexesUsable(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	mustExec(t, ctx, tx, `SET LOCAL enable_seqscan = off`)
	cases := []struct{ index, query string }{
		{"release_operations_attempt_fence_idx", `SELECT 1 FROM release_operations WHERE attempt_id=1 AND fence=1`},
		{"release_operations_job_idx", `SELECT 1 FROM release_operations WHERE job_id=1`},
		{"agent_jobs_rollback_target_idx", `SELECT 1 FROM agent_jobs WHERE rollback_target_release_id='r' AND state IN ('queued','leased','retry_wait')`},
		{"agent_jobs_owner_project_environment_idx", `SELECT environment_id FROM agent_jobs WHERE owner_id='o' AND project_id='p'`},
		{"agent_job_events_occurred_at_idx", `SELECT job_id FROM agent_job_events WHERE occurred_at < now() - interval '90 days'`},
		{"route_events_created_at_idx", `SELECT id FROM route_events WHERE created_at < now() - interval '90 days'`},
	}
	for _, c := range cases {
		rows, err := tx.Query(ctx, `EXPLAIN `+c.query)
		if err != nil {
			t.Fatalf("%s: %v", c.index, err)
		}
		var plan strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			plan.WriteString(line + "\n")
		}
		rows.Close()
		if !strings.Contains(plan.String(), c.index) {
			t.Errorf("planner does not use %s:\n%s", c.index, plan.String())
		}
	}
}
