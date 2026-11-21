//go:build linux

package checkoutquota

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type sweepHarness struct {
	t     *testing.T
	svc   *Service
	root  string
	now   time.Time
	mu    sync.Mutex
	calls []uint32
	// cleanupErr, when set, is returned by the cleanup seam instead of completing.
	cleanupErr error
}

// newSweepHarness builds a Service over temp directories with the quota-specific
// cleanup replaced by a seam (no XFS). The release path before the seam is real:
// it revokes with fchown(0,0), so the test needs root and skips otherwise.
func newSweepHarness(t *testing.T) *sweepHarness {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("revoke path needs root (fchown to root)")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	rf, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rf.Close() })
	sfd, err := unix.Open(state, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unix.Close(sfd) })
	m, err := identity(int(rf.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	h := &sweepHarness{t: t, root: root, now: time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)}
	h.svc = &Service{
		cfg:       Config{CheckoutRoot: root},
		root:      rf,
		stateDir:  sfd,
		stateName: "ledger.json",
		mount:     m,
		ledger:    ledger{Mount: m, Next: 1, Active: map[string]allocation{}, Terminal: map[string]terminal{}},
		started:   h.now,
		ttl:       DefaultReclaimTTL,
		interval:  DefaultSweepInterval,
		clock:     func() time.Time { return h.now },
	}
	h.svc.cleanupHook = func(fd int, rec allocation) error {
		h.mu.Lock()
		h.calls = append(h.calls, rec.ID)
		err := h.cleanupErr
		h.mu.Unlock()
		if err != nil {
			return err
		}
		return h.svc.completeLocked(rec)
	}
	return h
}

// add registers a workspace directory in the given state and returns its handle.
func (h *sweepHarness) add(state string, activated time.Time) (allocation, [16]byte) {
	h.t.Helper()
	id := h.svc.ledger.Next
	h.svc.ledger.Next++
	name := fmt.Sprintf(".checkout-%08x", id)
	if err := os.Mkdir(filepath.Join(h.root, name), 0o750); err != nil {
		h.t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(h.root, name), 0o750); err != nil {
		h.t.Fatal(err)
	}
	var st unix.Stat_t
	if err := unix.Stat(filepath.Join(h.root, name), &st); err != nil {
		h.t.Fatal(err)
	}
	var handle [16]byte
	handle[0], handle[1], handle[15] = byte(id), 0xA5, 1
	a := allocation{ID: id, Name: name, Handle: handle, Dev: uint64(st.Dev), Ino: st.Ino, Bytes: 1 << 20, Inodes: 100, State: state, Activated: activated}
	h.svc.ledger.Active[name] = a
	return a, handle
}

func (h *sweepHarness) callCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.calls)
}

func (h *sweepHarness) openWorkspace(a allocation) int {
	h.t.Helper()
	fd, err := unix.Open(filepath.Join(h.root, a.Name), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { unix.Close(fd) })
	return fd
}

func (h *sweepHarness) mode(a allocation) os.FileMode {
	h.t.Helper()
	info, err := os.Stat(filepath.Join(h.root, a.Name))
	if err != nil {
		h.t.Fatal(err)
	}
	return info.Mode().Perm()
}

func TestSweepReclaimsStaleActiveThroughNormalPath(t *testing.T) {
	h := newSweepHarness(t)
	a, handle := h.add(stateActive, h.now.Add(-7*time.Hour))
	if n := h.svc.reclaimStale(); n != 1 {
		t.Fatalf("reclaimed %d, want 1", n)
	}
	if _, ok := h.svc.ledger.Active[a.Name]; ok {
		t.Fatal("stale entry still active")
	}
	if _, ok := h.svc.ledger.Terminal[terminalKey(handle)]; !ok {
		t.Fatal("no terminal replay record for the reclaimed handle")
	}
	if h.callCount() != 1 {
		t.Fatalf("cleanup calls=%d", h.callCount())
	}
	// Revoked before cleanup, exactly like Release.
	if m := h.mode(a); m != 0 {
		t.Fatalf("workspace mode %v, want revoked 0", m)
	}
	b, err := readStateFile(h)
	if err != nil || strings.Contains(string(b), a.Name+`":{`) {
		t.Fatalf("persisted ledger still lists the entry: err=%v", err)
	}
}

