//go:build unix

package traefik

import (
	"errors"
	"io/fs"
	"syscall"
)

// openNoFollow is O_NOFOLLOW, applied via raw syscall flags.
const openNoFollow = syscall.O_NOFOLLOW

// ownerUID returns the owning UID recorded in fi.
func ownerUID(fi fs.FileInfo) (uint32, error) {
	sys, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errors.New("traefik: file info carries no Stat_t")
	}
	return uint32(sys.Uid), nil
}
