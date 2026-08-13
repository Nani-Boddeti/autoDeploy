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
)

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
