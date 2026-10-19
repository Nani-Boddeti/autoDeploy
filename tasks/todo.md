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

Status: **complete 2026-09-17** (approved 2026-09-16; bearer+digest auth; break-glass included; opt-in routes). Constrained by ADR 0001, 0003, 0004. Committed 89d01b7..8105a85; docs/ADR amendments done in Step 6d.
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

Step 6 owner decision (2026-09-17): server administration (register + one-time enrollment token, rotate,
revoke, mark-reconciled) lives in session+CSRF-protected web admin routes in internal/web, not cmd/admin.
Sub-steps: 6a controlplane wiring/config (agent routes opt-in, sweeper); 6b web server admin pages;
6c cmd/agent client (enroll, credential file, fence state, heartbeat, no executor); 6d docs/ADR amendments.

Step 6a result (2026-09-17): done. AUTODEPLOY_AGENT_ENABLED (strict, default off) and
AUTODEPLOY_AGENT_SWEEP_INTERVAL (15s default, 5s..5m); /agent/v1/ prefix routed before browser handler with
webhook trusted proxies; disabled path unchanged; sweeper runs only when enabled, logs a fixed
"agent lease sweep failed" line (no error text), and is stopped before the pool closes on every exit path.
Review: shutdown race on listener failure fixed. 6b styling decision: plain HTML like existing pages,
strict CSP unchanged; design system deferred to operations UI milestone.

Step 6b result (2026-09-17): done. Session+CSRF web routes GET /servers, POST /servers/register,
/servers/enrollment, /servers/revoke (reason + confirm), /environments/mark-reconciled; plain HTML, CSP
unchanged. Enrollment token shown once in 200 body (no redirect/log/cache). Owner-scoped ListServers and
ListReconcilingEnvironments. Server admin wired regardless of AUTODEPLOY_AGENT_ENABLED. Security review PASS.
Follow-ups: credential rotation needs an agent-initiated protocol (agent owns raw credential; enrollment is
single-use); register resubmission creates an orphan unenrolled server (add dedupe or cleanup).

Step 6c result (2026-09-17): done. cmd/agent `enroll` (https base URL, token from protected file/stdin,
credential generated locally, 0600 no-clobber atomic write, digest-only enrollment, removal on rejection) and
`run` (credential file O_NOFOLLOW/0600/owner check, heartbeat with executor_available=false, no claims without
an Executor). Fence state per environment plus .fence-index.json manifest; lost, unindexed, or corrupt state
fails closed (a crash between file and index write also fails closed, requiring reconciliation). HTTP client:
TLS only, never follows redirects (same-host https->http would resend Authorization), bounded bodies, retries
only 503/network, claim never retried. Evidence: make check, race, Linux amd64/arm64 agent builds, three fresh
full integration runs normal + -race; security review PASS WITH NOTES (redirect + claim retry fixed).
Follow-up: server-side claim idempotency key so a lost claim response cannot orphan a lease until expiry.

Step 6d result (2026-09-17): done. docs/IMPLEMENTATION_STATUS.md updated to 8/18 outcomes; new
"servers/credentials", "leases/fencing", "agent HTTP", "agent client/state", and "server admin web"
subsystem rows; schema section covers 000012/000013; next-slice section replaced with host execution
(checkout invocation, BuildKit build, container start/health checks) with follow-ups from Steps 4, 5,
6b, 6c listed. ADR 0004 gets a dated "Amendments (2026-09-17)" section (renewal produces no event,
attempt_count semantics, claim slot pre-creation/lock order, reconciliation ownership, revocation-only
attempt closure, sweeper try-lock) plus a minimal line-75 fix referencing it. ADR 0001 gets a dated
amendment on single-use enrollment tokens, agent-generated/digest-only credentials, web-admin-only
server administration, and deferred agent-initiated rotation. MEMORY.md and tasks/lessons.md updated;
no code changed.

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

## Approved Host Execution Slice 9a (control plane) and 9b (agent) — approved 2026-09-18

### Context

Progress is 8/18 outcomes. The queue, leases, fencing, agent protocol, and host agent client are
done, but **no lease has ever done real work**: `cmd/agent` registers no `Executor`, heartbeats
`executor_available=false`, and the control plane refuses to hand leases to such an agent. The next
approved outcome is host execution — checkout the exact commit, build the image, start the
container, health-gate it — with no release activation or Traefik routing.

Two blockers make this impossible as a single agent-side slice, both confirmed in code:

1. **No execution configuration exists anywhere.** There are no `projects`/`environments` tables;
   `project_id`/`environment_id` are free TEXT. Nothing records a container port, scheme, release
   strategy, resource limits, restart policy, or env-var/secret bindings. `frozen_secret_versions`
   exists but `freezeSecretVersions(ctx, tx, attemptID, nil)`
   (`internal/store/postgres/agent_lease_repository.go:421`) is called with `nil` — it has never
   written a row. ADR 0003 requires the exact secret-version set to be frozen *in the claim
   transaction*, so this is control-plane work, not agent work.
2. **The agent cannot mint a checkout credential.** `gitcheckout.Runner.Checkout` takes a concrete
   `*githubapp.CheckoutLease`, constructible only via `CheckoutService.Activate`, which needs the App
   signer and the reservation store — both forbidden on the agent by ADR 0001.

So the work splits: **9a** makes a claimed lease carry everything execution needs (frozen config,
frozen secret versions, materialization, progress reporting, a checkout credential seam); **9b**
implements the host side against that contract. Outcome of 9b: a real container from a real commit,
health-gated, with the deployment resting at `health_checking` — `activating -> healthy` belongs to
the activation slice.

### Owner decisions already taken

- Split into 9a then 9b; minimal release model introduced now (`candidate|ready|failed|removing|removed` only).
- Docker via **Engine API over the unix socket using stdlib `net/http` + `encoding/json`**, builds via
  **`docker buildx build --secret id=…,env=…`**. No moby/buildkit Go SDK — go.mod keeps its 4 lean
  direct dependencies, matching how `checkoutquota`/`checkoutcontain` already speak raw protocols.
- Checkout seam: **narrow interface in `gitcheckout`**, not an exported agent-side `CheckoutLease`
  constructor. `*githubapp.CheckoutLease` already satisfies the method set, so the control plane is unchanged.
- The pending privileged Linux checkout smoke is assumed correct; it stays a gate before the first
  real Hetzner deployment, not a blocker on writing this code.

### Slice 9a — control plane: configuration, freeze, materialization

#### Step 1 — migration 000014 (schema only, no Go)

Style follows 000012/000013: explicit CHECKs, one purpose-specific trigger function per table, no
shared generic trigger.

- `environment_runtime_configs` — PK `environment_id` (no FK; matches the `agent_jobs.server_id`
  precedent). `container_port` 1..65535, `container_scheme` http|https, `release_strategy`
  blue_green|stop_then_start, `drain_seconds` 1..300, `cpu_millis`, `memory_bytes`,
  `restart_policy` unless-stopped|on-failure, `health_policy_revision` (pin only — V1 hard-codes
  ADR 0005's default gate), `revision` for CAS, `updated_at`. Mutable desired state, CAS'd like
  `secret_heads.revision`; never deleted.
- `environment_runtime_bindings` — `(environment_id, variable_name)`, `binding_kind`
  secret|literal_nonsecret, `secret_head_id` REFERENCES `secret_heads` ON DELETE RESTRICT (NULL iff
  literal), `delivery_scope` build|runtime, CHECK that kind and head presence agree.
- Widen `frozen_secret_versions` with immutable `variable_name` and `delivery_scope`; PK becomes
  `(attempt_id, secret_version_id, variable_name)`. Existing no-update/no-delete triggers retained.
- `agent_job_attempt_execution` — PK `attempt_id` REFERENCES `agent_job_attempts`. Frozen copies of
  port, scheme, strategy, health_policy_revision, cpu/memory, restart policy, plus
  `environment_runtime_config_revision` so an operator can prove what an attempt actually ran with.
  Insert-once, no-update trigger.
- `releases` — immutable identity, `UNIQUE(deployment_id)` (one release per deployment),
  `environment_id`, `image_digest`, `config_digest`, `commit_sha`, frozen `strategy`, `state`.
  Trigger allows only `candidate->ready|failed|removing`, `ready->failed|removing`,
  `removing->removed`. **No `active`/`retained` edges** — the activation slice adds them.
- `release_runtime_instances` — `release_id`, `job_id`, `attempt_id`, `fence`, `container_name`,
  `observed_state` created|running|stopped|exited|missing. Immutable except `observed_state`, which
  only a write carrying the attempt's own fence may change.
- `release_operations` — append-only `prepare|activate|rollback|retire|cleanup` (only `prepare` is
  reachable in 9a/9b), with `job_id`, `attempt_id`, `fence`, `digest`, `state`, sanitized
  `outcome_summary`, `closed_at`. Update only `state`/`closed_at`; never delete.

Must prove (live PG 18): every immutability and CAS trigger positively and negatively — updating a
frozen column, skipping a `releases` state edge, deleting an append-only row, a stale-fence
`observed_state` write. Must not touch: any Go file.

Step 1 result (2026-09-18): done, uncommitted at time of writing then committed as its own atomic
commit. `migrations/000014_create_release_execution.sql` + `migrations/000014_release_execution_test.go`.
Corrections made to the plan's spec during implementation, all proven by tests:
- `releases.image_digest` is **nullable and write-once**, not NOT NULL: a release row must exist
  before the build runs (the prepare operation references its id) and the digest only exists after
  BuildKit finishes. `candidate -> ready` is refused while it is NULL; once set it can never change
  or be cleared.
- `release_operations.outcome_summary` is written **on the close transition**, not at insert. An
  outcome is not known when the operation opens.
- The release transition trigger permits same-state updates (that is how the digest is set) but
  freezes a terminal release (`failed`/`removed`) against any update at all.
- `agent_job_attempt_execution` also freezes `drain_seconds` (ADR 0005 line 335: the drain duration is
  environment-configurable and frozen into the operation). Review caught this omission.
- BEFORE INSERT triggers on `release_runtime_instances` and `release_operations` validate `fence` and
  `job_id` against the referenced `agent_job_attempts` row. Without them a wrong fence is accepted and
  only surfaces later as a fence-gated UPDATE silently matching zero rows. No row lock is taken: 000013's
  `validate_agent_job_attempt_update` makes `fence`/`job_id` immutable for the row's lifetime, so an
  unlocked read cannot go stale, and locking would add an edge to the ADR 0004 lock hierarchy for
  nothing.
- `environment_runtime_bindings` gets a scope trigger: a binding's `secret_head_id` must belong to the
  same `environment_id`, must agree on `delivery_scope`, and must not be a `provider`-purpose head.
  ADR 0003 names cross-environment materialization as an explicit threat. This is bind-time defense in
  depth and does **not** relieve Step 3a from re-checking scope immediately before decryption.
Collateral fixes outside migrations (required by the additive schema): two `TRUNCATE` fixture lists
(`deployment_repository_test.go`, `webhook_job_repository_test.go`) must name the new referencing
tables, and `agent_lease_schema_test.go`'s `frozen_secret_versions` insert must supply the new columns.
Evidence: `make check`, `go vet -tags=integration ./...`, `go test -race ./...`, three fresh PG18
integration runs normal **and** three fresh `-race` (store/postgres, agent/http, migrations, `-p 1`),
Linux amd64+arm64 builds, `git diff --check`. Independent zero-context review: PASS with three medium
findings, all three fixed and re-verified above.
Follow-ups recorded, not fixed here:
- `TRUNCATE` bypasses `BEFORE DELETE` triggers, so every "append-only"/"retained" table in 000012-000014
  is only as append-only as the app role's grants. No migration currently withholds `TRUNCATE` from the
  application role; it should.
- The two new mutable config tables are absent from the fixture `TRUNCATE` lists. Harmless today (tests
  use random-suffixed environment ids) but it will leak across tests once a fixed-name row appears.

#### Step 2 — claim-time freeze

Inside `tryClaim`, after the attempt row exists and before the existing `freezeSecretVersions` call:

- `SELECT … FROM environment_runtime_configs WHERE environment_id=$1 FOR SHARE`. **SHARE**, because
  freeze only reads; an administrator config edit must neither block nor be blocked by a claim.
  Lock-order position: **step 1b, after authorization rows, before the environment slot** — so a
  claim never holds the slot's UPDATE lock while waiting on a config lock. Administrator config
  updates must lock in the same order.
- **Missing config, missing binding target, or a non-active secret version fails only that
  candidate** (`ok=false`, like an occupied slot), never the whole `Claim` call. One misconfigured
  environment cannot stop claiming for every other environment on the server. No default fallback.
- Resolve bindings to exact `secret_versions.id` via `secret_heads.current_version_id`, then write
  `agent_job_attempt_execution` and real `frozen_secret_versions` rows in the same transaction.
- **Retry rule (ADR 0003):** for attempt N>1 of the same job, copy attempt N−1's frozen rows
  verbatim — both the secret versions and the execution config — rather than re-resolving against
  current heads. Re-resolving would silently adopt a version rotated mid-job, exactly what ADR 0003
  forbids. Freezing config forward too keeps one attempt = one provable snapshot.
- `queue.ClaimedLease` and the wire `leaseView`/`Lease` gain port, scheme, strategy,
  health-policy revision, cpu/memory, restart policy, and `SecretBindings` of
  `{VariableName, DeliveryScope, SecretVersionID}` — **version references only, never values**.

Must prove (live PG 18 + `-race`): claim racing a config update in both orders; attempt 2 freezing
the *same* versions after a rotation between attempts; attempt 2 not observing a config change;
missing config / missing active version failing one candidate while another still claims.

Step 2 result (2026-09-18): done. `internal/store/postgres/agent_lease_execution.go` (new) plus
`tryClaim`, `internal/queue/lease.go`, `internal/agent/http/claim.go`, `internal/agent/client/api.go`.
- Config is SHARE-locked as ADR 0004 position **1b**, the first lock `tryClaim` takes, before the slot.
  Deadlock freedom rests on the administrator config update being a single-statement, single-row
  transaction that holds nothing else while waiting. That premise is a comment today, not enforced
  code: **Step 3d must keep its config write single-statement**, and should carry a test that fails if
  it ever becomes multi-statement.
- Fresh-vs-copy-forward decision is deferred until after the job row is locked (that is where
  `attempt_count` lives, at hierarchy position 4, later than the config lock) but strictly before any
  mutation, so a misconfigured candidate fails with `ok=false` and zero partial writes.
- Attempt N>1 copies attempt N-1's frozen rows verbatim; since each attempt copied its own
  predecessor, attempt N is byte-identical to attempt 1's original resolution by induction. A missing
  predecessor row is classified `ErrAgentLeaseCorruptState`, never answered by re-resolving current
  secret heads.
- `config_digest`: sha256 over a length-prefixed encoding mirroring `internal/queue/job.go`'s
  `payloadDigest`, over the frozen config plus the binding set sorted by variable name.
- `releases.id` reuses `deployment_id` (already 1:1 via UNIQUE(deployment_id)). If a future slice ever
  needs more than one release per deployment this needs a real generated id and a schema change.
Evidence: three fresh PG18 integration runs normal and three fresh `-race`, plus `-race -count=20` on
the three concurrency tests, `make check`, `go vet -tags=integration ./...`, `go test -race ./...`,
`git diff --check`. Independent review: NEEDS CHANGES -> one medium fixed here (corrupt attempt chain
classified), one high deferred to Step 2b below, two lows recorded.
Lows recorded, not fixed: one Claim transaction can hold up to `claimDiscoveryMax` (200) config SHARE
locks until commit, which lengthens the worst-case wait an administrator config update can see under a
large backlog; worth a representative-scale check per ADR 0004.

#### Step 2b — literal binding values (required before Step 3d ships admin pages)

`environment_runtime_bindings.binding_kind` accepts `literal_nonsecret`, but 000014 gave it **no column
to hold a value**, and the freeze resolves only `binding_kind='secret'`. A literal binding is therefore
a silent no-op: an administrator sets `NODE_ENV=production`, Claim succeeds, and the variable simply
never exists — exactly the silent-default failure the rest of this design refuses. Close it before any
admin-facing path can create one: migration 000015 adds a bounded `literal_value` (present iff the kind
is literal, no control characters, and never logged), an append-only per-attempt frozen literal table
alongside `frozen_secret_versions`, copy-forward on retry, inclusion in `config_digest`, and the claim
DTO carries literals separately from secret references. Literal values are non-secret by classification
but still belong to the frozen snapshot, so one attempt remains one provable snapshot.

Step 2b result (2026-09-20): done. Migration 000015 plus the freeze/DTO wiring. A literal binding now
carries a bounded (1..4096 byte), control-character-free `literal_value`, required exactly when the kind
is `literal_nonsecret` and forbidden otherwise; `frozen_literal_values` is the append-only per-attempt
frozen copy, one value per variable per attempt, with its own no-update/no-delete triggers rather than
sharing `frozen_secret_versions`'. Literals are resolved on attempt 1, copied forward verbatim on retry
(an administrator edit between attempts is not observed), and included in `config_digest`, whose version
tag is bumped "1" -> "2" because the hashed shape changed. Literal values do cross the wire on the claim
response, which is safe only because ADR 0003 classifies them as non-secret protected metadata; secret
values still travel only as version references.
Newline-containing values are deliberately rejected: that shape belongs in a secret record, not a
literal binding.
Evidence: five new repository tests plus two migration tests, all observed passing; `make check`,
`go vet -tags=integration ./...`, `go test -race ./...`, Linux amd64+arm64 builds, `git diff --check`,
`gofmt` clean; three fresh PG18 `-race` sweeps and, in total, 22 consecutive fresh normal/race sweeps.
Collateral: `frozen_literal_values` had to join the fixture `TRUNCATE` lists (the same additive-schema
breakage Step 1 hit), and 000014's binding test now supplies a literal value.
**Unexplained single failure, recorded rather than buried:** during the first three-run sweep of this
step, run 2 reported FAIL for `internal/store/postgres` while `agent/http` and `migrations` passed. The
failing test name was lost because the command captured only the last three lines. It has NOT reproduced
in 22 subsequent fresh runs (normal and `-race`, including 6 consecutive postgres-only runs). Root cause
unknown, so this step's evidence is strong but not clean. Capture full output, never `tail`, on future
sweeps; if it recurs, suspect an ordering/timing assumption in a claim test rather than the schema, and
note this project has already been burned once (2026-09-16) by recorded evidence that did not reproduce.

#### Step 3a — `POST /agent/v1/leases/materialize-secrets`

Request `{attempt_id, fence}` — no variable list; the server decides what the attempt froze, so
"the deployment requires each requested value" stays a server-side check. Re-locks in `Complete`'s
order (credential SHARE -> slot UPDATE -> job -> attempt), confirms `leased`, fence match, unexpired
authority, then rechecks each frozen version is still eligible. The transaction returns eligible
version IDs; **KEK decryption happens after commit**, so no row locks are held across crypto.
Stale fence or expired authority returns the existing generic 401/stale shape — no new error class.
Must prove: version revoked between claim and materialize is refused; expired authority refused;
stale fence refused; a log-capture test asserting no plaintext in logs or audit rows.

Step 3a result (2026-09-20): done. `internal/store/postgres/agent_lease_materialize.go`,
`internal/agent/http/materialize.go`, `internal/secret/setauthorizer.go`, client `MaterializeSecrets`,
control-plane wiring.
- Authorization and decryption are separate steps on purpose: `AuthorizeMaterialization` re-derives the
  right to decrypt from locked rows in Complete's exact lock order (credential -> slot -> job ->
  attempt, all `FOR SHARE` since it mutates nothing) and returns references; decryption happens after
  that transaction commits, so no KEK operation ever runs while holding a row lock.
- The request names no variable and no version. The frozen set is the authority, so an agent cannot
  widen its own request.
- Refused: stale/ahead fence, non-occupant attempt, environment awaiting reconciliation, closed attempt,
  expired authority, cancelled job, another server's attempt, revoked credential, and a revoked secret
  version — all as the same generic 401 so an agent cannot tell the cases apart.
- **Superseded frozen versions still materialize** (ADR 0003: executing work keeps its frozen versions,
  so rotating a secret must not break an in-flight deployment); revoked ones do not, because that is
  compromise handling.
- `secret.Service` requires an `Authorizer` and an always-allow implementation would let any code path
  decrypt anything, so `secret.SetAuthorizer` admits exactly the keys the database just authorized, built
  per request. Empty/nil admits nothing. `Frozen` is deliberately not part of the key, so flipping it
  cannot reach a different secret.
- The KEK ring is loaded per request rather than held resident, matching `/readyz`; a load failure or a
  disabled secret subsystem answers 503 rather than an empty set an agent could mistake for "no secrets".
- A refusal partway through a set returns no plaintext at all: a container with some variables silently
  missing is worse than a failed deployment.
