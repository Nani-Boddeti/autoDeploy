//go:build linux

package checkoutcontain

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const (
	spawnFrameSize = 2048
	spawnReplySize = 16
	clientDeadline = 5 * time.Second
)

// Exported only for the root service; callers use SpawnClient.
func SpawnFrameSizeForService() int { return spawnFrameSize }
func DecodeSpawnRequestForService(b []byte) (GitOperation, string, string, SpawnLimits, int, error) {
	return decodeSpawnRequest(b)
}

func (c SpawnClient) Run(ctx context.Context, r SpawnRequest) (SpawnResult, error) {
	owned := []int{r.WorkspaceFD, r.StdoutFD, r.StderrFD, r.StdoutReadFD, r.StderrReadFD, r.AskpassReadFD}
	defer closeOwnedFDs(owned)
	if !validSpawnRequest(r) || !validClientFDLayout(r) || c.MaxOutput == 0 || c.MaxOutput > 1<<20 || r.Limits.MaxOutput > c.MaxOutput {
		return SpawnResult{}, ErrSpawnInvalid
	}
	fd, err := c.connect(ctx)
	if err != nil {
		return SpawnResult{}, err
	}
	defer unix.Close(fd)
	frame := encodeSpawnRequest(r)
	fds := []int{r.WorkspaceFD, r.StdoutFD, r.StderrFD}
	if r.AskpassReadFD >= 0 {
		fds = append(fds, r.AskpassReadFD)
	}
	if n, e := unix.SendmsgN(fd, frame[:], unix.UnixRights(fds...), nil, 0); e != nil || n != len(frame) {
		return SpawnResult{}, ErrUnavailable
	}
	// SCM_RIGHTS duplicates descriptors. Close our write ends immediately so
	// the readers can observe EOF once the service/child releases theirs.
	_ = unix.Close(r.StdoutFD)
	_ = unix.Close(r.StderrFD)
	_ = unix.Close(r.WorkspaceFD)
	owned[0], owned[1], owned[2] = -1, -1, -1
	if r.AskpassReadFD >= 0 {
		_ = unix.Close(r.AskpassReadFD)
		owned[5] = -1
	}
	responseTV := unix.NsecToTimeval(responseWaitTimeout(r.Limits).Nanoseconds())
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &responseTV); err != nil {
		return SpawnResult{}, ErrUnavailable
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Shutdown is deliberately the only concurrent operation on the protocol
	// socket: the reply goroutine remains its sole reader.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-runCtx.Done():
			_ = unix.Shutdown(fd, unix.SHUT_RDWR)
		case <-done:
		}
	}()
	var wg sync.WaitGroup
	drainErr := make(chan error, 2)
	budget := &outputBudget{remaining: int64(r.Limits.MaxOutput)}
	for _, pair := range []struct {
		fd int
		w  io.Writer
	}{{r.StdoutReadFD, r.Stdout}, {r.StderrReadFD, r.Stderr}} {
		p := pair
		if p.fd == r.StdoutReadFD {
			owned[3] = -1
		} else {
			owned[4] = -1
		}
		wg.Add(1)
		go func() { defer wg.Done(); drainErr <- drainOutput(runCtx, p.fd, p.w, budget) }()
	}
	response := make(chan struct {
		result SpawnResult
		err    error
	}, 1)
	go func() {
		result, e := receiveSpawnReply(runCtx, fd)
		response <- struct {
			result SpawnResult
			err    error
		}{result, e}
	}()
	var reply SpawnResult
	var responseErr error
	gotResponse := false
	for pending := 2; pending > 0 || !gotResponse; {
		select {
		case e := <-drainErr:
			pending--
			if e != nil {
				cancel()
				responseErr = ErrUnavailable
			}
		case got := <-response:
			reply, responseErr, gotResponse = got.result, got.err, true
			// Output can still be buffered after a reply; wait for both drains.
			for pending > 0 {
				e := <-drainErr
				pending--
				if e != nil {
					responseErr = ErrUnavailable
				}
			}
		}
		if responseErr != nil {
			cancel()
			break
		}
	}
	wg.Wait()
	if responseErr != nil {
		return SpawnResult{Class: ExitIndeterminate}, ErrUnavailable
	}
	return reply, nil
}

