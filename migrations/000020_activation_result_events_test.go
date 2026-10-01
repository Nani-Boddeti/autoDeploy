//go:build integration

package migrations

import (
	"context"
	"testing"
)

// TestAgentJobEventsActivationResultLiterals proves migration 000020 widened
// agent_job_events' event-type CHECK with exactly the two tx2 literals while
// keeping every earlier one, and still rejects an unknown literal. The
// constraint name is read from pg_constraint rather than assumed.
func TestAgentJobEventsActivationResultLiterals(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f := buildReleaseFixture(t, ctx, tx)

	var name string
	if err := tx.QueryRow(ctx, `SELECT conname FROM pg_constraint WHERE conrelid='agent_job_events'::regclass AND contype='c' AND pg_get_constraintdef(oid) LIKE '%event_type%'`).Scan(&name); err != nil {
		t.Fatalf("introspect event_type check: %v", err)
	}
	if name != "agent_job_events_event_type_check" {
		t.Fatalf("event_type check is named %q", name)
	}

	literals := []string{"enqueued", "claimed", "cancel_requested", "cancelled", "expired", "retry_scheduled", "succeeded", "failed", "dead", "reconciled", "activation_authorized", "activated", "activation_reconciliation_required"}
	for index, literal := range literals {
		if _, err := tx.Exec(ctx, `INSERT INTO agent_job_events(job_id, sequence, event_type, attempt_id) VALUES ($1,$2,$3,$4)`, f.jobID, 1000+index, literal, f.attemptID); err != nil {
			t.Fatalf("event type %q rejected: %v", literal, err)
		}
	}
	mustFail(t, ctx, tx, "expected rejection of an unknown event type",
		`INSERT INTO agent_job_events(job_id, sequence, event_type, attempt_id) VALUES ($1,2000,'activation_unknown',$2)`, f.jobID, f.attemptID)
}

