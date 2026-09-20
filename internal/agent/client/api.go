package agentclient

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

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
