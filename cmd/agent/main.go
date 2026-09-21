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

	agentclient "autodeploy/internal/agent/client"
	"autodeploy/internal/agent/executor"
	"autodeploy/internal/agent/state"
	"autodeploy/internal/checkoutcontain"
	"autodeploy/internal/checkoutquota"
	"autodeploy/internal/gitcheckout"
	"autodeploy/internal/runtime/docker"
)

// agentVersion is reported on every heartbeat. It has no runtime effect in
// this slice; it exists so operators can tell agent binaries apart in
// server_audit_events/heartbeat history once a later slice persists it.
const agentVersion = "0.1.0-dev"

// executorEnabled gates whether runAgent registers the real Executor Step
// 6b assembled. See the package doc above: this is the one line a later,
// separately reviewed commit flips to true. Everything else -- flag
// parsing, executor construction, the capability probe -- is fully wired
// and runs its own validation even while this stays false, so flipping it
// requires no other code change.
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
		default:
			return command{}, fmt.Errorf("unknown flag %q", flag)
		}
	}
	if c.baseURL == "" || c.serverID == "" || c.stateDir == "" || !filepath.IsAbs(c.stateDir) {
		return command{}, errors.New("--base-url, --server-id, and an absolute --state-dir are required")
	}
	if c.action == "run" && c.dockerSocket == "" {
		c.dockerSocket = "/var/run/docker.sock"
	}
	return c, nil
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
		exec, err := buildExecutor(ctx, c, client, logger)
		if err != nil {
			return fmt.Errorf("build agent executor: %w", err)
		}
		agent.Executor = exec
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
func buildExecutor(ctx context.Context, c command, client *agentclient.Client, logger *log.Logger) (agentclient.Executor, error) {
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
