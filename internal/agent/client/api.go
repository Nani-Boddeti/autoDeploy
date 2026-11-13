package agentclient

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	agentcheckout "autodeploy/internal/agent/checkout"
	"autodeploy/internal/deployment"
	"autodeploy/internal/github"
	"autodeploy/internal/githubapp"
	"autodeploy/internal/queue"
	"autodeploy/internal/runtime/dockerid"
	"autodeploy/internal/server"
)

// Enroll exchanges enrollmentToken (presented out of band by an
// administrator) plus the hex-encoded digest of a credential this process
// already generated and durably wrote for itself, for a durable credential
// version. It never sends the raw credential (internal/agent/http's enroll
// route never accepts one) and sends no Authorization header, matching that
// route's unauthenticated shape.
func (c *Client) Enroll(ctx context.Context, enrollmentToken server.EnrollmentToken, credentialDigest [32]byte) (int, error) {
	req := struct {
		ServerID         string `json:"server_id"`
		EnrollmentToken  string `json:"enrollment_token"`
		CredentialDigest string `json:"credential_digest"`
	}{ServerID: c.serverID, EnrollmentToken: enrollmentToken.String(), CredentialDigest: hex.EncodeToString(credentialDigest[:])}
	var resp struct {
		CredentialVersion int `json:"credential_version"`
	}
	if err := c.post(ctx, pathEnroll, req, &resp, false); err != nil {
		return 0, err
	}
	return resp.CredentialVersion, nil
}

// HeartbeatResult is the durable outcome of one heartbeat call.
type HeartbeatResult struct {
	ServerStatus             string
	HeartbeatIntervalSeconds int
}

// Heartbeat reports liveness and bounded capability information.
// executorAvailable must be false unless a real Executor is registered
// (Decision 10): this package never fabricates it.
func (c *Client) Heartbeat(ctx context.Context, capability Capability, executorAvailable bool) (HeartbeatResult, error) {
	req := struct {
		ProtocolVersion   int    `json:"protocol_version"`
		AgentVersion      string `json:"agent_version"`
		OS                string `json:"os"`
		Arch              string `json:"arch"`
		CPUCount          int    `json:"cpu_count"`
		MemoryBytes       int64  `json:"memory_bytes"`
		DiskFreeBytes     int64  `json:"disk_free_bytes"`
		ExecutorAvailable bool   `json:"executor_available"`
	}{
		ProtocolVersion: server.ProtocolVersion, AgentVersion: capability.AgentVersion,
		OS: capability.OS, Arch: capability.Arch, CPUCount: capability.CPUCount,
		MemoryBytes: capability.MemoryBytes, DiskFreeBytes: capability.DiskFreeBytes,
		ExecutorAvailable: executorAvailable,
	}
	var resp struct {
		ServerStatus             string `json:"server_status"`
		HeartbeatIntervalSeconds int    `json:"heartbeat_interval_seconds"`
	}
	if err := c.post(ctx, pathHeartbeat, req, &resp, true); err != nil {
		return HeartbeatResult{}, err
	}
	return HeartbeatResult{ServerStatus: resp.ServerStatus, HeartbeatIntervalSeconds: resp.HeartbeatIntervalSeconds}, nil
}

// SecretBinding is the agent-side typed view of one queue.SecretBinding: a
// version reference only, never a value (ADR 0003).
type SecretBinding struct {
	VariableName    string
	DeliveryScope   string
	SecretVersionID string
}

