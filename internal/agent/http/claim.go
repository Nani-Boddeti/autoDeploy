package agenthttp

import (
	"context"
	"net/http"
	"time"

	"autodeploy/internal/healthgate"
	"autodeploy/internal/queue"
	"autodeploy/internal/store/postgres"
)

// claimLimit is the only value V1 accepts (tasks/todo.md Step 5: "limit
// (1..1; V1 max 1)"); anything else is rejected as invalid input before any
// repository call.
const claimLimit = 1

// maxClaimCapabilities bounds the optional capabilities list of a claim.
const maxClaimCapabilities = 4

// claimRequest is the wire shape of POST /agent/v1/leases/claim. Capabilities
// is the agent's opt-in list (step 22-1c; ClaimCleanups' kinds precedent):
// absent, null or empty is the pre-22-1c request and gets the pre-22-1c
// response byte for byte; "health_policy" adds the lease's frozen
// health_policy object and allows non-default-policy jobs to be leased;
// "activation_not_switched" (slice 22-D) adds "activation_not_switched":true to
// each stop-then-start deployment lease.
type claimRequest struct {
	Limit             int      `json:"limit"`
	ExecutorAvailable bool     `json:"executor_available"`
	Capabilities      []string `json:"capabilities,omitempty"`
}

func (req claimRequest) valid() bool { return req.Limit == claimLimit }

// parseCapabilities reports the declared capabilities; an unknown name or an
// oversized list is an invalid request (the decoder is strict everywhere).
func (req claimRequest) parseCapabilities() (queue.ClaimCapabilities, bool) {
	var caps queue.ClaimCapabilities
	if len(req.Capabilities) > maxClaimCapabilities {
		return caps, false
	}
	for _, c := range req.Capabilities {
		switch c {
		case healthgate.CapabilityHealthPolicy:
			caps.HealthPolicy = true
		case queue.CapabilityActivationNotSwitched:
			caps.ActivationNotSwitched = true
		default:
			return caps, false
		}
	}
	return caps, true
}

// CapabilityClaimer is the optional extension of LeaseClaimer that serves a claim
// whose agent declared capabilities. (*postgres.AgentLeaseRepository)
// implements it; a LeaseClaimer without it is served by the plain Claim, which is
// the non-opted-in path and therefore never yields a non-default policy.
type CapabilityClaimer interface {
	ClaimWithCapabilities(ctx context.Context, serverID string, presentedDigest [32]byte, limit int, caps queue.ClaimCapabilities) ([]queue.ClaimedLease, error)
}

// Production wiring passes the repository, so both interfaces must hold: a
// missing method would silently drop the health_policy opt-in.
var (
	_ LeaseClaimer      = (*postgres.AgentLeaseRepository)(nil)
	_ CapabilityClaimer = (*postgres.AgentLeaseRepository)(nil)
)

// healthPolicyView is the wire shape of a frozen healthgate.Policy. tcp_port
// is an explicit null when no TCP check is configured.
type healthPolicyView struct {
	SuccessThreshold int  `json:"success_threshold"`
	IntervalSeconds  int  `json:"interval_seconds"`
	TimeoutSeconds   int  `json:"timeout_seconds"`
	DeadlineSeconds  int  `json:"deadline_seconds"`
	TCPPort          *int `json:"tcp_port"`
}

func newHealthPolicyView(p healthgate.Policy) *healthPolicyView {
	p = p.WithDefaults()
	v := &healthPolicyView{SuccessThreshold: p.SuccessThreshold, IntervalSeconds: p.IntervalSeconds, TimeoutSeconds: p.TimeoutSeconds, DeadlineSeconds: p.DeadlineSeconds}
	if p.TCPPort != 0 {
		port := p.TCPPort
		v.TCPPort = &port
	}
	return v
}

type claimResponse struct {
	Leases []leaseView `json:"leases"`
}

