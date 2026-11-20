//go:build integration

package postgres

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// runtimeRolePool connects like the installer's runtime credential: every
// session runs as autodeploy_app (DML only, no TRUNCATE, not an owner).
func runtimeRolePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(os.Getenv("AUTODEPLOY_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, `SET ROLE autodeploy_app`)
		return err
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// asReplica runs fixture-only statements with triggers bypassed (the
// superuser test connection), which the append-only guards otherwise forbid.
func replicaExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, sql, args...); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func countOf(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestEventRetentionPurgeAsRuntimeRole(t *testing.T) {
	ctx := context.Background()
	f := newLeaseFixture(t, 90) // queued job (non-terminal) + a delivery WITH an intent
	pool := f.pool
	const old, recent = "now() - interval '200 days'", "now() - interval '1 day'"

	// --- agent_job_events ---
	f.insertRuntimeConfig(t, "env-ret")
	terminalOld := f.insertJobAt(t, f.serverID, "env-ret", "ret-old", 0, "clock_timestamp()")
	terminalNew := f.insertJobAt(t, f.serverID, "env-ret", "ret-new", 0, "clock_timestamp()")
	for _, id := range []int64{terminalOld, terminalNew} {
		if _, err := pool.Exec(ctx, `UPDATE agent_jobs SET state='cancelled' WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
	}
	for i, kind := range []string{"cancelled", "reconciled", "activation_late_report"} {
		if _, err := pool.Exec(ctx, `INSERT INTO agent_job_events(job_id,sequence,event_type) VALUES ($1,$2,$3),($4,$2,$3)`, terminalOld, i+2, kind, terminalNew); err != nil {
			t.Fatal(err)
		}
	}
	replicaExec(t, pool, `UPDATE agent_job_events SET occurred_at = `+old+` WHERE job_id = ANY($1)`, []int64{f.jobID, terminalOld})
	// f.jobID stays queued: its 200-day-old event must survive.
	replicaExec(t, pool, `UPDATE agent_job_events SET occurred_at = `+recent+` WHERE job_id=$1`, terminalNew)

	// --- webhook deliveries ---
	ins := `INSERT INTO webhook_deliveries(app_registration_id,delivery_id,event_type,action,body_digest,verification_state,processing_state,failure_class,created_at)
		VALUES (1,$1,'push','',decode(lpad(to_hex($2::int),64,'0'),'hex'),'rejected','rejected','signature',` + old + `)`
	for i, id := range []string{"old-rejected", "old-rejected-2"} {
		if _, err := pool.Exec(ctx, ins, id, 1000+i); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO webhook_deliveries(app_registration_id,delivery_id,event_type,action,body_digest,verification_state,processing_state,failure_class,created_at)
		VALUES (1,'recent-rejected','push','',decode(lpad(to_hex(1002),64,'0'),'hex'),'rejected','rejected','signature',`+recent+`)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO webhook_deliveries(app_registration_id,delivery_id,event_type,action,body_digest,verification_state,processing_state,failure_class,installation_id,repository_id,github_owner_id,created_at)
		VALUES (1,'old-pending','push','',decode(lpad(to_hex(1003),64,'0'),'hex'),'verified','pending','',5,6,7,`+old+`)`); err != nil {
		t.Fatal(err)
	}
	// Old processed installation receipt WITH a lifecycle-repository child.
	var lifecycleID int64
	if err := pool.QueryRow(ctx, `INSERT INTO webhook_deliveries(app_registration_id,delivery_id,event_type,action,body_digest,verification_state,processing_state,failure_class,installation_id,github_owner_id,created_at)
		VALUES (1,'old-lifecycle','installation','created',decode(lpad(to_hex(1004),64,'0'),'hex'),'verified','pending','',5,6,`+old+`) RETURNING id`).Scan(&lifecycleID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO webhook_lifecycle_repositories(delivery_id,repository_id) VALUES ($1,77)`, lifecycleID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE webhook_deliveries SET processing_state='processed', processed_at=now() WHERE id=$1`, lifecycleID); err != nil {
		t.Fatal(err)
	}
	// The fixture's delivery owns an automatic_deployment_intents row; make it old.
	replicaExec(t, pool, `UPDATE webhook_deliveries SET created_at = `+old+` WHERE id = (SELECT delivery_id FROM automatic_deployment_intents LIMIT 1)`)
	intentDeliveries := countOf(t, pool, `SELECT count(*) FROM automatic_deployment_intents`)
	if intentDeliveries != 1 {
		t.Fatalf("fixture expected 1 intent, got %d", intentDeliveries)
	}

	// --- route_events ---
	f.insertRouteHead(t, "retention.example.com")
	for i, age := range []string{old, old, recent} {
		if _, err := pool.Exec(ctx, `INSERT INTO route_events(environment_id,event_kind,route_revision,hostname,created_at) VALUES ($1,'hostname_assigned',$2,'retention.example.com',`+age+`)`, f.envID, i); err != nil {
			t.Fatal(err)
		}
	}

	// --- purge as the runtime role, tiny batches to exercise looping ---
	rt := runtimeRolePool(t)
	var role string
	if err := rt.QueryRow(ctx, `SELECT current_user`).Scan(&role); err != nil || role != "autodeploy_app" {
		t.Fatalf("runtime pool role %q err %v", role, err)
	}
	var errCount atomic.Int32
	p, err := NewEventRetentionPurger(rt, EventRetentionOptions{BatchSize: 1, OnError: func() { errCount.Add(1) }})
	if err != nil {
		t.Fatal(err)
	}

	// A second purger (another replica) must skip while the lock is held.
	holder, err := pgx.Connect(ctx, os.Getenv("AUTODEPLOY_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	var got bool
	if err = holder.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, eventRetentionAdvisoryLockKey).Scan(&got); err != nil || !got {
		t.Fatalf("holder lock: %v %v", got, err)
	}
	p.RunOnce(ctx)
	if n := countOf(t, pool, `SELECT count(*) FROM route_events WHERE created_at < now() - interval '90 days'`); n != 2 {
		t.Fatalf("purger ran while another held the advisory lock (old route events left: %d)", n)
	}
	if _, err = holder.Exec(ctx, `SELECT pg_advisory_unlock($1)`, eventRetentionAdvisoryLockKey); err != nil {
		t.Fatal(err)
	}
	holder.Close(ctx)

	p.RunOnce(ctx)
	if errCount.Load() != 0 {
		t.Fatal("purge reported an error")
	}

	// events: only terminal+old+not-protected kinds are gone.
	for _, c := range []struct {
		name string
		sql  string
		args []any
		want int
	}{
		{"old terminal job: enqueued+cancelled purged", `SELECT count(*) FROM agent_job_events WHERE job_id=$1 AND event_type IN ('enqueued','cancelled')`, []any{terminalOld}, 0},
		{"old terminal job: reconciled kept", `SELECT count(*) FROM agent_job_events WHERE job_id=$1 AND event_type='reconciled'`, []any{terminalOld}, 1},
		{"old terminal job: late_report kept", `SELECT count(*) FROM agent_job_events WHERE job_id=$1 AND event_type='activation_late_report'`, []any{terminalOld}, 1},
		{"recent terminal job events kept", `SELECT count(*) FROM agent_job_events WHERE job_id=$1`, []any{terminalNew}, 4},
		{"non-terminal job old event kept", `SELECT count(*) FROM agent_job_events WHERE job_id=$1`, []any{f.jobID}, 1},
		{"old rejected deliveries kept", `SELECT count(*) FROM webhook_deliveries WHERE delivery_id LIKE 'old-rejected%'`, nil, 2},
		{"old processed delivery + child kept", `SELECT count(*) FROM webhook_deliveries WHERE delivery_id='old-lifecycle' AND processing_state='processed'`, nil, 1},
		{"lifecycle child kept", `SELECT count(*) FROM webhook_lifecycle_repositories`, nil, 1},
		{"recent delivery kept", `SELECT count(*) FROM webhook_deliveries WHERE delivery_id='recent-rejected'`, nil, 1},
		{"old pending delivery kept", `SELECT count(*) FROM webhook_deliveries WHERE delivery_id='old-pending'`, nil, 1},
		{"old delivery with intent kept", `SELECT count(*) FROM webhook_deliveries d JOIN automatic_deployment_intents i ON i.delivery_id=d.id WHERE d.created_at < now() - interval '90 days'`, nil, 1},
		{"old route events purged", `SELECT count(*) FROM route_events WHERE created_at < now() - interval '90 days'`, nil, 0},
		{"recent route event kept", `SELECT count(*) FROM route_events`, nil, 1},
	} {
		if got := countOf(t, pool, c.sql, c.args...); got != c.want {
			t.Errorf("%s: got %d want %d", c.name, got, c.want)
		}
	}

	// Second run is a no-op (idempotent) and the role still cannot bypass the GUC guard.
	p.RunOnce(ctx)
	if _, err := rt.Exec(ctx, `DELETE FROM route_events`); err == nil {
		t.Fatal("runtime role deleted route_events without the retention GUC")
	}
	// Deliveries are never deletable, not even old non-pending ones inside a
	// transaction that sets the retention GUC.
	tx, err := rt.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, retentionGUCStatement); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM webhook_deliveries WHERE processing_state <> 'pending' AND created_at < now() - interval '90 days'`); err == nil || !strings.Contains(err.Error(), "webhook deliveries are retained") {
		t.Fatalf("delivery delete under retention GUC must be rejected, got %v", err)
	}
}
