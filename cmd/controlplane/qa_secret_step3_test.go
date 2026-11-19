package main

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestQAStep3ControlplaneLoggerRedactsConfiguredValue(t *testing.T) {
	var output bytes.Buffer
	logger := controlplaneLogger(&output, [][]byte{[]byte("qa-sensitive-value")})
	logger.Error("before qa-sensitive-value after", "dsn", "qa-sensitive-value")
	if strings.Contains(output.String(), "qa-sensitive-value") || !strings.Contains(output.String(), "[REDACTED]") || !strings.HasSuffix(output.String(), "\n") {
		t.Fatalf("redacted output = %q", output.String())
	}
}

func TestQAStep3HTTPServerDoesNotBypassControlplaneLogger(t *testing.T) {
	server := newHTTPServer("127.0.0.1:0", http.NotFoundHandler(), controlplaneLogger(io.Discard, nil))
	if server.ErrorLog == nil {
		t.Fatal("HTTP server uses net/http's default process logger instead of the control-plane logging boundary")
	}
}

func TestHTTPServerErrorLogUsesInjectedLogger(t *testing.T) {
	var out bytes.Buffer
	logger := controlplaneLogger(&out, [][]byte{[]byte("qa-sensitive-value")})
	server := newHTTPServer("127.0.0.1:0", http.NotFoundHandler(), logger)
	server.ErrorLog.Printf("tls failure qa-sensitive-value")
	if got := out.String(); !strings.HasPrefix(got, "level=ERROR msg=") || strings.Contains(got, "qa-sensitive-value") || !strings.HasSuffix(got, "\n") {
		t.Fatalf("ErrorLog output = %q", got)
	}
}