// secretBindingView is the wire shape of one queue.SecretBinding: a
// version reference only. ADR 0003 forbids a secret value from ever
// appearing here.
type secretBindingView struct {
	VariableName    string `json:"variable_name"`
	DeliveryScope   string `json:"delivery_scope"`
	SecretVersionID string `json:"secret_version_id"`
}

// literalBindingView is the wire shape of one queue.LiteralBinding. This
// one does carry a value, which is safe only because a literal_nonsecret
// binding is non-secret by ADR 0003's classification; a secret value reaches
// the agent solely through fenced materialization, never here.
type literalBindingView struct {
	VariableName  string `json:"variable_name"`
	DeliveryScope string `json:"delivery_scope"`
	Value         string `json:"value"`
}

// leaseView is the wire shape of one queue.ClaimedLease: job, attempt,
// fence, environment, deployment, repository, ref, commit, both deadlines
// (RFC3339Nano UTC, tasks/todo.md Step 5), and -- migration 000014's
// claim-time freeze, Slice 9a Step 2 -- the frozen execution configuration,
// release id, and secret-version bindings an agent needs to run the
// container. Never a secret value.
type leaseView struct {
	JobID              int64  `json:"job_id"`
	AttemptID          int64  `json:"attempt_id"`
	AttemptNumber      int    `json:"attempt_number"`
	Fence              int64  `json:"fence"`
	ProjectID          string `json:"project_id"`
	EnvironmentID      string `json:"environment_id"`
	DeploymentID       string `json:"deployment_id"`
	RepositoryID       int64  `json:"repository_id"`
	Ref                string `json:"ref"`
	CommitSHA          string `json:"commit_sha"`
	LeaseExpiresAt     string `json:"lease_expires_at"`
	AuthorityExpiresAt string `json:"authority_expires_at"`

	ContainerPort        int                  `json:"container_port"`
	ContainerScheme      string               `json:"container_scheme"`
	ReleaseStrategy      string               `json:"release_strategy"`
	HealthPolicyRevision int                  `json:"health_policy_revision"`
	CPUMillis            int                  `json:"cpu_millis"`
	MemoryBytes          int64                `json:"memory_bytes"`
	RestartPolicy        string               `json:"restart_policy"`
	DrainSeconds         int                  `json:"drain_seconds"`
	HealthGatePath       string               `json:"health_gate_path"`
	HealthGateHost       string               `json:"health_gate_host"`
	ReleaseID            string               `json:"release_id"`
	SecretBindings       []secretBindingView  `json:"secret_bindings"`
	LiteralBindings      []literalBindingView `json:"literal_bindings"`
	// Set only for a claim that declared the health_policy capability
	// (step 22-1c); omitted otherwise so every other response is unchanged.
	HealthPolicy *healthPolicyView `json:"health_policy,omitempty"`
	// Set only for a claim that declared the activation_not_switched
	// capability (slice 22-D), and only on a stop-then-start deployment lease
	// (not a rollback): the control plane will consider a not_switched
	// activation-result outcome for it. Omitted otherwise so every other
	// response is unchanged.
	ActivationNotSwitched bool `json:"activation_not_switched,omitempty"`
	// Set only for a rollback lease (step 20D); omitted otherwise.
	RollbackTargetReleaseID         string `json:"rollback_target_release_id,omitempty"`
	RollbackTargetRuntimeInstanceID string `json:"rollback_target_runtime_instance_id,omitempty"`
}

