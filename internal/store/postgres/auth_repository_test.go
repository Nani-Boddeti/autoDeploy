//go:build integration

package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"autodeploy/internal/auth"
	"autodeploy/migrations"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func authIntegrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("AUTODEPLOY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AUTODEPLOY_TEST_DATABASE_URL is required for PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if err = migrations.Apply(ctx, conn); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func freshAuthRepository(t *testing.T) (*pgxpool.Pool, *AuthRepository, LoginUser, auth.PasswordVerifier) {
	t.Helper()
	pool := authIntegrationPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `TRUNCATE auth_audit_events, admin_sessions, auth_throttle_buckets, auth_username_throttles, auth_throttle_state, users, installation_auth, owners RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO installation_auth(id) VALUES(1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO auth_throttle_state(id) VALUES(1)`); err != nil {
		t.Fatal(err)
	}
	policy := auth.Argon2Policy{Revision: 1, MemoryKiB: 19 * 1024, Iterations: 2, Lanes: 1}
	verifier, err := auth.HashPassword(strings.Repeat("p", 15), policy, bytes.NewReader(bytes.Repeat([]byte{17}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	repo := NewAuthRepository(pool)
	if err = repo.Bootstrap(ctx, BootstrapInput{OwnerID: "owner-" + t.Name(), UserID: "user-" + t.Name(), Username: "administrator", AuditID: "audit-bootstrap-" + t.Name(), Verifier: verifier, Policy: policy}); err != nil {
		t.Fatal(err)
	}
	user, err := repo.FindLoginUserByCanonicalUsername(ctx, "administrator")
	if err != nil {
		t.Fatal(err)
	}
	return pool, repo, user, verifier
}

func testToken(t *testing.T, b byte) auth.SessionToken {
	t.Helper()
	token, err := auth.NewSessionToken(bytes.NewReader(bytes.Repeat([]byte{b}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestAuthBootstrapAndSessionPersistence(t *testing.T) {
	pool := authIntegrationPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `TRUNCATE auth_audit_events, admin_sessions, auth_throttle_buckets, auth_username_throttles, users, installation_auth, owners RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO installation_auth(id) VALUES(1)`); err != nil {
		t.Fatal(err)
	}
	policy := auth.Argon2Policy{Revision: 1, MemoryKiB: 19 * 1024, Iterations: 2, Lanes: 1}
	verifier, err := auth.HashPassword(strings.Repeat("p", 15), policy, bytes.NewReader(bytes.Repeat([]byte{1}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	repo := NewAuthRepository(pool)
	in := BootstrapInput{OwnerID: "owner-auth-test", UserID: "user-auth-test", Username: "administrator", AuditID: "audit-bootstrap", Verifier: verifier, Policy: policy}
	if err = repo.Bootstrap(ctx, in); err != nil {
		t.Fatal(err)
	}
	if err = repo.Bootstrap(ctx, in); !errors.Is(err, ErrAlreadyBootstrapped) {
		t.Fatalf("second bootstrap = %v", err)
	}
	loaded, err := repo.FindLoginUserByCanonicalUsername(ctx, "administrator")
	if err != nil || loaded.Verifier != verifier || !loaded.Active {
		t.Fatalf("loaded user=%+v err=%v", loaded, err)
	}
	token, err := auth.NewSessionToken(bytes.NewReader(bytes.Repeat([]byte{2}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repo.CreateSession(ctx, "session-auth-test", token, loaded, nil, "audit-login", "audit-login-limit"); err != nil {
		t.Fatal(err)
	}
	if _, err = repo.CreateSession(ctx, "session-audit-rollback", testToken(t, 3), loaded, nil, "audit-bootstrap", "audit-session-limit-rollback"); err == nil {
		t.Fatal("session creation accepted a duplicate audit id")
	}
	var persistedSessions int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM admin_sessions`).Scan(&persistedSessions); err != nil || persistedSessions != 1 {
		t.Fatalf("failed session audit did not roll back: sessions=%d err=%v", persistedSessions, err)
	}
	if _, err = repo.ValidateSession(ctx, token); err != nil {
		t.Fatal(err)
	}
	if err = repo.ResetPassword(ctx, loaded.ID, verifier, "audit-bootstrap", nil); err == nil {
		t.Fatal("reset accepted a duplicate audit id")
	}
	if _, err = repo.ValidateSession(ctx, token); err != nil {
		t.Fatalf("failed reset audit did not roll back revocation: %v", err)
	}
	var stored []byte
	if err = pool.QueryRow(ctx, `SELECT token_digest FROM admin_sessions WHERE id='session-auth-test'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored, []byte(token.String())) {
		t.Fatal("raw session token was stored")
	}
	if _, err := pool.Exec(ctx, `UPDATE auth_audit_events SET result='success' WHERE id='audit-login'`); err == nil {
		t.Fatal("audit update was accepted")
	}
	principal, err := repo.ValidateSession(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if err = repo.RevokeSession(ctx, token, principal, "audit-bootstrap"); err == nil {
		t.Fatal("logout accepted a duplicate audit id")
	}
	if _, err = repo.ValidateSession(ctx, token); err != nil {
		t.Fatalf("failed logout audit did not roll back revocation: %v", err)
	}
	if err = repo.RevokeSession(ctx, token, principal, "audit-logout"); err != nil {
		t.Fatal(err)
	}
	if _, err = repo.ValidateSession(ctx, token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("revoked session = %v", err)
	}
	if err = repo.ResetPassword(ctx, loaded.ID, verifier, "audit-reset", nil); err != nil {
		t.Fatal(err)
	}
	if _, err = repo.ValidateSession(ctx, token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("reset session = %v", err)
	}
}

func TestThrottlePersistenceLimitsAndCleanup(t *testing.T) {
	pool := authIntegrationPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `TRUNCATE auth_throttle_buckets, auth_username_throttles, auth_throttle_state`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO auth_throttle_state(id) VALUES(1)`); err != nil {
		t.Fatal(err)
	}
	repo := NewAuthRepository(pool)
	pair := ThrottleIdentity{Kind: ThrottlePair, KeyVersion: "v1", Digest: [32]byte{1}}
	for i := 0; i < 5; i++ {
		got, err := repo.ReserveThrottle(ctx, []ThrottleIdentity{pair}, 100)
		if err != nil || !got.Allowed {
			t.Fatalf("pair attempt %d: %+v %v", i, got, err)
		}
	}
	got, err := repo.ReserveThrottle(ctx, []ThrottleIdentity{pair}, 100)
	if err != nil || got.Allowed {
		t.Fatalf("pair limit: %+v %v", got, err)
	}
	ip := ThrottleIdentity{Kind: ThrottleIP, KeyVersion: "v1", Digest: [32]byte{2}}
	if got, err = repo.ReserveThrottle(ctx, []ThrottleIdentity{ip}, 100); err != nil || !got.Allowed {
		t.Fatalf("ip reservation: %+v %v", got, err)
	}
	if err = repo.FinalizeThrottle(ctx, true, []ThrottleIdentity{pair, ip}); err != nil {
		t.Fatal(err)
	}
	var pairRows, ipRows int
	if err = pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE kind='pair'),count(*) FILTER (WHERE kind='ip') FROM auth_throttle_buckets`).Scan(&pairRows, &ipRows); err != nil {
		t.Fatal(err)
	}
	if pairRows != 0 || ipRows != 1 {
		t.Fatalf("unexpected cleanup scope pair=%d ip=%d", pairRows, ipRows)
	}
	if _, err = repo.CleanupThrottle(ctx, 0); err == nil {
		t.Fatal("accepted invalid cleanup limit")
	}
}

func TestConcurrentBootstrapExactlyOne(t *testing.T) {
	pool := authIntegrationPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `TRUNCATE auth_audit_events, admin_sessions, users, installation_auth, owners RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO installation_auth(id) VALUES(1)`); err != nil {
		t.Fatal(err)
	}
	p := auth.Argon2Policy{Revision: 1, MemoryKiB: 19 * 1024, Iterations: 2, Lanes: 1}
	v, err := auth.HashPassword(strings.Repeat("p", 15), p, bytes.NewReader(bytes.Repeat([]byte{3}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results <- NewAuthRepository(pool).Bootstrap(ctx, BootstrapInput{OwnerID: "owner-concurrent", UserID: "user-concurrent", Username: "administrator", AuditID: "audit-concurrent-" + string(rune('a'+i)), Verifier: v, Policy: p})
		}(i)
	}
	wg.Wait()
	close(results)
	successes := 0
	already := 0
	for err := range results {
		if err == nil {
			successes++
		} else if errors.Is(err, ErrAlreadyBootstrapped) {
			already++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || already != 1 {
		t.Fatalf("success=%d already=%d", successes, already)
	}
}

func TestBootstrapAuditFailureRollsBackIdentity(t *testing.T) {
	pool := authIntegrationPool(t)
	ctx := context.Background()
	policy := auth.Argon2Policy{Revision: 1, MemoryKiB: 19 * 1024, Iterations: 2, Lanes: 1}
	verifier, err := auth.HashPassword(strings.Repeat("p", 15), policy, bytes.NewReader(bytes.Repeat([]byte{15}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE auth_audit_events, admin_sessions, auth_throttle_buckets, auth_username_throttles, auth_throttle_state, users, installation_auth, owners RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO installation_auth(id) VALUES(1)`,
		`INSERT INTO owners(id) VALUES('existing-owner')`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,owner_id,username,role,password_phc,password_policy_revision) VALUES('existing-user','existing-owner','existing','owner',$1,1)`, verifier.PHC); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO auth_audit_events(id,actor_user_id,owner_id,action,result) VALUES('duplicate-bootstrap-audit','existing-user','existing-owner','bootstrap','success')`); err != nil {
		t.Fatal(err)
	}
	err = NewAuthRepository(pool).Bootstrap(ctx, BootstrapInput{OwnerID: "rolled-owner", UserID: "rolled-user", Username: "rolled", AuditID: "duplicate-bootstrap-audit", Verifier: verifier, Policy: policy})
	if err == nil {
		t.Fatal("bootstrap accepted duplicate audit id")
	}
	var owners, users int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM owners WHERE id='rolled-owner'`).Scan(&owners); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE id='rolled-user'`).Scan(&users); err != nil {
		t.Fatal(err)
	}
	if owners != 0 || users != 0 {
		t.Fatalf("partial bootstrap persisted owners=%d users=%d", owners, users)
	}
}

func TestElevenSessionsRevokesOldest(t *testing.T) {
	pool := authIntegrationPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `TRUNCATE auth_audit_events, admin_sessions, users, installation_auth, owners RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO installation_auth(id) VALUES(1)`); err != nil {
		t.Fatal(err)
	}
	p := auth.Argon2Policy{Revision: 1, MemoryKiB: 19 * 1024, Iterations: 2, Lanes: 1}
	v, err := auth.HashPassword(strings.Repeat("p", 15), p, bytes.NewReader(bytes.Repeat([]byte{4}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	r := NewAuthRepository(pool)
	if err = r.Bootstrap(ctx, BootstrapInput{OwnerID: "owner-cap", UserID: "user-cap", Username: "administrator", AuditID: "audit-cap", Verifier: v, Policy: p}); err != nil {
		t.Fatal(err)
	}
	u, err := r.FindLoginUserByCanonicalUsername(ctx, "administrator")
	if err != nil {
		t.Fatal(err)
	}
	var first auth.SessionToken
	for i := 0; i < 11; i++ {
		token, e := auth.NewSessionToken(bytes.NewReader(bytes.Repeat([]byte{byte(i + 10)}, 32)))
		if e != nil {
			t.Fatal(e)
		}
		if i == 0 {
			first = token
		}
		if _, e = r.CreateSession(ctx, "session-cap-"+string(rune('a'+i)), token, u, nil, "audit-cap-"+string(rune('a'+i)), "audit-cap-limit-"+string(rune('a'+i))); e != nil {
			t.Fatal(e)
		}
	}
	var active int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM admin_sessions WHERE user_id='user-cap' AND revoked_at IS NULL`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 10 {
		t.Fatalf("active=%d", active)
	}
	var capAudits int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM auth_audit_events WHERE action='session_revoked' AND result='success'`).Scan(&capAudits); err != nil {
		t.Fatal(err)
	}
	if capAudits != 1 {
		t.Fatalf("session-limit audits=%d", capAudits)
	}
	if _, err = r.ValidateSession(ctx, first); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("oldest session=%v", err)
	}
}

func TestResetAndCreateSessionCannotCommitStaleAuthority(t *testing.T) {
	pool, repo, user, verifier := freshAuthRepository(t)
	ctx := context.Background()
	// Seed a session and every recovery bucket type. A reset may clear only supplied
	// pair/username digests, never independent IP evidence.
	old := testToken(t, 31)
	if _, err := repo.CreateSession(ctx, "session-before-reset", old, user, nil, "audit-login-before-reset", "audit-login-before-reset-limit"); err != nil {
		t.Fatal(err)
	}
	pair := ThrottleIdentity{Kind: ThrottlePair, KeyVersion: "v1", Digest: [32]byte{1}}
	username := ThrottleIdentity{Kind: ThrottleUsername, KeyVersion: "v1", Digest: [32]byte{2}}
	ip := ThrottleIdentity{Kind: ThrottleIP, KeyVersion: "v1", Digest: [32]byte{3}}
	for _, identity := range []ThrottleIdentity{pair, username, ip} {
		if got, err := repo.ReserveThrottle(ctx, []ThrottleIdentity{identity}, 100); err != nil || !got.Allowed {
			t.Fatalf("reserve %s: %#v %v", identity.Kind, got, err)
		}
	}
	stale := user
	created := testToken(t, 32)
	results := make(chan error, 2)
	go func() {
		_, err := NewAuthRepository(pool).CreateSession(ctx, "session-racing-reset", created, stale, nil, "audit-racing-login", "audit-racing-login-limit")
		results <- err
	}()
	go func() {
		results <- NewAuthRepository(pool).ResetPassword(ctx, user.ID, verifier, "audit-racing-reset", [][32]byte{pair.Digest, username.Digest})
	}()
	for range 2 {
		if err := <-results; err != nil && !errors.Is(err, ErrUnauthenticated) {
			t.Fatal(err)
		}
	}
	var revision, active, pairRows, usernameRows, ipRows int
	if err := pool.QueryRow(ctx, `SELECT auth_revision FROM users WHERE id=$1`, user.ID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM admin_sessions WHERE user_id=$1 AND revoked_at IS NULL`, user.ID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE kind='pair'), count(*) FILTER (WHERE kind='username'), count(*) FILTER (WHERE kind='ip') FROM auth_throttle_buckets`).Scan(&pairRows, &usernameRows, &ipRows); err != nil {
		t.Fatal(err)
	}
	if revision != 2 || active != 0 || pairRows != 0 || usernameRows != 0 || ipRows != 1 {
		t.Fatalf("revision=%d active=%d pair=%d username=%d ip=%d", revision, active, pairRows, usernameRows, ipRows)
	}
	if _, err := repo.ValidateSession(ctx, old); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("pre-reset session: %v", err)
	}
	if _, err := repo.ValidateSession(ctx, created); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("racing session: %v", err)
	}
}

func TestValidateSessionUsesDatabaseTimeAndUniformUnauthenticatedErrors(t *testing.T) {
	pool, repo, user, _ := freshAuthRepository(t)
	ctx := context.Background()
	newSession := func(id string, b byte) auth.SessionToken {
		token := testToken(t, b)
		if _, err := repo.CreateSession(ctx, id, token, user, nil, "audit-"+id, "audit-limit-"+id); err != nil {
			t.Fatal(err)
		}
		return token
	}
	noTouch := newSession("session-no-touch", 41)
	var before time.Time
	if err := pool.QueryRow(ctx, `SELECT last_seen_at FROM admin_sessions WHERE id='session-no-touch'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ValidateSession(ctx, noTouch); err != nil {
		t.Fatal(err)
	}
	var after time.Time
	if err := pool.QueryRow(ctx, `SELECT last_seen_at FROM admin_sessions WHERE id='session-no-touch'`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !after.Equal(before) {
		t.Fatalf("session touched before interval: %v -> %v", before, after)
	}

	touch := newSession("session-touch", 42)
	if _, err := pool.Exec(ctx, `UPDATE admin_sessions SET created_at=clock_timestamp()-interval '7 hours 59 minutes', last_seen_at=clock_timestamp()-interval '5 minutes', idle_expires_at=clock_timestamp()+interval '1 minute', absolute_expires_at=clock_timestamp()+interval '1 minute' WHERE id='session-touch'`); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ValidateSession(ctx, touch); err != nil {
		t.Fatal(err)
	}
	var idle, absolute time.Time
	if err := pool.QueryRow(ctx, `SELECT idle_expires_at,absolute_expires_at FROM admin_sessions WHERE id='session-touch'`).Scan(&idle, &absolute); err != nil {
		t.Fatal(err)
	}
	if idle.After(absolute) || absolute.Sub(idle) > time.Millisecond {
		t.Fatalf("touch exceeded absolute expiry: idle=%v absolute=%v", idle, absolute)
	}

	for _, tc := range []struct {
		id    string
		token auth.SessionToken
		sql   string
	}{
		{"session-idle-expired", newSession("session-idle-expired", 43), `UPDATE admin_sessions SET created_at=clock_timestamp()-interval '31 minutes', last_seen_at=clock_timestamp()-interval '31 minutes', idle_expires_at=clock_timestamp()-interval '1 second', absolute_expires_at=clock_timestamp()+interval '1 hour' WHERE id='session-idle-expired'`},
		{"session-absolute-expired", newSession("session-absolute-expired", 44), `WITH n AS (SELECT clock_timestamp() AS v) UPDATE admin_sessions SET created_at=n.v-interval '9 hours',last_seen_at=n.v-interval '2 seconds',idle_expires_at=n.v-interval '1 second',absolute_expires_at=n.v-interval '1 second' FROM n WHERE id='session-absolute-expired'`},
		{"session-revoked", newSession("session-revoked", 45), `UPDATE admin_sessions SET revoked_at=clock_timestamp(),revoked_reason='logout' WHERE id='session-revoked'`},
	} {
		if _, err := pool.Exec(ctx, tc.sql); err != nil {
			t.Fatalf("%s setup: %v", tc.id, err)
		}
		if _, err := repo.ValidateSession(ctx, tc.token); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("%s = %v", tc.id, err)
		}
	}
	// Composite ownership is enforced at the database boundary, making a
	// runtime cross-owner session structurally impossible.
	newSession("session-owner-mismatch", 47)
	if _, err := pool.Exec(ctx, `INSERT INTO owners(id) VALUES('owner-mismatch')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE admin_sessions SET owner_id='owner-mismatch' WHERE id='session-owner-mismatch'`); err == nil {
		t.Fatal("cross-owner session mutation was accepted")
	}
	stale := newSession("session-stale", 46)
	if _, err := pool.Exec(ctx, `UPDATE users SET auth_revision=auth_revision+1 WHERE id=$1`, user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ValidateSession(ctx, stale); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("stale revision = %v", err)
	}
}

func TestThrottleReservationsRetentionOverflowAndCleanup(t *testing.T) {
	pool, repo, _, _ := freshAuthRepository(t)
	ctx := context.Background()
	pair := ThrottleIdentity{Kind: ThrottlePair, KeyVersion: "current", Digest: [32]byte{61}, Aliases: []ThrottleAlias{{KeyVersion: "prior", Digest: [32]byte{62}}}}
	for i := 0; i < 5; i++ {
		if got, err := repo.ReserveThrottle(ctx, []ThrottleIdentity{pair}, 8); err != nil || !got.Allowed {
			t.Fatalf("pair %d: %#v %v", i, got, err)
		}
	}
	if got, err := repo.ReserveThrottle(ctx, []ThrottleIdentity{pair}, 8); err != nil || got.Allowed {
		t.Fatalf("pair cap: %#v %v", got, err)
	}
	if err := repo.FinalizeThrottle(ctx, false, []ThrottleIdentity{pair}); err != nil {
		t.Fatal(err)
	}
	var attempts int
	if err := pool.QueryRow(ctx, `SELECT attempts FROM auth_throttle_buckets WHERE kind='pair' AND key_version='current'`).Scan(&attempts); err != nil || attempts != 5 {
		t.Fatalf("unfinalized reservation attempts=%d err=%v", attempts, err)
	}
	if err := repo.FinalizeThrottle(ctx, true, []ThrottleIdentity{pair}); err != nil {
		t.Fatal(err)
	}
	// Four regular rows consume the regular capacity. The fifth unseen IP is
	// folded into the one shared IP overflow bucket.
	for i := 0; i < 4; i++ {
		identity := ThrottleIdentity{Kind: ThrottleIP, KeyVersion: "v1", Digest: [32]byte{byte(70 + i)}}
		if got, err := NewAuthRepository(pool).ReserveThrottle(ctx, []ThrottleIdentity{identity}, 8); err != nil || !got.Allowed {
			t.Fatalf("overflow %d: %#v %v", i, got, err)
		}
	}
	var rows, overflow int
	if err := pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE kind='ip_overflow') FROM auth_throttle_buckets`).Scan(&rows, &overflow); err != nil {
		t.Fatal(err)
	}
	if got, err := NewAuthRepository(pool).ReserveThrottle(ctx, []ThrottleIdentity{{Kind: ThrottleIP, KeyVersion: "v1", Digest: [32]byte{74}}}, 8); err != nil || !got.Allowed {
		t.Fatalf("initial overflow: %#v %v", got, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE kind='ip_overflow') FROM auth_throttle_buckets`).Scan(&rows, &overflow); err != nil {
		t.Fatal(err)
	}
	if rows > 8 || overflow != 1 {
		t.Fatalf("rows=%d overflow=%d", rows, overflow)
	}
	for i := 0; i < 19; i++ {
		identity := ThrottleIdentity{Kind: ThrottleIP, KeyVersion: "v1", Digest: [32]byte{byte(80 + i)}}
		if got, err := NewAuthRepository(pool).ReserveThrottle(ctx, []ThrottleIdentity{identity}, 8); err != nil || !got.Allowed {
			t.Fatalf("shared overflow attempt %d: %#v %v", i, got, err)
		}
	}
	if got, err := NewAuthRepository(pool).ReserveThrottle(ctx, []ThrottleIdentity{{Kind: ThrottleIP, KeyVersion: "v1", Digest: [32]byte{120}}}, 8); err != nil || got.Allowed {
		t.Fatalf("shared overflow cap: %#v %v", got, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE auth_throttle_buckets SET window_started_at=date_trunc('hour', clock_timestamp())-interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	deleted, err := repo.CleanupThrottle(ctx, 1)
	if err != nil || deleted != 1 {
		t.Fatalf("cleanup=%d err=%v", deleted, err)
	}
	if got, err := repo.ReserveThrottle(ctx, []ThrottleIdentity{{Kind: ThrottleIP, KeyVersion: "v2", Digest: [32]byte{99}}}, 8); err != nil || !got.Allowed {
		t.Fatalf("admission after delayed cleanup: %#v %v", got, err)
	}
}

func TestThrottleIPUsernameDelayAndRetainedAliases(t *testing.T) {
	pool, repo, _, _ := freshAuthRepository(t)
	ctx := context.Background()
	ip := ThrottleIdentity{Kind: ThrottleIP, KeyVersion: "v1", Digest: [32]byte{81}}
	for i := 0; i < 20; i++ {
		if got, err := repo.ReserveThrottle(ctx, []ThrottleIdentity{ip}, 100); err != nil || !got.Allowed {
			t.Fatalf("ip attempt %d: %#v %v", i, got, err)
		}
	}
	if got, err := repo.ReserveThrottle(ctx, []ThrottleIdentity{ip}, 100); err != nil || got.Allowed {
		t.Fatalf("ip cap: %#v %v", got, err)
	}
	username := ThrottleIdentity{Kind: ThrottleUsername, KeyVersion: "v1", Digest: [32]byte{82}}
	for i := 0; i < 10; i++ {
		if got, err := repo.ReserveThrottle(ctx, []ThrottleIdentity{username}, 100); err != nil || !got.Allowed {
			t.Fatalf("username failure %d: %#v %v", i, got, err)
		}
	}
	if got, err := repo.ReserveThrottle(ctx, []ThrottleIdentity{username}, 100); err != nil || got.Allowed || got.BlockedUntil.IsZero() {
		t.Fatalf("username delay: %#v %v", got, err)
	}
	prior := [32]byte{83}
	if _, err := pool.Exec(ctx, `INSERT INTO auth_throttle_buckets(kind,key_version,identifier_digest,window_started_at,attempts,failures) VALUES('pair','prior',$1,date_trunc('minute',clock_timestamp())-make_interval(mins => extract(minute from clock_timestamp())::int % 15),5,5)`, prior[:]); err != nil {
		t.Fatal(err)
	}
	retained := ThrottleIdentity{Kind: ThrottlePair, KeyVersion: "current", Digest: [32]byte{84}, Aliases: []ThrottleAlias{{KeyVersion: "prior", Digest: prior}}}
	if got, err := repo.ReserveThrottle(ctx, []ThrottleIdentity{retained}, 100); err != nil || got.Allowed {
		t.Fatalf("retained alias reset capacity: %#v %v", got, err)
	}
	var migratedAttempts, migratedAliases int
	if err := pool.QueryRow(ctx, `SELECT attempts FROM auth_throttle_buckets WHERE kind='pair' AND key_version='current' AND identifier_digest=$1`, retained.Digest[:]).Scan(&migratedAttempts); err != nil || migratedAttempts != 5 {
		t.Fatalf("retained alias migration attempts=%d err=%v", migratedAttempts, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM auth_throttle_buckets WHERE kind='pair' AND key_version='prior' AND identifier_digest=$1`, prior[:]).Scan(&migratedAliases); err != nil || migratedAliases != 0 {
		t.Fatalf("retained alias rows=%d err=%v", migratedAliases, err)
	}
	if err := repo.FinalizeThrottle(ctx, true, []ThrottleIdentity{retained}); err != nil {
		t.Fatal(err)
	}
	var aliases int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM auth_throttle_buckets WHERE kind='pair' AND identifier_digest IN ($1,$2)`, prior[:], retained.Digest[:]).Scan(&aliases); err != nil {
		t.Fatal(err)
	}
	if aliases != 0 {
		t.Fatalf("retained aliases not cleared: %d", aliases)
	}
}

func TestResetRecoveryDigestValidation(t *testing.T) {
	_, repo, user, verifier := freshAuthRepository(t)
	ctx := context.Background()
	valid := make([][32]byte, 10)
	for i := range valid {
		valid[i][0] = byte(i + 1)
	}
	if err := repo.ResetPassword(ctx, user.ID, verifier, "audit-reset-ten", valid); err != nil {
		t.Fatalf("ten recovery digests rejected: %v", err)
	}
	tooMany := append(valid, [32]byte{11})
	if err := repo.ResetPassword(ctx, user.ID, verifier, "audit-reset-too-many", tooMany); err == nil {
		t.Fatal("accepted more than ten recovery digests")
	}
	if err := repo.ResetPassword(ctx, user.ID, verifier, "audit-reset-duplicate", [][32]byte{{1}, {1}}); err == nil {
		t.Fatal("accepted duplicate recovery digest")
	}
	if err := repo.ResetPassword(ctx, user.ID, verifier, "audit-reset-zero", [][32]byte{{}}); err == nil {
		t.Fatal("accepted zero recovery digest")
	}
}

func TestResetPasswordClearsTaggedPairRecoveryBucketsOnly(t *testing.T) {
	pool, repo, user, verifier := freshAuthRepository(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,owner_id,username,role,password_phc,password_policy_revision) VALUES('other-reset-user',$1,'otherreset','owner',$2,1)`, user.OwnerID, verifier.PHC); err != nil {
		t.Fatal(err)
	}
	ownCurrent := ThrottleIdentity{Kind: ThrottlePair, KeyVersion: "current", Digest: [32]byte{31}, RecoveryUserID: user.ID}
	ownPrior := ThrottleIdentity{Kind: ThrottlePair, KeyVersion: "prior", Digest: [32]byte{32}, RecoveryUserID: user.ID}
	otherPair := ThrottleIdentity{Kind: ThrottlePair, KeyVersion: "current", Digest: [32]byte{33}, RecoveryUserID: "other-reset-user"}
	legacyPair := ThrottleIdentity{Kind: ThrottlePair, KeyVersion: "legacy", Digest: [32]byte{34}}
	for _, identity := range []ThrottleIdentity{ownCurrent, ownPrior, otherPair, legacyPair, {Kind: ThrottleIP, KeyVersion: "v1", Digest: [32]byte{35}}} {
		if reservation, err := repo.ReserveThrottle(ctx, []ThrottleIdentity{identity}, 100); err != nil || !reservation.Allowed {
			t.Fatalf("reserve %q: %#v %v", identity.Digest, reservation, err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO auth_throttle_buckets(kind,key_version,identifier_digest,window_started_at,attempts,failures) VALUES('pair_overflow','overflow',decode(repeat('00',32),'hex'),date_trunc('minute',clock_timestamp())-make_interval(mins => extract(minute from clock_timestamp())::int % 15),1,1)`); err != nil {
		t.Fatal(err)
	}
	if err := repo.ResetPassword(ctx, user.ID, verifier, "audit-reset-tagged", [][32]byte{legacyPair.Digest}); err != nil {
		t.Fatal(err)
	}
	var ownRows, otherRows, legacyRows, ipRows, overflowRows int
	if err := pool.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE kind='pair' AND recovery_user_id=$1),
		count(*) FILTER (WHERE kind='pair' AND recovery_user_id='other-reset-user'),
		count(*) FILTER (WHERE kind='pair' AND recovery_user_id IS NULL AND identifier_digest=$2),
		count(*) FILTER (WHERE kind='ip'),
		count(*) FILTER (WHERE kind='pair_overflow')
		FROM auth_throttle_buckets`, user.ID, legacyPair.Digest[:]).Scan(&ownRows, &otherRows, &legacyRows, &ipRows, &overflowRows); err != nil {
		t.Fatal(err)
	}
	if ownRows != 0 || otherRows != 1 || legacyRows != 0 || ipRows != 1 || overflowRows != 1 {
		t.Fatalf("own=%d other=%d legacy=%d ip=%d overflow=%d", ownRows, otherRows, legacyRows, ipRows, overflowRows)
	}
	if _, err := repo.ReserveThrottle(ctx, []ThrottleIdentity{{Kind: ThrottlePair, KeyVersion: otherPair.KeyVersion, Digest: otherPair.Digest, RecoveryUserID: user.ID}}, 100); err == nil {
		t.Fatal("cross-user pair digest collision was accepted")
	}
}

func TestResetPasswordSerializesWithThrottleReservationLock(t *testing.T) {
	pool, repo, user, verifier := freshAuthRepository(t)
	ctx := context.Background()
	pair := ThrottleIdentity{Kind: ThrottlePair, KeyVersion: "v1", Digest: [32]byte{39}, RecoveryUserID: user.ID}
	holder, err := pgx.Connect(ctx, os.Getenv("AUTODEPLOY_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { holder.Close(ctx) })
	if _, err = holder.Exec(ctx, `SELECT pg_advisory_lock(731492845631694123)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = holder.Exec(context.Background(), `SELECT pg_advisory_unlock(731492845631694123)`) })

	resetDone := make(chan error, 1)
	go func() { resetDone <- repo.ResetPassword(ctx, user.ID, verifier, "audit-reset-serialized", nil) }()
	for attempts := 0; ; attempts++ {
		var waiters int
		if err = pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND NOT granted`).Scan(&waiters); err != nil {
			t.Fatal(err)
		}
		if waiters > 0 {
			break
		}
		if attempts == 10000 {
			t.Fatal("reset did not wait for the throttle advisory lock")
		}
		runtime.Gosched()
	}
	reservationDone := make(chan error, 1)
	go func() {
		_, reserveErr := NewAuthRepository(pool).ReserveThrottle(ctx, []ThrottleIdentity{pair}, 100)
		reservationDone <- reserveErr
	}()
	for attempts := 0; ; attempts++ {
		var waiters int
		if err = pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND NOT granted`).Scan(&waiters); err != nil {
			t.Fatal(err)
		}
		if waiters >= 2 {
			break
		}
		if attempts == 10000 {
			t.Fatal("reservation did not wait behind the throttle advisory lock")
		}
		runtime.Gosched()
	}
	var rows int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM auth_throttle_buckets WHERE kind='pair' AND recovery_user_id=$1`, user.ID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("first-time tagged pair existed before receiving admission lock: %d", rows)
	}
	if _, err = holder.Exec(ctx, `SELECT pg_advisory_unlock(731492845631694123)`); err != nil {
		t.Fatal(err)
	}
	if err = <-resetDone; err != nil {
		t.Fatal(err)
	}
	if err = <-reservationDone; err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM auth_throttle_buckets WHERE kind='pair' AND recovery_user_id=$1`, user.ID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("reset then first-time tagged reservation ordering left rows=%d", rows)
	}
}

func TestCompleteLoginSerializesWithThrottleReservationAndReset(t *testing.T) {
	pool, repo, user, verifier := freshAuthRepository(t)
	ctx := context.Background()
	pair := ThrottleIdentity{Kind: ThrottlePair, KeyVersion: "v1", Digest: [32]byte{40}, RecoveryUserID: user.ID}
	username := ThrottleIdentity{Kind: ThrottleUsername, KeyVersion: "v1", Digest: [32]byte{41}}
	ip := ThrottleIdentity{Kind: ThrottleIP, KeyVersion: "v1", Digest: [32]byte{42}}
	holder, err := pgx.Connect(ctx, os.Getenv("AUTODEPLOY_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { holder.Close(ctx) })
	if _, err = holder.Exec(ctx, `SELECT pg_advisory_lock(731492845631694123)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = holder.Exec(context.Background(), `SELECT pg_advisory_unlock(731492845631694123)`) })
	loginDone := make(chan error, 1)
	go func() {
		_, loginErr := repo.CompleteLogin(ctx, CompleteLoginInput{SessionID: "complete-login-serialized", Token: testToken(t, 40), User: user, AuditID: "audit-complete-login-serialized", SessionLimitAuditID: "audit-complete-login-serialized-limit", ThrottleIdentities: []ThrottleIdentity{pair, username, ip}})
		loginDone <- loginErr
	}()
	resetDone := make(chan error, 1)
	go func() {
		resetDone <- NewAuthRepository(pool).ResetPassword(ctx, user.ID, verifier, "audit-complete-login-serialized-reset", nil)
	}()
	reservationDone := make(chan error, 1)
	go func() {
		_, reserveErr := NewAuthRepository(pool).ReserveThrottle(ctx, []ThrottleIdentity{pair}, 100)
		reservationDone <- reserveErr
	}()
	for attempts := 0; ; attempts++ {
		var waiters int
		if err = pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND NOT granted`).Scan(&waiters); err != nil {
			t.Fatal(err)
		}
		if waiters >= 3 {
			break
		}
		if attempts == 10000 {
			t.Fatal("complete login did not wait before user/session work")
		}
		runtime.Gosched()
	}
	var sessions int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM admin_sessions WHERE user_id=$1`, user.ID).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 {
		t.Fatalf("complete login created session before throttle admission: %d", sessions)
	}
	if _, err = holder.Exec(ctx, `SELECT pg_advisory_unlock(731492845631694123)`); err != nil {
		t.Fatal(err)
	}
	if err = <-resetDone; err != nil {
		t.Fatal(err)
	}
	if err = <-reservationDone; err != nil {
		t.Fatal(err)
	}
	if err = <-loginDone; err != nil && !errors.Is(err, ErrUnauthenticated) {
		t.Fatal(err)
	}
	var active int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM admin_sessions WHERE user_id=$1 AND revoked_at IS NULL`, user.ID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Fatalf("complete login/reset race left active sessions: %d", active)
	}
}

func TestPairThrottleRecoveryTagIsNeverReassigned(t *testing.T) {
	pool, repo, user, verifier := freshAuthRepository(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,owner_id,username,role,password_phc,password_policy_revision) VALUES('other-pair-user',$1,'otherpair','owner',$2,1)`, user.OwnerID, verifier.PHC); err != nil {
		t.Fatal(err)
	}
	tagged := ThrottleIdentity{Kind: ThrottlePair, KeyVersion: "v1", Digest: [32]byte{36}, RecoveryUserID: user.ID}
	if got, err := repo.ReserveThrottle(ctx, []ThrottleIdentity{tagged}, 100); err != nil || !got.Allowed {
		t.Fatalf("reserve tagged pair: %#v %v", got, err)
	}
	// An unknown attempt can increment the bucket but cannot remove its owner.
	unknown := tagged
	unknown.RecoveryUserID = ""
	if got, err := repo.ReserveThrottle(ctx, []ThrottleIdentity{unknown}, 100); err != nil || !got.Allowed {
		t.Fatalf("reserve unknown pair: %#v %v", got, err)
	}
	var owner string
	if err := pool.QueryRow(ctx, `SELECT recovery_user_id FROM auth_throttle_buckets WHERE kind='pair' AND key_version='v1' AND identifier_digest=$1`, tagged.Digest[:]).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != user.ID {
		t.Fatalf("unknown reservation reassigned tag to %q", owner)
	}

	conflicting := tagged
	conflicting.RecoveryUserID = "other-pair-user"
	if _, err := repo.ReserveThrottle(ctx, []ThrottleIdentity{conflicting}, 100); err == nil {
		t.Fatal("known-user reservation reassigned existing pair tag")
	}

	// A legacy alias is tagged only after a known-user reservation merges it.
	legacy := ThrottleAlias{KeyVersion: "legacy", Digest: [32]byte{37}}
	if _, err := pool.Exec(ctx, `INSERT INTO auth_throttle_buckets(kind,key_version,identifier_digest,window_started_at,attempts,failures) VALUES('pair',$1,$2,date_trunc('minute',clock_timestamp())-make_interval(mins => extract(minute from clock_timestamp())::int % 15),1,1)`, legacy.KeyVersion, legacy.Digest[:]); err != nil {
		t.Fatal(err)
	}
	merged := ThrottleIdentity{Kind: ThrottlePair, KeyVersion: "current", Digest: [32]byte{38}, Aliases: []ThrottleAlias{legacy}, RecoveryUserID: user.ID}
	if got, err := repo.ReserveThrottle(ctx, []ThrottleIdentity{merged}, 100); err != nil || !got.Allowed {
		t.Fatalf("merge legacy pair alias: %#v %v", got, err)
	}
	if err := pool.QueryRow(ctx, `SELECT recovery_user_id FROM auth_throttle_buckets WHERE kind='pair' AND key_version='current' AND identifier_digest=$1`, merged.Digest[:]).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != user.ID {
		t.Fatalf("merged legacy tag=%q want %q", owner, user.ID)
	}
}

func TestResetPasswordRollbackPreservesTaggedRecoveryBuckets(t *testing.T) {
	pool, repo, user, verifier := freshAuthRepository(t)
	ctx := context.Background()
	pair := ThrottleIdentity{Kind: ThrottlePair, KeyVersion: "v1", Digest: [32]byte{41}, RecoveryUserID: user.ID}
	if reservation, err := repo.ReserveThrottle(ctx, []ThrottleIdentity{pair}, 100); err != nil || !reservation.Allowed {
		t.Fatalf("reserve tagged pair: %#v %v", reservation, err)
	}
	if err := repo.ResetPassword(ctx, user.ID, verifier, "audit-bootstrap-"+t.Name(), nil); err == nil {
		t.Fatal("duplicate audit reset was accepted")
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM auth_throttle_buckets WHERE kind='pair' AND recovery_user_id=$1`, user.ID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("rollback removed tagged recovery pair rows: %d", rows)
	}
}

func TestCompleteLoginAtomicallyCreatesSessionAndClearsCredentialThrottles(t *testing.T) {
	pool, repo, user, verifier := freshAuthRepository(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,owner_id,username,role,password_phc,password_policy_revision) VALUES('other-login-user',$1,'otherlogin','owner',$2,1)`, user.OwnerID, verifier.PHC); err != nil {
		t.Fatal(err)
	}
	pair := ThrottleIdentity{Kind: ThrottlePair, KeyVersion: "v1", Digest: [32]byte{51}, RecoveryUserID: user.ID, Aliases: []ThrottleAlias{{KeyVersion: "prior", Digest: [32]byte{52}}}}
	username := ThrottleIdentity{Kind: ThrottleUsername, KeyVersion: "v1", Digest: [32]byte{53}, Aliases: []ThrottleAlias{{KeyVersion: "prior", Digest: [32]byte{54}}}}
	ip := ThrottleIdentity{Kind: ThrottleIP, KeyVersion: "v1", Digest: [32]byte{55}}
	other := ThrottleIdentity{Kind: ThrottlePair, KeyVersion: "v1", Digest: [32]byte{56}, RecoveryUserID: "other-login-user"}
	for _, identity := range []ThrottleIdentity{pair, username, ip, other} {
		if reservation, err := repo.ReserveThrottle(ctx, []ThrottleIdentity{identity}, 100); err != nil || !reservation.Allowed {
			t.Fatalf("reserve %s: %#v %v", identity.Kind, reservation, err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO auth_throttle_buckets(kind,key_version,identifier_digest,window_started_at,attempts,failures) VALUES('pair_overflow','overflow',decode(repeat('00',32),'hex'),date_trunc('minute',clock_timestamp())-make_interval(mins => extract(minute from clock_timestamp())::int % 15),1,1)`); err != nil {
		t.Fatal(err)
	}
	token := testToken(t, 51)
	session, err := repo.CompleteLogin(ctx, CompleteLoginInput{SessionID: "complete-login-session", Token: token, User: user, AuditID: "audit-complete-login", SessionLimitAuditID: "audit-complete-login-limit", ThrottleIdentities: []ThrottleIdentity{pair, username, ip}})
	if err != nil {
		t.Fatal(err)
	}
	if session.UserID != user.ID || session.AuthRevision != user.AuthRevision || session.IdleExpiresAt.Sub(session.CreatedAt) != 30*time.Minute || session.AbsoluteExpiresAt.Sub(session.CreatedAt) != 8*time.Hour {
		t.Fatalf("unexpected complete-login session: %+v", session)
	}
	var ownPair, usernameRows, ipRows, otherPair, overflowRows int
	if err = pool.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE kind='pair' AND recovery_user_id=$1),
		(SELECT count(*) FROM auth_username_throttles),
		count(*) FILTER (WHERE kind='ip'),
		count(*) FILTER (WHERE kind='pair' AND recovery_user_id='other-login-user'),
		count(*) FILTER (WHERE kind='pair_overflow')
		FROM auth_throttle_buckets`, user.ID).Scan(&ownPair, &usernameRows, &ipRows, &otherPair, &overflowRows); err != nil {
		t.Fatal(err)
	}
	if ownPair != 0 || usernameRows != 0 || ipRows != 1 || otherPair != 1 || overflowRows != 1 {
		t.Fatalf("own pair=%d username=%d ip=%d other=%d overflow=%d", ownPair, usernameRows, ipRows, otherPair, overflowRows)
	}
	if _, err = repo.ValidateSession(ctx, token); err != nil {
		t.Fatalf("complete login session invalid: %v", err)
	}
}

func TestCompleteLoginAuditFailureRollsBackSessionAndThrottleClear(t *testing.T) {
	pool, repo, user, _ := freshAuthRepository(t)
	ctx := context.Background()
	pair := ThrottleIdentity{Kind: ThrottlePair, KeyVersion: "v1", Digest: [32]byte{61}, RecoveryUserID: user.ID}
	username := ThrottleIdentity{Kind: ThrottleUsername, KeyVersion: "v1", Digest: [32]byte{62}}
	ip := ThrottleIdentity{Kind: ThrottleIP, KeyVersion: "v1", Digest: [32]byte{63}}
	for _, identity := range []ThrottleIdentity{pair, username, ip} {
		if reservation, err := repo.ReserveThrottle(ctx, []ThrottleIdentity{identity}, 100); err != nil || !reservation.Allowed {
			t.Fatalf("reserve %s: %#v %v", identity.Kind, reservation, err)
		}
	}
	_, err := repo.CompleteLogin(ctx, CompleteLoginInput{SessionID: "complete-login-rollback", Token: testToken(t, 61), User: user, AuditID: "audit-bootstrap-" + t.Name(), SessionLimitAuditID: "audit-complete-login-rollback-limit", ThrottleIdentities: []ThrottleIdentity{pair, username, ip}})
	if err == nil {
		t.Fatal("duplicate login audit was accepted")
	}
	var sessions, pairs int
	if err = pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM admin_sessions WHERE id='complete-login-rollback'),(SELECT count(*) FROM auth_throttle_buckets WHERE kind='pair' AND recovery_user_id=$1)`, user.ID).Scan(&sessions, &pairs); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 || pairs != 1 {
		t.Fatalf("rollback sessions=%d pairs=%d", sessions, pairs)
	}
}

func TestCompleteLoginRequiresExactlyKnownPairUsernameAndIP(t *testing.T) {
	_, repo, user, _ := freshAuthRepository(t)
	ctx := context.Background()
	pair := ThrottleIdentity{Kind: ThrottlePair, KeyVersion: "v1", Digest: [32]byte{64}, RecoveryUserID: user.ID}
	username := ThrottleIdentity{Kind: ThrottleUsername, KeyVersion: "v1", Digest: [32]byte{65}}
	ip := ThrottleIdentity{Kind: ThrottleIP, KeyVersion: "v1", Digest: [32]byte{66}}
	for index, identities := range [][]ThrottleIdentity{
		{pair, username},
		{pair, username, {Kind: ThrottlePair, KeyVersion: "v1", Digest: [32]byte{67}, RecoveryUserID: user.ID}},
		{pair, username, {Kind: ThrottleInvalidForward, KeyVersion: "v1", Digest: [32]byte{68}}},
	} {
		_, err := repo.CompleteLogin(ctx, CompleteLoginInput{SessionID: fmt.Sprintf("complete-login-invalid-%d", index), Token: testToken(t, byte(64+index)), User: user, AuditID: fmt.Sprintf("audit-complete-login-invalid-%d", index), SessionLimitAuditID: fmt.Sprintf("audit-complete-login-invalid-limit-%d", index), ThrottleIdentities: identities})
		if err == nil {
			t.Fatalf("accepted invalid successful-login identities: %#v", identities)
		}
	}
	if _, err := repo.CompleteLogin(ctx, CompleteLoginInput{SessionID: "complete-login-invalid-owner", Token: testToken(t, 69), User: user, AuditID: "audit-complete-login-invalid-owner", SessionLimitAuditID: "audit-complete-login-invalid-owner-limit", ThrottleIdentities: []ThrottleIdentity{{Kind: ThrottlePair, KeyVersion: "v1", Digest: [32]byte{69}, RecoveryUserID: "other-user"}, username, ip}}); err == nil {
		t.Fatal("accepted a pair owned by another user")
	}
}

func TestCompleteLoginRehashesAndEnforcesSessionCap(t *testing.T) {
	pool, repo, user, _ := freshAuthRepository(t)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		if _, err := repo.CreateSession(ctx, fmt.Sprintf("complete-login-cap-%02d", i), testToken(t, byte(70+i)), user, nil, fmt.Sprintf("audit-complete-login-cap-%02d", i), fmt.Sprintf("audit-complete-login-cap-limit-%02d", i)); err != nil {
			t.Fatal(err)
		}
	}
	newPolicy := auth.Argon2Policy{Revision: 2, MemoryKiB: 20 * 1024, Iterations: 2, Lanes: 1}
	if _, err := pool.Exec(ctx, `UPDATE installation_auth SET password_policy_revision=$1,password_memory_kib=$2,password_iterations=$3,password_lanes=$4 WHERE id=1`, int64(newPolicy.Revision), int32(newPolicy.MemoryKiB), int32(newPolicy.Iterations), int16(newPolicy.Lanes)); err != nil {
		t.Fatal(err)
	}
	user, err := repo.FindLoginUserByCanonicalUsername(ctx, "administrator")
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := auth.HashPassword(strings.Repeat("r", 15), newPolicy, bytes.NewReader(bytes.Repeat([]byte{71}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	pair := ThrottleIdentity{Kind: ThrottlePair, KeyVersion: "v1", Digest: [32]byte{71}, RecoveryUserID: user.ID}
	username := ThrottleIdentity{Kind: ThrottleUsername, KeyVersion: "v1", Digest: [32]byte{72}}
	ip := ThrottleIdentity{Kind: ThrottleIP, KeyVersion: "v1", Digest: [32]byte{73}}
	for _, identity := range []ThrottleIdentity{pair, username, ip} {
		if reservation, err := repo.ReserveThrottle(ctx, []ThrottleIdentity{identity}, 100); err != nil || !reservation.Allowed {
			t.Fatalf("reserve %s: %#v %v", identity.Kind, reservation, err)
		}
	}
	if _, err = repo.CompleteLogin(ctx, CompleteLoginInput{SessionID: "complete-login-cap-new", Token: testToken(t, 82), User: user, Replacement: &replacement, AuditID: "audit-complete-login-cap-new", SessionLimitAuditID: "audit-complete-login-cap-new-limit", ThrottleIdentities: []ThrottleIdentity{pair, username, ip}}); err != nil {
		t.Fatal(err)
	}
	var active, revoked int
	var storedPHC string
	var storedRevision int64
	if err = pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE revoked_at IS NULL),count(*) FILTER (WHERE revoked_reason='session_limit') FROM admin_sessions`).Scan(&active, &revoked); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT password_phc,password_policy_revision FROM users WHERE id=$1`, user.ID).Scan(&storedPHC, &storedRevision); err != nil {
		t.Fatal(err)
	}
	if active != 10 || revoked != 1 || storedPHC != replacement.PHC || storedRevision != int64(newPolicy.Revision) {
		t.Fatalf("active=%d revoked=%d replacement=%t policy=%d", active, revoked, storedPHC == replacement.PHC, storedRevision)
	}
}

func TestCompleteLoginCannotCommitAcrossPasswordReset(t *testing.T) {
	pool, repo, user, verifier := freshAuthRepository(t)
	ctx := context.Background()
	pair := ThrottleIdentity{Kind: ThrottlePair, KeyVersion: "v1", Digest: [32]byte{91}, RecoveryUserID: user.ID}
	username := ThrottleIdentity{Kind: ThrottleUsername, KeyVersion: "v1", Digest: [32]byte{92}}
	ip := ThrottleIdentity{Kind: ThrottleIP, KeyVersion: "v1", Digest: [32]byte{93}}
	for _, identity := range []ThrottleIdentity{pair, username, ip} {
		if reservation, err := repo.ReserveThrottle(ctx, []ThrottleIdentity{identity}, 100); err != nil || !reservation.Allowed {
			t.Fatalf("reserve %s: %#v %v", identity.Kind, reservation, err)
		}
	}
	errs := make(chan error, 2)
	go func() {
		_, err := NewAuthRepository(pool).CompleteLogin(ctx, CompleteLoginInput{SessionID: "complete-login-race", Token: testToken(t, 91), User: user, AuditID: "audit-complete-login-race", SessionLimitAuditID: "audit-complete-login-race-limit", ThrottleIdentities: []ThrottleIdentity{pair, username, ip}})
		errs <- err
	}()
	go func() {
		errs <- NewAuthRepository(pool).ResetPassword(ctx, user.ID, verifier, "audit-complete-login-reset", nil)
	}()
	for range 2 {
		if err := <-errs; err != nil && !errors.Is(err, ErrUnauthenticated) {
			t.Fatal(err)
		}
	}
	var active int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM admin_sessions WHERE user_id=$1 AND revoked_at IS NULL`, user.ID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Fatalf("reset/login race left active sessions: %d", active)
	}
}

func TestAuthMigrationConstraintsAndAuditDeletion(t *testing.T) {
	pool, _, user, verifier := freshAuthRepository(t)
	ctx := context.Background()
	for _, check := range []struct {
		statement string
		args      []any
	}{
		{`INSERT INTO installation_auth(id) VALUES(2)`, nil},
		{`INSERT INTO users(id,owner_id,username,role,password_phc,password_policy_revision) VALUES('bad-user',$1,'Bad','owner',$2,1)`, []any{user.OwnerID, verifier.PHC}},
		{`INSERT INTO admin_sessions(id,token_digest,user_id,owner_id,auth_revision,last_seen_at,idle_expires_at,absolute_expires_at) VALUES('bad-session',decode('00','hex'),$1,$2,1,clock_timestamp(),clock_timestamp(),clock_timestamp())`, []any{user.ID, user.OwnerID}},
		{`WITH n AS (SELECT clock_timestamp() AS v) INSERT INTO admin_sessions(id,token_digest,user_id,owner_id,auth_revision,created_at,last_seen_at,idle_expires_at,absolute_expires_at) SELECT 'bad-time',decode(repeat('00',32),'hex'),$1,$2,1,v,v-interval '1 second',v,v FROM n`, []any{user.ID, user.OwnerID}},
		{`INSERT INTO auth_throttle_buckets(kind,key_version,identifier_digest,window_started_at) VALUES('pair','v1',decode('00','hex'),clock_timestamp())`, nil},
		{`INSERT INTO auth_throttle_buckets(kind,key_version,identifier_digest,window_started_at,recovery_user_id) VALUES('ip','v1',decode(repeat('01',32),'hex'),clock_timestamp(),$1)`, []any{user.ID}},
		{`INSERT INTO auth_throttle_buckets(kind,key_version,identifier_digest,window_started_at,recovery_user_id) VALUES('pair','v1',decode(repeat('02',32),'hex'),clock_timestamp(),'missing-recovery-user')`, nil},
	} {
		if _, err := pool.Exec(ctx, check.statement, check.args...); err == nil {
			t.Fatalf("constraint accepted: %s", check.statement)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO owners(id) VALUES('other-owner')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO admin_sessions(id,token_digest,user_id,owner_id,auth_revision,last_seen_at,idle_expires_at,absolute_expires_at) VALUES('cross-owner-session',decode(repeat('ab',32),'hex'),$1,'other-owner',1,clock_timestamp(),clock_timestamp(),clock_timestamp())`, user.ID); err == nil {
		t.Fatal("cross-owner session was accepted")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO auth_audit_events(id,actor_user_id,owner_id,action,result) VALUES('cross-owner-audit',$1,'other-owner','logout','success')`, user.ID); err == nil {
		t.Fatal("cross-owner audit was accepted")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM auth_audit_events WHERE id=$1`, "audit-bootstrap-"+t.Name()); err == nil {
		t.Fatal("audit delete was accepted")
	}
}

func TestUsernameThrottleDedicatedBackoffStages(t *testing.T) {
	pool, repo, _, _ := freshAuthRepository(t)
	ctx := context.Background()
	identity := ThrottleIdentity{Kind: ThrottleUsername, KeyVersion: "v1", Digest: [32]byte{151}}
	for attempt := 1; attempt <= 9; attempt++ {
		got, err := repo.ReserveThrottle(ctx, []ThrottleIdentity{identity}, 100)
		if err != nil || !got.Allowed || !got.BlockedUntil.IsZero() {
			t.Fatalf("attempt %d = %#v %v", attempt, got, err)
		}
		var failures int
		var block *time.Time
		if err = pool.QueryRow(ctx, `SELECT failures,blocked_until FROM auth_username_throttles WHERE key_version='v1' AND identifier_digest=$1`, identity.Digest[:]).Scan(&failures, &block); err != nil || failures != attempt || block != nil {
			t.Fatalf("attempt %d state failures=%d block=%v err=%v", attempt, failures, block, err)
		}
	}
	firstBlock, err := repo.ReserveThrottle(ctx, []ThrottleIdentity{identity}, 100)
	if err != nil || !firstBlock.Allowed || firstBlock.BlockedUntil.IsZero() {
		t.Fatalf("tenth attempt = %#v %v", firstBlock, err)
	}
	if delta := firstBlock.BlockedUntil.Sub(time.Now()); delta < 20*time.Second || delta > 40*time.Second {
		t.Fatalf("first block duration=%v, want about 30s", delta)
	}
	var persisted time.Time
	if err = pool.QueryRow(ctx, `SELECT blocked_until FROM auth_username_throttles WHERE key_version='v1' AND identifier_digest=$1`, identity.Digest[:]).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if !persisted.Equal(firstBlock.BlockedUntil) {
		t.Fatalf("persisted block=%v returned=%v", persisted, firstBlock.BlockedUntil)
	}
	denied, err := repo.ReserveThrottle(ctx, []ThrottleIdentity{identity}, 100)
	if err != nil || denied.Allowed || !denied.BlockedUntil.Equal(persisted) {
		t.Fatalf("retry during block = %#v %v; persisted=%v", denied, err, persisted)
	}
	for _, want := range []time.Duration{60 * time.Second, 120 * time.Second, 240 * time.Second, 480 * time.Second, 900 * time.Second, 900 * time.Second} {
		if _, err = pool.Exec(ctx, `WITH n AS (SELECT clock_timestamp() AS v) UPDATE auth_username_throttles SET updated_at=n.v-interval '2 seconds',blocked_until=n.v-interval '1 second' FROM n WHERE key_version='v1' AND identifier_digest=$1`, identity.Digest[:]); err != nil {
			t.Fatal(err)
		}
		before := time.Now()
		got, reserveErr := repo.ReserveThrottle(ctx, []ThrottleIdentity{identity}, 100)
		if reserveErr != nil || !got.Allowed || got.BlockedUntil.IsZero() {
			t.Fatalf("next stage %v = %#v %v", want, got, reserveErr)
		}
		if delta := got.BlockedUntil.Sub(before); delta < want-10*time.Second || delta > want+10*time.Second {
			t.Fatalf("stage duration=%v want=%v", delta, want)
		}
		var failures int
		var storedBlock time.Time
		if err = pool.QueryRow(ctx, `SELECT failures,blocked_until FROM auth_username_throttles WHERE key_version='v1' AND identifier_digest=$1`, identity.Digest[:]).Scan(&failures, &storedBlock); err != nil {
			t.Fatal(err)
		}
		if failures < 11 || !storedBlock.Equal(got.BlockedUntil) {
			t.Fatalf("stage persistence failures=%d block=%v want=%v", failures, storedBlock, got.BlockedUntil)
		}
	}
	var bucketRows int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM auth_throttle_buckets WHERE kind='username'`).Scan(&bucketRows); err != nil || bucketRows != 0 {
		t.Fatalf("username fixed buckets=%d err=%v", bucketRows, err)
	}
}

func TestUsernameThrottleAliasesAndOverflowSurviveFinalization(t *testing.T) {
	pool, repo, _, _ := freshAuthRepository(t)
	ctx := context.Background()
	primary := ThrottleIdentity{Kind: ThrottleUsername, KeyVersion: "current", Digest: [32]byte{161}, Aliases: []ThrottleAlias{{KeyVersion: "prior-a", Digest: [32]byte{162}}, {KeyVersion: "prior-b", Digest: [32]byte{163}}}}
	if _, err := pool.Exec(ctx, `INSERT INTO auth_username_throttles(key_version,identifier_digest,failures,blocked_until,updated_at) VALUES
		('prior-a',$1,4,clock_timestamp()+interval '20 seconds',clock_timestamp()),
		('prior-b',$2,7,clock_timestamp()+interval '40 seconds',clock_timestamp())`, primary.Aliases[0].Digest[:], primary.Aliases[1].Digest[:]); err != nil {
		t.Fatal(err)
	}
	got, err := repo.ReserveThrottle(ctx, []ThrottleIdentity{primary}, 100)
	if err != nil || got.Allowed || got.BlockedUntil.IsZero() {
		t.Fatalf("alias live block = %#v %v", got, err)
	}
	var failures, aliases int
	var maxBlock time.Time
	if err = pool.QueryRow(ctx, `SELECT failures,blocked_until FROM auth_username_throttles WHERE key_version='current' AND identifier_digest=$1`, primary.Digest[:]).Scan(&failures, &maxBlock); err != nil {
		t.Fatal(err)
	}
	if failures != 11 || !maxBlock.Equal(got.BlockedUntil) {
		t.Fatalf("merged failures=%d block=%v returned=%v", failures, maxBlock, got.BlockedUntil)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM auth_username_throttles WHERE key_version IN ('prior-a','prior-b')`).Scan(&aliases); err != nil || aliases != 0 {
		t.Fatalf("retained aliases=%d err=%v", aliases, err)
	}
	// Four fixed rows force every unseen username into the single shared state.
	if _, err = pool.Exec(ctx, `TRUNCATE auth_throttle_buckets,auth_username_throttles`); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE auth_throttle_state SET max_rows=NULL WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		id := ThrottleIdentity{Kind: ThrottleIP, KeyVersion: "v1", Digest: [32]byte{byte(170 + i)}}
		if reservation, reserveErr := repo.ReserveThrottle(ctx, []ThrottleIdentity{id}, 8); reserveErr != nil || !reservation.Allowed {
			t.Fatalf("seed %d = %#v %v", i, reservation, reserveErr)
		}
	}
	overflow := ThrottleIdentity{Kind: ThrottleUsername, KeyVersion: "v1", Digest: [32]byte{180}}
	for i := 0; i < 10; i++ {
		if reservation, reserveErr := repo.ReserveThrottle(ctx, []ThrottleIdentity{overflow}, 8); reserveErr != nil || !reservation.Allowed {
			t.Fatalf("overflow attempt %d = %#v %v", i, reservation, reserveErr)
		}
	}
	if err = repo.FinalizeThrottle(ctx, true, []ThrottleIdentity{overflow}); err != nil {
		t.Fatal(err)
	}
	var overflowFailures int
	var overflowBlock time.Time
	if err = pool.QueryRow(ctx, `SELECT failures,blocked_until FROM auth_username_throttles WHERE key_version='overflow'`).Scan(&overflowFailures, &overflowBlock); err != nil || overflowFailures != 10 || overflowBlock.IsZero() {
		t.Fatalf("overflow after success failures=%d block=%v err=%v", overflowFailures, overflowBlock, err)
	}
	denied, err := repo.ReserveThrottle(ctx, []ThrottleIdentity{{Kind: ThrottleUsername, KeyVersion: "v2", Digest: [32]byte{181}}}, 8)
	if err != nil || denied.Allowed || !denied.BlockedUntil.Equal(overflowBlock) {
		t.Fatalf("overflow denial=%#v err=%v", denied, err)
	}
	if _, err = pool.Exec(ctx, `WITH n AS (SELECT clock_timestamp() AS v) UPDATE auth_username_throttles SET updated_at=n.v-interval '2 seconds',blocked_until=n.v-interval '1 second' FROM n WHERE key_version='overflow'`); err != nil {
		t.Fatal(err)
	}
	next, err := repo.ReserveThrottle(ctx, []ThrottleIdentity{{Kind: ThrottleUsername, KeyVersion: "v3", Digest: [32]byte{182}}}, 8)
	if err != nil || !next.Allowed || next.BlockedUntil.IsZero() || next.BlockedUntil.Sub(time.Now()) < 50*time.Second {
		t.Fatalf("overflow next stage=%#v err=%v", next, err)
	}
}

