// Command controlplane serves the AutoDeploy browser control plane.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"autodeploy/internal/admincli"
	agenthttp "autodeploy/internal/agent/http"
	"autodeploy/internal/auth"
	"autodeploy/internal/config"
	secretcrypto "autodeploy/internal/crypto"
	githubhttp "autodeploy/internal/github/http"
	"autodeploy/internal/githubapp"
	"autodeploy/internal/logging"
	"autodeploy/internal/metrics"
	"autodeploy/internal/secret"
	"autodeploy/internal/server"
	"autodeploy/internal/store/postgres"
	"autodeploy/internal/web"

	"github.com/jackc/pgx/v5/pgxpool"
)

// activationEnabled gates whether run wires Step 10c's tx1
// (POST /agent/v1/leases/activate) and tx2 (POST
// /agent/v1/leases/activation-result) onto the agent handler. It mirrors
// cmd/agent's executorEnabled convention exactly (see that file's package
// doc for the full rationale): a single hardcoded constant, flipped only by
// a separately reviewed commit, never an environment variable. While it
// stays false, agentHandler.Activator and .ActivationReporter are left nil below, and
// agenthttp.Handler's existing nil-dependency discipline (every other route
// in that package already follows it) answers 503 service_unavailable
// before touching any repository -- no separate per-request gate check is
// needed in the handler itself.
const activationEnabled = false

const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 15 * time.Second
	writeTimeout      = 20 * time.Second
	idleTimeout       = 60 * time.Second
	shutdownTimeout   = 20 * time.Second
	maxHeaderBytes    = 16 << 10
)

// startupTimeout bounds the startup database steps (ping, secret ledger check,
// password policy load).
const startupTimeout = 30 * time.Second

// Server-side limits applied to every control-plane connection. They stop a
// stuck row/advisory lock or runaway statement from wedging a request or the
// lease sweeper. Both surface as transient SQLSTATEs (55P03 lock_timeout,
// 57014 statement_timeout) in activation classification. Statements that need
// longer set their own (metrics snapshot uses SET LOCAL statement_timeout);
// migrations run on cmd/migrate's own connection and are not limited.
const (
	poolLockTimeoutMS      = "10000" // 10s: the longest legitimate lock holder is a short tx
	poolStatementTimeoutMS = "60000" // 60s: above the 45s sweep bound, below any wedge
)

// controlPlanePoolConfig parses dsn and applies the timeouts above unless the
// DSN already sets them explicitly.
func controlPlanePoolConfig(dsn string) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	for k, v := range map[string]string{"lock_timeout": poolLockTimeoutMS, "statement_timeout": poolStatementTimeoutMS} {
		if _, set := cfg.ConnConfig.RuntimeParams[k]; !set {
			cfg.ConnConfig.RuntimeParams[k] = v
		}
	}
	return cfg, nil
}

func main() {
	if err := run(context.Background()); err != nil {
		logger := controlplaneLogger(os.Stderr, nil)
		// Only the source line of the failing startup step is logged: error
		// text can carry configuration values (e.g. a database URL).
		if site := strings.TrimPrefix(failureSite(err), " at "); site != "" {
			logger.Error("control plane unavailable", logging.KeyStage, site)
		} else {
			logger.Error("control plane unavailable")
		}
		os.Exit(1)
	}
}

// controlplaneLogger is the sole process-log construction boundary. Callers
// pass any transient secret values when a future integration can emit related
// output; failures are deliberately reduced to fixed public messages.
func controlplaneLogger(dst io.Writer, values [][]byte) *slog.Logger {
	logger, err := logging.NewControlPlane(dst, values)
	if err != nil {
		return slog.New(slog.DiscardHandler)
	}
	return logger
}

// startupPasswordPolicy returns the installation's stored password policy.
// Before the first administrator exists there is none (ErrNotFound); the
// control plane must still start so the browser first-run /setup can serve, so
// it calibrates exactly as `admin bootstrap` does. The calibrated policy is
// then what /setup hashes with and persists, so it matches the stored one
// after bootstrap. durable=false tells the caller to let the web layer adopt
// the persisted policy lazily (a terminal bootstrap may persist a different
// calibration while this process runs). Any other load error stays fatal.
func startupPasswordPolicy(ctx context.Context, repository interface {
	LoadPasswordPolicy(context.Context) (auth.Argon2Policy, error)
}, calibrate func() (auth.Argon2Policy, error)) (policy auth.Argon2Policy, durable bool, err error) {
	policy, err = repository.LoadPasswordPolicy(ctx)
	if errors.Is(err, postgres.ErrNotFound) {
		policy, err = calibrate()
		return policy, false, err
	}
	return policy, err == nil, err
}

