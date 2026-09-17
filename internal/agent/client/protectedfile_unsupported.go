//go:build !darwin && !linux

package agentclient

import "errors"

// ErrProtectedFileRejected is returned for any protected file this package
// cannot trust. On an unsupported platform every protected file read fails
// closed rather than skip the ownership/mode checks.
var ErrProtectedFileRejected = errors.New("protected file rejected")

func readProtectedFile(string) ([]byte, error) { return nil, ErrProtectedFileRejected }
