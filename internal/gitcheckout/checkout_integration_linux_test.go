//go:build linux && checkout_integration

package gitcheckout

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"autodeploy/internal/checkoutcontain"
	"autodeploy/internal/checkoutquota"

	"golang.org/x/sys/unix"
)

// TestPrivilegedHelperIsolation is intentionally excluded from ordinary tests.
// When explicitly opted in, it proves that the installed root helpers accept a
// bounded allocation and an untrusted checkout UID can run only GitInit, then
// release its descriptor-backed workspace. It needs no GitHub credentials.
func TestPrivilegedHelperIsolation(t *testing.T) {
	c := integrationConfig(t)
	if uint32(os.Geteuid()) != c.agentUID || os.Geteuid() == 0 {
		t.Fatalf("run as the configured non-root agent UID %d (got %d)", c.agentUID, os.Geteuid())
	}
	if uint32(os.Getegid()) != c.agentGID {
		t.Fatalf("agent process primary GID must be agent GID %d (got %d)", c.agentGID, os.Getegid())
	}
	if c.agentUID == c.checkoutUID {
		t.Fatal("agent UID and checkout UID must be distinct")
	}
	preflightCgroup(t, c.cgroupRoot)

	r, err := NewProductionRunner(ProductionConfig{
		QuotaSocketPath: c.quotaSocket,
		SpawnSocketPath: c.spawnSocket,
		AgentGID:        c.agentGID,
		QuotaLimits:     checkoutquota.Limits{Bytes: c.quotaBytes, Inodes: c.quotaInodes},
		SpawnLimits:     checkoutcontain.SpawnLimits{TimeoutSeconds: c.timeoutSeconds, MaxOutput: c.maxOutput},
	})
	if err != nil {
		t.Fatalf("production runner configuration: %v", err)
	}
	quota, ok := r.config.Quota.(checkoutquota.Client)
	if !ok {
		t.Fatal("production runner did not construct quota client")
	}
	spawn, ok := r.config.Spawn.(checkoutcontain.SpawnClient)
	if !ok {
		t.Fatal("production runner did not construct spawn client")
	}

	before := checkoutLeaves(t, c.cgroupRoot)
	alloc, err := quota.Allocate(context.Background(), r.config.QuotaLimits)
	if err != nil {
		t.Fatalf("quota allocation (requires live root quota helper and XFS prjquota): %v", err)
	}
	var workspaceStat unix.Stat_t
	if err := unix.Fstat(alloc.Workspace, &workspaceStat); err != nil || workspaceStat.Uid != c.checkoutUID || workspaceStat.Gid != c.checkoutGID || workspaceStat.Mode&0o777 != 0o700 {
		t.Fatalf("active workspace ownership/mode = uid=%d gid=%d mode=%#o, err=%v", workspaceStat.Uid, workspaceStat.Gid, workspaceStat.Mode&0o777, err)
	}
	cleaned := false
	defer func() {
		if !cleaned {
			if err := quota.ReleaseAndCleanup(context.Background(), alloc); err != nil {
				t.Errorf("quota cleanup after test failure: %v", err)
			}
			_ = unix.Close(alloc.Workspace)
		}
	}()

	workspace, err := unix.Dup(alloc.Workspace)
	if err != nil {
		t.Fatalf("duplicate allocated workspace: %v", err)
	}
	stdoutR, stdoutW, stderrR, stderrW := integrationPipes(t)
	result, err := spawn.Run(context.Background(), checkoutcontain.SpawnRequest{
		Operation:     checkoutcontain.GitInit,
		WorkspaceFD:   workspace,
		StdoutFD:      stdoutW,
		StderrFD:      stderrW,
		StdoutReadFD:  stdoutR,
		StderrReadFD:  stderrR,
		AskpassReadFD: -1,
		Stdout:        &bytes.Buffer{},
		Stderr:        &bytes.Buffer{},
		Limits:        r.config.SpawnLimits,
	})
	if err != nil || result.Class != checkoutcontain.ExitSuccess || result.ExitCode != 0 {
		t.Fatalf("GitInit through root spawn helper = %#v, %v", result, err)
	}
	// The agent cannot traverse the checkout-owned 0700 workspace after GitInit.
	// A successful typed response therefore attests that checkout-exec accepted
	// the descriptor only after its UID:GID validation and ran its child with the
	// fixed credential, whose exactIdentity check also requires no extra groups.
	if err := quota.ReleaseAndCleanup(context.Background(), alloc); err != nil {
		t.Fatalf("quota release and cleanup: %v", err)
	}
	cleaned = true
	if err := unix.Close(alloc.Workspace); err != nil {
		t.Fatalf("close released workspace descriptor: %v", err)
	}
	after := checkoutLeaves(t, c.cgroupRoot)
	if !sameLeaves(before, after) {
		t.Fatalf("checkout cgroup cleanup leaked leaves: before=%v after=%v", before, after)
	}
}

