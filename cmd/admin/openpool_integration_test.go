//go:build integration

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"testing"

	"autodeploy/migrations"

	"github.com/jackc/pgx/v5"
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
