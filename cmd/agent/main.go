// Command agent is the AutoDeploy deployment-agent entry point (tasks/todo.md
// Step 6c, extended by Step 6b). Slice 9b Step 6b assembles a real Executor
// (internal/agent/executor) -- checkout, buildx build, container run, ADR
// 0005 health gate -- but executorEnabled below stays false: no Executor is
// registered on the Agent, so `agent run`'s heartbeat loop always reports
// executor_available=false and the claim/lease path in internal/agent/client
// never runs against real infrastructure. That is deliberate, not an
// oversight: the privileged Linux checkout smoke and a real Docker daemon
// have not yet validated internal/agent/executor's code (see its package doc
// and the Step 6b report), and flipping executorEnabled is what starts the
// control plane handing this agent real leases. Flip the single constant
// once that validation lands; nothing else in this file needs to change.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	agentclient "autodeploy/internal/agent/client"
	"autodeploy/internal/agent/executor"
	"autodeploy/internal/agent/state"
	"autodeploy/internal/checkoutcontain"
	"autodeploy/internal/checkoutquota"
	"autodeploy/internal/gitcheckout"
	"autodeploy/internal/runtime/docker"
	"autodeploy/internal/runtime/traefik"
)

// agentVersion is reported on every heartbeat. It has no runtime effect in
// this slice; it exists so operators can tell agent binaries apart in
// server_audit_events/heartbeat history once a later slice persists it.
const agentVersion = "0.1.0-dev"

// executorEnabled gates whether runAgent registers the real Executor Step
// 6b assembled. See the package doc above: this is the one line a later,
// separately reviewed commit flips to true. Flag parsing runs today; executor
// and activation construction, their validation (validateActivationFlags,
// NewExecutor) and the capability probe are wired but run ONLY when this is
// true -- they are covered by unit tests, not exercised at startup.
//
// Prerequisites for that flip (both outside this commit's scope):
//  1. The privileged Linux checkout smoke, already called out as pending
//     before the first real deployment.
//  2. A configured health-gate path/Host column: none exists yet
//     (migrations 000014/000016 carry only container_port/container_scheme),
//     so internal/agent/executor currently probes a fixed "/" path with a
//     direct-IP Host header for every deployment -- see
//     healthGatePathV1Default and healthGateHost in
//     internal/agent/executor/executor.go. An application that never
//     returns 2xx on "/" cannot pass the gate today, and that failure
//     classifies permanent_configuration (never retried), so this is a
//     real deployment-blocking gap, not a cosmetic one.
const executorEnabled = false

// Defaults for the checkout quota/spawn limits Step 6b's production
// checkout runner needs (internal/gitcheckout.ProductionConfig). These are
// conservative fixed values, not yet operator-tunable; revisit alongside
// flipping executorEnabled if a real host needs different bounds.
const (
	defaultCheckoutQuotaBytes      = 10 << 30 // 10 GiB
	defaultCheckoutQuotaInodes     = 2_000_000
	defaultCheckoutSpawnTimeoutSec = 120
	defaultCheckoutSpawnMaxOutput  = 1 << 20 // matches checkoutcontain's own upper bound
)

// Defaults for the activation flags (tasks/10d-plan.md, D-D). The route
// directory has no safe universal default and is required instead.
const (
	defaultTraefikAPIURL          = "http://127.0.0.1:8080"
	defaultTraefikHTTPSAddr       = "127.0.0.1:443"
	defaultActivationProbePath    = "/"
	defaultActivationProbeTimeout = 3 * time.Minute
	defaultDrainStopGrace         = 30 * time.Second
	maxDrainStopGrace             = 300 * time.Second
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "autodeploy agent: command failed")
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stderr io.Writer) error {
	c, err := parseArgs(args)
	if err != nil {
		return err
	}
	switch c.action {
	case "enroll":
		return runEnroll(c, stdin, stderr)
	default:
		return runAgent(c, stderr)
	}
}

