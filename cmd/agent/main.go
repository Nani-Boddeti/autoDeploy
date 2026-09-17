// Command agent is the AutoDeploy deployment-agent entry point (tasks/todo.md
// Step 6c). It never claims work in this slice: no Executor is registered
// (Decision 10, "no-op executor rejected"), so `agent run`'s heartbeat loop
// always reports executor_available=false and the claim/lease path in
// internal/agent/client never runs against real infrastructure -- it exists
// there only to be unit-tested against a fake executor ahead of a later,
// separately approved execution slice.
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
	"syscall"

	agentclient "autodeploy/internal/agent/client"
	"autodeploy/internal/agent/state"
)

// agentVersion is reported on every heartbeat. It has no runtime effect in
// this slice; it exists so operators can tell agent binaries apart in
// server_audit_events/heartbeat history once a later slice persists it.
const agentVersion = "0.1.0-dev"

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
		default:
			return command{}, fmt.Errorf("unknown flag %q", flag)
		}
	}
	if c.baseURL == "" || c.serverID == "" || c.stateDir == "" || !filepath.IsAbs(c.stateDir) {
		return command{}, errors.New("--base-url, --server-id, and an absolute --state-dir are required")
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
