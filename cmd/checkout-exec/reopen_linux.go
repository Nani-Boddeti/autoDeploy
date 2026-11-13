//go:build linux

package main

import (
	"errors"
	"os"
	"regexp"

	"golang.org/x/sys/unix"
)

// The quota helper and this service run in separate systemd mount namespaces,
// so a workspace directory FD received over SCM_RIGHTS references a vfsmount
// from the quota service's namespace copy. A child that fchdir()s to it gets
// getcwd() == ENOENT (git: "unable to get current working directory"), observed
// on the acer. The service therefore re-opens the very same directory through
// its own checkout-root FD and gives the child that FD as cwd.

// workspaceNameRE mirrors internal/checkoutquota Allocate: ".checkout-%08x".
var workspaceNameRE = regexp.MustCompile(`^\.checkout-[0-9a-f]{8}$`)

var errWorkspaceNotFound = errors.New("workspace not found under checkout root")

// openCheckoutRoot opens the checkout root (a mount point, so no RESOLVE_NO_XDEV)
// and validates it from the descriptor. The caller owns the returned FD.
func openCheckoutRoot(path string) (int, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	var fs unix.Statfs_t
	if err := unix.Fstatfs(fd, &fs); err != nil || fs.Type != unix.XFS_SUPER_MAGIC || !validRootDirectory(fd) {
		_ = unix.Close(fd)
		return -1, errors.New("untrusted checkout root")
	}
	return fd, nil
}

// validRootDirectory: a directory owned by root:root without group/other write.
func validRootDirectory(fd int) bool {
	var st unix.Stat_t
	return unix.Fstat(fd, &st) == nil && st.Mode&unix.S_IFMT == unix.S_IFDIR && st.Uid == 0 && st.Gid == 0 && st.Mode&0o022 == 0
}

// reopenWorkspace returns a new FD, opened through rootFD, for the same
// directory (dev+ino) as received. Exactly one ".checkout-xxxxxxxx" entry must
// match. The caller owns the returned FD; received is neither closed nor
// modified. On error no FD is leaked and -1 is returned.
func reopenWorkspace(rootFD, received int) (int, error) {
	var want unix.Stat_t
	if err := unix.Fstat(received, &want); err != nil || want.Mode&unix.S_IFMT != unix.S_IFDIR {
		return -1, errWorkspaceNotFound
	}
	// A fresh open description: dup'd FDs share the directory offset, which
	// would make concurrent or repeated listings come back empty.
	listFD, err := unix.Openat(rootFD, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	dir := os.NewFile(uintptr(listFD), "checkout-root")
	names, err := dir.Readdirnames(-1)
	_ = dir.Close()
	if err != nil {
		return -1, err
	}
	match := ""
	for _, name := range names {
		if !workspaceNameRE.MatchString(name) {
			continue
		}
		var st unix.Stat_t
		if unix.Fstatat(rootFD, name, &st, unix.AT_SYMLINK_NOFOLLOW) != nil || st.Dev != want.Dev || st.Ino != want.Ino {
			continue
		}
		if match != "" {
			return -1, errors.New("ambiguous workspace")
		}
		match = name
	}
	if match == "" {
		return -1, errWorkspaceNotFound
	}
	fd, err := unix.Openat2(rootFD, match, &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_XDEV | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		return -1, err
	}
	// Close the fstatat/open race: the opened directory must still be the received one.
	var got unix.Stat_t
	if unix.Fstat(fd, &got) != nil || got.Dev != want.Dev || got.Ino != want.Ino {
		_ = unix.Close(fd)
		return -1, errWorkspaceNotFound
	}
	return fd, nil
}