// command is the parsed, testable shape of one CLI invocation. Every field
// is validated by parseArgs before any I/O; runEnroll/runAgent never see a
// malformed value.
type command struct {
	action    string // "enroll" or "run"
	baseURL   string
	serverID  string
	stateDir  string
	tokenFile string // enroll only; empty means read stdin

	// The fields below configure Step 6b's Executor. They are accepted (and
	// validated when present) for "run" regardless of executorEnabled, so
	// flipping that constant later needs no flag-parsing change -- but they
	// are only read when executorEnabled is true; runAgent never requires
	// them while it stays false.
	dockerSocket        string // defaults to /var/run/docker.sock
	checkoutQuotaSocket string
	checkoutSpawnSocket string
	checkoutAgentGID    uint32
	environmentNetwork  string

	// Activation (Step 10d-5b). Like the fields above they are parsed for
	// "run" regardless of executorEnabled but validated and used only when it
	// is true (see validateActivationFlags).
	traefikRouteDir        string // required when the executor is enabled; no default
	traefikAPIURL          string // loopback only
	traefikHTTPSAddr       string // literal loopback host:port
	activationProbePath    string
	activationProbeTimeout time.Duration // > 0
	activationTimeout      time.Duration // 0 = derived default
	drainStopGrace         time.Duration // whole seconds, 1..300s (Step 10e-4)
}

func parseArgs(args []string) (command, error) {
	if len(args) < 1 || (args[0] != "enroll" && args[0] != "run") {
		return command{}, errors.New("usage: agent <enroll|run> --base-url <https://...> --server-id <id> --state-dir <dir> [--token-file <path>]")
	}
	c := command{action: args[0]}
	rest := args[1:]
	if len(rest)%2 != 0 {
		return command{}, errors.New("invalid arguments")
	}
	for i := 0; i < len(rest); i += 2 {
		flag, value := rest[i], rest[i+1]
		switch flag {
		case "--base-url":
			c.baseURL = value
		case "--server-id":
			c.serverID = value
		case "--state-dir":
			c.stateDir = value
		case "--token-file":
			if c.action != "enroll" {
				return command{}, errors.New("--token-file is only valid for enroll")
			}
			c.tokenFile = value
		case "--docker-socket":
			if c.action != "run" {
				return command{}, errors.New("--docker-socket is only valid for run")
			}
			c.dockerSocket = value
		case "--checkout-quota-socket":
			if c.action != "run" {
				return command{}, errors.New("--checkout-quota-socket is only valid for run")
			}
			c.checkoutQuotaSocket = value
		case "--checkout-spawn-socket":
			if c.action != "run" {
				return command{}, errors.New("--checkout-spawn-socket is only valid for run")
			}
			c.checkoutSpawnSocket = value
		case "--checkout-agent-gid":
			if c.action != "run" {
				return command{}, errors.New("--checkout-agent-gid is only valid for run")
			}
			gid, err := strconv.ParseUint(value, 10, 32)
			if err != nil {
				return command{}, fmt.Errorf("invalid --checkout-agent-gid: %w", err)
			}
			c.checkoutAgentGID = uint32(gid)
		case "--environment-network":
			if c.action != "run" {
				return command{}, errors.New("--environment-network is only valid for run")
			}
			c.environmentNetwork = value
		case "--traefik-route-dir":
			if c.action != "run" {
				return command{}, errors.New("--traefik-route-dir is only valid for run")
			}
			c.traefikRouteDir = value
		case "--traefik-api-url":
			if c.action != "run" {
				return command{}, errors.New("--traefik-api-url is only valid for run")
			}
			c.traefikAPIURL = value
		case "--traefik-https-addr":
			if c.action != "run" {
				return command{}, errors.New("--traefik-https-addr is only valid for run")
			}
			c.traefikHTTPSAddr = value
		case "--activation-probe-path":
			if c.action != "run" {
				return command{}, errors.New("--activation-probe-path is only valid for run")
			}
			c.activationProbePath = value
		case "--activation-probe-timeout":
			if c.action != "run" {
				return command{}, errors.New("--activation-probe-timeout is only valid for run")
			}
			d, err := time.ParseDuration(value)
			if err != nil {
				return command{}, fmt.Errorf("invalid --activation-probe-timeout: %w", err)
			}
			c.activationProbeTimeout = d
		case "--activation-timeout":
			if c.action != "run" {
				return command{}, errors.New("--activation-timeout is only valid for run")
			}
			d, err := time.ParseDuration(value)
			if err != nil {
				return command{}, fmt.Errorf("invalid --activation-timeout: %w", err)
			}
			c.activationTimeout = d
		case "--drain-stop-grace":
			if c.action != "run" {
				return command{}, errors.New("--drain-stop-grace is only valid for run")
			}
			d, err := time.ParseDuration(value)
			if err != nil {
				return command{}, fmt.Errorf("invalid --drain-stop-grace: %w", err)
			}
			c.drainStopGrace = d
		default:
			return command{}, fmt.Errorf("unknown flag %q", flag)
		}
	}
	if c.baseURL == "" || c.serverID == "" || c.stateDir == "" || !filepath.IsAbs(c.stateDir) {
		return command{}, errors.New("--base-url, --server-id, and an absolute --state-dir are required")
	}
	if c.action == "run" {
		if c.dockerSocket == "" {
			c.dockerSocket = "/var/run/docker.sock"
		}
		if c.traefikAPIURL == "" {
			c.traefikAPIURL = defaultTraefikAPIURL
		}
		if c.traefikHTTPSAddr == "" {
			c.traefikHTTPSAddr = defaultTraefikHTTPSAddr
		}
		if c.activationProbePath == "" {
			c.activationProbePath = defaultActivationProbePath
		}
		if c.activationProbeTimeout == 0 {
			c.activationProbeTimeout = defaultActivationProbeTimeout
		}
		if c.drainStopGrace == 0 {
			c.drainStopGrace = defaultDrainStopGrace
		}
	}
	return c, nil
}