Evidence (full output captured to files, not tailed, per Step 2b's lesson): 10 new repository integration
tests, 6 handler tests, 3 set-authorizer tests; three fresh PG18 sweeps normal and three fresh `-race`,
all 3/3; `make check`, `go vet -tags=integration ./...`, `go test -race ./...`, Linux amd64+arm64,
`git diff --check`. The Step 2b single unexplained failure did not recur in any of these six sweeps.
Not yet done: independent review of this step (delegation was blocked by the account spend limit).

#### Step 3b — `POST /agent/v1/leases/progress`

Request `{attempt_id, fence, phase}` with phase in `fetching|building|starting|health_checking` —
all already legal `deployment.Status` values. Reuses `transitionDeployment`.

**Correction to note:** `canTransition(X, X)` is `false` (`internal/deployment/deployment.go:283`),
so a repeated or backward phase is *rejected*, not absorbed. The handler must read the current status
inside the transaction and return success without transitioning when the deployment is already at or
past the reported phase. Duplicate progress after a network retry must never fail a build.

**No `agent_job_events` row per progress call** — same reasoning ADR 0004 gives for renewals; a long
build would otherwise write unbounded events. Phase history lives in the deployment's own
append-only event log. Must prove: progress racing `Complete` never moves a terminal deployment
backward; duplicate/backward phase is a no-op; row-count assertion that `agent_job_events` gains
nothing.

Step 3b result (2026-09-20): done. `ReportProgress` in
`internal/store/postgres/agent_lease_materialize.go`, `internal/agent/http/progress.go`,
`deployment.AgentReportablePhase`/`deployment.ExecutionOrder`, client `ReportProgress`, wiring.
- Same authorization re-derivation and lock order as materialization, except the job is locked
  `FOR UPDATE` because this path does mutate the deployment, which serializes concurrent progress
  reports for one job against each other and against Complete.
- `ExecutionOrder` exists precisely because `canTransition(X,X)` is false: a duplicate report (network
  retry) or a stale report about an earlier phase is **absorbed as success and appends nothing**, while a
  skip over a phase that never happened is refused as invalid input rather than forging the intermediate
  transitions in an append-only log. A test pins `ExecutionOrder` and `canTransition` together so they
  cannot drift.
- An agent may report only fetching/building/starting/health_checking. `activating` and `healthy` are the
  control plane's own verdicts under ADR 0005 and are rejected before any repository call.
- **Found while testing:** cancellation of a *leased* job is cooperative, so the deployment is still on
  the execution path and the first version of this endpoint happily advanced it after a cancel request.
  Now refused from the moment `cancel_requested_at` is set, consistent with materialization and because
  advancing a deployment toward healthy while it is being cancelled tells an operator the opposite of
  what is happening.
- Appends no `agent_job_events` row (ADR 0004's bounded-events rule, same reasoning as renewals);
  asserted by row count. Deployment phase history lives in the deployment's own append-only log.
Evidence (full output captured to files): 6 new repository integration tests, 5 handler tests, 3
deployment-domain tests; three fresh PG18 sweeps normal and three fresh `-race`, 6/6; `make check`,
`go vet -tags=integration ./...`, `go test -race ./...`, Linux amd64+arm64, `git diff --check`, gofmt.
Not yet done: independent review of Steps 3a and 3b (delegation still blocked by the spend limit).

#### Step 3c — checkout credential seam

- Extract the method set `Checkout` actually uses (`Binding`, `SetCancel`, `Start`,
  `NewAskPassBroker`, `RedactingWriter`, `Close`) as an interface in `gitcheckout`.
  `*githubapp.CheckoutLease` satisfies it unchanged — add a compile-time assertion.
- Agent-side implementation fed by `POST /agent/v1/leases/checkout-token`, which calls
  `CheckoutService.Activate` server-side and returns `{binding, token, expires_at}`. This is the one
  point where a token crosses the boundary, which ADR 0003 permits over the authenticated channel.
- `Start`/`Close` delegate to thin RPCs that call the existing unmodified `CheckoutService` paths, so
  **the agent never calls GitHub's revoke API and never needs the App key**.
- **Blocker to fix or accept explicitly:** `HTTPProvider.RevokeToken`
  (`internal/githubapp/http.go:218`) calls `p.signer.Token()` via `doJSON` even though revoke
  authenticates with the installation token. Harmless today (the control plane has the signer) but it
  is why revoke cannot move to the agent. Keeping revoke server-side sidesteps it; the latent
  coupling should be recorded as a follow-up rather than silently routed around.

Must prove: control-plane path compiles and behaves unchanged; agent-side type drives the same
`Checkout` code path against a fake transport; `CheckoutLease`'s one-use/invalidate semantics intact.

Step 3c part 1 result (2026-09-20): interface narrowing done. `gitcheckout.Lease` captures the six
methods `Checkout` actually uses (Binding, SetCancel, Start, NewAskPassBroker, RedactingWriter, Close);
`*githubapp.CheckoutLease` satisfies it unchanged, pinned by a `var _ Lease = ...` assertion so a break
on either side fails the build rather than a call site. Zero behaviour change; the existing checkout
suite passes untouched.
**Regression I introduced and caught:** with an interface parameter, `lease == nil` no longer rejects a
typed nil — `(*githubapp.CheckoutLease)(nil)` is a non-nil interface holding a nil pointer, so the guard
passed and `SetCancel` dereferenced nil. Now guarded by the package's existing `nilCapability` helper,
with a test for both the nil interface and the typed nil. Worth remembering for every other
concrete->interface conversion in this codebase.
Evidence: `make check`, `go test -race ./...`, `go vet -tags=integration ./...`, `GOOS=linux` vet with
`-tags='integration checkout_integration'` for the checkout packages, Linux amd64+arm64 builds, one full
fresh PG18 integration sweep, `git diff --check`.

Step 3c part 2 (next): the three-RPC token lifecycle. Planned shape, chosen to preserve the existing
invariant that the durable lease is marked running *immediately before* the credential is delivered:
- `POST /agent/v1/checkout/begin` {attempt_id, fence} -> reserve only, returns reservation id + binding,
  **no token**.
- `POST /agent/v1/checkout/start` {attempt_id, fence, reservation_id} -> Activate + StartCheckout, and
  only then return {token, expires_at}, so the credential and the durable running state ship together.
- `POST /agent/v1/checkout/close` -> CloseCheckout + RevokeToken.
All three must repeat materialization's four occupancy checks (slot occupant, current fence, not
cancelled, not reconciliation-pending) before minting anything. The App private key must never cross into
the agent-facing factory — only the derived short-lived installation token does. Known wrinkle to design
around: the activated lease is a process-local object, so start/close need a registry keyed by
reservation id, and a control-plane restart between them leaves revocation to token expiry.

Independent review of Steps 3a and 3b (2026-09-20): **PASS with notes**, no blocking issues.
- Low: `ReportProgress` reads `deployments` `FOR UPDATE` and then `transitionDeployment` locks the same
  row again in the same transaction. Safe (row locks are reentrant within a transaction) but a redundant
  round trip; not worth touching shared helper code for.
- Low, verified safe rather than a gap: authorization commits before decryption, so a version could in
  principle be revoked in that window. `secret.Service.MaterializeAuthorized` re-reads the version fresh
  and rechecks its state, so a revocation in the gap is caught at decrypt time.
- Reviewer confirmed: no cross-scope plaintext path, no lock-order inversion (every path that touches
  both the lease tables and `deployments` locks `deployments` last), no secret in logs/audit/error
  bodies, `SetAuthorizer` cannot be widened including via `Frozen`, and the KEK-load failure is
  fail-closed.

Step 3c part 2 result (2026-09-20): done. `AuthorizeCheckout` + `queue.CheckoutAuthorization`,
`internal/agent/http/checkout.go` (begin/start/close + registry), `internal/agent/checkout` (agent-side
`gitcheckout.Lease`), `githubapp.NewAskPassBrokerFromToken`, `githubapp.CheckoutLease.DeliverToken`,
client methods, control-plane wiring.
- `AuthorizeCheckout` repeats materialization's four occupancy checks; repository id and commit SHA come
  from the locked job row, never from caller input. Begin also cross-checks the reservation's resolved
  repository/commit against the job's and closes the reservation immediately on mismatch.
- Invariant preserved: begin reserves and returns **no token**; start activates, marks the durable lease
  running, and only then returns the credential; a failed Start closes the activated lease so no minted
  token is left live.
- The registry binds each reservation to the server, attempt and fence that created it, and an unknown
  reservation is indistinguishable from a mismatched one. 30-minute TTL, swept lazily, comfortably
  exceeding the store's 10-minute reservation window. A control-plane restart between start and close
  loses the registry, so revocation degrades to token expiry — deliberate and documented in the type.
Three corrections I made to the delegated work:
1. **Token delivery was draining the one-use askpass pipe** — functional, but a misuse of a mechanism
   built to feed a local Git subprocess. Replaced with `CheckoutLease.DeliverToken`, one-use and sharing
   the pipe's own `delivered` flag, so a lease either feeds a local Git or is delivered once to one
   agent, never both and never twice. ADR 0003 permits delivering transient material over the
   authenticated channel; what must never be exported is *minting*.
2. **Data race:** `match` returned the mutable registry entry, so a handler read `lease`/`closed` while
   another request wrote them through `setLease`/`markClosed`. Now returns an immutable snapshot taken
   under the lock.
3. **Double-mint:** two concurrent starts for one reservation both passed the ownership check, both
   activated, and the loser's token stayed live until expiry with nothing able to close it. Start is now
   a one-shot claim (`claimStart`/`releaseStart`), and a retried or concurrent start is refused.
   Neither 2 nor 3 was caught by the delegated tests because none drove one reservation concurrently;
   added tests do, including an 8-goroutine start race asserting exactly one token is minted.
Evidence: 10 repository integration tests, ~25 handler tests, 9 agent-lease tests, 3 new `DeliverToken`
tests; three fresh PG18 sweeps normal and three `-race`, 6/6; `make check`, `go vet -tags=integration
./...`, `go test -race ./...`, Linux amd64+arm64, `git diff --check`, gofmt.
Fixture gotcha recorded by the implementer: `environment_slots` requires `reconciliation_attempt_id`
whenever `reconciliation_required` is true (000013 CHECK), so set both together in tests.
Not wired into `cmd/agent` yet — that is Slice 9b's job.

#### Step 3d — runtime configuration admin pages

Session+CSRF-protected, owner-scoped pages in `internal/web`, following the existing
`/servers` pages: plain HTML, CSP unchanged, no design system (deferred to the operations UI
milestone). Routes for viewing and updating an environment's runtime config (CAS on `revision`, so a
concurrent edit is rejected rather than silently overwriting) and for adding/removing variable
bindings. Selecting a secret head shows only its identity and version number — never a value.
Every mutation is audited with the owner principal and the resulting revision.
Must prove: CSRF/origin rules and owner scoping match the server pages; a stale-revision update is
rejected; invalid port/scheme/strategy/limit values are refused before any write; no secret value
reaches the response, logs, or audit rows.

Step 3d result (2026-09-21): done — slice 9a complete.
`internal/store/postgres/environment_runtime_config_repository.go`, `internal/web/web.go` routes
`/environments/config`, `/environments/bindings/add`, `/environments/bindings/remove`, migration 000016.
- **Lock-order constraint satisfied and proven.** The write path locks only the administrator session/user
  rows and then the config row itself (single INSERT or single CAS UPDATE). The ownership check does name
  `agent_jobs` but as a plain unlocked SELECT, so it takes no row lock and cannot join a wait cycle. Proven
  two ways: a real-repository-vs-real-Claim concurrency test over 8 environments in both interleavings
  (`-race -count=20`, clean), and a source-reading test asserting no backtick SQL literal in the repository
  locks `environment_slots`/`agent_jobs`/`agent_job_attempts`/`servers`/`server_credentials`.
- I rewrote that second test: as delivered it inlined copies of the queries, so editing the real SQL and
  leaving the copy alone would still have passed — it restated the invariant instead of proving it. It now
  reads the repository source. Verified by injecting `FOR SHARE` on the ownership check: the test fails with
  the right message, and passes again once reverted.
- **Accepted deviation — migration 000016.** No existing audit table fits: `server_audit_events.action` is a
  closed enum with no config action and its `server_id` FK-references `servers`, which a config mutation has
  no value for; every other audit table is a fixed enum for an unrelated domain. 000016 adds one independent
  append-only `environment_runtime_config_audit_events` with its own no-update/no-delete triggers and touches
  no existing table. The alternative was shipping admin mutations with no durable audit trail, which ADR 0006
  would not accept.
- Implementer's own bug, found in testing and fixed: it first wrote a failure-audit row inside the same
  transaction after a Postgres error, which always fails because an errored transaction is poisoned until
  rollback. Failure audits for genuine server-side errors now write separately, while zero-row outcomes
  (stale-revision CAS, remove-not-found) still audit atomically.
- Validation reaches the operator: stale-revision CAS renders a distinct "changed underneath you" and
  refreshes the expected revision; schema/trigger rejections (cross-environment head, delivery-scope
  mismatch, provider-purpose head) surface as validation, never a 500; a secret binding shows only head id
  and version number, never a value.
- End-to-end: an environment with no config is skipped by `Claim`, then claimed once a config exists.
**Known limitation, recorded not assumed away:** `environment_id` has no owner column anywhere in this
schema, so ownership is derived from `agent_jobs.owner_id` exactly as `MarkReconciled` already does. Two
consequences: an environment cannot be pre-configured before its first job is queued, and two owners using
the same `environment_id` string would share one config row (V1 has one administrator, so not exploitable
today, but it is a latent multi-tenancy gap that wants an environments table or an owner column).
Evidence: three fresh PG18 sweeps normal and three `-race` across store/postgres, agent/http, web,
migrations (6/6); both deadlock proofs at `-race -count=20`; `make check`, `go vet -tags=integration ./...`,
`go test -race ./...`, Linux amd64+arm64, `git diff --check`, gofmt.

### Slice 9b — agent host execution

- **Step 4** — `internal/runtime/docker`: Engine API client over the unix socket (stdlib only),
  typed label builder (one function per resource kind — the single place ADR 0005's
  cleanup-by-exact-label invariant is enforced), `//go:build linux` implementation plus a `!linux`
  stub whose constructors return an unsupported-platform error, mirroring `checkoutcontain`. No
  executor wiring yet. Prove: `go build ./...` and `go vet` on darwin and linux.

Step 4 result (2026-09-21): done — `internal/runtime/docker`. Engine API client over the unix socket
(stdlib `net/http` only, go.mod still at its 4 direct deps), typed per-kind label builders, exact-match
selectors, `//go:build linux` constructor plus a `!linux` stub returning an unsupported-platform error.
No executor wiring, no `cmd/agent` change. Decisions and corrections, all proven by tests:
- **API version pinned at `v1.44`** (Engine 25.0). The Engine API is backward compatible, so a pinned
  older-but-supported version keeps working against newer daemons — safer across a fleet provisioned at
  different times than tracking the newest. `Probe` reads the daemon's own `MinAPIVersion..APIVersion`
  range and fails closed with a configuration error if v1.44 falls outside it, so an incompatible engine
  is caught at enrollment rather than mid-deployment.
- **The default request timeout must not be cancelled when the request function returns.** The first
  implementation used `defer cancel()`, which tore the context down as soon as the headers arrived —
  any response whose body trails its headers failed with `context canceled`. Caught by review, not by
  the delivered suite, which only exercised bodies that arrive with the headers. The cancel is now
  handed to the response body and fires on `Close`. This was not cosmetic: every streaming endpoint
  Steps 5 and 6 need (build log, container wait) is a trailing-body response.
  Regression test: `TestTrailingBodyDeadlineCoversTheWholeCall`.
- **Dial failures are not uniformly transient.** `ENOENT`/`EACCES`/`ENOTDIR` mean the configured socket
  path is wrong or unusable and are classified `configuration`; connection-refused/reset/timeout stay
  `transient`. The delivered test had asserted the opposite — it dialed a never-created path and demanded
  "transient" — which would have had a retry loop hammering a permanently misconfigured agent forever.
- `EngineError` carries the literal socket path in a dial failure's wrapped error. The path is kept
  (operators need it) but the type documents that `Error()`/`Err` are operator-facing only and must be
  redacted before reaching any tenant-visible surface.
- Buildx is deliberately NOT probed here: it is a CLI plugin with no Engine HTTP API surface. Its
  capability probe belongs to Step 5, its only caller.
- No selector exists for environment networks/volumes, by design: ADR 0005 forbids selecting stable
  environment infrastructure by label during release cleanup, so the package offers no way to do it.
**Obligation this places on Step 6, recorded so it is not rediscovered the hard way:** the release-scoped
selector requires the resource's *full* identity (attempt, fence, runtime-instance, image and config
digests). That is correct — ADR 0005 requires cleanup to fail closed rather than broaden a selector — but
it means the retry path cannot find a prior attempt's leftover container by guessing. Step 6 must persist
each attempt's complete identity durably at or before resource creation, and reconstruct the selector from
those stored values. `release_runtime_instances` (migration 000014) already holds job/attempt/fence and
the container name; confirm it carries everything the selector needs before writing the retry lookup.
Evidence: `make check`, `gofmt -l`, `go vet` incl. `-tags=integration`, `go test -race ./...`, three fresh
PG18 sweeps normal and three `-race` (6/6 clean), cross-build+vet darwin/arm64, linux/amd64, linux/arm64,
`git diff --check`. Guard tests were each broken on purpose and confirmed to fail: early-cancel restored,
dial classification reverted, a forbidden `attempt-id` label injected into a built image.
- **Step 5** — build: `docker buildx build` with `--secret id=<opaque>,env=<var>` so values travel
  the BuildKit session, never argv, ARG, context files, or layers. Missing BuildKit fails closed.
  Build output streams through `crypto.RedactingWriter` before it leaves the agent. Prove on Linux
  (skipped elsewhere): a fixture image's history, layers, and provenance contain no secret;
  BuildKit-unavailable fails closed; redaction failure suppresses output.

Step 5 result (2026-09-21): done — image build via `docker buildx build`, in `internal/runtime/docker`
(`build.go`, `build_linux.go`, `build_unsupported.go`). Secrets reach BuildKit only as
`--secret id=<opaque>,env=<VAR>` with the value in the subprocess environment; the id is positional
(`build-secret-0`, ...), never the variable name. There is no `--build-arg` call site in the package and
no fallback to the legacy builder, which has no `--secret` support at all: a missing or broken buildx
fails closed before the subprocess starts. Both stdout and stderr are wrapped in `crypto.RedactingWriter`
before the build runs, and a redactor construction failure suppresses output rather than emitting raw.
The image digest is read back from buildx's `--metadata-file` and validated through `requireHexDigest`;
everything downstream runs by digest.
Corrections made during review, each proven by a guard test that was then broken on purpose to confirm it
fails:
- **`--file` does not require the Dockerfile to live inside the build context.** The first implementation
  only rejected NUL/CR/LF, so an absolute or `../`-prefixed path would have made buildx open an arbitrary
  host file as a Dockerfile — and a parse error echoes the offending line into output that is redacted
  only for known secret patterns, not arbitrary file content. `resolveDockerfilePath` now rejects absolute
  paths, rejects anything escaping the context after `filepath.Clean`, and `os.Lstat`s every component
  from the context root down so no symlink (including an intermediate directory) is traversed.
- **A secret binding named after one of the CLI's own control variables repoints the CLI.** A binding
  named `DOCKER_HOST` or `BUILDKIT_HOST` lands verbatim in the child environment and could aim the build
  at another daemon or a less isolated builder. `resolveBuildSecrets` now rejects reserved names. Note the
  env allowlist below does NOT cover this: secret entries are appended last and would win regardless.
- **The build subprocess no longer inherits the agent's environment.** It was `append(os.Environ(), ...)`;
  this codebase configures through env (`AUTODEPLOY_DATABASE_URL` carries a password, plus KEK manifest and
  GitHub App key paths), so the child now gets an explicit allowlist only, then the secret entries.
- **`BuiltImageIdentity` lost `ImageDigest`/`ConfigDigest`.** An image's own digest cannot be a label on
  that image: labels live in the config blob the digest is computed over, so it is a genuine self-reference
  that no single build can satisfy. The digest-bearing variants were therefore selecting labels no image
  could ever carry, which would have made image cleanup match zero images forever and silently leak disk.
  The identity is now the four fields knowable before the build; digests are returned in `BuildResult` and
  recorded by the caller. ADR 0005 already says shared images are selected by exact image/config digest
  plus database and Docker reference checks, not a label filter.
  `TestBuiltImageSelectorMatchesLabelsTheBuildActuallyApplies` is the guard against the two drifting apart.
- One delivered test was vacuous and was caught by the break-on-purpose pass: the traversal test pointed at
  a path that did not exist, so an `Lstat` failure and a genuine escape rejection were indistinguishable and
  it passed against deliberately broken code. Rewritten to target a real file above a real context.
**NOT verified here, and it gates the first real deployment along with the privileged checkout smoke:**
there is no Docker daemon on the dev box, so every build test runs against a fake `docker` binary written
into a temp dir. That exhaustively proves the agent-side argv, env, redaction and fail-closed logic and
proves nothing about BuildKit's own behavior. The ADR 0003 claim that a built image's history, layers and
provenance contain no secret is therefore still UNPROVEN and must be demonstrated on the Linux host with
`docker history`/`docker inspect`/provenance inspection against a real build through this package.
Residual risk accepted for now: `resolveDockerfilePath` validates and buildx later opens the path, so a
symlink swapped in between the two is a TOCTOU window. The checkout is agent-controlled and the window is
small, but a repo-supplied context is not trusted input — revisit if checkout ever runs concurrently with
the build.
Evidence: `make check`, `gofmt -l`, `go vet` incl. `-tags=integration`, `go test -race ./...`, three fresh
PG18 sweeps normal and three `-race` (6/6 clean), cross-build+vet darwin/arm64, linux/amd64, linux/arm64,
`git diff --check`, go.mod/go.sum untouched (4 direct deps).
- **Step 6** — run and gate: create/start by image digest, no published ports, joined only to the
  stable environment network (network creation is environment provisioning, not per-deployment),
  resource limits, restart policy, immutable release/runtime-instance/attempt/fence labels. Health
  gate is ADR 0005's default shape hard-coded: exact host/path/port, 2xx, redirects off, 3
  consecutive successes, 2s interval and timeout, 60s deadline, container must stay running, bounded
  body discarded. Retry looks up prior-attempt resources by exact label match before creating
  anything. Then register the `Executor` in `cmd/agent` and flip `executor_available=true`.
  `FailureClass` mapping: Docker/network blips -> `transient`; bad Dockerfile or failed health gate
  with the container still running -> `permanent_configuration`; inconsistent local label/fence state
  -> `corrupt_state`. No new enum values.



**Step 6 was split into 6a (run + health gate) and 6b (executor wiring).** A subdivision, not a scope
change: the plan already called for independent review between steps, and 6b is what flips
`executor_available=true` and starts real leases flowing, which deserves its own review rather than
being bundled with probe mechanics.

Step 6a result (2026-09-21): done — `internal/runtime/docker/run.go`, `health.go`, `classify.go`, with
`engine.go` extended for container create/start/inspect/list and network inspect. Container runs by
immutable digest, publishes no ports (the wire struct has no `PortBindings`/`PublishAllPorts` field to
set), joins exactly one network whose ID must be resolved through `InspectNetwork` and match Docker's own
ID shape (so `host`/`bridge`/free text cannot reach `NetworkMode`), carries limits and restart policy from
the frozen config, and is named from a validated opaque runtime-instance ID. The health gate hard-codes
ADR 0005's default shape with no caller-facing override.
Corrections made during review, each proven by a guard test that was then broken on purpose:
- **`ClassifyHealthGateResult` ignored its argument entirely** and returned `permanent_configuration` for
  every failure. Only `transient` is retryable (`internal/queue/retry.go:73`), so an agent shutdown or a
  lost lease renewal mid-gate permanently failed a deployment when nothing about the application had
  failed. Now reason-driven: `context_cancelled` -> transient; `deadline_exceeded` -> permanent (the app
  came up but never passed the gate); `container_not_running` -> transient if OOM-killed, permanent
  otherwise; anything unrecognized -> corrupt_state, failing closed.
- **A host OOM kill was indistinguishable from an application crash.** `ContainerInspect` never read
  Docker's `State.OOMKilled`, which arrives on the same inspect call the gate already makes. Without it,
  the host running out of memory permanently failed a deployment that would likely have succeeded on the
  next attempt. The gate now carries exit code and OOM flag out in `HealthGateResult`.
- **The same deadline classified two different ways depending on timing.** If the gate's context expired
  at the top-of-loop check it produced a `deadline_exceeded` result; if it expired while an inspect was in
  flight, the engine client's transient-classed error escaped instead and the deployment was retried. One
  condition, two verdicts, decided by which internal timer noticed first. The loop now converts a state
  error into the deadline result whenever its own context has expired.
- **Losing a benign create race poisoned the deployment.** A concurrent execution for the same identity
  (a lease-timeout re-dispatch racing a live attempt) lost the create with a 409 and was classified
  `corrupt_state`, which is never retried — even though the race had already resolved correctly and the
  one right container existed. The 409 path now re-runs the exact-label lookup and adopts the container
  when it matches; a name held by a *different* identity is still `corrupt_state`. Verified under an
  8-way concurrent probe: exactly one container created, no data race.
- Two delivered tests proved less than their names claimed: the end-to-end create test never asserted
  `NetworkMode` or `Image` (so skipping network resolution or running by tag would have passed green), and
  the lookup handler only checked the filter was non-empty. Both strengthened. Note the strengthened
  handler builds its expectation from the selector itself, so it is a wiring check; the label set's
  contents are pinned independently by `labels_test.go`, which is what actually catches a dropped or
  broadened label (confirmed by deleting the fence label and watching it fail).
Accepted on its merits: the container runs by the image's **config digest**, not its manifest digest.
`buildx --load` imports into the local store where the image's only local address is its image ID, which
is the config digest; the manifest digest is not locally resolvable without a registry round-trip this
design avoids. ADR 0005 permits "immutable image ID/digest", so this satisfies "never a mutable tag".
Known gap this leaves: nothing verifies the `ConfigDigest` handed in actually matches the image now in the
local store — that trust sits on the build step that populated it.
**Process deviation:** the fix round for 6a was implemented by the orchestrator rather than delegated,
because the account hit its monthly spend limit (HTTP 429) and subagents became unavailable mid-step. The
independent review completed and its findings are incorporated; the implementation was not independently
reviewed afterwards, so 6a carries less review depth than Steps 4 and 5. Worth a second pass when
delegation is available again.
**NOT verified here:** every test runs against a fake Engine API over a unix socket and an httptest
server; no real dockerd was involved. Unproven until the Linux host: that the Engine v1.44 JSON field
names and semantics used here (`NanoCpus`, `Memory`, `RestartPolicy`, `NetworkMode`) behave as expected
against a real daemon, that a `--load`ed image is addressable as `sha256:<config-digest>`, real container
IP assignment timing, and the real 409 response shape.
Evidence: `make check`, `gofmt -l`, `go vet` incl. `-tags=integration`, `go test -race ./...`, three fresh
PG18 sweeps normal and three `-race` (6/6 clean), cross-build+vet darwin/arm64, linux/amd64, linux/arm64,
`git diff --check`. Six invariants broken on purpose and each confirmed to fail its guard: classification
collapse, the deadline ctx check, OOM evidence, the 409 adopt path, network resolution, and a dropped
fence label.


Step 6b result (2026-09-21): done — `internal/agent/executor` assembles the real Executor (checkout ->
build -> run -> health gate), registered in `cmd/agent` behind a gate. `Execute` reports
`fetching -> building -> starting -> health_checking` and STOPS there; activation is a later slice.
**`executor_available` is still false.** `cmd/agent/main.go`'s `const executorEnabled = false` is the
whole switch: while false `buildExecutor` is never called, `agent.Executor` stays nil, and the heartbeat
keeps reporting false exactly as before. `TestExecutorEnabledStaysFalse` fails the build if it is flipped
without updating that test, so the flip cannot happen by accident.
**Two prerequisites must be met before flipping it — flipping starts real deployments:**
1. The privileged Linux checkout smoke (still pending, gates the first real deployment).
2. A configured health-gate path (and Host) column. See below — without it the gate probes `/`, and an
   app that does not return 2xx there fails permanently.
Corrections made during review, each proven by a guard test broken on purpose:
- **The health gate had no configured path or Host, and the consequence is severe.** `migrations/000014`
  defines `container_port` and `container_scheme` only, while ADR 0005 requires an exact configured
  *path and Host header*. A failed gate with the container running classifies `permanent_configuration`,
  which `internal/queue/retry.go` never retries — so an application that does not answer 2xx on `/`
  could never deploy, with no automatic recovery. Decision: do NOT add the schema column in this step
  (control-plane work, and the slice is already large). Instead `/` is now an explicitly named V1 default
  documented as a temporary stand-in, and the Host header no longer carries a synthetic
  `lease.EnvironmentID` — it sends the literal dialed authority (`ip:port`) a direct-IP client would send,
  rather than attesting a hostname that is not real and could break a Host-routed app. The missing column
  is recorded as a prerequisite for the flip, at the flip site.
