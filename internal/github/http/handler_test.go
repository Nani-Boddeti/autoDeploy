package githubhttp

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"autodeploy/internal/github"
)

func signature(secret, body []byte) string {
	m := hmac.New(sha256.New, secret)
	_, _ = m.Write(body)
	return fmt.Sprintf("sha256=%x", m.Sum(nil))
}

func TestHandlerVerifiesBeforeParsingAndAcceptsTrustedHTTPS(t *testing.T) {
	secret := bytes.Repeat([]byte("x"), 32)
	body := []byte(`{"installation":{"id":1},"repository":{"id":2,"owner":{"id":3}},"ref":"refs/heads/main","after":"0123456789abcdef0123456789abcdef01234567"}`)
	called := 0
	h := Handler{Secrets: SecretRing{ActiveVersion: "v1", Active: secret}, TrustedProxyCIDRs: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}, Processor: processor{push: func(_ context.Context, _ github.Delivery, _ github.Push) error { called++; return nil }}}
	r := httptest.NewRequest(http.MethodPost, "http://example.test"+Path, bytes.NewReader(body))
	r.RemoteAddr = "192.0.2.10:443"
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-GitHub-Delivery", "delivery")
	r.Header.Set("X-GitHub-Event", "push")
	r.Header.Set("X-Hub-Signature-256", signature(secret, body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent || called != 1 {
		t.Fatalf("status=%d calls=%d", w.Code, called)
	}
}

func TestVerificationAndHeaderContracts(t *testing.T) {
	secret, body := bytes.Repeat([]byte("x"), 32), []byte("payload")
	ring := SecretRing{ActiveVersion: "active", Active: secret, Retained: map[string][]byte{"old": bytes.Repeat([]byte("y"), 32)}}
	if !verify(signature(secret, body), body, ring) {
		t.Fatal("active secret rejected")
	}
	if !verify(signature(ring.Retained["old"], body), body, ring) {
		t.Fatal("retained secret rejected")
	}
	if verify("sha1=bad", body, ring) {
		t.Fatal("bad signature accepted")
	}
	h := http.Header{"X-Github-Delivery": {"delivery"}, "X-Github-Event": {"push"}, "X-Hub-Signature-256": {signature(secret, body)}}
	if _, ok := canonicalHeaders(h); !ok {
		t.Fatal("canonical headers rejected")
	}
	h.Add("X-GitHub-Delivery", "delivery-2")
	if _, ok := canonicalHeaders(h); ok {
		t.Fatal("duplicate header accepted")
	}
}

func TestHandlerRejectsUntrustedForwardedHTTPS(t *testing.T) {
	h := Handler{Secrets: SecretRing{ActiveVersion: "v1", Active: bytes.Repeat([]byte("x"), 32)}}
	r := httptest.NewRequest(http.MethodPost, "http://example.test"+Path, bytes.NewReader(nil))
	r.RemoteAddr = "198.51.100.4:443"
	r.Header.Set("X-Forwarded-Proto", "https")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status %d", w.Code)
	}
}

func TestHandlerDoesNotParseBeforeSignatureVerification(t *testing.T) {
	secret := bytes.Repeat([]byte("x"), 32)
	processed, rejected := 0, 0
	h := Handler{Secrets: SecretRing{ActiveVersion: "v1", Active: secret}, Processor: processor{push: func(context.Context, github.Delivery, github.Push) error { processed++; return nil }, reject: func(context.Context, github.Delivery) error { rejected++; return nil }}}
	r := httptest.NewRequest(http.MethodPost, "https://example.test"+Path, bytes.NewBufferString(`not json`))
	r.TLS = &tls.ConnectionState{}
	r.Header.Set("X-GitHub-Delivery", "delivery")
	r.Header.Set("X-GitHub-Event", "push")
	r.Header.Set("X-Hub-Signature-256", "sha256="+strings.Repeat("0", 64))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized || processed != 0 || rejected != 0 {
		t.Fatalf("status=%d process=%d reject=%d", w.Code, processed, rejected)
	}
}

