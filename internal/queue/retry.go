package queue

import (
	"math/rand/v2"
	"time"
)

// MaxAttempts is the ADR 0004 V1 maximum attempt count, expiry included.
const MaxAttempts = 5

const (
	// retryBaseDelay and retryCapDelay are the ADR 0004 V1 retry defaults:
	// uniform(0, min(5m, 5s*2^(attempt_number-1))).
	retryBaseDelay = 5 * time.Second
	retryCapDelay  = 5 * time.Minute
)

// FullJitterDelay returns one full-jitter retry delay for a transient
// failure of attemptNumber (the 1-based attempt that just ended), per ADR
// 0004: uniform(0, min(5 minutes, 5 seconds * 2^(attempt_number-1))). The
// caller selects this exactly once per retry and persists the resulting
// available_at; retries never recompute it.
func FullJitterDelay(attemptNumber int) time.Duration {
	if attemptNumber < 1 {
		attemptNumber = 1
	}
	shift := attemptNumber - 1
	// 5s*2^20 already vastly exceeds the 5-minute cap, so clamping shift
	// here only guards against pathological input; it never changes the
	// selected cap for any attemptNumber this package actually produces
	// (1..MaxAttempts).
	if shift > 20 {
		shift = 20
	}
	backoff := retryBaseDelay * time.Duration(uint64(1)<<uint(shift))
	if backoff > retryCapDelay || backoff <= 0 {
		backoff = retryCapDelay
	}
	return time.Duration(rand.Float64() * float64(backoff))
}

// FailureClass mirrors agent_job_attempts.failure_class (migration 000013):
// a closed enum matching that column's CHECK constraint exactly. Only
// FailureClassTransient is retryable; every other value -- including any
// value outside this enum, should one ever reach Retryable -- is
// non-retryable, per ADR 0004's retry classifier.
type FailureClass string

const (
	FailureClassTransient              FailureClass = "transient"
	FailureClassAuthorization          FailureClass = "authorization"
	FailureClassInvalidInput           FailureClass = "invalid_input"
	FailureClassCancelled              FailureClass = "cancelled"
	FailureClassCorruptState           FailureClass = "corrupt_state"
	FailureClassPermanentConfiguration FailureClass = "permanent_configuration"
)

// Valid reports whether c is one of the closed enum values the schema
// accepts. Complete rejects any other value before it ever reaches SQL.
func (c FailureClass) Valid() bool {
	switch c {
	case FailureClassTransient, FailureClassAuthorization, FailureClassInvalidInput,
		FailureClassCancelled, FailureClassCorruptState, FailureClassPermanentConfiguration:
		return true
	default:
		return false
	}
}

// Retryable reports whether a failure of this class may be retried.
// Anything other than the transient class -- known non-retryable or
// otherwise unrecognized -- is treated as non-retryable.
func (c FailureClass) Retryable() bool {
	return c == FailureClassTransient
}

// CompletionOutcome is the result Complete reports for a first-time
// completion: exactly succeeded or failed. Cancellation acknowledgement is a
// distinct operation (AcknowledgeCancellation), not a CompletionOutcome.
type CompletionOutcome string

const (
	CompletionSucceeded CompletionOutcome = "succeeded"
	CompletionFailed    CompletionOutcome = "failed"
)