- **Another vacuous guard test.** `TestExecuteBuildSecretNeverReachesContainerEnv` never reached the
  defense-in-depth re-check it claimed to prove: the binding's own scope was already Build, so the outer
  filter excluded it first and deleting the inner check left the test green. Rewritten so the lease-side
  binding claims Runtime while the materialized value comes back scoped Build. Third occurrence of this
  pattern on this project.
- **The progress tracker's idempotency guard is per-call and unpersisted**, so after an agent restart
  mid-lease correctness rests on the control plane absorbing a repeated status — an assumption the code
  asserted but never verified, with the error swallowed if wrong. Verified for real:
  `ReportProgress` DOES absorb, committing without calling `transitionDeployment` when
  `targetOrder <= currentOrder`. Worth recording how nearly this became a fourth vacuous test: an
  exact-repeat test passes even with the absorb branch deleted, because `deployment.Transition` has its
  own same-status shortcut that absorbs it regardless of the postgres layer. Only a STALE report
  (`building` then `fetching` again) exercises `ReportProgress`'s own ordering check, since
  `canTransition(building, fetching)` is false. Also found: `ProgressReporter` was never wired into
  `newTestHandler`, so that route had been 503ing in the integration suite.
- `EngineError` now documents that a non-2xx body is embedded verbatim in its error text while
  `CreateContainer` carries secret-bearing `Env` in the same request — an accepted, monitored gap that
  obliges every logging call site to treat the text as untrusted. Docker does not echo request bodies today.
- `labels.go`'s `SourceDigest` doc was actually wrong (it claimed a digest of Dockerfile plus context).
  Corrected in the schema doc: it is `sha256(commitSHA)`, not reproducible by hashing the checkout, and
  safe only because this slice does no build caching off it. `RuntimeInstanceID` and `BuildOperationID`
  deliberately share one derived value; separating them is a prerequisite for cross-attempt image sharing.
Additions outside the step's stated boundary, both load-bearing and minimal: `Env` on
`docker.RunContainerRequest` (Step 6a had no env-delivery mechanism at all, so runtime secrets could never
reach the container; ADR 0003's "Runtime-secret delivery" explicitly sanctions Engine API env injection),
and `gitcheckout.Result.WorkspaceFD()` (Step 3c's `Result` exposed no accessor, so no caller could ever
use a checkout). The checkout workspace reaches the build as a path via `/proc/self/fd/<n>`, resolved once;
the residual TOCTOU is the same class Step 5 already accepted.
**NOT verified, and unchanged by this step:** no part of this has met a real Docker daemon or a real
checkout. `Execute` is proven only against fakes for every seam; `productionCheckout` is Linux-only and has
never run; the `Env` field has never been sent to a real Engine API. The plan's end-to-end proof (push to a
configured repo on the Hetzner host and watch the deployment walk to `health_checking`) remains pending and
is gated behind both prerequisites above.
Evidence: `make check`, `gofmt -l`, `go vet` incl. `-tags=integration`, `go test -race ./...`, three fresh
PG18 sweeps normal and three `-race` (6/6 clean), the new progress tests at `-race -count=20`,
cross-build+vet darwin/arm64, linux/amd64, linux/arm64, `git diff --check`, go.mod/go.sum untouched.

### Verification

Per step, the bar the completed slices already met: `make check`, `go vet` including
`-tags=integration`, `go test -race ./...`, and three fresh PostgreSQL 18 integration runs (normal
and `-race`) via the CI `AUTODEPLOY_TEST_DATABASE_URL` contract with `-p 1` across
`internal/store/postgres`, `internal/agent/http`, `migrations`; concurrency-sensitive tests repeated
`-race -count=20`. Linux amd64/arm64 cross-builds where tagged code changes. Independent
zero-context correctness and security review after every step, findings fixed before the next step.
Finish with `git diff --check` and a status/doc update to 9/18.

End-to-end proof for 9b on the Hetzner host (after the privileged checkout smoke): push to a
configured repo, watch the deployment walk `assigned -> fetching -> building -> starting ->
health_checking`, confirm the container runs by digest with correct labels and no published ports,
then confirm a forced failure leaves no resources labeled as a successful attempt.

### Risks and rollback

- Nothing here has run live; every prior step's review found real security or concurrency bugs, so
  per-step independent review is not optional.
- The attempt-1-resolves / attempt-N-copies split must be tested explicitly or it will silently
  degrade to re-resolving, reopening the ADR 0003 violation it exists to prevent.
- Engine API over raw HTTP means we own the small slice of JSON we use, and buildx must be installed
  on the host — add a startup capability probe so a missing buildx fails closed at enrollment rather
  than mid-deployment.
- 000014 is additive; rollback is a new forward migration dropping the added tables/columns, safe
  because nothing writes them until Step 2. Steps 2–6 are code reverts. 9b never touches routing or
  activation, so no currently-healthy release can be disturbed by construction.

### Step order

1 (migration) -> 2 (claim freeze) -> 3a (materialize) -> 3b (progress) -> 3c (checkout seam) ->
3d (admin pages) -> 4 (docker skeleton) -> 5 (build) -> 6 (run + gate + executor wiring).
One subagent per step, independent review between steps, atomic commit per step.

## Approved Slice 10 — health-gate configuration + blue/green activation (approved 2026-09-22: D1 blue/green only, D2 Host required, D3 same lease; D4/D5 defaults)

### Context
- 9b stops at `health_checking`; `executorEnabled = false`; nothing has run on a host.
- Executor flip blocked by: (1) privileged Linux checkout smoke (owner), (2) no configured health-gate path/Host — `internal/agent/executor/executor.go` hard-codes `healthGatePathV1Default = "/"` and synthesizes Host from `ip:port`; a failed gate with the container running is `permanent_configuration` (never retried).
- ADR 0005 already decides: Traefik watched **file provider** (no Docker socket to Traefik), atomic temp-file+fsync+rename+fsync-dir, two-transaction fenced activation, no blind automatic rollback, 30s drain then stop (not remove) predecessor.
- Verified ready: `internal/deployment` already allows `health_checking -> activating -> healthy|failed`; `AgentReportablePhase` excludes `activating`/`healthy`.
- Verified missing: no route/hostname table, no Traefik code, `releases.state` lacks `active`/`retained` (000014 CHECK + `validate_release_update`).

### Scope
Slice 10 = Step 0 + blue/green activation only. Deferred to slice 11+: stop-then-start (needs maintenance service), rollback, retention/cleanup batches, full host reconciler (this slice fails closed to reconciliation-required; resolution is operator `mark-reconciled`).

### Step 0 — configured health-gate path + Host (land first, independently; one subagent)
- Migration `000017`: `environment_runtime_configs.health_gate_path TEXT NOT NULL DEFAULT '/'`, `health_gate_host TEXT` (see decision D2); same columns frozen NOT NULL on `agent_job_attempt_execution`. CHECKs: path starts with `/`, not `//`, ≤2048 bytes, no whitespace/control chars/`#`; host is a DNS hostname with optional `:port` (strict charset, ≤253), no userinfo/path. Test file per 000014/000015 precedent. Safe additive: neither table has ever had a production row (`executorEnabled=false`).
- `environment_runtime_config_repository.go`: input fields + `valid()` mirroring the CHECKs exactly; create/update/get SQL.
- `internal/web/web.go` runtime-config page: two fields, validation, display; `environment_config_test.go` negatives.
- `agent_lease_execution.go`: fresh-resolve SELECT, attempt INSERT, copy-forward SELECT gain both columns; **`config_digest` inputs widen** (otherwise path/Host could change without changing the digest).
- Wire: `internal/agent/http/claim.go` + `internal/agent/client/api.go` Lease fields.
- Executor: delete `healthGatePathV1Default` and `healthGateHost`; pass the lease values.
- `internal/runtime/docker/health.go`: build the probe URL with `url.URL{Scheme, Host, Path/RawQuery}` instead of string concatenation (today `Scheme + "://" + Host + Path`), and re-validate path/host agent-side (defense in depth; the dial target stays `ip:port`).
- `ClassifyHealthGateResult` unchanged unless review finds a misclassification independent of this gap — report either way.
- Must-prove: CHECK negatives; CAS trigger still enforced over widened row (break trigger, see test fail); copy-forward keeps attempt-1 path/Host after config changes; digest changes when path/Host change (guard: remove from digest inputs, see test fail); executor passes configured values (fake gate); `//evil`, `?`-only, header-injection and userinfo host rejected at every layer.
- Fully provable on macOS. Outcome counter does not move.

#### Step 0 result (2026-09-22, verified; uncommitted — git blocked by unaccepted Xcode license)
- Migration 000017: `health_gate_path` (default `/`) and required `health_gate_host` on `environment_runtime_configs`, frozen NOT NULL on `agent_job_attempt_execution`. Path grammar is an ASCII RFC 3986 allowlist (well-formed `%XX` only, optional query, not `//`, ≤2048 bytes); host is DNS labels with optional port 1–65535. `internal/healthgate` holds the Go validators as the same literal regex strings the CHECKs use, and the repository, web form and agent all reuse them.
- Claim freeze, copy-forward and `config_digest` (tag 3) carry both values; the lease wire carries them; the executor's hard-coded `/` and synthetic Host are deleted. A lease with an empty value fails closed as `corrupt_state` before any checkout/build/container work.
- The probe URL is built with `url.ParseRequestURI` and must satisfy `RequestURI() == configured path` (else `corrupt_state`). Probe invariant errors now propagate; network/TLS/timeout failures are still failed probes. SNI follows the configured Host (documented net/http behaviour; not captured on the wire).
- Defect found in review, fixed before landing: the first cut used `url.URL{Path: p}`, which re-encoded `/health%2Fcheck` as `/health%252Fcheck` and `/hé` as `/h%C3%A9` — a valid config would have failed permanently. Orchestrator property-checked 200,012 grammar-generated paths: 0 round-trip mismatches.
- Every new guard was broken on purpose and seen to fail (8 in the first pass, 4 in the fix pass). The one exception is `buildProbeURL`'s own assertion, which no accepted input can reach (see the property check).
- Evidence: gofmt, `go vet` incl. integration, build, `go test -race ./...` (27 packages ok), three fresh PG18 sweeps normal + `-race` (6/6, 4/4 packages each), concurrency tests `-race -count=20`, Linux amd64/arm64 vet. `git diff --check` not run (git unusable). `make` is also unusable until the license is accepted, so the targets were run directly with `GOFLAGS=-buildvcs=false`.
- Executor-flip prerequisite 2 (configured health-gate path/Host) is now closed. Prerequisite 1 (privileged Linux checkout smoke) is still pending.

### Step 10a — route head + release states (migration `000018`; one subagent)
- New `route_heads` (PK `environment_id`, `route_revision BIGINT` CAS, `active_release_id` FK nullable, `hostname` nullable, `reconciliation_required`, append-only `route_events`).
- New forward migration widens `releases.state` with `active`, `retained` and edges `ready->active`, `active->retained`, `retained->active` (drop/recreate CHECK + replace trigger function; never edit 000014).
- Minimal hostname assignment: one hostname per environment, audited admin mutation (mirrors 000016 audit), explicit authority acknowledgement per ADR 0005; no ownership challenge (single admin V1).
- Tests: CHECK/trigger edges both legal and illegal; CAS revision advance-by-one; audit-with-mutation.

#### Step 10a result (2026-09-22, verified)
- Migration 000018: `route_heads` (unique lowercase hostname, CAS `route_revision`, nullable `active_release_id`, `reconciliation_required`), append-only `route_events` and `route_head_audit_events`, `releases.state` widened with `active`/`retained` (new edges `ready->active`, `active->retained`, `retained->active`; every 000014 rule unchanged), and at most one `active` release per environment (partial unique index).
- Same-environment pointer is structural: composite FK `(active_release_id, environment_id) -> releases(id, environment_id)`; `releases.environment_id` is immutable.
- Deferred constraint triggers enforce at commit that a head points only at an `active` release and that no referenced release leaves `active`. They are deferred so 10c's finalization can retire, activate and repoint in one transaction; they re-read rows because each write queues its own firing.
- `retained -> removing` not added: retention is deferred to slice 11 and no transaction yet enforces the reference check ADR 0005 attaches to that edge.
- `RouteHeadRepository.AssignRouteHostname`: authorization, then `SELECT ... FOR UPDATE` on the head, then decide not-found / immutable / stale from the locked row. Hostname changes are refused once a release is active (typed error; the trigger also refuses). Duplicate hostname maps to a typed error. `/environments/route` admin form requires an authority acknowledgement and accepts exact field names only.
- Route hostnames need at least two labels and a final label that is not all digits (no IP literals, no `localhost`); same literal regex in SQL and `internal/healthgate`.
- Defects found in review and fixed before landing: diagnostic error-class race after a zero-row CAS update; no repository-level test of the immutable rule; IP/single-label hostnames accepted; unknown fifth form key tolerated; no link between `active_release_id` and release state (orchestrator); and a **write skew** in the first deferred triggers, where the head side and release side both passed on committed state and a head ended up pointing at a `retained` release. Orchestrator reproduced it live, and the fix is `FOR SHARE` on the release read. `TestRouteHeadActiveReleaseWriteSkewPrevented` covers both orderings and fails in both when the lock is removed.
- Every guard was broken on purpose and seen to fail.
- Evidence: gofmt, `go vet` incl. integration, Linux amd64/arm64 vet, build, `git diff --check`, `go test -race ./...` (27 packages ok), three fresh PG18 sweeps normal + `-race` (6/6), concurrency incl. write skew and hostname race `-race -count=20`.

### Step 10b — Traefik file-provider adapter (agent; one subagent)
- New `internal/runtime/traefik`: deterministic render of one environment's dynamic config from route-head state, canonical digest, schema validation, no secret values structurally (render input type has no secret fields), atomic write per ADR 0005, Linux/unsupported build split.
- Traefik static config (entrypoints, ACME resolver, watched dir) is host provisioning — documented here, installed by the packaging slice.
- macOS proves render/digest/atomic-write against a temp dir only; real reload timing is a Linux gate.

#### Step 10b result (2026-09-22, verified)
- New `internal/runtime/traefik` (agent-side library; not wired into the executor). Traefik's file provider takes YAML or TOML, not JSON; YAML is hand-rendered as a fixed write sequence (no maps, no new dependency), with a SHA-256 digest over the bytes.
- Backend is `<scheme>://<container-name>:<port>` via `docker.ContainerName`, i.e. Docker DNS on the environment network Traefik joins (ADR 0005 network model), not a container IP. Router `autodeploy-router-<env>`; service `autodeploy-svc-<env>-<release>-<fence>`.
- Atomic write: `O_EXCL|O_NOFOLLOW` temp file with a non-provider extension, fsync, rename, fsync dir; idempotent on equal digest, reports the previous digest; bounded `O_NOFOLLOW` read-back. Directory must be owned by the agent UID or root and not world-writable (expected packaging topology documented in `write.go`).
- Observer: loopback-only Traefik API client (no proxy, no redirects, bounded body); tells not-loaded, loaded-but-wrong and transport errors apart. `not loaded` is `transient` and is safe only under a caller-enforced deadline (10d).
- Defects found in review and fixed: the YAML quoter passed bytes ≥0x80 through (NEL read back as a space and invalid UTF-8 broke parsing in PyYAML and yaml.v3); ASCII-only is now enforced. Also fixed: `O_EXCL` untested, an owner==agent-only directory check that would fail on a root-provisioned host, and an unbounded `os.ReadFile` that followed symlinks.
- Open before activation is enabled: (1) Traefik API auth. ADR 0005 says private and authenticated; the client currently makes unauthenticated loopback requests. Recommendation: keep the API bound to loopback and add an optional credential in the packaging slice. (2) The API JSON shapes come from the docs and have not been observed against a running Traefik. (3) Entrypoint and cert-resolver names are unverifiable here; a typo shows up as a router that never loads.
- Every guard was broken on purpose and seen to fail; the orchestrator re-broke `O_EXCL` independently.
- Evidence: gofmt, vet incl. integration, Linux amd64/arm64 vet+build, `git diff --check`, `go test -race ./...`, concurrent-writer test `-race -count=20`, three fresh PG18 sweeps normal + `-race` (6/6).

### Step 10c — fenced activation, control plane (one subagent)
- Activation continues under the **same lease/attempt** (decision D3): new `/agent/v1/leases/activate` (tx1: recheck authorization, lease, fence, cancellation, route revision, source active release, candidate `ready`; transition `activating`; record switch-authorized operation with digest) and `/agent/v1/leases/activation-result` (tx2: recheck op/revision/fence; advance route revision + active pointer; candidate `active`, predecessor `retained`; deployment `healthy`; append events/audit). Ambiguous/failed after switch-authorized → `reconciliation_required`, environment blocked.
- Lock order: ADR 0004 amendment — route-head rows between deployment heads (6) and secret versions (7).
- Idempotency: same op digest replays as success; conflicting digest fails.
- Control-plane `activationEnabled = false` gate until 10e is reviewed.
- Tests (live PG18): racing activators on one route head → exactly one operation (several goroutines, one row); cancel vs activate race; stale fence refused; replay idempotent; guard tests by breaking the fence recheck.

#### Step 10c orchestrator decisions on the design (2026-09-22)
- **The control plane renders the route document and computes its digest** in tx1, not the agent. The draft had the agent present a digest it cannot compute yet: the hostname and the new route revision only reach it in tx1's response. The control plane imports 10b's pure render function (no host access is implied by linking it; extract the naming helpers if that would pull the Engine client into the control-plane binary) and stores the digest on the operation. The agent re-renders from the response and refuses to write unless its digest matches. The operation's replay key is `(attempt_id, fence, image_digest)`.
- **Entrypoint and cert-resolver names** come from control-plane configuration with V1 defaults and are returned in tx1's response. The agent uses what the response says rather than a second copy of the literals.
- **tx2 locks in ADR 0004 order**: slot, job, attempt, deployment, route head, then release rows by id ascending. The draft started at the attempt.
- **Owner decision (2026-09-22): operator-declared reconciliation outcome ships in 10c-2.** Admin `mark-reconciled` for an activation-caused block requires a declared outcome: `activated` finalizes exactly as tx2's success path, `not_activated` fails the deployment and leaves the route head where it was. Audited, fenced, and refused unless the environment is actually blocked by an activation operation. Without it a blocked environment would need manual SQL.
- Confirmed by reading: `tryClaim` does read `environment_slots.reconciliation_required` (`agent_lease_repository.go:355`), so a blocked environment is genuinely unclaimable. `RequestCancellation` already refuses once the deployment is `activating`, so only `Complete`, the sweeper and `Reconcile` need changing.
- **Gaps in shipped 9b code that 10c must close** (verified): `Complete` never transitions the deployment on success, and nothing anywhere sets `releases.image_digest` or moves a release `candidate -> ready`. The agent's built image digest has never reached the control plane.
- Split confirmed: **10c-1** = migration 000019 + `/leases/activate` + the `activationEnabled` gate; **10c-2** = `/leases/activation-result` + the `Complete` guard + sweeper diversion + the operator-declared reconciliation outcome.

#### Step 10c design (draft for orchestrator review)

##### 1. Where a 9b attempt ends today, and what must change

Verified in `internal/store/postgres/agent_lease_repository.go`'s `Complete` (line ~845):
on `queue.CompletionSucceeded` it closes `agent_job_attempts` to `succeeded`, frees
`environment_slots.current_attempt_id`, closes `agent_jobs` to `succeeded`, and appends a
`succeeded` job event — **it never calls `transitionDeployment` on the success path** (only
`newJobState == "dead" || "failed"` does, line 989). So a passing 9b gate today leaves the
deployment stuck at `health_checking` forever; nothing moves it to `activating`/`healthy`. This
is exactly the gap `deployment.canTransition` already anticipates (`health_checking -> activating`
is the only non-terminal edge out of `health_checking` in `internal/deployment/deployment.go`
line ~296) but no code path takes it.

Also verified: **nothing in the codebase ever sets `releases.image_digest` or moves a release
`candidate -> ready`.** `grep -rn image_digest` outside migrations and tests matches only
`agent_lease_execution.go`'s claim-time insert (`image_digest` NULL) and its comment. The build
result (`buildResult.ImageDigest`, `internal/agent/executor/executor.go` line ~227) is computed
agent-side and is currently never transmitted to the control plane by any endpoint — `progress.go`
carries only a `Phase` enum excluding `activating`/`healthy`, and `Complete`'s wire struct
(`leases.go`) carries no digest field of any release kind. This means **the new `/leases/activate`
request must be the first time the control plane learns the candidate's image digest**, and tx1
must itself perform `candidate -> ready` (setting `image_digest` write-once, satisfying
`validate_release_update`'s `state='ready' requires image_digest IS NOT NULL`) as a precondition
check, not assume some earlier step already did it. This is a real, previously-unrecognized gap in
9b/10a/10b, not an omission in this design — flagged explicitly per the task's "flag every
uncertainty" instruction.

Required changes: `executor.go`'s `Execute` must call `/agent/v1/leases/activate` (not `Complete`)
immediately after a passing gate, carrying `buildResult.ImageDigest`; `Complete` continues to be the
only path for build/health-gate *failure* (unchanged) and becomes, for success, reachable only after
activation's own two transactions finish (see §4's "completion follows activation" point) — `Complete`
itself must refuse to run while an activation operation is open (§4).

##### 2. Migration 000019 schema

