# AutoDeploy implementation handoff

Snapshot: 2026-09-21. Branch: main; base 177e181 (feat: add runtime configuration admin pages). The server agent/credentials/leases slice (89d01b7..8105a85) and the control-plane half of host execution (slice 9a, 2f2b427..177e181) are committed and unpushed. .tmp/ is intentionally untracked but **not gitignored**: exclude it manually and never use blanket git add. Treat the worktree as shared: inspect git status --short before changing or staging anything.

## Scope and current progress

AutoDeploy replaces Coolify for the owner's private GitHub Dockerfile applications on Hetzner. V1 has one administrator and one initial server but retains owner/project/environment/server scope. The approved stack is Go, PostgreSQL, Docker/BuildKit, and Traefik; routes require HTTPS and environments select health-gated blue/green by default or explicit stop-then-start.

Progress is still **8/18 implementation-sequence outcomes (approximately 44%)**, with roughly **50–55% weighted effort** complete. Slice 9a moved no outcome on that list and is not meant to: it is the control-plane foundation host execution needs — frozen execution configuration, frozen secret versions, fenced materialization, phase progress, and a checkout credential seam. This is an execution roadmap, not a maturity claim. **Nothing has executed yet**: no image has been built, no container started, no deployment run end to end. Slice 9b is now code-complete — the agent can check out, build, run, and health-gate — but `cmd/agent` deliberately keeps `executorEnabled = false`, so no executor is registered, it heartbeats `executor_available=false`, and it claims nothing. Release routing, operations UI, packaging, and end-to-end recovery remain.

Completed outcomes:

1. Go repository/tooling/CI and PostgreSQL migration runner.
2. Deployment aggregate, transition/event model, and PostgreSQL persistence.
3. Administrator bootstrap, sessions, CSRF, authorization, audit, and throttling.
4. Versioned envelope encryption, KEK rotation metadata, and centralized redaction.
5. GitHub App installation/setup, explicit repository binding, narrow checkout credentials, and exact-commit checkout adapter.
6. Raw GitHub webhook verification, delivery/replay idempotency, repository/ref filtering, and durable deployment-job enqueue.
7. Architecture decisions ADR 0001–0006 that constrain remaining work.
8. Server registration with single-use enrollment tokens, scoped agent credentials (digest-only storage, rotation, revocation), authenticated heartbeat, fenced job claim/renewal, cancellation, expiry, and reconciliation over the existing queue; an authenticated `/agent/v1/` HTTP protocol; and a host agent client (enroll, credential file, per-environment fence state, heartbeat, renew loop) with no executor wired in.

Beyond those outcomes, **slice 9a (control-plane host-execution foundation) is complete and committed**: migrations 000014/000015, claim-time freeze of execution configuration and secret versions, the minimal release model, fenced secret materialization, deployment phase progress, the checkout credential lifecycle, and the session/CSRF admin pages that write the runtime configuration a claim freezes. **Slice 9b (agent host execution) is code-complete and committed** (2026-09-21): an Engine API client
over the Docker unix socket using only the standard library, the typed label authority ADR 0005's
cleanup-by-exact-label invariant depends on, `docker buildx build` with secrets travelling the BuildKit
session, container run by immutable digest with no published ports on one environment network, ADR 0005's
default health gate, and an executor in `cmd/agent` assembling all of it. No release activation or Traefik
routing is in scope, and a successful gate deliberately leaves the deployment at `health_checking`.

**It has never run.** Every test in 9b exercises a fake: a fake Engine API over a real unix socket, a fake
`docker` binary on PATH, an `httptest` server for the probe. No real dockerd, no real BuildKit, no real
checkout. `executor_available` stays false until two prerequisites are met, both tracked in
[tasks/todo.md](../tasks/todo.md):

1. The **privileged Linux checkout smoke** (XFS prjquota, delegated cgroup v2, root helpers), still pending
   and only the owner can run it.
2. A **configured health-gate path column**. ADR 0005 requires an exact configured path and Host header;
   migration 000014 froze port and scheme only. The gate currently probes `/`, and a failed gate with the
   container running is classified `permanent_configuration`, which is never retried — so an application
   that does not answer 2xx at `/` would fail permanently with no automatic recovery. This is a gap in the
   approved plan, found in review, not a coding slip.

