//go:build integration

package postgres

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"autodeploy/internal/githubapp"

	"github.com/jackc/pgx/v5"
)

const lockOrderSHA = "0123456789abcdef0123456789abcdef01234567"

// lockOrderConn opens a dedicated connection that is always closed on cleanup.
func lockOrderConn(t *testing.T) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), os.Getenv("AUTODEPLOY_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

func lockOrderTx(t *testing.T) pgx.Tx {
	t.Helper()
	tx, err := lockOrderConn(t).Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

type lockOrderCase struct {
	name        string
	reservation bool
	// prepare advances state and returns the operation under test.
	prepare func(t *testing.T, repo *GitHubRepository, b githubapp.CheckoutBinding) (id string, op func() error, wantState string)
}

func lockOrderReserve(t *testing.T, repo *GitHubRepository, b githubapp.CheckoutBinding) string {
	t.Helper()
	res, err := repo.ReserveCheckout(context.Background(), b.OwnerID, b.ProjectID, lockOrderSHA)
	if err != nil {
		t.Fatal(err)
	}
	return res.ID
}

func lockOrderCases() []lockOrderCase {
	ctx := context.Background()
	return []lockOrderCase{
		{"ReserveCheckout", false, func(t *testing.T, repo *GitHubRepository, b githubapp.CheckoutBinding) (string, func() error, string) {
			return "", func() error {
				_, err := repo.ReserveCheckout(ctx, b.OwnerID, b.ProjectID, lockOrderSHA)
				return err
			}, ""
		}},
		{"BeginMintAttempt", true, func(t *testing.T, repo *GitHubRepository, b githubapp.CheckoutBinding) (string, func() error, string) {
			id := lockOrderReserve(t, repo, b)
			return id, func() error { _, err := repo.BeginMintAttempt(ctx, id); return err }, "minting"
		}},
		{"ActivateMintAttempt", true, func(t *testing.T, repo *GitHubRepository, b githubapp.CheckoutBinding) (string, func() error, string) {
			id := lockOrderReserve(t, repo, b)
			got, err := repo.BeginMintAttempt(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			return id, func() error { return repo.ActivateMintAttempt(ctx, id, got) }, "active"
		}},
		{"StartCheckout", true, func(t *testing.T, repo *GitHubRepository, b githubapp.CheckoutBinding) (string, func() error, string) {
			id := lockOrderReserve(t, repo, b)
			got, err := repo.BeginMintAttempt(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if err = repo.ActivateMintAttempt(ctx, id, got); err != nil {
				t.Fatal(err)
			}
			return id, func() error { return repo.StartCheckout(ctx, id, got) }, "running"
		}},
		{"RevalidateCheckoutBinding", false, func(t *testing.T, repo *GitHubRepository, b githubapp.CheckoutBinding) (string, func() error, string) {
			return "", func() error { return repo.RevalidateCheckoutBinding(ctx, b) }, ""
		}},
	}
}

// waitBlockedBy polls until some backend waits on the given pid.
func waitBlockedBy(t *testing.T, tx pgx.Tx, pid int32) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		if err := tx.Conn().QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))`, pid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n >= 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("operation never blocked behind the registration lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// startBlocked runs op in a goroutine; the returned func joins it. On cleanup
// the blocker tx is released before joining, so an early t.Fatal cannot leave
// the op waiting behind it for the whole join timeout.
func startBlocked(t *testing.T, op func() error, blocker pgx.Tx) func() error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- op() }()
	joined := false
	var result error
	join := func() error {
		if !joined {
			select {
			case result = <-done:
				joined = true
			case <-time.After(20 * time.Second):
				t.Error("operation did not finish")
				return errors.New("timeout")
			}
		}
		return result
	}
	t.Cleanup(func() {
		_ = blocker.Rollback(context.Background())
		_ = join()
	})
	return join
}

func TestCheckoutLockOrderHoldsNothingBelowRegistration(t *testing.T) {
	for _, tc := range lockOrderCases() {
		t.Run(tc.name, func(t *testing.T) {
			repo, b := qaCheckoutBinding(t)
			id, op, wantState := tc.prepare(t, repo, b)
			ctx := context.Background()
			tx1 := lockOrderTx(t)
			var pid int32
			if err := tx1.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				t.Fatal(err)
			}
			if _, err := tx1.Exec(ctx, `SELECT 1 FROM github_app_registrations WHERE id=1 FOR SHARE`); err != nil {
				t.Fatal(err)
			}
			join := startBlocked(t, op, tx1)
			tx3 := lockOrderTx(t)
			waitBlockedBy(t, tx3, pid)
			probes := []struct {
				name, query string
				args        []any
			}{
				{"installation", `SELECT 1 FROM github_installations WHERE installation_id=$1 FOR UPDATE NOWAIT`, []any{b.InstallationID}},
				{"repository", `SELECT 1 FROM github_repositories WHERE repository_id=$1 FOR UPDATE NOWAIT`, []any{b.RepositoryID}},
				{"binding", `SELECT 1 FROM github_project_bindings WHERE owner_id=$1 AND project_id=$2 FOR UPDATE NOWAIT`, []any{b.OwnerID, b.ProjectID}},
			}
			if tc.reservation {
				probes = append(probes, struct {
					name, query string
					args        []any
				}{"reservation", `SELECT 1 FROM github_checkout_reservations WHERE id=$1 FOR UPDATE NOWAIT`, []any{id}})
			}
			for _, p := range probes {
				if _, err := tx3.Exec(ctx, `SAVEPOINT p`); err != nil {
					t.Fatal(err)
				}
				tag, err := tx3.Exec(ctx, p.query, p.args...)
				if err != nil {
					t.Errorf("%s row locked while op waits on registration: %v", p.name, err)
					if _, rerr := tx3.Exec(ctx, `ROLLBACK TO SAVEPOINT p`); rerr != nil {
						t.Fatal(rerr)
					}
				} else if tag.RowsAffected() != 1 {
					t.Errorf("%s probe locked %d rows, want 1 (vacuous probe)", p.name, tag.RowsAffected())
				}
			}
			// Probe locks must be released before the op can proceed.
			if err := tx3.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			if err := tx1.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			if err := join(); err != nil {
				t.Fatalf("op after release: %v", err)
			}
			if id != "" {
				var state string
				if err := repo.pool.QueryRow(ctx, `SELECT state FROM github_checkout_reservations WHERE id=$1`, id).Scan(&state); err != nil || state != wantState {
					t.Fatalf("state=%q err=%v want %q", state, err, wantState)
				}
			}
		})
	}
}

// A DisableInstallation-style writer holding registration FOR UPDATE must make
// a queued checkout deny cleanly (no 40P01, no hang) once it commits.
func TestCheckoutLockOrderDeniesAfterDisableWriter(t *testing.T) {
	repo, b := qaCheckoutBinding(t)
	ctx := context.Background()
	id := lockOrderReserve(t, repo, b)
	tx1 := lockOrderTx(t)
	var pid int32
	if err := tx1.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := tx1.Exec(ctx, `SELECT 1 FROM github_app_registrations WHERE id=1 FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	join := startBlocked(t, func() error { _, err := repo.BeginMintAttempt(ctx, id); return err }, tx1)
	waitBlockedBy(t, tx1, pid)
	if _, err := tx1.Exec(ctx, `UPDATE github_installations SET authorization_revision=authorization_revision+1 WHERE installation_id=$1`, b.InstallationID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx1.Exec(ctx, `UPDATE github_checkout_reservations SET state='invalidated' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err := tx1.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := join(); !errors.Is(err, ErrGitHubDenied) {
		t.Fatalf("err=%v want ErrGitHubDenied", err)
	}
}

// StartCheckout must reject a passed binding whose revisions differ from the
// reservation's own columns even when they match live authority.
func TestCheckoutLockOrderStartCheckoutCrossChecksReservation(t *testing.T) {
	repo, b := qaCheckoutBinding(t)
	ctx := context.Background()
	id := lockOrderReserve(t, repo, b)
	got, err := repo.BeginMintAttempt(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err = repo.ActivateMintAttempt(ctx, id, got); err != nil {
		t.Fatal(err)
	}
	if _, err = repo.pool.Exec(ctx, `UPDATE github_checkout_reservations SET binding_revision=binding_revision+1 WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err = repo.StartCheckout(ctx, id, got); !errors.Is(err, ErrGitHubDenied) {
		t.Fatalf("err=%v want ErrGitHubDenied", err)
	}
}

// revalidateTx must deny a binding with unknown (zero) ids rather than learn
// them: ActivateMintAttempt's update does not re-check installation/repository.
func TestCheckoutLockOrderRevalidateDeniesZeroIDs(t *testing.T) {
	repo, b := qaCheckoutBinding(t)
	ctx := context.Background()
	id := lockOrderReserve(t, repo, b)
	got, err := repo.BeginMintAttempt(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	zero := got
	zero.InstallationID, zero.RepositoryID = 0, 0
	if err = repo.ActivateMintAttempt(ctx, id, zero); !errors.Is(err, ErrGitHubDenied) {
		t.Fatalf("err=%v want ErrGitHubDenied", err)
	}
	if err = repo.ActivateMintAttempt(ctx, id, got); err != nil {
		t.Fatalf("matching binding after zero-id denial: %v", err)
	}
}

// lockOrderRepointReservation binds a second project of the same owner to a
// copy of the fixture repository and moves the reservation's (owner, project)
// to it, so the reservation no longer belongs to the passed binding while that
// binding's authority stays live.
func lockOrderRepointReservation(t *testing.T, repo *GitHubRepository, b githubapp.CheckoutBinding, id string) {
	t.Helper()
	ctx := context.Background()
	tx, err := repo.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, `CREATE TEMP TABLE lo_repo ON COMMIT DROP AS SELECT * FROM github_repositories WHERE owner_id=$1 AND repository_id=$2`, b.OwnerID, b.RepositoryID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `CREATE TEMP TABLE lo_bind ON COMMIT DROP AS SELECT * FROM github_project_bindings WHERE owner_id=$1 AND project_id=$2`, b.OwnerID, b.ProjectID); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`UPDATE lo_repo SET repository_id=repository_id+1000`,
		`INSERT INTO github_repositories SELECT * FROM lo_repo`,
		`UPDATE lo_bind SET project_id='qa_other_project', repository_id=repository_id+1000`,
		`INSERT INTO github_project_bindings SELECT * FROM lo_bind`,
	} {
		if _, err = tx.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE github_checkout_reservations SET project_id='qa_other_project' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// StartCheckout and ActivateMintAttempt must deny a reservation that belongs
// to another project of the same owner, even with equal ids and revisions.
func TestCheckoutLockOrderReservationMustBelongToBinding(t *testing.T) {
	t.Run("StartCheckout", func(t *testing.T) {
		repo, b := qaCheckoutBinding(t)
		ctx := context.Background()
		id := lockOrderReserve(t, repo, b)
		got, err := repo.BeginMintAttempt(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if err = repo.ActivateMintAttempt(ctx, id, got); err != nil {
			t.Fatal(err)
		}
		lockOrderRepointReservation(t, repo, got, id)
		if err = repo.StartCheckout(ctx, id, got); !errors.Is(err, ErrGitHubDenied) {
			t.Fatalf("err=%v want ErrGitHubDenied", err)
		}
	})
	t.Run("ActivateMintAttempt", func(t *testing.T) {
		repo, b := qaCheckoutBinding(t)
		ctx := context.Background()
		id := lockOrderReserve(t, repo, b)
		got, err := repo.BeginMintAttempt(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		lockOrderRepointReservation(t, repo, got, id)
		if err = repo.ActivateMintAttempt(ctx, id, got); !errors.Is(err, ErrGitHubDenied) {
			t.Fatalf("err=%v want ErrGitHubDenied", err)
		}
	})
}
