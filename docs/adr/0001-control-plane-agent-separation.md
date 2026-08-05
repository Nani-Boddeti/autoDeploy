# ADR 0001: Separate the Control Plane from Deployment Agents

- Status: Accepted
- Date: 2026-08-05

## Context

AutoDeploy receives public GitHub webhooks and administrative traffic while also building and
running user-controlled Dockerfiles on deployment hosts. Combining those capabilities in one
trust boundary would give an internet-facing process direct control of Docker and widen the
impact of a control-plane compromise.

The first installation targets one Hetzner server, but the architecture must support multiple
deployment servers without changing the domain or authorization model. Asynchronous deployments
must remain durable, auditable, recoverable after agent failure, and scoped to immutable project,
environment, server, and deployment identities.

## Decision

AutoDeploy consists of separate control-plane and deployment-agent processes with independent
credentials and responsibilities. They may be co-located for a small installation, but
co-location shares the physical host trust boundary: compromise of the Docker-capable agent or
host can expose the control-plane process and credentials. Separate processes still preserve
least-privilege configuration, responsibility ownership, and the ability to move either role to
another host without redesign; separate hosts or an independently specified isolation boundary
are required for host-compromise isolation.

Agents initiate authenticated outbound communication with the control-plane API. An agent does
not accept control-plane SSH sessions, expose Docker remotely, or connect directly to the
PostgreSQL database.

The control plane authorizes and records desired operations. An agent performs only operations
that are authorized for its immutable server identity and deployment scope.

## Control-plane responsibilities

- Receive the administrative UI/API and verified GitHub webhooks.
- Authorize projects, environments, servers, routes, and deployments.
- Persist desired state, deployment history, audit events, and durable jobs.
- Schedule work and issue scoped, expiring job leases.
- Validate state transitions and accept sanitized agent results and logs.
- Decide when a healthy release may become active and record rollback intent.

The control plane must not receive a Docker socket, remote Docker API access, or broad SSH access
to deployment servers.

## Agent responsibilities

- Register and authenticate as one immutable deployment server identity.
- Claim, renew, complete, or abandon scoped job leases.
- Fetch the exact authorized commit and invoke local Docker/BuildKit.
- Create, inspect, health-check, activate, retain, roll back, and remove scoped releases.
- Apply authorized target-host routing changes through the future Traefik adapter.
- Redact sensitive values before transmitting logs, status, or diagnostic data.
- Stop or clean up safely when authorization, lease ownership, or cancellation state changes.

Only the agent may access Docker/BuildKit on its deployment host. Host administrators and the
local Docker daemon remain inside the trusted-host boundary.

## Communication and trust boundaries

- All agent coordination uses an authenticated control-plane API; agents never query or mutate
  PostgreSQL directly.
- Agent credentials are scoped to one server identity and must not authorize administrative or
  cross-server operations.
- Agents initiate connections, so deployment hosts do not require inbound SSH or Docker/API
  exposure for AutoDeploy.
- Every destructive command is scoped by immutable project, environment, server, deployment,
  and release identifiers as applicable.
- Durable desired state is committed before asynchronous work is offered to an agent.
- Agent reports are untrusted inputs and must be authorized, validated, size-limited, sanitized,
  and idempotently processed by the control plane.

## Failure and recovery invariants

- Job ownership expires through leases; abandoned jobs can be reclaimed without accepting stale
  completion from a previous owner.
- Losing agent connectivity must not erase durable deployment intent or audit history.
- A release cannot receive production traffic before its health gate succeeds.
- A failed or disconnected agent must not replace the currently healthy active release.
- Retries must preserve deployment revision checks and idempotent operation boundaries.

## Consequences

Benefits:

- A separately hosted control plane has a smaller infrastructure blast radius; logical
  separation also prevents the control-plane process from requiring Docker privileges.
- Deployment hosts require no inbound management port for AutoDeploy.
- Additional agents and servers can be introduced without redesigning persistence ownership.
- Durable authorization and audit decisions remain centralized.

Costs:

- Agent registration, scoped credentials, leases, retries, cancellation, and protocol versioning
  become explicit product requirements.
- Co-located installations still run and supervise two processes.
- Co-location shares one host compromise boundary and therefore provides weaker security than
  separate hosts or a future independently reviewed isolation mechanism.
- Host mutations require carefully designed idempotent agent operations and result reporting.

## Alternatives considered

- Give the control plane the Docker socket: rejected because an internet-facing compromise would
  become immediate host-level Docker control.
- Drive servers through broad SSH access: rejected because credential scope and command auditing
  are too broad for the deployment boundary.
- Combine both roles into one process: rejected as the required trust boundary, although the two
  processes may run on the same host.
- Let agents access PostgreSQL directly: rejected because it bypasses API authorization, couples
  agents to storage schema, and expands database network exposure.
- Require an inbound agent listener: rejected because outbound agent communication avoids opening
  deployment-host management ingress.

## Deferred decisions

- Polling, long polling, or streaming transport.
- Agent registration, credential issuance, rotation, revocation, and protocol versioning.
- Lease durations, renewal cadence, retry policy, and stale-result handling details.
- Exact Docker/BuildKit, Traefik, log-delivery, and cancellation adapters.
- Whether the initial control plane is physically co-located with the deployment agent.

These details require dedicated approved implementation slices and must preserve this ADR's trust
and ownership boundaries.

## Validation criteria

- The control-plane process can run without Docker or SSH credentials.
- The agent can run without PostgreSQL credentials or administrative API permissions.
- Deployment documentation states whether roles share a host trust boundary; it must not claim
  host-compromise isolation from process separation alone.
- A server can be added through the agent protocol without changing deployment ownership fields.
- Expired or revoked agent authority cannot mutate another server or complete a stale lease.
- Failed builds, health checks, or agent connectivity leave the active healthy release unchanged.
