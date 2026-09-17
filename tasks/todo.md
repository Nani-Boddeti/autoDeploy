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
- [x] Add architecture decision records:
  - [x] Control-plane/agent separation.
  - [x] GitHub App authentication.
  - [x] Secret handling.
  - [x] PostgreSQL queue leases.
  - [x] Release strategies.
- [x] Implement PostgreSQL migrations and typed persistence repositories.
- [x] Implement admin bootstrap, secure session cookies, CSRF protection, and authorization.
- [x] Implement envelope encryption, key rotation metadata, and centralized redaction.
- [x] Implement GitHub App installation flow and short-lived clone authentication.
- [x] Implement raw-body webhook verification, delivery idempotency, repository/branch filtering,
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

## GitHub App installation and checkout slice — implementation complete; privileged operational validation pending

Scope completed: installation setup, explicit repository selection, single-repository checkout
credential issuance, and the production exact-commit Git adapter. Raw webhook ingestion and durable
job creation are also complete in the following recorded slice; authenticated agent transport remains
the next implementation boundary.

### Approval assumptions

- Repository discovery may mint one control-plane-only installation token limited to metadata/read
  discovery, paginate bounded repository metadata, then revoke it immediately. Checkout tokens stay
  exact-repository `contents:read`, one checkout, short-lived, in memory, and immediately revoked.
- Implement the real checkout issuer, one-use credential broker/askpass helper, and exact-commit Git
  adapter now. Do not fake cross-process delivery; only authenticated agent transport is deferred.
- Persist constrained opaque `project_id` bindings without a foreign key until the project aggregate
  exists. Numeric GitHub IDs are authorization identity; mutable names are display/routing metadata.
- Add only minimal server-rendered setup confirmation and repository-selection forms required for
  same-origin POST/CSRF semantics. Coolify-inspired dashboard work remains a later UI slice.

### Step 1 — ADR amendment, App identity, signer, and provider adapter

- [x] Amend ADR 0002 for the temporary metadata-only discovery token, GitHub REST version
      `2026-03-10`, App client ID as JWT issuer, and confidential future agent-response delivery as
      the only token-delivery exception. Do not broaden checkout-token scope.
- [x] Add `internal/githubapp` typed identities/errors plus hardened mounted RSA PKCS#1/PKCS#8 key
      loading, RS256 App JWT generation (`iat` backdated 60 seconds; expiry within ten minutes), and
      a fixed-origin `net/http` adapter with no redirects, bounded bodies/pages/counts, explicit API
      headers, timeouts, rate-limit classification, and sanitized provider request IDs.
- [x] Verify App ID/client ID/slug/owner and required permissions before installation operations.
      Test fixed JWT vectors, malformed/weak/encrypted keys, exact headers/bodies, pagination,
      redirects, opaque token formats, response scope/expiry, rate limits, and provider failures.

#### Step 1 result

- ADR 0002 now records the metadata-only discovery exception, current GitHub REST contract, client-ID
  JWT issuer, and the future confidential agent-response delivery boundary without broadening
  checkout-token scope.
- Added hardened RSA PKCS#1/PKCS#8 mounted-key loading and signer-owned deep-copied key material;
  canonical RS256 JWTs use a 60-second backdate and nine-minute lifetime.
- Added the fixed-origin GitHub provider adapter with strict status/header/body/pagination/retry/rate
  contracts, full App permission/event allowlists, installation identity checks, exact returned token
  scope/lifetime validation, and best-effort revocation of every rejected or discovery token.
- Full format/vet/unit/build/race and Windows cross-compilation checks pass. Independent QA and
  zero-context security review return PASS; token/sentinel values remain absent from errors.

### Step 2 — installation state, Strict handoff, repository discovery, and binding

- [x] Add forward-only migrations `000005`/`000006` for App registration, bounded one-use setup states,
      verified pending handoffs, installations, discovered repositories, explicit project bindings,
      authorization revisions, and append-only sanitized audit; enforce composite owner-scoped FKs,
      immutable numeric identities, cardinality/cleanup bounds, and no credential/provider-body data.
- [x] Add typed repository/service. Authenticated CSRF POST creates setup state; the cross-site
      callback validates and consumes state plus exact installation/owner identity but cannot grant
      authorization; a separate Strict-cookie same-origin GET/POST handoff revalidates session/user/
      owner/auth revision before atomic activation and audit.
- [x] Discover repositories with the temporary metadata-only token, revoke it in every terminal path,
      and require a separate authenticated CSRF POST to bind a numeric repository ID to an opaque
      local project. Suspension/removal/permission loss disables bindings and increments revisions.
- [x] Add only exact setup/callback/handoff/selection routes and forms. Test replay/expiry/races,
      stale sessions, forbidden callback side effects, cross-owner corruption, cleanup caps, cookie/
      CSRF/provenance, clean redirects, repository pagination/revocation, and suspension denial.

#### Step 2 result

- GitHub App setup uses digest-only bounded one-use states, callback leases, Strict handoff, exact
  session/user/owner/revision and active-registration rechecks, composite session FKs, and explicit
  numeric project bindings.
- Complete discovery always revokes its metadata token and atomically refreshes/removes authority;
  authenticated reconciliation fails closed on App/installation loss. Disable paths revise active
  installation/repository/binding authority and append collision-safe sanitized audits.
- Per-owner append-only audit admission is serialized and dynamically reserves future disable audit
  capacity for all active installations/repositories; ordinary grants fail closed at capacity.
- Fresh PostgreSQL 18.4 normal/race integration, full format/vet/unit/build/race checks, migration
  checksum/drift, `git diff --check`, independent QA, and zero-context security review pass.

### Step 3 — checkout authorization, token broker, and exact-commit Git adapter

Current status: implementation and static validation are complete. The production runner now uses
only reviewed local quota/spawn clients: helper-created XFS-project-quota workspaces, a distinct
fixed checkout UID/GID, atomic cgroup-v2 admission, typed Git operations, pipe-only askpass,
bounded redacted output, exact SHA/tree checks, resumable cleanup, and unconditional revocation.
Unit/race, repository-wide checks, Linux amd64/arm64 builds, tagged integration compilation, and
independent security reviews pass. Step 3 remains operationally incomplete until the explicit
privileged Linux smoke runs on XFS `prjquota` with the installed root helpers and delegated cgroup v2.

#### Preventive Linux execution boundary — approved 2026-08-13

Privileged-helper boundary approved 2026-08-13: a root-owned quota service is fixed to one XFS mount,
authenticates only the agent UID, creates root-inaccessible workspaces itself, owns a mount-bound
non-reusing project-ID ledger, verifies hard byte/inode enforcement before granting the workspace to
the distinct checkout UID/GID, and performs descriptor-relative resumable cleanup. It exposes no
arbitrary path, device, project ID, command, or quota-disable operation.

Phase 2 review finding: a payload running as the same UID that owns the delegated cgroup subtree can
move itself or descendants into a sibling cgroup and escape leaf limits/kill authority. Production
containment therefore requires a distinct fixed checkout payload UID with no write access to the
delegated hierarchy. Granting the agent/launcher UID-switch authority or adding a privileged spawn
service is a separate privilege expansion. Approved 2026-08-13: replace the unprivileged launcher
with a root-owned local fixed-purpose spawn service. It may spawn only validated pre-opened Git and
workspace descriptors under one configured checkout UID, with no supplementary groups/capabilities,
an allowlisted environment, fixed/bounded Git arguments, and atomic cgroup-FD admission. It exposes
no arbitrary path, executable, shell, UID, cgroup, or command selection and fails closed unless UID
separation, capability removal, descriptor closure, and cgroup placement can be verified.

