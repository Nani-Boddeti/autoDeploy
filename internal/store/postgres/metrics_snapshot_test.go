//go:build integration

package postgres

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"testing"
	"time"

	"autodeploy/internal/errclass"

	"github.com/jackc/pgx/v5/pgxpool"
)

// groupCounts is the independent oracle: the same facts read with plain SQL.
func groupCounts(t *testing.T, pool *pgxpool.Pool, query string) map[string]int64 {
	t.Helper()
	rows, err := pool.Query(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var k string
		var n int64
		if err := rows.Scan(&k, &n); err != nil {
			t.Fatal(err)
		}
		out[k] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func assertCounts(t *testing.T, name string, got, observed map[string]int64, known []string) {
	t.Helper()
	for _, s := range known {
		if _, ok := got[s]; !ok {
			t.Errorf("%s: known state %q not zero-filled", name, s)
		}
	}
	for s, n := range observed {
		if got[s] != n {
			t.Errorf("%s[%s] = %d, want %d", name, s, got[s], n)
		}
	}
	for s, n := range got {
		if n != observed[s] {
			t.Errorf("%s[%s] = %d, want %d", name, s, n, observed[s])
		}
	}
}

func TestMetricsSnapshotCountsAreCorrect(t *testing.T) {
	a := newActivatedFixture(t, 150, false) // leased job, candidate release, one open activation operation
	ctx := context.Background()
	a.insertServer(t, "server-silent") // never heartbeated: must be omitted
	a.insertServer(t, "server-revoked")
	a.insertJob(t, "server", "env-two", "metrics-job-1", 1)
	a.insertJob(t, "server", "env-three", "metrics-job-2", 1)
	for _, q := range []string{
		`UPDATE servers SET last_heartbeat_at = clock_timestamp() WHERE id = 'server'`,
		`UPDATE servers SET last_heartbeat_at = clock_timestamp() WHERE id = 'server-revoked'`,
		`UPDATE servers SET status = 'revoked', revoked_at = clock_timestamp() WHERE id = 'server-revoked'`,
	} {
		if _, err := a.pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	snap, err := NewMetricsRepository(a.pool).MetricsSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertCounts(t, "agent_jobs", snap.AgentJobs, groupCounts(t, a.pool, `SELECT state, count(*) FROM agent_jobs GROUP BY state`), AgentJobStates)
	assertCounts(t, "releases", snap.Releases, groupCounts(t, a.pool, `SELECT state, count(*) FROM releases GROUP BY state`), ReleaseStates)
	assertCounts(t, "webhook_jobs", snap.WebhookJobs, groupCounts(t, a.pool, `SELECT processing_state, count(*) FROM webhook_deliveries WHERE processing_state = 'pending' GROUP BY processing_state`), WebhookJobStates)
	if snap.AgentJobs["queued"] != 2 || snap.AgentJobs["leased"] != 1 {
		t.Fatalf("agent jobs = %v, want 2 queued and 1 leased", snap.AgentJobs)
	}
	if snap.ReleaseOperationsOpen != 1 || snap.EnvironmentsReconciliationRequired != 0 {
		t.Fatalf("open ops = %d reconciliation = %d, want 1 and 0", snap.ReleaseOperationsOpen, snap.EnvironmentsReconciliationRequired)
	}
	age, ok := snap.HeartbeatAgeSeconds["server"]
	if !ok || age < 0 || age > 60 || len(snap.HeartbeatAgeSeconds) != 1 {
		t.Fatalf("heartbeat ages = %v, want only the active, heartbeated server", snap.HeartbeatAgeSeconds)
	}
}

func TestMetricsSnapshotCountsReconciliationRequired(t *testing.T) {
	b := plainBlock(t, 160)
	snap, err := NewMetricsRepository(b.pool).MetricsSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.EnvironmentsReconciliationRequired != 1 {
		t.Fatalf("reconciliation required = %d, want 1", snap.EnvironmentsReconciliationRequired)
	}
}

// The state lists are mirrored from CHECK constraints; this fails if a
// migration adds or renames a state without the Go constant following.
func TestMetricsKnownStatesMatchCheckConstraints(t *testing.T) {
	pool := newLeaseFixture(t, 171).pool
	literal := regexp.MustCompile(`'([a-z_]+)'`)
	for _, c := range []struct {
		constraint string
		want       []string
	}{
		{"agent_jobs_state_check", AgentJobStates},
		{"releases_state_check", ReleaseStates},
		{"webhook_deliveries_processing_state_check", AllWebhookJobStates},
	} {
		var def string
		if err := pool.QueryRow(context.Background(), `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = $1`, c.constraint).Scan(&def); err != nil {
			t.Fatalf("%s: %v", c.constraint, err)
		}
		var db []string
		for _, m := range literal.FindAllStringSubmatch(def, -1) {
			db = append(db, m[1])
		}
		slices.Sort(db)
		want := slices.Clone(c.want)
		slices.Sort(want)
		if !slices.Equal(db, want) {
			t.Fatalf("%s: db states %v, Go states %v", c.constraint, db, want)
		}
	}
}

func TestMetricsSnapshotStatementTimeoutReturnsErrorNotHang(t *testing.T) {
	a := newLeaseFixture(t, 170)
	ctx := context.Background()
	lock, err := a.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(ctx) //nolint:errcheck
	if _, err := lock.Exec(ctx, `LOCK TABLE agent_jobs IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	repo := NewMetricsRepository(a.pool)
	repo.statementTimeout = 150 * time.Millisecond
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	start := time.Now()
	_, err = repo.MetricsSnapshot(callCtx)
	if err == nil {
		t.Fatal("expected an error while the table is locked")
	}
	if time.Since(start) > 5*time.Second || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("not the server-side statement timeout: %v after %v", err, time.Since(start))
	}
	if got := errclass.Of(err); got != "postgres_57014" {
		t.Fatalf("class = %s, want postgres_57014 (query_canceled)", got)
	}
}