// Lease is the agent-side typed view of one claimed lease: the wire
// timestamps of internal/agent/http's leaseView, parsed, plus migration
// 000014's claim-time freeze -- the frozen execution configuration, release
// id, and secret-version bindings needed to run the container.
type Lease struct {
	JobID              int64
	AttemptID          int64
	AttemptNumber      int
	Fence              int64
	ProjectID          string
	EnvironmentID      string
	DeploymentID       string
	RepositoryID       int64
	Ref                string
	CommitSHA          string
	LeaseExpiresAt     time.Time
	AuthorityExpiresAt time.Time

	ContainerPort        int
	ContainerScheme      string
	ReleaseStrategy      string
	HealthPolicyRevision int
	CPUMillis            int
	MemoryBytes          int64
	RestartPolicy        string
	DrainSeconds         int
	HealthGatePath       string
	HealthGateHost       string
	ReleaseID            string
	SecretBindings       []SecretBinding
	LiteralBindings      []LiteralBinding
	// RollbackTargetReleaseID is non-empty only for a rollback lease (step
	// 20D): the retained release whose container is restarted, with the runtime
	// instance of that container. ReleaseID equals RollbackTargetReleaseID.
	RollbackTargetReleaseID         string
	RollbackTargetRuntimeInstanceID string
}

// LiteralBinding is one variable's frozen non-secret value. It carries the
// value itself, unlike SecretBinding, because a literal_nonsecret binding is
// non-secret by ADR 0003's classification. A secret value never arrives this
// way; it reaches the agent only through fenced materialization.
type LiteralBinding struct {
	VariableName  string
	DeliveryScope string
	Value         string
}

type secretBindingWire struct {
	VariableName    string `json:"variable_name"`
	DeliveryScope   string `json:"delivery_scope"`
	SecretVersionID string `json:"secret_version_id"`
}

type literalBindingWire struct {
	VariableName  string `json:"variable_name"`
	DeliveryScope string `json:"delivery_scope"`
	Value         string `json:"value"`
}

type leaseWire struct {
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
	SecretBindings       []secretBindingWire  `json:"secret_bindings"`
	LiteralBindings      []literalBindingWire `json:"literal_bindings"`

	RollbackTargetReleaseID         string `json:"rollback_target_release_id"`
	RollbackTargetRuntimeInstanceID string `json:"rollback_target_runtime_instance_id"`
}

func (w leaseWire) parse() (Lease, error) {
	leaseExpires, err := time.Parse(time.RFC3339Nano, w.LeaseExpiresAt)
	if err != nil {
		return Lease{}, err
	}
	authorityExpires, err := time.Parse(time.RFC3339Nano, w.AuthorityExpiresAt)
	if err != nil {
		return Lease{}, err
	}
	bindings := make([]SecretBinding, 0, len(w.SecretBindings))
	for _, b := range w.SecretBindings {
		bindings = append(bindings, SecretBinding{VariableName: b.VariableName, DeliveryScope: b.DeliveryScope, SecretVersionID: b.SecretVersionID})
	}
	literals := make([]LiteralBinding, 0, len(w.LiteralBindings))
	for _, b := range w.LiteralBindings {
		literals = append(literals, LiteralBinding{VariableName: b.VariableName, DeliveryScope: b.DeliveryScope, Value: b.Value})
	}
	return Lease{
		JobID: w.JobID, AttemptID: w.AttemptID, AttemptNumber: w.AttemptNumber, Fence: w.Fence,
		ProjectID: w.ProjectID, EnvironmentID: w.EnvironmentID, DeploymentID: w.DeploymentID,
		RepositoryID: w.RepositoryID, Ref: w.Ref, CommitSHA: w.CommitSHA,
		LeaseExpiresAt: leaseExpires, AuthorityExpiresAt: authorityExpires,

		ContainerPort:        w.ContainerPort,
		ContainerScheme:      w.ContainerScheme,
		ReleaseStrategy:      w.ReleaseStrategy,
		HealthPolicyRevision: w.HealthPolicyRevision,
		CPUMillis:            w.CPUMillis,
		MemoryBytes:          w.MemoryBytes,
		RestartPolicy:        w.RestartPolicy,
		DrainSeconds:         w.DrainSeconds,
		HealthGatePath:       w.HealthGatePath,
		HealthGateHost:       w.HealthGateHost,
		ReleaseID:            w.ReleaseID,
		SecretBindings:       bindings,
		LiteralBindings:      literals,

		RollbackTargetReleaseID:         w.RollbackTargetReleaseID,
		RollbackTargetRuntimeInstanceID: w.RollbackTargetRuntimeInstanceID,
	}, nil
}