func TestThrottleGlobalCapConcurrentAndCleanupBound(t *testing.T) {
	pool, repo, _, _ := freshAuthRepository(t)
	ctx := context.Background()
	var err error
	const maxRows = 8
	otherPool, err := pgxpool.New(ctx, os.Getenv("AUTODEPLOY_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(otherPool.Close)
	repos := []*AuthRepository{NewAuthRepository(pool), NewAuthRepository(otherPool)}
	var wg sync.WaitGroup
	errs := make(chan error, 24)
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			kind := ThrottleIP
			if i%3 == 0 {
				kind = ThrottleUsername
			}
			_, err := repos[i%len(repos)].ReserveThrottle(ctx, []ThrottleIdentity{{Kind: kind, KeyVersion: "v1", Digest: [32]byte{byte(i + 1)}}}, maxRows)
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM auth_throttle_buckets)+(SELECT count(*) FROM auth_username_throttles)`).Scan(&rows); err != nil || rows > maxRows {
		t.Fatalf("global rows=%d err=%v", rows, err)
	}
	if _, err = repo.ReserveThrottle(ctx, []ThrottleIdentity{{Kind: ThrottleIP, KeyVersion: "v1", Digest: [32]byte{201}}}, 9); err == nil || !strings.Contains(err.Error(), "differs") {
		t.Fatalf("conflicting cap error=%v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE auth_throttle_buckets SET window_started_at=date_trunc('hour',clock_timestamp())-interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	if deleted, cleanupErr := repo.CleanupThrottle(ctx, 1); cleanupErr != nil || deleted > 1 {
		t.Fatalf("cleanup=%d err=%v", deleted, cleanupErr)
	}
	if _, err = repo.ReserveThrottle(ctx, []ThrottleIdentity{{Kind: ThrottleIP, KeyVersion: "v2", Digest: [32]byte{202}}}, maxRows); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM auth_throttle_buckets)+(SELECT count(*) FROM auth_username_throttles)`).Scan(&rows); err != nil || rows > maxRows {
		t.Fatalf("rows after cleanup/admission=%d err=%v", rows, err)
	}
}

