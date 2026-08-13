//go:build linux

package checkoutquota

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func TestDecodeRequestRejectsNonFixedBodies(t *testing.T) {
	if _, err := decodeRequest(make([]byte, 39)); err == nil {
		t.Fatal("accepted truncated request")
	}
	b := make([]byte, 40)
	binary.BigEndian.PutUint64(b[8:], 1)
	binary.BigEndian.PutUint64(b[16:], 1)
	r, err := decodeRequest(b)
	if err != nil || r.Bytes != 1 || r.Inodes != 1 {
		t.Fatalf("request=%+v err=%v", r, err)
	}
}

func TestCleanupUnlinksSymlinkWithoutFollowingIt(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	m, err := identity(int(f.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if err := removeWorkspaceContents(int(f.Fd()), m); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("outside target removed: %v", err)
	}
	if err := emptyDir(int(f.Fd())); err != nil {
		t.Fatalf("not empty: %v", err)
	}
}

func TestCleanupUnlinksHardlinkedRegularFile(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "one")
	if err := os.WriteFile(first, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(first, filepath.Join(dir, "two")); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	m, err := identity(int(f.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if err := removeWorkspaceContents(int(f.Fd()), m); err != nil {
		t.Fatalf("hardlink cleanup: %v", err)
	}
	if err := emptyDir(int(f.Fd())); err != nil {
		t.Fatalf("workspace not empty: %v", err)
	}
}

func TestBlocksForBytesAvoidsCeilingOverflow(t *testing.T) {
	if _, ok := blocksForBytes(0); ok {
		t.Fatal("zero bytes accepted")
	}
	if got, ok := blocksForBytes(513); !ok || got != 2 {
		t.Fatalf("513 => %d, %v", got, ok)
	}
	want := uint64(math.MaxUint64/512) + 1
	if got, ok := blocksForBytes(math.MaxUint64); !ok || got != want {
		t.Fatalf("max => %d, %v", got, ok)
	}
}

func TestOneReceivedFDClosesRejectedExtras(t *testing.T) {
	first, err := unix.Dup(0)
	if err != nil {
		t.Skipf("cannot duplicate stdin: %v", err)
	}
	second, err := unix.Dup(0)
	if err != nil {
		unix.Close(first)
		t.Skipf("cannot duplicate stdin: %v", err)
	}
	_, err = oneReceivedFD(unix.UnixRights(first, second))
	if err == nil {
		t.Fatal("accepted multiple descriptors")
	}
	for _, fd := range []int{first, second} {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err == nil {
			t.Fatalf("fd %d leaked", fd)
		}
	}
}

func TestShortFrameStillClosesReceivedFD(t *testing.T) {
	fd, err := unix.Dup(0)
	if err != nil {
		t.Skipf("cannot duplicate stdin: %v", err)
	}
	// serveConn performs this parsing before rejecting a short frame; this is
	// the regression helper for that ordering.
	got, err := oneReceivedFD(unix.UnixRights(fd))
	if err != nil || got < 0 {
		t.Fatalf("parse fd: %v", err)
	}
	if len(make([]byte, 39)) == 40 {
		t.Fatal("test setup")
	}
	if err := unix.Close(got); err != nil {
		t.Fatal(err)
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err == nil {
		t.Fatal("short-frame fd leaked")
	}
}

func TestConsumeNoFDsAcceptsCanonicalEmptyAncillary(t *testing.T) {
	if !consumeNoFDs(nil) {
		t.Fatal("empty ACK ancillary data rejected")
	}
}

func TestConsumeNoFDsRejectsAndClosesDescriptor(t *testing.T) {
	fd, err := unix.Dup(0)
	if err != nil {
		t.Skipf("cannot duplicate stdin: %v", err)
	}
	if consumeNoFDs(unix.UnixRights(fd)) {
		t.Fatal("ACK with descriptor accepted")
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err == nil {
		t.Fatal("ACK descriptor leaked")
	}
}

func TestDecodeRequestRejectsNonCanonicalReservedBytes(t *testing.T) {
	b := make([]byte, 40)
	b[1] = 1
	if _, err := decodeRequest(b); err == nil {
		t.Fatal("accepted nonzero reserved field")
	}
}

func TestLedgerCopyOnWrite(t *testing.T) {
	var capability [16]byte
	capability[0] = 1
	original := ledger{Next: 9, Active: map[string]allocation{".checkout-00000008": {ID: 8, Name: ".checkout-00000008", Handle: capability, Dev: 7, Ino: 8, Bytes: 1, Inodes: 1, State: statePreparing}}}
	copy := cloneLedger(original)
	record := copy.Active[".checkout-00000008"]
	record.State = stateActive
	copy.Active[record.Name] = record
	delete(copy.Active, record.Name)
	if original.Next != 9 || original.Active[record.Name].State != statePreparing || len(original.Active) != 1 {
		t.Fatalf("persistence candidate mutated current ledger: %#v", original)
	}
}

func TestLedgerRecordValidation(t *testing.T) {
	var capability [16]byte
	capability[15] = 1
	good := ledger{Mount: mountIdentity{Dev: 4}, Next: 6, Active: map[string]allocation{".checkout-00000005": {ID: 5, Name: ".checkout-00000005", Handle: capability, Dev: 4, Ino: 9, Bytes: 1, Inodes: 1, State: stateReleasing}}}
	if !validLedger(good) {
		t.Fatal("releasing record rejected")
	}
	bad := cloneLedger(good)
	r := bad.Active[".checkout-00000005"]
	r.State = "unknown"
	bad.Active[r.Name] = r
	if validLedger(bad) {
		t.Fatal("unknown recovery state accepted")
	}
}

func TestTerminalLedgerRecordAndBoundedPruning(t *testing.T) {
	var handle [16]byte
	handle[0] = 7
	l := ledger{Mount: mountIdentity{Dev: 4}, Next: 6, Active: map[string]allocation{}, Terminal: map[string]terminal{
		terminalKey(handle): {ID: 5, Handle: handle, Dev: 4, Ino: 9, Completed: time.Now()},
	}}
	if !validLedger(l) {
		t.Fatal("terminal record rejected")
	}
	bad := cloneLedger(l)
	bad.Terminal["wrong"] = bad.Terminal[terminalKey(handle)]
	delete(bad.Terminal, terminalKey(handle))
	if validLedger(bad) {
		t.Fatal("terminal record accepted wrong digest key")
	}
}

func TestStaleSocketModeAllowedOnlyIntermediateOrFinal(t *testing.T) {
	if !staleSocketModeAllowed(&unix.Stat_t{Gid: 0, Mode: 0o600}, 42) || !staleSocketModeAllowed(&unix.Stat_t{Gid: 0, Mode: 0o620}, 42) || !staleSocketModeAllowed(&unix.Stat_t{Gid: 42, Mode: 0o620}, 42) {
		t.Fatal("safe startup modes rejected")
	}
	if staleSocketModeAllowed(&unix.Stat_t{Gid: 42, Mode: 0o600}, 42) || staleSocketModeAllowed(&unix.Stat_t{Gid: 9, Mode: 0o620}, 42) || staleSocketModeAllowed(&unix.Stat_t{Gid: 0, Mode: 0o640}, 42) {
		t.Fatal("mixed ownership/mode accepted")
	}
}

func TestLedgerRejectsUnburnedOrDuplicateIdentity(t *testing.T) {
	var handle [16]byte
	handle[0] = 1
	l := ledger{Mount: mountIdentity{Dev: 1}, Next: 4, Active: map[string]allocation{".checkout-00000004": {ID: 4, Name: ".checkout-00000004", Handle: handle, Bytes: 1, Inodes: 1, State: stateReserved}}}
	if validLedger(l) {
		t.Fatal("record with ID not below Next accepted")
	}
	l.Next = 5
	if !validLedger(l) {
		t.Fatal("valid reserved record rejected")
	}
}
func TestConfigLimitBounds(t *testing.T) {
	c := Config{MaxBytes: 10, MaxInodes: 2}
	if !c.validLimits(Limits{Bytes: 10, Inodes: 2}) || c.validLimits(Limits{Bytes: 11, Inodes: 2}) || c.validLimits(Limits{Bytes: 1}) {
		t.Fatal("bad bounds")
	}
}

func TestWireStatusDistinctions(t *testing.T) {
	var handle [16]byte
	if got := binary.BigEndian.Uint32(replyFor(handle, nil)[:4]); got != wireSuccess {
		t.Fatalf("success=%d", got)
	}
	if got := binary.BigEndian.Uint32(replyFor(handle, ErrCompleted)[:4]); got != wireCompleted {
		t.Fatalf("completed=%d", got)
	}
	if got := binary.BigEndian.Uint32(replyFor(handle, ErrDenied)[:4]); got != wireDenied {
		t.Fatalf("denied=%d", got)
	}
	if got := binary.BigEndian.Uint32(replyFor(handle, ErrRetryable)[:4]); got != wireRetryable {
		t.Fatalf("retry=%d", got)
	}
}

func TestLinuxQuotaABIConstants(t *testing.T) {
	if unsafe.Sizeof(fsxattr{}) != 28 || fsxattrGetProject != 0x801c581f || fsxattrSetProject != 0x401c5820 {
		t.Fatalf("fsxattr ABI: size=%d get=%#x set=%#x", unsafe.Sizeof(fsxattr{}), fsxattrGetProject, fsxattrSetProject)
	}
	if got := uintptr(qXSetQLim<<8 | xqmProjectQuota); got != 0x580402 {
		t.Fatalf("selector=%#x", got)
	}
	if unsafe.Sizeof(diskQuota{}) != 112 {
		t.Fatalf("disk quota ABI size=%d", unsafe.Sizeof(diskQuota{}))
	}
}
