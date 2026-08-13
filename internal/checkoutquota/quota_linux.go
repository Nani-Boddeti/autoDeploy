//go:build linux

package checkoutquota

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	fsxattrGetProject  = 0x801c581f // FS_IOC_FSGETXATTR; fsxattr is 28 bytes
	fsxattrSetProject  = 0x401c5820 // FS_IOC_FSSETXATTR; fsxattr is 28 bytes
	fsXflagProjInherit = 0x00000200
	qXGetQuota         = 0x5803 // XQM_CMD(3)
	qXSetQLim          = 0x5804 // XQM_CMD(4)
	qXGetQStatV        = 0x5808 // XQM_CMD(8)
	xqmProjectQuota    = 2
	fsProjectQuota     = 2
	fsQuotaProjectAcct = 0x0010
	fsQuotaProjectEnfd = 0x0020
	fsDqBlkHard        = 0x0008
	fsDqInoHard        = 0x0002
)

type fsxattr struct {
	XFlags, ExtSize, Nextents, ProjID, CowExtSize uint32
	Pad                                           [8]byte
}
type diskQuota struct {
	Version                                                    int8
	Flags                                                      int8
	FieldMask                                                  uint16
	ID                                                         uint32
	BlockHard, BlockSoft, InodeHard, InodeSoft, Blocks, Inodes uint64
	ITimer, BTimer                                             int32
	IWarn, BWarn                                               uint16
	Pad2                                                       int32
	RTBlockHard, RTBlockSoft, RTBlocks                         uint64
	RTBTimer                                                   int32
	RTBWarn                                                    uint16
	Pad3                                                       int16
	Pad4                                                       [8]byte
}
type quotaStatV struct {
	Version uint8
	Pad1    uint8
	Flags   uint16
	Pad2    uint32
	Rest    [152]byte
}
type mountIdentity struct {
	Dev          uint64
	FSType       int64
	Fsid0, Fsid1 int32
}
type ledger struct {
	Mount       mountIdentity         `json:"mount"`
	CheckoutUID uint32                `json:"checkout_uid"`
	CheckoutGID uint32                `json:"checkout_gid"`
	Next        uint32                `json:"next"`
	Active      map[string]allocation `json:"active"`
	Terminal    map[string]terminal   `json:"terminal"`
}
type terminal struct {
	ID        uint32    `json:"id"`
	Handle    [16]byte  `json:"handle"`
	Dev       uint64    `json:"dev"`
	Ino       uint64    `json:"ino"`
	Completed time.Time `json:"completed"`
}
type allocation struct {
	ID     uint32   `json:"id"`
	Name   string   `json:"name"`
	Handle [16]byte `json:"handle"`
	Dev    uint64   `json:"dev"`
	Ino    uint64   `json:"ino"`
	Bytes  uint64   `json:"bytes"`
	Inodes uint64   `json:"inodes"`
	State  string   `json:"state"`
}

type Service struct {
	cfg       Config
	root      *os.File
	lockFD    int
	stateDir  int
	stateName string
	mount     mountIdentity
	mu        sync.Mutex
	ledger    ledger
	conn      chan struct{}
}

const (
	statePreparing = "preparing"
	stateReserved  = "reserved"
	stateReady     = "ready"
	stateOffered   = "offered"
	stateGranting  = "granting"
	stateActive    = "active"
	stateReleasing = "releasing"
)

const ackDeadlineSeconds = 5

const (
	wireSuccess uint32 = iota
	wireCompleted
	wireDenied
	wireRetryable
)

const (
	maxCleanupEntries = 100000
	maxCleanupDepth   = 64
	cleanupDeadline   = 30 * time.Second
	terminalMaxAge    = 10 * time.Minute
	terminalMaxCount  = 1024
)

