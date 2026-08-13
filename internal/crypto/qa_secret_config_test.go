package crypto_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"autodeploy/internal/config"
)

func TestQAKEKManifestRejectsDuplicateCanonicalPaths(t *testing.T) {
	workspace := qaSecureTempDir(t)
	outside := qaSecureTempDir(t)
	keyPath := filepath.Join(outside, "key")
	key := make([]byte, 32)
	key[0] = 1
	if err := os.WriteFile(keyPath, key, 0600); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(outside, "manifest")
	manifest := []byte(`{"active_key_id":"active","database_generation_id":"0123456789abcdef","keys":[{"id":"active","state":"active","path":"` + keyPath + `"},{"id":"retiring","state":"retiring","path":"` + outside + `/./key"}]}`)
	if err := os.WriteFile(manifestPath, manifest, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.RepositoryRootEnv, workspace)
	t.Setenv(config.KEKManifestFileEnv, manifestPath)
	if _, err := config.KEKRingFromEnvironment(); !errors.Is(err, config.ErrInvalidEnvironment) {
		t.Fatalf("duplicate canonical path accepted: %v", err)
	}
}

func TestQAKEKRingRejectsObviouslyNonRandomZeroKey(t *testing.T) {
	workspace := qaSecureTempDir(t)
	outside := qaSecureTempDir(t)
	keyPath := filepath.Join(outside, "key")
	if err := os.WriteFile(keyPath, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(outside, "manifest")
	manifest := []byte(`{"active_key_id":"active","database_generation_id":"0123456789abcdef","keys":[{"id":"active","state":"active","path":"` + keyPath + `"}]}`)
	if err := os.WriteFile(manifestPath, manifest, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.RepositoryRootEnv, workspace)
	t.Setenv(config.KEKManifestFileEnv, manifestPath)
	if _, err := config.KEKRingFromEnvironment(); !errors.Is(err, config.ErrInvalidEnvironment) {
		t.Fatalf("all-zero KEK accepted: %v", err)
	}
}

func qaSecureTempDir(t *testing.T) string {
	t.Helper()
	path, err := os.MkdirTemp("/private/tmp", "autodeploy-secret-qa-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(path) })
	return path
}
