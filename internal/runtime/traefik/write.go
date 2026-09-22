package traefik

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// This file is ADR 0005's "Traefik configuration ownership" atomic-write
// procedure: temp file in the same directory with a non-provider
// extension, write, fsync the file, rename over the final provider
// filename, fsync the directory. "Atomic rename is only the request to
// switch configuration" -- this file's job ends at a durable, complete
// rename; it makes no claim about when or whether Traefik has reloaded.

// routeTempFileExtension is deliberately not a Traefik dynamic-config
// extension (".yml"/".yaml"/".toml"): ADR 0005 requires the provider's
// watcher to ignore the in-progress temp file, and write_test.go proves
// that requirement the only way this package can on its own -- by
// asserting no file bearing a provider extension is ever visible under a
// name other than the final target during the write.
const routeTempFileExtension = ".tmp"

const routeFileMode fs.FileMode = 0640

// renameHook and writeHook are the fault-injection seams write_test.go uses
// to prove the atomic-write procedure's failure paths (leftover temp file
// cleanup, no partial final file) without needing real disk-full/EIO
// conditions, which are not reproducible in a portable unit test. Production
// code never assigns to these; they are exported at package scope only
// because Go has no other way to substitute a call an internal function
// makes without changing that function's signature, and both are reset by
// every test that touches them (see write_test.go's t.Cleanup calls).
var (
	renameHook = os.Rename
	writeHook  = func(f *os.File, doc []byte) (int, error) { return f.Write(doc) }
	// tempSuffixHook generates the random component of a temp file name.
	// write_test.go overrides it to force a name collision deterministically
	// (proving the O_EXCL guard actually does something -- see
	// TestCreateTempFileRefusesToOverwriteOrFollowAPredictedCollision)
	// instead of relying on crypto/rand ever colliding by chance, which it
	// won't in a portable test. Production code never assigns to it.
	tempSuffixHook = func() (string, error) { return randomHex(8) }
)

var (
	// ErrSymlinkRefused is returned when the route directory or an
	// existing route file is a symlink. This package never writes through
	// a symlink: a symlinked directory could point outside the intended
	// Traefik-watched mount, and a symlinked existing file could be an
	// attempt to make this package's "atomic replace" instead silently
	// follow the link and corrupt or disclose something else entirely.
	ErrSymlinkRefused = errors.New("traefik: refusing to operate through a symlink")
	// ErrNotDirectory is returned when the configured route directory path
	// exists but is not a directory.
	ErrNotDirectory = errors.New("traefik: route directory path is not a directory")
	// ErrDirectoryOwnership is returned when the route directory is not
	// owned by this process's effective UID -- a directory this agent
	// process does not own is not "configured as expected" for a file this
	// package is about to replace atomically and unconditionally.
	ErrDirectoryOwnership = errors.New("traefik: route directory is not owned by this process")
	// ErrNotRegularFile is returned when an existing path at the final
	// route file location exists but is neither absent nor a plain
	// regular file (a symlink is reported separately as
	// ErrSymlinkRefused).
	ErrNotRegularFile = errors.New("traefik: existing route file is not a regular file")
	// ErrDirectoryPermissions is returned when the route directory's
	// permission bits are unsafe: world-writable in every case, or
	// group-writable when the directory is not (also) owned by the agent
	// UID or root. See checkDirectoryPolicy's doc comment for the expected
	// packaging topology this enforces.
	ErrDirectoryPermissions = errors.New("traefik: route directory has unsafe permissions")
	// ErrRouteFileTooLarge is returned when an existing route file at the
	// final path exceeds maxRouteFileBytes. This package never writes a
	// file anywhere near that size itself, so an oversized file there is
	// local state this package did not produce.
	ErrRouteFileTooLarge = errors.New("traefik: existing route file exceeds the size limit")
)

// maxRouteFileBytes bounds every read this package performs of an existing
// route file (the idempotent-write comparison, Remove's existence check,
// and ReadDigest). The largest document this package itself ever renders
// is a few hundred bytes; this is deliberately generous while still
// bounding memory against a file this package did not create sitting at
// the path it is about to manage.
const maxRouteFileBytes = 1 << 20 // 1 MiB

// WriteOutcome distinguishes whether Write actually replaced the file on
// disk or found it already correct.
type WriteOutcome int

const (
	WriteOutcomeUnknown WriteOutcome = iota
	// WriteOutcomeWritten means a new file was rendered and installed via
	// atomic rename.
	WriteOutcomeWritten
	// WriteOutcomeUnchanged means the file already on disk had exactly the
	// digest the requested EnvironmentRoute renders to; Write performed no
	// filesystem mutation at all.
	WriteOutcomeUnchanged
)

