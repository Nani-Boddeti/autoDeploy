# Step 15 — operations UI: plan (owner decisions recorded 2026-11-11)

Goal: a Coolify-like UI to manage the owner's apps on one Hetzner server. Gates `executorEnabled` and
`activationEnabled` stay false: no deployment exists on a real host, so every page must say "no data / nothing has
executed" honestly. Rollback and retention are slice 11 (not planned here).

## 1. Current state (verified)
- UI = `internal/web/web.go` (1832 lines, single file) + tests in `internal/web/*_test.go`. Hand-rolled `ServeHTTP`
  switch on exact path; each feature has a narrow interface in `Config` (`ServerAdmin`, `RuntimeConfigAdmin`,
  `RouteHeadAdmin`), nil = route 404s. Pattern per page: `h.session` -> redirect `/login` if none; CSRF via
  `auth.NewSessionCSRF`; owner scoping inside the repository (principal passed in); `html/template` pages.
  Query strings are rejected except on `/github/setup/callback`, `/environments/config`, `/environments/route`.
- CSP is `default-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'` (web.go `securityHeaders`):
  NO inline/external CSS, NO scripts, NO connect. Existing pages are unstyled plain HTML; todo.md (6b, 3d) defers a
  design system to "the operations UI milestone" => no established style, so the owner's pastel palette applies.
  Styling and any live update need CSP changes (see D1, D2). `Cache-Control: no-store` on every response.
- No `projects` or `environments` tables. `project_id`/`environment_id` are opaque TEXT keys (migrations 000001,
  000005 `github_project_bindings`, 000013 `environment_slots`, 000014 comment line 13). A "projects view" is a
  derived listing: `github_project_bindings` (owner-scoped) joined to `agent_jobs`/`environment_slots`/`route_heads`
  (each has owner or environment key; `deployments` has NO owner_id — owner scope must come via `agent_jobs.owner_id`).
- Store has per-id reads only for deployments (`DeploymentRepository.GetByID`, no owner scoping, no list). No list
  query for jobs/attempts/releases/events exists. Tables to read: `deployments`+`deployment_events` (status
  history), `agent_jobs` (state, cancel_requested_at, owner/project/env/server), `agent_job_events`,
  `agent_job_attempts` (state, failure_class, fence), `agent_job_attempt_execution` (phase), `releases`
  (state, strategy, commit, image digest), `release_runtime_instances`, `release_operations`, `route_heads`
  (active_release_id), `route_events`.
- LOGS ARE NOT PERSISTED OR TRANSPORTED. Agent->server carries only `ReportProgress(phase)`
  (`internal/agent/client/api.go:524`). Build output goes to `Request.Stdout/Stderr` -> redacting writer
  (`internal/runtime/docker/build.go:480`) -> `executor.Config.BuildOutput`, which defaults to `io.Discard`
  (`executor.go:126`); nothing sets it. Runtime container logs are not read at all. No log table, no log endpoint.
  => live logs need a NEW agent->server log channel (own migration, endpoint, agent shipper, redaction, bounds,
  retention). Larger than the rest of step 15 combined.
- Cancel: server-side op EXISTS: `AgentLeaseRepository.RequestCancellation(ctx, principal, jobID)`
  (agent_lease_repository.go:2765; owner-scoped, session-locked, slot-then-job lock order, immediate for queued,
  cooperative for leased, rejected after activating linearization point, idempotent). Used only by tests; not wired
  to web or cmd/controlplane (main.go builds `leaseRepository`).
- Retry: NO manual retry. Automatic retry exists (`retry_wait`, `internal/queue/retry.go`, MaxAttempts). A manual
  "retry" of a failed/cancelled job = new deployment+job for the same commit: needs new enqueue path (idempotency key
  `(owner_id, idempotency_key)`, `UNIQUE(deployment_id, job_kind)`, jobs immutable), reusing the webhook enqueue
  transaction. New server work, high risk.
- Rollback: not built (slice 11). Retained releases exist only as `releases.state` + route head; no server op.
- Dependency note: `executorEnabled=false` => the agent heartbeats `executor_available=false`; pages derive
  "executor off" from server heartbeat data, but the control plane has no direct read of the agent gate.

