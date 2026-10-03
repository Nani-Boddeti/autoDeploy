package agentclient

import (
	"errors"
	"testing"
)

// TestMapStatusActivationRefusalsAreNonRetryable pins the class of every
// 409 code activation introduces: anything but stale_lease must be the
// non-retryable ErrIntegrityConflict, never a retryable service error.
func TestMapStatusActivationRefusalsAreNonRetryable(t *testing.T) {
	c := &Client{}
	for _, code := range []string{"activation_open", "route_moved", "corrupt_state", "constraint_violation", "candidate_not_ready", "no_route_head", "integrity_conflict", "environment_activation_blocked"} {
		err := c.mapStatus(409, []byte(`{"error":"`+code+`"}`))
		if !errors.Is(err, ErrIntegrityConflict) {
			t.Fatalf("409 %s mapped to %v, want ErrIntegrityConflict", code, err)
		}
		if errors.Is(err, ErrServiceUnavailable) {
			t.Fatalf("409 %s must not be retryable", code)
		}
	}
}
