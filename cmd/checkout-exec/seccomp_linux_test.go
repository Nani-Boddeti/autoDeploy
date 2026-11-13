//go:build linux && (amd64 || arm64)

package main

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// evalFilter is a minimal classic-BPF interpreter for the instructions namespaceFilter emits.
func evalFilter(t *testing.T, arch, nr, arg0 uint32) uint32 {
	t.Helper()
	prog := namespaceFilter()
	var a uint32
	for pc := 0; pc < len(prog); pc++ {
		in := prog[pc]
		switch in.Code {
		case bpfLdAbsW:
			switch in.K {
			case 0:
				a = nr
			case 4:
				a = arch
			case 16:
				a = arg0
			default:
				t.Fatalf("unexpected load offset %d", in.K)
			}
		case bpfJeqK:
			if a == in.K {
				pc += int(in.Jt)
			} else {
				pc += int(in.Jf)
			}
		case bpfJsetK:
			if a&in.K != 0 {
				pc += int(in.Jt)
			} else {
				pc += int(in.Jf)
			}
		case bpfRet:
			return in.K
		default:
			t.Fatalf("unexpected opcode %#x", in.Code)
		}
	}
	t.Fatal("filter fell off the end")
	return 0
}

func TestNamespaceFilterDecisions(t *testing.T) {
	arch := uint32(auditArchX8664)
	if runtime.GOARCH == "arm64" {
		arch = auditArchAArch64
	}
	cases := []struct {
		name         string
		arch, nr, a0 uint32
		want         uint32
	}{
		{"clone3 -> ENOSYS", arch, unix.SYS_CLONE3, 0, seccompRetErrno | uint32(unix.ENOSYS)},
		{"unshare -> EPERM", arch, unix.SYS_UNSHARE, 0, seccompRetErrno | uint32(unix.EPERM)},
		{"setns -> EPERM", arch, unix.SYS_SETNS, 0, seccompRetErrno | uint32(unix.EPERM)},
		{"clone(NEWUSER) -> EPERM", arch, unix.SYS_CLONE, 0x10000000 | 0x11, seccompRetErrno | uint32(unix.EPERM)},
		{"clone(NEWNET) -> EPERM", arch, unix.SYS_CLONE, 0x40000000, seccompRetErrno | uint32(unix.EPERM)},
		{"clone(thread) allowed", arch, unix.SYS_CLONE, 0x003d0f00, seccompRetAllow},
		{"read allowed", arch, unix.SYS_READ, 0x10000000, seccompRetAllow},
		{"foreign arch killed", 0x40000003, unix.SYS_READ, 0, seccompRetKillProc},
	}
	for _, c := range cases {
		if got := evalFilter(t, c.arch, c.nr, c.a0); got != c.want {
			t.Errorf("%s: got %#x want %#x", c.name, got, c.want)
		}
	}
}

// The kernel-enforced check: a re-executed copy installs the filter on its locked thread and probes.
func TestNamespaceFilterEnforcedByKernel(t *testing.T) {
	if os.Getenv("CHECKOUT_FILTER_PROBE") == "1" {
		runtime.LockOSThread()
		if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
			t.Fatal(err)
		}
		if err := installNamespaceFilter(); err != nil {
			t.Fatalf("install: %v", err)
		}
		if _, _, e := syscall.RawSyscall(unix.SYS_UNSHARE, 0x10000000, 0, 0); e != unix.EPERM {
			t.Fatalf("unshare(NEWUSER) = %v, want EPERM", e)
		}
		if _, _, e := syscall.RawSyscall6(unix.SYS_CLONE3, 0, 0, 0, 0, 0, 0); e != unix.ENOSYS {
			t.Fatalf("clone3 = %v, want ENOSYS", e)
		}
		if unix.Getpid() == 0 {
			t.Fatal("ordinary syscalls blocked")
		}
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestNamespaceFilterEnforcedByKernel$")
	cmd.Env = append(os.Environ(), "CHECKOUT_FILTER_PROBE=1")
	if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "PASS") {
		t.Fatalf("probe failed: %v\n%s", err, out)
	}
}
