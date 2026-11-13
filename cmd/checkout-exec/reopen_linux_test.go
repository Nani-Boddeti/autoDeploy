//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func openDir(t *testing.T, p string) int {
	t.Helper()
	fd, err := unix.Open(p, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("open %s: %v", p, err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	return fd
}

func identityOf(t *testing.T, fd int) (uint64, uint64) {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		t.Fatal(err)
	}
	return uint64(st.Dev), uint64(st.Ino)
}

func mkWorkspaces(t *testing.T, names ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, n := range names {
		if err := os.Mkdir(filepath.Join(root, n), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func openFDCount(t *testing.T) int {
	t.Helper()
	e, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skip("no /proc")
	}
	return len(e)
}

func TestReopenWorkspaceMatchesSameDirectory(t *testing.T) {
	root := mkWorkspaces(t, ".checkout-0000000a")
	rootFD, recv := openDir(t, root), openDir(t, filepath.Join(root, ".checkout-0000000a"))
	fd, err := reopenWorkspace(rootFD, recv)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if fd == recv {
		t.Fatal("received FD returned instead of a new one")
	}
	d1, i1 := identityOf(t, recv)
	d2, i2 := identityOf(t, fd)
	if d1 != d2 || i1 != i2 {
		t.Fatal("reopened FD is a different directory")
	}
}

func TestReopenWorkspacePicksMatchingCandidate(t *testing.T) {
	root := mkWorkspaces(t, ".checkout-00000001", ".checkout-00000002", ".checkout-0000000f")
	rootFD, recv := openDir(t, root), openDir(t, filepath.Join(root, ".checkout-00000002"))
	// Repeated calls must work too (fresh directory offset each time).
	for range 2 {
		fd, err := reopenWorkspace(rootFD, recv)
		if err != nil {
			t.Fatal(err)
		}
		d1, i1 := identityOf(t, recv)
		d2, i2 := identityOf(t, fd)
		_ = unix.Close(fd)
		if d1 != d2 || i1 != i2 {
			t.Fatal("wrong candidate returned")
		}
	}
}

func TestReopenWorkspaceNoMatch(t *testing.T) {
	root := mkWorkspaces(t, ".checkout-00000001")
	other := t.TempDir()
	rootFD, recv := openDir(t, root), openDir(t, other)
	if fd, err := reopenWorkspace(rootFD, recv); err == nil {
		_ = unix.Close(fd)
		t.Fatal("unrelated directory accepted")
	}
}

func TestReopenWorkspaceRefusesSymlink(t *testing.T) {
	// Relative link inside the root: only NOFOLLOW (not BENEATH) can refuse it.
	root := mkWorkspaces(t, "realdir")
	if err := os.Symlink("realdir", filepath.Join(root, ".checkout-0000000b")); err != nil {
		t.Fatal(err)
	}
	rootFD, recv := openDir(t, root), openDir(t, filepath.Join(root, "realdir"))
	if fd, err := reopenWorkspace(rootFD, recv); err == nil {
		_ = unix.Close(fd)
		t.Fatal("symlinked workspace accepted")
	}
}

func TestReopenWorkspaceIgnoresNonMatchingNames(t *testing.T) {
	root := mkWorkspaces(t, "checkout-00000001", ".checkout-ZZZZZZZZ", ".checkout-0000001", ".checkout-000000001", ".checkout-0000000A", "other")
	for _, n := range []string{"checkout-00000001", ".checkout-ZZZZZZZZ", ".checkout-0000000A"} {
		rootFD, recv := openDir(t, root), openDir(t, filepath.Join(root, n))
		if fd, err := reopenWorkspace(rootFD, recv); err == nil {
			_ = unix.Close(fd)
			t.Fatalf("name %q accepted", n)
		}
	}
}

func TestReopenWorkspaceDoesNotLeakFDs(t *testing.T) {
	root := mkWorkspaces(t, ".checkout-00000001", ".checkout-00000002")
	other := t.TempDir()
	rootFD, good, bad := openDir(t, root), openDir(t, filepath.Join(root, ".checkout-00000001")), openDir(t, other)
	before := openFDCount(t)
	for range 20 {
		if fd, err := reopenWorkspace(rootFD, good); err != nil {
			t.Fatal(err)
		} else {
			_ = unix.Close(fd)
		}
		if fd, err := reopenWorkspace(rootFD, bad); err == nil {
			_ = unix.Close(fd)
			t.Fatal("unexpected match")
		}
	}
	if after := openFDCount(t); after != before {
		t.Fatalf("FD leak: %d -> %d", before, after)
	}
}

func TestValidRootDirectoryRejectsNonRootOwnerAndWritableMode(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root")
	}
	d := t.TempDir()
	_ = os.Chmod(d, 0o700)
	if validRootDirectory(openDir(t, d)) {
		t.Fatal("non-root-owned directory accepted")
	}
}
