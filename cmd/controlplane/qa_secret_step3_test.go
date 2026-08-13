package main

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

func TestQAStep3ControlplaneLoggerRedactsConfiguredValue(t *testing.T) {
	var output bytes.Buffer
	logger, closer := controlplaneLogger(&output, [][]byte{[]byte("qa-sensitive-value")})
	logger.Print("before qa-sensitive-value after")
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "qa-sensitive-value") || !strings.Contains(output.String(), "[REDACTED]") {
		t.Fatalf("redacted output = %q", output.String())
	}
}

func TestQAStep3HTTPServerDoesNotBypassControlplaneLogger(t *testing.T) {
	server := newHTTPServer("127.0.0.1:0", http.NotFoundHandler())
	if server.ErrorLog == nil {
		t.Fatal("HTTP server uses net/http's default process logger instead of the control-plane logging boundary")
	}
}
