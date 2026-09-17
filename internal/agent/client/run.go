package agentclient

import (
	"context"
	"errors"
	"log"
	"time"

	"autodeploy/internal/agent/state"
	"autodeploy/internal/queue"
)

const (
	defaultHeartbeatInterval = 20 * time.Second
	minHeartbeatInterval     = 5 * time.Second
	maxHeartbeatInterval     = 5 * time.Minute
	// renewInterval is the ADR 0004 V1 agent renewal cadence.
	renewInterval = 30 * time.Second
)

// Outcome is what an Executor reports back for one lease: either success or
// a classified failure, with the operation_id/result digest this slice's
// completion protocol requires for idempotent replay (ADR 0004).
type Outcome struct {
	Succeeded    bool
	FailureClass queue.FailureClass
	OperationID  string
	ResultDigest [32]byte
}

// Executor performs the actual deployment work for one claimed lease.
// cmd/agent never registers one in this slice (Decision 10: "no-op executor
// rejected" -- cmd/agent never reports a fabricated success): Agent.Executor
// is nil in production, so Run never calls Claim and this interface exists
// solely so the claim/renew/complete/cancel-ack lease lifecycle below is
// unit-testable against a fake before a real Docker/BuildKit executor is
// approved and wired in a later slice.
//
// Execute must return promptly once ctx is cancelled (lost renewal, a stale
// fence, or shutdown) rather than continue mutating host state unboundedly;
// Agent waits for it to return before reporting any outcome.
type Executor interface {
	Execute(ctx context.Context, lease Lease) Outcome
}

// Agent runs the heartbeat loop, and -- only when Executor is non-nil -- the
// claim/renew/complete lease loop, for one enrolled server identity.
type Agent struct {
	Client   *Client
	State    state.Store
	Executor Executor // nil in production this slice.
	Logger   *log.Logger
	Version  string // agent build version reported on every heartbeat.
	StateDir string // used only for the best-effort disk-free capability read.

	// RenewInterval overrides the ADR 0004 30s renewal cadence; zero/negative
	// uses renewInterval. Production leaves this unset -- it exists so
	// tests can exercise the renewal loop without a real 30-second wait.
	RenewInterval time.Duration
}

func (a *Agent) renewInterval() time.Duration {
	if a.RenewInterval > 0 {
		return a.RenewInterval
	}
	return renewInterval
}

func (a *Agent) logger() *log.Logger {
	if a.Logger == nil {
		return log.Default()
	}
	return a.Logger
}