// validateActivationFlags checks the activation flags. runAgent calls it only
// when executorEnabled is true; it is a separate function so it is unit-tested
// directly. Errors name the flag and never echo secrets (none are involved).
func validateActivationFlags(c command) error {
	if c.traefikRouteDir == "" || !filepath.IsAbs(c.traefikRouteDir) {
		return errors.New("--traefik-route-dir is required and must be an absolute path when the executor is enabled")
	}
	if fi, err := os.Stat(c.traefikRouteDir); err != nil || !fi.IsDir() {
		return errors.New("--traefik-route-dir must be an existing directory")
	}
	if _, err := traefik.NewObserver(c.traefikAPIURL); err != nil {
		return fmt.Errorf("invalid --traefik-api-url: %w", err)
	}
	if err := traefik.ValidateProbeAddress(c.traefikHTTPSAddr); err != nil {
		return fmt.Errorf("invalid --traefik-https-addr: %w", err)
	}
	if err := traefik.ValidateProbePath(c.activationProbePath); err != nil {
		return fmt.Errorf("invalid --activation-probe-path: %w", err)
	}
	if c.activationProbeTimeout <= 0 {
		return errors.New("--activation-probe-timeout must be greater than zero")
	}
	if c.activationTimeout < 0 {
		return errors.New("--activation-timeout must not be negative")
	}
	if c.drainStopGrace < time.Second || c.drainStopGrace > maxDrainStopGrace || c.drainStopGrace%time.Second != 0 {
		return errors.New("--drain-stop-grace must be whole seconds within 1s..300s")
	}
	return nil
}

// activationParts is everything the activation sequence needs from cmd/agent.
type activationParts struct {
	config   executor.ActivationConfig
	reporter *agentclient.ActivationReporter
	recover  func()
	drainer  *executor.Drainer
}

