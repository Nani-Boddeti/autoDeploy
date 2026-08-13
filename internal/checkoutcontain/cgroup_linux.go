//go:build linux

// Package checkoutcontain provides the fail-closed cgroup-v2 execution boundary.
package checkoutcontain

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// ErrUnavailable deliberately contains no host paths or kernel details.
var ErrUnavailable = errors.New("checkout containment unavailable")

const (
	maxLeafName = 64
	cleanupWait = 5 * time.Second
)

// Limits are hard cgroup-v2 limits. IOMax applies to both read and write bytes
// per second on the configured block device.
type Limits struct {
	MemoryMax, PIDsMax, IOMax uint64
	CPUQuota, CPUPeriod       uint64
	DeviceMajor, DeviceMinor  uint32
}

type Manager struct {
	root   *os.File
	limits Limits
	mu     sync.Mutex
	closed bool
	leaves uint32
}

// Leaf owns one empty cgroup-v2 child. It must remain open until the command
// using SysProcAttr has started; close always kills all descendants first.
type Leaf struct {
	manager  *Manager
	root     *os.File
	dir      *os.File
	name     string
	dev, ino uint64
	mu       sync.Mutex
	done     bool
}

// New opens an operator-configured delegated cgroup root. It is intentionally
// Linux-only and has no fallback for cgroup v1 or post-spawn migration.
func New(root string, limits Limits) (*Manager, error) {
	if !validLimits(limits) || !validAbsolute(root) {
		return nil, ErrUnavailable
	}
	fd, err := openPinnedAbsolute(root, unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		return nil, ErrUnavailable
	}
	f := os.NewFile(uintptr(fd), "cgroup-root")
	if !validCgroupRoot(fd) {
		_ = f.Close()
		return nil, ErrUnavailable
	}
	return &Manager{root: f, limits: limits}, nil
}

func validLimits(l Limits) bool {
	return l.MemoryMax > 0 && l.PIDsMax > 0 && l.IOMax > 0 && l.CPUQuota > 0 && l.CPUPeriod > 0 && l.CPUQuota <= l.CPUPeriod
}

func validCgroupRoot(fd int) bool {
	var fs unix.Statfs_t
	var st unix.Stat_t
	if unix.Fstatfs(fd, &fs) != nil || fs.Type != unix.CGROUP2_SUPER_MAGIC || unix.Fstat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Mode&0o022 != 0 {
		return false
	}
	// A delegated root may be owned by the unprivileged agent, but must not be
	// writable by any other principal. Its ownership is verified from this FD.
	if st.Uid != uint32(os.Geteuid()) && st.Uid != 0 {
		return false
	}
	controllers, err := readControl(fd, "cgroup.controllers")
	if err != nil || !hasControllers(controllers) {
		return false
	}
	enabled, err := readControl(fd, "cgroup.subtree_control")
	if err != nil || !hasControllers(enabled) {
		return false
	}
	for _, name := range []string{"cgroup.kill", "cgroup.events", "cgroup.procs"} {
		controlFD, err := openControl(fd, name, unix.O_RDONLY)
		if err != nil {
			return false
		}
		_ = unix.Close(controlFD)
	}
	// Controller-enabled non-leaf cgroups cannot contain processes. Refuse a
	// root that violates this kernel invariant rather than creating a sibling in
	// an ambiguous hierarchy.
	procs, err := readControl(fd, "cgroup.procs")
	return err == nil && strings.TrimSpace(procs) == ""
}

func hasControllers(v string) bool {
	got := make(map[string]bool)
	for _, x := range strings.Fields(v) {
		got[x] = true
	}
	return got["cpu"] && got["memory"] && got["pids"] && got["io"]
}