func TestHandlerParsesVerifiedLifecycleStrictly(t *testing.T) {
	secret := bytes.Repeat([]byte("x"), 32)
	body := []byte(`{"action":"removed","installation":{"id":1,"account":{"id":3}},"repositories_removed":[{"id":2,"owner":{"id":3}}]}`)
	called := 0
	h := Handler{Secrets: SecretRing{ActiveVersion: "v1", Active: secret}, Processor: processor{lifecycle: func(_ context.Context, _ github.Delivery, value github.Lifecycle) error {
		called++
		if value.Event != github.EventInstallationRepositories || value.Action != "removed" || value.InstallationID != 1 || value.RepositoryID != 2 || value.OwnerID != 3 {
			t.Fatalf("lifecycle %#v", value)
		}
		return nil
	}}}
	r := httptest.NewRequest(http.MethodPost, "https://example.test"+Path, bytes.NewReader(body))
	r.TLS = &tls.ConnectionState{}
	r.Header.Set("X-GitHub-Delivery", "lifecycle-delivery")
	r.Header.Set("X-GitHub-Event", "installation_repositories")
	r.Header.Set("X-Hub-Signature-256", signature(secret, body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent || called != 1 {
		t.Fatalf("status=%d calls=%d", w.Code, called)
	}
}

func TestParseLifecycleCanonicalizesRepositorySet(t *testing.T) {
	body := []byte(`{"action":"removed","installation":{"id":1,"account":{"id":3}},"repositories_removed":[{"id":9,"owner":{"id":3}},{"id":2,"owner":{"id":3}}]}`)
	value, err := parseLifecycle(github.EventInstallationRepositories, body)
	if err != nil || len(value.RepositoryIDs) != 2 || value.RepositoryIDs[0] != 2 || value.RepositoryIDs[1] != 9 || value.RepositoryID != 2 {
		t.Fatalf("lifecycle=%#v err=%v", value, err)
	}
	badOwner := []byte(`{"action":"removed","installation":{"id":1,"account":{"id":3}},"repositories_removed":[{"id":2,"owner":{"id":4}}]}`)
	if _, err := parseLifecycle(github.EventInstallationRepositories, badOwner); err == nil {
		t.Fatal("mismatched owner accepted")
	}
}

func TestHandlerMakesVerifiedPersistenceFailureRetryable(t *testing.T) {
	secret := bytes.Repeat([]byte("x"), 32)
	body := []byte(`{"installation":{"id":1},"repository":{"id":2,"owner":{"id":3}},"ref":"refs/heads/main","after":"0123456789abcdef0123456789abcdef01234567"}`)
	rejected := 0
	h := Handler{Secrets: SecretRing{ActiveVersion: "v1", Active: secret}, Processor: processor{push: func(context.Context, github.Delivery, github.Push) error { return errors.New("database unavailable") }, reject: func(context.Context, github.Delivery) error { rejected++; return nil }}}
	r := httptest.NewRequest(http.MethodPost, "https://example.test"+Path, bytes.NewReader(body))
	r.TLS = &tls.ConnectionState{}
	r.Header.Set("X-GitHub-Delivery", "retryable-delivery")
	r.Header.Set("X-GitHub-Event", "push")
	r.Header.Set("X-Hub-Signature-256", signature(secret, body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable || rejected != 0 {
		t.Fatalf("status=%d rejected=%d", w.Code, rejected)
	}
}

func TestHandlerMakesTerminalWriteFailureRetryable(t *testing.T) {
	secret := bytes.Repeat([]byte("x"), 32)
	body := []byte(`not json`)
	h := Handler{Secrets: SecretRing{ActiveVersion: "v1", Active: secret}, Processor: processor{reject: func(context.Context, github.Delivery) error { return errors.New("database unavailable") }}}
	r := httptest.NewRequest(http.MethodPost, "https://example.test"+Path, bytes.NewReader(body))
	r.TLS = &tls.ConnectionState{}
	r.Header.Set("X-GitHub-Delivery", "terminal-write-retry")
	r.Header.Set("X-GitHub-Event", "push")
	r.Header.Set("X-Hub-Signature-256", signature(secret, body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d", w.Code)
	}
}

type processor struct {
	push      func(context.Context, github.Delivery, github.Push) error
	lifecycle func(context.Context, github.Delivery, github.Lifecycle) error
	reject    func(context.Context, github.Delivery) error
}

func (p processor) ProcessPush(c context.Context, d github.Delivery, v github.Push) error {
	if p.push != nil {
		return p.push(c, d, v)
	}
	return nil
}
func (p processor) ProcessLifecycle(c context.Context, d github.Delivery, v github.Lifecycle) error {
	if p.lifecycle != nil {
		return p.lifecycle(c, d, v)
	}
	return nil
}
func (p processor) Reject(c context.Context, d github.Delivery) error {
	if p.reject != nil {
		return p.reject(c, d)
	}
	return nil
}
