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

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// freshServerRepository bootstraps a single owner/administrator (via
// freshAuthRepository) and returns a live, validated principal for that
// owner alongside a truncated server repository. Tests that need a second,
// distinct owner use secondOwnerPrincipal.
func freshServerRepository(t *testing.T) (*pgxpool.Pool, *ServerRepository, *AuthRepository, auth.Principal) {
	t.Helper()
	pool, authRepo, user, _ := freshAuthRepository(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `TRUNCATE server_audit_events, server_credentials, server_enrollment_tokens, servers RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	token := testToken(t, 200)
	if _, err := authRepo.CreateSession(ctx, "server-owner-session-"+t.Name(), token, user, nil, "server-owner-login-"+t.Name(), "server-owner-limit-"+t.Name()); err != nil {
		t.Fatal(err)
	}
	principal, err := authRepo.ValidateSession(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	return pool, NewServerRepository(pool), authRepo, principal
}

// secondOwnerPrincipal inserts a second owner and administrator directly
// (Bootstrap only ever provisions one owner per installation) and returns a
// live, validated principal for it, so cross-owner authorization tests can
// exercise a genuinely distinct owner scope.
func secondOwnerPrincipal(t *testing.T, pool *pgxpool.Pool, authRepo *AuthRepository, seed byte) auth.Principal {
	t.Helper()
	ctx := context.Background()
	ownerID := "owner-b-" + t.Name()
	userID := "user-b-" + t.Name()
	if _, err := pool.Exec(ctx, `INSERT INTO owners(id) VALUES($1)`, ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,owner_id,username,role,password_phc,password_policy_revision) VALUES($1,$2,'owner-b','owner',$3,1)`, userID, ownerID, strings.Repeat("x", 68)); err != nil {
		t.Fatal(err)
	}
	token := testToken(t, seed)
	user := LoginUser{ID: userID, OwnerID: ownerID, AuthRevision: 1, Active: true}
	if _, err := authRepo.CreateSession(ctx, "owner-b-session-"+t.Name(), token, user, nil, "owner-b-login-"+t.Name(), "owner-b-limit-"+t.Name()); err != nil {
		t.Fatal(err)
	}
	p, err := authRepo.ValidateSession(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func registerTestServer(t *testing.T, repo *ServerRepository, principal auth.Principal, id string, enrollmentSeed byte) (server.EnrollmentToken, server.Server) {
	t.Helper()
	enrollment, err := server.NewEnrollmentToken(bytes.NewReader(bytes.Repeat([]byte{enrollmentSeed}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	enrollmentDigest, err := server.EnrollmentTokenDigest(enrollment)
	if err != nil {
		t.Fatal(err)
	}
	s, _, err := repo.RegisterServer(context.Background(), principal, RegisterServerInput{ServerID: id, Name: "hetzner-" + id, ProtocolVersion: server.ProtocolVersion, EnrollmentDigest: enrollmentDigest, EnrollmentTTLSeconds: 900})
	if err != nil {
		t.Fatal(err)
	}
	return enrollment, s
}

func TestServerRegisterAndEnrollAndAuthenticate(t *testing.T) {
	pool, repo, _, principal := freshServerRepository(t)
	ctx := context.Background()
	enrollment, registered := registerTestServer(t, repo, principal, "server_a", 1)
	if registered.Status != server.StatusActive || registered.OwnerID != principal.OwnerID {
		t.Fatalf("registered=%+v", registered)
	}

	credential, err := server.NewCredentialToken(bytes.NewReader(bytes.Repeat([]byte{2}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	enrollmentDigest, err := server.EnrollmentTokenDigest(enrollment)
	if err != nil {
		t.Fatal(err)
	}
	credentialDigest, err := server.CredentialTokenDigest(credential)
	if err != nil {
		t.Fatal(err)
	}
	version, err := repo.Enroll(ctx, "server_a", enrollmentDigest, credentialDigest)
	if err != nil || version != 1 {
		t.Fatalf("version=%d err=%v", version, err)
	}

	authenticated, err := repo.Authenticate(ctx, "server_a", credentialDigest)
	if err != nil || authenticated.ServerID != "server_a" || authenticated.OwnerID != principal.OwnerID || authenticated.CredentialVersion != 1 {
		t.Fatalf("authenticated=%+v err=%v", authenticated, err)
	}

	wrongDigest := credentialDigest
	wrongDigest[0] ^= 0xFF
	if _, err = repo.Authenticate(ctx, "server_a", wrongDigest); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("wrong credential digest=%v", err)
	}
	if _, err = repo.Authenticate(ctx, "server_unknown", credentialDigest); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("unknown server=%v", err)
	}

	var events int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM server_audit_events WHERE server_id='server_a'`).Scan(&events); err != nil || events != 2 {
		t.Fatalf("events=%d err=%v", events, err)
	}
}

func TestServerEnrollRejectsDigestMismatchWithoutMutation(t *testing.T) {
	pool, repo, _, principal := freshServerRepository(t)
	ctx := context.Background()
	_, _ = registerTestServer(t, repo, principal, "server_b", 3)

	wrongEnrollment, err := server.NewEnrollmentToken(bytes.NewReader(bytes.Repeat([]byte{99}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	wrongDigest, err := server.EnrollmentTokenDigest(wrongEnrollment)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := server.NewCredentialToken(bytes.NewReader(bytes.Repeat([]byte{4}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	credentialDigest, err := server.CredentialTokenDigest(credential)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repo.Enroll(ctx, "server_b", wrongDigest, credentialDigest); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("mismatched digest enroll=%v", err)
	}
	var credentials int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM server_credentials WHERE server_id='server_b'`).Scan(&credentials); err != nil || credentials != 0 {
		t.Fatalf("credentials=%d err=%v", credentials, err)
	}
	var consumedAt *time.Time
	if err = pool.QueryRow(ctx, `SELECT consumed_at FROM server_enrollment_tokens WHERE server_id='server_b'`).Scan(&consumedAt); err != nil || consumedAt != nil {
		t.Fatalf("token consumed after rejection consumedAt=%v err=%v", consumedAt, err)
	}
	// A rejected Enroll must write zero audit rows: an unauthenticated
	// caller who only knows (or guesses) a server ID could otherwise grow
	// server_audit_events without bound by retrying with a wrong enrollment
	// digest. Only registerTestServer's own "register" row exists.
	var events int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM server_audit_events WHERE server_id='server_b'`).Scan(&events); err != nil || events != 1 {
		t.Fatalf("audit events after rejected enroll=%d err=%v", events, err)
	}
}

func TestServerEnrollRejectsExpiredToken(t *testing.T) {
	pool, repo, _, principal := freshServerRepository(t)
	ctx := context.Background()
	enrollment, _ := registerTestServer(t, repo, principal, "server_c", 5)
	if _, err := pool.Exec(ctx, `UPDATE server_enrollment_tokens SET expires_at=issued_at+interval '1 microsecond' WHERE server_id='server_c'`); err != nil {
		t.Fatal(err)
	}
	enrollmentDigest, err := server.EnrollmentTokenDigest(enrollment)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := server.NewCredentialToken(bytes.NewReader(bytes.Repeat([]byte{6}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	credentialDigest, err := server.CredentialTokenDigest(credential)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repo.Enroll(ctx, "server_c", enrollmentDigest, credentialDigest); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("expired enroll=%v", err)
	}
}

func TestServerEnrollRejectsReplayAfterConsumption(t *testing.T) {
	pool, repo, _, principal := freshServerRepository(t)
	_ = pool
	ctx := context.Background()
	enrollment, _ := registerTestServer(t, repo, principal, "server_d", 7)
	enrollmentDigest, err := server.EnrollmentTokenDigest(enrollment)
	if err != nil {
		t.Fatal(err)
	}
	firstCredential, err := server.NewCredentialToken(bytes.NewReader(bytes.Repeat([]byte{8}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	firstDigest, err := server.CredentialTokenDigest(firstCredential)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repo.Enroll(ctx, "server_d", enrollmentDigest, firstDigest); err != nil {
		t.Fatal(err)
	}
	secondCredential, err := server.NewCredentialToken(bytes.NewReader(bytes.Repeat([]byte{9}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	secondDigest, err := server.CredentialTokenDigest(secondCredential)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repo.Enroll(ctx, "server_d", enrollmentDigest, secondDigest); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("replayed enroll=%v", err)
	}
	// Reissue must also be rejected once the token has been consumed.
	if _, err = repo.IssueEnrollment(ctx, principal, "server_d", enrollmentDigest, 900); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("reissue after enrollment=%v", err)
	}
}

func TestServerIssueEnrollmentReissueInvalidatesPriorToken(t *testing.T) {
	pool, repo, _, principal := freshServerRepository(t)
	_ = pool
	ctx := context.Background()
	oldEnrollment, _ := registerTestServer(t, repo, principal, "server_e", 10)
	newEnrollment, err := server.NewEnrollmentToken(bytes.NewReader(bytes.Repeat([]byte{11}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	newDigest, err := server.EnrollmentTokenDigest(newEnrollment)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repo.IssueEnrollment(ctx, principal, "server_e", newDigest, 900); err != nil {
		t.Fatal(err)
	}

	credential, err := server.NewCredentialToken(bytes.NewReader(bytes.Repeat([]byte{12}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	credentialDigest, err := server.CredentialTokenDigest(credential)
	if err != nil {
		t.Fatal(err)
	}
	oldDigest, err := server.EnrollmentTokenDigest(oldEnrollment)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repo.Enroll(ctx, "server_e", oldDigest, credentialDigest); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("stale enrollment token accepted: %v", err)
	}
	if _, err = repo.Enroll(ctx, "server_e", newDigest, credentialDigest); err != nil {
		t.Fatalf("reissued enrollment token rejected: %v", err)
	}
}

func TestServerHeartbeatSucceedsAndAdvancesLastHeartbeatAt(t *testing.T) {
	pool, repo, _, principal := freshServerRepository(t)
	ctx := context.Background()
	enrollment, _ := registerTestServer(t, repo, principal, "server_h1", 40)
	enrollmentDigest, err := server.EnrollmentTokenDigest(enrollment)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := server.NewCredentialToken(bytes.NewReader(bytes.Repeat([]byte{41}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	credentialDigest, err := server.CredentialTokenDigest(credential)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repo.Enroll(ctx, "server_h1", enrollmentDigest, credentialDigest); err != nil {
		t.Fatal(err)
	}
	var before *time.Time
	if err = pool.QueryRow(ctx, `SELECT last_heartbeat_at FROM servers WHERE id='server_h1'`).Scan(&before); err != nil || before != nil {
		t.Fatalf("last_heartbeat_at before first heartbeat=%v err=%v", before, err)
	}
	if err = repo.Heartbeat(ctx, "server_h1", credentialDigest); err != nil {
		t.Fatalf("heartbeat=%v", err)
	}
	var first *time.Time
	if err = pool.QueryRow(ctx, `SELECT last_heartbeat_at FROM servers WHERE id='server_h1'`).Scan(&first); err != nil || first == nil {
		t.Fatalf("last_heartbeat_at after first heartbeat=%v err=%v", first, err)
	}
	if err = repo.Heartbeat(ctx, "server_h1", credentialDigest); err != nil {
		t.Fatalf("second heartbeat=%v", err)
	}
	var second *time.Time
	if err = pool.QueryRow(ctx, `SELECT last_heartbeat_at FROM servers WHERE id='server_h1'`).Scan(&second); err != nil || second == nil || second.Before(*first) {
		t.Fatalf("last_heartbeat_at not monotonic: first=%v second=%v err=%v", first, second, err)
	}
}

// TestServerHeartbeatRejectsInvalidCredentialWithoutMutation proves a
// rejected Heartbeat call -- unknown server, wrong credential digest, or a
// revoked/absent active credential -- writes zero audit rows and mutates
// nothing (not even last_heartbeat_at). Before this fix, every rejected
// heartbeat wrote a "heartbeat_rejected" server_audit_events row; since
// Heartbeat takes no principal and is reachable by anyone who merely knows
// a server ID, that was an unauthenticated, unrate-limited write-
// amplification path.
func TestServerHeartbeatRejectsInvalidCredentialWithoutMutation(t *testing.T) {
	pool, repo, _, principal := freshServerRepository(t)
	ctx := context.Background()
	enrollment, _ := registerTestServer(t, repo, principal, "server_h2", 42)
	enrollmentDigest, err := server.EnrollmentTokenDigest(enrollment)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := server.NewCredentialToken(bytes.NewReader(bytes.Repeat([]byte{43}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	credentialDigest, err := server.CredentialTokenDigest(credential)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repo.Enroll(ctx, "server_h2", enrollmentDigest, credentialDigest); err != nil {
		t.Fatal(err)
	}

	wrongDigest := credentialDigest
	wrongDigest[0] ^= 0xFF
	if err = repo.Heartbeat(ctx, "server_h2", wrongDigest); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("wrong credential heartbeat=%v", err)
	}
	if err = repo.Heartbeat(ctx, "server_unknown_h2", credentialDigest); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("unknown server heartbeat=%v", err)
	}

	var lastHeartbeat *time.Time
	if err = pool.QueryRow(ctx, `SELECT last_heartbeat_at FROM servers WHERE id='server_h2'`).Scan(&lastHeartbeat); err != nil || lastHeartbeat != nil {
		t.Fatalf("last_heartbeat_at mutated by rejected heartbeat=%v err=%v", lastHeartbeat, err)
	}
	var events int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM server_audit_events WHERE server_id='server_h2'`).Scan(&events); err != nil || events != 2 {
		// register + enrolled only; no heartbeat_rejected rows.
		t.Fatalf("audit events after rejected heartbeats=%d err=%v", events, err)
	}
}

func TestServerRotateCredentialAtomicity(t *testing.T) {
	pool, repo, _, principal := freshServerRepository(t)
	_ = pool
	ctx := context.Background()
	enrollment, _ := registerTestServer(t, repo, principal, "server_f", 13)
	enrollmentDigest, err := server.EnrollmentTokenDigest(enrollment)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := server.NewCredentialToken(bytes.NewReader(bytes.Repeat([]byte{14}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	credentialDigest, err := server.CredentialTokenDigest(credential)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repo.Enroll(ctx, "server_f", enrollmentDigest, credentialDigest); err != nil {
		t.Fatal(err)
	}

	rotated, err := server.NewCredentialToken(bytes.NewReader(bytes.Repeat([]byte{15}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	rotatedDigest, err := server.CredentialTokenDigest(rotated)
	if err != nil {
		t.Fatal(err)
	}
	version, err := repo.RotateCredential(ctx, principal, "server_f", rotatedDigest)
	if err != nil || version != 2 {
		t.Fatalf("version=%d err=%v", version, err)
	}

	if _, err = repo.Authenticate(ctx, "server_f", credentialDigest); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("old credential still authenticates: %v", err)
	}
	authenticated, err := repo.Authenticate(ctx, "server_f", rotatedDigest)
	if err != nil || authenticated.CredentialVersion != 2 {
		t.Fatalf("authenticated=%+v err=%v", authenticated, err)
	}
}

func TestServerRevokeStopsAuthenticationAndMutation(t *testing.T) {
	pool, repo, _, principal := freshServerRepository(t)
	_ = pool
	ctx := context.Background()
	enrollment, _ := registerTestServer(t, repo, principal, "server_g", 16)
	enrollmentDigest, err := server.EnrollmentTokenDigest(enrollment)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := server.NewCredentialToken(bytes.NewReader(bytes.Repeat([]byte{17}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	credentialDigest, err := server.CredentialTokenDigest(credential)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repo.Enroll(ctx, "server_g", enrollmentDigest, credentialDigest); err != nil {
		t.Fatal(err)
	}
	if err = repo.RevokeServer(ctx, principal, "server_g", "compromised host"); err != nil {
		t.Fatal(err)
	}
	if err = repo.RevokeServer(ctx, principal, "server_g", "double revoke"); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("double revoke=%v", err)
	}
	if _, err = repo.Authenticate(ctx, "server_g", credentialDigest); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("revoked server authenticated: %v", err)
	}
	rotated, err := server.NewCredentialToken(bytes.NewReader(bytes.Repeat([]byte{18}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	rotatedDigest, err := server.CredentialTokenDigest(rotated)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repo.RotateCredential(ctx, principal, "server_g", rotatedDigest); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("rotate on revoked server=%v", err)
	}
}

func TestServerConcurrentDoubleEnrollExactlyOneWinner(t *testing.T) {
	pool, repo, _, principal := freshServerRepository(t)
	_ = pool
	ctx := context.Background()
	enrollment, _ := registerTestServer(t, repo, principal, "server_h", 19)
	enrollmentDigest, err := server.EnrollmentTokenDigest(enrollment)
	if err != nil {
		t.Fatal(err)
	}
	credentialA, err := server.NewCredentialToken(bytes.NewReader(bytes.Repeat([]byte{20}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	digestA, err := server.CredentialTokenDigest(credentialA)
	if err != nil {
		t.Fatal(err)
	}
	credentialB, err := server.NewCredentialToken(bytes.NewReader(bytes.Repeat([]byte{21}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	digestB, err := server.CredentialTokenDigest(credentialB)
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	var arrived, wg sync.WaitGroup
	arrived.Add(2)
	for _, d := range [][32]byte{digestA, digestB} {
		wg.Add(1)
		go func(digest [32]byte) {
			defer wg.Done()
			arrived.Done()
			<-start
			_, enrollErr := repo.Enroll(context.Background(), "server_h", enrollmentDigest, digest)
			results <- enrollErr
		}(d)
	}
	arrived.Wait() // force both goroutines to reach the barrier before release
	close(start)
	wg.Wait()
	close(results)
	var succeeded, failed int
	for err := range results {
		if err == nil {
			succeeded++
		} else if errors.Is(err, ErrServerAuthorizationDenied) {
			failed++
		} else {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if succeeded != 1 || failed != 1 {
		t.Fatalf("succeeded=%d failed=%d", succeeded, failed)
	}
	var activeCredentials int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM server_credentials WHERE server_id='server_h' AND status='active'`).Scan(&activeCredentials); err != nil || activeCredentials != 1 {
		t.Fatalf("active credentials=%d err=%v", activeCredentials, err)
	}
}

// TestServerConcurrentEnrollVsIssueEnrollmentSerializes exercises the race
// between an agent consuming the current enrollment token and an
// administrator reissuing it. Both paths lock the servers row FOR UPDATE
// first, so PostgreSQL fully serializes them; either interleaving is a
// correct outcome, but the two operations' results and the final row state
// must be mutually consistent (never a consumed token that was also
// reissued, and never a credential minted from a token that was overwritten
// first).
func TestServerConcurrentEnrollVsIssueEnrollmentSerializes(t *testing.T) {
	pool, repo, _, principal := freshServerRepository(t)
	ctx := context.Background()
	originalEnrollment, _ := registerTestServer(t, repo, principal, "server_m", 30)
	originalDigest, err := server.EnrollmentTokenDigest(originalEnrollment)
	if err != nil {
		t.Fatal(err)
	}
	newEnrollment, err := server.NewEnrollmentToken(bytes.NewReader(bytes.Repeat([]byte{31}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	newDigest, err := server.EnrollmentTokenDigest(newEnrollment)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := server.NewCredentialToken(bytes.NewReader(bytes.Repeat([]byte{32}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	credentialDigest, err := server.CredentialTokenDigest(credential)
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	var arrived, wg sync.WaitGroup
	arrived.Add(2)
	var enrollErr, issueErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		arrived.Done()
		<-start
		_, enrollErr = repo.Enroll(context.Background(), "server_m", originalDigest, credentialDigest)
	}()
	go func() {
		defer wg.Done()
		arrived.Done()
		<-start
		_, issueErr = repo.IssueEnrollment(context.Background(), principal, "server_m", newDigest, 900)
	}()
	arrived.Wait()
	close(start)
	wg.Wait()

	var consumedAt *time.Time
	var storedDigest []byte
	if err = pool.QueryRow(ctx, `SELECT consumed_at,digest FROM server_enrollment_tokens WHERE server_id='server_m'`).Scan(&consumedAt, &storedDigest); err != nil {
		t.Fatal(err)
	}
	var activeCredentials int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM server_credentials WHERE server_id='server_m' AND status='active'`).Scan(&activeCredentials); err != nil {
		t.Fatal(err)
	}

	switch {
	case enrollErr == nil:
		// Enroll won: the token must be consumed with the original digest
		// still on record, exactly one active credential exists, and the
		// reissue attempt must have found a consumed token and failed.
		if consumedAt == nil || !bytes.Equal(storedDigest, originalDigest[:]) || activeCredentials != 1 {
			t.Fatalf("enroll won but state inconsistent: consumedAt=%v digestMatchesOriginal=%v activeCredentials=%d", consumedAt, bytes.Equal(storedDigest, originalDigest[:]), activeCredentials)
		}
		if !errors.Is(issueErr, ErrServerAuthorizationDenied) {
			t.Fatalf("reissue after enroll should fail, got %v", issueErr)
		}
	case issueErr == nil:
		// Reissue won: the token must carry the new digest, be unconsumed,
		// and no credential can exist because enroll's digest comparison
		// (against the now-stale original digest) must have failed.
		if consumedAt != nil || !bytes.Equal(storedDigest, newDigest[:]) || activeCredentials != 0 {
			t.Fatalf("reissue won but state inconsistent: consumedAt=%v digestMatchesNew=%v activeCredentials=%d", consumedAt, bytes.Equal(storedDigest, newDigest[:]), activeCredentials)
		}
		if !errors.Is(enrollErr, ErrServerAuthorizationDenied) {
			t.Fatalf("enroll after reissue should fail, got %v", enrollErr)
		}
	default:
		t.Fatalf("both operations failed: enrollErr=%v issueErr=%v", enrollErr, issueErr)
	}
}

// TestServerConcurrentEnrollVsRevokeServerSerializes exercises the race
// between an agent enrolling and an administrator revoking the same server.
// Whichever wins the servers row lock first determines the outcome, but a
// revoked server must never end up with an active credential.
func TestServerConcurrentEnrollVsRevokeServerSerializes(t *testing.T) {
	pool, repo, _, principal := freshServerRepository(t)
	ctx := context.Background()
	enrollment, _ := registerTestServer(t, repo, principal, "server_n", 33)
	enrollmentDigest, err := server.EnrollmentTokenDigest(enrollment)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := server.NewCredentialToken(bytes.NewReader(bytes.Repeat([]byte{34}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	credentialDigest, err := server.CredentialTokenDigest(credential)
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	var arrived, wg sync.WaitGroup
	arrived.Add(2)
	var enrollErr, revokeErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		arrived.Done()
		<-start
		_, enrollErr = repo.Enroll(context.Background(), "server_n", enrollmentDigest, credentialDigest)
	}()
	go func() {
		defer wg.Done()
		arrived.Done()
		<-start
		revokeErr = repo.RevokeServer(context.Background(), principal, "server_n", "concurrent revoke")
	}()
	arrived.Wait()
	close(start)
	wg.Wait()

	var status string
	if err = pool.QueryRow(ctx, `SELECT status FROM servers WHERE id='server_n'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	var activeCredentials int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM server_credentials WHERE server_id='server_n' AND status='active'`).Scan(&activeCredentials); err != nil {
		t.Fatal(err)
	}
	// The invariant that must hold regardless of interleaving: a revoked
	// server (both goroutines run against an initially active server, and
	// revoke always succeeds below) never has an active credential --
	// RevokeServer revokes any credential enroll managed to create.
	if activeCredentials != 0 {
		t.Fatalf("revoked server has an active credential: activeCredentials=%d", activeCredentials)
	}
	if revokeErr != nil {
		t.Fatalf("revoke should always succeed once (server was active): %v", revokeErr)
	}
	if status != "revoked" {
		t.Fatalf("server not revoked: status=%s", status)
	}
	var totalCredentials int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM server_credentials WHERE server_id='server_n'`).Scan(&totalCredentials); err != nil {
		t.Fatal(err)
	}
	if enrollErr == nil && totalCredentials != 1 {
		t.Fatalf("enroll succeeded but no credential row recorded (any status): %d", totalCredentials)
	}
	if enrollErr != nil && totalCredentials != 0 {
		t.Fatalf("enroll failed but a credential row exists: %d", totalCredentials)
	}
}

// TestServerCrossOwnerIsolationDeniesAdministrativeOperations proves an
// administrator authenticated for owner B cannot IssueEnrollment, rotate, or
// revoke a server owned by owner A: every attempt returns the same generic
// error as an unknown server ID, and none of them mutate the server or
// leave an audit trail.
func TestServerCrossOwnerIsolationDeniesAdministrativeOperations(t *testing.T) {
	pool, repo, authRepo, ownerA := freshServerRepository(t)
	ctx := context.Background()
	enrollment, _ := registerTestServer(t, repo, ownerA, "server_x", 40)
	ownerB := secondOwnerPrincipal(t, pool, authRepo, 41)

	enrollmentDigest, err := server.EnrollmentTokenDigest(enrollment)
	if err != nil {
		t.Fatal(err)
	}
	unknownErr := func() error {
		_, err := repo.IssueEnrollment(ctx, ownerB, "server_unknown", enrollmentDigest, 900)
		return err
	}()
	if !errors.Is(unknownErr, ErrServerAuthorizationDenied) {
		t.Fatalf("unknown-server baseline=%v", unknownErr)
	}
	if _, err = repo.IssueEnrollment(ctx, ownerB, "server_x", enrollmentDigest, 900); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("cross-owner issue enrollment=%v", err)
	}

	credential, err := server.NewCredentialToken(bytes.NewReader(bytes.Repeat([]byte{42}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	credentialDigest, err := server.CredentialTokenDigest(credential)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repo.RotateCredential(ctx, ownerB, "server_x", credentialDigest); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("cross-owner rotate=%v", err)
	}
	if err = repo.RevokeServer(ctx, ownerB, "server_x", "cross-owner takeover attempt"); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("cross-owner revoke=%v", err)
	}

	var status, owner string
	if err = pool.QueryRow(ctx, `SELECT status,owner_id FROM servers WHERE id='server_x'`).Scan(&status, &owner); err != nil {
		t.Fatal(err)
	}
	if status != "active" || owner != ownerA.OwnerID {
		t.Fatalf("server mutated by cross-owner attempts: status=%s owner=%s", status, owner)
	}
	var credentials int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM server_credentials WHERE server_id='server_x'`).Scan(&credentials); err != nil || credentials != 0 {
		t.Fatalf("credential created by cross-owner rotate: %d err=%v", credentials, err)
	}
	var auditEvents int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM server_audit_events WHERE server_id='server_x'`).Scan(&auditEvents); err != nil || auditEvents != 1 {
		t.Fatalf("cross-owner attempts left an audit trail: events=%d err=%v", auditEvents, err)
	}
}

// TestServerAdministrativeMethodsRejectInvalidPrincipal covers the
// structural principal.Validate() gate shared by every administrative
// method, independent of any live session.
func TestServerAdministrativeMethodsRejectInvalidPrincipal(t *testing.T) {
	_, repo, _, _ := freshServerRepository(t)
	ctx := context.Background()
	var invalid auth.Principal
	if _, _, err := repo.RegisterServer(ctx, invalid, RegisterServerInput{ServerID: "server_y", Name: "n", ProtocolVersion: server.ProtocolVersion, EnrollmentDigest: [32]byte{1}, EnrollmentTTLSeconds: 900}); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("invalid principal register=%v", err)
	}
	if _, err := repo.IssueEnrollment(ctx, invalid, "server_y", [32]byte{1}, 900); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("invalid principal issue enrollment=%v", err)
	}
	if _, err := repo.RotateCredential(ctx, invalid, "server_y", [32]byte{1}); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("invalid principal rotate=%v", err)
	}
	if err := repo.RevokeServer(ctx, invalid, "server_y", "reason"); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("invalid principal revoke=%v", err)
	}
}

func TestServerAuditWriteFailureRollsBackMutation(t *testing.T) {
	pool, repo, _, principal := freshServerRepository(t)
	ctx := context.Background()
	enrollment, err := server.NewEnrollmentToken(bytes.NewReader(bytes.Repeat([]byte{22}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	enrollmentDigest, err := server.EnrollmentTokenDigest(enrollment)
	if err != nil {
		t.Fatal(err)
	}
	repo.failAfter = func(stage string) error {
		if stage == "audit_insert" {
			return errors.New("injected audit failure")
		}
		return nil
	}
	_, _, err = repo.RegisterServer(ctx, principal, RegisterServerInput{ServerID: "server_i", Name: "hetzner-i", ProtocolVersion: server.ProtocolVersion, EnrollmentDigest: enrollmentDigest, EnrollmentTTLSeconds: 900})
	if err == nil {
		t.Fatal("injected audit failure did not surface")
	}
	repo.failAfter = nil
	var servers, tokens, audits int
	if err = pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM servers),(SELECT count(*) FROM server_enrollment_tokens),(SELECT count(*) FROM server_audit_events)`).Scan(&servers, &tokens, &audits); err != nil {
		t.Fatal(err)
	}
	if servers != 0 || tokens != 0 || audits != 0 {
		t.Fatalf("partial write survived rollback: servers=%d tokens=%d audits=%d", servers, tokens, audits)
	}

	// Enroll's credential insert must roll back together with the token
	// consumption when the audit write fails.
	_, registered := registerTestServer(t, repo, principal, "server_j", 23)
	_ = registered
	credential, err := server.NewCredentialToken(bytes.NewReader(bytes.Repeat([]byte{24}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	credentialDigest, err := server.CredentialTokenDigest(credential)
	if err != nil {
		t.Fatal(err)
	}
	jEnrollment, err := server.NewEnrollmentToken(bytes.NewReader(bytes.Repeat([]byte{23}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	jEnrollmentDigest, err := server.EnrollmentTokenDigest(jEnrollment)
	if err != nil {
		t.Fatal(err)
	}
	repo.failAfter = func(stage string) error {
		if stage == "credential_insert" {
			return errors.New("injected credential failure")
		}
		return nil
	}
	if _, err = repo.Enroll(ctx, "server_j", jEnrollmentDigest, credentialDigest); err == nil {
		t.Fatal("injected credential failure did not surface")
	}
	repo.failAfter = nil
	var consumedAt *time.Time
	if err = pool.QueryRow(ctx, `SELECT consumed_at FROM server_enrollment_tokens WHERE server_id='server_j'`).Scan(&consumedAt); err != nil || consumedAt != nil {
		t.Fatalf("enrollment token consumed despite rollback: consumedAt=%v err=%v", consumedAt, err)
	}
	var credentials int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM server_credentials WHERE server_id='server_j'`).Scan(&credentials); err != nil || credentials != 0 {
		t.Fatalf("credential row survived rollback: %d err=%v", credentials, err)
	}
}

func TestServerImmutabilityTriggersRejectDirectSQL(t *testing.T) {
	pool, repo, _, principal := freshServerRepository(t)
	ctx := context.Background()
	enrollment, _ := registerTestServer(t, repo, principal, "server_k", 25)
	enrollmentDigest, err := server.EnrollmentTokenDigest(enrollment)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := server.NewCredentialToken(bytes.NewReader(bytes.Repeat([]byte{26}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	credentialDigest, err := server.CredentialTokenDigest(credential)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repo.Enroll(ctx, "server_k", enrollmentDigest, credentialDigest); err != nil {
		t.Fatal(err)
	}

	if _, err = pool.Exec(ctx, `UPDATE servers SET name='renamed' WHERE id='server_k'`); err == nil {
		t.Fatal("server identity mutation succeeded")
	}
	// Revoking twice via direct SQL: first succeeds (legal active->revoked),
	// the immediate second attempt must be rejected (revoked->revoked).
	if _, err = pool.Exec(ctx, `UPDATE servers SET status='revoked',revoked_at=clock_timestamp() WHERE id='server_k'`); err != nil {
		t.Fatalf("legal revoke transition rejected: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE servers SET status='revoked',revoked_at=clock_timestamp() WHERE id='server_k'`); err == nil {
		t.Fatal("revoked->revoked transition succeeded")
	}
	if _, err = pool.Exec(ctx, `UPDATE servers SET status='active',revoked_at=NULL WHERE id='server_k'`); err == nil {
		t.Fatal("revoked->active transition succeeded")
	}
	if _, err = pool.Exec(ctx, `DELETE FROM servers WHERE id='server_k'`); err == nil {
		t.Fatal("server delete succeeded")
	}
	if _, err = pool.Exec(ctx, `UPDATE server_credentials SET version=99 WHERE server_id='server_k'`); err == nil {
		t.Fatal("credential identity mutation succeeded")
	}
	if _, err = pool.Exec(ctx, `DELETE FROM server_credentials WHERE server_id='server_k'`); err == nil {
		t.Fatal("credential delete succeeded")
	}
	if _, err = pool.Exec(ctx, `UPDATE server_enrollment_tokens SET server_id='server_other' WHERE server_id='server_k'`); err == nil {
		t.Fatal("enrollment token server mutation succeeded")
	}
	if _, err = pool.Exec(ctx, `DELETE FROM server_enrollment_tokens WHERE server_id='server_k'`); err == nil {
		t.Fatal("enrollment token delete succeeded")
	}
	var auditID int64
	if err = pool.QueryRow(ctx, `SELECT id FROM server_audit_events WHERE server_id='server_k' LIMIT 1`).Scan(&auditID); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE server_audit_events SET result='failure' WHERE id=$1`, auditID); err == nil {
		t.Fatal("audit event mutation succeeded")
	}
	if _, err = pool.Exec(ctx, `DELETE FROM server_audit_events WHERE id=$1`, auditID); err == nil {
		t.Fatal("audit event delete succeeded")
	}
}

func TestServerOneActiveCredentialIndexRejectsDirectDuplicate(t *testing.T) {
	pool, repo, _, principal := freshServerRepository(t)
	ctx := context.Background()
	enrollment, _ := registerTestServer(t, repo, principal, "server_l", 27)
	enrollmentDigest, err := server.EnrollmentTokenDigest(enrollment)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := server.NewCredentialToken(bytes.NewReader(bytes.Repeat([]byte{28}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	credentialDigest, err := server.CredentialTokenDigest(credential)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repo.Enroll(ctx, "server_l", enrollmentDigest, credentialDigest); err != nil {
		t.Fatal(err)
	}
	otherDigest := credentialDigest
	otherDigest[0] ^= 0xFF
	if _, err = pool.Exec(ctx, `INSERT INTO server_credentials(server_id,version,digest,status) VALUES('server_l',2,$1,'active')`, otherDigest[:]); err == nil {
		t.Fatal("second active credential accepted")
	}
}

// TestValidReason mirrors the server_audit_events.reason CHECK constraint
// directly, with no database involved.
func TestValidReason(t *testing.T) {
	cases := []struct {
		reason string
		want   bool
	}{
		{"", true},
		{"compromised host", true},
		{strings.Repeat("a", 512), true},
		{strings.Repeat("a", 513), false},
		{"bad\x00reason", false},
		{"bad\x7freason", false},
		{"newline\nreason", false},
		{"tab\treason", false},
	}
	for _, c := range cases {
		if got := validReason(c.reason); got != c.want {
			t.Fatalf("validReason(%q)=%v want %v", c.reason, got, c.want)
		}
	}
}

// TestServerPgErrorClassification verifies isForeignKeyViolation and
// isUniqueViolation classify SQLSTATEs correctly and do not overlap. This is
// exercised directly (no database) because RegisterServer's owner now comes
// exclusively from a live-session-validated principal (see
// TestServerCrossOwnerIsolationDeniesAdministrativeOperations and
// lockAdministratorSession): the owners table foreign key that
// isForeignKeyViolation maps to a generic error can no longer be reached
// with an actually-unknown owner through the public RegisterServer path,
// since a valid principal already implies its owner row exists. The mapping
// remains as defense-in-depth against any future caller that revalidates
// the principal more loosely.
// TestServerListServersOwnerScopedFieldsAndEnrollmentStates exercises the
// web admin /servers page's read path directly against the repository:
// every derived EnrollmentState, the active-credential version, and -- most
// importantly -- that a server belonging to a different owner never appears
// (cross-owner isolation).
func TestServerListServersOwnerScopedFieldsAndEnrollmentStates(t *testing.T) {
	pool, repo, authRepo, principal := freshServerRepository(t)
	ctx := context.Background()

	registerTestServer(t, repo, principal, "server_pending", 10)

	enrollment, _ := registerTestServer(t, repo, principal, "server_consumed", 11)
	credential, err := server.NewCredentialToken(bytes.NewReader(bytes.Repeat([]byte{12}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	enrollmentDigest, err := server.EnrollmentTokenDigest(enrollment)
	if err != nil {
		t.Fatal(err)
	}
	credentialDigest, err := server.CredentialTokenDigest(credential)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repo.Enroll(ctx, "server_consumed", enrollmentDigest, credentialDigest); err != nil {
		t.Fatal(err)
	}

	registerTestServer(t, repo, principal, "server_expired", 13)
	if _, err = pool.Exec(ctx, `UPDATE server_enrollment_tokens SET issued_at=clock_timestamp()-interval '1 hour', expires_at=clock_timestamp()-interval '1 minute' WHERE server_id='server_expired'`); err != nil {
		t.Fatal(err)
	}

	registerTestServer(t, repo, principal, "server_revoked", 14)
	if err = repo.RevokeServer(ctx, principal, "server_revoked", "decommissioned for test"); err != nil {
		t.Fatal(err)
	}

	// A second owner's server must never leak into this owner's list.
	otherPrincipal := secondOwnerPrincipal(t, pool, authRepo, 15)
	registerTestServer(t, repo, otherPrincipal, "server_other_owner", 16)

	summaries, err := repo.ListServers(ctx, principal)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]ServerSummary, len(summaries))
	for _, s := range summaries {
		byID[s.ID] = s
	}
	if len(summaries) != 4 {
		t.Fatalf("expected 4 owner-scoped servers, got %d: %+v", len(summaries), summaries)
	}
	if _, ok := byID["server_other_owner"]; ok {
		t.Fatal("cross-owner server leaked into ListServers")
	}
	if s := byID["server_pending"]; s.Enrollment != EnrollmentPending || s.Status != server.StatusActive || s.ActiveCredentialVersion != 0 {
		t.Fatalf("server_pending=%+v", s)
	}
	if s := byID["server_consumed"]; s.Enrollment != EnrollmentConsumed || s.Status != server.StatusActive || s.ActiveCredentialVersion != 1 {
		t.Fatalf("server_consumed=%+v", s)
	}
	if s := byID["server_expired"]; s.Enrollment != EnrollmentExpired || s.ActiveCredentialVersion != 0 {
		t.Fatalf("server_expired=%+v", s)
	}
	if s := byID["server_revoked"]; s.Status != server.StatusRevoked || s.ActiveCredentialVersion != 0 {
		t.Fatalf("server_revoked=%+v", s)
	}
}

func TestServerListServersInvalidPrincipalDenied(t *testing.T) {
	_, repo, _, _ := freshServerRepository(t)
	if _, err := repo.ListServers(context.Background(), auth.Principal{}); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("invalid principal err=%v", err)
	}
}

func TestServerPgErrorClassification(t *testing.T) {
	fk := &pgconn.PgError{Code: "23503"}
	unique := &pgconn.PgError{Code: "23505"}
	other := &pgconn.PgError{Code: "23514"}
	if !isForeignKeyViolation(fk) || isForeignKeyViolation(unique) || isForeignKeyViolation(other) {
		t.Fatal("foreign key violation classification wrong")
	}
	if !isUniqueViolation(unique) || isUniqueViolation(fk) || isUniqueViolation(other) {
		t.Fatal("unique violation classification wrong")
	}
}
