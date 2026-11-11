//go:build integration

package migrations

import (
	"context"
	"strings"
	"testing"
)

// Attempt ids sit far above any id the store tests commit to a shared database.
const logAttemptBase = 9_000_000_000

const insertChunk = `INSERT INTO deployment_log_chunks(attempt_id, stream, writer_epoch, seq, body) VALUES ($1,$2,$3,$4,$5)`

// TestDeploymentLogChunksConstraints proves each CHECK/UNIQUE of 000022 by
// SQLSTATE-agnostic rejection of an otherwise valid row that differs in
// exactly one column, after proving the valid row itself is accepted.
func TestDeploymentLogChunksConstraints(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)

	if _, err := tx.Exec(ctx, insertChunk, logAttemptBase+7, "build", 1, 1, "hello"); err != nil {
		t.Fatalf("valid chunk rejected: %v", err)
	}
	if _, err := tx.Exec(ctx, insertChunk, logAttemptBase+7, "system", 0, 1, strings.Repeat("é", 2048)); err != nil {
		t.Fatalf("4096-byte multibyte body / epoch 0 rejected: %v", err)
	}
	for _, stream := range []string{"phase", "health", "activation", "drain"} {
		if _, err := tx.Exec(ctx, insertChunk, logAttemptBase+7, stream, 1, 1, "x"); err != nil {
			t.Fatalf("stream %q rejected: %v", stream, err)
		}
	}
	mustFail(t, ctx, tx, "unknown stream accepted", insertChunk, logAttemptBase+7, "stderr", 1, 1, "x")
	mustFail(t, ctx, tx, "empty body accepted", insertChunk, logAttemptBase+7, "build", 1, 2, "")
	mustFail(t, ctx, tx, "4097-byte body accepted", insertChunk, logAttemptBase+7, "build", 1, 2, strings.Repeat("a", 4097))
	mustFail(t, ctx, tx, "seq 0 accepted", insertChunk, logAttemptBase+7, "build", 1, 0, "x")
	mustFail(t, ctx, tx, "negative epoch accepted", insertChunk, logAttemptBase+7, "build", -1, 2, "x")
	mustFail(t, ctx, tx, "attempt 0 accepted", insertChunk, 0, "build", 1, 2, "x")
	mustFail(t, ctx, tx, "NUL byte accepted", insertChunk, logAttemptBase+7, "build", 1, 2, "a\x00b")
	mustFail(t, ctx, tx, "duplicate idempotency key accepted", insertChunk, logAttemptBase+7, "build", 1, 1, "other")
	// Same seq under another epoch, stream or attempt is a different key.
	for _, c := range []struct {
		attempt int
		stream  string
		epoch   int
	}{{7, "build", 2}, {8, "build", 1}} {
		if _, err := tx.Exec(ctx, insertChunk, logAttemptBase+int64(c.attempt), c.stream, c.epoch, 1, "x"); err != nil {
			t.Fatalf("distinct key %+v rejected: %v", c, err)
		}
	}
	var fks int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE contype='f' AND conrelid IN ('deployment_log_chunks'::regclass,'deployment_log_attempt_totals'::regclass)`).Scan(&fks); err != nil || fks != 0 {
		t.Fatalf("log tables must carry no foreign key to attempts (lock-free appends): fks=%d err=%v", fks, err)
	}
}

// TestDeploymentLogChunksAppendOnly proves UPDATE is always rejected and
// DELETE only succeeds under the transaction-local purge GUC, which must not
// leak past its transaction. Both rejections are asserted by exact trigger
// message so a CHECK or missing-row case cannot pass vacuously.
func TestDeploymentLogChunksAppendOnly(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	if _, err := tx.Exec(ctx, insertChunk, logAttemptBase+9, "build", 1, 1, "line"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO deployment_log_attempt_totals(attempt_id, bytes, chunks) VALUES ($1, 4, 1)`, logAttemptBase+9); err != nil {
		t.Fatal(err)
	}
	mustRaiseTrigger(t, ctx, tx, "deployment log chunks are append-only", `UPDATE deployment_log_chunks SET body='changed' WHERE attempt_id=$1`, logAttemptBase+9)
	mustRaiseTrigger(t, ctx, tx, "deployment log rows may only be deleted by the retention purge", `DELETE FROM deployment_log_chunks WHERE attempt_id=$1`, logAttemptBase+9)
	mustRaiseTrigger(t, ctx, tx, "deployment log rows may only be deleted by the retention purge", `DELETE FROM deployment_log_attempt_totals WHERE attempt_id=$1`, logAttemptBase+9)
	// The totals row stays updatable (it is the counter).
	if _, err := tx.Exec(ctx, `UPDATE deployment_log_attempt_totals SET bytes=5 WHERE attempt_id=$1`, logAttemptBase+9); err != nil {
		t.Fatalf("totals update rejected: %v", err)
	}
	// A wrong GUC value does not open the gate.
	if _, err := tx.Exec(ctx, `SELECT set_config('autodeploy.log_purge','off',true)`); err != nil {
		t.Fatal(err)
	}
	mustRaiseTrigger(t, ctx, tx, "deployment log rows may only be deleted by the retention purge", `DELETE FROM deployment_log_chunks WHERE attempt_id=$1`, logAttemptBase+9)
	if _, err := tx.Exec(ctx, `SELECT set_config('autodeploy.log_purge','on',true)`); err != nil {
		t.Fatal(err)
	}
	if tag, err := tx.Exec(ctx, `DELETE FROM deployment_log_chunks WHERE attempt_id=$1`, logAttemptBase+9); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("purge-GUC delete: %v rows=%d", err, tag.RowsAffected())
	}
	if tag, err := tx.Exec(ctx, `DELETE FROM deployment_log_attempt_totals WHERE attempt_id=$1`, logAttemptBase+9); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("purge-GUC totals delete: %v rows=%d", err, tag.RowsAffected())
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	// Transaction-local: a fresh transaction on the same connection is gated.
	tx2 := beginTx(t, ctx, conn)
	if _, err := tx2.Exec(ctx, `SELECT set_config('autodeploy.log_purge','on',true)`); err != nil {
		t.Fatal(err)
	}
	if err := tx2.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	tx3 := beginTx(t, ctx, conn)
	if _, err := tx3.Exec(ctx, insertChunk, logAttemptBase+10, "build", 1, 1, "x"); err != nil {
		t.Fatal(err)
	}
	mustRaiseTrigger(t, ctx, tx3, "deployment log rows may only be deleted by the retention purge", `DELETE FROM deployment_log_chunks WHERE attempt_id=$1`, logAttemptBase+10)
}
