package main

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"autodeploy/internal/agent/executor"
	"autodeploy/internal/agent/state"
)

func TestParseArgs(t *testing.T) {
	c, err := parseArgs([]string{"enroll", "--base-url", "https://cp.example", "--server-id", "server_1", "--state-dir", "/var/lib/agent", "--token-file", "/etc/agent/token"})
	if err != nil || c.action != "enroll" || c.baseURL != "https://cp.example" || c.serverID != "server_1" || c.stateDir != "/var/lib/agent" || c.tokenFile != "/etc/agent/token" {
		t.Fatalf("%+v %v", c, err)
	}

	c, err = parseArgs([]string{"run", "--base-url", "https://cp.example", "--server-id", "server_1", "--state-dir", "/var/lib/agent"})
	if err != nil || c.action != "run" || c.tokenFile != "" {
		t.Fatalf("%+v %v", c, err)
	}
}

// TestExecutorEnabledStaysFalse pins the gate Step 6b's report describes:
// it must take a deliberate, separately reviewed source change to flip it,
// never a silent regression. A test is the guard that actually enforces
// that -- a comment alone would not stop a future commit from flipping it
// by accident.
func TestExecutorEnabledStaysFalse(t *testing.T) {
	if executorEnabled {
		t.Fatal("executorEnabled must stay false until the privileged checkout smoke and a real Docker daemon have validated internal/agent/executor")
	}
}

func TestParseArgsAcceptsExecutorFlagsForRun(t *testing.T) {
	c, err := parseArgs([]string{
		"run", "--base-url", "https://cp.example", "--server-id", "server_1", "--state-dir", "/var/lib/agent",
		"--docker-socket", "/var/run/docker.sock",
		"--checkout-quota-socket", "/run/autodeploy/quota.sock",
		"--checkout-spawn-socket", "/run/autodeploy/spawn.sock",
		"--checkout-agent-gid", "999",
		"--environment-network", "autodeploy-env-1",
	})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if c.dockerSocket != "/var/run/docker.sock" || c.checkoutQuotaSocket != "/run/autodeploy/quota.sock" ||
		c.checkoutSpawnSocket != "/run/autodeploy/spawn.sock" || c.checkoutAgentGID != 999 || c.environmentNetwork != "autodeploy-env-1" {
		t.Fatalf("%+v", c)
	}
}