`internal/store/postgres/route_head_repository.go`'s own header comment already reserves the ADR
0004 lock slot ("D5: route-head rows ... immediately after deployment aggregate heads (position 6)
and before secret-version/frozen-reference rows (position 7)") without touching the ADR text, and
000018's own comment says `route_events`'s `'activated'` kind was "named now ... ahead of use" for
exactly this step. Both are ready to receive 10c's writes with no further schema change needed on
`route_heads`/`route_events`.

**Reused, not reinvented:** `release_operations` (migration 000014, lines 275-332) is already the
ADR-0005-shaped "one append-only operation ... immutable operation ID/type, canonical digest,
environment route revision, job/attempt/fence, source/target release IDs, state, sanitized outcome"
record — `kind` already includes `'activate'` ("named now ... ahead of use", matching 000018's own
precedent), it already FK's to `release_id`/`job_id`/`attempt_id`/`fence` and validates the
attempt/fence/job_id triple by trigger exactly like `release_runtime_instances` does, and **it has
zero application writers today** (`grep -rln release_operations` over non-test `.go` files: no
hits) — so, like every other 000014/000018 table, it is safe to widen additively. Its current
`state` enum (`'started','succeeded','failed'`) doesn't yet distinguish "authorized, host work not
yet observed" from "in flight", and it has no columns for the *expected* prior state an activation
must recheck against. Design: widen it in place rather than add a parallel table.

Migration `000019` (schema-only, extends 000014's table, never edits 000014 itself — matching
000018's own precedent for 000014's `releases.state`):

```sql
ALTER TABLE release_operations ADD COLUMN environment_id TEXT;
ALTER TABLE release_operations ADD COLUMN expected_active_release_id TEXT
  REFERENCES releases(id) ON DELETE RESTRICT;
ALTER TABLE release_operations ADD COLUMN expected_route_revision BIGINT
  CHECK (expected_route_revision IS NULL OR expected_route_revision >= 0);
```

`environment_id` is denormalized from `releases.environment_id` (same join `route_heads` already
uses) purely so a **partial unique index can express "at most one open activation per environment"
without a cross-table join**, which Postgres partial indexes cannot do. It is validated, not
trusted: extend `validate_release_operation_insert` (already checks fence/job_id against the named
attempt with an unlocked read, because those columns are immutable on `agent_job_attempts`) to also
`SELECT environment_id FROM releases WHERE id = NEW.release_id` and reject a mismatch — same
no-lock reasoning applies (`releases.environment_id` is immutable per `validate_release_update`).
`environment_id` and the two `expected_*` columns stay NULL for `kind IN ('prepare','rollback',
'retire','cleanup')` (only `'activate'` populates them in this slice) — enforced by a CHECK:

```sql
ALTER TABLE release_operations ADD CONSTRAINT release_operations_activate_expectations_check
  CHECK ((kind = 'activate') = (environment_id IS NOT NULL));
-- expected_active_release_id is separately nullable *within* kind='activate' (first
-- deployment: no prior active release — see §7), so it is not tied to kind by a CHECK,
-- only environment_id and expected_route_revision are (route_revision is always known,
-- even 0, once a route head exists, which activation already requires).
ALTER TABLE release_operations ALTER COLUMN expected_route_revision
  SET NOT NULL /* only enforceable together with the kind check above via a second CHECK: */;
ALTER TABLE release_operations ADD CONSTRAINT release_operations_activate_route_revision_check
  CHECK ((kind = 'activate') = (expected_route_revision IS NOT NULL));
```

(The two-CHECK split above is illustrative; the implementing subagent should fold them into one
`CHECK` expression per column, following 000018's `CHECK ((event_kind = 'hostname_assigned') =
(hostname IS NOT NULL))` idiom exactly.)

State widening — this is the switch_authorized → succeeded | reconciliation_required machine the
task asks for, layered onto the existing `started`/`succeeded`/`failed` enum:

```sql
ALTER TABLE release_operations DROP CONSTRAINT release_operations_state_check; -- name per introspection
ALTER TABLE release_operations ADD CONSTRAINT release_operations_state_check
  CHECK (state IN ('started','succeeded','failed','switch_authorized','reconciliation_required'));
ALTER TABLE release_operations DROP CONSTRAINT release_operations_closed_at_check; -- the CHECK((state='started')=(closed_at IS NULL)) shape
ALTER TABLE release_operations ADD CONSTRAINT release_operations_closed_at_check
  CHECK ((state IN ('started','switch_authorized')) = (closed_at IS NULL));
```

For `kind='activate'` only, the legal edges (enforced in `validate_release_operation_update`,
which today unconditionally requires `OLD.state = 'started'` and rejects any further write once
closed — that blanket rule must become kind-aware):

- `started` is never used by `kind='activate'` in this design — tx1 inserts the row directly as
  `switch_authorized` (there is no separate "started, not yet authorized" moment for activation:
  authorization *is* the insert, all in the same transaction as the deployment's
  `health_checking -> activating` move). `started` remains exactly as today for `prepare` and the
  as-yet-unimplemented `rollback`/`retire`/`cleanup` kinds.
- `switch_authorized -> succeeded`: tx2's success path (§4).
- `switch_authorized -> reconciliation_required`: tx2's failure/ambiguous path, or the expiry
  sweeper firing mid-`switch_authorized` (§6).
- `reconciliation_required` is terminal for this table (closed_at set on entry, matching the CHECK
  above) — the *environment's* block is lifted by the existing `POST
  /agent/v1/environments/reconcile` / `mark-reconciled` flow (ADR 0004 amendment, already
  implemented), which is a statement about `environment_slots`/`route_heads`, not a reopening of
  this operation row. This reuses machinery that already exists rather than inventing a second
  reconciliation path — see §4 and §9 for exactly which flags that flow must now also touch.

Linking to `route_events`/deployment events: tx1 does **not** append a `route_events` row (no route
mutation has happened yet — only the deployment moved to `activating` and the operation was
opened); tx2's success path appends `route_events(kind='activated', route_revision=<new>,
active_release_id=<candidate>)` exactly like `route_head_repository.go`'s existing
`insertRouteEvent` helper, reusing that function unchanged. `agent_job_events` gets a new
`activation_authorized` event type from tx1 and `activated` / `activation_reconciliation_required`
from tx2, mirroring `appendAgentJobEvent`'s existing shape (no schema change needed there: it is a
free-text-checked column already, per 000013 — verify the exact CHECK list before adding a new
literal; if it is a closed enum, migration 000019 must widen it too, alongside the `release_operations`
changes above. **Uncertainty**: I did not read `agent_job_events`'s exact CHECK in this pass; the
implementing subagent must check it before assuming the new literals are legal.

##### 3. `POST /agent/v1/leases/activate` (tx1)

**Wire structs** (mirroring `leases.go`'s existing shape — `attempt_id`/`fence` first, one
domain-specific field, hex-digest fields via the existing `decodeDigest`/`encodeDigest` helpers in
`enroll.go`):

```go
type activateRequest struct {
    AttemptID   int64  `json:"attempt_id"`
    Fence       int64  `json:"fence"`
    ImageDigest string `json:"image_digest"` // hex sha256, candidate's built image
    RouteDigest string `json:"route_digest"` // hex sha256, agent-rendered canonical route document
}
type activateResponse struct {
    OperationID          string `json:"operation_id"`
    RouteRevision        int64  `json:"route_revision"`         // the NEW revision the agent must write
    Hostname             string `json:"hostname"`
    EntrypointName       string `json:"entrypoint_name"`
    CertResolverName     string `json:"cert_resolver_name"`
    BackendRuntimeInstanceID string `json:"backend_runtime_instance_id"`
}
```

No `operation_id` is accepted from the agent on this call (unlike `Complete`/`AcknowledgeCancellation`,
which accept an agent-computed `operation_id`/`result_digest` for their own idempotent-replay
scheme) — tx1 **mints** the operation row and its id server-side, because the operation must exist
before the agent does any host work at all (ADR 0005: "Begin the fenced activation operation and
transition to `activating`" precedes "Atomically replace the environment route file"). The agent's
replay key for *this* call is `(attempt_id, fence, ImageDigest, RouteDigest)`: a retried POST with
the identical digests against the same still-`switch_authorized` operation returns the same stored
`OperationID`/response (idempotent replay, per the task's wording); a retried POST with a
*different* `RouteDigest` for the same attempt/fence while `switch_authorized` is refused — see the
"idempotent replay" bullet below. No secret leaves this response — matches ADR 0005 "Route
documents ... contain no secret values"; `Hostname`/`EntrypointName`/`CertResolverName`/backend
identity are exactly `internal/runtime/traefik.EnvironmentRoute`'s non-secret fields.

**Who computes the route digest — agent or control plane?** The agent renders and sends it.
Justification: `internal/runtime/traefik.Render`/its digest function (`render.go`, verified to
produce "a SHA-256 digest over the bytes" per Step 10b's result note) live in `internal/runtime/traefik`,
an **agent-side library not imported by `internal/store/postgres` or `internal/agent/http`** (Step
10b's own result note: "not wired into the executor" — i.e. it is agent code, never control-plane
code, and control-plane binaries do not vendor `internal/runtime/traefik` at all as far as this pass
found). Re-rendering the exact same YAML byte-for-byte on the control plane would require
duplicating that renderer (or importing it into the control-plane binary, which ADR 0005's "no
Docker socket to Traefik" boundary keeps deliberately separate from anything that talks to hosts) —
and even if imported, the control plane does not know `BackendRuntimeInstanceID` (the candidate's
runtime-instance ID) independently of what the agent already computed at container-start time
(`attemptOpaqueID(lease.AttemptID)`, `executor.go`) unless it re-derives that too. The simpler,
already-precedented shape is: the agent renders (as 10b already builds it to do), computes the
digest, and **presents** it; the control plane's job is to recheck it *matches the operation it
already authorized* (idempotent-replay compare, not independent recomputation) and, for the actual
switch-safety property ADR 0005 needs ("a conflicting digest fails"), to compare against what it
told the agent to render (the `RouteRevision`/`Hostname`/candidate identity it already committed in
`activateResponse`) — i.e. tx1 does not need to recompute bytes, only to confirm the operation row
it stored what the agent is about to write and that the resulting digest is the one recorded.
**This means tx1 cannot verify a maliciously wrong digest is byte-correct** — it can only verify
*consistency* (same operation, same digest across a replay). That is an accepted trust boundary
identical to today's: `Complete`'s `result_digest` is agent-computed and control-plane-compared,
never independently recomputed by the control plane either (`Complete`, line ~927:
`digestMatches := ... [32]byte(storedResultDigest) == resultDigest`). **Flagged uncertainty**: ADR
0005 does not say explicitly which side computes the digest; this design infers it from where the
renderer already lives and from the existing `Complete`/`AcknowledgeCancellation` precedent of
agent-computed, control-plane-compared digests. If the orchestrator disagrees, tx1's shape changes
significantly (it would need the full `EnvironmentRoute` struct as input and would need to import
`internal/runtime/traefik` into the control plane).

**Entrypoint/cert-resolver names**: verified there is **no schema column anywhere** for these — not
`environment_runtime_configs` (000017 only added `health_gate_path`/`health_gate_host`), not
`route_heads` (000018 has no such columns). Step 10b's own result note calls Traefik's static
config, including entrypoints and the ACME resolver, "host provisioning — documented here,
installed by the packaging slice," and `route.go`'s own comment on `EntrypointName`/
`CertResolverName` calls them "static Traefik configuration objects ... provisioned by host
packaging, not database-controlled identifiers." Given V1 is one Traefik instance per host with
one entrypoint and one ACME resolver (no multi-entrypoint/multi-resolver requirement stated
anywhere in ADR 0005), the simplest consistent design is: **compile-time/config constants shared by
the control plane and the agent** (e.g. `"websecure"` / `"letsencrypt-production"`), not a database
row — there is exactly one value system-wide in this slice, so a DB column would be a CAS-tracked
mutable value for something that never varies per environment. **Flagged uncertainty**: this is
inferred, not decided by any ADR text; if a future slice needs per-server or per-environment
entrypoints, this becomes a real config table then. For 10c, recommend a small
`internal/runtime/traefik` (or new `internal/activation`) package-level constant plus a
control-plane config flag of the same value, verified equal at startup (fail closed if they
disagree) rather than trusting two independently-maintained literals to never drift.

**Every ADR 0005 recheck**, mapped onto verified code:

| ADR 0005 recheck | Verified mechanism |
|---|---|
| current authorization | `lockActiveServerCredential` (existing helper, same as `Complete`) |
| lease authority | attempt row `FOR UPDATE`; `authority_expires_at.After(now)` (mirrors `Complete` line 937) |
| operation digest | compare presented `RouteDigest`/`ImageDigest` against any already-stored operation for this attempt/fence (replay path) |
| environment fence | `attemptFence == fence && slotFence == fence && slotCurrentAttemptID == attemptID` (mirrors `Complete` line 937 verbatim) |
| route revision | lock `route_heads FOR UPDATE` (new — no existing caller takes this lock for write), compare to a caller-independent value: **tx1 does not accept a client-presented "expected route revision"** — it reads the current `route_heads.route_revision` itself under the lock and uses that as `expected_route_revision` in the operation row. (Unlike `AssignRouteHostname`'s CAS, the agent does not know the route revision independently — it never sees it until this response — so there is nothing for it to assert; the control plane is the sole source of truth here.) |
| source active release | read `route_heads.active_release_id` under the same lock; NULL is legal (§7, first deployment) |
| candidate readiness | lock the candidate `releases` row `FOR UPDATE`; if `state='candidate'`, perform the `candidate->ready` transition described in §1 (set `image_digest` from the request, matching the write-once trigger, then `state='ready'`) in the *same* tx1 transaction, then require `state='ready'` before continuing — a `state` other than `candidate`/`ready` (e.g. `failed`) is refused |
| cancellation state | `agent_jobs.cancel_requested_at IS NULL` under the job row lock (same row `RequestCancellation` locks) |

**Deployment transition**: `transitionDeployment(ctx, tx, r.deployments, deploymentID,
deployment.StatusActivating, now, r.inject)` — reuses the existing helper (`agent_lease_repository.go`
line 583) exactly as `Complete`/`RequestCancellation` already do; `canTransition` already allows
`health_checking -> activating` so no `deployment` package change is needed.

**Idempotent replay**: after locking the attempt, check for an existing `release_operations` row
`WHERE attempt_id=$1 AND fence=$2 AND kind='activate'`. None found → proceed as above (mint a new
row). Found with `state='switch_authorized'` and `digest` equal to a canonical hash of
`(ImageDigest, RouteDigest)` (the operation's own `digest` column, reusing 000014's existing
`digest BYTEA` field for this composite rather than adding two more digest columns) → return the
same stored `OperationID`/response, no further mutation (true replay, matching `Complete`'s
"already accepted sequence" idiom). Found with a **different** digest while still
`switch_authorized` → `ErrAgentLeaseIntegrityConflict` (existing sentinel, same one `Complete`
uses for a conflicting replay) — "a conflicting digest fails," per ADR 0005 verbatim. Found already
`succeeded`/`reconciliation_required` → also a conflict/stale response (this call cannot be
reached again for that attempt/fence in the intended flow, since completion is gated on the
operation being closed — see §4 — but the check must exist defensively).

**Lock order** (ADR 0004 hierarchy §9, route heads inserted between position 6 and 7 per D5):

1. Authorization: server credential (`lockActiveServerCredential`, `FOR SHARE`) — position 1.
2. Environment execution slot (`environment_slots FOR UPDATE`) — position 3 (position 2, webhook/
   automatic-intent rows, is enqueue-only and never touched here, matching every existing lease
   endpoint).
3. Logical job row (`agent_jobs FOR UPDATE`) — position 4.
4. Attempt row (`agent_job_attempts FOR UPDATE`) — position 5.
5. Deployment aggregate head (`deployments FOR UPDATE`, via `transitionDeployment`'s existing lock —
   verify it takes `FOR UPDATE`; `RequestCancellation`'s leased branch already locks it explicitly
   at line 1123) — position 6.
6. **Route head (`route_heads FOR UPDATE`) — new position "6b", per D5, before secret versions.**
7. Candidate release row (`releases FOR UPDATE`) — this slice has no secret-version freeze on the
   activate path (secrets were already frozen at claim time, per ADR 0003/000013), so nothing
   occupies position 7 here; the release row is not itself in ADR 0004's original list (it postdates
   the ADR) but belongs logically alongside "secret-version and frozen-reference rows" as a
   per-deployment resource row locked after the route head, consistent with 000018's own note that
   route heads sit **before** secret-version rows, i.e. `releases` locks after `route_heads` in this
   transaction's own order even though ADR 0004 never enumerated it. **Flagged uncertainty**: ADR
   0004's hierarchy predates `releases` entirely; this ordering (route head before release) is this
   design's own choice, consistent with D5's stated intent but not literally specified by either ADR.
8. Append-only inserts (`release_operations`, `agent_job_events`) — position 8, last, per hierarchy.

##### 4. `POST /agent/v1/leases/activation-result` (tx2)

**Wire**: `{attempt_id, fence, operation_id, succeeded bool, observed_route_digest string}` on
success, or `{attempt_id, fence, operation_id, succeeded: false, failure_summary string}` (bounded/
sanitized, matching `release_operations.outcome_summary`'s own CHECK) on failure/ambiguity — a
single struct with `Succeeded bool` and an optional summary is simplest, mirroring `completeRequest`'s
own `Outcome`/`FailureClass` split shape.

**Success path** — recheck, then in one transaction:

1. Lock attempt (`FOR UPDATE`), verify `attemptFence == fence`, authority still unexpired *is not*
   required here in the same way as `Complete` — ADR 0005: "An in-flight syscall cannot be
   preempted," and the whole point of `switch_authorized` is that authority may have lapsed while
   the agent was mid-syscall; tx2 instead requires the **operation** to still be `switch_authorized`
   for this exact `attempt_id`/`fence` (its own identity columns, immutable per
   `validate_release_operation_update`), which is the actual linearization point, not the lease
   clock. **This is a deliberate, load-bearing difference from every other lease endpoint's
   `authority_expires_at.After(now)` check**, and must be called out to reviewers explicitly: if the
   implementing subagent copies `Complete`'s authority check here unmodified, a legitimately
   authorized activation whose lease later expired mid-syscall would be wrongly rejected, directly
   contradicting "An in-flight syscall cannot be preempted."
2. Lock `route_heads FOR UPDATE`; recheck `route_revision == operation.expected_route_revision` and
   `active_release_id IS NOT DISTINCT FROM operation.expected_active_release_id` (both must still
   match what tx1 recorded — nothing else may have moved the head in between, which D5/the deferred
   triggers already guarantee structurally but this is the *operation's own* linearization check,
   independent of the trigger).
3. Lock the candidate `releases` row `FOR UPDATE`; require `state='ready'`.
4. If `operation.expected_active_release_id` is non-NULL, lock that predecessor `releases` row
   `FOR UPDATE FOR SHARE`... **actually `FOR UPDATE`**, since this transaction is about to write
   its state (`active -> retained`) — note 000018's write-skew fix uses `FOR SHARE` inside the
   *deferred trigger*, which fires automatically at commit; tx2 itself should take `FOR UPDATE`
   directly on both the predecessor and candidate rows since it is the one performing the writes,
   and the deferred trigger becomes a second, redundant-but-harmless line of defense exactly as
   000018 designed it to be for any writer, not just this one.
5. Write, in this order (all still inside tx2, deferred triggers fire at commit):
   - `releases`: predecessor `active -> retained` (skip if no predecessor, §7).
   - `releases`: candidate `ready -> active`.
   - `route_heads`: `active_release_id = candidate`, `route_revision = route_revision + 1`,
     `updated_at = clock_timestamp()` (single UPDATE, satisfying `validate_route_head_update`'s
     CAS-by-exactly-one trigger).
   - `route_events`: append `kind='activated'`, `route_revision=<new>`, `active_release_id=<candidate>`
     (reuses `insertRouteEvent`).
   - `release_operations`: `switch_authorized -> succeeded`, `closed_at=now`, `outcome_summary`
     from the sanitized `observed_route_digest`/success note.
   - `deployments`: `activating -> healthy` via `transitionDeployment` (already a legal edge per
     `canTransition`).
   - `agent_job_attempts`: close `succeeded` (this is where completion actually happens now — see
     "completion follows" below).
   - `environment_slots`: free `current_attempt_id` (same predicate as `Complete` today:
     `WHERE environment_id=$1 AND current_attempt_id=$2`).
   - `agent_jobs`: `state='succeeded'`.
   - `agent_job_events`: append `activated`, then the existing `succeeded` job event.
6. Commit.

**Completion folded into tx2, not a separate `Complete` call.** This directly answers the "how does
completion follow, and how must it refuse to complete an attempt whose activation is open" part of
the task: the answer is that **for the activation path, `Complete` is never called again after
`/activate`** — tx2 *is* the completion transaction for a successful activation, performing exactly
the same attempt-close/slot-free/job-close/event steps `Complete` performs today, plus the
activation-specific writes, in one atomic unit (ADR 0004 "Transaction boundaries": "Completion/
failure, slot release, deployment transition, and events" must commit atomically — tx2 satisfies
this for the activation case exactly as `Complete` does for the non-activation case). The executor
therefore stops calling `/leases/complete` on the success path entirely once activation ships;
`Complete` remains reachable only for build/health-gate failure before `/activate` was ever called.
**`Complete`'s own guard**: add, at the top of `Complete`'s attempt-state check (after loading the
attempt row), a check for an open activation: `SELECT 1 FROM release_operations WHERE attempt_id=$1
AND fence=$2 AND kind='activate' AND state='switch_authorized'` — if found, return a new sentinel
(e.g. `ErrAgentLeaseActivationOpen`) rather than proceeding, so a stray/duplicate `Complete` call
racing an in-flight activation can never free the slot or close the job out from under tx2. This
mirrors `RequestCancellation`'s existing `ErrAgentJobActivating` guard (which checks
`deployments.status == activating` directly) but checks the **operation** row instead of deployment
status, because by the time tx2 runs, deployment status has already flipped to `healthy` — a
`Complete` call arriving *after* tx2 already succeeded must instead hit the ordinary "attempt already
closed, replay-or-conflict" branch that exists today, which is unaffected by this change.

**Failure/ambiguous path** — recheck the operation is still `switch_authorized` for this attempt/
fence (same as step 1 above; if it is not, this is itself a stale/conflicting call and should be
rejected, not silently accepted as a second failure report), then in one transaction:

1. `release_operations`: `switch_authorized -> reconciliation_required`, `closed_at=now`,
   `outcome_summary` from the sanitized failure summary.
2. `route_heads`: `reconciliation_required = true` (no revision bump — the desired state did not
   move; this is exactly the column 000018 added "so this slice's scope note says resolution is the
   existing operator mark-reconciled flow, not new code here").
3. `environment_slots`: `reconciliation_required = true`, `reconciliation_attempt_id = attemptID`
   (reusing the **existing** ADR 0004 mechanism `closeExpiredOrRevokedAttempt` already uses) —
   **but do not free `current_attempt_id`** and **do not close the `agent_job_attempts`/`agent_jobs`
   rows to any terminal state**. This is the "environment remains blocked" ADR 0005 requires:
   the slot stays occupied by this exact attempt/fence indefinitely (ADR 0004's "no-slot-reuse"
   rule), and the job/attempt/deployment stay exactly where they are (`activating`/`leased`) — there
   is no legal `deployment.canTransition` edge out of `activating` other than `-> healthy` or `->
   failed`, and ADR 0005 explicitly forbids resolving ambiguity as either ("never resolved by blind
   automatic rollback"). The **existing** `POST /agent/v1/environments/reconcile` (owning server,
   matching attempt/fence) or the session-authenticated `mark-reconciled` break-glass are the only
   two ways out, exactly as the ADR 0004 amendment already documents — and per that amendment's own
   text, whichever one runs must (this slice's necessary but not-yet-existing addition) also finish
   the deployment/attempt/job/route-head resolution once the operator/agent supplies the real
   outcome; **that finishing logic is out of 10c's stated scope** (10c's own bullet says "environment
   blocked" is the end state) and is a gap the plan should either accept as deferred-to-later-slice
   or explicitly assign — flagged as an open question for the orchestrator, since `Reconcile`
   today only clears `environment_slots.reconciliation_required` and appends a `reconciled` event; it
   does **not** touch `route_heads.reconciliation_required`, `release_operations.state`, or the
   deployment status at all in its current (10a/pre-10c) form. **This means Step 10c must also
   extend `Reconcile`** (not just add the two new endpoints) so that clearing reconciliation for an
   activation-caused block also resolves the operation/route-head/deployment/attempt/job rows
   consistently — otherwise "operator resolution" has no code path that actually unblocks anything
   for this specific cause. This is the single largest scope question in this design; see §11.
4. Commit. No `agent_job_events` "failed"/"dead"/"retry_scheduled" event — this is not a retry-
   eligible failure by ADR 0005's own words ("It does not permit a different attempt to activate
   concurrently"); append a new `activation_reconciliation_required` event instead (subject to the
   `agent_job_events` CHECK-widening flagged in §2).

##### 5. Cancellation vs activation race

Verified: `RequestCancellation`'s `leased` branch already has the exact guard needed —
`if deploymentStatus == string(deployment.StatusActivating) { return ErrAgentJobActivating }`
(line 1126), checked **after** locking the deployment row (line 1123), which is locked **after**
the job row (line 1062) and **before** the environment slot lock at line 1056 — wait, verified
order in `RequestCancellation` is: admin session (no lock helper name, but `lockAdministratorSession`)
→ environment slot `FOR UPDATE` (line 1056, via a peeked environment ID) → job `FOR UPDATE` (line
1062) → attempt `FOR UPDATE` (line 1119, leased branch only) → deployment `FOR UPDATE` (line 1123).
This is **already** slot-before-job-before-attempt-before-deployment, i.e. positions 3-4-5-6 in
order, but deployment (position 6) is locked *after* attempt (position 5) here, whereas tx1 above
locks deployment via `transitionDeployment` and then additionally needs `route_heads` afterward —
**no conflict**: cancellation never touches `route_heads` at all, so the two paths only ever
contend on the shared prefix (slot → job → attempt → deployment), and standard lock-ordering
reasoning applies: whichever transaction locks the deployment row first wins the race outright,
because by the time either reaches the deployment lock it has already committed to a course of
action that the other side's own check (tx1's `cancel_requested_at IS NULL` check, or
`RequestCancellation`'s `deploymentStatus == activating` check) will observe consistently:

- **Cancel locks deployment first** (already `health_checking`, not yet `activating`): cancel sees
  `deploymentStatus != activating`, proceeds to cap deadlines and mark `cancel_requested_at`,
  commits. tx1 (activate), running concurrently, blocks on the deployment lock until cancel
  commits, then re-reads and finds `cancel_requested_at IS NOT NULL` → **tx1 must refuse** ("cancel
  after switch-authorized must be refused" is the ADR's phrasing for the reverse order; the forward
  order — cancel-before-authorize — is simply "cancellation observed, activation refused," already
  implied by tx1's own cancellation-state recheck in §3's table).
- **Activate locks deployment first**: tx1 transitions `health_checking -> activating` and commits.
  `RequestCancellation`, blocked on the same deployment lock, then re-reads `deploymentStatus ==
  activating` and returns `ErrAgentJobActivating` — **no change needed to this existing code path
  at all**; it already refuses correctly, because activation already committed by the time
  cancellation's own lock is granted. This is the "cancel after switch-authorized must be refused"
  behavior the task asks to "show where it must change" — **verified finding: it does not need to
  change**, `RequestCancellation` already has this guard from Step 3's own work, predating this
  slice. The only change this slice makes to the cancellation path is the `Complete`
  activation-open guard in §4 (a different endpoint, not `RequestCancellation`).

**Lock order both paths take** (shared prefix, per above): environment slot → job → attempt (leased
branch only) → deployment. tx1 additionally continues to route head → candidate release after
deployment; `RequestCancellation` never proceeds past deployment. Since neither path acquires a lock
the other needs *after* diverging, there is no deadlock risk from the divergence itself — both paths
already lock the shared prefix in the identical order today.

##### 6. Expiry/sweeper vs an open `switch_authorized` operation

Verified `expireOne` (line 1331): it locks slot → job → attempt, checks `authorityExpiresAt.After(now)`,
and if expired calls `closeExpiredOrRevokedAttempt`, which unconditionally closes the attempt to
`expired`/`cancelled`, advances the slot fence, marks `environment_slots.reconciliation_required=true`,
and — critically — **transitions the deployment to `failed`/`cancelled`** via `transitionDeployment`
when the resulting job state is `dead`/`cancelled` (lines 796-807). **This is wrong for an attempt
whose operation is `switch_authorized`**: ADR 0005 says ambiguity is "never resolved by blind
automatic rollback," but `closeExpiredOrRevokedAttempt` as written would advance the fence (freeing
the slot for *reuse* pending reconciliation — actually it does NOT free `current_attempt_id`, it
sets it NULL at line 764 while also setting `reconciliation_required=true` — re-reading: it clears
`current_attempt_id` to NULL *and* sets `reconciliation_required=true` in the same statement, which
is the existing ADR 0004 "in-flight syscall, block slot reuse pending reconciliation" shape, not an
actual slot release for reuse — `reconciliation_required=true` is what actually blocks new claims,
confirmed by needing to check `Claim`'s candidate predicates for this flag, not read in this pass;
**flagged uncertainty**: I have not verified `tryClaim` actually checks `environment_slots.
reconciliation_required` before claiming a new candidate for that environment — this must be
confirmed, since if it does not, this whole mechanism does not actually block reuse and is a
pre-existing gap unrelated to 10c) — and it would incorrectly transition the deployment/job to a
terminal failed/cancelled state and schedule (or skip) a retry, contradicting "no replacement
attempt is leased meanwhile" and "no auto-retry."

**Required change**: `expireOne` (or `closeExpiredOrRevokedAttempt`) must check, before doing any
of its existing work, whether `release_operations` has an open (`switch_authorized`) `kind='activate'`
row for this `attempt_id`/`fence`. If so:
- Do **not** close the attempt to `expired`/`cancelled` (its terminal-state meaning would be wrong —
  the syscall may still be running or may have already succeeded on the host, unresolved from the
  database's point of view).
- Do **not** touch `agent_jobs.state` or call `transitionDeployment` at all.
- Do set `environment_slots.reconciliation_required=true` / `reconciliation_attempt_id=attemptID`
  (same durable block, so no new claim can occupy this environment).
- Optionally transition `release_operations` itself `switch_authorized -> reconciliation_required`
  right here (rather than waiting for a `/activation-result` that may now never arrive, since the
  agent's local executor is the only thing that knows whether the syscall is still running, and the
  control plane cannot distinguish "slow" from "lost" purely from lease expiry) — recommended, since
  it gives the operator-facing state a name (`reconciliation_required`) instead of leaving it silently
  at `switch_authorized` forever with no durable signal that expiry, not just slowness, occurred.
- Leave `agent_job_attempts.state='leased'` (not closed) so a legitimate late
  `/leases/activation-result` from the agent (if the syscall does eventually finish and report back)
  can still be evaluated against real state rather than hitting a "no matching attempt" wall — though
  by ADR 0005's own reconciliation model, a *late* result at this point should itself route to
  reconciliation rather than blind success/failure finalization; the safest form is for
  `/activation-result` to check `release_operations.state=='reconciliation_required'` (already set by
  the sweeper above) and, in that case, record the late report as reconciliation evidence rather than
  finalizing the transaction — **this exact behavior is not fully specified by ADR 0005 and is
  flagged as an implementation judgment call for the subagent, to be confirmed against "It then
  resumes/finalizes the same operation, restores the last verified route when safe, or requires
  operator recovery."**

##### 7. First deployment and repeat activation

**First deployment** (`route_heads.active_release_id IS NULL`, no predecessor): tx1's "source active
release" recheck (§3) simply reads `active_release_id` as NULL and stores
`expected_active_release_id = NULL` in the operation row (nullable, per §2's schema). tx2's success
path skips step 4 (predecessor `active -> retained`) entirely when `expected_active_release_id IS
NULL` — no predecessor row exists to lock or transition. Everything else is unchanged: the route
head still advances `route_revision` by exactly one and gets its `active_release_id` set for the
first time (`validate_route_head_update`'s hostname-immutability check only fires when
`OLD.active_release_id IS NOT NULL`, so the very first activation is unaffected by it).

**Repeat activation of the same release** (re-running the same already-`active` release, e.g. after
a crash before the agent ever got the tx1 response, or an operator retrying a no-op activation):
tx1's candidate-readiness recheck (§3) requires the candidate release's state to be `candidate` or
`ready` before proceeding to `ready` — a release that is **already `active`** fails this check
outright (not `candidate`/`ready`), so tx1 refuses with a typed error before minting any operation.
This is correct: ADR 0005's release state machine has no `active -> active` edge, and re-activating
an already-active release is not a distinct intent this slice models (that would be closer to a
no-op "verify current route" operation, out of scope). **Flagged as a real product question, not
just an implementation detail**: is "the deployment that is already active gets re-attempted" even
reachable in practice? It would require a new deployment job whose release row is the *same*
`releases.id` as the currently active one — but `releases.id` is `deploymentID` (per
`ensureRelease`'s doc comment: "releaseID reuses deploymentID directly"), and each deployment has
exactly one release (UNIQUE(deployment_id)), so a *different* deployment can never name an
already-active release as its own candidate. The only way this path is reachable at all is a
**retry of the same job/deployment** after a prior attempt's activation already succeeded — which
cannot happen either, because a successful tx2 already closed the job `succeeded` (terminal,
`Claim`/retry logic never revisits a `succeeded` job). So "repeat activation of the same release" is,
on inspection, **unreachable in the intended flow** and only matters as a defensive check against a
corrupted/adversarial request — worth stating plainly in the design rather than treating as a live
scenario requiring special-case product behavior.

##### 8. `activationEnabled` gate

Mirrors the existing `executorEnabled`/`Authenticator`-gated pattern (`claim.go`'s
`executor_available=false` short-circuit is the closest precedent, per `handler.go`'s comment on
`Authenticator`: "used only where a route must gate business logic ... without performing that
logic"). Recommend a control-plane-side boolean (config/env, not a DB row, matching
`executorEnabled`'s own home — **uncertainty: I did not locate where `executorEnabled` itself is
actually stored/read in this pass**; the implementing subagent should mirror that exact mechanism
for consistency rather than inventing a second gating convention) checked at the very top of
`handleActivate`/`handleActivationResult`, before touching any repository: while disabled, both
endpoints return `503 service_unavailable` (the existing `h.writeError(w, http.StatusServiceUnavailable,
"service_unavailable")` idiom every nil-dependency check already uses) — an agent that reaches a
passing gate while `activationEnabled=false` simply cannot advance past `health_checking`'s "next
call" and its lease eventually expires through the ordinary (non-`switch_authorized`, since tx1 never
ran) expiry path, retrying `health_checking` from scratch on the next attempt — acceptable, since
`activationEnabled=false` means "review not yet complete, do not activate anything," not "special
handling for gated deployments."

##### 9. ADR 0004 amendment text (new lock class)

Proposed addition to `docs/adr/0004-postgresql-queue-leases.md`'s existing "Amendments (2026-09-17)"
section (a **new** dated amendment block, never editing the existing 2026-09-17 entries — matching
000018's own migration-file discipline of never editing a prior migration):

> **Amendments (2026-09-22)**
>
> The fenced activation slice (migration 000019; `internal/store/postgres/agent_lease_repository.go`
> `/agent/v1/leases/activate` and `/agent/v1/leases/activation-result`) inserts one new row class
> into the global lock hierarchy defined above, between position 6 (deployment aggregate heads) and
> position 7 (secret-version and frozen-reference rows):
>
> - **6b. Route head rows (`route_heads`) by environment ID**, then the **candidate and predecessor
>   release rows (`releases`)** they govern, by release ID ascending when both must be locked in the
>   same transaction.
>
> Activation's own database transactions (tx1 `/activate`, tx2 `/activation-result`) lock
> authorization, slot, job, attempt, and deployment rows in the existing order (positions 1, 3-6),
> then route head and release rows (position 6b) before any append-only event insert (position 8).
> No activation transaction locks secret-version rows at all (secrets are frozen once at claim time,
> per the original ADR text and ADR 0003); position 7 is simply skipped by these transactions, which
> is safe because skipping an unused position in a total lock order cannot introduce a cycle with a
> transaction that does use it.
>
> The expiry sweeper (`expireOne`/`closeExpiredOrRevokedAttempt`) must recognize an open
> `switch_authorized` activation operation for the attempt it is about to expire and divert to
> marking the environment `reconciliation_required` without closing the attempt/job/deployment to any
> terminal state — see the fenced-activation design note in `tasks/todo.md` Step 10c for the full
> reasoning; this amendment records only the lock-order and "in-flight syscall cannot be preempted"
> consequence, which the original ADR text already states in principle ("expiry/cancellation during
> an in-flight host mutation marks the environment `reconciliation_required`") but which no code
> implemented before this slice.

##### 10. Must-prove test list (live PG18)

Functional/recheck:
- Full happy path: claim → gate pass → `/activate` (candidate `candidate->ready`, deployment
  `activating`, operation `switch_authorized`) → `/activation-result` success (predecessor
  `retained`, candidate `active`, route head repointed + revision+1, deployment `healthy`, attempt/
  job `succeeded`, slot freed) — first deployment (no predecessor) and a second deployment
  (predecessor present), both.
- `/activate` refuses: wrong fence, wrong server, cancelled job, candidate not `candidate`/`ready`,
  candidate already `active` (§7), missing route head (never assigned a hostname).
- `/activation-result` refuses: operation not `switch_authorized` (already resolved, or never
  authorized), route revision/active-release mismatch against the operation's own expectations,
  wrong fence.
- Idempotent replay: repeated `/activate` with identical `(ImageDigest, RouteDigest)` returns the
  same operation/response without a second state change; repeated `/activation-result` success
  replay is a no-op (second call after commit observes the operation already `succeeded` and
  returns success without re-mutating); conflicting digest on either call is refused.
- `Complete` refuses while an activation operation is `switch_authorized` for that attempt/fence
  (§4's new guard) — and still replays/refuses correctly for every *existing* `Complete` scenario
  unchanged (regression coverage, not just new coverage).

Concurrency (goroutines against one live PG18, matching Step 10a's own
`TestRouteHeadActiveReleaseWriteSkewPrevented` style):
- Racing `/activate` calls for the same environment/attempt (simulating a retried agent call under
  network uncertainty) → exactly one `release_operations` row ever reaches `switch_authorized`, the
  rest observe the replay or conflict path — never two.
- `/activate` racing `RequestCancellation` on the same job, both orderings (§5) → exactly one side
  wins, the other observes a consistent refusal, never both succeeding.
- `/activate` racing the expiry sweeper on the same attempt (lease expires the instant it's granted)
  → either tx1 commits first (sweeper then must see `switch_authorized` and divert, §6) or the
  sweeper closes the attempt first (tx1 then must see the attempt no longer `leased` and refuse) —
  never both "succeeding" into inconsistent state.
- Stale fence presented to either `/activate` or `/activation-result` after a fence bump elsewhere
  (e.g. via `Reconcile`/`RevokeServer`) → refused.
- `/activation-result` replay racing a second, conflicting `/activation-result` call for the same
  operation → exactly one write wins; the other sees the now-`succeeded`/`reconciliation_required`
  state and is rejected as stale/conflicting, not accepted as a second finalization.
- Deferred-trigger handoff under concurrency: tx2's explicit `FOR UPDATE` locks on predecessor/
  candidate `releases` rows (§4 step 4) must not reintroduce the exact write-skew window
  000018's `TestRouteHeadActiveReleaseWriteSkewPrevented` already closed — add a variant of that
  test driven through tx2's actual code path (not raw SQL) to prove the application-level locking
  and the deferred trigger agree.

Guard tests (break each recheck on purpose, prove the test fails, matching every prior step's
discipline): fence recheck, route-revision recheck, active-release-id recheck, candidate-state
recheck, cancellation recheck, the `Complete` activation-open guard, and the sweeper's
`switch_authorized` diversion (remove each check one at a time, observe the specific guard test
fail, then restore it).

##### 11. Sizing recommendation

**Split into two subagents, not one**, given the scope actually uncovered in this design pass —
larger than the task's own framing assumed:

- **10c-1: schema (migration 000019) + `/agent/v1/leases/activate` (tx1) + the `activationEnabled`
  gate.** Self-contained: it only ever *reads* `route_heads`/`releases` state and opens an operation;
  nothing it does is hard to unwind if 10c-2 needs rework. Includes the `agent_job_events` CHECK
  investigation from §2 and the `release_operations` widening.
- **10c-2: `/agent/v1/leases/activation-result` (tx2) + the `Complete` activation-open guard + the
  expiry-sweeper diversion (§6) + the `RequestCancellation`/`Reconcile` extension needed to actually
  resolve a `reconciliation_required` activation (§4's flagged scope gap).** This half is
  substantially larger and riskier than 10c-1: it touches three existing, already-shipped functions
  (`expireOne`/`closeExpiredOrRevokedAttempt`, `Complete`, `Reconcile`) rather than only adding new
  ones, and its correctness depends on getting the "in-flight syscall cannot be preempted" semantics
  right in each of them consistently. Splitting it out lets its review focus entirely on
  interactions with existing code, rather than sharing a review pass with the more mechanical tx1
  addition.

**Recommend resolving §4's flagged scope gap (does 10c also extend `Reconcile` to actually finish an
activation-blocked environment, or is that explicitly deferred to a later slice with `Reconcile`
staying as-is and the environment simply staying blocked until a manual, not-yet-designed recovery
procedure runs) before either subagent starts** — it changes 10c-2's size substantially and is a
product/scope decision, not an implementation detail this design can settle unilaterally.

#### 10c-1 review finding, recorded for 10c-2 (2026-09-23, not implemented in 10c-1)

Migration 000019's `release_operations_environment_open_activation_idx` partial unique index
(`UNIQUE(environment_id) WHERE kind='activate' AND state='switch_authorized'`) means: once
`activationEnabled` flips, an environment left with an un-closed `switch_authorized` row (a crash
between `Activate` committing and `/leases/activation-result` ever running, or any other abandoned
deploy) will make **every subsequent `Activate` call for that same environment fail the unique index
outright** with a raw PostgreSQL `23505` unique-violation error. `Activate`'s own idempotent-replay
branch only recognizes a *matching* `(attempt_id, fence, kind='activate')` row; a *different*
attempt/fence hitting the same still-open environment never reaches that branch at all -- it fails
at the `INSERT` with a database error `mapError` (`internal/agent/http/handler.go`) has no case for,
so it falls to the generic default 503 `service_unavailable`, and the agent's client
(`internal/agent/client/httpclient.go` `mapStatus`) treats that as `ErrServiceUnavailable` --
retryable. A permanently stuck environment (nothing will ever unblock it without operator/agent
action) would therefore look exactly like ordinary transient backpressure to every layer above the
database, indefinitely.

**10c-2 must**: (a) detect this specific unique-violation (`pgErr.Code == "23505"` on constraint
`release_operations_environment_open_activation_idx`, mirroring `isUniqueViolation`'s existing
pattern in this package) inside `Activate` and translate it into a typed, non-retryable refusal
(a new sentinel, mapped to its own 409-family wire code, not 503) rather than letting the raw
constraint violation surface; and (b) make sure the reconciliation path
(`/agent/v1/environments/reconcile`, `MarkReconciled`) can actually clear a stranded open
`switch_authorized` operation for this cause -- today neither touches `release_operations` at all
(see §4's already-flagged gap above), so an environment wedged this way has no code path back to
claimable even via break-glass until 10c-2 adds one. This is a prerequisite for 10c-2's own
`/leases/activation-result` and sweeper-diversion work, not a 10c-1 defect: 10c-1 never leaves an
operation open on its own (it only ever inserts a fresh `switch_authorized` row inside one
transaction that either commits whole or rolls back whole), so this scenario is unreachable until a
real "in-flight syscall never reports back" failure mode exists, which is exactly what 10c-2 adds.

#### Step 10c-1 result (2026-09-23, verified)
- Migration `000019` widens `release_operations` (`environment_id`, `expected_active_release_id`, `expected_route_revision`, `route_digest`), widens its `state` CHECK with `switch_authorized`/`reconciliation_required`, adds a partial unique index for "at most one open activation per environment", makes `validate_release_operation_update` kind-aware, and widens `agent_job_events` with `activation_authorized`. Constraint names were introspected, not assumed: the `(state='started')=(closed_at IS NULL)` CHECK is really `release_operations_check1`, not the design draft's guessed `release_operations_closed_at_check`.
- `Activate` (tx1) mints one `switch_authorized` operation, performs the `candidate -> ready` transition with the write-once `image_digest` (the first time the agent's built digest ever reaches the control plane — the 9b gap §1 identified), and moves the deployment `health_checking -> activating`. Lock order: credential (SHARE) -> slot -> job -> attempt -> deployment -> route head -> candidate release -> append-only inserts.
- Binding decision applied: **the control plane renders the route document and computes its digest**, not the agent. This required extracting `internal/runtime/dockerid` (pure leaf package) so that importing `internal/runtime/traefik` into the control plane no longer drags in `internal/runtime/docker`'s Engine socket client. `go list -deps` confirms the dependency is gone. Entrypoint/cert-resolver are shared Go constants (`traefik.DefaultEntrypointName`/`DefaultCertResolverName`) rather than two configs checked for equality at startup — one value, so it cannot drift.
- `activationEnabled = false` in `cmd/controlplane/main.go` mirrors `executorEnabled`: the repository is simply not wired, and the existing nil-dependency branch answers 503. `TestActivationEnabledStaysFalse` guards it.
- Defects found in review and fixed: both new sentinels fell through `mapError` to the generic **503, which the agent client classifies retryable** — a permanent "no hostname assigned" or "candidate in a terminal state" would have been retried to lease expiry, and was indistinguishable from the gate's own 503; now 409 `no_route_head` / `candidate_not_ready`. Also fixed: a `LeaseActivator` doc comment attached to `LeaseCanceller`, and `fmt.Errorf("...: %w", err)` on a branch where `err` is nil.
- Independent zero-context review (PASS WITH NOTES) confirmed 000019's `CREATE OR REPLACE`d trigger functions preserve every 000014 rule, and found the cancel-vs-activate "race" test was **sequential** — it would have passed with every `FOR UPDATE` in both methods deleted. Replaced with `TestAgentLeaseActivateGenuinelyRacesRequestCancellation`: a third transaction holds the slot row, both calls block on it (confirmed via `pg_stat_activity`, not a sleep), then contend for real; asserts exactly one winner and the matching committed end state. Proved non-vacuous by removing `Activate`'s slot `FOR UPDATE` and watching it fail.
- Review also corrected a wrong causal claim in `Activate`'s comments and the ADR 0004 amendment: both races are decided at the **environment slot row (position 3)**, not the attempt (5) or deployment (6) row. Conclusions were right; the stated mechanism was not — the kind of error that later convinces someone a lock is removable.
- Evidence: gofmt, `go build`, `go vet`, `make check`, `go test -race ./...`, three fresh PG18 sweeps normal + `-race` (6/6, 4 packages each), `TestAgentLeaseActivate*` at `-race -count=20`, Linux amd64/arm64 cross-builds, `git diff --check`.
- Outcome counter unchanged: nothing has executed. `/leases/activate` is unreachable while `activationEnabled` is false, and no agent calls it until 10d.

### Step 10d — agent activation execution (one subagent)
- After a passing gate the executor calls activate, switches the route via 10b, verifies Traefik API + HTTPS data plane (dial local Traefik with SNI = hostname, verify cert), reports activation-result.
- Classification (decision D4): write/reload/transport failures `transient` **before** switch authorization only; after authorization every failure is reconciliation-required, never auto-retried as a fresh attempt.

### Step 10e — drain and stop predecessor (one subagent)
- Frozen `drain_seconds` (default 30), then stop (not remove) predecessor selected by exact labels; inspect until stopped. Failure here does not undo activation; recorded for reconciliation.

### Owner decisions (recommended default first)
- D1 Scope: blue/green only; stop-then-start/rollback/retention → slice 11.
- D2 Health-gate Host: **required** (admin-time validation error, frozen NOT NULL) vs NULL falling back to `ip:port`. Required matches ADR 0005 "exact configured Host header" and turns a permanent runtime failure into a form error.
- D3 Activation under the same lease (recommended) vs a separate queued activation job (more queue machinery, second fence handoff).
- D4 Classification as above.
- D5 Lock slot for route heads after deployment heads (ADR 0004 amendment).

### Verification
Per step: subagent runs affected tests (normal + `-race`, one fresh PG18); orchestrator runs full bar before each commit (`make check`, `go test -race ./...`, three fresh PG18 sweeps normal + `-race`, concurrency `-race -count=20`, Linux cross-builds, `git diff --check`); independent zero-context review after every step, findings fixed first.

### Not provable on macOS
Real Traefik reload, ACME/HTTPS probe, fsync/rename semantics, real dockerd/BuildKit. End-to-end bar: on the Hetzner host a push walks to `healthy` behind a real hostname while the old release serves through the gate, then the predecessor is stopped and retained.

### Outcome counter
Step 0: no change. 10a–10e proven on a real host: mark "Traefik route generation, HTTPS, atomic switching" (9/18); blue/green/stop-then-start/retention/rollback stays open until slice 11.

### Rollback
Forward-only migrations (rollback = new migration dropping added tables/columns; safe while unwritten). Code behind `executorEnabled`/`activationEnabled`.

### Step order
0 -> 10a -> 10b -> 10c -> 10d -> 10e. One subagent per step, review between steps, atomic commit per step.

#### Step 10c-2 plan (approved 2026-09-30)


## Context

Step 10c-1 (`08bd6cc`) shipped migration `000019` and `POST /agent/v1/leases/activate` — tx1, which
mints exactly one `switch_authorized` `release_operations` row per environment, records the
control-plane-rendered route digest, performs the candidate `candidate -> ready` transition with the
write-once `image_digest` (the first time the agent's built digest ever reaches the control plane),
and moves the deployment `health_checking -> activating`.

Nothing closes that operation. Once `activationEnabled` flips, an authorized switch has no end state,
and three already-shipped functions behave wrongly while one is open (all verified this session):

- **`Complete`** (`agent_lease_repository.go:850`) never queries `release_operations` and, on
  `CompletionSucceeded`, closes the attempt, frees the slot and closes the job — out from under an
  in-flight activation — while never transitioning the deployment at all.
- **`closeExpiredOrRevokedAttempt`** (`:757`, reached from `expireOne` `:1661` and
  `revokeInFlightLeases` `:1956`) closes the attempt `expired`/`cancelled`, advances the slot fence,
  clears `current_attempt_id`, closes the job `dead`/`cancelled` and transitions the deployment to
  `failed`/`cancelled` — blind automatic rollback of an authorized switch, which ADR 0005 forbids
  ("an in-flight syscall cannot be preempted").
- **`Reconcile`** (`:1747`) and **`MarkReconciled`** (`:1879`) write only
  `environment_slots.reconciliation_required=false` and a `reconciled` event. Neither touches
  `release_operations`, `route_heads.reconciliation_required` or the deployment, so an
  activation-blocked environment has no code path back to claimable. Worse, `Reconcile` today
  *rejects* exactly this case: it denies unless the attempt is non-`leased` with `closed_at` set, and
  an activation-blocked attempt stays `leased` with `closed_at` NULL by design.

Plus the defect the 10c-1 review deferred here: `000019`'s partial unique index
`release_operations_environment_open_activation_idx` means a stranded open activation makes every
later `Activate` for that environment fail at the INSERT (`:1318`) with a raw `23505`, wrapped as
`insert activation operation: %w` (`:1322`). `mapError` has no case, so it becomes the default 503
`service_unavailable`, which the agent client's `mapStatus` classifies **retryable** — a permanently
wedged environment is indistinguishable from transient backpressure, forever.

Outcome: an authorized switch always reaches a durable, named end state — `succeeded`, or
`reconciliation_required` with the environment blocked and a fenced, audited, declared way out.
`activationEnabled` stays `false`; nothing ships enabled, and the outcome counter stays 8/18.

## Owner decisions (2026-09-30)

- **Two subagents**, 10c-2a then 10c-2b, independent zero-context review between them.
- **The agent-owned reconcile also declares an outcome.** `POST /agent/v1/environments/reconcile`
  carries `activated | not_activated` for an activation-caused block and finalizes the same way the
  admin break-glass does; typed refusal when the block is activation-caused and no outcome is
  declared, so the existing clear-the-flag path can never leave an operation silently open.
- **The sweeper diversion neither advances the slot fence nor clears `current_attempt_id`**, unlike
  the existing close path: the slot stays occupied by this exact attempt/fence (ADR 0004
  no-slot-reuse), matching tx2's own failure path. `reconciliation_required` is what actually blocks
  new claims (`tryClaim` reads it, `agent_lease_repository.go:355`).

## Step 10c-2a — `/agent/v1/leases/activation-result` (tx2) + the `Complete` guard

Implements the design already in `tasks/todo.md` §4. Load-bearing points the implementer must not
"tidy away":

- **tx2 deliberately omits the `authority_expires_at.After(now)` check** that every other lease
  endpoint makes. The linearization point is the operation still being `switch_authorized` for this
  exact `attempt_id`/`fence` (immutable identity columns per `validate_release_operation_update`),
  not the lease clock. Copying `Complete`'s stale check here would reject a legitimately authorized
  activation whose lease lapsed mid-syscall — the exact case ADR 0005 protects.
- **tx2 *is* the completion transaction** for a successful activation: it performs the attempt close,
  slot free, job close and job event that `Complete` does today (`:941`-`:987` as the model), plus the
  activation writes, atomically (ADR 0004 transaction boundaries). On success the executor will never
  call `/leases/complete` (wiring in 10d); `Complete` stays the failure-before-activation path.
- **Lock order**: credential SHARE -> slot -> job -> attempt -> deployment -> route head -> release
  rows by id ascending, i.e. the same sequence `Activate` uses (`:1087`-`:1241`). `FOR UPDATE` on both
  predecessor and candidate releases — tx2 is the writer; `000018`'s deferred `FOR SHARE` trigger
  remains a redundant second line of defense.
- Success writes: recheck `route_revision == expected_route_revision` and `active_release_id IS NOT
  DISTINCT FROM expected_active_release_id`; predecessor `active -> retained` (skipped when
  `expected_active_release_id IS NULL`, the first deployment); candidate `ready -> active`; route head
  `active_release_id` + `route_revision + 1` in one UPDATE (`validate_route_head_update` enforces
  CAS-by-exactly-one); `route_events` `activated` via the existing `insertRouteEvent`; operation
  `switch_authorized -> succeeded` with `closed_at` and sanitized `outcome_summary`; deployment
  `activating -> healthy` via `transitionDeployment`; then attempt/slot/job closes and
  `appendAgentJobEvent` `activated` + `succeeded`.
- Failure or ambiguity: operation -> `reconciliation_required`; `route_heads.reconciliation_required =
  true` with **no** revision bump; slot `reconciliation_required = true` +
  `reconciliation_attempt_id`. Nothing else — `current_attempt_id` stays set, attempt and job stay
  open, deployment stays `activating`. Event `activation_reconciliation_required`; no
  `failed`/`dead`/`retry_scheduled` event, this is not retry-eligible.
- **`Complete`'s guard** goes immediately after the non-`leased` replay block closes (`:930`) and
  before the stale check (`:932`) — the one point where slot, job and attempt locks are all held, the
  attempt is known `leased`, and `environmentID`/`jobID`/`attemptID`/`fence` are in scope. A
  `switch_authorized` `kind='activate'` row for this attempt/fence returns a new
  `ErrAgentLeaseActivationOpen`. A `Complete` arriving *after* a successful tx2 must still fall
  through to the existing replay-or-conflict branch, unchanged.
- **Extract the success finalization into an internal helper taking an open `pgx.Tx`**, so 10c-2b's
  `activated` outcome reuses it verbatim rather than duplicating the write sequence.
- Both new sentinels get explicit `mapError` cases (`internal/agent/http/handler.go:308`) with
  409-family wire codes — the 10c-1 review's finding was precisely this fall-through to a retryable
  503; do not repeat it. Note `mapStatus` (`internal/agent/client/httpclient.go:196`) turns any 409
  that is not `stale_lease` into `ErrIntegrityConflict` (non-retryable), which is the correct class.

Files: `internal/store/postgres/agent_lease_repository.go`; new
`internal/agent/http/activation_result.go` mirroring `activate.go`'s handler shape (identity 401 ->
nil-dependency gate 503 -> `readJSON` -> `parse` 400 -> repository -> `mapError`);
`internal/agent/http/handler.go` (route constant, dispatch, `Handler` field, interface, `mapError`);
`internal/queue/lease.go` if a result type is needed. No migration. No executor rewiring (10d).

## Step 10c-2b — stranded activations: typed refusal, sweeper diversion, declared resolution

1. **Typed `23505` refusal in `Activate`** at the INSERT (`:1318`-`:1322`): match
   `pgErr.ConstraintName == "release_operations_environment_open_activation_idx"` reusing the existing
   `isUniqueViolation` (`deployment_repository.go:254`; `pgconn` is already imported at
   `agent_lease_repository.go:22`), and return a new non-retryable sentinel with its own 409 wire code
   — never the raw violation.
2. **Sweeper diversion** in `closeExpiredOrRevokedAttempt` (covering both `expireOne` and
   `revokeInFlightLeases`): when a `switch_authorized` activation exists for this attempt/fence, skip
   the attempt close, the fence advance, the `current_attempt_id` clear, the `agent_jobs` write and
   the `transitionDeployment` call entirely. Do set slot `reconciliation_required` +
   `reconciliation_attempt_id`, and move the operation `switch_authorized -> reconciliation_required`
   so expiry has a durable name instead of sitting at `switch_authorized` forever. Attempt stays
   `leased` so a late genuine report can be evaluated. Keep the existing `inject(...)` hook points so
   the rollback tests still cover every stage.
3. **A late `/activation-result`** against an operation already at `reconciliation_required` records
   the report as reconciliation evidence (`outcome_summary` + event) and does **not** finalize.
4. **Declared resolution** in `Reconcile` (agent, owning server, matching attempt/fence) and
   `MarkReconciled` (admin break-glass, audited). Both require `activated | not_activated` when the
   block is activation-caused, and refuse with a typed error otherwise or when the environment is not
   actually blocked by an activation operation. `Reconcile` additionally needs a new branch for a
   still-`leased`, `closed_at IS NULL` attempt — today that shape is denied outright (`:1747`ff), and
   it is exactly the shape an activation block leaves behind.
   - `activated` -> reuse 10c-2a's finalize helper exactly; operation `reconciliation_required ->
     succeeded`.
   - `not_activated` -> route head `active_release_id`/`route_revision` byte-identical, its
     `reconciliation_required` cleared, operation -> `failed`, deployment `activating -> failed`,
     attempt and job closed, slot freed and unblocked.
   - Both clear the slot's `reconciliation_required`/`reconciliation_attempt_id` and append the
     existing `reconciled` event plus an activation-specific one; `MarkReconciled` keeps its
     `insertServerAuditEvent`.
5. Wire the outcome through `internal/agent/http/reconcile.go` (`reconcileRequest`), the
   `EnvironmentReconciler`/`web.ServerAdmin` signatures (`handler.go:110`, `internal/web/web.go:63`)
   and the admin route `POST /environments/mark-reconciled` (`web.go:241`, `:801`) — its exact-key
   count goes from 3 to 4, and the outcome is an explicit choice with no default.

Migration: only if a state value or `agent_job_events` kind beyond `000019`'s set is needed —
introspect first, add `000020` forward-only, never edit `000019` in place.

## Verification

Per step, by the implementing subagent: affected packages only, normal + `-race`, one fresh PG18
(memory's recipe: `postgres:18` on `55433` via `AUTODEPLOY_TEST_DATABASE_URL`, drop/recreate the
database between sweeps and always after any migration change).

Orchestrator before each commit: `gofmt`, `go build`, `go vet`, `make check`, `go test -race ./...`,
three fresh PG18 sweeps normal + `-race` over `./internal/store/postgres ./internal/agent/http
./internal/web ./migrations`, the new concurrency tests at `-race -count=20`, Linux amd64/arm64
cross-builds, `git diff --check`. Independent zero-context `code-reviewer` after each step, findings
fixed before the commit.

Must-prove tests, each proved non-vacuous by breaking the thing it tests — the 10c-1 review caught a
"race" test that was sequential and would have passed with every `FOR UPDATE` deleted, so use
`TestAgentLeaseActivateGenuinelyRacesRequestCancellation` (`agent_lease_activate_test.go:531`) as the
pattern: a third transaction holds the slot row, both callers confirmed blocked via
`pg_stat_activity`, never a sleep.

- Two concurrent `/activation-result` calls -> exactly one finalizes; `Complete` racing an open
  activation -> `ErrAgentLeaseActivationOpen`, slot never freed, job never closed.
- Stale fence, and a stale `expected_route_revision`/`expected_active_release_id`, both refused;
  replay of a succeeded result idempotent; conflicting digest refused.
- Expiry with an open activation: attempt still `leased`, fence unchanged, `current_attempt_id` still
  set, job untouched, deployment still `activating`, slot blocked, operation
  `reconciliation_required`. Same for `revokeInFlightLeases`.
- The wedge: strand an open activation, assert a different attempt's `Activate` returns the typed
  non-retryable refusal (and that `mapStatus` does not classify it retryable), then assert each
  declared outcome unwedges the environment to claimable — via `Reconcile` and via `MarkReconciled`.
- First deployment (no predecessor) succeeds; `not_activated` leaves the route head byte-identical.
- Guard tests stay green: `TestActivationEnabledStaysFalse` and the `executorEnabled` equivalent.

#### Step 10c-2a status (2026-09-30, end of session — NOT committed)

Implemented and live-verified on PG 18.4, then **failed independent zero-context review**; a fix
round was in flight when the session ended. Nothing committed; all work is in the working tree.

Added (untracked): `migrations/000020_activation_result_events.sql` + its test,
`internal/agent/http/activation_result.go` + test,
`internal/store/postgres/agent_lease_activation_result_test.go`,
`internal/store/postgres/agent_lease_activation_sanitize_test.go`,
`internal/agent/client/activation_status_test.go`.
Modified: `internal/store/postgres/agent_lease_repository.go` (+~423),
`internal/agent/http/handler.go`, `cmd/controlplane/main.go`, `tasks/todo.md`.

`migration 000020` does two things: widens `agent_job_events_event_type_check` with `activated` and
`activation_reconciliation_required`, and **replaces `validate_route_head_update`** to exempt an
update that leaves `route_revision` unchanged when only `reconciliation_required` and `updated_at`
differ. That second part was forced: `000018:126` requires `route_revision = OLD + 1` on *every*
update with no column filter, so the designed failure path (set the flag, no revision bump) was
impossible. Option B (bump on failure) was rejected — it burns a revision the control plane already
rendered and handed the agent, and breaks 10c-2b's byte-identical `not_activated`.

Two other schema walls found by reading, not by design review: `agent_job_events`' CHECK had neither
event kind (000019 deferred them deliberately), and **10c-2b will hit a third** —
`validate_release_operation_update` allows only `switch_authorized -> succeeded|reconciliation_required`
and raises "release operation % is closed" for any update once `reconciliation_required`, so the
planned `reconciliation_required -> succeeded` (declared `activated`) and `-> failed`
(`not_activated`) are both impossible today. `failed` is not even a legal state for an `activate`
operation. 10c-2b needs its own migration, and `finalizeActivationSucceeded`'s operation UPDATE
(currently `WHERE state='switch_authorized'`) must be parameterized for reuse.

Fix list issued and in flight (blocking the commit):
1. `Complete`'s guard matches only `switch_authorized` — widen to `reconciliation_required` too.
   After a failure report the attempt is still `leased`, so `Complete(failed)` transitions the
   deployment to `failed`: blind resolution of an ambiguous switch, ADR 0005 forbids it. The closed
   attempt then makes `Reconcile` available, clearing the slot flag as well. (Orchestrator's spec
   error, not the implementer's.)
2. Ambiguous success (digest mismatch, `ErrAgentLeaseActivationRouteMoved`) currently rolls back and
   writes nothing, leaving the operation `switch_authorized` with no flag and no event. Route both
   through `blockActivationForReconciliation` with the discrepancy as evidence; still return 409.
   "Does not finalize" was right, "writes nothing" was wrong.
3. Failure report must be idempotent on identical replay (today `ErrAgentLeaseStale`, so a lost
   response is indistinguishable from a lost lease).
4. Catch-all mapping unexpected `pgconn` 23xxx to a typed 409 — a deferred trigger failure at
   `Commit` or a `transitionDeployment` error is otherwise retried forever as a 503. Genuine
   infrastructure failures stay 503.
5. `RowsAffected()==1` on the route-head flag UPDATE.
6. Read the clock *after* the blocking locks (or `GREATEST(updated_at, $n)`) — the new monotonicity
   rule can otherwise trip on a concurrent writer.
7. Enforce `release_operations.outcome_summary`'s real CHECK at `parse()`; test empty and
   whitespace-only.
8. `TestCompleteGenuinelyRacesOpenActivation` is vacuous against the guard (hoisting the EXISTS above
   the locks still passes) — add the tx2-first / `Complete`-second contended order.
9. Atomicity test injects only at `job_event`; inject at every stage of both paths.
10. The migration's hostname-at-unchanged-revision case is vacuous — the pre-existing hostname freeze
    raises regardless, so deleting the new exemption's hostname clause still passes. Isolate it, and
    add `created_at`-changed and active-to-different-active cases.
11. Assert post-state in the `fence bumped elsewhere` / `never authorized` / `wrong server` subtests;
    confirm `wrong server` reaches the ownership check rather than being denied at
    `lockActiveServerCredential`.
12. Prove or soften the "serializes at the slot row" comment — the race tests cannot distinguish the
    slot row from the job or operation row (`FOR SHARE` would still pass).
13. **Unexplained intermittent failure**: sweep 3 of `go test -race -p 1 -tags=integration
    ./internal/store/postgres` FAILED; 5 later runs passed and the failing test name was not
    captured. Hunt it over >=8 fresh-database runs with full logs kept. Suspicion (unconfirmed): a
    `pg_stat_activity` blocked-backend wait in `holdSlotAndRace` being slower under `-race`. Fix
    without making the wait vacuous — keep polling the real condition, no sleeps.
14. For the record only: does 10b's adapter enforce revision monotonicity / same-revision-same-digest?
    After an ambiguous failure the head stays at revision N, so a later attempt re-renders N with a
    different digest.

Deferred to 10c-2b as planned, comments left at each site: the sweeper and `revokeInFlightLeases`
still closing an attempt with an open activation, and the unmapped 23505 from
`release_operations_environment_open_activation_idx`.

Verified this session before the review: `gofmt`, `go build`, `go vet` (both tag sets), `make check`
clean; 5 of 6 fresh-PG18 sweeps green (the 6th is item 13). Gates unchanged:
`activationEnabled = false` (`cmd/controlplane/main.go:42`), `executorEnabled = false`
(`cmd/agent/main.go:60`). Outcome counter still 8/18 — nothing has executed.

**Tomorrow, first actions:** `git status` to see what the fix round actually landed; re-run the full
bar from scratch; re-review independently (the fix list above is the checklist, so a lost subagent
report is not fatal); then commit 10c-2a atomically and start 10c-2b with its migration.

##### 10c-2a addendum (2026-09-30, late): fix round landed, still uncommitted and NOT yet re-reviewed

All 14 items above were addressed in the working tree. The implementer reports the full bar green
(gofmt, build, vet both tag sets, `git diff --check`, three fresh-database sweeps of the four
packages normal and `-race` = 6/6, the race/atomicity/lapsed-authority tests at `-race -count=20`,
and unit `go test ./...`), with a mutation proof per changed test. **None of that has been
re-verified by the orchestrator, and no independent re-review has run.** Both are tomorrow's first
actions, before any commit. Do not treat the implementer's green bar as the commit gate —
[[subagent-verification-scope]].

Substantive outcomes worth carrying forward:

- **Item 13 root-caused, and my suspicion was wrong.** Not a `pg_stat_activity` wait timeout. The
  failing test was the implementer's own `TestReportActivationResultIgnoresLapsedAuthority`, failing
  2 of 10 runs with `agent_job_attempts_check` (23514): its UPDATE called `clock_timestamp()` once
  per column, so `authority_expires_at` could land microseconds after `lease_expires_at`, violating
  `authority_expires_at <= lease_expires_at`. Fixed with one shared timestamp; passed at
  `-count=60`. `waitForLockWaiters` was left unchanged.
- **Item 2 changed tx2's lock behaviour**: to avoid writing before detecting a moved route head, tx2
  now locks the deployment and route head and rechecks the route *before* calling
  `blockActivationForReconciliation`, with `finalizeActivationSucceeded` keeping its own recheck as
  defence in depth. **Tomorrow's re-review must re-walk the lock order against ADR 0004 and against
  `Activate`/`Complete`/`RequestCancellation`/the sweeper** — the previous review cleared an order
  that has since changed.
- **Item 4**: the classifier maps pgconn class 23 **and `P0001`** to a typed 409 —
  our triggers use plain `RAISE EXCEPTION`, which is `P0001`, not class 23, so class 23 alone would
  have missed exactly the deferred-trigger-at-commit case the item existed for. Deadlock (40P01),
  serialization (40001), cancellation and connection errors stay retryable.
- **Item 12 accepted as softened, not solved.** The comment now claims only what the tests prove
  (each caller blocks on the slot row when held; outcomes consistent) and explicitly does not claim
  which row lock decides a race. A test that discriminates (e.g. one that fails under `FOR SHARE` on
  the slot) remains unbuilt — a separate task if wanted.
- **Item 14 answered, and it matters for 10c-2b/10d**: `internal/runtime/traefik` enforces **no**
  revision monotonicity and no same-revision-same-digest rule. `RouteRevision` is rendered only into
  a `# route_revision:` comment (`render.go:161`), so it feeds the digest but is never compared;
  `Write` (`write.go:360-385`) compares the on-disk digest with the new render and overwrites on any
  difference, whether the revision is higher, equal or lower. So after an ambiguous failure leaves
  the head at revision N, a later attempt at revision N with a different release or fence silently
  overwrites the file. Any monotonicity or ownership guard must live in the control plane's CAS or be
  added to 10b.
- Not mutation-proven, by the implementer's own admission: the `RowsAffected()==1` branches (item 5 —
  unreachable under the schema), the added post-state assertions in `fence bumped elsewhere` and
  `never authorized`, a guard hoist that stays below the replay block, and a real deferred-trigger
  failure at `Commit` (covered only by a synthetic `PgError` in a unit test).
