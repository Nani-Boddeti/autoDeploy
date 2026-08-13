//go:build linux

package checkoutcontain

import (
	"bytes"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// Directory is a pinned, operator-controlled directory descriptor. The caller
// owns it and must Close it after all relative opens have completed.
type Directory struct {
	file *os.File
	dev  uint64
	ino  uint64
	uid  uint32
}

// OpenTrustedDirectory opens an absolute configured root without resolving any
// symlink, magic link, mount crossing, or attacker-provided path component.
func OpenTrustedDirectory(path string, ownerUID uint32) (*Directory, error) {
	if !validAbsolute(path) {
		return nil, ErrUnavailable
	}
	fd, err := openTrustedAbsolute(path, unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		return nil, ErrUnavailable
	}
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Uid != ownerUID || st.Mode&0o022 != 0 {
		_ = unix.Close(fd)
		return nil, ErrUnavailable
	}
	return &Directory{file: os.NewFile(uintptr(fd), "trusted-directory"), dev: st.Dev, ino: st.Ino, uid: st.Uid}, nil
}

func validAbsolute(path string) bool {
	return strings.HasPrefix(path, "/") && path != "/" && len(path) <= 4096 && !strings.ContainsAny(path, "\x00\r\n")
}
func openTrustedAbsolute(path string, flags int) (int, error) {
	return openAbsolute(path, flags, unix.RESOLVE_BENEATH|unix.RESOLVE_NO_SYMLINKS|unix.RESOLVE_NO_MAGICLINKS|unix.RESOLVE_NO_XDEV)
}
func openPinnedAbsolute(path string, flags int) (int, error) {
	return openAbsolute(path, flags, unix.RESOLVE_BENEATH|unix.RESOLVE_NO_SYMLINKS|unix.RESOLVE_NO_MAGICLINKS)
}
func openAbsolute(path string, flags int, resolve uint64) (int, error) {
	root, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	defer unix.Close(root)
	return unix.Openat2(root, strings.TrimPrefix(path, "/"), &unix.OpenHow{Flags: uint64(flags | unix.O_CLOEXEC | unix.O_NOFOLLOW), Resolve: resolve})
}

// OpenExecutable opens a single basename relative to the pinned binary root.
// The returned descriptor is suitable for passing to checkout-exec as FD 3.
func (d *Directory) OpenExecutable(name string) (*os.File, error) {
	if d == nil || d.file == nil || !validRelativeName(name) || !d.stable() {
		return nil, ErrUnavailable
	}
	fd, err := unix.Openat2(int(d.file.Fd()), name, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_XDEV})
	if err != nil {
		return nil, ErrUnavailable
	}
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Uid != d.uid || st.Mode&0o022 != 0 || st.Mode&0o111 == 0 || !isELF(fd) {
		_ = unix.Close(fd)
		return nil, ErrUnavailable
	}
	return os.NewFile(uintptr(fd), "trusted-executable"), nil
}

// OpenDirectory opens a single checkout-relative directory and verifies its
// identity, so a rename/replacement cannot change the launch working directory.
func (d *Directory) OpenDirectory(name string, expectedDev, expectedIno uint64) (*os.File, error) {
	if d == nil || d.file == nil || !validRelativeName(name) || !d.stable() {
		return nil, ErrUnavailable
	}
	fd, err := unix.Openat2(int(d.file.Fd()), name, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_XDEV})
	if err != nil {
		return nil, ErrUnavailable
	}
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Uid != d.uid || st.Mode&0o022 != 0 || (expectedDev != 0 && (st.Dev != expectedDev || st.Ino != expectedIno)) {
		_ = unix.Close(fd)
		return nil, ErrUnavailable
	}
	return os.NewFile(uintptr(fd), "trusted-cwd"), nil
}
func validRelativeName(name string) bool {
	return name != "" && name != "." && name != ".." && len(name) <= 255 && !strings.ContainsAny(name, "/\x00\r\n")
}
func (d *Directory) stable() bool {
	var st unix.Stat_t
	return unix.Fstat(int(d.file.Fd()), &st) == nil && st.Mode&unix.S_IFMT == unix.S_IFDIR && st.Dev == d.dev && st.Ino == d.ino && st.Uid == d.uid && st.Mode&0o022 == 0
}
func (d *Directory) Close() error {
	if d == nil || d.file == nil {
		return nil
	}
	err := d.file.Close()
	d.file = nil
	if err != nil {
		return ErrUnavailable
	}
	return nil
}

func isELF(fd int) bool {
	buf := make([]byte, 20)
	n, err := unix.Pread(fd, buf, 0)
	return err == nil && n == len(buf) && bytes.Equal(buf[:4], []byte{0x7f, 'E', 'L', 'F'}) && (buf[4] == 1 || buf[4] == 2) && buf[5] == 1 && buf[6] == 1 && buf[7] == 0 && buf[16] == 2
}