func formatDeadline(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func newLeaseView(l queue.ClaimedLease) leaseView {
	bindings := make([]secretBindingView, 0, len(l.SecretBindings))
	for _, b := range l.SecretBindings {
		bindings = append(bindings, secretBindingView{VariableName: b.VariableName, DeliveryScope: b.DeliveryScope, SecretVersionID: b.SecretVersionID})
	}
	literals := make([]literalBindingView, 0, len(l.LiteralBindings))
	for _, b := range l.LiteralBindings {
		literals = append(literals, literalBindingView{VariableName: b.VariableName, DeliveryScope: b.DeliveryScope, Value: b.Value})
	}
	return leaseView{
		JobID:              l.JobID,
		AttemptID:          l.AttemptID,
		AttemptNumber:      l.AttemptNumber,
		Fence:              l.Fence,
		ProjectID:          l.ProjectID,
		EnvironmentID:      l.EnvironmentID,
		DeploymentID:       l.DeploymentID,
		RepositoryID:       l.RepositoryID,
		Ref:                l.Ref,
		CommitSHA:          l.CommitSHA,
		LeaseExpiresAt:     formatDeadline(l.LeaseExpiresAt),
		AuthorityExpiresAt: formatDeadline(l.AuthorityExpiresAt),

		ContainerPort:        l.ContainerPort,
		ContainerScheme:      l.ContainerScheme,
		ReleaseStrategy:      l.ReleaseStrategy,
		HealthPolicyRevision: l.HealthPolicyRevision,
		CPUMillis:            l.CPUMillis,
		MemoryBytes:          l.MemoryBytes,
		RestartPolicy:        l.RestartPolicy,
		DrainSeconds:         l.DrainSeconds,
		HealthGatePath:       l.HealthGatePath,
		HealthGateHost:       l.HealthGateHost,
		ReleaseID:            l.ReleaseID,
		SecretBindings:       bindings,
		LiteralBindings:      literals,

		RollbackTargetReleaseID:         l.RollbackTargetReleaseID,
		RollbackTargetRuntimeInstanceID: l.RollbackTargetRuntimeInstanceID,
	}
}

// handleClaim requires ExecutorAvailable to be true in the request before
// it will ever call Claimer.Claim (Decision 10: "cmd/agent in this slice
// never reports a fabricated success ... claim returns nothing to such
// agents"). Authentication itself still always runs first, via
// Authenticator, so an unauthenticated caller cannot use
// executor_available=false as a way to probe this route without a valid
// credential.
func (h Handler) handleClaim(w http.ResponseWriter, r *http.Request) {
	serverID, digest, ok := h.identity(r)
	if !ok {
		h.writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if h.Authenticator == nil || h.Claimer == nil {
		h.writeError(w, http.StatusServiceUnavailable, "service_unavailable")
		return
	}
	var req claimRequest
	if !h.readJSON(w, r, MaxBodyBytes, &req) {
		return
	}
	if !req.valid() {
		h.writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	caps, capsOK := req.parseCapabilities()
	if !capsOK {
		h.writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if err := h.Authenticator.Authenticate(r.Context(), serverID, digest); err != nil {
		h.mapError(w, err)
		return
	}
	if !req.ExecutorAvailable {
		h.writeJSON(w, http.StatusOK, claimResponse{Leases: []leaseView{}})
		return
	}
	var (
		leases []queue.ClaimedLease
		err    error
	)
	if cc, ok := h.Claimer.(CapabilityClaimer); ok && caps.HealthPolicy {
		leases, err = cc.ClaimWithCapabilities(r.Context(), serverID, digest, req.Limit, caps)
	} else {
		leases, err = h.Claimer.Claim(r.Context(), serverID, digest, req.Limit)
	}
	if err != nil {
		h.mapError(w, err)
		return
	}
	views := make([]leaseView, 0, len(leases))
	for _, l := range leases {
		v := newLeaseView(l)
		if caps.HealthPolicy {
			v.HealthPolicy = newHealthPolicyView(l.HealthPolicy)
		}
		if caps.ActivationNotSwitched && l.ReleaseStrategy == "stop_then_start" && l.RollbackTargetReleaseID == "" {
			v.ActivationNotSwitched = true
		}
		views = append(views, v)
	}
	h.writeJSON(w, http.StatusOK, claimResponse{Leases: views})
}
