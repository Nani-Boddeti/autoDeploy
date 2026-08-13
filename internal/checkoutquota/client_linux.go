//go:build linux

package checkoutquota

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

const clientDeadline = 5 * time.Second

func (c Client) Allocate(ctx context.Context, limits Limits) (Allocation, error) {
	fd, err := c.connect(ctx)
	if err != nil {
		return Allocation{}, err
	}
	defer unix.Close(fd)
	var req [40]byte
	req[0] = 1
	binary.BigEndian.PutUint64(req[8:], limits.Bytes)
	binary.BigEndian.PutUint64(req[16:], limits.Inodes)
	if err := sendFixed(fd, req[:], nil); err != nil {
		return Allocation{}, err
	}
	handle, workspace, err := receiveReply(fd, true)
	if err != nil {
		return Allocation{}, err
	}
	defer func() {
		if workspace >= 0 {
			_ = unix.Close(workspace)
		}
	}()
	var ack [40]byte
	ack[0] = 3
	copy(ack[24:], handle[:])
	if err := sendFixed(fd, ack[:], nil); err != nil {
		return Allocation{}, err
	}
	_, extra, err := receiveReply(fd, false)
	if extra >= 0 {
		_ = unix.Close(extra)
		return Allocation{}, ErrInvalid
	}
	if err != nil {
		result := Allocation{Workspace: workspace, Handle: handle}
		workspace = -1
		for attempt := 0; attempt < 2; attempt++ {
			resolved, resolveErr := c.ResolveAllocation(ctx, result)
			if resolveErr == nil {
				return resolved, nil
			}
			if errors.Is(resolveErr, ErrDenied) {
				_ = unix.Close(result.Workspace)
				return Allocation{}, resolveErr
			}
			time.Sleep(time.Duration(attempt+1) * 25 * time.Millisecond)
		}
		return Allocation{}, &IndeterminateAllocationError{Allocation: result, Err: err}
	}
	result := Allocation{Workspace: workspace, Handle: handle}
	workspace = -1
	return result, nil
}

// ResolveAllocation determines whether an ACK-loss allocation became active.
// It never creates another workspace and retains the supplied capability.
func (c Client) ResolveAllocation(ctx context.Context, a Allocation) (Allocation, error) {
	if a.Workspace < 0 || a.Handle == [16]byte{} {
		return Allocation{}, ErrInvalid
	}
	fd, err := c.connect(ctx)
	if err != nil {
		return Allocation{}, err
	}
	defer unix.Close(fd)
	var req [40]byte
	req[0] = 3
	copy(req[24:], a.Handle[:])
	if err = sendFixed(fd, req[:], nil); err != nil {
		return Allocation{}, err
	}
	_, received, err := receiveReply(fd, false)
	if received >= 0 {
		_ = unix.Close(received)
		return Allocation{}, ErrInvalid
	}
	if err != nil {
		return Allocation{}, err
	}
	return a, nil
}

// ReleaseAndCleanup asks the helper to revoke access, delete only through its
// pinned descriptor, then retire quota accounting. Repeating a completed
// request is intentionally treated as success by the protocol client.
func (c Client) ReleaseAndCleanup(ctx context.Context, a Allocation) error {
	if a.Workspace < 0 || a.Handle == [16]byte{} {
		return ErrInvalid
	}
	fd, err := c.connect(ctx)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	var req [40]byte
	req[0] = 4
	copy(req[24:], a.Handle[:])
	if err := sendFixed(fd, req[:], []int{a.Workspace}); err != nil {
		return err
	}
	_, received, err := receiveReply(fd, false)
	if received >= 0 {
		_ = unix.Close(received)
		return ErrInvalid
	}
	if errors.Is(err, ErrCompleted) {
		return nil
	}
	if !errors.Is(err, ErrRetryable) {
		return err
	}
	// Releasing is durable; bounded reconnect retries let cleanup make progress.
	for attempt := 0; attempt < 2; attempt++ {
		if ctx.Err() != nil {
			return ErrUnavailable
		}
		time.Sleep(time.Duration(attempt+1) * 25 * time.Millisecond)
		retryFD, retryErr := c.connect(ctx)
		if retryErr != nil {
			return retryErr
		}
		err = sendFixed(retryFD, req[:], []int{a.Workspace})
		if err == nil {
			_, received, err = receiveReply(retryFD, false)
			if received >= 0 {
				_ = unix.Close(received)
				err = ErrInvalid
			}
		}
		unix.Close(retryFD)
		if errors.Is(err, ErrCompleted) || err == nil {
			return nil
		}
		if !errors.Is(err, ErrRetryable) {
			return err
		}
	}
	return ErrRetryable
}

