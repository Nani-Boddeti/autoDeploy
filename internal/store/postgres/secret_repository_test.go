//go:build integration

package postgres

import (
	"context"
	"errors"
	"sync"
	"testing"

	"autodeploy/internal/crypto"
	"autodeploy/internal/secret"
)

func freshSecretRepository(t *testing.T) (*SecretRepository, *KeyRepository) {
	t.Helper()
	pool := authIntegrationPool(t)
	if _, err := pool.Exec(context.Background(), `TRUNCATE secret_audit_events, secret_versions, secret_heads, secret_wrap_invocations RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	keys := NewKeyRepository(pool)
	var generation string
	if err := pool.QueryRow(context.Background(), `SELECT id FROM secret_database_generation WHERE singleton=true`).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"kek-1", "kek-old", "kek-new"} {
		if err := keys.InitializeActiveLedger(context.Background(), id, generation); err != nil {
			t.Fatal(err)
		}
	}
	return NewSecretRepository(pool), keys
}
func secretMutation(id, version, audit string) secret.Mutation {
	s := secret.Scope{OwnerID: "owner", ProjectID: "project", EnvironmentID: "environment", SecretID: id, Purpose: "runtime", DeliveryScope: "runtime"}
	return secret.Mutation{Version: secret.Version{ID: version, Scope: s, Number: 1, State: secret.StateActive, Envelope: crypto.Envelope{Suite: crypto.SuiteAES256GCMRandomNonceV1, AADVersion: crypto.AADFormatV1, KEKID: "kek-1", Ciphertext: make([]byte, 28), WrappedDEK: make([]byte, 60)}}, Audit: secret.Audit{ID: audit, OwnerID: s.OwnerID, SecretID: id, VersionID: version, Action: "created", ActorID: "actor", Result: "success", KEKID: "kek-1"}}
}
func TestSecretLedgerBoundaryAndConcurrentReservations(t *testing.T) {
	_, keys := freshSecretRepository(t)
	ctx := context.Background()
	var generation string
	if err := keys.pool.QueryRow(ctx, `SELECT id FROM secret_database_generation WHERE singleton=true`).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	if err := keys.InitializeActiveLedger(ctx, "limit", generation); err != nil {
		t.Fatal(err)
	}
	if _, err := keys.pool.Exec(ctx, `UPDATE secret_wrap_invocations SET reserved_invocations=2147483646 WHERE kek_id='limit'`); err != nil {
		t.Fatal(err)
	}
	if err := keys.ReserveWrapInvocation("limit"); err != nil {
		t.Fatal(err)
	}
	var limitCount int64
	if err := keys.pool.QueryRow(ctx, `SELECT reserved_invocations FROM secret_wrap_invocations WHERE kek_id='limit'`).Scan(&limitCount); err != nil || limitCount != 2147483647 {
		t.Fatalf("limit count=%d err=%v", limitCount, err)
	}
	if err := keys.ReserveWrapInvocation("limit"); !errors.Is(err, ErrWrapInvocationExhausted) {
		t.Fatalf("limit=%v", err)
	}
	if err := keys.InitializeActiveLedger(ctx, "concurrent", generation); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- keys.ReserveWrapInvocation("concurrent") }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var count int64
	if err := keys.pool.QueryRow(ctx, `SELECT reserved_invocations FROM secret_wrap_invocations WHERE kek_id='concurrent'`).Scan(&count); err != nil || count != 20 {
		t.Fatalf("count=%d err=%v", count, err)
	}
}

func TestSecretLedgerRequiresExplicitBoundInitialization(t *testing.T) {
	_, keys := freshSecretRepository(t)
	ctx := context.Background()
	if _, err := keys.pool.Exec(ctx, `DELETE FROM secret_wrap_invocations WHERE kek_id='fresh-kek'`); err != nil {
		t.Fatal(err)
	}
	if err := keys.ReserveWrapInvocation("fresh-kek"); !errors.Is(err, ErrWrapInvocationExhausted) {
		t.Fatalf("reservation before init = %v", err)
	}
	var rows int
	if err := keys.pool.QueryRow(ctx, `SELECT count(*) FROM secret_wrap_invocations WHERE kek_id='fresh-kek'`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("missing ledger created rows=%d err=%v", rows, err)
	}
	var generation string
	if err := keys.pool.QueryRow(ctx, `SELECT id FROM secret_database_generation WHERE singleton=true`).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	if err := keys.InitializeActiveLedger(ctx, "fresh-kek", generation); err != nil {
		t.Fatal(err)
	}
	if err := keys.ReserveWrapInvocation("fresh-kek"); err != nil {
		t.Fatal(err)
	}
	if err := keys.InitializeActiveLedger(ctx, "fresh-kek", generation); !errors.Is(err, ErrKeyLedgerContinuity) {
		t.Fatalf("duplicate init = %v", err)
	}
	if err := keys.InitializeActiveLedger(ctx, "other-fresh-kek", "mismatched-generation"); !errors.Is(err, ErrKeyLedgerContinuity) {
		t.Fatalf("mismatched init = %v", err)
	}
	var count int64
	if err := keys.pool.QueryRow(ctx, `SELECT reserved_invocations FROM secret_wrap_invocations WHERE kek_id='fresh-kek'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("ledger reset count=%d err=%v", count, err)
	}
}
func TestSecretCreateAtomicIsolationAndImmutability(t *testing.T) {
	repo, keys := freshSecretRepository(t)
	ctx := context.Background()
	m := secretMutation("secret-1", "version-1", "audit-1")
	if err := keys.ReserveWrapInvocation("kek-1"); err != nil {
		t.Fatal(err)
	}
	if err := repo.Create(ctx, m); err != nil {
		t.Fatal(err)
	}
	other := m.Version.Scope
	other.OwnerID = "other"
	if _, err := repo.GetVersion(ctx, other, ""); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("isolation=%v", err)
	}
	if _, err := repo.pool.Exec(ctx, `UPDATE secret_versions SET ciphertext='x' WHERE id='version-1'`); err == nil {
		t.Fatal("ciphertext mutation accepted")
	}
	if _, err := repo.pool.Exec(ctx, `UPDATE secret_audit_events SET action='revoked' WHERE id='audit-1'`); err == nil {
		t.Fatal("audit mutation accepted")
	}
	// Keep the already-persisted setup audit valid; reject only the audit row
	// written by the mutation under test.
	if _, err := repo.pool.Exec(ctx, `ALTER TABLE secret_audit_events ADD CONSTRAINT reject_secret_audit CHECK (id <> 'audit-r')`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = repo.pool.Exec(context.Background(), `ALTER TABLE secret_audit_events DROP CONSTRAINT IF EXISTS reject_secret_audit`)
	})
	m2 := secretMutation("secret-rollback", "version-r", "audit-r")
	if err := repo.Create(ctx, m2); err == nil {
		t.Fatal("audit failure accepted")
	}
	var heads int
	if err := repo.pool.QueryRow(ctx, `SELECT count(*) FROM secret_heads WHERE id='secret-rollback'`).Scan(&heads); err != nil || heads != 0 {
		t.Fatalf("atomic heads=%d err=%v", heads, err)
	}
}
func TestSecretReplacementSupersedesPriorVersion(t *testing.T) {
	repo, _ := freshSecretRepository(t)
	ctx := context.Background()
	m := secretMutation("secret-1", "version-1", "audit-1")
	if err := repo.Create(ctx, m); err != nil {
		t.Fatal(err)
	}
	next := secretMutation("secret-1", "version-2", "audit-2")
	next.Version.Number = 2
	next.Audit.Action = "replaced"
	next.Audit.OldVersionID = "version-1"
	if err := repo.Replace(ctx, next); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := repo.pool.QueryRow(ctx, `SELECT state FROM secret_versions WHERE id='version-1'`).Scan(&state); err != nil || state != "superseded" {
		t.Fatalf("state=%s err=%v", state, err)
	}
	got, err := repo.GetVersion(ctx, next.Version.Scope, "")
	if err != nil || got.ID != "version-2" {
		t.Fatalf("head=%#v err=%v", got, err)
	}
}