- The 10c-2b schema wall is reconfirmed: `validate_release_operation_update` refuses any update to an
  `activate` operation once it is `reconciliation_required`, so both declared outcomes need a new
  migration.

##### 10c-2a addendum (2026-10-01): orchestrator bar green, independent re-review FAIL, second fix round in flight

**Orchestrator-run verification of the fix-round tree (not the implementer's numbers):** `gofmt -l`
clean, `go build ./...`, `go vet ./...` and `go vet -tags=integration ./...` clean, `git diff --check`
clean, `make check` exit 0, `go test -race -count=1 ./...` exit 0 across 29 packages. Integration
sweeps on live PG 18.4 reached **4 of 6 green** (3/3 normal, 1/3 `-race`) and were then deliberately
stopped: the code is changing again, and the fix agent needs the same database. Full per-run logs kept
this time (no `tail` on a live run). Gates re-confirmed false: `activationEnabled`
(`cmd/controlplane/main.go:42`), `executorEnabled` (`cmd/agent/main.go:60`).

**Migration `000020` fidelity verified by the orchestrator independently**, line by line against
`000018`: all four original rules survive, and the new unchanged-revision exemption accounts for every
column in `route_heads` — `environment_id` via rule 1 above it, `hostname`/`active_release_id`/
`created_at` explicitly, `route_revision` by the branch condition, `updated_at` by the monotonic rule.
No rule silently dropped. The review agreed. **But two of the tests guarding those rules are vacuous**
(below), so the preservation was unproven, not wrong.

**Re-review verdict: FAIL.** The lock order — the thing the stale review made urgent — is **clean**:
tx2 goes creds SHARE -> slot -> job -> attempt -> **release_operations** -> deployment -> route head ->
releases by id asc -> inserts, and no path anywhere (`Activate`, `Complete`, `RequestCancellation`,
`AcknowledgeCancellation`, `Reconcile`, `expireOne`, server revoke, `Heartbeat`,
`AssignRouteHostname`) takes two rows in the opposite order. Every path touching `route_heads` /
`releases` / `deployments` / `release_operations` for a leased attempt takes the environment slot row
first. No cycle, no integrity regression. Two by-products: the `release_operations` row is an
**undocumented lock class** (ADR 0004 never mentions it, and its amendment still calls tx2 "not yet
implemented"), and the late lock-order change in `blockActivationForReconciliation` is **cosmetic** —
the operation row is already locked there, so the explicit route-head lock changes nothing in ordering
terms and its comment overclaims.

Must-fix, now in flight:

1. **Silent rollback on deterministic finalize failures — my spec error, third repeat of the pattern.**
   Fix-list item 2 named *two* ambiguous paths (digest mismatch, route moved), so exactly those two got
   the durable block. Every other deterministic failure inside `finalizeActivationSucceeded` still
   rolls back to `switch_authorized` with no flag and no event: missing route head, candidate not
   `ready`, predecessor not `active`, `ErrInvalidTransition`, and a deferred-trigger failure at COMMIT.
   The classifier then returns a terminal 409, so the agent stops reporting while the host may already
   have switched. **The rule, stated as a rule this time:** once a success report has passed the fence,
   occupant and authority checks, *any* deterministic failure to finalize must durably block for
   reconciliation with the discrepancy as evidence and still return the typed non-retryable error; only
   genuinely retryable failures (40001, 40P01, 57014, 08xxx, context) may roll back. Two windows:
   savepoint rollback for in-transaction errors (locks survive), a fresh transaction re-taking the lock
   order for COMMIT-time errors, returning the *original* error if the second transaction also fails.
   Lesson: **specify guards by rule, never by enumerating instances** — I did this with the `Complete`
   guard (named one state) and now again here (named two paths).
2. **Absent `succeeded` parsed as a declared failure** (`internal/agent/http/activation_result.go`):
   `Succeeded bool` has no presence check, so a body omitting the field durably blocks an environment
   with an empty summary. `*bool` + 400.
3. **Two vacuous `000020` tests, both shape (a)** — asserting a new rule rejects what a pre-existing
   constraint already rejected: the `environment_id` case is caught by `000018`'s composite FK
   `(active_release_id, environment_id)`, and both non-monotonic `updated_at` cases subtract an hour,
   falling below `created_at` and tripping the table CHECK rather than the trigger. Deleting rule 1 or
   the monotonic block respectively still passes. So the header's claim that "all four 000018 rules
   still fire" was only ever true for the revision and hostname rules.

Also in flight: the sanitize test's 512-byte cut lands on a rune boundary (400 two-byte runes), so the
rune back-off loop can be deleted and it still passes; the "stale fence" test is redundant (the
`(attempt_id, fence)` lookup finds no row and returns stale anyway, and the fence equality is forced by
`validate_release_operation_insert`); `reconciliation_required=true` now raises P0001 when the flag is
*already* true (unreachable today, would bite 10c-2b); the three overclaiming comments; and the ADR 0004
amendment.

Carried to 10c-2b as blockers before `executorEnabled` flips: after a declared failure the attempt stays
leased, so once authority lapses (~90s + 15s tick) `expireOne` closes it, advances the fence and ignores
the open operation — any later report then hits `ErrAgentLeaseStale` and nothing durable records the
host switch. `TestReportActivationResultIgnoresLapsedAuthority` does not run the sweeper.

Not covered by any test, and accepted as such for now: the lock order itself. Deleting the deployment
`FOR UPDATE` or the route-head lock breaks nothing, and no test runs tx2 concurrently with `Activate`,
`RequestCancellation` or the sweeper on the route-head/deployment rows.

**Next actions:** re-run the full bar from scratch on the second fix round (all six sweeps this time),
re-review the newly added reconciliation-on-finalize-failure paths specifically, then commit 10c-2a.

##### 10c-2a addendum (2026-10-01, later): second re-review also FAIL — the same defect class one level deeper

Fix round 2 closed all three must-fix items and the five smaller ones, and the orchestrator's static bar
was green on it (`gofmt`, build, both vet tag sets, `diff --check`, `make check`, `go test -race ./...`;
integration sweeps reached 4/6 before being stopped for round 3). The second independent review still
**FAILED** it, and the finding is the important kind: **the block-writing paths can themselves fail, and
when they do the code returns the *terminal* cause.** So the agent stops reporting and nothing durable
exists — the identical lost-ambiguity hazard, now in the error handling of the fix for that hazard. One
test, `...CommitTimeBlockFailureKeepsOriginalError`, asserted exactly the hazardous behaviour.

Two high findings: `blockAfterCommitFailure` converted every fresh-transaction failure (pool timeout,
40P01, 40001, dropped connection, the ~10s request context expiring) into the original deterministic
cause via `withSecondary`, which `classifyActivationResultError` turns into a terminal 409; and a failed
`ROLLBACK TO SAVEPOINT` wrapped `cause` with `%w` where the line three below it already used `%v`
deliberately for exactly that reason.

**Design decision (orchestrator, this session): invert `isDeterministicActivationFailure` to a NEGATIVE
definition** — block on anything not *known* transient. Once a failed block write falls back to a
retryable error, this is **self-correcting**: if the error really was a dead connection or a
serialization failure, the block attempt fails too and the result is retryable anyway. A dead connection
cannot write a block, so there is no need to predict which errors are writable. This collapses the
enumeration problem rather than patching six call sites, and it makes the known-transient list the only
list that has to be right. Consequence accepted deliberately: an unrecognized error now flags an
environment for an operator (conservative, visible) instead of looping a 503 forever with the operation
silently open. `TestReportActivationResultIsAtomic` had to be updated to assert the new behaviour — the
test follows the rule, not the reverse.

Transient list was materially incomplete; round 3 adds 25006 (read-only transaction during
failover/standby promotion — every write including the block fails), 25P03/25P04/57P05 (session and
transaction timeouts that kill the session), 58030, 40003, 26000 and 0A000 (the pgx cached-plan error
after a migration or pooler reset — **this slice ships a migration**, so that path is live). Also folded
in: `ErrRevisionConflict`/`ErrNotFound`/`ErrRevisionOverflow` indicate corruption inside tx2, since the
deployment row is already locked there.

**Orchestrator's own finding, same class:** `blockAfterCommitFailure` revalidated the server credential,
so a credential revoked between the report and the commit failure meant nothing was recorded. Removed —
authentication already happened in tx1 and the block grants the agent nothing, it only records that
something ambiguous occurred. Writing a block for a revoked server is harmless; losing it is not. This
must fail safe.

Lock order **cleared by two independent reviews now** and unchanged: creds SHARE -> slot -> job ->
attempt -> `release_operations` -> deployment -> route head -> releases by id asc -> inserts. The fresh
transaction and `refuseAmbiguous` both take proper subsequences. ADR 0004's amendment now matches the
code. Savepoint semantics confirmed correct: locks taken before the savepoint survive the rollback, and
the block's `route_heads` UPDATE touches only `reconciliation_required`/`updated_at` so
`route_heads_active_release_after_write` (`UPDATE OF active_release_id`) does not fire — which is why
the predecessor-retained case can still commit its block.

Tests that proved nothing and are being fixed: the `attemptState != 'leased'` and slot-occupant clauses
could both be **deleted** with every test still passing (the "never authorized" case is caught earlier by
operation-not-found, "fence bumped" covers only `slotFence`, "already succeeded" is caught by the
`opState` clause alone); and all four `FOR UPDATE`s in `blockAfterCommitFailure` could be downgraded to
plain SELECTs with every test still passing.

Honest limits now on record: the commit-time test installs its **own** deferrable constraint trigger — no
production deferred trigger can be made to fail from inside finalize, because finalize holds every lock
they read — so it proves "any deferred failure at COMMIT is handled", not that a production trigger does
it. The retryable-code tests inject rather than forcing a real 40001/deadlock, which cannot be made
deterministic at that statement. The "predecessor not active" and "route head missing" states are
unreachable in production (the no-delete and reference triggers prevent them), so those tests exercise
defensive branches only. `finalizeActivationSucceeded`'s ErrNoRows branch is dead — kept as defence in
depth, now commented as unreachable rather than deleted.

**Lesson, recurring and worth stating as a rule:** every time this hazard was fixed, the fix introduced a
new instance of it one level down — first by naming two paths instead of a rule, then in the error
handling of the blocking code itself. When a fix exists to guarantee a durable record, audit the fix's
*own* failure paths against the same guarantee before calling it done.

##### 10c-2a addendum (2026-10-01, evening): third re-review FAIL — the fifth instance, found OUTSIDE tx2

Round 3 implemented the negative definition and closed all four named hazard sites. Orchestrator's static
bar green (`gofmt`, build, both vet tag sets, `diff --check`, `make check`, `go test -race ./...`);
integration sweeps 5/6 green with the sixth running when round 4 was queued. **Coherence was explicitly
verified** before trusting any of it, because the round-2 agent had resumed and re-reported after round 3
was dispatched: `isDeterministicActivationFailure` is genuinely `!isTransientActivationFailure(err)`, the
`errActivationBlockUnrecorded` sentinel exists, the new SQLSTATEs are present, and
`lockActiveServerCredential` survives only in the main transactions, not the fresh path. No clobber.

**Process error to not repeat: two fix agents were live on the same files at once.** The round-2 agent was
flagged as having background work still running and was dispatched over anyway. Nothing was corrupted,
but that was luck. **Stop the predecessor before dispatching a successor.** Also: its resumed hand-back
opened "All findings fixed" with a full mutation list and was distinguishable from round 3's only by
checking its *substance* against the brief — it described the positive definition, the retained credential
revalidation and the commit path returning the original error, i.e. exactly what round 3 was changing.
**Read a hand-back against what was asked for, never by its headline.**

**Third review FAIL — the fifth instance of the hazard, and it is pre-existing code outside tx2:**
`lockActiveServerCredential` (`agent_lease_repository.go:114,120`) collapses ANY error from its two
SELECTs into `ErrServerAuthorizationDenied` -> 401, and the client never retries a 401
(`internal/agent/client/httpclient.go:137,200-202`). So a connection reset, failover, context deadline or
57P01 on tx2's *first statement* silently drops a genuine success report with the operation
`switch_authorized`, no flag, no event. The helper is shared with Claim/Renew/Complete, where a lost call
is harmless; **tx2 is the first caller for which it is the hazard.** Lesson: when a slice makes a new
demand of a shared helper, re-audit the helper against the new demand — four reviews looked only at new
code.

Second high: `classifyActivationResultError` maps any non-transient `PgError` to a terminal 409 **including
the pre-occupant read/lock statements**. Class-42 schema skew during a rolling deploy then tells the agent
to stop with nothing durable, where a 503 would have succeeded on retry. Round 2's broadening was right
**for post-occupant writes** (a block exists, so the answer must be terminal) and wrong before the occupant
check — so the fix is **positional**, not a narrower class list, preserving round 2's rationale where it
applies.

Third: `blockUnrecorded` wraps the inner error with `%w`, and `blockActivationForReconciliation` can return
`ErrAgentLeaseStale`/`ErrAgentLeaseCorruptState`; `Handler.mapError` tests `errors.Is` on those before its
default branch, so an unrecorded block would still answer a terminal 409 — contradicting round 3's own
"block failures are never terminal" claim. Latent (those branches are unreachable while the operation,
slot and head are all locked) but it must not be left as a trap.

**Two corrections to the orchestrator's own reasoning, both recorded because this project's failure mode is
a right conclusion resting on a wrong mechanism:**
1. **"Self-correcting" was overstated.** I justified the negative definition by "a dead connection cannot
   write a block either". But `abortAndBlock`, the commit-failure paths and `recordBlockInFreshTransaction`
   write on a *different pooled connection*, so a misjudged transient error on one connection does not stop
   the block — it succeeds and **spuriously blocks** an environment. Blocking by default is still the right
   direction; the stated mechanism was wrong and the comment must say so.
2. **"Retried forever" was false.** The client retries 5 times over ~3s total
   (`httpclient.go:42,132-149`), and the window is further bounded by authority expiry through the sweeper
   gap. So today a long outage turns "transient, retry" into a silent loss. The
   `errActivationBlockUnrecorded` trade therefore **depends on 10d adding a persistent retry** — a hard
   gate, not an assumption.

Also to fix: missing transient cases that would spuriously block — XX000 ("cache lookup failed", "could not
open relation" around DDL) and non-`PgError` pgx errors ("conn closed", closed pool); and 0A000 is gated on
an **English** message substring, so a non-English `lc_messages` server blocks instead — use
`pgErr.Routine == "RevalidateCachedQuery"`, which is language independent. ADR 0004:615-616 still lists the
credential in the fresh path; it is gone.

**Security question answered independently** (it was the orchestrator's own removal, so it needed an outside
check): dropping credential revalidation from the fresh path is sound — the only call sites are in
`reportActivationResult` after `lockActiveServerCredential(serverID)` passed *and*
`ownerServerID == serverID` was checked, so a different server cannot reach it, and a server revoked
mid-flight can only block its own attempt's environment. **Invariant to keep documented:** the
`server_id=$2` predicate is defence in depth and becomes a tautology if 10c-2b ever passes a serverID read
from the attempt itself.

Confirmed by the orchestrator directly: `000019:43` does carry
`CHECK (route_digest IS NULL OR octet_length(route_digest) = 32)`.

**HARD GATES before `activationEnabled` may flip** (beyond the pre-existing privileged-Linux smoke): 10c-2b
must land (it alone closes the sweeper gap, the `activationBlockNoopOther` window, and the fact that nothing
clears a block cleanly today — `Reconcile` refuses leased attempts and `MarkReconciled` clears only the slot
flag, leaving operation and route head flagged), and 10d must add a **persistent** retry for the result
report. The sweeper gap is worse than previously recorded: it bounds *every* retryable bucket by authority
expiry.

Still untested and accepted as such: the route-head lock in `blockActivationForReconciliation` (no test
holds a `route_heads` lock, so a plain SELECT survives); the fresh path's lock *order* (a job-before-slot
reordering survives); the admin `AssignRouteHostname` contention; and the transient-COMMIT branch at the
three commit sites. The contention tests discriminate `FOR UPDATE` from `FOR SHARE` by query-text match —
real but textual, proving each lock is requested rather than which one serializes.

**Reframing for the commit decision:** 10c-2a ships behind `activationEnabled = false`. The bar for
committing it is therefore "correct, with every gap recorded", not "production-ready" — production-readiness
is gated on 10c-2b and 10d above. Round 4 closes the four must-fixes and the two cheap should-fixes; the
rest is recorded as gates.

##### 10c-2a addendum (2026-10-01, final): round 4 landed, orchestrator decisions, ready to commit

Round 4 closed all four must-fixes from review 3 plus the two cheap should-fixes, each with a mutation proof.

1. **`lockActiveServerCredential`** now reserves `ErrServerAuthorizationDenied` for a genuinely absent row or
   a non-active server/credential; every other database error returns wrapped, so a reset connection,
   failover, deadline or 57P01 on tx2's first statement is a retryable 503 instead of a 401 the client never
   retries. **Verified by the orchestrator directly: the constant-time property is intact** — both
   not-found cases still fall through to `matchServerCredential` with the dummy digest rather than
   early-returning, so the timing-attack protection is unchanged, and no digest reaches any error message.
   All 10 call sites checked; none wrapped, inspected or re-mapped the error, so the change strictly improves
   each.
2. **Classification is now POSITIONAL**, via an `occupantChecked` flag set immediately after the occupant
   check, with `classifyActivationResultError` applied only when it is set. The PgError class list was NOT
   narrowed, so round 2's post-occupant guarantee is preserved exactly where it applies. Typed sentinels
   (e.g. `ErrAgentLeaseStale` from the occupant check itself) still map correctly unclassified.
3. **`ErrActivationBlockUnrecorded` is exported and `mapError` checks it FIRST**, so an unrecorded block
   answers 503 regardless of which sentinel is wrapped inside it. The store-level test cannot assert the 503
   end to end (a postgres-package test importing `agenthttp` would be an import cycle); the handler test
   proves it separately.
4. ADR 0004 corrected (the fresh path does not take the credential, and why), the **"self-correcting" comment
   replaced with the truth** (the block writes on a different pooled connection, so a misjudged transient
   error spuriously blocks rather than failing safe; blocking by default is still right because losing the
   record is worse than a spurious block an operator can clear), and the `server_id=$2` tautology invariant
   documented on `recordBlockInFreshTransaction`.
5/6. XX000 and `pgconn.ErrConnClosed` added to the transient set; 0A000 now gated on
   `pgErr.Routine == "RevalidateCachedQuery"` instead of an English message substring.

`TestReportActivationResultIsAtomic` was correctly *adapted* rather than bent: the new pre-occupant stages
must stay retryable with nothing written, so a `preOccupantStages` set flips the expectation for them. The
test follows the rule.

**Orchestrator decision — reverted round 4's `go.mod` change.** It had hand-promoted
`github.com/jackc/puddle/v2` from indirect to direct (without `go mod tidy`) to reference
`puddle.ErrClosedPool`. Removed from both the production list and the sanitize test, because it buys no
behaviour: treated as deterministic, a closed pool makes the block attempt acquire another pooled
connection, that acquire fails on the same closed pool, the failure is wrapped as
`ErrActivationBlockUnrecorded`, and the result is retryable — the identical end state, without a direct
dependency on pgx's pool internals. Reasoning is recorded in a comment at the call site. `go.mod`/`go.sum`
are now byte-identical to HEAD, so **no dependency change rides along in this commit**.

**Deliberately NOT fixed, recorded as a follow-up:** `authenticateServerCandidate` (~:155) has the identical
error-collapse bug (`if err != nil || status != "active"` -> denial). It is Claim's non-transactional
pre-check, so a transient error there costs a *lost claim* — the job stays queued and the next cycle retries
— not a lost traffic switch. Different method, outside 10c-2a's contract. Fix it in its own change with
Claim's tests re-run.

**Still unproven at commit time, by design:** no test runs the real `Handler` against the real repository end
to end; no test shows a real dropped connection or closed pool arriving as those exact sentinels; the
transient-COMMIT branch at the three commit sites, the route-head lock in
`blockActivationForReconciliation`, the fresh path's lock *order*, and admin `AssignRouteHostname`
contention all remain untested. These are recorded rather than closed because 10c-2a ships behind
`activationEnabled = false`: the commit bar is "correct, with every gap recorded", not "production-ready".

**Production-readiness gates (unchanged, both hard):** 10c-2b must land (it alone closes the sweeper gap, the
`activationBlockNoopOther` window, and the fact that nothing clears a block cleanly today), and 10d must add
a **persistent** retry for the result report — the client really retries 5 times over ~3s, so "503 retried
forever" is false today and the sweeper gap bounds every retryable bucket by authority expiry.

## Step 10c-2b — design pass reviewed, plan amended, owner-approved 2026-10-03

Design pass ran first (no code), per the rule that a risky step gets a reviewed design before implementation.
Full document `.tmp/10c-2b-design.md`, proposed SQL `.tmp/10c-2b-000021-proposed.sql` (both gitignored).
It materially changed the previously approved plan, so the amended plan was re-approved by the owner.

**What the design found that the approved plan did not:**

1. **A fourth schema wall.** Item 3 (record a late report as evidence) needs
   `reconciliation_required -> reconciliation_required`, refused today. The migration is **000021**, not 000020.
2. **A real hole, proven by experiment, not argued.** The "one open activation per environment" unique index is
   partial on `state='switch_authorized'` ONLY:
   `release_operations_environment_open_activation_idx ON release_operations(environment_id) WHERE kind='activate' AND state='switch_authorized'`.
   With a second occupant forced, slot flagged and route head unflagged, **`Activate` succeeded and inserted a
   second operation** — only `Claim` incidentally refuses, and `Activate` never reads the slot flag. So the index
   and the slot flag guard two DIFFERENT windows. Owner approved recreating it as a rule:
   `WHERE kind='activate' AND state NOT IN ('succeeded','failed')`. Consequence accepted: an unresolved
   activation hard-wedges new activations for that environment until an operator resolves it, with item 1's
   typed 409 explaining why.
3. **The approved plan carried the instance-naming bug itself (D1).** Item 2's diversion keyed on
   `switch_authorized`, so the SECOND sweep sees `reconciliation_required` and runs the ordinary close path —
   reintroducing the exact hazard 10c-2a spent four rounds closing. The same instance-naming is in three
   **shipped** 10c-2a sites: `Complete`, `recordBlockInFreshTransaction`'s `opState != 'switch_authorized'`, and
   tx2's `switch op.state`. Rule: divert on any activate op with `state NOT IN ('succeeded','failed')`, via one
   shared predicate (step B). Third occurrence of this spec-defect class in slice 10.
4. **Nothing in the codebase ever clears `route_heads.reconciliation_required`** (D6), so `activated` must clear
   it or every later `Activate` dies at the route-head flag check — a second silent wedge.
5. `transitionDeployment` locks the deployment row only when it SAVES, so it must be called before the
   route-head lock in `not_activated`.
6. D1b: diverted attempts match `ExpireBatch`'s discovery query every tick, sort first, and can starve the
   100-row LIMIT — exclude `reconciliation_required` ops from discovery.

**Orchestrator decisions:**

- **U1 attempt shape on `not_activated` = `expired`, and this is SCHEMA-FORCED, not a preference.**
  `agent_job_attempts_check3` requires `state='failed'` to carry BOTH `result_operation_id` and `result_digest`.
  In the stranded case no agent report ever arrived, so there is no digest — attempt-`failed` would mean
  fabricating evidence. `expired` requires all three NULL and fits exactly. (My own first instinct was `failed`;
  the schema overruled it.)
- **`closed_at` preserved** via `COALESCE(closed_at, $now)` and immutable once set: it records when the
  ambiguity was recorded, which is forensic. Resolution time lives on the attempt row, the job events and the
  audit row.
- **U2: an operator-declared `activated` does NOT require a matching route digest** (owner-approved). Requiring
  one makes the path unusable in exactly the case it exists for — a stranded activation where no report ever
  arrived. Any digest that does exist is recorded as evidence. ADR 0005 forbids blind resolution by the SYSTEM,
  not operator-declared resolution.
- **U3: the candidate release stays `ready` on `not_activated`.** The artifact is valid; only the activation
  failed, so it stays redeployable without a rebuild. Precedent: `Complete` never touches releases.
- **U4: the audit outcome goes in the structured reason**, not new `server_audit_events_action_check` literals —
  the action is still "mark reconciled" and the outcome is its parameter. No migration needed for it.
- **Branch C ("attempt closed but operation unresolved") is REFUSED with a typed error in both paths.** That
  shape is only creatable by today's buggy sweeper, which step D fixes, and nothing has ever executed, so there
  are no legacy rows. Recourse if it ever appears is owner SQL, consistent with the existing spurious-block note.
- Migration review by the orchestrator before dispatch: the proposed identity block and non-activate branch are
  **byte-identical to the live `prosrc`**; `succeeded`/`failed`/`started` still raise via the ELSE branch; and
  `closed_at` immutability is consistent with `release_operations_check1` on every transition. Two amendments
  issued: the new branch's error message must state the permitted set accurately (it omitted that `rr -> rr` is
  allowed), and the index recreation must note that it FAILS if an environment already holds two unresolved
  activate operations — intended, since it surfaces corruption rather than hiding it.

**Also corrected a recorded claim.** `tasks/todo.md` and the project memory said "`failed` is not even a legal
state for an `activate` operation". `release_operations_state_check` permits `failed` for EVERY kind; the
exclusion lives in the trigger's `NEW.state` list. Right conclusion, wrong mechanism — the error class that
sends the next reader to the wrong place.

**Steps, one subagent each, independent review between them (owner chose the full sequence):**
A migration 000021 + test (must land first) -> B shared unresolved-op predicate + block-routine refactor, no
behaviour change -> C item 1 typed 23505 refusal -> D item 2 sweeper diversion + discovery exclusion -> E item 3
late report -> F finalize-helper parameterization -> G `Reconcile`/`MarkReconciled` resolution, both outcomes,
with real contention tests -> H item 5 HTTP/admin/form wiring -> I docs (orchestrator). A, F and G need the
migration in place.

Known stale references in the pre-existing plan text, corrected by the design pass: the `Activate` INSERT is at
`agent_lease_repository.go:1360-1367` (not :1318), and `Reconcile`'s denial of a still-leased attempt is at
`:2769` (not :1747).

**Pre-existing risk recorded, not fixed here:** `AssignRouteHostname` ignores the reconciliation flag entirely,
so it can move the hostname or revision while an activation is open, which then makes `activated` fail with
route_moved.

#### 10c-2b step A committed (1354760); step B reviewed, PASS, fix round in flight (2026-10-03)

**Step A** committed after one fix round that closed four mutation holes (any of which would have let a
production rule be deleted with every test green): the `id` identity clause — `UPDATE ... SET id=DEFAULT`
DOES reach the trigger, unlike `SET id=<value>`, which the orchestrator initially got wrong and corrected;
the index's rule-versus-enumeration claim (the mutation `state IN ('switch_authorized','reconciliation_required')`
— the exact enumeration the comment says was rejected — passed every test until a `started` collision case
was added); and two weakenings of the `closed_at` comparison (`<>` and `>` for `IS DISTINCT FROM`). Also
fixed an impossible documented remedy: the SQL told an operator to resolve duplicates "by hand and re-run",
but the pre-000021 trigger forbids exactly that UPDATE — it now gives the DISABLE TRIGGER path and a
detection query. Full bar green incl. 6 sweeps, 9 concurrency tests at `-race -count=20`, Linux
amd64/arm64 cross-builds.