Flipping `cmd/agent`'s `executorEnabled` constant is what starts real deployments; a test fails the build
if it is flipped without being updated deliberately.

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
| Frozen execution config | migration 000014/000015; agent_lease_execution.go | Environment runtime configuration is CAS-versioned desired state; each attempt freezes an insert-once snapshot plus its exact secret-version and literal set; attempt N>1 copies attempt N-1's frozen rows verbatim, so a rotation or config edit mid-job is never adopted; a missing config or an unusable binding fails that candidate alone. |
| Release model | migration 000014; ensureRelease | One release per deployment (UNIQUE(deployment_id)); image digest is nullable and write-once and gates candidate->ready; terminal releases are frozen; runtime-instance and operation rows validate fence/job against the referencing attempt at insert. active/retained states do not exist yet. |
| Secret materialization | agent_lease_materialize.go; internal/agent/http/materialize.go; internal/secret/setauthorizer.go | Authorization is re-derived from locked rows in Complete's order and returns references only; decryption happens after that transaction commits, so no KEK work holds a row lock. The frozen set is the authority — the agent names nothing. A superseded frozen version still materializes; a revoked one does not. Per-request SetAuthorizer admits exactly the authorized keys and nothing when empty. KEK material is loaded per request, never resident. |
| Phase progress | agent_lease_materialize.go (ReportProgress); internal/agent/http/progress.go | An agent may report only fetching/building/starting/health_checking; activating and healthy stay control-plane verdicts. Duplicate and stale phases are absorbed and append nothing, a skipped phase is refused, a terminal or cancel-requested deployment refuses all progress, and no agent_job_events row is written per report. |
| Runtime config admin | internal/web/web.go; environment_runtime_config_repository.go (migration 000016) | Session/CSRF, owner-scoped, audited pages create and edit an environment's configuration and bindings; revision CAS refuses a concurrent edit; schema and trigger rejections surface as validation, never a 500; a secret binding shows head id and version only. The write path locks only the administrator session rows and the config row, which is what Step 2's deadlock-freedom argument requires, enforced by a source-reading test plus a real-claim concurrency test. |
| Checkout credential | internal/agent/http/checkout.go; internal/agent/checkout; gitcheckout.Lease | begin reserves and returns no token; start activates, marks the durable lease running, then delivers the credential once via CheckoutLease.DeliverToken; close releases and revokes. Reservations are bound to the server/attempt/fence that created them, start is a one-shot claim, and the App private key never leaves the control plane. |

Webhook ingress accepts only exact POST /webhooks/github, canonical single headers, trusted HTTPS provenance, and a bounded body. HMAC-SHA256 is constant-time checked against an active plus bounded retained secret ring before JSON parsing/database work. Malformed terminal receipts are sanitized; retryable persistence failures stay retryable. Push eligibility requires active App, installation, repository, binding, target fields, exact full ref, and nonzero lowercase 40-hex SHA. Delivery ID and verified body digest have independent replay defense; automatic intent is unique by active binding/repository/ref/commit.

## Schema, configuration, and entrypoints

Migrations are forward-only and embedded by migrations/migrations.go:

