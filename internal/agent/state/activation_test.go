package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newActs(t *testing.T) (string, ActivationStore) {
	t.Helper()
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Activations()
	if err != nil {
		t.Fatal(err)
	}
	return root, a
}

func authorized(id int64) PendingActivation {
	return PendingActivation{
		AttemptID: id, Fence: 3, EnvironmentID: "env_1", OperationID: "42",
		RouteDigest: [32]byte{1}, ImageDigest: [32]byte{2}, RouteRevision: 5,
		Phase: PhaseAuthorized,
	}
}

func succeeded(id int64) PendingActivation {
	p := authorized(id)
	p.Phase, p.Succeeded, p.ObservedRouteDigest = PhaseResultReady, true, [32]byte{1}
	return p
}

func failed(id int64) PendingActivation {
	p := authorized(id)
	p.Phase, p.FailureSummary = PhaseResultReady, "probe failed"
	return p
}

func TestActivationRoundTripAndRestart(t *testing.T) {
	root, a := newActs(t)
	for _, p := range []PendingActivation{authorized(1), succeeded(2), failed(3)} {
		if err := a.SaveActivation(p); err != nil {
			t.Fatal(err)
		}
	}
	s2, _ := New(root)
	b, err := s2.Activations() // restart: new instance, same dir
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []PendingActivation{authorized(1), succeeded(2), failed(3)} {
		got, ok, err := b.LoadActivation(want.AttemptID)
		if err != nil || !ok {
			t.Fatalf("load %d: ok=%v err=%v", want.AttemptID, ok, err)
		}
		if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() || got.CreatedAt.Location() != time.UTC {
			t.Fatalf("timestamps: %+v", got)
		}
		got.CreatedAt, got.UpdatedAt = time.Time{}, time.Time{}
		if got != want {
			t.Fatalf("got %+v want %+v", got, want)
		}
	}
	if _, ok, err := b.LoadActivation(99); ok || err != nil {
		t.Fatalf("absent: ok=%v err=%v", ok, err)
	}
}

