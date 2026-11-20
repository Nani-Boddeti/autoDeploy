package executor

import (
	"context"
	"errors"
	"time"

	agentcheckout "autodeploy/internal/agent/checkout"
	"autodeploy/internal/gitcheckout"
	"autodeploy/internal/githubapp"
)

// CheckoutResult is what one lease-authorized checkout produces: a build
// context directory plus the exact source identity it was checked out from,
// and Close, which releases the underlying workspace allocation and the
// checkout credential. Close must be called exactly once, and ContextDir
// must not be used after Close.
type CheckoutResult struct {
	ContextDir string
	Binding    githubapp.CheckoutBinding
	Close      func(context.Context) error
}

// CheckoutSeam performs one lease-authorized checkout for attemptID/fence.
// It is the seam Execute tests against a fake; production wiring is
// newProductionCheckout, driving internal/agent/checkout.Begin (the
// credential lifecycle -- ADR 0001 forbids this package from holding a
// GitHub App signer or a ReservationStore, and it never does) over
// internal/gitcheckout.Runner.Checkout.
type CheckoutSeam interface {
	Checkout(ctx context.Context, attemptID, fence int64) (CheckoutResult, error)
}

// checkoutRunner is the one gitcheckout.Runner method this package needs,
// narrowed to an interface so tests never have to construct a real
// *gitcheckout.Runner -- which needs privileged, root-owned quota/spawn
// helper sockets this package has no business faking.
type checkoutRunner interface {
	Checkout(ctx context.Context, lease gitcheckout.Lease) (*gitcheckout.Result, error)
}

type productionCheckout struct {
	client agentcheckout.Client
	runner checkoutRunner
}

func newProductionCheckout(client agentcheckout.Client, runner checkoutRunner) *productionCheckout {
	return &productionCheckout{client: client, runner: runner}
}

// Checkout reserves and starts a checkout credential (agentcheckout.Begin),
// drives the fixed Git checkout over it, and resolves the resulting
// workspace descriptor to a build-context path (contextDirFromWorkspace).
// It never touches an App signer or a ReservationStore itself -- both stay
// control-plane-only per ADR 0001; this package's only capability is the
// short-lived token agentcheckout.Lease fetches over the already-
// authenticated agent channel.
func (p *productionCheckout) Checkout(ctx context.Context, attemptID, fence int64) (CheckoutResult, error) {
	lease, err := agentcheckout.Begin(ctx, p.client, attemptID, fence)
	if err != nil {
		return CheckoutResult{}, err
	}
	binding := lease.Binding()
	result, err := p.runner.Checkout(ctx, lease)
	if err != nil {
		// gitcheckout.Checkout already closes the lease itself on every
		// return path (its own deferred cleanup). The one thing it can hand
		// back still owned is a Result whose mandatory cleanup failed
		// (*gitcheckout.CleanupError); dropping it would strand the
		// workspace allocation, so release it with the bounded retry.
		var pending *gitcheckout.CleanupError
		if errors.As(err, &pending) && pending.Result != nil {
			if closeErr := releaseOwned(ctx, pending.Result, closeBackoff); closeErr != nil {
				return CheckoutResult{}, errors.Join(err, closeErr)
			}
		}
		return CheckoutResult{}, err
	}
	contextDir, err := contextDirFromWorkspace(result.WorkspaceFD())
	if err != nil {
		if closeErr := releaseOwned(ctx, result, closeBackoff); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
		return CheckoutResult{}, err
	}
	return CheckoutResult{
		ContextDir: contextDir,
		Binding:    binding,
		Close: func(ctx context.Context) error {
			return closeWithRetry(ctx, result.Close, func() bool { return result.WorkspaceFD() < 0 }, closeBackoff)
		},
	}, nil
}

// ownedResult is the part of *gitcheckout.Result a failure path needs.
type ownedResult interface {
	Close() error
	WorkspaceFD() int
}

// releaseOwned closes a still-owned Result on a failure path with the same
// bounded retry as the success-path Close. The caller's ctx is usually the
// reason we are failing (cancelled), so the retries run on a detached,
// time-boxed context instead.
func releaseOwned(ctx context.Context, r ownedResult, backoff []time.Duration) error {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseBudget)
	defer cancel()
	return closeWithRetry(cctx, r.Close, func() bool { return r.WorkspaceFD() < 0 }, backoff)
}

// releaseBudget caps the total time spent releasing on a failure path.
const releaseBudget = 15 * time.Second

// closeBackoff is the wait before each retry of a failed workspace release;
// its length bounds the retries (len+1 attempts in all).
var closeBackoff = []time.Duration{500 * time.Millisecond, 1 * time.Second, 2 * time.Second, 4 * time.Second}

// closeWithRetry drives gitcheckout.Result.Close, which on a retryable quota
// failure keeps the workspace descriptor (the quota protocol needs that same
// capability to retry) and tries again on the next call. Nothing else retries
// it, so a transient quota-service failure would strand the allocation. It
// retries synchronously -- no goroutine -- stopping at success, at a terminal
// close (released() reports the descriptor gone, so a repeat would only return
// the cached error), when the backoff schedule is exhausted, or when ctx ends
// (the caller's detached cleanup budget). The last error is returned.
func closeWithRetry(ctx context.Context, closeFn func() error, released func() bool, backoff []time.Duration) error {
	err := closeFn()
	for i := 0; err != nil && !released() && i < len(backoff); i++ {
		t := time.NewTimer(backoff[i])
		select {
		case <-ctx.Done():
			t.Stop()
			return errors.Join(err, ctx.Err())
		case <-t.C:
		}
		err = closeFn()
	}
	return err
}
