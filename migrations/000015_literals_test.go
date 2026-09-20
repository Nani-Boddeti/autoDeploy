//go:build integration

package migrations

import (
	"context"
	"testing"
)

// TestEnvironmentRuntimeBindingLiteralPresence proves migration 000015's
// literal_value CHECKs: the value is required exactly when the binding kind
// is literal_nonsecret, forbidden otherwise, bounded, and free of control
// characters (a literal reaches container environment injection and operator
// display, so a stray NUL or newline is an injection/log-corruption vector).
func TestEnvironmentRuntimeBindingLiteralPresence(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	envID := "env-literal-" + randomSuffix(t)

	// A literal binding with a value is accepted.
	if _, err := tx.Exec(ctx, `INSERT INTO environment_runtime_bindings(environment_id, variable_name, binding_kind, delivery_scope, literal_value)
	      VALUES ($1,'NODE_ENV','literal_nonsecret','runtime','production')`, envID); err != nil {
		t.Fatalf("insert literal binding: %v", err)
	}

	// A literal binding without a value is rejected.
	mustFail(t, ctx, tx, "expected rejection of literal binding with NULL value",
		`INSERT INTO environment_runtime_bindings(environment_id, variable_name, binding_kind, delivery_scope)
		 VALUES ($1,'MISSING_VALUE','literal_nonsecret','runtime')`, envID)

	// A control character in a value is rejected.
	mustFail(t, ctx, tx, "expected rejection of control character in literal value",
		`INSERT INTO environment_runtime_bindings(environment_id, variable_name, binding_kind, delivery_scope, literal_value)
		 VALUES ($1,'BAD_VALUE','literal_nonsecret','runtime',$2)`, envID, "line\nbreak")

	// An empty value is rejected: absent and empty must not be conflated.
	mustFail(t, ctx, tx, "expected rejection of empty literal value",
		`INSERT INTO environment_runtime_bindings(environment_id, variable_name, binding_kind, delivery_scope, literal_value)
		 VALUES ($1,'EMPTY_VALUE','literal_nonsecret','runtime','')`, envID)

	// Over the 4096-byte bound is rejected.
	long := make([]byte, 4097)
	for i := range long {
		long[i] = 'x'
	}
	mustFail(t, ctx, tx, "expected rejection of oversized literal value",
		`INSERT INTO environment_runtime_bindings(environment_id, variable_name, binding_kind, delivery_scope, literal_value)
		 VALUES ($1,'LONG_VALUE','literal_nonsecret','runtime',$2)`, envID, string(long))

	// A secret binding may not carry a literal value.
	f := buildReleaseFixture(t, ctx, tx)
	mustFail(t, ctx, tx, "expected rejection of secret binding carrying a literal value",
		`INSERT INTO environment_runtime_bindings(environment_id, variable_name, binding_kind, secret_head_id, delivery_scope, literal_value)
		 VALUES ($1,'BOTH','secret',$2,'runtime','oops')`, f.environmentID, f.secretHeadID)
}

// TestFrozenLiteralValuesAppendOnly proves the frozen literal rows are as
// immutable as frozen_secret_versions: an attempt's executed configuration
// cannot be rewritten after the fact.
func TestFrozenLiteralValuesAppendOnly(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	tx := beginTx(t, ctx, conn)
	f := buildReleaseFixture(t, ctx, tx)

	if _, err := tx.Exec(ctx, `INSERT INTO frozen_literal_values(attempt_id, variable_name, delivery_scope, literal_value)
	      VALUES ($1,'NODE_ENV','runtime','production')`, f.attemptID); err != nil {
		t.Fatalf("insert frozen literal: %v", err)
	}
	mustFail(t, ctx, tx, "expected update rejection",
		`UPDATE frozen_literal_values SET literal_value='staging' WHERE attempt_id=$1`, f.attemptID)
	mustFail(t, ctx, tx, "expected delete rejection",
		`DELETE FROM frozen_literal_values WHERE attempt_id=$1`, f.attemptID)
	// One value per variable per attempt.
	mustFail(t, ctx, tx, "expected duplicate variable rejection",
		`INSERT INTO frozen_literal_values(attempt_id, variable_name, delivery_scope, literal_value)
		 VALUES ($1,'NODE_ENV','runtime','staging')`, f.attemptID)
}
