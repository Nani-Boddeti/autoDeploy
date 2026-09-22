// Package executor assembles Slice 9b's real Executor (tasks/todo.md, Step
// 6b) from the pieces Steps 4, 5 and 6a built: internal/agent/checkout's
// credential lifecycle over internal/gitcheckout's fixed Git checkout,
// internal/runtime/docker's buildx build, container run, and ADR 0005
// health gate. It implements internal/agent/client.Executor.
//
// cmd/agent constructs an Executor but does not register it on
// agentclient.Agent yet -- see cmd/agent's executorEnabled constant. This
// package's own logic is fully wired and tested against fakes; what remains
// unproven is stated in the Step 6b report, not hidden here.
package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"strconv"
	"sync"
	"time"

	agentcheckout "autodeploy/internal/agent/checkout"
	agentclient "autodeploy/internal/agent/client"
	"autodeploy/internal/deployment"
	"autodeploy/internal/gitcheckout"
	"autodeploy/internal/queue"
	"autodeploy/internal/runtime/docker"
)

// probeBuildxFunc is docker.ProbeBuildx, indirected through a package
// variable so NewExecutor's fail-closed startup probe is testable without a
// real docker/buildx CLI on PATH. Production never reassigns it.
var probeBuildxFunc = docker.ProbeBuildx

// ControlPlane is the complete control-plane capability this package needs:
// the checkout RPCs (ADR 0001 -- no App signer or ReservationStore here),
// secret materialization, and progress reporting. *agentclient.Client
// satisfies it structurally; tests use a fake.
type ControlPlane interface {
	agentcheckout.Client
	controlPlaneSecrets
	controlPlaneProgress
}

// dockerRuntime is the subset of *docker.Client's method set Execute calls
// directly (EnsureReleaseRuntimeContainer, RunHealthGate) plus the startup
// capability probe (Probe). docker.Build is a package-level function, not a
// method, so it is injected separately (see Config.buildFunc/Executor.build).
type dockerRuntime interface {
	Probe(ctx context.Context) (docker.VersionInfo, error)
	EnsureReleaseRuntimeContainer(ctx context.Context, req docker.RunContainerRequest, networkNameOrID string) (docker.ContainerInspect, error)
	RunHealthGate(ctx context.Context, containerID, networkID string, healthPolicyRevision int, target docker.HealthGateTarget) (docker.HealthGateResult, error)
}

// Config is everything NewExecutor needs to assemble a production Executor.
type Config struct {
	// ControlPlane is the agent's own control-plane client
	// (*agentclient.Client in production).
	ControlPlane ControlPlane
	// Docker is a constructed Engine API client (docker.NewClient in
	// production, which is Linux-only and fails closed elsewhere).
	Docker dockerRuntime
	// CheckoutRunner drives the fixed Git checkout (gitcheckout.
	// NewProductionRunner in production, which needs privileged local
	// quota/spawn helper sockets and is Linux-only).
	CheckoutRunner checkoutRunner
	// ServerID is this agent's own enrolled server identity, used as
	// docker.ReleaseResourceIdentity.ServerID.
	ServerID string
	// Network is the environment's stable Docker network name or ID that
	// EnsureReleaseRuntimeContainer joins the candidate to.
	Network string
	// BuildOutput receives buildx's redacted stdout/stderr. Optional; a nil
	// value discards build output.
	BuildOutput io.Writer
	Logger      *log.Logger
}

// Executor implements agentclient.Executor. Every dependency is a narrow
// interface (or, for docker.Build, a func value) so Execute is fully
// unit-testable against fakes without a real Docker daemon or privileged
// checkout helpers.
type Executor struct {
	checkout     CheckoutSeam
	docker       dockerRuntime
	build        func(ctx context.Context, req docker.BuildRequest) (docker.BuildResult, error)
	controlPlane ControlPlane
	serverID     string
	network      string
	buildOutput  io.Writer
	logger       *log.Logger
}