// WriteResult is Write's outcome, including enough digest history for a
// 10c/10d caller to compare against an expected digest without a separate
// ReadDigest call.
type WriteResult struct {
	Outcome WriteOutcome
	// Digest is the canonical digest of the document now on disk --
	// whether this call wrote it or it was already there.
	Digest string
	// PreviousDigest is the digest of whatever file existed at the target
	// path before this call, or "" if no file existed there yet. It equals
	// Digest exactly when Outcome is WriteOutcomeUnchanged.
	PreviousDigest string
}

// verifyDirectory refuses to proceed unless dir exists, is a real
// directory (Lstat, so a symlinked directory is caught before following
// it), and passes checkDirectoryPolicy's ownership/permission rules. This
// runs before any temp file is created.
func verifyDirectory(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("traefik: stat route directory %s: %w", dir, err)
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%w: route directory %s", ErrSymlinkRefused, dir)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%w: %s", ErrNotDirectory, dir)
	}
	sys, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("traefik: cannot verify ownership of route directory %s", dir)
	}
	if err := checkDirectoryPolicy(uint32(sys.Uid), fi.Mode().Perm()); err != nil {
		return fmt.Errorf("%w: %s", err, dir)
	}
	return nil
}

// checkDirectoryPolicy is the pure, seamable decision this package makes
// about whether a directory is safe to write through, given only its
// owning UID and permission bits -- deliberately factored out of
// verifyDirectory so write_test.go can exercise the full accept/reject
// matrix without needing to chown a real file (which an unprivileged test
// process cannot do).
//
// Expected packaging topology (see also this package's doc comment): the
// route directory is created by host packaging as owned by the agent's own
// user (or root, for a directory provisioned before the agent's user
// exists), mode 0750 -- owner read/write/execute, group read/execute,
// nothing for other -- with Traefik's own user added to the directory's
// group so its file-provider watch can list and open files inside it
// without ever needing write access. A setgid bit on the directory keeps
// files this package creates inside it group-owned the same way.
//
// The policy:
//
//   - The owning UID must be this process's own UID or root (0). A
//     directory owned by any other user is not "configured as expected"
//     for a file this package is about to replace atomically and
//     unconditionally -- see ErrDirectoryOwnership. Root is accepted
//     alongside the agent's own UID because host packaging may
//     provision this directory before the agent's dedicated user account
//     exists, or may deliberately keep it root-owned with the agent
//     granted access some other way (e.g. an ACL); requiring exact-UID-only
//     match would make that entirely ordinary provisioning path fail
//     permanently for a reason this package cannot distinguish from a real
//     misconfiguration.
//   - World-writable (perm&0002) is always rejected: this is checked
//     unconditionally regardless of owner, exactly like ErrDirectoryOwnership
//     already gates entry above it.
//   - Group-writable (perm&0020) is rejected UNLESS the owner is already
//     the agent's own UID or root. This branch is unreachable through
//     verifyDirectory today, because verifyDirectory already returned
//     ErrDirectoryOwnership above for any other owner before this function
//     is ever called with perm intact -- it is kept as an explicit, named,
//     independently-tested rule (not folded into "owner check already
//     covers it") so that if a future change ever relaxes or reorders the
//     ownership gate, group-writable-by-an-untrusted-owner is still an
//     enforced rule rather than a silently regressed assumption. A
//     group-writable directory that IS owned by the agent or root is
//     accepted: the realistic topology above only needs the directory
//     group-readable+executable for Traefik, but this package does not
//     itself require group access to be read-only when ownership already
//     establishes trust.
func checkDirectoryPolicy(ownerUID uint32, perm os.FileMode) error {
	trustedOwner := ownerUID == uint32(os.Getuid()) || ownerUID == 0
	if !trustedOwner {
		return ErrDirectoryOwnership
	}
	if perm&0002 != 0 {
		return fmt.Errorf("%w: world-writable", ErrDirectoryPermissions)
	}
	if perm&0020 != 0 && !trustedOwner {
		return fmt.Errorf("%w: group-writable and not owned by the agent or root", ErrDirectoryPermissions)
	}
	return nil
}

// statExistingRouteFile Lstats path (the final, provider-recognized route
// file) and classifies what -- if anything -- is there: absent, a symlink
// (refused), a non-regular file (refused), or a regular file (returns its
// FileInfo for the caller to read).
func statExistingRouteFile(path string) (fi os.FileInfo, exists bool, err error) {
	fi, err = os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("traefik: stat route file %s: %w", path, err)
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return nil, false, fmt.Errorf("%w: route file %s", ErrSymlinkRefused, path)
	}
	if !fi.Mode().IsRegular() {
		return nil, false, fmt.Errorf("%w: %s", ErrNotRegularFile, path)
	}
	return fi, true, nil
}