func closeOwnedFDs(fds []int) {
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

func responseWaitTimeout(l SpawnLimits) time.Duration {
	const cleanupMargin = 20 * time.Second // three leaf-close attempts plus retry delay.
	if l.TimeoutSeconds == 0 || l.TimeoutSeconds > 300 {
		return clientDeadline + cleanupMargin
	}
	return time.Duration(l.TimeoutSeconds)*time.Second + cleanupMargin
}

func validClientFDLayout(r SpawnRequest) bool {
	fds := []int{r.WorkspaceFD, r.StdoutFD, r.StderrFD, r.StdoutReadFD, r.StderrReadFD}
	if r.AskpassReadFD >= 0 {
		fds = append(fds, r.AskpassReadFD)
	}
	return uniqueFDs(fds) && validDirectoryFD(r.WorkspaceFD) && validWriteFD(r.StdoutFD) && validWriteFD(r.StderrFD) && validReadFD(r.StdoutReadFD) && validReadFD(r.StderrReadFD) && (r.AskpassReadFD < 0 || validReadOnlyFD(r.AskpassReadFD))
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
func validReadFD(fd int) bool {
	mode, ok := fdMode(fd)
	return ok && (mode == unix.O_RDONLY || mode == unix.O_RDWR)
}
func validReadOnlyFD(fd int) bool { mode, ok := fdMode(fd); return ok && mode == unix.O_RDONLY }
func validDirectoryFD(fd int) bool {
	var st unix.Stat_t
	return unix.Fstat(fd, &st) == nil && st.Mode&unix.S_IFMT == unix.S_IFDIR
}

type outputBudget struct {
	mu        sync.Mutex
	remaining int64
}

func (b *outputBudget) take(n int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if int64(n) > b.remaining {
		return false
	}
	b.remaining -= int64(n)
	return true
}

func drainOutput(ctx context.Context, fd int, w io.Writer, budget *outputBudget) error {
	if w == nil || budget == nil {
		_ = unix.Close(fd)
		return errors.New("output")
	}
	f := os.NewFile(uintptr(fd), "checkout-output")
	defer f.Close()
	buf := make([]byte, 32*1024)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		ready := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN | unix.POLLHUP | unix.POLLERR}}
		if _, err := unix.Poll(ready, 100); err != nil {
			return err
		}
		if ready[0].Revents == 0 {
			continue
		}
		m, e := f.Read(buf)
		if m > 0 {
			if !budget.take(m) {
				return errors.New("output limit")
			}
			if wn, we := w.Write(buf[:m]); we != nil || wn != m {
				return errors.New("output writer")
			}
		}
		if e == io.EOF {
			return nil
		}
		if e != nil {
			return e
		}
	}
}

func receiveSpawnReply(ctx context.Context, fd int) (SpawnResult, error) {
	if ctx.Err() != nil {
		return SpawnResult{}, ErrUnavailable
	}
	b := make([]byte, spawnReplySize)
	oob := make([]byte, unix.CmsgSpace(4))
	n, on, flags, _, err := unix.Recvmsg(fd, b, oob, unix.MSG_CMSG_CLOEXEC)
	// Parse and close ancillary data before rejecting it, including malformed
	// replies, so a malicious service cannot leak client descriptors.
	if on > 0 {
		closeReplyFDs(oob[:on])
	}
	if err != nil || n != len(b) || on != 0 || flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 {
		return SpawnResult{}, ErrUnavailable
	}
	status, code := binary.BigEndian.Uint32(b[:4]), binary.BigEndian.Uint32(b[4:8])
	if code > 255 || binary.BigEndian.Uint32(b[8:]) != 0 || binary.BigEndian.Uint32(b[12:]) != 0 {
		return SpawnResult{}, ErrSpawnInvalid
	}
	switch status {
	case 0:
		if code != 0 {
			return SpawnResult{}, ErrSpawnInvalid
		}
		return SpawnResult{0, ExitSuccess}, nil
	case 1:
		if code == 0 {
			return SpawnResult{}, ErrSpawnInvalid
		}
		return SpawnResult{int(code), ExitFailed}, nil
	case 2:
		if code == 0 {
			return SpawnResult{}, ErrSpawnInvalid
		}
		return SpawnResult{int(code), ExitTimedOut}, nil
	case 3:
		if code != 0 {
			return SpawnResult{}, ErrSpawnInvalid
		}
		return SpawnResult{}, ErrSpawnDenied
	default:
		return SpawnResult{}, ErrSpawnInvalid
	}
}

func closeReplyFDs(oob []byte) {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return
	}
	for _, msg := range msgs {
		fds, err := unix.ParseUnixRights(&msg)
		if err != nil {
			continue
		}
		for _, received := range fds {
			_ = unix.Close(received)
		}
	}
}

func validSpawnRequest(r SpawnRequest) bool {
	if r.WorkspaceFD < 0 || r.StdoutFD < 0 || r.StderrFD < 0 || r.StdoutReadFD < 0 || r.StderrReadFD < 0 || r.Stdout == nil || r.Stderr == nil || r.Limits.TimeoutSeconds == 0 || r.Limits.TimeoutSeconds > 300 || r.Limits.MaxOutput == 0 || r.Limits.MaxOutput > 1<<20 {
		return false
	}
	if len(r.Arg) > 96 || len(r.RemoteURL) > 1800 || strings.ContainsAny(r.Arg+r.RemoteURL, "\x00\r\n") {
		return false
	}
	return ValidSpawnOperation(r.Operation, r.Arg, r.RemoteURL, r.AskpassReadFD >= 0)
}