func readStateFile(h *sweepHarness) ([]byte, error) {
	fd, err := unix.Openat(h.svc.stateDir, h.svc.stateName, unix.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "ledger")
	defer f.Close()
	buf := make([]byte, 1<<16)
	n, err := f.Read(buf)
	return buf[:n], err
}

func TestSweepLeavesFreshActiveAlone(t *testing.T) {
	h := newSweepHarness(t)
	a, _ := h.add(stateActive, h.now.Add(-time.Hour))
	edge, _ := h.add(stateActive, h.now.Add(-DefaultReclaimTTL+time.Second))
	if n := h.svc.reclaimStale(); n != 0 {
		t.Fatalf("reclaimed %d fresh entries", n)
	}
	for _, x := range []allocation{a, edge} {
		if h.svc.ledger.Active[x.Name].State != stateActive || h.mode(x) != 0o750 || h.callCount() != 0 {
			t.Fatalf("fresh entry %s disturbed", x.Name)
		}
	}
	h.now = h.now.Add(2 * time.Second) // edge entry reaches exactly TTL
	if n := h.svc.reclaimStale(); n != 1 {
		t.Fatalf("entry at TTL not reclaimed: %d", n)
	}
}

func TestSweepEntriesWithoutTimestampAgeFromServiceStart(t *testing.T) {
	h := newSweepHarness(t)
	a, _ := h.add(stateActive, time.Time{})
	// Clock far past the epoch: age must NOT be measured from the zero time.
	if n := h.svc.reclaimStale(); n != 0 {
		t.Fatalf("unknown-age entry reclaimed immediately (%d)", n)
	}
	h.now = h.svc.started.Add(DefaultReclaimTTL - time.Second)
	if n := h.svc.reclaimStale(); n != 0 {
		t.Fatalf("reclaimed before TTL after service start (%d)", n)
	}
	h.now = h.svc.started.Add(DefaultReclaimTTL)
	if n := h.svc.reclaimStale(); n != 1 {
		t.Fatalf("not reclaimed TTL after service start (%d)", n)
	}
	if _, ok := h.svc.ledger.Active[a.Name]; ok {
		t.Fatal("entry still active")
	}
}

func TestSweepNeverTouchesOtherStates(t *testing.T) {
	h := newSweepHarness(t)
	old := h.now.Add(-48 * time.Hour)
	var all []allocation
	for _, st := range []string{stateReserved, statePreparing, stateReady, stateOffered, stateGranting} {
		a, _ := h.add(st, old)
		all = append(all, a)
	}
	if n := h.svc.reclaimStale(); n != 0 || h.callCount() != 0 {
		t.Fatalf("reclaimed=%d calls=%d", n, h.callCount())
	}
	for _, a := range all {
		if h.svc.ledger.Active[a.Name].State != a.State || h.mode(a) != 0o750 {
			t.Fatalf("%s entry disturbed", a.State)
		}
	}
}

func TestSweepThenReleaseGetsTerminalReplay(t *testing.T) {
	h := newSweepHarness(t)
	a, handle := h.add(stateActive, h.now.Add(-7*time.Hour))
	fd := h.openWorkspace(a) // the holder's descriptor
	if n := h.svc.reclaimStale(); n != 1 {
		t.Fatalf("reclaimed %d", n)
	}
	// The agent sees ErrCompleted, which the client maps to success.
	if err := h.svc.Release(fd, handle); !errors.Is(err, ErrCompleted) {
		t.Fatalf("late Release err=%v, want ErrCompleted", err)
	}
	// A different handle against the same workspace is still refused.
	var other [16]byte
	other[0] = 0xEE
	if err := h.svc.Release(fd, other); !errors.Is(err, ErrDenied) {
		t.Fatalf("foreign handle err=%v, want ErrDenied", err)
	}
	if h.callCount() != 1 {
		t.Fatalf("cleanup calls=%d", h.callCount())
	}
}