// NewExecutor validates cfg, runs the startup capability probe (Docker
// Engine API version compatibility, then buildx), and returns an assembled
// Executor. It fails closed: an incapable host never gets an Executor,
// matching the plan's "missing buildx or unreachable engine must fail
// closed at enrollment, not mid-deployment."
func NewExecutor(ctx context.Context, cfg Config) (*Executor, error) {
	if cfg.ControlPlane == nil || cfg.Docker == nil || cfg.CheckoutRunner == nil || cfg.ServerID == "" || cfg.Network == "" {
		return nil, errors.New("agent executor: incomplete configuration")
	}
	if _, err := cfg.Docker.Probe(ctx); err != nil {
		return nil, fmt.Errorf("agent executor: docker engine capability probe failed: %w", err)
	}
	if err := probeBuildxFunc(ctx); err != nil {
		return nil, fmt.Errorf("agent executor: buildx capability probe failed: %w", err)
	}
	logger := cfg.Logger
	if logger == nil {
		logger = log.Default()
	}
	buildOutput := cfg.BuildOutput
	if buildOutput == nil {
		buildOutput = io.Discard
	}
	return &Executor{
		checkout:     newProductionCheckout(cfg.ControlPlane, cfg.CheckoutRunner),
		docker:       cfg.Docker,
		build:        docker.Build,
		controlPlane: cfg.ControlPlane,
		serverID:     cfg.ServerID,
		network:      cfg.Network,
		buildOutput:  buildOutput,
		logger:       logger,
	}, nil
}

