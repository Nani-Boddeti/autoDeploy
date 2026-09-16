# AutoDeploy implementation handoff

Snapshot: 2026-08-13. Branch: main; base c18e3f3 (feat: add secure secrets and GitHub checkout). The GitHub webhook/durable-enqueue slice is implemented but uncommitted alongside source, migrations, ADR amendments, tests, and plan/memory updates. .tmp/ is intentionally untracked but **not gitignored**: exclude it manually and never use blanket git add. Treat the worktree as shared: inspect git status --short before changing or staging anything.

## Scope and current progress

AutoDeploy replaces Coolify for the owner's private GitHub Dockerfile applications on Hetzner. V1 has one administrator and one initial server but retains owner/project/environment/server scope. The approved stack is Go, PostgreSQL, Docker/BuildKit, and Traefik; routes require HTTPS and environments select health-gated blue/green by default or explicit stop-then-start.

Progress is **7/18 implementation-sequence outcomes (38.9%, approximately 39%)**, with roughly **35–40% weighted effort** complete. This is an execution roadmap, not a maturity claim: agents, leases, host execution, release routing, operations UI, packaging, and end-to-end recovery remain.

Completed outcomes:

1. Go repository/tooling/CI and PostgreSQL migration runner.
2. Deployment aggregate, transition/event model, and PostgreSQL persistence.
3. Administrator bootstrap, sessions, CSRF, authorization, audit, and throttling.
4. Versioned envelope encryption, KEK rotation metadata, and centralized redaction.
5. GitHub App installation/setup, explicit repository binding, narrow checkout credentials, and exact-commit checkout adapter.
6. Raw GitHub webhook verification, delivery/replay idempotency, repository/ref filtering, and durable deployment-job enqueue.
7. Architecture decisions ADR 0001–0006 that constrain remaining work.

The next approved slice is **server registration, scoped agent authentication, heartbeat, job claim/renewal, cancellation, and recovery**. It must use existing queued jobs; do not bypass them or let agents access PostgreSQL.

## Architecture and trust boundaries

- The public control plane owns authentication, authorization, desired state, PostgreSQL, GitHub App credentials, webhook verification, jobs, and audit. It must not receive Docker socket, remote Docker, or broad SSH access.
- The agent initiates authenticated confidential outbound communication. It owns only its target host Docker/BuildKit, scoped routing mutations, cleanup, and sanitized reports; it never has control-plane App keys or PostgreSQL credentials.
- PostgreSQL is the V1 durable at-least-once queue. Receipt + authorized deployment + job commit atomically. Exactly-once execution is deliberately not claimed; leases, environment fences, idempotent host operations, and reconciliation are required next.
- GitHub authorization uses numeric App/installation/repository identities plus an explicit active project binding and target fields. Names, sender data, callbacks, and a valid signature alone are not authorization.
- Secrets stay out of the repository and durable queue payloads. Protected mounted files provide provider credentials; encrypted build/runtime values use envelope encryption; redaction fails closed.

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

Webhook ingress accepts only exact POST /webhooks/github, canonical single headers, trusted HTTPS provenance, and a bounded body. HMAC-SHA256 is constant-time checked against an active plus bounded retained secret ring before JSON parsing/database work. Malformed terminal receipts are sanitized; retryable persistence failures stay retryable. Push eligibility requires active App, installation, repository, binding, target fields, exact full ref, and nonzero lowercase 40-hex SHA. Delivery ID and verified body digest have independent replay defense; automatic intent is unique by active binding/repository/ref/commit.

## Schema, configuration, and entrypoints

Migrations are forward-only and embedded by migrations/migrations.go:

- 000001 deployments; 000002–000003 administrator/auth recovery; 000004 encrypted secrets; 000005–000008 GitHub setup, owner integrity, checkout authorization/reservations; 000009 binding targets; 000010 webhook deliveries, automatic intents, queued agent jobs, and enqueue events.
- 000011 corrects 000008's checkout-reservation clone URL CHECK: its double-escaped regex (`\\.` in a standard string) rejected every canonical clone URL, so no reservation could be inserted.
- 000010 intentionally does **not** create claims, attempts, leases, environment slots, fences, retry scheduling, or agent credentials. Those belong to the next slice.

cmd/controlplane/main.go is the composition root. GitHub is opt-in; when enabled it loads GitHub App configuration and protected signing/webhook-secret files, ensures App registration, builds browser setup service, and mounts the isolated webhook route outside browser session/CSRF handling. It clears loaded webhook bytes on shutdown. Browser settings and trusted proxy CIDRs come from internal/config/database.go. Configuration categories:

- database URL file and credential-root/owner settings;
- public HTTPS origin, listen address, CSRF and username-throttle key rings, trusted proxy CIDRs;
- optional secret-subsystem/KEK manifest;
- opt-in GitHub App stable identity/installation URL and protected private-key file;
- active webhook-secret version/file and optional retained-secret-file overlap.

Never document or commit values. Startup fails closed on invalid enabled GitHub/credential settings; disabled GitHub preserves existing operation.

## Validation evidence and limits