// Create makes and configures a unique caller-selected opaque leaf name. The
// name is only a directory entry beneath the pinned root descriptor.
func (m *Manager) Create(name string) (*Leaf, error) {
	if m == nil || !validLeafName(name) {
		return nil, ErrUnavailable
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.root == nil {
		return nil, ErrUnavailable
	}
	rootFD := int(m.root.Fd())
	if err := unix.Mkdirat(rootFD, name, 0o700); err != nil {
		return nil, ErrUnavailable
	}
	fd, err := unix.Openat2(rootFD, name, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_XDEV})
	if err != nil {
		_ = unix.Unlinkat(rootFD, name, unix.AT_REMOVEDIR)
		return nil, ErrUnavailable
	}
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil {
		_ = unix.Close(fd)
		_ = unix.Unlinkat(rootFD, name, unix.AT_REMOVEDIR)
		return nil, ErrUnavailable
	}
	leaf := &Leaf{root: m.root, dir: os.NewFile(uintptr(fd), "cgroup-leaf"), name: name, dev: st.Dev, ino: st.Ino}
	if !validCgroupLeaf(fd) || !configureLeaf(fd, m.limits) {
		_ = leaf.Close()
		return nil, ErrUnavailable
	}
	m.leaves++
	leaf.manager = m
	return leaf, nil
}

// CreateUnique creates an unpredictable child name below the pinned root. A
// collision is retried a bounded number of times; it never reuses a leaf.
func (m *Manager) CreateUnique(prefix string) (*Leaf, error) {
	if !validLeafName(prefix) || len(prefix) > maxLeafName-17 {
		return nil, ErrUnavailable
	}
	for range 8 {
		name, err := uniqueLeafName(prefix)
		if err != nil {
			return nil, ErrUnavailable
		}
		leaf, err := m.Create(name)
		if err == nil {
			return leaf, nil
		}
	}
	return nil, ErrUnavailable
}

func uniqueLeafName(prefix string) (string, error) {
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return prefix + "-" + hex.EncodeToString(random[:]), nil
}

func validCgroupLeaf(fd int) bool {
	var fs unix.Statfs_t
	var st unix.Stat_t
	if unix.Fstatfs(fd, &fs) != nil || fs.Type != unix.CGROUP2_SUPER_MAGIC || unix.Fstat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Mode&0o022 != 0 {
		return false
	}
	for _, name := range []string{"memory.max", "cpu.max", "pids.max", "io.max", "cgroup.kill", "cgroup.events"} {
		if fd, err := openControl(fd, name, unix.O_RDONLY); err != nil {
			return false
		} else {
			_ = unix.Close(fd)
		}
	}
	return true
}

func configureLeaf(fd int, l Limits) bool {
	values := []struct{ name, value string }{
		{"memory.max", strconv.FormatUint(l.MemoryMax, 10)},
		{"cpu.max", fmt.Sprintf("%d %d", l.CPUQuota, l.CPUPeriod)},
		{"pids.max", strconv.FormatUint(l.PIDsMax, 10)},
		{"io.max", fmt.Sprintf("%d:%d rbps=%d wbps=%d", l.DeviceMajor, l.DeviceMinor, l.IOMax, l.IOMax)},
	}
	for _, v := range values {
		if !writeExact(fd, v.name, v.value) {
			return false
		}
	}
	return true
}

func validLeafName(name string) bool {
	return len(name) > 0 && len(name) <= maxLeafName && name != "." && name != ".." && !strings.ContainsAny(name, "/\x00")
}