- [x] Add a Linux-only checkout executor that requires cgroup v2 and an XFS checkout filesystem
      mounted with project quotas; production checkout fails closed when either boundary is absent,
      misconfigured, or cannot be verified.
- [x] Allocate a unique XFS project ID per checkout, set the hard block/inode limits before Git can
      write, attach the private workspace to that project, and remove quota/accounting state during
      idempotent cleanup without following attacker-controlled links.
- [x] Create a dedicated cgroup before process start with bounded memory, CPU, PIDs, and I/O policy;
      place Git and descendants into it at spawn, kill the complete process group/cgroup on timeout,
      cancellation, or authority invalidation, and verify termination before cleanup.
- [x] Open and validate operator-controlled executables, checkout root, empty home, and required
      control files through trusted descriptors. Execute through trusted descriptors where Linux
      supports it and reject ownership, permissions, type, mount, or identity drift.
- [x] Keep the existing anonymous-pipe-only token handoff, fixed Git environment, exact-SHA/tree
      validation, LFS/submodule rejection, bounded redacted output, and unconditional revocation.
- [ ] Add focused unit tests plus privileged Linux integration tests for quota exhaustion, inode/PID/
      memory limits, descendant escape attempts, cancellation, cleanup, descriptor/path replacement,
      unsupported filesystems, unavailable controllers, and secret-sentinel absence.
- [x] Document the XFS `prjquota` and delegated cgroup v2 host prerequisites and recovery behavior.
- [ ] Run format, vet, unit, race, Linux integration, migration drift, `git diff --check`, independent
      QA, and zero-context security review before marking checkout Step 3 complete.

Operational compatibility: this is an intentional production host requirement. Portable/unit test
adapters may exercise the same interfaces, but production checkout must not fall back to post-write
directory scans or an unbounded subprocess on unsupported hosts.

- [x] Implement two-phase authorization: validate and capture binding revision; mint one exact-repo
      `contents:read` token; validate token scope/permissions/expiry; re-lock and revalidate revision;
      revoke and fail closed if stale. Never share/cache tokens across checkouts.
- [x] Add a one-use anonymous-pipe credential broker and dedicated askpass executable. Tokens must
      never enter URL, argv, Git config, ambient credential helpers, durable jobs, logs, errors, or
      environment values; only a non-secret inherited FD/path may be passed to Git.
- [x] Fetch the exact authorized SHA into a fresh directory, verify `FETCH_HEAD`, detached-checkout,
      disable terminal prompts/helpers/LFS smudge, reject submodules and tracked LFS pointers, bound
      output through centralized redaction, clean up all paths, and revoke the token on every exit.
- [ ] Test sentinel-token absence from argv/environment/URL/config/logs/errors, one password delivery,
      exact SHA, one bounded auth remint after lease/revision revalidation, cancellation/TLS/output/
      cleanup/revocation, submodule/LFS rejection, and provider/checkout error classification.

#### Step 3 static result

- Root quota and fixed-UID spawn services, concrete production clients/factory, typed runner, tests,
  operator guide, CI Linux cross-builds, and opt-in `make test-checkout-integration` are implemented.
- `make check`, `go test -race ./...`, Linux amd64/arm64 helper/runner builds, tagged privileged-test
  compilation, `git diff --check`, and independent reviews pass with no high/medium findings.
- Pending proof: run the non-cached privileged smoke on a provisioned Linux host. It must allocate on
  XFS `prjquota`, execute typed `GitInit` as the distinct checkout UID/GID inside delegated cgroup v2,
  clean the project quota/workspace, and leave no cgroup leaf. No macOS fallback can prove this.

### Validation and risks

- [x] Step 2: format/vet/unit/build/race checks, fresh PostgreSQL 18.4 normal
      and race integration where relevant, migration checksum/drift, `git diff --check`, independent
      QA, and zero-context security review.
- Callback-ID spoofing/replay: digest-only one-use state, exact App-authenticated installation lookup,
  owner/session/revision binding, and a separate same-origin Strict handoff.
- Token overreach/leakage: exact numeric repository scope, permission/expiry response validation,
  two-phase revision recheck, no persistence/cache, pipe-only askpass delivery, redaction, and revoke.
- Provider drift/outage: explicit API version, bounded contract parsing, typed rate/transient errors,
  bounded GET retries only, and readiness based on local key/config—not continuous GitHub reachability.
- Agent boundary: production issuer and clone adapter exist now; no insecure local simulator or fake
  cross-process token transport is introduced before authenticated agent enrollment/transport.

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
- [x] ADR 0005 records blue/green and stop-then-start release models, health gates, Traefik file
      routing, fenced activation/reconciliation, rollback, retention, and cleanup boundaries.
- [x] ADR 0006 records administrator bootstrap/recovery, Argon2id password verification,
      server-side sessions, CSRF/origin protection, durable throttling, trusted-proxy handling,
      owner-scoped authorization, audit, and the Strict-cookie GitHub handoff boundary.
- [x] Administrator authentication domain primitives implemented, race-tested, independently
      QA-verified, and security-reviewed.
- [x] Administrator-authentication PostgreSQL migration and typed persistence workflows are
      implemented, live-integration/race-tested, independently QA-verified, and security-reviewed.
- Progress snapshot: 7/18 top-level implementation-sequence items complete (38.9%, approximately
  39%). Weighted evidence estimate remains approximately 35–40%; the isolated-checkout top-level
  milestone stays unchecked until its privileged Linux operational smoke passes.

## Approved Administrator Authentication Slice

Approved on 2026-08-05. Implement sequentially; each production step receives its own
implementation specialist and review. Do not add deployment administration until deployment
records carry explicit owner scope.

### Architecture and persistence

- [x] Record the security and failure invariants in ADR 0006.
- [x] Add a forward-only additive migration for owners, permanent bootstrap state, users,
      server-side sessions, durable throttle buckets, and append-only authentication audit events.
- [x] Add typed PostgreSQL transactions for singleton bootstrap, login attempt reservation and
      finalization, session creation/validation/revocation, password reset, and cleanup.
- [x] Preserve exact owner ID and authentication revision checks in every security-sensitive
      transaction; audit failure must roll back its associated mutation.

### Authentication domain

- [x] Implement canonical ASCII usernames and the 15-to-1,024-byte password policy.
- [x] Implement bounded Argon2id PHC parsing, hashing, verification, dummy verification, and
      rehash signaling using a currently supported stable dependency.
- [x] Implement versioned 32-byte opaque session tokens, domain-separated digesting, expiry
      policy, and non-secret principal types.
- [x] Implement HMAC-derived session CSRF and independent reusable pre-authentication login CSRF.
- [x] Implement exact owner-scoped authorization helpers without a role-based global bypass.

### Operator and configuration boundary

- [x] Add validated mounted-file credential loading for production database/signing material;
      accept only non-secret paths and public settings through environment configuration.
- [x] Add interactive-terminal-only `admin bootstrap` and `admin reset-password` commands.
- [x] Make bootstrap permanently one-time; make reset revoke all sessions, increment the auth
      revision, clear recovery throttle state, and audit atomically.

#### Credential and administrator CLI implementation plan

- [x] Add a Linux mounted-credential loader that requires absolute paths outside configured
      workspace roots; uses descriptor-relative no-follow opening; verifies the opened regular
      file's owner/mode/size; reads through the verified descriptor; and returns only sanitized
      errors. Unsupported platforms fail closed.
- [x] Accept the production database credential only through
      `AUTODEPLOY_DATABASE_URL_FILE`; remove the credential-bearing environment-value path from
      `migrate` and share the same loader/config boundary with the administrator command.