// Serve accepts only SOCK_SEQPACKET requests from the configured agent UID.
// Allocate carries no FD; release carries exactly the helper-prepared FD.
func (s *Service) Serve(ctx context.Context) error {
	if s == nil || s.cfg.SocketPath == "" || !filepath.IsAbs(s.cfg.SocketPath) || s.cfg.AgentGID == 0 {
		return ErrInvalid
	}
	if err := trustedPath(filepath.Dir(s.cfg.SocketPath)); err != nil {
		return err
	}
	if err := recoverStaleSocket(s.cfg.SocketPath, s.cfg.AgentGID); err != nil {
		return ErrDenied
	}
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return ErrUnavailable
	}
	var closeListener sync.Once
	closeFD := func() { closeListener.Do(func() { _ = unix.Close(fd) }) }
	defer closeFD()
	var socketStat unix.Stat_t
	socketBound := false
	defer func() {
		var st unix.Stat_t
		if socketBound && unix.Lstat(s.cfg.SocketPath, &st) == nil && st.Ino == socketStat.Ino && st.Dev == socketStat.Dev {
			_ = os.Remove(s.cfg.SocketPath)
		}
	}()
	// Unix-domain bind honours umask. Constrain the transient inode explicitly
	// before goroutines exist, then restore the process setting immediately.
	oldUmask := unix.Umask(0o177)
	err = unix.Bind(fd, &unix.SockaddrUnix{Name: s.cfg.SocketPath})
	unix.Umask(oldUmask)
	if err != nil {
		return ErrUnavailable
	}
	// Capture identity before any later setup step; failures before chown must
	// still remove only the inode we created.
	if err = unix.Lstat(s.cfg.SocketPath, &socketStat); err != nil {
		return ErrUnavailable
	}
	socketBound = true
	// The helper identity stays root. Connect permission is delegated only to
	// the configured agent group; peers are still checked with SO_PEERCRED.
	if err = os.Chmod(s.cfg.SocketPath, 0o620); err != nil {
		return ErrUnavailable
	}
	if err = unix.Chown(s.cfg.SocketPath, 0, int(s.cfg.AgentGID)); err != nil {
		return ErrUnavailable
	}
	if err = unix.Listen(fd, 16); err != nil {
		return ErrUnavailable
	}
	if s.conn == nil {
		s.conn = make(chan struct{}, 16)
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			closeFD()
		case <-done:
		}
	}()
	defer close(done)
	for {
		conn, _, e := unix.Accept4(fd, unix.SOCK_CLOEXEC)
		if e != nil {
			if ctx.Err() != nil {
				return nil
			}
			return ErrUnavailable
		}
		select {
		case s.conn <- struct{}{}:
			go func() { defer func() { <-s.conn }(); s.serveConn(conn) }()
		default:
			_ = unix.Close(conn)
		}
	}
}
func recoverStaleSocket(path string, gid uint32) error {
	if err := trustedPath(filepath.Dir(path)); err != nil {
		return ErrDenied
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		return ErrDenied
	}
	old, ok := info.Sys().(*unix.Stat_t)
	if !ok || old.Uid != 0 || !staleSocketModeAllowed(old, gid) {
		return ErrDenied
	}
	// A successful connect proves a listener is live. Refuse to disrupt it.
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err == nil {
		err = unix.Connect(fd, &unix.SockaddrUnix{Name: path})
		_ = unix.Close(fd)
	}
	if err == nil {
		return ErrDenied
	}
	if !errors.Is(err, unix.ECONNREFUSED) && !errors.Is(err, unix.ENOENT) {
		return ErrDenied
	}
	parent := filepath.Dir(path)
	name := filepath.Base(path)
	pfd, err := unix.Open(parent, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return ErrDenied
	}
	defer unix.Close(pfd)
	var st unix.Stat_t
	if unix.Fstatat(pfd, name, &st, unix.AT_SYMLINK_NOFOLLOW) != nil || st.Uid != 0 || st.Mode&unix.S_IFMT != unix.S_IFSOCK || !staleSocketModeAllowed(&st, gid) {
		return ErrDenied
	}
	if st.Dev != old.Dev || st.Ino != old.Ino {
		return ErrDenied
	}
	if unix.Unlinkat(pfd, name, 0) != nil {
		return ErrDenied
	}
	return nil
}

// The root:root 0600 form exists only between bind and chown; root:agent 0620
// is the final listener. Both are recoverable only after listener and inode
// stability checks above.
func staleSocketModeAllowed(st *unix.Stat_t, gid uint32) bool {
	return (st.Gid == 0 && (st.Mode&0o777 == 0o600 || st.Mode&0o777 == 0o620)) || (st.Gid == gid && st.Mode&0o777 == 0o620)
}
func (s *Service) serveConn(fd int) {
	defer unix.Close(fd)
	_ = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Sec: 5})
	cred, err := unix.GetsockoptUcred(fd, unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil || cred.Uid != s.cfg.AgentUID {
		return
	}
	body := make([]byte, 40)
	oob := make([]byte, unix.CmsgSpace(4))
	n, on, flags, _, err := unix.Recvmsg(fd, body, oob, unix.MSG_CMSG_CLOEXEC)
	// Parse rights before interpreting the request. Every malformed/rejected
	// request closes every received descriptor rather than leaking it to the
	// privileged daemon.
	if on < 0 || on > len(oob) {
		return
	}
	workspace, rightsErr := oneReceivedFD(oob[:on])
	if workspace >= 0 {
		defer unix.Close(workspace)
	}
	if err != nil || flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 || n != len(body) {
		return
	}
	req, err := decodeRequest(body)
	var handle [16]byte
	var outFD int = -1
	if err == nil && rightsErr != nil && on != 0 {
		err = rightsErr
	}
	if err == nil {
		switch req.Op {
		case 1:
			if req.Handle != [16]byte{} {
				err = ErrInvalid
				break
			}
			if workspace >= 0 || on != 0 {
				err = ErrInvalid
				break
			}
			s.handleAllocateConn(fd, Limits{Bytes: req.Bytes, Inodes: req.Inodes})
			return
		case 2, 4:
			if req.Bytes != 0 || req.Inodes != 0 {
				err = ErrInvalid
			} else {
				err = s.Release(workspace, req.Handle)
			}
		case 3:
			if workspace >= 0 || on != 0 || req.Bytes != 0 || req.Inodes != 0 || req.Handle == [16]byte{} {
				err = ErrInvalid
			} else {
				err = s.activeAckStatus(req.Handle)
			}
		default:
			err = ErrInvalid
		}
	}
	reply := replyFor(handle, err)
	var control []byte
	if outFD >= 0 {
		control = unix.UnixRights(outFD)
		defer unix.Close(outFD)
	}
	if _, sendErr := unix.SendmsgN(fd, reply, control, nil, 0); sendErr != nil && outFD >= 0 {
		// An unacknowledged descriptor must never become a permanently active
		// inaccessible checkout when delivery fails.
		_ = s.withdrawOffer(outFD, handle)
	}
}

// activeAckStatus is an idempotent recovery query for a lost final ACK reply.
// It never returns a descriptor and never transitions offered/granting state.
func (s *Service) activeAckStatus(handle [16]byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.ledger.Active {
		if subtle.ConstantTimeCompare(a.Handle[:], handle[:]) == 1 && a.State == stateActive {
			return nil
		}
	}
	return ErrDenied
}

func replyFor(handle [16]byte, err error) []byte {
	reply := make([]byte, 24)
	status := wireSuccess
	switch {
	case err == nil:
		copy(reply[4:], handle[:])
	case errors.Is(err, ErrCompleted):
		status = wireCompleted
	case errors.Is(err, ErrRetryable), errors.Is(err, ErrUnavailable):
		status = wireRetryable
	case errors.Is(err, ErrDenied), errors.Is(err, ErrInvalid):
		status = wireDenied
	default:
		status = wireRetryable
	}
	binary.BigEndian.PutUint32(reply[:4], status)
	return reply
}