// createTempFile creates a new, exclusively-owned regular file in dir
// named "<base>-<random>Ext", opened O_RDWR|O_CREAT|O_EXCL|O_NOFOLLOW so it
// can never be an existing symlink (O_NOFOLLOW) and can never silently
// reuse or truncate a pre-existing file at the same path (O_EXCL: open
// fails outright if anything -- symlink or regular file -- already exists
// there). Concurrent callers each get their own randomized name (that is
// what makes the concurrent-writers test provable: two goroutines racing
// Write on the same environment never contend for the same temp file, only
// for the final rename, which the kernel serializes atomically), so under
// normal operation O_EXCL never actually fires; it exists as a guard
// against the astronomically unlikely random collision and, more
// concretely, against a forced/predicted collision -- see
// TestCreateTempFileRefusesToOverwriteOrFollowAPredictedCollision, which
// forces one via tempSuffixHook and proves O_EXCL is what refuses it.
//
// O_NOFOLLOW is applied via the raw syscall flag rather than os.OpenFile's
// portable flag set, which has no portable O_NOFOLLOW constant; both linux
// and darwin's syscall package define O_NOFOLLOW, so this compiles
// unchanged on every platform this project targets (agent Linux hosts,
// developer macOS) without a build-tag split. It is retried a bounded
// number of times on a name collision (EEXIST), exactly like os.CreateTemp
// does internally.
func createTempFile(dir, base string) (*os.File, string, error) {
	const maxAttempts = 8
	var lastErr error
	for i := 0; i < maxAttempts; i++ {
		suffix, err := tempSuffixHook()
		if err != nil {
			return nil, "", fmt.Errorf("traefik: generate temp file suffix: %w", err)
		}
		name := base + "-" + suffix + routeTempFileExtension
		path := filepath.Join(dir, name)
		fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW, uint32(routeFileMode))
		if err != nil {
			if errors.Is(err, syscall.EEXIST) {
				lastErr = err
				continue
			}
			return nil, "", fmt.Errorf("traefik: create temp file %s: %w", path, err)
		}
		return os.NewFile(uintptr(fd), path), path, nil
	}
	return nil, "", fmt.Errorf("traefik: could not create a unique temp file after %d attempts: %w", maxAttempts, lastErr)
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// readRouteFileBounded opens path O_RDONLY|O_NOFOLLOW (so a symlink swapped
// in after statExistingRouteFile's Lstat check -- a TOCTOU window that
// check alone cannot close -- is refused here too, at the point this
// package actually reads content), re-confirms via fstat that what it
// opened is a regular file (ErrNotRegularFile), and reads at most
// maxRouteFileBytes+1 bytes so a file larger than the cap is detected and
// rejected (ErrRouteFileTooLarge) rather than silently truncated and
// digested as if it were the whole file. Every caller that reads an
// existing route file's content -- Write's idempotent-compare and
// ReadDigest -- goes through this single function.
func readRouteFileBounded(path string) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("%w: %s", ErrSymlinkRefused, path)
		}
		return nil, fmt.Errorf("traefik: open route file %s: %w", path, err)
	}
	f := os.NewFile(uintptr(fd), path)
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("traefik: stat opened route file %s: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s", ErrNotRegularFile, path)
	}

	data, err := io.ReadAll(io.LimitReader(f, maxRouteFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("traefik: read route file %s: %w", path, err)
	}
	if len(data) > maxRouteFileBytes {
		return nil, fmt.Errorf("%w: %s", ErrRouteFileTooLarge, path)
	}
	return data, nil
}

// openDirNoFollow opens dir read-only for the sole purpose of calling
// Sync() on it (the directory-fsync step ADR 0005 requires after the
// rename). O_NOFOLLOW here is defense in depth against a TOCTOU swap of
// dir between verifyDirectory's Lstat and this open; it is layered on top
// of, not a replacement for, that earlier check.
func openDirNoFollow(dir string) (*os.File, error) {
	fd, err := syscall.Open(dir, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("traefik: open route directory %s: %w", dir, err)
	}
	f := os.NewFile(uintptr(fd), dir)
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("traefik: stat opened route directory %s: %w", dir, err)
	}
	if !fi.IsDir() {
		_ = f.Close()
		return nil, fmt.Errorf("%w: %s", ErrNotDirectory, dir)
	}
	return f, nil
}

