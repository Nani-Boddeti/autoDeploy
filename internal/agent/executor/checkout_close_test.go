package executor

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"autodeploy/internal/agent/logship"
	"autodeploy/internal/runtime/docker"
)

const leakyCheckoutErr = "fatal: unable to access 'https://x-access-token:ghs_SECRET123@github.com/o/r.git/': /var/lib/autodeploy/ws/9"

func TestCheckoutFailureShipsClassOnlyAndJournalsText(t *testing.T) {
	var journal bytes.Buffer
	var jmu sync.Mutex
	sender := &captureSender{}
	factory, err := logship.NewFactory(logship.Config{
		Sender: sender, BatchInterval: 10 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(lockedBuf{&jmu, &journal}, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	e := newTestExecutor(&fakeControlPlane{}, &fakeCheckout{err: errors.New(leakyCheckoutErr)}, &fakeDocker{}, nil)
	e.logs = factory
	if out := e.Execute(context.Background(), testLease()); out.HandedOff {
		t.Fatalf("unexpected handoff: %+v", out)
	}
	phase := sender.text("phase")
	if !strings.Contains(phase, "checkout: failed (") {
		t.Fatalf("phase stream = %q", phase)
	}
	for _, leak := range []string{"https://", "github.com", "ghs_", "/var/lib"} {
		if strings.Contains(phase, leak) {
			t.Fatalf("shipped line leaks %q: %q", leak, phase)
		}
	}
	jmu.Lock()
	defer jmu.Unlock()
	if !strings.Contains(journal.String(), "github.com") {
		t.Fatalf("journal lacks the diagnostic text: %q", journal.String())
	}
}

func TestExecuteLogsButSurvivesCheckoutCloseFailure(t *testing.T) {
	co := &fakeCheckout{result: happyCheckoutResult(), closeErr: errors.New("quota down")}
	dk := &fakeDocker{}
	e := newTestExecutor(&fakeControlPlane{}, co, dk, func(context.Context, docker.BuildRequest) (docker.BuildResult, error) {
		return docker.BuildResult{}, errors.New("build failed")
	})
	_ = e.Execute(context.Background(), testLease())
	if co.closeN != 1 {
		t.Fatalf("closeN = %d, want 1 (retry lives inside Close)", co.closeN)
	}
}

var noWait = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}

func TestCloseWithRetryRecoversAfterTransientFailures(t *testing.T) {
	calls := 0
	released := false
	err := closeWithRetry(context.Background(), func() error {
		calls++
		if calls <= 2 {
			return errors.New("retryable")
		}
		released = true
		return nil
	}, func() bool { return released }, noWait)
	if err != nil || calls != 3 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestCloseWithRetryIsBoundedAndLeaksNoGoroutine(t *testing.T) {
	before := runtime.NumGoroutine()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	calls := 0
	boom := errors.New("always")
	err := closeWithRetry(ctx, func() error { calls++; return boom }, func() bool { return false }, noWait)
	if !errors.Is(err, boom) || calls != len(noWait)+1 {
		t.Fatalf("err=%v calls=%d want %d", err, calls, len(noWait)+1)
	}
	if ctx.Err() != nil {
		t.Fatal("bounded retry consumed the whole test budget")
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("goroutines %d -> %d", before, after)
	}
}

func TestCloseWithRetryStopsWhenTerminal(t *testing.T) {
	calls := 0
	err := closeWithRetry(context.Background(), func() error { calls++; return errors.New("denied") }, func() bool { return true }, noWait)
	if err == nil || calls != 1 {
		t.Fatalf("err=%v calls=%d, terminal close must not retry", err, calls)
	}
}

func TestCloseWithRetryHonorsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	err := closeWithRetry(ctx, func() error { calls++; return errors.New("x") }, func() bool { return false }, []time.Duration{time.Hour})
	if calls != 1 || !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

type fakeOwned struct {
	failures int // Close fails this many times, then succeeds
	closes   int
	fd       int
}

func (f *fakeOwned) Close() error {
	f.closes++
	if f.closes <= f.failures {
		return errors.New("quota retryable")
	}
	f.fd = -1
	return nil
}
func (f *fakeOwned) WorkspaceFD() int { return f.fd }

func TestReleaseOwnedRetriesTransientFailure(t *testing.T) {
	f := &fakeOwned{failures: 2, fd: 7}
	if err := releaseOwned(context.Background(), f, []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}); err != nil {
		t.Fatalf("err = %v", err)
	}
	if f.closes != 3 {
		t.Fatalf("closes = %d, want 3", f.closes)
	}
}

func TestReleaseOwnedIsBoundedAndSurvivesCancelledCaller(t *testing.T) {
	f := &fakeOwned{failures: 1 << 30, fd: 7}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the caller's ctx is usually why we are failing
	err := releaseOwned(ctx, f, []time.Duration{time.Millisecond, time.Millisecond})
	if err == nil || f.closes != 3 {
		t.Fatalf("err=%v closes=%d, want failure after exactly 3 attempts", err, f.closes)
	}
}

// Neither failure path may drop an owned Result with a single bare Close.
func TestCheckoutFailurePathsUseBoundedRelease(t *testing.T) {
	src, err := os.ReadFile("checkout.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "_ = result.Close()") || strings.Count(string(src), "releaseOwned(ctx,") < 2 {
		t.Fatal("checkout.go failure paths must route through releaseOwned")
	}
}
