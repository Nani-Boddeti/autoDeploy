package docker

import (
	"errors"

	"autodeploy/internal/queue"
)

// This file maps this package's own errors and HealthGateResult onto
// queue.FailureClass. It imports internal/queue directly rather than
// returning only this package's own ErrorClass and leaving translation to
// a later wiring step: queue.FailureClass adds no dependency back onto
// internal/runtime/docker (internal/queue imports only internal/github),
// so there is no import cycle and no "ugly dependency" to route around --
// importing it here is the more direct, less error-prone choice, since the
// mapping the plan specifies (Docker/network blips -> transient; bad
// Dockerfile or a failed health gate with the container still running ->
// permanent_configuration; inconsistent local label/fence state ->
// corrupt_state) is expressed once, in the one package that knows the
// full shape of every error it can produce, rather than re-derived by
// every future caller. No new queue.FailureClass values are introduced;
// every branch below maps onto one that already exists.
//
// ClassifyFailure is deliberately package-scoped only, at classification
// granularity that follows ADR 0005 word for word, and is not a general
// "explain any Go error" function: an error this package never returns
// (including a plain context.DeadlineExceeded from a caller's own
// unrelated timeout) falls through to the FailureClassCorruptState
// default, on the theory that an unrecognized failure shape from this
// package is itself a signal something is wrong locally, not a routine
// blip to retry silently.

// ClassifyFailure maps an error returned by this package (from
// EngineError, ErrContainerNameConflict, ErrInconsistentLocalState, or
// build.go's ErrBuildFailed) onto queue.FailureClass. It does not classify
// a health gate's ordinary failure, which carries no error at all --
// see ClassifyHealthGateResult for that.
func ClassifyFailure(err error) queue.FailureClass {
	if err == nil {
		return queue.FailureClassTransient
	}

	var engineErr *EngineError
	if errors.As(err, &engineErr) {
		switch engineErr.Class {
		case ErrClassTransient:
			// Docker/network blips: dial refused/reset/timeout, context
			// deadline, Engine 5xx.
			return queue.FailureClassTransient
		case ErrClassConfiguration:
			// Engine 4xx (bad request shape, missing network/image,
			// invalid local input): retrying unchanged will not help, and
			// this is exactly the "bad Dockerfile"-shaped bucket -- an
			// application/environment problem, not this host's local
			// state being wrong.
			return queue.FailureClassPermanentConfiguration
		case ErrClassMalformed:
			// The engine responded, but not with a shape this package's
			// own invariants allow (e.g. containerNetworkIP finding the
			// container missing from the one network this package itself
			// attached it to). That is exactly "local state disagrees
			// with what Docker actually has."
			return queue.FailureClassCorruptState
		}
	}

	switch {
	case errors.Is(err, ErrContainerNameConflict):
		// A name collision this package's own deterministic naming
		// scheme should make impossible under correct operation -- see
		// ErrContainerNameConflict's doc comment.
		return queue.FailureClassCorruptState
	case errors.Is(err, ErrInconsistentLocalState):
		// The retry-lookup label selector matched more than one
		// container for one identity.
		return queue.FailureClassCorruptState
	case errors.Is(err, ErrBuildFailed):
		// A bad Dockerfile (or any other buildx failure): an
		// application-configuration problem, per the plan text verbatim.
		return queue.FailureClassPermanentConfiguration
	case errors.Is(err, ErrBuildxUnavailable):
		// Missing/broken buildx on this host is this host's own
		// environment being misconfigured, not the repository's.
		return queue.FailureClassPermanentConfiguration
	case errors.Is(err, ErrInvalidBuildRequest), errors.Is(err, ErrInvalidIdentifier), errors.Is(err, ErrMissingIdentifier):
		return queue.FailureClassPermanentConfiguration
	}

	// Unrecognized failure shape from this package: fail closed into
	// corrupt_state rather than guessing it is safe to retry.
	return queue.FailureClassCorruptState
}

// ClassifyHealthGateResult maps a failed HealthGateResult (Success ==
// false) onto queue.FailureClass. Only transient failures are ever retried
// (queue.FailureClass.Retryable), so each reason is weighed against that
// consequence rather than collapsed into one verdict:
//
//   - deadline_exceeded: the gate ran its full deadline with the container
//     running and never got three consecutive successes. The application
//     came up but never became healthy -- an application/configuration
//     problem exactly like a bad Dockerfile, and retrying it unchanged
//     would fail the same way.
//   - context_cancelled: the agent's own context went away (shutdown, a
//     lost lease renewal, a stale fence). Nothing about the application
//     failed, so this must stay retryable; permanently failing a
//     deployment because the agent restarted mid-gate would be wrong.
//     Note this is distinct from a control-plane cancellation, which the
//     agent acknowledges through CancelAck and never routes through here.
//   - container_not_running, OOMKilled: the kernel killed the container
//     under host memory pressure. That is a host condition, not a defect
//     in the deployed application, so it is retryable -- possibly on a
//     less loaded host or a quieter moment.
//   - container_not_running otherwise: the application exited on its own
//     during the gate. A crash on boot is a genuine application defect and
//     retrying it unchanged will reproduce it.
//
// An unrecognized or empty reason (including a Success result, which is a
// caller bug -- callers branch on Success first) falls through to
// corrupt_state, matching ClassifyFailure's fail-closed default rather
// than guessing that an unexplained gate failure is safe to retry.
func ClassifyHealthGateResult(result HealthGateResult) queue.FailureClass {
	switch result.Reason {
	case HealthGateFailureDeadlineExceeded:
		return queue.FailureClassPermanentConfiguration
	case HealthGateFailureContextCancelled:
		return queue.FailureClassTransient
	case HealthGateFailureContainerNotRunning:
		if result.OOMKilled {
			return queue.FailureClassTransient
		}
		return queue.FailureClassPermanentConfiguration
	default:
		return queue.FailureClassCorruptState
	}
}
