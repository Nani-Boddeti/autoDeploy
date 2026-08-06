package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"autodeploy/internal/config"
)

func TestDatabaseURLFromEnvironmentRequiresOnlyCredentialFile(t *testing.T) {
	root := tempDir(t)
	outside := tempDir(t)
	path := filepath.Join(outside, "db-url")
	if err := os.WriteFile(path, []byte("postgresql://user:password@localhost/app"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.RepositoryRootEnv, root)
	t.Setenv(config.DatabaseURLFileEnv, path)
	if got, err := config.DatabaseURLFromEnvironment(); err != nil || got == "" {
		t.Fatalf("got %q err %v", got, err)
	}
	t.Setenv("AUTODEPLOY_DATABASE_URL", "postgresql://legacy:secret@localhost/app")
	if _, err := config.DatabaseURLFromEnvironment(); !errors.Is(err, config.ErrInvalidEnvironment) {
		t.Fatalf("legacy env error %v", err)
	}
	t.Setenv("AUTODEPLOY_DATABASE_URL", "")
	t.Setenv(config.RepositoryRootEnv, "relative")
	if _, err := config.DatabaseURLFromEnvironment(); !errors.Is(err, config.ErrInvalidEnvironment) {
		t.Fatalf("root error %v", err)
	}
}

func TestUsernameThrottleKeyRingFromEnvironment(t *testing.T) {
	root, outside := tempDir(t), tempDir(t)
	active, retained := filepath.Join(outside, "active"), filepath.Join(outside, "retained")
	activeKey := make([]byte, 32)
	activeKey[0] = 2
	if err := os.WriteFile(active, activeKey, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(retained, append([]byte{1}, make([]byte, 31)...), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.RepositoryRootEnv, root)
	t.Setenv(config.UsernameThrottleActiveVersionEnv, "v2")
	t.Setenv(config.UsernameThrottleActiveKeyFileEnv, active)
	t.Setenv(config.UsernameThrottleRetainedKeyFilesEnv, "v1="+retained)
	ring, err := config.UsernameThrottleKeyRingFromEnvironment()
	if err != nil || len(ring.RetainedVersions) != 1 {
		t.Fatalf("ring %#v err %v", ring, err)
	}
	t.Setenv(config.UsernameThrottleRetainedKeyFilesEnv, "v2="+retained)
	if _, err := config.UsernameThrottleKeyRingFromEnvironment(); !errors.Is(err, config.ErrInvalidEnvironment) {
		t.Fatalf("duplicate err %v", err)
	}
	for _, setup := range []func(){
		func() { t.Setenv(config.UsernameThrottleActiveVersionEnv, "") },
		func() {
			t.Setenv(config.UsernameThrottleActiveVersionEnv, "v2")
			t.Setenv(config.UsernameThrottleActiveKeyFileEnv, "")
		},
		func() {
			t.Setenv(config.UsernameThrottleActiveKeyFileEnv, active)
			t.Setenv(config.UsernameThrottleRetainedKeyFilesEnv, "a="+retained+",b="+filepath.Join(outside, "b")+",c="+filepath.Join(outside, "c")+",d="+filepath.Join(outside, "d")+",e="+filepath.Join(outside, "e"))
		},
		func() { t.Setenv(config.UsernameThrottleRetainedKeyFilesEnv, "v1="+active) },
	} {
		setup()
		if _, err := config.UsernameThrottleKeyRingFromEnvironment(); !errors.Is(err, config.ErrInvalidEnvironment) {
			t.Fatalf("invalid ring error %v", err)
		}
	}
	t.Setenv(config.UsernameThrottleActiveVersionEnv, "v2")
	t.Setenv(config.UsernameThrottleActiveKeyFileEnv, active)
	t.Setenv(config.UsernameThrottleRetainedKeyFilesEnv, "")
	if err := os.WriteFile(active, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.UsernameThrottleKeyRingFromEnvironment(); !errors.Is(err, config.ErrInvalidEnvironment) {
		t.Fatalf("zero key err %v", err)
	}
}

func tempDir(t *testing.T) string {
	t.Helper()
	path, err := os.MkdirTemp("/private/tmp", "autodeploy-config-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(path) })
	return path
}
