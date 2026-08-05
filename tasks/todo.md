# AutoDeploy MVP Plan

## Status

- Implementation is active on `main`; scaffold, deployment domain, and deployment persistence
  slices are committed and synchronized with `origin/main` through `d6fb873`.

## Product Goal

Replace the currently used Coolify deployment path for personally managed applications on
one Hetzner server:

1. A push to a configured private GitHub repository triggers a signed webhook.
2. AutoDeploy records and queues the exact repository, branch, and commit.
3. A registered server agent fetches the source and builds its existing Dockerfile.
4. Build-time secrets and variables are supplied without becoming runtime configuration.
5. Runtime variables are decrypted only for the selected deployment server.
6. The new container is health checked.
7. Traffic is switched to the healthy release and the prior release remains available for rollback.

The first installation uses one server, but the control-plane/domain model and agent protocol
must support registering and targeting multiple deployment servers without redesign.

## Confirmed Requirements

- Initial deployment target: one Hetzner server.
- Future requirement: multiple deployment servers.
- Application repositories contain Dockerfiles.
- GitHub repositories are private.
- Implementation should prioritize low resource use and reliability.
- Deployment behavior may differ by application.

## Approved Architecture Decisions

- Stack: Go control plane and Go deployment agent, PostgreSQL, server-rendered HTML with
  minimal JavaScript, Docker/BuildKit, and Traefik.
- GitHub integration: GitHub App with installation-scoped repository access and signed webhooks.
- Access: one administrator account initially; schema retains explicit ownership boundaries.
- Routing: custom domains and automatic Let's Encrypt HTTPS through Traefik.
- Default release mode: blue/green with health-gated traffic switching.
- Alternate release mode: stop-then-start for applications that cannot run two revisions.
- Job queue: PostgreSQL-backed leases; no Redis dependency in the first version.
- Agent communication: outbound authenticated polling/streaming from each server so the
  control plane does not require an inbound Docker API or SSH port.

## Security and Architecture Invariants

- The public control plane never receives direct access to the Docker socket.
- Only the deployment agent has local Docker/BuildKit access.
- GitHub webhook signatures are verified against the unmodified request body.
- GitHub delivery IDs are unique and idempotent; duplicates do not create deployments.
- Repository and branch bindings are checked before a job is accepted.
- GitHub App installation tokens are short lived and never persisted as plaintext.
- Recoverable stored environment/provider values are envelope-encrypted; comparison-only
  agent/session tokens are hashed, and private signing credentials remain provider-held.
- Secret values are redacted from API responses, UI, audit records, and build/deployment logs.
- Build secrets require BuildKit secret mounts; ordinary Docker `ARG` values are
  classified as non-secret because image history or build output may expose them.
- Runtime secrets are scoped only to the target deployment, but Docker retains them in container
  configuration. Host administrators remain trusted because they can inspect that configuration.
- Deployment state transitions are durable and auditable.
- Agents claim jobs using expiring leases; abandoned work can be recovered safely.
- A release never receives production traffic before its configured health gate succeeds.
- Failed releases preserve the currently healthy release.
- All destructive operations are scoped by immutable project, environment, server, and
  deployment identifiers.

## Proposed Repository Structure

```text
cmd/
  controlplane/             # HTTP API, webhook receiver, UI, scheduler
  agent/                    # Server registration, job execution, log delivery
internal/
  auth/                     # Admin sessions and authorization
  crypto/                   # Envelope encryption and secret redaction
  deployment/               # Deployment domain and state machine
  github/                   # GitHub App and webhook adapter
  queue/                    # PostgreSQL job leases
  server/                   # Server registration and agent protocol
  runtime/docker/           # Docker/BuildKit implementation
  routing/traefik/          # Routes, HTTPS metadata, traffic switching
  store/postgres/           # Durable repositories and transactions
  web/                      # HTTP handlers, templates, static assets, SSE
migrations/                 # Forward-only PostgreSQL schema migrations
deploy/                     # Compose/bootstrap configuration for AutoDeploy
tests/                      # Integration and end-to-end fixtures
docs/                       # Operator setup, backup, restore, and recovery
```

## Domain Model

- `users`: initial administrator and credential state.
- `github_installations`: installation identity and repository authorization metadata.
- `projects`: repository binding and default branch.
- `environments`: project environment, deployment mode, health policy, and target server.
- `servers`: registered agent, labels, capacity, last heartbeat, and status.
- `environment_variables`: encrypted value, build/runtime scope, secret flag, and version.
- `routes`: hostname, target service port, TLS policy, and active release.
- `webhook_deliveries`: delivery ID, event type, verification result, and processing status.
- `deployments`: commit SHA, desired server, state, timings, release identifiers, and failure summary.
- `deployment_events`: append-only state transitions and audit details.
- `deployment_logs`: ordered redacted log chunks with retention limits.
- `agent_jobs`: lease owner, lease expiry, attempts, cancellation, and result.

## Deployment State Machine

