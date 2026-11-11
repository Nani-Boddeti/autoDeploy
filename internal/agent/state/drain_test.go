package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newDrains(t *testing.T) (string, DrainStore) {
	t.Helper()
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.Drains()
	if err != nil {
		t.Fatal(err)
	}
	return root, d
}

func drainRec(id int64) PendingDrain {
	return PendingDrain{
		AttemptID: id, EnvironmentID: "env_1",
		ReleaseID: "rel_new", BackendRuntimeInstanceID: "42",
		PredecessorReleaseID: "rel_old", PredecessorRuntimeInstanceID: "41",
		DrainSeconds: 30, ArmedAt: time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC),
		ObservedRouteDigest: [32]byte{7}, Phase: DrainPending,
	}
}

func TestDrainRoundTripAndRestart(t *testing.T) {
	root, d := newDrains(t)
	failed := drainRec(2)
	failed.Phase, failed.FailureSummary, failed.Attempts = DrainStopFailed, "engine refused: é", 1
	for _, p := range []PendingDrain{drainRec(1), failed} {
		if err := d.SaveDrain(p); err != nil {
			t.Fatal(err)
		}
	}
	s2, _ := New(root)
	d2, err := s2.Drains()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []PendingDrain{drainRec(1), failed} {
		got, ok, err := d2.LoadDrain(want.AttemptID)
		if err != nil || !ok {
			t.Fatalf("load %d: ok=%v err=%v", want.AttemptID, ok, err)
		}
		if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
			t.Fatal("timestamps not set")
		}
		got.CreatedAt, got.UpdatedAt = time.Time{}, time.Time{}
		if got != want {
			t.Fatalf("got %+v want %+v", got, want)
		}
	}
	if _, ok, err := d2.LoadDrain(99); ok || err != nil {
		t.Fatalf("absent: ok=%v err=%v", ok, err)
	}
	recs, bad, err := d2.ScanDrains()
	if err != nil || len(bad) != 0 || len(recs) != 2 || recs[0].AttemptID != 1 {
		t.Fatalf("scan: %v %v %v", recs, bad, err)
	}
	if err := d2.DeleteDrain(1); err != nil {
		t.Fatal(err)
	}
	if err := d2.DeleteDrain(1); err != nil { // idempotent
		t.Fatal(err)
	}
	if _, ok, _ := d2.LoadDrain(1); ok {
		t.Fatal("still present")
	}
}

func TestSaveDrainValidation(t *testing.T) {
	mut := func(f func(*PendingDrain)) PendingDrain { p := drainRec(1); f(&p); return p }
	cases := map[string]PendingDrain{
		"attempt zero":     mut(func(p *PendingDrain) { p.AttemptID = 0 }),
		"env empty":        mut(func(p *PendingDrain) { p.EnvironmentID = "" }),
		"release empty":    mut(func(p *PendingDrain) { p.ReleaseID = "" }),
		"backend invalid":  mut(func(p *PendingDrain) { p.BackendRuntimeInstanceID = "bad id!" }),
		"backend empty":    mut(func(p *PendingDrain) { p.BackendRuntimeInstanceID = "" }),
		"pred rel empty":   mut(func(p *PendingDrain) { p.PredecessorReleaseID = "" }),
		"pred rt invalid":  mut(func(p *PendingDrain) { p.PredecessorRuntimeInstanceID = "../x" }),
		"target is new rt": mut(func(p *PendingDrain) { p.PredecessorRuntimeInstanceID = p.BackendRuntimeInstanceID }),
		"target is new rl": mut(func(p *PendingDrain) { p.PredecessorReleaseID = p.ReleaseID }),
		"drain zero":       mut(func(p *PendingDrain) { p.DrainSeconds = 0 }),
		"drain 301":        mut(func(p *PendingDrain) { p.DrainSeconds = 301 }),
		"armed zero":       mut(func(p *PendingDrain) { p.ArmedAt = time.Time{} }),
		"digest zero":      mut(func(p *PendingDrain) { p.ObservedRouteDigest = [32]byte{} }),
		"attempts neg":     mut(func(p *PendingDrain) { p.Attempts = -1 }),
		"unknown phase":    mut(func(p *PendingDrain) { p.Phase = "x" }),
		"pending+summary":  mut(func(p *PendingDrain) { p.FailureSummary = "x" }),
		"failed no summ":   mut(func(p *PendingDrain) { p.Phase = DrainStopFailed }),
		"failed long":      mut(func(p *PendingDrain) { p.Phase, p.FailureSummary = DrainStopFailed, strings.Repeat("a", 2049) }),
		"failed bad utf8":  mut(func(p *PendingDrain) { p.Phase, p.FailureSummary = DrainStopFailed, "a\xffb" }),
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			root, d := newDrains(t)
			if err := d.SaveDrain(p); !errors.Is(err, ErrInvalidDrain) {
				t.Fatalf("err=%v", err)
			}
			if entries, _ := os.ReadDir(filepath.Join(root, "drains")); len(entries) != 0 {
				t.Fatalf("wrote files: %v", entries)
			}
		})
	}
	_, d := newDrains(t)
	p := drainRec(1)
	p.DrainSeconds = 300
	p.Phase, p.FailureSummary = DrainStopFailed, strings.Repeat("a", 2048)
	if err := d.SaveDrain(p); err != nil {
		t.Fatalf("boundary values must be accepted: %v", err)
	}
}

