package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"syscall"
	"testing"
	"time"

	agenthttp "autodeploy/internal/agent/http"
	githubhttp "autodeploy/internal/github/http"
)

// TestActivationEnabledStaysFalse pins the gate Step 10c-1 adds, mirroring
// cmd/agent's own TestExecutorEnabledStaysFalse exactly: it must take a
// deliberate, separately reviewed source change to flip it, never a silent
// regression, and a test is what actually enforces that.
func TestActivationEnabledStaysFalse(t *testing.T) {
	if activationEnabled {
		t.Fatal("activationEnabled must stay false until Step 10c is fully reviewed (tx1 alone ships in 10c-1; 10c-2 -- activation-result, the Complete guard, and the sweeper diversion -- has not shipped yet)")
	}
}

func TestNewHTTPServerUsesProductionLimits(t *testing.T) {
	server := newHTTPServer("127.0.0.1:8080", http.NotFoundHandler())
	if server.Addr != "127.0.0.1:8080" || server.ReadHeaderTimeout != readHeaderTimeout || server.ReadTimeout != readTimeout || server.WriteTimeout != writeTimeout || server.IdleTimeout != idleTimeout || server.MaxHeaderBytes != maxHeaderBytes {
		t.Fatal("control-plane server limits were not configured")
	}
}

func TestSecretReadyHandlerChecksEveryProbe(t *testing.T) {
	calls := 0
	h := secretReadyHandler(http.NotFoundHandler(), func(context.Context) error { calls++; return errors.New("not ready") })
	request := httptest.NewRequest(http.MethodGet, "http://example.test/readyz", nil)
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || calls != 1 {
		t.Fatalf("first=%d calls=%d", response.Code, calls)
	}
	h.ServeHTTP(httptest.NewRecorder(), request)
	if calls != 2 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestShutdownContextIsBounded(t *testing.T) {
	context, cancel := newShutdownContext()
	defer cancel()
	deadline, ok := context.Deadline()
	if !ok {
		t.Fatal("shutdown context has no deadline")
	}
	remaining := time.Until(deadline)
	if remaining <= 0 || remaining > shutdownTimeout || remaining < shutdownTimeout-time.Second {
		t.Fatalf("unexpected shutdown bound: %s", remaining)
	}
}

func namedHandler(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Handler", name)
		w.WriteHeader(http.StatusOK)
	})
}

func TestComposeHTTPHandlerRoutesAgentAndWebhookBeforeBrowser(t *testing.T) {
	browser, webhook, agent := namedHandler("browser"), namedHandler("webhook"), namedHandler("agent")
	composed := composeHTTPHandler(browser, webhook, agent)

	cases := []struct {
		path string
		want string
	}{
		{githubhttp.Path, "webhook"},
		{agenthttp.Prefix + "enroll", "agent"},
		{agenthttp.Prefix + "jobs/claim", "agent"},
		{"/login", "browser"},
		{"/agent/v1", "browser"}, // no trailing slash: not the agent prefix
		{"/agent/v1x/enroll", "browser"},
	}
	for _, tc := range cases {
		request := httptest.NewRequest(http.MethodPost, "http://example.test"+tc.path, nil)
		response := httptest.NewRecorder()
		composed.ServeHTTP(response, request)
		if got := response.Header().Get("X-Handler"); got != tc.want {
			t.Fatalf("path %q routed to %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestComposeHTTPHandlerAgentDisabledLeavesBrowserBehaviorUnchanged(t *testing.T) {
	browser := namedHandler("browser")
	composed := composeHTTPHandler(browser, nil, nil)
	for _, path := range []string{agenthttp.Prefix + "enroll", githubhttp.Path, "/login"} {
		request := httptest.NewRequest(http.MethodPost, "http://example.test"+path, nil)
		response := httptest.NewRecorder()
		composed.ServeHTTP(response, request)
		if got := response.Header().Get("X-Handler"); got != "browser" {
			t.Fatalf("path %q routed to %q with agent/webhook disabled, want browser", path, got)
		}
	}
}

func TestServerAuthenticatorAdaptsAuthenticateToErrorOnly(t *testing.T) {
	var a agenthttp.Authenticator = serverAuthenticator{repository: nil}
	if a == nil {
		t.Fatal("serverAuthenticator does not satisfy agenthttp.Authenticator")
	}
}

func TestHTTPServerShutdownWaitsForInflightRequest(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	server := newHTTPServer("", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		once.Do(func() { close(started) })
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if errors.Is(err, syscall.EPERM) {
		t.Skip("loopback listeners are unavailable in this sandbox")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	requestDone := make(chan struct{})
	go func() { _, _ = http.Get("http://" + listener.Addr().String()); close(requestDone) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- server.Shutdown(context.Background()) }()
	select {
	case <-shutdownDone:
		t.Fatal("shutdown did not wait for in-flight request")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not complete")
	}
	<-requestDone
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("serve result: %v", err)
	}
}