// Execute drives one lease through checkout -> build -> run -> health gate,
// reporting progress at each phase, and returns promptly once ctx is
// cancelled. A successful run stops at health_checking; activating/healthy
// belongs to a later slice.
func (e *Executor) Execute(ctx context.Context, lease agentclient.Lease) agentclient.Outcome {
	// Fail closed before any container work at all -- checkout, build, and
	// run all happen after this point. Migration 000017 makes both
	// health_gate_path and health_gate_host NOT NULL with CHECK constraints
	// on environment_runtime_configs, and agent_job_attempt_execution
	// freezes them the same way, so a lease reaching here with either
	// field empty cannot reflect a real, validly configured environment: it
	// means the control plane sent a lease this attempt cannot trust (a
	// wire/decoding defect, a stale binary on either side of the claim
	// protocol, or a genuine control-plane bug), not an application or
	// source defect and not a transient condition retrying will clear.
	// That is exactly the shape queue.FailureClassCorruptState exists for
	// (see ErrAgentLeaseCorruptState's own doc comment: "a mismatch ...
	// means either this process or a previous one has a bug"), and unlike
	// FailureClassPermanentConfiguration (a real application/config defect)
	// or FailureClassTransient (retryable, per internal/queue/retry.go's
	// Retryable), corrupt_state is the one class that says "do not retry
	// this and do not blame the application" -- it is never retried
	// (Retryable() is true only for FailureClassTransient), matching that a
	// resend of the same corrupt lease would fail identically.
	if lease.HealthGatePath == "" || lease.HealthGateHost == "" {
		return e.outcome(lease, false, queue.FailureClassCorruptState)
	}

	progress := newProgressReporter(e.controlPlane)
	materializer := newSecretMaterializer(e.controlPlane, lease.AttemptID, lease.Fence)
	tracker := &phaseTracker{}

	if err := tracker.advance(ctx, progress, lease.AttemptID, lease.Fence, deployment.StatusFetching); err != nil {
		return e.outcome(lease, false, queue.FailureClassTransient)
	}

	checkoutResult, err := e.checkout.Checkout(ctx, lease.AttemptID, lease.Fence)
	if err != nil {
		return e.outcome(lease, false, classifyPhaseError(ctx, err))
	}
	var closeOnce sync.Once
	closeCheckout := func() {
		closeOnce.Do(func() {
			closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_ = checkoutResult.Close(closeCtx)
		})
	}
	defer closeCheckout()

	if err := tracker.advance(ctx, progress, lease.AttemptID, lease.Fence, deployment.StatusBuilding); err != nil {
		return e.outcome(lease, false, queue.FailureClassTransient)
	}

	buildResult, err := e.build(ctx, docker.BuildRequest{
		ContextDir: checkoutResult.ContextDir,
		Identity: docker.BuiltImageIdentity{
			OwnerID:          checkoutResult.Binding.OwnerID,
			ProjectID:        lease.ProjectID,
			SourceDigest:     sourceDigest(lease.CommitSHA),
			BuildOperationID: attemptOpaqueID(lease.AttemptID),
		},
		SecretBindings: docker.BuildScopedBindings(toScopedBindings(lease.SecretBindings)),
		Materializer:   materializer,
		Stdout:         e.buildOutput,
		Stderr:         e.buildOutput,
	})
	if err != nil {
		return e.outcome(lease, false, docker.ClassifyFailure(err))
	}
	// The checkout is no longer needed once the image is built; release its
	// quota allocation promptly rather than holding it through run+gate.
	closeCheckout()

	if err := tracker.advance(ctx, progress, lease.AttemptID, lease.Fence, deployment.StatusStarting); err != nil {
		return e.outcome(lease, false, queue.FailureClassTransient)
	}

	env, err := e.containerEnv(ctx, materializer, lease)
	if err != nil {
		return e.outcome(lease, false, classifyPhaseError(ctx, err))
	}

	identity := docker.ReleaseResourceIdentity{
		OwnerID:           checkoutResult.Binding.OwnerID,
		ProjectID:         lease.ProjectID,
		EnvironmentID:     lease.EnvironmentID,
		ServerID:          e.serverID,
		DeploymentID:      lease.DeploymentID,
		ReleaseID:         lease.ReleaseID,
		JobID:             strconv.FormatInt(lease.JobID, 10),
		AttemptID:         strconv.FormatInt(lease.AttemptID, 10),
		FenceID:           strconv.FormatInt(lease.Fence, 10),
		RuntimeInstanceID: attemptOpaqueID(lease.AttemptID),
		CommitSHA:         lease.CommitSHA,
		ImageDigest:       buildResult.ImageDigest,
		ConfigDigest:      buildResult.ConfigDigest,
	}

	inspect, err := e.docker.EnsureReleaseRuntimeContainer(ctx, docker.RunContainerRequest{
		Identity:      identity,
		CPUMillis:     lease.CPUMillis,
		MemoryBytes:   lease.MemoryBytes,
		RestartPolicy: lease.RestartPolicy,
		Env:           env,
	}, e.network)
	if err != nil {
		return e.outcome(lease, false, docker.ClassifyFailure(err))
	}

	if err := tracker.advance(ctx, progress, lease.AttemptID, lease.Fence, deployment.StatusHealthChecking); err != nil {
		return e.outcome(lease, false, queue.FailureClassTransient)
	}

	// The IP itself is no longer needed here: RunHealthGate resolves it
	// internally from containerID+networkID (see health.go's
	// containerNetworkIP) now that the Host header comes from the
	// configured lease value, not a synthesized ip:port. resolveNetworkAndIP
	// is still called for its validation (exactly one network, a resolved
	// IP present) -- a malformed inspect result must still fail closed here.
	networkID, _, err := resolveNetworkAndIP(inspect)
	if err != nil {
		return e.outcome(lease, false, docker.ClassifyFailure(err))
	}

	gateResult, err := e.docker.RunHealthGate(ctx, inspect.ID, networkID, lease.HealthPolicyRevision, docker.HealthGateTarget{
		Host:   lease.HealthGateHost,
		Path:   lease.HealthGatePath,
		Port:   lease.ContainerPort,
		Scheme: lease.ContainerScheme,
	})
	if err != nil {
		return e.outcome(lease, false, docker.ClassifyFailure(err))
	}
	if !gateResult.Success {
		return e.outcome(lease, false, docker.ClassifyHealthGateResult(gateResult))
	}

	return e.outcome(lease, true, queue.FailureClassTransient) // FailureClass ignored on success.
}

