package main

import (
	agenthttp "autodeploy/internal/agent/http"
	"autodeploy/internal/store/postgres"
)

// The agent handler discovers predecessor-health support (22-D2) through an
// optional interface on its Activator. main wires the lease repository
// directly; this assertion fails the build if that type stops satisfying the
// extension, which would otherwise silently disable the feature (fail closed,
// but dead).
var _ agenthttp.PredecessorHealthActivator = (*postgres.AgentLeaseRepository)(nil)
