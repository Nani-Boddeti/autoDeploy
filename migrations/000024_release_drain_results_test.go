//go:build integration

package migrations

import (
	"context"
	"strings"
	"testing"
)

const insertDrainResult = `INSERT INTO release_drain_results(attempt_id, environment_id, release_id, predecessor_release_id, predecessor_runtime_instance_id, outcome, outcome_summary)
	VALUES ($1,$2,$3,$4,$5,$6,$7)`

func TestReleaseDrainResultsConstraints(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f := buildReleaseFixture(t, ctx, tx)
	rel := insertRelease(t, ctx, tx, f, "candidate")
	pred := insertRelease(t, ctx, tx, f, "candidate")

	for name, bad := range map[string]string{
		"unknown outcome": "restarted", "empty outcome": "", "uppercase outcome": "STOPPED",
	} {
		mustFail(t, ctx, tx, name+" accepted", insertDrainResult, f.attemptID, f.environmentID, rel, pred, "41", bad, "s")
	}
	for name, bad := range map[string]string{
		"empty": "", "leading zero": "041", "zero": "0", "non-digit": "4x", "negative": "-1", "20 digits": strings.Repeat("9", 20), "newline": "41\n",
	} {
		mustFail(t, ctx, tx, "runtime instance "+name+" accepted", insertDrainResult, f.attemptID, f.environmentID, rel, pred, bad, "stopped", "s")
	}
	mustFail(t, ctx, tx, "2049-byte summary accepted", insertDrainResult, f.attemptID, f.environmentID, rel, pred, "41", "stopped", strings.Repeat("a", 2049))
	mustFail(t, ctx, tx, "newline in summary accepted", insertDrainResult, f.attemptID, f.environmentID, rel, pred, "41", "stopped", "a\nb")
	mustFail(t, ctx, tx, "NUL-free control char accepted", insertDrainResult, f.attemptID, f.environmentID, rel, pred, "41", "stopped", "a\x1fb")
	mustFail(t, ctx, tx, "DEL accepted", insertDrainResult, f.attemptID, f.environmentID, rel, pred, "41", "stopped", "a\x7fb")
	mustFail(t, ctx, tx, "release equal to predecessor accepted", insertDrainResult, f.attemptID, f.environmentID, rel, rel, "41", "stopped", "s")
	mustFail(t, ctx, tx, "unknown release accepted", insertDrainResult, f.attemptID, f.environmentID, "no-such-release", pred, "41", "stopped", "s")
	mustFail(t, ctx, tx, "unknown predecessor accepted", insertDrainResult, f.attemptID, f.environmentID, rel, "no-such-release", "41", "stopped", "s")
	mustFail(t, ctx, tx, "unknown attempt accepted", insertDrainResult, int64(9_000_000_000_000), f.environmentID, rel, pred, "41", "stopped", "s")
	mustFail(t, ctx, tx, "unknown environment accepted", insertDrainResult, f.attemptID, "no-such-env", rel, pred, "41", "stopped", "s")

	// Boundary: exactly 2048 bytes and a 19-digit instance id are valid.
	if _, err := tx.Exec(ctx, insertDrainResult, f.attemptID, f.environmentID, rel, pred, "9223372036854775807", "stop_failed", strings.Repeat("a", 2048)); err != nil {
		t.Fatalf("valid drain result rejected: %v", err)
	}
	// One row per attempt.
	mustFail(t, ctx, tx, "second row for the attempt accepted", insertDrainResult, f.attemptID, f.environmentID, rel, pred, "41", "stopped", "s")
}

func TestReleaseDrainResultsAcceptEveryOutcome(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f := buildReleaseFixture(t, ctx, tx)
	rel := insertRelease(t, ctx, tx, f, "candidate")
	pred := insertRelease(t, ctx, tx, f, "candidate")
	// attempt_id is UNIQUE, so each outcome needs its own attempt; the fixture
	// has one, so check the CHECK via savepoints on the same row instead.
	for _, outcome := range []string{"stopped", "already_stopped", "gone", "stop_failed", "route_moved", "identity_error"} {
		mustExec(t, ctx, tx, `SAVEPOINT o`)
		if _, err := tx.Exec(ctx, insertDrainResult, f.attemptID, f.environmentID, rel, pred, "41", outcome, "s"); err != nil {
			t.Fatalf("outcome %q rejected: %v", outcome, err)
		}
		mustExec(t, ctx, tx, `ROLLBACK TO SAVEPOINT o`)
	}
}

func TestReleaseDrainResultsImmutable(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f := buildReleaseFixture(t, ctx, tx)
	rel := insertRelease(t, ctx, tx, f, "candidate")
	pred := insertRelease(t, ctx, tx, f, "candidate")
	if _, err := tx.Exec(ctx, insertDrainResult, f.attemptID, f.environmentID, rel, pred, "41", "stopped", "s"); err != nil {
		t.Fatal(err)
	}
	mustRaiseTrigger(t, ctx, tx, "release drain results are immutable", `UPDATE release_drain_results SET outcome='gone' WHERE attempt_id=$1`, f.attemptID)
	mustRaiseTrigger(t, ctx, tx, "release drain results are immutable", `UPDATE release_drain_results SET outcome_summary='x' WHERE attempt_id=$1`, f.attemptID)
	mustRaiseTrigger(t, ctx, tx, "release drain results are immutable", `DELETE FROM release_drain_results WHERE attempt_id=$1`, f.attemptID)
}
