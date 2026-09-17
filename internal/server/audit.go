package server

// ActorType identifies who caused a server_audit_events row: an
// administrator through cmd/admin, the agent itself through its
// authenticated channel, or the control plane acting on its own (e.g. the
// sweeper). It mirrors the servers_audit_events actor_type CHECK.
type ActorType string

const (
	ActorAdministrator ActorType = "administrator"
	ActorAgent         ActorType = "agent"
	ActorSystem        ActorType = "system"
)
