//go:build integration

package migrations

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// insertRuntimeConfigWithGate inserts a fresh environment_runtime_configs
// row with an explicit, valid health_gate_path/health_gate_host, following
// buildReleaseFixture's own suffix convention so concurrent test runs never
// collide.
func insertRuntimeConfigWithGate(t *testing.T, ctx context.Context, tx pgx.Tx, envID, path, host string) {
	t.Helper()
	if _, err := tx.Exec(ctx, `INSERT INTO environment_runtime_configs
	      (environment_id, container_port, container_scheme, release_strategy, drain_seconds, cpu_millis, memory_bytes, restart_policy, health_policy_revision, health_gate_path, health_gate_host)
	      VALUES ($1,8080,'http','blue_green',30,500,268435456,'unless-stopped',1,$2,$3)`, envID, path, host); err != nil {
		t.Fatalf("insert runtime config with gate path=%q host=%q: %v", path, host, err)
	}
}

// TestEnvironmentRuntimeConfigHealthGatePathDefaultAndCheck proves
// health_gate_path defaults to "/" when omitted, and that migration
// 000017's CHECK rejects every path shape the spec calls out: not starting
// with '/', "//..." (never exactly one leading slash), a space, a '#', a
// control character, and an oversized value; the boundary (2048 bytes) is
// accepted.
func TestEnvironmentRuntimeConfigHealthGatePathDefaultAndCheck(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)

	// Omitted entirely: defaults to "/".
	envDefault := "env-gate-path-default-" + randomSuffix(t)
	if _, err := tx.Exec(ctx, `INSERT INTO environment_runtime_configs
	      (environment_id, container_port, container_scheme, release_strategy, drain_seconds, cpu_millis, memory_bytes, restart_policy, health_policy_revision, health_gate_host)
	      VALUES ($1,8080,'http','blue_green',30,500,268435456,'unless-stopped',1,'app.internal')`, envDefault); err != nil {
		t.Fatalf("insert without health_gate_path: %v", err)
	}
	var path string
	if err := tx.QueryRow(ctx, `SELECT health_gate_path FROM environment_runtime_configs WHERE environment_id=$1`, envDefault).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if path != "/" {
		t.Fatalf("default health_gate_path = %q, want \"/\"", path)
	}

	envID := "env-gate-path-" + randomSuffix(t)
	insertRuntimeConfigWithGate(t, ctx, tx, envID, "/healthz", "app.internal")

	// Accepted: exactly 2048 bytes, starting with '/'.
	longOK := "/" + strings.Repeat("a", 2047)
	insertRuntimeConfigWithGate(t, ctx, tx, "env-gate-path-long-ok-"+randomSuffix(t), longOK, "app.internal")

	// Accepted: RFC 3986 path/query characters, including an
	// already-percent-encoded triplet and a query string -- the tightened,
	// allowlist-based grammar independent review required after finding
	// these could otherwise reach internal/runtime/docker/health.go's
	// probe and be silently re-encoded (see that migration's own comment).
	good := map[string]string{
		"percent-encoded slash": "/health%2Fcheck",
		"semicolon":             "/a;b",
		"query string":          "/p?x=1&y=%20",
		"colon and at-sign":     "/a:b@c",
	}
	for _, value := range good {
		insertRuntimeConfigWithGate(t, ctx, tx, "env-gate-path-good-"+randomSuffix(t), value, "app.internal")
	}

	bad := map[string]string{
		"missing leading slash":  "healthz",
		"double leading slash":   "//evil",
		"space":                  "/a b",
		"fragment":               "/a#b",
		"control character":      "/a\tb",
		"oversized":              "/" + strings.Repeat("a", 2048),
		"empty":                  "",
		"non-ASCII literal byte": "/hé",
		"malformed percent (zz)": "/a%zz",
		"truncated percent":      "/a%2",
		"double quote":           `/a"b`,
		"angle bracket":          "/a<b",
		"backslash":              `/a\b`,
		"caret":                  "/a^b",
		"pipe":                   "/a|b",
		"curly brace":            "/a{b",
		"backtick":               "/a`b",
	}
	for name, value := range bad {
		mustFail(t, ctx, tx, "expected rejection of health_gate_path: "+name,
			`INSERT INTO environment_runtime_configs
			 (environment_id, container_port, container_scheme, release_strategy, drain_seconds, cpu_millis, memory_bytes, restart_policy, health_policy_revision, health_gate_path, health_gate_host)
			 VALUES ($1,8080,'http','blue_green',30,500,268435456,'unless-stopped',1,$2,'app.internal')`,
			"env-gate-path-bad-"+randomSuffix(t), value)
	}
}

