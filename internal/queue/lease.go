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

// ClaimedLease is the durable result of one successful claim: everything an
// agent needs to execute one attempt, and everything a later renew/complete/
// cancel call must present back for fencing (ADR 0004 atomic claim
// boundary). It carries no validation of its own -- every field was already
// checked by the schema/trigger layer that produced the underlying rows.
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
}

// RenewalResult is the durable outcome of one successful renewal, including
// an idempotent replay of the already-accepted sequence (ADR 0004 renewal).
type RenewalResult struct {
	LeaseExpiresAt     time.Time
	AuthorityExpiresAt time.Time
	CancelRequested    bool
}