- 000001 deployments; 000002–000003 administrator/auth recovery; 000004 encrypted secrets; 000005–000008 GitHub setup, owner integrity, checkout authorization/reservations; 000009 binding targets; 000010 webhook deliveries, automatic intents, queued agent jobs, and enqueue events.
- 000011 corrects 000008's checkout-reservation clone URL CHECK: its double-escaped regex (`\\.` in a standard string) rejected every canonical clone URL, so no reservation could be inserted.
- 000012 adds server identity: `servers` (immutable id/owner/created_at, status active|revoked, `last_heartbeat_at`), `server_credentials` (version, 32-byte digest, one-active partial unique index, only active->revoked update), `server_enrollment_tokens` (digest, issued_at, expires_at, consumed_at, single use), and append-only `server_audit_events`.
- 000013 adds the lease schema: replaces the 000010 `agent_jobs_no_update` trigger with a transition-validating trigger and widens the job state CHECK to queued|leased|retry_wait|succeeded|failed|cancelled|dead, plus `attempt_count`/`cancel_requested_at`; adds `environment_slots` (never-decreasing fence, current attempt, `reconciliation_required`), `agent_job_attempts` (job/attempt_number/server/environment/fence/state/renewal_sequence/lease+authority deadlines/result digest, legal-transition-only), and `frozen_secret_versions` (append-only, empty until the execution slice); widens `agent_job_events`; adds the claimable and authority-expiry partial indexes from ADR 0004.
- 000014 adds the host-execution schema: `environment_runtime_configs` (CAS-versioned desired state, retained, never deleted), `environment_runtime_bindings` (variable -> secret head or literal, with a bind-time trigger requiring the head to share the environment, agree on delivery scope, and not be `provider`-purpose), a widened `frozen_secret_versions` (variable name and delivery scope), insert-once `agent_job_attempt_execution` (including frozen `drain_seconds`), and the minimal release model (`releases`, `release_runtime_instances`, `release_operations`).
- 000016 adds append-only `environment_runtime_config_audit_events`, because `server_audit_events`' action enum has no member for a configuration change and its `server_id` references a server such a mutation does not have.
- 000015 gives `literal_nonsecret` bindings the `literal_value` column they had been missing — before it, such a binding was a silent no-op — plus append-only `frozen_literal_values`. Literal values are bounded, reject control characters, and are frozen and copied forward exactly like secret references.

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

Slice 9b evidence (2026-09-21; four atomic commits, each independently reviewed with findings fixed before
the next, and each verified with the full bar before commit — `make check`, `go vet` including
`-tags=integration`, `go test -race ./...`, three fresh PostgreSQL 18 integration sweeps normal and three
`-race`, cross-build and vet for darwin/arm64 and linux/amd64/arm64, `git diff --check`, go.mod unchanged
at four direct dependencies):

- defects found by review that the delivered, green, `-race`-clean work could not see: a request timeout
  cancelled on return, killing any response body that trails its headers — the shape of every streaming
  endpoint the build and run paths need; dial failures all classified retryable, so a misconfigured socket
  path would have been retried forever; `buildx --file` accepting an absolute or `../` path, letting a
  build open any file on the host and echo its contents into output redacted only for known secrets; a
  secret binding named `DOCKER_HOST` able to repoint the CLI at another daemon; the agent's whole
  environment — database password included — handed to the build subprocess; a built-image selector
  demanding labels no image can carry, which would have matched nothing forever while reporting success;
  a health-gate classifier that permanently failed deployments for an agent restart or a host OOM kill,
  discarding evidence Docker returns on the inspect call it already makes; and the same deadline
  classified two different ways depending on which internal timer noticed first.
- **three delivered tests passed against deliberately broken code** and were rewritten: one restating the
  invariant instead of exercising it, one pointed at a nonexistent path so a missing file and a real
  rejection were indistinguishable, and one whose secret never reached the defense-in-depth check it
  claimed to prove. A fourth was caught in the act: an exact-repeat progress test passes even with the
  absorbing branch deleted, because `deployment.Transition` absorbs an exact repeat by itself.
- every guard test above was broken on purpose and confirmed to fail before being accepted.

Slice 9a evidence (2026-09-18..2026-09-21; each step verified before the next, most independently reviewed):

