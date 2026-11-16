//go:build integration

package migrations

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

const flaggedHostnameMessage = "route head hostname cannot change while reconciliation is required"

// mustFailWith is mustFail plus an assertion on the error message, so a
// refusal from an unrelated rule cannot satisfy these tests.
func mustFailWith(t *testing.T, ctx context.Context, tx pgx.Tx, want, sql string, args ...any) {
	t.Helper()
	savepointCounter++
	sp := fmt.Sprintf("sp_%d", savepointCounter)
	if _, err := tx.Exec(ctx, "SAVEPOINT "+sp); err != nil {
		t.Fatalf("create savepoint: %v", err)
	}
	_, err := tx.Exec(ctx, sql, args...)
	if err == nil {
		t.Fatalf("expected refusal containing %q, statement succeeded", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("expected refusal containing %q, got: %v", want, err)
	}
	if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT "+sp); err != nil {
		t.Fatalf("rollback to savepoint: %v", err)
	}
}

// execOne runs an allowed write and requires exactly one affected row, so an
// "allowed" case cannot pass by matching nothing and skipping the trigger.
func execOne(t *testing.T, ctx context.Context, tx pgx.Tx, what, sql string, args ...any) {
	t.Helper()
	tag, err := tx.Exec(ctx, sql, args...)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("%s: rows=%d err=%v", what, tag.RowsAffected(), err)
	}
}

// flaggedRouteHead returns a transaction holding a route head that is flagged
// (reconciliation_required) with no active release, and the environment id.
func flaggedRouteHead(t *testing.T, ctx context.Context) (pgx.Tx, string) {
	t.Helper()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f := buildReleaseFixture(t, ctx, tx)
	insertRouteHead(t, ctx, tx, f.environmentID, "app-"+randomSuffix(t)+".example.com")
	execOne(t, ctx, tx, "set flag alone", `UPDATE route_heads SET reconciliation_required=true, updated_at=clock_timestamp() WHERE environment_id=$1`, f.environmentID)
	return tx, f.environmentID
}

// Migration 000027: a route head with reconciliation_required set (and no
// active release, an ambiguous first activation) cannot change hostname.

func TestRouteHeadFlaggedHostnameChangeRefused(t *testing.T) {
	ctx := context.Background()
	tx, env := flaggedRouteHead(t, ctx)
	mustFailWith(t, ctx, tx, flaggedHostnameMessage,
		`UPDATE route_heads SET hostname=$2, route_revision=route_revision+1, updated_at=clock_timestamp() WHERE environment_id=$1`, env, "other-"+randomSuffix(t)+".example.com")
}

func TestRouteHeadFlaggedClearAndHostnameChangeRefused(t *testing.T) {
	ctx := context.Background()
	tx, env := flaggedRouteHead(t, ctx)
	mustFailWith(t, ctx, tx, flaggedHostnameMessage,
		`UPDATE route_heads SET hostname=$2, reconciliation_required=false, route_revision=route_revision+1, updated_at=clock_timestamp() WHERE environment_id=$1`, env, "other-"+randomSuffix(t)+".example.com")
}

// TestRouteHeadFlagMovesAndUnflaggedHostnameStillAllowed proves 000027 does
// not over-reach: the flag moves alone in both directions at an unchanged
// revision, and an unflagged head without an active release can still change
// hostname with revision+1.
func TestRouteHeadFlagMovesAndUnflaggedHostnameStillAllowed(t *testing.T) {
	ctx := context.Background()
	tx, env := flaggedRouteHead(t, ctx) // set-flag-alone already succeeded
	execOne(t, ctx, tx, "clear flag alone", `UPDATE route_heads SET reconciliation_required=false, updated_at=clock_timestamp() WHERE environment_id=$1`, env)
	execOne(t, ctx, tx, "unflagged hostname change", `UPDATE route_heads SET hostname=$2, route_revision=route_revision+1, updated_at=clock_timestamp() WHERE environment_id=$1`, env, "other-"+randomSuffix(t)+".example.com")
}

// TestRouteHeadOldOnlyRuleAllowsFlagAndHostnameTogether pins the owner's
// OLD-only decision: on an unflagged head, one statement that sets the flag and
// changes hostname (revision+1) is not refused by 000027, mirroring the app
// check, which looks at the stored flag only.
func TestRouteHeadOldOnlyRuleAllowsFlagAndHostnameTogether(t *testing.T) {
	ctx := context.Background()
	tx, env := flaggedRouteHead(t, ctx)
	execOne(t, ctx, tx, "clear flag alone", `UPDATE route_heads SET reconciliation_required=false, updated_at=clock_timestamp() WHERE environment_id=$1`, env)
	execOne(t, ctx, tx, "set flag and change hostname together", `UPDATE route_heads SET hostname=$2, reconciliation_required=true, route_revision=route_revision+1, updated_at=clock_timestamp() WHERE environment_id=$1`, env, "other-"+randomSuffix(t)+".example.com")
}

// TestRouteHeadHostnameActiveReleaseMessageUnchanged keeps the 000018 rule's
// own message distinct from the flagged-head one.
func TestRouteHeadHostnameActiveReleaseMessageUnchanged(t *testing.T) {
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
	if _, err := tx.Exec(ctx, `UPDATE route_heads SET active_release_id=$2, route_revision=route_revision+1 WHERE environment_id=$1`, f.environmentID, releaseID); err != nil {
		t.Fatalf("activate: %v", err)
	}
	mustFailWith(t, ctx, tx, "route head hostname cannot change while a release is active",
		`UPDATE route_heads SET hostname='changed-'||hostname, route_revision=route_revision+1, updated_at=clock_timestamp() WHERE environment_id=$1`, f.environmentID)
}
