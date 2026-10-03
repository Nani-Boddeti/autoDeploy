//go:build integration

package postgres

import (
	"context"
	"regexp"
	"testing"
)

// TestUnresolvedActivationSQLAgreesWithGo evaluates the SQL fragment against
// every state the table's CHECK allows and compares with the Go predicate. The
// state list is read from the live constraint, so a state added by a later
// migration fails here until the predicate is reviewed for it.
func TestUnresolvedActivationSQLAgreesWithGo(t *testing.T) {
	pool := newLeaseFixture(t, 0xC1).pool
	ctx := context.Background()

	var def string
	if err := pool.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname='release_operations_state_check' AND conrelid='release_operations'::regclass`).Scan(&def); err != nil {
		t.Fatal(err)
	}
	states := regexp.MustCompile(`'([a-z_]+)'`).FindAllStringSubmatch(def, -1)
	if len(states) != 5 {
		t.Fatalf("release_operations_state_check changed (%q); review unresolvedActivation for the new state", def)
	}
	for _, m := range states {
		state := m[1]
		var sqlUnresolved bool
		q := `SELECT EXISTS (SELECT 1 FROM (VALUES ('activate', $1::text)) AS t(kind, state) WHERE ` + unresolvedActivationSQL + `)`
		if err := pool.QueryRow(ctx, q, state).Scan(&sqlUnresolved); err != nil {
			t.Fatal(err)
		}
		if sqlUnresolved != unresolvedActivation(state) {
			t.Errorf("state %q: SQL=%v Go=%v", state, sqlUnresolved, unresolvedActivation(state))
		}
	}
	// A non-activate kind is never an unresolved activation.
	var other bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM (VALUES ('deploy','started')) AS t(kind, state) WHERE `+unresolvedActivationSQL+`)`).Scan(&other); err != nil {
		t.Fatal(err)
	}
	if other {
		t.Error("non-activate kind matched the unresolved-activation fragment")
	}
}

// TestOpenActivationIndexPredicateAgreesWithGo ties migration 000021's
// hand-written unique-index predicate to the Go-generated fragment. The
// predicate is read from the live catalog and both forms are evaluated by
// Postgres over the same VALUES rows, for every state the table's CHECK
// allows, so neither textual normalization (NOT IN becomes <> ALL) nor a
// shared Go slice can hide a divergence.
func TestOpenActivationIndexPredicateAgreesWithGo(t *testing.T) {
	pool := newLeaseFixture(t, 0xC2).pool
	ctx := context.Background()

	const idx = "release_operations_environment_open_activation_idx"
	var unique bool
	var table string
	var cols []string
	var pred *string
	err := pool.QueryRow(ctx, `
SELECT i.indisunique, i.indrelid::regclass::text,
       ARRAY(SELECT a.attname::text FROM unnest(i.indkey::int2[]) WITH ORDINALITY k(attnum, ord) JOIN pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=k.attnum ORDER BY k.ord),
       pg_get_expr(i.indpred, i.indrelid)
FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid WHERE c.relname=$1`, idx).Scan(&unique, &table, &cols, &pred)
	if err != nil {
		t.Fatalf("read index %s: %v", idx, err)
	}
	if !unique {
		t.Error("index is not UNIQUE")
	}
	if table != "release_operations" {
		t.Errorf("index table = %q, want release_operations", table)
	}
	if len(cols) != 1 || cols[0] != "environment_id" {
		t.Errorf("index columns = %v, want [environment_id]", cols)
	}
	if pred == nil || *pred == "" {
		t.Fatal("index is not partial (no predicate)")
	}

	var def string
	if err := pool.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname='release_operations_state_check' AND conrelid='release_operations'::regclass`).Scan(&def); err != nil {
		t.Fatal(err)
	}
	states := regexp.MustCompile(`'([a-z_]+)'`).FindAllStringSubmatch(def, -1)
	if len(states) != 5 {
		t.Fatalf("release_operations_state_check changed (%q); review the index predicate", def)
	}
	eval := func(where, kind, state string) bool {
		var got bool
		q := `SELECT EXISTS (SELECT 1 FROM (VALUES ($1::text, $2::text)) AS t(kind, state) WHERE ` + where + `)`
		if err := pool.QueryRow(ctx, q, kind, state).Scan(&got); err != nil {
			t.Fatalf("evaluate %q: %v", where, err)
		}
		return got
	}
	for _, kind := range []string{"activate", "deploy"} {
		for _, m := range states {
			state := m[1]
			if idxGot, goGot := eval(*pred, kind, state), eval(unresolvedActivationSQL, kind, state); idxGot != goGot {
				t.Errorf("kind %q state %q: index predicate=%v Go fragment=%v (index predicate %q)", kind, state, idxGot, goGot, *pred)
			}
		}
	}
}
