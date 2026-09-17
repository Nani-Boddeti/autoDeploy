# AutoDeploy implementation handoff

Snapshot: 2026-09-17. Branch: main; base 8105a85 (feat: add host agent enrollment and run loop). The server agent, credentials, and leases slice (commits 89d01b7 through 8105a85) is committed. .tmp/ is intentionally untracked but **not gitignored**: exclude it manually and never use blanket git add. Treat the worktree as shared: inspect git status --short before changing or staging anything.

## Scope and current progress

AutoDeploy replaces Coolify for the owner's private GitHub Dockerfile applications on Hetzner. V1 has one administrator and one initial server but retains owner/project/environment/server scope. The approved stack is Go, PostgreSQL, Docker/BuildKit, and Traefik; routes require HTTPS and environments select health-gated blue/green by default or explicit stop-then-start.

Progress is **8/18 implementation-sequence outcomes (44.4%, approximately 44%)**, with roughly **40–45% weighted effort** complete (leases/agent-protocol is transport and coordination, not host execution). This is an execution roadmap, not a maturity claim: host execution, release routing, operations UI, packaging, and end-to-end recovery remain.

Completed outcomes:

1. Go repository/tooling/CI and PostgreSQL migration runner.
2. Deployment aggregate, transition/event model, and PostgreSQL persistence.
3. Administrator bootstrap, sessions, CSRF, authorization, audit, and throttling.
4. Versioned envelope encryption, KEK rotation metadata, and centralized redaction.
5. GitHub App installation/setup, explicit repository binding, narrow checkout credentials, and exact-commit checkout adapter.
6. Raw GitHub webhook verification, delivery/replay idempotency, repository/ref filtering, and durable deployment-job enqueue.
7. Architecture decisions ADR 0001–0006 that constrain remaining work.
8. Server registration with single-use enrollment tokens, scoped agent credentials (digest-only storage, rotation, revocation), authenticated heartbeat, fenced job claim/renewal, cancellation, expiry, and reconciliation over the existing queue; an authenticated `/agent/v1/` HTTP protocol; and a host agent client (enroll, credential file, per-environment fence state, heartbeat, renew loop) with no executor wired in.

The next approved slice is **host execution**: invoke the existing exact-commit checkout adapter from the agent, add Docker/BuildKit build execution, and start/health-check the runtime container. No release activation or Traefik routing is in scope for that slice.

## Architecture and trust boundaries

- The public control plane owns authentication, authorization, desired state, PostgreSQL, GitHub App credentials, webhook verification, jobs, leases, server/credential records, and audit. It must not receive Docker socket, remote Docker, or broad SSH access.
- The agent initiates authenticated confidential outbound communication over `/agent/v1/`. It owns only its target host Docker/BuildKit, scoped routing mutations, cleanup, and sanitized reports; it never has control-plane App keys or PostgreSQL credentials. It generates its own bearer credential locally at enrollment and sends only its SHA-256 digest to the control plane, which stores digest-only.
- PostgreSQL is the V1 durable at-least-once queue, now with durable leases, environment fences, and attempts. Receipt + authorized deployment + job commit atomically; claim/renew/complete/cancel/expire/reconcile are durable, fenced transactions. Exactly-once execution is deliberately not claimed; idempotent host operations remain the agent's responsibility once an executor exists.
- GitHub authorization uses numeric App/installation/repository identities plus an explicit active project binding and target fields. Names, sender data, callbacks, and a valid signature alone are not authorization.
- Secrets stay out of the repository and durable queue payloads. Protected mounted files provide provider credentials; encrypted build/runtime values use envelope encryption; redaction fails closed. Agent bearer credentials and enrollment tokens follow the same never-log/never-persist-plaintext rule: only digests are stored, and unauthenticated `/agent/v1/` rejections (bad/missing/expired credential or token) are deliberately **not** audited, to avoid letting an unauthenticated caller drive unbounded audit-row write amplification.

Read [ADR 0001](adr/0001-control-plane-agent-separation.md), [ADR 0002](adr/0002-github-app-authentication.md), [ADR 0003](adr/0003-secret-handling.md), [ADR 0004](adr/0004-postgresql-queue-leases.md), [ADR 0005](adr/0005-release-strategies.md), and [ADR 0006](adr/0006-administrator-authentication.md) before altering these boundaries.

## Implemented subsystems and invariants