- [x] Add forward migration `000003` so ordinary known-user pair throttle rows carry a nullable
      recovery user ID. Keep unknown-user, IP, invalid-forward, and shared-overflow evidence
      unassociated and uncleared.
- [x] Extend pair reservation and password reset so reset atomically clears every tagged pair row
      for the user plus supplied legacy/username aliases, while preserving IP and shared-overflow
      evidence. Never modify the applied `000002` migration.
- [x] Add exact `admin bootstrap --username <name>` and
      `admin reset-password --username <name>` flows. Open and verify `/dev/tty`, read the password
      twice without echo, reject piped input and all secret argv/env/config inputs, generate opaque
      IDs from `crypto/rand`, calibrate bootstrap Argon2id once within ADR bounds, and load the
      durable policy for reset.
- [x] Cover unsafe file paths/types/ownership/modes/sizes/replacement, strict CLI parsing,
      terminal refusal, password boundaries/mismatch, concurrent singleton bootstrap, migration
      upgrade, reset rollback/session revocation/all tagged-pair cleanup, and preservation of
      IP/overflow evidence. Run unit/race/static checks plus live PostgreSQL integration and
      independent QA/security review before commit.

### Web boundary

- [x] Approve a signed, timestamped stateless `__Host-ad_login` envelope so every replica can
      enforce the ten-minute pre-authentication lifetime without a new persistence table.
- [x] Step 1 — domain/config/request primitives: implement and test the signed login envelope,
      login/session CSRF derivation, throttle digests, public HTTPS origin, CSRF signing-key ring,
      trusted-proxy CIDRs, listener settings, and bounded throttle-cardinality configuration.
- [x] Step 2 — atomic login persistence: add and test one `CompleteLogin` transaction that follows
      throttle-admission → user → session lock order, conditionally rehashes the password,
      creates/caps the session, writes audit state, and clears only pair/username throttle evidence.
- [x] Step 3 — HTTP/control-plane wiring: implement and test only `GET/POST /login`, `POST /logout`,
      protected `GET /`, `/livez`, and `/readyz`; strict routing/input/provenance/proxy validation;
      generic failures; security headers; bounded server timeouts; and graceful shutdown. Do not
      auto-run migrations or add deployment handlers.

#### Step 3 implementation plan — complete

- [x] Add a transport-only `internal/web` boundary with a narrow repository interface over the
      existing password-policy, user lookup, throttle reservation, atomic login, session
      validation, and session revocation operations; keep authentication and PostgreSQL policy in
      their current packages.
- [x] Implement exact routes only: `GET /login`, `POST /login`, `POST /logout`, protected `GET /`,
      `GET /livez`, and `GET /readyz`. Reject unsupported methods, non-empty query strings, unknown
      paths, malformed bodies, duplicate/unexpected fields, and non-canonical provenance.
- [x] Enforce the ADR 0006 browser boundary: escaped server-rendered HTML; Strict, Secure,
      HttpOnly `__Host-` cookies; login/session CSRF; exact Origin/Referer validation; generic login
      failures; fixed 303 redirects; no-store plus CSP/frame/nosniff/referrer security headers.
- [x] Resolve client identity from the direct peer unless that peer is in configured trusted-proxy
      CIDRs; validate trusted X-Forwarded-For chains right-to-left with bounded length and reject
      malformed chains without trusting attacker-supplied forwarding headers.
- [x] Wire `cmd/controlplane` to mounted database/config credentials, the durable password policy,
      the startup-built dummy verifier, the PostgreSQL pool, and the HTTP handler. Never auto-run
      migrations. Configure explicit header/read/write/idle limits and bounded graceful shutdown.
- [x] Keep `/livez` process-only and make `/readyz` return generic 503 unless the database and all
      startup-required credential/config state are ready; expose no diagnostic or secret detail.
- [x] Add focused unit and HTTP tests for routing,
      envelope reuse/expiry, cookie fixation, provenance/CSRF, malformed and oversized input,
      generic failure equivalence, throttle ordering, proxy chains, session expiry/revision/logout,
      readiness, server limits, and in-flight graceful shutdown.
- [x] Rerun the live PostgreSQL normal and race-integration suites against an ephemeral loopback
      PostgreSQL 18.4 service; all integration tests execute without skips and pass.
- [x] Validate with `gofmt`, `make check`, `go test -race ./...`, live non-skipped integration and
      race-integration suites, and `git diff --check`; then obtain independent QA and zero-context
      security/code review before marking Step 3 complete.

Approval assumptions: success redirects to `/`; login failure, logout, and unauthenticated landing
redirect to `/login`; browser POST redirects use 303; health endpoints allow GET only with short
plain-text bodies; authenticated `GET /login` redirects to `/`; startup validates snapshot
credentials and readiness probes the database without periodic credential reload; existing keyed
throttle configuration is reused with its domain-separated pair/username/IP digests. Concrete
body/header/XFF/server/shutdown limits will be conservative named constants covered by boundary
tests and constrained by existing configuration/ADR limits.
- [x] Add only login, POST logout, a minimal protected landing page, and non-sensitive health
      endpoints.
- [x] Enforce Strict `__Host-` cookies, exact HTTPS origin/provenance, CSRF, content-type and body
      bounds, fixed 303 redirects, security headers, timeouts, and graceful shutdown.
- [x] Persist replica-safe username/client and IP throttles before password verification; keep all
      authentication failures generic.
- [x] Bound throttle/audit cardinality and preserve live limits across keyed-identifier rotation.
- [x] Trust forwarded client addresses only through explicitly configured proxy CIDRs; default to
      no trusted proxies.
- [x] Keep browser middleware separate from GitHub webhook and future agent API authentication.

### Verification and handoff

- [x] ADR 0006 passed independent security review after resolving CSRF provenance, malformed proxy
      chains, throttle-key rotation/cardinality, username syntax, durable Argon2 policy, login CSRF,
      bounded audit growth, and GitHub handoff test requirements.
- [x] Authentication primitives pass unit/race/static checks and independent QA/security review;
      strict parser bounds, policy downgrade rejection, policy-aligned dummy work, origin
      canonicalization, digest separation, and exact-owner denial have persistent regression tests.
- [x] Add unit, race, PostgreSQL integration, concurrency, malformed-input, cross-owner, session,
      CSRF/origin, throttle, forwarded-address, audit, HTTP-limit, and shutdown coverage required by
      ADR 0006.
- [x] Run `make check`, `go test -race ./...`, the complete integration suite, and
      `git diff --check`.
- [x] Complete independent QA and zero-context security/code review; record findings and results.

### Authentication domain result

- Added `internal/auth` without HTTP, database, CLI, or deployment-handler coupling.
- Pinned the latest verified Go cryptography module, `golang.org/x/crypto v0.54.0`.
- Password verifiers carry their durable policy revision; future revisions fail closed and older
  valid hashes authenticate only with a rehash signal.
- Unknown-user Argon2 work is explicitly built outside the request path, bound to the exact current
  policy, and cannot return an authentication-success result.
- Untrusted username, PHC, origin, session, nonce, and CSRF inputs are size/canonicality checked
  before expensive allocation or cryptographic work.
- Independent QA passed with 88.0% statement coverage; full unit, race, vet, build, tidy-diff, and
  whitespace checks pass.

### Authentication persistence result

- Added an extension-free forward migration with permanent bootstrap state, exact owner-scoped
  composite foreign keys, digest-only sessions, append-only audit, durable global throttle
  cardinality, and bounded cleanup indexes.
- Added typed transactions for concurrent singleton bootstrap, policy/user loading, session
  creation/validation/logout/cap/cleanup, password reset, throttle reservation/finalization, and
  bounded throttle cleanup.