// TestEnvironmentRuntimeConfigHealthGateHostRequiredAndCheck proves
// health_gate_host is required (decision D2: NOT NULL, no default) and that
// migration 000017's CHECK rejects every host shape the spec calls out:
// userinfo, an embedded path, an IPv6 literal in brackets, port 0, port
// 65536, and a 254-byte hostname; a hostname with a valid port and the
// 253-byte boundary are both accepted.
func TestEnvironmentRuntimeConfigHealthGateHostRequiredAndCheck(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)

	// NULL/omitted is rejected outright -- there is no default.
	mustFail(t, ctx, tx, "expected rejection of missing health_gate_host",
		`INSERT INTO environment_runtime_configs
		 (environment_id, container_port, container_scheme, release_strategy, drain_seconds, cpu_millis, memory_bytes, restart_policy, health_policy_revision)
		 VALUES ($1,8080,'http','blue_green',30,500,268435456,'unless-stopped',1)`,
		"env-gate-host-missing-"+randomSuffix(t))

	insertRuntimeConfigWithGate(t, ctx, tx, "env-gate-host-ok-"+randomSuffix(t), "/", "app.internal")
	insertRuntimeConfigWithGate(t, ctx, tx, "env-gate-host-port-"+randomSuffix(t), "/", "app.internal:8443")

	// Exactly 253 bytes (the RFC 1035 boundary) is accepted.
	label := strings.Repeat("a", 63)
	host253 := label + "." + label + "." + label + "." + strings.Repeat("a", 253-3*64)
	if len(host253) != 253 {
		t.Fatalf("test bug: constructed host is %d bytes, want 253", len(host253))
	}
	insertRuntimeConfigWithGate(t, ctx, tx, "env-gate-host-253-"+randomSuffix(t), "/", host253)

	bad := map[string]string{
		"userinfo":       "user@host",
		"embedded path":  "host/path",
		"ipv6 brackets":  "[::1]",
		"port zero":      "app.internal:0",
		"port too large": "app.internal:65536",
		"254 bytes":      host253 + "a",
		"empty":          "",
	}
	for name, value := range bad {
		mustFail(t, ctx, tx, "expected rejection of health_gate_host: "+name,
			`INSERT INTO environment_runtime_configs
			 (environment_id, container_port, container_scheme, release_strategy, drain_seconds, cpu_millis, memory_bytes, restart_policy, health_policy_revision, health_gate_path, health_gate_host)
			 VALUES ($1,8080,'http','blue_green',30,500,268435456,'unless-stopped',1,'/',$2)`,
			"env-gate-host-bad-"+randomSuffix(t), value)
	}
}

