package traefik

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

func mkfifo(path string) error {
	return syscall.Mkfifo(path, 0640)
}

func listDirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	for _, name := range listDirNames(t, dir) {
		if strings.HasSuffix(name, routeTempFileExtension) {
			t.Fatalf("leftover temp file in %s: %s", dir, name)
		}
	}
}

func TestWriteCreatesFileAtomically(t *testing.T) {
	dir := t.TempDir()
	r := validRoute()

	result, err := Write(dir, r)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if result.Outcome != WriteOutcomeWritten {
		t.Fatalf("Outcome = %v, want WriteOutcomeWritten", result.Outcome)
	}
	if result.PreviousDigest != "" {
		t.Fatalf("PreviousDigest = %q, want empty for a fresh write", result.PreviousDigest)
	}

	finalPath := filepath.Join(dir, routeFileName(r.EnvironmentID))
	data, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", finalPath, err)
	}
	wantDigest := Digest(data)
	if result.Digest != wantDigest {
		t.Fatalf("result digest %s != on-disk digest %s", result.Digest, wantDigest)
	}

	fi, err := os.Stat(finalPath)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi.Mode().Perm() != routeFileMode {
		t.Fatalf("file mode = %v, want %v", fi.Mode().Perm(), routeFileMode)
	}

	assertNoTempFiles(t, dir)
}

func TestWriteIdempotentUnchanged(t *testing.T) {
	dir := t.TempDir()
	r := validRoute()

	first, err := Write(dir, r)
	if err != nil {
		t.Fatalf("first Write: %v", err)
	}

	finalPath := filepath.Join(dir, routeFileName(r.EnvironmentID))
	fiBefore, err := os.Stat(finalPath)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	second, err := Write(dir, r)
	if err != nil {
		t.Fatalf("second Write: %v", err)
	}
	if second.Outcome != WriteOutcomeUnchanged {
		t.Fatalf("second Outcome = %v, want WriteOutcomeUnchanged", second.Outcome)
	}
	if second.Digest != first.Digest {
		t.Fatalf("second digest %s != first digest %s", second.Digest, first.Digest)
	}
	if second.PreviousDigest != first.Digest {
		t.Fatalf("second PreviousDigest %s != first digest %s", second.PreviousDigest, first.Digest)
	}

	fiAfter, err := os.Stat(finalPath)
	if err != nil {
		t.Fatalf("Stat after: %v", err)
	}
	if !fiBefore.ModTime().Equal(fiAfter.ModTime()) {
		t.Fatalf("file was mutated on an unchanged write: mtime %v -> %v", fiBefore.ModTime(), fiAfter.ModTime())
	}
	assertNoTempFiles(t, dir)
}

func TestWriteReplacesWithNewDigestAndReportsPrevious(t *testing.T) {
	dir := t.TempDir()
	r := validRoute()

	first, err := Write(dir, r)
	if err != nil {
		t.Fatalf("first Write: %v", err)
	}

	r2 := r
	r2.RouteRevision++
	second, err := Write(dir, r2)
	if err != nil {
		t.Fatalf("second Write: %v", err)
	}
	if second.Outcome != WriteOutcomeWritten {
		t.Fatalf("second Outcome = %v, want WriteOutcomeWritten", second.Outcome)
	}
	if second.PreviousDigest != first.Digest {
		t.Fatalf("second.PreviousDigest = %s, want %s", second.PreviousDigest, first.Digest)
	}
	if second.Digest == first.Digest {
		t.Fatalf("digest did not change after mutating RouteRevision")
	}
	assertNoTempFiles(t, dir)
}

func TestWriteRefusesSymlinkedDirectory(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0750); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	_, err := Write(link, validRoute())
	if !errors.Is(err, ErrSymlinkRefused) {
		t.Fatalf("expected ErrSymlinkRefused, got %v", err)
	}
	assertNoTempFiles(t, real)
}