| Area | Source | Invariant |
| --- | --- | --- |
| Deployment | internal/deployment; deployment_repository.go | Immutable identity, validated transitions, append-only events, revision CAS; no silent repair. |
| Administrator | internal/auth; internal/web; auth repository | Local bootstrap/recovery only; opaque server-side sessions, CSRF/origin/proxy rules, throttling, owner scope, audit-with-mutation. |
| Secrets | internal/crypto; internal/credential; internal/config | Protected external keys; AES-GCM envelope versions with AAD scope; no plaintext/API/log/job leakage. |
| GitHub checkout | internal/githubapp; internal/gitcheckout; cmd/checkout-* | Implementation/static validation complete: App key stays control-plane-only; token is exact-repository/read-only/transient; checkout exact authorized commit. Privileged Linux operational validation remains pending. |
| Webhook/enqueue | internal/github/http; internal/github; internal/queue; webhook_job_repository.go | Verify bounded raw bytes before parse; no raw body/signature/secret persistence; one transaction commits receipt, deployment/event, job/event. |
| Lifecycle invalidation | webhook processor/repository | Installation suspension/deletion and repository removal disable authority/bindings atomically; addition/unsuspension never silently activates a binding. |
| Servers/credentials | internal/server; server_repository.go (migration 000012) | Immutable server identity; one active credential (digest-only, versioned) per server; single-use enrollment tokens (`enr.` prefix, 15 min TTL); rotation revokes+issues atomically; revocation is immediate and irreversible; append-only `server_audit_events`. |
| Leases/fencing | agent_lease_repository.go, agent_lease_sweeper.go (migration 000013) | Lock order server/credential (SHARE) -> environment slot (UPDATE) -> job (UPDATE SKIP LOCKED) -> attempt; monotonic never-decreasing environment fence; claim/renew/complete/cancel/expire/reconcile revalidate authority from locked rows, never a discovery snapshot; expiry sets `reconciliation_required`, which blocks claims until the owning server reconciles or an audited administrator marks reconciled; sweeper uses `pg_try_advisory_xact_lock` with a bounded batch. |
| Agent HTTP | internal/agent/http (package agenthttp) | POST-only `/agent/v1/{enroll,heartbeat,jobs/claim,leases/renew,leases/complete,leases/cancel-ack,environments/reconcile}`; webhook-identical trusted-HTTPS provenance; strict bounded JSON (`DisallowUnknownFields`); one generic 401 for every auth failure; authorization re-derived from locked rows inside each transaction; unauthenticated rejections are not audited. |
| Agent client/state | internal/agent/client; internal/agent/state; cmd/agent | `enroll` generates the credential locally and writes it 0600, no-clobber, atomically; `run` loads the credential O_NOFOLLOW/0600/owner-checked, heartbeats with `executor_available=false`, and never claims without a registered Executor; per-environment fence state plus a `.fence-index.json` manifest fails closed on missing/unindexed/corrupt state; HTTP client is TLS-only, never follows redirects, and retries only 503/network (never claim). |
| Server admin web | internal/web/web.go | Session+CSRF-protected `/servers`, `/servers/register`, `/servers/enrollment`, `/servers/revoke` (reason + `confirm=revoke`), `/environments/mark-reconciled`; owner-scoped; enrollment token shown once in the response body, never redirected/logged/cached; wired regardless of `AUTODEPLOY_AGENT_ENABLED`. |

Webhook ingress accepts only exact POST /webhooks/github, canonical single headers, trusted HTTPS provenance, and a bounded body. HMAC-SHA256 is constant-time checked against an active plus bounded retained secret ring before JSON parsing/database work. Malformed terminal receipts are sanitized; retryable persistence failures stay retryable. Push eligibility requires active App, installation, repository, binding, target fields, exact full ref, and nonzero lowercase 40-hex SHA. Delivery ID and verified body digest have independent replay defense; automatic intent is unique by active binding/repository/ref/commit.

## Schema, configuration, and entrypoints

Migrations are forward-only and embedded by migrations/migrations.go:

