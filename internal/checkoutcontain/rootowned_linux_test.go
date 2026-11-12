//go:build linux

package checkoutcontain

import (
	"os"
	"testing"
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
