// Package dockerid holds the pure, dependency-free identifier derivations
// that both internal/runtime/docker (the agent-side Engine API client) and
// internal/runtime/traefik (the route-document renderer, now also imported
// directly by the control plane -- see Step 10c) need to agree on, without
// either one pulling in the other's host-touching code.
//
// Before this package existed, internal/runtime/traefik's names.go/route.go
// imported internal/runtime/docker only for docker.ContainerName -- a pure
// string transform -- but importing that package also compiles engine.go
// and client_linux.go/client_unsupported.go into the caller's binary: a
// hand-rolled HTTP-over-Unix-socket client that dials the Docker daemon.
// That is harmless in the agent binary (which already needs it), but Step
// 10c's control plane needs internal/runtime/traefik.Render/EnvironmentRoute
// to compute a route digest in its own activation transaction, and the
// control plane must never link code that dials a Docker socket at all
// (ADR 0005's control-plane/agent boundary). Moving ContainerName's pure
// logic here, with no import of internal/runtime/docker in either
// direction, removes that coupling: internal/runtime/docker now calls into
// this package too (see run.go), so there is exactly one implementation,
// and internal/runtime/traefik (and therefore the control plane) never
// compiles engine.go/client_linux.go at all.
//
// AttemptRuntimeInstanceID lives here for the same reason: Step 10c's
// control plane must derive the same opaque runtime-instance ID the agent's
// executor already computes (internal/agent/executor's former
// attemptOpaqueID) so both sides name the same backend service in the route
// document, without the control plane importing internal/agent/executor
// (an agent-side package) at all.
package dockerid

import (
	"fmt"
	"regexp"
	"strconv"
)

// ErrInvalidIdentifier mirrors internal/runtime/docker's own sentinel of the
// same name (validate.go): it is not the same variable, and nothing outside
// this package's own ContainerName return value is expected to compare
// against it with errors.Is -- see run.go's wrapping comment for why that is
// safe.
var ErrInvalidIdentifier = fmt.Errorf("dockerid: identifier is not a validated opaque id")

// opaqueIDPattern is internal/runtime/docker's validate.go opaqueIDPattern,
// restated here rather than imported (the same duplication precedent
// internal/runtime/traefik/route.go's identifierPattern already documents
// for the same reason): letters, digits, '-', '_' only, bounded length.
var opaqueIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// ContainerNamePrefix and containerNamePattern mirror
// internal/runtime/docker/run.go's own containerNamePrefix/
// containerNamePattern exactly. The prefix is exported so
// internal/runtime/docker/run_test.go's existing assertions (written
// against the package-local constant) keep working unchanged against a
// same-valued alias -- see run.go.
const ContainerNamePrefix = "autodeploy-rt-"

var containerNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]+$`)

// ContainerName builds the collision-checked, safe container name for a
// release runtime container: ContainerNamePrefix plus the immutable,
// opaque, internal runtime-instance ID. See
// internal/runtime/docker/run.go's ContainerName doc comment (which now
// wraps this function) for the full rationale; this is a pure string
// transform with no Docker Engine API dependency.
func ContainerName(runtimeInstanceID string) (string, error) {
	if !opaqueIDPattern.MatchString(runtimeInstanceID) {
		return "", fmt.Errorf("%w: runtime-instance-id", ErrInvalidIdentifier)
	}
	name := ContainerNamePrefix + runtimeInstanceID
	if len(name) > 255 {
		return "", fmt.Errorf("%w: container name exceeds 255 bytes", ErrInvalidIdentifier)
	}
	if !containerNamePattern.MatchString(name) {
		return "", fmt.Errorf("%w: container name %q is not Docker-safe", ErrInvalidIdentifier, name)
	}
	return name, nil
}

// AttemptRuntimeInstanceID derives a stable, opaque identifier from an
// attempt's own durable primary key (agent_job_attempts.id). It was
// internal/agent/executor's private attemptOpaqueID (used as both
// RuntimeInstanceID and BuiltImageIdentity.BuildOperationID there -- see
// that package's doc comment on the function it now calls here for the full
// stability argument); Step 10c's control plane needs the identical value
// independently, to name the same backend runtime instance in the route
// document it renders in tx1, before the agent has told it anything beyond
// attempt_id/fence/image_digest. Moving it here gives both sides one
// implementation instead of two literals that could drift.
func AttemptRuntimeInstanceID(attemptID int64) string {
	return strconv.FormatInt(attemptID, 10)
}
