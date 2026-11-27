package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	secretcrypto "autodeploy/internal/crypto"
	"autodeploy/internal/store/postgres"

	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
)

func TestDecideGitHubSource(t *testing.T) {
	cases := []struct {
		env, db, kek bool
		source       string
		ignored      bool
		fail         bool
	}{
		{false, false, false, "none", false, false},
		{false, false, true, "none", false, false},
		{true, false, false, "env", false, false},
		{true, false, true, "env", false, false},
		{true, true, false, "env", true, false},
		{true, true, true, "env", true, false},
		{false, true, false, "", false, true},
		{false, true, true, "database", false, false},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("env=%v db=%v kek=%v", c.env, c.db, c.kek), func(t *testing.T) {
			source, ignored, err := decideGitHubSource(c.env, c.db, c.kek)
			if (err != nil) != c.fail || source != c.source || ignored != c.ignored {
				t.Fatalf("got (%q,%v,%v)", source, ignored, err)
			}
			if c.fail && err.Error() != "GitHub setup unavailable" {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestGitHubInstallationURLEscapesSlug(t *testing.T) {
	if got := githubInstallationURL("my-app"); got != "https://github.com/apps/my-app/installations/new" {
		t.Fatal(got)
	}
	if got := githubInstallationURL("a/b?c"); got != "https://github.com/apps/a%2Fb%3Fc/installations/new" {
		t.Fatal(got)
	}
}

func TestExitCodeFor(t *testing.T) {
	if exitCodeFor(nil) != 0 || exitCodeFor(errors.New("boom")) != 1 {
		t.Fatal("plain mapping")
	}
	if exitCodeFor(errRestartRequested) != 75 || exitCodeFor(errors.Join(errors.New("shutdown"), errRestartRequested)) != 75 {
		t.Fatal("restart must map to 75, also when joined with a shutdown error")
	}
	if restartExitCode != 75 {
		t.Fatal("systemd contract: 75")
	}
}

func TestRestartHookFiresOnceAndLogsOnce(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	logger := slog.New(slog.NewTextHandler(&lockedWriter{&buf, &mu}, nil))
	hook := newRestartHook(500*time.Millisecond, logger)
	select {
	case <-hook.Requested():
		t.Fatal("fired before request")
	default:
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); hook.Request() }()
	}
	wg.Wait()
	select {
	case <-hook.Requested():
		t.Fatal("fired before the flush delay")
	default:
	}
	select {
	case <-hook.Requested():
	case <-time.After(2 * time.Second):
		t.Fatal("never fired")
	}
	hook.Request() // must not panic on a closed channel
	mu.Lock()
	defer mu.Unlock()
	if n := strings.Count(buf.String(), "github app created; restarting to load it"); n != 1 {
		t.Fatalf("log lines = %d: %s", n, buf.String())
	}
}

type lockedWriter struct {
	b  *bytes.Buffer
	mu *sync.Mutex
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

type fakeManifestStore struct {
	exists    bool
	existsErr error
	reg       postgres.ManifestRegistration
	pem, sec  []byte
	found     bool
	loadErr   error
	loads     int
}

func (f *fakeManifestStore) ManifestAppExists(context.Context) (bool, error) {
	return f.exists, f.existsErr
}

func (f *fakeManifestStore) LoadRegistration(context.Context, secretcrypto.KeyProvider) (postgres.ManifestRegistration, []byte, []byte, bool, error) {
	f.loads++
	return f.reg, f.pem, f.sec, f.found, f.loadErr
}

func kekOK() (secretcrypto.KeyProvider, error)   { return secretcrypto.KeyRing{}, nil }
func kekDown() (secretcrypto.KeyProvider, error) { return nil, errors.New("no kek") }

func testPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

func TestResolveGitHubSource(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("relation does not exist")
	cases := []struct {
		name    string
		store   *fakeManifestStore
		env     bool
		kek     func() (secretcrypto.KeyProvider, error)
		source  string
		ignored bool
		fail    bool
	}{
		{"env and exists-check error: env path unaffected", &fakeManifestStore{existsErr: boom}, true, kekDown, "env", false, false},
		{"env and db app: ignored note", &fakeManifestStore{exists: true}, true, kekDown, "env", true, false},
		{"database and exists-check error: fatal", &fakeManifestStore{existsErr: boom}, false, kekOK, "", false, true},
		{"none: nothing stored", &fakeManifestStore{}, false, kekDown, "none", false, false},
		{"database app with KEK", &fakeManifestStore{exists: true}, false, kekOK, "database", false, false},
		{"database app without KEK: fatal", &fakeManifestStore{exists: true}, false, kekDown, "", false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var logged bytes.Buffer
			source, ignored, err := resolveGitHubSource(ctx, c.store, c.env, c.kek, slog.New(slog.NewTextHandler(&logged, nil)))
			if (err != nil) != c.fail || source != c.source || ignored != c.ignored {
				t.Fatalf("got (%q,%v,%v)", source, ignored, err)
			}
			if c.fail && !errors.Is(err, errGitHubSetupUnavailable) {
				t.Fatalf("err = %v", err)
			}
			if c.env && c.store.existsErr != nil && strings.Contains(logged.String(), "relation") {
				t.Fatal("warning leaked the database error text")
			}
		})
	}
}

func TestLoadDatabaseGitHubApp(t *testing.T) {
	ctx := context.Background()
	reg := postgres.ManifestRegistration{AppID: 11, Slug: "my app", ClientID: "Iv1.c", GitHubOwnerID: 7, GitHubOwnerType: "User"}
	t.Run("found builds the wiring and clears plaintext", func(t *testing.T) {
		pemBytes, secret := testPEM(t), bytes.Repeat([]byte{9}, 32)
		store := &fakeManifestStore{reg: reg, pem: pemBytes, sec: secret, found: true}
		w, err := loadDatabaseGitHubApp(ctx, store, kekOK)
		if err != nil || w.signer == nil || w.identity.AppID != 11 || w.owner.AccountID != 7 || w.owner.Type != "User" ||
			w.installationURL != "https://github.com/apps/my%20app/installations/new" ||
			w.webhookRing.ActiveVersion != "1" || len(w.webhookRing.Active) != 32 || w.webhookRing.Active[0] != 9 || len(w.webhookRing.Retained) != 0 {
			t.Fatalf("wiring=%+v err=%v", w, err)
		}
		if !allZero(pemBytes) || !allZero(secret) {
			t.Fatal("plaintext buffers not cleared after build")
		}
	})
	t.Run("decrypt error", func(t *testing.T) {
		if _, err := loadDatabaseGitHubApp(ctx, &fakeManifestStore{loadErr: errors.New("aead")}, kekOK); !errors.Is(err, errGitHubSetupUnavailable) {
			t.Fatal(err)
		}
	})
	t.Run("not found", func(t *testing.T) {
		// Valid-looking material with found=false must still fail (not masked by parse errors).
		if _, err := loadDatabaseGitHubApp(ctx, &fakeManifestStore{reg: reg, pem: testPEM(t), sec: bytes.Repeat([]byte{9}, 32)}, kekOK); !errors.Is(err, errGitHubSetupUnavailable) {
			t.Fatal(err)
		}
	})
	t.Run("kek unavailable never reaches the store", func(t *testing.T) {
		store := &fakeManifestStore{found: true}
		if _, err := loadDatabaseGitHubApp(ctx, store, kekDown); !errors.Is(err, errGitHubSetupUnavailable) || store.loads != 0 {
			t.Fatalf("err=%v loads=%d", err, store.loads)
		}
	})
	t.Run("bad PEM clears both buffers and fails", func(t *testing.T) {
		pemBytes, secret := []byte("not a pem"), bytes.Repeat([]byte{9}, 32)
		if _, err := loadDatabaseGitHubApp(ctx, &fakeManifestStore{reg: reg, pem: pemBytes, sec: secret, found: true}, kekOK); !errors.Is(err, errGitHubSetupUnavailable) {
			t.Fatal(err)
		}
		if !allZero(pemBytes) || !allZero(secret) {
			t.Fatal("plaintext buffers not cleared on failure")
		}
	})
	t.Run("short secret fails and clears", func(t *testing.T) {
		pemBytes, secret := testPEM(t), bytes.Repeat([]byte{9}, 8)
		if _, err := loadDatabaseGitHubApp(ctx, &fakeManifestStore{reg: reg, pem: pemBytes, sec: secret, found: true}, kekOK); !errors.Is(err, errGitHubSetupUnavailable) {
			t.Fatal(err)
		}
		if !allZero(pemBytes) || !allZero(secret) {
			t.Fatal("plaintext buffers not cleared on failure")
		}
	})
}

func allZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}