// containerEnv merges runtime-scoped materialized secrets with runtime-
// scoped literal bindings into the container's complete environment.
// internal/runtime/docker's RunContainerRequest.validate (via
// buildEnvEntries) rejects a duplicate name from any source.
func (e *Executor) containerEnv(ctx context.Context, m *secretMaterializer, lease agentclient.Lease) ([]docker.ContainerEnv, error) {
	secretEnv, err := m.runtimeSecretEnv(ctx, lease.SecretBindings)
	if err != nil {
		return nil, err
	}
	return append(secretEnv, literalRuntimeEnv(lease.LiteralBindings)...), nil
}

// sourceDigest derives BuiltImageIdentity.SourceDigest from the checkout's
// already-verified commit SHA (gitcheckout.Checkout cross-checks `git
// rev-parse HEAD` against the authorized binding before returning). Reusing
// that verified 40-hex commit SHA as the seed for a 64-hex digest (the only
// format docker.requireHexDigest accepts) avoids a second, TOCTOU-prone
// filesystem walk of the checked-out tree purely to produce a label value.
func sourceDigest(commitSHA string) string {
	sum := sha256.Sum256([]byte(commitSHA))
	return hex.EncodeToString(sum[:])
}

// attemptOpaqueID derives a stable, opaque identifier from an attempt's own
// durable primary key (agent_job_attempts.id), used both as
// RuntimeInstanceID and as BuiltImageIdentity.BuildOperationID.
//
// RuntimeInstanceID: Step 4's result block requires the release-scoped
// label selector's full identity to be reconstructible from durably stored
// values, not guessed. AttemptID and Fence are exactly that: persisted in
// agent_job_attempts/environment_slots before this executor ever runs, and
// Fence does not change across renewals within one attempt (Agent.
// claimAndRun always renews with the lease's original Fence), so this value
// is stable for the whole lifetime of one attempt. That is also as far as
// it needs to be stable: FindReleaseRuntimeContainer's selector requires
// AttemptID/FenceID to match exactly too, so it can only ever hit the
// in-process "I already created this, adopt it" retry Step 6a built for one
// attempt reconstructing its own prior work (e.g. after an agent process
// restart mid-lease) -- never a genuinely new attempt's, which always gets
// a new AttemptID. Cross-attempt container/image reuse is not attempted in
// this slice.
//
// BuildOperationID: this slice performs one fresh build per attempt with no
// cross-attempt build caching/sharing (a later optimization), so tying it
// 1:1 to the attempt is correct today.
//
// RuntimeInstanceID and BuildOperationID deliberately share this one
// derived value in this slice -- there is no reason yet for them to differ,
// since neither cross-attempt container reuse nor cross-attempt image
// sharing is attempted here. If a later slice adds build caching/sharing
// across attempts (so one image legitimately serves more than one
// RuntimeInstanceID), BuildOperationID must be derived independently at
// that point -- tying it to AttemptID would then make every attempt look
// like a distinct build operation even when the image was reused, which is
// exactly backwards for a shared-image lookup.
func attemptOpaqueID(attemptID int64) string {
	return strconv.FormatInt(attemptID, 10)
}

// resolveNetworkAndIP extracts the one network ID and IP address a release
// runtime container must have (run.go's own container-create payload sets
// exactly one NetworkMode, so InspectContainer's NetworkSettings.Networks
// map always has exactly one entry for a container this package created).
// More or less than one network, or an empty IP, means the engine returned
// something inconsistent with what this package itself created -- a
// malformed-response class error, matching containerNetworkIP's own
// reasoning in internal/runtime/docker/health.go (which this duplicates
// narrowly rather than calling: that method is unexported and this executor
// needs the IP for healthGateHost before RunHealthGate resolves it
// internally for the probe itself).
func resolveNetworkAndIP(inspect docker.ContainerInspect) (networkID, ip string, err error) {
	count := 0
	for _, n := range inspect.NetworkSettings.Networks {
		networkID, ip = n.NetworkID, n.IPAddress
		count++
	}
	if count != 1 || networkID == "" || ip == "" {
		return "", "", &docker.EngineError{
			Class: docker.ErrClassMalformed,
			Op:    "container-network-resolve",
			Err:   fmt.Errorf("container %s is attached to %d networks, want exactly 1 with a resolved IP", inspect.ID, count),
		}
	}
	return networkID, ip, nil
}