- New password verifiers must exactly match the durable Argon2 policy; future/stale authority and
  reset/login races fail closed under explicit user-then-session locking.
- Pair/IP throttles use fixed 15-minute windows. Username-wide exponential state is separate and
  durable through 30/60/120/240/480/900-second stages, retained-key migration, and shared overflow.
- PostgreSQL admission serialization and one durable replica configuration bound combined ordinary
  and overflow throttle rows even when cleanup is delayed.
- Real PostgreSQL 18.4 integration and race-integration suites pass, including concurrency,
  rollback, ownership, expiry/touch, exact digest, audit, cardinality, overflow, rotation, and
  cleanup regressions. Independent QA and security review report no medium-or-higher findings.

### Operator and configuration result

- Production database credentials now load only from `AUTODEPLOY_DATABASE_URL_FILE`; the legacy
  credential-bearing environment value is rejected. An explicit repository root prevents storing
  mounted credentials in the workspace.
- Darwin/Linux loaders traverse descriptors without following links, verify trusted directories
  and the opened regular file, bound reads, and compare pre/post inode and metadata so partial
  in-place replacement fails closed. Unsupported platforms compile but cannot load credentials.
- Exact raw 32-byte active/retained username-throttle keys are mounted separately; key bytes never
  enter environment configuration. Domain-separated digests clear every retained username alias.
- Migration `000003` removes un-attributable pre-web pair rows, adds nullable known-user recovery
  ownership, and preserves IP, invalid-forward, username, and shared-overflow evidence. Reset and
  reservation share throttle-admission → user → session lock order.
- `admin bootstrap` calibrates the bounded Argon2id policy before prompting and persists it through
  the permanent singleton bootstrap transaction. `admin reset-password` uses the durable policy,
  revokes sessions, increments authority, clears account recovery state, and audits atomically.
- Terminal preflight occurs before credential loading, database access, migrations, or calibration;
  passwords are read twice from one verified `/dev/tty` session and never accepted from argv,
  environment values, configuration files, or piped stdin.
- Unit/race/static/format/tidy checks, Linux/Windows builds, and fresh PostgreSQL 18.4 normal and
  race-integration suites pass. Independent QA and zero-context security review report no
  medium-or-higher findings.

### Web boundary Step 1 result

- Added a bounded, canonical, ten-minute login envelope authenticated over the envelope version,
  key-version selector, issued-at timestamp, 32-byte nonce, and canonical HTTPS origin; retained
  keys validate old envelopes and selector rewriting fails even during identical-key overlap.
- Added exact 32-byte mounted CSRF key-ring loading plus validated public origin, trusted proxy
  CIDRs, listener address, and global authentication-throttle row cap configuration.
- Added domain-separated, length-prefixed keyed throttle digests for later request identity
  accounting. Focused unit/race tests, vet, compile-only full-module checks, formatting, and
  whitespace validation pass.

### Web boundary Step 2 result

- Added one `CompleteLogin` transaction that re-acquires throttle admission before user/session
  locks, revalidates authority and durable password policy, applies only a required rehash, creates
  and caps digest-only sessions, audits mutations, and selectively clears pair/username evidence.
- Successful authority minting requires the previously reserved known-user pair, username, and IP
  identities. IP, invalid-forward, overflow, unrelated-user, and unrelated recovery state remain.
- Focused unit/race checks and six non-skipped live PostgreSQL integration tests pass, covering
  lock ordering, reset races, selective clearing, audit rollback, rehash, session cap, and the
  successful-login throttle input contract.

### Web boundary Step 3 result

- Added a transport-only HTTP boundary for exact login/logout/protected landing and health routes,
  with generic failures, strict forms/provenance/CSRF/cookies, security headers, and bounded input.
- Trusted forwarding defaults off; configured chains are canonicalized and walked right-to-left.
  Ambiguous/malformed chains fail closed into proxy-scoped, rotation-safe durable throttle state.
- The control plane now loads mounted credentials and durable policy, constructs the PostgreSQL
  repository and dummy verifier, configures explicit HTTP limits, and performs bounded graceful
  shutdown without running migrations.
- Full unit/race/static/build checks and repeated security regressions pass. Independent QA and
  zero-context security review return PASS with no findings. Verbose PostgreSQL 18.4 normal and
  race-integration suites execute without skips and pass against an ephemeral loopback service.

## Secret handling slice — complete

Scope: complete the next implementation-sequence item as three sequential production substeps.
Do not add GitHub, agent, Docker, routing, or UI behavior in this slice.

### Proposed decisions

- External KEK configuration uses one protected non-secret manifest containing the active key ID
  and active/retiring key-file paths; each key file contains exactly one 32-byte AES-256 key.
- Secret plaintext is bounded to 64 KiB. Empty values are valid because an explicitly empty runtime
  or build value differs from an absent variable.
- V1 purposes are `build`, `runtime`, and `provider`; immutable version lifecycle states are
  `active`, `superseded`, and `revoked`. Ownership and opaque project/environment identifiers are
  retained even before those domain tables exist.
- Redaction uses the constant `[REDACTED]`, covers raw values plus standard padded/unpadded base64
  encodings generated by AutoDeploy, and is bounded to 4,096 active patterns and 8 MiB total raw
  pattern bytes per operation.
- Rotation is an explicit operator command using stable keyset pagination in batches of 100. It is
  restartable without a durable job table; CAS conflicts and failed attempts safely consume wrap
  reservations. Old key removal remains external and is refused while live rows reference it.
- Existing startup remains backward-compatible: KEK configuration is required only when the secret
  subsystem is enabled. The local generation binding detects only a DB/manifest mismatch; it cannot
  detect both being rolled back together. Production restore therefore requires an operator to install
  a new active KEK ID; restored keys are decrypt-only. Automatic rollback detection requires a
  separately approved non-rollbackable external authority.

### Step 1 — crypto, key provider, and redaction primitives

- [x] Add typed AAD, envelope, suite/version, key-provider, and streaming-redactor boundaries under
      `internal/crypto`; use fresh DEKs, AES-256-GCM random nonces, domain-separated exact
      length-prefixed AAD, and sanitized errors with no secret-bearing fields.
- [x] Extend the hardened mounted-file credential boundary and config parsing for the manifest/key
      ring; reject unsafe files, duplicate IDs/paths, invalid states, replacement races, and
      malformed or oversized material.
- [x] Test known-answer and randomized round trips, every AAD substitution, wrong/unknown keys,
      malformed envelopes, suite/version rejection, error sanitization, and chunk-safe redaction at
      every split point, including initialization/processing/downstream-write failure suppression.

#### Step 1 result

- Added mandatory exact-KEK pre-wrap reservation, independently fixed AAD vectors, bounded envelope
  parsing, caller-owned key copies, best-effort key clearing, and fail-closed mounted manifest/key
  loading with canonical unique paths and unsupported-platform stubs.
- Added a compact stateful multi-pattern redactor with combined raw/encoded pattern, node, memory,
  candidate-ring, and overflow-safe work bounds. Replacement output is safety-checked before release
  so marker/context synthesis, short writes, and nested-prefix amplification fail closed.
- Full format/vet/unit/build/race checks and Windows config/credential cross-compilation pass.
  Independent QA returns PASS; zero-context security review returns PASS with its sole arithmetic
  hardening note resolved and regression-tested.

### Step 2 — durable ledger, immutable secrets, and audit

- [x] Add forward-only migration `000004` for the per-KEK wrap-invocation ledger, logical secret
      heads, immutable versions, and append-only sanitized audit events; PostgreSQL must not become
      authoritative for active/retiring KEK state.