- every step: `make check`, `go vet` including `-tags=integration`, `go test -race ./...`, Linux amd64/arm64 builds, `git diff --check`;
- three fresh PostgreSQL 18 integration sweeps normal **and** three fresh `-race` per step, against a database recreated between runs; concurrency-sensitive tests additionally `-race -count=20`;
- independent zero-context review of the schema step (three medium findings fixed: frozen `drain_seconds`, insert-time fence/job validation, binding scope trigger), of the claim-freeze step (one high deferred into 000015, one medium fixed), and of materialization + progress (PASS with notes: a redundant same-row lock, and an authorize-then-decrypt window shown closed by the service's own re-read);
- defects found and fixed before landing, recorded per step in [tasks/todo.md](../tasks/todo.md): an `image_digest NOT NULL` the next step could not have used; an outcome column frozen before the outcome existed; a transition trigger that rejected the digest-setting update; `literal_nonsecret` bindings silently doing nothing; a typed-nil hazard introduced by converting a parameter to an interface; progress advancing a deployment already being cancelled; and, in delegated work that passed its own tests and `-race`, a mutable registry entry handed out of a lock plus a double-mint on concurrent start.

One unexplained single failure is on record and was never reproduced: during Step 2b's first sweep, one run reported FAIL for `internal/store/postgres` while the other packages passed, and the failing test name was lost because that command captured only its last three lines. It did not recur in 22 subsequent fresh runs, normal and `-race`. Root cause unknown; capture full output to files rather than tailing, and treat a recurrence as a real signal.

Checkout implementation is complete, but the top-level isolated-checkout milestone remains unchecked until an operator runs the non-cached privileged smoke on provisioned Linux: installed root helpers, XFS mounted with prjquota, delegated cgroup v2, and distinct checkout UID/GID must prove typed GitInit, quota/workspace cleanup, and no cgroup leaf remains. Linux cross-compilation/tagged-test compilation do not prove that boundary; macOS cannot substitute.

No Docker/BuildKit or Traefik executor exists yet; the host agent client never claims work because no executor is registered in this slice (heartbeat reports `executor_available=false`). There is no release activation and no checkout invocation from the agent. Do not present the current queue as runnable by an agent.

## Next slice: 9b, agent host execution

Slice 9a gave a claimed lease everything it needs: frozen configuration and secret references on the claim
response, a fenced endpoint that turns those references into values, a phase-progress endpoint, and a
checkout credential lifecycle the agent can drive without the App private key. What remains is the host
side, under ADR 0001/0003/0005, wiring an Executor into `cmd/agent` so a claimed lease does real work for
the first time:

1. Drive the checkout through `internal/agent/checkout`'s lease (already written and tested against the
   `gitcheckout.Lease` interface, but not yet constructed by the real agent binary).
2. Build with `docker buildx build --secret id=…,env=…` so values travel the BuildKit session and never
   reach argv, an `ARG`, a build-context file, or an image layer. Missing BuildKit fails closed. Output
   streams through `crypto.RedactingWriter` before leaving the agent.
