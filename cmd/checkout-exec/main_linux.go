//go:build linux

// checkout-exec is a root-owned local checkout spawn service. Its child mode is
// internal: it receives only already-validated descriptors from the service.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"autodeploy/internal/checkoutcontain"
	"golang.org/x/sys/unix"
)

const (
	execFD    = 3
	cwdFD     = 4
	askpassFD = 5
	maxFrame  = 4096
	maxOutput = 1 << 20
)

type config struct {
	socket, binRoot, gitName, cgroupRoot         string
	agentUID, agentGID, checkoutUID, checkoutGID uint
	memory, pids, io, cpuQuota, cpuPeriod        uint64
	major, minor                                 uint
	timeout                                      time.Duration
	maxOutput                                    uint
}
type service struct {
	cfg                  config
	git                  *os.File
	manager              *checkoutcontain.Manager
	sem                  chan struct{}
	ctx                  context.Context
	wg                   sync.WaitGroup
	listener             int
	listenerOnce         sync.Once
	parent               int
	socketName           string
	socketDev, socketIno uint64
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--checkout-child" {
		child()
		return
	}
	c := config{}
	fs := flag.NewFlagSet("checkout-exec", flag.ExitOnError)
	fs.StringVar(&c.socket, "socket", "", "root-owned SOCK_SEQPACKET path")
	fs.StringVar(&c.binRoot, "bin-root", "", "trusted binary directory")
	fs.StringVar(&c.gitName, "git-name", "git", "git executable basename")
	fs.StringVar(&c.cgroupRoot, "cgroup-root", "", "delegated cgroup root")
	fs.UintVar(&c.agentUID, "agent-uid", 0, "agent uid")
	fs.UintVar(&c.agentGID, "agent-gid", 0, "agent primary gid")
	fs.UintVar(&c.checkoutUID, "checkout-uid", 0, "checkout uid")
	fs.UintVar(&c.checkoutGID, "checkout-gid", 0, "checkout gid")
	fs.DurationVar(&c.timeout, "max-duration", 2*time.Minute, "maximum git execution duration")
	fs.UintVar(&c.maxOutput, "max-output", maxOutput, "maximum combined stdout/stderr bytes")
	fs.Uint64Var(&c.memory, "memory-max", 0, "memory bytes")
	fs.Uint64Var(&c.pids, "pids-max", 0, "pids")
	fs.Uint64Var(&c.io, "io-max", 0, "io bytes/s")
	fs.Uint64Var(&c.cpuQuota, "cpu-quota", 0, "cpu quota")
	fs.Uint64Var(&c.cpuPeriod, "cpu-period", 0, "cpu period")
	fs.UintVar(&c.major, "device-major", 0, "block major")
	fs.UintVar(&c.minor, "device-minor", 0, "block minor")
	_ = fs.Parse(os.Args[1:])
	s, err := newService(c)
	if err != nil {
		os.Exit(126)
	}
	defer s.close()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	_ = s.serve(ctx) // Serve drains every handler before returning.
}
func newService(c config) (*service, error) {
	if !validConfig(c) {
		return nil, errors.New("invalid")
	}
	bin, err := checkoutcontain.OpenTrustedDirectory(c.binRoot, 0)
	if err != nil {
		return nil, err
	}
	git, err := bin.OpenExecutable(c.gitName)
	_ = bin.Close()
	if err != nil {
		return nil, err
	}
	m, err := checkoutcontain.New(c.cgroupRoot, checkoutcontain.Limits{MemoryMax: c.memory, PIDsMax: c.pids, IOMax: c.io, CPUQuota: c.cpuQuota, CPUPeriod: c.cpuPeriod, DeviceMajor: uint32(c.major), DeviceMinor: uint32(c.minor)})
	if err != nil {
		_ = git.Close()
		return nil, err
	}
	if !validSelfExecutable() || unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) != nil {
		_ = git.Close()
		_ = m.Close()
		return nil, errors.New("unsafe daemon")
	}
	nnp, e := unix.PrctlRetInt(unix.PR_GET_NO_NEW_PRIVS, 0, 0, 0, 0)
	if e != nil || nnp != 1 {
		_ = git.Close()
		_ = m.Close()
		return nil, errors.New("unsafe daemon")
	}
	return &service{cfg: c, git: git, manager: m, sem: make(chan struct{}, 8)}, nil
}
func validConfig(c config) bool {
	return os.Geteuid() == 0 && c.agentUID != 0 && c.agentGID != 0 && c.checkoutUID != 0 && c.checkoutGID != 0 && c.agentUID != c.checkoutUID && c.agentGID != c.checkoutGID && c.timeout > 0 && c.timeout <= 10*time.Minute && c.maxOutput > 0 && c.maxOutput <= maxOutput && filepath.IsAbs(c.socket) && !strings.ContainsAny(c.socket, "\x00\r\n")
}
func validSelfExecutable() bool {
	fd, err := unix.Open("/proc/self/exe", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return false
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Uid != 0 || st.Mode&0o6022 != 0 {
		return false
	}
	b := make([]byte, 1)
	_, err = unix.Fgetxattr(fd, "security.capability", b)
	return err == unix.ENODATA
}
func (s *service) close() {
	if s.git != nil {
		_ = s.git.Close()
	}
	if s.manager != nil {
		_ = s.manager.Close()
	}
}
func (s *service) serve(ctx context.Context) error {
	parent, name, err := openSocketParent(s.cfg.socket)
	if err != nil {
		return err
	}
	s.parent, s.socketName, s.ctx = parent, name, ctx
	defer unix.Close(parent)
	if err := unix.Fstatat(parent, name, &unix.Stat_t{}, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		return errors.New("socket exists")
	}
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	s.listener = fd
	defer s.closeListener()
	if err = unix.Bind(fd, &unix.SockaddrUnix{Name: s.cfg.socket}); err != nil {
		return err
	}
	var st unix.Stat_t
	if unix.Fstatat(parent, name, &st, unix.AT_SYMLINK_NOFOLLOW) != nil {
		return errors.New("socket identity")
	}
	s.socketDev, s.socketIno = st.Dev, st.Ino
	go func() { <-ctx.Done(); s.closeListener() }()
	if err = os.Chmod(s.cfg.socket, 0o620); err != nil {
		return err
	}
	if err = unix.Chown(s.cfg.socket, 0, int(s.cfg.agentGID)); err != nil {
		return err
	}
	if err = unix.Listen(fd, 8); err != nil {
		return err
	}
	for {
		c, _, err := unix.Accept4(fd, unix.SOCK_CLOEXEC)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			return err
		}
		select {
		case s.sem <- struct{}{}:
			s.wg.Add(1)
			go func() { defer func() { <-s.sem; s.wg.Done() }(); s.handle(c) }()
		default:
			_ = unix.Close(c)
		}
	}
	s.wg.Wait() // command contexts are bounded; never abandon leaf cleanup.
	return nil
}
func (s *service) closeListener() {
	s.listenerOnce.Do(func() {
		if s.listener >= 0 {
			_ = unix.Close(s.listener)
			s.listener = -1
		}
		s.cleanupSocket()
	})
}
func openSocketParent(path string) (int, string, error) {
	if !filepath.IsAbs(path) || filepath.Base(path) == "." {
		return -1, "", errors.New("socket")
	}
	if err := safeSocketParent(path); err != nil {
		return -1, "", err
	}
	fd, err := unix.Openat2(unix.AT_FDCWD, filepath.Dir(path), &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW, Resolve: unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_XDEV})
	if err != nil {
		return -1, "", err
	}
	return fd, filepath.Base(path), nil
}
func (s *service) cleanupSocket() {
	if s.parent < 0 || s.socketName == "" {
		return
	}
	var st unix.Stat_t
	if unix.Fstatat(s.parent, s.socketName, &st, unix.AT_SYMLINK_NOFOLLOW) == nil && st.Dev == s.socketDev && st.Ino == s.socketIno {
		_ = unix.Unlinkat(s.parent, s.socketName, 0)
	}
}
func safeSocketParent(path string) error {
	for current := filepath.Dir(path); ; current = filepath.Dir(current) {
		st, err := os.Lstat(current)
		if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm()&0o022 != 0 || st.Sys() == nil || st.Sys().(*syscall.Stat_t).Uid != 0 {
			return errors.New("unsafe socket parent")
		}
		if current == "/" {
			break
		}
	}
	return nil
}
func (s *service) handle(fd int) {
	defer unix.Close(fd)
	_ = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Sec: 5})
	cred, err := unix.GetsockoptUcred(fd, unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil || cred.Uid != uint32(s.cfg.agentUID) {
		return
	}
	body := make([]byte, checkoutcontain.SpawnFrameSizeForService())
	oob := make([]byte, unix.CmsgSpace(4*4))
	n, on, flags, _, err := unix.Recvmsg(fd, body, oob, unix.MSG_CMSG_CLOEXEC)
	fds, e := receivedFDs(oob[:max(0, on)])
	if err != nil || flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 || e != nil {
		closeFDs(fds)
		return
	}
	op, arg, remote, limits, fdCount, e := checkoutcontain.DecodeSpawnRequestForService(body[:n])
	if e != nil || len(fds) != fdCount || !uniqueFDs(fds) {
		closeFDs(fds)
		return
	}
	if !checkoutcontain.ValidSpawnOperation(op, arg, remote, len(fds) == 4) {
		closeFDs(fds)
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	stopPeer := make(chan struct{})
	go watchPeerDisconnect(ctx, fd, cancel, stopPeer)
	code, timedOut := s.spawn(ctx, op, arg, remote, limits, fds)
	close(stopPeer)
	cancel()
	reply := make([]byte, 16)
	if timedOut {
		reply[3], reply[7] = 2, byte(code)
	} else if code != 0 {
		reply[3], reply[7] = 1, byte(code)
	}
	_ = unix.Sendmsg(fd, reply, nil, nil, 0)
}
func receivedFDs(oob []byte) ([]int, error) {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return nil, err
	}
	var out []int
	for _, m := range msgs {
		f, e := unix.ParseUnixRights(&m)
		if e != nil {
			return out, e
		}
		out = append(out, f...)
	}
	return out, nil
}
func closeFDs(fds []int) {
	seen := make(map[int]struct{}, len(fds))
	for _, fd := range fds {
		if fd >= 0 {
			if _, ok := seen[fd]; ok {
				continue
			}
			seen[fd] = struct{}{}
			_ = unix.Close(fd)
		}
	}
}