`queued -> assigned -> fetching -> building -> starting -> health_checking -> activating -> healthy`

Terminal/alternate states:

- `failed`: stage-specific sanitized failure; active release unchanged.
- `cancelled`: only before activation; agent performs scoped cleanup.
- `superseded`: a newer queued commit replaces an older deployment when configured.
- `rolled_back`: traffic returned to a retained healthy release.

State transitions must be validated in the domain layer and committed transactionally.

## Implementation Sequence

- [x] Initialize Git repository, Go module, formatting, linting, and test commands.
- [ ] Add architecture decision records:
  - [x] Control-plane/agent separation.
  - [x] GitHub App authentication.
  - [x] Secret handling.
  - [x] PostgreSQL queue leases.
  - [ ] Release strategies.
- [x] Implement PostgreSQL migrations and typed persistence repositories.
- [ ] Implement admin bootstrap, secure session cookies, CSRF protection, and authorization.
- [ ] Implement envelope encryption, key rotation metadata, and centralized redaction.
- [ ] Implement GitHub App installation flow and short-lived clone authentication.
- [ ] Implement raw-body webhook verification, delivery idempotency, repository/branch filtering,
      and durable job creation.
- [ ] Implement server registration, scoped agent credentials, heartbeat, job leasing, renewal,
      cancellation, and recovery.
- [ ] Implement isolated checkout of the exact commit with repository and size controls.
- [ ] Implement Docker/BuildKit build execution, secret mounts, resource/time limits, labels,
      log redaction, and scoped cleanup.
- [ ] Implement runtime container creation with environment variables, networks, resource limits,
      restart policy, and immutable release labels.
- [ ] Implement HTTP/TCP/container health policies with bounded retries.
- [ ] Implement Traefik route generation, automatic HTTPS integration, and atomic active-release
      switching.
- [ ] Implement blue/green activation, stop-then-start mode, release retention, and rollback.
- [ ] Implement minimal UI for projects, servers, environments, variables, domains, deployments,
      live logs, retry, cancel, and rollback.
- [ ] Add metrics, structured sanitized logs, readiness/liveness endpoints, retention cleanup,
      backup instructions, and disaster-recovery documentation.
- [ ] Package the control plane, agent, PostgreSQL, and Traefik for installation on Hetzner.
- [ ] Run unit, integration, security, failure-recovery, and end-to-end validation.

## Required Tests

- Unit tests:
  - Webhook signature verification and raw-body handling.
  - Deployment state transition rules.
  - Environment scope and secret redaction.
  - Agent lease acquisition, renewal, expiry, and retry.
  - Route validation and release selection.
- PostgreSQL integration tests:
  - Duplicate GitHub deliveries create one deployment.
  - Concurrent agents cannot claim the same job.
  - Failed transactions do not leave partial deployment state.
  - Ownership/server boundaries prevent cross-scope access.
- Docker integration tests:
  - Successful Dockerfile build and container start.
  - Build failure preserves the active release.
  - Runtime variables are applied only at runtime.
  - Secret values do not occur in persisted logs.
  - Timeout, cancellation, restart, and cleanup behavior.
- End-to-end tests:
  - Signed push webhook through healthy HTTPS activation.
  - Invalid signature and unauthorized repository rejection.
  - Failed health check leaves the previous version serving traffic.
  - Blue/green rollback restores the prior release.
  - Stop-then-start behavior is explicit and correctly reports downtime.
  - Agent disconnect and lease expiry recover without duplicate activation.

## Edge Cases

- Duplicate, delayed, and out-of-order GitHub webhook deliveries.
- Several commits pushed while another commit is building.
- Force-pushed or deleted branches.
- GitHub token expiry during checkout.
- BuildKit/Docker restart or disk exhaustion during a build.
- Agent disconnect after starting a container but before reporting success.
- Two releases racing to activate the same route.
- Health endpoint returns success before the application is actually ready.
- Application uses persistent volumes and cannot run two revisions concurrently.
- Domain DNS does not point to the server or Let's Encrypt rate limits issuance.
- Secret rotation while a deployment is queued.
- Rollback target image has been removed by retention cleanup.

## Risks and Mitigations

- Agent compromise grants Docker-level host control.
  - Run only on explicitly registered servers, use scoped rotating credentials, restrict the
    control-plane API, audit commands, and document the Docker trust boundary.
- User-controlled Dockerfiles can execute arbitrary code during builds.
  - Treat configured repository maintainers as trusted deployers; apply CPU, memory, disk,
    duration, and network policies where supported.
- Blue/green is unsafe for some stateful applications.
  - Make release strategy explicit per environment and require compatible health/volume settings.
- Secrets can leak through application output or unsafe Dockerfile instructions.
  - Redact known values, prefer BuildKit mounts, warn on secret build args, and document that
    application-generated transformations cannot always be detected.
- A single initial server remains a failure domain.
  - Keep control-plane backups, retained releases, documented recovery, and a server abstraction
    that permits later migration or additional agents.

