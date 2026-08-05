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
- Persistence commit `d6fb873 feat: add PostgreSQL deployment persistence` is synchronized with
  `origin/main`.
- ADR 0001 accepts separate control-plane and agent process/trust boundaries:
  - co-location is allowed and preserves separate credentials/responsibilities, but shares the
    physical host compromise boundary; separate hosts or separately reviewed isolation are needed
    for host-compromise isolation;
  - agents initiate authenticated outbound API communication and never access PostgreSQL;
  - the control plane owns authorization, desired state, jobs, and audit records without Docker
    or broad SSH access;
  - agents own authorized target-host Docker/BuildKit, runtime, cleanup, and routing mutations.
- ADR 0002 accepts a private GitHub App owned by the same account as every V1 target repository:
  - explicit permission is Contents: read; explicit subscription is Push, plus mandatory
    installation lifecycle events;
  - stable numeric installation/repository IDs and explicit local project binding authorize
    access; mutable names, webhook sender, callback parameters, or signatures alone do not;
  - the control plane alone holds App/webhook secrets and mints opaque, one-repository,
    read-only installation tokens near checkout; tokens and App JWTs are never persisted;
  - agents receive only transient checkout tokens and never App private keys or PostgreSQL access;
  - webhook HMAC is verified over the bounded raw body before parsing; delivery attempts bind App
    registration ID, delivery ID, and digest, while a per-App digest index blocks replay with a
    substituted delivery ID;
  - automatic push intent is unique by active project binding, repository ID, full ref, and commit
    SHA; manual redeploy is a separate authenticated intent;
  - transient token delivery requires confidentiality-protected transport with server identity
    verification and no intermediary credential logging;
  - suspension/removal blocks new work and token minting while preserving the healthy release;
    additions require reconciliation and explicit binding/reactivation;
  - service-readable mounted files outside the repository are the V1 credential source behind
    fail-closed provider interfaces; detailed file protections are deferred to ADR 0003;
  - private cross-repository submodules are rejected unless each repository is independently
    authorized.

## Resume Point

- Persistence is complete, verified, committed, and pushed.
- Next task: ADR 0003 for secret handling. Complete the remaining ADRs sequentially,
  then plan the admin authentication slice.
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