- [x] Add typed PostgreSQL repositories and a secret service. Authorization precedes decryption;
      wrap reservations commit independently before wrapping; secret mutation and audit commit
      atomically; corruption, unknown key, AAD mismatch, and AEAD failure share one sanitized class.
- [x] Test the `2^31` invocation boundary, consumed reservations after failed writes, concurrent
      replicas, owner/scope isolation, immutable ciphertext, mutation/audit rollback, dump/DTO
      plaintext exclusion, malformed rows, and migration checksum/idempotency behavior.

#### Step 2 result

- Added durable independently committed per-KEK wrap reservations with a hard `2^31` ceiling,
  immutable secret heads/versions, CAS-only wrapped-DEK rewrap fields, and append-only sanitized
  audit records with exact actor/service and deployment/lease correlation.
- Exact version/reference authorization precedes retrieval and decryption. Frozen superseded
  references can retry; new superseded or revoked access fails closed. Successful, denied, and
  failed materialization outcomes are audited without exposing envelope or plaintext material.
- Added operation-specific create/replace validation, coherent head/version/audit constraints,
  metadata-only service results, sanitized error boundaries, immutable head scope, concurrent CAS,
  and exact affected-row enforcement.
- Full unit/race/static/build checks pass. Fresh PostgreSQL 18.4 normal and race integration suites
  pass, including seven service-to-repository tests for materialization, audit failure suppression,
  rewrap invariants, concurrent replacement, durable reservation consumption, tamper rejection,
  and persisted timestamps. Independent QA and zero-context security review return PASS.

### Step 3 — rotation and production wiring

- [x] Add restartable bounded rewrap service and operator command. Rewrap changes only wrapped DEK,
      KEK ID, and CAS revision; compromise recovery creates a wholly new immutable secret version.
- [x] Wire opt-in startup/readiness validation and centralized control-plane redaction boundaries;
      agent-side and persisted-log integration remain for their later approved slices.
- [x] Test mixed-key reads, interrupted/idempotent batches, CAS conflicts, unchanged value
      ciphertext/version, retirement guards, restore fail-closed behavior, and compromise recovery.
- [x] Run formatting/static/unit/race checks, verbose non-skipped PostgreSQL 18.4 normal/race
      integration, migration drift checks, `git diff --check`, and independent QA/security review
      after each sequential step.

#### Step 3 result

- Added restartable keyset-paginated rewrap batches capped at 100. CAS conflicts stop before the
  failed row so retry cannot skip old-key references; every attempted wrap consumes an independently
  committed active-key reservation. Revoked versions are never rewrapped but still block retirement.
- Added terminal-only `secret init-active-kek`, `secret rewrap`, and `secret can-retire` operations
  with credential/DB preflight, no migration execution, fixed sanitized output, active-key
  retirement refusal, and explicit generation-bound ledger initialization that never resets an ID.
- Secret-enabled control-plane startup and every readiness probe reload the protected manifest and
  verify active-key ledger/generation binding. The default-disabled path remains backward-compatible;
  the HTTP server error logger uses the centralized redacting boundary.
- Compromise recovery creates and audits a new immutable version, revokes the predecessor, and never
  rewraps the compromised version. Fresh PostgreSQL 18.4 race integration verifies schema triggers,
  rotation pagination/count, reservation ceiling, compromise handling, and all prior persistence.
- Full format/vet/unit/build/race checks pass. Independent QA and zero-context security review pass
  the local implementation. Generation binding detects DB/manifest mismatch only; joint rollback of
  both cannot be detected locally. Restore procedure therefore requires a fresh active KEK ID.
  Automatic rollback detection remains a separate decision requiring a non-rollbackable authority.

### Risks and mitigations

- Nonce/key reuse: durable pre-wrap reservations, hard invocation ceiling, fresh DEKs, and restored
  databases requiring a new active KEK ID.
- Plaintext leakage: secret-free error/DTO/log types, streaming fail-closed redaction, dump scans,
  and dual agent/control-plane redaction at their respective integration slices.
- Rotation interruption or key loss: immutable ciphertext, CAS/keyset batches, decrypt-only retiring
  keys, usage checks before removal, and operator-controlled offline recovery copies.
- Premature domain coupling: constrained opaque ownership/project/environment IDs now; add foreign
  keys only when those domain aggregates exist.

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

## Proposed GitHub Webhook and Durable Enqueue Slice

Status: complete. This slice follows ADR 0002 and ADR 0004. It does not return a
successful webhook response until verified receipt, deployment creation, and logical job enqueue
have committed together. Agent authentication, claims, leases, attempts, fencing, retry recovery,
and checkout-token delivery remain the next slice.

### Approval assumptions

- Add the smallest real deployment-target authority now: extend the active owner/project GitHub
  binding with one canonical full branch ref, environment ID, and server ID. These remain
  constrained opaque identifiers until the project/environment/server aggregates are implemented;
  no local-only or placeholder target resolver is allowed.
- Handle `push`, `installation`, and `installation_repositories` deliveries in this slice. Pushes
  may enqueue work; lifecycle deliveries only apply verified, fail-closed authorization changes.
- Continue terminating public TLS at the trusted reverse proxy. The webhook route accepts only a
  request whose connection/validated trusted-proxy metadata proves the original HTTPS scheme; it
  never trusts forwarding headers from an untrusted peer.
- Load an active webhook secret plus optional retained overlap secret from protected mounted files.
  Accept either during bounded operator-controlled rotation, identify which version verified the
  delivery without persisting secret material, and do not automate retirement in this slice.
- Store only bounded normalized identifiers, digests, states, and sanitized failure classes. Never
  persist or log the raw payload, signature, secret, token, sender object, commit messages, or other
  unrestricted GitHub content.

### Step 1 — target authority, webhook domain, and queue domain

- [x] Amend the accepted ADRs only where needed to place delivery/intent rows in the global lock
      hierarchy and document the selected binding-target and trusted-proxy HTTPS contracts.
- [x] Add strict typed webhook header, digest, normalized push/lifecycle event, verification-state,
      processing-state, and sanitized failure models. Require positive numeric stable IDs, a
      canonical `refs/heads/<branch>` ref, and a lowercase 40-hex nonzero commit SHA for pushes.
- [x] Add a typed immutable logical deployment-execution job model with schema version, owner,
      project, environment, server, deployment, repository, ref, commit, bounded priority,
      availability, idempotency key, canonical payload digest, and queued state. It must contain no
      credentials, raw JSON, arbitrary command, URL, or path supplied by the webhook.
- [x] Extend repository binding creation/update so administrators configure one full ref,
      environment ID, and server ID; preserve existing production behavior by leaving existing
      rows inactive for automatic deploy until all target fields are explicitly configured.

#### Step 1 result

- Added strict, credential-free normalized webhook values and immutable deployment-execution job
  values with focused validation tests; HTTP ingress, signature handling, and persistence remain
  deferred.
- Binding target authority now requires a canonical branch ref and opaque environment/server IDs
  on new or updated bindings. Legacy rows intentionally retain null target fields and cannot
  authorize automatic deployment when Step 2 persistence is introduced.
- Migration `000009_github_binding_targets.sql` is Step 1 target-only authority. Migration `000010`
  is reserved for the Step 2 webhook-delivery, intent, job, and event persistence transaction.

### Step 2 — forward-only schema and transaction-aware persistence

- [x] Binding target authority is complete in Step 1 migration
      `000009_github_binding_targets.sql`. Add forward-only migration `000010` for durable webhook
      deliveries, automatic deployment intents, logical agent jobs, and append-only job events.
- [x] Enforce unique `(app_registration_id, delivery_id)`, independent per-App verified body-digest
      replay protection, and unique automatic intent `(binding identity, repository ID, full ref,
      commit SHA)`. Same-key/same-digest is idempotent; any scope/digest mismatch is an integrity
      conflict with no state change.
