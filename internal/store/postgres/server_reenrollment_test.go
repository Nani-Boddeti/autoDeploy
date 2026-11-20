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
	"autodeploy/internal/server"

	"github.com/jackc/pgx/v5/pgxpool"
)

func reenrollEnrollmentDigest(t *testing.T, seed byte) [32]byte {
	t.Helper()
	token, err := server.NewEnrollmentToken(bytes.NewReader(bytes.Repeat([]byte{seed}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	digest, err := server.EnrollmentTokenDigest(token)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func reenrollCredentialDigest(t *testing.T, seed byte) [32]byte {
	t.Helper()
	token, err := server.NewCredentialToken(bytes.NewReader(bytes.Repeat([]byte{seed}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	digest, err := server.CredentialTokenDigest(token)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

// enrolledTestServer registers serverID and enrolls it with credential seed
// credSeed, returning that credential's digest (version 1).
func enrolledTestServer(t *testing.T, repo *ServerRepository, principal auth.Principal, serverID string, enrollSeed, credSeed byte) [32]byte {
	t.Helper()
	_, _ = registerTestServer(t, repo, principal, serverID, enrollSeed)
	cred := reenrollCredentialDigest(t, credSeed)
	if v, err := repo.Enroll(context.Background(), serverID, reenrollEnrollmentDigest(t, enrollSeed), cred); err != nil || v != 1 {
		t.Fatalf("enroll version=%d err=%v", v, err)
	}
	return cred
}

func serverTableCounts(t *testing.T, pool *pgxpool.Pool) (tokens, audits, credentials int) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM server_enrollment_tokens),(SELECT count(*) FROM server_audit_events),(SELECT count(*) FROM server_credentials)`).Scan(&tokens, &audits, &credentials); err != nil {
		t.Fatal(err)
	}
	return
}

func TestIssueReenrollmentRefusals(t *testing.T) {
	pool, repo, authRepo, principal := freshServerRepository(t)
	ctx := context.Background()
	other := secondOwnerPrincipal(t, pool, authRepo, 201)
	enrolledTestServer(t, repo, principal, "srv_ok", 1, 2)
	enrolledTestServer(t, repo, principal, "srv_revoked", 3, 4)
	if err := repo.RevokeServer(ctx, principal, "srv_revoked", "decommissioned"); err != nil {
		t.Fatal(err)
	}
	_, _ = registerTestServer(t, repo, principal, "srv_never", 5)
	digest := reenrollEnrollmentDigest(t, 50)

	t0, a0, c0 := serverTableCounts(t, pool)
	cases := []struct {
		name      string
		principal auth.Principal
		serverID  string
		ttl       int
		reason    string
		want      error
	}{
		{"revoked server", principal, "srv_revoked", 900, "restore", ErrServerAuthorizationDenied},
		{"foreign owner enrolled server", other, "srv_ok", 900, "restore", ErrServerAuthorizationDenied},
		// Owner scope must win over the never-enrolled refusal: a foreign
		// owner learns nothing (not even "exists but never enrolled").
		{"foreign owner never-enrolled server", other, "srv_never", 900, "restore", ErrServerAuthorizationDenied},
		{"unknown server", principal, "srv_missing", 900, "restore", ErrServerAuthorizationDenied},
		{"never enrolled server (use IssueEnrollment)", principal, "srv_never", 900, "restore", ErrServerNotEnrolled},
		{"empty reason", principal, "srv_ok", 900, "", ErrServerAuthorizationDenied},
		{"control character in reason", principal, "srv_ok", 900, "a\x00b", ErrServerAuthorizationDenied},
		{"oversize reason", principal, "srv_ok", 900, strings.Repeat("x", 513), ErrServerAuthorizationDenied},
		{"ttl zero", principal, "srv_ok", 0, "restore", ErrServerAuthorizationDenied},
		{"ttl over cap", principal, "srv_ok", 3601, "restore", ErrServerAuthorizationDenied},
		{"invalid server id", principal, "bad\x00id", 900, "restore", ErrServerAuthorizationDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := repo.IssueReenrollment(ctx, tc.principal, tc.serverID, digest, tc.ttl, tc.reason); !errors.Is(err, tc.want) {
				t.Fatalf("err=%v, want %v", err, tc.want)
			}
		})
	}
	if t1, a1, c1 := serverTableCounts(t, pool); t1 != t0 || a1 != a0 || c1 != c0 {
		t.Fatalf("refusals mutated state: tokens %d->%d audits %d->%d credentials %d->%d", t0, t1, a0, a1, c0, c1)
	}
}

func TestIssueReenrollmentRefusedWhileUnconsumedTokenExists(t *testing.T) {
	pool, repo, _, principal := freshServerRepository(t)
	ctx := context.Background()
	enrolledTestServer(t, repo, principal, "srv_p", 1, 2)
	first := reenrollEnrollmentDigest(t, 60)
	if _, err := repo.IssueReenrollment(ctx, principal, "srv_p", first, 900, "restore"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.IssueReenrollment(ctx, principal, "srv_p", reenrollEnrollmentDigest(t, 61), 900, "again"); !errors.Is(err, ErrReenrollmentPending) {
		t.Fatalf("second issue while pending: %v", err)
	}
	// The pending token is untouched by the refused attempt.
	if _, err := repo.Enroll(ctx, "srv_p", first, reenrollCredentialDigest(t, 62)); err != nil {
		t.Fatalf("original pending token no longer valid: %v", err)
	}
	var unconsumed int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM server_enrollment_tokens WHERE server_id='srv_p' AND consumed_at IS NULL`).Scan(&unconsumed); err != nil || unconsumed != 0 {
		t.Fatalf("unconsumed=%d err=%v", unconsumed, err)
	}
}

func TestReenrollConsumeReplacesCredentialAndAudits(t *testing.T) {
	pool, repo, _, principal := freshServerRepository(t)
	ctx := context.Background()
	oldCred := enrolledTestServer(t, repo, principal, "srv_r", 1, 2)
	token := reenrollEnrollmentDigest(t, 70)
	expires, err := repo.IssueReenrollment(ctx, principal, "srv_r", token, 900, "restored from backup")
	if err != nil || expires.IsZero() {
		t.Fatalf("issue: %v", err)
	}
	// Issuing alone changes nothing about the live credential.
	if _, err := repo.Authenticate(ctx, "srv_r", oldCred); err != nil {
		t.Fatalf("old credential rejected before consume: %v", err)
	}
	newCred := reenrollCredentialDigest(t, 71)
	version, err := repo.Enroll(ctx, "srv_r", token, newCred)
	if err != nil || version != 2 {
		t.Fatalf("reenroll version=%d err=%v", version, err)
	}
	if _, err := repo.Authenticate(ctx, "srv_r", oldCred); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("old credential still accepted: %v", err)
	}
	if got, err := repo.Authenticate(ctx, "srv_r", newCred); err != nil || got.CredentialVersion != 2 {
		t.Fatalf("new credential: %+v %v", got, err)
	}
	if err := repo.Heartbeat(ctx, "srv_r", newCred); err != nil {
		t.Fatalf("heartbeat with new credential: %v", err)
	}
	if err := repo.Heartbeat(ctx, "srv_r", oldCred); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("heartbeat with old credential: %v", err)
	}
	// Single use.
	if _, err := repo.Enroll(ctx, "srv_r", token, reenrollCredentialDigest(t, 72)); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("token replay: %v", err)
	}
	rows, err := pool.Query(ctx, `SELECT version,status FROM server_credentials WHERE server_id='srv_r' ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int]string{}
	for rows.Next() {
		var v int
		var s string
		if err := rows.Scan(&v, &s); err != nil {
			t.Fatal(err)
		}
		got[v] = s
	}
	rows.Close()
	if len(got) != 2 || got[1] != "revoked" || got[2] != "active" {
		t.Fatalf("credentials=%v", got)
	}
	// Audit trail: issue (administrator, with reason) then enrolled and
	// credential_rotated (agent, version 2).
	type ev struct {
		actorType, actorID, action, reason string
		version                            *int
	}
	arows, err := pool.Query(ctx, `SELECT actor_type,actor_id,action,reason,credential_version FROM server_audit_events WHERE server_id='srv_r' AND id > (SELECT max(id) FROM server_audit_events WHERE server_id='srv_r' AND action='enrolled' AND credential_version=1) ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var events []ev
	for arows.Next() {
		var e ev
		if err := arows.Scan(&e.actorType, &e.actorID, &e.action, &e.reason, &e.version); err != nil {
			t.Fatal(err)
		}
		events = append(events, e)
	}
	arows.Close()
	if len(events) != 3 {
		t.Fatalf("events=%+v", events)
	}
	if e := events[0]; e.action != "reenrollment_issued" || e.actorType != "administrator" || e.actorID != principal.UserID || e.reason != "restored from backup" || e.version != nil {
		t.Fatalf("issue event=%+v", e)
	}
	if e := events[1]; e.action != "enrolled" || e.actorType != "agent" || e.actorID != "srv_r" || e.version == nil || *e.version != 2 {
		t.Fatalf("enrolled event=%+v", e)
	}
	if e := events[2]; e.action != "credential_rotated" || e.actorType != "agent" || e.actorID != "srv_r" || e.version == nil || *e.version != 2 {
		t.Fatalf("rotated event=%+v", e)
	}
	// A second restore later yields version 3.
	token2 := reenrollEnrollmentDigest(t, 73)
	if _, err := repo.IssueReenrollment(ctx, principal, "srv_r", token2, 900, "second restore"); err != nil {
		t.Fatal(err)
	}
	if v, err := repo.Enroll(ctx, "srv_r", token2, reenrollCredentialDigest(t, 74)); err != nil || v != 3 {
		t.Fatalf("second reenroll version=%d err=%v", v, err)
	}
}

func TestReenrollTokenExpiryAndExpiredReplacement(t *testing.T) {
	pool, repo, _, principal := freshServerRepository(t)
	ctx := context.Background()
	oldCred := enrolledTestServer(t, repo, principal, "srv_x", 1, 2)
	stale := reenrollEnrollmentDigest(t, 80)
	if _, err := repo.IssueReenrollment(ctx, principal, "srv_x", stale, 900, "restore"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE server_enrollment_tokens SET issued_at=clock_timestamp()-interval '1 hour', expires_at=clock_timestamp()-interval '1 minute' WHERE server_id='srv_x' AND consumed_at IS NULL`); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Enroll(ctx, "srv_x", stale, reenrollCredentialDigest(t, 81)); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("expired token accepted: %v", err)
	}
	if _, err := repo.Authenticate(ctx, "srv_x", oldCred); err != nil {
		t.Fatalf("expired consume disturbed the live credential: %v", err)
	}
	// An expired, unconsumed token must not block the operator forever: a
	// fresh issue replaces it and the stale digest stops working.
	fresh := reenrollEnrollmentDigest(t, 82)
	if _, err := repo.IssueReenrollment(ctx, principal, "srv_x", fresh, 900, "retry"); err != nil {
		t.Fatalf("replacement of expired token: %v", err)
	}
	if _, err := repo.Enroll(ctx, "srv_x", stale, reenrollCredentialDigest(t, 83)); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("replaced digest accepted: %v", err)
	}
	if v, err := repo.Enroll(ctx, "srv_x", fresh, reenrollCredentialDigest(t, 84)); err != nil || v != 2 {
		t.Fatalf("fresh token: version=%d err=%v", v, err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM server_enrollment_tokens WHERE server_id='srv_x'`).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("token rows=%d err=%v (want enroll + one reenroll)", rows, err)
	}
}

func TestReenrollTokenForRevokedServerIsRefused(t *testing.T) {
	pool, repo, _, principal := freshServerRepository(t)
	ctx := context.Background()
	enrolledTestServer(t, repo, principal, "srv_v", 1, 2)
	token := reenrollEnrollmentDigest(t, 90)
	if _, err := repo.IssueReenrollment(ctx, principal, "srv_v", token, 900, "restore"); err != nil {
		t.Fatal(err)
	}
	if err := repo.RevokeServer(ctx, principal, "srv_v", "lost"); err != nil {
		t.Fatal(err)
	}
	_, a0, c0 := serverTableCounts(t, pool)
	if _, err := repo.Enroll(ctx, "srv_v", token, reenrollCredentialDigest(t, 91)); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("revoked server re-enrolled: %v", err)
	}
	if _, a1, c1 := serverTableCounts(t, pool); a1 != a0 || c1 != c0 {
		t.Fatalf("refused enroll mutated state")
	}
}

func TestEnrollPurposeEnrollPathUnchanged(t *testing.T) {
	pool, repo, _, principal := freshServerRepository(t)
	ctx := context.Background()
	cred := enrolledTestServer(t, repo, principal, "srv_u", 1, 2)
	// First enrollment still mints version 1 and audits only `enrolled`.
	var purpose string
	if err := pool.QueryRow(ctx, `SELECT purpose FROM server_enrollment_tokens WHERE server_id='srv_u'`).Scan(&purpose); err != nil || purpose != "enroll" {
		t.Fatalf("purpose=%q err=%v", purpose, err)
	}
	var rotated int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM server_audit_events WHERE server_id='srv_u' AND action='credential_rotated'`).Scan(&rotated); err != nil || rotated != 0 {
		t.Fatalf("first enrollment audited a rotation: %d %v", rotated, err)
	}
	// IssueEnrollment keeps refusing an enrolled server, with or without a
	// pending re-enrollment token.
	if _, err := repo.IssueEnrollment(ctx, principal, "srv_u", reenrollEnrollmentDigest(t, 3), 900); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("IssueEnrollment on enrolled server: %v", err)
	}
	if _, err := repo.IssueReenrollment(ctx, principal, "srv_u", reenrollEnrollmentDigest(t, 4), 900, "restore"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.IssueEnrollment(ctx, principal, "srv_u", reenrollEnrollmentDigest(t, 5), 900); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("IssueEnrollment with pending reenroll token: %v", err)
	}
	// The refused IssueEnrollment must not have touched the reenroll token.
	if v, err := repo.Enroll(ctx, "srv_u", reenrollEnrollmentDigest(t, 4), reenrollCredentialDigest(t, 6)); err != nil || v != 2 {
		t.Fatalf("pending reenroll token damaged: version=%d err=%v", v, err)
	}
	if _, err := repo.Authenticate(ctx, "srv_u", cred); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("first credential survived re-enrollment: %v", err)
	}
	// A never-enrolled server still reissues over its enroll-purpose token.
	_, _ = registerTestServer(t, repo, principal, "srv_n", 7)
	reissue := reenrollEnrollmentDigest(t, 8)
	if _, err := repo.IssueEnrollment(ctx, principal, "srv_n", reissue, 900); err != nil {
		t.Fatalf("reissue for never-enrolled server: %v", err)
	}
	if v, err := repo.Enroll(ctx, "srv_n", reissue, reenrollCredentialDigest(t, 9)); err != nil || v != 1 {
		t.Fatalf("enroll after reissue: %d %v", v, err)
	}
}

func TestReenrollRollsBackWhenCredentialInsertFails(t *testing.T) {
	pool, repo, _, principal := freshServerRepository(t)
	ctx := context.Background()
	oldCred := enrolledTestServer(t, repo, principal, "srv_f", 1, 2)
	token := reenrollEnrollmentDigest(t, 100)
	if _, err := repo.IssueReenrollment(ctx, principal, "srv_f", token, 900, "restore"); err != nil {
		t.Fatal(err)
	}
	repo.failAfter = func(stage string) error {
		if stage == "credential_rotate_insert" {
			return errors.New("injected failure after revoke")
		}
		return nil
	}
	if _, err := repo.Enroll(ctx, "srv_f", token, reenrollCredentialDigest(t, 101)); err == nil {
		t.Fatal("injected failure did not surface")
	}
	repo.failAfter = nil
	if _, err := repo.Authenticate(ctx, "srv_f", oldCred); err != nil {
		t.Fatalf("old credential lost after rolled-back re-enrollment: %v", err)
	}
	var consumed *string
	if err := pool.QueryRow(ctx, `SELECT consumed_at::text FROM server_enrollment_tokens WHERE server_id='srv_f' AND purpose='reenroll'`).Scan(&consumed); err != nil || consumed != nil {
		t.Fatalf("token consumed despite rollback: %v %v", consumed, err)
	}
	// The same token still works afterwards.
	if v, err := repo.Enroll(ctx, "srv_f", token, reenrollCredentialDigest(t, 102)); err != nil || v != 2 {
		t.Fatalf("retry: %d %v", v, err)
	}
}

// TestReenrollConcurrentConsumeExactlyOneWinner forces real lock contention: a
// third transaction holds the servers row FOR UPDATE, which both Enroll calls
// lock first (lock order owner -> server -> credential). Both are started
// and observed blocked in pg_stat_activity before the holder releases, so the
// winner is decided by PostgreSQL's lock queue, not goroutine scheduling.
func TestReenrollConcurrentConsumeExactlyOneWinner(t *testing.T) {
	pool, repo, _, principal := freshServerRepository(t)
	ctx := context.Background()
	oldCred := enrolledTestServer(t, repo, principal, "srv_c", 1, 2)
	token := reenrollEnrollmentDigest(t, 110)
	if _, err := repo.IssueReenrollment(ctx, principal, "srv_c", token, 900, "restore"); err != nil {
		t.Fatal(err)
	}
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx) //nolint:errcheck
	if _, err := holder.Exec(ctx, `SELECT 1 FROM servers WHERE id='srv_c' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	digests := [2][32]byte{reenrollCredentialDigest(t, 111), reenrollCredentialDigest(t, 112)}
	var errs [2]error
	var versions [2]int
	var wg sync.WaitGroup
	for i := range digests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			versions[i], errs[i] = repo.Enroll(ctx, "srv_c", token, digests[i])
		}()
	}
	waitForLockWaiters(t, ctx, pool, "FROM servers WHERE id", 2)
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()

	winner := -1
	for i := range errs {
		switch {
		case errs[i] == nil:
			if winner != -1 {
				t.Fatalf("two winners")
			}
			winner = i
			if versions[i] != 2 {
				t.Fatalf("winner version=%d", versions[i])
			}
		case !errors.Is(errs[i], ErrServerAuthorizationDenied):
			t.Fatalf("loser err=%v, want ErrServerAuthorizationDenied", errs[i])
		}
	}
	if winner == -1 {
		t.Fatal("no winner")
	}
	loser := 1 - winner
	if _, err := repo.Authenticate(ctx, "srv_c", digests[winner]); err != nil {
		t.Fatalf("winner credential: %v", err)
	}
	for name, d := range map[string][32]byte{"loser": digests[loser], "old": oldCred} {
		if _, err := repo.Authenticate(ctx, "srv_c", d); !errors.Is(err, ErrServerAuthorizationDenied) {
			t.Fatalf("%s credential accepted: %v", name, err)
		}
	}
	var versionsN, active, enrolledV2 int
	if err := pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE status='active'), (SELECT count(*) FROM server_audit_events WHERE server_id='srv_c' AND action='enrolled' AND credential_version=2) FROM server_credentials WHERE server_id='srv_c'`).Scan(&versionsN, &active, &enrolledV2); err != nil {
		t.Fatal(err)
	}
	if versionsN != 2 || active != 1 || enrolledV2 != 1 {
		t.Fatalf("credentials=%d active=%d enrolled-v2 audits=%d", versionsN, active, enrolledV2)
	}
}

// TestReenrollConcurrentIssueExactlyOnePending races two administrators issuing
// a re-enrollment token for the same server: both block on the session row, so
// the second one sees the first one's pending token and is refused.
func TestReenrollConcurrentIssueExactlyOnePending(t *testing.T) {
	pool, repo, _, principal := freshServerRepository(t)
	ctx := context.Background()
	enrolledTestServer(t, repo, principal, "srv_i", 1, 2)
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx) //nolint:errcheck
	if _, err := holder.Exec(ctx, `SELECT 1 FROM admin_sessions WHERE id=$1 FOR UPDATE`, principal.SessionID); err != nil {
		t.Fatal(err)
	}
	var errs [2]error
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = repo.IssueReenrollment(ctx, principal, "srv_i", reenrollEnrollmentDigest(t, byte(120+i)), 900, "restore")
		}()
	}
	waitForLockWaiters(t, ctx, pool, "admin_sessions", 2)
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	var ok, pending int
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrReenrollmentPending):
			pending++
		default:
			t.Fatalf("unexpected err=%v", err)
		}
	}
	if ok != 1 || pending != 1 {
		t.Fatalf("ok=%d pending=%d", ok, pending)
	}
	var unconsumed int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM server_enrollment_tokens WHERE server_id='srv_i' AND consumed_at IS NULL`).Scan(&unconsumed); err != nil || unconsumed != 1 {
		t.Fatalf("unconsumed=%d err=%v", unconsumed, err)
	}
}

