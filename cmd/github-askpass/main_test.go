package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func qaAskPassBinary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "github-askpass")
	cmd := exec.Command("go", "build", "-o", path, ".")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build askpass: %v: %s", err, output)
	}
	return path
}

func qaTokenPipe(t *testing.T, token string) *os.File {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(token + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func qaRunAskPass(path, prompt string, reader *os.File) ([]byte, error) {
	cmd := exec.Command(path, prompt)
	cmd.ExtraFiles = []*os.File{reader}
	cmd.Env = []string{"AUTODEPLOY_GIT_ASKPASS_FD=3"}
	return cmd.CombinedOutput()
}

func TestQAAskPassUsernameDoesNotConsumePassword(t *testing.T) {
	binary := qaAskPassBinary(t)
	reader := qaTokenPipe(t, "qa-askpass-secret-sentinel")
	username, err := qaRunAskPass(binary, "Username for GitHub", reader)
	if err != nil || string(username) != "x-access-token\n" {
		t.Fatalf("username output=%q err=%v", username, err)
	}
	password, err := qaRunAskPass(binary, "Password for GitHub", reader)
	if err != nil || string(password) != "qa-askpass-secret-sentinel\n" {
		t.Fatalf("password transfer failed: length=%d err=%v", len(password), err)
	}
}

func TestQAAskPassPasswordIsOneUse(t *testing.T) {
	binary := qaAskPassBinary(t)
	reader := qaTokenPipe(t, "qa-askpass-once-sentinel")
	if _, err := qaRunAskPass(binary, "Password", reader); err != nil {
		t.Fatal(err)
	}
	output, err := qaRunAskPass(binary, "Password", reader)
	if err == nil {
		t.Fatal("password pipe was reusable")
	}
	if strings.Contains(string(output), "qa-askpass-once-sentinel") || strings.Contains(err.Error(), "qa-askpass-once-sentinel") {
		t.Fatal("failed reuse exposed credential")
	}
}

func TestQAAskPassRejectsMalformedInput(t *testing.T) {
	binary := qaAskPassBinary(t)
	for _, tc := range []struct {
		name, prompt, token string
	}{
		{"unknown prompt", "OTP", "safe"},
		{"newline", "Password", "bad\nvalue"},
		{"carriage return", "Password", "bad\rvalue"},
		{"oversized", "Password", strings.Repeat("x", 8193)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output, err := qaRunAskPass(binary, tc.prompt, qaTokenPipe(t, tc.token))
			if err == nil || len(output) != 0 {
				t.Fatalf("malformed input accepted: output length=%d err=%v", len(output), err)
			}
		})
	}
}