type checkoutIntegrationConfig struct {
	quotaSocket, spawnSocket, cgroupRoot         string
	agentUID, agentGID, checkoutUID, checkoutGID uint32
	quotaBytes, quotaInodes                      uint64
	timeoutSeconds, maxOutput                    uint32
}

func integrationConfig(t *testing.T) checkoutIntegrationConfig {
	t.Helper()
	if os.Getenv("AUTODEPLOY_CHECKOUT_INTEGRATION") != "1" {
		t.Skip("set AUTODEPLOY_CHECKOUT_INTEGRATION=1 to run privileged checkout integration")
	}
	c := checkoutIntegrationConfig{
		quotaSocket:    envAbsolute(t, "AUTODEPLOY_CHECKOUT_QUOTA_SOCKET"),
		spawnSocket:    envAbsolute(t, "AUTODEPLOY_CHECKOUT_SPAWN_SOCKET"),
		cgroupRoot:     envAbsolute(t, "AUTODEPLOY_CHECKOUT_CGROUP_ROOT"),
		agentUID:       envUint32(t, "AUTODEPLOY_CHECKOUT_AGENT_UID"),
		agentGID:       envUint32(t, "AUTODEPLOY_CHECKOUT_AGENT_GID"),
		checkoutUID:    envUint32(t, "AUTODEPLOY_CHECKOUT_UID"),
		checkoutGID:    envUint32(t, "AUTODEPLOY_CHECKOUT_GID"),
		quotaBytes:     envUint64(t, "AUTODEPLOY_CHECKOUT_QUOTA_BYTES"),
		quotaInodes:    envUint64(t, "AUTODEPLOY_CHECKOUT_QUOTA_INODES"),
		timeoutSeconds: envUint32(t, "AUTODEPLOY_CHECKOUT_TIMEOUT_SECONDS"),
		maxOutput:      envUint32(t, "AUTODEPLOY_CHECKOUT_MAX_OUTPUT"),
	}
	if c.agentUID == 0 || c.agentGID == 0 || c.checkoutUID == 0 || c.checkoutGID == 0 || c.agentUID == c.checkoutUID || c.agentGID == c.checkoutGID || c.quotaBytes == 0 || c.quotaInodes == 0 || c.timeoutSeconds == 0 || c.maxOutput == 0 {
		t.Fatal("integration numeric configuration must be nonzero")
	}
	return c
}

func envAbsolute(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if !filepath.IsAbs(v) {
		t.Fatalf("%s must be an absolute path", key)
	}
	return v
}

func envUint32(t *testing.T, key string) uint32 {
	t.Helper()
	v := envUint64(t, key)
	if v > uint64(^uint32(0)) {
		t.Fatalf("%s exceeds uint32", key)
	}
	return uint32(v)
}

func envUint64(t *testing.T, key string) uint64 {
	t.Helper()
	v, err := strconv.ParseUint(os.Getenv(key), 10, 64)
	if err != nil {
		t.Fatalf("%s must be an unsigned integer: %v", key, err)
	}
	return v
}

func preflightCgroup(t *testing.T, root string) {
	t.Helper()
	var fs unix.Statfs_t
	if err := unix.Statfs(root, &fs); err != nil || fs.Type != unix.CGROUP2_SUPER_MAGIC {
		t.Fatalf("delegated cgroup v2 root required at %s: %v", root, err)
	}
	b, err := os.ReadFile(filepath.Join(root, "cgroup.subtree_control"))
	if err != nil {
		t.Fatalf("read delegated cgroup controllers: %v", err)
	}
	for _, controller := range []string{"cpu", "io", "memory", "pids"} {
		if !strings.Contains(" "+string(b)+" ", " "+controller+" ") {
			t.Fatalf("delegated cgroup controller %q missing from cgroup.subtree_control", controller)
		}
	}
}

func integrationPipes(t *testing.T) (int, int, int, int) {
	t.Helper()
	var out, errp [2]int
	if err := unix.Pipe2(out[:], unix.O_CLOEXEC); err != nil {
		t.Fatal(err)
	}
	if err := unix.Pipe2(errp[:], unix.O_CLOEXEC); err != nil {
		_ = unix.Close(out[0])
		_ = unix.Close(out[1])
		t.Fatal(err)
	}
	return out[0], out[1], errp[0], errp[1]
}

func checkoutLeaves(t *testing.T, root string) map[string]struct{} {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read cgroup root: %v", err)
	}
	leaves := make(map[string]struct{})
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), "checkout-") {
			leaves[entry.Name()] = struct{}{}
		}
	}
	return leaves
}

func sameLeaves(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}
