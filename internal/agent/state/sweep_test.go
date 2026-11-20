package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewSweepsOnlyStaleAtomicTemps(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, ".fence-123456.tmp")
	keep := filepath.Join(dir, "env-1.json")
	other := filepath.Join(dir, ".fence-notes.txt")
	target := filepath.Join(t.TempDir(), "victim")
	link := filepath.Join(dir, ".fence-link.tmp")
	subdir := filepath.Join(dir, ".fence-dir.tmp")
	for _, p := range []string{stale, keep, other, target} {
		if err := os.WriteFile(p, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(subdir, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := New(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale temp survived: %v", err)
	}
	for _, p := range []string{keep, other, target, subdir} {
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("%s must survive: %v", p, err)
		}
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("symlink must survive: %v", err)
	}
}
