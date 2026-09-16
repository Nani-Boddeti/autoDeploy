package githubhttp

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"autodeploy/internal/github"
)

func TestQAHandlerRejectsOversizedBodyBeforeProcessor(t *testing.T) {
	secret := bytes.Repeat([]byte("q"), 32)
	body := bytes.Repeat([]byte("x"), MaxBodyBytes+1)
	processed, rejected := 0, 0
	h := Handler{
		Secrets: SecretRing{ActiveVersion: "v1", Active: secret},
		Processor: processor{
			push: func(context.Context, github.Delivery, github.Push) error {
				processed++
				return nil
			},
			reject: func(context.Context, github.Delivery) error {
				rejected++
				return nil
			},
		},
	}
	r := httptest.NewRequest(http.MethodPost, "https://example.test"+Path, bytes.NewReader(body))
	r.TLS = &tls.ConnectionState{}
	r.Header.Set("X-GitHub-Delivery", "oversized-delivery")
	r.Header.Set("X-GitHub-Event", "push")
	r.Header.Set("X-Hub-Signature-256", signature(secret, body))
	w := httptest.NewRecorder()

	h.ServeHTTP(w, r)

	if w.Code != http.StatusRequestEntityTooLarge || processed != 0 || rejected != 0 {
		t.Fatalf("status=%d processed=%d rejected=%d", w.Code, processed, rejected)
	}
}

func TestQAHandlerRejectsBodyReadFailureBeforeProcessor(t *testing.T) {
	secret := bytes.Repeat([]byte("q"), 32)
	processed, rejected := 0, 0
	h := Handler{
		Secrets: SecretRing{ActiveVersion: "v1", Active: secret},
		Processor: processor{
			push: func(context.Context, github.Delivery, github.Push) error {
				processed++
				return nil
			},
			reject: func(context.Context, github.Delivery) error {
				rejected++
				return nil
			},
		},
	}
	r := httptest.NewRequest(http.MethodPost, "https://example.test"+Path, nil)
	r.TLS = &tls.ConnectionState{}
	r.Body = io.NopCloser(failingReader{})
	r.Header.Set("X-GitHub-Delivery", "short-read-delivery")
	r.Header.Set("X-GitHub-Event", "push")
	r.Header.Set("X-Hub-Signature-256", "sha256="+string(bytes.Repeat([]byte("0"), 64)))
	w := httptest.NewRecorder()

	h.ServeHTTP(w, r)

	if w.Code != http.StatusRequestEntityTooLarge || processed != 0 || rejected != 0 {
		t.Fatalf("status=%d processed=%d rejected=%d", w.Code, processed, rejected)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestQAParseLifecycleRejectsDuplicateAndOversizedRepositorySets(t *testing.T) {
	duplicate := []byte(`{"action":"removed","installation":{"id":1,"account":{"id":3}},"repositories_removed":[{"id":2,"owner":{"id":3}},{"id":2,"owner":{"id":3}}]}`)
	if _, err := parseLifecycle(github.EventInstallationRepositories, duplicate); err == nil {
		t.Fatal("duplicate repository ID accepted")
	}

	var body bytes.Buffer
	body.WriteString(`{"action":"removed","installation":{"id":1,"account":{"id":3}},"repositories_removed":[`)
	for index := 0; index <= github.MaxLifecycleRepositories; index++ {
		if index > 0 {
			body.WriteByte(',')
		}
		body.WriteString(`{"id":2,"owner":{"id":3}}`)
	}
	body.WriteString(`]}`)
	if _, err := parseLifecycle(github.EventInstallationRepositories, body.Bytes()); err == nil {
		t.Fatal("oversized repository set accepted")
	}
}

func TestQAHandlerClassifiesUnsupportedLifecycleAction(t *testing.T) {
	secret := bytes.Repeat([]byte("q"), 32)
	body := []byte(`{"action":"renamed","installation":{"id":1,"account":{"id":3}}}`)
	var rejected github.Delivery
	h := Handler{
		Secrets: SecretRing{ActiveVersion: "v1", Active: secret},
		Processor: processor{reject: func(_ context.Context, delivery github.Delivery) error {
			rejected = delivery
			return nil
		}},
	}
	r := httptest.NewRequest(http.MethodPost, "https://example.test"+Path, bytes.NewReader(body))
	r.TLS = &tls.ConnectionState{}
	r.Header.Set("X-GitHub-Delivery", "unsupported-lifecycle-action")
	r.Header.Set("X-GitHub-Event", "installation")
	r.Header.Set("X-Hub-Signature-256", signature(secret, body))
	w := httptest.NewRecorder()

	h.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", w.Code)
	}
	if rejected.Failure != github.FailureUnsupported {
		t.Fatalf("failure=%q, want %q", rejected.Failure, github.FailureUnsupported)
	}
}

func TestQAHandlerRejectsUnsupportedEventBeforeProcessor(t *testing.T) {
	secret := bytes.Repeat([]byte("q"), 32)
	body := []byte(`{"installation":{"id":1}}`)
	processed, rejected := 0, 0
	h := Handler{
		Secrets: SecretRing{ActiveVersion: "v1", Active: secret},
		Processor: processor{
			push: func(context.Context, github.Delivery, github.Push) error {
				processed++
				return nil
			},
			reject: func(context.Context, github.Delivery) error {
				rejected++
				return nil
			},
		},
	}
	r := httptest.NewRequest(http.MethodPost, "https://example.test"+Path, bytes.NewReader(body))
	r.TLS = &tls.ConnectionState{}
	r.Header.Set("X-GitHub-Delivery", "unsupported-event")
	r.Header.Set("X-GitHub-Event", "repository")
	r.Header.Set("X-Hub-Signature-256", signature(secret, body))
	w := httptest.NewRecorder()

	h.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest || processed != 0 || rejected != 0 {
		t.Fatalf("status=%d processed=%d rejected=%d", w.Code, processed, rejected)
	}
}