// Run blocks until ctx is cancelled, heartbeating at the interval the
// control plane's last response requested (bounded; defaultHeartbeatInterval
// until the first response). It returns ctx.Err() on cancellation, never a
// heartbeat/claim failure: those are logged and retried on the next tick so
// a transient control-plane outage does not crash the process.
func (a *Agent) Run(ctx context.Context) error {
	ticker := time.NewTicker(defaultHeartbeatInterval)
	defer ticker.Stop()
	for {
		if next := a.tick(ctx); next > 0 {
			ticker.Reset(next)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// tick sends one heartbeat and, only when an Executor is registered,
// attempts one claim afterward. It returns the bounded next heartbeat
// interval, or 0 to leave the current interval unchanged (a failed
// heartbeat should not itself change the cadence the last successful
// response requested).
func (a *Agent) tick(ctx context.Context) time.Duration {
	executorAvailable := a.Executor != nil
	capability := localCapability(a.Version, a.StateDir)
	result, err := a.Client.Heartbeat(ctx, capability, executorAvailable)
	if err != nil {
		// ErrUnauthorized is already logged with a fixed message by the
		// Client itself; every other failure (network/503/etc.) is
		// transient and simply retried on the next tick.
		if !errors.Is(err, ErrUnauthorized) {
			a.logger().Print("agent heartbeat failed")
		}
		return 0
	}
	if executorAvailable {
		a.claimAndRun(ctx)
	}
	return boundedHeartbeatInterval(result.HeartbeatIntervalSeconds)
}

func boundedHeartbeatInterval(seconds int) time.Duration {
	d := time.Duration(seconds) * time.Second
	if d < minHeartbeatInterval || d > maxHeartbeatInterval {
		return defaultHeartbeatInterval
	}
	return d
}

// claimAndRun requests at most one lease and, if one is returned, runs it to
// completion before returning. It is only ever called when a.Executor is
// non-nil (tick's executorAvailable guard); Client.Claim is passed
// executorAvailable=true accordingly, matching internal/agent/http's
// requirement that a false value short-circuits before any real claim.
func (a *Agent) claimAndRun(ctx context.Context) {
	leases, err := a.Client.Claim(ctx, true)
	if err != nil {
		if !errors.Is(err, ErrUnauthorized) {
			a.logger().Print("agent claim failed")
		}
		return
	}
	for _, lease := range leases {
		a.runLease(ctx, lease)
	}
}

// runLease persists the claimed fence locally before acting (Decision 5),
// runs the Executor concurrently with a renewal loop, and reports the
// result as either a completion or a cancellation acknowledgement.
func (a *Agent) runLease(ctx context.Context, lease Lease) {
	stored, existed, err := a.State.Load(lease.EnvironmentID)
	if err != nil {
		a.logger().Print("agent fence state unreadable; refusing to act on environment")
		return
	}
	if existed && (stored.ReconciliationRequired || lease.Fence <= stored.HighestAcceptedFence) {
		a.logger().Print("agent rejecting lease: environment reconciling or fence not advancing")
		return
	}
	if err := a.State.Save(state.Fence{EnvironmentID: lease.EnvironmentID, HighestAcceptedFence: lease.Fence}); err != nil {
		a.logger().Print("agent failed to persist fence state; refusing to act")
		return
	}

	executorCtx, cancelExecutor := context.WithCancel(ctx)
	defer cancelExecutor()
	resultCh := make(chan Outcome, 1)
	go func() { resultCh <- a.Executor.Execute(executorCtx, lease) }()

	ticker := time.NewTicker(a.renewInterval())
	defer ticker.Stop()
	sequence := 0
	cancelled := false
	for {
		select {
		case outcome := <-resultCh:
			cancelExecutor()
			a.reportOutcome(ctx, lease, outcome, cancelled)
			return
		case <-ticker.C:
			sequence++
			renewResult, err := a.Client.Renew(ctx, lease.AttemptID, lease.Fence, sequence)
			if err != nil {
				if errors.Is(err, ErrStaleLease) {
					cancelExecutor()
					a.markReconciliationRequired(lease.EnvironmentID)
					<-resultCh
					return
				}
				// Lost renewal response (network/503): ADR 0004 requires
				// replaying the same sequence, not advancing past a
				// renewal the server may not have durably accepted.
				sequence--
				continue
			}
			if renewResult.CancelRequested && !cancelled {
				cancelled = true
				cancelExecutor()
			}
		case <-ctx.Done():
			cancelExecutor()
			<-resultCh
			return
		}
	}
}

func (a *Agent) reportOutcome(ctx context.Context, lease Lease, outcome Outcome, cancelled bool) {
	var err error
	switch {
	case cancelled:
		err = a.Client.CancelAck(ctx, lease.AttemptID, lease.Fence, outcome.OperationID, outcome.ResultDigest)
	case outcome.Succeeded:
		err = a.Client.Complete(ctx, lease.AttemptID, lease.Fence, queue.CompletionSucceeded, "", outcome.OperationID, outcome.ResultDigest)
	default:
		err = a.Client.Complete(ctx, lease.AttemptID, lease.Fence, queue.CompletionFailed, outcome.FailureClass, outcome.OperationID, outcome.ResultDigest)
	}
	if err != nil {
		a.logger().Print("agent lease outcome report failed")
	}
}

// markReconciliationRequired flips the durable flag for environmentID
// without changing its stored fence (Decision 5: "never reset locally").
func (a *Agent) markReconciliationRequired(environmentID string) {
	stored, _, err := a.State.Load(environmentID)
	if err != nil {
		a.logger().Print("agent could not read fence state to mark reconciliation required")
		return
	}
	stored.EnvironmentID = environmentID
	stored.ReconciliationRequired = true
	if err := a.State.Save(stored); err != nil {
		a.logger().Print("agent could not persist reconciliation-required state")
	}
}