CI is [.github/workflows/ci.yml](../.github/workflows/ci.yml): a formatting diagnostic (gofmt -d, which prints differences but does not itself fail), vet, unit tests, PostgreSQL 18 integration tests, build, and Linux amd64/arm64 checkout compilation. Makefile exposes enforcing local check/test/integration targets; make check runs fmt, whose gofmt -l assertion fails on unformatted files. CI formatting enforcement is a small known gap; do not describe it as enforcing until CI changes.

Current webhook-slice evidence:

- focused webhook domain/handler/repository unit and race tests; go vet; formatting and git diff --check;
- 2026-09-16 correction: the earlier recorded PostgreSQL 18 evidence was not reproducible. A fresh full integration run failed on a stale deployment-test TRUNCATE (000010 FKs), a binding-target test violating UNIQUE(owner_id, repository_id), the 000008 clone URL CHECK (fixed by 000011), and a race-only nondeterministic secret replace concurrency test (test barrier fixed; production CAS verified by logging). After fixes, full integration passes normal and -race across three fresh runs;
- fresh PostgreSQL 18 normal and race runs for webhook repository/migrations: delivery/digest conflicts, automatic-intent idempotency, authorization races, rollback at write boundaries, immutable rows/events, lifecycle disable/removal;
- repository-wide unit/race/vet/diff checks, independent QA, and zero-context security review recorded in [tasks/todo.md](../tasks/todo.md).

Checkout implementation is complete, but the top-level isolated-checkout milestone remains unchecked until an operator runs the non-cached privileged smoke on provisioned Linux: installed root helpers, XFS mounted with prjquota, delegated cgroup v2, and distinct checkout UID/GID must prove typed GitInit, quota/workspace cleanup, and no cgroup leaf remains. Linux cross-compilation/tagged-test compilation do not prove that boundary; macOS cannot substitute. No agent transport, claim protocol, lease renewal, cancellation cleanup, fencing persistence, recovery, Docker/BuildKit execution, Traefik, or end-to-end deployment validation exists yet. Do not present the current queue as runnable by an agent.

## Next slice: server agent and leases

Implement sequentially under ADR 0001/0003/0004:

1. Durable server registration: immutable server identity, scoped credential digest/rotation/revocation, protocol version, owner scope, audit. Agent credentials cannot authorize browser administration or another server.
2. Authenticated outbound heartbeat with bounded sanitized capability/observation data and revision/revocation checks. Disconnect never erases desired state.
3. Claim queued jobs in short READ COMMITTED transactions using FOR UPDATE SKIP LOCKED, durable attempt, 90-second lease, server authorization, environment slot, monotonic signed BIGINT fence, canonical PostgreSQL time, frozen secret versions, append-only events.
4. Renew every 30 seconds with monotonic replay-safe sequences; complete idempotently only with matching terminal operation/digest. Reject stale credential, server, attempt, fence, lease, or result.
5. Implement queued/retry-wait cancellation; leased cancellation has bounded cleanup authority. Expiry advances fencing and blocks reuse pending reconciliation; recovery tolerates host effects before response commit.

Prove simultaneous claims, agent replay, revoked credential after claim, clock uncertainty, lost completion response, expired lease with late success, cancellation during activation, lost agent fence state after restart, and no cross-server/environment mutation.

## Resume order and commands

1. Read this file, [tasks/todo.md](../tasks/todo.md), [MEMORY.md](../MEMORY.md), then ADR 0001, ADR 0003, and ADR 0004. For webhook changes also read ADR 0002 and migration checks.
2. Before relying on checkout from agent execution, run/preflight the pending privileged Linux checkout smoke with Makefile's explicit test-checkout-integration contract on provisioned XFS prjquota plus delegated cgroup v2; keep the top-level checkout milestone unchecked until it passes.
3. Inspect git status --short, touched migrations, and focused tests before editing. Do not reset, clean, overwrite shared uncommitted files, or use blanket git add because .tmp/ is not ignored.
4. Create/approve a detailed plan in tasks/todo.md for a nontrivial slice, including transaction ownership, lock order, negative tests, migration, and rollback behavior.
5. Run make check, go test -race ./..., and a fresh PostgreSQL 18 integration run using CI's AUTODEPLOY_TEST_DATABASE_URL contract. Run Linux cross-builds where tagged code changes; finish with git diff --check and independent review/QA.

Commits are atomic and descriptive (feat:, fix:, refactor:, chore:), manually exclude .tmp/ (it is untracked, not ignored), credentials, and incidental worktree changes, and never include generated attribution. Do not commit the current slice until its shared diff is reviewed as one coherent unit.

## Definition of done for the next slice

Control plane and agent authenticate independently; agents claim only their server's authorized queued work; every claim/renewal/cancel/expiry/completion is durable, fenced, auditable, idempotent, and bounded; revoked/expired/stale authority cannot mutate a host; recovery is proven with live PostgreSQL concurrency tests; no credential or secret reaches logs, events, jobs, or the wrong trust boundary. The existing healthy release remains protected until a future health-gated activation slice explicitly authorizes traffic change.
