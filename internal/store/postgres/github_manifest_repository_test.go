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
	"autodeploy/internal/crypto"
	"autodeploy/internal/secret"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	testPEM           = "-----BEGIN PRIVATE KEY-----\nSUPERSECRETPEMBODY\n-----END PRIVATE KEY-----\n"
	testWebhookSecret = "whsec-SUPERSECRETWEBHOOK"
)

type manifestEnv struct {
	repo   *GitHubManifestRepository // runs as autodeploy_app
	admin  *pgxpool.Pool             // fixture / inspection (superuser)
	p1, p2 auth.Principal
	ring   crypto.KeyRing
}

func manifestRing(t *testing.T, key byte) crypto.KeyRing {
	t.Helper()
	ring, err := crypto.NewKeyRing("mkek-1", map[string][]byte{"mkek-1": bytes.Repeat([]byte{key}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	return ring
}

func freshManifestEnv(t *testing.T) manifestEnv {
	t.Helper()
	githubRepo, _, p1 := freshGitHubRepository(t)
	admin := githubRepo.pool
	ctx := context.Background()
	if _, err := admin.Exec(ctx, `TRUNCATE github_manifest_states, github_manifest_apps, secret_audit_events, secret_versions, secret_heads, secret_wrap_invocations RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	// The github fixture registers a 000005 env App; the manifest flow refuses while one exists.
	if _, err := admin.Exec(ctx, `DELETE FROM github_app_registrations`); err != nil {
		t.Fatal(err)
	}
	var generation string
	if err := admin.QueryRow(ctx, `SELECT id FROM secret_database_generation WHERE singleton=true`).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	if err := NewKeyRepository(admin).InitializeActiveLedger(ctx, "mkek-1", generation); err != nil {
		t.Fatal(err)
	}
	p2 := p1
	p2.SessionID = "manifest-session-2-" + t.Name()
	if _, err := admin.Exec(ctx, `INSERT INTO admin_sessions(id,token_digest,user_id,owner_id,auth_revision,created_at,last_seen_at,idle_expires_at,absolute_expires_at) VALUES($1,$2,$3,$4,$5,now(),now(),now()+interval '1 hour',now()+interval '2 hours')`,
		p2.SessionID, bytes.Repeat([]byte{9}, 32), p2.UserID, p2.OwnerID, int64(p2.AuthRevision)); err != nil {
		t.Fatal(err)
	}
	return manifestEnv{repo: NewGitHubManifestRepository(runtimeRolePool(t)), admin: admin, p1: p1, p2: p2, ring: manifestRing(t, 1)}
}

func (e manifestEnv) state(t *testing.T, p auth.Principal, b byte) [32]byte {
	t.Helper()
	d := digest(b)
	if err := e.repo.CreateManifestState(context.Background(), d, p, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	return d
}

func (e manifestEnv) input(p auth.Principal, d [32]byte, appID int64) ManifestRegistrationInput {
	return ManifestRegistrationInput{Principal: p, StateDigest: d, AppID: appID, GitHubOwnerID: 42, Slug: "autodeploy-test", ClientID: "Iv1.client", GitHubOwnerType: "Organization", GitHubOwnerLogin: "acme", HTMLURL: "https://github.com/apps/autodeploy-test",
		PrivateKeyPEM: []byte(testPEM), WebhookSecret: []byte(testWebhookSecret)}
}

func (e manifestEnv) create(t *testing.T, p auth.Principal, b byte, appID int64) ManifestRegistration {
	t.Helper()
	ctx := context.Background()
	d := e.state(t, p, b)
	if err := e.repo.ConsumeManifestStateForCreate(ctx, d, p); err != nil {
		t.Fatal(err)
	}
	reg, err := e.repo.InsertRegistration(ctx, e.input(p, d, appID), e.ring)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestManifestCreateConsumeInsertLoadRoundTrip(t *testing.T) {
	e := freshManifestEnv(t)
	ctx := context.Background()
	if _, _, _, found, err := e.repo.LoadRegistration(ctx, e.ring); err != nil || found {
		t.Fatalf("empty load: found=%v err=%v", found, err)
	}
	in := e.input(e.p1, e.state(t, e.p1, 1), 777)
	d := in.StateDigest
	if err := e.repo.ConsumeManifestStateForCreate(ctx, d, e.p1); err != nil {
		t.Fatal(err)
	}
	if _, err := e.repo.InsertRegistration(ctx, in, e.ring); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(in.PrivateKeyPEM, []byte("SUPERSECRET")) || bytes.Contains(in.WebhookSecret, []byte("SUPERSECRET")) {
		t.Fatal("input plaintext buffers were not cleared")
	}
	reg, pem, secret, found, err := e.repo.LoadRegistration(ctx, e.ring)
	if err != nil || !found {
		t.Fatalf("load: found=%v err=%v", found, err)
	}
	if string(pem) != testPEM || string(secret) != testWebhookSecret {
		t.Fatal("decrypted secrets differ from input")
	}
	if reg.AppID != 777 || reg.Slug != "autodeploy-test" || reg.GitHubOwnerType != "Organization" || reg.GitHubOwnerID != 42 || reg.OwnerID != e.p1.OwnerID {
		t.Fatalf("metadata: %+v", reg)
	}
	// Stored bytes and audit rows contain no secret material.
	var stored, meta string
	if err := e.admin.QueryRow(ctx, `SELECT encode(private_key_envelope||webhook_secret_envelope,'escape') FROM github_manifest_apps`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, "SUPERSECRET") {
		t.Fatal("plaintext found in stored envelopes")
	}
	if err := e.admin.QueryRow(ctx, `SELECT string_agg(metadata::text,'|') FROM github_audit_events WHERE action IN ('app_create_started','app_created')`).Scan(&meta); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(meta, "SUPERSECRET") || !strings.Contains(meta, `"app_id": 777`) {
		t.Fatalf("audit metadata: %s", meta)
	}
	if n := countOf(t, e.admin, `SELECT count(*) FROM github_audit_events WHERE action='app_create_started'`); n != 1 {
		t.Fatalf("app_create_started rows = %d", n)
	}
	if n := countOf(t, e.admin, `SELECT count(*) FROM github_audit_events WHERE action='app_created' AND actor_user_id=$1`, e.p1.UserID); n != 1 {
		t.Fatalf("app_created rows = %d", n)
	}
}

func TestManifestStateReplayAndAppExistsRefusals(t *testing.T) {
	e := freshManifestEnv(t)
	ctx := context.Background()
	d1 := e.state(t, e.p1, 1)
	d2 := e.state(t, e.p2, 2) // issued before any App exists
	if err := e.repo.ConsumeManifestStateForCreate(ctx, d1, e.p1); err != nil {
		t.Fatal(err)
	}
	if err := e.repo.ConsumeManifestStateForCreate(ctx, d1, e.p1); !errors.Is(err, ErrManifestStateConsumed) {
		t.Fatalf("replay: %v", err)
	}
	// Another session cannot start a second creation while the first is in flight.
	if err := e.repo.ConsumeManifestStateForCreate(ctx, d2, e.p2); !errors.Is(err, ErrManifestCreateInFlight) {
		t.Fatalf("in flight: %v", err)
	}
	if _, err := e.repo.InsertRegistration(ctx, e.input(e.p1, d1, 5), e.ring); err != nil {
		t.Fatal(err)
	}
	// With an App present every path is refused, including a state issued earlier.
	if err := e.repo.ConsumeManifestStateForCreate(ctx, d2, e.p2); !errors.Is(err, ErrManifestAppExists) {
		t.Fatalf("consume with App: %v", err)
	}
	if err := e.repo.CreateManifestState(ctx, digest(3), e.p1, time.Minute); !errors.Is(err, ErrManifestAppExists) {
		t.Fatalf("create state with App: %v", err)
	}
	before := reservedInvocations(t, e)
	if _, err := e.repo.InsertRegistration(ctx, e.input(e.p1, d1, 6), e.ring); !errors.Is(err, ErrManifestAppExists) {
		t.Fatalf("second insert: %v", err)
	}
	if reservedInvocations(t, e) != before {
		t.Fatal("App-exists refusal burned reservations")
	}
	if n := countOf(t, e.admin, `SELECT count(*) FROM github_manifest_apps`); n != 1 {
		t.Fatalf("apps = %d", n)
	}
}

// livePrincipal adds a fully valid, live owner/user/session so a refusal can
// only come from the state-binding comparison, not from session validation.
func (e manifestEnv) livePrincipal(t *testing.T, owner, user, session string, ownerID string) auth.Principal {
	t.Helper()
	ctx := context.Background()
	if ownerID == "" {
		ownerID = owner
		if _, err := e.admin.Exec(ctx, `INSERT INTO owners(id) VALUES($1)`, owner); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.admin.Exec(ctx, `INSERT INTO users(id,owner_id,username,role,password_phc,password_policy_revision) VALUES($1,$2,$3,'owner',$4,1)`, user, ownerID, "u-"+user, strings.Repeat("x", 68)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.admin.Exec(ctx, `INSERT INTO admin_sessions(id,token_digest,user_id,owner_id,auth_revision,created_at,last_seen_at,idle_expires_at,absolute_expires_at) VALUES($1,$2,$3,$4,1,now(),now(),now()+interval '1 hour',now()+interval '2 hours')`, session, bytes.Repeat([]byte{byte(len(session))}, 32), user, ownerID); err != nil {
		t.Fatal(err)
	}
	p := e.p1
	p.OwnerID, p.UserID, p.SessionID, p.AuthRevision = ownerID, user, session, 1
	return p
}

func TestManifestStateBindingAndExpiry(t *testing.T) {
	e := freshManifestEnv(t)
	ctx := context.Background()
	d := e.state(t, e.p1, 1)

	// Every principal below has a live session, so only the binding comparison under test can refuse it.
	otherUser := e.livePrincipal(t, "", "other-user", "other-user-session", e.p1.OwnerID)
	otherOwner := e.livePrincipal(t, "other-owner", "other-owner-user", "other-owner-session", "")
	// Same owner/user/session as its state, but the account moved to a newer auth revision.
	revUser := e.livePrincipal(t, "", "rev-user", "rev-user-session", e.p1.OwnerID)
	revState := e.state(t, revUser, 5)
	if _, err := e.admin.Exec(ctx, `UPDATE users SET auth_revision=2 WHERE id=$1`, revUser.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.admin.Exec(ctx, `UPDATE admin_sessions SET auth_revision=2 WHERE id=$1`, revUser.SessionID); err != nil {
		t.Fatal(err)
	}
	bumped := revUser
	bumped.AuthRevision = 2

	cases := []struct {
		name  string
		state [32]byte
		p     auth.Principal
	}{
		{"other session", d, e.p2},
		{"other user", d, otherUser},
		{"other owner", d, otherOwner},
		{"other revision", revState, bumped},
	}
	for _, c := range cases {
		if err := e.repo.ConsumeManifestStateForCreate(ctx, c.state, c.p); !errors.Is(err, ErrManifestStateInvalid) {
			t.Fatalf("%s: %v", c.name, err)
		}
	}
	if err := e.repo.ConsumeManifestStateForCreate(ctx, digest(99), e.p1); !errors.Is(err, ErrManifestStateInvalid) {
		t.Fatalf("unknown state: %v", err)
	}
	if n := countOf(t, e.admin, `SELECT count(*) FROM github_manifest_states WHERE consumed_at IS NOT NULL`); n != 0 {
		t.Fatal("a refused attempt consumed a state")
	}
	// Retire (shorten) the expiry to now: expired state is refused.
	if _, err := e.admin.Exec(ctx, `UPDATE github_manifest_states SET expires_at=clock_timestamp() WHERE digest=$1`, d[:]); err != nil {
		t.Fatal(err)
	}
	if err := e.repo.ConsumeManifestStateForCreate(ctx, d, e.p1); !errors.Is(err, ErrManifestStateExpired) {
		t.Fatalf("expired: %v", err)
	}
	// A revoked session cannot consume.
	d2 := e.state(t, otherUser, 2)
	if _, err := e.admin.Exec(ctx, `UPDATE admin_sessions SET revoked_at=clock_timestamp(), revoked_reason='logout' WHERE id=$1`, otherUser.SessionID); err != nil {
		t.Fatal(err)
	}
	if err := e.repo.ConsumeManifestStateForCreate(ctx, d2, otherUser); !errors.Is(err, ErrManifestStateInvalid) {
		t.Fatalf("revoked session: %v", err)
	}
}

func TestManifestNewStateRetiresPreviousOfSameSession(t *testing.T) {
	e := freshManifestEnv(t)
	ctx := context.Background()
	old := e.state(t, e.p1, 1)
	fresh := e.state(t, e.p1, 2)
	if err := e.repo.ConsumeManifestStateForCreate(ctx, old, e.p1); !errors.Is(err, ErrManifestStateExpired) {
		t.Fatalf("superseded state: %v", err)
	}
	if err := e.repo.ConsumeManifestStateForCreate(ctx, fresh, e.p1); err != nil {
		t.Fatal(err)
	}
}

func TestManifestInsertRequiresConsumedStateOfSamePrincipal(t *testing.T) {
	e := freshManifestEnv(t)
	ctx := context.Background()
	d := e.state(t, e.p1, 1)
	if _, err := e.repo.InsertRegistration(ctx, e.input(e.p1, d, 1), e.ring); !errors.Is(err, ErrManifestStateInvalid) {
		t.Fatalf("insert with unconsumed state: %v", err)
	}
	if err := e.repo.ConsumeManifestStateForCreate(ctx, d, e.p1); err != nil {
		t.Fatal(err)
	}
	if _, err := e.repo.InsertRegistration(ctx, e.input(e.p2, d, 1), e.ring); !errors.Is(err, ErrManifestStateInvalid) {
		t.Fatalf("insert by other session: %v", err)
	}
	bad := e.input(e.p1, d, 0)
	if _, err := e.repo.InsertRegistration(ctx, bad, e.ring); !errors.Is(err, ErrManifestInputInvalid) {
		t.Fatalf("invalid input: %v", err)
	}
	if n := countOf(t, e.admin, `SELECT count(*) FROM github_manifest_apps`); n != 0 {
		t.Fatal("a refused insert left a row")
	}
}

func TestManifestLoadFailsLoudlyOnWrongKeyOrSwappedEnvelope(t *testing.T) {
	e := freshManifestEnv(t)
	ctx := context.Background()
	e.create(t, e.p1, 1, 31)
	// Same KEK id, different key bytes.
	_, pem, secret, found, err := e.repo.LoadRegistration(ctx, manifestRing(t, 2))
	if err == nil || found || pem != nil || secret != nil || !errors.Is(err, crypto.ErrCryptographicFail) {
		t.Fatalf("wrong KEK: found=%v err=%v", found, err)
	}
	if strings.Contains(err.Error(), "SUPERSECRET") {
		t.Fatal("error leaks plaintext")
	}
	// KEK id unknown to the ring.
	other, kerr := crypto.NewKeyRing("other", map[string][]byte{"other": bytes.Repeat([]byte{3}, 32)})
	if kerr != nil {
		t.Fatal(kerr)
	}
	if _, _, _, found, err = e.repo.LoadRegistration(ctx, other); err == nil || found || !errors.Is(err, crypto.ErrUnknownKey) {
		t.Fatalf("unknown KEK: found=%v err=%v", found, err)
	}
	// Envelopes are bound to their variable: swapping the two columns must fail.
	replicaExec(t, e.admin, `UPDATE github_manifest_apps SET private_key_envelope=webhook_secret_envelope, webhook_secret_envelope=private_key_envelope`)
	if _, _, _, found, err = e.repo.LoadRegistration(ctx, e.ring); err == nil || found {
		t.Fatalf("swapped envelopes: found=%v err=%v", found, err)
	}
}

func TestManifestEnvelopeBoundToRegistrationID(t *testing.T) {
	e := freshManifestEnv(t)
	ctx := context.Background()
	e.create(t, e.p1, 1, 32)
	replicaExec(t, e.admin, `UPDATE github_manifest_apps SET id='forged-registration-id'`)
	if _, _, _, found, err := e.repo.LoadRegistration(ctx, e.ring); err == nil || found {
		t.Fatalf("re-keyed registration id: found=%v err=%v", found, err)
	}
}

func TestManifestRuntimeRoleCannotMutateRegistration(t *testing.T) {
	e := freshManifestEnv(t)
	ctx := context.Background()
	e.create(t, e.p1, 1, 33)
	rt := runtimeRolePool(t)
	for _, sql := range []string{
		`UPDATE github_manifest_apps SET slug='x'`,
		`DELETE FROM github_manifest_apps`,
		`UPDATE github_manifest_states SET digest=digest`,
		`DELETE FROM github_manifest_states`,
	} {
		_, err := rt.Exec(ctx, sql)
		if pgErrorCode(err) != "42501" {
			t.Fatalf("%s: code %q err=%v, want 42501", sql, pgErrorCode(err), err)
		}
	}
}

// TestManifestConcurrentCreatesYieldOneApp forces real lock contention: a
// holder transaction owns the advisory lock while two sessions each start a
// full consume-then-insert create. Both must be genuinely blocked before the
// holder lets go; afterwards at most one create may succeed end to end.
func TestManifestConcurrentCreatesYieldOneApp(t *testing.T) {
	e := freshManifestEnv(t)
	ctx := context.Background()
	d1, d2 := e.state(t, e.p1, 1), e.state(t, e.p2, 2)

	holder, err := e.admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx) //nolint:errcheck
	if _, err = holder.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(manifestLockKey)); err != nil {
		t.Fatal(err)
	}
	type result struct {
		consumed, inserted bool
		err                error
	}
	run := func(p auth.Principal, d [32]byte, appID int64, out *result, wg *sync.WaitGroup) {
		defer wg.Done()
		if err := e.repo.ConsumeManifestStateForCreate(ctx, d, p); err != nil {
			out.err = err
			return
		}
		out.consumed = true
		if _, err := e.repo.InsertRegistration(ctx, e.input(p, d, appID), e.ring); err != nil {
			out.err = err
			return
		}
		out.inserted = true
	}
	var wg sync.WaitGroup
	var r1, r2 result
	wg.Add(2)
	go run(e.p1, d1, 101, &r1, &wg)
	go run(e.p2, d2, 102, &r2, &wg)

	deadline := time.Now().Add(15 * time.Second)
	for {
		var waiting int
		if err := e.admin.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND wait_event='advisory'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d backends blocked on the manifest lock; the create path does not take it", waiting)
		}
		time.Sleep(5 * time.Millisecond) // poll interval only; progress is gated on pg_stat_activity
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()

	inserted, consumed := 0, 0
	for _, r := range []result{r1, r2} {
		if r.consumed {
			consumed++
		}
		if r.inserted {
			inserted++
		}
	}
	if inserted != 1 || consumed != 1 {
		t.Fatalf("consumed=%d inserted=%d (want exactly one end-to-end create): r1=%+v r2=%+v", consumed, inserted, r1, r2)
	}
	for _, r := range []result{r1, r2} {
		if !r.consumed && !errors.Is(r.err, ErrManifestCreateInFlight) && !errors.Is(r.err, ErrManifestAppExists) {
			t.Fatalf("loser got unexpected error: %v", r.err)
		}
	}
	if n := countOf(t, e.admin, `SELECT count(*) FROM github_manifest_apps`); n != 1 {
		t.Fatalf("apps = %d", n)
	}
}

func reservedInvocations(t *testing.T, e manifestEnv) int {
	t.Helper()
	return countOf(t, e.admin, `SELECT COALESCE(sum(reserved_invocations),0)::int FROM secret_wrap_invocations`)
}

func TestManifestInsertRefusedAfterInFlightWindow(t *testing.T) {
	e := freshManifestEnv(t)
	ctx := context.Background()
	d := e.state(t, e.p1, 1)
	if err := e.repo.ConsumeManifestStateForCreate(ctx, d, e.p1); err != nil {
		t.Fatal(err)
	}
	// Backdate beyond the window (fixture-only: the immutability trigger is bypassed).
	replicaExec(t, e.admin, `UPDATE github_manifest_states SET created_at=clock_timestamp()-interval '30 minutes', consumed_at=clock_timestamp()-interval '10 minutes' WHERE digest=$1`, d[:])
	before := reservedInvocations(t, e)
	if _, err := e.repo.InsertRegistration(ctx, e.input(e.p1, d, 8), e.ring); !errors.Is(err, ErrManifestStateInvalid) {
		t.Fatalf("stale consumed state: %v", err)
	}
	if reservedInvocations(t, e) != before {
		t.Fatal("refused insert burned wrap-ledger reservations")
	}
	if n := countOf(t, e.admin, `SELECT count(*) FROM github_manifest_apps`); n != 0 {
		t.Fatal("stale insert left a row")
	}
}

// blockedInsert consumes a state, parks InsertRegistration on the manifest
// advisory lock (it has already passed its unlocked pre-check by then), runs
// change while it is genuinely blocked, then lets it proceed.
func blockedInsert(t *testing.T, e manifestEnv, change func()) error {
	t.Helper()
	ctx := context.Background()
	d := e.state(t, e.p1, 1)
	if err := e.repo.ConsumeManifestStateForCreate(ctx, d, e.p1); err != nil {
		t.Fatal(err)
	}
	holder, err := e.admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx) //nolint:errcheck
	if _, err = holder.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(manifestLockKey)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := e.repo.InsertRegistration(ctx, e.input(e.p1, d, 9), e.ring)
		done <- err
	}()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var waiting int
		if err := e.admin.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND wait_event='advisory'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("InsertRegistration never blocked on the manifest lock")
		}
		time.Sleep(5 * time.Millisecond) // poll interval only
	}
	change()
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	return <-done
}

func TestManifestInsertInTxRechecksSessionAfterLockWait(t *testing.T) {
	e := freshManifestEnv(t)
	ctx := context.Background()
	err := blockedInsert(t, e, func() {
		if _, err := e.admin.Exec(ctx, `UPDATE admin_sessions SET revoked_at=clock_timestamp(), revoked_reason='logout' WHERE id=$1`, e.p1.SessionID); err != nil {
			t.Fatal(err)
		}
	})
	if !errors.Is(err, ErrManifestStateInvalid) {
		t.Fatalf("session revoked while waiting: %v", err)
	}
	if n := countOf(t, e.admin, `SELECT count(*) FROM github_manifest_apps`); n != 0 {
		t.Fatal("insert succeeded for a revoked session")
	}
}

// If the in-tx manifestInsertable call were removed, the singleton primary key
// (23505 -> ErrManifestAppExists) would still produce this same error, so this
// test pins the outcome, not which of the two guards produced it.
func TestManifestInsertInTxSeesAppCreatedAfterLockWait(t *testing.T) {
	e := freshManifestEnv(t)
	ctx := context.Background()
	err := blockedInsert(t, e, func() {
		env := bytes.Repeat([]byte{7}, 120)
		if _, err := e.admin.Exec(ctx, `INSERT INTO github_manifest_apps(id,app_id,slug,client_id,github_owner_id,github_owner_type,github_owner_login,html_url,private_key_envelope,webhook_secret_envelope,owner_id,created_by_user_id,private_key_kek_id,webhook_secret_kek_id) VALUES('winner',1,'s','c',1,'User','l','https://x',$1,$1,$2,$3,'mkek-1','mkek-1')`, env, e.p1.OwnerID, e.p1.UserID); err != nil {
			t.Fatal(err)
		}
	})
	if !errors.Is(err, ErrManifestAppExists) {
		t.Fatalf("App created while waiting: %v", err)
	}
	if n := countOf(t, e.admin, `SELECT count(*) FROM github_manifest_apps`); n != 1 {
		t.Fatalf("apps = %d", n)
	}
}

func TestManifestInsertRejectsOversizeFieldsBeforeEncrypting(t *testing.T) {
	e := freshManifestEnv(t)
	ctx := context.Background()
	d := e.state(t, e.p1, 1)
	if err := e.repo.ConsumeManifestStateForCreate(ctx, d, e.p1); err != nil {
		t.Fatal(err)
	}
	before := reservedInvocations(t, e)
	for name, mutate := range map[string]func(*ManifestRegistrationInput){
		"slug":      func(in *ManifestRegistrationInput) { in.Slug = strings.Repeat("a", 101) },
		"client id": func(in *ManifestRegistrationInput) { in.ClientID = strings.Repeat("a", 256) },
		"login":     func(in *ManifestRegistrationInput) { in.GitHubOwnerLogin = strings.Repeat("a", 101) },
		"html url":  func(in *ManifestRegistrationInput) { in.HTMLURL = strings.Repeat("a", 513) },
	} {
		in := e.input(e.p1, d, 10)
		mutate(&in)
		if _, err := e.repo.InsertRegistration(ctx, in, e.ring); !errors.Is(err, ErrManifestInputInvalid) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if reservedInvocations(t, e) != before {
		t.Fatal("oversize input burned reservations")
	}
	// Boundary values are accepted.
	in := e.input(e.p1, d, 11)
	in.Slug, in.ClientID, in.GitHubOwnerLogin, in.HTMLURL = strings.Repeat("a", 100), strings.Repeat("a", 255), strings.Repeat("a", 100), strings.Repeat("a", 512)
	if _, err := e.repo.InsertRegistration(ctx, in, e.ring); err != nil {
		t.Fatalf("max-size fields: %v", err)
	}
}

func TestManifestStateAtExactlyMaxTTL(t *testing.T) {
	e := freshManifestEnv(t)
	if err := e.repo.CreateManifestState(context.Background(), digest(1), e.p1, manifestMaxTTL); err != nil {
		t.Fatalf("ttl == max: %v", err)
	}
	if err := e.repo.CreateManifestState(context.Background(), digest(2), e.p1, manifestMaxTTL+time.Second); !errors.Is(err, ErrManifestInputInvalid) {
		t.Fatalf("ttl > max: %v", err)
	}
}

func TestManifestRefusedInsertsDoNotBurnReservations(t *testing.T) {
	e := freshManifestEnv(t)
	ctx := context.Background()
	d := e.state(t, e.p1, 1) // never consumed
	before := reservedInvocations(t, e)
	if _, err := e.repo.InsertRegistration(ctx, e.input(e.p1, d, 1), e.ring); !errors.Is(err, ErrManifestStateInvalid) {
		t.Fatal(err)
	}
	if reservedInvocations(t, e) != before {
		t.Fatal("unconsumed-state refusal burned reservations")
	}
}

func TestManifestInsertRefusedForBumpedAuthRevision(t *testing.T) {
	e := freshManifestEnv(t)
	ctx := context.Background()
	d := e.state(t, e.p1, 1)
	if err := e.repo.ConsumeManifestStateForCreate(ctx, d, e.p1); err != nil {
		t.Fatal(err)
	}
	// Session and user both move to revision 2, so the live-session check passes
	// for the bumped principal; only the state's recorded revision can refuse.
	if _, err := e.admin.Exec(ctx, `UPDATE users SET auth_revision=2 WHERE id=$1`, e.p1.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.admin.Exec(ctx, `UPDATE admin_sessions SET auth_revision=2 WHERE id=$1`, e.p1.SessionID); err != nil {
		t.Fatal(err)
	}
	bumped := e.p1
	bumped.AuthRevision = 2
	if _, err := e.repo.InsertRegistration(ctx, e.input(bumped, d, 12), e.ring); !errors.Is(err, ErrManifestStateInvalid) {
		t.Fatalf("bumped revision insert: %v", err)
	}
}

func TestManifestAppPinsItsKEK(t *testing.T) {
	e := freshManifestEnv(t)
	ctx := context.Background()
	e.create(t, e.p1, 1, 41)
	// The recorded KEK ids match the envelopes' own.
	var keyBlob, secretBlob []byte
	var keyKEK, secretKEK string
	if err := e.admin.QueryRow(ctx, `SELECT private_key_envelope,webhook_secret_envelope,private_key_kek_id,webhook_secret_kek_id FROM github_manifest_apps`).Scan(&keyBlob, &secretBlob, &keyKEK, &secretKEK); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		blob []byte
		kek  string
	}{{keyBlob, keyKEK}, {secretBlob, secretKEK}} {
		env, err := crypto.ParseEnvelope(c.blob)
		if err != nil || env.KEKID != c.kek || c.kek != "mkek-1" {
			t.Fatalf("recorded kek %q vs envelope %q (%v)", c.kek, env.KEKID, err)
		}
	}
	// can-retire refuses the App's KEK even though no secret_versions row uses it.
	var generation string
	if err := e.admin.QueryRow(ctx, `SELECT id FROM secret_database_generation WHERE singleton=true`).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	ring, err := crypto.NewKeyRing("mkek-2", map[string][]byte{"mkek-1": bytes.Repeat([]byte{1}, 32), "mkek-2": bytes.Repeat([]byte{4}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	rotator := secret.NewRotator(NewSecretRepository(e.admin), ring, generation)
	if err := rotator.CanRetire(ctx, "mkek-1"); !errors.Is(err, secret.ErrRetirement) {
		t.Fatalf("can-retire of the App's KEK: %v", err)
	}
	if err := rotator.CanRetire(ctx, "mkek-3"); err != nil {
		t.Fatalf("can-retire of an unreferenced KEK: %v", err)
	}
	// Ledger initialization refuses it for the reference alone (the ledger row is removed first,
	// so the ON CONFLICT path cannot be what refuses).
	if _, err := e.admin.Exec(ctx, `DELETE FROM secret_wrap_invocations WHERE kek_id='mkek-1'`); err != nil {
		t.Fatal(err)
	}
	if err := NewKeyRepository(e.admin).InitializeActiveLedger(ctx, "mkek-1", generation); !errors.Is(err, ErrKeyLedgerContinuity) {
		t.Fatalf("ledger init for referenced KEK: %v", err)
	}
	if err := NewKeyRepository(e.admin).InitializeActiveLedger(ctx, "mkek-9", generation); err != nil {
		t.Fatalf("ledger init for unreferenced KEK: %v", err)
	}
}

func TestManifestAppExistsWithoutDecrypting(t *testing.T) {
	e := freshManifestEnv(t)
	ctx := context.Background()
	if exists, err := e.repo.ManifestAppExists(ctx); err != nil || exists {
		t.Fatalf("empty: exists=%v err=%v", exists, err)
	}
	e.create(t, e.p1, 1, 801)
	if exists, err := e.repo.ManifestAppExists(ctx); err != nil || !exists {
		t.Fatalf("after create: exists=%v err=%v", exists, err)
	}
}

func TestManifestRefusedWhenPriorEnvAppRegistered(t *testing.T) {
	e := freshManifestEnv(t)
	ctx := context.Background()
	prior := func() {
		if _, err := e.admin.Exec(ctx, `INSERT INTO github_app_registrations(id,app_id,client_id,slug,owner_account_id,owner_type) VALUES(1,5,'c','s',1,'User')`); err != nil {
			t.Fatal(err)
		}
	}
	if has, err := e.repo.PriorAppRegistered(ctx); err != nil || has {
		t.Fatalf("PriorAppRegistered (none) = %v, %v", has, err)
	}
	prior()
	if has, err := e.repo.PriorAppRegistered(ctx); err != nil || !has {
		t.Fatalf("PriorAppRegistered = %v, %v", has, err)
	}
	if err := e.repo.CreateManifestState(ctx, digest(1), e.p1, time.Minute); !errors.Is(err, ErrManifestPriorAppRegistered) {
		t.Fatalf("create state: %v", err)
	}
	// Issue and consume states while no prior row exists, then let it appear.
	if _, err := e.admin.Exec(ctx, `DELETE FROM github_app_registrations`); err != nil {
		t.Fatal(err)
	}
	d := e.state(t, e.p1, 2)
	d2 := e.state(t, e.p2, 3)
	if err := e.repo.ConsumeManifestStateForCreate(ctx, d, e.p1); err != nil {
		t.Fatal(err)
	}
	prior()
	if err := e.repo.ConsumeManifestStateForCreate(ctx, d2, e.p2); !errors.Is(err, ErrManifestPriorAppRegistered) {
		t.Fatalf("consume: %v", err)
	}
	before := reservedInvocations(t, e)
	if _, err := e.repo.InsertRegistration(ctx, e.input(e.p1, d, 7), e.ring); !errors.Is(err, ErrManifestPriorAppRegistered) {
		t.Fatalf("insert: %v", err)
	}
	if reservedInvocations(t, e) != before {
		t.Fatal("prior-App refusal burned reservations")
	}
	if n := countOf(t, e.admin, `SELECT count(*) FROM github_manifest_apps`); n != 0 {
		t.Fatal("insert left a row")
	}
}

func TestManifestInsertInTxRechecksPriorEnvApp(t *testing.T) {
	e := freshManifestEnv(t)
	ctx := context.Background()
	if _, err := e.admin.Exec(ctx, `DELETE FROM github_app_registrations`); err != nil {
		t.Fatal(err)
	}
	err := blockedInsert(t, e, func() {
		if _, err := e.admin.Exec(ctx, `INSERT INTO github_app_registrations(id,app_id,client_id,slug,owner_account_id,owner_type) VALUES(1,5,'c','s',1,'User')`); err != nil {
			t.Fatal(err)
		}
	})
	if !errors.Is(err, ErrManifestPriorAppRegistered) {
		t.Fatalf("prior App appeared while waiting: %v", err)
	}
}