// newDrainDocker builds the drainer's Engine client; a variable so tests can
// run on platforms where the docker runtime is unsupported.
var newDrainDocker = func(socket string) (executor.DrainDocker, error) { return docker.NewClient(socket) }

// buildActivation validates the activation flags and assembles the durable
// stores, Traefik observer, reporter, recovery hook and a complete
// executor.ActivationConfig. Only reached when executorEnabled is true.
func buildActivation(c command, client *agentclient.Client, stateStore state.Store, logger *log.Logger) (activationParts, error) {
	if err := validateActivationFlags(c); err != nil {
		return activationParts{}, err
	}
	activations, err := stateStore.Activations()
	if err != nil {
		return activationParts{}, fmt.Errorf("open pending activation state: %w", err)
	}
	revisions, err := stateStore.RouteRevisions()
	if err != nil {
		return activationParts{}, fmt.Errorf("open route revision state: %w", err)
	}
	observer, err := traefik.NewObserver(c.traefikAPIURL)
	if err != nil {
		return activationParts{}, fmt.Errorf("configure traefik observer: %w", err)
	}
	drains, err := stateStore.Drains()
	if err != nil {
		return activationParts{}, fmt.Errorf("open pending drain state: %w", err)
	}
	dockerClient, err := newDrainDocker(c.dockerSocket)
	if err != nil {
		return activationParts{}, fmt.Errorf("configure docker engine client: %w", err)
	}
	reporter := &agentclient.ActivationReporter{Client: client, Store: activations, Drains: drains, Logger: logger}
	if err := reporter.Validate(); err != nil {
		return activationParts{}, err
	}
	drainer := &executor.Drainer{
		Store: drains, Docker: dockerClient, RouteDir: c.traefikRouteDir,
		Logger: logger, StopGrace: c.drainStopGrace,
	}
	if err := drainer.Validate(); err != nil {
		return activationParts{}, fmt.Errorf("configure predecessor drainer: %w", err)
	}
	return activationParts{
		config: executor.ActivationConfig{
			Client:       client,
			Observer:     observer,
			Store:        activations,
			Revisions:    revisions,
			Drains:       drains,
			Kicker:       reporter,
			RouteDir:     c.traefikRouteDir,
			ProbeAddress: c.traefikHTTPSAddr,
			ProbePath:    c.activationProbePath,
			ProbeTimeout: c.activationProbeTimeout,
			Timeout:      c.activationTimeout,
		},
		reporter: reporter,
		drainer:  drainer,
		recover: func() {
			executor.RecoverActivations(activations, c.traefikRouteDir, logger)
		},
	}, nil
}

func runEnroll(c command, stdin io.Reader, stderr io.Writer) error {
	token, err := agentclient.ReadEnrollmentToken(c.tokenFile, stdin)
	if err != nil {
		return fmt.Errorf("read enrollment token: %w", err)
	}
	version, err := agentclient.Enroll(context.Background(), agentclient.EnrollInput{
		BaseURL: c.baseURL, ServerID: c.serverID, EnrollmentToken: token, StateDir: c.stateDir,
	}, nil)
	if err != nil {
		return fmt.Errorf("enroll: %w", err)
	}
	fmt.Fprintf(stderr, "enrolled: credential_version=%d\n", version)
	return nil
}

