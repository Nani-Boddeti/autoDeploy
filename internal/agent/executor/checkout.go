package executor

import (
	"context"

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
		// return path (its own deferred cleanup) -- nothing further to
		// release here.
		return CheckoutResult{}, err
	}
	contextDir, err := contextDirFromWorkspace(result.WorkspaceFD())
	if err != nil {
		_ = result.Close()
		return CheckoutResult{}, err
	}
	return CheckoutResult{
		ContextDir: contextDir,
		Binding:    binding,
		Close:      func(context.Context) error { return result.Close() },
	}, nil
}