func TestWriteRefusesSymlinkedTargetFile(t *testing.T) {
	dir := t.TempDir()
	r := validRoute()
	finalPath := filepath.Join(dir, routeFileName(r.EnvironmentID))
	elsewhere := filepath.Join(dir, "elsewhere")
	if err := os.WriteFile(elsewhere, []byte("not a route file"), 0640); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Symlink(elsewhere, finalPath); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	_, err := Write(dir, r)
	if !errors.Is(err, ErrSymlinkRefused) {
		t.Fatalf("expected ErrSymlinkRefused, got %v", err)
	}
	assertNoTempFiles(t, dir)

	// The symlink itself must be untouched -- Write must not have replaced
	// or removed it.
	fi, err := os.Lstat(finalPath)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected the symlink to remain untouched at %s", finalPath)
	}
}

func TestWriteFaultInjectedWriteFailureLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	injected := errors.New("injected write failure")
	orig := writeHook
	writeHook = func(f *os.File, doc []byte) (int, error) {
		return 0, injected
	}
	t.Cleanup(func() { writeHook = orig })

	_, err := Write(dir, validRoute())
	if !errors.Is(err, injected) {
		t.Fatalf("expected injected write failure, got %v", err)
	}
	assertNoTempFiles(t, dir)
	if names := listDirNames(t, dir); len(names) != 0 {
		t.Fatalf("expected empty directory after failed write, got %v", names)
	}
}

func TestWriteFaultInjectedRenameFailureLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	injected := errors.New("injected rename failure")
	orig := renameHook
	renameHook = func(oldpath, newpath string) error {
		return injected
	}
	t.Cleanup(func() { renameHook = orig })

	_, err := Write(dir, validRoute())
	if !errors.Is(err, injected) {
		t.Fatalf("expected injected rename failure, got %v", err)
	}
	assertNoTempFiles(t, dir)
	if names := listDirNames(t, dir); len(names) != 0 {
		t.Fatalf("expected empty directory after failed rename, got %v", names)
	}
}

func TestWriteRejectsInvalidRoute(t *testing.T) {
	dir := t.TempDir()
	r := validRoute()
	r.Hostname = "not a hostname"
	if _, err := Write(dir, r); !errors.Is(err, ErrInvalidRoute) {
		t.Fatalf("expected ErrInvalidRoute, got %v", err)
	}
	assertNoTempFiles(t, dir)
	if names := listDirNames(t, dir); len(names) != 0 {
		t.Fatalf("expected empty directory, got %v", names)
	}
}

func TestConcurrentWritersProduceOneCompleteDocumentNoLeftoverTemps(t *testing.T) {
	dir := t.TempDir()
	r := validRoute()

	const goroutines = 20
	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rr := r
			rr.RouteRevision = uint64(i) // each writer renders a distinct valid document
			_, err := Write(dir, rr)
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: Write failed: %v", i, err)
		}
	}

	assertNoTempFiles(t, dir)
	names := listDirNames(t, dir)
	if len(names) != 1 {
		t.Fatalf("expected exactly one file in %s after concurrent writes, got %v", dir, names)
	}

	finalPath := filepath.Join(dir, routeFileName(r.EnvironmentID))
	data, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// The winning writer's content must be a complete, well-formed
	// document -- not a torn concatenation of two writers' bytes. Every
	// writer renders the same fixed shape differing only in
	// route_revision, so asserting the file parses back to exactly one of
	// the goroutines' own rendered documents is a strong proof against
	// tearing: a torn write (partial content from two renames somehow
	// interleaved) could not match any single goroutine's full rendered
	// bytes.
	matched := false
	for i := 0; i < goroutines; i++ {
		rr := r
		rr.RouteRevision = uint64(i)
		doc, _, err := Render(rr)
		if err != nil {
			t.Fatalf("Render(%d): %v", i, err)
		}
		if string(doc) == string(data) {
			matched = true
			break
		}
	}
	if !matched {
		t.Fatalf("final file content does not match any single writer's complete rendered document (possible torn write): %q", data)
	}
}

func TestRemoveIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	r := validRoute()
	if _, err := Write(dir, r); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := Remove(dir, r.EnvironmentID); err != nil {
		t.Fatalf("first Remove: %v", err)
	}
	if _, exists, err := ReadDigest(dir, r.EnvironmentID); err != nil || exists {
		t.Fatalf("expected file removed, exists=%v err=%v", exists, err)
	}
	// Second Remove on an already-absent file must succeed.
	if err := Remove(dir, r.EnvironmentID); err != nil {
		t.Fatalf("second (idempotent) Remove: %v", err)
	}
}

