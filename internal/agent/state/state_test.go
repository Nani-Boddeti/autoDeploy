package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fence := Fence{EnvironmentID: "env_1", HighestAcceptedFence: 7, ReconciliationRequired: true}
	if err := store.Save(fence); err != nil {
		t.Fatal(err)
	}
	got, existed, err := store.Load("env_1")
	if err != nil || !existed || got != fence {
		t.Fatalf("got=%+v existed=%v err=%v", got, existed, err)
	}
}

// TestBrandNewStateDirIsLegitimate covers the explicit non-fail-closed
// case: no index and no fence files at all is what every state directory
// looks like before its first Save, and must not be confused with a lost
// index.
func TestBrandNewStateDirIsLegitimate(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got, existed, err := store.Load("env_never_seen")
	if err != nil {
		t.Fatalf("missing state must not error: %v", err)
	}
	if existed {
		t.Fatal("missing state must report existed=false")
	}
	if got.HighestAcceptedFence != 0 || got.ReconciliationRequired {
		t.Fatalf("missing state must be zero: %+v", got)
	}
}

func TestSaveRecordsEnvironmentInIndex(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(Fence{EnvironmentID: "env_idx", HighestAcceptedFence: 1}); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(filepath.Join(dir, indexFileName))
	if err != nil {
		t.Fatal(err)
	}
	var idx index
	if err := json.Unmarshal(contents, &idx); err != nil {
		t.Fatal(err)
	}
	if len(idx.Environments) != 1 || idx.Environments[0] != "env_idx" {
		t.Fatalf("index = %+v", idx)
	}
}

func TestLoadListedButFileMissingFailsClosed(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(Fence{EnvironmentID: "env_lost", HighestAcceptedFence: 1}); err != nil {
		t.Fatal(err)
	}
	// Simulate the fence file being lost (deleted, disk failure) while the
	// index still records that this agent once accepted work here.
	if err := os.Remove(fenceFilePath(t, dir, "env_lost")); err != nil {
		t.Fatal(err)
	}
	if _, existed, err := store.Load("env_lost"); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("expected ErrCorruptState for an indexed-but-missing fence file, got existed=%v err=%v", existed, err)
	}
}

func TestLoadFileNotListedFailsClosed(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	// A fence file present without any index entry: this is exactly the
	// shape Save's ordering (file first, index second) leaves behind after
	// a crash between the two writes, and must fail closed rather than
	// trust the orphaned file.
	fence := Fence{EnvironmentID: "env_orphan", HighestAcceptedFence: 4}
	contents, err := json.Marshal(fence)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fenceFilePath(t, dir, "env_orphan"), contents, 0600); err != nil {
		t.Fatal(err)
	}
	if _, existed, err := store.Load("env_orphan"); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("expected ErrCorruptState for an un-indexed fence file, got existed=%v err=%v", existed, err)
	}
}

// TestSimulatedCrashBetweenFenceWriteAndIndexUpdate directly exercises
// Save's documented crash ordering: call writeFileAtomic for the fence file
// exactly as Save does, but stop before addToIndex runs. Load must then
// fail closed for that environment (the same "not listed" bucket as
// TestLoadFileNotListedFailsClosed), and must not affect any other,
// properly-saved environment.
func TestSimulatedCrashBetweenFenceWriteAndIndexUpdate(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(Fence{EnvironmentID: "env_ok", HighestAcceptedFence: 2}); err != nil {
		t.Fatal(err)
	}
	fence := Fence{EnvironmentID: "env_crash", HighestAcceptedFence: 9}
	path, err := store.path(fence.EnvironmentID)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := json.Marshal(fence)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(dir, path, contents); err != nil {
		t.Fatal(err)
	}
	// "Crash" here: addToIndex is never called for env_crash.

	if _, existed, err := store.Load("env_crash"); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("expected ErrCorruptState after a simulated crash before the index update, got existed=%v err=%v", existed, err)
	}
	got, existed, err := store.Load("env_ok")
	if err != nil || !existed || got.HighestAcceptedFence != 2 {
		t.Fatalf("unrelated environment must be unaffected: got=%+v existed=%v err=%v", got, existed, err)
	}
}

