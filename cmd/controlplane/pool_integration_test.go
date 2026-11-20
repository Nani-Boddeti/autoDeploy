//go:build integration

package main

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The pool's lock_timeout turns a wedged advisory lock into a transient 55P03
// instead of an unbounded wait, and statement_timeout into 57014.
func TestControlPlanePoolTimeoutsApplyServerSide(t *testing.T) {
	dsn := os.Getenv("AUTODEPLOY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AUTODEPLOY_TEST_DATABASE_URL is required for PostgreSQL integration tests")
	}
	ctx := context.Background()
	cfg, err := controlPlanePoolConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["lock_timeout"] = "300"
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = "500"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx) //nolint:errcheck
	const key = 731492845631699999
	if _, err = holder.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, key); err != nil {
		t.Fatal(err)
	}
	waiter, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer waiter.Rollback(ctx) //nolint:errcheck
	start := time.Now()
	_, err = waiter.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, key)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" || time.Since(start) > 5*time.Second {
		t.Fatalf("err = %v after %v, want 55P03 promptly", err, time.Since(start))
	}

	_, err = pool.Exec(ctx, `SELECT pg_sleep(10)`)
	if !errors.As(err, &pgErr) || pgErr.Code != "57014" {
		t.Fatalf("err = %v, want 57014", err)
	}
}
