//go:build linux

package checkoutquota

import "golang.org/x/sys/unix"

// Raw unix calls bypass the standard library's ignoringEINTR, and the Go
// runtime interrupts threads with signals (SIGURG preemption), so any blocking
// syscall here can fail with EINTR. Found on the acer: checkouts failed
// intermittently with "interrupted system call". Retry exactly as os does.

func recvmsgRetry(fd int, p, oob []byte, flags int) (n, oobn, recvflags int, from unix.Sockaddr, err error) {
	for {
		n, oobn, recvflags, from, err = unix.Recvmsg(fd, p, oob, flags)
		if err != unix.EINTR {
			return n, oobn, recvflags, from, err
		}
	}
}

func sendmsgNRetry(fd int, p, oob []byte, to unix.Sockaddr, flags int) (int, error) {
	for {
		n, err := unix.SendmsgN(fd, p, oob, to, flags)
		if err != unix.EINTR {
			return n, err
		}
	}
}

func accept4Retry(fd, flags int) (int, unix.Sockaddr, error) {
	for {
		nfd, sa, err := unix.Accept4(fd, flags)
		if err != unix.EINTR {
			return nfd, sa, err
		}
	}
}