func TestLoadMissingIndexWithFenceFilesFailsClosedForEveryEnvironment(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(Fence{EnvironmentID: "env_a", HighestAcceptedFence: 1}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(Fence{EnvironmentID: "env_b", HighestAcceptedFence: 1}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, indexFileName)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Load("env_a"); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("expected ErrCorruptState for env_a with a lost index, got %v", err)
	}
	if _, _, err := store.Load("env_b"); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("expected ErrCorruptState for env_b with a lost index, got %v", err)
	}
	// Even an environment neither Saved nor ever seen must fail closed:
	// the state directory is not trustworthy at all once fence files exist
	// with no index.
	if _, _, err := store.Load("env_never_seen"); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("expected ErrCorruptState for an unseen environment while the index is lost, got %v", err)
	}
}

func TestLoadCorruptIndexFailsClosed(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(Fence{EnvironmentID: "env_c", HighestAcceptedFence: 1}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, indexFileName), []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Load("env_c"); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("expected ErrCorruptState for a corrupt index, got %v", err)
	}
}

func TestLoadCorruptFenceFileFailsClosed(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(Fence{EnvironmentID: "env_2", HighestAcceptedFence: 1}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fenceFilePath(t, dir, "env_2"), []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	_, existed, err := store.Load("env_2")
	if !errors.Is(err, ErrCorruptState) {
		t.Fatalf("expected ErrCorruptState, got existed=%v err=%v", existed, err)
	}
}

func TestLoadCorruptEnvironmentIDMismatchFailsClosed(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(Fence{EnvironmentID: "env_3", HighestAcceptedFence: 1}); err != nil {
		t.Fatal(err)
	}
	// Overwrite with a validly-shaped but wrong environment_id: Load for
	// "env_3" must reject this as corrupt rather than trust a swapped file.
	swapped := `{"environment_id":"env_other","highest_accepted_fence":1,"reconciliation_required":false}`
	if err := os.WriteFile(fenceFilePath(t, dir, "env_3"), []byte(swapped), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Load("env_3"); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("expected ErrCorruptState, got %v", err)
	}
}

func TestSaveRejectsInvalidFence(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(Fence{EnvironmentID: "", HighestAcceptedFence: 1}); err == nil {
		t.Fatal("expected error for empty environment id")
	}
	if err := store.Save(Fence{EnvironmentID: "env_4", HighestAcceptedFence: -1}); err == nil {
		t.Fatal("expected error for negative fence")
	}
}

func TestFenceStateFilenameEscapesPathSeparators(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	tricky := "../../etc/passwd"
	if !validOpaqueForTest(tricky) {
		t.Skip("id no longer opaque-valid; adjust fixture")
	}
	if err := store.Save(Fence{EnvironmentID: tricky, HighestAcceptedFence: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "etc", "passwd")); err == nil {
		t.Fatal("state escaped the state directory")
	}
	got, existed, err := store.Load(tricky)
	if err != nil || !existed || got.HighestAcceptedFence != 1 {
		t.Fatalf("got=%+v existed=%v err=%v", got, existed, err)
	}
}

// fenceFilePath resolves the exact on-disk path Store uses for
// environmentID, via the package-private path method, so tests never
// duplicate encodeName's algorithm.
func fenceFilePath(t *testing.T, dir, environmentID string) string {
	t.Helper()
	s := Store{dir: dir}
	path, err := s.path(environmentID)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// validOpaqueForTest mirrors github.ValidOpaqueID's bound without importing
// it just for this one fixture guard.
func validOpaqueForTest(s string) bool { return len(s) > 0 && len(s) <= 128 }
