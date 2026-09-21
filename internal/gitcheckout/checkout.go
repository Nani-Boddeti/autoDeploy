// Package gitcheckout performs the fixed, lease-authorized Git checkout.
package gitcheckout

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"time"

	"autodeploy/internal/checkoutcontain"
	"autodeploy/internal/checkoutquota"
	"autodeploy/internal/crypto"
	"autodeploy/internal/githubapp"

	"golang.org/x/sys/unix"
)

var (
	ErrInvalidConfig = errors.New("Git checkout configuration invalid")
	ErrGitFailed     = errors.New("Git checkout failed")
	ErrUnsafeSource  = errors.New("Git checkout rejected unsafe source")
)

type quotaClient interface {
	Allocate(context.Context, checkoutquota.Limits) (checkoutquota.Allocation, error)
	ResolveAllocation(context.Context, checkoutquota.Allocation) (checkoutquota.Allocation, error)
	ReleaseAndCleanup(context.Context, checkoutquota.Allocation) error
}
type spawnClient interface {
	Run(context.Context, checkoutcontain.SpawnRequest) (checkoutcontain.SpawnResult, error)
}

// RunnerConfig deliberately carries capabilities and limits only. The quota
// helper creates the workspace; this package never accepts a checkout path or
// executable path and has no local execution fallback.
type RunnerConfig struct {
	Quota       quotaClient
	Spawn       spawnClient
	QuotaLimits checkoutquota.Limits
	SpawnLimits checkoutcontain.SpawnLimits
}

type Runner struct{ config RunnerConfig }

func NewRunner(c RunnerConfig) (*Runner, error) {
	if nilCapability(c.Quota) || nilCapability(c.Spawn) || c.QuotaLimits.Bytes == 0 || c.QuotaLimits.Inodes == 0 || c.SpawnLimits.TimeoutSeconds == 0 || c.SpawnLimits.MaxOutput == 0 {
		return nil, ErrInvalidConfig
	}
	return &Runner{config: c}, nil
}

func nilCapability(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	return (rv.Kind() == reflect.Ptr || rv.Kind() == reflect.Interface) && rv.IsNil()
}

// Result owns the quota allocation, including its workspace descriptor. Close
// releases and cleans it through the helper and is safe to call repeatedly.
type Result struct {
	quota  quotaClient
	alloc  checkoutquota.Allocation
	mu     sync.Mutex
	closed bool
	err    error
}

// WorkspaceFD returns the raw workspace descriptor this Result owns, valid
// until Close (Close sets it to -1). It exists so a caller that needs to
// actually use the checked-out tree -- Slice 9b's build step -- can address
// it without this package ever turning the descriptor back into a pathname
// itself; that "never reconstruct a path from a descriptor" rule is this
// package's own (see checkoutquota's removeWorkspaceContents), not
// something a caller assembling a build is bound by. Callers must never
// close the returned descriptor themselves -- Close owns that -- and must
// keep this Result open for as long as anything derived from the
// descriptor (e.g. a resolved /proc/self/fd path) is still in use.
func (r *Result) WorkspaceFD() int {
	if r == nil {
		return -1
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.alloc.Workspace
}

func (r *Result) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return r.err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := r.quota.ReleaseAndCleanup(ctx, r.alloc)
	// Retryable transport/service failures retain the descriptor because the
	// quota protocol requires that same capability on a later cleanup attempt.
	if err != nil && !errors.Is(err, checkoutquota.ErrDenied) {
		r.err = err
		return err
	}
	closeErr := unix.Close(r.alloc.Workspace)
	r.closed = true
	r.alloc.Workspace = -1
	r.err = errors.Join(err, closeErr)
	return r.err
}

// CleanupError exposes a still-owned Result when mandatory cleanup could not
// complete. Callers must retry Result.Close rather than losing the capability.
type CleanupError struct {
	Result  *Result
	Cause   error
	Cleanup error
}

func (e *CleanupError) Error() string { return "Git checkout cleanup pending" }
func (e *CleanupError) Unwrap() []error {
	if e == nil {
		return nil
	}
	if e.Cleanup == nil {
		return []error{e.Cause}
	}
	return []error{e.Cause, e.Cleanup}
}