// Claim requests at most one lease (internal/agent/http's V1 claimLimit).
// Callers must never set executorAvailable true unless a real Executor will
// actually run the returned lease.
func (c *Client) Claim(ctx context.Context, executorAvailable bool) ([]Lease, error) {
	req := struct {
		Limit             int  `json:"limit"`
		ExecutorAvailable bool `json:"executor_available"`
	}{Limit: 1, ExecutorAvailable: executorAvailable}
	var resp struct {
		Leases []leaseWire `json:"leases"`
	}
	// Claim has no server-side idempotency key: retrying after a lost
	// response could lease a second job. A single attempt leaves any
	// orphaned lease to expiry and reconciliation; the next tick claims again.
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	if err := c.attempt(ctx, pathClaim, payload, &resp, true); err != nil {
		return nil, err
	}
	leases := make([]Lease, 0, len(resp.Leases))
	for _, w := range resp.Leases {
		lease, err := w.parse()
		if err != nil {
			return nil, fmt.Errorf("%w: malformed lease in response", ErrTransportFailure)
		}
		leases = append(leases, lease)
	}
	return leases, nil
}

// RenewResult is the durable outcome of one renewal.
type RenewResult struct {
	LeaseExpiresAt     time.Time
	AuthorityExpiresAt time.Time
	CancelRequested    bool
}

// Renew extends (or, for an already-accepted sequence, replays) one
// attempt's lease/authority deadlines.
func (c *Client) Renew(ctx context.Context, attemptID, fence int64, sequence int) (RenewResult, error) {
	req := struct {
		AttemptID int64 `json:"attempt_id"`
		Fence     int64 `json:"fence"`
		Sequence  int   `json:"sequence"`
	}{AttemptID: attemptID, Fence: fence, Sequence: sequence}
	var resp struct {
		LeaseExpiresAt     string `json:"lease_expires_at"`
		AuthorityExpiresAt string `json:"authority_expires_at"`
		CancelRequested    bool   `json:"cancel_requested"`
	}
	if err := c.post(ctx, pathRenew, req, &resp, true); err != nil {
		return RenewResult{}, err
	}
	leaseExpires, err := time.Parse(time.RFC3339Nano, resp.LeaseExpiresAt)
	if err != nil {
		return RenewResult{}, fmt.Errorf("%w: malformed renew response", ErrTransportFailure)
	}
	authorityExpires, err := time.Parse(time.RFC3339Nano, resp.AuthorityExpiresAt)
	if err != nil {
		return RenewResult{}, fmt.Errorf("%w: malformed renew response", ErrTransportFailure)
	}
	return RenewResult{LeaseExpiresAt: leaseExpires, AuthorityExpiresAt: authorityExpires, CancelRequested: resp.CancelRequested}, nil
}

// Complete reports first-time success or failure for attemptID/fence.
// Replaying an identical call is idempotent server-side; a mismatched
// digest is reported back as ErrIntegrityConflict.
func (c *Client) Complete(ctx context.Context, attemptID, fence int64, outcome queue.CompletionOutcome, failureClass queue.FailureClass, operationID string, resultDigest [32]byte) error {
	req := struct {
		AttemptID    int64  `json:"attempt_id"`
		Fence        int64  `json:"fence"`
		Outcome      string `json:"outcome"`
		FailureClass string `json:"failure_class,omitempty"`
		OperationID  string `json:"operation_id"`
		ResultDigest string `json:"result_digest"`
	}{
		AttemptID: attemptID, Fence: fence, Outcome: string(outcome), FailureClass: string(failureClass),
		OperationID: operationID, ResultDigest: hex.EncodeToString(resultDigest[:]),
	}
	return c.post(ctx, pathComplete, req, nil, true)
}