func runAgent(c command, stderr io.Writer) error {
	token, err := agentclient.LoadCredential(c.stateDir)
	if err != nil {
		return fmt.Errorf("load credential: %w", err)
	}
	logger := log.New(stderr, "", log.LstdFlags)
	client, err := agentclient.NewClient(c.baseURL, c.serverID, token, nil, logger)
	if err != nil {
		return fmt.Errorf("configure control-plane client: %w", err)
	}
	fenceStore, err := state.New(filepath.Join(c.stateDir, "fences"))
	if err != nil {
		return fmt.Errorf("open agent fence state: %w", err)
	}
	agent := &agentclient.Agent{
		Client: client, State: fenceStore, Executor: nil,
		Logger: logger, Version: agentVersion, StateDir: c.stateDir,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// executorEnabled stays false in this commit (see the package doc), so
	// buildExecutor is never called and agent.Executor stays nil: the
	// heartbeat loop keeps reporting executor_available=false and the
	// claim/lease path never runs. Flipping the constant is the only change
	// needed to start calling buildExecutor.
	if executorEnabled {
		parts, err := buildActivation(c, client, fenceStore, logger)
		if err != nil {
			return fmt.Errorf("configure activation: %w", err)
		}
		exec, err := buildExecutor(ctx, c, client, logger, parts.config)
		if err != nil {
			return fmt.Errorf("build agent executor: %w", err)
		}
		agent.Executor = exec
		agent.Reporter = parts.reporter
		agent.Recover = parts.recover
		agent.Drainer = parts.drainer
	}

	stopped := make(chan os.Signal, 1)
	signal.Notify(stopped, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(stopped)
	go func() {
		select {
		case <-stopped:
			cancel()
		case <-ctx.Done():
		}
	}()

	if err := agent.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// buildExecutor assembles Step 6b's real Executor from c's docker/checkout
// flags: an Engine API client over the docker socket, a production checkout
// runner over the privileged quota/spawn helper sockets, and
// executor.NewExecutor, which runs the fail-closed capability probe
// (Engine API version compatibility, then buildx) before returning. It is
// only ever called when executorEnabled is true.
func buildExecutor(ctx context.Context, c command, client *agentclient.Client, logger *log.Logger, activation executor.ActivationConfig) (agentclient.Executor, error) {
	if c.checkoutQuotaSocket == "" || c.checkoutSpawnSocket == "" || c.checkoutAgentGID == 0 || c.environmentNetwork == "" {
		return nil, errors.New("--checkout-quota-socket, --checkout-spawn-socket, --checkout-agent-gid, and --environment-network are required when the executor is enabled")
	}
	dockerClient, err := docker.NewClient(c.dockerSocket)
	if err != nil {
		return nil, fmt.Errorf("configure docker engine client: %w", err)
	}
	checkoutRunner, err := gitcheckout.NewProductionRunner(gitcheckout.ProductionConfig{
		QuotaSocketPath: c.checkoutQuotaSocket,
		SpawnSocketPath: c.checkoutSpawnSocket,
		AgentGID:        c.checkoutAgentGID,
		QuotaLimits:     checkoutquota.Limits{Bytes: defaultCheckoutQuotaBytes, Inodes: defaultCheckoutQuotaInodes},
		SpawnLimits:     checkoutcontain.SpawnLimits{TimeoutSeconds: defaultCheckoutSpawnTimeoutSec, MaxOutput: defaultCheckoutSpawnMaxOutput},
	})
	if err != nil {
		return nil, fmt.Errorf("configure checkout runner: %w", err)
	}
	// executor.ControlPlane's three embedded interfaces (the checkout RPCs,
	// MaterializeSecrets, ReportProgress) are all satisfied by
	// *agentclient.Client structurally; no adapter type is needed.
	return executor.NewExecutor(ctx, executor.Config{
		ControlPlane:   client,
		Docker:         dockerClient,
		CheckoutRunner: checkoutRunner,
		ServerID:       c.serverID,
		Network:        c.environmentNetwork,
		BuildOutput:    logWriter{logger},
		Logger:         logger,
		Activation:     &activation,
	})
}

// logWriter adapts a *log.Logger to io.Writer for redacted build output,
// one Print call per Write (log.Logger already serializes concurrent
// writers, so this needs no locking of its own).
type logWriter struct{ logger *log.Logger }

func (w logWriter) Write(p []byte) (int, error) {
	w.logger.Print(string(p))
	return len(p), nil
}
