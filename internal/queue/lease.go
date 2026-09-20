package queue

import "time"

// AttemptState mirrors agent_job_attempts.state (migration 000013). Only
// LeaseSucceeded, LeaseFailed, LeaseExpired, and LeaseCancelled are terminal;
// AttemptLeased is the sole non-terminal state.
type AttemptState string

const (
	AttemptLeased    AttemptState = "leased"
	AttemptSucceeded AttemptState = "succeeded"
	AttemptFailed    AttemptState = "failed"
	AttemptExpired   AttemptState = "expired"
	AttemptCancelled AttemptState = "cancelled"
)

// SecretBinding is one variable's frozen secret-version reference, carried
// on a ClaimedLease and delivered to the agent over the wire -- a version
// identifier only. ADR 0003 forbids a secret value from ever appearing on a
// ClaimedLease, in a log line, or in any audit row; materialization (a later
// slice) is the one point where a version reference becomes plaintext, after
// commit, never inside this transaction.
type SecretBinding struct {
	VariableName    string
	DeliveryScope   string
	SecretVersionID string
}

// LiteralBinding is one variable's frozen non-secret value, carried on a
// ClaimedLease and delivered to the agent over the wire. Unlike
// SecretBinding this carries the value itself, which is safe only because a
// literal_nonsecret binding is non-secret by ADR 0003's classification
// ("Non-secret protected metadata"): an ordinary configuration value such as
// NODE_ENV=production, with no confidentiality requirement. A secret value
// must never be routed through this type -- it belongs to the encrypted
// secret_versions path and reaches the agent only through fenced
// materialization.
type LiteralBinding struct {
	VariableName  string
	DeliveryScope string
	Value         string
}

// ClaimedLease is the durable result of one successful claim: everything an
// agent needs to execute one attempt, and everything a later renew/complete/
// cancel call must present back for fencing (ADR 0004 atomic claim
// boundary). It carries no validation of its own -- every field was already
// checked by the schema/trigger layer that produced the underlying rows.
// ContainerPort/ContainerScheme/ReleaseStrategy/HealthPolicyRevision/
// CPUMillis/MemoryBytes/RestartPolicy/DrainSeconds/ReleaseID/SecretBindings/
// LiteralBindings
// are the migration 000014 claim-time freeze (tasks/todo.md Slice 9a Step 2):
// on attempt 1 they are resolved from environment_runtime_configs/
// environment_runtime_bindings; on a retry (attempt N>1 of the same job)
// they are copied forward verbatim from attempt N-1, never re-resolved
// against current state (ADR 0003).
type ClaimedLease struct {
	JobID              int64
	AttemptID          int64
	AttemptNumber      int
	Fence              int64
	OwnerID            string
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

// RenewalResult is the durable outcome of one successful renewal, including
// an idempotent replay of the already-accepted sequence (ADR 0004 renewal).
type RenewalResult struct {
	LeaseExpiresAt     time.Time
	AuthorityExpiresAt time.Time
	CancelRequested    bool
}