**Step B** introduced `internal/store/postgres/activation_state.go`: `activationTerminalStates =
{succeeded, failed}` defined ONCE, `unresolvedActivation(state)` derived by negation, and
`unresolvedActivationSQL` **generated from the same Go slice** so the two forms cannot drift. Deriving from
terminal states (rather than listing open ones) is what makes a future state unresolved by default — it
fails closed. Matches 000021's index predicate.

**Accurate bookkeeping:** exactly ONE production site uses the predicate (`Complete`'s guard, `:976`).
`finalizeActivationSucceeded` was PARAMETERIZED (`WHERE id=$1 AND state=$4` via a new `fromState`), not
routed. An earlier report said "2 of 5 routed"; that was loose.

**Step B's most useful output was a REFUSAL.** Three of the five sites the orchestrator named must NOT use
the predicate, and the design doc was wrong to say otherwise. They are a different KIND of state test:
- `Activate`'s replay (`:1209`) tests whether the AUTHORIZATION is live. With the predicate a
  `reconciliation_required` row would take the replay branch and hand the agent a success-shaped "you may
  switch" response for a blocked environment — the opposite of the rule. -> step C's typed refusal.
- tx2's `switch op.state` (`:1918-1950`) is a DISPATCH over specific states.
- `recordBlockInFreshTransaction` (`:2246`) guards the SOURCE state of the `switch_authorized ->
  reconciliation_required` transition; with the predicate a `reconciliation_required` row would reach an
  UPDATE whose `WHERE state='switch_authorized'` matches nothing, return `ErrAgentLeaseStale`, get wrapped
  by `blockUnrecorded` into a RETRYABLE error, and the agent would retry forever on a block that already
  exists.
**Lesson: "is this operation unresolved?" and "is this operation in the source state this transition
requires?" are different questions.** The orchestrator's own brief conflated them by listing five sites as
instances of one rule — the enumeration defect recurring one level up, in the instructions this time.

**Real behaviour change, stated precisely:** `Complete`'s guard now also covers `started`.
`validate_release_operation_insert` does NOT constrain `state` (introspected), so a `started` activate row is
insertable by direct SQL, though `Activate` only ever inserts `switch_authorized`. Blocking `Complete` on it
is the fail-safe reading (`check1` ties `started` to `closed_at IS NULL`, so it genuinely is unresolved) and
it is unreachable through code.

**MUST FIX BEFORE / DURING THE NAMED LATER STEPS** (from the step B review):
1. **tx2 needs an explicit `failed` case** (step E). After 000021 makes `reconciliation_required -> failed`
   legal, an operator declaring `not_activated` means a late or retried agent report falls to tx2's
   `default` and returns `ErrAgentLeaseCorruptState` — mislabelling an operator-resolved state as
   corruption, non-retryably, so the agent stops on a misleading error.
2. **Typed refusal in `Activate`** for an existing unresolved-but-not-authorized row (step C). Today the
   caller gets a generic `ErrAgentLeaseStale`, and a stranded row surfaces as a raw 23505 at `:1363`.
3. **`MarkReconciled` (`:2872`) clears `environment_slots.reconciliation_required` with NO check that an
   unresolved activate operation exists** — an admin can clear the slot flag while the operation stays
   unresolved and the route-head flag stays set, desynchronizing the two. `Reconcile` (`:2740`) is only
   INCIDENTALLY protected by its `attemptState == "leased"` denial. Decide explicitly in step G; defence in
   depth is to refuse when `EXISTS(... unresolvedActivationSQL)`.
4. **A `started` activate row has NO resolution path after 000021** — the update trigger rejects every
   transition out of it ("is closed"), and the widened index blocks all further activations for that
   environment. The only exit is the manual trigger-disable surgery 000021 documents. An honest cost of the
   orchestrator-approved index widening. Either constrain activate rows to insert as `switch_authorized`, or
   give the resolver a path. Unreachable via code today.
5. **`RequestCancellation` (`:2449`) uses `deploymentStatus == activating` as a PROXY** for an in-flight
   activation, consistent only because tx2 leaves the deployment `activating`. Re-review when step G moves
   the deployment. `Activate`'s route-head flag check (`:1266`) is the same kind of proxy.
6. `closeExpiredOrRevokedAttempt` (comment `:759`, called from `expireOne` `:2715` and `revokeInFlightLeases`
   `:2998`) is the single choke point for step D's diversion; it should use `unresolvedActivationSQL` and
   take the operation lock after the attempt lock, which fits the shipped order.

**Drift gap being closed in the step B fix round:** 000021's index predicate is hand-written SQL with nothing
tying it to the Go slice, and the agreement test is largely tautological for state membership because both
forms derive from the same slice (its real value is catching fragment-shape bugs like `IN` vs `NOT IN`, plus a
state-count tripwire). A test reading the index predicate from `pg_get_expr` and agreeing it with the Go
fragment per state closes the more important half. Also in that round: parenthesize the fragment (it
misbinds after `OR `), validate `fromState` (empty silently matches no row -> misleading Stale), and fix the
comments at `:759`/`:1395` that still name one state where the rule is meant.

#### 10c-2b step C committed: typed refusal for a stranded environment (2026-10-04)

Took three rounds, and the pattern is the slice's recurring one at finer grain each time: **each round fixed
the hazard and left a smaller version of it one level earlier.**
- Round 1 raised the typed sentinel at the INSERT (23505 by constraint name) and in the replay branch. But
  `Activate`'s route-head gate returns stale at `:1277`, ~100 lines BEFORE the INSERT at `:1373`, and tx2 sets
  BOTH the operation state and the route-head flag — so the real stranded path never reached it. Round 1's
  tests only reached the typed paths by forcing operation state WITHOUT the flag, a state production never
  produces. **The step fixed a path that does not occur.**
- Round 2 moved the refusal into the gate — but consumed the environment-scoped check only INSIDE the flag
  branch, so the rule stayed flag-gated. Reachable consequence: flag clear + unresolved operation (what the
  step D sweeper gap really produces) fell through to `transitionDeployment`, the candidate checks, the
  frozen-execution read and the render, any of which yields `candidate_not_ready`, a wrong-coded
  `integrity_conflict`, or a wrapped read error that becomes a RETRYABLE 503.
- Round 3 returns the sentinel immediately after the environment-scoped EXISTS, before `transitionDeployment`,
  independent of the flag. The replay block still precedes it, so a same-attempt replay on its own
  `switch_authorized` operation still replays. INSERT backstop kept as defence in depth.

**A new vacuity shape for the list: two observationally identical enforcement points.** The gate and the
INSERT backstop return the same sentinel with the same rollback, so NO outcome distinguished them — which is
why hardcoding the gate's query to `state='reconciliation_required'`, and deleting its flag condition
outright, both survived the entire suite. Not a weak assertion; a test that passes while the point it claims
to cover is dead. Fixed by constructing the state where only one can fire (flag clear + unresolved operation)
and by giving the second attempt no candidate release, so anything getting past the refusal fails with
corrupt-state instead of the typed error.

Mutation proofs that now pin it: reverting the round-3 hoist fails the flag-clear matrix cases and the
sweeper-gap test with `corrupt state: deployment ... has no release row, want
ErrAgentLeaseActivationEnvironmentBlocked`; hardcoding the query to `reconciliation_required` fails three
tests; deleting the early return fails six including both end-to-end tests; disabling the 23505 mapping fails
the backstop test with the raw unique violation.

**Reachability finding (important for D):** after an ambiguous tx2 the slot keeps its occupant and `Claim`
skips a reconciling slot, so a second attempt CANNOT reach `Activate` until the sweeper closes the stranded
attempt and `Reconcile`/`MarkReconciled` clears the slot flag. So the end-to-end proof is built on the very
defects D and G remove. Both end-to-end tests now assert the full precondition triple (operation still
unresolved, route-head flag still set, slot free/clear then held by the second attempt) immediately before
the final call, so a later step changing any of it fails LOUDLY instead of silently re-routing the proof
through the backstop. `TestActivateSweeperStrandedSwitchAuthorizedFlagClearIsBlocked` captures the
flag-clear scenario while it is still reachable; it carries a comment saying step D removes it and the test
must be re-pointed.

Also in step C: `isUniqueViolationOf(err, constraint)` added beside `isUniqueViolation` (unchanged — six
callers rely on its any-23505 meaning); `mapError` -> 409 `environment_activation_blocked`, verified
non-retryable on both sides (`mapStatus` maps any non-`stale_lease` 409 to a terminal integrity conflict;
the client retries only service-unavailable/transport-failure); a false comment corrected (the INSERT IS a
`release_operations` access after the route-head lock — it is a write at ADR position 8, so the claim is
"no READ"); and ADR 0004 gains the **forward constraint**: any future `release_operations` writer (D/E/G)
must take the `environment_slots` lock first — that is what makes the plain non-locking EXISTS read sound
rather than a TOCTOU. Four prior reviews cleared the lock order and the early return only skips later
locks, so it cannot invert.

**Intentional behaviour change:** an environment with an unresolved operation and no route head now returns
the blocked refusal instead of `ErrAgentLeaseActivationNoRouteHead`. The unresolved operation is the more
fundamental problem; the no-route-head case is unreachable in practice since tx1 required a head.

**Open, not closed:** the `MarkReconciled`-clears-the-route-flag guard in the precondition triple is NOT
mutation-proven (it is a tripwire for step G, so its value lands then); item 8's `expired`-instead-of-`n==1`
assertion was not mutation-tested; and no third independent review was run on round 3 — a deliberate scope
call, since the mutation proofs directly pin the moved enforcement point, the remainder is comment-level,
step D touches `Activate` again, and the whole area is behind `activationEnabled = false`.

**Process risk worth recording:** a subagent truncated the untracked
`internal/agent/http/activation_blocked_integration_test.go` and rebuilt it from an earlier read. Untracked
files have NO git recovery path, and "I rebuilt it and it passes" is not evidence that what it previously
asserted survived. The orchestrator read it rather than trusting the report; it was faithful and in fact
stronger than described (it asserts the wire code, not merely the 409).

#### 10c-2b step D committed: sweeper diversion (2026-10-04)

**This is the step that closes the hazard the whole slice is about.** The sweeper no longer destroys the
record of a traffic switch that may already have happened. `closeExpiredOrRevokedAttempt` now diverts when an
unresolved activate operation exists: it records the reconciliation block and leaves the attempt `leased`, the
slot occupied with the fence unchanged, the job leased and the deployment `activating`. Reused the shipped
`blockActivationForReconciliation` (now a free function taking `inject`) rather than hand-rolling the writes.
Review verdict: PASS on production logic — lock order, idempotence, over-reach and post-state all correct.

**The approved plan was wrong and the step fixed it.** Item 2 specified diverting on `switch_authorized`;
after the first sweep the operation is `reconciliation_required` and the attempt is still leased, so the
SECOND sweep would have run the ordinary close path and reintroduced the bug. Mutation proof:
`repeat expireOne #1 = (true, <nil>), want (false, nil)`. **Fourth appearance of the instance-naming defect
in slice 10, and the first caught before shipping.**

**A proxy replaced by a construction.** The discovery exclusion (which stops diverted attempts starving
`ExpireBatch`'s LIMIT) first keyed on `closed_at IS NOT NULL` as a stand-in for "already recorded". Via
`check1` that means "not in either open state" — i.e. it enumerated the open states INDIRECTLY, exactly what
`activation_state.go` exists to prevent, in disguise. It also livelocked: a `started` activate row has
`closed_at IS NULL`, so it was never excluded, was re-discovered every tick, diverted with no writes, and
permanently held a discovery slot — `min(limit,100)` such rows starve a batch completely (only 2 at the
starvation test's own `limit=2`), plus `FOR UPDATE` lock churn on four tables every tick. Now keyed on the
block routine's transition-SOURCE state via one shared constant (`activationStateSwitchAuthorized`) used by
all four sites that previously hard-coded the literal. The exclusion now equals "the diversion would be a
no-op" BY CONSTRUCTION, and the livelock is gone by construction rather than patched. Mutation proof:
reverting gives `plain expired lease state=leased after one tick, want expired: started activations starve
the batch`.
**The distinction that resolves this, now established twice:** naming a single state is legitimate when it is
a TRANSITION'S SOURCE state; illegitimate when it stands in for "unresolved". (Step B established it;
step D's exclusion is the second application.)

**A regression step D introduced, contained in step D.** Before D, the legacy close freed the slot and
`Activate` refused loudly. After D the diversion leaves the slot OCCUPIED by a still-`leased` attempt, so if
`MarkReconciled` cleared only the slot flag the sweeper would exclude that attempt forever,
`ListReconcilingEnvironments` would stop listing it and `Claim` would skip it — **the wedge becomes
INVISIBLE**. `MarkReconciled` now refuses with the typed `ErrAgentLeaseActivationUnresolved` while an
unresolved activate operation exists for the environment, keyed on the durable fact (traffic may have
switched) rather than on the attempt being `leased`, which step G will change. Correct semantics, not just a
guard: an activation-caused block must be resolved by a DECLARED OUTCOME, never by clearing a flag. The admin
path does not use the agent `mapError`; it goes through `environmentsMarkReconciled` in `internal/web/web.go`,
where every error used to be a bare 400 — now 409 for this one.
**Honest consequence, stated in a comment:** between step D and step G an activation-caused block has NO
resolution path at all. Refusing loudly beats half-clearing into an invisible wedge, and both gates are false.

**First test in this slice that discriminates WHICH lock serializes.**
`TestSweeperDiversionWaitsForOperationHoldingTheSlot` holds the operation row and probes the slot with NOWAIT
from a third connection, expecting 55P03 — proving the slot lock is HELD WHILE waiting on the operation. Three
earlier reviews flagged that every concurrency test here could show only that *some* lock was requested.
Caveat recorded: `TestSweeperDiversionTakesSlotLockFirst` re-verifies the CALLERS' SKIP LOCKED vs blocking
behaviour (the diversion takes no slot lock itself), so its comment was corrected; nothing proves the
operation is locked before the route head (low value — the slot serializes every writer).

**Vacuous-assertion shape, third appearance: two mechanisms both keep the attempt open.** The diversion AND
the discovery exclusion each suffice, so three "the sweeper diverts" assertions passed with the diversion
DELETED (they only checked `state=='leased'`, which the exclusion guarantees). Two were made discriminating by
calling `expireOne` directly (bypassing discovery). The third — the HTTP test — exercises the real batch path,
so contorting it would have distorted what it tests; its comment and failure message were corrected to say
"left leased by the batch sweep" instead. **An accurate claim about weaker coverage beats a test bent into
proving something it structurally cannot.**

A deviation from the orchestrator's instruction that was better than the instruction: asked to add
`route_heads` to the HTTP fixture cleanup, the implementer found that fixture deliberately never resets shared
tables and that `route_heads` carries a no-delete trigger — so it gave the test a unique hostname instead.

**HARD PREREQUISITES FOR STEP G** (both confirmed against the source by the orchestrator):
1. **G must close the attempt in the SAME transaction as the resolution.** The moment G moves the operation to
   `succeeded`/`failed` while the attempt is still `leased`, the discovery exclusion stops applying and the
   sweeper runs the ORDINARY close — fence advance, slot flag, job retry — on an attempt G just resolved.
2. **G must add an explicit unresolved-activation check to `Reconcile` AS it adds the leased-attempt branch.**
   `Reconcile` is protected today only INCIDENTALLY: its guard refuses when
   `attemptState == "leased" || closedAt == nil`, which is exactly a diverted attempt's shape — but it answers
   the generic `ErrServerAuthorizationDenied`, not a typed error. The approved plan has G add a branch for
   precisely that shape, which REMOVES the incidental protection and reopens the invisible wedge through the
   agent door that step D just closed on the admin door.
3. A revoked server can never authenticate, so no agent `Reconcile` can resolve its diverted attempt — G's
   path must be operator-only for that case.

**Open/recorded, not fixed:** a `started` activate row still has no programmatic resolution path (pre-existing;
the livelock is fixed but the row remains unresolvable); untested paths are a missing route head under the
sweeper, and a pre-flagged head with a `switch_authorized` operation; `ExpireBatch` returns on the first
`expireOne` error so one persistently failing candidate stalls the batch (pre-existing, but the diversion adds
new error sources); the `operation_lock` hook name is shared with tx2's (disjoint code paths, commented).

#### 10c-2b session end 2026-10-04 — steps A-D committed, step E WIP uncommitted

**Committed and fully verified (each on a green full bar: gofmt, build, vet both tag sets, `diff --check`,
`make check`, `go test -race ./...`, Linux amd64+arm64 cross-builds, 6 fresh-database PG18.4 sweeps
(3 normal + 3 `-race`), 9 concurrency tests at `-race -count=20`):**
- `1354760` step A — migration 000021
- `e8aa2ae` step B — shared unresolved-activation predicate (rule, not state name)
- `f98e6e5` step C — typed refusal in `Activate` (took 3 rounds)
- `612fcb0` step D — sweeper diversion + `MarkReconciled` refusal **(the step the slice exists for)**

Both gates remain false: `activationEnabled` (`cmd/controlplane/main.go:42`), `executorEnabled`
(`cmd/agent/main.go:60`). Nothing has executed; outcome count still 8/18.

**STEP E IS UNCOMMITTED WORK IN PROGRESS — stopped mid-flight, no implementer report exists.**
Files: modified `internal/store/postgres/agent_lease_repository.go`, `internal/agent/http/handler.go`,
`internal/agent/client/activation_status_test.go`, `internal/agent/http/activation_result_test.go`,
`internal/store/postgres/agent_lease_activation_result_test.go`; new
`internal/store/postgres/activation_late_report.go`, `activation_late_report_unit_test.go`,
`agent_lease_activation_late_report_test.go`.

State as measured by the orchestrator, not reported by the agent:
- `go build ./...` and `go vet -tags=integration ./...` are CLEAN, so the tree is coherent.
- The narrowed integration run FAILS 2 of 3 packages' worth of tests — specifically **2 tests, both
  pre-existing 10c-2a tests, and both failures are EXPECTED rather than defects:**
  - `agent_lease_activation_block_failure_test.go:562` —
    `block won but the success report err=agent lease activation report recorded as reconciliation evidence;
    awaiting operator resolution, want ErrAgentLeaseStale`
  - `agent_lease_activation_finalize_failure_test.go:262` —
    `replay err=agent lease activation report recorded as reconciliation evidence; awaiting operator
    resolution, want ErrAgentLeaseStale`
  Both assert `ErrAgentLeaseStale` for a report against an already-blocked operation. **That stale answer was
  the explicit placeholder 10c-2a left for this step** — its own code comment called it "the 10c-2b hook". So
  step E is correctly replacing it, and these two tests must be RE-POINTED at the new late-report sentinel,
  not "fixed". Worth a moment's thought on the first one: it is a RACE test where the block won and the
  success report lost; recording that report as evidence is better than answering stale, but confirm that is
  the intended semantics before re-pointing.
- The agent's dying note said "M8 and M9 exit 1 with 0 fails: likely compile errors" — those were mutation
  runs; build/vet are clean now, so the restore landed, but **which mutation proofs actually ran is unknown.**

**Because no report exists, the next session must re-derive step E's decisions by READING the code** — do not
assume: append-vs-replace for `outcome_summary` (and the 2048-byte/rune-boundary truncation rule), how a
replay is detected, what the `failed` case now returns, the chosen sentinel and wire code, the
statement-by-statement lock order, and the rule for which attempt shapes accept evidence. Then: finish the
mutation proofs, re-point the two tests above, run the full bar, get an independent review, commit.

**HARD PREREQUISITES FOR STEP G** (confirmed against source; see the step D block for detail):
1. G must close the attempt in the SAME transaction as the resolution — once the operation becomes terminal
   while the attempt is still `leased`, the discovery exclusion stops applying and the sweeper runs the
   ordinary close (fence advance, slot flag, job retry) on the attempt G just resolved.
2. G must add an explicit unresolved-activation check to `Reconcile` **as** it adds the leased-attempt branch.
   `Reconcile` is protected today only INCIDENTALLY by `attemptState == "leased" || closedAt == nil`, which is
   exactly a diverted attempt's shape, and it answers the generic `ErrServerAuthorizationDenied`. The planned
   branch targets that shape, so it removes the protection and reopens the invisible wedge through the agent
   door that D closed on the admin door.
3. G's path must be operator-only where a revoked server is involved — a revoked server can never
   authenticate, so no agent `Reconcile` can resolve its diverted attempt.
4. Between D and G an activation-caused block has NO resolution path; `MarkReconciled` refuses by design.

**Remaining steps:** E (finish the WIP), F (finalize parameterization — partly done in B via `fromState`),
G (resolution paths; see prerequisites), H (HTTP/admin/form wiring — invoke `/impeccable`, pastel palette),
I (docs: ADR 0004's amendment around line 655 still says no code implements the sweeper diversion).

#### 10c-2b step E — session 2026-10-19: decisions re-derived, review FAILED, fix round in flight

**Decisions re-derived from code:** late report against a `reconciliation_required` op is EVIDENCE, never
finalizes; append `<existing> | late report #<16-hex sha256 fp>: <body>` (replace would destroy why the block
exists); the fingerprint inside `outcome_summary` IS the replay ledger (entry + `activation_late_report` event
in one tx); 2048-byte cap, cut on rune boundary, marker never cut; replay of the block-creating failure report
stays idempotent nil; `failed` op -> `ErrAgentLeaseIntegrityConflict`; wire 409 `late_report_recorded`
(client maps to non-retryable `ErrIntegrityConflict`); evidence accepted only from the leased slot occupant at
this fence; lock order unchanged (adds only op UPDATE + event INSERT after the op row lock).
Race case confirmed: block won, success lost -> recording as evidence beats stale (host may have switched).

**Orchestrator review found:** late SUCCESS dropped once failure entries filled the column (answered
"recorded" with nothing written) -> success-only reserve. Mutation proofs M1-M10 all killed.

**Independent review FAILED (fix round F1-F4 in flight):**
- F1 HIGH (confirmed in source): `recordBlockInFreshTransaction` returns `NoopOther,nil` when the op is already
  rr -> `blockAfterAbortedTransaction` answers terminal `cause`, failure-commit path answers stale; report
  lost. Window: tx2 rolled back (slot lock released) and sweeper/another report blocked in between. Fix: fresh
  tx records late evidence with the same fingerprint.
- F2: reserve sized for TWO success entries; `matches authorized digest` moved before the digest.
- F3: `RowsAffected()!=1` in recordLateReport -> retryable, not stale.
- F4: restore weakened test assertions (`assertBlocked` in result test; explicit late-event count in
  `assertReconciliationRecorded`).

**Deferred, accepted:**
- Marker forgery: an earlier free-text body containing `late report #<fp>:` makes a distinct report look like
  a replay. Needs an authenticated agent able to compute the hash; revisit if the ledger moves out of free text.
- `isOriginalBlockReplay` swallows any failure report textually equal to the block text (incl. sweeper const).
- Failure fingerprints hash the 512-byte-truncated summary; differences past 512 collide.
- Beyond two distinct success reports, NoRoom writes nothing and still answers 409.
- **For step G:** a success report against an operator-declared `not_activated` (`failed`) op is answered
  `integrity_conflict` with NO trace — it contradicts the operator's verdict and traffic may have switched.
  G should at least emit an event.
- **For 10d:** the client collapses `late_report_recorded` into `ErrIntegrityConflict`; the agent cannot tell
  "evidence recorded, await operator" from a real conflict. Verify what the agent caller does on
  `ErrIntegrityConflict` (must not complete/fail the job and contradict the block); consider a dedicated
  `ErrLateReportRecorded`.

**Step E fix rounds closed 2026-10-19 — second independent review PASS.** F1-F4 landed; fresh-transaction rr
branch now records late evidence (shared `writeLateEvidence`, same fingerprint, retry through tx2 is a replay),
original-block replay checked before the occupant check on both paths, reserve = 512 (two success entries),
success body puts `matches authorized digest` before the digest, `RowsAffected!=1` retryable. Mutations
M1-M16 all killed (M13, M14 survived first; killed by new caller-level race tests). Full bar green: gofmt,
build, vet both tag sets, `diff --check`, Linux amd64/arm64, `make check`, `go test -race ./...` (29 pkgs),
6/6 fresh-database PG18 sweeps (3 normal + 3 `-race`), 14 activation concurrency tests at `-race -count=20`.
Accepted, not fixed: a retried success report whose own failure created the block adds one entry; an entry
cut exactly at the cap may end in a space. Next: step F.

#### 10c-2b step F committed: finalize helper works from reconciliation_required (2026-10-19)

`finalizeActivationSucceeded` now serves both source states (live-schema introspection confirmed each rule):
- op `closed_at=COALESCE(closed_at,$now)` — 000021 makes an rr op's closed_at immutable.
- route head UPDATE clears `reconciliation_required` (the only place it is ever cleared); a FLAGGED head with
  `fromState=switch_authorized` is refused as `ErrAgentLeaseCorruptState` BEFORE any write (review finding:
  the unconditional clear would erase an unresolved flag on tx2). Not required for rr: unflagged head resolves.
- slot UPDATE frees + clears the flag pair in one statement only when the flag is NULL or owned by this
  attempt (rule = owner, not fromState; a foreign flag matches 0 rows -> corrupt).
- RowsAffected checks on attempt (`AND state='leased'`), slot, job -> `ErrAgentLeaseCorruptState`; on tx2 these
  are deterministic -> durable block via `blockAfterFinalizeFailure` (proved end to end by
  `TestReportActivationResultCorrupt{FlaggedHead,SlotFlaggedByOtherAttempt}Blocks`).
Mutations F-M1..F-M8 killed. Independent review PASS. Full bar green (same stages as step E).

**For step G (from review + implementer):**
- G writes the `reconciled` event and audit row itself; take locks in documented order; read the clock AFTER locks
  (slot/job/release `updated_at` use a bare `$now`).
- On `ErrAgentLeaseActivationRouteMoved` or any finalize error: roll back, NO new block (do not route through
  `blockAfterFinalizeFailure` — `blockActivationForReconciliation` requires `switch_authorized`).
- Pre-verify the slot flag belongs to this attempt (helper only refuses a FOREIGN flag).
- Helper requires attempt+job still `leased`; decide whether any legitimate rr shape violates that.
- `outcome_summary` is overwritten by `in.summary`: G must pass block evidence + late evidence + declared text,
  bounded to 2048 bytes, or the evidence is lost.
- Pass a deliberate `resultDigest` (op.route_digest per design 6.4).
- Known, pinned, unchanged: `blockActivationForReconciliation` overwrites a foreign `reconciliation_attempt_id`
  (reachable only on already-corrupt state).

#### 10c-2b step G1 committed: operator resolution `ResolveActivation` (2026-10-19)

Plan + owner decisions: `tasks/10c-2b-step-G-plan.md`. Repository only (no web/handler/client); `MarkReconciled`
behaviour identical (tail factored into `clearPlainReconciliation`). Outcomes `activated | not_activated |
no_activation`, required, checked both ways under the locks (`ErrAgentLeaseOutcomeMismatch`). Lock order: admin
session -> slot -> [non-locking owner peek] -> job -> attempt -> op (`unresolvedActivationSQL`) -> deployment ->
head -> releases (activated only); clock after all locks. Only exact `reconciliation_required` resolvable.
Failure = rollback, never a new block. Audit reason `outcome=<x>; <reason>` (validReason on the combined string).

Review PASS; fix round: replay anchored to the environment's highest-id attempt (agent_job_events has only a
per-job `smallint` sequence, no global order); `no_activation` refuses a leased attempt with no unresolved op
(corrupt); race test uses two admin sessions so both queue on the slot; clock test holds head/release rows too.
Full bar green incl. 18 concurrency tests at `-race -count=20`.

**Accepted:** after a later attempt B is itself resolved, a stale duplicate of A's outcome is judged against B
(nothing written either way). Malformed-digest guard untestable (CHECK forbids the shape) — defence in depth.

**For G2:** explicit unresolved-activation check + branch B together; agent `reconciled` events are read by
`confirmResolvedOutcome` (anchor = latest attempt); late success vs a declared `not_activated` op still leaves NO
trace (tx2 `failed` arm) — emit an event there. **For H:** form calls `ResolveActivation` for all three outcomes;
bound the operator reason to ~489 bytes (prefix counts toward 512); map errors, never print `err.Error()`
(corrupt-state texts carry ids).

#### 10c-2b step G2a committed: agent `ReconcileActivation` + `Reconcile` hardening (2026-10-19)

New method beside `Reconcile` (deviation from the outline, recorded in the plan): keeps `Reconcile`'s incidental
leased-attempt denial intact until H rewires the endpoint. Shared post-lock core `resolveDeclared` used by both
`ResolveActivation` (unchanged; G1 tests untouched) and `ReconcileActivation`. Agent path: creds SHARE -> [peek:
server/env/fence denied BEFORE the job lock] -> slot -> job -> attempt -> op -> deployment -> head -> releases;
branch B for a leased attempt at exactly `reconciliation_required`; `activated` requires digest == op.route_digest;
no audit row; replay anchored to the named attempt. `Reconcile` now refuses a closed attempt with an unresolved
activate op as `ErrAgentLeaseCorruptState` (explicit, by rule); without it Reconcile silently cleared the flag.
HTTP test `TestAgentHTTPStrandedEnvironmentActivateIsBlockedAfterLegacySweeperClose` now asserts 409
`corrupt_state` on that shape and forces the legacy clear in SQL. Review PASS + follow-ups. Full bar green; the
concurrency stage (21 tests x20 `-race`, 656s) now needs `-timeout 60m` — the default 10m killed it mid-run.
Next: G2b (trace a late success against a declared `not_activated`).

#### 10c-2b step G2b committed: trace a report that contradicts a declared outcome (2026-10-19)

tx2's terminal arms: when the answer would be `integrity_conflict` AND the attempt has a `reconciled` event (outcome
was DECLARED by `ResolveActivation`/`ReconcileActivation`), append ONE `activation_late_report` event (dedup: none
with a sequence above the attempt's latest `reconciled`), commit, still answer `integrity_conflict`. Widened from the
plan's single case by rule: success after `not_activated`, failure after `activated`, wrong digest after `activated`.
tx2-finalized outcomes (no `reconciled` event) byte-identical. Commit failure stays retryable; retry deduped.
Event sequence = `MAX+1` per job under the job lock (no wrap; smallint overflow would error, ~32k events/job).
**For 10d:** a failure report after a declared `not_activated` agrees but is still answered `integrity_conflict`
(the `failed` arm has no agreeing path) — misleading but terminal either way.
Review PASS. Full bar green (concurrency 21 tests x20 `-race`, 719s).
