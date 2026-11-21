package main

import (
	"context"
	"errors"
	"strconv"
	"syscall"
)

// outcome is the helper's verdict for one spawn: the exit code reported to the
// client, whether the deadline fired, and an optional fixed diagnostic line.
type outcome struct {
	code     int
	timedOut bool
	diag     string
}

// exitStatus is the part of *exec.ExitError that classifyFailure needs, so tests
// can supply a fake wait status.
type exitStatus interface {
	ExitCode() int
	Sys() any
}

// Exit 125 has several helper-side causes; each gets one fixed diagnostic line so
// the client's last stderr line tells them apart. Lines never carry error strings,
// paths, arguments or environment values.
const (
	diagCleanupFailed = "checkout-exec: helper cleanup failed (exit 125)"
	diagCancelled     = "checkout-exec: cancelled before completion (exit 125)"
	diagAbnormalExit  = "checkout-exec: git terminated abnormally (exit 125)"
)

// classifyFailure maps the post-run state to the exit code and diagnostic.
// Precedence matches the original inline logic: cleanup failure, deadline (124),
// other cancellation, then the child's own result.
func classifyFailure(cleanupOK bool, ctxErr, runErr error) outcome {
	if !cleanupOK {
		return outcome{code: 125, diag: diagCleanupFailed}
	}
	if errors.Is(ctxErr, context.DeadlineExceeded) {
		return outcome{code: 124, timedOut: true}
	}
	if ctxErr != nil {
		return outcome{code: 125, diag: diagCancelled}
	}
	if runErr == nil {
		return outcome{}
	}
	var exit exitStatus
	if errors.As(runErr, &exit) {
		if code := exit.ExitCode(); code >= 1 && code <= 255 {
			return outcome{code: code}
		}
		if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return outcome{code: 125, diag: "checkout-exec: git terminated by signal " + signalName(ws.Signal()) + " (exit 125)"}
		}
		return outcome{code: 125, diag: diagAbnormalExit}
	}
	return outcome{code: 125, diag: "checkout-exec: could not start git: " + startFailure(runErr) + " (exit 125)"}
}

func signalName(sig syscall.Signal) string {
	names := map[syscall.Signal]string{
		syscall.SIGHUP: "SIGHUP", syscall.SIGINT: "SIGINT", syscall.SIGQUIT: "SIGQUIT",
		syscall.SIGILL: "SIGILL", syscall.SIGABRT: "SIGABRT", syscall.SIGBUS: "SIGBUS",
		syscall.SIGFPE: "SIGFPE", syscall.SIGKILL: "SIGKILL", syscall.SIGSEGV: "SIGSEGV",
		syscall.SIGPIPE: "SIGPIPE", syscall.SIGTERM: "SIGTERM",
	}
	if name, ok := names[sig]; ok {
		return name
	}
	return "SIG" + strconv.Itoa(int(sig))
}

// startFailure returns only a symbolic errno class, never the error text.
func startFailure(err error) string {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return "start failed"
	}
	names := map[syscall.Errno]string{
		syscall.EPERM: "EPERM", syscall.ENOENT: "ENOENT", syscall.EACCES: "EACCES",
		syscall.EAGAIN: "EAGAIN", syscall.ENOMEM: "ENOMEM", syscall.EINVAL: "EINVAL",
		syscall.EMFILE: "EMFILE", syscall.ENFILE: "ENFILE", syscall.ENOEXEC: "ENOEXEC",
		syscall.E2BIG: "E2BIG", syscall.ENOTDIR: "ENOTDIR", syscall.ELOOP: "ELOOP",
		syscall.EIO: "EIO",
	}
	if name, ok := names[errno]; ok {
		return name
	}
	return "errno " + strconv.Itoa(int(errno))
}