// Write renders r and installs it as dir's environment route file via the
// full atomic sequence: verify dir, render, compare against whatever is
// already at the final path (no-op if identical), write a same-directory
// temp file, fsync it, close it, rename it over the final
// provider-recognized name, fsync the directory, close the directory.
//
// Every failure path removes the temp file it created before returning (or
// never created one at all, for a failure that occurs before
// createTempFile). There is one exception documented at the return
// site below: once rename has succeeded, the temp file no longer exists
// under its temp name (rename is what removed it), so there is nothing
// left to clean up even if the subsequent directory fsync then fails --
// that failure is reported, but it does not mean the write was rolled
// back; the new content is durably named on disk regardless (ADR 0005
// treats an ambiguous post-rename failure as a reconciliation concern for
// a later step, not something this function can undo).
func Write(dir string, r EnvironmentRoute) (WriteResult, error) {
	if err := verifyDirectory(dir); err != nil {
		return WriteResult{}, err
	}
	doc, digest, err := Render(r)
	if err != nil {
		return WriteResult{}, err
	}

	finalPath := filepath.Join(dir, routeFileName(r.EnvironmentID))
	_, exists, err := statExistingRouteFile(finalPath)
	if err != nil {
		return WriteResult{}, err
	}
	var previousDigest string
	if exists {
		existingBytes, err := readRouteFileBounded(finalPath)
		if err != nil {
			return WriteResult{}, err
		}
		previousDigest = Digest(existingBytes)
		if previousDigest == digest {
			return WriteResult{Outcome: WriteOutcomeUnchanged, Digest: digest, PreviousDigest: previousDigest}, nil
		}
	}

	tempFile, tempPath, err := createTempFile(dir, routeFilePrefix+r.EnvironmentID)
	if err != nil {
		return WriteResult{}, err
	}
	// cleanupTemp is disarmed the instant rename succeeds; every other
	// return path (including a panic-free early return from a write/fsync
	// failure) runs it.
	tempStillPresent := true
	cleanupTemp := func() {
		if tempStillPresent {
			_ = os.Remove(tempPath)
		}
	}
	defer cleanupTemp()

	if _, err := writeHook(tempFile, doc); err != nil {
		_ = tempFile.Close()
		return WriteResult{}, fmt.Errorf("traefik: write temp route file %s: %w", tempPath, err)
	}
	if err := tempFile.Sync(); err != nil {
		_ = tempFile.Close()
		return WriteResult{}, fmt.Errorf("traefik: fsync temp route file %s: %w", tempPath, err)
	}
	if err := tempFile.Close(); err != nil {
		return WriteResult{}, fmt.Errorf("traefik: close temp route file %s: %w", tempPath, err)
	}

	if err := renameHook(tempPath, finalPath); err != nil {
		return WriteResult{}, fmt.Errorf("traefik: rename %s to %s: %w", tempPath, finalPath, err)
	}
	// From here on the temp file's name no longer refers to anything (the
	// rename consumed it), so the deferred cleanup must not attempt to
	// remove it -- doing so would race a brand-new, unrelated temp file
	// that happened to reuse the same random name after this one was
	// renamed away, however astronomically unlikely.
	tempStillPresent = false

	dirFile, err := openDirNoFollow(dir)
	if err != nil {
		return WriteResult{}, err
	}
	defer func() { _ = dirFile.Close() }()
	if err := dirFile.Sync(); err != nil {
		return WriteResult{}, fmt.Errorf("traefik: fsync route directory %s: %w", dir, err)
	}

	return WriteResult{Outcome: WriteOutcomeWritten, Digest: digest, PreviousDigest: previousDigest}, nil
}

// Remove idempotently deletes environmentID's route file from dir. A
// missing file is success, not an error. An existing symlink at that path
// is refused exactly as Write refuses one, rather than silently removed:
// this package never resolves or acts through a path it did not itself
// create as a plain regular file.
func Remove(dir, environmentID string) error {
	if err := verifyDirectory(dir); err != nil {
		return err
	}
	path := filepath.Join(dir, routeFileName(environmentID))
	_, exists, err := statExistingRouteFile(path)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("traefik: remove route file %s: %w", path, err)
	}
	return nil
}

// ReadDigest returns the canonical digest of environmentID's current route
// file in dir, or exists == false if no such file is present.
func ReadDigest(dir, environmentID string) (digest string, exists bool, err error) {
	if err := verifyDirectory(dir); err != nil {
		return "", false, err
	}
	path := filepath.Join(dir, routeFileName(environmentID))
	_, exists, err = statExistingRouteFile(path)
	if err != nil || !exists {
		return "", exists, err
	}
	data, err := readRouteFileBounded(path)
	if err != nil {
		return "", false, err
	}
	return Digest(data), true, nil
}
