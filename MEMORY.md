# AutoDeploy Memory

## Product and Architecture Decisions

- Goal: replace Coolify for the user's own Dockerfile-based applications on Hetzner.
- Initial topology: one Hetzner server; server/agent model must support multiple servers later
  without redesign.
- Source: private GitHub repositories via GitHub App and verified push webhooks.
- Stack approved: Go control plane and agent, PostgreSQL, Docker/BuildKit, Traefik, and a
  server-rendered UI with minimal JavaScript.
- PostgreSQL is the durable queue; Redis is intentionally excluded from V1.
- Agents initiate authenticated outbound communication. The public control plane must never
  receive Docker-socket or broad SSH access.
- Domains and automatic HTTPS are in scope.
- Each environment chooses health-gated blue/green or explicit stop-then-start deployment.
- Build and runtime variables are distinct. Secret build values should use BuildKit secret
  mounts; runtime values are encrypted at rest and redacted from output.
- Single administrator initially; durable domain boundaries retain ownership/scope fields.

## Safety Invariants

- Verify GitHub signatures against the raw request body and deduplicate delivery IDs.
- Persist deployment state before asynchronous work; agents use expiring job leases.
- Never route traffic to an unhealthy release; failed releases preserve the active release.
- Agent/Docker access is a trusted host boundary. User-controlled Dockerfiles can execute code,
  so builds require explicit repository trust and resource/time limits.
- Never print credentials or environment values. No secrets are currently stored in the repo.

## Completed

- Git repository initialized on `main`.
- Scaffold commit: `eb69761 chore: scaffold autodeploy platform`.
- Current stable baselines verified from official sources on 2026-07-23:
  - Go 1.26.5;
  - PostgreSQL 18.4;
  - GitHub Actions checkout/setup-go v6.
- Repository scaffold, CI, Makefile, Go entry points, and bootstrap Compose exist.
- Go 1.26.5 installed locally.
- Deployment domain implemented under `internal/deployment`:
  - immutable aggregate and identity;
  - complete lifecycle state machine;
  - deterministic timestamps;
  - idempotent duplicate transitions;
  - transition errors preserve the original aggregate;
  - CAS revision increments per persisted event;
  - separate snapshot schema version;
  - validated Snapshot/Rehydrate contract with defensive copies.
- Independent code review passed after remediation.
- `go test -race ./...`, `make check`, and `git diff --check` pass using
  `GOCACHE=/tmp/autodeploy-go-cache`.

## Resume Point

- Pause requested after completing and committing the deployment-domain slice.
- Next task: implement PostgreSQL schema and typed repositories only.
- Persistence must use the domain Revision as an optimistic compare-and-swap token and the
  Snapshot schema version for format evolution.
- Follow `tasks/todo.md`; do not start GitHub, agent, Docker, routing, or UI work until the
  persistence slice passes tests and independent review.
