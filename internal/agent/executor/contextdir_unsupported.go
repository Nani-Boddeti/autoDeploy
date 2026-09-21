//go:build !linux

package executor

import "errors"

// ErrUnsupportedPlatform mirrors internal/runtime/docker.ErrUnsupportedPlatform:
// the agent's checkout-to-build-context bridge is a Linux-only /proc magic
// symlink trick (see contextdir_linux.go) and has no portable equivalent.
var ErrUnsupportedPlatform = errors.New("agent executor unsupported on this platform")

func contextDirFromWorkspace(int) (string, error) {
	return "", ErrUnsupportedPlatform
}
