//go:build integration

package migrations

import (
	"context"
	"strings"
	"testing"
)

// TestDeploymentLogChunksHealthIndex asserts the 000031 partial index exists
// and that the newest-health-line lookup shape used by attemptHealthLaterals
// (internal/store/postgres/operations_read.go) is planned through it.
func TestDeploymentLogChunksHealthIndex(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)

	var def string
	if err := conn.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE schemaname='public' AND tablename='deployment_log_chunks' AND indexname='deployment_log_chunks_health_idx'`).Scan(&def); err != nil {
		t.Fatalf("health index missing: %v", err)
	}
	for _, want := range []string{"(attempt_id, id DESC)", "WHERE (stream = 'health'::text)"} {
		if !strings.Contains(def, want) {
			t.Fatalf("index definition %q lacks %q", def, want)
		}
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)

	// Fixture: one attempt with many build/phase chunks and a few health ones
	// (appends only; the rollback discards it, so no purge GUC is needed).
	// Planner settings are the defaults; ANALYZE below only refreshes stats.
	const attemptID = 987654321
	mustExec(t, ctx, tx, `INSERT INTO deployment_log_chunks (attempt_id, stream, writer_epoch, seq, body)
	  SELECT $1, CASE WHEN g % 2 = 0 THEN 'build' ELSE 'phase' END, 1, g, 'line ' || g FROM generate_series(1, 2000) g`, attemptID)
	// Other attempts' chunks, so attempt_id is selective as in production.
	mustExec(t, ctx, tx, `INSERT INTO deployment_log_chunks (attempt_id, stream, writer_epoch, seq, body)
	  SELECT $1 + a, CASE g % 3 WHEN 0 THEN 'build' WHEN 1 THEN 'phase' ELSE 'health' END, 1, g, 'other ' || g
	  FROM generate_series(1, 200) a, generate_series(1, 200) g`, attemptID)
	// A long-running gate emits many health lines; the unique key
	// (attempt_id, stream, ...) already narrows to them but cannot give the
	// newest match without sorting all of them.
	mustExec(t, ctx, tx, `INSERT INTO deployment_log_chunks (attempt_id, stream, writer_epoch, seq, body)
	  SELECT $1, 'health', 1, 5000 + g, 'health gate: poll ' || g FROM generate_series(1, 2000) g`, attemptID)
	mustExec(t, ctx, tx, `INSERT INTO deployment_log_chunks (attempt_id, stream, writer_epoch, seq, body) VALUES
	  ($1,'health',1,9001,'health gate: last probe: status=503'),
	  ($1,'health',1,9002,'health gate: failed (reason=timeout)')`, attemptID)
	mustExec(t, ctx, tx, `ANALYZE deployment_log_chunks`)

	for _, like := range []string{`health gate: failed (reason=%`, `health gate: last probe: %`} {
		rows, err := tx.Query(ctx, `EXPLAIN (FORMAT TEXT) SELECT c.body FROM deployment_log_chunks c WHERE c.attempt_id=$1 AND c.stream='health' AND c.body LIKE $2 ORDER BY c.id DESC LIMIT 1`, attemptID, like)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, line)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		text := strings.Join(plan, "\n")
		if !strings.Contains(text, "deployment_log_chunks_health_idx") {
			t.Fatalf("lookup for %q does not use the health index (default planner settings, after ANALYZE):\n%s", like, text)
		}
		t.Logf("plan for %q:\n%s", like, text)
	}
}
