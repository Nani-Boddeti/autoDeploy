package main

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"sync"
	"time"

	agenthttp "autodeploy/internal/agent/http"
	"autodeploy/internal/config"
	secretcrypto "autodeploy/internal/crypto"
	githubhttp "autodeploy/internal/github/http"
	"autodeploy/internal/githubapp"
	"autodeploy/internal/store/postgres"
	"autodeploy/internal/web"

	"github.com/jackc/pgx/v5/pgxpool"
)

// restartExitCode is the process exit status after a requested restart. Any
// non-zero status makes systemd's Restart=on-failure bring the unit back.
const restartExitCode = 75

// restartDelay lets the handler that requested the restart flush its response
// before the graceful shutdown begins.
const restartDelay = time.Second

// errRestartRequested is returned by run after a graceful shutdown that was
// triggered by restartHook (a GitHub App was just created).
var errRestartRequested = errors.New("restart requested")

var errGitHubSetupUnavailable = errors.New("GitHub setup unavailable")

// exitCodeFor maps run's result to the process exit status.
func exitCodeFor(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errRestartRequested):
		return restartExitCode
	default:
		return 1
	}
}

// restartHook turns web.Config.RequestRestart into a one-shot signal that run
// selects on, so shutdown uses the normal graceful path (never os.Exit from a
// handler).
type restartHook struct {
	once   sync.Once
	delay  time.Duration
	logger *slog.Logger
	signal chan struct{}
}

func newRestartHook(delay time.Duration, logger *slog.Logger) *restartHook {
	return &restartHook{delay: delay, logger: logger, signal: make(chan struct{})}
}

// Request schedules the restart once; later calls are no-ops.
func (h *restartHook) Request() {
	h.once.Do(func() {
		if h.logger != nil {
			h.logger.Info("github app created; restarting to load it")
		}
		time.AfterFunc(h.delay, func() { close(h.signal) })
	})
}

func (h *restartHook) Requested() <-chan struct{} { return h.signal }

const (
	gitHubSourceNone     = web.GitHubAppSourceNone
	gitHubSourceEnv      = web.GitHubAppSourceEnv
	gitHubSourceDatabase = web.GitHubAppSourceDatabase
)

// decideGitHubSource applies the precedence env > database > none. An env App
// wins and a database App is then only reported as ignored. A database App
// without a loadable KEK is a startup failure, never a silent "none".
func decideGitHubSource(envEnabled, dbAppExists, kekAvailable bool) (source string, dbAppIgnored bool, err error) {
	switch {
	case envEnabled:
		return gitHubSourceEnv, dbAppExists, nil
	case dbAppExists && !kekAvailable:
		return "", false, errGitHubSetupUnavailable
	case dbAppExists:
		return gitHubSourceDatabase, false, nil
	default:
		return gitHubSourceNone, false, nil
	}
}

// githubManifestStore is the slice of *postgres.GitHubManifestRepository that
// startup needs.
type githubManifestStore interface {
	ManifestAppExists(ctx context.Context) (bool, error)
	LoadRegistration(ctx context.Context, provider secretcrypto.KeyProvider) (postgres.ManifestRegistration, []byte, []byte, bool, error)
}

// loadKEKProvider loads the KEK ring from the host files. Callers keep the
// result only in a narrow scope: the control plane holds no KEK resident.
func loadKEKProvider() (secretcrypto.KeyProvider, error) {
	ring, _, err := config.KEKRingAndGenerationFromEnvironment()
	if err != nil {
		return nil, err
	}
	return ring, nil
}

// resolveGitHubSource determines the App source. The env App never depends on
// the manifest table: a failing existence check there only drops the "DB App
// ignored" note. For database/none the check is authoritative and its failure
// is fatal (a database App must not be silently skipped; creation needs the
// table). The KEK is probed only when a database App would be used, and the
// probe result is dropped immediately.
func resolveGitHubSource(ctx context.Context, store githubManifestStore, envEnabled bool, kek func() (secretcrypto.KeyProvider, error), logger *slog.Logger) (source string, dbAppIgnored bool, err error) {
	exists, existsErr := store.ManifestAppExists(ctx)
	if existsErr != nil {
		if envEnabled {
			if logger != nil {
				logger.Warn("github app database check failed; using environment app")
			}
			return gitHubSourceEnv, false, nil
		}
		return "", false, errGitHubSetupUnavailable
	}
	kekAvailable := false
	if !envEnabled && exists {
		_, kekErr := kek()
		kekAvailable = kekErr == nil
	}
	return decideGitHubSource(envEnabled, exists, kekAvailable)
}

