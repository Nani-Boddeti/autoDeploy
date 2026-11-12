//go:build linux

package checkoutcontain

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// os.Lstat returns *syscall.Stat_t; asserting *unix.Stat_t always failed.
func TestRootOwnedSpawnRecognisesRealFileInfo(t *testing.T) {
	info, err := os.Lstat("/")
	if err != nil {
		t.Fatal(err)
	}
	if !rootOwnedSpawn(info) {
		t.Fatal("rootOwnedSpawn(os.Lstat(\"/\")) = false; / is root-owned on Linux")
	}
}

func TestPresenceOpenFlagsWriteOnlyKill(t *testing.T) {
	if presenceOpenFlags("cgroup.kill") != unix.O_WRONLY {
		t.Fatal("cgroup.kill must be probed O_WRONLY (it is mode 0200)")
	}
	for _, n := range []string{"cgroup.events", "cgroup.procs", "memory.max"} {
		if presenceOpenFlags(n) != unix.O_RDONLY {
			t.Fatalf("%s must be probed O_RDONLY", n)
		}
	}
}
