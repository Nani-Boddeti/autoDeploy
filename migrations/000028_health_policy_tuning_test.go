//go:build integration

package migrations

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"
)

var policyTables = []string{"environment_runtime_configs", "agent_job_attempt_execution"}

// acceptedInSavepoint runs sql and requires success, then rolls the savepoint
// back so a frozen (insert-once) table can be probed repeatedly.
func acceptedInSavepoint(t *testing.T, ctx context.Context, tx pgx.Tx, what, sql string, args ...any) {
	t.Helper()
	savepointCounter++
	sp := fmt.Sprintf("sp_%d", savepointCounter)
	if _, err := tx.Exec(ctx, "SAVEPOINT "+sp); err != nil {
		t.Fatalf("create savepoint: %v", err)
	}
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("expected acceptance of %s: %v", what, err)
	}
	if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT "+sp); err != nil {
		t.Fatalf("rollback to savepoint: %v", err)
	}
}

// policyInsert returns an INSERT for table whose five policy columns come from
// $2..$6 (threshold, interval, timeout, deadline, tcp port) and whose key is $1.
func policyInsert(table string) string {
	if table == "environment_runtime_configs" {
		return `INSERT INTO environment_runtime_configs
		 (environment_id, container_port, container_scheme, release_strategy, drain_seconds, cpu_millis, memory_bytes, restart_policy, health_policy_revision, health_gate_host,
		  health_success_threshold, health_interval_seconds, health_timeout_seconds, health_deadline_seconds, health_tcp_port)
		 VALUES ($1,8080,'http','blue_green',30,500,268435456,'unless-stopped',1,'app.internal',$2,$3,$4,$5,$6)`
	}
	return `INSERT INTO agent_job_attempt_execution
		 (attempt_id, container_port, container_scheme, release_strategy, drain_seconds, health_policy_revision, cpu_millis, memory_bytes, restart_policy, environment_runtime_config_revision, health_gate_path, health_gate_host,
		  health_success_threshold, health_interval_seconds, health_timeout_seconds, health_deadline_seconds, health_tcp_port)
		 VALUES ($1,8080,'http','blue_green',30,1,500,268435456,'unless-stopped',0,'/','app.internal',$2,$3,$4,$5,$6)`
}

type policyCase struct {
	name                                   string
	threshold, interval, timeout, deadline int
	tcp                                    any
	ok                                     bool
}

func policyCases() []policyCase {
	return []policyCase{
		{"defaults", 3, 2, 2, 60, nil, true},
		{"threshold min", 1, 2, 2, 60, nil, true},
		{"threshold max", 10, 6, 2, 60, nil, true},
		{"threshold 0", 0, 2, 2, 60, nil, false},
		{"threshold 11", 11, 2, 2, 600, nil, false},
		{"interval min", 3, 1, 1, 60, nil, true},
		{"interval max", 3, 30, 2, 600, nil, true},
		{"interval 0", 3, 0, 0, 60, nil, false},
		{"interval 31", 3, 31, 2, 600, nil, false},
		{"timeout min", 3, 2, 1, 60, nil, true},
		{"timeout max", 3, 30, 30, 600, nil, true},
		{"timeout 0", 3, 2, 0, 60, nil, false},
		{"timeout 31", 3, 31, 31, 600, nil, false},
		{"deadline min", 3, 2, 2, 10, nil, true},
		{"deadline max", 3, 2, 2, 600, nil, true},
		{"deadline 9", 1, 2, 2, 9, nil, false},
		{"deadline 601", 3, 2, 2, 601, nil, false},
		{"tcp min", 3, 2, 2, 60, 1, true},
		{"tcp max", 3, 2, 2, 60, 65535, true},
		{"tcp 0", 3, 2, 2, 60, 0, false},
		{"tcp 65536", 3, 2, 2, 60, 65536, false},
		{"tcp negative", 3, 2, 2, 60, -1, false},
		{"timeout equals interval", 3, 5, 5, 60, nil, true},
		{"timeout above interval", 3, 2, 3, 60, nil, false},
		{"deadline equals threshold*interval", 3, 5, 5, 15, nil, true},
		{"deadline one below threshold*interval", 3, 5, 5, 14, nil, false},
		{"deadline below product at max", 10, 30, 2, 299, nil, false},
		{"deadline equals product at max", 10, 30, 2, 300, nil, true},
	}
}