// handleAllocateConn implements the two-message transaction. The offered FD
// names a root-owned 000 directory; the agent must not touch it until the
// final success reply following its same-connection ACK.
func (s *Service) handleAllocateConn(conn int, limits Limits) {
	_, handle, workspace, err := s.Allocate(limits)
	if err != nil {
		s.sendReply(conn, [16]byte{}, -1, err)
		return
	}
	defer unix.Close(workspace)
	if err = s.sendReply(conn, handle, workspace, nil); err != nil {
		_ = s.withdrawOffer(workspace, handle)
		return
	}
	_ = unix.SetsockoptTimeval(conn, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Sec: ackDeadlineSeconds})
	body := make([]byte, 40)
	oob := make([]byte, unix.CmsgSpace(4))
	n, on, flags, _, recvErr := unix.Recvmsg(conn, body, oob, unix.MSG_CMSG_CLOEXEC)
	if on < 0 || on > len(oob) {
		_ = s.withdrawOffer(workspace, handle)
		return
	}
	noFDs := consumeNoFDs(oob[:on])
	req, decodeErr := decodeRequest(body)
	if recvErr != nil || flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 || n != len(body) || !noFDs || decodeErr != nil || req.Op != 3 || req.Bytes != 0 || req.Inodes != 0 || subtle.ConstantTimeCompare(req.Handle[:], handle[:]) != 1 {
		_ = s.withdrawOffer(workspace, handle)
		return
	}
	err = s.Ack(handle)
	_ = s.sendReply(conn, [16]byte{}, -1, err)
}

// consumeNoFDs accepts empty ancillary data. If any rights are supplied it
// closes every installed descriptor and rejects the frame.
func consumeNoFDs(oob []byte) bool {
	if len(oob) == 0 {
		return true
	}
	fd, _ := oneReceivedFD(oob)
	if fd >= 0 {
		_ = unix.Close(fd)
	}
	return false
}
func (s *Service) sendReply(fd int, handle [16]byte, outFD int, err error) error {
	reply := replyFor(handle, err)
	var control []byte
	if outFD >= 0 {
		control = unix.UnixRights(outFD)
	}
	_, sendErr := unix.SendmsgN(fd, reply, control, nil, 0)
	return sendErr
}
func oneReceivedFD(oob []byte) (int, error) {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return -1, ErrInvalid
	}
	var got []int
	for _, m := range msgs {
		fds, e := unix.ParseUnixRights(&m)
		if e == nil {
			got = append(got, fds...)
		}
	}
	if len(got) != 1 {
		for _, fd := range got {
			_ = unix.Close(fd)
		}
		return -1, ErrInvalid
	}
	return got[0], nil
}

func New(c Config) (*Service, error) {
	if c.CheckoutRoot == "" || !filepath.IsAbs(c.CheckoutRoot) || c.StatePath == "" || !filepath.IsAbs(c.StatePath) || filepath.Base(c.StatePath) == "/" || c.AgentUID == 0 || c.AgentGID == 0 || c.CheckoutUID == 0 || c.CheckoutGID == 0 || c.CheckoutUID == c.AgentUID || c.CheckoutGID == c.AgentGID || c.MaxBytes == 0 || c.MaxInodes == 0 || (c.Initialize && (c.InitialProjectID == 0 || c.InitialProjectID == ^uint32(0))) {
		return nil, ErrInvalid
	}
	if err := trustedPath(c.CheckoutRoot); err != nil {
		return nil, err
	}
	if err := trustedPath(filepath.Dir(c.StatePath)); err != nil {
		return nil, err
	}
	root, err := os.Open(c.CheckoutRoot)
	if err != nil {
		return nil, ErrUnavailable
	}
	info, err := root.Stat()
	if err != nil || !info.IsDir() || !rootOwned(info) || info.Mode().Perm()&0o022 != 0 {
		root.Close()
		return nil, ErrDenied
	}
	m, err := identity(int(root.Fd()))
	if err != nil || m.FSType != unix.XFS_SUPER_MAGIC {
		root.Close()
		return nil, ErrUnavailable
	}
	if err := quotasEnabled(int(root.Fd())); err != nil {
		root.Close()
		return nil, ErrUnavailable
	}
	stateDir, err := unix.Open(filepath.Dir(c.StatePath), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		root.Close()
		return nil, ErrUnavailable
	}
	stateName := filepath.Base(c.StatePath)
	lockFD, err := unix.Openat(stateDir, stateName+".lock", unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		unix.Close(stateDir)
		root.Close()
		return nil, ErrUnavailable
	}
	if err = unix.Flock(lockFD, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		unix.Close(lockFD)
		unix.Close(stateDir)
		root.Close()
		return nil, ErrDenied
	}
	l, err := loadLedgerAt(stateDir, stateName, m, c.Initialize, c.InitialProjectID, int(root.Fd()), c.CheckoutUID, c.CheckoutGID)
	if err != nil {
		unix.Close(lockFD)
		unix.Close(stateDir)
		root.Close()
		return nil, err
	}
	s := &Service{cfg: c, root: root, lockFD: lockFD, stateDir: stateDir, stateName: stateName, mount: m, ledger: l}
	// A damaged individual record never grants access. Reconciliation is
	// best-effort per record, so one uncertain checkout does not brick restart.
	if err := s.reconcile(); err != nil {
		unix.Close(lockFD)
		unix.Close(stateDir)
		root.Close()
		return nil, err
	}
	return s, nil
}
func rootOwned(i os.FileInfo) bool {
	st, ok := i.Sys().(*unix.Stat_t)
	return ok && st.Uid == 0 && st.Gid == 0
}
func trustedPath(p string) error {
	for {
		i, e := os.Lstat(p)
		if e != nil || !i.IsDir() || !rootOwned(i) || i.Mode().Perm()&0o022 != 0 {
			return ErrDenied
		}
		n := filepath.Dir(p)
		if n == p {
			return nil
		}
		p = n
	}
}

