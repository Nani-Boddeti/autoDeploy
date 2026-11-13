//go:build integration

package migrations

import (
	"context"
	"strings"
	"testing"
)

const insertCleanup = `INSERT INTO release_cleanups(release_id, server_id, environment_id) VALUES ($1,$2,$3)`

func TestReleaseStateRetainedToRemovingEdge(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f := buildReleaseFixture(t, ctx, tx)

	retained := insertRelease(t, ctx, tx, f, "retained")
	mustExec(t, ctx, tx, `UPDATE releases SET state='removing' WHERE id=$1`, retained)
	mustExec(t, ctx, tx, `UPDATE releases SET state='removed' WHERE id=$1`, retained)
	mustRaiseTrigger(t, ctx, tx, "release is terminal in state removed", `UPDATE releases SET state='retained' WHERE id=$1`, retained)

	// removing never goes back, and active/failed/removed never reach removing
	// through the new edge.
	removing := insertRelease(t, ctx, tx, f, "removing")
	setImageDigest(t, ctx, tx, removing)
	for _, to := range []string{"retained", "active", "ready", "candidate", "failed"} {
		mustRaiseTrigger(t, ctx, tx, "invalid release transition removing -> "+to, `UPDATE releases SET state=$2 WHERE id=$1`, removing, to)
	}
	active := insertRelease(t, ctx, tx, f, "active")
	mustRaiseTrigger(t, ctx, tx, "invalid release transition active -> removing", `UPDATE releases SET state='removing' WHERE id=$1`, active)
	failed := insertRelease(t, ctx, tx, f, "failed")
	mustRaiseTrigger(t, ctx, tx, "release is terminal in state failed", `UPDATE releases SET state='removing' WHERE id=$1`, failed)

	// retained -> active (rollback) and active -> retained are unchanged, and
	// retained still cannot jump to ready/candidate/failed.
	r2 := insertRelease(t, ctx, tx, f, "retained")
	setImageDigest(t, ctx, tx, r2)
	for _, to := range []string{"ready", "candidate", "failed", "removed"} {
		mustRaiseTrigger(t, ctx, tx, "invalid release transition retained -> "+to, `UPDATE releases SET state=$2 WHERE id=$1`, r2, to)
	}
	mustExec(t, ctx, tx, `UPDATE releases SET state='retained' WHERE id=$1`, active) // one active per environment
	mustExec(t, ctx, tx, `UPDATE releases SET state='active' WHERE id=$1`, r2)
	mustExec(t, ctx, tx, `UPDATE releases SET state='retained' WHERE id=$1`, r2)
}

func TestReleaseCleanupsConstraints(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f := buildReleaseFixture(t, ctx, tx)

	// Insert requires a removing release in the same environment.
	retained := insertRelease(t, ctx, tx, f, "retained")
	mustRaiseTrigger(t, ctx, tx, "release cleanup requires a removing release in the same environment", insertCleanup, retained, f.serverID, f.environmentID)
	removing := insertRelease(t, ctx, tx, f, "removing")
	mustRaiseTrigger(t, ctx, tx, "release cleanup requires a removing release in the same environment", insertCleanup, removing, f.serverID, "no-such-env")
	mustFail(t, ctx, tx, "unknown release accepted", insertCleanup, "no-such-release", f.serverID, f.environmentID)
	mustFail(t, ctx, tx, "unknown server accepted", insertCleanup, removing, "no-such-server", f.environmentID)
	mustRaiseTrigger(t, ctx, tx, "release cleanup must be inserted open", `INSERT INTO release_cleanups(release_id, server_id, environment_id, result, completed_at) VALUES ($1,$2,$3,'removed',clock_timestamp())`, removing, f.serverID, f.environmentID)

	mustExec(t, ctx, tx, insertCleanup, removing, f.serverID, f.environmentID)
	// At most one open cleanup per release.
	mustFail(t, ctx, tx, "second open cleanup accepted", insertCleanup, removing, f.serverID, f.environmentID)

	// One-time result write.
	complete := `UPDATE release_cleanups SET result=$2, error_class=$3, image_kept=$4, outcome_summary=$5, completed_at=clock_timestamp() WHERE release_id=$1 AND completed_at IS NULL`
	mustFail(t, ctx, tx, "unknown result accepted", complete, removing, "gone", nil, false, "")
	mustFail(t, ctx, tx, "failed without error class accepted", complete, removing, "failed", nil, false, "")
	mustFail(t, ctx, tx, "skipped without error class accepted", complete, removing, "skipped", nil, false, "")
	mustFail(t, ctx, tx, "removed with error class accepted", complete, removing, "removed", "engine", false, "")
	mustFail(t, ctx, tx, "unknown error class accepted", complete, removing, "failed", "boom", false, "")
	mustFail(t, ctx, tx, "failed with image_kept accepted", complete, removing, "failed", "engine", true, "")
	mustFail(t, ctx, tx, "2049-byte summary accepted", complete, removing, "failed", "engine", false, strings.Repeat("a", 2049))
	mustFail(t, ctx, tx, "control char summary accepted", complete, removing, "failed", "engine", false, "a\nb")
	mustFail(t, ctx, tx, "result without completed_at accepted", `UPDATE release_cleanups SET result='removed' WHERE release_id=$1`, removing)
	mustFail(t, ctx, tx, "completed_at without result accepted", `UPDATE release_cleanups SET completed_at=clock_timestamp() WHERE release_id=$1`, removing)
	mustFail(t, ctx, tx, "completed before claimed accepted", `UPDATE release_cleanups SET result='removed', completed_at=claimed_at - interval '1 second' WHERE release_id=$1`, removing)
	mustFail(t, ctx, tx, "image_kept before a result accepted", `UPDATE release_cleanups SET image_kept=true WHERE release_id=$1`, removing)
	mustRaiseTrigger(t, ctx, tx, "release cleanup update must record its result", `UPDATE release_cleanups SET outcome_summary='x' WHERE release_id=$1`, removing)
	mustRaiseTrigger(t, ctx, tx, "release cleanup identity is immutable", `UPDATE release_cleanups SET server_id='x' WHERE release_id=$1`, removing)

	mustExec(t, ctx, tx, complete, removing, "failed", "engine", false, strings.Repeat("a", 2048))
	// Immutable once written; never deleted.
	mustRaiseTrigger(t, ctx, tx, "release cleanup result is write-once", `UPDATE release_cleanups SET result='removed', error_class=NULL WHERE release_id=$1`, removing)
	mustRaiseTrigger(t, ctx, tx, "release cleanup result is write-once", `UPDATE release_cleanups SET outcome_summary='x' WHERE release_id=$1`, removing)
	mustRaiseTrigger(t, ctx, tx, "release cleanups are never deleted", `DELETE FROM release_cleanups WHERE release_id=$1`, removing)

	// A completed failure allows a new open cleanup (retry), but the release is
	// removed at most once.
	mustExec(t, ctx, tx, insertCleanup, removing, f.serverID, f.environmentID)
	mustExec(t, ctx, tx, complete, removing, "removed", nil, true, "")
	// A release is removed at most once: a further open row is allowed by the
	// open-row index but cannot also end "removed".
	mustExec(t, ctx, tx, insertCleanup, removing, f.serverID, f.environmentID)
	mustFail(t, ctx, tx, "second removed result accepted", complete, removing, "removed", nil, false, "")
}
