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
	"syscall"
	"time"

	"autodeploy/internal/auth"
	"autodeploy/internal/config"
	secretcrypto "autodeploy/internal/crypto"
	"autodeploy/internal/githubapp"
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
	if githubConfig.Enabled {
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
	}
	handler, err := web.New(repository, webOptions)
	if err != nil {
		return err
	}
	var httpHandler http.Handler = handler
	if enabled {
		httpHandler = secretReadyHandler(handler, func(requestContext context.Context) error {
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
	if err = server.Shutdown(shutdownContext); err != nil {
		return err
	}
	return nil
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
