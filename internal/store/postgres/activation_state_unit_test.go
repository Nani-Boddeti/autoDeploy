package postgres

import "testing"

func TestUnresolvedActivationPredicate(t *testing.T) {
	// Every state in release_operations_state_check.
	cases := map[string]bool{
		"started":                 true,
		"switch_authorized":       true,
		"reconciliation_required": true,
		"succeeded":               false,
		"failed":                  false,
	}
	for state, want := range cases {
		if got := unresolvedActivation(state); got != want {
			t.Errorf("unresolvedActivation(%q)=%v want %v", state, got, want)
		}
	}
	if !unresolvedActivation("some_future_open_state") {
		t.Error("an unknown (future) state must be unresolved by default")
	}
}

func TestValidateFinalizeFromState(t *testing.T) {
	for _, ok := range []string{"switch_authorized", "reconciliation_required"} {
		if err := validateFinalizeFromState(ok); err != nil {
			t.Errorf("fromState %q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "succeeded", "started", "SWITCH_AUTHORIZED"} {
		if err := validateFinalizeFromState(bad); err == nil {
			t.Errorf("fromState %q accepted", bad)
		}
	}
}
