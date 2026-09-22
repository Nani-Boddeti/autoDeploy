package traefik

import (
	"errors"

	"autodeploy/internal/queue"
)

// This file maps this package's own errors onto queue.FailureClass,
// mirroring internal/runtime/docker/classify.go's approach (one function
// per failure surface, each branch justified against ADR 0005's own
// words) rather than sharing types across the two packages -- see
// client.go's ErrorClass doc comment for why the enum itself is
// redeclared rather than imported.
//
// Both functions below are deliberately narrow: an error either package
// never returns falls through to FailureClassCorruptState, exactly like
// docker.ClassifyFailure's own default, on the same fail-closed theory --
// an unrecognized failure shape from this package is a signal something
// about this host's own state is wrong, not a routine condition safe to
// retry silently.

// ClassifyWrite maps an error returned by Write, Remove, or ReadDigest onto
// queue.FailureClass. Per ADR 0005's activation procedure and Slice 10's
// own plan text, this classification is meaningful only for a write
// attempted *before* switch authorization -- a caller past that point must
// not use this to decide whether to retry a fresh attempt at all (any
// post-authorization failure is reconciliation-required, never a fresh
// retry; see this package's doc comment and Step 10d's classification
// rules for the boundary this function does not itself enforce).
//
//   - ErrSymlinkRefused, ErrDirectoryOwnership, ErrNotDirectory,
//     ErrDirectoryPermissions: the configured route directory itself is not
//     what this host was provisioned to have -- a host-configuration
//     defect, not a transient condition. permanent_configuration, matching
//     docker's ErrBuildxUnavailable precedent ("missing/broken buildx on
//     this host is this host's own environment being misconfigured").
//   - ErrNotRegularFile, ErrRouteFileTooLarge: something other than this
//     package's own atomic write left an unexpected object -- a
//     non-regular file, or a regular file far larger than anything this
//     package has ever rendered -- at the target path. Local state
//     disagreeing with what this package itself should have put there.
//     corrupt_state, matching docker's ErrInconsistentLocalState precedent.
//   - ErrInvalidRoute: the caller supplied a document this package refuses
//     to render at all. Retrying the identical input will fail identically,
//     so this is the write-time echo of "rendered document rejected" --
//     permanent_configuration.
//   - Everything else (open/write/fsync/rename I/O errors: disk full,
//     EIO, a transient rename failure): transient, exactly as the Slice 10
//     plan text specifies ("write ... failures transient ... before switch
//     authorization only").
func ClassifyWrite(err error) queue.FailureClass {
	if err == nil {
		return queue.FailureClassTransient
	}
	switch {
	case errors.Is(err, ErrSymlinkRefused),
		errors.Is(err, ErrDirectoryOwnership),
		errors.Is(err, ErrNotDirectory),
		errors.Is(err, ErrDirectoryPermissions):
		return queue.FailureClassPermanentConfiguration
	case errors.Is(err, ErrNotRegularFile), errors.Is(err, ErrRouteFileTooLarge):
		return queue.FailureClassCorruptState
	case errors.Is(err, ErrInvalidRoute):
		return queue.FailureClassPermanentConfiguration
	default:
		return queue.FailureClassTransient
	}
}

// ClassifyObservation maps an error returned by Observer.Verify (or
// ObserveRouter/ObserveService directly) onto queue.FailureClass:
//
//   - ErrNonLoopbackBaseURL: this host's own Traefik API base URL is
//     configured wrong. permanent_configuration -- no retry fixes a bad
//     configuration value.
//   - ErrRouterNotLoaded, ErrServiceNotLoaded: "not yet loaded" -- Traefik
//     has not finished an asynchronous reload yet. transient, so a caller
//     polling within its own bounded deadline (per ADR 0005's "Traefik
//     reload is asynchronous") keeps retrying; a caller past its deadline
//     is responsible for treating repeated transient failures as
//     reconciliation-required itself, exactly as Step 10d's own
//     before/after-authorization split already requires regardless of the
//     class this function reports.
//   - ErrConfigRejected: Traefik loaded the name but reports a provider
//     error -- the rendered document itself was rejected.
//     permanent_configuration: retrying the identical document will be
//     rejected identically. A WRONG classification here (e.g. transient)
//     would silently retry a deployment forever against a document that
//     can never load -- this is exactly the case the plan calls out by
//     name.
//   - ErrRuleMismatch, ErrTLSMismatch, ErrWrongBackend: the name loaded
//     cleanly but does not match what was authorized -- observed state
//     disagreeing with desired state. corrupt_state: this is not "the
//     input was bad" (permanent_configuration) or "try again shortly"
//     (transient), it is "what is actually being served is not provably
//     what was authorized", which per ADR 0005 must block for
//     reconciliation rather than be silently retried OR silently accepted.
//     A WRONG classification here (permanent_configuration, discarding the
//     deployment) would abandon a route that might in fact still be
//     recoverable by reconciliation; a WRONG classification as transient
//     would keep re-attempting fresh activations against a host that is
//     already serving something unexpected.
//   - *ObservationError{Class: ErrClassTransient} (dial/connect/timeout/
//     unexpected non-2xx-non-404 status): transient.
//   - *ObservationError{Class: ErrClassMalformed} (oversized body, refused
//     redirect, undecodable JSON): corrupt_state -- a loopback-only,
//     single-purpose management API answering outside its documented shape
//     is itself a local-inconsistency signal, exactly like docker's own
//     ErrClassMalformed precedent.
//   - *ObservationError{Class: ErrClassConfiguration}: permanent_configuration.
func ClassifyObservation(err error) queue.FailureClass {
	if err == nil {
		return queue.FailureClassTransient
	}
	switch {
	case errors.Is(err, ErrNonLoopbackBaseURL):
		return queue.FailureClassPermanentConfiguration
	case errors.Is(err, ErrRouterNotLoaded), errors.Is(err, ErrServiceNotLoaded):
		return queue.FailureClassTransient
	case errors.Is(err, ErrConfigRejected):
		return queue.FailureClassPermanentConfiguration
	case errors.Is(err, ErrRuleMismatch), errors.Is(err, ErrTLSMismatch), errors.Is(err, ErrWrongBackend):
		return queue.FailureClassCorruptState
	}

	var obsErr *ObservationError
	if errors.As(err, &obsErr) {
		switch obsErr.Class {
		case ErrClassTransient:
			return queue.FailureClassTransient
		case ErrClassMalformed:
			return queue.FailureClassCorruptState
		case ErrClassConfiguration:
			return queue.FailureClassPermanentConfiguration
		}
	}

	return queue.FailureClassCorruptState
}
