//go:build integration

package migrations

import (
	"context"
	"testing"
)

// TestSetupTokenRuntimeGrants pins the deliberately narrow 000032 grants: the
// runtime role may read, insert, and update only the lifecycle columns; it may
// never delete, truncate, or rewrite the digest/issued_at.
func TestSetupTokenRuntimeGrants(t *testing.T) {
	ctx := context.Background()
	conn := connectAndApply(t)
	check := func(privilege string, want bool) {
		t.Helper()
		var got bool
		if err := conn.QueryRow(ctx, `SELECT has_table_privilege('autodeploy_app','public.installation_setup_tokens',$1)`, privilege).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("table privilege %s = %v, want %v", privilege, got, want)
		}
	}
	check("SELECT", true)
	check("INSERT", true)
	check("UPDATE", false) // table-wide UPDATE is withheld; only columns below
	check("DELETE", false)
	check("TRUNCATE", false)
	check("TRIGGER", false)
	for column, want := range map[string]bool{"consumed_at": true, "expires_at": true, "digest": false, "issued_at": false, "id": false} {
		var got bool
		if err := conn.QueryRow(ctx, `SELECT has_column_privilege('autodeploy_app','public.installation_setup_tokens',$1,'UPDATE')`, column).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("column UPDATE on %s = %v, want %v", column, got, want)
		}
	}
}