func TestReadDigestReportsAbsence(t *testing.T) {
	dir := t.TempDir()
	digest, exists, err := ReadDigest(dir, "env01ABCxyz")
	if err != nil {
		t.Fatalf("ReadDigest: %v", err)
	}
	if exists || digest != "" {
		t.Fatalf("expected absent, got exists=%v digest=%q", exists, digest)
	}
}

func TestReadDigestMatchesWrite(t *testing.T) {
	dir := t.TempDir()
	r := validRoute()
	result, err := Write(dir, r)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	digest, exists, err := ReadDigest(dir, r.EnvironmentID)
	if err != nil {
		t.Fatalf("ReadDigest: %v", err)
	}
	if !exists {
		t.Fatalf("expected file to exist")
	}
	if digest != result.Digest {
		t.Fatalf("ReadDigest = %s, want %s", digest, result.Digest)
	}
}

func TestWriteRefusesDirectoryNotOwnedShape(t *testing.T) {
	// This test proves ErrNotDirectory fires for a plain file passed where
	// a directory is expected -- the ownership branch itself requires
	// running as a different UID to exercise directly (not available in
	// this sandbox), so it is broken/restored by code review inspection
	// (see the task report) rather than a live test here; ErrNotDirectory
	// and ErrSymlinkRefused, which share verifyDirectory's guard chain
	// with the ownership check, are both proven by adjacent tests.
	dir := t.TempDir()
	notADir := filepath.Join(dir, "file")
	if err := os.WriteFile(notADir, []byte("x"), 0640); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := Write(notADir, validRoute()); !errors.Is(err, ErrNotDirectory) {
		t.Fatalf("expected ErrNotDirectory, got %v", err)
	}
}

// TestCreateTempFileRefusesToOverwriteOrFollowAPredictedCollision proves
// O_EXCL is not vacuous: it forces tempSuffixHook to always produce the
// same, predictable suffix (impossible to arrange against the real
// crypto/rand source in a portable test), pre-creates an ordinary regular
// file at the exact path that predicted temp name resolves to, and proves
// Write both fails and leaves that pre-existing file's content completely
// untouched -- proving createTempFile never opened it for writing at all,
// which is exactly what O_EXCL (not O_NOFOLLOW, since this pre-existing
// path is a regular file, not a symlink) is responsible for.
func TestCreateTempFileRefusesToOverwriteOrFollowAPredictedCollision(t *testing.T) {
	dir := t.TempDir()
	r := validRoute()

	const fixedSuffix = "deadbeef"
	orig := tempSuffixHook
	tempSuffixHook = func() (string, error) { return fixedSuffix, nil }
	t.Cleanup(func() { tempSuffixHook = orig })

	predictedTempPath := filepath.Join(dir, routeFilePrefix+r.EnvironmentID+"-"+fixedSuffix+routeTempFileExtension)
	const marker = "PRE-EXISTING-CONTENT-MARKER-DO-NOT-TOUCH"
	if err := os.WriteFile(predictedTempPath, []byte(marker), 0640); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := Write(dir, r)
	if err == nil {
		t.Fatalf("expected Write to fail when every temp-name attempt collides")
	}

	got, err := os.ReadFile(predictedTempPath)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", predictedTempPath, err)
	}
	if string(got) != marker {
		t.Fatalf("pre-existing file was mutated: got %q, want unchanged %q", got, marker)
	}

	// The final route file must not have been installed either -- Write
	// never got past temp-file creation.
	finalPath := filepath.Join(dir, routeFileName(r.EnvironmentID))
	if _, err := os.Lstat(finalPath); !os.IsNotExist(err) {
		t.Fatalf("expected no final route file, Lstat error = %v", err)
	}
}