func TestSaveDrainImmutableFields(t *testing.T) {
	changes := map[string]func(*PendingDrain){
		"env":      func(p *PendingDrain) { p.EnvironmentID = "env_2" },
		"release":  func(p *PendingDrain) { p.ReleaseID = "rel_x" },
		"backend":  func(p *PendingDrain) { p.BackendRuntimeInstanceID = "43" },
		"pred rel": func(p *PendingDrain) { p.PredecessorReleaseID = "rel_y" },
		"pred rt":  func(p *PendingDrain) { p.PredecessorRuntimeInstanceID = "40" },
		"drain":    func(p *PendingDrain) { p.DrainSeconds = 31 },
		"digest":   func(p *PendingDrain) { p.ObservedRouteDigest = [32]byte{8} },
	}
	for name, f := range changes {
		t.Run(name, func(t *testing.T) {
			_, d := newDrains(t)
			if err := d.SaveDrain(drainRec(1)); err != nil {
				t.Fatal(err)
			}
			p := drainRec(1)
			f(&p)
			if err := d.SaveDrain(p); !errors.Is(err, ErrDrainImmutable) {
				t.Fatalf("SaveDrain err=%v", err)
			}
			if err := d.ArmDrain(p); !errors.Is(err, ErrDrainImmutable) {
				t.Fatalf("ArmDrain err=%v", err)
			}
		})
	}
}

func TestSaveDrainTransitionsAndMonotonicAttempts(t *testing.T) {
	_, d := newDrains(t)
	with := func(ph DrainPhase, attempts int) PendingDrain {
		p := drainRec(1)
		p.Phase, p.Attempts = ph, attempts
		if ph == DrainStopFailed {
			p.FailureSummary = "boom"
		}
		return p
	}
	steps := []struct {
		p   PendingDrain
		err error
	}{
		{with(DrainPending, 0), nil},
		{with(DrainPending, 1), nil},    // retry in flight
		{with(DrainStopFailed, 1), nil}, // pending -> stop_failed
		{with(DrainStopFailed, 1), nil}, // identical no-op
		{with(DrainStopFailed, 2), nil}, // stop_failed -> stop_failed
		{with(DrainStopFailed, 1), ErrDrainRegressed},
		{with(DrainPending, 2), ErrDrainRegressed}, // stop_failed -> pending needs higher attempts
		{with(DrainPending, 1), ErrDrainRegressed},
		{with(DrainPending, 3), nil}, // bounded retry
	}
	for i, s := range steps {
		if err := d.SaveDrain(s.p); !errors.Is(err, s.err) || (s.err == nil && err != nil) {
			t.Fatalf("step %d: err=%v want %v", i, err, s.err)
		}
	}
	got, _, _ := d.LoadDrain(1)
	if got.Phase != DrainPending || got.Attempts != 3 {
		t.Fatalf("final %+v", got)
	}
}

func TestDrainArmedAtPreserved(t *testing.T) {
	_, d := newDrains(t)
	first := drainRec(1)
	if err := d.SaveDrain(first); err != nil {
		t.Fatal(err)
	}
	later := drainRec(1)
	later.ArmedAt = first.ArmedAt.Add(time.Hour)
	if err := d.SaveDrain(later); err != nil {
		t.Fatal(err)
	}
	if err := d.ArmDrain(later); err != nil {
		t.Fatal(err)
	}
	later.Phase, later.FailureSummary, later.Attempts = DrainStopFailed, "x", 1
	if err := d.SaveDrain(later); err != nil {
		t.Fatal(err)
	}
	got, _, _ := d.LoadDrain(1)
	if !got.ArmedAt.Equal(first.ArmedAt) {
		t.Fatalf("armed_at moved: %v", got.ArmedAt)
	}
}

func TestArmDrainDoesNotRewindProgress(t *testing.T) {
	_, d := newDrains(t)
	p := drainRec(1)
	p.Phase, p.FailureSummary, p.Attempts = DrainStopFailed, "x", 2
	if err := d.SaveDrain(p); err != nil {
		t.Fatal(err)
	}
	if err := d.ArmDrain(drainRec(1)); err != nil {
		t.Fatal(err)
	}
	got, _, _ := d.LoadDrain(1)
	if got.Phase != DrainStopFailed || got.Attempts != 2 {
		t.Fatalf("re-arm rewound the record: %+v", got)
	}
	if err := d.ArmDrain(p); !errors.Is(err, ErrInvalidDrain) {
		t.Fatalf("arming a non-pending record: %v", err)
	}
}