// classifyPhaseError classifies a failure from the checkout or
// materialization phases -- the phases internal/runtime/docker's own
// ClassifyFailure/ClassifyHealthGateResult do not cover, since those are
// scoped to that package's own errors (see classify.go's doc comment).
// Docker/run/build/health-gate failures always go through
// docker.ClassifyFailure or docker.ClassifyHealthGateResult directly,
// never through here.
//
// Every branch maps onto an existing queue.FailureClass; nothing new is
// introduced, mirroring classify.go's own constraint extended to the one
// set of phases it does not reach.
func classifyPhaseError(ctx context.Context, err error) queue.FailureClass {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		// Agent-side interruption (shutdown, lost renewal, stale fence) is
		// not an application or source defect -- see
		// docker.ClassifyHealthGateResult's identical reasoning for
		// context_cancelled.
		return queue.FailureClassTransient
	}
	switch {
	case errors.Is(err, agentclient.ErrServiceUnavailable), errors.Is(err, agentclient.ErrTransportFailure):
		return queue.FailureClassTransient
	case errors.Is(err, agentclient.ErrUnauthorized):
		return queue.FailureClassAuthorization
	case errors.Is(err, agentclient.ErrStaleLease), errors.Is(err, agentclient.ErrIntegrityConflict):
		return queue.FailureClassCorruptState
	case errors.Is(err, agentclient.ErrInvalidRequest):
		return queue.FailureClassInvalidInput
	case errors.Is(err, errMaterializeMismatch):
		return queue.FailureClassCorruptState
	case errors.Is(err, gitcheckout.ErrUnsafeSource):
		// The repository content itself is not deployable; retrying
		// unchanged reproduces the same rejection.
		return queue.FailureClassPermanentConfiguration
	case errors.Is(err, gitcheckout.ErrGitFailed), errors.Is(err, gitcheckout.ErrInvalidConfig), errors.Is(err, agentcheckout.ErrUnavailable):
		return queue.FailureClassPermanentConfiguration
	default:
		// Unrecognized failure shape: fail closed, matching
		// docker.ClassifyFailure's own default rather than guessing it is
		// safe to retry.
		return queue.FailureClassCorruptState
	}
}

// outcome builds one agentclient.Outcome, including the deterministic
// OperationID/ResultDigest ADR 0004's completion protocol needs for
// idempotent replay: internal/store/postgres's Complete/AcknowledgeCancel-
// lation accept a repeated call for an already-closed attempt only when
// both the operation id and the result digest match what was already
// recorded (agent_lease_repository.go). Deriving both deterministically
// from (attemptID, fence, succeeded, failureClass) means a second Execute
// for the same still-leased attempt/fence (the only way this can be called
// twice for one attempt -- see attemptOpaqueID) reports byte-identical
// values when it reaches the same real result, so a lost/retried Complete
// call replays cleanly. A second Execute that reaches a genuinely different
// real result (rare: it implies the same attempt/fence was re-dispatched
// and the underlying condition changed between runs) produces a different
// digest under the same operation id, which the control plane's own check
// then refuses as a conflict rather than silently overwriting a previously
// recorded terminal outcome -- fail closed, not fail silent.
//
// What this cannot reconstruct: if the control plane itself loses the
// completion record for reasons outside this attempt/fence (a genuinely
// new attempt), a new AttemptID makes this a fresh, unrelated operation id
// by construction -- as intended, since it is not a replay of the same
// work.
func (e *Executor) outcome(lease agentclient.Lease, succeeded bool, failureClass queue.FailureClass) agentclient.Outcome {
	operationID := fmt.Sprintf("op-%d-%d", lease.AttemptID, lease.Fence)
	seed := fmt.Sprintf("%d|%d|%t|%s", lease.AttemptID, lease.Fence, succeeded, failureClass)
	digest := sha256.Sum256([]byte(seed))
	out := agentclient.Outcome{Succeeded: succeeded, OperationID: operationID, ResultDigest: digest}
	if !succeeded {
		out.FailureClass = failureClass
	}
	return out
}