// Lease is the capability set Checkout actually needs from an authorized
// checkout lease. It exists so the agent can drive a checkout using a
// short-lived token fetched over its authenticated channel while the GitHub
// App private key stays control-plane-only (ADR 0001): the control plane's
// *githubapp.CheckoutLease satisfies this unchanged, and an agent-side
// implementation can delegate the durable state transitions back over
// /agent/v1 without this package knowing anything about transport.
//
// Checkout still accepts no URL, revision, destination, executable, or
// credential from a caller: every source identity comes from Binding(), and
// the credential reaches Git only through NewAskPassBroker's one-use pipe.
type Lease interface {
	// Binding is the authorized source identity: owner, project, repository,
	// clone URL, and exact commit.
	Binding() githubapp.CheckoutBinding
	// SetCancel installs the hook that stops Git if the lease is invalidated
	// while it runs.
	SetCancel(context.CancelFunc)
	// Start marks the durable lease running immediately before the credential
	// is delivered, and fails closed if authority was withdrawn meanwhile.
	Start(context.Context) error
	// NewAskPassBroker transfers the credential through a one-use pipe. A
	// lease can only do this once.
	NewAskPassBroker() (*githubapp.AskPassBroker, error)
	// RedactingWriter wraps command output so the credential cannot appear in
	// it. This is the only output boundary that sees the credential.
	RedactingWriter(io.Writer) (*crypto.RedactingWriter, error)
	// Close consumes the lease: it releases the durable reservation and
	// revokes the credential, and is called even after a successful checkout.
	Close(context.Context) error
}

// The control plane's own lease type must keep satisfying this interface; a
// change to either side that breaks the pairing fails the build here rather
// than at a call site.
var _ Lease = (*githubapp.CheckoutLease)(nil)