// TestHealthPolicyColumnsShapeBothTables proves both tables carry the five
// columns with the right nullability, that the live table defaults them to
// ADR 0005's policy, and that the frozen table has NO default (a frozen row
// must always carry the exact values its attempt ran with).
func TestHealthPolicyColumnsShapeBothTables(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	cols := []string{"health_success_threshold", "health_interval_seconds", "health_timeout_seconds", "health_deadline_seconds", "health_tcp_port"}
	liveDefaults := map[string]string{"health_success_threshold": "3", "health_interval_seconds": "2", "health_timeout_seconds": "2", "health_deadline_seconds": "60"}
	for _, table := range policyTables {
		for _, col := range cols {
			var nullable string
			var def *string
			var dataType string
			err := conn.QueryRow(ctx, `SELECT is_nullable, column_default, data_type FROM information_schema.columns WHERE table_schema=current_schema() AND table_name=$1 AND column_name=$2`, table, col).Scan(&nullable, &def, &dataType)
			if err != nil {
				t.Fatalf("%s.%s: %v", table, col, err)
			}
			wantNullable, wantType := "NO", "smallint"
			if col == "health_tcp_port" {
				wantNullable, wantType = "YES", "integer"
			}
			if nullable != wantNullable || dataType != wantType {
				t.Errorf("%s.%s nullable=%s type=%s, want %s/%s", table, col, nullable, dataType, wantNullable, wantType)
			}
			wantDef, hasDef := liveDefaults[col]
			if table == "environment_runtime_configs" && hasDef {
				if def == nil || *def != wantDef {
					t.Errorf("%s.%s default = %v, want %s", table, col, def, wantDef)
				}
			} else if def != nil {
				t.Errorf("%s.%s has default %q, want none", table, col, *def)
			}
		}
		for _, suffix := range []string{"health_success_threshold_check", "health_interval_seconds_check", "health_timeout_seconds_check", "health_deadline_seconds_check", "health_tcp_port_check", "health_timeout_interval_check", "health_deadline_streak_check"} {
			var n int
			if err := conn.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE conname=$1 AND conrelid=$2::regclass`, table+"_"+suffix, table).Scan(&n); err != nil || n != 1 {
				t.Errorf("constraint %s_%s count=%d err=%v, want 1 (explicit name)", table, suffix, n, err)
			}
		}
	}
}

// TestHealthPolicyLiveTableDefaultsWhenOmitted proves an INSERT that names none
// of the new columns yields 3/2/2/60/NULL, so every pre-22-1 caller is unchanged.
func TestHealthPolicyLiveTableDefaultsWhenOmitted(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	env := "env-policy-default-" + randomSuffix(t)
	if _, err := tx.Exec(ctx, `INSERT INTO environment_runtime_configs
	      (environment_id, container_port, container_scheme, release_strategy, drain_seconds, cpu_millis, memory_bytes, restart_policy, health_policy_revision, health_gate_host)
	      VALUES ($1,8080,'http','blue_green',30,500,268435456,'unless-stopped',1,'app.internal')`, env); err != nil {
		t.Fatal(err)
	}
	var s, i, to, d int
	var tcpNull bool
	if err := tx.QueryRow(ctx, `SELECT health_success_threshold, health_interval_seconds, health_timeout_seconds, health_deadline_seconds, health_tcp_port IS NULL FROM environment_runtime_configs WHERE environment_id=$1`, env).Scan(&s, &i, &to, &d, &tcpNull); err != nil {
		t.Fatal(err)
	}
	if s != 3 || i != 2 || to != 2 || d != 60 || !tcpNull {
		t.Fatalf("defaults = %d/%d/%d/%d null=%v, want 3/2/2/60/NULL", s, i, to, d, tcpNull)
	}
}

// TestHealthPolicyBoundsAndCrossChecks drives every bound (edge accepted, just
// outside rejected) and both cross CHECKs against both tables.
func TestHealthPolicyBoundsAndCrossChecks(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f := buildReleaseFixture(t, ctx, tx)
	for _, table := range policyTables {
		sql := policyInsert(table)
		for _, c := range policyCases() {
			key := any("env-policy-" + randomSuffix(t))
			if table == "agent_job_attempt_execution" {
				key = f.attemptID
			}
			args := []any{key, c.threshold, c.interval, c.timeout, c.deadline, c.tcp}
			what := table + ": " + c.name
			if c.ok {
				acceptedInSavepoint(t, ctx, tx, what, sql, args...)
			} else {
				mustFail(t, ctx, tx, "expected rejection of "+what, sql, args...)
			}
		}
	}
}

// TestAgentJobAttemptExecutionHealthPolicyRequiredAndImmutable proves the
// frozen table rejects an insert that omits any new NOT NULL column (no
// default), accepts a NULL tcp port, and that the existing insert-once trigger
// rejects an UPDATE of every new column.
func TestAgentJobAttemptExecutionHealthPolicyRequiredAndImmutable(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f := buildReleaseFixture(t, ctx, tx)

	mustFail(t, ctx, tx, "expected rejection of frozen insert without policy columns",
		`INSERT INTO agent_job_attempt_execution
		 (attempt_id, container_port, container_scheme, release_strategy, drain_seconds, health_policy_revision, cpu_millis, memory_bytes, restart_policy, environment_runtime_config_revision, health_gate_path, health_gate_host)
		 VALUES ($1,8080,'http','blue_green',30,1,500,268435456,'unless-stopped',0,'/','app.internal')`, f.attemptID)

	if _, err := tx.Exec(ctx, policyInsert("agent_job_attempt_execution"), f.attemptID, 5, 4, 3, 120, nil); err != nil {
		t.Fatalf("insert frozen policy row: %v", err)
	}
	for _, set := range []string{"health_success_threshold=4", "health_interval_seconds=5", "health_timeout_seconds=2", "health_deadline_seconds=90", "health_tcp_port=80"} {
		mustFail(t, ctx, tx, "expected update rejection (insert-once) of "+set,
			`UPDATE agent_job_attempt_execution SET `+set+` WHERE attempt_id=$1`, f.attemptID)
	}
	var s, i, to, d int
	var tcpNull bool
	if err := tx.QueryRow(ctx, `SELECT health_success_threshold, health_interval_seconds, health_timeout_seconds, health_deadline_seconds, health_tcp_port IS NULL FROM agent_job_attempt_execution WHERE attempt_id=$1`, f.attemptID).Scan(&s, &i, &to, &d, &tcpNull); err != nil {
		t.Fatal(err)
	}
	if s != 5 || i != 4 || to != 3 || d != 120 || !tcpNull {
		t.Fatalf("frozen = %d/%d/%d/%d null=%v", s, i, to, d, tcpNull)
	}
}

// TestHealthPolicyMigrationBackfillsExistingFrozenRows applies migrations
// 000001..000027 to a scratch database, inserts a live config and a frozen
// execution row the way the pre-22-1 schema allowed, applies 000028, and
// proves both rows end up with 3/2/2/60/NULL (the frozen table has no default
// afterwards, so this exercises the ADD COLUMN DEFAULT then DROP DEFAULT step,
// including that it does not trip the immutability trigger).
func TestHealthPolicyMigrationBackfillsExistingFrozenRows(t *testing.T) {
	dsn := os.Getenv("AUTODEPLOY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AUTODEPLOY_TEST_DATABASE_URL is required for PostgreSQL integration tests")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close(ctx) })
	scratch := "autodeploy_bf_" + randomSuffix(t)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+scratch); err != nil {
		t.Skipf("cannot create scratch database: %v", err)
	}
	t.Cleanup(func() { admin.Exec(ctx, "DROP DATABASE IF EXISTS "+scratch+" WITH (FORCE)") }) //nolint:errcheck

	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = scratch
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(ctx) })

	original := migrationFiles
	t.Cleanup(func() { migrationFiles = original })
	all, err := fs.ReadDir(original, ".")
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, e := range all {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	before := fstest.MapFS{}
	for _, n := range names {
		if n >= "000028" {
			continue
		}
		data, err := fs.ReadFile(original, n)
		if err != nil {
			t.Fatal(err)
		}
		before[n] = &fstest.MapFile{Data: data}
	}
	if len(before) == len(names) {
		t.Fatal("migration 000028 not found in the embedded set")
	}
	migrationFiles = before
	if err := Apply(ctx, conn); err != nil {
		t.Fatalf("apply pre-000028: %v", err)
	}

	tx := beginTx(t, ctx, conn)
	f := buildReleaseFixture(t, ctx, tx)
	if _, err := tx.Exec(ctx, `INSERT INTO environment_runtime_configs
	      (environment_id, container_port, container_scheme, release_strategy, drain_seconds, cpu_millis, memory_bytes, restart_policy, health_policy_revision, health_gate_host)
	      VALUES ($1,8080,'http','blue_green',30,500,268435456,'unless-stopped',1,'app.internal')`, f.environmentID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO agent_job_attempt_execution
	      (attempt_id, container_port, container_scheme, release_strategy, drain_seconds, health_policy_revision, cpu_millis, memory_bytes, restart_policy, environment_runtime_config_revision, health_gate_path, health_gate_host)
	      VALUES ($1,8080,'http','blue_green',30,1,500,268435456,'unless-stopped',0,'/','app.internal')`, f.attemptID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	migrationFiles = original
	if err := Apply(ctx, conn); err != nil {
		t.Fatalf("apply 000028 over existing rows: %v", err)
	}
	q := `SELECT health_success_threshold, health_interval_seconds, health_timeout_seconds, health_deadline_seconds, health_tcp_port IS NULL FROM %s WHERE %s=$1`
	for _, tc := range []struct {
		table, key string
		id         any
	}{{"environment_runtime_configs", "environment_id", f.environmentID}, {"agent_job_attempt_execution", "attempt_id", f.attemptID}} {
		var s, i, to, d int
		var tcpNull bool
		if err := conn.QueryRow(ctx, fmt.Sprintf(q, tc.table, tc.key), tc.id).Scan(&s, &i, &to, &d, &tcpNull); err != nil {
			t.Fatalf("%s: %v", tc.table, err)
		}
		if s != 3 || i != 2 || to != 2 || d != 60 || !tcpNull {
			t.Fatalf("%s backfill = %d/%d/%d/%d null=%v, want 3/2/2/60/NULL", tc.table, s, i, to, d, tcpNull)
		}
	}
	var def *string
	if err := conn.QueryRow(ctx, `SELECT column_default FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='agent_job_attempt_execution' AND column_name='health_success_threshold'`).Scan(&def); err != nil || def != nil {
		t.Fatalf("frozen default after migration = %v err=%v, want none", def, err)
	}
}
