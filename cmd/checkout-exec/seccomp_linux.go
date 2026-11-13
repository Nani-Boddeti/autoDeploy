//go:build linux && (amd64 || arm64)

package main

import (
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Why this exists: the unit cannot use RestrictNamespaces=. systemd implements it by answering
// clone3 with ENOSYS, and Go's os/exec has no fallback when SysProcAttr.UseCgroupFD (clone3
// CLONE_INTO_CGROUP) is used, so every spawn failed with "function not implemented" (reproduced
// on the acer, systemd 255, with a transient unit). The git child therefore gets the equivalent
// filter here, installed right before execveat: no new namespaces and no clone3 (glibc falls back
// to clone on ENOSYS, exactly what systemd's own filter relies on).
const (
	auditArchX8664   = 0xC000003E
	auditArchAArch64 = 0xC00000B7
	// CLONE_NEWNS|NEWCGROUP|NEWUTS|NEWIPC|NEWUSER|NEWPID|NEWNET|NEWTIME
	cloneNamespaceMask = 0x00020000 | 0x02000000 | 0x04000000 | 0x08000000 | 0x10000000 | 0x20000000 | 0x40000000 | 0x00000080
	seccompRetAllow    = 0x7fff0000
	seccompRetKillProc = 0x80000000
	seccompRetErrno    = 0x00050000
	bpfLdAbsW          = 0x20 // BPF_LD|BPF_W|BPF_ABS
	bpfJeqK            = 0x15 // BPF_JMP|BPF_JEQ|BPF_K
	bpfJsetK           = 0x45 // BPF_JMP|BPF_JSET|BPF_K
	bpfRet             = 0x06 // BPF_RET|BPF_K
)

func namespaceFilter() []unix.SockFilter {
	arch := uint32(auditArchX8664)
	if runtime.GOARCH == "arm64" {
		arch = auditArchAArch64
	}
	enosys, eperm := seccompRetErrno|uint32(unix.ENOSYS), seccompRetErrno|uint32(unix.EPERM)
	stmt := func(code uint16, k uint32) unix.SockFilter { return unix.SockFilter{Code: code, K: k} }
	jump := func(code uint16, k uint32, jt, jf uint8) unix.SockFilter {
		return unix.SockFilter{Code: code, Jt: jt, Jf: jf, K: k}
	}
	return []unix.SockFilter{
		stmt(bpfLdAbsW, 4), // seccomp_data.arch
		jump(bpfJeqK, arch, 1, 0),
		stmt(bpfRet, seccompRetKillProc),
		stmt(bpfLdAbsW, 0), // seccomp_data.nr
		jump(bpfJeqK, unix.SYS_CLONE3, 7, 0),
		jump(bpfJeqK, unix.SYS_UNSHARE, 7, 0),
		jump(bpfJeqK, unix.SYS_SETNS, 6, 0),
		jump(bpfJeqK, unix.SYS_CLONE, 0, 3),
		stmt(bpfLdAbsW, 16), // args[0] low word = clone flags
		jump(bpfJsetK, cloneNamespaceMask, 3, 0),
		stmt(bpfRet, seccompRetAllow), // plain clone/fork/thread
		stmt(bpfRet, seccompRetAllow), // every other syscall
		stmt(bpfRet, enosys),          // clone3 target
		stmt(bpfRet, eperm),           // unshare/setns/namespaced clone target
	}
}

// installNamespaceFilter applies the filter to the calling thread only (seccomp is per-thread
// without TSYNC); the caller must have locked its OS thread and must exec from that same thread.
func installNamespaceFilter() error {
	f := namespaceFilter()
	prog := unix.SockFprog{Len: uint16(len(f)), Filter: &f[0]}
	return unix.Prctl(unix.PR_SET_SECCOMP, unix.SECCOMP_MODE_FILTER, uintptr(unsafe.Pointer(&prog)), 0, 0)
}
