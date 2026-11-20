package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"autodeploy/internal/config"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestApplyFailureShowsOnlyClassAndConstraint(t *testing.T) {
	err := &pgconn.PgError{Code: "23505", Message: "duplicate key value (secret@host)", Detail: "Key (x)=(hunter2)", ConstraintName: "users_name_key\nINJECT"}
	got := applyFailure(err)
	if got != "migrate: apply migrations failed class=postgres_23505 constraint=users_name_keyINJECT" {
		t.Fatalf("got %q", got)
	}
	for _, leak := range []string{"hunter2", "secret@host", "duplicate", "\n"} {
		if strings.Contains(got, leak) {
			t.Fatalf("leaked %q in %q", leak, got)
		}
	}
}

func TestRunRejectsBadArgsAndMissingConfig(t *testing.T) {
	t.Setenv(config.DatabaseURLFileEnv, "")
	var buf bytes.Buffer
	if code := run(context.Background(), []string{"-timeout=0"}, &buf); code != 2 {
		t.Fatalf("bad flag code = %d", code)
	}
	buf.Reset()
	if code := run(context.Background(), nil, &buf); code != 1 || !strings.Contains(buf.String(), "invalid configuration") {
		t.Fatalf("code=%d out=%q", code, buf.String())
	}
}

// A server that accepts the TCP connection but never answers must not hang
// migrate past its -timeout.
func TestRunHonoursTimeoutAgainstSilentServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() })
		}
	}()
	dir := safeTempDir(t)
	path := filepath.Join(dir, "db-url")
	url := "postgresql://u:p@" + ln.Addr().String() + "/app?sslmode=disable"
	if err := os.WriteFile(path, []byte(url), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.RepositoryRootEnv, safeTempDir(t))
	t.Setenv(config.DatabaseURLFileEnv, path)
	var buf bytes.Buffer
	start := time.Now()
	code := run(context.Background(), []string{"-timeout=300ms"}, &buf)
	if code != 1 || time.Since(start) > 10*time.Second || !strings.Contains(buf.String(), "class=deadline_exceeded") || strings.Contains(buf.String(), "u:p") {
		t.Fatalf("code=%d elapsed=%v out=%q", code, time.Since(start), buf.String())
	}
}

// safeTempDir returns a symlink-free, owner-only directory, which the
// credential loader requires.
func safeTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}