## Completion Evidence

- All automated tests pass.
- Static analysis and formatting checks pass.
- A real private test repository push deploys to a Hetzner staging hostname over HTTPS.
- Invalid webhooks and duplicate deliveries are demonstrably rejected/idempotent.
- A deliberately broken build and failed health check leave the previous release serving.
- A rollback completes and is recorded in the audit trail.
- Logs are inspected for credential and environment-value leakage.
- Final diff is reviewed for minimal scope, production safety, and backward-compatible behavior.

## Review

Approved by the user:

1. Automatic domain routing and HTTPS should be included.
2. Single-admin authentication is acceptable for the first release.
3. Go + PostgreSQL + Docker/BuildKit + Traefik is acceptable.
4. Both blue/green and stop-then-start strategies should be available per environment.

## Implementation Progress

- [x] Repository scaffold and CI/tooling verified with Go 1.26.5.
- [x] Deployment aggregate and lifecycle state machine implemented with:
  - deterministic transition timestamps;
  - immutable identity and defensive event copies;
  - explicit transition, cancellation, failure, supersession, and rollback rules;
  - optimistic-lock revision;
  - separately versioned and validated persistence snapshots;
  - race-tested transition-matrix and corruption coverage.
- [x] PostgreSQL schema and typed deployment repository implemented and independently verified.
- [x] ADR 0001 records the control-plane/agent process, ownership, and trust boundaries.
- [x] ADR 0002 records private GitHub App installation authentication, least-privilege repository
      access, webhook verification, and credential boundaries.
- [x] ADR 0003 records envelope encryption, immutable secret versions, materialization, rotation,
      redaction, credential-provider, build/runtime delivery, backup, and deletion boundaries.
- [x] ADR 0004 records PostgreSQL at-least-once jobs, attempts, lease renewal/expiry, environment
      fencing, retry/dead-letter, cancellation, recovery, and transaction boundaries.
- [ ] Next: ADR 0005 for release strategies.

## Approved Persistence Slice

Approved on 2026-08-05. Keep this slice limited to the existing deployment aggregate; do not
invent persistence contracts for users, projects, environments, servers, jobs, webhooks, or
other domains that are not implemented yet.

### Implementation

- [x] Add a forward-only PostgreSQL migration for `deployments` and append-only
      `deployment_events`.
- [x] Enforce identifiers, lifecycle statuses, positive snapshot versions/revisions, timestamp
      ordering, event shape, and useful environment/server indexes in PostgreSQL.
- [x] Prevent deployment-event updates and deletes at the database boundary.
- [x] Add a typed `pgx/v5` deployment repository with `Create`, `GetByID`, and revision-CAS
      `Save` operations.
- [x] Keep head updates and event appends in one transaction; preserve immutable identity and
      distinguish not-found, duplicate, and revision-conflict errors.
- [x] Validate all writes through `Snapshot` and all reads through `Rehydrate`.
- [x] Canonicalize domain timestamps to UTC microsecond precision for deterministic PostgreSQL
      round trips.
- [x] Reject revisions that cannot fit PostgreSQL `bigint`.
- [x] Add a dedicated forward-only migration execution path using currently supported stable
      dependencies verified against official documentation before selection.
- [x] Keep opaque text identifiers and defer foreign keys to unimplemented domain tables.

### Validation

- [x] Test migration application against real PostgreSQL 18.
- [x] Test create/load equality and every deployment lifecycle status.
- [x] Test duplicate IDs, missing IDs, stale revisions, and concurrent compare-and-swap saves.
- [x] Prove failed event insertion rolls back the head update.
- [x] Test corrupted persisted state rejection and deterministic event ordering.
- [x] Test append-only enforcement, equal timestamps, multi-transition saves, cancellation,
      supersession, and rollback.
- [x] Update CI and Makefile integration-test commands without weakening existing checks.
- [x] Pass `make check`, `go test -race ./...`, PostgreSQL integration tests, and
      `git diff --check`.
- [x] Obtain independent QA and zero-context code review before marking this slice complete.

### Result

- PostgreSQL 18 integration tests cover repository and migration packages.
- Aggregate reads use a repeatable-read snapshot; writes use revision CAS and row locking.
- Migration execution is advisory-lock serialized and verifies SHA-256 checksums.
- Final independent QA and zero-context review verdicts: PASS with no blockers.

### Risks and Mitigations

- PostgreSQL stores `timestamptz` at microsecond precision: canonicalize timestamps at domain
  entry points and cover equality with tests.
- PostgreSQL `bigint` is signed while the domain revision is unsigned: validate before SQL
  conversion and fail explicitly.
- Concurrent writers can fork event history: use a conditional head update on the expected
  revision and append events in the same transaction.
- Head/event corruption must not be silently repaired: return domain snapshot validation errors.
- Database status constraints duplicate domain values: update both through reviewed migrations
  whenever lifecycle states change.