## 2. Sub-steps (one subagent each, review between, atomic commits; `impeccable` skill invoked before 15a UI code)
- **15a Design foundation + CSP decision** (medium): D1 outcome. Shared layout template, pastel token palette,
  nav (Servers, Projects, Deployments, GitHub), applied to existing pages without behaviour change. Files:
  `internal/web/web.go` (securityHeaders; may split templates into `internal/web/templates/` via `embed`),
  `web_test.go` (CSP assertion updated). Risk: medium (security header change; no SQL).
- **15b Read model: projects + deployments queries** (high-ish: new SQL, read-only, no locks): new
  `internal/store/postgres/operations_read.go` (+ tests): `ListProjects(principal)`, `ListDeployments(principal,
  filter, keyset page)`, `GetDeploymentDetail(principal, id)` returning deployment status + events, job state +
  events, attempts (state/failure_class/phase), release + runtime instance state, route head active release.
  Plain `READ COMMITTED` SELECTs, no `FOR UPDATE`, owner via `agent_jobs.owner_id`, explicit bounded LIMITs, index
  check with EXPLAIN on a seeded set. Risk: high class by rule (new SQL) but no locks/transitions.
- **15c Pages: `/projects`, `/deployments`, `/deployments/detail?id=`** (low): new `OperationsRead` interface in
  `Config` (nil = 404), handlers + templates, whitelisted query params (extend the query-string allowlist narrowly),
  honest banners ("no executor registered / activation disabled: nothing has run" when no heartbeat executor).
  Files: `internal/web/operations.go`, `operations_test.go`, `web.go` (route cases), `cmd/controlplane/main.go`
  (wiring). Risk: low (read-only UI).
- **15d Cancel action** (high): wire existing `RequestCancellation` as `POST /deployments/cancel` (session +
  CSRF + origin like `/servers/revoke`, confirm field, job id from server-side lookup not client-trusted beyond
  owner scoping). Map `ErrServerAuthorizationDenied` to 404-style generic response. No new SQL. Files: `web.go`/
  `operations.go`, `operations_test.go`, integration test in `internal/web` against fresh PG18, main.go wiring.
  Risk: medium-high (reuses a locked state transition; web-level tests must prove CSRF/owner/idempotent/after-
  activating refusal surfaces correctly).
- **15e Live status via refresh** (low): meta-refresh (or link-based refresh) on detail page while job non-terminal;
  no JS. Rolls into 15c if D2 = meta refresh. Files: template only.
- **15f Retry action** (high, only if D3 = in scope): new store op `RetryDeployment(principal, deploymentID)`:
  allowed only for terminal failed/cancelled (not `activating`/unresolved-activation states), creates new
  deployment + job via the existing enqueue transaction with a derived idempotency key, honours env slot / route
  head / newest-commit-wins rules (supersede interaction!). Needs a design note + ADR 0004 amendment, full
  PG18 concurrency tests (lock order slot -> job -> ...; concurrent retry+webhook; double-click).
- **15g Log channel** (highest, only if D1-log = in scope): migration `deployment_log_chunks` (append-only, bounded
  per attempt, retention-purgable), agent shipper (batch, sequence numbers, drop-on-overflow marker), agent-
  authenticated `POST /agent/v1/leases/logs` (fenced to the attempt like progress), server redaction pass,
  `/deployments/logs?id=&after=` page. Separate plan + ADR amendment before any code. Phases: 15g-1 schema+store,
  15g-2 endpoint, 15g-3 agent shipper wiring `BuildOutput`, 15g-4 runtime container log capture, 15g-5 UI.
- **15h Rollback exposure**: no code now. After slice 11, add `POST /deployments/rollback` over a slice-11 store op.
  Until then the detail page shows retained releases read-only with no button (see D4).
- **15i Docs**: IMPLEMENTATION_STATUS, todo.md (tick UI item only when pages + actions + logs done; partial otherwise),
  ADR 0006/web notes.

## Owner decisions (2026-11-11)
- D1 = (a) same-origin static CSS, CSP gains only `style-src 'self'`; pastel palette.
- D1-log = (b) BUILD the log channel now as 15g (own plan + ADR amendment before any 15g code).
- D3 = cancel only (15d); retry (15f) deferred until after the real-host run.
- Orchestrator defaults (not owner-asked): D2 meta-refresh, no JS; D4 rollback button deferred (read-only retained
  releases); D5 derived projects view, no new tables; D6 UI before the real-host run (owner has no host yet).
- Order: 15a -> 15b -> 15c(+15e) -> 15d -> 15g plan -> 15g-1..5 -> 15i. 15f, 15h not in this step.