func TestParseArgsDefaultsDockerSocketForRun(t *testing.T) {
	c, err := parseArgs([]string{"run", "--base-url", "https://cp.example", "--server-id", "server_1", "--state-dir", "/var/lib/agent"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if c.dockerSocket != "/var/run/docker.sock" {
		t.Fatalf("dockerSocket = %q, want the default", c.dockerSocket)
	}
}

func TestParseArgsRejectsInvalidInput(t *testing.T) {
	cases := [][]string{
		{},
		{"bogus"},
		{"enroll"},
		{"enroll", "--base-url", "https://cp.example"},
		{"enroll", "--base-url", "https://cp.example", "--server-id", "server_1", "--state-dir", "relative/path"},
		{"run", "--base-url", "https://cp.example", "--server-id", "server_1", "--state-dir", "/var/lib/agent", "--token-file", "/etc/agent/token"},
		{"run", "--base-url", "https://cp.example", "--server-id", "server_1", "--state-dir", "/var/lib/agent", "--unknown-flag", "x"},
		{"enroll", "--base-url", "https://cp.example", "--server-id", "server_1", "--state-dir", "/var/lib/agent", "--docker-socket", "/var/run/docker.sock"},
		{"run", "--base-url", "https://cp.example", "--server-id", "server_1", "--state-dir", "/var/lib/agent", "--checkout-agent-gid", "not-a-number"},
		{"run", "--base-url", "https://cp.example", "--server-id"},
	}
	for _, args := range cases {
		if _, err := parseArgs(args); err == nil {
			t.Fatalf("expected an error for args=%v", args)
		}
	}
}

func runArgs(extra ...string) []string {
	return append([]string{"run", "--base-url", "https://cp.example", "--server-id", "server_1", "--state-dir", "/var/lib/agent"}, extra...)
}

func TestParseArgsActivationDefaults(t *testing.T) {
	c, err := parseArgs(runArgs())
	if err != nil {
		t.Fatal(err)
	}
	if c.traefikRouteDir != "" || c.traefikAPIURL != "http://127.0.0.1:8080" || c.traefikHTTPSAddr != "127.0.0.1:443" ||
		c.activationProbePath != "/" || c.activationProbeTimeout != 3*time.Minute || c.activationTimeout != 0 || c.drainStopGrace != 30*time.Second {
		t.Fatalf("%+v", c)
	}
}

func TestParseArgsActivationFlags(t *testing.T) {
	c, err := parseArgs(runArgs(
		"--traefik-route-dir", "/etc/traefik/dynamic", "--traefik-api-url", "http://localhost:9090",
		"--traefik-https-addr", "[::1]:8443", "--activation-probe-path", "/healthz",
		"--activation-probe-timeout", "90s", "--activation-timeout", "10m", "--drain-stop-grace", "45s"))
	if err != nil {
		t.Fatal(err)
	}
	if c.traefikRouteDir != "/etc/traefik/dynamic" || c.traefikAPIURL != "http://localhost:9090" || c.traefikHTTPSAddr != "[::1]:8443" ||
		c.activationProbePath != "/healthz" || c.activationProbeTimeout != 90*time.Second || c.activationTimeout != 10*time.Minute || c.drainStopGrace != 45*time.Second {
		t.Fatalf("%+v", c)
	}
}

func TestParseArgsRejectsBadActivationFlagSyntax(t *testing.T) {
	for _, args := range [][]string{
		runArgs("--activation-probe-timeout", "soon"),
		runArgs("--activation-timeout", "10"),
		runArgs("--drain-stop-grace", "30"),
		{"enroll", "--base-url", "https://cp.example", "--server-id", "s", "--state-dir", "/var/lib/agent", "--drain-stop-grace", "30s"},
		{"enroll", "--base-url", "https://cp.example", "--server-id", "s", "--state-dir", "/var/lib/agent", "--traefik-route-dir", "/x"},
	} {
		if _, err := parseArgs(args); err == nil {
			t.Fatalf("expected an error for args=%v", args)
		}
	}
}

func validActivationCommand(t *testing.T) command {
	t.Helper()
	c, err := parseArgs(runArgs("--traefik-route-dir", t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestValidateActivationFlagsAcceptsDefaults(t *testing.T) {
	if err := validateActivationFlags(validActivationCommand(t)); err != nil {
		t.Fatal(err)
	}
}

func TestValidateActivationFlagsRejectsInvalidValues(t *testing.T) {
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*command){
		"route dir missing":        func(c *command) { c.traefikRouteDir = "" },
		"route dir relative":       func(c *command) { c.traefikRouteDir = "routes" },
		"route dir absent":         func(c *command) { c.traefikRouteDir = filepath.Join(c.traefikRouteDir, "nope") },
		"route dir is a file":      func(c *command) { c.traefikRouteDir = file },
		"api url non-loopback":     func(c *command) { c.traefikAPIURL = "http://traefik.example:8080" },
		"api url bad scheme":       func(c *command) { c.traefikAPIURL = "ftp://127.0.0.1:8080" },
		"https addr non-loopback":  func(c *command) { c.traefikHTTPSAddr = "203.0.113.5:443" },
		"https addr hostname":      func(c *command) { c.traefikHTTPSAddr = "localhost:443" },
		"https addr no port":       func(c *command) { c.traefikHTTPSAddr = "127.0.0.1" },
		"probe path relative":      func(c *command) { c.activationProbePath = "healthz" },
		"probe path whitespace":    func(c *command) { c.activationProbePath = "/a b" },
		"probe timeout zero":       func(c *command) { c.activationProbeTimeout = 0 },
		"probe timeout negative":   func(c *command) { c.activationProbeTimeout = -time.Second },
		"activation timeout minus": func(c *command) { c.activationTimeout = -time.Second },
		"drain grace zero":         func(c *command) { c.drainStopGrace = 0 },
		"drain grace negative":     func(c *command) { c.drainStopGrace = -time.Second },
		"drain grace fractional":   func(c *command) { c.drainStopGrace = 1500 * time.Millisecond },
		"drain grace too long":     func(c *command) { c.drainStopGrace = 301 * time.Second },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := validActivationCommand(t)
			mutate(&c)
			if err := validateActivationFlags(c); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestBuildActivationRequiresRouteDir(t *testing.T) {
	c, err := parseArgs(runArgs())
	if err != nil {
		t.Fatal(err)
	}
	st, err := state.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := buildActivation(c, nil, st, log.New(io.Discard, "", 0)); err == nil || !strings.Contains(err.Error(), "--traefik-route-dir") {
		t.Fatalf("err = %v, want the missing route dir named", err)
	}
}

type stubDrainDocker struct{ executor.DrainDocker }

func TestBuildActivationAssemblesCompleteConfig(t *testing.T) {
	orig := newDrainDocker
	newDrainDocker = func(string) (executor.DrainDocker, error) { return stubDrainDocker{}, nil }
	defer func() { newDrainDocker = orig }()
	c := validActivationCommand(t)
	c.activationTimeout = 20 * time.Minute
	st, err := state.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	parts, err := buildActivation(c, nil, st, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	cfg := parts.config
	if cfg.Store == nil || cfg.Revisions == nil || cfg.Observer == nil || cfg.Kicker == nil || cfg.RouteDir != c.traefikRouteDir ||
		cfg.ProbeAddress != c.traefikHTTPSAddr || cfg.ProbePath != "/" || cfg.ProbeTimeout != 3*time.Minute || cfg.Timeout != 20*time.Minute ||
		cfg.Drains == nil || parts.reporter == nil || parts.reporter.Drains == nil || parts.recover == nil ||
		parts.drainer == nil || parts.drainer.StopGrace != 30*time.Second || parts.drainer.RouteDir != c.traefikRouteDir ||
		parts.drainer.Store == nil || parts.drainer.Docker == nil {
		t.Fatalf("incomplete wiring: %+v", parts)
	}
	parts.recover() // empty store: must be a harmless no-op
}

func TestValidateActivationFlagsAcceptsGraceBounds(t *testing.T) {
	for _, g := range []time.Duration{time.Second, 300 * time.Second} {
		c := validActivationCommand(t)
		c.drainStopGrace = g
		if err := validateActivationFlags(c); err != nil {
			t.Fatalf("grace %s: %v", g, err)
		}
	}
}

func TestBuildActivationRejectsMisconfiguredDrainer(t *testing.T) {
	orig := newDrainDocker
	newDrainDocker = func(string) (executor.DrainDocker, error) { return stubDrainDocker{}, nil }
	defer func() { newDrainDocker = orig }()
	c := validActivationCommand(t)
	// Bypasses flag validation to prove the drainer's own Validate gates startup.
	c.drainStopGrace = 30 * time.Second
	st, err := state.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	newDrainDocker = func(string) (executor.DrainDocker, error) { return nil, nil }
	if _, err := buildActivation(c, nil, st, log.New(io.Discard, "", 0)); err == nil || !strings.Contains(err.Error(), "drainer") {
		t.Fatalf("err = %v, want a drainer configuration error", err)
	}
}
