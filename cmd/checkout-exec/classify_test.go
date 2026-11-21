//go:build unix

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

// fakeExit satisfies the exitStatus interface classifyFailure extracts with errors.As.
type fakeExit struct {
	code int
	sys  any
}

func (f *fakeExit) Error() string { return "fake exit" }
func (f *fakeExit) ExitCode() int { return f.code }
func (f *fakeExit) Sys() any      { return f.sys }

func TestClassifyFailure(t *testing.T) {
	sigKill := syscall.WaitStatus(9) // low 7 bits carry the signal on linux and darwin
	cases := []struct {
		name      string
		cleanupOK bool
		ctxErr    error
		runErr    error
		want      outcome
	}{
		{"success", true, nil, nil, outcome{code: 0}},
		{"cleanup failed", false, nil, nil, outcome{125, false, "checkout-exec: helper cleanup failed (exit 125)"}},
		{"cleanup failed after git error", false, nil, &fakeExit{1, nil}, outcome{125, false, "checkout-exec: helper cleanup failed (exit 125)"}},
		{"deadline", true, context.DeadlineExceeded, errors.New("killed"), outcome{code: 124, timedOut: true}},
		{"cancelled", true, context.Canceled, errors.New("killed"), outcome{125, false, "checkout-exec: cancelled before completion (exit 125)"}},
		{"exit 1", true, nil, &fakeExit{1, nil}, outcome{code: 1}},
		{"exit 128", true, nil, &fakeExit{128, nil}, outcome{code: 128}},
		{"exit 255", true, nil, &fakeExit{255, nil}, outcome{code: 255}},
		{"signal kill", true, nil, &fakeExit{-1, sigKill}, outcome{125, false, "checkout-exec: git terminated by signal SIGKILL (exit 125)"}},
		{"signal unnamed", true, nil, &fakeExit{-1, syscall.WaitStatus(60)}, outcome{125, false, "checkout-exec: git terminated by signal SIG60 (exit 125)"}},
		{"exit -1 not signalled", true, nil, &fakeExit{-1, syscall.WaitStatus(0)}, outcome{125, false, "checkout-exec: git terminated abnormally (exit 125)"}},
		{"start EPERM", true, nil, &os.PathError{Op: "fork/exec", Path: "/secret/path", Err: syscall.EPERM}, outcome{125, false, "checkout-exec: could not start git: EPERM (exit 125)"}},
		{"start ENOENT wrapped", true, nil, fmt.Errorf("x: %w", &os.PathError{Op: "fork/exec", Path: "/p", Err: syscall.ENOENT}), outcome{125, false, "checkout-exec: could not start git: ENOENT (exit 125)"}},
		{"start unknown errno", true, nil, syscall.Errno(4242), outcome{125, false, "checkout-exec: could not start git: errno 4242 (exit 125)"}},
		{"start non-errno", true, nil, errors.New("secret token=abc /etc/x"), outcome{125, false, "checkout-exec: could not start git: start failed (exit 125)"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyFailure(tc.cleanupOK, tc.ctxErr, tc.runErr)
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			if strings.Contains(got.diag, "secret") || strings.Contains(got.diag, "/") {
				t.Fatalf("diag leaks detail: %q", got.diag)
			}
		})
	}
}

func TestClassifyFailureRealChildSignal(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skip(err)
	}
	_ = cmd.Process.Kill()
	err := cmd.Wait()
	got := classifyFailure(true, nil, err)
	want := outcome{125, false, "checkout-exec: git terminated by signal SIGKILL (exit 125)"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestClassifyFailureRealStartFailure(t *testing.T) {
	cmd := exec.Command("/nonexistent-binary-for-test")
	err := cmd.Start()
	got := classifyFailure(true, nil, err)
	want := outcome{125, false, "checkout-exec: could not start git: ENOENT (exit 125)"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestClassifyFailureDiagLinesAreDistinct(t *testing.T) {
	seen := map[string]string{}
	inputs := map[string]outcome{
		"cleanup":   classifyFailure(false, nil, nil),
		"cancelled": classifyFailure(true, context.Canceled, nil),
		"signal":    classifyFailure(true, nil, &fakeExit{-1, syscall.WaitStatus(9)}),
		"abnormal":  classifyFailure(true, nil, &fakeExit{-1, syscall.WaitStatus(0)}),
		"start":     classifyFailure(true, nil, syscall.EPERM),
	}
	for name, o := range inputs {
		if o.code != 125 || o.diag == "" {
			t.Fatalf("%s: got %+v", name, o)
		}
		if prev, dup := seen[o.diag]; dup {
			t.Fatalf("%s and %s share diag %q", prev, name, o.diag)
		}
		seen[o.diag] = name
	}
}