func calibratePasswordPolicy() (auth.Argon2Policy, error) {
	return admincli.Calibrate(func(policy auth.Argon2Policy) (time.Duration, error) {
		start := time.Now()
		_, err := auth.HashPassword("autodeploy-calibration-password", policy, bytes.NewReader(bytes.Repeat([]byte{0x6d}, 16)))
		return time.Since(start), err
	})
}

func run(ctx context.Context) error {
	// One redacting process logger, installed as the default before any
	// startup step can log, so stray library or std log output is redacted too.
	processLogger := controlplaneLogger(os.Stderr, nil)
	slog.SetDefault(processLogger)
	dsn, err := config.DatabaseURLFromEnvironment()
	if err != nil {
		return failAt(err)
	}
	webConfig, err := config.WebConfigFromEnvironment()
	if err != nil {
		return failAt(err)
	}
	metricsAddress, err := config.MetricsListenAddressFromEnvironment()
	if err != nil {
		return failAt(err)
	}
	throttleKeys, err := config.UsernameThrottleKeyRingFromEnvironment()
	if err != nil {
		return failAt(err)
	}
	poolConfig, err := controlPlanePoolConfig(dsn)
	if err != nil {
		return failAt(err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return failAt(err)
	}
	defer pool.Close()
	buildInfo, buildInfoOK := debug.ReadBuildInfo()
	metricSet := newMetricsSet(buildVersion(buildInfo, buildInfoOK), postgres.NewMetricsRepository(pool).MetricsSnapshot, processLogger)
	repository := postgres.NewAuthRepository(pool)
	// Startup database steps share one deadline so an unreachable or wedged
	// database fails the unit instead of hanging it. Serving uses ctx.
	startupCtx, cancelStartup := context.WithTimeout(ctx, startupTimeout)
	defer cancelStartup()
	if err = repository.Ping(startupCtx); err != nil {
		return failAt(err)
	}
	enabled, err := config.SecretSubsystemEnabled()
	if err != nil {
		return failAt(err)
	}
	if enabled {
		ring, generation, ringErr := config.KEKRingAndGenerationFromEnvironment()
		if ringErr != nil {
			return failAt(errors.New("secret subsystem unavailable"))
		}
		if readyErr := secretLedgerReady(startupCtx, postgres.NewKeyRepository(pool), ring, generation); readyErr != nil {
			return failAt(errors.New("secret subsystem unavailable"))
		}
	}
	policy, durablePolicy, err := startupPasswordPolicy(startupCtx, repository, calibratePasswordPolicy)
	if err != nil {
		return failAt(err)
	}
	dummy, err := auth.BuildDummyPasswordVerifier(policy)
	if err != nil {
		return failAt(err)
	}
	var policyLoader func(context.Context) (auth.Argon2Policy, error)
	if !durablePolicy {
		policyLoader = repository.LoadPasswordPolicy
	}
	// The web and agent handlers log through processLogger: nothing in them
	// may write to standard error directly.
	webOptions := web.Config{PublicOrigin: webConfig.PublicOrigin, CSRFKeys: webConfig.CSRFKeys, UsernameThrottleKeys: throttleKeys, TrustedProxyCIDRs: webConfig.TrustedProxyCIDRs, AuthThrottleMaxRows: webConfig.AuthThrottleMaxRows, PasswordPolicy: policy, PolicyLoader: policyLoader, Setup: repository, DummyPassword: dummy, ActivationEnabled: activationEnabled, Logger: processLogger}
	githubConfig, err := config.GitHubAppConfigFromEnvironment()
	if err != nil {
		return failAt(err)
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
			return failAt(errors.New("GitHub setup unavailable"))
		}
		defer clearWebhookSecrets(&webhookSecrets)
		signer, signerErr := config.GitHubAppSignerFromMountedKey(githubConfig)
		if signerErr != nil {
			return failAt(errors.New("GitHub setup unavailable"))
		}
		provider, providerErr := githubapp.NewHTTPProvider(signer, "autodeploy-controlplane")
		if providerErr != nil {
			return failAt(errors.New("GitHub setup unavailable"))
		}
		identity := githubapp.AppIdentity{AppID: githubConfig.AppID, ClientID: githubConfig.ClientID, Slug: githubConfig.Slug}
		owner := githubapp.Owner{AccountID: githubConfig.OwnerAccountID, Type: githubConfig.OwnerType}
		githubRepository := postgres.NewGitHubRepository(pool)
		if registerErr := githubRepository.EnsureAppRegistration(ctx, identity, owner); registerErr != nil {
			return failAt(errors.New("GitHub setup unavailable"))
		}
		setup, setupErr := githubapp.NewSetupService(githubRepository, provider, identity, owner, rand.Reader)
		if setupErr != nil {
			return failAt(errors.New("GitHub setup unavailable"))
		}
		webOptions.GitHubSetup, webOptions.GitHubInstallationURL = setup, githubConfig.InstallationURL
		webOptions.RepositoryLister = githubRepository
		webhookRepository := postgres.NewWebhookJobRepository(pool, postgres.NewDeploymentRepository(pool))
		// Manual deploy (step 19d) needs the GitHub App provider to resolve and
		// verify commits, so it is only wired when the subsystem is enabled.
		webOptions.ManualDeploy, webOptions.CommitResolver = webhookRepository, provider
		webhookHandler = githubhttp.Handler{Secrets: webhookSecrets, TrustedProxyCIDRs: webConfig.TrustedProxyCIDRs, Processor: postgres.NewWebhookProcessor(webhookRepository)}
		checkoutService, err = githubapp.NewCheckoutService(githubRepository, provider)
		if err != nil {
			return failAt(errors.New("GitHub setup unavailable"))
		}
		checkoutStore = githubRepository
	}
	// serverRepository and leaseRepository back the session/CSRF-protected
	// web /servers and /environments/resolve admin routes (Step 6b);
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
	// The route page (hostname assignment) was never wired, so /environments/route
	// always 404ed (found on the acer). RouteHeadRepository satisfies web.RouteHeadAdmin.
	webOptions.RouteHeadAdmin = postgres.NewRouteHeadRepository(pool)
	webOptions.OperationsReader = postgres.NewOperationsReadRepository(pool)
	webOptions.DeploymentCanceller = leaseRepository
	// Rollback (step 20D) restarts a retained container; it needs no GitHub
	// access, so it is wired whether or not the GitHub App subsystem is enabled.
	webOptions.Rollback = postgres.NewWebhookJobRepository(pool, postgres.NewDeploymentRepository(pool))
	webOptions.DeploymentLogReader = postgres.NewDeploymentLogRepository(pool)
	agentConfig, err := config.AgentConfigFromEnvironment()
	if err != nil {
		return failAt(err)
	}
	var agentHandler http.Handler
	var sweeper *postgres.AgentLeaseSweeper
	var retention *postgres.EventRetentionPurger
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
			Logger:            processLogger,
		}
		if handler, ok := agentHandler.(agenthttp.Handler); ok {
			// Secret materialization is wired only when the secret subsystem
			// is enabled. Left nil, the route answers 503 rather than an empty
			// secret set an agent could mistake for "this deployment has no
			// secrets" (agenthttp.handleMaterializeSecrets fails closed).
			handler.MaterializationAuthorizer = leaseRepository
			handler.ProgressReporter = leaseRepository
			handler.LogAppender = postgres.NewDeploymentLogRepository(pool)
			if activationEnabled {
				handler.Activator = leaseRepository
				handler.ActivationReporter = leaseRepository
				handler.DrainRecorder = leaseRepository
				handler.CleanupStore = leaseRepository
			}
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
		logPurger, purgeErr := postgres.NewLogPurgeScheduler(pool, postgres.NewDeploymentLogRepository(pool), postgres.LogPurgeOptions{
			Retention: postgres.LogRetention{MaxAge: agentConfig.LogRetentionMaxAge, KeepNewestPerEnv: agentConfig.LogRetentionKeepNewest},
			OnError:   func() { processLogger.Error("deployment log purge failed", logging.KeyComponent, "log_purge") },
			OnRun:     metricSet.onPurgeRun,
		})
		if purgeErr != nil {
			return purgeErr
		}
		// F13 retention: hourly purge of old webhook deliveries, terminal-job
		// events and route events, single-flight across replicas.
		retentionPurger, retentionErr := postgres.NewEventRetentionPurger(pool, postgres.EventRetentionOptions{
			OnError: func() { processLogger.Error("event retention purge failed", logging.KeyComponent, "event_retention") },
		})
		if retentionErr != nil {
			return retentionErr
		}
		retention = retentionPurger
		sweeper = postgres.NewAgentLeaseSweeper(pool, leaseRepository).WithTick(agentConfig.SweepInterval).WithErrorHandler(func() { processLogger.Error("agent lease sweep failed", logging.KeyComponent, "lease_sweep") }).WithRunHandler(metricSet.onSweepRun).WithLogPurge(logPurger)
	}
	handler, err := web.New(repository, webOptions)
	if err != nil {
		return failAt(err)
	}
	httpHandler := composeHTTPHandler(handler, webhookHandler, agentHandler, metricSet.requests)
	if enabled {
		httpHandler = secretReadyHandler(httpHandler, func(requestContext context.Context) error {
			ring, generation, loadErr := config.KEKRingAndGenerationFromEnvironment()
			if loadErr != nil {
				return failAt(errors.New("secret subsystem unavailable"))
			}
			return secretLedgerReady(requestContext, postgres.NewKeyRepository(pool), ring, generation)
		})
	}
	server := newHTTPServer(webConfig.ListenAddress, httpHandler, processLogger)
	// The metrics listener (loopback-only, disabled unless configured) binds
	// before anything serves, so a bind failure fails startup like the main
	// listener's does.
	metricsServer, metricsListener, err := startMetricsListener(metricsAddress, newMetricsHandler(metricSet.registry), processLogger)
	if err != nil {
		return failAt(err)
	}
	errorsChannel := make(chan error, 1)
	go func() { errorsChannel <- server.ListenAndServe() }()
	if metricsServer != nil {
		// A metrics listener failure is logged, never fatal to the control plane.
		serveMetrics(metricsServer, metricsListener, processLogger)
	}

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
	var retentionDone chan struct{}
	var retentionCancel context.CancelFunc
	if retention != nil {
		var retentionContext context.Context
		retentionContext, retentionCancel = context.WithCancel(context.Background())
		retentionDone = make(chan struct{})
		go func() { defer close(retentionDone); retention.Run(retentionContext) }()
	}
	// stopRetention joins the purger within the bounded shutdown window.
	stopRetention := func(wait context.Context) {
		if retentionCancel == nil {
			return
		}
		retentionCancel()
		select {
		case <-retentionDone:
		case <-wait.Done():
		}
	}
	// Deferred after the pool close, so it runs first on every return path:
	// the pool is never closed while a sweep may still be using it.
	defer func() {
		waitRetention, cancelRetention := newShutdownContext()
		defer cancelRetention()
		stopRetention(waitRetention)
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
			return failAt(err)
		}
		return nil
	case <-ctx.Done():
	case <-stopped:
	}
	shutdownContext, cancel := newShutdownContext()
	defer cancel()
	shutdownErr := server.Shutdown(shutdownContext)
	if metricsServer != nil {
		shutdownErr = errors.Join(shutdownErr, metricsServer.Shutdown(shutdownContext))
	}
	if sweeperCancel != nil {
		sweeperCancel()
		select {
		case <-sweeperDone:
		case <-shutdownContext.Done():
		}
	}
	stopRetention(shutdownContext)
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
//
// A non-nil requests counter wraps each surface's handler with the
// status-counting middleware (surface web, agent, webhook); nil leaves the
// handlers untouched.
func composeHTTPHandler(browserHandler, webhookHandler, agentHandler http.Handler, requests *metrics.Counter) http.Handler {
	browserHandler = instrument(surfaceWeb, requests, browserHandler)
	webhookHandler = instrument(surfaceWebhook, requests, webhookHandler)
	agentHandler = instrument(surfaceAgent, requests, agentHandler)
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
func (a serverAdmin) IssueReenrollment(ctx context.Context, p auth.Principal, serverID string, digest [32]byte, ttlSeconds int, reason string) (time.Time, error) {
	return a.servers.IssueReenrollment(ctx, p, serverID, digest, ttlSeconds, reason)
}
func (a serverAdmin) RevokeServer(ctx context.Context, p auth.Principal, serverID, reason string) error {
	return a.servers.RevokeServer(ctx, p, serverID, reason)
}
func (a serverAdmin) ListReconcilingEnvironments(ctx context.Context, p auth.Principal) ([]postgres.ReconcilingEnvironment, error) {
	return a.leases.ListReconcilingEnvironments(ctx, p)
}
func (a serverAdmin) ResolveActivation(ctx context.Context, p auth.Principal, environmentID string, expectedAttemptID int64, outcome, reason string) error {
	return a.leases.ResolveActivation(ctx, p, environmentID, expectedAttemptID, outcome, reason)
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

// newHTTPServer routes net/http's ErrorLog through the process logger's own
// handler (one core, one mutex) at ERROR level.
func newHTTPServer(address string, handler http.Handler, logger *slog.Logger) *http.Server {
	errorLog := slog.NewLogLogger(logger.Handler(), slog.LevelError)
	return &http.Server{Addr: address, Handler: handler, ErrorLog: errorLog, ReadHeaderTimeout: readHeaderTimeout, ReadTimeout: readTimeout, WriteTimeout: writeTimeout, IdleTimeout: idleTimeout, MaxHeaderBytes: maxHeaderBytes}
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

// startupError records where run() failed without exposing the error text.
type startupError struct {
	err  error
	site string
}

func (e startupError) Error() string { return e.err.Error() }
func (e startupError) Unwrap() error { return e.err }

// failAt tags err with the caller's file:line (nil stays nil).
func failAt(err error) error {
	if err == nil {
		return nil
	}
	_, file, line, ok := runtime.Caller(1)
	if !ok {
		return err
	}
	return startupError{err: err, site: fmt.Sprintf("%s:%d", filepath.Base(file), line)}
}

// failureSite returns " at file:line" for a startupError, else "".
func failureSite(err error) string {
	var se startupError
	if errors.As(err, &se) {
		return " at " + se.site
	}
	return ""
}
