package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	agentclient "autodeploy/internal/agent/client"
	"autodeploy/internal/agent/logship"
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
// Two exceptions: the starting phase (phaseRequiresAcceptance), which gates
// every container create/start, and the caller's own context: a cancelled/expired ctx
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

// startingRetryBackoff is the wait before the 2nd and 3rd attempt of a
// required (starting) report: at most three attempts in total. A var so tests
// can shorten it.
var startingRetryBackoff = []time.Duration{time.Second, 2 * time.Second}

// report calls the control plane, logging (never returning) a non-context
// failure, except for a phase that requires acceptance. It returns non-nil
// when ctx itself is done or a required report was not accepted.
//
// A required report is retried only on a positively known transient error
// (agentclient.ErrServiceUnavailable, ErrTransportFailure); a denial, stale
// fence, integrity conflict, invalid request or anything unrecognized is
// final (negative definition), so a refusal never turns into a create.
func (r *progressReporter) report(ctx context.Context, attemptID, fence int64, phase deployment.Status) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := r.client.ReportProgress(ctx, attemptID, fence, phase)
	for retry := 0; err != nil && phaseRequiresAcceptance(phase) && isTransientReportError(err) && retry < len(startingRetryBackoff); retry++ {
		timer := time.NewTimer(startingRetryBackoff[retry])
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		err = r.client.ReportProgress(ctx, attemptID, fence, phase)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if err != nil && phaseRequiresAcceptance(phase) {
		return fmt.Errorf("progress %s not accepted by the control plane: %w", phase, err)
	}
	return nil
}

// isTransientReportError reports whether err is positively known to be a
// transient control-plane or network failure.
func isTransientReportError(err error) bool {
	return errors.Is(err, agentclient.ErrServiceUnavailable) || errors.Is(err, agentclient.ErrTransportFailure)
}

// phaseRequiresAcceptance reports whether a refused or failed progress report
// for phase must stop the attempt. It is the agent half of slice 23-F
// (ADR 0004 amendment 2026-11-21): the control plane lets an expired or
// revoked attempt vacate the slot without the reconciliation block while the
// deployment is at or before building, which is sound only because no
// container is created or started until the starting report was accepted
// under the current fence (the store refuses that report once the attempt is
// no longer the slot's current attempt). Every path that creates or starts a
// container -- build-then-run, stop-then-start, rollback -- reports starting
// first, so requiring acceptance here covers all of them. Earlier phases stay
// best-effort observability: they precede no container call.
func phaseRequiresAcceptance(phase deployment.Status) bool {
	return phase == deployment.StatusStarting
}

// phaseTracker is Execute's local idempotency guard: it never lets the same
// phase be reported twice, regardless of what the control plane would do
// with a repeat. Zero value is ready to use.
type phaseTracker struct {
	last    deployment.Status
	reached bool
	logs    *logship.Attempt // optional; nil-safe
	// record, when non-nil, durably records the phase BEFORE it is reported or
	// begins (slice 23-G), so the attempt record never claims an earlier phase
	// than the work in progress. A failure stops the phase from starting.
	record agentclient.PhaseRecorder
}

// advance reports phase through r unless phase is the phase already
// reported, in which case it is skipped locally without a network call.
func (t *phaseTracker) advance(ctx context.Context, r *progressReporter, attemptID, fence int64, phase deployment.Status) error {
	if t.reached && t.last == phase {
		return ctx.Err()
	}
	if t.record != nil {
		if err := t.record(string(phase)); err != nil {
			return fmt.Errorf("record phase %s: %w", phase, err)
		}
	}
	if err := r.report(ctx, attemptID, fence, phase); err != nil {
		return err
	}
	t.last = phase
	t.reached = true
	t.logs.Linef(logship.StreamPhase, "phase: %s", phase)
	return nil
}