// ValidSpawnOperation validates all caller-controlled typed operation fields.
// The root service calls this again after decoding the untrusted wire frame.
func ValidSpawnOperation(op GitOperation, arg, remote string, hasAskpass bool) bool {
	if len(arg) > 96 || len(remote) > 1800 || strings.ContainsAny(arg+remote, "\x00\r\n") {
		return false
	}
	switch op {
	case GitInit, GitRevParseFetchHead, GitRevParseHead, GitCheckoutFetchHead, GitLSTree, GitGrepLFS:
		return arg == "" && remote == "" && !hasAskpass
	case GitRemoteAddOrigin:
		return arg == "" && validGitHubURL(remote) && !hasAskpass
	case GitFetchExact:
		return len(arg) == 40 && isLowerHex(arg) && remote == "" && hasAskpass
	}
	return false
}
func validGitHubURL(s string) bool {
	u, err := url.ParseRequestURI(s)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.EscapedPath() != u.Path {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) != 2 || !strings.HasSuffix(parts[1], ".git") {
		return false
	}
	return validGitHubSegment(parts[0]) && validGitHubSegment(strings.TrimSuffix(parts[1], ".git"))
}
func validGitHubSegment(s string) bool {
	if len(s) == 0 || len(s) > 100 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}
func isLowerHex(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func encodeSpawnRequest(r SpawnRequest) [spawnFrameSize]byte {
	var b [spawnFrameSize]byte
	b[0] = 1
	b[1] = byte(r.Operation)
	b[2] = 3
	if r.AskpassReadFD >= 0 {
		b[2] = 4
	}
	binary.BigEndian.PutUint32(b[4:], r.Limits.TimeoutSeconds)
	binary.BigEndian.PutUint32(b[8:], r.Limits.MaxOutput)
	binary.BigEndian.PutUint16(b[12:], uint16(len(r.Arg)))
	binary.BigEndian.PutUint16(b[14:], uint16(len(r.RemoteURL)))
	copy(b[16:], r.Arg)
	copy(b[16+len(r.Arg):], r.RemoteURL)
	return b
}
func decodeSpawnRequest(b []byte) (GitOperation, string, string, SpawnLimits, int, error) {
	if len(b) != spawnFrameSize || b[0] != 1 || (b[2] != 3 && b[2] != 4) {
		return 0, "", "", SpawnLimits{}, 0, errors.New("frame")
	}
	al, ul := int(binary.BigEndian.Uint16(b[12:])), int(binary.BigEndian.Uint16(b[14:]))
	if al > 96 || ul > 1800 || 16+al+ul > len(b) {
		return 0, "", "", SpawnLimits{}, 0, errors.New("length")
	}
	for _, v := range b[16+al+ul:] {
		if v != 0 {
			return 0, "", "", SpawnLimits{}, 0, errors.New("padding")
		}
	}
	return GitOperation(b[1]), string(b[16 : 16+al]), string(b[16+al : 16+al+ul]), SpawnLimits{binary.BigEndian.Uint32(b[4:]), binary.BigEndian.Uint32(b[8:])}, int(b[2]), nil
}
func (c SpawnClient) connect(ctx context.Context) (int, error) {
	if ctx == nil || ctx.Err() != nil || c.SocketPath == "" || !filepath.IsAbs(c.SocketPath) || !trustedSpawnSocket(c.SocketPath, c.AgentGID) {
		return -1, ErrUnavailable
	}
	fd, e := unix.Socket(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if e != nil {
		return -1, ErrUnavailable
	}
	tv := unix.NsecToTimeval(clientDeadline.Nanoseconds())
	if unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv) != nil || unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_SNDTIMEO, &tv) != nil || unix.Connect(fd, &unix.SockaddrUnix{Name: c.SocketPath}) != nil {
		unix.Close(fd)
		return -1, ErrUnavailable
	}
	cred, e := unix.GetsockoptUcred(fd, unix.SOL_SOCKET, unix.SO_PEERCRED)
	if e != nil || cred.Uid != 0 {
		unix.Close(fd)
		return -1, ErrSpawnDenied
	}
	return fd, nil
}
func trustedSpawnSocket(path string, gid uint32) bool {
	for p := filepath.Dir(path); ; p = filepath.Dir(p) {
		i, e := os.Lstat(p)
		if e != nil || !i.IsDir() || !rootOwnedSpawn(i) || i.Mode().Perm()&0o022 != 0 {
			return false
		}
		if p == "/" {
			break
		}
	}
	i, e := os.Lstat(path)
	if e != nil || i.Mode()&os.ModeSocket == 0 || i.Mode().Perm() != 0o620 {
		return false
	}
	st, ok := i.Sys().(*unix.Stat_t)
	return ok && st.Uid == 0 && st.Gid == gid
}
func rootOwnedSpawn(i os.FileInfo) bool { st, ok := i.Sys().(*unix.Stat_t); return ok && st.Uid == 0 }
