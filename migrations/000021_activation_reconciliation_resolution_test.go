//go:build integration

package migrations

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const openActivationIndex = "release_operations_environment_open_activation_idx"

// Every rejection below is asserted by SQLSTATE and exact message, never by
// "some error happened". The trigger raises with RAISE EXCEPTION (P0001); a
// pre-existing table CHECK (23514, e.g. release_operations_check1 or
// release_operations_check) or a unique violation (23505) has a different
// code and message, so a case the trigger does not itself reject fails here
// instead of passing vacuously. Each case is also built so that no CHECK
// could fire even if the trigger let it through: closed states carry a
// closed_at, open states carry NULL, so release_operations_check1 holds.

// mustRaiseTrigger runs sql in a savepoint and requires the trigger's own
// RAISE EXCEPTION with exactly wantMsg.
func mustRaiseTrigger(t *testing.T, ctx context.Context, tx pgx.Tx, wantMsg, sql string, args ...any) {
	t.Helper()
	savepointCounter++
	sp := fmt.Sprintf("sp_%d", savepointCounter)
	if _, err := tx.Exec(ctx, "SAVEPOINT "+sp); err != nil {
		t.Fatalf("create savepoint: %v", err)
	}
	_, err := tx.Exec(ctx, sql, args...)
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		t.Fatalf("expected trigger rejection %q, statement succeeded: %s", wantMsg, sql)
	case !errors.As(err, &pgErr):
		t.Fatalf("expected PgError, got %v", err)
	case pgErr.Code != "P0001" || pgErr.Message != wantMsg:
		t.Fatalf("expected trigger P0001 %q, got %s %q (constraint %q): %s", wantMsg, pgErr.Code, pgErr.Message, pgErr.ConstraintName, sql)
	}
	if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT "+sp); err != nil {
		t.Fatalf("rollback to savepoint: %v", err)
	}
}

