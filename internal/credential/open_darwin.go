//go:build darwin

package credential

import (
	"errors"
	"strings"

	"golang.org/x/sys/unix"
)

func openCredentialFile(path string, expectedUID int) (int, error) {
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for index, part := range parts {
		last := index == len(parts)-1
		flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW
		if last {
			flags |= unix.O_NONBLOCK
		}
		if !last {
			flags |= unix.O_DIRECTORY
		}
		next, openErr := unix.Openat(fd, part, flags, 0)
		unix.Close(fd)
		if openErr != nil {
			return -1, openErr
		}
		fd = next
		if !last && !safeDirectory(fd, expectedUID) {
			unix.Close(fd)
			return -1, errors.New("unsafe credential directory")
		}
	}
	return fd, nil
}

func safeDirectory(fd, expectedUID int) bool {
	var stat unix.Stat_t
	if unix.Fstat(fd, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return false
	}
	if int(stat.Uid) != 0 && int(stat.Uid) != expectedUID {
		return false
	}
	perms := stat.Mode & 0777
	return perms&0022 == 0 || (stat.Uid == 0 && stat.Mode&unix.S_ISVTX != 0)
}
