package agentclient

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"autodeploy/internal/agent/state"
)

func newDrainStore(t *testing.T, root string) state.DrainStore {
	t.Helper()
	s, err := state.New(root)
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.Drains()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func withPred(p state.PendingActivation) state.PendingActivation {
	p.ReleaseID, p.BackendRuntimeInstanceID = "rel_new", "42"
	p.PredecessorReleaseID, p.PredecessorRuntimeInstanceID = "rel_old", "41"
	p.DrainSeconds = 30
	return p
}

func drains(t *testing.T, d state.DrainStore) []state.PendingDrain {
	t.Helper()
	recs, bad, err := d.ScanDrains()
	if err != nil || len(bad) != 0 {
		t.Fatalf("scan: %v %v", bad, err)
	}
	return recs
}

func TestReporterArmsDrainOn200BeforeDelete(t *testing.T) {
	root := t.TempDir()
	a, d := newActStore(t, root), newDrainStore(t, root)
	rec := withPred(readySucceeded(1))
	save(t, a, rec)
	h := newHarness(t, a, nil)
	h.rep.Drains = d
	h.client.hook = func(int64) {
		if len(drains(t, d)) != 0 {
			t.Error("drain armed before the report was answered")
		}
	}
	h.runUntil(t, func() bool { return len(remaining(t, a)) == 0 })
	got := drains(t, d)
	if len(got) != 1 {
		t.Fatalf("drains=%v", got)
	}
	g := got[0]
	if g.AttemptID != 1 || g.Phase != state.DrainPending || g.Attempts != 0 ||
		g.PredecessorRuntimeInstanceID != "41" || g.BackendRuntimeInstanceID != "42" ||
		g.DrainSeconds != 30 || g.ObservedRouteDigest != rec.ObservedRouteDigest {
		t.Fatalf("drain=%+v", g)
	}
}

func TestReporterArmedAtEqualsClock(t *testing.T) {
	root := t.TempDir()
	a, d := newActStore(t, root), newDrainStore(t, root)
	save(t, a, withPred(readySucceeded(1)))
	h := newHarness(t, a, nil)
	h.rep.Drains = d
	fixed := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	h.rep.now = func() time.Time { return fixed }
	h.runUntil(t, func() bool { return len(remaining(t, a)) == 0 })
	if got := drains(t, d); len(got) != 1 || !got[0].ArmedAt.Equal(fixed) {
		t.Fatalf("drains=%+v", got)
	}
}

// flakyArmer fails the first fails ArmDrain calls, then delegates.
type flakyArmer struct {
	inner state.DrainStore
	mu    sync.Mutex
	fails int
	calls int
}

func (f *flakyArmer) ArmDrain(d state.PendingDrain) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.fails > 0 {
		f.fails--
		return errors.New("disk full")
	}
	return f.inner.ArmDrain(d)
}

func TestReporterArmFailureKeepsRecordThenArmsAndDeletes(t *testing.T) {
	root := t.TempDir()
	a, d := newActStore(t, root), newDrainStore(t, root)
	save(t, a, withPred(readySucceeded(1)))
	arm := &flakyArmer{inner: d, fails: 2}
	h := newHarness(t, a, nil)
	h.rep.Drains = arm
	h.runUntil(t, func() bool { return len(remaining(t, a)) == 0 })
	if h.client.count(1) < 3 || arm.calls < 3 {
		t.Fatalf("expected re-sends until armed, calls=%d arms=%d", h.client.count(1), arm.calls)
	}
	got := drains(t, d)
	if len(got) != 1 || got[0].Attempts != 0 {
		t.Fatalf("drains=%+v", got)
	}
	if !strings.Contains(h.logText(), "could not be armed") {
		t.Fatal("arm failure not logged")
	}
	// A later identical arm (a crash after arm, before delete) keeps ArmedAt.
	first := got[0].ArmedAt
	again := got[0]
	again.ArmedAt = first.Add(time.Hour)
	if err := d.ArmDrain(again); err != nil {
		t.Fatal(err)
	}
	if after := drains(t, d); !after[0].ArmedAt.Equal(first) {
		t.Fatalf("armed_at moved: %v -> %v", first, after[0].ArmedAt)
	}
}

func TestReporterNeverArmsOutsideAcknowledgedSuccess(t *testing.T) {
	answers := map[string]error{}
	for _, c := range serverConflictCodes {
		answers["409 "+c] = fmt.Errorf("wrapped: %w", &ConflictError{Code: c})
	}
	answers["stale_lease"] = ErrStaleLease
	for name, ans := range answers {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			a, d := newActStore(t, root), newDrainStore(t, root)
			save(t, a, withPred(readySucceeded(1)))
			h := newHarness(t, a, map[int64][]error{1: {ans}})
			h.rep.Drains = d
			h.runUntil(t, func() bool { return h.client.count(1) >= 2 || len(remaining(t, a)) == 0 })
			if got := drains(t, d); len(got) != 0 {
				t.Fatalf("armed on %s: %+v", name, got)
			}
		})
	}
}

func TestReporterNoDrainWithoutPredecessorOrOnFailure(t *testing.T) {
	root := t.TempDir()
	a, d := newActStore(t, root), newDrainStore(t, root)
	failedWithPred := withPred(readyFailed(2))
	save(t, a, readySucceeded(1), failedWithPred)
	h := newHarness(t, a, nil)
	h.rep.Drains = d
	h.runUntil(t, func() bool { return len(remaining(t, a)) == 0 })
	if h.client.count(1) != 1 || h.client.count(2) != 1 {
		t.Fatalf("calls %d %d", h.client.count(1), h.client.count(2))
	}
	if got := drains(t, d); len(got) != 0 {
		t.Fatalf("drains=%+v", got)
	}
}

func TestReporterNilDrainStoreLogsEachLostDrainAndDeletes(t *testing.T) {
	a := newActStore(t, t.TempDir())
	save(t, a, withPred(readySucceeded(1)), withPred(readySucceeded(2)))
	h := newHarness(t, a, nil)
	h.runUntil(t, func() bool { return len(remaining(t, a)) == 0 })
	for _, id := range []string{"attempt 1:", "attempt 2:"} {
		if !strings.Contains(h.logText(), "ERROR "+id+" a predecessor drain is owed but no drain store") {
			t.Fatalf("lost drain %s not logged: %s", id, h.logText())
		}
	}
	if n := strings.Count(h.logText(), "no drain store is configured"); n != 2 {
		t.Fatalf("logged %d times", n)
	}
}

func TestReporterArmOverCorruptDrainKeepsRecordAndRateLimitsLog(t *testing.T) {
	root := t.TempDir()
	a, d := newActStore(t, root), newDrainStore(t, root)
	save(t, a, withPred(readySucceeded(1)))
	if err := os.WriteFile(filepath.Join(root, "drains", "attempt-1.json"), []byte("{corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, a, nil)
	h.rep.Drains = d
	h.runUntil(t, func() bool { return h.client.count(1) >= 5 })
	if len(remaining(t, a)) != 1 {
		t.Fatal("activation record deleted although the drain could not be armed")
	}
	if n := strings.Count(h.logText(), "could not be armed"); n != 1 {
		t.Fatalf("arm error logged %d times, want once (rate-limited)", n)
	}
	// After the limiter interval it logs again.
	h.mu.Lock()
	h.clock = h.clock.Add(2 * time.Hour)
	h.mu.Unlock()
	h.runUntil(t, func() bool { return strings.Count(h.logText(), "could not be armed") >= 2 })
}