func TestConcurrentSessionCapAndAuditRollback(t *testing.T) {
	pool, repo, user, _ := freshAuthRepository(t)
	ctx := context.Background()
	var err error
	var wg sync.WaitGroup
	errs := make(chan error, 11)
	tokens := make([]auth.SessionToken, 11)
	for i := range tokens {
		tokens[i] = testToken(t, byte(i+30))
	}
	for i := 0; i < 11; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := NewAuthRepository(pool).CreateSession(ctx, fmt.Sprintf("session-concurrent-%02d", i), tokens[i], user, nil, fmt.Sprintf("audit-concurrent-%02d", i), fmt.Sprintf("audit-cap-concurrent-%02d", i))
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for createErr := range errs {
		if createErr != nil {
			t.Fatal(createErr)
		}
	}
	var active, capAudits int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE revoked_at IS NULL),count(*) FILTER (WHERE revoked_reason='session_limit') FROM admin_sessions`).Scan(&active, &capAudits); err != nil || active != 10 || capAudits != 1 {
		t.Fatalf("active=%d capAudits=%d err=%v", active, capAudits, err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO auth_audit_events(id,actor_user_id,owner_id,action,result) VALUES('duplicate-cap-audit',$1,$2,'session_revoked','success')`, user.ID, user.OwnerID); err != nil {
		t.Fatal(err)
	}
	var before int
	if err = pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE revoked_at IS NULL) FROM admin_sessions`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	_, err = repo.CreateSession(ctx, "session-cap-audit-rollback", testToken(t, 99), user, nil, "audit-cap-audit-rollback", "duplicate-cap-audit")
	if err == nil {
		t.Fatal("duplicate limit audit accepted")
	}
	var after, inserted int
	if err = pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE revoked_at IS NULL),count(*) FILTER (WHERE id='session-cap-audit-rollback') FROM admin_sessions`).Scan(&after, &inserted); err != nil || after != before || inserted != 0 {
		t.Fatalf("after=%d inserted=%d before=%d err=%v", after, inserted, before, err)
	}
}

