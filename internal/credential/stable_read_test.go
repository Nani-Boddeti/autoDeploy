//go:build darwin || linux

package credential

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadKeyRejectsInPlaceMutationAndAcceptsAtomicReplacement(t *testing.T) {
	root := stableReadTempDir(t)
	outside := stableReadTempDir(t)
	path := filepath.Join(outside, "key")
	first := keyedBytes(1)
	if err := os.WriteFile(path, first, 0600); err != nil {
		t.Fatal(err)
	}
	afterCredentialPreReadForTest = func() {
		if err := os.WriteFile(path, keyedBytes(2)[:16], 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { afterCredentialPreReadForTest = nil })
	if _, err := LoadKey(path, root, os.Geteuid()); !errors.Is(err, ErrInvalidCredentialFile) {
		t.Fatalf("mutation err %v", err)
	}
	afterCredentialPreReadForTest = nil
	if err := os.WriteFile(path, keyedBytes(2), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKey(path, root, os.Geteuid()); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(outside, "replacement")
	if err := os.WriteFile(replacement, keyedBytes(3), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadKey(path, root, os.Geteuid()); err != nil || got[0] != 3 {
		t.Fatalf("atomic replacement got=%v err=%v", got, err)
	}
}

func keyedBytes(first byte) []byte { value := make([]byte, 32); value[0] = first; return value }
func stableReadTempDir(t *testing.T) string {
	path, err := os.MkdirTemp("/private/tmp", "autodeploy-stable-read-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(path) })
	return path
}