func identity(fd int) (mountIdentity, error) {
	var st unix.Stat_t
	var fs unix.Statfs_t
	if err := unix.Fstat(fd, &st); err != nil {
		return mountIdentity{}, err
	}
	if err := unix.Fstatfs(fd, &fs); err != nil {
		return mountIdentity{}, err
	}
	return mountIdentity{Dev: uint64(st.Dev), FSType: fs.Type, Fsid0: fs.Fsid.Val[0], Fsid1: fs.Fsid.Val[1]}, nil
}
func quotasEnabled(fd int) error {
	q := quotaStatV{Version: 1}
	if err := quotactlFD(fd, qXGetQStatV, 0, unsafe.Pointer(&q)); err != nil {
		return err
	}
	if q.Flags&(fsQuotaProjectAcct|fsQuotaProjectEnfd) != (fsQuotaProjectAcct | fsQuotaProjectEnfd) {
		return unix.EPERM
	}
	return nil
}

// Allocate makes limits enforceable before returning its mount-unique project ID.
func (s *Service) Allocate(l Limits) (uint32, [16]byte, int, error) {
	var handle [16]byte
	if s == nil || !s.cfg.validLimits(l) {
		return 0, handle, -1, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.ledger.Next
	if id == 0 || id == ^uint32(0) {
		return 0, handle, -1, ErrUnavailable
	}
	name := fmt.Sprintf(".checkout-%08x", id)
	if _, err := rand.Read(handle[:]); err != nil {
		return 0, handle, -1, ErrUnavailable
	}
	reserved := cloneLedger(s.ledger)
	reserved.Next = id + 1
	reserved.Active[name] = allocation{ID: id, Name: name, Handle: handle, Bytes: l.Bytes, Inodes: l.Inodes, State: stateReserved}
	if err := s.persist(reserved); err != nil {
		return 0, handle, -1, err
	}
	s.ledger = reserved
	fd, err := unix.Openat(int(s.root.Fd()), name, unix.O_DIRECTORY|unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err == nil {
		unix.Close(fd)
		return 0, handle, -1, ErrUnavailable
	}
	if err = unix.Mkdirat(int(s.root.Fd()), name, 0o000); err != nil {
		return 0, handle, -1, ErrUnavailable
	}
	fd, err = unix.Openat(int(s.root.Fd()), name, unix.O_DIRECTORY|unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return 0, handle, -1, ErrUnavailable
	}
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil {
		unix.Close(fd)
		return 0, handle, -1, ErrUnavailable
	}
	next := cloneLedger(s.ledger)
	next.Active[name] = allocation{ID: id, Name: name, Handle: handle, Dev: uint64(st.Dev), Ino: st.Ino, Bytes: l.Bytes, Inodes: l.Inodes, State: statePreparing}
	if err := s.persist(next); err != nil {
		unix.Close(fd)
		return 0, handle, -1, err
	}
	s.ledger = next
	if err := setProject(fd, id, true); err != nil {
		unix.Close(fd)
		return 0, handle, -1, ErrUnavailable
	}
	if err := setLimits(int(s.root.Fd()), int(id), l); err != nil {
		unix.Close(fd)
		return 0, handle, -1, ErrUnavailable
	}
	if err := verifyApplied(int(s.root.Fd()), fd, id, l); err != nil {
		unix.Close(fd)
		return 0, handle, -1, ErrUnavailable
	}
	ready := cloneLedger(s.ledger)
	r := ready.Active[name]
	r.State = stateReady
	ready.Active[name] = r
	if err := s.persist(ready); err != nil {
		unix.Close(fd)
		return 0, handle, -1, ErrUnavailable
	}
	s.ledger = ready
	offered := cloneLedger(s.ledger)
	active := offered.Active[name]
	active.State = stateOffered
	offered.Active[name] = active
	if err := s.persist(offered); err != nil {
		unix.Close(fd)
		return 0, handle, -1, ErrUnavailable
	}
	s.ledger = offered // only after fsync + atomic rename succeeded
	return id, handle, fd, nil
}

// Ack is deliberately handle-only: an agent may use an offered descriptor
// only after this acknowledgement has made the allocation active.
func (s *Service) Ack(handle [16]byte) error {
	if s == nil || handle == [16]byte{} {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var record *allocation
	for _, a := range s.ledger.Active {
		if subtle.ConstantTimeCompare(a.Handle[:], handle[:]) == 1 && a.State == stateOffered {
			x := a
			record = &x
		}
	}
	if record == nil {
		return ErrDenied
	}
	fd, err := unix.Openat(int(s.root.Fd()), record.Name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return ErrDenied
	}
	defer unix.Close(fd)
	if err = s.verifyRecordFD(fd, *record, false); err != nil {
		return ErrDenied
	}
	next := cloneLedger(s.ledger)
	a := next.Active[record.Name]
	a.State = stateGranting
	next.Active[record.Name] = a
	if err = s.persist(next); err != nil {
		return err
	}
	s.ledger = next
	if err = unix.Fchown(fd, int(s.cfg.CheckoutUID), int(s.cfg.CheckoutGID)); err != nil {
		_ = s.withdrawOfferLocked(fd, handle)
		return ErrUnavailable
	}
	if err = unix.Fchmod(fd, 0o700); err != nil {
		_ = s.withdrawOfferLocked(fd, handle)
		return ErrUnavailable
	}
	if err = s.verifyRecordFD(fd, *record, true); err != nil {
		_ = s.withdrawOfferLocked(fd, handle)
		return ErrDenied
	}
	active := cloneLedger(s.ledger)
	a = active.Active[record.Name]
	a.State = stateActive
	active.Active[record.Name] = a
	if err = s.persist(active); err != nil {
		_ = s.withdrawOfferLocked(fd, handle)
		return err
	}
	s.ledger = active
	return nil
}

func (s *Service) withdrawOffer(fd int, handle [16]byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.withdrawOfferLocked(fd, handle)
}
func (s *Service) withdrawOfferLocked(fd int, handle [16]byte) error {
	for name, a := range s.ledger.Active {
		if (a.State == stateOffered || a.State == stateGranting) && subtle.ConstantTimeCompare(a.Handle[:], handle[:]) == 1 {
			if err := revokeWorkspace(fd); err != nil {
				return err
			}
			next := cloneLedger(s.ledger)
			b := next.Active[name]
			b.State = stateReleasing
			next.Active[name] = b
			if err := s.persist(next); err != nil {
				return err
			}
			s.ledger = next
			return s.cleanupLocked(fd, b)
		}
	}
	return ErrDenied
}
func revokeWorkspace(fd int) error {
	if err := unix.Fchown(fd, 0, 0); err != nil {
		return ErrUnavailable
	}
	if err := unix.Fchmod(fd, 0); err != nil {
		return ErrUnavailable
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil || st.Uid != 0 || st.Mode&0o777 != 0 {
		return ErrUnavailable
	}
	return nil
}
func (s *Service) cleanupLocked(fd int, record allocation) error {
	if s.verifyWorkspaceFD(fd) != nil || s.verifyRecordIdentity(fd, record) != nil {
		return ErrDenied
	}
	if err := removeWorkspaceContents(fd, s.mount); err != nil {
		return err
	}
	if emptyDir(fd) != nil {
		return ErrRetryable
	}
	// A retry after a crash may find this stage already complete.
	if verifyProjectCleared(fd) != nil {
		if setProject(fd, 0, false) != nil || verifyProjectCleared(fd) != nil {
			return ErrUnavailable
		}
	}
	q, err := getQuota(int(s.root.Fd()), int(record.ID))
	if err != nil || q.Blocks != 0 || q.Inodes != 0 {
		return ErrRetryable
	}
	if err = clearLimits(int(s.root.Fd()), record.ID); err != nil {
		return ErrUnavailable
	}
	if err = unix.Unlinkat(int(s.root.Fd()), record.Name, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
		return ErrUnavailable
	}
	return s.completeLocked(record)
}

func (s *Service) verifyRecordIdentity(fd int, a allocation) error {
	if s.matchesRecordFD(fd, a) != nil {
		return ErrDenied
	}
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Uid != 0 || st.Mode&0o777 != 0 {
		return ErrDenied
	}
	return nil
}
func (s *Service) matchesRecordFD(fd int, a allocation) error {
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || uint64(st.Dev) != a.Dev || st.Ino != a.Ino || st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return ErrDenied
	}
	m, err := identity(fd)
	if err != nil || m != s.mount {
		return ErrDenied
	}
	return nil
}

func (s *Service) verifyRecordFD(fd int, a allocation, requireCheckoutAccess bool) error {
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || uint64(st.Dev) != a.Dev || st.Ino != a.Ino || st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return ErrDenied
	}
	m, err := identity(fd)
	if err != nil || m != s.mount {
		return ErrDenied
	}
	if requireCheckoutAccess && (st.Uid != s.cfg.CheckoutUID || st.Gid != s.cfg.CheckoutGID || st.Mode&0o777 != 0o700) {
		return ErrDenied
	}
	if !requireCheckoutAccess && (st.Uid != 0 || st.Mode&0o777 != 0) {
		return ErrDenied
	}
	if err = verifyApplied(int(s.root.Fd()), fd, a.ID, Limits{Bytes: a.Bytes, Inodes: a.Inodes}); err != nil {
		return ErrDenied
	}
	return nil
}

// Release clears limits only after the caller supplied an empty workspace and
// kernel quota usage is zero. IDs are intentionally never recycled.
func (s *Service) Release(workspaceFD int, handle [16]byte) error {
	if s == nil || workspaceFD < 0 || handle == [16]byte{} {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var st unix.Stat_t
	if unix.Fstat(workspaceFD, &st) != nil {
		return ErrDenied
	}
	var record *allocation
	for _, a := range s.ledger.Active {
		// The handle is a capability, not the project ID. Do not short-circuit
		// its comparison, even when another record field differs.
		match := subtle.ConstantTimeCompare(a.Handle[:], handle[:])
		if match == 1 && a.Dev == uint64(st.Dev) && a.Ino == st.Ino && (a.State == stateActive || a.State == stateReleasing) {
			x := a
			record = &x
		}
	}
	if record == nil {
		if t, ok := s.ledger.Terminal[terminalKey(handle)]; ok && subtle.ConstantTimeCompare(t.Handle[:], handle[:]) == 1 && t.Dev == uint64(st.Dev) && t.Ino == st.Ino {
			return ErrCompleted
		}
		return ErrDenied
	}
	if err := revokeWorkspace(workspaceFD); err != nil {
		return err
	}
	if err := s.verifyWorkspaceFD(workspaceFD); err != nil {
		return err
	}
	// Record the irreversible cleanup intent before changing project accounting.
	next := cloneLedger(s.ledger)
	r := next.Active[record.Name]
	r.State = stateReleasing
	next.Active[record.Name] = r
	if err := s.persist(next); err != nil {
		return err
	}
	s.ledger = next
	// This transition is persisted before deletion. A crash leaves a root-only
	// workspace in stateReleasing, which reconciliation retries without ever
	// restoring caller access.
	return s.cleanupLocked(workspaceFD, *record)
}

func terminalKey(handle [16]byte) string { return fmt.Sprintf("%x", sha256.Sum256(handle[:])) }
func (s *Service) completeLocked(record allocation) error {
	next := cloneLedger(s.ledger)
	delete(next.Active, record.Name)
	next.Terminal[terminalKey(record.Handle)] = terminal{ID: record.ID, Handle: record.Handle, Dev: record.Dev, Ino: record.Ino, Completed: time.Now().UTC()}
	pruneTerminal(&next, time.Now())
	if err := s.persist(next); err != nil {
		return err
	}
	s.ledger = next
	return nil
}
func pruneTerminal(l *ledger, now time.Time) {
	for k, t := range l.Terminal {
		if now.Sub(t.Completed) > terminalMaxAge {
			delete(l.Terminal, k)
		}
	}
	for len(l.Terminal) > terminalMaxCount {
		var oldest string
		var when time.Time
		for k, t := range l.Terminal {
			if oldest == "" || t.Completed.Before(when) {
				oldest, when = k, t.Completed
			}
		}
		delete(l.Terminal, oldest)
	}
}
func (s *Service) verifyWorkspaceFD(fd int) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return ErrDenied
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return ErrDenied
	}
	m, err := identity(fd)
	if err != nil || m != s.mount {
		return ErrDenied
	}
	p, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd))
	if err != nil {
		return ErrDenied
	}
	root := strings.TrimSuffix(s.cfg.CheckoutRoot, "/") + "/"
	if !strings.HasPrefix(filepath.Clean(p)+"/", root) {
		return ErrDenied
	}
	return nil
}