func TestListServersOneRowPerServerWithReenrollment(t *testing.T) {
	_, repo, _, principal := freshServerRepository(t)
	ctx := context.Background()
	enrolledTestServer(t, repo, principal, "srv_l", 1, 2)
	token := reenrollEnrollmentDigest(t, 130)
	if _, err := repo.IssueReenrollment(ctx, principal, "srv_l", token, 900, "restore"); err != nil {
		t.Fatal(err)
	}
	list, err := repo.ListServers(ctx, principal)
	if err != nil || len(list) != 1 || list[0].Enrollment != EnrollmentPending {
		t.Fatalf("pending: %+v %v", list, err)
	}
	if _, err := repo.Enroll(ctx, "srv_l", token, reenrollCredentialDigest(t, 131)); err != nil {
		t.Fatal(err)
	}
	list, err = repo.ListServers(ctx, principal)
	if err != nil || len(list) != 1 || list[0].Enrollment != EnrollmentConsumed || list[0].ActiveCredentialVersion != 2 {
		t.Fatalf("consumed: %+v %v", list, err)
	}
}

// holdServerRowLock holds FOR UPDATE on the server row in another tx until
// the returned release func runs.
func holdServerRowLock(t *testing.T, pool *pgxpool.Pool, serverID string) (release func()) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var id string
	if err := tx.QueryRow(ctx, `SELECT id FROM servers WHERE id=$1 FOR UPDATE`, serverID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return func() { _ = tx.Rollback(ctx) }
}

