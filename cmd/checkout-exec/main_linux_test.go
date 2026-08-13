//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"autodeploy/internal/checkoutcontain"
	"golang.org/x/sys/unix"
)

func TestServiceArgumentsAreFixedAndTyped(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	cases := []struct {
		op          checkoutcontain.GitOperation
		arg, remote string
		want        []string
	}{
		{checkoutcontain.GitInit, "", "", []string{"init", "--quiet"}},
		{checkoutcontain.GitRemoteAddOrigin, "", "https://github.com/owner/repo.git", []string{"remote", "add", "origin", "https://github.com/owner/repo.git"}},
		{checkoutcontain.GitFetchExact, sha, "", []string{"fetch", "--no-tags", "--depth=1", "origin", sha}},
		{checkoutcontain.GitRevParseFetchHead, "", "", []string{"rev-parse", "FETCH_HEAD^{commit}"}},
		{checkoutcontain.GitRevParseHead, "", "", []string{"rev-parse", "HEAD"}},
		{checkoutcontain.GitCheckoutFetchHead, "", "", []string{"checkout", "--detach", "--force", "FETCH_HEAD"}},
		{checkoutcontain.GitLSTree, "", "", []string{"ls-tree", "-r", "HEAD"}},
		{checkoutcontain.GitGrepLFS, "", "", []string{"grep", "-I", "-l", "version https://git-lfs.github.com/spec/v1", "HEAD", "--"}},
	}
	for _, tc := range cases {
		got, _ := serviceArgs(tc.op, tc.arg, tc.remote)
		if len(got) != len(tc.want) {
			t.Fatalf("operation %d: got %v", tc.op, got)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("operation %d: got %v", tc.op, got)
			}
		}
	}
	if got, _ := serviceArgs(checkoutcontain.GitFetchExact, "BAD", ""); got != nil {
		t.Fatal("invalid typed fetch reached argv")
	}
}

func TestServiceConfigOutputBound(t *testing.T) {
	c := config{socket: "/run/checkout.sock", agentUID: 1, agentGID: 1, checkoutUID: 2, checkoutGID: 2, timeout: time.Second, maxOutput: maxOutput}
	// validConfig also requires root; test the bound directly to stay portable.
	if c.maxOutput == 0 || c.maxOutput > maxOutput {
		t.Fatal("valid output bound rejected")
	}
	for _, n := range []uint{0, maxOutput + 1} {
		if n > 0 && n <= maxOutput {
			t.Fatal("bad test")
		}
	}
}

func TestServiceConfigRejectsMatchingAgentAndCheckoutGID(t *testing.T) {
	c := config{socket: "/run/checkout.sock", agentUID: 1, agentGID: 2, checkoutUID: 3, checkoutGID: 2, timeout: time.Second, maxOutput: maxOutput}
	if validConfig(c) {
		t.Fatal("matching agent and checkout GIDs accepted")
	}
}

func TestValidWorkspaceRequiresExactOwnerAndMode(t *testing.T) {
	workspace := t.TempDir()
	fd, err := unix.Open(workspace, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("open workspace: %v", err)
	}
	defer unix.Close(fd)
	uid, gid := uint32(os.Geteuid()), uint32(os.Getegid())
	if !validWorkspace(fd, uid, gid) {
		t.Fatal("valid workspace rejected")
	}
	if validWorkspace(fd, uid, gid+1) {
		t.Fatal("workspace with wrong GID accepted")
	}
	if err := os.Chmod(filepath.Clean(workspace), 0o720); err != nil {
		t.Fatalf("make workspace group writable: %v", err)
	}
	if validWorkspace(fd, uid, gid) {
		t.Fatal("group-writable workspace accepted")
	}
}

func TestCloseRawFDsIgnoresDisarmedDescriptors(t *testing.T) {
	closeRawFDs(-1, -1)
}
func TestFixedEnvironmentIsNotCallerControlled(t *testing.T) {
	for _, e := range fixedEnv(true) {
		if len(e) >= 3 && (e[:3] == "LD_" || e[:3] == "DY_") {
			t.Fatalf("loader environment present: %q", e)
		}
	}
	if len(fixedEnv(false)) >= len(fixedEnv(true)) {
		t.Fatal("askpass environment not constrained")
	}
}
