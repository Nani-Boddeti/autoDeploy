package docker

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// This file is Step 6a: creating and starting a release's runtime container
// over the Engine API client extended in engine.go. See ADR 0005
// ("Immutable identity, names, and labels", "Network and port model") for
// the invariants enforced here, and the Step 4/5 result blocks in
// tasks/todo.md for the obligations this file discharges (the release-scoped
// selector needs the resource's full identity; ReleaseRuntimeContainerLabels
// is the single place labels are built).
//
// No executor wiring and no cmd/agent changes live here -- that is a later
// step. Everything in this file is untagged: it runs the same HTTP-over-
// unix-socket calls engine.go already proved testable on any OS via a fake
// listener, so there is no platform gate to add.

// containerNamePrefix makes every AutoDeploy-managed container name
// recognizable at a glance and, combined with the opaque runtime-instance ID
// appended after it, gives Docker's own name uniqueness a namespace that
// cannot collide with an operator's own unrelated containers.
const containerNamePrefix = "autodeploy-rt-"

// containerNamePattern is Docker's own container-name constraint
// (^[a-zA-Z0-9][a-zA-Z0-9_.-]+$) restated here so ContainerName fails a bad
// name before ever reaching the Engine API, not just after a 400 comes
// back. requireOpaqueID's charset ([A-Za-z0-9_-]) is already a strict subset
// of Docker's, so this only needs to check the prefixed result's length.
var containerNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]+$`)

// ContainerName builds the collision-checked, safe container name for a
// release runtime container: containerNamePrefix plus the immutable,
// opaque, internal runtime-instance ID. It is deliberately not derived from
// any user input, branch name, repository name, or commit message -- only
// requireOpaqueID's validated identifier -- and it is deterministic, which
// is what makes it collision-checkable: the same runtime-instance ID always
// produces the same name, so a genuine retry of the same attempt resolves
// to a name Docker itself already holds under this package's own labels
// (found by the label-selector lookup, see FindReleaseRuntimeContainer),
// while any other collision under that name is necessarily foreign state
// (see ErrContainerNameConflict in engine.go).
func ContainerName(runtimeInstanceID string) (string, error) {
	id, err := requireOpaqueID("runtime-instance-id", runtimeInstanceID)
	if err != nil {
		return "", err
	}
	name := containerNamePrefix + id
	if len(name) > 255 {
		return "", fmt.Errorf("%w: container name exceeds 255 bytes", ErrInvalidIdentifier)
	}
	if !containerNamePattern.MatchString(name) {
		return "", fmt.Errorf("%w: container name %q is not Docker-safe", ErrInvalidIdentifier, name)
	}
	return name, nil
}

// networkIDPattern matches a Docker network ID exactly: 12- or 64-character
// lowercase hex, both of which are forms the Engine API itself returns from
// NetworkInspect.ID. Critically, no Docker "magic" network-mode string
// ("host", "none", "bridge", "container:<id>") can ever match this pattern,
// so requireNetworkID is the structural reason HostConfig.NetworkMode can
// never become host networking: the only way a value reaches that field is
// by first coming back from a successful InspectNetwork call and passing
// this check.
var networkIDPattern = regexp.MustCompile(`^[a-f0-9]{12}$|^[a-f0-9]{64}$`)

func requireNetworkID(id string) (string, error) {
	if !networkIDPattern.MatchString(id) {
		return "", fmt.Errorf("%w: network id %q is not a resolved Docker network ID", ErrInvalidIdentifier, id)
	}
	return id, nil
}

// RestartPolicyUnlessStopped and RestartPolicyOnFailure are the only two
// restart policies this package accepts, mirroring the CHECK constraint on
// environment_runtime_configs.restart_policy /
// agent_job_attempt_execution.restart_policy (migrations 000014/000016).
// They also happen to be exactly the Docker Engine API restart-policy names,
// so no translation table is needed.
const (
	RestartPolicyUnlessStopped = "unless-stopped"
	RestartPolicyOnFailure     = "on-failure"
)

// millisPerCPU is the NanoCpus-per-millicpu conversion factor: Docker's
// NanoCpus field is nanoseconds of CPU time per second (1e9 == 1 full CPU),
// and the frozen config's cpu_millis is thousandths of a CPU (1000 ==
// 1 full CPU), so nanocpus = cpu_millis * 1e6.
const millisPerCPU = 1_000_000

// RunContainerRequest is everything Step 6a needs to create and start one
// release runtime container. Every field is required and validated before
// any Engine API call is made.
type RunContainerRequest struct {
	// Identity supplies the container's immutable release/runtime/
	// attempt/fence label set (see ReleaseResourceIdentity) and, via
	// Identity.ImageDigest, the image reference this container runs.
	//
	// Identity.ImageDigest is NOT what this package runs the container
	// by, and that is deliberate: ImageDigest is the manifest digest
	// (buildx's containerimage.digest), which only exists once an image
	// has been pushed to or pulled from a registry. Step 5's build runs
	// `buildx build --load`, which imports the image straight into the
	// local Docker image store with no registry round-trip at all, and a
	// locally loaded image is addressable there only by its image ID --
	// which is its config digest (buildx's containerimage.config.digest,
	// carried on ReleaseResourceIdentity.ConfigDigest). So the container
	// is created with Image "sha256:<ConfigDigest>": still an immutable
	// content digest, never a tag, just the one digest that is actually
	// resolvable in this host's local image store. See
	// buildContainerCreatePayload.
	Identity ReleaseResourceIdentity

	// NetworkID is the stable environment network's resolved Docker
	// network ID (from InspectNetwork -- Ensure resolves and validates
	// this itself given a network name; see EnsureReleaseRuntimeContainer).
	NetworkID string

	CPUMillis     int
	MemoryBytes   int64
	RestartPolicy string
}

func (r RunContainerRequest) validate() error {
	if r.CPUMillis <= 0 {
		return fmt.Errorf("%w: cpu millis must be positive", ErrInvalidIdentifier)
	}
	if r.MemoryBytes <= 0 {
		return fmt.Errorf("%w: memory bytes must be positive", ErrInvalidIdentifier)
	}
	switch r.RestartPolicy {
	case RestartPolicyUnlessStopped, RestartPolicyOnFailure:
	default:
		return fmt.Errorf("%w: unsupported restart policy %q", ErrInvalidIdentifier, r.RestartPolicy)
	}
	if _, err := requireNetworkID(r.NetworkID); err != nil {
		return err
	}
	return nil
}

// buildContainerCreatePayload translates a validated RunContainerRequest
// plus its already-built labels into the exact wire request engine.go's
// CreateContainer sends. It is pure (no I/O), which is what makes every
// non-negotiable in it directly testable: no published ports (the struct
// literal below sets neither PortBindings -- which containerHostConfig does
// not even declare -- nor PublishAllPorts), exactly one network (NetworkMode
// is a single string), and the image reference is the digest form, never a
// tag.
func buildContainerCreatePayload(req RunContainerRequest, labels map[string]string) containerCreateRequest {
	return containerCreateRequest{
		Image:  "sha256:" + strings.TrimPrefix(req.Identity.ConfigDigest, "sha256:"),
		Labels: labels,
		HostConfig: containerHostConfig{
			NetworkMode: req.NetworkID,
			RestartPolicy: containerRestartPolicy{
				Name: req.RestartPolicy,
			},
			NanoCPUs: int64(req.CPUMillis) * millisPerCPU,
			Memory:   req.MemoryBytes,
		},
	}
}

// ErrInconsistentLocalState is returned by EnsureReleaseRuntimeContainer
// when the retry-lookup label selector matches more than one container.
// Under ADR 0005's exact-label-match discipline, one release/attempt/fence
// identity must own at most one runtime container; finding more than one
// means local state (or a prior partial cleanup) disagrees with what Docker
// actually has, and the correct response is to fail closed into
// reconciliation, not to guess which one is authoritative.
var ErrInconsistentLocalState = errors.New("docker: label selector matched more than one container")

// FindReleaseRuntimeContainer looks up a prior attempt's runtime container
// by exact label match on its full identity -- the retry-idempotency lookup
// Step 4's result block obligates this step to perform before creating
// anything. It returns (nil, nil) when no prior container exists, the
// container summary when exactly one does, and ErrInconsistentLocalState
// when more than one does.
//
// This can only ever reconstruct the selector from
// ReleaseResourceIdentity's fields, which is what
// release_runtime_instances (migration 000014) actually stores per attempt
// (release_id, job_id, attempt_id, fence, container_name) plus whatever the
// caller already knows from the release/build records (deployment,
// environment, server, owner, project, commit SHA, image/config digests).
// It does NOT and cannot widen the selector to, say, release_id alone: ADR
// 0005 forbids that, and release_runtime_instances carries no broader key
// that would make a wider lookup meaningful anyway.
func (c *Client) FindReleaseRuntimeContainer(ctx context.Context, id ReleaseResourceIdentity) (*ContainerSummary, error) {
	selector, err := ReleaseRuntimeContainerSelector(id)
	if err != nil {
		return nil, err
	}
	matches, err := c.ListContainersByLabels(ctx, selector)
	if err != nil {
		return nil, err
	}
	switch len(matches) {
	case 0:
		return nil, nil
	case 1:
		return &matches[0], nil
	default:
		return nil, fmt.Errorf("%w: kind=%s", ErrInconsistentLocalState, KindReleaseRuntimeContainer)
	}
}

// EnsureReleaseRuntimeContainer is the idempotent create-and-start entry
// point: it first looks up a prior attempt's container by exact label
// match (FindReleaseRuntimeContainer); if one exists it is reused (started
// again if not already running, which StartContainer already treats as a
// no-op) rather than creating a second container for the same identity.
// Only when no prior container exists does it resolve the environment
// network (failing closed if absent -- network creation is environment
// provisioning, never done here) and create one.
//
// networkNameOrID is the stable environment network's name or ID; it is an
// operator/provisioning-time value, never user input, and is resolved via
// InspectNetwork to its canonical ID before it can ever reach
// HostConfig.NetworkMode.
func (c *Client) EnsureReleaseRuntimeContainer(ctx context.Context, req RunContainerRequest, networkNameOrID string) (ContainerInspect, error) {
	if networkNameOrID == "" {
		return ContainerInspect{}, fmt.Errorf("%w: empty environment network name", ErrInvalidIdentifier)
	}

	if existing, err := c.FindReleaseRuntimeContainer(ctx, req.Identity); err != nil {
		return ContainerInspect{}, err
	} else if existing != nil {
		if err := c.StartContainer(ctx, existing.ID); err != nil {
			return ContainerInspect{}, err
		}
		return c.InspectContainer(ctx, existing.ID)
	}

	network, err := c.InspectNetwork(ctx, networkNameOrID)
	if err != nil {
		return ContainerInspect{}, err
	}
	networkID, err := requireNetworkID(network.ID)
	if err != nil {
		return ContainerInspect{}, err
	}
	req.NetworkID = networkID
	if err := req.validate(); err != nil {
		return ContainerInspect{}, err
	}

	labels, err := ReleaseRuntimeContainerLabels(req.Identity)
	if err != nil {
		return ContainerInspect{}, err
	}
	name, err := ContainerName(req.Identity.RuntimeInstanceID)
	if err != nil {
		return ContainerInspect{}, err
	}
	payload := buildContainerCreatePayload(req, labels)

	id, err := c.CreateContainer(ctx, name, payload)
	if err != nil {
		// A name conflict means something already holds this identity's
		// deterministic name. Between the lookup above and this create
		// there is a window in which a concurrent execution for the same
		// identity -- a lease-timeout re-dispatch racing a still-live
		// attempt, say -- can win the create. That race has already
		// resolved correctly: exactly one container exists and it is the
		// one this call wanted. Re-run the exact-label lookup and adopt
		// it rather than failing the deployment into corrupt_state, which
		// is never retried. Only a name held by something that does NOT
		// match this identity is genuinely inconsistent local state.
		if errors.Is(err, ErrContainerNameConflict) {
			existing, findErr := c.FindReleaseRuntimeContainer(ctx, req.Identity)
			if findErr != nil {
				return ContainerInspect{}, findErr
			}
			if existing == nil {
				return ContainerInspect{}, err
			}
			if startErr := c.StartContainer(ctx, existing.ID); startErr != nil {
				return ContainerInspect{}, startErr
			}
			return c.InspectContainer(ctx, existing.ID)
		}
		return ContainerInspect{}, err
	}
	if err := c.StartContainer(ctx, id); err != nil {
		return ContainerInspect{}, err
	}
	return c.InspectContainer(ctx, id)
}
