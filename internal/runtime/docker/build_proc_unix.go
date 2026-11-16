//go:build unix

package docker

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// killProcessGroupOnCancel puts the build subprocess in its own process
// group and makes context cancellation SIGKILL that whole group, not just
// the docker CLI: buildx and anything it spawns die together, and a child
// that ignores SIGTERM cannot outlive the limit. Setpgid also keeps a
// terminal ^C aimed at the agent from reaching buildx behind the agent's
// own cancellation handling.
func killProcessGroupOnCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// Setpgid makes the child its own group leader, so pgid == pid.
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}
