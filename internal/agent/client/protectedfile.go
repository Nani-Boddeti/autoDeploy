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

// ReadProtectedFile (exported for the agent's other owner-only secret files,
// e.g. the Traefik API credential) opens path with O_NOFOLLOW so a symlink at
// the final path component is always rejected, then requires a regular file
// owned by the current effective user with mode exactly 0600 before reading
// its bounded contents. This is the same discipline tasks/todo.md Step 6c
// requires of the credential file. O_NONBLOCK keeps a FIFO at path from
// blocking open(2) before the regular-file check rejects it. Parent
// directories are not walked the way internal/credential does for
// orchestrator-mounted secrets: that is sufficient for files in the agent's
// own 0700 state directory; for an operator-chosen path (the Traefik API
// credentials file) the owner and exact-0600 checks on the opened descriptor
// are the protection.
func ReadProtectedFile(path string) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
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
