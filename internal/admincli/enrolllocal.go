package admincli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"autodeploy/internal/server"
	"autodeploy/internal/store/postgres"
)

var (
	// ErrAdministratorMissing: no administrator exists yet (first-run setup pending).
	ErrAdministratorMissing = errors.New("no administrator exists yet; finish first-run setup first")
	// ErrAdministratorDisabled: the owner's administrator account is disabled (not polled by --wait).
	ErrAdministratorDisabled = errors.New("the administrator account is disabled; re-enable it or reset it before enrolling")
	// ErrTokenFileUnusable: the token file path was refused (relative, exists, symlink, no directory).
	ErrTokenFileUnusable = errors.New("token file must be a new absolute path in an existing directory (it is created 0600, never overwritten, symlinks refused)")
	// ErrLocalServerUnavailable: the recorded server id is not an active server of this owner (revoked or removed).
	ErrLocalServerUnavailable = errors.New("the recorded server id is not an active server of this installation (revoked or removed)")
	// ErrServerNeedsReenroll: the server's token was consumed but it has no active credential.
	ErrServerNeedsReenroll = errors.New("the server was enrolled before but has no active credential; use Servers > Re-enroll in the UI")
)

// LocalEnrollRepository is the narrow persistence surface of `admin server-enroll-local`.
type LocalEnrollRepository interface {
	LocalOwner(context.Context) (postgres.LocalOwner, error)
	EnrollLocalServer(ctx context.Context, recordedID, name, newID string, digest [32]byte, ttlSeconds int) (postgres.LocalEnrollment, error)
}

// LocalEnrollOptions configures EnrollLocal. Random and Sleep are test seams.
type LocalEnrollOptions struct {
	Name      string        // required; server.ValidName
	ServerID  string        // recorded id from agent-local.env; empty = none recorded
	TokenFile string        // required; created O_EXCL 0600
	Wait      time.Duration // poll for the first administrator up to this long (0 = no wait)
	WaitOnly  bool          // cmd-level mode: only wait for the administrator, no token file
	Random    io.Reader
	Sleep     func(context.Context, time.Duration) error
}

const localEnrollPollInterval = 2 * time.Second

// EnrollLocal registers (or reuses) this host's server under the sole owner and
// writes a fresh single-use enrollment token to opts.TokenFile. Only the server
// id goes to out; the token is written to the file and nowhere else.
//
// alreadyEnrolled=true means the server has an active credential: the id is
// printed, nothing was minted and no file remains. Otherwise a never-enrolled
// server (recorded, or one this path registered itself under the same name) is
// reused with a fresh token, or a new one is registered; find and
// register/issue run in one serialized transaction in the repository. The
// token file is created first so a bad path fails before anything is minted;
// if any later step fails the file this call created is removed.
func EnrollLocal(ctx context.Context, repo LocalEnrollRepository, opts LocalEnrollOptions, out io.Writer) (alreadyEnrolled bool, err error) {
	if repo == nil || out == nil || !server.ValidName(opts.Name) || opts.TokenFile == "" {
		return false, ErrOperationFailed
	}
	if _, err = WaitForAdministrator(ctx, repo, opts.Wait, opts.Sleep); err != nil {
		return false, err
	}
	file, err := createTokenFile(opts.TokenFile)
	if err != nil {
		return false, err
	}
	result, token, err := mintLocal(ctx, repo, opts)
	if err == nil && !result.Enrolled {
		_, err = io.WriteString(file, token+"\n")
		if err == nil {
			err = file.Sync()
		}
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil || result.Enrolled {
		_ = os.Remove(opts.TokenFile) // the file this call just created
	}
	if err != nil {
		if errors.Is(err, ErrOperationFailed) || errors.Is(err, ErrServerNeedsReenroll) || errors.Is(err, ErrLocalServerUnavailable) {
			return false, err
		}
		return false, ErrOperationFailed
	}
	return result.Enrolled, writeID(out, result.ServerID)
}

func writeID(out io.Writer, id string) error {
	if _, err := io.WriteString(out, id+"\n"); err != nil {
		return ErrOperationFailed
	}
	return nil
}

// WaitForAdministrator returns the owner once an administrator exists, polling
// up to wait. A disabled administrator is reported at once, never polled.
func WaitForAdministrator(ctx context.Context, repo interface {
	LocalOwner(context.Context) (postgres.LocalOwner, error)
}, wait time.Duration, sleep func(context.Context, time.Duration) error) (postgres.LocalOwner, error) {
	if sleep == nil {
		sleep = sleepContext
	}
	deadline := time.Now().Add(wait)
	for {
		owner, err := repo.LocalOwner(ctx)
		if err == nil {
			return owner, nil
		}
		if errors.Is(err, postgres.ErrOwnerInactive) {
			return postgres.LocalOwner{}, ErrAdministratorDisabled
		}
		if !errors.Is(err, postgres.ErrNoAdministrator) {
			return postgres.LocalOwner{}, ErrOperationFailed
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return postgres.LocalOwner{}, ErrAdministratorMissing
		}
		if sleep(ctx, min(localEnrollPollInterval, remaining)) != nil {
			return postgres.LocalOwner{}, ErrOperationFailed
		}
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// mintLocal mints the token via the same constructors the UI uses and hands
// its digest to the repository, which finds, registers or reissues atomically.
func mintLocal(ctx context.Context, repo LocalEnrollRepository, opts LocalEnrollOptions) (postgres.LocalEnrollment, string, error) {
	enrollment, err := server.NewEnrollmentToken(opts.Random)
	if err != nil {
		return postgres.LocalEnrollment{}, "", ErrOperationFailed
	}
	digest, err := server.EnrollmentTokenDigest(enrollment)
	if err != nil {
		return postgres.LocalEnrollment{}, "", ErrOperationFailed
	}
	id, err := server.NewID(opts.Random)
	if err != nil {
		return postgres.LocalEnrollment{}, "", ErrOperationFailed
	}
	result, err := repo.EnrollLocalServer(ctx, opts.ServerID, opts.Name, id, digest, server.EnrollmentTTLSeconds)
	switch {
	case err == nil:
		return result, enrollment.String(), nil
	case errors.Is(err, postgres.ErrServerNeedsReenroll):
		return postgres.LocalEnrollment{}, "", ErrServerNeedsReenroll
	case errors.Is(err, postgres.ErrServerAuthorizationDenied) && opts.ServerID != "":
		return postgres.LocalEnrollment{}, "", ErrLocalServerUnavailable
	default:
		return postgres.LocalEnrollment{}, "", ErrOperationFailed
	}
}

// createTokenFile creates path exclusively with mode 0600. O_EXCL refuses an
// existing file or any symlink at the final component; O_NOFOLLOW states the
// intent; the path must be absolute and clean so no relative surprise applies.
func createTokenFile(path string) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, ErrTokenFileUnusable
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, ErrTokenFileUnusable
	}
	return file, nil
}