// CancelAck acknowledges a cooperative cancellation for attemptID/fence.
func (c *Client) CancelAck(ctx context.Context, attemptID, fence int64, operationID string, resultDigest [32]byte) error {
	req := struct {
		AttemptID    int64  `json:"attempt_id"`
		Fence        int64  `json:"fence"`
		OperationID  string `json:"operation_id"`
		ResultDigest string `json:"result_digest"`
	}{AttemptID: attemptID, Fence: fence, OperationID: operationID, ResultDigest: hex.EncodeToString(resultDigest[:])}
	return c.post(ctx, pathCancelAck, req, nil, true)
}

// Reconcile clears reconciliation_required for one environment once this
// server reports the outcome of the operation that made it required. outcome
// is required and says what the block was ("activated", "not_activated" or
// "no_activation"); the control plane refuses a missing or unknown value.
func (c *Client) Reconcile(ctx context.Context, environmentID string, attemptID, fence int64, outcome string, outcomeDigest [32]byte) error {
	req := struct {
		EnvironmentID string `json:"environment_id"`
		AttemptID     int64  `json:"attempt_id"`
		Fence         int64  `json:"fence"`
		Outcome       string `json:"outcome"`
		OutcomeDigest string `json:"outcome_digest"`
	}{EnvironmentID: environmentID, AttemptID: attemptID, Fence: fence, Outcome: outcome, OutcomeDigest: hex.EncodeToString(outcomeDigest[:])}
	return c.post(ctx, pathReconcile, req, nil, true)
}

// ReportDrainResult reports what the predecessor drain for the activating
// attemptID ended in (step 20B; visibility only). Credential-authenticated, no
// lease or fence. A 200 and a terminal 409 (ConflictCode) both mean the server
// will never take another answer; 503/transport errors are retried by post and
// returned for the caller to back off and re-send.
func (c *Client) ReportDrainResult(ctx context.Context, attemptID int64, predecessorReleaseID, predecessorRuntimeInstanceID, outcome, summary string) error {
	req := struct {
		AttemptID                    int64  `json:"attempt_id"`
		PredecessorReleaseID         string `json:"predecessor_release_id"`
		PredecessorRuntimeInstanceID string `json:"predecessor_runtime_instance_id"`
		Outcome                      string `json:"outcome"`
		Summary                      string `json:"summary"`
	}{attemptID, predecessorReleaseID, predecessorRuntimeInstanceID, outcome, summary}
	return c.post(ctx, pathDrainResult, req, nil, true)
}

const (
	routeDigestPrefix              = "sha256:"
	maxActivationFailureSummaryLen = 2048
)

// ActivateResponse is the control plane's tx1 answer: the route the agent
// must render and compare against RouteDigest before switching.
type ActivateResponse struct {
	OperationID              string
	RouteRevision            int64
	Hostname                 string
	EntrypointName           string
	CertResolverName         string
	BackendRuntimeInstanceID string
	RouteDigest              [32]byte
	// PredecessorReleaseID and PredecessorRuntimeInstanceID identify the
	// release (and its container's runtime instance) the route served before
	// this switch. Both empty means the server affirmatively reported no
	// predecessor (first activation; the keys were present but empty), otherwise
	// both are set. A response that omits the keys is malformed.
	PredecessorReleaseID         string
	PredecessorRuntimeInstanceID string
}