func TestCleanupSessionsAndFiveMinuteTouch(t *testing.T) {
	pool, repo, user, _ := freshAuthRepository(t)
	ctx := context.Background()
	newSession := func(id string, b byte) auth.SessionToken {
		token := testToken(t, b)
		if _, err := repo.CreateSession(ctx, id, token, user, nil, "audit-"+id, "audit-limit-"+id); err != nil {
			t.Fatal(err)
		}
		return token
	}
	active := newSession("session-clean-active", 211)
	newSession("session-clean-revoked", 212)
	newSession("session-clean-idle", 213)
	for _, update := range []string{
		`UPDATE admin_sessions SET revoked_at=clock_timestamp(),revoked_reason='logout' WHERE id='session-clean-revoked'`,
		`UPDATE admin_sessions SET created_at=clock_timestamp()-interval '31 minutes',last_seen_at=clock_timestamp()-interval '31 minutes',idle_expires_at=clock_timestamp()-interval '1 second',absolute_expires_at=clock_timestamp()+interval '1 hour' WHERE id='session-clean-idle'`,
	} {
		if _, err := pool.Exec(ctx, update); err != nil {
			t.Fatal(err)
		}
	}
	newSession("session-clean-absolute", 214)
	if _, err := pool.Exec(ctx, `WITH n AS (SELECT clock_timestamp() AS v) UPDATE admin_sessions SET created_at=n.v-interval '9 hours',last_seen_at=n.v-interval '1 hour',idle_expires_at=n.v-interval '1 second',absolute_expires_at=n.v-interval '1 second' FROM n WHERE id='session-clean-absolute'`); err != nil {
		t.Fatal(err)
	}
	if deleted, err := repo.CleanupSessions(ctx, 2); err != nil || deleted != 2 {
		t.Fatalf("first cleanup=%d err=%v", deleted, err)
	}
	if deleted, err := repo.CleanupSessions(ctx, 2); err != nil || deleted != 1 {
		t.Fatalf("second cleanup=%d err=%v", deleted, err)
	}
	if _, err := repo.ValidateSession(ctx, active); err != nil {
		t.Fatalf("active session=%v", err)
	}
	// Recreate a session whose persisted last-seen is eligible for normal touch.
	touch := newSession("session-touch-five-minutes", 215)
	if _, err := pool.Exec(ctx, `WITH n AS (SELECT clock_timestamp() AS v) UPDATE admin_sessions SET created_at=n.v-interval '6 minutes',last_seen_at=n.v-interval '5 minutes',idle_expires_at=n.v+interval '1 minute' FROM n WHERE id='session-touch-five-minutes'`); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ValidateSession(ctx, touch); err != nil {
		t.Fatal(err)
	}
	var last, idle, absolute time.Time
	if err := pool.QueryRow(ctx, `SELECT last_seen_at,idle_expires_at,absolute_expires_at FROM admin_sessions WHERE id='session-touch-five-minutes'`).Scan(&last, &idle, &absolute); err != nil {
		t.Fatal(err)
	}
	if idle.Before(last.Add(29*time.Minute)) || idle.After(last.Add(30*time.Minute+time.Second)) || idle.After(absolute) {
		t.Fatalf("touch last=%v idle=%v absolute=%v", last, idle, absolute)
	}
}