- [x] Enforce immutable typed job scope/payload digest, `UNIQUE (deployment_id, job_kind)`, legal
      queued shape, bounded priority/timestamps, useful runnable/idempotency indexes, and append-only
      initial enqueue events. Lease/attempt/slot tables remain deferred.
- [x] Introduce a narrow caller-owned PostgreSQL Unit of Work. Add transaction-aware deployment
      creation while preserving the existing standalone repository API, and explicitly commit or
      roll back every transaction.
- [x] In one short `READ COMMITTED` transaction, lock/revalidate App registration, installation,
      repository, project binding, and target authority in deterministic order; resolve delivery
      and automatic-intent idempotency; create the queued deployment/event and job/event; then mark
      the normalized delivery processed before commit. Perform no GitHub/network call in this
      transaction.

#### Step 2 result

- Added migration `000010_create_webhook_jobs.sql` with normalized terminal receipts, independent
  verified-body replay protection, immutable automatic intents, queued deployment-execution jobs,
  append-only enqueue events, canonical source/timestamp constraints, and runnable/idempotency
  indexes. Claims, leases, attempts, environment slots, fences, and retries remain deferred.
- Added one `READ COMMITTED` enqueue unit of work with deterministic App/installation/repository/
  binding `FOR SHARE` authorization locks, exact immutable replay comparison, sanitized collision
  handling, caller-owned deployment creation, and explicit rollback/commit. Successful lifecycle
  receipt plus authority mutation remains an atomic Step 3 operation; only terminal pre-parse-safe
  rejection receipts can commit independently.
- Focused unit/race/vet/diff checks and integration compilation pass. Live PostgreSQL 18 normal and
  race runs pass all focused webhook repository and migration tests, including exact/digest/intent
  replay, concurrent distinct deliveries, every write-stage rollback, terminal rejection storage,
  named schema constraints, immutable receipts/jobs/events, and authorization-lock fencing.

### Step 3 — raw ingress, credential loading, lifecycle effects, and wiring

- [x] Add a dedicated webhook handler outside browser session/CSRF middleware. Require one canonical
      delivery header, one supported event header, one `sha256=` signature header, an exact path and
      POST method, trusted HTTPS provenance, and a bounded body read exactly once.
- [x] Verify HMAC-SHA256 over the unmodified bytes with constant-time comparison before JSON parsing
      or database work. Support active/retained mounted-secret overlap using the existing hardened
      credential-file boundary and sanitized configuration errors.
- [x] Strictly parse only the normalized fields required for authorization. Reject unsupported
      actions/events, deletion/zero SHA pushes, malformed/duplicate headers, trailing JSON, unknown
      authority, wrong ref, suspended/removed repositories, and inconsistent numeric identities.
- [x] Apply verified installation suspension/deletion and repository removal fail-closed by
      revisioning/disabling the matching authority and bindings transactionally. Additions and
      unsuspension refresh known authority only; they never create or reactivate a project binding.
- [x] Compose the webhook route with the browser handler in the control-plane entrypoint, preserve
      default-disabled GitHub behavior, readiness, security headers, timeouts, and fixed sanitized
      process logging.

### Files expected

- Step 1 complete: `internal/github/webhook.go`, webhook tests, `internal/queue/job.go`, queue
  tests, binding-target store/UI tests, and `migrations/000009_github_binding_targets.sql`.
- Step 2 create: `internal/store/postgres/webhook_job_repository.go`, repository/integration
  tests, and `migrations/000010_create_webhook_jobs.sql`.
- Modify: ADR 0002/0004 as narrowly required, GitHub binding domain/store/UI tests,
  `internal/deployment` persistence transaction seam, `internal/config/database.go`,
  `cmd/controlplane/main.go`, migration tests, `MEMORY.md`, `tasks/todo.md`, and
  `tasks/lessons.md` if implementation reveals a reusable correction.
- Avoid modifying `internal/web/web.go` for webhook authentication; it remains the browser-only
  boundary except for the minimal binding-target form fields required above.

### Edge cases and validation

- [x] Unit-test missing/duplicate/malformed headers, signature algorithm/hex/length mismatch,
      oversized/short-read bodies, active/retained secret overlap, constant-time verification, and
      proof that parsing/persistence never runs before verification.
- [x] Test malformed JSON/trailing values, unsupported lifecycle actions, nonpositive stable IDs,
      wrong installation/repository/owner scope, wrong ref, deletion SHA, uppercase/non-40-hex SHA,
      delayed/out-of-order pushes, and sanitized error/log output.
- [x] PostgreSQL integration-test concurrent exact redelivery, delivery-ID digest conflict,
      same-body/new-ID replay, distinct deliveries for the same automatic intent, authority revision
      races, and injected failure at every receipt/deployment/event/job/event stage to prove full
      rollback.
- [x] Test append-only/immutable constraints, migration checksum/idempotency, runnable indexes, and
      that persisted rows/dumps contain no raw body, signature, secret, token, or unrestricted JSON.
- [x] Test lifecycle disable/removal races and verify additions/unsuspension cannot silently
      authorize deployment.
- [x] Run `gofmt`, `go vet ./...`, `go test ./...`, `go test -race ./...`, Linux cross-builds where
      platform-tagged code is affected, fresh PostgreSQL 18 integration tests with `-count=1`,
      `git diff --check`, independent QA, and zero-context security review.

#### Step 3 result

- Added an isolated exact-path webhook boundary with trusted HTTPS provenance, bounded single-read
  bodies, constant-time active/retained HMAC verification before parsing or persistence, strict
  normalized push/lifecycle parsing, fixed public errors, and mounted-secret startup validation.
- Push authorization, canonical time, deterministic deployment/job construction, durable receipt,
  deployment/event, logical job/event, and processed state now commit in one `READ COMMITTED`
  transaction. Installation and multi-repository removal effects lock authority deterministically,
  disable fail-closed, never reactivate bindings, and persist a sealed immutable repository set.
- Control-plane wiring remains opt-in with GitHub disabled by default. Fresh PostgreSQL 18 normal
  and race integration, migration tests, repository-wide unit/race/vet/diff checks, independent QA,
  and zero-context security review pass. Claims, leases, attempts, fencing, retries, agent transport,
  and checkout-token delivery remain the next queue/agent slice.

### Documentation handoff result

- [x] Added README.md and docs/IMPLEMENTATION_STATUS.md as the compact authoritative handoff for
      continuing implementation: current scope/branch/worktree, 7/18 progress estimate, trust
      boundaries, completed invariants, migrations/configuration categories, validation evidence
      and limits, next server-agent lease slice, risks, resume commands, git discipline, and
      definition of done. .tmp/ is untracked but not gitignored, so it must be manually excluded
      from commits; never use blanket git add.

### Risks and mitigations

- Premature target schema: use constrained opaque IDs on the binding only until real aggregates
  exist; keep automatic deployment disabled for legacy/incomplete bindings.
- Replay or split-brain idempotency: use independent delivery and body-digest constraints plus one
  caller-owned transaction and deterministic lock order.
- Secret exposure: mounted-file-only secret loading, bounded in-memory raw bytes, fixed public
  errors, centralized redaction, and no raw payload persistence.
- Proxy spoofing: accept forwarded scheme only from configured trusted proxy CIDRs and require one
  canonical value; direct untrusted peers cannot assert HTTPS.
- Queue overreach: implement immutable enqueue state/events only; claims, attempts, leases, slots,
  fencing, retries, and agent transport remain explicitly deferred.

## Proposed Server Agent, Credentials, and Leases Slice