3. Create and start the container by image digest over the Docker Engine API (stdlib `net/http` over the
   unix socket — the owner rejected the moby/buildkit Go SDKs to keep go.mod's four direct dependencies):
   no published host ports, the stable environment network only, resource limits, restart policy, and the
   immutable release/runtime-instance/attempt/fence labels ADR 0005 specifies.
4. Health-gate with ADR 0005's default shape hard-coded: exact host/path/port, 2xx, redirects off, three
   consecutive successes, 2s interval and timeout, 60s overall deadline, container must stay running,
   bounded body discarded.

A retry must find prior-attempt resources by exact label match before creating anything. `FailureClass`
maps Docker/network blips to `transient`, a bad Dockerfile or a failed gate with the container still
running to `permanent_configuration`, and inconsistent local label/fence state to `corrupt_state`; no new
enum values. `internal/runtime/docker` needs `//go:build linux` files with a `!linux` stub so macOS
development keeps building, mirroring `checkoutcontain`.

Release activation and Traefik routing remain out of scope; they follow in the blue/green/rollback slice
per ADR 0005. A successful 9b attempt therefore leaves the deployment resting at `health_checking`, since
`activating -> healthy` belongs to that later slice.

## Known follow-ups

From the server/leases slice:

- Agent-initiated credential rotation protocol: rotation currently requires the agent to already hold its raw credential; there is no agent-initiated rotation path yet (Step 6b).
- Server-side claim idempotency key so a lost claim response cannot orphan a lease until expiry (Step 6c).
- Register resubmission can create an orphan unenrolled server; needs dedupe or cleanup (Step 6b).
- Persist the reconcile outcome digest once a consumer exists; needs a migration (Step 4).
- A revoked server cannot reconcile its own environments; those need administrator `mark-reconciled` (Step 4).
- No rate limiting on `/agent/v1/` (low severity; a valid credential is still required) (Step 5).
From slice 9a:

- `TRUNCATE` bypasses `BEFORE DELETE` triggers, so every append-only/retained table in 000012–000015 is only as
  append-only as the application role's grants. No migration withholds `TRUNCATE` from that role; one should.
- One `Claim` transaction can hold up to `claimDiscoveryMax` (200) `environment_runtime_configs` share locks until it
  commits, which lengthens the worst-case wait an administrator config write can see under a large backlog. Worth a
  representative-scale check per ADR 0004.
- Step 2's deadlock-freedom argument requires the administrator config write to acquire only authorization rows and the
  config row, never the slot/job/attempt/server rows. That is enforced by review and a concurrency test, not by the
  type system: re-read it before changing the admin write path.
- `ReportProgress` locks the `deployments` row and then `transitionDeployment` locks it again in the same transaction.
  Safe (row locks are reentrant) but a redundant round trip.
- `releases.id` reuses `deployment_id`. Safe while the 1:1 invariant holds; more than one release per deployment would
  need a generated id and a schema change.
- An activated checkout lease is process-local, so a control-plane restart between `checkout/start` and
  `checkout/close` loses the registry entry and revocation degrades to token expiry.
- `environment_id` has no owner column anywhere, so the admin pages derive ownership from `agent_jobs.owner_id` (as `MarkReconciled` already does). An environment therefore cannot be configured before its first job is queued, and two owners using the same `environment_id` string would share one configuration row. Not exploitable with one administrator, but it wants an environments table or an owner column before multi-tenancy.
- `HTTPProvider.RevokeToken` calls `signer.Token()` through `doJSON` although GitHub's revoke call authenticates with
  the installation token, so revoke needlessly requires the App private key. Harmless today because revoke stays
  control-plane-side; it would surprise anyone moving it.

- ADR 0004 amendments below (renewal events, attempt-count/expiry, claim slot pre-creation and lock order, reconciliation ownership, revocation-only attempt closure, sweeper locking) reflect what shipped; re-read them before extending the lease protocol.

## Resume order and commands

1. Read this file, [tasks/todo.md](../tasks/todo.md) (its slice 9a step-result blocks record every correction and why), [MEMORY.md](../MEMORY.md), then ADR 0001, ADR 0003, ADR 0004 (including its 2026-09-17 amendments), and ADR 0005. For webhook changes also read ADR 0002 and migration checks.
2. Before relying on checkout from agent execution, run/preflight the pending privileged Linux checkout smoke with Makefile's explicit test-checkout-integration contract on provisioned XFS prjquota plus delegated cgroup v2; keep the top-level checkout milestone unchecked until it passes.
3. Inspect git status --short, touched migrations, and focused tests before editing. Do not reset, clean, overwrite shared uncommitted files, or use blanket git add because .tmp/ is not ignored.
4. Create/approve a detailed plan in tasks/todo.md for a nontrivial slice, including transaction ownership, lock order, negative tests, migration, and rollback behavior.
5. Run make check, go test -race ./..., and a fresh PostgreSQL 18 integration run using CI's AUTODEPLOY_TEST_DATABASE_URL contract (`-p 1` across internal/store/postgres, internal/agent/http, internal/web, and migrations). Run Linux cross-builds where tagged code changes; finish with git diff --check and independent review/QA.

Commits are atomic and descriptive (feat:, fix:, refactor:, chore:), manually exclude .tmp/ (it is untracked, not ignored), credentials, and incidental worktree changes, and never include generated attribution.

## Definition of done for slice 9b

A claimed lease drives a real checkout, build, and container start on the target host, gated by the ADR 0005 default health check, with no release activation or traffic change; every host operation is idempotent, fenced, and labeled so recovery/reconciliation can identify and clean up partial work; secrets reach the build only through BuildKit mounts and runtime environment injection inside the trusted-host boundary, never logs or job payloads; failures leave the currently healthy release untouched; recovery is proven with live PostgreSQL concurrency tests plus host-side idempotency tests; docs updated to 9/18, and the top-level isolated-checkout milestone ticked only after the privileged Linux smoke passes.