// removeWorkspaceContents never turns a descriptor back into a pathname.
// It removes regular files and symlinks without resolving them, and descends
// only through same-mount directories opened with no-follow resolution.
func removeWorkspaceContents(rootFD int, mount mountIdentity) error {
	deadline := time.Now().Add(cleanupDeadline)
	entries := 0
	var walk func(int, int) error
	walk = func(dirFD, depth int) error {
		if depth > maxCleanupDepth || time.Now().After(deadline) {
			return ErrRetryable
		}
		dup, err := unix.Dup(dirFD)
		if err != nil {
			return ErrDenied
		}
		dir := os.NewFile(uintptr(dup), "workspace-cleanup")
		defer dir.Close()
		for {
			names, err := dir.ReadDir(256)
			if err != nil && err != io.EOF {
				return ErrDenied
			}
			for _, entry := range names {
				entries++
				if entries > maxCleanupEntries || time.Now().After(deadline) {
					return ErrRetryable
				}
				if !validCleanupName(entry.Name()) {
					return ErrDenied
				}
				var st unix.Stat_t
				if unix.Fstatat(dirFD, entry.Name(), &st, unix.AT_SYMLINK_NOFOLLOW) != nil || uint64(st.Dev) != mount.Dev {
					return ErrDenied
				}
				switch st.Mode & unix.S_IFMT {
				case unix.S_IFREG, unix.S_IFLNK, unix.S_IFIFO, unix.S_IFSOCK, unix.S_IFCHR, unix.S_IFBLK:
					// unlinkat never opens the object, including hard links and special nodes.
					if unix.Unlinkat(dirFD, entry.Name(), 0) != nil {
						return ErrDenied
					}
				case unix.S_IFDIR:
					child, err := unix.Openat2(dirFD, entry.Name(), &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_XDEV})
					if err != nil {
						return ErrDenied
					}
					walkErr := walk(child, depth+1)
					_ = unix.Close(child)
					if walkErr != nil {
						return walkErr
					}
					if unlinkErr := unix.Unlinkat(dirFD, entry.Name(), unix.AT_REMOVEDIR); unlinkErr != nil {
						return ErrDenied
					}
				default:
					return ErrDenied
				}
			}
			if err == io.EOF {
				break
			}
		}
		return nil
	}
	return walk(rootFD, 0)
}

