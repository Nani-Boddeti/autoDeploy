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

func TestWebConfigFromEnvironment(t *testing.T) {
	root, outside := tempDir(t), tempDir(t)
	active, retained := filepath.Join(outside, "csrf-active"), filepath.Join(outside, "csrf-retained")
	if err := os.WriteFile(active, append([]byte{1}, make([]byte, 31)...), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(retained, append([]byte{2}, make([]byte, 31)...), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.RepositoryRootEnv, root)
	t.Setenv(config.PublicOriginEnv, "HTTPS://Admin.Example.test:443/")
	t.Setenv(config.CSRFActiveVersionEnv, "v2")
	t.Setenv(config.CSRFActiveKeyFileEnv, active)
	t.Setenv(config.CSRFRetainedKeyFilesEnv, "v1="+retained)
	t.Setenv(config.TrustedProxyCIDRsEnv, "192.0.2.0/24,2001:db8::/32")
	got, err := config.WebConfigFromEnvironment()
	if err != nil || got.PublicOrigin != "https://admin.example.test" || got.ListenAddress != ":8080" || got.AuthThrottleMaxRows != 10000 || len(got.CSRFKeys.Keys) != 2 || len(got.TrustedProxyCIDRs) != 2 {
		t.Fatalf("web config %#v err %v", got, err)
	}
	t.Setenv(config.ListenAddressEnv, "127.0.0.1:8081")
	t.Setenv(config.AuthThrottleMaxRowsEnv, "8")
	got, err = config.WebConfigFromEnvironment()
	if err != nil || got.ListenAddress != "127.0.0.1:8081" || got.AuthThrottleMaxRows != 8 {
		t.Fatalf("configured web config %#v err %v", got, err)
	}
	for _, setup := range []func(){
		func() { t.Setenv(config.PublicOriginEnv, "http://admin.example.test") },
		func() {
			t.Setenv(config.PublicOriginEnv, "https://admin.example.test")
			t.Setenv(config.CSRFRetainedKeyFilesEnv, "v2="+retained)
		},
		func() {
			t.Setenv(config.CSRFRetainedKeyFilesEnv, "v1="+retained)
			t.Setenv(config.TrustedProxyCIDRsEnv, "192.0.2.1/24")
		},
		func() {
			t.Setenv(config.TrustedProxyCIDRsEnv, "192.0.2.0/24")
			t.Setenv(config.AuthThrottleMaxRowsEnv, "7")
		},
	} {
		setup()
		if _, err := config.WebConfigFromEnvironment(); !errors.Is(err, config.ErrInvalidEnvironment) {
			t.Fatalf("invalid web config error %v", err)
		}
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