func (c Client) connect(ctx context.Context) (int, error) {
	if ctx == nil || c.SocketPath == "" || c.AgentGID == 0 || !filepath.IsAbs(c.SocketPath) || ctx.Err() != nil || !trustedSocket(c.SocketPath, c.AgentGID) {
		return -1, ErrUnavailable
	}
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return -1, ErrUnavailable
	}
	tv := unix.NsecToTimeval(clientDeadline.Nanoseconds())
	if unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv) != nil || unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_SNDTIMEO, &tv) != nil || unix.Connect(fd, &unix.SockaddrUnix{Name: c.SocketPath}) != nil {
		unix.Close(fd)
		return -1, ErrUnavailable
	}
	cred, err := unix.GetsockoptUcred(fd, unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil || cred.Uid != 0 {
		unix.Close(fd)
		return -1, ErrDenied
	}
	return fd, nil
}

func trustedSocket(path string, agentGID uint32) bool {
	for parent := filepath.Dir(path); ; parent = filepath.Dir(parent) {
		info, err := os.Lstat(parent)
		if err != nil || !info.IsDir() || !rootOwned(info) || info.Mode().Perm()&0o022 != 0 {
			return false
		}
		if parent == "/" {
			break
		}
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o620 {
		return false
	}
	st, ok := info.Sys().(*unix.Stat_t)
	return ok && st.Uid == 0 && st.Gid == agentGID
}

func sendFixed(fd int, body []byte, fds []int) error {
	if len(body) != 40 {
		return ErrInvalid
	}
	var oob []byte
	if len(fds) == 1 {
		oob = unix.UnixRights(fds[0])
	} else if len(fds) != 0 {
		return ErrInvalid
	}
	n, err := unix.SendmsgN(fd, body, oob, nil, 0)
	if err != nil || n != len(body) {
		return ErrUnavailable
	}
	return nil
}

func receiveReply(fd int, wantFD bool) ([16]byte, int, error) {
	var out [16]byte
	body, oob := make([]byte, 24), make([]byte, unix.CmsgSpace(4))
	n, on, flags, _, err := unix.Recvmsg(fd, body, oob, unix.MSG_CMSG_CLOEXEC)
	got := -1
	// Inspect ancillary data before framing validation so malformed replies cannot
	// leak an installed descriptor. Invalid ancillary lengths cannot name data.
	if on > 0 && on <= len(oob) {
		var fdErr error
		got, fdErr = oneReceivedFD(oob[:on])
		if fdErr != nil {
			return out, -1, ErrInvalid
		}
	}
	if err != nil || n != len(body) || on < 0 || on > len(oob) || flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 {
		if got >= 0 {
			_ = unix.Close(got)
		}
		return out, -1, ErrUnavailable
	}
	status := binary.BigEndian.Uint32(body[:4])
	if status > wireRetryable {
		if got >= 0 {
			_ = unix.Close(got)
		}
		return out, -1, ErrInvalid
	}
	// Only a successful allocation reply may carry a descriptor. A descriptor
	// on any terminal/error status is a protocol violation, not that status.
	if status != wireSuccess && got >= 0 {
		_ = unix.Close(got)
		return out, -1, ErrInvalid
	}
	if status == wireCompleted {
		return out, -1, ErrCompleted
	}
	if status == wireDenied {
		return out, -1, ErrDenied
	}
	if status == wireRetryable {
		return out, -1, ErrRetryable
	}
	if (wantFD && got < 0) || (!wantFD && got >= 0) {
		if got >= 0 {
			_ = unix.Close(got)
		}
		return out, -1, ErrInvalid
	}
	copy(out[:], body[4:])
	return out, got, nil
}