func TestActivationListOrderAndDelete(t *testing.T) {
	_, a := newActs(t)
	for _, id := range []int64{10, 2, 33} {
		if err := a.SaveActivation(authorized(id)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := a.ListActivations()
	if err != nil || len(got) != 3 || got[0].AttemptID != 2 || got[1].AttemptID != 10 || got[2].AttemptID != 33 {
		t.Fatalf("list=%+v err=%v", got, err)
	}
	for i := 0; i < 2; i++ {
		if err := a.DeleteActivation(10); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.DeleteActivation(777); err != nil {
		t.Fatal(err)
	}
	got, _ = a.ListActivations()
	if len(got) != 2 {
		t.Fatalf("after delete: %+v", got)
	}
}

func TestSaveActivationValidation(t *testing.T) {
	mut := func(f func(*PendingActivation)) PendingActivation { p := authorized(1); f(&p); return p }
	mutF := func(f func(*PendingActivation)) PendingActivation { p := failed(1); f(&p); return p }
	mutS := func(f func(*PendingActivation)) PendingActivation { p := succeeded(1); f(&p); return p }
	cases := map[string]PendingActivation{
		"attempt zero":        mut(func(p *PendingActivation) { p.AttemptID = 0 }),
		"fence zero":          mut(func(p *PendingActivation) { p.Fence = 0 }),
		"env empty":           mut(func(p *PendingActivation) { p.EnvironmentID = "" }),
		"op empty":            mut(func(p *PendingActivation) { p.OperationID = "" }),
		"op non numeric":      mut(func(p *PendingActivation) { p.OperationID = "abc" }),
		"op zero":             mut(func(p *PendingActivation) { p.OperationID = "0" }),
		"op negative":         mut(func(p *PendingActivation) { p.OperationID = "-4" }),
		"op non canonical":    mut(func(p *PendingActivation) { p.OperationID = "007" }),
		"route digest zero":   mut(func(p *PendingActivation) { p.RouteDigest = [32]byte{} }),
		"image digest zero":   mut(func(p *PendingActivation) { p.ImageDigest = [32]byte{} }),
		"revision zero":       mut(func(p *PendingActivation) { p.RouteRevision = 0 }),
		"unknown phase":       mut(func(p *PendingActivation) { p.Phase = "x" }),
		"authorized+success":  mut(func(p *PendingActivation) { p.Succeeded = true }),
		"authorized+summary":  mut(func(p *PendingActivation) { p.FailureSummary = "x" }),
		"authorized+observed": mut(func(p *PendingActivation) { p.ObservedRouteDigest = [32]byte{1} }),
		"ok without observed": mutS(func(p *PendingActivation) { p.ObservedRouteDigest = [32]byte{} }),
		"ok with summary":     mutS(func(p *PendingActivation) { p.FailureSummary = "x" }),
		"fail empty summary":  mutF(func(p *PendingActivation) { p.FailureSummary = "" }),
		"fail long summary":   mutF(func(p *PendingActivation) { p.FailureSummary = strings.Repeat("a", 2049) }),
		"fail with observed":  mutF(func(p *PendingActivation) { p.ObservedRouteDigest = [32]byte{1} }),
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			root, a := newActs(t)
			if err := a.SaveActivation(p); !errors.Is(err, ErrInvalidActivation) {
				t.Fatalf("err=%v", err)
			}
			entries, _ := os.ReadDir(filepath.Join(root, "activations"))
			if len(entries) != 0 {
				t.Fatalf("wrote files: %v", entries)
			}
		})
	}
	_, a := newActs(t)
	p := failed(1)
	p.FailureSummary = strings.Repeat("a", 2048)
	if err := a.SaveActivation(p); err != nil {
		t.Fatalf("2048 bytes must be accepted: %v", err)
	}
}

func TestSaveActivationTransitions(t *testing.T) {
	_, a := newActs(t)
	if err := a.SaveActivation(authorized(1)); err != nil {
		t.Fatal(err)
	}
	first, _, _ := a.LoadActivation(1)
	if err := a.SaveActivation(authorized(1)); err != nil { // same-phase rewrite
		t.Fatal(err)
	}
	if err := a.SaveActivation(succeeded(1)); err != nil {
		t.Fatal(err)
	}
	got, _, _ := a.LoadActivation(1)
	if !got.CreatedAt.Equal(first.CreatedAt) {
		t.Fatal("CreatedAt must be preserved")
	}
	if err := a.SaveActivation(authorized(1)); !errors.Is(err, ErrActivationRegressed) {
		t.Fatalf("regress: %v", err)
	}
	if after, _, _ := a.LoadActivation(1); after.Phase != PhaseResultReady {
		t.Fatal("regression changed stored phase")
	}
	imm := map[string]func(*PendingActivation){
		"fence":    func(p *PendingActivation) { p.Fence++ },
		"env":      func(p *PendingActivation) { p.EnvironmentID = "other" },
		"op":       func(p *PendingActivation) { p.OperationID = "43" },
		"route":    func(p *PendingActivation) { p.RouteDigest = [32]byte{9} },
		"image":    func(p *PendingActivation) { p.ImageDigest = [32]byte{9} },
		"revision": func(p *PendingActivation) { p.RouteRevision++ },
	}
	for name, f := range imm {
		p := succeeded(1)
		f(&p)
		if err := a.SaveActivation(p); !errors.Is(err, ErrActivationImmutable) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestActivationCorruptionFailsClosed(t *testing.T) {
	good := func(t *testing.T, a ActivationStore) []byte {
		t.Helper()
		if err := a.SaveActivation(authorized(1)); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(a.path(1))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	cases := map[string]func(good []byte) []byte{
		"bad json":      func([]byte) []byte { return []byte("{not json") },
		"unknown field": func(g []byte) []byte { return []byte(strings.Replace(string(g), "{", `{"extra":1,`, 1)) },
		"trailing data": func(g []byte) []byte { return append(append([]byte{}, g...), []byte(" {}")...) },
		"name mismatch": func(g []byte) []byte {
			return []byte(strings.Replace(string(g), `"attempt_id":1`, `"attempt_id":2`, 1))
		},
		"invalid":   func(g []byte) []byte { return []byte(strings.Replace(string(g), `"fence":3`, `"fence":0`, 1)) },
		"upper hex": func(g []byte) []byte { return []byte(strings.Replace(string(g), "01000000", "0A000000", 1)) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			_, a := newActs(t)
			g := good(t, a)
			if err := os.WriteFile(a.path(1), mutate(g), 0600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := a.LoadActivation(1); !errors.Is(err, ErrCorruptState) {
				t.Fatalf("load: %v", err)
			}
			if _, err := a.ListActivations(); !errors.Is(err, ErrCorruptState) {
				t.Fatalf("list: %v", err)
			}
			if err := a.SaveActivation(authorized(1)); !errors.Is(err, ErrCorruptState) {
				t.Fatalf("save over corrupt: %v", err)
			}
		})
	}
	t.Run("stray file", func(t *testing.T) {
		_, a := newActs(t)
		if err := os.WriteFile(filepath.Join(a.dir, "notes.txt"), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := a.ListActivations(); !errors.Is(err, ErrCorruptState) {
			t.Fatalf("list: %v", err)
		}
	})
	t.Run("file named for other attempt", func(t *testing.T) {
		_, a := newActs(t)
		g := good(t, a)
		if err := os.WriteFile(a.path(7), g, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := a.ListActivations(); !errors.Is(err, ErrCorruptState) {
			t.Fatalf("list: %v", err)
		}
	})
}

func TestActivationTempFileIgnored(t *testing.T) {
	_, a := newActs(t)
	if err := a.SaveActivation(authorized(1)); err != nil {
		t.Fatal(err)
	}
	// Exactly what writeFileAtomic's os.CreateTemp pattern produces.
	if err := os.WriteFile(filepath.Join(a.dir, ".fence-123456.tmp"), []byte("garbage"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := a.ListActivations()
	if err != nil || len(got) != 1 {
		t.Fatalf("list=%+v err=%v", got, err)
	}
}

func TestActivationPermissions(t *testing.T) {
	root, a := newActs(t)
	if err := a.SaveActivation(authorized(1)); err != nil {
		t.Fatal(err)
	}
	di, err := os.Stat(filepath.Join(root, "activations"))
	if err != nil || di.Mode().Perm() != 0700 {
		t.Fatalf("dir perm: %v %v", di, err)
	}
	fi, err := os.Stat(a.path(1))
	if err != nil || fi.Mode().Perm() != 0600 {
		t.Fatalf("file perm: %v %v", fi, err)
	}
}

func newRevs(t *testing.T, root string) RouteRevisionStore {
	t.Helper()
	s, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.RouteRevisions()
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRouteRevision(t *testing.T) {
	root := t.TempDir()
	r := newRevs(t, root)
	if v, err := r.LoadRouteRevision("env_1"); v != 0 || err != nil {
		t.Fatalf("absent: %d %v", v, err)
	}
	if err := r.RecordRouteRevision("env_1", 5); err != nil {
		t.Fatal(err)
	}
	if err := r.RecordRouteRevision("env_1", 5); err != nil {
		t.Fatalf("equal: %v", err)
	}
	if err := r.RecordRouteRevision("env_1", 9); err != nil {
		t.Fatal(err)
	}
	if err := r.RecordRouteRevision("env_1", 8); !errors.Is(err, ErrRouteRevisionRegressed) {
		t.Fatalf("decrease: %v", err)
	}
	if err := r.RecordRouteRevision("env_1", 0); err == nil {
		t.Fatal("zero must be refused")
	}
	if err := r.RecordRouteRevision("env_2", 1); err != nil {
		t.Fatal(err)
	}
	r2 := newRevs(t, root) // restart
	if v, _ := r2.LoadRouteRevision("env_1"); v != 9 {
		t.Fatalf("survive restart: %d", v)
	}
	if v, _ := r2.LoadRouteRevision("env_2"); v != 1 {
		t.Fatalf("per-env: %d", v)
	}
	fi, err := os.Stat(r.mustPath(t, "env_1"))
	if err != nil || fi.Mode().Perm() != 0600 {
		t.Fatalf("perm: %v %v", fi, err)
	}
}

func (r RouteRevisionStore) mustPath(t *testing.T, env string) string {
	t.Helper()
	p, err := r.path(env)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRouteRevisionCorruptFailsClosed(t *testing.T) {
	for name, body := range map[string]string{
		"bad json":     "{x",
		"unknown":      `{"environment_id":"env_1","revision":3,"x":1}`,
		"wrong env":    `{"environment_id":"other","revision":3}`,
		"non positive": `{"environment_id":"env_1","revision":0}`,
	} {
		t.Run(name, func(t *testing.T) {
			r := newRevs(t, t.TempDir())
			if err := os.WriteFile(r.mustPath(t, "env_1"), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := r.LoadRouteRevision("env_1"); !errors.Is(err, ErrCorruptState) {
				t.Fatalf("load: %v", err)
			}
			if err := r.RecordRouteRevision("env_1", 10); !errors.Is(err, ErrCorruptState) {
				t.Fatalf("record: %v", err)
			}
		})
	}
}

func TestResultReadyResultIsImmutable(t *testing.T) {
	changes := map[string]func(*PendingActivation){
		"succeeded": func(p *PendingActivation) {
			p.Succeeded = false
			p.ObservedRouteDigest = [32]byte{}
			p.FailureSummary = "x"
		},
		"observed digest": func(p *PendingActivation) { p.ObservedRouteDigest = [32]byte{7} },
		"summary":         func(p *PendingActivation) { p.FailureSummary = "different" },
	}
	for name, f := range changes {
		t.Run(name, func(t *testing.T) {
			_, a := newActs(t)
			base := succeeded(1)
			if name == "summary" {
				base = failed(1)
			}
			if err := a.SaveActivation(base); err != nil {
				t.Fatal(err)
			}
			if err := a.SaveActivation(base); err != nil { // identical rewrite is a no-op
				t.Fatalf("same result: %v", err)
			}
			before, _ := os.ReadFile(a.path(1))
			p := base
			f(&p)
			if err := a.SaveActivation(p); !errors.Is(err, ErrActivationImmutable) {
				t.Fatalf("err=%v", err)
			}
			after, _ := os.ReadFile(a.path(1))
			if string(before) != string(after) {
				t.Fatal("file changed")
			}
		})
	}
}

func TestFailureSummaryUTF8(t *testing.T) {
	root, a := newActs(t)
	p := failed(1)
	p.FailureSummary = "bad\xffbyte"
	if err := a.SaveActivation(p); !errors.Is(err, ErrInvalidActivation) {
		t.Fatalf("err=%v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(root, "activations")); len(entries) != 0 {
		t.Fatalf("wrote files: %v", entries)
	}
	p.FailureSummary = strings.Repeat("é", 1024) // 2048 bytes, multi-byte
	if err := a.SaveActivation(p); err != nil {
		t.Fatal(err)
	}
	got, _, err := a.LoadActivation(1)
	if err != nil || got.FailureSummary != p.FailureSummary {
		t.Fatalf("round trip: err=%v", err)
	}
	if err := a.SaveActivation(p); err != nil { // identical resend
		t.Fatal(err)
	}
	if _, err := a.ListActivations(); err != nil {
		t.Fatal(err)
	}
}

func TestResultImmutableFailedToSucceeded(t *testing.T) {
	_, a := newActs(t)
	if err := a.SaveActivation(failed(1)); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(a.path(1))
	if err := a.SaveActivation(succeeded(1)); !errors.Is(err, ErrActivationImmutable) {
		t.Fatalf("err=%v", err)
	}
	after, _ := os.ReadFile(a.path(1))
	if string(before) != string(after) {
		t.Fatal("file changed")
	}
}

func TestZeroValueStoresDoNotPanic(t *testing.T) {
	var a ActivationStore
	if err := a.SaveActivation(authorized(1)); err == nil {
		t.Fatal("expected error")
	}
	if _, _, err := a.LoadActivation(1); err == nil {
		t.Fatal("expected error")
	}
	if _, err := a.ListActivations(); err == nil {
		t.Fatal("expected error")
	}
	if err := a.DeleteActivation(1); err == nil {
		t.Fatal("expected error")
	}
	var r RouteRevisionStore
	if _, err := r.LoadRouteRevision("e"); err == nil {
		t.Fatal("expected error")
	}
	if err := r.RecordRouteRevision("e", 1); err == nil {
		t.Fatal("expected error")
	}
}

func TestCorruptErrorKeepsCause(t *testing.T) {
	_, a := newActs(t)
	if err := os.WriteFile(a.path(1), []byte("{x"), 0600); err != nil {
		t.Fatal(err)
	}
	_, _, err := a.LoadActivation(1)
	if !errors.Is(err, ErrCorruptState) || err.Error() == ErrCorruptState.Error() {
		t.Fatalf("cause not visible: %v", err)
	}
}

// Many goroutines, each with its OWN store instance over one dir, race
// authorized rewrites against two different results. All instances are built
// first and released together; repeated on fresh dirs.
func TestConcurrentSaveSeparateInstances(t *testing.T) {
	for iter := 0; iter < 20; iter++ {
		root := t.TempDir()
		s, _ := New(root)
		first, _ := s.Activations()
		if err := first.SaveActivation(authorized(1)); err != nil {
			t.Fatal(err)
		}
		const n = 24
		stores := make([]ActivationStore, n)
		for i := range stores {
			st, err := New(root)
			if err != nil {
				t.Fatal(err)
			}
			if stores[i], err = st.Activations(); err != nil {
				t.Fatal(err)
			}
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		var okSucc, okFail atomic.Int32
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				a := stores[i]
				switch i % 3 {
				case 0:
					if err := a.SaveActivation(succeeded(1)); err == nil {
						okSucc.Add(1)
					} else if !errors.Is(err, ErrActivationImmutable) {
						t.Errorf("succ: %v", err)
					}
				case 1:
					if err := a.SaveActivation(failed(1)); err == nil {
						okFail.Add(1)
					} else if !errors.Is(err, ErrActivationImmutable) {
						t.Errorf("fail: %v", err)
					}
				default:
					if err := a.SaveActivation(authorized(1)); err != nil &&
						!errors.Is(err, ErrActivationRegressed) {
						t.Errorf("auth: %v", err)
					}
				}
			}(i)
		}
		close(start)
		wg.Wait()
		if okSucc.Load() > 0 && okFail.Load() > 0 {
			t.Fatalf("both results won: %d %d", okSucc.Load(), okFail.Load())
		}
		got, _, err := first.LoadActivation(1)
		if err != nil || got.Phase != PhaseResultReady || got.Succeeded != (okSucc.Load() > 0) {
			t.Fatalf("final=%+v err=%v", got, err)
		}
	}
}

func TestConcurrentRecordRouteRevision(t *testing.T) {
	for iter := 0; iter < 20; iter++ {
		root := t.TempDir()
		const n = 40
		stores := make([]RouteRevisionStore, n)
		for i := range stores {
			stores[i] = newRevs(t, root)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				rev := int64((i*7)%n + 1) // permutation of 1..n
				if err := stores[i].RecordRouteRevision("env_1", rev); err != nil &&
					!errors.Is(err, ErrRouteRevisionRegressed) {
					t.Errorf("rev %d: %v", rev, err)
				}
			}(i)
		}
		close(start)
		wg.Wait()
		if v, err := stores[0].LoadRouteRevision("env_1"); err != nil || v != n {
			t.Fatalf("final=%d err=%v", v, err)
		}
	}
}

func TestStoreCanonicalizesSymlinkedDir(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	a, err := New(link)
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(real)
	if err != nil {
		t.Fatal(err)
	}
	if a.dir != b.dir {
		t.Fatalf("dirs differ: %q vs %q", a.dir, b.dir)
	}
	aa, _ := a.Activations()
	bb, _ := b.Activations()
	if lockDir(aa.dir) != lockDir(bb.dir) {
		t.Fatal("lock not shared")
	}
}

func TestZeroValueStoreFactories(t *testing.T) {
	wd, _ := os.Getwd()
	var s Store
	if _, err := s.Activations(); !errors.Is(err, errUninitialized) {
		t.Fatalf("err=%v", err)
	}
	if _, err := s.RouteRevisions(); !errors.Is(err, errUninitialized) {
		t.Fatalf("err=%v", err)
	}
	for _, d := range []string{"activations", "route-revisions"} {
		if _, err := os.Stat(filepath.Join(wd, d)); err == nil {
			t.Fatalf("%s created in working dir", d)
		}
	}
}

func TestRouteRevisionOverlongEnvironmentID(t *testing.T) {
	r := newRevs(t, t.TempDir())
	long := strings.Repeat("/", 128) // each byte encodes to 3
	if err := r.RecordRouteRevision(long, 1); !errors.Is(err, ErrInvalidRouteRevision) {
		t.Fatalf("err=%v", err)
	}
	if _, err := r.LoadRouteRevision(long); !errors.Is(err, ErrInvalidRouteRevision) {
		t.Fatalf("err=%v", err)
	}
}

func TestScanActivations(t *testing.T) {
	root, a := newActs(t)
	dir := filepath.Join(root, "activations")
	for _, p := range []PendingActivation{failed(5), succeeded(2), authorized(9)} {
		if err := a.SaveActivation(p); err != nil {
			t.Fatal(err)
		}
	}
	good, err := os.ReadFile(a.path(2))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"attempt-3.json":   []byte("{not json"),
		"attempt-4.json":   []byte(strings.Replace(string(good), `"fence":3`, `"fence":0`, 1)),
		"attempt-6.json":   []byte(strings.Replace(string(good), `"attempt_id":2`, `"attempt_id":7`, 1)),
		"attempt-007.json": good,
		"stray.txt":        []byte("x"),
		".fence-123.tmp":   []byte("partial"),
	}
	for n, c := range files {
		if err := os.WriteFile(filepath.Join(dir, n), c, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "attempt-8.json"), 0700); err != nil {
		t.Fatal(err)
	}
	recs, bad, err := a.ScanActivations()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 || recs[0].AttemptID != 2 || recs[1].AttemptID != 5 || recs[2].AttemptID != 9 {
		t.Fatalf("records: %+v", recs)
	}
	want := map[string]bool{"attempt-3.json": true, "attempt-4.json": true, "attempt-6.json": true, "attempt-007.json": true, "stray.txt": true, "attempt-8.json": true}
	if len(bad) != len(want) {
		t.Fatalf("corrupt: %v", bad)
	}
	for _, n := range bad {
		if !want[n] {
			t.Fatalf("unexpected corrupt %q", n)
		}
	}
	for n, c := range files {
		got, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil || string(got) != string(c) {
			t.Fatalf("%s modified: %v", n, err)
		}
	}
	if _, err := a.ListActivations(); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("ListActivations must stay strict: %v", err)
	}
}

func TestScanActivationsDirFailure(t *testing.T) {
	root, a := newActs(t)
	if err := os.RemoveAll(filepath.Join(root, "activations")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.ScanActivations(); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("got %v", err)
	}
	if _, _, err := (ActivationStore{}).ScanActivations(); err == nil {
		t.Fatal("zero value must error")
	}
}