// Activate asks the control plane to authorize the traffic switch for
// attemptID/fence and image imageDigest. Replaying the same
// (attempt, fence, image digest) is the server's idempotency key. A 200 whose
// fields are missing or malformed yields ErrMalformedResponse.
//
// A 409 (ConflictError / ErrStaleLease) is a definite refusal. Any other
// error -- ErrTransportFailure or ErrServiceUnavailable after retries, a 200
// that fails decoding, or ErrMalformedResponse -- leaves the server-side
// operation state UNKNOWN: tx1 may have committed and the response been lost,
// so callers must not treat it as "certainly not authorized". Retrying
// Activate is safe because the server replays on (attempt, fence,
// image_digest).
func (c *Client) Activate(ctx context.Context, attemptID, fence int64, imageDigest [32]byte) (ActivateResponse, error) {
	req := struct {
		AttemptID   int64  `json:"attempt_id"`
		Fence       int64  `json:"fence"`
		ImageDigest string `json:"image_digest"`
	}{AttemptID: attemptID, Fence: fence, ImageDigest: hex.EncodeToString(imageDigest[:])}
	var resp struct {
		OperationID                  string  `json:"operation_id"`
		RouteRevision                int64   `json:"route_revision"`
		Hostname                     string  `json:"hostname"`
		EntrypointName               string  `json:"entrypoint_name"`
		CertResolverName             string  `json:"cert_resolver_name"`
		BackendRuntimeInstanceID     string  `json:"backend_runtime_instance_id"`
		RouteDigest                  string  `json:"route_digest"`
		PredecessorReleaseID         *string `json:"predecessor_release_id"`
		PredecessorRuntimeInstanceID *string `json:"predecessor_runtime_instance_id"`
	}
	if err := c.post(ctx, pathActivate, req, &resp, true); err != nil {
		return ActivateResponse{}, err
	}
	if resp.OperationID == "" || resp.RouteRevision <= 0 || resp.Hostname == "" || resp.EntrypointName == "" ||
		resp.CertResolverName == "" || resp.BackendRuntimeInstanceID == "" {
		return ActivateResponse{}, fmt.Errorf("%w: activate response missing a required field", ErrMalformedResponse)
	}
	digest, ok := parseRouteDigest(resp.RouteDigest)
	if !ok {
		return ActivateResponse{}, fmt.Errorf("%w: activate response route digest", ErrMalformedResponse)
	}
	if resp.PredecessorReleaseID == nil || resp.PredecessorRuntimeInstanceID == nil {
		return ActivateResponse{}, fmt.Errorf("%w: activate response missing predecessor fields", ErrMalformedResponse)
	}
	if !validPredecessor(*resp.PredecessorReleaseID, *resp.PredecessorRuntimeInstanceID, resp.BackendRuntimeInstanceID) {
		return ActivateResponse{}, fmt.Errorf("%w: activate response predecessor identity", ErrMalformedResponse)
	}
	return ActivateResponse{
		PredecessorReleaseID: *resp.PredecessorReleaseID, PredecessorRuntimeInstanceID: *resp.PredecessorRuntimeInstanceID,
		OperationID: resp.OperationID, RouteRevision: resp.RouteRevision, Hostname: resp.Hostname,
		EntrypointName: resp.EntrypointName, CertResolverName: resp.CertResolverName,
		BackendRuntimeInstanceID: resp.BackendRuntimeInstanceID, RouteDigest: digest,
	}, nil
}

// validPredecessor accepts both identities empty (first activation), or both
// set with a valid opaque release id and a runtime instance id that forms a
// valid container name and differs from the new backend's.
func validPredecessor(releaseID, runtimeInstanceID, backendRuntimeInstanceID string) bool {
	if releaseID == "" && runtimeInstanceID == "" {
		return true
	}
	if releaseID == "" || runtimeInstanceID == "" || !github.ValidOpaqueID(releaseID) || runtimeInstanceID == backendRuntimeInstanceID {
		return false
	}
	_, err := dockerid.ContainerName(runtimeInstanceID)
	return err == nil
}

// parseRouteDigest accepts exactly "sha256:" + 64 lowercase hex.
func parseRouteDigest(s string) ([32]byte, bool) {
	var d [32]byte
	hexPart, found := strings.CutPrefix(s, routeDigestPrefix)
	if !found || len(hexPart) != hex.EncodedLen(len(d)) || hexPart != strings.ToLower(hexPart) {
		return d, false
	}
	b, err := hex.DecodeString(hexPart)
	if err != nil || len(b) != len(d) {
		return d, false
	}
	copy(d[:], b)
	return d, true
}

// ActivationResult is the agent's declared outcome of a traffic switch. It
// can only be built through ActivationSucceeded or ActivationFailed, so a
// success without a digest, a failure without a summary, or both together
// cannot be represented; the zero value is rejected by ReportActivationResult.
type ActivationResult struct {
	valid          bool
	succeeded      bool
	observedDigest [32]byte
	failureSummary string
}