// The protocol has exactly one request frame.  Polling for HUP never consumes
// bytes, so it cannot race the initial Recvmsg or corrupt framing.
func watchPeerDisconnect(ctx context.Context, fd int, cancel context.CancelFunc, stop <-chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			cancel()
			return
		case <-stop:
			return
		default:
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLHUP | unix.POLLERR | unix.POLLRDHUP}}
		if _, err := unix.Poll(fds, 100); err != nil {
			cancel()
			return
		}
		if fds[0].Revents&(unix.POLLHUP|unix.POLLERR|unix.POLLNVAL|unix.POLLRDHUP) != 0 {
			cancel()
			return
		}
	}
}
func (s *service) spawn(parent context.Context, op checkoutcontain.GitOperation, arg, remote string, limits checkoutcontain.SpawnLimits, fds []int) (code int, timedOut bool) {
	defer closeFDs(fds)
	if limits.TimeoutSeconds == 0 || limits.TimeoutSeconds > uint32(s.cfg.timeout/time.Second) || limits.MaxOutput == 0 || uint64(limits.MaxOutput) > uint64(s.cfg.maxOutput) || !checkoutcontain.ValidSpawnOperation(op, arg, remote, len(fds) == 4) {
		return 126, false
	}
	workspace, stdout, stderr := fds[0], fds[1], fds[2]
	if !validWorkspace(workspace, uint32(s.cfg.checkoutUID), uint32(s.cfg.checkoutGID)) || !validWritePipe(stdout) || !validWritePipe(stderr) {
		return 126, false
	}
	args, fetch := serviceArgs(op, arg, remote)
	if args == nil {
		return 126, false
	}
	ask := -1
	if len(fds) == 4 {
		if !fetch {
			return 126, false
		}
		ask = fds[3]
		if !validReadOnlyPipe(ask) {
			return 126, false
		}
	} else if fetch {
		return 126, false
	}
	leaf, err := s.manager.CreateUnique("checkout")
	if err != nil {
		return 126, false
	}
	cleanup := func() bool {
		for range 3 {
			if leaf.Close() == nil {
				return true
			}
			time.Sleep(25 * time.Millisecond)
		}
		return false
	}
	cleaned := false
	defer func() {
		if !cleaned && !cleanup() {
			code = 126
		}
	}()
	exe := dup(int(s.git.Fd()))
	cwdCopy, stdoutCopy, stderrCopy := dup(workspace), dup(stdout), dup(stderr)
	// The child receives only duplicates. Keeping the received write ends open
	// in the daemon would suppress EOF at the client and deadlock pipe drains.
	_ = unix.Close(stdout)
	_ = unix.Close(stderr)
	// The deferred owner must not close an FD number after it has been released:
	// the kernel may reuse it for a child duplicate.
	fds[1], fds[2] = -1, -1
	if exe < 0 || cwdCopy < 0 || stdoutCopy < 0 || stderrCopy < 0 {
		closeRawFDs(exe, cwdCopy, stdoutCopy, stderrCopy)
		return 126, false
	}
	gitFile, cwdFile := os.NewFile(uintptr(exe), "git"), os.NewFile(uintptr(cwdCopy), "cwd")
	defer gitFile.Close()
	defer cwdFile.Close()
	stdoutFile, stderrFile := os.NewFile(uintptr(stdoutCopy), "stdout"), os.NewFile(uintptr(stderrCopy), "stderr")
	defer stdoutFile.Close()
	defer stderrFile.Close()
	files := []*os.File{os.Stdin, stdoutFile, stderrFile, gitFile, cwdFile}
	if ask >= 0 {
		askCopy := dup(ask)
		if askCopy < 0 {
			return 126, false
		}
		askFile := os.NewFile(uintptr(askCopy), "askpass")
		defer askFile.Close()
		files = append(files, askFile)
	}
	childArgs := []string{"--checkout-child", strconv.FormatUint(uint64(s.cfg.checkoutUID), 10), strconv.FormatUint(uint64(s.cfg.checkoutGID), 10)}
	childArgs = append(childArgs, args...)
	ctx, cancel := context.WithTimeout(parent, time.Duration(limits.TimeoutSeconds)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/proc/self/exe", childArgs...)
	cmd.Stdout, cmd.Stderr = stdoutFile, stderrFile
	cmd.ExtraFiles = files[3:]
	cmd.Env = fixedEnv(ask >= 0)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(s.cfg.checkoutUID), Gid: uint32(s.cfg.checkoutGID), Groups: []uint32{}, NoSetGroups: false}}
	err = leaf.Start(cmd)
	if err == nil {
		err = cmd.Wait()
	}
	if !cleanup() {
		return 125, false
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return 124, true
	}
	if ctx.Err() != nil {
		return 125, false
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if exitCode := exitErr.ExitCode(); exitCode >= 1 && exitCode <= 255 {
				return exitCode, false
			}
		}
		return 125, false
	}
	cleaned = true
	return 0, false
}
func closeRawFDs(fds ...int) {
	for _, fd := range fds {
		if fd >= 0 {
			_ = unix.Close(fd)
		}
	}
}
func serviceArgs(op checkoutcontain.GitOperation, arg, remote string) ([]string, bool) {
	if !checkoutcontain.ValidSpawnOperation(op, arg, remote, op == checkoutcontain.GitFetchExact) {
		return nil, false
	}
	switch op {
	case checkoutcontain.GitInit:
		return []string{"init", "--quiet"}, false
	case checkoutcontain.GitRemoteAddOrigin:
		return []string{"remote", "add", "origin", remote}, false
	case checkoutcontain.GitFetchExact:
		return []string{"fetch", "--no-tags", "--depth=1", "origin", arg}, true
	case checkoutcontain.GitRevParseFetchHead:
		return []string{"rev-parse", "FETCH_HEAD^{commit}"}, false
	case checkoutcontain.GitRevParseHead:
		return []string{"rev-parse", "HEAD"}, false
	case checkoutcontain.GitCheckoutFetchHead:
		return []string{"checkout", "--detach", "--force", "FETCH_HEAD"}, false
	case checkoutcontain.GitLSTree:
		return []string{"ls-tree", "-r", "HEAD"}, false
	case checkoutcontain.GitGrepLFS:
		return []string{"grep", "-I", "-l", "version https://git-lfs.github.com/spec/v1", "HEAD", "--"}, false
	}
	return nil, false
}
func dup(fd int) int {
	n, e := unix.Dup(fd)
	if e != nil {
		return -1
	}
	return n
}
func validWorkspace(fd int, uid, gid uint32) bool {
	var st unix.Stat_t
	return unix.Fstat(fd, &st) == nil && st.Mode&unix.S_IFMT == unix.S_IFDIR && st.Uid == uid && st.Gid == gid && st.Mode&0o022 == 0
}
func sameDirectory(left, right int) bool {
	var a, b unix.Stat_t
	return unix.Fstat(left, &a) == nil && unix.Fstat(right, &b) == nil && a.Dev == b.Dev && a.Ino == b.Ino && validWorkspace(right, a.Uid, a.Gid)
}
func validWritePipe(fd int) bool {
	var st unix.Stat_t
	return unix.Fstat(fd, &st) == nil && st.Mode&unix.S_IFMT == unix.S_IFIFO && validWriteFD(fd)
}
func validReadOnlyPipe(fd int) bool {
	var st unix.Stat_t
	return unix.Fstat(fd, &st) == nil && st.Mode&unix.S_IFMT == unix.S_IFIFO && validReadOnlyFD(fd)
}
func uniqueFDs(fds []int) bool {
	seen := make(map[int]struct{}, len(fds))
	for _, fd := range fds {
		if fd < 0 {
			return false
		}
		if _, ok := seen[fd]; ok {
			return false
		}
		seen[fd] = struct{}{}
	}
	return true
}
func fdMode(fd int) (int, bool) {
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	return flags & unix.O_ACCMODE, err == nil
}
func validWriteFD(fd int) bool {
	mode, ok := fdMode(fd)
	return ok && (mode == unix.O_WRONLY || mode == unix.O_RDWR)
}
func validReadOnlyFD(fd int) bool { mode, ok := fdMode(fd); return ok && mode == unix.O_RDONLY }
func fixedEnv(ask bool) []string {
	e := []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "XDG_CONFIG_HOME=/nonexistent", "XDG_CACHE_HOME=/nonexistent", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_COUNT=4", "GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0=", "GIT_CONFIG_KEY_1=core.hooksPath", "GIT_CONFIG_VALUE_1=/dev/null", "GIT_CONFIG_KEY_2=protocol.file.allow", "GIT_CONFIG_VALUE_2=never", "GIT_CONFIG_KEY_3=http.sslVerify", "GIT_CONFIG_VALUE_3=true"}
	if ask {
		e = append(e, "AUTODEPLOY_GIT_ASKPASS_FD=5", "GIT_ASKPASS=/usr/local/lib/autodeploy/github-askpass")
	}
	return e
}
func child() {
	if len(os.Args) < 4 {
		os.Exit(126)
	}
	uid, e1 := strconv.ParseUint(os.Args[2], 10, 32)
	gid, e2 := strconv.ParseUint(os.Args[3], 10, 32)
	if e1 != nil || e2 != nil || !exactIdentity(uint32(uid), uint32(gid)) || !validExecFD(execFD) || !validWorkspace(cwdFD, uint32(uid), uint32(gid)) {
		os.Exit(126)
	}
	nnp, e := unix.PrctlRetInt(unix.PR_GET_NO_NEW_PRIVS, 0, 0, 0, 0)
	if e != nil || nnp != 1 || unix.Fchdir(cwdFD) != nil {
		os.Exit(126)
	}
	unix.CloseOnExec(execFD)
	unix.CloseOnExec(cwdFD)
	if unix.CloseRange(6, ^uint(0), 0) != nil {
		os.Exit(126)
	}
	if err := execveat(execFD, append([]string{"git"}, os.Args[4:]...), os.Environ()); err != nil {
		os.Exit(126)
	}
}
func exactIdentity(uid, gid uint32) bool {
	r, e, s := unix.Getresuid()
	rg, eg, sg := unix.Getresgid()
	groups, err := unix.Getgroups()
	return err == nil && len(groups) == 0 && uint32(r) == uid && uint32(e) == uid && uint32(s) == uid && uint32(rg) == gid && uint32(eg) == gid && uint32(sg) == gid
}
func validExecFD(fd int) bool {
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Uid != 0 || st.Mode&0o022 != 0 || st.Mode&0o111 == 0 {
		return false
	}
	b := make([]byte, 20)
	n, e := unix.Pread(fd, b, 0)
	return e == nil && n == 20 && bytes.Equal(b[:4], []byte{0x7f, 'E', 'L', 'F'}) && b[5] == 1 && b[6] == 1 && b[7] == 0 && b[16] == 2
}
func execveat(fd int, args, env []string) error {
	a, e := ptrs(args)
	if e != nil {
		return e
	}
	v, e := ptrs(env)
	if e != nil {
		return e
	}
	z := []byte{0}
	_, _, errno := unix.Syscall6(unix.SYS_EXECVEAT, uintptr(fd), uintptr(unsafe.Pointer(&z[0])), uintptr(unsafe.Pointer(&a[0])), uintptr(unsafe.Pointer(&v[0])), unix.AT_EMPTY_PATH, 0)
	if errno != 0 {
		return errno
	}
	return nil
}
func ptrs(v []string) ([]*byte, error) {
	out := make([]*byte, 0, len(v)+1)
	for _, s := range v {
		p, e := unix.BytePtrFromString(s)
		if e != nil {
			return nil, e
		}
		out = append(out, p)
	}
	return append(out, nil), nil
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
