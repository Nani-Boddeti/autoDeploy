//go:build integration

package postgres

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"autodeploy/internal/auth"

	"github.com/jackc/pgx/v5/pgxpool"
)

// setupFixture resets the installation to the pre-bootstrap state (no owner,
// no setup codes) and returns the repository plus a calibrated test policy.
func setupFixture(t *testing.T) (*pgxpool.Pool, *AuthRepository, auth.Argon2Policy) {
	t.Helper()
	pool := authIntegrationPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `TRUNCATE installation_setup_tokens, auth_audit_events, admin_sessions, auth_throttle_buckets, auth_username_throttles, auth_throttle_state, users, installation_auth, owners RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO installation_auth(id) VALUES(1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO auth_throttle_state(id) VALUES(1)`); err != nil {
		t.Fatal(err)
	}
	return pool, NewAuthRepository(pool), auth.Argon2Policy{Revision: 1, MemoryKiB: 19 * 1024, Iterations: 2, Lanes: 1}
}

func mintSetupCode(t *testing.T, repo *AuthRepository, seed byte) (auth.SetupToken, [32]byte) {
	t.Helper()
	token, err := auth.NewSetupToken(bytes.NewReader(bytes.Repeat([]byte{seed}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	digest, err := auth.SetupTokenDigest(token)
	if err != nil {
		t.Fatal(err)
	}
	if err = repo.IssueSetupToken(context.Background(), digest, time.Hour); err != nil {
		t.Fatal(err)
	}
	return token, digest
}

func setupInput(t *testing.T, policy auth.Argon2Policy, username, suffix string) BootstrapInput {
	t.Helper()
	verifier, err := auth.HashPassword(strings.Repeat("p", 15), policy, bytes.NewReader(bytes.Repeat([]byte{21}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	return BootstrapInput{OwnerID: "owner-" + suffix, UserID: "user-" + suffix, Username: username, AuditID: "audit-" + suffix, Verifier: verifier, Policy: policy}
}

func setupCount(t *testing.T, pool *pgxpool.Pool, query string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestBootstrapWithSetupTokenSuccess(t *testing.T) {
	pool, repo, policy := setupFixture(t)
	ctx := context.Background()
	_, digest := mintSetupCode(t, repo, 1)
	if err := repo.BootstrapWithSetupToken(ctx, digest, setupInput(t, policy, "administrator", "ok")); err != nil {
		t.Fatal(err)
	}
	user, err := repo.FindLoginUserByCanonicalUsername(ctx, "administrator")
	if err != nil || !user.Active {
		t.Fatalf("user=%+v err=%v", user, err)
	}
	if got, err := repo.LoadPasswordPolicy(ctx); err != nil || got != policy {
		t.Fatalf("policy=%+v err=%v", got, err)
	}
	if n := setupCount(t, pool, `SELECT count(*) FROM installation_setup_tokens WHERE consumed_at IS NOT NULL`); n != 1 {
		t.Fatalf("consumed tokens = %d", n)
	}
	if n := setupCount(t, pool, `SELECT count(*) FROM auth_audit_events WHERE action='bootstrap_web' AND result='success'`); n != 1 {
		t.Fatalf("bootstrap_web audit rows = %d", n)
	}
	if n := setupCount(t, pool, `SELECT count(*) FROM auth_audit_events WHERE action='bootstrap'`); n != 0 {
		t.Fatalf("terminal bootstrap audit rows = %d", n)
	}
}

func TestBootstrapWithSetupTokenRefusesBadCodes(t *testing.T) {
	pool, repo, policy := setupFixture(t)
	ctx := context.Background()
	live, liveDigest := mintSetupCode(t, repo, 2)
	_ = live
	unknown, err := auth.NewSetupToken(bytes.NewReader(bytes.Repeat([]byte{3}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	unknownDigest, _ := auth.SetupTokenDigest(unknown)
	if err = repo.BootstrapWithSetupToken(ctx, unknownDigest, setupInput(t, policy, "administrator", "wrong")); !errors.Is(err, ErrSetupCodeInvalid) {
		t.Fatalf("wrong code = %v", err)
	}
	// Expired: a live code whose lifetime has passed. Shorten it through the
	// only permitted mutation (retirement) rather than touching the clock.
	if _, err = pool.Exec(ctx, `UPDATE installation_setup_tokens SET expires_at = issued_at + interval '1 microsecond' WHERE digest=$1`, liveDigest[:]); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if err = repo.BootstrapWithSetupToken(ctx, liveDigest, setupInput(t, policy, "administrator", "expired")); !errors.Is(err, ErrSetupCodeInvalid) {
		t.Fatalf("expired code = %v", err)
	}
	if n := setupCount(t, pool, `SELECT count(*) FROM users`) + setupCount(t, pool, `SELECT count(*) FROM owners`); n != 0 {
		t.Fatalf("refused attempts created %d identity rows", n)
	}
	// Consumed: use a fresh code once, then replay it after a terminal reset of
	// the owner is impossible, so replay against the same (already bootstrapped)
	// installation must fail as already-bootstrapped, and a consumed code must
	// still be refused as invalid when the installation is somehow unbootstrapped.
	_, digest := mintSetupCode(t, repo, 4)
	if err = repo.BootstrapWithSetupToken(ctx, digest, setupInput(t, policy, "administrator", "first")); err != nil {
		t.Fatal(err)
	}
	if err = repo.BootstrapWithSetupToken(ctx, digest, setupInput(t, policy, "other-admin", "replay")); !errors.Is(err, ErrAlreadyBootstrapped) {
		t.Fatalf("replay after bootstrap = %v", err)
	}
	// Reset to pre-bootstrap keeping the consumed token row: it must stay unusable.
	if _, err = pool.Exec(ctx, `TRUNCATE auth_audit_events, admin_sessions, users, installation_auth, owners CASCADE`); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO installation_auth(id) VALUES(1)`); err != nil {
		t.Fatal(err)
	}
	if err = repo.BootstrapWithSetupToken(ctx, digest, setupInput(t, policy, "administrator", "consumed")); !errors.Is(err, ErrSetupCodeInvalid) {
		t.Fatalf("consumed code = %v", err)
	}
	if n := setupCount(t, pool, `SELECT count(*) FROM owners`); n != 0 {
		t.Fatalf("consumed-code attempt created %d owners", n)
	}
}

func TestBootstrapWithSetupTokenRefusedAfterTerminalBootstrap(t *testing.T) {
	_, repo, policy := setupFixture(t)
	ctx := context.Background()
	_, digest := mintSetupCode(t, repo, 5)
	if err := repo.Bootstrap(ctx, setupInput(t, policy, "administrator", "terminal")); err != nil {
		t.Fatal(err)
	}
	if err := repo.BootstrapWithSetupToken(ctx, digest, setupInput(t, policy, "second-admin", "web")); !errors.Is(err, ErrAlreadyBootstrapped) {
		t.Fatalf("web bootstrap after terminal bootstrap = %v", err)
	}
	if err := repo.IssueSetupToken(ctx, digest, time.Hour); !errors.Is(err, ErrAlreadyBootstrapped) {
		t.Fatalf("minting after bootstrap = %v", err)
	}
}

func TestIssueSetupTokenRetiresOlderCodes(t *testing.T) {
	_, repo, policy := setupFixture(t)
	ctx := context.Background()
	_, oldDigest := mintSetupCode(t, repo, 6)
	_, newDigest := mintSetupCode(t, repo, 7)
	if err := repo.BootstrapWithSetupToken(ctx, oldDigest, setupInput(t, policy, "administrator", "old")); !errors.Is(err, ErrSetupCodeInvalid) {
		t.Fatalf("superseded code = %v", err)
	}
	if err := repo.BootstrapWithSetupToken(ctx, newDigest, setupInput(t, policy, "administrator", "new")); err != nil {
		t.Fatalf("newest code = %v", err)
	}
}

func TestIssueSetupTokenRejectsBadTTL(t *testing.T) {
	_, repo, _ := setupFixture(t)
	if err := repo.IssueSetupToken(context.Background(), [32]byte{1}, 0); err == nil {
		t.Fatal("zero TTL accepted")
	}
}

// TestBootstrapWithSetupTokenConcurrentDoubleSubmit forces real contention on
// the installation_auth row lock: a third connection holds the lock until both
// goroutines are blocked on it (observed in pg_stat_activity), then releases.
func TestBootstrapWithSetupTokenConcurrentDoubleSubmit(t *testing.T) {
	pool, repo, policy := setupFixture(t)
	ctx := context.Background()
	_, digest := mintSetupCode(t, repo, 8)
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx) //nolint:errcheck
	if _, err = holder.Exec(ctx, `SELECT 1 FROM installation_auth WHERE id=1 FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i, name := range []string{"first-admin", "second-admin"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- repo.BootstrapWithSetupToken(ctx, digest, setupInput(t, policy, name, "race-"+string(rune('a'+i))))
		}()
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		if n := setupCount(t, pool, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query ILIKE '%installation_auth%FOR UPDATE%' AND pid<>pg_backend_pid()`); n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("both submissions never blocked on the installation_auth lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err = holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	close(results)
	successes, refused := 0, 0
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrAlreadyBootstrapped):
			refused++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if successes != 1 || refused != 1 {
		t.Fatalf("successes=%d refused=%d", successes, refused)
	}
	if n := setupCount(t, pool, `SELECT count(*) FROM users`); n != 1 {
		t.Fatalf("users = %d", n)
	}
	if n := setupCount(t, pool, `SELECT count(*) FROM installation_setup_tokens WHERE consumed_at IS NOT NULL`); n != 1 {
		t.Fatalf("consumed tokens = %d", n)
	}
}

func TestSetupTokenTriggersProtectRows(t *testing.T) {
	pool, repo, policy := setupFixture(t)
	ctx := context.Background()
	_, digest := mintSetupCode(t, repo, 9)
	if _, err := pool.Exec(ctx, `UPDATE installation_setup_tokens SET expires_at = expires_at + interval '1 hour' WHERE digest=$1`, digest[:]); err == nil {
		t.Fatal("lifetime extension accepted")
	}
	if _, err := pool.Exec(ctx, `UPDATE installation_setup_tokens SET digest=$2 WHERE digest=$1`, digest[:], bytes.Repeat([]byte{9}, 32)); err == nil {
		t.Fatal("digest rewrite accepted")
	}
	if err := repo.BootstrapWithSetupToken(ctx, digest, setupInput(t, policy, "administrator", "trig")); err != nil {
		t.Fatal(err)
	}
	for name, statement := range map[string]string{
		"unconsume": `UPDATE installation_setup_tokens SET consumed_at = NULL`,
		"touch":     `UPDATE installation_setup_tokens SET expires_at = expires_at - interval '1 microsecond'`,
		"delete":    `DELETE FROM installation_setup_tokens`,
	} {
		if _, err := pool.Exec(ctx, statement); err == nil {
			t.Fatalf("%s accepted on a consumed setup token", name)
		}
	}
}

// waitForSetupLockWaiters blocks until n other backends are waiting on a row lock
// of installation_auth (observed in pg_stat_activity), proving real contention.
func waitForSetupLockWaiters(t *testing.T, pool *pgxpool.Pool, n int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for setupCount(t, pool, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query ILIKE '%installation_auth%FOR UPDATE%' AND pid<>pg_backend_pid()`) < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d submissions never blocked on the installation_auth lock", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A failure after the code was consumed (duplicate owner id) must roll the
// consume back: the code stays usable and no administrator exists.
func TestBootstrapWithSetupTokenFailureAfterConsumeRollsBack(t *testing.T) {
	pool, repo, policy := setupFixture(t)
	ctx := context.Background()
	_, digest := mintSetupCode(t, repo, 10)
	if _, err := pool.Exec(ctx, `INSERT INTO owners(id) VALUES('owner-dup')`); err != nil {
		t.Fatal(err)
	}
	in := setupInput(t, policy, "administrator", "dup")
	in.OwnerID = "owner-dup" // the owners insert runs after the consume and fails
	err := repo.BootstrapWithSetupToken(ctx, digest, in)
	if err == nil || errors.Is(err, ErrSetupCodeInvalid) || errors.Is(err, ErrAlreadyBootstrapped) {
		t.Fatalf("want a duplicate-key failure, got %v", err)
	}
	if n := setupCount(t, pool, `SELECT count(*) FROM installation_setup_tokens WHERE consumed_at IS NOT NULL`); n != 0 {
		t.Fatalf("consume was not rolled back: %d consumed", n)
	}
	if n := setupCount(t, pool, `SELECT count(*) FROM users`) + setupCount(t, pool, `SELECT count(*) FROM installation_auth WHERE owner_id IS NOT NULL`); n != 0 {
		t.Fatalf("partial administrator created: %d rows", n)
	}
	if live, err := repo.SetupCodeLive(ctx, digest); err != nil || !live {
		t.Fatalf("code no longer live after rollback: %v %v", live, err)
	}
	if err = repo.BootstrapWithSetupToken(ctx, digest, setupInput(t, policy, "administrator", "retry")); err != nil {
		t.Fatalf("retry with the same code: %v", err)
	}
}

// Minting and bootstrapping serialize on the installation_auth lock. Whichever
// reaches the lock first wins, and the loser sees a consistent state: a mint
// that wins retires the old code (bootstrap then refuses it); a bootstrap that
// wins closes setup (mint then refuses). Both orders are forced by starting the
// second goroutine only once the first is already waiting.
func TestIssueSetupTokenRacesBootstrapWithRealContention(t *testing.T) {
	for _, mintFirst := range []bool{true, false} {
		name := "bootstrap first"
		if mintFirst {
			name = "mint first"
		}
		t.Run(name, func(t *testing.T) {
			pool, repo, policy := setupFixture(t)
			ctx := context.Background()
			_, oldDigest := mintSetupCode(t, repo, 11)
			newToken, err := auth.NewSetupToken(bytes.NewReader(bytes.Repeat([]byte{12}, 32)))
			if err != nil {
				t.Fatal(err)
			}
			newDigest, _ := auth.SetupTokenDigest(newToken)
			holder, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer holder.Rollback(ctx) //nolint:errcheck
			if _, err = holder.Exec(ctx, `SELECT 1 FROM installation_auth WHERE id=1 FOR UPDATE`); err != nil {
				t.Fatal(err)
			}
			mintErr, bootErr := make(chan error, 1), make(chan error, 1)
			runMint := func() { mintErr <- repo.IssueSetupToken(ctx, newDigest, time.Hour) }
			runBoot := func() {
				bootErr <- repo.BootstrapWithSetupToken(ctx, oldDigest, setupInput(t, policy, "administrator", "race"))
			}
			first, second := runBoot, runMint
			if mintFirst {
				first, second = runMint, runBoot
			}
			go first()
			waitForSetupLockWaiters(t, pool, 1)
			go second()
			waitForSetupLockWaiters(t, pool, 2)
			if err = holder.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			mErr, bErr := <-mintErr, <-bootErr
			users := setupCount(t, pool, `SELECT count(*) FROM users`)
			if mintFirst {
				if mErr != nil || !errors.Is(bErr, ErrSetupCodeInvalid) || users != 0 {
					t.Fatalf("mint=%v bootstrap=%v users=%d", mErr, bErr, users)
				}
				if live, _ := repo.SetupCodeLive(ctx, newDigest); !live {
					t.Fatal("new code not live after winning mint")
				}
			} else {
				if bErr != nil || !errors.Is(mErr, ErrAlreadyBootstrapped) || users != 1 {
					t.Fatalf("mint=%v bootstrap=%v users=%d", mErr, bErr, users)
				}
				if n := setupCount(t, pool, `SELECT count(*) FROM installation_setup_tokens`); n != 1 {
					t.Fatalf("a losing mint left %d token rows", n)
				}
			}
		})
	}
}
