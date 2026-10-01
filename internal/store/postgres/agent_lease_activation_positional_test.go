//go:build integration

package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// TestLockActiveServerCredentialTransientErrorIsNotDenial: a transient database
// error on the helper's first statement must stay retryable. The agent client
// never retries a 401, so reading it as ErrServerAuthorizationDenied silently
// drops a genuine success report. Genuine denials must still be denials.
func TestLockActiveServerCredentialTransientErrorIsNotDenial(t *testing.T) {
	f := newLeaseFixture(t, 210)
	cred := f.withCredential(t, 211)
	ctx := context.Background()

	check := func(t *testing.T, ctx context.Context, tx pgx.Tx, serverID string, d [32]byte) error {
		t.Helper()
		_, err := lockActiveServerCredential(ctx, tx, serverID, d)
		return err
	}
	begin := func(t *testing.T) pgx.Tx {
		t.Helper()
		tx, err := f.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tx.Rollback(ctx) })
		return tx
	}

	t.Run("healthy credential passes", func(t *testing.T) {
		if err := check(t, ctx, begin(t), f.serverID, cred); err != nil {
			t.Fatalf("err=%v, want nil", err)
		}
	})
	t.Run("canceled context is not a denial", func(t *testing.T) {
		tx := begin(t)
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		err := check(t, canceled, tx, f.serverID, cred)
		if err == nil || errors.Is(err, ErrServerAuthorizationDenied) || !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v, want a wrapped context.Canceled and not ErrServerAuthorizationDenied", err)
		}
	})
	t.Run("closed transaction is not a denial", func(t *testing.T) {
		tx := begin(t)
		_ = tx.Rollback(ctx)
		err := check(t, ctx, tx, f.serverID, cred)
		if err == nil || errors.Is(err, ErrServerAuthorizationDenied) {
			t.Fatalf("err=%v, want a non-denial error", err)
		}
	})
	t.Run("transient error on the credential statement is not a denial", func(t *testing.T) {
		// Server row is readable; the credential SELECT is the one that fails.
		tx := begin(t)
		if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout = '1ms'`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `SELECT pg_sleep(0.05)`); err == nil {
			t.Fatal("expected statement_timeout to fire")
		}
		err := check(t, ctx, tx, f.serverID, cred) // aborted transaction: 25P02 on the first statement
		var pgErr *pgconn.PgError
		if err == nil || errors.Is(err, ErrServerAuthorizationDenied) || !errors.As(err, &pgErr) {
			t.Fatalf("err=%v, want a wrapped PgError and not ErrServerAuthorizationDenied", err)
		}
	})
	t.Run("error text never carries the digest", func(t *testing.T) {
		tx := begin(t)
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		err := check(t, canceled, tx, f.serverID, cred)
		if err == nil || strings.Contains(err.Error(), fmt.Sprintf("%x", cred[:])) {
			t.Fatalf("err=%v must not carry credential material", err)
		}
	})
	t.Run("unknown server is denied", func(t *testing.T) {
		if err := check(t, ctx, begin(t), "no-such-server", cred); !errors.Is(err, ErrServerAuthorizationDenied) {
			t.Fatalf("err=%v, want ErrServerAuthorizationDenied", err)
		}
	})
	t.Run("wrong digest is denied", func(t *testing.T) {
		if err := check(t, ctx, begin(t), f.serverID, digest(99)); !errors.Is(err, ErrServerAuthorizationDenied) {
			t.Fatalf("err=%v, want ErrServerAuthorizationDenied", err)
		}
	})
	t.Run("revoked credential is denied", func(t *testing.T) {
		if _, err := f.pool.Exec(ctx, `UPDATE server_credentials SET status='revoked', revoked_at=clock_timestamp() WHERE server_id=$1`, f.serverID); err != nil {
			t.Fatal(err)
		}
		if err := check(t, ctx, begin(t), f.serverID, cred); !errors.Is(err, ErrServerAuthorizationDenied) {
			t.Fatalf("err=%v, want ErrServerAuthorizationDenied", err)
		}
	})
	t.Run("revoked server is denied", func(t *testing.T) {
		if _, err := f.pool.Exec(ctx, `UPDATE servers SET status='revoked', revoked_at=clock_timestamp() WHERE id=$1`, f.serverID); err != nil {
			t.Fatal(err)
		}
		if err := check(t, ctx, begin(t), f.serverID, cred); !errors.Is(err, ErrServerAuthorizationDenied) {
			t.Fatalf("err=%v, want ErrServerAuthorizationDenied", err)
		}
	})
}

// TestReportActivationResultClassificationIsPositional: a non-transient PgError
// raised by a PRE-occupant read/lock statement wrote nothing durable, so it
// must stay retryable (503); the very same error after the occupant check is
// terminal and leaves a recorded block.
func TestReportActivationResultClassificationIsPositional(t *testing.T) {
	ctx := context.Background()
	pgErrors := map[string]error{
		"schema skew 42703": &pgconn.PgError{Code: "42703", Message: "column does not exist"},
		"privilege 42501":   &pgconn.PgError{Code: "42501"},
		"check 23514":       &pgconn.PgError{Code: "23514", ConstraintName: "injected_check"},
	}
	for _, stage := range []string{"attempt_peek", "slot_lock", "job_lock", "attempt_lock", "operation_lock"} {
		for name, injected := range pgErrors {
			t.Run("pre-occupant/"+stage+"/"+name, func(t *testing.T) {
				a := newActivatedFixture(t, 212, true)
				a.repo.failAfter = func(s string) error {
					if s == stage {
						return injected
					}
					return nil
				}
				err := a.report(ctx, true, "")
				if !errors.Is(err, injected) {
					t.Fatalf("err=%v, want the injected error", err)
				}
				if errors.Is(err, ErrAgentLeaseConstraintViolation) || errors.Is(err, ErrAgentLeaseCorruptState) {
					t.Fatalf("err=%v must stay retryable (unclassified): nothing durable was written", err)
				}
				a.assertNothingRecorded(t)
				a.repo.failAfter = nil
				if err := a.report(ctx, true, ""); err != nil {
					t.Fatalf("retry: %v", err)
				}
				a.assertFinalized(t, 2)
			})
		}
	}
	for name, injected := range pgErrors {
		t.Run("post-occupant/"+name, func(t *testing.T) {
			a := newActivatedFixture(t, 213, true)
			a.repo.failAfter = func(s string) error {
				if s == "deployment_lock" {
					return injected
				}
				return nil
			}
			err := a.report(ctx, true, "")
			if !errors.Is(err, ErrAgentLeaseConstraintViolation) {
				t.Fatalf("err=%v, want terminal ErrAgentLeaseConstraintViolation", err)
			}
			a.assertBlocked(t, 1, a.predecessorID, "activation finalize failed")
		})
	}
}

// TestReportActivationResultUnrecordedBlockWrapsInnerSentinelRetryably: when
// writing the block itself fails with an error that matches a terminal
// sentinel (here ErrAgentLeaseCorruptState at operation_close), the result
// must carry ErrActivationBlockUnrecorded -- which Handler.mapError tests first
// and answers 503 -- and nothing durable may exist.
func TestReportActivationResultUnrecordedBlockWrapsInnerSentinelRetryably(t *testing.T) {
	ctx := context.Background()
	a := newActivatedFixture(t, 214, true)
	a.repo.failAfter = func(s string) error {
		if s == "operation_close" {
			return fmt.Errorf("injected: %w", ErrAgentLeaseCorruptState)
		}
		return nil
	}
	err := a.report(ctx, false, "host reported failure")
	if !errors.Is(err, ErrActivationBlockUnrecorded) {
		t.Fatalf("err=%v, want ErrActivationBlockUnrecorded", err)
	}
	if !errors.Is(err, ErrAgentLeaseCorruptState) {
		t.Fatalf("err=%v: the inner sentinel is what makes mapError ordering load-bearing", err)
	}
	a.assertNothingRecorded(t)
}
