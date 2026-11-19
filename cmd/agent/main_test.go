package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"autodeploy/internal/agent/executor"
	"autodeploy/internal/agent/state"
	"autodeploy/internal/server"
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
		"build timeout zero":       func(c *command) { c.buildTimeout = 0 },
		"build timeout negative":   func(c *command) { c.buildTimeout = -time.Minute },
		"build timeout too short":  func(c *command) { c.buildTimeout = time.Minute - time.Nanosecond },
		"build timeout too long":   func(c *command) { c.buildTimeout = 2*time.Hour + time.Nanosecond },
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

func TestBuildTimeoutFlag(t *testing.T) {
	c, err := parseArgs(runArgs())
	if err != nil {
		t.Fatal(err)
	}
	if c.buildTimeout != 30*time.Minute {
		t.Fatalf("default buildTimeout = %s, want 30m", c.buildTimeout)
	}
	c, err = parseArgs(runArgs("--build-timeout", "45m"))
	if err != nil || c.buildTimeout != 45*time.Minute {
		t.Fatalf("parsed = %s, %v", c.buildTimeout, err)
	}
	if _, err := parseArgs(runArgs("--build-timeout", "soon")); err == nil {
		t.Fatal("expected an error for an unparsable duration")
	}
	if _, err := parseArgs([]string{"enroll", "--base-url", "https://cp.example", "--server-id", "s", "--state-dir", "/var/lib/agent", "--build-timeout", "5m"}); err == nil {
		t.Fatal("--build-timeout must be rejected for enroll")
	}
	for name, d := range map[string]time.Duration{"min": time.Minute, "max": 2 * time.Hour} {
		t.Run(name, func(t *testing.T) {
			c := validActivationCommand(t)
			c.buildTimeout = d
			if err := validateActivationFlags(c); err != nil {
				t.Fatalf("%s must be accepted: %v", d, err)
			}
		})
	}
	c = validActivationCommand(t)
	c.buildTimeout = 3 * time.Hour
	if err := validateActivationFlags(c); err == nil || !strings.Contains(err.Error(), "--build-timeout") {
		t.Fatalf("err = %v, want one naming --build-timeout", err)
	}
}

