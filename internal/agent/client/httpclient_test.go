package agentclient

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"autodeploy/internal/server"
)

func newTestClient(t *testing.T, srv *httptest.Server, logger *log.Logger) *Client {
	t.Helper()
	token, err := server.NewCredentialToken(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewClient(srv.URL, "server_test", token, srv.Client(), logger)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNewClientRejectsHTTPBaseURL(t *testing.T) {
	if _, err := NewClient("http://example.invalid", "server_test", "", nil, nil); err == nil {
		t.Fatal("expected http:// base URL to be rejected")
	}
}

func TestNewClientRejectsInvalidServerID(t *testing.T) {
	if _, err := NewClient("https://example.invalid", "", "", nil, nil); err == nil {
		t.Fatal("expected empty server id to be rejected")
	}
}

func TestPostRetriesOn503(t *testing.T) {
	var calls int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "service_unavailable"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"server_status": "active"})
	}))
	defer srv.Close()

	c := newTestClient(t, srv, nil)
	var resp struct {
		ServerStatus string `json:"server_status"`
	}
	if err := c.post(context.Background(), "/agent/v1/heartbeat", struct{}{}, &resp, true); err != nil {
		t.Fatal(err)
	}
	if resp.ServerStatus != "active" {
		t.Fatalf("resp=%+v", resp)
	}
	if calls != 3 {
		t.Fatalf("expected 3 attempts, got %d", calls)
	}
}

func TestPostRetriesOnNetworkError(t *testing.T) {
	// An address nothing listens on: every attempt fails as a network
	// error, exercising the retry path all the way to maxRetries.
	token, err := server.NewCredentialToken(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewClient("https://127.0.0.1:1", "server_test", token, &http.Client{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = c.post(context.Background(), "/agent/v1/heartbeat", struct{}{}, nil, true)
	if !errors.Is(err, ErrTransportFailure) {
		t.Fatalf("expected ErrTransportFailure after exhausting retries, got %v", err)
	}
}

func TestPostStopsRetryingOnContextCancellation(t *testing.T) {
	c, err := NewClient("https://127.0.0.1:1", "server_test", "", &http.Client{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = c.post(ctx, "/agent/v1/heartbeat", struct{}{}, nil, true)
	if err == nil {
		t.Fatal("expected an error for a cancelled context")
	}
}

func TestPostDoesNotRetryOn401(t *testing.T) {
	var calls int32
	var buf bytes.Buffer
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
	}))
	defer srv.Close()

	c := newTestClient(t, srv, log.New(&buf, "", 0))
	err := c.post(context.Background(), "/agent/v1/heartbeat", struct{}{}, nil, true)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected exactly one attempt on 401, got %d", calls)
	}
	if buf.String() != "agent credential rejected\n" {
		t.Fatalf("expected the fixed rejection log line, got %q", buf.String())
	}
}

func TestPostDoesNotRetryOn400(t *testing.T) {
	var calls int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_request"})
	}))
	defer srv.Close()

	c := newTestClient(t, srv, nil)
	err := c.post(context.Background(), "/agent/v1/heartbeat", struct{}{}, nil, true)
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("expected ErrInvalidRequest, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected exactly one attempt on 400, got %d", calls)
	}
}

func TestPostRejectsOversizedResponse(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		big := make([]byte, maxResponseBytes+1024)
		for i := range big {
			big[i] = ' '
		}
		big[0], big[len(big)-1] = '"', '"'
		_, _ = w.Write(big)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, nil)
	var out string
	err := c.post(context.Background(), "/agent/v1/heartbeat", struct{}{}, &out, true)
	if !errors.Is(err, ErrTransportFailure) {
		t.Fatalf("expected ErrTransportFailure for an oversized response, got %v", err)
	}
}

func TestPostRejectsUnknownFieldsStrictDecode(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"server_status": "active", "unexpected_field": "x"})
	}))
	defer srv.Close()

	c := newTestClient(t, srv, nil)
	var resp struct {
		ServerStatus string `json:"server_status"`
	}
	err := c.post(context.Background(), "/agent/v1/heartbeat", struct{}{}, &resp, true)
	if !errors.Is(err, ErrTransportFailure) {
		t.Fatalf("expected strict decode to reject an unknown field, got %v", err)
	}
}

func TestClientNeverFollowsRedirects(t *testing.T) {
	var plaintextCalls int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&plaintextCalls, 1)
	}))
	defer plain.Close()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, nil)
	if err := c.post(context.Background(), "/agent/v1/heartbeat", struct{}{}, nil, true); err == nil {
		t.Fatal("redirect response accepted")
	}
	if n := atomic.LoadInt32(&plaintextCalls); n != 0 {
		t.Fatalf("redirect followed to plaintext %d times", n)
	}
}

func TestClaimIsNotRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "service_unavailable"})
	}))
	defer srv.Close()

	c := newTestClient(t, srv, nil)
	if _, err := c.Claim(context.Background(), true); !errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("claim error = %v, want service unavailable", err)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("claim attempts = %d, want 1", n)
	}
}
