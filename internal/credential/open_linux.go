//go:build linux

package credential

import (
	"errors"
	"strings"

	"golang.org/x/sys/unix"
)

func openCredentialFile(path string, expectedUID int) (int, error) {
	rootFD, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	defer unix.Close(rootFD)
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	current := rootFD
	for index, part := range parts {
		last := index == len(parts)-1
		flags := uint64(unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW)
		if !last {
			flags |= unix.O_DIRECTORY
		} else {
			flags |= unix.O_NONBLOCK
		}
		next, openErr := unix.Openat2(current, part, &unix.OpenHow{Flags: flags, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS})
		if openErr != nil {
			if current != rootFD {
				unix.Close(current)
			}
			return -1, openErr
		}
		if current != rootFD {
			unix.Close(current)
		}
		current = next
		if !last && !safeDirectory(current, expectedUID) {
			unix.Close(current)
			return -1, errors.New("unsafe credential directory")
		}
	}
	return current, nil
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