Status: **approved 2026-09-16** (bearer+digest auth; break-glass included; opt-in routes). Constrained by ADR 0001, 0003, 0004.
No Docker/BuildKit, Traefik, checkout invocation, or release activation in this slice.

### Understanding
- Add durable server identity, scoped agent credentials, heartbeat, and the full lease lifecycle
  (claim, renew, complete, cancel, expire, reconcile) over existing `agent_jobs` from 000010.
- Agents talk only to the control plane over authenticated outbound HTTPS; never PostgreSQL or App keys.
- Deployment aggregate moves only `queued -> assigned` (first claim), `-> cancelled`, `-> failed`.
  Success does not advance the deployment; the execution slice owns `fetching` onward.

### Decisions (recommended; owner-reviewable)
1. **Transport**: HTTPS/1.1 JSON polling under `/agent/v1/` on the existing public origin, mounted
   like the webhook route (outside session/CSRF, same trusted-HTTPS provenance). No gRPC/streaming.
   Rejected: gRPC (second listener, HTTP/2 via Traefik, proto tooling); mTLS (new PKI subsystem).
2. **Credential**: opaque bearer `srv.v1.<32 random bytes base64url>`; store SHA-256 digest + version
   only. Rejected in draft review: HMAC signing keyed by the raw credential is unverifiable when only
   the digest is stored, and keying by the digest makes a DB read equal impersonation. Replay safety
   comes from the protocol itself: fence + attempt + monotonic renewal sequence + completion
   operation-id/digest idempotency. No nonce table.
3. **Credential binding**: attempts bind to `server_id`; every request requires the *currently active*
   credential. Revocation stops all mutation immediately; rotation (revoke+issue in one tx) does not
   strand leased work. Server revocation stops everything for that server.
4. **Enrollment**: `cmd/admin server register` creates the server (system-generated opaque ID) and
   prints a single-use enrollment token (`enr.` prefix, 15 min TTL) once. `cmd/agent enroll`
   exchanges it for the durable credential, written by the agent to a 0600 file. Distinct prefixes
   prevent cross-use. Shared installation secrets rejected.
5. **Agent-local fence state**: per-environment file (write temp, fsync, rename):
   highest accepted fence, renewal sequence, reconciliation flag. Missing/corrupt => fail closed,
   claim nothing, require reconcile. Never reset locally.
6. **Reconciliation (ADR 0004 §fencing)**: expiry/cancel-deadline of a leased attempt increments the
   environment fence and sets `reconciliation_required`. Claim refuses any environment that is
   occupied or requires reconciliation. Only `POST /agent/v1/environments/{id}/reconcile` from the
   owning server, carrying the closed attempt/fence and sanitized outcome, clears it (idempotent).
   Owner break-glass `cmd/admin environment mark-reconciled` with mandatory audit reason: see owner
   decisions.
7. **Sweeper**: control-plane goroutine, 15 s tick, `pg_try_advisory_xact_lock`, bounded batch of 100,
   one short tx per expired attempt. Claim/renew/complete also check authority deadlines inline.
8. **Retry**: exactly ADR 0004 (full jitter `uniform(0, min(5m, 5s*2^(n-1)))`, 5 attempts, fixed
   non-retryable classes). Closed failure-class enum; unknown classes are non-retryable.
9. **Events**: widen `agent_job_events` vocabulary (claimed, cancel_requested, cancelled, expired,
   retry_scheduled, succeeded, failed, dead, reconciled). Renewals update the attempt row only; no
   per-renewal event. New append-only `server_audit_events` for register/enroll/rotate/revoke/
   heartbeat-rejection/break-glass. No raw credential, digest, or token in any event or log.
10. **No-op executor rejected**: `cmd/agent` in this slice never reports a fabricated success.
    Heartbeat reports `executor_available=false`; claim returns nothing to such agents. Protocol
    end-to-end is exercised by integration tests with a test executor only.
11. **Existing rows**: `agent_jobs.server_id` has no FK (bindings predate `servers`). Jobs for unknown
    servers remain queued and are simply unclaimable; attempts FK to `servers`.

### Steps (one subagent each; peer review before next)
**Step 1 — server domain + migration 000012**
- `internal/server`: `Server`, `CredentialToken`/`EnrollmentToken` parse/digest (mirror
  `auth.SessionToken`), validation.
- 000012: `servers` (immutable id/owner/created_at; status active|revoked; `last_heartbeat_at`),
  `server_credentials` (version, digest 32 bytes, one-active partial unique index, only
  active->revoked update), `server_enrollment_tokens` (digest, issued_at, expires_at, consumed_at,
  single use), `server_audit_events` (append-only triggers).
- Postgres repository: register, issue enrollment, enroll (consume + credential v1 atomically),
  rotate, revoke, authenticate (FOR SHARE server + active credential, constant-time digest compare).
- Tests: token cross-prefix confusion, expired/consumed/replayed enrollment, concurrent double enroll
  (one winner), rotation atomicity, revoked server/credential rejection, audit rollback.

Step 1 result (2026-09-17): done. Admin methods take `auth.Principal`, re-lock live admin session/user
(parity with GitHub repository) and scope server locks by owner; Enroll/Authenticate compare against a
dummy digest on not-found paths (residual audit round-trip timing accepted). Evidence: make check, vet
(incl. integration tag), go test -race, three fresh PG18 full integration runs normal and -race,
concurrency/cross-owner tests -race x20; correctness review PASS WITH NOTES (fixed), security review
FAIL (owner scope) -> fixed -> re-review PASS.
Step 6 open question: server administration needs a live admin session, so `cmd/admin server ...` cannot
call these methods directly; decide web-admin routes vs a CLI operator path before Step 6.

**Step 2 — lease schema migration 000013**
- Replace `agent_jobs_no_update` with a transition-validating trigger; widen state CHECK to
  queued|leased|retry_wait|succeeded|failed|cancelled|dead; add attempt_count, cancel_requested_at.
- `environment_slots` (fence BIGINT, never-decrease trigger, current attempt, reconciliation_required).
- `agent_job_attempts` (job, attempt_number, server, environment, fence, state, renewal_sequence,
  lease/authority deadlines, result operation id + digest, closed_at; legal transitions only).
- `frozen_secret_versions` (attempt, secret version; append-only; empty until execution slice).
- Widen `agent_job_events`; claimable and authority-expiry partial indexes per ADR 0004.
- Tests: existing queued/enqueued rows remain valid; every illegal SQL transition rejected;
  fence decrease rejected; one occupant per slot.

Step 2 result (2026-09-17): done (migration 000013 + 13 schema tests). Claim ordering fixed by insert
trigger: bump slot fence -> job leased with attempt_count+1 -> insert attempt (fence = slot fence,
attempt_number = attempt_count, server/environment = job). attempt_count counts started attempts, so
expiry must NOT increment it again. Closure shapes: leased/expired none; succeeded result; failed
class+result; cancelled result optional. Runnable index matches ADR incl. created_at. Evidence: make check,
vet (incl. integration), go test -race, three fresh PG18 full runs normal and -race; review NEEDS CHANGES
(closure CHECK, fence tie, index, cancel deadline) -> fixed -> re-review PASS.
Step 4 carry-overs: set job cancel_requested_at and attempt cancel_deadline_at in the same tx (trigger reads
job unlocked; add repository test); credential/server revocation must advance fence and mark
reconciliation_required for in-flight environments (ADR 0004 revocation paragraph).

**Step 3 — claim and renew repository**
- Lock order per ADR 0004: server/credential (SHARE) -> environment slot (UPDATE) -> job
  (UPDATE SKIP LOCKED) -> attempt. Revalidate after locks; never act on discovery snapshot.
