package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

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

func TestGitHubAppConfigFromEnvironmentIsOptInAndStrict(t *testing.T) {
	if got, err := config.GitHubAppConfigFromEnvironment(); err != nil || got.Enabled {
		t.Fatalf("default %#v err %v", got, err)
	}
	t.Setenv(config.GitHubAppEnabledEnv, "true")
	t.Setenv(config.GitHubAppIDEnv, "17")
	t.Setenv(config.GitHubAppClientIDEnv, "Iv1.client")
	t.Setenv(config.GitHubAppSlugEnv, "auto-deploy")
	t.Setenv(config.GitHubAppOwnerIDEnv, "21")
	t.Setenv(config.GitHubAppOwnerTypeEnv, "Organization")
	t.Setenv(config.GitHubAppInstallationURLEnv, "https://github.com/apps/auto-deploy/installations/new")
	t.Setenv(config.GitHubAppPrivateKeyFileEnv, "/run/secrets/github-app.pem")
	got, err := config.GitHubAppConfigFromEnvironment()
	if err != nil || !got.Enabled || got.AppID != 17 || got.OwnerAccountID != 21 {
		t.Fatalf("config %#v err %v", got, err)
	}
	t.Setenv(config.GitHubAppInstallationURLEnv, "http://github.com/apps/auto-deploy")
	if _, err := config.GitHubAppConfigFromEnvironment(); !errors.Is(err, config.ErrInvalidEnvironment) {
		t.Fatalf("unsafe URL err %v", err)
	}
}

func TestAgentConfigFromEnvironmentIsOptInAndStrict(t *testing.T) {
	if got, err := config.AgentConfigFromEnvironment(); err != nil || got.Enabled || got.SweepInterval != 15*time.Second {
		t.Fatalf("default %#v err %v", got, err)
	}
	t.Setenv(config.AgentEnabledEnv, "false")
	if got, err := config.AgentConfigFromEnvironment(); err != nil || got.Enabled {
		t.Fatalf("explicit false %#v err %v", got, err)
	}
	t.Setenv(config.AgentEnabledEnv, "true")
	got, err := config.AgentConfigFromEnvironment()
	if err != nil || !got.Enabled || got.SweepInterval != 15*time.Second {
		t.Fatalf("enabled default interval %#v err %v", got, err)
	}
	t.Setenv(config.AgentSweepIntervalEnv, "30s")
	if got, err := config.AgentConfigFromEnvironment(); err != nil || got.SweepInterval != 30*time.Second {
		t.Fatalf("custom interval %#v err %v", got, err)
	}
	t.Setenv(config.AgentEnabledEnv, "yes")
	if _, err := config.AgentConfigFromEnvironment(); !errors.Is(err, config.ErrInvalidEnvironment) {
		t.Fatalf("invalid enabled value err %v", err)
	}
	t.Setenv(config.AgentEnabledEnv, "true")
	for _, interval := range []string{"not-a-duration", "1s", "4s", "5m1s", "1h"} {
		t.Setenv(config.AgentSweepIntervalEnv, interval)
		if _, err := config.AgentConfigFromEnvironment(); !errors.Is(err, config.ErrInvalidEnvironment) {
			t.Fatalf("interval %q err %v", interval, err)
		}
	}
	for _, interval := range []string{"5s", "5m"} {
		t.Setenv(config.AgentSweepIntervalEnv, interval)
		if _, err := config.AgentConfigFromEnvironment(); err != nil {
			t.Fatalf("boundary interval %q err %v", interval, err)
		}
	}
	t.Setenv(config.AgentEnabledEnv, "")
	t.Setenv(config.AgentSweepIntervalEnv, "bogus")
	if _, err := config.AgentConfigFromEnvironment(); !errors.Is(err, config.ErrInvalidEnvironment) {
		t.Fatalf("invalid interval while disabled err %v", err)
	}
}

func TestGitHubWebhookSecretRingValidatesActiveMetadata(t *testing.T) {
	root, outside := tempDir(t), tempDir(t)
	active := filepath.Join(outside, "webhook-active")
	if err := os.WriteFile(active, append([]byte{1}, make([]byte, 31)...), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.RepositoryRootEnv, root)
	t.Setenv(config.GitHubWebhookActiveVersionEnv, "v1")
	t.Setenv(config.GitHubWebhookActiveSecretFileEnv, active)
	if ring, err := config.GitHubWebhookSecretRingFromEnvironment(); err != nil || len(ring.Active) != 32 {
		t.Fatalf("ring err=%v", err)
	}
	for _, setup := range []func(){
		func() { t.Setenv(config.GitHubWebhookActiveVersionEnv, "") },
		func() {
			t.Setenv(config.GitHubWebhookActiveVersionEnv, "v1")
			t.Setenv(config.GitHubWebhookActiveSecretFileEnv, "")
		},
		func() { t.Setenv(config.GitHubWebhookActiveSecretFileEnv, "relative") },
	} {
		setup()
		if _, err := config.GitHubWebhookSecretRingFromEnvironment(); !errors.Is(err, config.ErrInvalidEnvironment) {
			t.Fatalf("invalid active metadata err=%v", err)
		}
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

func TestKEKRingFromEnvironment(t *testing.T) {
	root, outside := tempDir(t), tempDir(t)
	active, retiring := filepath.Join(outside, "kek-active"), filepath.Join(outside, "kek-retiring")
	if err := os.WriteFile(active, append([]byte{7}, make([]byte, 31)...), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(retiring, append([]byte{8}, make([]byte, 31)...), 0600); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(outside, "kek-manifest.json")
	contents := []byte(`{"active_key_id":"k2","database_generation_id":"0123456789abcdef","keys":[{"id":"k2","state":"active","path":"` + active + `"},{"id":"k1","state":"retiring","path":"` + retiring + `"}]}`)
	if err := os.WriteFile(manifest, contents, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.RepositoryRootEnv, root)
	t.Setenv(config.KEKManifestFileEnv, manifest)
	ring, err := config.KEKRingFromEnvironment()
	activeKey, activeErr := ring.Resolve("k2")
	retainedKey, retainedErr := ring.Resolve("k1")
	if err != nil || activeErr != nil || retainedErr != nil || len(activeKey) != 32 || len(retainedKey) != 32 {
		t.Fatalf("ring %#v err %v", ring, err)
	}
	if err := os.WriteFile(manifest, []byte(`{"active_key_id":"k2","database_generation_id":"0123456789abcdef","keys":[{"id":"k2","state":"active","path":"`+active+`"},{"id":"k2","state":"retiring","path":"`+retiring+`"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.KEKRingFromEnvironment(); !errors.Is(err, config.ErrInvalidEnvironment) {
		t.Fatalf("duplicate manifest %v", err)
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
