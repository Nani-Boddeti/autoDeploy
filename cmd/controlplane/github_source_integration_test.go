//go:build integration

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"autodeploy/internal/auth"
	"autodeploy/internal/config"
	secretcrypto "autodeploy/internal/crypto"
	"autodeploy/internal/githubapp"
	"autodeploy/internal/store/postgres"
	"autodeploy/internal/web"
	"autodeploy/migrations"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The database-sourced App goes through the same wireGitHubApp as the env App:
// signer from PEM bytes, ring from bytes, 000005 registration ensured.
func TestWireGitHubAppFromDatabaseShapedInput(t *testing.T) {
	dsn := os.Getenv("AUTODEPLOY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AUTODEPLOY_TEST_DATABASE_URL is required for PostgreSQL integration tests")
	}
	ctx := context.Background()
	migrateConn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err = migrations.Apply(ctx, migrateConn); err != nil {
		t.Fatal(err)
	}
	migrateConn.Close(ctx)
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err = admin.Exec(ctx, `TRUNCATE github_audit_events, github_project_bindings, github_repositories, github_installations, github_pending_handoffs, github_setup_states, github_app_registrations RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, execErr := c.Exec(ctx, `SET ROLE autodeploy_app`)
		return execErr
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	signer, err := config.GitHubAppSignerFromPEM("Iv1.client", pemBytes)
	if err != nil {
		t.Fatal(err)
	}
	ring, err := config.GitHubWebhookSecretRingFromBytes(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	var options web.Config
	services, err := wireGitHubApp(ctx, pool, nil, &options, githubAppWiring{
		identity:        githubapp.AppIdentity{AppID: 4242, ClientID: "Iv1.client", Slug: "my-app"},
		owner:           githubapp.Owner{AccountID: 99, Type: "User"},
		installationURL: githubInstallationURL("my-app"),
		signer:          signer,
		webhookRing:     ring,
	})
	if err != nil {
		t.Fatal(err)
	}
	if services.webhookHandler == nil || services.checkoutStore == nil || services.checkoutService == nil || options.GitHubSetup == nil || options.RepositoryLister == nil || options.ManualDeploy == nil || options.CommitResolver == nil {
		t.Fatalf("incomplete wiring: %+v", services)
	}
	if options.GitHubInstallationURL != "https://github.com/apps/my-app/installations/new" {
		t.Fatal(options.GitHubInstallationURL)
	}
	var appID int64
	if err = admin.QueryRow(ctx, `SELECT app_id FROM github_app_registrations`).Scan(&appID); err != nil || appID != 4242 {
		t.Fatalf("registration app_id=%d err=%v", appID, err)
	}
}

func manifestTestRing(t *testing.T, key byte) secretcrypto.KeyRing {
	t.Helper()
	ring, err := secretcrypto.NewKeyRing("mkek-1", map[string][]byte{"mkek-1": bytes.Repeat([]byte{key}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	return ring
}

// A real registration is stored through the repository API, then loaded and
// wired exactly as startup does; a wrong KEK fails startup instead.
func TestDatabaseGitHubAppLoadsAndWiresEndToEnd(t *testing.T) {
	dsn := os.Getenv("AUTODEPLOY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AUTODEPLOY_TEST_DATABASE_URL is required for PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err = migrations.Apply(ctx, conn); err != nil {
		t.Fatal(err)
	}
	conn.Close(ctx)
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	for _, stmt := range []string{
		`TRUNCATE auth_audit_events, admin_sessions, auth_throttle_buckets, auth_username_throttles, auth_throttle_state, users, installation_auth, owners RESTART IDENTITY CASCADE`,
		`INSERT INTO installation_auth(id) VALUES(1)`,
		`INSERT INTO auth_throttle_state(id) VALUES(1)`,
		`TRUNCATE github_manifest_states, github_manifest_apps, github_audit_events, github_app_registrations, secret_audit_events, secret_versions, secret_heads, secret_wrap_invocations RESTART IDENTITY CASCADE`,
	} {
		if _, err = admin.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	policy := auth.Argon2Policy{Revision: 1, MemoryKiB: 19 * 1024, Iterations: 2, Lanes: 1}
	verifier, err := auth.HashPassword(strings.Repeat("p", 15), policy, bytes.NewReader(bytes.Repeat([]byte{17}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	authRepo := postgres.NewAuthRepository(admin)
	if err = authRepo.Bootstrap(ctx, postgres.BootstrapInput{OwnerID: "owner-e3", UserID: "user-e3", Username: "administrator", AuditID: "audit-e3", Verifier: verifier, Policy: policy}); err != nil {
		t.Fatal(err)
	}
	user, err := authRepo.FindLoginUserByCanonicalUsername(ctx, "administrator")
	if err != nil {
		t.Fatal(err)
	}
	token, err := auth.NewSessionToken(bytes.NewReader(bytes.Repeat([]byte{71}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = authRepo.CreateSession(ctx, "session-e3", token, user, nil, "login-e3", "limit-e3"); err != nil {
		t.Fatal(err)
	}
	principal, err := authRepo.ValidateSession(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	var generation string
	if err = admin.QueryRow(ctx, `SELECT id FROM secret_database_generation WHERE singleton=true`).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	if err = postgres.NewKeyRepository(admin).InitializeActiveLedger(ctx, "mkek-1", generation); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, execErr := c.Exec(ctx, `SET ROLE autodeploy_app`)
		return execErr
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	repo := postgres.NewGitHubManifestRepository(pool)

	kekOK := func() (secretcrypto.KeyProvider, error) { return manifestTestRing(t, 1), nil }
	if source, _, resolveErr := resolveGitHubSource(ctx, repo, false, kekOK, nil); resolveErr != nil || source != gitHubSourceNone {
		t.Fatalf("empty: source=%q err=%v", source, resolveErr)
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	digest := [32]byte{5}
	if err = repo.CreateManifestState(ctx, digest, principal, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err = repo.ConsumeManifestStateForCreate(ctx, digest, principal); err != nil {
		t.Fatal(err)
	}
	if _, err = repo.InsertRegistration(ctx, postgres.ManifestRegistrationInput{Principal: principal, StateDigest: digest, AppID: 5150, GitHubOwnerID: 42, Slug: "e3-app", ClientID: "Iv1.e3", GitHubOwnerType: "Organization", GitHubOwnerLogin: "acme", HTMLURL: "https://github.com/apps/e3-app",
		PrivateKeyPEM: pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), WebhookSecret: bytes.Repeat([]byte{9}, 32)}, manifestTestRing(t, 1)); err != nil {
		t.Fatal(err)
	}

	if source, ignored, resolveErr := resolveGitHubSource(ctx, repo, false, kekOK, nil); resolveErr != nil || source != gitHubSourceDatabase || ignored {
		t.Fatalf("db app: source=%q ignored=%v err=%v", source, ignored, resolveErr)
	}
	if source, ignored, resolveErr := resolveGitHubSource(ctx, repo, true, kekOK, nil); resolveErr != nil || source != gitHubSourceEnv || !ignored {
		t.Fatalf("env wins: source=%q ignored=%v err=%v", source, ignored, resolveErr)
	}
	wiring, err := loadDatabaseGitHubApp(ctx, repo, kekOK)
	if err != nil {
		t.Fatal(err)
	}
	var options web.Config
	services, err := wireGitHubApp(ctx, pool, nil, &options, wiring)
	if err != nil || services.webhookHandler == nil || options.GitHubSetup == nil || options.GitHubInstallationURL != "https://github.com/apps/e3-app/installations/new" {
		t.Fatalf("wiring err=%v options=%+v", err, options.GitHubInstallationURL)
	}
	var appID int64
	if err = admin.QueryRow(ctx, `SELECT app_id FROM github_app_registrations`).Scan(&appID); err != nil || appID != 5150 {
		t.Fatalf("000005 row app_id=%d err=%v", appID, err)
	}

	kekWrong := func() (secretcrypto.KeyProvider, error) { return manifestTestRing(t, 2), nil }
	if _, err = loadDatabaseGitHubApp(ctx, repo, kekWrong); !errors.Is(err, errGitHubSetupUnavailable) {
		t.Fatalf("wrong KEK: err=%v", err)
	}
	kekMissing := func() (secretcrypto.KeyProvider, error) { return nil, errors.New("no kek") }
	if _, _, err = resolveGitHubSource(ctx, repo, false, kekMissing, nil); !errors.Is(err, errGitHubSetupUnavailable) {
		t.Fatalf("missing KEK: err=%v", err)
	}
}