// TestEnvironmentRuntimeConfigCASWidenedByHealthGate proves migration
// 000014's validate_environment_runtime_config_update trigger (identity
// immutable, revision must advance by exactly one, updated_at monotonic)
// still enforces correctly over the widened row: a CAS update that also
// changes health_gate_path/health_gate_host succeeds when it advances
// revision by one, and is still rejected when it does not.
func TestEnvironmentRuntimeConfigCASWidenedByHealthGate(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	envID := "env-gate-cas-" + randomSuffix(t)
	insertRuntimeConfigWithGate(t, ctx, tx, envID, "/", "app.internal")

	tag, err := tx.Exec(ctx, `UPDATE environment_runtime_configs SET health_gate_path='/healthz', health_gate_host='new.internal', revision=revision+1 WHERE environment_id=$1 AND revision=0`, envID)
	if err != nil {
		t.Fatalf("cas update changing gate path/host: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("expected 1 row affected, got %d", tag.RowsAffected())
	}
	var path, host string
	if err := tx.QueryRow(ctx, `SELECT health_gate_path, health_gate_host FROM environment_runtime_configs WHERE environment_id=$1`, envID).Scan(&path, &host); err != nil {
		t.Fatal(err)
	}
	if path != "/healthz" || host != "new.internal" {
		t.Fatalf("gate path/host after cas update = %q/%q", path, host)
	}

	// A matched row that changes the gate host without advancing revision
	// is still rejected by the trigger -- proving the trigger's revision
	// check runs (and still fires) regardless of which columns changed.
	mustFail(t, ctx, tx, "expected rejection of update that does not advance revision",
		`UPDATE environment_runtime_configs SET health_gate_host='another.internal' WHERE environment_id=$1 AND revision=1`, envID)

	// An update that tries to set an invalid new host is still rejected by
	// the CHECK, even though the CAS predicate and revision bump are
	// otherwise correct.
	mustFail(t, ctx, tx, "expected rejection of invalid health_gate_host on update",
		`UPDATE environment_runtime_configs SET health_gate_host='bad host', revision=revision+1 WHERE environment_id=$1 AND revision=1`, envID)
}

// TestAgentJobAttemptExecutionHealthGateFrozenAndChecked proves
// agent_job_attempt_execution's widened health_gate_path/health_gate_host
// columns are required (no default at all, unlike the admin-facing
// environment_runtime_configs.health_gate_path), CHECK-validated with the
// identical rules, and insert-once like every other column on this table.
func TestAgentJobAttemptExecutionHealthGateFrozenAndChecked(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f := buildReleaseFixture(t, ctx, tx)

	// Missing health_gate_path/health_gate_host (no default on this table)
	// is rejected outright.
	mustFail(t, ctx, tx, "expected rejection of missing health_gate_path/host",
		`INSERT INTO agent_job_attempt_execution
		 (attempt_id, container_port, container_scheme, release_strategy, drain_seconds, health_policy_revision, cpu_millis, memory_bytes, restart_policy, environment_runtime_config_revision)
		 VALUES ($1,8080,'http','blue_green',30,1,500,268435456,'unless-stopped',0)`, f.attemptID)

	mustFail(t, ctx, tx, "expected rejection of invalid frozen health_gate_path",
		`INSERT INTO agent_job_attempt_execution
		 (attempt_id, container_port, container_scheme, release_strategy, drain_seconds, health_policy_revision, cpu_millis, memory_bytes, restart_policy, environment_runtime_config_revision, health_gate_path, health_gate_host)
		 VALUES ($1,8080,'http','blue_green',30,1,500,268435456,'unless-stopped',0,'//evil','app.internal')`, f.attemptID)

	mustFail(t, ctx, tx, "expected rejection of invalid frozen health_gate_host",
		`INSERT INTO agent_job_attempt_execution
		 (attempt_id, container_port, container_scheme, release_strategy, drain_seconds, health_policy_revision, cpu_millis, memory_bytes, restart_policy, environment_runtime_config_revision, health_gate_path, health_gate_host)
		 VALUES ($1,8080,'http','blue_green',30,1,500,268435456,'unless-stopped',0,'/','user@host')`, f.attemptID)

	if _, err := tx.Exec(ctx, `INSERT INTO agent_job_attempt_execution
	      (attempt_id, container_port, container_scheme, release_strategy, drain_seconds, health_policy_revision, cpu_millis, memory_bytes, restart_policy, environment_runtime_config_revision, health_gate_path, health_gate_host)
	      VALUES ($1,8080,'http','blue_green',30,1,500,268435456,'unless-stopped',0,'/healthz','app.internal:8443')`, f.attemptID); err != nil {
		t.Fatalf("insert valid frozen health gate row: %v", err)
	}

	mustFail(t, ctx, tx, "expected update rejection of frozen health_gate_path (insert-once)",
		`UPDATE agent_job_attempt_execution SET health_gate_path='/other' WHERE attempt_id=$1`, f.attemptID)
	mustFail(t, ctx, tx, "expected update rejection of frozen health_gate_host (insert-once)",
		`UPDATE agent_job_attempt_execution SET health_gate_host='other.internal' WHERE attempt_id=$1`, f.attemptID)

	var path, host string
	if err := tx.QueryRow(ctx, `SELECT health_gate_path, health_gate_host FROM agent_job_attempt_execution WHERE attempt_id=$1`, f.attemptID).Scan(&path, &host); err != nil {
		t.Fatal(err)
	}
	if path != "/healthz" || host != "app.internal:8443" {
		t.Fatalf("frozen gate path/host = %q/%q", path, host)
	}
}
