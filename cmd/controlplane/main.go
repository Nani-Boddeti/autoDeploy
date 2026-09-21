// Command controlplane serves the AutoDeploy browser control plane.
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	agenthttp "autodeploy/internal/agent/http"
	"autodeploy/internal/auth"
	"autodeploy/internal/config"
	secretcrypto "autodeploy/internal/crypto"
	githubhttp "autodeploy/internal/github/http"
	"autodeploy/internal/githubapp"
	"autodeploy/internal/secret"
	"autodeploy/internal/server"
	"autodeploy/internal/store/postgres"
	"autodeploy/internal/web"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 15 * time.Second
	writeTimeout      = 20 * time.Second
	idleTimeout       = 60 * time.Second
	shutdownTimeout   = 20 * time.Second
	maxHeaderBytes    = 16 << 10
)

func main() {
	if err := run(context.Background()); err != nil {
		logger, closeLogger := controlplaneLogger(os.Stderr, nil)
		logger.Print("control plane unavailable")
		_ = closeLogger.Close()
		os.Exit(1)
	}
}

// controlplaneLogger is the sole process-log construction boundary. Callers
// pass any transient secret values when a future integration can emit related
// output; failures are deliberately reduced to fixed public messages.
func controlplaneLogger(dst io.Writer, values [][]byte) (*log.Logger, io.Closer) {
	redactor, err := secretcrypto.NewRedactingWriter(dst, values)
	if err != nil {
		return log.New(io.Discard, "", 0), nopCloser{}
	}
	return log.New(redactor, "", 0), redactor
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

func run(ctx context.Context) error {
	dsn, err := config.DatabaseURLFromEnvironment()
	if err != nil {
		return err
	}
	webConfig, err := config.WebConfigFromEnvironment()
	if err != nil {
		return err
	}
	throttleKeys, err := config.UsernameThrottleKeyRingFromEnvironment()
	if err != nil {
		return err
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	repository := postgres.NewAuthRepository(pool)
	if err = repository.Ping(ctx); err != nil {
		return err
	}
	enabled, err := config.SecretSubsystemEnabled()
	if err != nil {
		return err
	}
	if enabled {
		ring, generation, ringErr := config.KEKRingAndGenerationFromEnvironment()
		if ringErr != nil {
			return errors.New("secret subsystem unavailable")
		}
		if readyErr := secretLedgerReady(ctx, postgres.NewKeyRepository(pool), ring, generation); readyErr != nil {
			return errors.New("secret subsystem unavailable")
		}
	}
	policy, err := repository.LoadPasswordPolicy(ctx)
	if err != nil {
		return err
	}
	dummy, err := auth.BuildDummyPasswordVerifier(policy)
	if err != nil {
		return err
	}
	webOptions := web.Config{PublicOrigin: webConfig.PublicOrigin, CSRFKeys: webConfig.CSRFKeys, UsernameThrottleKeys: throttleKeys, TrustedProxyCIDRs: webConfig.TrustedProxyCIDRs, AuthThrottleMaxRows: webConfig.AuthThrottleMaxRows, PasswordPolicy: policy, DummyPassword: dummy}
	githubConfig, err := config.GitHubAppConfigFromEnvironment()
	if err != nil {
		return err
	}
	var webhookHandler http.Handler
	// checkoutStore and checkoutService back the agent-facing checkout routes
	// (Step 3c part 2). Both are only ever non-nil when the GitHub App
	// subsystem itself is enabled: those routes need it unconditionally, and
	// must answer 503 rather than a partial success when it is not
	// configured. Neither leaks the App signer or its private key past this
	// function -- only the provider's already-derived, short-lived
	// installation-token minting is reachable through checkoutService.
	var checkoutStore agenthttp.CheckoutReservationStore
	var checkoutService *githubapp.CheckoutService
	if githubConfig.Enabled {
		webhookSecrets, webhookErr := config.GitHubWebhookSecretRingFromEnvironment()
		if webhookErr != nil {
			return errors.New("GitHub setup unavailable")
		}
		defer clearWebhookSecrets(&webhookSecrets)
		signer, signerErr := config.GitHubAppSignerFromMountedKey(githubConfig)
		if signerErr != nil {
			return errors.New("GitHub setup unavailable")
		}
		provider, providerErr := githubapp.NewHTTPProvider(signer, "autodeploy-controlplane")
		if providerErr != nil {
			return errors.New("GitHub setup unavailable")
		}
		identity := githubapp.AppIdentity{AppID: githubConfig.AppID, ClientID: githubConfig.ClientID, Slug: githubConfig.Slug}
		owner := githubapp.Owner{AccountID: githubConfig.OwnerAccountID, Type: githubConfig.OwnerType}
		githubRepository := postgres.NewGitHubRepository(pool)
		if registerErr := githubRepository.EnsureAppRegistration(ctx, identity, owner); registerErr != nil {
			return errors.New("GitHub setup unavailable")
		}
		setup, setupErr := githubapp.NewSetupService(githubRepository, provider, identity, owner, rand.Reader)
		if setupErr != nil {
			return errors.New("GitHub setup unavailable")
		}
		webOptions.GitHubSetup, webOptions.GitHubInstallationURL = setup, githubConfig.InstallationURL
		webhookRepository := postgres.NewWebhookJobRepository(pool, postgres.NewDeploymentRepository(pool))
		webhookHandler = githubhttp.Handler{Secrets: webhookSecrets, TrustedProxyCIDRs: webConfig.TrustedProxyCIDRs, Processor: postgres.NewWebhookProcessor(webhookRepository)}
		checkoutService, err = githubapp.NewCheckoutService(githubRepository, provider)
		if err != nil {
			return errors.New("GitHub setup unavailable")
		}
		checkoutStore = githubRepository
	}
	// serverRepository and leaseRepository back the session/CSRF-protected
	// web /servers and /environments/mark-reconciled admin routes (Step 6b);
	// an administrator can register and manage servers whether or not the
	// unauthenticated /agent/v1 protocol below is itself enabled, so these
	// are constructed unconditionally, not gated by agentConfig.Enabled.
	serverRepository := postgres.NewServerRepository(pool)
	leaseRepository := postgres.NewAgentLeaseRepository(pool, postgres.NewDeploymentRepository(pool))
	webOptions.ServerAdmin = serverAdmin{serverRepository, leaseRepository}
	// EnvironmentRuntimeConfigRepository already satisfies web.RuntimeConfigAdmin's
	// method set directly (Slice 9a Step 3d), so no adapter type is needed
	// here the way serverAdmin composes two repositories above.
	webOptions.RuntimeConfigAdmin = postgres.NewEnvironmentRuntimeConfigRepository(pool)
	agentConfig, err := config.AgentConfigFromEnvironment()
	if err != nil {
		return err
	}
	var agentHandler http.Handler
	var sweeper *postgres.AgentLeaseSweeper
	if agentConfig.Enabled {
		agentHandler = agenthttp.Handler{
			Enroller:          serverRepository,
			Authenticator:     serverAuthenticator{serverRepository},
			Heartbeater:       serverRepository,
			Claimer:           leaseRepository,
			Renewer:           leaseRepository,
			Completer:         leaseRepository,
			Canceller:         leaseRepository,
			Reconciler:        leaseRepository,
			TrustedProxyCIDRs: webConfig.TrustedProxyCIDRs,
		}
		if handler, ok := agentHandler.(agenthttp.Handler); ok {
			// Secret materialization is wired only when the secret subsystem
			// is enabled. Left nil, the route answers 503 rather than an empty
			// secret set an agent could mistake for "this deployment has no
			// secrets" (agenthttp.handleMaterializeSecrets fails closed).
			handler.MaterializationAuthorizer = leaseRepository
			handler.ProgressReporter = leaseRepository
			if enabled {
				handler.SecretMaterializerFactory = secretMaterializerFactory{postgres.NewSecretRepository(pool)}
			}
			// The checkout routes need the GitHub App subsystem in addition to
			// the agent protocol; leaving any one of these four nil answers
			// 503, never a partial success (agenthttp's own dependency-nil
			// discipline), so all four are wired together or not at all.
			if checkoutService != nil && checkoutStore != nil {
				handler.CheckoutAuthorizer = leaseRepository
				handler.CheckoutStore = checkoutStore
				handler.CheckoutActivator = checkoutService
				handler.CheckoutRegistry = agenthttp.NewCheckoutRegistry()
			}
			agentHandler = handler
		}
		sweepLogger, _ := controlplaneLogger(os.Stderr, nil)
		sweeper = postgres.NewAgentLeaseSweeper(pool, leaseRepository).WithTick(agentConfig.SweepInterval).WithErrorHandler(func() { sweepLogger.Print("agent lease sweep failed") })
	}
	handler, err := web.New(repository, webOptions)
	if err != nil {
		return err
	}
	httpHandler := composeHTTPHandler(handler, webhookHandler, agentHandler)
	if enabled {
		httpHandler = secretReadyHandler(httpHandler, func(requestContext context.Context) error {
			ring, generation, loadErr := config.KEKRingAndGenerationFromEnvironment()
			if loadErr != nil {
				return errors.New("secret subsystem unavailable")
			}
			return secretLedgerReady(requestContext, postgres.NewKeyRepository(pool), ring, generation)
		})
	}
	server := newHTTPServer(webConfig.ListenAddress, httpHandler)
	errorsChannel := make(chan error, 1)
	go func() { errorsChannel <- server.ListenAndServe() }()

	// The sweeper only runs when the agent protocol is enabled; its context
	// is cancelled alongside the HTTP server's shutdown signal so it never
	// outlives this process, and shutdown waits for it to stop within the
	// same bounded shutdown window as the HTTP server.
	var sweeperDone chan struct{}
	var sweeperCancel context.CancelFunc
	if sweeper != nil {
		var sweeperContext context.Context
		sweeperContext, sweeperCancel = context.WithCancel(context.Background())
		sweeperDone = make(chan struct{})
		go func() { defer close(sweeperDone); sweeper.Run(sweeperContext) }()
	}
	// Deferred after the pool close, so it runs first on every return path:
	// the pool is never closed while a sweep may still be using it.
	defer func() {
		if sweeperCancel == nil {
			return
		}
		sweeperCancel()
		waitContext, cancelWait := newShutdownContext()
		defer cancelWait()
		select {
		case <-sweeperDone:
		case <-waitContext.Done():
		}
	}()

	stopped := make(chan os.Signal, 1)
	signal.Notify(stopped, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(stopped)
	select {
	case err = <-errorsChannel:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
	case <-stopped:
	}
	shutdownContext, cancel := newShutdownContext()
	defer cancel()
	shutdownErr := server.Shutdown(shutdownContext)
	if sweeperCancel != nil {
		sweeperCancel()
		select {
		case <-sweeperDone:
		case <-shutdownContext.Done():
		}
	}
	if shutdownErr != nil {
		return shutdownErr
	}
	return nil
}

// composeHTTPHandler mirrors the webhook route's mount point exactly for the
// agent route: both are matched and dispatched before the browser
// session/CSRF handler ever runs, and a nil handler (protocol disabled)
// leaves that path/prefix routed to the browser handler exactly as before
// this wiring existed. agentHandler is matched by prefix because it serves
// multiple sub-paths (enroll, heartbeat, jobs/claim, ...); webhookHandler
// keeps its single exact-path match.
func composeHTTPHandler(browserHandler, webhookHandler, agentHandler http.Handler) http.Handler {
	if webhookHandler == nil && agentHandler == nil {
		return browserHandler
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if webhookHandler != nil && r.URL != nil && r.URL.Path == githubhttp.Path {
			webhookHandler.ServeHTTP(w, r)
			return
		}
		if agentHandler != nil && r.URL != nil && strings.HasPrefix(r.URL.Path, agenthttp.Prefix) {
			agentHandler.ServeHTTP(w, r)
			return
		}
		browserHandler.ServeHTTP(w, r)
	})
}

// serverAdmin composes ServerRepository (server register/enrollment/revoke)
// and AgentLeaseRepository (reconciling-environment reads and break-glass)
// into the single web.ServerAdmin surface the /servers routes use.
type serverAdmin struct {
	servers *postgres.ServerRepository
	leases  *postgres.AgentLeaseRepository
}

func (a serverAdmin) ListServers(ctx context.Context, p auth.Principal) ([]postgres.ServerSummary, error) {
	return a.servers.ListServers(ctx, p)
}
func (a serverAdmin) RegisterServer(ctx context.Context, p auth.Principal, in postgres.RegisterServerInput) (server.Server, time.Time, error) {
	return a.servers.RegisterServer(ctx, p, in)
}
func (a serverAdmin) IssueEnrollment(ctx context.Context, p auth.Principal, serverID string, digest [32]byte, ttlSeconds int) (time.Time, error) {
	return a.servers.IssueEnrollment(ctx, p, serverID, digest, ttlSeconds)
}
func (a serverAdmin) RevokeServer(ctx context.Context, p auth.Principal, serverID, reason string) error {
	return a.servers.RevokeServer(ctx, p, serverID, reason)
}
func (a serverAdmin) ListReconcilingEnvironments(ctx context.Context, p auth.Principal) ([]postgres.ReconcilingEnvironment, error) {
	return a.leases.ListReconcilingEnvironments(ctx, p)
}
func (a serverAdmin) MarkReconciled(ctx context.Context, p auth.Principal, environmentID, reason string) error {
	return a.leases.MarkReconciled(ctx, p, environmentID, reason)
}

// serverAuthenticator adapts (*postgres.ServerRepository).Authenticate, which
// returns the authenticated server record, to agenthttp.Authenticator, which
// only ever needs to know whether authentication succeeded.
type serverAuthenticator struct {
	repository *postgres.ServerRepository
}

func (a serverAuthenticator) Authenticate(ctx context.Context, serverID string, presentedDigest [32]byte) error {
	_, err := a.repository.Authenticate(ctx, serverID, presentedDigest)
	return err
}

func clearWebhookSecrets(ring *githubhttp.SecretRing) {
	if ring == nil {
		return
	}
	clear(ring.Active)
	ring.Active = nil
	for version, value := range ring.Retained {
		clear(value)
		ring.Retained[version] = nil
	}
}

func secretLedgerReady(ctx context.Context, repository *postgres.KeyRepository, ring secretcrypto.KeyRing, generation string) error {
	id, key, err := ring.Active()
	clear(key)
	if err != nil {
		return errors.New("secret subsystem unavailable")
	}
	exists, err := repository.LedgerReady(ctx, id, generation)
	if err != nil || !exists {
		return errors.New("secret subsystem unavailable")
	}
	return nil
}

func newShutdownContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), shutdownTimeout)
}

