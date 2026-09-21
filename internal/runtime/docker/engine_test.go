package docker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// shortTempDir returns a short-named temp directory, independent of the
// test name. t.TempDir() nests under a path that includes the full test
// name, which routinely exceeds the ~104-byte Unix domain socket path
// limit once subtests are involved; a Unix socket fixture needs a short
// path regardless of how long the test name is.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "adkr")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func startFakeEngine(t *testing.T, handler http.Handler) string {
	t.Helper()
	sockPath := filepath.Join(shortTempDir(t), "d.sock")
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: handler}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return sockPath
}

func engineErrorClass(t *testing.T, err error) ErrorClass {
	t.Helper()
	var ee *EngineError
	if !errors.As(err, &ee) {
		t.Fatalf("want *EngineError, got %T: %v", err, err)
	}
	return ee.Class
}

func TestNewClientEmptySocketPath(t *testing.T) {
	_, err := newClient("")
	if err == nil {
		t.Fatal("want error for empty socket path")
	}
	if got := engineErrorClass(t, err); got != ErrClassConfiguration {
		t.Fatalf("class = %v, want configuration", got)
	}
}

func TestPingSuccess(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.44/_ping", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("OK"))
	})
	c, err := newClient(startFakeEngine(t, mux))
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestPingMalformedBody(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.44/_ping", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("nope"))
	})
	c, err := newClient(startFakeEngine(t, mux))
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	err = c.Ping(context.Background())
	if err == nil {
		t.Fatal("want error for malformed ping body")
	}
	if got := engineErrorClass(t, err); got != ErrClassMalformed {
		t.Fatalf("class = %v, want malformed", got)
	}
}

func TestVersionSuccess(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.44/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"Version":"25.0.0","ApiVersion":"1.44","MinAPIVersion":"1.24","Os":"linux","Arch":"amd64"}`))
	})
	c, err := newClient(startFakeEngine(t, mux))
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	v, err := c.Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if v.APIVersion != "1.44" || v.Os != "linux" {
		t.Fatalf("unexpected version info: %+v", v)
	}
}

func TestVersionMalformedJSON(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.44/version", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{not json`))
	})
	c, err := newClient(startFakeEngine(t, mux))
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	_, err = c.Version(context.Background())
	if got := engineErrorClass(t, err); got != ErrClassMalformed {
		t.Fatalf("class = %v, want malformed", got)
	}
}

func TestVersionMissingFieldsIsMalformed(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.44/version", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"Version":"25.0.0"}`))
	})
	c, err := newClient(startFakeEngine(t, mux))
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	_, err = c.Version(context.Background())
	if got := engineErrorClass(t, err); got != ErrClassMalformed {
		t.Fatalf("class = %v, want malformed", got)
	}
}

func Test4xxIsConfiguration(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.44/version", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("bad request"))
	})
	c, err := newClient(startFakeEngine(t, mux))
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	_, err = c.Version(context.Background())
	var ee *EngineError
	if !errors.As(err, &ee) {
		t.Fatalf("want *EngineError, got %v", err)
	}
	if ee.Class != ErrClassConfiguration || ee.Status != http.StatusBadRequest {
		t.Fatalf("got class=%v status=%d, want configuration/400", ee.Class, ee.Status)
	}
}

func Test5xxIsTransient(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.44/version", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("overloaded"))
	})
	c, err := newClient(startFakeEngine(t, mux))
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	_, err = c.Version(context.Background())
	var ee *EngineError
	if !errors.As(err, &ee) {
		t.Fatalf("want *EngineError, got %v", err)
	}
	if ee.Class != ErrClassTransient || ee.Status != http.StatusServiceUnavailable {
		t.Fatalf("got class=%v status=%d, want transient/503", ee.Class, ee.Status)
	}
}

// TestDialNeverCreatedSocketIsConfiguration covers a socket path that was
// never created (the ENOENT case: a typo'd path, or a daemon that was never
// installed). Retrying this unchanged can never succeed, so it must be
// classified configuration, not transient.
func TestDialNeverCreatedSocketIsConfiguration(t *testing.T) {
	sock := filepath.Join(shortTempDir(t), "never-created.sock")
	c, err := newClient(sock)
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	err = c.Ping(context.Background())
	if err == nil {
		t.Fatal("want error dialing a socket that was never created")
	}
	if got := engineErrorClass(t, err); got != ErrClassConfiguration {
		t.Fatalf("class = %v, want configuration", got)
	}
}