- 000001 deployments; 000002–000003 administrator/auth recovery; 000004 encrypted secrets; 000005–000008 GitHub setup, owner integrity, checkout authorization/reservations; 000009 binding targets; 000010 webhook deliveries, automatic intents, queued agent jobs, and enqueue events.
- 000011 corrects 000008's checkout-reservation clone URL CHECK: its double-escaped regex (`\\.` in a standard string) rejected every canonical clone URL, so no reservation could be inserted.
- 000012 adds server identity: `servers` (immutable id/owner/created_at, status active|revoked, `last_heartbeat_at`), `server_credentials` (version, 32-byte digest, one-active partial unique index, only active->revoked update), `server_enrollment_tokens` (digest, issued_at, expires_at, consumed_at, single use), and append-only `server_audit_events`.
- 000013 adds the lease schema: replaces the 000010 `agent_jobs_no_update` trigger with a transition-validating trigger and widens the job state CHECK to queued|leased|retry_wait|succeeded|failed|cancelled|dead, plus `attempt_count`/`cancel_requested_at`; adds `environment_slots` (never-decreasing fence, current attempt, `reconciliation_required`), `agent_job_attempts` (job/attempt_number/server/environment/fence/state/renewal_sequence/lease+authority deadlines/result digest, legal-transition-only), and `frozen_secret_versions` (append-only, empty until the execution slice); widens `agent_job_events`; adds the claimable and authority-expiry partial indexes from ADR 0004.

cmd/controlplane/main.go is the composition root. GitHub is opt-in; when enabled it loads GitHub App configuration and protected signing/webhook-secret files, ensures App registration, builds browser setup service, and mounts the isolated webhook route outside browser session/CSRF handling. It clears loaded webhook bytes on shutdown. The `/agent/v1/` handler and lease repository are constructed unconditionally (server admin and web routes work regardless of the flag), but the route is only mounted and the sweeper only started when the agent protocol is enabled. Browser settings and trusted proxy CIDRs come from internal/config/database.go. Configuration categories:

- database URL file and credential-root/owner settings;
- public HTTPS origin, listen address, CSRF and username-throttle key rings, trusted proxy CIDRs;
- optional secret-subsystem/KEK manifest;
- opt-in GitHub App stable identity/installation URL and protected private-key file;
- active webhook-secret version/file and optional retained-secret-file overlap;
- opt-in `AUTODEPLOY_AGENT_ENABLED` (strict boolean, default off) gating the `/agent/v1/` route and sweeper, and `AUTODEPLOY_AGENT_SWEEP_INTERVAL` (default 15s, bounded 5s..5m).

Never document or commit values. Startup fails closed on invalid enabled GitHub/credential/agent settings; disabled GitHub or disabled agent protocol preserves existing operation.

## Validation evidence and limits

CI is [.github/workflows/ci.yml](../.github/workflows/ci.yml): a formatting diagnostic (gofmt -d, which prints differences but does not itself fail), vet, unit tests, PostgreSQL 18 integration tests, build, and Linux amd64/arm64 checkout compilation. Makefile exposes enforcing local check/test/integration targets; make check runs fmt, whose gofmt -l assertion fails on unformatted files. CI formatting enforcement is a small known gap; do not describe it as enforcing until CI changes. The integration target now runs `go test -p 1 -tags=integration ./internal/store/postgres ./internal/agent/http ./migrations`: `-p 1` is required because these packages share one database and cannot run as concurrent Go test binaries.

Current webhook-slice evidence:

- focused webhook domain/handler/repository unit and race tests; go vet; formatting and git diff --check;
- 2026-09-16 correction: the earlier recorded PostgreSQL 18 evidence was not reproducible. A fresh full integration run failed on a stale deployment-test TRUNCATE (000010 FKs), a binding-target test violating UNIQUE(owner_id, repository_id), the 000008 clone URL CHECK (fixed by 000011), and a race-only nondeterministic secret replace concurrency test (test barrier fixed; production CAS verified by logging). After fixes, full integration passes normal and -race across three fresh runs;
- fresh PostgreSQL 18 normal and race runs for webhook repository/migrations: delivery/digest conflicts, automatic-intent idempotency, authorization races, rollback at write boundaries, immutable rows/events, lifecycle disable/removal;
- repository-wide unit/race/vet/diff checks, independent QA, and zero-context security review recorded in [tasks/todo.md](../tasks/todo.md).

Server/leases-slice evidence (steps 1-6c, each independently reviewed before the next):

- every step's `make check`, `go vet` (including `-tags=integration`), and `go test -race ./...` passed before integration;
- three fresh full PostgreSQL 18 integration runs, normal and `-race`, per step (server domain/migration 000012; lease schema/migration 000013; claim+renew; complete/cancel/expire/reconcile+sweeper; agent HTTP alone on a fresh DB, then the full suite); concurrency-sensitive tests additionally repeated `-race -count=20`;
- correctness and security review each step; findings fixed before proceeding (notable: Step 1 owner-scope security FAIL fixed and re-reviewed PASS; Step 3 claim deadlock/occupant-check/pre-auth/limit fixes; Step 5 write-amplification fix for unauthenticated-rejection audit rows; Step 6c redirect-credential-leak and claim-retry fixes);
- Linux amd64/arm64 builds for `cmd/agent`.

