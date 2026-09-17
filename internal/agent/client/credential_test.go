package agentclient

import (
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"autodeploy/internal/server"
)

func TestWriteCredentialAtomicMode0600(t *testing.T) {
	dir := t.TempDir()
	token, err := server.NewCredentialToken(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeCredentialAtomic(dir, token); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(credentialPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("credential mode = %v, want 0600", info.Mode().Perm())
	}
	contents, err := os.ReadFile(credentialPath(dir))
	if err != nil || string(contents) != token.String() {
		t.Fatalf("credential contents mismatch: %v %q", err, contents)
	}
}

func TestWriteCredentialRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	first, err := server.NewCredentialToken(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeCredentialAtomic(dir, first); err != nil {
		t.Fatal(err)
	}
	second, err := server.NewCredentialToken(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeCredentialAtomic(dir, second); !errors.Is(err, ErrCredentialExists) {
		t.Fatalf("expected ErrCredentialExists, got %v", err)
	}
	contents, err := os.ReadFile(credentialPath(dir))
	if err != nil || string(contents) != first.String() {
		t.Fatalf("original credential must survive a refused overwrite: %v %q", err, contents)
	}
}

func TestWriteCredentialLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	token, err := server.NewCredentialToken(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeCredentialAtomic(dir, token); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != credentialFileName {
		t.Fatalf("expected exactly one entry named %q, got %v", credentialFileName, entries)
	}
}

func TestLoadCredentialRoundTrip(t *testing.T) {
	dir := t.TempDir()
	token, err := server.NewCredentialToken(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeCredentialAtomic(dir, token); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadCredential(dir)
	if err != nil || loaded != token {
		t.Fatalf("loaded=%v err=%v", loaded, err)
	}
}

func TestLoadCredentialRejectsBadMode(t *testing.T) {
	dir := t.TempDir()
	token, err := server.NewCredentialToken(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeCredentialAtomic(dir, token); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(credentialPath(dir), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCredential(dir); !errors.Is(err, ErrProtectedFileRejected) {
		t.Fatalf("expected ErrProtectedFileRejected for mode 0644, got %v", err)
	}
}

func TestLoadCredentialRejectsMissingFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadCredential(dir); !errors.Is(err, ErrProtectedFileRejected) {
		t.Fatalf("expected ErrProtectedFileRejected for a missing file, got %v", err)
	}
}

func TestLoadCredentialRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real-credential")
	token, err := server.NewCredentialToken(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte(token.String()), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, credentialPath(dir)); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCredential(dir); !errors.Is(err, ErrProtectedFileRejected) {
		t.Fatalf("expected ErrProtectedFileRejected for a symlinked credential, got %v", err)
	}
}