func openControl(dirFD int, name string, flags int) (int, error) {
	return unix.Openat2(dirFD, name, &unix.OpenHow{Flags: uint64(flags | unix.O_CLOEXEC | unix.O_NOFOLLOW), Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_XDEV})
}
func readControl(dirFD int, name string) (string, error) {
	fd, err := openControl(dirFD, name, unix.O_RDONLY)
	if err != nil {
		return "", err
	}
	defer unix.Close(fd)
	b := make([]byte, 4096)
	n, err := unix.Read(fd, b)
	if err != nil || n == len(b) {
		return "", ErrUnavailable
	}
	return string(b[:n]), nil
}
func writeExact(dirFD int, name, value string) bool {
	fd, err := openControl(dirFD, name, unix.O_WRONLY)
	if err != nil {
		return false
	}
	n, err := unix.Write(fd, []byte(value))
	closeErr := unix.Close(fd)
	if err != nil || n != len(value) || closeErr != nil {
		return false
	}
	got, err := readControl(dirFD, name)
	if err != nil {
		return false
	}
	return canonicalControl(name, got) == canonicalControl(name, value)
}
func canonicalControl(name, value string) string {
	fields := strings.Fields(value)
	if name != "io.max" {
		return strings.Join(fields, " ")
	}
	if len(fields) < 2 {
		return ""
	}
	// The kernel can reorder io.max attributes; compare the exact requested
	// device and rates, rejecting missing or extra conflicting values.
	want := make(map[string]string)
	for _, f := range fields[1:] {
		p := strings.SplitN(f, "=", 2)
		if len(p) != 2 || (p[0] != "rbps" && p[0] != "wbps") || want[p[0]] != "" {
			return ""
		}
		want[p[0]] = p[1]
	}
	if len(want) != 2 || want["rbps"] == "" || want["wbps"] == "" {
		return ""
	}
	return fields[0] + " rbps=" + want["rbps"] + " wbps=" + want["wbps"]
}

// Start atomically places cmd in this leaf via clone3 cgroup-FD.
func (l *Leaf) Start(cmd *exec.Cmd) error {
	if l == nil || cmd == nil || cmd.SysProcAttr == nil {
		return ErrUnavailable
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.done || l.dir == nil {
		return ErrUnavailable
	}
	cmd.SysProcAttr.UseCgroupFD = true
	cmd.SysProcAttr.CgroupFD = int(l.dir.Fd())
	if err := cmd.Start(); err != nil {
		return ErrUnavailable
	}
	return nil
}

// Close kills descendants, observes cgroup.events until empty, then removes
// precisely its original directory entry. It is idempotent and fail-closed.
func (l *Leaf) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.done {
		return nil
	}
	if l.dir == nil || l.root == nil {
		return ErrUnavailable
	}
	dirFD := int(l.dir.Fd())
	if !writeKill(dirFD) || !waitEmpty(dirFD, cleanupWait) {
		return ErrUnavailable
	}
	entryFD, err := unix.Openat2(int(l.root.Fd()), l.name, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_XDEV})
	if err != nil {
		return ErrUnavailable
	}
	var entry unix.Stat_t
	err = unix.Fstat(entryFD, &entry)
	_ = unix.Close(entryFD)
	if err != nil || entry.Dev != l.dev || entry.Ino != l.ino {
		return ErrUnavailable
	}
	removeErr := unix.Unlinkat(int(l.root.Fd()), l.name, unix.AT_REMOVEDIR)
	closeErr := l.dir.Close()
	if removeErr != nil && removeErr != unix.ENOENT {
		return ErrUnavailable
	}
	if closeErr != nil {
		return ErrUnavailable
	}
	l.done = true
	if l.manager != nil {
		l.manager.mu.Lock()
		if l.manager.leaves > 0 {
			l.manager.leaves--
		}
		l.manager.mu.Unlock()
	}
	return nil
}
func writeKill(fd int) bool {
	killFD, err := openControl(fd, "cgroup.kill", unix.O_WRONLY)
	if err != nil {
		return false
	}
	n, err := unix.Write(killFD, []byte("1"))
	closeErr := unix.Close(killFD)
	return err == nil && n == 1 && closeErr == nil
}
func waitEmpty(fd int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		events, err := readControl(fd, "cgroup.events")
		if err == nil && eventsPopulatedZero(events) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}
func eventsPopulatedZero(v string) bool {
	for _, line := range strings.Split(v, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "populated" && fields[1] == "0" {
			return true
		}
	}
	return false
}

// Close releases the pinned cgroup root after all leafs have been closed.
func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	if m.leaves != 0 {
		return ErrUnavailable
	}
	m.closed = true
	if m.root == nil {
		return nil
	}
	if err := m.root.Close(); err != nil {
		return ErrUnavailable
	}
	return nil
}