Checkout implementation is complete, but the top-level isolated-checkout milestone remains unchecked until an operator runs the non-cached privileged smoke on provisioned Linux: installed root helpers, XFS mounted with prjquota, delegated cgroup v2, and distinct checkout UID/GID must prove typed GitInit, quota/workspace cleanup, and no cgroup leaf remains. Linux cross-compilation/tagged-test compilation do not prove that boundary; macOS cannot substitute.

No Docker/BuildKit or Traefik executor exists yet; the host agent client never claims work because no executor is registered in this slice (heartbeat reports `executor_available=false`). There is no release activation and no checkout invocation from the agent. Do not present the current queue as runnable by an agent.

## Next slice: host execution

Implement sequentially under ADR 0001/0003/0005, wiring an Executor into `cmd/agent` so claimed leases do real work for the first time:

1. Invoke the existing exact-commit checkout adapter (internal/gitcheckout) from the agent for a claimed lease's authorized commit, scoped to the environment workspace.
2. Docker/BuildKit build execution with secret mounts (ADR 0003), resource/time limits, immutable ownership labels (environment/deployment/attempt/fence), and redacted log output.
3. Runtime container creation: environment variables, stable environment network, resource limits, restart policy, immutable release labels; no published host ports.
4. HTTP/TCP/container health policies with bounded retries, matching ADR 0005's default gate (exact host/path/port, 2xx, redirects off, three consecutive successes, 2s interval/timeout, 60s overall deadline).

Release activation and Traefik routing remain out of scope for this slice; they follow in the blue/green/rollback slice per ADR 0005.

## Known follow-ups from this slice

- Agent-initiated credential rotation protocol: rotation currently requires the agent to already hold its raw credential; there is no agent-initiated rotation path yet (Step 6b).
- Server-side claim idempotency key so a lost claim response cannot orphan a lease until expiry (Step 6c).
- Register resubmission can create an orphan unenrolled server; needs dedupe or cleanup (Step 6b).
- Persist the reconcile outcome digest once a consumer exists; needs a migration (Step 4).
- A revoked server cannot reconcile its own environments; those need administrator `mark-reconciled` (Step 4).
- No rate limiting on `/agent/v1/` (low severity; a valid credential is still required) (Step 5).
- ADR 0004 amendments below (renewal events, attempt-count/expiry, claim slot pre-creation and lock order, reconciliation ownership, revocation-only attempt closure, sweeper locking) reflect what shipped; re-read them before extending the lease protocol.

## Resume order and commands

1. Read this file, [tasks/todo.md](../tasks/todo.md), [MEMORY.md](../MEMORY.md), then ADR 0001, ADR 0004 (including its 2026-09-17 amendments), and ADR 0005. For webhook changes also read ADR 0002 and migration checks.
2. Before relying on checkout from agent execution, run/preflight the pending privileged Linux checkout smoke with Makefile's explicit test-checkout-integration contract on provisioned XFS prjquota plus delegated cgroup v2; keep the top-level checkout milestone unchecked until it passes.
3. Inspect git status --short, touched migrations, and focused tests before editing. Do not reset, clean, overwrite shared uncommitted files, or use blanket git add because .tmp/ is not ignored.
4. Create/approve a detailed plan in tasks/todo.md for a nontrivial slice, including transaction ownership, lock order, negative tests, migration, and rollback behavior.
5. Run make check, go test -race ./..., and a fresh PostgreSQL 18 integration run using CI's AUTODEPLOY_TEST_DATABASE_URL contract (`-p 1` across internal/store/postgres, internal/agent/http, and migrations). Run Linux cross-builds where tagged code changes; finish with git diff --check and independent review/QA.

Commits are atomic and descriptive (feat:, fix:, refactor:, chore:), manually exclude .tmp/ (it is untracked, not ignored), credentials, and incidental worktree changes, and never include generated attribution.

## Definition of done for the next slice

A claimed lease drives a real checkout, build, and container start on the target host, gated by the ADR 0005 default health check, with no release activation or traffic change; every host operation is idempotent, fenced, and labeled so recovery/reconciliation can identify and clean up partial work; secrets reach the build only through BuildKit mounts and runtime environment injection inside the trusted-host boundary, never logs or job payloads; failures leave the currently healthy release untouched; recovery is proven with live PostgreSQL concurrency tests plus host-side idempotency tests; docs updated to 9/18.
