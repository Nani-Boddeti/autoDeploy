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

func TestParseArgsRejectsInvalidInput(t *testing.T) {
	cases := [][]string{
		{},
		{"bogus"},
		{"enroll"},
		{"enroll", "--base-url", "https://cp.example"},
		{"enroll", "--base-url", "https://cp.example", "--server-id", "server_1", "--state-dir", "relative/path"},
		{"run", "--base-url", "https://cp.example", "--server-id", "server_1", "--state-dir", "/var/lib/agent", "--token-file", "/etc/agent/token"},
		{"run", "--base-url", "https://cp.example", "--server-id", "server_1", "--state-dir", "/var/lib/agent", "--unknown-flag", "x"},
		{"run", "--base-url", "https://cp.example", "--server-id"},
	}
	for _, args := range cases {
		if _, err := parseArgs(args); err == nil {
			t.Fatalf("expected an error for args=%v", args)
		}
	}
}
