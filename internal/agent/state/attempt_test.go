package state

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func newAttemptStore(t *testing.T) (AttemptStore, string) {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Attempts()
	if err != nil {
		t.Fatal(err)
	}
	return a, a.dir
}

func sampleAttempt(id int64) AttemptRecord {
	return AttemptRecord{AttemptID: id, JobID: 7, DeploymentID: "dep_1", EnvironmentID: "env_1", Fence: 3, Phase: AttemptPhaseClaimed}
}

func TestAttemptRoundTripUpdateDeleteList(t *testing.T) {
	a, _ := newAttemptStore(t)
	if err := a.SaveAttempt(sampleAttempt(2)); err != nil {
		t.Fatal(err)
	}
	if err := a.SaveAttempt(sampleAttempt(1)); err != nil {
		t.Fatal(err)
	}
	if err := a.SetAttemptPhase(2, "building"); err != nil {
		t.Fatal(err)
	}
	got, ok, err := a.LoadAttempt(2)
	if err != nil || !ok || got.Phase != "building" || got.Fence != 3 || got.JobID != 7 || got.DeploymentID != "dep_1" || got.EnvironmentID != "env_1" || got.UpdatedAt.IsZero() {
		t.Fatalf("load = %+v ok=%v err=%v", got, ok, err)
	}
	list, err := a.ListAttempts()
	if err != nil || len(list) != 2 || list[0].AttemptID != 1 || list[1].AttemptID != 2 {
		t.Fatalf("list = %+v err=%v", list, err)
	}
	if err := a.DeleteAttempt(1); err != nil {
		t.Fatal(err)
	}
	if err := a.DeleteAttempt(1); err != nil {
		t.Fatalf("delete must be idempotent: %v", err)
	}
	if _, ok, err := a.LoadAttempt(1); ok || err != nil {
		t.Fatalf("deleted record loaded: ok=%v err=%v", ok, err)
	}
	if err := a.SetAttemptPhase(99, "building"); !errors.Is(err, ErrInvalidAttempt) {
		t.Fatalf("phase update of a missing record = %v, want ErrInvalidAttempt", err)
	}
	r := sampleAttempt(2)
	r.Fence = 4
	if err := a.SaveAttempt(r); !errors.Is(err, ErrInvalidAttempt) {
		t.Fatalf("changing the fence of an existing record = %v, want ErrInvalidAttempt", err)
	}
}

func TestAttemptFileMode0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix modes")
	}
	a, dir := newAttemptStore(t)
	if err := a.SaveAttempt(sampleAttempt(1)); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "attempt-1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}
	di, err := os.Stat(dir)
	if err != nil || di.Mode().Perm() != 0700 {
		t.Fatalf("dir mode = %v err=%v, want 0700", di.Mode().Perm(), err)
	}
}

// A crash between the temp write and the rename leaves the previous committed
// file untouched and a stray temp that listing ignores.
func TestAttemptCrashBeforeRenameKeepsOldFile(t *testing.T) {
	a, dir := newAttemptStore(t)
	if err := a.SaveAttempt(sampleAttempt(1)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".fence-crashed.tmp"), []byte(`{"version":1,"attempt_id":1,"phase":"starting"`), 0600); err != nil {
		t.Fatal(err)
	}
	list, err := a.ListAttempts()
	if err != nil || len(list) != 1 || list[0].Phase != AttemptPhaseClaimed {
		t.Fatalf("list = %+v err=%v, want the old committed record", list, err)
	}
	// A failed rename (target is a directory) must leave no committed change.
	b, bdir := newAttemptStore(t)
	if err := os.Mkdir(filepath.Join(bdir, "attempt-5.json"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := b.SaveAttempt(sampleAttempt(5)); err == nil {
		t.Fatal("expected save to fail when the target cannot be replaced")
	}
}

func TestAttemptCorruptFilesFailClosed(t *testing.T) {
	good := `{"version":1,"attempt_id":1,"job_id":7,"deployment_id":"dep_1","environment_id":"env_1","fence":3,"phase":"building","updated_at":"2026-11-21T00:00:00Z"}`
	cases := map[string]string{
		"malformed":       `not json`,
		"unknown field":   `{"version":1,"attempt_id":1,"job_id":7,"deployment_id":"dep_1","environment_id":"env_1","fence":3,"phase":"building","updated_at":"2026-11-21T00:00:00Z","extra":true}`,
		"wrong version":   `{"version":2,"attempt_id":1,"job_id":7,"deployment_id":"dep_1","environment_id":"env_1","fence":3,"phase":"building","updated_at":"2026-11-21T00:00:00Z"}`,
		"missing version": `{"attempt_id":1,"job_id":7,"deployment_id":"dep_1","environment_id":"env_1","fence":3,"phase":"building","updated_at":"2026-11-21T00:00:00Z"}`,
		"trailing data":   good + ` {}`,
		"bad phase":       `{"version":1,"attempt_id":1,"job_id":7,"deployment_id":"dep_1","environment_id":"env_1","fence":3,"phase":"Bad Phase","updated_at":"2026-11-21T00:00:00Z"}`,
		"zero fence":      `{"version":1,"attempt_id":1,"job_id":7,"deployment_id":"dep_1","environment_id":"env_1","fence":0,"phase":"building","updated_at":"2026-11-21T00:00:00Z"}`,
		"id mismatch":     `{"version":1,"attempt_id":2,"job_id":7,"deployment_id":"dep_1","environment_id":"env_1","fence":3,"phase":"building","updated_at":"2026-11-21T00:00:00Z"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			a, dir := newAttemptStore(t)
			if err := os.WriteFile(filepath.Join(dir, "attempt-1.json"), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := a.LoadAttempt(1); !errors.Is(err, ErrCorruptState) {
				t.Fatalf("load = %v, want ErrCorruptState", err)
			}
			if _, err := a.ListAttempts(); !errors.Is(err, ErrCorruptState) {
				t.Fatalf("list = %v, want ErrCorruptState", err)
			}
			if err := a.SetAttemptPhase(1, "starting"); !errors.Is(err, ErrCorruptState) {
				t.Fatalf("set phase = %v, want ErrCorruptState", err)
			}
		})
	}
	t.Run("stray entry", func(t *testing.T) {
		a, dir := newAttemptStore(t)
		if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := a.ListAttempts(); !errors.Is(err, ErrCorruptState) {
			t.Fatalf("list = %v, want ErrCorruptState", err)
		}
	})
	t.Run("valid", func(t *testing.T) {
		a, dir := newAttemptStore(t)
		if err := os.WriteFile(filepath.Join(dir, "attempt-1.json"), []byte(good), 0600); err != nil {
			t.Fatal(err)
		}
		if r, ok, err := a.LoadAttempt(1); err != nil || !ok || r.Phase != "building" {
			t.Fatalf("load = %+v %v %v", r, ok, err)
		}
	})
}

// Attempt records live in a subdirectory, so they never look like fence files
// to the fence store's brand-new-directory check.
func TestAttemptRecordsDoNotAffectFenceStore(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Attempts()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SaveAttempt(sampleAttempt(1)); err != nil {
		t.Fatal(err)
	}
	if _, existed, err := s.Load("env_new"); err != nil || existed {
		t.Fatalf("fence load = existed %v err %v, want a clean first encounter", existed, err)
	}
}
