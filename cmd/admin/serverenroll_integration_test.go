//go:build integration

package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"autodeploy/internal/admincli"
	"autodeploy/internal/auth"
	"autodeploy/internal/server"
	"autodeploy/internal/store/postgres"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type enrollFixture struct {
	pool  *pgxpool.Pool
	repo  *postgres.ServerRepository
	auth  *postgres.AuthRepository
	super *pgx.Conn // superuser connection to the same scratch database (fixture edits only)
}

func newEnrollFixture(t *testing.T) enrollFixture {
	t.Helper()
	dsn, super := scratchRuntimeDSN(t) // runtime-role login: the least-privilege shape the CLI runs as
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return enrollFixture{pool: pool, repo: postgres.NewServerRepository(pool), auth: postgres.NewAuthRepository(pool), super: super}
}

func (f enrollFixture) bootstrap(t *testing.T) {
	t.Helper()
	policy := auth.Argon2Policy{Revision: 1, MemoryKiB: 19 * 1024, Iterations: 2, Lanes: 1}
	verifier, err := auth.HashPassword(strings.Repeat("p", 15), policy, bytes.NewReader(bytes.Repeat([]byte{9}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	if err = f.auth.Bootstrap(context.Background(), postgres.BootstrapInput{OwnerID: "owner-enr", UserID: "user-enr", Username: "administrator", AuditID: "audit-enr", Verifier: verifier, Policy: policy}); err != nil {
		t.Fatal(err)
	}
}

func (f enrollFixture) count(t *testing.T, table string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM `+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func fastSleep(context.Context, time.Duration) error { time.Sleep(20 * time.Millisecond); return nil }

func (f enrollFixture) run(t *testing.T, opts admincli.LocalEnrollOptions) (string, error) {
	t.Helper()
	out, _, err := f.runEnrolled(t, opts)
	return out, err
}

func (f enrollFixture) runEnrolled(t *testing.T, opts admincli.LocalEnrollOptions) (string, bool, error) {
	t.Helper()
	var out bytes.Buffer
	opts.Sleep = fastSleep
	enrolled, err := admincli.EnrollLocal(context.Background(), f.repo, opts, &out)
	return out.String(), enrolled, err
}

func readToken(t *testing.T, path string) server.EnrollmentToken {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	token, err := server.ParseEnrollmentToken(strings.TrimSuffix(string(raw), "\n"))
	if err != nil {
		t.Fatalf("token file content is not an enrollment token: %v", err)
	}
	return token
}

func TestServerEnrollLocalRefusesBeforeAdministratorAndTouchesNothing(t *testing.T) {
	f := newEnrollFixture(t)
	path := filepath.Join(t.TempDir(), "token")
	out, err := f.run(t, admincli.LocalEnrollOptions{Name: "box", TokenFile: path})
	if !errors.Is(err, admincli.ErrAdministratorMissing) || out != "" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
		t.Fatalf("token file created before an administrator exists: %v", statErr)
	}
	if f.count(t, "servers") != 0 {
		t.Fatal("server registered before an administrator exists")
	}
}

func TestServerEnrollLocalWaitSucceedsOnceAdministratorAppears(t *testing.T) {
	f := newEnrollFixture(t)
	path := filepath.Join(t.TempDir(), "token")
	go func() {
		time.Sleep(150 * time.Millisecond)
		f.bootstrap(t)
	}()
	out, err := f.run(t, admincli.LocalEnrollOptions{Name: "box", TokenFile: path, Wait: 10 * time.Second})
	if err != nil || !strings.HasPrefix(out, "server_") {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if f.count(t, "servers") != 1 {
		t.Fatal("want exactly one server")
	}
}

func TestServerEnrollLocalWaitTimesOutClearly(t *testing.T) {
	f := newEnrollFixture(t)
	_, err := f.run(t, admincli.LocalEnrollOptions{Name: "box", TokenFile: filepath.Join(t.TempDir(), "token"), Wait: 100 * time.Millisecond})
	if !errors.Is(err, admincli.ErrAdministratorMissing) {
		t.Fatalf("err=%v", err)
	}
}

func TestServerEnrollLocalFirstRunRegistersOneServerAndWritesToken0600(t *testing.T) {
	f := newEnrollFixture(t)
	f.bootstrap(t)
	var logs bytes.Buffer
	prevSlog, prevLogOut := slog.Default(), log.Writer()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	log.SetOutput(&logs)
	t.Cleanup(func() { slog.SetDefault(prevSlog); log.SetOutput(prevLogOut) })

	path := filepath.Join(t.TempDir(), "token")
	out, err := f.run(t, admincli.LocalEnrollOptions{Name: "box", TokenFile: path})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode().Perm() != 0o600 || !info.Mode().IsRegular() {
		t.Fatalf("token file mode %v err %v", info, err)
	}
	token := readToken(t, path)
	id := strings.TrimSuffix(out, "\n")
	if out != id+"\n" || !strings.HasPrefix(id, "server_") {
		t.Fatalf("stdout must be exactly the server id, got %q", out)
	}
	if strings.Contains(out, token.String()) || strings.Contains(logs.String(), token.String()) || strings.Contains(logs.String(), strings.TrimPrefix(token.String(), "enr.v1.")) {
		t.Fatal("token leaked to stdout or logs")
	}
	if f.count(t, "servers") != 1 || f.count(t, "server_enrollment_tokens") != 1 {
		t.Fatal("want exactly one server and one token row")
	}
	// The stored digest is the digest of the token in the file, and it enrolls.
	digest, _ := server.EnrollmentTokenDigest(token)
	cred, _ := server.NewCredentialToken(nil)
	credDigest, _ := server.CredentialTokenDigest(cred)
	if _, err = f.repo.Enroll(context.Background(), id, digest, credDigest); err != nil {
		t.Fatalf("token from file did not enroll: %v", err)
	}
	var name, owner string
	if err = f.pool.QueryRow(context.Background(), `SELECT name,owner_id FROM servers WHERE id=$1`, id).Scan(&name, &owner); err != nil || name != "box" || owner != "owner-enr" {
		t.Fatalf("name=%q owner=%q err=%v", name, owner, err)
	}
}

// The audit row must have exactly the shape of the UI's registration: same
// actor type, actor id = the owner's user id, same action/result/reason.
func TestServerEnrollLocalAuditMatchesUIRegistrationShape(t *testing.T) {
	f := newEnrollFixture(t)
	f.bootstrap(t)
	ctx := context.Background()
	user, err := f.auth.FindLoginUserByCanonicalUsername(ctx, "administrator")
	if err != nil {
		t.Fatal(err)
	}
	sessionToken, err := auth.NewSessionToken(bytes.NewReader(bytes.Repeat([]byte{5}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.auth.CreateSession(ctx, "enr-session", sessionToken, user, nil, "enr-login", "enr-limit"); err != nil {
		t.Fatal(err)
	}
	principal, err := f.auth.ValidateSession(ctx, sessionToken)
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := server.EnrollmentTokenDigest(server.EnrollmentToken("enr.v1." + strings.Repeat("A", 43)))
	if _, _, err = f.repo.RegisterServer(ctx, principal, postgres.RegisterServerInput{ServerID: "server_ui", Name: "ui", ProtocolVersion: server.ProtocolVersion, EnrollmentDigest: digest, EnrollmentTTLSeconds: server.EnrollmentTTLSeconds}); err != nil {
		t.Fatal(err)
	}
	out, err := f.run(t, admincli.LocalEnrollOptions{Name: "box", TokenFile: filepath.Join(t.TempDir(), "token")})
	if err != nil {
		t.Fatal(err)
	}
	local := strings.TrimSpace(out)
	shape := func(serverID string) string {
		var s string
		if err := f.pool.QueryRow(ctx, `SELECT actor_type||'|'||actor_id||'|'||action||'|'||result||'|'||reason||'|'||coalesce(credential_version::text,'-') FROM server_audit_events WHERE server_id=$1`, serverID).Scan(&s); err != nil {
			t.Fatalf("audit row for %s: %v", serverID, err)
		}
		return s
	}
	// Only the reason marker differs from the UI row (M4).
	if shape("server_ui") != "administrator|"+user.ID+"|register|success||-" || shape(local) != "administrator|"+user.ID+"|register|success|local-enroll|-" {
		t.Fatalf("ui=%q local=%q", shape("server_ui"), shape(local))
	}
	var ttlUI, ttlLocal float64
	row := `SELECT extract(epoch FROM expires_at-issued_at) FROM server_enrollment_tokens WHERE server_id=$1`
	if err = f.pool.QueryRow(ctx, row, "server_ui").Scan(&ttlUI); err != nil {
		t.Fatal(err)
	}
	if err = f.pool.QueryRow(ctx, row, local).Scan(&ttlLocal); err != nil || int(ttlUI) != int(ttlLocal) || int(ttlLocal) != 900 {
		t.Fatalf("ttl ui=%v local=%v err=%v", ttlUI, ttlLocal, err)
	}
}

func TestServerEnrollLocalRerunAfterEnrollmentMintsNothing(t *testing.T) {
	f := newEnrollFixture(t)
	f.bootstrap(t)
	path := filepath.Join(t.TempDir(), "token")
	out, err := f.run(t, admincli.LocalEnrollOptions{Name: "box", TokenFile: path})
	if err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(out)
	token := readToken(t, path)
	digest, _ := server.EnrollmentTokenDigest(token)
	cred, _ := server.NewCredentialToken(nil)
	credDigest, _ := server.CredentialTokenDigest(cred)
	if _, err = f.repo.Enroll(context.Background(), id, digest, credDigest); err != nil {
		t.Fatal(err)
	}
	auditBefore := f.count(t, "server_audit_events")
	path2 := filepath.Join(t.TempDir(), "token2")
	out2, enrolledFlag, err := f.runEnrolled(t, admincli.LocalEnrollOptions{Name: "box", ServerID: id, TokenFile: path2})
	if err != nil || !enrolledFlag || strings.TrimSpace(out2) != id {
		t.Fatalf("out=%q err=%v", out2, err)
	}
	if _, statErr := os.Lstat(path2); !os.IsNotExist(statErr) {
		t.Fatal("a token file was written for an already enrolled server")
	}
	if f.count(t, "servers") != 1 || f.count(t, "server_audit_events") != auditBefore {
		t.Fatal("rerun after enrollment changed state")
	}
}

func TestServerEnrollLocalRerunAfterCrashReusesServerWithFreshToken(t *testing.T) {
	f := newEnrollFixture(t)
	f.bootstrap(t)
	dir := t.TempDir()
	first := filepath.Join(dir, "token1")
	out, err := f.run(t, admincli.LocalEnrollOptions{Name: "box", TokenFile: first})
	if err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(out)
	old := readToken(t, first)
	// Simulated crash: the host lost the token file and never recorded the id,
	// so the rerun has only the name. It must reuse, not duplicate.
	second := filepath.Join(dir, "token2")
	out2, err := f.run(t, admincli.LocalEnrollOptions{Name: "box", TokenFile: second})
	if err != nil || strings.TrimSpace(out2) != id {
		t.Fatalf("out=%q err=%v want %s", out2, err, id)
	}
	if f.count(t, "servers") != 1 {
		t.Fatal("rerun duplicated the server")
	}
	fresh := readToken(t, second)
	if fresh == old {
		t.Fatal("rerun reused the old token")
	}
	oldDigest, _ := server.EnrollmentTokenDigest(old)
	cred, _ := server.NewCredentialToken(nil)
	credDigest, _ := server.CredentialTokenDigest(cred)
	if _, err = f.repo.Enroll(context.Background(), id, oldDigest, credDigest); err == nil {
		t.Fatal("the superseded token still enrolls")
	}
	freshDigest, _ := server.EnrollmentTokenDigest(fresh)
	if _, err = f.repo.Enroll(context.Background(), id, freshDigest, credDigest); err != nil {
		t.Fatalf("fresh token does not enroll: %v", err)
	}
	var issued int
	if err = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM server_audit_events WHERE server_id=$1 AND action='enrollment_issued' AND actor_type='administrator'`, id).Scan(&issued); err != nil || issued != 1 {
		t.Fatalf("enrollment_issued audit rows=%d err=%v", issued, err)
	}
}

func TestServerEnrollLocalRecordedUnknownServerIDRefused(t *testing.T) {
	f := newEnrollFixture(t)
	f.bootstrap(t)
	path := filepath.Join(t.TempDir(), "token")
	_, err := f.run(t, admincli.LocalEnrollOptions{Name: "box", ServerID: "server_missing", TokenFile: path})
	if !errors.Is(err, admincli.ErrLocalServerUnavailable) || f.count(t, "servers") != 0 {
		t.Fatalf("err=%v", err)
	}
	if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
		t.Fatal("token file created")
	}
}

func TestServerEnrollLocalTokenFileRefusals(t *testing.T) {
	f := newEnrollFixture(t)
	f.bootstrap(t)
	dir := t.TempDir()
	existing := filepath.Join(dir, "exists")
	if err := os.WriteFile(existing, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	link, dangling := filepath.Join(dir, "link"), filepath.Join(dir, "dangling")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "nowhere"), dangling); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"exists": existing, "symlink": link, "dangling symlink": dangling, "relative": "token", "missing dir": filepath.Join(dir, "no", "token"), "unclean": dir + "/x/../y"} {
		_, err := f.run(t, admincli.LocalEnrollOptions{Name: "box", TokenFile: path})
		if !errors.Is(err, admincli.ErrTokenFileUnusable) {
			t.Fatalf("%s: err=%v", name, err)
		}
	}
	for _, p := range []string{existing, target} {
		if b, _ := os.ReadFile(p); string(b) != "keep" {
			t.Fatalf("%s was modified", p)
		}
	}
	if _, err := os.Lstat(filepath.Join(dir, "nowhere")); !os.IsNotExist(err) {
		t.Fatal("dangling symlink target was created")
	}
	if f.count(t, "servers") != 0 || f.count(t, "server_enrollment_tokens") != 0 {
		t.Fatal("a refused token file still minted a server or token")
	}
}