func TestSaveDrainFirstIdenticalSaveIsNoOp(t *testing.T) {
	_, d := newDrains(t)
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	d.now = func() time.Time { return clock }
	if err := d.SaveDrain(drainRec(1)); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Minute)
	if err := d.SaveDrain(drainRec(1)); err != nil {
		t.Fatal(err)
	}
	got, _, _ := d.LoadDrain(1)
	if !got.UpdatedAt.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("identical save rewrote the file: %v", got.UpdatedAt)
	}
}

func TestScanDrainsCorruptShapes(t *testing.T) {
	root, d := newDrains(t)
	if err := d.SaveDrain(drainRec(1)); err != nil {
		t.Fatal(err)
	}
	good, _ := os.ReadFile(d.path(1))
	dir := filepath.Join(root, "drains")
	write := func(name string, b []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("attempt-2.json", []byte("{nope"))
	write("attempt-3.json", []byte(strings.Replace(string(good), "{", `{"extra":1,`, 1)))
	write("attempt-4.json", good) // name/content mismatch
	write("attempt-05.json", good)
	write("stray.txt", []byte("x"))
	write(".fence-abc.tmp", []byte("partial")) // atomic temp: skipped
	if err := os.Mkdir(filepath.Join(dir, "attempt-6.json"), 0700); err != nil {
		t.Fatal(err)
	}
	recs, bad, err := d.ScanDrains()
	if err != nil || len(recs) != 1 || recs[0].AttemptID != 1 {
		t.Fatalf("recs=%v err=%v", recs, err)
	}
	if len(bad) != 6 {
		t.Fatalf("corrupt=%v", bad)
	}
	if _, _, err := d.LoadDrain(2); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("load corrupt: %v", err)
	}
	if err := d.SaveDrain(drainRec(2)); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("save over corrupt must fail closed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "attempt-2.json")); err != nil {
		t.Fatal("corrupt file was modified or removed")
	}
}

func TestDrainPermissionsAndZeroValue(t *testing.T) {
	root, d := newDrains(t)
	if err := d.SaveDrain(drainRec(1)); err != nil {
		t.Fatal(err)
	}
	if di, err := os.Stat(filepath.Join(root, "drains")); err != nil || di.Mode().Perm() != 0700 {
		t.Fatalf("dir perm: %v %v", di, err)
	}
	if fi, err := os.Stat(d.path(1)); err != nil || fi.Mode().Perm() != 0600 {
		t.Fatalf("file perm: %v %v", fi, err)
	}
	var z DrainStore
	if err := z.SaveDrain(drainRec(1)); err == nil {
		t.Fatal("zero store save")
	}
	if err := z.ArmDrain(drainRec(1)); err == nil {
		t.Fatal("zero store arm")
	}
	if _, _, err := z.LoadDrain(1); err == nil {
		t.Fatal("zero store load")
	}
	if _, _, err := z.ScanDrains(); err == nil {
		t.Fatal("zero store scan")
	}
	if err := z.DeleteDrain(1); err == nil {
		t.Fatal("zero store delete")
	}
	if _, err := (Store{}).Drains(); err == nil {
		t.Fatal("zero Store.Drains")
	}
}

func withPredecessor(p PendingActivation) PendingActivation {
	p.ReleaseID, p.BackendRuntimeInstanceID = "rel_new", "42"
	p.PredecessorReleaseID, p.PredecessorRuntimeInstanceID = "rel_old", "41"
	p.DrainSeconds = 30
	return p
}