func TestCheckDirectoryPolicy(t *testing.T) {
	self := uint32(os.Getuid())
	const other = 0xFFFF // not self, not root (test process is never uid 65535)
	if self == other {
		t.Skip("test process itself runs as the sentinel 'other' uid")
	}

	cases := []struct {
		name    string
		uid     uint32
		perm    os.FileMode
		wantErr error // nil means accept
	}{
		{"owned-by-agent-safe-perms", self, 0750, nil},
		{"owned-by-root-safe-perms", 0, 0750, nil},
		{"owned-by-other-safe-perms", other, 0750, ErrDirectoryOwnership},
		{"owned-by-agent-world-writable", self, 0757, ErrDirectoryPermissions},
		{"owned-by-root-world-writable", 0, 0752, ErrDirectoryPermissions},
		{"owned-by-other-world-writable", other, 0757, ErrDirectoryOwnership}, // ownership checked first
		{"owned-by-agent-group-writable", self, 0770, nil},                    // trusted owner: group-write allowed
		{"owned-by-root-group-writable", 0, 0770, nil},
		{"owned-by-other-group-writable", other, 0770, ErrDirectoryOwnership},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkDirectoryPolicy(tc.uid, tc.perm)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("checkDirectoryPolicy(%d, %v) = %v, want accept", tc.uid, tc.perm, err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("checkDirectoryPolicy(%d, %v) = %v, want %v", tc.uid, tc.perm, err, tc.wantErr)
			}
		})
	}
}

func TestVerifyDirectoryRejectsWorldWritable(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0777); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	if _, err := Write(dir, validRoute()); !errors.Is(err, ErrDirectoryPermissions) {
		t.Fatalf("expected ErrDirectoryPermissions, got %v", err)
	}
}

// TestReadRouteFileBoundedRefusesSymlink proves readRouteFileBounded's own
// O_NOFOLLOW open refuses a symlink independently of
// statExistingRouteFile's earlier Lstat-based check -- i.e. even called
// directly against a path neither Write nor ReadDigest pre-screened, it
// still refuses to read through a symlink. This is the layer that closes
// the TOCTOU window between an Lstat check and a later read.
func TestReadRouteFileBoundedRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	elsewhere := filepath.Join(dir, "elsewhere")
	if err := os.WriteFile(elsewhere, []byte("data"), 0640); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(elsewhere, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if _, err := readRouteFileBounded(link); !errors.Is(err, ErrSymlinkRefused) {
		t.Fatalf("expected ErrSymlinkRefused, got %v", err)
	}
}

func TestReadRouteFileBoundedRefusesOversized(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "huge")
	huge := make([]byte, maxRouteFileBytes+1)
	if err := os.WriteFile(path, huge, 0640); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := readRouteFileBounded(path); !errors.Is(err, ErrRouteFileTooLarge) {
		t.Fatalf("expected ErrRouteFileTooLarge, got %v", err)
	}
}

func TestWriteRefusesOversizedExistingFile(t *testing.T) {
	dir := t.TempDir()
	r := validRoute()
	finalPath := filepath.Join(dir, routeFileName(r.EnvironmentID))
	huge := make([]byte, maxRouteFileBytes+1)
	if err := os.WriteFile(finalPath, huge, 0640); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := Write(dir, r); !errors.Is(err, ErrRouteFileTooLarge) {
		t.Fatalf("expected ErrRouteFileTooLarge, got %v", err)
	}
	assertNoTempFiles(t, dir)
}

func TestReadDigestRefusesOversizedExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, routeFileName("env01ABCxyz"))
	huge := make([]byte, maxRouteFileBytes+1)
	if err := os.WriteFile(path, huge, 0640); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, _, err := ReadDigest(dir, "env01ABCxyz"); !errors.Is(err, ErrRouteFileTooLarge) {
		t.Fatalf("expected ErrRouteFileTooLarge, got %v", err)
	}
}

func TestWriteRefusesNonRegularExistingFile(t *testing.T) {
	dir := t.TempDir()
	r := validRoute()
	finalPath := filepath.Join(dir, routeFileName(r.EnvironmentID))
	// A named pipe (FIFO) is neither a symlink nor a regular file --
	// exercises the ErrNotRegularFile branch distinctly from
	// ErrSymlinkRefused.
	if err := mkfifo(finalPath); err != nil {
		t.Skipf("mkfifo unsupported in this environment: %v", err)
	}
	if _, err := Write(dir, r); !errors.Is(err, ErrNotRegularFile) {
		t.Fatalf("expected ErrNotRegularFile, got %v", err)
	}
}