func validCleanupName(name string) bool {
	return name != "" && name != "." && name != ".." && len(name) <= 255 && !strings.ContainsAny(name, "/\x00")
}
func emptyDir(fd int) error {
	dup, err := unix.Dup(fd)
	if err != nil {
		return ErrDenied
	}
	f := os.NewFile(uintptr(dup), "workspace")
	defer f.Close()
	names, err := f.ReadDir(1)
	if err == io.EOF {
		return nil
	}
	if err != nil {
		return ErrDenied
	}
	if len(names) > 0 {
		return ErrDenied
	}
	return nil
}
func setProject(fd int, id uint32, inherit bool) error {
	x := fsxattr{ProjID: id}
	if inherit {
		x.XFlags = fsXflagProjInherit
	}
	return ioctlPointer(fd, fsxattrSetProject, unsafe.Pointer(&x))
}
func getProject(fd int) (uint32, error) {
	var x fsxattr
	if err := ioctlPointer(fd, fsxattrGetProject, unsafe.Pointer(&x)); err != nil {
		return 0, err
	}
	if x.ProjID == 0 || x.XFlags&fsXflagProjInherit == 0 {
		return 0, ErrDenied
	}
	return x.ProjID, nil
}
func setLimits(root, id int, l Limits) error {
	blocks, ok := blocksForBytes(l.Bytes)
	if !ok {
		return ErrInvalid
	}
	q := diskQuota{Version: 1, Flags: fsProjectQuota, FieldMask: fsDqBlkHard | fsDqInoHard, ID: uint32(id), BlockHard: blocks, InodeHard: l.Inodes}
	return quotactlFD(root, qXSetQLim, id, unsafe.Pointer(&q))
}
func getQuota(root, id int) (diskQuota, error) {
	q := diskQuota{Version: 1, Flags: fsProjectQuota, ID: uint32(id)}
	return q, quotactlFD(root, qXGetQuota, id, unsafe.Pointer(&q))
}
func verifyApplied(root, fd int, id uint32, l Limits) error {
	blocks, ok := blocksForBytes(l.Bytes)
	if !ok {
		return ErrUnavailable
	}
	q, e := getQuota(root, int(id))
	if e != nil || q.BlockHard != blocks || q.InodeHard != l.Inodes {
		return ErrUnavailable
	}
	var x fsxattr
	if e = ioctlPointer(fd, fsxattrGetProject, unsafe.Pointer(&x)); e != nil || x.ProjID != id || x.XFlags&fsXflagProjInherit == 0 {
		return ErrUnavailable
	}
	return nil
}

