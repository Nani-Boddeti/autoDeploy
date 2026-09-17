//go:build darwin || linux

package agentclient

import (
	"errors"

	"golang.org/x/sys/unix"
)

// maxProtectedFileBytes bounds every file this package treats as sensitive
// input (the credential file, and an optional enrollment-token file). Both
// are short opaque tokens; this is a generous ceiling, not an expected size.
const maxProtectedFileBytes = 4096

// ErrProtectedFileRejected is returned for any protected file this package
// cannot trust: missing, a symlink, wrong owner, wrong mode, or too large.
// It deliberately never reveals which check failed.
var ErrProtectedFileRejected = errors.New("protected file rejected")

// readProtectedFile opens path with O_NOFOLLOW so a symlink at the final
// path component is always rejected, then requires a regular file owned by
// the current effective user with mode exactly 0600 before reading its
// bounded contents. This is the same discipline tasks/todo.md Step 6c
// requires of the credential file. It deliberately does not walk and
// validate parent directories the way internal/credential does for
// control-plane secrets mounted by an external orchestrator: the agent
// state directory is created and owned by this same process (state.New /
// writeCredentialAtomic use 0700), so that additional hardening protects
// against a threat this package does not have.
func readProtectedFile(path string) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, ErrProtectedFileRejected
	}
	defer unix.Close(fd) //nolint:errcheck

	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || !protectedFileStatOK(stat) {
		return nil, ErrProtectedFileRejected
	}
	if stat.Size < 0 || stat.Size > maxProtectedFileBytes {
		return nil, ErrProtectedFileRejected
	}
	contents, err := readAllFD(fd, int(stat.Size))
	if err != nil || int64(len(contents)) != stat.Size {
		return nil, ErrProtectedFileRejected
	}
	return contents, nil
}

func protectedFileStatOK(stat unix.Stat_t) bool {
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return false
	}
	if stat.Mode&0777 != 0600 {
		return false
	}
	return int(stat.Uid) == unix.Geteuid()
}

func readAllFD(fd, size int) ([]byte, error) {
	contents := make([]byte, 0, size)
	buffer := make([]byte, 512)
	for {
		n, err := unix.Read(fd, buffer)
		if n > 0 {
			contents = append(contents, buffer[:n]...)
			if len(contents) > size {
				return nil, errors.New("protected file grew during read")
			}
		}
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return contents, nil
		}
	}
}