// loadDatabaseGitHubApp decrypts the stored App and builds the wiring input.
// The KEK provider lives only in this frame, and the PEM and webhook secret
// plaintext are cleared right after they are turned into the signer and ring
// (success or failure). Every failure is errGitHubSetupUnavailable.
func loadDatabaseGitHubApp(ctx context.Context, store githubManifestStore, kek func() (secretcrypto.KeyProvider, error)) (githubAppWiring, error) {
	provider, err := kek()
	if err != nil {
		return githubAppWiring{}, errGitHubSetupUnavailable
	}
	registration, pem, webhookSecret, found, err := store.LoadRegistration(ctx, provider)
	if err != nil || !found {
		clear(pem)
		clear(webhookSecret)
		return githubAppWiring{}, errGitHubSetupUnavailable
	}
	signer, signerErr := config.GitHubAppSignerFromPEM(registration.ClientID, pem)
	clear(pem)
	ring, ringErr := config.GitHubWebhookSecretRingFromBytes(webhookSecret)
	clear(webhookSecret)
	if signerErr != nil || ringErr != nil {
		clearWebhookSecrets(&ring)
		return githubAppWiring{}, errGitHubSetupUnavailable
	}
	return githubAppWiring{
		identity:        githubapp.AppIdentity{AppID: registration.AppID, ClientID: registration.ClientID, Slug: registration.Slug},
		owner:           githubapp.Owner{AccountID: registration.GitHubOwnerID, Type: registration.GitHubOwnerType},
		installationURL: githubInstallationURL(registration.Slug),
		signer:          signer,
		webhookRing:     ring,
	}, nil
}

// githubAppWiring is everything the GitHub subsystem needs, independent of
// whether it came from the environment or the database.
type githubAppWiring struct {
	identity        githubapp.AppIdentity
	owner           githubapp.Owner
	installationURL string
	signer          *githubapp.Signer
	webhookRing     githubhttp.SecretRing
}

// githubAppServices are the handlers/stores wired for the App.
type githubAppServices struct {
	webhookHandler  http.Handler
	checkoutStore   agenthttp.CheckoutReservationStore
	checkoutService *githubapp.CheckoutService
}

// githubInstallationURL is the public install page for a stored App slug.
func githubInstallationURL(slug string) string {
	return "https://github.com/apps/" + url.PathEscape(slug) + "/installations/new"
}

// wireGitHubApp is the single wiring path for both App sources. Every failure
// reduces to the fixed "GitHub setup unavailable" error.
func wireGitHubApp(ctx context.Context, pool *pgxpool.Pool, trustedProxies []netip.Prefix, webOptions *web.Config, in githubAppWiring) (githubAppServices, error) {
	provider, err := githubapp.NewHTTPProvider(in.signer, "autodeploy-controlplane")
	if err != nil {
		return githubAppServices{}, errGitHubSetupUnavailable
	}
	githubRepository := postgres.NewGitHubRepository(pool)
	if err = githubRepository.EnsureAppRegistration(ctx, in.identity, in.owner); err != nil {
		return githubAppServices{}, errGitHubSetupUnavailable
	}
	setup, err := githubapp.NewSetupService(githubRepository, provider, in.identity, in.owner, rand.Reader)
	if err != nil {
		return githubAppServices{}, errGitHubSetupUnavailable
	}
	webOptions.GitHubSetup, webOptions.GitHubInstallationURL = setup, in.installationURL
	webOptions.RepositoryLister = githubRepository
	webhookRepository := postgres.NewWebhookJobRepository(pool, postgres.NewDeploymentRepository(pool))
	// Manual deploy (step 19d) needs the GitHub App provider to resolve and
	// verify commits, so it is only wired when the subsystem is enabled.
	webOptions.ManualDeploy, webOptions.CommitResolver = webhookRepository, provider
	services := githubAppServices{webhookHandler: githubhttp.Handler{Secrets: in.webhookRing, TrustedProxyCIDRs: trustedProxies, Processor: postgres.NewWebhookProcessor(webhookRepository)}}
	services.checkoutService, err = githubapp.NewCheckoutService(githubRepository, provider)
	if err != nil {
		return githubAppServices{}, errGitHubSetupUnavailable
	}
	services.checkoutStore = githubRepository
	return services, nil
}