// Checkout derives every source identity from the activated lease. It never
// accepts a URL, revision, destination, executable, or credential from a caller.
func (r *Runner) Checkout(ctx context.Context, lease Lease) (result *Result, err error) {
	// nilCapability, not lease == nil: now that this is an interface, a typed
	// nil (for example a (*githubapp.CheckoutLease)(nil) passed by a caller
	// whose own construction failed) is a non-nil interface holding a nil
	// pointer, and the first method call on it would dereference nil instead
	// of failing closed here.
	if r == nil || nilCapability(lease) || nilCapability(r.config.Quota) || nilCapability(r.config.Spawn) {
		return nil, ErrInvalidConfig
	}
	checkoutCtx, cancelCheckout := context.WithTimeout(ctx, time.Duration(r.config.SpawnLimits.TimeoutSeconds)*time.Second)
	defer cancelCheckout()
	lease.SetCancel(cancelCheckout)
	var cleanupResult *Result
	defer func() {
		closeCtx, cancel := cleanupContext(ctx)
		defer cancel()
		// A lease is always consumed, including after a successful checkout: the
		// allocation, not the credential lease, keeps the checkout alive.
		if err != nil && cleanupResult != nil {
			var pending *CleanupError
			isPending := errors.As(err, &pending) && pending.Result == cleanupResult
			if cleanupErr := cleanupResult.Close(); cleanupErr != nil {
				cause := err
				if isPending {
					cause = pending.Cause
				}
				err = &CleanupError{Result: cleanupResult, Cause: cause, Cleanup: cleanupErr}
				result = cleanupResult
			} else {
				if isPending {
					err = pending.Cause
				}
				result = nil
			}
		}
		if closeErr := lease.Close(closeCtx); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()

	binding := lease.Binding()
	if !binding.CloneURLValid() || len(binding.CommitSHA) != 40 || !isLowerHex(binding.CommitSHA) || binding.ReservationID == "" {
		return nil, ErrGitFailed
	}
	alloc, allocErr := r.config.Quota.Allocate(checkoutCtx, r.config.QuotaLimits)
	if allocErr != nil {
		var indeterminate *checkoutquota.IndeterminateAllocationError
		if !errors.As(allocErr, &indeterminate) {
			return nil, errors.Join(ErrGitFailed, allocErr)
		}
		// The capability is owned even if acknowledgement was lost. Resolve if
		// possible, then always ask the helper to retire it; never orphan it.
		alloc = indeterminate.Allocation
		cleanupCtx, cancel := cleanupContext(ctx)
		defer cancel()
		cleanupResult = &Result{quota: r.config.Quota, alloc: alloc}
		cause := errors.Join(ErrGitFailed, allocErr)
		if resolved, resolveErr := r.config.Quota.ResolveAllocation(cleanupCtx, alloc); resolveErr == nil {
			alloc = resolved
			cleanupResult.alloc = alloc
			if cleanupErr := cleanupResult.Close(); cleanupErr != nil && !cleanupResult.closed {
				return cleanupResult, &CleanupError{Result: cleanupResult, Cause: cause, Cleanup: cleanupErr}
			}
		} else {
			cause = errors.Join(cause, resolveErr)
			if cleanupErr := cleanupResult.Close(); cleanupErr != nil && !cleanupResult.closed {
				return cleanupResult, &CleanupError{Result: cleanupResult, Cause: cause, Cleanup: cleanupErr}
			}
		}
		if !cleanupResult.closed {
			return cleanupResult, &CleanupError{Result: cleanupResult, Cause: cause}
		}
		return nil, cause
	}
	result = &Result{quota: r.config.Quota, alloc: alloc}
	cleanupResult = result

	if err = r.run(checkoutCtx, lease, alloc.Workspace, checkoutcontain.GitInit, "", "", -1, false); err != nil {
		return nil, err
	}
	if err = r.run(checkoutCtx, lease, alloc.Workspace, checkoutcontain.GitRemoteAddOrigin, "", binding.CloneURL, -1, false); err != nil {
		return nil, err
	}
	if err = lease.Start(checkoutCtx); err != nil {
		return nil, errors.Join(ErrGitFailed, err)
	}
	broker, brokerErr := lease.NewAskPassBroker()
	if brokerErr != nil {
		return nil, errors.Join(ErrGitFailed, brokerErr)
	}
	defer broker.Close()
	reader := broker.Reader()
	if reader == nil {
		return nil, ErrGitFailed
	}
	defer reader.Close()
	askpassFD, dupErr := unix.Dup(int(reader.Fd()))
	if dupErr != nil {
		return nil, errors.Join(ErrGitFailed, dupErr)
	}
	if err = r.run(checkoutCtx, lease, alloc.Workspace, checkoutcontain.GitFetchExact, binding.CommitSHA, "", askpassFD, false); err != nil {
		return nil, err
	}
	if head, runErr := r.output(checkoutCtx, lease, alloc.Workspace, checkoutcontain.GitRevParseFetchHead); runErr != nil || strings.TrimSpace(head) != binding.CommitSHA {
		return nil, errors.Join(ErrGitFailed, runErr)
	}
	if err = r.run(checkoutCtx, lease, alloc.Workspace, checkoutcontain.GitCheckoutFetchHead, "", "", -1, false); err != nil {
		return nil, err
	}
	if head, runErr := r.output(checkoutCtx, lease, alloc.Workspace, checkoutcontain.GitRevParseHead); runErr != nil || strings.TrimSpace(head) != binding.CommitSHA {
		return nil, errors.Join(ErrGitFailed, runErr)
	}
	if tree, runErr := r.output(checkoutCtx, lease, alloc.Workspace, checkoutcontain.GitLSTree); runErr != nil {
		return nil, errors.Join(ErrGitFailed, runErr)
	} else if unsafeTree(tree) {
		return nil, ErrUnsafeSource
	}
	// ls-tree permits only ordinary blob modes below. That rejects Git symlinks
	// (120000), submodules (160000), and .gitmodules, so a path walk would add a
	// TOCTOU-prone reconstruction of the descriptor-relative workspace.
	if lfs, runErr := r.output(checkoutCtx, lease, alloc.Workspace, checkoutcontain.GitGrepLFS); runErr != nil {
		if !isNoMatch(runErr) {
			return nil, errors.Join(ErrGitFailed, runErr)
		}
	} else if strings.TrimSpace(lfs) != "" {
		return nil, ErrUnsafeSource
	}
	return result, nil
}

func (r *Runner) run(ctx context.Context, lease Lease, workspace int, op checkoutcontain.GitOperation, arg, remote string, askpass int, capture bool) error {
	_, err := r.execute(ctx, lease, workspace, op, arg, remote, askpass, capture)
	return err
}
func (r *Runner) output(ctx context.Context, lease Lease, workspace int, op checkoutcontain.GitOperation) (string, error) {
	return r.execute(ctx, lease, workspace, op, "", "", -1, true)
}
func (r *Runner) execute(ctx context.Context, lease Lease, workspace int, op checkoutcontain.GitOperation, arg, remote string, askpass int, capture bool) (string, error) {
	askpassOwned := askpass >= 0
	defer func() {
		if askpassOwned {
			_ = unix.Close(askpass)
		}
	}()
	stdoutR, stdoutW, err := pipe()
	if err != nil {
		return "", errors.Join(ErrGitFailed, err)
	}
	stderrR, stderrW, err := pipe()
	if err != nil {
		closeFDs(stdoutR, stdoutW)
		return "", errors.Join(ErrGitFailed, err)
	}
	var output bytes.Buffer
	destination := io.Writer(io.Discard)
	if capture {
		destination = &output
	}
	shared := &outputBudget{w: destination, n: int64(r.config.SpawnLimits.MaxOutput)}
	stdoutRedactor, err := lease.RedactingWriter(&limitedWriter{budget: shared})
	if err != nil {
		closeFDs(stdoutR, stdoutW, stderrR, stderrW)
		return "", ErrGitFailed
	}
	stderrRedactor, err := lease.RedactingWriter(&limitedWriter{budget: shared})
	if err != nil {
		closeFDs(stdoutR, stdoutW, stderrR, stderrW)
		_ = stdoutRedactor.Close()
		return "", ErrGitFailed
	}
	// SpawnClient consumes its descriptor inputs. Keep the allocation's original
	// descriptor exclusively for Result.Close and transfer one duplicate per op.
	workspaceCopy, dupErr := unix.Dup(workspace)
	if dupErr != nil {
		closeFDs(stdoutR, stdoutW, stderrR, stderrW)
		_ = stdoutRedactor.Close()
		_ = stderrRedactor.Close()
		return "", errors.Join(ErrGitFailed, dupErr)
	}
	request := checkoutcontain.SpawnRequest{Operation: op, Arg: arg, RemoteURL: remote, WorkspaceFD: workspaceCopy, StdoutFD: stdoutW, StderrFD: stderrW, StdoutReadFD: stdoutR, StderrReadFD: stderrR, AskpassReadFD: askpass, Stdout: stdoutRedactor, Stderr: stderrRedactor, Limits: r.config.SpawnLimits}
	// SpawnClient owns every request descriptor after Run begins, including the
	// duplicated askpass reader. Earlier returns remain this function's duty.
	askpassOwned = false
	spawnResult, runErr := r.config.Spawn.Run(ctx, request)
	closeErr := errors.Join(stdoutRedactor.Close(), stderrRedactor.Close())
	if closeErr != nil {
		return "", errors.Join(ErrGitFailed, closeErr)
	}
	if runErr != nil {
		return "", errors.Join(ErrGitFailed, runErr)
	}
	if spawnResult.Class != checkoutcontain.ExitSuccess || spawnResult.ExitCode != 0 {
		if op == checkoutcontain.GitGrepLFS && spawnResult.Class == checkoutcontain.ExitFailed && spawnResult.ExitCode == 1 {
			return output.String(), errNoMatch
		}
		return "", fmt.Errorf("%w: %s exit %d", ErrGitFailed, operationName(op), spawnResult.ExitCode)
	}
	return output.String(), nil
}

var errNoMatch = errors.New("git grep found no matches")

func isNoMatch(err error) bool { return errors.Is(err, errNoMatch) }

func pipe() (int, int, error) {
	fds := []int{0, 0}
	if err := unix.Pipe(fds); err != nil {
		return -1, -1, err
	}
	unix.CloseOnExec(fds[0])
	unix.CloseOnExec(fds[1])
	return fds[0], fds[1], nil
}
func closeFDs(fds ...int) {
	for _, fd := range fds {
		if fd >= 0 {
			_ = unix.Close(fd)
		}
	}
}

type limitedWriter struct {
	budget *outputBudget
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	return w.budget.Write(p)
}

type outputBudget struct {
	mu sync.Mutex
	w  io.Writer
	n  int64
}

func (w *outputBudget) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if int64(len(p)) > w.n {
		return 0, errors.New("git output limit")
	}
	w.n -= int64(len(p))
	return w.w.Write(p)
}

func unsafeTree(entries string) bool {
	for _, entry := range strings.Split(strings.TrimSpace(entries), "\n") {
		if entry == "" {
			continue
		}
		parts := strings.SplitN(entry, "\t", 2)
		fields := strings.Fields(parts[0])
		if len(parts) != 2 || len(fields) < 3 || parts[1] == ".gitmodules" || (fields[0] != "100644" && fields[0] != "100755") || fields[1] != "blob" {
			return true
		}
	}
	return false
}
func isLowerHex(v string) bool {
	for _, c := range v {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}
func operationName(op checkoutcontain.GitOperation) string {
	return fmt.Sprintf("typed git operation %d", op)
}