func TestValidateActivationFlagsTimeoutBudget(t *testing.T) {
	probe := validActivationCommand(t).activationProbeTimeout
	// The verify budget is fixed at its 30s default, so the minimum is
	// probe + 30s verify + 30s margin.
	minimum := probe + 60*time.Second
	for name, tc := range map[string]struct {
		timeout time.Duration
		wantErr bool
	}{
		"zero means derived": {0, false},
		"too small":          {time.Second, true},
		"one below minimum":  {minimum - time.Nanosecond, true},
		"exactly minimum":    {minimum, false},
	} {
		t.Run(name, func(t *testing.T) {
			c := validActivationCommand(t)
			c.activationTimeout = tc.timeout
			if err := validateActivationFlags(c); (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestValidateActivationFlagsRejectsReadOnlyRouteDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	c := validActivationCommand(t)
	c.traefikRouteDir = dir
	if err := validateActivationFlags(c); err == nil || !strings.Contains(err.Error(), "--traefik-route-dir") {
		t.Fatalf("read-only route dir accepted or unnamed: %v", err)
	}
}

func TestValidateActivationFlagsLeavesNoProbeFile(t *testing.T) {
	c := validActivationCommand(t)
	if err := validateActivationFlags(c); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(c.traefikRouteDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("route dir not clean: %v %v", entries, err)
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
	if _, err := buildActivation(c, nil, st, slog.New(slog.DiscardHandler)); err == nil || !strings.Contains(err.Error(), "--traefik-route-dir") {
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
	parts, err := buildActivation(c, nil, st, slog.New(slog.DiscardHandler))
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
	if _, err := buildActivation(c, nil, st, slog.New(slog.DiscardHandler)); err == nil || !strings.Contains(err.Error(), "drainer") {
		t.Fatalf("err = %v, want a drainer configuration error", err)
	}
}

const testTraefikPassword = "Sup3r-S3cret-pw"

func writeCredFile(t *testing.T, contents string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "traefik-creds")
	if err := os.WriteFile(p, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTraefikCredentialsFlagParsedForRunOnly(t *testing.T) {
	c, err := parseArgs(runArgs("--traefik-api-credentials-file", "/x/creds"))
	if err != nil || c.traefikAPICredentialsFile != "/x/creds" {
		t.Fatalf("run: %v %q", err, c.traefikAPICredentialsFile)
	}
	if _, err := parseArgs([]string{"enroll", "--base-url", "https://x", "--server-id", "s", "--state-dir", "/s", "--traefik-api-credentials-file", "/x"}); err == nil {
		t.Fatal("flag must be rejected outside run")
	}
}

func TestTraefikObserverWithoutCredentialsFileIsUnauthenticated(t *testing.T) {
	c := validActivationCommand(t)
	if c.traefikAPICredentialsFile != "" {
		t.Fatal("default must be empty")
	}
	if err := validateActivationFlags(c); err != nil {
		t.Fatal(err)
	}
	if _, err := newTraefikObserver(c); err != nil {
		t.Fatal(err)
	}
}

func TestTraefikCredentialsFileAccepted(t *testing.T) {
	for _, body := range []string{"agent:" + testTraefikPassword, "agent:" + testTraefikPassword + "\n", "agent:" + testTraefikPassword + "\r\n", "agent:pa:ss"} {
		c := validActivationCommand(t)
		c.traefikAPICredentialsFile = writeCredFile(t, body, 0o600)
		if err := validateActivationFlags(c); err != nil {
			t.Fatalf("%q: %v", body, err)
		}
		o, err := newTraefikObserver(c)
		if err != nil || o == nil {
			t.Fatalf("%q: %v", body, err)
		}
		if strings.Contains(fmt.Sprintf("%v %+v %#v", o, o, o), testTraefikPassword) {
			t.Fatal("observer formatting leaks password")
		}
	}
}

func TestTraefikCredentialsSentToObserverEndpoint(t *testing.T) {
	var gotUser, gotPass string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, _ = r.BasicAuth()
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := validActivationCommand(t)
	c.traefikAPIURL = srv.URL
	c.traefikAPICredentialsFile = writeCredFile(t, "agent:"+testTraefikPassword+"\n", 0o600)
	o, err := newTraefikObserver(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.ObserveRouter(context.Background(), "r"); err != nil {
		t.Fatal(err)
	}
	if gotUser != "agent" || gotPass != testTraefikPassword {
		t.Fatalf("credential not applied: %q", gotUser)
	}
}

func TestTraefikCredentialsFileRejected(t *testing.T) {
	cases := map[string]string{
		"no colon":     "agent" + testTraefikPassword,
		"empty user":   ":" + testTraefikPassword,
		"empty pass":   "agent:",
		"two lines":    "agent:" + testTraefikPassword + "\nsecond:line\n",
		"nul":          "agent:" + testTraefikPassword + "\x00",
		"tab":          "agent:" + testTraefikPassword + "\t",
		"bare cr":      "agent:" + testTraefikPassword + "\rx",
		"empty file":   "",
		"only newline": "\n",
	}
	for name, body := range cases {
		c := validActivationCommand(t)
		c.traefikAPICredentialsFile = writeCredFile(t, body, 0o600)
		for label, err := range map[string]error{
			"validate": validateActivationFlags(c),
			"observer": func() error { _, err := newTraefikObserver(c); return err }(),
		} {
			if err == nil {
				t.Fatalf("%s/%s: expected rejection", name, label)
			}
			if strings.Contains(err.Error(), testTraefikPassword) {
				t.Fatalf("%s/%s: error leaks password: %v", name, label, err)
			}
		}
	}
}

func TestTraefikCredentialsFileRejectsUntrustedFile(t *testing.T) {
	body := "agent:" + testTraefikPassword + "\n"
	good := writeCredFile(t, body, 0o600)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	paths := map[string]string{
		"mode 0644": writeCredFile(t, body, 0o644),
		"mode 0400": writeCredFile(t, body, 0o400),
		"symlink":   link,
		"missing":   filepath.Join(t.TempDir(), "absent"),
		"directory": t.TempDir(),
		"relative":  "creds",
		"oversized": writeCredFile(t, "agent:"+strings.Repeat("x", 5000), 0o600),
	}
	for name, p := range paths {
		c := validActivationCommand(t)
		c.traefikAPICredentialsFile = p
		err := validateActivationFlags(c)
		if err == nil {
			t.Fatalf("%s: expected rejection", name)
		}
		if strings.Contains(err.Error(), testTraefikPassword) {
			t.Fatalf("%s: error leaks password: %v", name, err)
		}
	}
}

// The quota helper refuses requests above its configured maximum, so the unit passes the
// configured values; invalid ones must be rejected at startup, not at the first checkout.
func TestParseArgsCheckoutQuotaLimits(t *testing.T) {
	c, err := parseArgs(runArgs("--checkout-quota-bytes", "2147483648", "--checkout-quota-inodes", "400000"))
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if c.checkoutQuotaBytes != 2147483648 || c.checkoutQuotaInodes != 400000 {
		t.Fatalf("%+v", c)
	}
	for _, args := range [][]string{
		runArgs("--checkout-quota-bytes", "0"),
		runArgs("--checkout-quota-inodes", "-1"),
		runArgs("--checkout-quota-bytes", "abc"),
		{"enroll", "--base-url", "https://cp.example", "--server-id", "server_1", "--state-dir", "/var/lib/agent", "--checkout-quota-bytes", "1"},
	} {
		if _, err := parseArgs(args); err == nil {
			t.Fatalf("expected an error for args=%v", args)
		}
	}
}

func TestBuildResourceFlags(t *testing.T) {
	c, err := parseArgs(runArgs())
	if err != nil {
		t.Fatal(err)
	}
	if c.buildMemory != 2<<30 || c.buildCPUs != 2 {
		t.Fatalf("defaults = %d bytes, %d cpus; want 2GiB, 2", c.buildMemory, c.buildCPUs)
	}
	for in, want := range map[string]int64{"512MiB": 512 << 20, "4GiB": 4 << 30, "1073741824": 1 << 30, "32GiB": 32 << 30} {
		c, err := parseArgs(runArgs("--build-memory", in))
		if err != nil || c.buildMemory != want {
			t.Fatalf("--build-memory %s = %d, %v; want %d", in, c.buildMemory, err, want)
		}
	}
	for _, bad := range []string{"", "0", "-1GiB", "2 GiB", "2GB", "GiB", "lots", "99999999999999GiB"} {
		if _, err := parseArgs(runArgs("--build-memory", bad)); err == nil {
			t.Errorf("--build-memory %q must be rejected", bad)
		}
	}
	c, err = parseArgs(runArgs("--build-cpus", "4"))
	if err != nil || c.buildCPUs != 4 {
		t.Fatalf("--build-cpus 4 = %d, %v", c.buildCPUs, err)
	}
	for _, bad := range []string{"", "0", "-1", "1.5", "many"} {
		if _, err := parseArgs(runArgs("--build-cpus", bad)); err == nil {
			t.Errorf("--build-cpus %q must be rejected", bad)
		}
	}
	for _, f := range [][]string{{"--build-memory", "2GiB"}, {"--build-cpus", "2"}} {
		args := append([]string{"enroll", "--base-url", "https://cp.example", "--server-id", "s", "--state-dir", "/var/lib/agent"}, f...)
		if _, err := parseArgs(args); err == nil {
			t.Errorf("%s must be rejected for enroll", f[0])
		}
	}
}

func TestValidateActivationFlagsBuildResourceBounds(t *testing.T) {
	for name, mutate := range map[string]func(*command){
		"memory below min": func(c *command) { c.buildMemory = 512<<20 - 1 },
		"memory above max": func(c *command) { c.buildMemory = 32<<30 + 1 },
		"cpus zero":        func(c *command) { c.buildCPUs = 0 },
		"cpus above max":   func(c *command) { c.buildCPUs = 33 },
	} {
		c := validActivationCommand(t)
		mutate(&c)
		if err := validateActivationFlags(c); err == nil || !strings.Contains(err.Error(), "--build-") {
			t.Errorf("%s: err = %v, want one naming the flag", name, err)
		}
	}
	for name, mutate := range map[string]func(*command){
		"memory min": func(c *command) { c.buildMemory = 512 << 20 },
		"memory max": func(c *command) { c.buildMemory = 32 << 30 },
		"cpus min":   func(c *command) { c.buildCPUs = 1 },
		"cpus max":   func(c *command) { c.buildCPUs = 32 },
	} {
		c := validActivationCommand(t)
		mutate(&c)
		if err := validateActivationFlags(c); err != nil {
			t.Errorf("%s must be accepted: %v", name, err)
		}
	}
}

func TestBuilderConfigFromCommand(t *testing.T) {
	c := validActivationCommand(t)
	got := builderConfig(c)
	if got.ConfigDir != filepath.Join(c.stateDir, "buildx") || got.MemoryBytes != c.buildMemory || got.CPUs != c.buildCPUs {
		t.Fatalf("builderConfig = %+v", got)
	}
}

func TestBuildCacheKeepFlag(t *testing.T) {
	c, err := parseArgs(runArgs())
	if err != nil || c.buildCacheKeep != 5<<30 {
		t.Fatalf("default = %d, %v; want 5GiB", c.buildCacheKeep, err)
	}
	for in, want := range map[string]int64{"1GiB": 1 << 30, "20GiB": 20 << 30, "1073741824": 1 << 30} {
		c, err := parseArgs(runArgs("--build-cache-keep", in))
		if err != nil || c.buildCacheKeep != want {
			t.Fatalf("--build-cache-keep %s = %d, %v; want %d", in, c.buildCacheKeep, err, want)
		}
	}
	for _, bad := range []string{"", "0", "-1GiB", "5GB", "lots"} {
		if _, err := parseArgs(runArgs("--build-cache-keep", bad)); err == nil {
			t.Errorf("--build-cache-keep %q must be rejected", bad)
		}
	}
	enroll := []string{"enroll", "--base-url", "https://cp.example", "--server-id", "s", "--state-dir", "/var/lib/agent", "--build-cache-keep", "5GiB"}
	if _, err := parseArgs(enroll); err == nil {
		t.Error("--build-cache-keep must be rejected for enroll")
	}
}

func TestValidateActivationFlagsBuildCacheKeepBounds(t *testing.T) {
	for name, v := range map[string]int64{"below min": 1<<30 - 1, "above max": 500<<30 + 1} {
		c := validActivationCommand(t)
		c.buildCacheKeep = v
		if err := validateActivationFlags(c); err == nil || !strings.Contains(err.Error(), "--build-cache-keep") {
			t.Errorf("%s: err = %v, want one naming the flag", name, err)
		}
	}
	for name, v := range map[string]int64{"min": 1 << 30, "max": 500 << 30} {
		c := validActivationCommand(t)
		c.buildCacheKeep = v
		if err := validateActivationFlags(c); err != nil {
			t.Errorf("%s must be accepted: %v", name, err)
		}
	}
}

// The agent journal logger redacts the credential token (raw and base64) and
// emits each record as one complete line in a single Write.
func TestNewAgentLoggerRedactsCredentialToken(t *testing.T) {
	token, err := server.NewCredentialToken(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var out countingWriter
	logger, err := newAgentLogger(&out, token)
	if err != nil {
		t.Fatal(err)
	}
	b64 := base64.StdEncoding.EncodeToString([]byte(token))
	logger.Error("probe failed", "detail", "raw="+string(token)+" b64="+b64)
	got := out.buf.String()
	if strings.Contains(got, string(token)) || strings.Contains(got, b64) {
		t.Fatalf("credential leaked into the journal: %q", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("no redaction marker: %q", got)
	}
	if out.writes != 1 || !strings.HasSuffix(got, "\n") {
		t.Fatalf("record must arrive complete in one Write: writes=%d got=%q", out.writes, got)
	}
}

// buildOutput records pass through the same redacting logger.
func TestLogWriterRedactsBuildOutputToken(t *testing.T) {
	token, err := server.NewCredentialToken(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var out countingWriter
	logger, err := newAgentLogger(&out, token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (logWriter{logger}).Write([]byte("step 3: " + string(token) + "\n")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.buf.String(), string(token)) || !strings.Contains(out.buf.String(), "component=build") {
		t.Fatalf("build output not redacted or not tagged: %q", out.buf.String())
	}
}

type countingWriter struct {
	buf    bytes.Buffer
	writes int
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.writes++
	return c.buf.Write(p)
}
