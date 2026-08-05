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
- Deployment persistence implemented with PostgreSQL 18 and `pgx/v5`:
  - forward-only embedded migration plus dedicated migration command;
  - constrained `deployments` head and append-only `deployment_events` audit stream;
  - typed Create/GetByID/Save repository with transactional revision CAS;
  - repeatable-read aggregate loads and locked writes prevent mixed head/event snapshots;
  - advisory-lock serialized migrations with SHA-256 drift detection;
  - UTC microsecond timestamps and signed-`bigint` revision guards;
  - real PostgreSQL coverage for lifecycle, corruption, rollback, concurrency, and migration
    idempotency/integrity.
- Independent code review passed after remediation.
- `make check`, `go test -race ./...`, `make test-integration` against PostgreSQL 18, and
  `git diff --check` pass. Independent QA and code review report no blockers.

## Resume Point

- Persistence is complete and verified locally but uncommitted.
- Next task: add the architecture decision records already listed in `tasks/todo.md`, then plan
  the admin authentication slice.
- Do not start GitHub, agent, Docker, routing, or UI work before their dedicated approved slices.

## Persistence Invariants and Lessons

- Domain Revision is the optimistic CAS token; snapshot schema version evolves independently.
- Head and event reads must share one repeatable-read snapshot; separate pool queries can mix
  revisions during concurrent saves.
- Save contenders lock the head row and equal/stale revisions return a conflict.
- Migration ledger checks and DDL must share an advisory-locked transaction on one connection.
- Applied migration checksums are immutable; drift must fail rather than silently diverge.
- Tests that mutate schema must register cleanup immediately so failed assertions cannot poison
  later runs.