func ioctlPointer(fd int, request uintptr, p unsafe.Pointer) error {
	_, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), request, uintptr(p))
	if e != 0 {
		return e
	}
	return nil
}
func quotactlFD(fd, op, id int, p unsafe.Pointer) error {
	// QCMD(command, type): XQM commands are themselves the command passed in
	// the high portion of the kernel quotactl selector.
	selector := uintptr(op<<8 | xqmProjectQuota)
	_, _, e := unix.Syscall6(unix.SYS_QUOTACTL_FD, uintptr(fd), selector, uintptr(id), uintptr(p), 0, 0)
	if e != 0 {
		return e
	}
	return nil
}
func loadLedgerAt(dirfd int, name string, m mountIdentity, initialize bool, floor uint32, rootfd int, checkoutUID, checkoutGID uint32) (ledger, error) {
	fd, e := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(e, unix.ENOENT) {
		if !initialize || !pristineReservedNamespace(rootfd) {
			return ledger{}, ErrUnavailable
		}
		l := ledger{Mount: m, CheckoutUID: checkoutUID, CheckoutGID: checkoutGID, Next: floor, Active: make(map[string]allocation), Terminal: make(map[string]terminal)}
		return l, writeLedgerAt(dirfd, name, l)
	}
	if e != nil || initialize {
		return ledger{}, ErrUnavailable
	}
	f := os.NewFile(uintptr(fd), "quota-ledger")
	b, e := io.ReadAll(io.LimitReader(f, 1<<20))
	c := f.Close()
	if e != nil || c != nil {
		return ledger{}, ErrUnavailable
	}
	var l ledger
	if json.Unmarshal(b, &l) != nil || l.Mount != m || l.CheckoutUID != checkoutUID || l.CheckoutGID != checkoutGID || l.Next == 0 || l.Active == nil {
		return ledger{}, ErrUnavailable
	}
	// Terminal records were added after the original active-only ledger. An
	// existing valid ledger begins with an empty replay window on first write.
	if l.Terminal == nil {
		l.Terminal = make(map[string]terminal)
	}
	if !validLedger(l) {
		return ledger{}, ErrUnavailable
	}
	return l, nil
}
func (s *Service) persist(l ledger) error {
	if err := writeLedgerAt(s.stateDir, s.stateName, l); err != nil {
		return ErrUnavailable
	}
	return nil
}
func writeLedgerAt(dirfd int, name string, l ledger) error {
	b, e := json.Marshal(l)
	if e != nil {
		return e
	}
	// O_EXCL makes a stale or attacker-created temporary file a hard failure.
	tmp := fmt.Sprintf(".%s.%d.%d", name, os.Getpid(), time.Now().UnixNano())
	fd, e := unix.Openat(dirfd, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if e != nil {
		return e
	}
	defer func() { _ = unix.Unlinkat(dirfd, tmp, 0) }()
	for len(b) > 0 {
		n, werr := unix.Write(fd, b)
		if werr != nil {
			unix.Close(fd)
			return werr
		}
		b = b[n:]
	}
	if e = unix.Fsync(fd); e == nil {
		e = unix.Close(fd)
	} else {
		_ = unix.Close(fd)
	}
	if e != nil {
		return e
	}
	if e = unix.Renameat(dirfd, tmp, dirfd, name); e != nil {
		return e
	}
	return unix.Fsync(dirfd)
}

// reconcile never resurrects an incomplete allocation. Records that cannot be
// proven safe remain inaccessible and consume their ID permanently; healthy
// active records are deliberately left alone so a helper restart is survivable.
func (s *Service) reconcile() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, a := range s.ledger.Active {
		if a.State == stateReserved {
			fd, err := unix.Openat(int(s.root.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
			if err == nil {
				unix.Close(fd)
				// Reserved predates durable inode capture. A present entry is
				// therefore unprovable and remains quarantined rather than touched.
				continue
			}
			if errors.Is(err, unix.ENOENT) {
				next := cloneLedger(s.ledger)
				delete(next.Active, name)
				if s.persist(next) == nil {
					s.ledger = next
				}
			}
			continue
		}
		if a.State == stateActive {
			fd, err := unix.Openat(int(s.root.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
			if err == nil && s.verifyRecordFD(fd, a, true) == nil {
				unix.Close(fd)
				continue
			}
			if fd >= 0 {
				// Do not revoke a name unless its durable identity still matches.
				if s.matchesRecordFD(fd, a) != nil {
					unix.Close(fd)
					continue
				}
				if revokeWorkspace(fd) != nil {
					unix.Close(fd)
					return ErrUnavailable
				}
				unix.Close(fd)
			}
			next := cloneLedger(s.ledger)
			b := next.Active[name]
			b.State = stateReleasing
			next.Active[name] = b
			if s.persist(next) == nil {
				s.ledger = next
			}
			// Continue with a later restart if it cannot be proven empty.
			continue
		}
		fd, err := unix.Openat(int(s.root.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			if !errors.Is(err, unix.ENOENT) {
				continue
			}
			q, qe := getQuota(int(s.root.Fd()), int(a.ID))
			if qe != nil || q.Blocks != 0 || q.Inodes != 0 {
				continue
			}
			if err = clearLimits(int(s.root.Fd()), a.ID); err != nil {
				continue
			}
			_ = s.completeLocked(a)
			continue
		}
		// Do not use a path after this point. Every operation is descriptor based.
		if s.matchesRecordFD(fd, a) != nil {
			unix.Close(fd)
			continue
		}
		if revokeWorkspace(fd) != nil {
			unix.Close(fd)
			return ErrUnavailable
		}
		if s.verifyWorkspaceFD(fd) != nil || removeWorkspaceContents(fd, s.mount) != nil || emptyDir(fd) != nil || ((a.State == stateReady || a.State == stateOffered) && s.verifyRecordFD(fd, a, false) != nil) {
			unix.Close(fd)
			continue
		}
		if verifyProjectCleared(fd) != nil && setProject(fd, 0, false) != nil {
			unix.Close(fd)
			continue
		}
		unix.Close(fd)
		q, qe := getQuota(int(s.root.Fd()), int(a.ID))
		if qe != nil || q.Blocks != 0 || q.Inodes != 0 || clearLimits(int(s.root.Fd()), a.ID) != nil {
			continue
		}
		if unlinkErr := unix.Unlinkat(int(s.root.Fd()), name, unix.AT_REMOVEDIR); unlinkErr != nil && !errors.Is(unlinkErr, unix.ENOENT) {
			continue
		}
		_ = s.completeLocked(a)
	}
	return nil
}
func verifyProjectCleared(fd int) error {
	var x fsxattr
	if err := ioctlPointer(fd, fsxattrGetProject, unsafe.Pointer(&x)); err != nil || x.ProjID != 0 || x.XFlags&fsXflagProjInherit != 0 {
		return ErrDenied
	}
	return nil
}
func clearLimits(rootfd int, id uint32) error {
	q, err := getQuota(rootfd, int(id))
	if err != nil {
		return err
	}
	q.BlockHard, q.BlockSoft, q.InodeHard, q.InodeSoft = 0, 0, 0, 0
	q.FieldMask = fsDqBlkHard | fsDqInoHard
	if err = quotactlFD(rootfd, qXSetQLim, int(id), unsafe.Pointer(&q)); err != nil {
		return err
	}
	q, err = getQuota(rootfd, int(id))
	if err != nil || q.BlockHard != 0 || q.InodeHard != 0 {
		return ErrUnavailable
	}
	return nil
}

func cloneLedger(in ledger) ledger {
	out := in
	out.Active = make(map[string]allocation, len(in.Active))
	for k, v := range in.Active {
		out.Active[k] = v
	}
	out.Terminal = make(map[string]terminal, len(in.Terminal))
	for k, v := range in.Terminal {
		out.Terminal[k] = v
	}
	return out
}
func validLedger(l ledger) bool {
	ids := make(map[uint32]struct{}, len(l.Active))
	handles := make(map[[16]byte]struct{}, len(l.Active))
	for name, a := range l.Active {
		if name != a.Name || !validWorkspaceName(name, a.ID) || a.ID == 0 || a.ID >= l.Next || a.Handle == [16]byte{} || a.Bytes == 0 || a.Inodes == 0 {
			return false
		}
		if _, ok := ids[a.ID]; ok {
			return false
		}
		ids[a.ID] = struct{}{}
		if _, ok := handles[a.Handle]; ok {
			return false
		}
		handles[a.Handle] = struct{}{}
		if a.State == stateReserved {
			if a.Dev != 0 || a.Ino != 0 {
				return false
			}
			continue
		}
		if a.Dev != l.Mount.Dev || a.Ino == 0 || (a.State != statePreparing && a.State != stateReady && a.State != stateOffered && a.State != stateGranting && a.State != stateActive && a.State != stateReleasing) {
			return false
		}
	}
	for key, t := range l.Terminal {
		if key != terminalKey(t.Handle) || t.ID == 0 || t.ID >= l.Next || t.Handle == [16]byte{} || t.Dev != l.Mount.Dev || t.Ino == 0 || t.Completed.IsZero() {
			return false
		}
		if _, exists := handles[t.Handle]; exists {
			return false
		}
		handles[t.Handle] = struct{}{}
	}
	return true
}
func validWorkspaceName(name string, id uint32) bool {
	return name == fmt.Sprintf(".checkout-%08x", id)
}
func pristineReservedNamespace(rootfd int) bool {
	dup, err := unix.Dup(rootfd)
	if err != nil {
		return false
	}
	f := os.NewFile(uintptr(dup), "checkout-root")
	defer f.Close()
	names, err := f.ReadDir(-1)
	if err != nil {
		return false
	}
	for _, n := range names {
		if strings.HasPrefix(n.Name(), ".checkout-") {
			return false
		}
	}
	return true
}

// wireRequest is intentionally fixed-size and carries no path/device/project ID.
type wireRequest struct {
	Op            byte
	_             [7]byte
	Bytes, Inodes uint64
	Handle        [16]byte
}

func decodeRequest(b []byte) (wireRequest, error) {
	var r wireRequest
	if len(b) != 40 || !allZero(b[1:8]) {
		return r, ErrInvalid
	}
	if err := binary.Read(bytes.NewReader(b), binary.BigEndian, &r); err != nil {
		return r, ErrInvalid
	}
	return r, nil
}
func allZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}