func TestServerEnrollLocalDBFailureLeavesNoTokenFile(t *testing.T) {
	f := newEnrollFixture(t)
	f.bootstrap(t)
	path := filepath.Join(t.TempDir(), "token")
	// Repository failure after the token file exists: no file may survive.
	failing := failingRepo{f.repo}
	var out bytes.Buffer
	_, err := admincli.EnrollLocal(context.Background(), failing, admincli.LocalEnrollOptions{Name: "box", TokenFile: path}, &out)
	if !errors.Is(err, admincli.ErrOperationFailed) || out.Len() != 0 {
		t.Fatalf("out=%q err=%v", out.String(), err)
	}
	if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
		t.Fatal("failed run left a token file")
	}
}

type failingRepo struct{ *postgres.ServerRepository }

func (failingRepo) EnrollLocalServer(context.Context, string, string, string, [32]byte, int) (postgres.LocalEnrollment, error) {
	return postgres.LocalEnrollment{}, errors.New("boom")
}

func (f enrollFixture) uiRegister(t *testing.T, id, name string) {
	t.Helper()
	ctx := context.Background()
	user, err := f.auth.FindLoginUserByCanonicalUsername(ctx, "administrator")
	if err != nil {
		t.Fatal(err)
	}
	sessionToken, err := auth.NewSessionToken(bytes.NewReader(bytes.Repeat([]byte{6}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.auth.CreateSession(ctx, "ui-session-"+id, sessionToken, user, nil, "ui-login-"+id, "ui-limit-"+id); err != nil {
		t.Fatal(err)
	}
	principal, err := f.auth.ValidateSession(ctx, sessionToken)
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := server.EnrollmentTokenDigest(server.EnrollmentToken("enr.v1." + strings.Repeat("B", 43)))
	if _, _, err = f.repo.RegisterServer(ctx, principal, postgres.RegisterServerInput{ServerID: id, Name: name, ProtocolVersion: server.ProtocolVersion, EnrollmentDigest: digest, EnrollmentTTLSeconds: server.EnrollmentTTLSeconds}); err != nil {
		t.Fatal(err)
	}
}

// M4: a server the owner registered in the UI for another machine, same name,
// never enrolled, must NOT be adopted by the local path.
func TestServerEnrollLocalDoesNotAdoptUIRegisteredSameNameServer(t *testing.T) {
	f := newEnrollFixture(t)
	f.bootstrap(t)
	f.uiRegister(t, "server_ui_other", "box")
	out, err := f.run(t, admincli.LocalEnrollOptions{Name: "box", TokenFile: filepath.Join(t.TempDir(), "token")})
	if err != nil || strings.TrimSpace(out) == "server_ui_other" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if f.count(t, "servers") != 2 {
		t.Fatal("want the UI server untouched and one new local server")
	}
	var consumedOrChanged int
	if err = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM server_audit_events WHERE server_id='server_ui_other' AND action<>'register'`).Scan(&consumedOrChanged); err != nil || consumedOrChanged != 0 {
		t.Fatalf("UI server was touched: %d err=%v", consumedOrChanged, err)
	}
}

// M2: token consumed but no active credential, and a revoked recorded id, get
// distinct errors (not the generic failure).
func TestServerEnrollLocalReenrollAndUnavailableAreDistinct(t *testing.T) {
	f := newEnrollFixture(t)
	f.bootstrap(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "token")
	out, err := f.run(t, admincli.LocalEnrollOptions{Name: "box", TokenFile: path})
	if err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(out)
	digest, _ := server.EnrollmentTokenDigest(readToken(t, path))
	cred, _ := server.NewCredentialToken(nil)
	credDigest, _ := server.CredentialTokenDigest(cred)
	if _, err = f.repo.Enroll(ctx, id, digest, credDigest); err != nil {
		t.Fatal(err)
	}
	// Superuser-only fixture: revoke the credential but keep the server active.
	super := f.super
	if _, err = super.Exec(ctx, `UPDATE server_credentials SET status='revoked',revoked_at=clock_timestamp() WHERE server_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	p2 := filepath.Join(t.TempDir(), "token2")
	if _, err = f.run(t, admincli.LocalEnrollOptions{Name: "box", ServerID: id, TokenFile: p2}); !errors.Is(err, admincli.ErrServerNeedsReenroll) {
		t.Fatalf("want ErrServerNeedsReenroll, got %v", err)
	}
	if _, statErr := os.Lstat(p2); !os.IsNotExist(statErr) {
		t.Fatal("token file left behind")
	}
	if _, err = super.Exec(ctx, `UPDATE servers SET status='revoked',revoked_at=clock_timestamp() WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err = f.run(t, admincli.LocalEnrollOptions{Name: "box", ServerID: id, TokenFile: filepath.Join(t.TempDir(), "t3")}); !errors.Is(err, admincli.ErrLocalServerUnavailable) {
		t.Fatalf("want ErrLocalServerUnavailable, got %v", err)
	}
}

// L5: a disabled administrator is reported at once, never polled by --wait.
func TestServerEnrollLocalDisabledAdministratorFailsFast(t *testing.T) {
	f := newEnrollFixture(t)
	f.bootstrap(t)
	super := f.super
	if _, err := super.Exec(context.Background(), `UPDATE users SET status='disabled'`); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := f.run(t, admincli.LocalEnrollOptions{Name: "box", TokenFile: filepath.Join(t.TempDir(), "token"), Wait: time.Minute})
	if !errors.Is(err, admincli.ErrAdministratorDisabled) || time.Since(start) > 5*time.Second {
		t.Fatalf("err=%v after %v", err, time.Since(start))
	}
}

// M3: real lock contention. Concurrent local enrollments serialize in the
// database: exactly one server and one register audit row; the stored token
// digest is the one in the file of the call that committed last, so no file
// holds a token that silently disagrees with the database unless it was
// superseded by a later successful call.
func TestServerEnrollLocalConcurrentCallsYieldOneServer(t *testing.T) {
	f := newEnrollFixture(t)
	f.bootstrap(t)
	const n = 6
	dir := t.TempDir()
	type result struct {
		out, path string
		err       error
	}
	results := make(chan result, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func(i int) {
			<-start
			path := filepath.Join(dir, "token"+string(rune('a'+i)))
			out, err := f.run(t, admincli.LocalEnrollOptions{Name: "box", TokenFile: path})
			results <- result{out, path, err}
		}(i)
	}
	close(start)
	var ids = map[string]bool{}
	var matching int
	var storedDigest []byte
	if err := f.pool.QueryRow(context.Background(), `SELECT 1`).Scan(new(int)); err != nil {
		t.Fatal(err)
	}
	var paths []string
	for i := 0; i < n; i++ {
		r := <-results
		if r.err != nil {
			t.Fatalf("call failed: %v", r.err)
		}
		ids[strings.TrimSpace(r.out)] = true
		paths = append(paths, r.path)
	}
	if len(ids) != 1 || f.count(t, "servers") != 1 {
		t.Fatalf("ids=%v servers=%d", ids, f.count(t, "servers"))
	}
	var registers int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM server_audit_events WHERE action='register'`).Scan(&registers); err != nil || registers != 1 {
		t.Fatalf("register rows=%d err=%v", registers, err)
	}
	if err := f.pool.QueryRow(context.Background(), `SELECT digest FROM server_enrollment_tokens`).Scan(&storedDigest); err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		d, _ := server.EnrollmentTokenDigest(readToken(t, p))
		if bytes.Equal(d[:], storedDigest) {
			matching++
		}
	}
	if matching != 1 {
		t.Fatalf("exactly one token file must match the stored digest, got %d", matching)
	}
}
