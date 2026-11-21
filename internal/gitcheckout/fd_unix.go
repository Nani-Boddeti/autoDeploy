//go:build unix

package gitcheckout

import "golang.org/x/sys/unix"

func closeFD(fd int) error { return unix.Close(fd) }

func dupFD(fd int) (int, error) { return unix.Dup(fd) }

// cloexecPipe returns a pipe whose both ends are close-on-exec.
func cloexecPipe() (int, int, error) {
	fds := []int{0, 0}
	if err := unix.Pipe(fds); err != nil {
		return -1, -1, err
	}
	unix.CloseOnExec(fds[0])
	unix.CloseOnExec(fds[1])
	return fds[0], fds[1], nil
}