func TestReleaseThenSweepIsNoop(t *testing.T) {
	h := newSweepHarness(t)
	a, handle := h.add(stateActive, h.now.Add(-7*time.Hour))
	fd := h.openWorkspace(a)
	if err := h.svc.Release(fd, handle); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if n := h.svc.reclaimStale(); n != 0 {
		t.Fatalf("sweep reclaimed %d after Release", n)
	}
	if h.callCount() != 1 {
		t.Fatalf("cleanup calls=%d", h.callCount())
	}
}

func TestSweepAndReleaseConcurrentlyCleanOnce(t *testing.T) {
	for i := 0; i < 50; i++ {
		h := newSweepHarness(t)
		a, handle := h.add(stateActive, h.now.Add(-7*time.Hour))
		fd := h.openWorkspace(a)
		var wg sync.WaitGroup
		var relErr error
		wg.Add(2)
		go func() { defer wg.Done(); h.svc.reclaimStale() }()
		go func() { defer wg.Done(); relErr = h.svc.Release(fd, handle) }()
		wg.Wait()
		if relErr != nil && !errors.Is(relErr, ErrCompleted) {
			t.Fatalf("iter %d: Release err=%v", i, relErr)
		}
		if h.callCount() != 1 {
			t.Fatalf("iter %d: cleanup calls=%d", i, h.callCount())
		}
		if len(h.svc.ledger.Active) != 0 || len(h.svc.ledger.Terminal) != 1 {
			t.Fatalf("iter %d: ledger active=%d terminal=%d", i, len(h.svc.ledger.Active), len(h.svc.ledger.Terminal))
		}
	}
}

func TestSweepRetriesReleasingUntilItCompletes(t *testing.T) {
	h := newSweepHarness(t)
	a, _ := h.add(stateActive, h.now.Add(-7*time.Hour))
	h.cleanupErr = ErrRetryable
	if n := h.svc.reclaimStale(); n != 0 {
		t.Fatalf("reclaimed %d despite failed cleanup", n)
	}
	if h.svc.ledger.Active[a.Name].State != stateReleasing {
		t.Fatalf("state=%s, want releasing", h.svc.ledger.Active[a.Name].State)
	}
	// Failed again: stays releasing, retried (no TTL needed).
	if n := h.svc.reclaimStale(); n != 0 || h.callCount() != 2 || h.svc.ledger.Active[a.Name].State != stateReleasing {
		t.Fatalf("n=%d calls=%d state=%s", n, h.callCount(), h.svc.ledger.Active[a.Name].State)
	}
	h.cleanupErr = nil
	if n := h.svc.reclaimStale(); n != 1 || h.callCount() != 3 {
		t.Fatalf("next sweep did not finish it: n=%d calls=%d", n, h.callCount())
	}
	if _, ok := h.svc.ledger.Active[a.Name]; ok {
		t.Fatal("still active after completion")
	}
}

func TestSweepIncompleteCleanupLeavesReleasingForReleaseToFinish(t *testing.T) {
	h := newSweepHarness(t)
	a, handle := h.add(stateActive, h.now.Add(-7*time.Hour))
	fd := h.openWorkspace(a)
	h.cleanupErr = ErrRetryable
	h.svc.reclaimStale()
	h.cleanupErr = nil
	if err := h.svc.Release(fd, handle); err != nil {
		t.Fatalf("Release after partial sweep: %v", err)
	}
	if n := h.svc.reclaimStale(); n != 0 {
		t.Fatalf("sweep after Release reclaimed %d", n)
	}
}

func TestSweepFreshReleasingIsRetriedWithoutTTL(t *testing.T) {
	h := newSweepHarness(t)
	h.add(stateReleasing, h.now) // brand new, well under the TTL
	if n := h.svc.reclaimStale(); n != 1 || h.callCount() != 1 {
		t.Fatalf("n=%d calls=%d", n, h.callCount())
	}
}

