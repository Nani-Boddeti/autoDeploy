//go:build darwin || linux

package agentclient

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A FIFO at a protected path must be rejected promptly, not block open(2)
// waiting for a writer before the regular-file check can run.
func TestReadProtectedFileRejectsFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "creds")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := ReadProtectedFile(path)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrProtectedFileRejected) {
			t.Fatalf("err=%v, want ErrProtectedFileRejected", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReadProtectedFile blocked on a FIFO")
	}
}
