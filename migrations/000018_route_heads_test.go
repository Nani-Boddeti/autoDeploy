//go:build integration

package migrations

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestReleaseStateWidenedTransitions proves migration 000018's widened
// releases.state CHECK and replaced validate_release_update trigger accept
// exactly the three new edges ADR 0005 names (ready->active,
// active->retained, retained->active) and reject every illegal edge this
// slice's must-prove list names, alongside the pre-existing edges 000014
// already proved (which TestReleaseStateTransitions in
// 000014_release_execution_test.go continues to cover unchanged).
func TestReleaseStateWidenedTransitions(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f := buildReleaseFixture(t, ctx, tx)

	// Legal: candidate -> ready -> active -> retained -> active.
	releaseID := insertRelease(t, ctx, tx, f, "candidate")
	setImageDigest(t, ctx, tx, releaseID)
	if _, err := tx.Exec(ctx, `UPDATE releases SET state='ready' WHERE id=$1`, releaseID); err != nil {
		t.Fatalf("candidate->ready: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE releases SET state='active' WHERE id=$1`, releaseID); err != nil {
		t.Fatalf("ready->active: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE releases SET state='retained' WHERE id=$1`, releaseID); err != nil {
		t.Fatalf("active->retained: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE releases SET state='active' WHERE id=$1`, releaseID); err != nil {
		t.Fatalf("retained->active: %v", err)
	}

	// Illegal: active -> candidate.
	mustFail(t, ctx, tx, "expected rejection of active->candidate",
		`UPDATE releases SET state='candidate' WHERE id=$1`, releaseID)

	// Illegal: retained -> ready. Move to retained first.
	if _, err := tx.Exec(ctx, `UPDATE releases SET state='retained' WHERE id=$1`, releaseID); err != nil {
		t.Fatalf("active->retained (2): %v", err)
	}
	mustFail(t, ctx, tx, "expected rejection of retained->ready",
		`UPDATE releases SET state='ready' WHERE id=$1`, releaseID)

	// Illegal: active -> removing (ADR 0005: "active cannot transition
	// directly to removing").
	if _, err := tx.Exec(ctx, `UPDATE releases SET state='active' WHERE id=$1`, releaseID); err != nil {
		t.Fatalf("retained->active (2): %v", err)
	}
	mustFail(t, ctx, tx, "expected rejection of active->removing",
		`UPDATE releases SET state='removing' WHERE id=$1`, releaseID)

	// Illegal: retained -> removing (deferred to slice 11 per this
	// migration's own decision comment; not a legal edge in this slice).
	if _, err := tx.Exec(ctx, `UPDATE releases SET state='retained' WHERE id=$1`, releaseID); err != nil {
		t.Fatalf("active->retained (3): %v", err)
	}
	mustFail(t, ctx, tx, "expected rejection of retained->removing",
		`UPDATE releases SET state='removing' WHERE id=$1`, releaseID)

	// Illegal: removed -> active. Build a fresh release and walk it to removed.
	releaseID2 := insertRelease(t, ctx, tx, f, "candidate")
	if _, err := tx.Exec(ctx, `UPDATE releases SET state='removing' WHERE id=$1`, releaseID2); err != nil {
		t.Fatalf("candidate->removing: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE releases SET state='removed' WHERE id=$1`, releaseID2); err != nil {
		t.Fatalf("removing->removed: %v", err)
	}
	mustFail(t, ctx, tx, "expected rejection of removed->active",
		`UPDATE releases SET state='active' WHERE id=$1`, releaseID2)

	// Illegal: failed -> active.
	releaseID3 := insertRelease(t, ctx, tx, f, "candidate")
	if _, err := tx.Exec(ctx, `UPDATE releases SET state='failed' WHERE id=$1`, releaseID3); err != nil {
		t.Fatalf("candidate->failed: %v", err)
	}
	mustFail(t, ctx, tx, "expected rejection of failed->active",
		`UPDATE releases SET state='active' WHERE id=$1`, releaseID3)

	// Illegal: candidate -> active / candidate -> retained directly (no
	// predecessor edge permits either).
	releaseID4 := insertRelease(t, ctx, tx, f, "candidate")
	mustFail(t, ctx, tx, "expected rejection of candidate->active",
		`UPDATE releases SET state='active' WHERE id=$1`, releaseID4)
	mustFail(t, ctx, tx, "expected rejection of candidate->retained",
		`UPDATE releases SET state='retained' WHERE id=$1`, releaseID4)
}

// TestReleaseOnlyOneActivePerEnvironment proves the partial unique index
// releases_environment_active_idx: a second release cannot become 'active'
// in the same environment while one is already active.
func TestReleaseOnlyOneActivePerEnvironment(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f := buildReleaseFixture(t, ctx, tx)

	first := insertRelease(t, ctx, tx, f, "candidate")
	setImageDigest(t, ctx, tx, first)
	if _, err := tx.Exec(ctx, `UPDATE releases SET state='ready' WHERE id=$1`, first); err != nil {
		t.Fatalf("first candidate->ready: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE releases SET state='active' WHERE id=$1`, first); err != nil {
		t.Fatalf("first ready->active: %v", err)
	}

	second := insertRelease(t, ctx, tx, f, "candidate")
	setImageDigest(t, ctx, tx, second)
	if _, err := tx.Exec(ctx, `UPDATE releases SET state='ready' WHERE id=$1`, second); err != nil {
		t.Fatalf("second candidate->ready: %v", err)
	}
	mustFail(t, ctx, tx, "expected rejection of a second active release in the same environment",
		`UPDATE releases SET state='active' WHERE id=$1`, second)

	// Once the first is retained, the second may become active.
	if _, err := tx.Exec(ctx, `UPDATE releases SET state='retained' WHERE id=$1`, first); err != nil {
		t.Fatalf("first active->retained: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE releases SET state='active' WHERE id=$1`, second); err != nil {
		t.Fatalf("second ready->active after first retained: %v", err)
	}
}

// insertRouteHead creates a fresh route_heads row for f.environmentID with
// the given hostname, at route_revision 0.
func insertRouteHead(t *testing.T, ctx context.Context, tx pgx.Tx, environmentID, hostname string) {
	t.Helper()
	if _, err := tx.Exec(ctx, `INSERT INTO route_heads (environment_id, hostname) VALUES ($1,$2)`, environmentID, hostname); err != nil {
		t.Fatalf("insert route head: %v", err)
	}
}

func TestRouteHeadCAS(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f := buildReleaseFixture(t, ctx, tx)
	insertRouteHead(t, ctx, tx, f.environmentID, "app-"+randomSuffix(t)+".example.com")

	// Correct-revision CAS update succeeds and advances revision.
	tag, err := tx.Exec(ctx, `UPDATE route_heads SET hostname='new-'||hostname, route_revision=route_revision+1 WHERE environment_id=$1 AND route_revision=0`, f.environmentID)
	if err != nil {
		t.Fatalf("cas update: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("expected 1 row affected, got %d", tag.RowsAffected())
	}

	// Stale (replayed) revision affects zero rows.
	tag, err = tx.Exec(ctx, `UPDATE route_heads SET hostname='again-'||hostname, route_revision=route_revision+1 WHERE environment_id=$1 AND route_revision=0`, f.environmentID)
	if err != nil {
		t.Fatalf("stale cas update: %v", err)
	}
	if tag.RowsAffected() != 0 {
		t.Fatalf("expected 0 rows affected for stale (replayed) revision, got %d", tag.RowsAffected())
	}

	// Skipped revision (e.g. 2 instead of 1) also matches zero rows.
	tag, err = tx.Exec(ctx, `UPDATE route_heads SET hostname='skip-'||hostname, route_revision=route_revision+1 WHERE environment_id=$1 AND route_revision=2`, f.environmentID)
	if err != nil {
		t.Fatalf("skipped cas update: %v", err)
	}
	if tag.RowsAffected() != 0 {
		t.Fatalf("expected 0 rows affected for skipped revision, got %d", tag.RowsAffected())
	}

	// A matched row that forgets to advance revision is rejected by the trigger.
	mustFail(t, ctx, tx, "expected rejection of update that does not advance revision",
		`UPDATE route_heads SET hostname='forgot-'||hostname WHERE environment_id=$1 AND route_revision=1`, f.environmentID)

	// Identity is immutable.
	mustFail(t, ctx, tx, "expected rejection of environment_id change",
		`UPDATE route_heads SET environment_id='other', route_revision=route_revision+1 WHERE environment_id=$1 AND route_revision=1`, f.environmentID)

	// Retained: delete is rejected.
	mustFail(t, ctx, tx, "expected delete rejection",
		`DELETE FROM route_heads WHERE environment_id=$1`, f.environmentID)
}

// TestRouteHeadHostnameUniqueAcrossEnvironments proves the UNIQUE(hostname)
// constraint spans environments (two different environments cannot share
// one hostname), and that the CHECK's lowercase-only charset rejects a case
// variant outright before uniqueness is ever tested.
func TestRouteHeadHostnameUniqueAcrossEnvironments(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f1 := buildReleaseFixture(t, ctx, tx)
	f2 := buildReleaseFixture(t, ctx, tx)

	hostname := "shared-" + randomSuffix(t) + ".example.com"
	insertRouteHead(t, ctx, tx, f1.environmentID, hostname)

	mustFail(t, ctx, tx, "expected rejection of the same hostname on a second environment",
		`INSERT INTO route_heads (environment_id, hostname) VALUES ($1,$2)`, f2.environmentID, hostname)

	// A case variant of the same hostname is rejected by the CHECK's
	// lowercase-only grammar (never reaches the UNIQUE constraint at all).
	mustFail(t, ctx, tx, "expected rejection of an uppercase hostname",
		`INSERT INTO route_heads (environment_id, hostname) VALUES ($1,$2)`, f2.environmentID, "Shared-"+randomSuffix(t)+".Example.com")
}

// TestRouteHeadHostnameStructuralRules proves the CHECK's two additional
// restrictions beyond the DNS label grammar (ADR 0005 "Domains, TLS, and
// servers": a route hostname must be usable for public HTTPS/ACME): at
// least two labels, and a final label that is not all-digits (the IPv4
// dotted-quad rejection heuristic).
func TestRouteHeadHostnameStructuralRules(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f := buildReleaseFixture(t, ctx, tx)

	bad := []string{"localhost", "app", "1.2.3.4", "192.168.0.1"}
	for _, hostname := range bad {
		mustFail(t, ctx, tx, "expected rejection of "+hostname,
			`INSERT INTO route_heads (environment_id, hostname) VALUES ($1,$2)`, f.environmentID, hostname)
	}

	good := []string{"xn--bcher-kva.example", "a.b.example", "app1.example.io"}
	for _, hostname := range good {
		f2 := buildReleaseFixture(t, ctx, tx)
		if _, err := tx.Exec(ctx, `INSERT INTO route_heads (environment_id, hostname) VALUES ($1,$2)`, f2.environmentID, hostname); err != nil {
			t.Fatalf("expected %q to be accepted: %v", hostname, err)
		}
	}
}

// TestRouteHeadHostnameImmutableOnceActive proves
// validate_route_head_update's defense-in-depth guard: once
// active_release_id is set, the hostname on that row can never change
// through an UPDATE, even though route_revision/updated_at still advance
// normally for other columns.
func TestRouteHeadHostnameImmutableOnceActive(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f := buildReleaseFixture(t, ctx, tx)
	insertRouteHead(t, ctx, tx, f.environmentID, "app-"+randomSuffix(t)+".example.com")

	releaseID := insertRelease(t, ctx, tx, f, "candidate")
	setImageDigest(t, ctx, tx, releaseID)
	if _, err := tx.Exec(ctx, `UPDATE releases SET state='ready' WHERE id=$1`, releaseID); err != nil {
		t.Fatalf("candidate->ready: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE releases SET state='active' WHERE id=$1`, releaseID); err != nil {
		t.Fatalf("ready->active: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE route_heads SET active_release_id=$2, route_revision=route_revision+1 WHERE environment_id=$1`, f.environmentID, releaseID); err != nil {
		t.Fatalf("set active_release_id: %v", err)
	}

	mustFail(t, ctx, tx, "expected rejection of hostname change while a release is active",
		`UPDATE route_heads SET hostname='changed-'||hostname, route_revision=route_revision+1 WHERE environment_id=$1`, f.environmentID)

	// A same-hostname update (no actual change) is unaffected by the guard.
	if _, err := tx.Exec(ctx, `UPDATE route_heads SET hostname=hostname, reconciliation_required=true, route_revision=route_revision+1 WHERE environment_id=$1`, f.environmentID); err != nil {
		t.Fatalf("expected same-hostname update to succeed: %v", err)
	}
}

// TestRouteHeadActiveReleaseSameEnvironment proves the composite
// FOREIGN KEY (active_release_id, environment_id) REFERENCES
// releases(id, environment_id): a route head cannot point at a release
// belonging to a different environment.
func TestRouteHeadActiveReleaseSameEnvironment(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f1 := buildReleaseFixture(t, ctx, tx)
	f2 := buildReleaseFixture(t, ctx, tx)
	insertRouteHead(t, ctx, tx, f1.environmentID, "app-"+randomSuffix(t)+".example.com")

	// A release that belongs to f2's environment.
	foreignRelease := insertRelease(t, ctx, tx, f2, "candidate")
	setImageDigest(t, ctx, tx, foreignRelease)
	if _, err := tx.Exec(ctx, `UPDATE releases SET state='ready' WHERE id=$1`, foreignRelease); err != nil {
		t.Fatalf("candidate->ready: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE releases SET state='active' WHERE id=$1`, foreignRelease); err != nil {
		t.Fatalf("ready->active: %v", err)
	}

	mustFail(t, ctx, tx, "expected rejection of active_release_id from a different environment",
		`UPDATE route_heads SET active_release_id=$2, route_revision=route_revision+1 WHERE environment_id=$1`, f1.environmentID, foreignRelease)

	// The same release accepted for its own environment's route head.
	insertRouteHead(t, ctx, tx, f2.environmentID, "app2-"+randomSuffix(t)+".example.com")
	if _, err := tx.Exec(ctx, `UPDATE route_heads SET active_release_id=$2, route_revision=route_revision+1 WHERE environment_id=$1`, f2.environmentID, foreignRelease); err != nil {
		t.Fatalf("expected same-environment active_release_id to succeed: %v", err)
	}
}

func TestRouteEventsAppendOnly(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f := buildReleaseFixture(t, ctx, tx)
	insertRouteHead(t, ctx, tx, f.environmentID, "app-"+randomSuffix(t)+".example.com")

	if _, err := tx.Exec(ctx, `INSERT INTO route_events (environment_id, event_kind, route_revision, hostname) VALUES ($1,'hostname_assigned',0,'app.example.com')`, f.environmentID); err != nil {
		t.Fatalf("insert route event: %v", err)
	}
	// Malformed: hostname_assigned without a hostname.
	mustFail(t, ctx, tx, "expected rejection of hostname_assigned event with no hostname",
		`INSERT INTO route_events (environment_id, event_kind, route_revision, hostname) VALUES ($1,'hostname_assigned',1,NULL)`, f.environmentID)

	mustFail(t, ctx, tx, "expected update rejection (append-only)",
		`UPDATE route_events SET route_revision=99 WHERE environment_id=$1`, f.environmentID)
	mustFail(t, ctx, tx, "expected delete rejection (append-only)",
		`DELETE FROM route_events WHERE environment_id=$1`, f.environmentID)
}

func TestRouteHeadAuditEventsAppendOnly(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f := buildReleaseFixture(t, ctx, tx)

	if _, err := tx.Exec(ctx, `INSERT INTO route_head_audit_events (actor_type, actor_id, owner_id, environment_id, action, hostname, resulting_revision, result)
	      VALUES ('administrator','admin-1',$1,$2,'hostname_assigned','app.example.com',0,'success')`, f.ownerID, f.environmentID); err != nil {
		t.Fatalf("insert audit event: %v", err)
	}
	// A success result must carry a resulting_revision.
	mustFail(t, ctx, tx, "expected rejection of success result with no resulting_revision",
		`INSERT INTO route_head_audit_events (actor_type, actor_id, owner_id, environment_id, action, hostname, result)
	      VALUES ('administrator','admin-1',$1,$2,'hostname_assigned','app.example.com','success')`, f.ownerID, f.environmentID)

	mustFail(t, ctx, tx, "expected update rejection (append-only)",
		`UPDATE route_head_audit_events SET result='failure' WHERE environment_id=$1`, f.environmentID)
	mustFail(t, ctx, tx, "expected delete rejection (append-only)",
		`DELETE FROM route_head_audit_events WHERE environment_id=$1`, f.environmentID)
}

// TestRouteHeadActiveReleaseMustBeActiveAtCommit proves
// validate_route_head_active_release / route_heads_active_release_after_write:
// a route head whose active_release_id names a release that is not (yet, or
// no longer) 'active' is accepted per-statement (the trigger is DEFERRABLE
// INITIALLY DEFERRED) but rejected once constraints are checked -- here
// forced early with SET CONSTRAINTS ALL IMMEDIATE rather than a real COMMIT,
// so the assertion can still run inside this file's shared
// beginTx/rollback-per-test convention (see mustFail's SAVEPOINT comment:
// SET CONSTRAINTS is transactional and is undone by ROLLBACK TO SAVEPOINT
// exactly like any other statement, so this does not leak into later
// assertions in the same test).
func TestRouteHeadActiveReleaseMustBeActiveAtCommit(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	// Setup, committed in its own transaction: a release already sitting in
	// 'ready' (never touched again), and a route head that does not yet
	// reference it. Doing this setup in a separate, already-committed
	// transaction -- rather than folding it into the assertion below --
	// means the assertion transaction contains exactly one write (the route
	// head's active_release_id UPDATE), so only
	// route_heads_active_release_after_write can possibly fire; if this
	// setup instead updated releases.state inside the same transaction as
	// the assertion, releases_active_reference_after_update would also fire
	// and this test would not isolate which of the two triggers actually
	// caught the violation.
	setupTx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f := buildReleaseFixture(t, ctx, setupTx)
	releaseID := insertRelease(t, ctx, setupTx, f, "candidate")
	setImageDigest(t, ctx, setupTx, releaseID)
	if _, err := setupTx.Exec(ctx, `UPDATE releases SET state='ready' WHERE id=$1`, releaseID); err != nil {
		t.Fatalf("candidate->ready: %v", err)
	}
	insertRouteHead(t, ctx, setupTx, f.environmentID, "app-"+randomSuffix(t)+".example.com")
	if err := setupTx.Commit(ctx); err != nil {
		t.Fatalf("commit setup: %v", err)
	}

	tx := beginTx(t, ctx, conn)
	if _, err := tx.Exec(ctx, `UPDATE route_heads SET active_release_id=$2, route_revision=route_revision+1 WHERE environment_id=$1`, f.environmentID, releaseID); err != nil {
		t.Fatalf("point route head at a 'ready' (not yet active) release: %v", err)
	}

	mustFail(t, ctx, tx, "expected commit-time rejection of a route head pointing at a non-active release",
		"SET CONSTRAINTS ALL IMMEDIATE")
}

// TestRouteHeadReleaseCannotLeaveActiveWhileReferenced proves the converse
// direction: validate_release_not_referenced_unless_active /
// releases_active_reference_after_update rejects a release leaving 'active'
// while a route head still names it, checked (via SET CONSTRAINTS ALL
// IMMEDIATE) the same deferred way as the test above. As above, the
// pre-existing active release and the route head already pointing at it are
// set up and committed in a separate transaction first, so the assertion
// transaction contains exactly one write (the release's state UPDATE) and
// only releases_active_reference_after_update can possibly fire.
func TestRouteHeadReleaseCannotLeaveActiveWhileReferenced(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)

	setupTx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f := buildReleaseFixture(t, ctx, setupTx)
	releaseID := insertRelease(t, ctx, setupTx, f, "candidate")
	setImageDigest(t, ctx, setupTx, releaseID)
	if _, err := setupTx.Exec(ctx, `UPDATE releases SET state='ready' WHERE id=$1`, releaseID); err != nil {
		t.Fatalf("candidate->ready: %v", err)
	}
	if _, err := setupTx.Exec(ctx, `UPDATE releases SET state='active' WHERE id=$1`, releaseID); err != nil {
		t.Fatalf("ready->active: %v", err)
	}
	insertRouteHead(t, ctx, setupTx, f.environmentID, "app-"+randomSuffix(t)+".example.com")
	if _, err := setupTx.Exec(ctx, `UPDATE route_heads SET active_release_id=$2, route_revision=route_revision+1 WHERE environment_id=$1`, f.environmentID, releaseID); err != nil {
		t.Fatalf("point route head at the active release: %v", err)
	}
	if err := setupTx.Commit(ctx); err != nil {
		t.Fatalf("commit setup: %v", err)
	}

	// Retire the release without repointing the head away from it first.
	tx := beginTx(t, ctx, conn)
	if _, err := tx.Exec(ctx, `UPDATE releases SET state='retained' WHERE id=$1`, releaseID); err != nil {
		t.Fatalf("active->retained: %v", err)
	}

	mustFail(t, ctx, tx, "expected commit-time rejection of retiring a release still referenced by a route head",
		"SET CONSTRAINTS ALL IMMEDIATE")
}

// TestRouteHeadActiveReleaseHandoffCommitsInOneTransaction proves the
// legitimate case the deferred check exists to allow: Step 10c's real
// finalization sequence (predecessor active->retained, candidate
// ready->active, head repointed + revision advanced) all in one
// transaction.
//
// The pre-existing state (route head already pointing at an already-active
// predecessor, and a ready candidate) is set up and committed in a first,
// separate transaction, exactly like it would exist in production before a
// rollout ever begins -- Step 10c's transaction never creates the route
// head or drives the predecessor to 'active' itself, it only performs the
// three-write handoff against state that already exists. Folding the setup
// into the same transaction as the handoff would be a test bug, not a
// faithful model: each deferred ROW-level trigger firing is queued and
// re-evaluated independently at commit against whatever the referenced
// tables look like *then*, so an earlier, now-superseded firing (e.g. the
// route head's initial UPDATE that pointed it at the predecessor while the
// predecessor was still active) would be re-checked at commit and now see
// the predecessor as 'retained', failing spuriously -- exactly what a real
// Step 10c transaction never does, since it only ever issues the route
// head's *one* repoint UPDATE.
//
// Unlike every other test in this file, this test manages its own
// transactions and issues real COMMITs rather than relying on the shared
// beginTx/rollback-per-test convention (mustFail's SET CONSTRAINTS ALL
// IMMEDIATE trick only proves a rejection; proving an actual commit
// succeeds requires an actual commit). Every identifier is freshly
// suffixed, so the committed rows cannot collide with any other test's.
func TestRouteHeadActiveReleaseHandoffCommitsInOneTransaction(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)

	// Setup transaction: pre-existing, already-committed state.
	setupTx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f := buildReleaseFixture(t, ctx, setupTx)
	predecessor := insertRelease(t, ctx, setupTx, f, "candidate")
	setImageDigest(t, ctx, setupTx, predecessor)
	if _, err := setupTx.Exec(ctx, `UPDATE releases SET state='ready' WHERE id=$1`, predecessor); err != nil {
		t.Fatalf("predecessor candidate->ready: %v", err)
	}
	if _, err := setupTx.Exec(ctx, `UPDATE releases SET state='active' WHERE id=$1`, predecessor); err != nil {
		t.Fatalf("predecessor ready->active: %v", err)
	}
	insertRouteHead(t, ctx, setupTx, f.environmentID, "handoff-"+randomSuffix(t)+".example.com")
	if _, err := setupTx.Exec(ctx, `UPDATE route_heads SET active_release_id=$2, route_revision=route_revision+1 WHERE environment_id=$1`, f.environmentID, predecessor); err != nil {
		t.Fatalf("point route head at predecessor: %v", err)
	}
	candidate := insertRelease(t, ctx, setupTx, f, "candidate")
	setImageDigest(t, ctx, setupTx, candidate)
	if _, err := setupTx.Exec(ctx, `UPDATE releases SET state='ready' WHERE id=$1`, candidate); err != nil {
		t.Fatalf("candidate candidate->ready: %v", err)
	}
	if err := setupTx.Commit(ctx); err != nil {
		t.Fatalf("commit setup: %v", err)
	}

	// Handoff transaction: the real Step 10c sequence -- predecessor
	// active->retained, candidate ready->active, head repointed +
	// revision advanced -- exactly one UPDATE per row, all in one
	// transaction.
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	if _, err := tx.Exec(ctx, `UPDATE releases SET state='retained' WHERE id=$1`, predecessor); err != nil {
		t.Fatalf("predecessor active->retained: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE releases SET state='active' WHERE id=$1`, candidate); err != nil {
		t.Fatalf("candidate ready->active: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE route_heads SET active_release_id=$2, route_revision=route_revision+1 WHERE environment_id=$1`, f.environmentID, candidate); err != nil {
		t.Fatalf("repoint route head at candidate: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("expected the full handoff sequence to commit: %v", err)
	}
	committed = true
}
