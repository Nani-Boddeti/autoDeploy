//go:build linux

package checkoutcontain

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestSpawnRequestAllowlist(t *testing.T) {
	base := SpawnRequest{WorkspaceFD: 3, StdoutFD: 4, StderrFD: 5, StdoutReadFD: 6, StderrReadFD: 7, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, Limits: SpawnLimits{TimeoutSeconds: 1, MaxOutput: 1}}
	checkout := base
	checkout.Operation, checkout.AskpassReadFD = GitCheckoutFetchHead, -1
	if !validSpawnRequest(checkout) {
		t.Fatal("valid checkout rejected")
	}
	fetch := base
	fetch.Operation, fetch.Arg, fetch.AskpassReadFD = GitFetchExact, "0123456789abcdef0123456789abcdef01234567", 8
	if !validSpawnRequest(fetch) {
		t.Fatal("valid fetch rejected")
	}
	bad := checkout
	bad.Operation = 99
	if validSpawnRequest(bad) {
		t.Fatal("arbitrary git argument accepted")
	}
}

func TestTypedValidationRejectsMalformedSHAAndURL(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	if !ValidSpawnOperation(GitFetchExact, sha, "", true) {
		t.Fatal("valid SHA denied")
	}
	for _, v := range []string{"0123456789ABCDEF0123456789abcdef01234567", sha[:39], sha + "0"} {
		if ValidSpawnOperation(GitFetchExact, v, "", true) {
			t.Fatalf("invalid SHA accepted: %q", v)
		}
	}
	if !ValidSpawnOperation(GitRemoteAddOrigin, "", "https://github.com/owner/repo.git", false) {
		t.Fatal("valid URL denied")
	}
	for _, v := range []string{"https://github.com/owner/repo", "https://github.com:443/owner/repo.git", "https://user@github.com/owner/repo.git", "https://github.com/owner/repo.git?x=1", "https://github.com/owner%2frepo.git"} {
		if ValidSpawnOperation(GitRemoteAddOrigin, "", v, false) {
			t.Fatalf("invalid URL accepted: %q", v)
		}
	}
}

func TestDrainOutputCancellationClosesWrapperOnce(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := drainOutput(ctx, int(r.Fd()), &bytes.Buffer{}, &outputBudget{remaining: 1}); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestResponseWaitTimeoutCoversRequestedExecution(t *testing.T) {
	if got := responseWaitTimeout(SpawnLimits{TimeoutSeconds: 300}); got != 320*time.Second {
		t.Fatalf("got %s", got)
	}
	if got := responseWaitTimeout(SpawnLimits{}); got != 20*time.Second+clientDeadline {
		t.Fatalf("invalid timeout got %s", got)
	}
}

func TestSpawnFrameIsFixedAndDoesNotContainDescriptors(t *testing.T) {
	r := SpawnRequest{Operation: GitFetchExact, Arg: "0123456789abcdef0123456789abcdef01234567", WorkspaceFD: 100, StdoutFD: 101, StderrFD: 102, StdoutReadFD: 103, StderrReadFD: 104, AskpassReadFD: 105, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, Limits: SpawnLimits{TimeoutSeconds: 4, MaxOutput: 99}}
	b := encodeSpawnRequest(r)
	if len(b) != spawnFrameSize || b[2] != 4 || b[12] != 0 || b[13] != 40 {
		t.Fatalf("bad frame: len=%d fds=%d arg=%d", len(b), b[2], b[13])
	}
	for _, v := range b[16+len(r.Arg):] {
		if v != 0 {
			t.Fatal("noncanonical tail")
		}
	}
}