func TestSessionStoresOnlyExactTokenDigest(t *testing.T) {
	pool, repo, user, _ := freshAuthRepository(t)
	ctx := context.Background()
	token := testToken(t, 221)
	if _, err := repo.CreateSession(ctx, "session-digest-exact", token, user, nil, "audit-digest-exact", "audit-digest-limit"); err != nil {
		t.Fatal(err)
	}
	want, err := auth.SessionTokenDigest(token)
	if err != nil {
		t.Fatal(err)
	}
	var stored []byte
	if err = pool.QueryRow(ctx, `SELECT token_digest FROM admin_sessions WHERE id='session-digest-exact'`).Scan(&stored); err != nil || !bytes.Equal(stored, want[:]) {
		t.Fatalf("stored digest=%x want=%x err=%v", stored, want, err)
	}
	var rawColumns int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='admin_sessions' AND column_name ILIKE '%token%' AND column_name <> 'token_digest'`).Scan(&rawColumns); err != nil || rawColumns != 0 {
		t.Fatalf("raw token columns=%d err=%v", rawColumns, err)
	}
}

func TestUsernameOverflowCapAndReservedKeyVersionBoundary(t *testing.T) {
	pool, repo, _, _ := freshAuthRepository(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO auth_throttle_buckets(kind,key_version,identifier_digest,window_started_at,attempts,failures)
		SELECT 'ip','prior',set_byte(decode(repeat('00',32),'hex'),0,n),date_trunc('hour',clock_timestamp())-interval '1 hour',1,1 FROM generate_series(1,8) AS n`); err != nil {
		t.Fatal(err)
	}
	username := ThrottleIdentity{Kind: ThrottleUsername, KeyVersion: "v1", Digest: [32]byte{231}}
	if got, err := repo.ReserveThrottle(ctx, []ThrottleIdentity{username}, 8); err != nil || got.Allowed {
		t.Fatalf("full-cap username admission=%#v err=%v", got, err)
	}
	var total, usernameOverflow int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM auth_throttle_buckets)+(SELECT count(*) FROM auth_username_throttles),count(*) FROM auth_username_throttles WHERE key_version='overflow'`).Scan(&total, &usernameOverflow); err != nil || total != 8 || usernameOverflow != 0 {
		t.Fatalf("total=%d usernameOverflow=%d err=%v", total, usernameOverflow, err)
	}
	for _, identity := range []ThrottleIdentity{
		{Kind: ThrottleIP, KeyVersion: "overflow", Digest: [32]byte{232}},
		{Kind: ThrottleUsername, KeyVersion: "v1", Digest: [32]byte{233}, Aliases: []ThrottleAlias{{KeyVersion: "overflow", Digest: [32]byte{234}}}},
	} {
		if _, err := repo.ReserveThrottle(ctx, []ThrottleIdentity{identity}, 8); err == nil {
			t.Fatalf("accepted reserved key version: %#v", identity)
		}
	}
}
