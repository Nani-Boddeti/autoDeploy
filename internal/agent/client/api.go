package agentclient

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	agentcheckout "autodeploy/internal/agent/checkout"
	"autodeploy/internal/deployment"
	"autodeploy/internal/githubapp"
	"autodeploy/internal/queue"
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
// server reports the outcome of the operation that made it required.
func (c *Client) Reconcile(ctx context.Context, environmentID string, attemptID, fence int64, outcomeDigest [32]byte) error {
	req := struct {
		EnvironmentID string `json:"environment_id"`
		AttemptID     int64  `json:"attempt_id"`
		Fence         int64  `json:"fence"`
		OutcomeDigest string `json:"outcome_digest"`
	}{EnvironmentID: environmentID, AttemptID: attemptID, Fence: fence, OutcomeDigest: hex.EncodeToString(outcomeDigest[:])}
	return c.post(ctx, pathReconcile, req, nil, true)
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