func TestPendingActivationDrainFieldValidation(t *testing.T) {
	mut := func(f func(*PendingActivation)) PendingActivation {
		p := withPredecessor(authorized(1))
		f(&p)
		return p
	}
	cases := map[string]PendingActivation{
		"pred rel only":        mut(func(p *PendingActivation) { p.PredecessorRuntimeInstanceID = "" }),
		"pred rt only":         mut(func(p *PendingActivation) { p.PredecessorReleaseID = "" }),
		"pred rt invalid":      mut(func(p *PendingActivation) { p.PredecessorRuntimeInstanceID = "a b" }),
		"pred rel invalid":     mut(func(p *PendingActivation) { p.PredecessorReleaseID = strings.Repeat("a", 129) }),
		"pred == backend":      mut(func(p *PendingActivation) { p.PredecessorRuntimeInstanceID = p.BackendRuntimeInstanceID }),
		"pred rel == release":  mut(func(p *PendingActivation) { p.PredecessorReleaseID = p.ReleaseID }),
		"drain 0 with pred":    mut(func(p *PendingActivation) { p.DrainSeconds = 0 }),
		"drain 301 with pred":  mut(func(p *PendingActivation) { p.DrainSeconds = 301 }),
		"drain negative":       mut(func(p *PendingActivation) { p.DrainSeconds = -1 }),
		"backend invalid":      mut(func(p *PendingActivation) { p.BackendRuntimeInstanceID = "x/y" }),
		"release invalid":      mut(func(p *PendingActivation) { p.ReleaseID = strings.Repeat("a", 129) }),
		"pred without release": mut(func(p *PendingActivation) { p.ReleaseID = "" }),
		"pred without backend": mut(func(p *PendingActivation) { p.BackendRuntimeInstanceID = "" }),
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			_, a := newActs(t)
			if err := a.SaveActivation(p); !errors.Is(err, ErrInvalidActivation) {
				t.Fatalf("err=%v", err)
			}
		})
	}
	_, a := newActs(t)
	none := authorized(1) // no predecessor: zero drain seconds is fine
	if err := a.SaveActivation(none); err != nil {
		t.Fatal(err)
	}
	ok := withPredecessor(authorized(2))
	ok.DrainSeconds = 300
	if err := a.SaveActivation(ok); err != nil {
		t.Fatal(err)
	}
	got, _, err := a.LoadActivation(2)
	got.CreatedAt, got.UpdatedAt = time.Time{}, time.Time{}
	if err != nil || got != ok {
		t.Fatalf("round trip: %+v err=%v", got, err)
	}
}

func TestPendingActivationDrainFieldsImmutable(t *testing.T) {
	changes := map[string]func(*PendingActivation){
		"release":  func(p *PendingActivation) { p.ReleaseID = "rel_z" },
		"backend":  func(p *PendingActivation) { p.BackendRuntimeInstanceID = "43" },
		"pred rel": func(p *PendingActivation) { p.PredecessorReleaseID = "rel_q" },
		"pred rt":  func(p *PendingActivation) { p.PredecessorRuntimeInstanceID = "40" },
		"drain":    func(p *PendingActivation) { p.DrainSeconds = 31 },
	}
	for name, f := range changes {
		t.Run(name, func(t *testing.T) {
			_, a := newActs(t)
			if err := a.SaveActivation(withPredecessor(authorized(1))); err != nil {
				t.Fatal(err)
			}
			p := withPredecessor(succeeded(1))
			f(&p)
			if err := a.SaveActivation(p); !errors.Is(err, ErrActivationImmutable) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestOldShapeActivationFileStillLoads(t *testing.T) {
	_, a := newActs(t)
	if err := a.SaveActivation(succeeded(1)); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(a.path(1))
	old := string(b)
	for _, k := range []string{"release_id", "backend_runtime_instance_id", "predecessor_release_id", "predecessor_runtime_instance_id"} {
		old = strings.Replace(old, `,"`+k+`":""`, "", 1)
	}
	old = strings.Replace(old, `,"drain_seconds":0`, "", 1)
	if strings.Contains(old, "predecessor") || strings.Contains(old, "drain_seconds") {
		t.Fatalf("could not build an old-shape file: %s", old)
	}
	if err := os.WriteFile(a.path(1), []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	got, ok, err := a.LoadActivation(1)
	if err != nil || !ok || got.HasPredecessor() || got.DrainSeconds != 0 || got.ReleaseID != "" {
		t.Fatalf("old shape: %+v ok=%v err=%v", got, ok, err)
	}
	if _, bad, err := a.ScanActivations(); err != nil || len(bad) != 0 {
		t.Fatalf("scan: %v %v", bad, err)
	}
	// An unknown key must still be rejected.
	if err := os.WriteFile(a.path(1), []byte(strings.Replace(old, "{", `{"bogus":1,`, 1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.LoadActivation(1); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("unknown key accepted: %v", err)
	}
}

func TestDrainFromActivation(t *testing.T) {
	armed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	ok := withPredecessor(succeeded(5))
	d, owed := DrainFromActivation(ok, armed)
	if !owed || d.PredecessorRuntimeInstanceID != "41" || d.ArmedAt != armed || d.ObservedRouteDigest != ok.ObservedRouteDigest || d.Phase != DrainPending || d.validate() != nil {
		t.Fatalf("%+v owed=%v err=%v", d, owed, d.validate())
	}
	for name, p := range map[string]PendingActivation{
		"no predecessor": succeeded(5),
		"failure":        withPredecessor(failed(5)),
		"authorized":     withPredecessor(authorized(5)),
	} {
		if _, owed := DrainFromActivation(p, armed); owed {
			t.Fatalf("%s: drain owed", name)
		}
	}
}