func mustExec(t *testing.T, ctx context.Context, tx pgx.Tx, sql string, args ...any) {
	t.Helper()
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// insertActivateOp inserts an activate operation directly at state (the
// insert trigger does not constrain state). Closed states get closed_at =
// clock_timestamp() + 1 hour (strictly after created_at, so a test can move it
// backwards and still stay >= created_at) and open ones NULL, satisfying release_operations_check1.
func insertActivateOp(t *testing.T, ctx context.Context, tx pgx.Tx, f releaseFixture, releaseID, state string) int64 {
	t.Helper()
	var id int64
	err := tx.QueryRow(ctx, `INSERT INTO release_operations
	      (release_id, job_id, attempt_id, fence, kind, digest, state, closed_at, environment_id, expected_route_revision, route_digest)
	      VALUES ($1,$2,$3,1,'activate',$4,$5::text,CASE WHEN $5::text IN ('started','switch_authorized') THEN NULL ELSE clock_timestamp() + interval '1 hour' END,$6,1,$7) RETURNING id`,
		releaseID, f.jobID, f.attemptID, randomBytes(t, 32), state, f.environmentID, randomBytes(t, 32)).Scan(&id)
	if err != nil {
		t.Fatalf("insert activate op at %s: %v", state, err)
	}
	return id
}

// newActivateOp gives the operation its own environment so the open-activation
// unique index can never be what rejects or shapes a transition case.
func newActivateOp(t *testing.T, ctx context.Context, tx pgx.Tx, state string) (releaseFixture, int64) {
	t.Helper()
	f := buildReleaseFixture(t, ctx, tx)
	return f, insertActivateOp(t, ctx, tx, f, insertRelease(t, ctx, tx, f, "candidate"), state)
}

func newPrepareOp(t *testing.T, ctx context.Context, tx pgx.Tx, state string) int64 {
	t.Helper()
	f := buildReleaseFixture(t, ctx, tx)
	var id int64
	err := tx.QueryRow(ctx, `INSERT INTO release_operations (release_id, job_id, attempt_id, fence, kind, digest, state, closed_at)
	      VALUES ($1,$2,$3,1,'prepare',$4,$5::text,CASE WHEN $5::text = 'started' THEN NULL ELSE clock_timestamp() END) RETURNING id`,
		insertRelease(t, ctx, tx, f, "candidate"), f.jobID, f.attemptID, randomBytes(t, 32), state).Scan(&id)
	if err != nil {
		t.Fatalf("insert prepare op at %s: %v", state, err)
	}
	return id
}

func opState(t *testing.T, ctx context.Context, tx pgx.Tx, id int64) string {
	t.Helper()
	var s string
	if err := tx.QueryRow(ctx, `SELECT state FROM release_operations WHERE id=$1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestActivateReconciliationResolutionTransitions covers the kind='activate'
// branch of validate_release_operation_update as replaced by 000021.
func TestActivateReconciliationResolutionTransitions(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)

	// New edges out of reconciliation_required. closed_at is untouched, so
	// check1 ((state IN open) = (closed_at IS NULL)) holds before and after.
	for _, target := range []string{"succeeded", "failed"} {
		_, id := newActivateOp(t, ctx, tx, "reconciliation_required")
		mustExec(t, ctx, tx, `UPDATE release_operations SET state=$2 WHERE id=$1`, id, target)
		if got := opState(t, ctx, tx, id); got != target {
			t.Fatalf("rr -> %s left state %s", target, got)
		}
	}
	// rr -> rr with closed_at unchanged: a late-evidence write.
	_, rr := newActivateOp(t, ctx, tx, "reconciliation_required")
	mustExec(t, ctx, tx, `UPDATE release_operations SET outcome_summary='late agent evidence' WHERE id=$1`, rr)

	// closed_at immutability. The new instant is finite and >= created_at, and
	// the state stays closed, so neither release_operations_check nor check1 can
	// be the rejecter -- only the trigger can.
	const closedAtMsg = "activate release operation closed_at is immutable once set"
	mustRaiseTrigger(t, ctx, tx, closedAtMsg,
		`UPDATE release_operations SET closed_at=clock_timestamp() + interval '2 hours' WHERE id=$1`, rr)
	mustRaiseTrigger(t, ctx, tx, closedAtMsg,
		`UPDATE release_operations SET state='succeeded', closed_at=clock_timestamp() + interval '2 hours' WHERE id=$1`, rr)
	mustRaiseTrigger(t, ctx, tx, closedAtMsg,
		`UPDATE release_operations SET state='failed', closed_at=clock_timestamp() + interval '2 hours' WHERE id=$1`, rr)

	// NULL on one side: IS DISTINCT FROM must catch it. (A weaker <> would let
	// this fall through to a 23514 on release_operations_check1, which
	// mustRaiseTrigger rejects by SQLSTATE and message.)
	mustRaiseTrigger(t, ctx, tx, closedAtMsg,
		`UPDATE release_operations SET closed_at=NULL WHERE id=$1`, rr)
	// Backwards, to a value still >= created_at (so release_operations_check
	// cannot be the rejecter): the rule is "unchanged", not "not later".
	mustRaiseTrigger(t, ctx, tx, closedAtMsg,
		`UPDATE release_operations SET closed_at=created_at WHERE id=$1`, rr)

	// rr may not go anywhere else. closed_at = NULL keeps check1 satisfied for
	// the open target states, so the trigger's state list is the only rejecter.
	const rrMsg = "reconciliation_required activate release operation may only move to succeeded, failed or stay reconciliation_required"
	mustRaiseTrigger(t, ctx, tx, rrMsg, `UPDATE release_operations SET state='switch_authorized', closed_at=NULL WHERE id=$1`, rr)
	mustRaiseTrigger(t, ctx, tx, rrMsg, `UPDATE release_operations SET state='started', closed_at=NULL WHERE id=$1`, rr)

	// Previously permitted edges still work; switch_authorized -> failed still
	// raises (closed_at set, so check1 holds and the trigger is the rejecter).
	for _, target := range []string{"succeeded", "reconciliation_required"} {
		_, id := newActivateOp(t, ctx, tx, "switch_authorized")
		mustExec(t, ctx, tx, `UPDATE release_operations SET state=$2, closed_at=clock_timestamp() WHERE id=$1`, id, target)
		if got := opState(t, ctx, tx, id); got != target {
			t.Fatalf("switch_authorized -> %s left state %s", target, got)
		}
	}
	_, sw := newActivateOp(t, ctx, tx, "switch_authorized")
	mustRaiseTrigger(t, ctx, tx, "activate release operation must close to succeeded or reconciliation_required",
		`UPDATE release_operations SET state='failed', closed_at=clock_timestamp() WHERE id=$1`, sw)

	// Anything out of succeeded / failed / started still raises "is closed". The
	// closed -> closed targets keep closed_at, the started -> closed target sets
	// it, so check1 holds in every case.
	for _, from := range []string{"succeeded", "failed"} {
		for _, to := range []string{"succeeded", "failed", "reconciliation_required"} {
			_, id := newActivateOp(t, ctx, tx, from)
			mustRaiseTrigger(t, ctx, tx, fmt.Sprintf("release operation %s is closed", from),
				`UPDATE release_operations SET state=$2 WHERE id=$1`, id, to)
		}
	}
	_, started := newActivateOp(t, ctx, tx, "started")
	mustRaiseTrigger(t, ctx, tx, "release operation started is closed",
		`UPDATE release_operations SET state='succeeded', closed_at=clock_timestamp() WHERE id=$1`, started)
}

// TestNonActivateReleaseOperationBranchUnchanged pins the non-activate branch
// 000021 must leave byte-for-byte alone.
func TestNonActivateReleaseOperationBranchUnchanged(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)

	for _, target := range []string{"succeeded", "failed"} {
		id := newPrepareOp(t, ctx, tx, "started")
		mustExec(t, ctx, tx, `UPDATE release_operations SET state=$2, closed_at=clock_timestamp() WHERE id=$1`, id, target)
		if got := opState(t, ctx, tx, id); got != target {
			t.Fatalf("started -> %s left state %s", target, got)
		}
	}
	started := newPrepareOp(t, ctx, tx, "started")
	mustRaiseTrigger(t, ctx, tx, "release operation must close to succeeded or failed",
		`UPDATE release_operations SET state='reconciliation_required', closed_at=clock_timestamp() WHERE id=$1`, started)
	// The new activate-only reconciliation edge must not leak to other kinds:
	// a closed prepare operation is still closed, rr or not.
	for _, from := range []string{"succeeded", "failed"} {
		id := newPrepareOp(t, ctx, tx, from)
		mustRaiseTrigger(t, ctx, tx, fmt.Sprintf("release operation %s is closed", from),
			`UPDATE release_operations SET state='succeeded' WHERE id=$1`, id)
	}
	rrPrepare := newPrepareOp(t, ctx, tx, "reconciliation_required")
	mustRaiseTrigger(t, ctx, tx, "release operation reconciliation_required is closed",
		`UPDATE release_operations SET state='succeeded' WHERE id=$1`, rrPrepare)
}

// TestReleaseOperationIdentityStillImmutable proves every identity column the
// trigger guards still raises. The id column IS reachable: it is GENERATED
// ALWAYS AS IDENTITY, so SET id=99 is refused by Postgres (428C9), but
// SET id=DEFAULT is allowed, draws a new identity value and reaches the
// trigger. Each case changes ONLY the one column and leaves state alone
// (switch_authorized would otherwise be rejected with a different message).
// Some values (e.g. fence=2 with a job_id that does not match) would not
// satisfy every CHECK/FK; that is harmless only because the BEFORE ROW
// trigger fires first and raises before any constraint is evaluated.
func TestReleaseOperationIdentityStillImmutable(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)

	f, activate := newActivateOp(t, ctx, tx, "switch_authorized")
	other := buildReleaseFixture(t, ctx, tx)
	otherRelease := insertRelease(t, ctx, tx, f, "candidate") // same environment as f
	prepare := newPrepareOp(t, ctx, tx, "started")

	const msg = "release operation identity is immutable"
	cases := []struct {
		column string
		op     int64
		set    string
		arg    any
	}{
		{"id", activate, `id=DEFAULT`, nil},
		{"release_id", activate, `release_id=$2`, otherRelease},
		{"job_id", activate, `job_id=$2`, other.jobID},
		{"attempt_id", activate, `attempt_id=$2`, other.attemptID},
		{"fence", activate, `fence=$2`, int64(2)},
		{"kind", prepare, `kind=$2`, "cleanup"},
		{"digest", activate, `digest=$2`, randomBytes(t, 32)},
		{"created_at", activate, `created_at=created_at - interval '1 hour'`, nil},
		{"environment_id", activate, `environment_id=$2`, "env-other-" + randomSuffix(t)},
		{"expected_active_release_id", activate, `expected_active_release_id=$2`, otherRelease},
		{"expected_route_revision", activate, `expected_route_revision=expected_route_revision+1`, nil},
		{"route_digest", activate, `route_digest=$2`, randomBytes(t, 32)},
	}
	for _, tc := range cases {
		t.Run(tc.column, func(t *testing.T) {
			sql := `UPDATE release_operations SET ` + tc.set + ` WHERE id=$1`
			args := []any{tc.op}
			if tc.arg != nil {
				args = append(args, tc.arg)
			}
			mustRaiseTrigger(t, ctx, tx, msg, sql, args...)
		})
	}
}

// TestOpenActivationIndexCoversEveryUnresolvedState is the non-vacuity test
// for 000021 part 3. Under 000019's predicate (state = 'switch_authorized')
// every colliding pair below coexisted; the NOT IN rule makes them collide.
func TestOpenActivationIndexCoversEveryUnresolvedState(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)

	var def string
	if err := tx.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE tablename='release_operations' AND indexname=$1`, openActivationIndex).Scan(&def); err != nil {
		t.Fatalf("introspect open-activation index: %v", err)
	}

	mustCollide := func(f releaseFixture, releaseID, state string) {
		t.Helper()
		savepointCounter++
		sp := fmt.Sprintf("sp_%d", savepointCounter)
		mustExec(t, ctx, tx, "SAVEPOINT "+sp)
		var id int64
		err := tx.QueryRow(ctx, `INSERT INTO release_operations
		      (release_id, job_id, attempt_id, fence, kind, digest, state, closed_at, environment_id, expected_route_revision, route_digest)
		      VALUES ($1,$2,$3,1,'activate',$4,$5::text,CASE WHEN $5::text IN ('started','switch_authorized') THEN NULL ELSE clock_timestamp() END,$6,1,$7) RETURNING id`,
			releaseID, f.jobID, f.attemptID, randomBytes(t, 32), state, f.environmentID, randomBytes(t, 32)).Scan(&id)
		var pgErr *pgconn.PgError
		if err == nil {
			t.Fatalf("second unresolved activation (%s) inserted beside an unresolved one; index def: %s", state, def)
		}
		if !errors.As(err, &pgErr) || pgErr.Code != "23505" || pgErr.ConstraintName != openActivationIndex {
			t.Fatalf("expected 23505 on %s, got %v", openActivationIndex, err)
		}
		mustExec(t, ctx, tx, "ROLLBACK TO SAVEPOINT "+sp)
	}

	// switch_authorized + reconciliation_required in the SAME environment:
	// the experiment that motivated the migration. A is parked at rr by a real
	// close, then a second Activate (B, switch_authorized) is attempted.
	f := buildReleaseFixture(t, ctx, tx)
	rel := insertRelease(t, ctx, tx, f, "candidate")
	a := insertActivateOp(t, ctx, tx, f, rel, "switch_authorized")
	mustExec(t, ctx, tx, `UPDATE release_operations SET state='reconciliation_required', closed_at=clock_timestamp() WHERE id=$1`, a)
	mustCollide(f, rel, "switch_authorized")
	// ...and the other order, plus rr + rr.
	mustCollide(f, rel, "reconciliation_required")

	f2 := buildReleaseFixture(t, ctx, tx)
	rel2 := insertRelease(t, ctx, tx, f2, "candidate")
	insertActivateOp(t, ctx, tx, f2, rel2, "switch_authorized")
	mustCollide(f2, rel2, "reconciliation_required")
	mustCollide(f2, rel2, "switch_authorized")
	// 'started' is unresolved too. An enumerated predicate (IN
	// ('switch_authorized','reconciliation_required')) would let it through:
	// this is the rule-versus-enumeration case, in both directions.
	mustCollide(f2, rel2, "started")
	f4 := buildReleaseFixture(t, ctx, tx)
	rel4 := insertRelease(t, ctx, tx, f4, "candidate")
	insertActivateOp(t, ctx, tx, f4, rel4, "started")
	mustCollide(f4, rel4, "started")
	mustCollide(f4, rel4, "switch_authorized")
	mustCollide(f4, rel4, "reconciliation_required")

	// The index must not over-reach: resolved operations coexist freely, with
	// each other and beside one unresolved operation.
	f3 := buildReleaseFixture(t, ctx, tx)
	rel3 := insertRelease(t, ctx, tx, f3, "candidate")
	for _, state := range []string{"succeeded", "succeeded", "failed", "failed"} {
		insertActivateOp(t, ctx, tx, f3, rel3, state)
	}
	open := insertActivateOp(t, ctx, tx, f3, rel3, "switch_authorized")
	insertActivateOp(t, ctx, tx, f3, rel3, "succeeded")
	// Resolving the unresolved one frees the slot for the next activation.
	mustExec(t, ctx, tx, `UPDATE release_operations SET state='reconciliation_required', closed_at=clock_timestamp() WHERE id=$1`, open)
	mustExec(t, ctx, tx, `UPDATE release_operations SET state='failed' WHERE id=$1`, open)
	insertActivateOp(t, ctx, tx, f3, rel3, "switch_authorized")
}

// TestAgentJobEventsReconciliationResolutionLiterals proves 000021 widened the
// event-type CHECK with the two new literals, kept every 000020 literal, and
// still rejects an unknown one (by the named CHECK, 23514).
func TestAgentJobEventsReconciliationResolutionLiterals(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f := buildReleaseFixture(t, ctx, tx)

	literals := []string{"enqueued", "claimed", "cancel_requested", "cancelled", "expired", "retry_scheduled", "succeeded", "failed", "dead", "reconciled", "activation_authorized", "activated", "activation_reconciliation_required", "activation_not_activated", "activation_late_report"}
	for index, literal := range literals {
		if _, err := tx.Exec(ctx, `INSERT INTO agent_job_events(job_id, sequence, event_type, attempt_id) VALUES ($1,$2,$3,$4)`, f.jobID, 1000+index, literal, f.attemptID); err != nil {
			t.Fatalf("event type %q rejected: %v", literal, err)
		}
	}
	savepointCounter++
	sp := fmt.Sprintf("sp_%d", savepointCounter)
	mustExec(t, ctx, tx, "SAVEPOINT "+sp)
	_, err := tx.Exec(ctx, `INSERT INTO agent_job_events(job_id, sequence, event_type, attempt_id) VALUES ($1,2000,'activation_unknown',$2)`, f.jobID, f.attemptID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "agent_job_events_event_type_check" {
		t.Fatalf("expected 23514 on agent_job_events_event_type_check for an unknown literal, got %v", err)
	}
	mustExec(t, ctx, tx, "ROLLBACK TO SAVEPOINT "+sp)
}