func TestEnrollBogusTokenRejectedWithoutServerLock(t *testing.T) {
	pool, repo, _, principal := freshServerRepository(t)
	_, _ = registerTestServer(t, repo, principal, "srv_lock", 1)
	release := holdServerRowLock(t, pool, "srv_lock")
	defer release()

	bogus := reenrollEnrollmentDigest(t, 99)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	_, err := repo.Enroll(ctx, "srv_lock", bogus, reenrollCredentialDigest(t, 98))
	if !errors.Is(err, ErrServerAuthorizationDenied) || time.Since(start) > time.Second {
		t.Fatalf("bogus enroll must be refused promptly without the row lock: err=%v after %v", err, time.Since(start))
	}
	// Unknown server too.
	if _, err := repo.Enroll(ctx, "srv_nope", bogus, reenrollCredentialDigest(t, 98)); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("unknown server: %v", err)
	}
	// A VALID token does queue on the lock (proves the lock is really held
	// and the fast path is what avoided it).
	short, cancel2 := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel2()
	if _, err := repo.Enroll(short, "srv_lock", reenrollEnrollmentDigest(t, 1), reenrollCredentialDigest(t, 97)); err == nil || errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("valid enroll should block on the held lock and hit the ctx deadline, got %v", err)
	}
}

func TestEnrollValidStillWorksAndRechecksUnderLock(t *testing.T) {
	pool, repo, _, principal := freshServerRepository(t)
	ctx := context.Background()
	_, _ = registerTestServer(t, repo, principal, "srv_race", 1)
	// Token consumed by someone else between the unlocked fast check and the lock.
	repo.failAfter = func(stage string) error {
		if stage == "enroll_fast_pass" {
			_, err := pool.Exec(ctx, `UPDATE server_enrollment_tokens SET consumed_at=clock_timestamp() WHERE server_id='srv_race'`)
			return err
		}
		return nil
	}
	if _, err := repo.Enroll(ctx, "srv_race", reenrollEnrollmentDigest(t, 1), reenrollCredentialDigest(t, 2)); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("token consumed after fast check must be refused under lock: %v", err)
	}
	var creds int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM server_credentials WHERE server_id='srv_race'`).Scan(&creds); err != nil || creds != 0 {
		t.Fatalf("credentials=%d err=%v", creds, err)
	}
	// Server revoked between fast check and lock.
	_, _ = registerTestServer(t, repo, principal, "srv_race2", 3)
	repo.failAfter = func(stage string) error {
		if stage == "enroll_fast_pass" {
			return repo.RevokeServer(ctx, principal, "srv_race2", "revoked mid-enroll")
		}
		return nil
	}
	if _, err := repo.Enroll(ctx, "srv_race2", reenrollEnrollmentDigest(t, 3), reenrollCredentialDigest(t, 4)); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("server revoked after fast check must be refused under lock: %v", err)
	}
	// Plain valid enroll still works.
	repo.failAfter = nil
	_, _ = registerTestServer(t, repo, principal, "srv_ok2", 5)
	if v, err := repo.Enroll(ctx, "srv_ok2", reenrollEnrollmentDigest(t, 5), reenrollCredentialDigest(t, 6)); err != nil || v != 1 {
		t.Fatalf("valid enroll: v=%d err=%v", v, err)
	}
}