// ActivationSucceeded declares the switch observed with routeDigest.
func ActivationSucceeded(observedRouteDigest [32]byte) ActivationResult {
	return ActivationResult{valid: true, succeeded: true, observedDigest: observedRouteDigest}
}

// ActivationFailed declares failure or ambiguity with a non-empty summary of
// at most 2048 bytes; otherwise it returns ErrInvalidRequest.
func ActivationFailed(summary string) (ActivationResult, error) {
	if summary == "" || len(summary) > maxActivationFailureSummaryLen {
		return ActivationResult{}, fmt.Errorf("%w: activation failure summary must be 1-%d bytes", ErrInvalidRequest, maxActivationFailureSummaryLen)
	}
	return ActivationResult{valid: true, failureSummary: summary}, nil
}

// ReportActivationResult reports tx2 for operationID. It rejects a zero
// ActivationResult or invalid ids with ErrInvalidRequest before sending.
// The control plane answers 409 late_report_recorded when the report arrived
// after the operation was blocked (recorded as evidence, never finalized).
func (c *Client) ReportActivationResult(ctx context.Context, attemptID, fence int64, operationID string, result ActivationResult) error {
	if !result.valid || attemptID <= 0 || fence <= 0 || operationID == "" || len(operationID) > 128 {
		return fmt.Errorf("%w: activation result", ErrInvalidRequest)
	}
	req := struct {
		AttemptID           int64  `json:"attempt_id"`
		Fence               int64  `json:"fence"`
		OperationID         string `json:"operation_id"`
		Succeeded           bool   `json:"succeeded"`
		ObservedRouteDigest string `json:"observed_route_digest,omitempty"`
		FailureSummary      string `json:"failure_summary,omitempty"`
	}{AttemptID: attemptID, Fence: fence, OperationID: operationID, Succeeded: result.succeeded, FailureSummary: result.failureSummary}
	if result.succeeded {
		req.ObservedRouteDigest = routeDigestPrefix + hex.EncodeToString(result.observedDigest[:])
	}
	return c.post(ctx, pathActivationRes, req, nil, true)
}

// MaterializedSecret is one decrypted value for this attempt. It exists only
// in the agent's memory for as long as the executor needs it: ADR 0003
// forbids writing it to a durable job, a persistent temporary file, or a log
// line, and the agent installs its redactor before invoking any build or
// runtime tool so the value cannot escape through tool output either.
type MaterializedSecret struct {
	VariableName  string
	DeliveryScope string
	Value         string
}

// MaterializeSecrets fetches the decrypted values for the frozen secret set
// of attemptID/fence. The request names no variable: the control plane
// decides what this attempt froze, so the agent cannot widen its own
// request. A stale fence, closed attempt, expired authority, cancelled job,
// or revoked version is refused with the same generic error every other
// agent route returns.
//
// This call is never retried on anything but a network/503 failure (the
// post helper's own rule), because a refusal here is an authorization
// decision rather than a transient condition.
func (c *Client) MaterializeSecrets(ctx context.Context, attemptID, fence int64) ([]MaterializedSecret, error) {
	req := struct {
		AttemptID int64 `json:"attempt_id"`
		Fence     int64 `json:"fence"`
	}{AttemptID: attemptID, Fence: fence}
	var resp struct {
		Secrets []struct {
			VariableName  string `json:"variable_name"`
			DeliveryScope string `json:"delivery_scope"`
			Value         string `json:"value"`
		} `json:"secrets"`
	}
	if err := c.post(ctx, pathMaterialize, req, &resp, true); err != nil {
		return nil, err
	}
	secrets := make([]MaterializedSecret, 0, len(resp.Secrets))
	for _, s := range resp.Secrets {
		secrets = append(secrets, MaterializedSecret{VariableName: s.VariableName, DeliveryScope: s.DeliveryScope, Value: s.Value})
	}
	return secrets, nil
}

