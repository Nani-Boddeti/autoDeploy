package main

import (
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"golang.org/x/term"
)

type ttyRecorder struct {
	mu         sync.Mutex
	restores   int
	exitCodes  []int
	sigCh      chan<- os.Signal
	stopped    int
	readErr    error
	stateErr   error
	block      chan struct{}
	exited     chan struct{}
	registered chan struct{}
}

func (r *ttyRecorder) ops() ttyOps {
	return ttyOps{
		getState: func(int) (*term.State, error) { return &term.State{}, r.stateErr },
		restore: func(int, *term.State) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.restores++
			return nil
		},
		readPassword: func(int) ([]byte, error) {
			if r.block != nil {
				<-r.block
			}
			return []byte("v"), r.readErr
		},
		notify: func(c chan<- os.Signal) {
			r.sigCh = c
			if r.registered != nil {
				close(r.registered)
			}
		},
		stop: func(chan<- os.Signal) { r.mu.Lock(); r.stopped++; r.mu.Unlock() },
		exit: func(code int) {
			r.mu.Lock()
			r.exitCodes = append(r.exitCodes, code)
			r.mu.Unlock()
			close(r.exited)
		},
	}
}

func (r *ttyRecorder) restoreCount() int { r.mu.Lock(); defer r.mu.Unlock(); return r.restores }

func TestGuardedReadRestoresOnSuccessAndError(t *testing.T) {
	for _, readErr := range []error{nil, errors.New("read failed")} {
		r := &ttyRecorder{readErr: readErr}
		var out strings.Builder
		_, err := guardedReadSecret(0, &out, "p: ", r.ops())
		if (err != nil) != (readErr != nil) {
			t.Fatalf("err=%v", err)
		}
		if r.restoreCount() < 1 || r.stopped != 1 {
			t.Fatalf("restores=%d stopped=%d", r.restoreCount(), r.stopped)
		}
	}
}

func TestGuardedReadStateFailureReadsNothing(t *testing.T) {
	r := &ttyRecorder{stateErr: errors.New("no state")}
	ops := r.ops()
	ops.readPassword = func(int) ([]byte, error) { t.Fatal("read without saved state"); return nil, nil }
	var out strings.Builder
	if _, err := guardedReadSecret(0, &out, "p: ", ops); err == nil {
		t.Fatal("expected error")
	}
}

func TestGuardedReadRestoresAndExitsOnSignal(t *testing.T) {
	r := &ttyRecorder{block: make(chan struct{}), exited: make(chan struct{}), registered: make(chan struct{})}
	var out strings.Builder
	done := make(chan struct{})
	go func() { _, _ = guardedReadSecret(0, &out, "p: ", r.ops()); close(done) }()
	<-r.registered
	r.sigCh <- os.Interrupt
	<-r.exited
	if out.String() != "p: " {
		t.Fatalf("signal path wrote %q", out.String())
	}
	if r.restoreCount() < 1 || len(r.exitCodes) != 1 || r.exitCodes[0] == 0 {
		t.Fatalf("restores=%d exit=%v", r.restoreCount(), r.exitCodes)
	}
	close(r.block)
	<-done
}