// TestDialRefusedIsTransient covers a socket path that exists but nothing
// is (or is no longer) listening on (the ECONNREFUSED case: a daemon that
// is restarting). This is exactly the kind of blip a retry can outlive, so
// it must be classified transient.
func TestDialRefusedIsTransient(t *testing.T) {
	sock := filepath.Join(shortTempDir(t), "refused.sock")
	addr, err := net.ResolveUnixAddr("unix", sock)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	l, err := net.ListenUnix("unix", addr)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	// Net.Listen for "unix" removes the socket file on Close by default;
	// disable that so the file stays in place with nothing accepting on
	// it, which is what produces ECONNREFUSED rather than ENOENT.
	l.SetUnlinkOnClose(false)
	l.Close()

	c, err := newClient(sock)
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	err = c.Ping(context.Background())
	if err == nil {
		t.Fatal("want error dialing a socket refusing connections")
	}
	if got := engineErrorClass(t, err); got != ErrClassTransient {
		t.Fatalf("class = %v, want transient", got)
	}
}

func TestContextDeadlineIsTransient(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.44/_ping", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Write([]byte("OK"))
	})
	c, err := newClient(startFakeEngine(t, mux))
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err = c.Ping(ctx)
	if err == nil {
		t.Fatal("want deadline error")
	}
	if got := engineErrorClass(t, err); got != ErrClassTransient {
		t.Fatalf("class = %v, want transient", got)
	}
}

func TestProbeSucceedsWithinSupportedRange(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.44/_ping", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("OK")) })
	mux.HandleFunc("/v1.44/version", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"Version":"27.0.0","ApiVersion":"1.47","MinAPIVersion":"1.24","Os":"linux","Arch":"amd64"}`))
	})
	c, err := newClient(startFakeEngine(t, mux))
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	if _, err := c.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
}

func TestProbeFailsClosedOnIncompatibleEngine(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.44/_ping", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("OK")) })
	mux.HandleFunc("/v1.44/version", func(w http.ResponseWriter, r *http.Request) {
		// A very old engine whose supported range tops out below v1.44.
		w.Write([]byte(`{"Version":"1.9.0","ApiVersion":"1.21","MinAPIVersion":"1.12","Os":"linux","Arch":"amd64"}`))
	})
	c, err := newClient(startFakeEngine(t, mux))
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	_, err = c.Probe(context.Background())
	if err == nil {
		t.Fatal("want Probe to fail closed on an incompatible engine")
	}
	if got := engineErrorClass(t, err); got != ErrClassConfiguration {
		t.Fatalf("class = %v, want configuration", got)
	}
}

// TestTrailingBodyDeadlineCoversTheWholeCall is a regression test: do()
// used to apply its default timeout with a deferred cancel, which fired the
// instant do() returned with headers — before any caller read the body. A
// response whose body arrives after a delay past the headers (exactly what
// the build-log and container-wait streams Steps 5/6 need look like) would
// then fail with a spurious "context canceled" even though the whole call
// comfortably finished within the timeout.
func TestTrailingBodyDeadlineCoversTheWholeCall(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.44/_ping", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(80 * time.Millisecond)
		fmt.Fprint(w, "OK")
	})
	c, err := newClient(startFakeEngine(t, mux))
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

// TestConcurrentCallsOnOneClient is a permanent regression test for the
// shared *Client under concurrent use: one client, 100 goroutines each
// issuing a Ping and a Version call, asserting zero failures.
func TestConcurrentCallsOnOneClient(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.44/_ping", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "OK")
	})
	mux.HandleFunc("/v1.44/version", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"Version":"25.0.0","ApiVersion":"1.44","MinAPIVersion":"1.24","Os":"linux","Arch":"amd64"}`)
	})
	c, err := newClient(startFakeEngine(t, mux))
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}

	const n = 100
	var wg sync.WaitGroup
	errs := make(chan error, 2*n)
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); errs <- c.Ping(context.Background()) }()
		go func() { defer wg.Done(); _, e := c.Version(context.Background()); errs <- e }()
	}
	wg.Wait()
	close(errs)

	failures := 0
	for e := range errs {
		if e != nil {
			failures++
			t.Logf("call failed: %v", e)
		}
	}
	if failures > 0 {
		t.Fatalf("%d/%d concurrent calls failed", failures, 2*n)
	}
}

// TestOversizedBodyIsBoundedAndClassified proves maxResponseBody actually
// bounds a read: the fake engine writes far more than the cap, and the
// client must return promptly (it never reads the whole stream) with a
// classified error rather than hanging or succeeding on garbage.
func TestOversizedBodyIsBoundedAndClassified(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.44/_ping", func(w http.ResponseWriter, r *http.Request) {
		chunk := strings.Repeat("x", 64*1024)
		for written := 0; written < 2*maxResponseBody; written += len(chunk) {
			if _, err := w.Write([]byte(chunk)); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	})
	c, err := newClient(startFakeEngine(t, mux))
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}

	start := time.Now()
	err = c.Ping(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("want error for an oversized, non-OK ping body")
	}
	if got := engineErrorClass(t, err); got != ErrClassMalformed {
		t.Fatalf("class = %v, want malformed", got)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("took %s to fail on an oversized body; maxResponseBody is not bounding the read", elapsed)
	}
}
