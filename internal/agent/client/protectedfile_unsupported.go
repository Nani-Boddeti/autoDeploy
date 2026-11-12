//go:build !darwin && !linux

package agentclient

import "errors"

// ErrProtectedFileRejected is returned for any protected file this package
// cannot trust. On an unsupported platform every protected file read fails
// closed rather than skip the ownership/mode checks.
var ErrProtectedFileRejected = errors.New("protected file rejected")

// ReadProtectedFile always fails closed on this platform.
func ReadProtectedFile(string) ([]byte, error) { return nil, ErrProtectedFileRejected }
