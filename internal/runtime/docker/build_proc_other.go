//go:build !unix

package docker

import "os/exec"

// killProcessGroupOnCancel keeps exec's default (kill the process only) on
// platforms without POSIX process groups; the agent's build path targets
// Linux (see Build).
func killProcessGroupOnCancel(*exec.Cmd) {}