func TestSweepMissingDirectoryFinishesLikeReconcile(t *testing.T) {
	h := newSweepHarness(t)
	var finished []uint32
	h.svc.missingHook = func(a allocation) error {
		finished = append(finished, a.ID)
		return h.svc.completeLocked(a)
	}
	a, handle := h.add(stateActive, h.now.Add(-7*time.Hour))
	fresh, _ := h.add(stateActive, h.now) // fresh: its missing dir must not matter
	for _, x := range []allocation{a, fresh} {
		if err := os.RemoveAll(filepath.Join(h.root, x.Name)); err != nil { // test-owned temp dir
			t.Fatal(err)
		}
	}
	if n := h.svc.reclaimStale(); n != 1 {
		t.Fatalf("reclaimed %d", n)
	}
	if len(finished) != 1 || finished[0] != a.ID || h.callCount() != 0 {
		t.Fatalf("finished=%v cleanupCalls=%d", finished, h.callCount())
	}
	if _, ok := h.svc.ledger.Terminal[terminalKey(handle)]; !ok {
		t.Fatal("no terminal record")
	}
	if _, ok := h.svc.ledger.Active[fresh.Name]; !ok {
		t.Fatal("fresh entry removed")
	}
}

func TestSweepMissingDirectoryIncompleteStaysActive(t *testing.T) {
	h := newSweepHarness(t)
	h.svc.missingHook = func(allocation) error { return ErrRetryable }
	a, _ := h.add(stateActive, h.now.Add(-7*time.Hour))
	if err := os.RemoveAll(filepath.Join(h.root, a.Name)); err != nil {
		t.Fatal(err)
	}
	if n := h.svc.reclaimStale(); n != 0 || h.svc.ledger.Active[a.Name].State != stateActive {
		t.Fatalf("n=%d state=%s", n, h.svc.ledger.Active[a.Name].State)
	}
}

func TestSweepCapsWorkPerTickOldestFirst(t *testing.T) {
	h := newSweepHarness(t)
	total := maxSweepEntries + 5
	for i := 0; i < total; i++ {
		// Entry i is (total-i) hours past the TTL boundary: later i = younger.
		h.add(stateActive, h.now.Add(-DefaultReclaimTTL-time.Duration(total-i)*time.Hour))
	}
	if n := h.svc.reclaimStale(); n != maxSweepEntries || h.callCount() != maxSweepEntries {
		t.Fatalf("first tick n=%d calls=%d, want %d", n, h.callCount(), maxSweepEntries)
	}
	if len(h.svc.ledger.Active) != 5 {
		t.Fatalf("remaining=%d, want 5", len(h.svc.ledger.Active))
	}
	// Oldest first: IDs 1..16 went first, the 5 youngest remain.
	for _, a := range h.svc.ledger.Active {
		if a.ID <= uint32(maxSweepEntries) {
			t.Fatalf("id %d reclaimed late; not oldest first", a.ID)
		}
	}
	if n := h.svc.reclaimStale(); n != 5 || len(h.svc.ledger.Active) != 0 {
		t.Fatalf("second tick n=%d remaining=%d", n, len(h.svc.ledger.Active))
	}
}

func TestLedgerActivatedBackwardCompatibleDecode(t *testing.T) {
	old := `{"mount":{"Dev":1,"FSType":0,"Fsid0":0,"Fsid1":0},"checkout_uid":5,"checkout_gid":6,"next":3,` +
		`"active":{".checkout-00000001":{"id":1,"name":".checkout-00000001","handle":[1,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0],"dev":1,"ino":9,"bytes":10,"inodes":10,"state":"active"}},` +
		`"terminal":{}}`
	var l ledger
	if err := json.Unmarshal([]byte(old), &l); err != nil {
		t.Fatal(err)
	}
	a := l.Active[".checkout-00000001"]
	if !a.Activated.IsZero() || a.State != stateActive || !validLedger(l) {
		t.Fatalf("old ledger decoded wrong: %+v valid=%v", a, validLedger(l))
	}
	b, err := json.Marshal(l)
	if err != nil || strings.Contains(string(b), "activated") {
		t.Fatalf("zero Activated must be omitted: %s err=%v", b, err)
	}
	a.Activated = time.Date(2026, 11, 21, 1, 2, 3, 0, time.UTC)
	l.Active[a.Name] = a
	b, _ = json.Marshal(l)
	var back ledger
	if err = json.Unmarshal(b, &back); err != nil || !back.Active[a.Name].Activated.Equal(a.Activated) {
		t.Fatalf("round trip lost Activated: %s err=%v", b, err)
	}
}
