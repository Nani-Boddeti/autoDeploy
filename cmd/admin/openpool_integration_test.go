//go:build integration

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"autodeploy/internal/auth"
	"autodeploy/internal/store/postgres"
	"autodeploy/migrations"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// scratchRuntimeDSN builds a fresh database, migrates it as the superuser test
// connection and returns a DSN that logs in as a LOGIN member of autodeploy_app
// (the installer's runtime credential shape) plus the superuser DSN for it.
func scratchRuntimeDSN(t *testing.T) (runtimeDSN string, adminConn *pgx.Conn) {
	t.Helper()
	ctx := context.Background()
	base := os.Getenv("AUTODEPLOY_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("AUTODEPLOY_TEST_DATABASE_URL not set")
	}
	super, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	suffix := hex.EncodeToString(buf)
	dbName, role := "adm_"+suffix, "adm_login_"+suffix
	if _, err := super.Exec(ctx, `CREATE DATABASE `+dbName); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = super.Exec(context.Background(), `DROP DATABASE IF EXISTS `+dbName+` WITH (FORCE)`)
		_, _ = super.Exec(context.Background(), `DROP ROLE IF EXISTS `+role)
		_ = super.Close(context.Background())
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + dbName
	migrator, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = migrator.Close(context.Background()) })
	if err := migrations.Apply(ctx, migrator); err != nil {
		t.Fatal(err)
	}
	// The role exists (created by 000030, NOLOGIN); add a LOGIN member of it.
	if _, err := super.Exec(ctx, `CREATE ROLE `+role+` LOGIN PASSWORD 'pw' IN ROLE autodeploy_app`); err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(role, "pw")
	return u.String(), migrator
}

func TestOpenPoolAsRuntimeRoleWhenSchemaCurrent(t *testing.T) {
	ctx := context.Background()
	dsn, _ := scratchRuntimeDSN(t)
	pool, err := openPool(ctx, dsn)
	if err != nil {
		t.Fatalf("openPool as runtime role on a current schema: %v", err)
	}
	defer pool.Close()
	var one int
	if err := pool.QueryRow(ctx, `SELECT 1`).Scan(&one); err != nil || one != 1 {
		t.Fatalf("pool unusable: %v", err)
	}
}

func TestOpenPoolRefusesWhenMigrationMissingOrChanged(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct{ name, sql string }{
		{"row missing", `DELETE FROM schema_migrations WHERE name LIKE '000030%'`},
		{"checksum differs", `UPDATE schema_migrations SET checksum='bad' WHERE name LIKE '000029%'`},
		{"ledger absent", `DROP TABLE schema_migrations`},
	} {
		dsn, owner := scratchRuntimeDSN(t) // fresh database per case
		if _, err := owner.Exec(ctx, tc.sql); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		pool, err := openPool(ctx, dsn)
		if pool != nil {
			pool.Close()
		}
		if !errors.Is(err, errSchemaNotCurrent) {
			t.Fatalf("%s: want errSchemaNotCurrent, got %v", tc.name, err)
		}
		if err.Error() != "database schema is not current; run autodeploy-run migrate" {
			t.Fatalf("message %q", err.Error())
		}
	}
}

// TestSetupTokenPathsWorkAsRuntimeRole runs mint, pre-check, bootstrap and the
// retire-on-remint update as the least-privilege runtime role (000032 grants
// only SELECT, INSERT and UPDATE(consumed_at, expires_at)), and asserts the
// withheld privileges really are withheld.
func TestSetupTokenPathsWorkAsRuntimeRole(t *testing.T) {
	ctx := context.Background()
	dsn, _ := scratchRuntimeDSN(t)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	repo := postgres.NewAuthRepository(pool)
	mint := func(seed byte) [32]byte {
		token, tokenErr := auth.NewSetupToken(bytes.NewReader(bytes.Repeat([]byte{seed}, 32)))
		if tokenErr != nil {
			t.Fatal(tokenErr)
		}
		digest, _ := auth.SetupTokenDigest(token)
		if err = repo.IssueSetupToken(ctx, digest, time.Hour); err != nil {
			t.Fatalf("mint as runtime role: %v", err)
		}
		return digest
	}
	old, current := mint(1), mint(2) // second mint retires the first (UPDATE expires_at)
	if live, err := repo.SetupCodeLive(ctx, old); err != nil || live {
		t.Fatalf("retired code live=%v err=%v", live, err)
	}
	if live, err := repo.SetupCodeLive(ctx, current); err != nil || !live {
		t.Fatalf("current code live=%v err=%v", live, err)
	}
	policy := auth.Argon2Policy{Revision: 1, MemoryKiB: 19 * 1024, Iterations: 2, Lanes: 1}
	verifier, err := auth.HashPassword(strings.Repeat("p", 15), policy, bytes.NewReader(bytes.Repeat([]byte{9}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	in := postgres.BootstrapInput{OwnerID: "owner-rt", UserID: "user-rt", Username: "administrator", AuditID: "audit-rt", Verifier: verifier, Policy: policy}
	if err = repo.BootstrapWithSetupToken(ctx, current, in); err != nil {
		t.Fatalf("bootstrap as runtime role: %v", err)
	}
	for name, statement := range map[string]string{
		"delete":         `DELETE FROM installation_setup_tokens`,
		"rewrite digest": `UPDATE installation_setup_tokens SET digest = digest`,
		"truncate":       `TRUNCATE installation_setup_tokens`,
	} {
		_, execErr := pool.Exec(ctx, statement)
		var pgErr *pgconn.PgError
		if !errors.As(execErr, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("%s: want insufficient_privilege, got %v", name, execErr)
		}
	}
}