// TestRouteHeadReconciliationFlagWithoutRevisionBump proves 000020's widened
// validate_route_head_update: the reconciliation flag may move in either
// direction alone at an unchanged revision, nothing else may ride along with
// an unchanged revision, and the 000018 hostname and updated_at rules still
// fire (the identity rule is isolated in TestRouteHeadEnvironmentIDIsImmutable).
func TestRouteHeadReconciliationFlagWithoutRevisionBump(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f := buildReleaseFixture(t, ctx, tx)
	insertRouteHead(t, ctx, tx, f.environmentID, "app-"+randomSuffix(t)+".example.com")

	releaseID := insertRelease(t, ctx, tx, f, "candidate")
	setImageDigest(t, ctx, tx, releaseID)
	for _, state := range []string{"ready", "active"} {
		if _, err := tx.Exec(ctx, `UPDATE releases SET state=$2 WHERE id=$1`, releaseID, state); err != nil {
			t.Fatalf("release -> %s: %v", state, err)
		}
	}
	// Ordinary activation-shaped update still accepted (revision +1 with
	// active_release_id).
	if _, err := tx.Exec(ctx, `UPDATE route_heads SET active_release_id=$2, route_revision=route_revision+1 WHERE environment_id=$1`, f.environmentID, releaseID); err != nil {
		t.Fatalf("activation-shaped update: %v", err)
	}

	revision := func() int64 {
		var r int64
		if err := tx.QueryRow(ctx, `SELECT route_revision FROM route_heads WHERE environment_id=$1`, f.environmentID).Scan(&r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	before := revision()

	// Flag set alone, then cleared alone, at an unchanged revision.
	if _, err := tx.Exec(ctx, `UPDATE route_heads SET reconciliation_required=true, updated_at=clock_timestamp() WHERE environment_id=$1`, f.environmentID); err != nil {
		t.Fatalf("set flag alone: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE route_heads SET reconciliation_required=false, updated_at=clock_timestamp() WHERE environment_id=$1`, f.environmentID); err != nil {
		t.Fatalf("clear flag alone: %v", err)
	}
	if got := revision(); got != before {
		t.Fatalf("revision moved from %d to %d on flag-only updates", before, got)
	}

	// Nothing else may ride along with an unchanged revision.
	mustFail(t, ctx, tx, "expected rejection of active_release_id change at unchanged revision",
		`UPDATE route_heads SET reconciliation_required=true, active_release_id=NULL WHERE environment_id=$1`, f.environmentID)
	mustFail(t, ctx, tx, "expected rejection of created_at change at unchanged revision",
		`UPDATE route_heads SET reconciliation_required=true, created_at=created_at - interval '1 hour' WHERE environment_id=$1`, f.environmentID)
	mustFail(t, ctx, tx, "expected rejection of a no-op update at unchanged revision",
		`UPDATE route_heads SET updated_at=clock_timestamp() WHERE environment_id=$1`, f.environmentID)
	// Revision deltas other than 0 or +1 still raise.
	mustFail(t, ctx, tx, "expected rejection of revision +2",
		`UPDATE route_heads SET route_revision=route_revision+2 WHERE environment_id=$1`, f.environmentID)
	mustFail(t, ctx, tx, "expected rejection of revision -1",
		`UPDATE route_heads SET route_revision=route_revision-1 WHERE environment_id=$1`, f.environmentID)

	// The other three 000018 rules still fire. Each statement below is
	// constructed so that ONLY the trigger rule can reject it (environment_id
	// is covered separately below, on a head with no active release):
	//  - hostname: the head has an active release, and no table constraint
	//    pins the hostname.
	//  - updated_at: first pushed an hour AHEAD, then set back by only 30
	//    minutes, which stays above created_at, so the table CHECK
	//    (updated_at >= created_at) cannot be what rejects it.
	mustFail(t, ctx, tx, "expected rejection of hostname change while a release is active",
		`UPDATE route_heads SET hostname='changed-'||hostname, route_revision=route_revision+1 WHERE environment_id=$1`, f.environmentID)
	if _, err := tx.Exec(ctx, `UPDATE route_heads SET updated_at=updated_at + interval '1 hour', route_revision=route_revision+1 WHERE environment_id=$1`, f.environmentID); err != nil {
		t.Fatalf("push updated_at ahead: %v", err)
	}
	mustFail(t, ctx, tx, "expected rejection of non-monotonic updated_at (above created_at, so only the trigger can reject it)",
		`UPDATE route_heads SET updated_at=updated_at - interval '30 minutes', route_revision=route_revision+1 WHERE environment_id=$1`, f.environmentID)
	mustFail(t, ctx, tx, "expected rejection of non-monotonic updated_at on a flag-only update (above created_at, so only the trigger can reject it)",
		`UPDATE route_heads SET reconciliation_required=true, updated_at=updated_at - interval '30 minutes' WHERE environment_id=$1`, f.environmentID)
}

// TestRouteHeadEnvironmentIDIsImmutable isolates 000018's identity rule. On a
// head WITH an active release the composite FK (active_release_id,
// environment_id) rejects an environment_id change on its own, so the rule is
// exercised on a head whose active_release_id is NULL (the FK does not apply)
// and whose target environment genuinely exists, so no other constraint can
// be what rejects the update. The revision advances by one, so rule 2 does not
// fire either.
func TestRouteHeadEnvironmentIDIsImmutable(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f := buildReleaseFixture(t, ctx, tx)
	insertRouteHead(t, ctx, tx, f.environmentID, "app-"+randomSuffix(t)+".example.com")
	other := "env-other-" + randomSuffix(t)
	if _, err := tx.Exec(ctx, `INSERT INTO environment_slots (environment_id) VALUES ($1)`, other); err != nil {
		t.Fatalf("second environment: %v", err)
	}
	mustFail(t, ctx, tx, "expected rejection of environment_id change on a head with no active release",
		`UPDATE route_heads SET environment_id=$2, route_revision=route_revision+1 WHERE environment_id=$1`, f.environmentID, other)
}

// TestRouteHeadFlagExemptionRejectsRidersInIsolation isolates each clause of
// 000020's exemption from the rules that would reject the same statement
// anyway. The hostname freeze (an active release pins the hostname) raises
// for a hostname change on any route head with an active release, so the
// hostname clause is only exercised on a route head with NO active release;
// likewise moving active_release_id non-NULL -> a different non-NULL at an
// unchanged revision is not caught by any other BEFORE-UPDATE rule (the
// deferred active-release trigger only fires at commit, which these
// rolled-back tests never reach). Each statement must therefore be rejected
// by the exemption itself: deleting its clause makes the UPDATE succeed.
func TestRouteHeadFlagExemptionRejectsRidersInIsolation(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)

	t.Run("hostname at unchanged revision, no active release", func(t *testing.T) {
		tx := beginTx(t, ctx, conn)
		f := buildReleaseFixture(t, ctx, tx)
		insertRouteHead(t, ctx, tx, f.environmentID, "app-"+randomSuffix(t)+".example.com")
		mustFail(t, ctx, tx, "expected rejection of hostname change riding a flag-only update",
			`UPDATE route_heads SET reconciliation_required=true, hostname='x-'||hostname WHERE environment_id=$1`, f.environmentID)
	})

	t.Run("flag unchanged at unchanged revision", func(t *testing.T) {
		tx := beginTx(t, ctx, conn)
		f := buildReleaseFixture(t, ctx, tx)
		insertRouteHead(t, ctx, tx, f.environmentID, "app-"+randomSuffix(t)+".example.com")
		mustFail(t, ctx, tx, "expected rejection of an update that changes nothing relevant",
			`UPDATE route_heads SET updated_at=clock_timestamp() WHERE environment_id=$1`, f.environmentID)
	})

	t.Run("active_release_id moved to a different release", func(t *testing.T) {
		tx := beginTx(t, ctx, conn)
		f := buildReleaseFixture(t, ctx, tx)
		insertRouteHead(t, ctx, tx, f.environmentID, "app-"+randomSuffix(t)+".example.com")
		first := insertRelease(t, ctx, tx, f, "candidate")
		setImageDigest(t, ctx, tx, first)
		for _, state := range []string{"ready", "active"} {
			if _, err := tx.Exec(ctx, `UPDATE releases SET state=$2 WHERE id=$1`, first, state); err != nil {
				t.Fatalf("first release -> %s: %v", state, err)
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE route_heads SET active_release_id=$2, route_revision=route_revision+1 WHERE environment_id=$1`, f.environmentID, first); err != nil {
			t.Fatalf("activate first: %v", err)
		}
		// Hand the active slot to a second release so the pointer could
		// legally move (one active per environment is enforced immediately).
		second := insertRelease(t, ctx, tx, f, "candidate")
		setImageDigest(t, ctx, tx, second)
		if _, err := tx.Exec(ctx, `UPDATE releases SET state='retained' WHERE id=$1`, first); err != nil {
			t.Fatalf("first -> retained: %v", err)
		}
		for _, state := range []string{"ready", "active"} {
			if _, err := tx.Exec(ctx, `UPDATE releases SET state=$2 WHERE id=$1`, second, state); err != nil {
				t.Fatalf("second release -> %s: %v", state, err)
			}
		}
		mustFail(t, ctx, tx, "expected rejection of active_release_id moving non-NULL to a different non-NULL at unchanged revision",
			`UPDATE route_heads SET reconciliation_required=true, active_release_id=$2 WHERE environment_id=$1`, f.environmentID, second)
		// Control: the same move with the legitimate +1 revision is accepted.
		if _, err := tx.Exec(ctx, `UPDATE route_heads SET active_release_id=$2, route_revision=route_revision+1 WHERE environment_id=$1`, f.environmentID, second); err != nil {
			t.Fatalf("control: revision+1 pointer move rejected: %v", err)
		}
	})

	t.Run("created_at at unchanged revision, no active release", func(t *testing.T) {
		tx := beginTx(t, ctx, conn)
		f := buildReleaseFixture(t, ctx, tx)
		insertRouteHead(t, ctx, tx, f.environmentID, "app-"+randomSuffix(t)+".example.com")
		mustFail(t, ctx, tx, "expected rejection of created_at change riding a flag-only update",
			`UPDATE route_heads SET reconciliation_required=true, created_at=created_at - interval '1 hour' WHERE environment_id=$1`, f.environmentID)
	})
}
