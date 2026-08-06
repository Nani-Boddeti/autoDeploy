//go:build darwin || linux

package credential_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"autodeploy/internal/credential"
	"golang.org/x/sys/unix"
)

func TestLoadDatabaseURLRejectsUnsafePathsAndContents(t *testing.T) {
	root := secureTempDir(t)
	outside := secureTempDir(t)
	valid := filepath.Join(outside, "database-url")
	writeCredential(t, valid, "postgresql://user:password@localhost:5432/app\n", 0600)
	if got, err := credential.LoadDatabaseURL(valid, root, os.Geteuid()); err != nil || !strings.HasPrefix(got, "postgresql:") {
		t.Fatalf("valid load got %q err %v", got, err)
	}
	cases := []struct {
		name, path, content string
		mode                os.FileMode
		want                error
	}{
		{"relative", "relative", "", 0, credential.ErrInvalidCredentialFile},
		{"workspace", filepath.Join(root, "credential"), "postgresql://user:password@localhost/app", 0600, credential.ErrInvalidCredentialFile},
		{"empty", filepath.Join(outside, "empty"), "", 0600, credential.ErrInvalidDatabaseURL},
		{"newline", filepath.Join(outside, "newline"), "postgresql://user:password@localhost/app\nmore", 0600, credential.ErrInvalidDatabaseURL},
		{"malformed", filepath.Join(outside, "malformed"), "not-a-url", 0600, credential.ErrInvalidDatabaseURL},
		{"unsafe-mode", filepath.Join(outside, "unsafe-mode"), "postgresql://user:password@localhost/app", 0640, credential.ErrInvalidCredentialFile},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if test.path != "relative" {
				writeCredential(t, test.path, test.content, test.mode)
			}
			_, err := credential.LoadDatabaseURL(test.path, root, os.Geteuid())
			if !errors.Is(err, test.want) {
				t.Fatalf("error %v, want %v", err, test.want)
			}
		})
	}
}

func TestLoadDatabaseURLRejectsSymlinkWrongOwnerAndOversize(t *testing.T) {
	root, outside := secureTempDir(t), secureTempDir(t)
	target := filepath.Join(outside, "target")
	writeCredential(t, target, "postgresql://user:password@localhost/app", 0600)
	link := filepath.Join(outside, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	intermediate := filepath.Join(outside, "intermediate")
	if err := os.Symlink(outside, intermediate); err != nil {
		t.Fatal(err)
	}
	for _, args := range []struct {
		path string
		uid  int
	}{{link, os.Geteuid()}, {filepath.Join(intermediate, "target"), os.Geteuid()}, {target, os.Geteuid() + 1}} {
		if _, err := credential.LoadDatabaseURL(args.path, root, args.uid); !errors.Is(err, credential.ErrInvalidCredentialFile) {
			t.Fatalf("error %v", err)
		}
	}
	if _, err := credential.LoadDatabaseURL(outside, root, os.Geteuid()); !errors.Is(err, credential.ErrInvalidCredentialFile) {
		t.Fatalf("directory error %v", err)
	}
	fifo := filepath.Join(outside, "credential-fifo")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := credential.LoadDatabaseURL(fifo, root, os.Geteuid()); !errors.Is(err, credential.ErrInvalidCredentialFile) {
		t.Fatalf("fifo error %v", err)
	}
	over := filepath.Join(outside, "over")
	writeCredential(t, over, strings.Repeat("a", credential.MaxDatabaseURLBytes+1), 0600)
	if _, err := credential.LoadDatabaseURL(over, root, os.Geteuid()); !errors.Is(err, credential.ErrInvalidCredentialFile) {
		t.Fatalf("oversize error %v", err)
	}
}

func TestLoadDatabaseURLReopensReplacementAndDoesNotLeakSecret(t *testing.T) {
	root, outside := secureTempDir(t), secureTempDir(t)
	path := filepath.Join(outside, "credential")
	writeCredential(t, path, "postgresql://user:first@localhost/app", 0600)
	if _, err := credential.LoadDatabaseURL(path, root, os.Geteuid()); err != nil {
		t.Fatal(err)
	}
	secret := "postgresql://user:replacement-secret@localhost/app"
	writeCredential(t, path, secret, 0600)
	got, err := credential.LoadDatabaseURL(path, root, os.Geteuid())
	if err != nil || got != secret {
		t.Fatalf("replacement err %v", err)
	}
	_, err = credential.LoadDatabaseURL(filepath.Join(root, "secret"), root, os.Geteuid())
	if strings.Contains(err.Error(), secret) {
		t.Fatal("secret leaked")
	}
}

func TestLoadKeyRequiresExactRawBytes(t *testing.T) {
	root, outside := secureTempDir(t), secureTempDir(t)
	path := filepath.Join(outside, "key")
	key := make([]byte, 32)
	key[0] = 1
	if err := os.WriteFile(path, key, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := credential.LoadKey(path, root, os.Geteuid()); err != nil {
		t.Fatal(err)
	}
	for _, value := range [][]byte{make([]byte, 31), append(make([]byte, 32), '\n'), make([]byte, 33)} {
		if err := os.WriteFile(path, value, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := credential.LoadKey(path, root, os.Geteuid()); !errors.Is(err, credential.ErrInvalidCredentialFile) {
			t.Fatalf("size %d err %v", len(value), err)
		}
	}
}

func writeCredential(t *testing.T, path, contents string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func secureTempDir(t *testing.T) string {
	t.Helper()
	path, err := os.MkdirTemp("/private/tmp", "autodeploy-credential-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(path) })
	return path
}