## 3. Open decisions for the owner (as drafted)
- D1 Styling/CSP: (a) keep CSP; serve CSS as a same-origin static route and relax to `style-src 'self'` only
  [recommended: smallest widening, no inline, no scripts]; (b) keep unstyled; (c) inline styles with a nonce.
  Pastel palette applies (no established style).
- D2 Live view: (a) polling via `<meta http-equiv=refresh>` on non-terminal pages, no CSP change [recommended for
  status]; (b) SSE/fetch needs `script-src 'self'` + `connect-src 'self'` (bigger surface); (c) none. Real live logs
  need D1-log first; start with refresh-based tail of persisted chunks.
- D1-log Live logs now? (a) Defer all log work until a real-host run proves the pipeline; ship status + phase
  timeline only [recommended: nothing has run, can't validate redaction/volume]; (b) build the channel now as 15g.
- D3 Retry/cancel scope: cancel now (server op exists) [recommended]; retry deferred until after the real-host run
  (new locked server op, little value while nothing deploys) — or include 15f.
- D4 Rollback button: defer (recommended), or render a disabled button with "available after slice 11".
- D5 Projects view source: derived from `github_project_bindings` + jobs (recommended; no schema change) vs. creating
  real `projects`/`environments` tables (migration + backfill, much larger; reject for step 15).
- D6 Sequencing vs real-host run: UI over an empty database is hard to verify visually; recommend doing 15a-15c
  with seeded integration fixtures, but the owner may prefer the real-host run first.

## 4. Guards / invariants
- Every new route: session required (redirect `/login`); owner scoping inside the store with the principal, never
  via a client-supplied owner/id alone; unknown or other-owner id -> same generic 404.
- Every POST: CSRF token + origin check identical to `/servers/revoke`; method allowlist; no GET side effects.
- No secret display: pages select only whitelisted columns; never digests/credentials/frozen values; image digest
  shown truncated hex only if wanted; failure text only the sanitized `failure_class`/phase enums.
- Read queries take no `FOR UPDATE`/advisory locks; bounded LIMIT + keyset paging; cancel keeps the documented
  lock order (session -> slot -> job -> attempt).
- All dynamic output through `html/template` (auto-escape); `Cache-Control: no-store` retained; query params
  whitelisted per route (existing global query-string rejection extended narrowly).
- Honesty: banners driven by data (no executor heartbeat, `activationEnabled=false`); never imply a release is live
  without `route_heads.active_release_id` evidence.
- Gates untouched; no rollback/retention logic; no Docker access from the control plane (ADR 0001/0005).

## 5. Verification by risk
- 15a, 15c, 15e (read-only/presentation): gofmt, vet, `make check`, `go test -race ./internal/web/...`, template
  render + escaping tests, auth/CSRF/404 tests per route.
- 15b (new read SQL): above + `internal/store/postgres` integration on fresh PG18 (owner isolation with two owners,
  keyset paging, no-lock proof via `pg_locks` or a concurrent writer not blocked, EXPLAIN uses indexes).
- 15d/15f (state transitions/locks): full bar: `go test -race ./...`, PG18 integration with real contention
  (concurrent cancel vs claim/renew/activate; double-submit idempotency; cancel after activating refused), CSRF/owner
  tests, independent review (FAIL->fix->re-review loop), vacuous-test audit per delegated-work-review notes.
- 15g (new channel): full bar plus redaction fuzz, overflow/backpressure, fence-mismatch rejection, retention.
- Independent code-reviewer after each step; atomic commits; verify `git status`/hash after each.

## 6. Risks
- Biggest: the log channel (D1-log) is an unscoped greenfield subsystem touching agent, protocol, schema,
  redaction and retention; unvalidated without a real-host run.
- CSP widening for CSS/live views could weaken the strict policy; keep `default-src 'none'`, no inline/script.
- `deployments` lacks owner_id; a missed join on `agent_jobs.owner_id` would leak cross-owner data (test with two
  owners). Only one owner exists today, so the bug would be invisible in practice.
- No project/environment tables: derived project view may be confusing/incomplete for projects with no jobs yet.
- Retry interacts with idempotency, supersede, env slot and stranded-activation resolution; easy to violate
  invariants (hence deferral recommendation).
- `web.go` is already 1832 lines; add new files rather than growing it.
- UI built on an empty DB can look fine while untested against real data volumes/long-running states.