func newHTTPServer(address string, handler http.Handler) *http.Server {
	logger, _ := controlplaneLogger(os.Stderr, nil)
	return &http.Server{Addr: address, Handler: handler, ErrorLog: logger, ReadHeaderTimeout: readHeaderTimeout, ReadTimeout: readTimeout, WriteTimeout: writeTimeout, IdleTimeout: idleTimeout, MaxHeaderBytes: maxHeaderBytes}
}

// secretMaterializerFactory builds one request-scoped secret service whose
// authorizer admits exactly the frozen set the agent lease repository just
// authorized, so no long-lived object in this process can decrypt anything
// wider.
//
// The KEK ring is loaded per request rather than held resident, matching how
// the readiness probe reloads it: this control plane deliberately keeps no
// KEK material in memory between operations, and materialization is rare and
// bounded enough to pay a file read for that property. A load failure returns
// nil, which the route turns into a fail-closed 503.
type secretMaterializerFactory struct {
	repository *postgres.SecretRepository
}

func (f secretMaterializerFactory) MaterializerForFrozenSet(keys []secret.AuthorizationKey) agenthttp.SecretMaterializer {
	ring, _, err := config.KEKRingAndGenerationFromEnvironment()
	if err != nil {
		return nil
	}
	return secret.NewService(f.repository, ring, secret.NewSetAuthorizer(keys))
}

func secretReadyHandler(next http.Handler, ready func(context.Context) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL != nil && r.URL.Path == "/readyz" && ready(r.Context()) != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "unavailable\n")
			return
		}
		next.ServeHTTP(w, r)
	})
}