- Claim: slot must be free and not reconciling; increment fence; attempt with 90 s deadlines;
  job -> leased; deployment queued->assigned on attempt 1 only; event.
- Renew: same sequence => return stored deadlines; sequence+1 => extend to min(now+90s, cancel cap);
  otherwise reject. Returns cancel_requested.

Step 3 result (2026-09-17): done. Claim: non-locking pre-auth -> discovery -> slot rows pre-created in one
sorted committed statement (avoids opposite-order insert deadlock) -> tx: FOR SHARE re-auth, slot and job
FOR UPDATE SKIP LOCKED (priority order; skip never waits), revalidate job incl. environment/server,
fence+1, job leased, attempt, slot occupant, deployment assigned on attempt 1, claimed event. limit<=0 is a
no-op. Renew: auth -> slot -> job -> attempt; requires occupant == attempt and fence match; expired
authority fails even for replayed sequence; cancel cap applies to both deadlines. No per-renew event
(Decision 9; amend ADR 0004 wording in Step 6 docs). Evidence: make check, vet, race, three fresh full
PG18 runs normal + -race, claim tests -race x20; reviews -> deadlock, occupant check, pre-auth, limit fixes.

**Step 4 — complete, cancel, expire, reconcile repository + sweeper**
- Complete: first call closes attempt, frees slot if still owned, job -> succeeded|retry_wait|failed|
  dead; identical replay returns stored result; mismatched digest => integrity error, no mutation.
- Cancel: queued/retry_wait terminal immediately; leased sets cancel_requested and caps deadlines at
  now+60 s; ack closes as cancelled. Rejected once deployment is `activating` (domain rule).
- Expire: close attempt expired/cancelled, fence++, reconciliation_required, schedule retry/dead.
- Reconcile: owning server + matching closed attempt/fence clears flag; idempotent.
- Sweeper with advisory lock and bounded batch; graceful shutdown.

Step 4 result (2026-09-17): done. Complete/AcknowledgeCancellation replay-safe (exact stored result
match, even after expiry; mismatch integrity error; expiry-closed attempts stale). Retry full jitter from
DB time, dead at attempt_count 5, expiry never increments. RequestCancellation owner-scoped, same-tx
cancel marker + deadline cap, rejects activating under deployment row lock. ExpireBatch SKIP LOCKED;
sweeper uses pg_try_advisory_xact_lock. Reconcile: owning server + attempt fence (not advanced slot fence);
replay detected via durable reconciled event. MarkReconciled derives owner from pending attempt's job.
Only RevokeServer (not rotation) closes in-flight attempts (Decision 3). Evidence: make check, vet, race,
three fresh full PG18 runs normal + -race, concurrency tests -race x20; review PASS.
Follow-ups: persist reconcile outcome digest when a consumer exists (needs migration); a revoked server
cannot reconcile, so its environments need MarkReconciled; amend ADR 0004 (no per-renew event) in Step 6.

**Step 5 — HTTP transport**
- `internal/agent/http`: enroll, heartbeat, claim, renew, complete, cancel-ack, reconcile. Strict
  JSON (`DisallowUnknownFields`), bounded bodies, fixed-schema capability struct (<= 1 KiB), one
  generic auth failure response, authorization re-derived from locked rows inside each tx.

Step 5 result (2026-09-17): done. internal/agent/http (package agenthttp), POST-only /agent/v1/{enroll,
heartbeat,jobs/claim,leases/renew,leases/complete,leases/cancel-ack,environments/reconcile}; webhook-identical
HTTPS provenance; strict bounded JSON; Bearer srv.v1 + X-AutoDeploy-Server-ID; repositories re-validate in
tx. Enrollment: agent generates credential locally, sends digest only. Claim gated on executor_available.
Errors: 401 generic, 409 stale_lease/integrity_conflict, 400, 413, 503; nothing echoed. Security fix:
unauthenticated Enroll/Heartbeat rejections write no audit rows (write amplification). Integration target
and CI now run -p 1 ./internal/store/postgres ./internal/agent/http ./migrations. Evidence: make check, vet,
race, agent HTTP alone on fresh DB, three fresh full runs normal + -race; security review PASS WITH NOTES.
Follow-up: no rate limiting on /agent/v1 (low; credential required).

**Step 6 — CLI, wiring, agent client**
- `cmd/admin`: server register/rotate/revoke, environment mark-reconciled (if approved).
- `cmd/controlplane`: `AUTODEPLOY_AGENT_ENABLED` opt-in, mount routes, start sweeper.
- `cmd/agent`: enroll, credential file load, fence-state files, heartbeat, renew loop; claims only
  when an executor is registered (none in production this slice).
- Docs: IMPLEMENTATION_STATUS, ADR 0004 amendment only if needed, MEMORY.

### Must-prove tests (live PostgreSQL 18, normal and -race, three fresh full runs)
1. Simultaneous claims: one winner per job and per environment slot.
2. Agent replay: duplicate renew/complete idempotent; mismatched digest rejected without mutation.
3. Revoked credential or server after claim: renew/complete/reconcile rejected.
4. Clock uncertainty: deadlines only from PostgreSQL time; boundary accept/reject deterministic.
5. Lost completion response: identical retry through HTTP returns original result.
6. Expired lease with late success: stale fence completion rejected; slot stays reconciling.
7. Cancellation vs activation: cancel after `activating` rejected; cancel during lease caps deadline.
8. Lost agent fence state: agent fails closed; lower fence rejected client-side.
9. No cross-server/environment mutation: wrong server or substituted IDs rejected at auth lock step.
10. Renewal sequence gaps/out-of-order handled exactly as specified.
11. Concurrent sweepers: one processes each attempt.
12. Reconciliation gate: no claim on a reconciling environment until owning server reconciles.
13. No credential/token/digest in logs, events, errors, or job payloads.

### Owner decisions (defaults assumed unless changed)
- Break-glass admin `mark-reconciled` command: **include** (audited, reason required).
- Agent routes: **opt-in** `AUTODEPLOY_AGENT_ENABLED`, default off.
- Heartbeat 20 s; capability fields: agent version, OS/arch, CPU count, memory, disk free,
  Docker/BuildKit versions (reported as absent until execution slice).

### Risks and mitigations
- New public trust boundary: isolated route, generic errors, locked-row authorization, security review.
- Relaxing 000010 immutability triggers: replaced by transition-validating triggers + SQL tests.
- Environment wedged by reconciliation: agent reconcile path + audited break-glass.
- Scope size: six sequential steps, each reviewed and verified before the next.

### Rollback
- Forward-only migrations; defects fixed by new migrations. Every repository method commits once
  with deferred rollback; injected-failure tests prove no partial writes. Feature off by default.

### Definition of done
- Per docs/IMPLEMENTATION_STATUS.md next-slice definition; all must-prove tests pass fresh three
  times normal and -race; make check, go test -race ./..., Linux cross-build, git diff --check,
  independent QA, zero-context security review; docs updated to 8/18.

### Resume notes (2026-09-17)
- Committed: b015ea5 webhook slice, 3766be5 migration 000011 clone URL CHECK fix, 3b92c22 secret race test fix.
- Step 1 finished by subagent (internal/server, migration 000012, server_repository.go + 11 integration
  tests, ErrServerAuthorizationDenied); uncommitted; subagent reports all checks green, NOT yet independently
  verified. Tomorrow: inspect `git status`; rerun gofmt, vet
  (incl. -tags=integration), `go test -race ./...`, full fresh PG18 integration normal + -race; then
  independent review before Step 2.
- Integration DB: throwaway `postgres:18` container via Docker Desktop, AUTODEPLOY_TEST_DATABASE_URL.
