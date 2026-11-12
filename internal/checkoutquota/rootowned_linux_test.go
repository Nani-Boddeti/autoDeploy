//go:build linux

package checkoutquota

import (
	"os"
	"testing"
)

// os.Lstat returns *syscall.Stat_t, not *unix.Stat_t; asserting the wrong type
// made every ownership check fail closed, so the service could never start.
func TestRootOwnedRecognisesRealFileInfo(t *testing.T) {
	info, err := os.Lstat("/")
	if err != nil {
		t.Fatal(err)
	}
	if !rootOwned(info) {
		t.Fatal("rootOwned(os.Lstat(\"/\")) = false; / is root:root on Linux")
	}
	if err := trustedPath("/"); err != nil {
		t.Fatalf("trustedPath(\"/\") = %v", err)
	}
}
