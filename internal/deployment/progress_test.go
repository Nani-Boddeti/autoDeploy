package deployment

import "testing"

func TestAgentReportablePhaseExcludesControlPlaneStates(t *testing.T) {
	for _, status := range []Status{StatusFetching, StatusBuilding, StatusStarting, StatusHealthChecking} {
		if !AgentReportablePhase(status) {
			t.Fatalf("%s must be reportable by an agent", status)
		}
	}
	// An agent runs the build and the direct health gate; route switching and
	// the healthy verdict are the control plane's, so an agent must not be
	// able to claim them.
	for _, status := range []Status{StatusQueued, StatusAssigned, StatusActivating, StatusHealthy,
		StatusFailed, StatusCancelled, StatusSuperseded, StatusRolledBack, Status("nonsense")} {
		if AgentReportablePhase(status) {
			t.Fatalf("%s must not be reportable by an agent", status)
		}
	}
}

func TestExecutionOrderIsStrictlyIncreasingAlongThePath(t *testing.T) {
	path := []Status{StatusQueued, StatusAssigned, StatusFetching, StatusBuilding, StatusStarting,
		StatusHealthChecking, StatusActivating, StatusHealthy}
	previous := -1
	for _, status := range path {
		order, ok := ExecutionOrder(status)
		if !ok {
			t.Fatalf("%s must be on the execution path", status)
		}
		if order <= previous {
			t.Fatalf("%s order %d does not advance past %d", status, order, previous)
		}
		previous = order
	}
	for _, status := range []Status{StatusFailed, StatusCancelled, StatusSuperseded, StatusRolledBack, Status("")} {
		if _, ok := ExecutionOrder(status); ok {
			t.Fatalf("%s must not be on the execution path", status)
		}
	}
}

// TestExecutionOrderAgreesWithTransitionRules guards the two from drifting
// apart: every legal forward step along the path must also be a legal
// transition, or progress reporting would accept something the aggregate then
// refuses.
func TestExecutionOrderAgreesWithTransitionRules(t *testing.T) {
	path := []Status{StatusQueued, StatusAssigned, StatusFetching, StatusBuilding, StatusStarting,
		StatusHealthChecking, StatusActivating, StatusHealthy}
	for i := 0; i+1 < len(path); i++ {
		if !canTransition(path[i], path[i+1]) {
			t.Fatalf("%s -> %s is on the execution path but not a legal transition", path[i], path[i+1])
		}
	}
	// And a same-status transition is still rejected, which is exactly why
	// ExecutionOrder exists.
	if canTransition(StatusBuilding, StatusBuilding) {
		t.Fatal("canTransition must still reject a same-status transition")
	}
}
