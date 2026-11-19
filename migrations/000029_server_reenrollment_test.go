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

// failsWith runs sql in a savepoint and requires it to fail with an error
// whose text contains want. Naming the exact index, constraint or trigger
// message keeps each rule tested in isolation: a statement rejected by an
// older, unrelated clause would carry a different message and fail here.
func failsWith(t *testing.T, ctx context.Context, tx pgx.Tx, want, sql string, args ...any) {
	t.Helper()
	savepointCounter++
	sp := fmt.Sprintf("sp_%d", savepointCounter)
	if _, err := tx.Exec(ctx, "SAVEPOINT "+sp); err != nil {
		t.Fatalf("create savepoint: %v", err)
	}
	_, err := tx.Exec(ctx, sql, args...)
	if err == nil {
		t.Fatalf("expected failure containing %q, statement succeeded", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("expected failure containing %q, got: %v", want, err)
	}
	if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT "+sp); err != nil {
		t.Fatalf("rollback to savepoint: %v", err)
	}
}

// TestServerReenrollmentMigration applies 000001..000028, creates servers with
// consumed, pending and absent enrollment tokens, applies 000029 over them,
// and proves existing rows survive as purpose='enroll' and that every new rule
// holds on its own.
func TestServerReenrollmentMigration(t *testing.T) {
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
	scratch := "autodeploy_reenroll_" + randomSuffix(t)
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
		if n >= "000029" {
			continue
		}
		data, err := fs.ReadFile(original, n)
		if err != nil {
			t.Fatal(err)
		}
		before[n] = &fstest.MapFile{Data: data}
	}
	if len(before) == len(names) {
		t.Fatal("migration 000029 not found in the embedded set")
	}
	migrationFiles = before
	if err := Apply(ctx, conn); err != nil {
		t.Fatalf("apply pre-000029: %v", err)
	}

	seed := func(sql string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	seed(`INSERT INTO owners(id) VALUES ('o1')`)
	for _, s := range []struct{ id, status string }{{"enrolled", "active"}, {"pending", "active"}, {"revoked", "active"}, {"never", "active"}} {
		seed(`INSERT INTO servers(id,owner_id,name,protocol_version,status) VALUES ($1,'o1',$1,1,'active')`, s.id)
	}
	digest := func(b byte) []byte { return []byte(strings.Repeat(string([]byte{b}), 32)) }
	seed(`INSERT INTO server_enrollment_tokens(server_id,digest,issued_at,expires_at,consumed_at) VALUES ('enrolled',$1,now()-interval '1 hour',now()+interval '1 hour',now()-interval '30 minutes')`, digest(1))
	seed(`INSERT INTO server_enrollment_tokens(server_id,digest,issued_at,expires_at) VALUES ('pending',$1,now(),now()+interval '1 hour')`, digest(2))
	seed(`INSERT INTO server_enrollment_tokens(server_id,digest,issued_at,expires_at,consumed_at) VALUES ('revoked',$1,now()-interval '1 hour',now()+interval '1 hour',now()-interval '30 minutes')`, digest(3))
	seed(`UPDATE servers SET status='revoked',revoked_at=now() WHERE id='revoked'`)
	var consumedBefore string
	if err := conn.QueryRow(ctx, `SELECT consumed_at::text FROM server_enrollment_tokens WHERE server_id='enrolled'`).Scan(&consumedBefore); err != nil {
		t.Fatal(err)
	}

	migrationFiles = original
	if err := Apply(ctx, conn); err != nil {
		t.Fatalf("apply 000029 over existing rows: %v", err)
	}

	// Existing rows survive untouched and are first-enrollment tokens.
	var rows, enrollRows, distinctIDs int
	if err := conn.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE purpose='enroll'), count(DISTINCT id) FROM server_enrollment_tokens`).Scan(&rows, &enrollRows, &distinctIDs); err != nil {
		t.Fatal(err)
	}
	if rows != 3 || enrollRows != 3 || distinctIDs != 3 {
		t.Fatalf("rows=%d enroll=%d distinct ids=%d, want 3/3/3", rows, enrollRows, distinctIDs)
	}
	var consumedAfter string
	if err := conn.QueryRow(ctx, `SELECT consumed_at::text FROM server_enrollment_tokens WHERE server_id='enrolled'`).Scan(&consumedAfter); err != nil || consumedAfter != consumedBefore {
		t.Fatalf("consumed_at %q -> %q err=%v", consumedBefore, consumedAfter, err)
	}
	var def *string
	if err := conn.QueryRow(ctx, `SELECT column_default FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='server_enrollment_tokens' AND column_name='purpose'`).Scan(&def); err != nil || def != nil {
		t.Fatalf("purpose default after migration = %v err=%v, want none", def, err)
	}

	tx := beginTx(t, ctx, conn)
	const ins = `INSERT INTO server_enrollment_tokens(server_id,digest,issued_at,expires_at,purpose) VALUES ($1,$2,now(),now()+interval '1 hour',$3)`

	// Rule: at most one 'enroll' token per server ever (the retained rule
	// that an enrolled server never gets a fresh first-enrollment token).
	// 'enrolled' has no unconsumed row, so only this index can reject it.
	failsWith(t, ctx, tx, "server_enrollment_tokens_one_enroll_uq", ins, "enrolled", digest(9), "enroll")
	// Rule: purpose is a closed set.
	failsWith(t, ctx, tx, "server_enrollment_tokens_purpose_check", ins, "enrolled", digest(9), "bogus")
	// Rule: a re-enrollment token needs an enrolled server; 'never' has no
	// token at all, so no index could reject this -- only the insert trigger.
	failsWith(t, ctx, tx, "requires an already enrolled server", ins, "never", digest(9), "reenroll")
	// Rule: ... and 'pending' has an unconsumed enroll token (never enrolled).
	failsWith(t, ctx, tx, "requires an already enrolled server", ins, "pending", digest(9), "reenroll")
	// Rule: a re-enrollment token needs an active server ('revoked' is
	// enrolled and has no unconsumed row, so only the trigger rejects it).
	failsWith(t, ctx, tx, "requires an active server", ins, "revoked", digest(9), "reenroll")
	// Rule: born unconsumed.
	failsWith(t, ctx, tx, "must be inserted unconsumed", `INSERT INTO server_enrollment_tokens(server_id,digest,issued_at,expires_at,consumed_at,purpose) VALUES ('enrolled',$1,now()-interval '1 minute',now()+interval '1 hour',now(),'reenroll')`, digest(9))

	// Legal: a re-enrollment token for the active, enrolled server.
	if _, err := tx.Exec(ctx, ins, "enrolled", digest(4), "reenroll"); err != nil {
		t.Fatalf("legal reenroll insert: %v", err)
	}
	// Rule: at most one unconsumed token per server (trigger passes: the
	// server is active and enrolled, so only the partial unique index fires).
	failsWith(t, ctx, tx, "server_enrollment_tokens_one_unconsumed_uq", ins, "enrolled", digest(5), "reenroll")

	var reID int64
	if err := tx.QueryRow(ctx, `SELECT id FROM server_enrollment_tokens WHERE server_id='enrolled' AND purpose='reenroll'`).Scan(&reID); err != nil {
		t.Fatal(err)
	}
	// Immutability of the unconsumed row's identity.
	failsWith(t, ctx, tx, "purpose is immutable", `UPDATE server_enrollment_tokens SET purpose='enroll' WHERE id=$1`, reID)
	failsWith(t, ctx, tx, "server is immutable", `UPDATE server_enrollment_tokens SET server_id='never' WHERE id=$1`, reID)
	failsWith(t, ctx, tx, "consumption must not alter issuance", `UPDATE server_enrollment_tokens SET consumed_at=now(),digest=$2 WHERE id=$1`, reID, digest(8))
	// An unconsumed token may still be reissued (digest/lifetime overwrite).
	if _, err := tx.Exec(ctx, `UPDATE server_enrollment_tokens SET digest=$2 WHERE id=$1`, reID, digest(6)); err != nil {
		t.Fatalf("reissue of unconsumed token: %v", err)
	}
	failsWith(t, ctx, tx, "enrollment tokens are retained", `DELETE FROM server_enrollment_tokens WHERE id=$1`, reID)

	// Consume it; the consumed row is frozen and the old consumed enroll
	// row stays frozen too.
	if _, err := tx.Exec(ctx, `UPDATE server_enrollment_tokens SET consumed_at=now() WHERE id=$1`, reID); err != nil {
		t.Fatalf("consume reenroll token: %v", err)
	}
	failsWith(t, ctx, tx, "consumed enrollment token cannot change", `UPDATE server_enrollment_tokens SET digest=$2 WHERE id=$1`, reID, digest(7))
	failsWith(t, ctx, tx, "consumed enrollment token cannot change", `UPDATE server_enrollment_tokens SET expires_at=expires_at+interval '1 day' WHERE server_id='enrolled' AND purpose='enroll'`)
	failsWith(t, ctx, tx, "enrollment tokens are retained", `DELETE FROM server_enrollment_tokens WHERE server_id='enrolled'`)
	// A later restore may need another re-enrollment: allowed once the
	// previous token is consumed.
	if _, err := tx.Exec(ctx, ins, "enrolled", digest(10), "reenroll"); err != nil {
		t.Fatalf("second reenroll after consumption: %v", err)
	}

	// Audit action list.
	const aud = `INSERT INTO server_audit_events(actor_type,actor_id,action,server_id,result,reason) VALUES ('administrator','u',$1,'enrolled','success','')`
	acceptedInSavepoint(t, ctx, tx, "reenrollment_issued", aud, "reenrollment_issued")
	for _, a := range []string{"register", "enrollment_issued", "enrolled", "enrollment_rejected", "credential_rotated", "credential_revoked", "server_revoked", "heartbeat_rejected", "environment_mark_reconciled"} {
		acceptedInSavepoint(t, ctx, tx, a, aud, a)
	}
	failsWith(t, ctx, tx, "server_audit_events_action_check", aud, "reenrollment_bogus")
}
