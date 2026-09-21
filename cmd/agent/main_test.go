package main

import "testing"

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