// ReportProgress tells the control plane that this attempt has entered phase,
// so an operator can watch a deployment move through checkout, build, start,
// and the health gate. Phase must be one of deployment.StatusFetching,
// StatusBuilding, StatusStarting, or StatusHealthChecking; activation and the
// healthy verdict belong to the control plane.
//
// Progress is observability, so a caller should not abort execution because a
// report failed: a duplicate or already-passed phase is accepted by the
// control plane, and a genuine authorization failure will surface again on the
// next renewal.
func (c *Client) ReportProgress(ctx context.Context, attemptID, fence int64, phase deployment.Status) error {
	req := struct {
		AttemptID int64  `json:"attempt_id"`
		Fence     int64  `json:"fence"`
		Phase     string `json:"phase"`
	}{AttemptID: attemptID, Fence: fence, Phase: string(phase)}
	return c.post(ctx, pathProgress, req, nil, true)
}

type checkoutBindingWire struct {
	OwnerID        string `json:"owner_id"`
	ProjectID      string `json:"project_id"`
	InstallationID int64  `json:"installation_id"`
	RepositoryID   int64  `json:"repository_id"`
	CloneURL       string `json:"clone_url"`
	CommitSHA      string `json:"commit_sha"`
	ReservationID  string `json:"reservation_id"`
}

// CheckoutBegin reserves a checkout credential for attemptID/fence. No token
// is minted or returned here (internal/agent/checkout.Begin).
func (c *Client) CheckoutBegin(ctx context.Context, attemptID, fence int64) (agentcheckout.Reservation, error) {
	req := struct {
		AttemptID int64 `json:"attempt_id"`
		Fence     int64 `json:"fence"`
	}{AttemptID: attemptID, Fence: fence}
	var resp struct {
		ReservationID string              `json:"reservation_id"`
		Binding       checkoutBindingWire `json:"binding"`
	}
	if err := c.post(ctx, pathCheckoutBegin, req, &resp, true); err != nil {
		return agentcheckout.Reservation{}, err
	}
	return agentcheckout.Reservation{
		ReservationID: resp.ReservationID,
		Binding: githubapp.CheckoutBinding{
			OwnerID:        resp.Binding.OwnerID,
			ProjectID:      resp.Binding.ProjectID,
			InstallationID: resp.Binding.InstallationID,
			RepositoryID:   resp.Binding.RepositoryID,
			CloneURL:       resp.Binding.CloneURL,
			CommitSHA:      resp.Binding.CommitSHA,
			ReservationID:  resp.Binding.ReservationID,
		},
	}, nil
}

// CheckoutStart activates reservationID and returns the minted credential.
// The control plane marks the durable lease running before this response is
// ever written, so a caller that receives a token here also knows the
// running state was recorded first.
func (c *Client) CheckoutStart(ctx context.Context, attemptID, fence int64, reservationID string) (agentcheckout.Credential, error) {
	req := struct {
		AttemptID     int64  `json:"attempt_id"`
		Fence         int64  `json:"fence"`
		ReservationID string `json:"reservation_id"`
	}{AttemptID: attemptID, Fence: fence, ReservationID: reservationID}
	var resp struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := c.post(ctx, pathCheckoutStart, req, &resp, true); err != nil {
		return agentcheckout.Credential{}, err
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, resp.ExpiresAt)
	if err != nil {
		return agentcheckout.Credential{}, fmt.Errorf("%w: malformed checkout start response", ErrTransportFailure)
	}
	return agentcheckout.Credential{Token: resp.Token, ExpiresAt: expiresAt}, nil
}

// CheckoutClose releases reservationID and revokes its credential if one was
// ever minted. It is safe to call on a reservation that was begun but never
// started, and safe to retry.
func (c *Client) CheckoutClose(ctx context.Context, attemptID, fence int64, reservationID string) error {
	req := struct {
		AttemptID     int64  `json:"attempt_id"`
		Fence         int64  `json:"fence"`
		ReservationID string `json:"reservation_id"`
	}{AttemptID: attemptID, Fence: fence, ReservationID: reservationID}
	return c.post(ctx, pathCheckoutClose, req, nil, true)
}
