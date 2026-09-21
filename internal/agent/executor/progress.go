package executor

import (
	"context"

	"autodeploy/internal/deployment"
)

// controlPlaneProgress is the narrow control-plane capability this file
// needs. *agentclient.Client satisfies it structurally; tests use a fake.
type controlPlaneProgress interface {
	ReportProgress(ctx context.Context, attemptID, fence int64, phase deployment.Status) error
}

// progressReporter reports execution phases to the control plane, never
// aborting Execute over a report failure -- ReportProgress's own doc says a
// caller should not abort execution because a report failed, since a
// genuine authorization failure surfaces again on the next lease renewal.
// The one exception is the caller's own context: a cancelled/expired ctx
// must make Execute return promptly (Agent.runLease waits for it), so
// report still surfaces ctx.Err().
//
// phaseTracker (below) is the reason this type does not simply call
// agentclient.Client.ReportProgress directly: deployment.canTransition(X, X)
// is false (see tasks/lessons -- "deployment.canTransition(X, X) is false,
// so a repeated status is rejected rather than absorbed"), so anything that
// might report the same phase twice must not rely on the control plane's
// own transition rules to make that safe.
//
// What the control plane actually does with a repeat, established by
// reading (*postgres.AgentLeaseRepository).ReportProgress
// (internal/store/postgres/agent_lease_materialize.go): it compares the
// requested phase's deployment.ExecutionOrder against the deployment's
// current status and, when the requested phase is at or behind the current
// one, commits the transaction without calling transitionDeployment at
// all -- so canTransition(X, X) is never reached and the call returns
// success, not an error. This is not an assumption; it is proven twice:
// TestReportProgressAbsorbsDuplicateAndStalePhases
// (internal/store/postgres/agent_lease_materialize_test.go) proves it at
// the repository layer, and TestAgentHTTPFullLifecycle
// (internal/agent/http/handler_integration_test.go) proves it through the
// actual HTTP route agentclient.Client.ReportProgress calls -- both against
// a real PostgreSQL 18 database (AUTODEPLOY_TEST_DATABASE_URL,
// -tags=integration).
//
// phaseTracker still does not lean on that server-side behavior: it tracks
// the last phase it reported locally and skips a call outright when asked
// to report the phase it is already at. That is defense in depth, not the
// only thing standing between a duplicate report and a failed deployment --
// belt (local skip) and suspenders (proven server-side absorption), so a
// bug in either one alone does not break progress reporting.
type progressReporter struct {
	client controlPlaneProgress
}

func newProgressReporter(client controlPlaneProgress) *progressReporter {
	return &progressReporter{client: client}
}

// report calls the control plane, logging (never returning) a non-context
// failure. It returns non-nil only when ctx itself is done.
func (r *progressReporter) report(ctx context.Context, attemptID, fence int64, phase deployment.Status) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_ = r.client.ReportProgress(ctx, attemptID, fence, phase)
	return ctx.Err()
}

// phaseTracker is Execute's local idempotency guard: it never lets the same
// phase be reported twice, regardless of what the control plane would do
// with a repeat. Zero value is ready to use.
type phaseTracker struct {
	last    deployment.Status
	reached bool
}

// advance reports phase through r unless phase is the phase already
// reported, in which case it is skipped locally without a network call.
func (t *phaseTracker) advance(ctx context.Context, r *progressReporter, attemptID, fence int64, phase deployment.Status) error {
	if t.reached && t.last == phase {
		return ctx.Err()
	}
	if err := r.report(ctx, attemptID, fence, phase); err != nil {
		return err
	}
	t.last = phase
	t.reached = true
	return nil
}
