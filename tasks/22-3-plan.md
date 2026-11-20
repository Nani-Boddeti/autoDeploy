# 22-3 UI overhaul plan (rev 2, 2026-11-20, awaiting owner approval)

Written so each slice can be handed to a Sonnet subagent as a self-contained brief. The orchestrator (Opus) approves, reviews and commits. Read "Facts" before any slice.

## Goal and owner rules
- A productive operator UI with fewer nested clicks and the same information as today.
- Owner rules (2026-11-20):
  - R1: no hidden or nested pages. Every page type is listed in the sidebar nav.
  - R2: config and route pages are first-class (in the nav, no typed IDs).
  - R3: no page renders text alone. Every page and empty state carries data, a form, or the setup checklist.
  - R4: keep information parity. No fact, form or link the current UI shows is lost.
- Original 22-3 scope (tasks/22-plan.md): (a) failure reason on the deployment page; (b) dashboard with inline actions; (c) live logs.
- Design context: `.impeccable.md` (calm, precise, quiet; pastel; light with auto dark).

## Owner decisions
- D1 personality: calm, precise, quiet.
- D2 theme: light, plus automatic dark via `prefers-color-scheme`.
- D3 live logs: one same-origin `/static/logs.js` plus SSE, falling back to the meta refresh without JS.
- D4 environment pages: "Runtime config" and "Routes" are nav items with an environment strip, defaulting to the first environment.
- D5 (YES, owner 2026-11-20): ship the health gate `last_probe` to the deployment logs, so the UI can show "container_unhealthy · last probe: status=200; tcp ok".
  - Today it goes only to the agent journal (`Diagf`: executor.go:418, rollback.go:86).
  - Change: add one `logs.Linef(logship.StreamHealth, "health gate: last probe: %s", ...)`. Linef redacts.
  - Agent-side change, so acer needs a reinstall. Recommend yes.
- D6 (owner 2026-11-20): system fonts only. No embedded fonts and no `font-src` CSP change.

## Facts (verified 2026-11-20; do not re-derive)
### Web layer (internal/web)
- `Handler{repository, config}`, built by `New(repo, Config)` (web.go ~154).
  - Dependencies are optional `Config` fields: `ServerAdmin`, `RuntimeConfigAdmin`, `RouteHeadAdmin`, `OperationsReader`, `DeploymentCanceller`, `DeploymentLogReader`, `ManualDeploy`, `Rollback`, `CommitResolver`, `RepositoryLister`, `GitHubSetup`.
  - A nil dependency makes its route return 404.
  - `manualDeployEnabled()` is defined at manual_deploy.go:35.
  - Optional interfaces are discovered by type assertion on `Config.OperationsReader` (pattern: `EnvironmentReleaseReader`, environment_releases.go:19).
- Router: one `switch r.URL.Path` in `ServeHTTP` (web.go:180-438).
  - Query-string allowlist at web.go:188. A new GET route that takes query params must be added there.
  - Pattern: nil-dependency check, then method check (`methodNotAllowed`), then handler.
- Templates:
  - Pages are loaded with `parsePage(name)` (templates.go:19) from layout.html plus templates/NAME.html, embedded via `//go:embed templates/*.html`. There is no FuncMap.
  - Page skeleton: `{{template "head" "Title"}}{{template "nav" "key"}}<main>...</main>{{template "foot"}}`.
  - Refresh variants: `headrefresh` (5s) and `headrefreshfast` (3s).
  - `execnotice` is a defined block.
- Rendering:
  - `renderOperations(w, tmpl, view)` (operations.go:167) buffers, then writes. On error it returns a 500 plain response. Use it for all new pages.
  - The older pages call `tmpl.Execute(w, v)` directly. Migrate them to `renderOperations` when touched.
- Security:
  - `securityHeaders` (web.go:1951) sets the CSP `default-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; style-src 'self'`.
  - The CSP is pinned by `wantCSP` (layout_test.go:13).
  - Static: only `/static/app.css` (embedded `appCSS`) is served today.
- Session and forms:
  - `h.operationsPrelude(w, r) (auth.Principal, bool)` (operations.go:139) handles GET pages.
  - CSRF: `auth.NewSessionCSRF(keys, p.SessionID, origin)` puts the token in the view's `CSRF` field.
  - POST handlers check `auth.ValidateSessionCSRF` plus `h.provenance(r)`.
  - Nonces come from `newManualNonce()` (manual_deploy.go:42).
  - `offerRetry` (operations.go ~455) and `offerRollback` (rollback.go) build the retry and rollback offers.
  - `redirect(w, url)` sends a 303.
- Refresh:
  - `terminalJobStates` = succeeded, failed, cancelled, dead (operations.go ~50).
  - Detail refreshes every 5s while the job is non-terminal.
  - Logs refresh every 3s, only on the last page and while non-terminal.
- Logs handler (logs.go:88):
  - Params: `id` and `after`. `parseChunkCursor` accepts digits only.
  - Calls `ListDeploymentLogs(ctx, p, id, after, 0)`, where 0 means 200 rows.
  - Groups consecutive lines by `AttemptNumber`. `logStreamClass` maps against the allowlist build, phase, health, activation, drain, system.
  - `Marker` is set for the truncation and dropped-line markers.
  - `StartHref` appears when `after > 0`, because the keyset can miss late commits.
- http.Server (cmd/controlplane/main.go:51-54, 535): ReadHeader 5s, Read 15s, **Write 20s**, Idle 60s.
  - SSE must call `http.NewResponseController(w).SetWriteDeadline(...)` per event. Never change the global timeout.
- Test kit:
  - Builders: `testHandler(t, *repositoryStub)` (web_test.go:84; origin `https://admin.example.test`, fixed `Now` 2026-08-10).
  - `serversHandler`, `operationsHandler` and `logsHandler` return `(h, stub, principal)`.
  - Requests: `serversRequest(method, target, form, p, h)` adds the session cookie, plus Origin for forms. `opsGet(h, p, method, target)`.
  - Fixtures: `sampleDetail(jobState, active)` and `line(id, attempt, stream, body)`.
  - Stubs: `operationsStub`, `logStub`, `serverAdminStub`, `runtimeConfigAdminStub`, `routeHeadAdminStub`, `releaseReaderStub`, `rollbackStoreStub`, `manualStoreStub`, `resolverStub`, `cancellerStub`.
  - `TestEveryPageUsesLayoutAndStaysCSPClean` (layout_test.go:~83) GETs a fixed page list and bans `style=`, `<style`, `<script`, `javascript:` and `onclick=`. It requires `<nav aria-label="Primary">`.
  - There are about 100 tests using `strings.Contains` on `name=`, `action=`, `href=` and sentences.

### Store and schema
- **There is no environments table and no projects table.** IDs are free TEXT.
  - Owner-to-environment linkage comes only from `agent_jobs(owner_id, environment_id)`, through `environmentOwnedBy` (environment_runtime_config_repository.go:227), and from `github_project_bindings(owner_id, project_id, environment_id, server_id, deploy_ref, active)`.
  - `route_heads`, `releases`, `environment_runtime_configs` and `environment_slots` have **no owner column**. The `environment_id` namespace is global, so any join to them MUST be guarded by `EXISTS (SELECT 1 FROM agent_jobs WHERE environment_id=X AND owner_id=$owner)`.
  - Without that guard, one owner could see another owner's route or config for a colliding environment ID. This is a hard security invariant.
- An environment that is bound but has no job is not owned under `environmentOwnedBy`, so its config and route pages deny it today.
  - It is shown from binding data only, and its next step is "Deploy".
  - This is an inherited limitation. Do not "fix" it in 22-3.
- Read methods and scoping:
  - `ListProjects` (operations_read.go:263) has `ProjectSummary{..., BindingEnvironmentID, BindingServerID, EnvironmentIDs, LatestDeployment}`.
  - `ListDeployments` (:337): `DeploymentFilter{ProjectID, EnvironmentID}`, `DeploymentPage{Limit<=100 (default 25), After *DeploymentCursor}`, scoped by `j.owner_id=$1`.
  - `GetDeploymentDetail` (:401) has `AttemptView{ID, AttemptNumber, State, FailureClass, Fence, ServerID, ReleaseStrategy, CreatedAt, ClosedAt}`.
  - `ListEnvironmentReleases` (environment_releases_read.go:33, max 10).
  - `ListServers` (server_repository.go:623).
  - `ListReconcilingEnvironments` (agent_lease_repository.go:3589).
  - `GetRouteHead` and `GetEnvironmentRuntimeConfig` (both use `environmentOwnedBy`).
  - `ListDeploymentLogs` (deployment_logs.go:292).
  - OperationsReadRepository reads use `beginRead`, which runs `requireLiveSession` (operations_read.go ~212) and then filters on `owner_id`.
- Logs:
  - `deployment_log_chunks(id, attempt_id, stream, writer_epoch, seq, body<=4096, created_at)` has index `(attempt_id, id)`. There is no deployment_id column; reach it through `agent_job_attempts.job_id` to `agent_jobs.deployment_id`.
  - Rows are already redacted at ingest (agent `Linef` plus server `sanitizeLogBody`).
  - Retention is 14 days, keeping the newest 20 attempts per environment. Old deployments can lose their reason line; fall back to the class.
- Failure reason sources:
  - Health stream lines (exact formats; the class may be absent):
    - executor.go:417 `health gate: failed (reason=%s class=%s)`
    - sts.go:310 `health gate: failed (reason=%s)`
    - rollback.go:85 `health gate: failed (reason=%s class=%s)`
  - Reason enum (health.go ~200): deadline_exceeded, context_cancelled, container_not_running, container_unhealthy.
  - `release_operations.outcome_summary` is free text, re-sanitized and at most 512 bytes. It carries `reason=... last_probe=...` for stop-then-start activation failures only.
  - The activation stream also has `activation: refused by the control plane (%s)` and `release: candidate start failed (%s)`.
- `failure_class` enum (queue/retry.go:50-55): transient, authorization, invalid_input, cancelled, corrupt_state, permanent_configuration.
- Integration fixtures (internal/store/postgres, tag `integration`):
  - `newOpsFixture(t, seed)` (operations_read_integration_test.go:29) provides owner A and B, `principal` and `principalB`, and `repo`.
  - `seedOpsDeployment(t, pool, owner, project, env, depID, createdAt)` (:58).
  - `newLeaseFixture` (agent_lease_schema_test.go:30).
  - `f.leaseJob` and `f.advanceToAttempt`.
  - `insertActiveRelease` (route_head_repository_test.go:43) and `f.insertRouteHead`.
  - `newLogFixture` (deployment_logs_integration_test.go:28) with `f.append(...)` and `lines(...)`.
  - Standard reset: `resetWebhookJobs`.

## Information architecture
Sidebar groups. The nav key is the `{{template "nav" "key"}}` argument.

| Group | Label | Key | URL | Status |
|---|---|---|---|---|
| Operate | Overview | home | `/` | rework (replaces placeholder) |
| Operate | Environments | environments | `/environments` | NEW |
| Operate | Deployments | deployments | `/deployments` | keep + filter selects + reason column |
| Operate | Logs | logs | `/deployments/logs` | works without `id` + SSE |
| Configure | Runtime config | config | `/environments/config` | works without `environment_id` + strip |
| Configure | Routes | routes | `/environments/route` | works without `environment_id` + strip |
| Configure | Projects | projects | `/projects` | keep + binding target + reason |
| Configure | GitHub | github | `/github` | NEW (moved from landing) |
| Infrastructure | Servers | servers | `/servers` | keep, minus reconciling table |
| Infrastructure | Reconciliation | reconciliation | `/reconciliation` | NEW (moved from servers), count badge |

- Footer: Sign out (POST `/logout`, unchanged).
- Detail pages: `/deployments/detail?id=` uses nav key `deployments` and shows a breadcrumb.
- Flow-only pages keep the shell and nav: the enrollment token page (POST result), github complete, and github continue.
- A nav item whose dependency is nil is omitted. Each view gets a `Nav navView` built by one helper, so the layout knows which items exist.

## Visual system (applies to all slices)
- One CSS file: `static/app.css`, a rewrite. All tokens live on `:root`, with dark mode under `@media (prefers-color-scheme: dark)`.
  - Neutrals are tinted toward hue about 270. One accent (lavender-blue).
  - State tokens: ok (green), busy (blue), failed (rose), attention (amber).
  - Pastel fills paired with darker same-hue text for AA contrast.
- Layout: CSS grid `sidebar 224px | main`.
  - Below 760px, the sidebar becomes a top bar with a `<details class="menu">` disclosure. No JS.
- Type:
  - rem scale 0.8 / 0.9 / 1 / 1.25 / 1.6.
  - `font-variant-numeric: tabular-nums` on time, ID and digest cells.
  - Monospace only for commits, digests and log bodies.
- Spacing: `--space-2xs` 4px, `xs` 8, `sm` 12, `md` 16, `lg` 24, `xl` 32, `2xl` 48. Use `gap` and vary the rhythm.
- Reusable classes, defined once:
  - `.board` (environment rows), `.facts` (dl grid), `.strip` (environment chooser), `.pill.{ok,busy,bad,warn}`
  - `.reason` (failure reason chip), `.actions` (inline forms row), `.checklist` (setup steps)
  - `.btn`, `.btn.ghost`, `.btn.danger`, `.crumbs`, `.badge`
- Bans (impeccable):
  - no `border-left`/`border-right` wider than 1px as an accent;
  - no gradient text, glass effects or nested cards;
  - no hero metrics or identical card grids;
  - no inline style.
- Motion: only 120ms color/background transitions on hover and focus, disabled under `prefers-reduced-motion`.

## Shared partials (layout.html; added in 22-3c)
- `nav` takes `navView{Active string; Items []navItem; ReconcilingCount int}`, where `navItem{Key, Label, Href, Group string}`.
  - Built by `h.navFor(active string) navView`, which omits items whose dependency is nil.
  - `ReconcilingCount` is fetched lazily only when ServerAdmin != nil. On error it is 0, logged at debug, and the page still renders.
- `envstrip` takes `[]envChoice{ID, Href string; Current bool}`.
  - Renders a `<nav aria-label="Environments" class="strip">` of links.
  - With more than 12 environments, it also renders a GET form `<select name="environment_id">` fallback.
- `checklist` takes `checklistView`: three steps (connect GitHub, register server, bind repository), each with `Done bool` plus its inline form or link. Used by every zero-data state.
- `reason` takes `reasonView{Class, ClassLabel, Reason, ReasonLabel, Probe string}` and renders a pill plus label text. It renders nothing when empty.

## Human labels (one map in internal/web/labels.go; unknown values render raw, escaped)
- Failure class:
  - transient: "Temporary failure"
  - authorization: "Authorization failed"
  - invalid_input: "Invalid input"
  - cancelled: "Cancelled"
  - corrupt_state: "Inconsistent state"
  - permanent_configuration: "Configuration problem"
- Reason:
  - container_unhealthy: "Image HEALTHCHECK reported unhealthy"
  - deadline_exceeded: "Health check deadline exceeded"
  - container_not_running: "Container stopped before passing"
  - context_cancelled: "Health check cancelled"

---

## Slice briefs
Every slice runs this loop:
1. Fail-first test(s).
2. Implement.
3. Verify (commands below).
4. Peer review by a fresh shell-capable general-purpose reviewer (cross-file).
5. Fix.
6. The orchestrator checks the diff.
7. Atomic commit, done by Haiku with a given message. No Co-Authored-By.

Standard verify commands:
- Web: `go vet ./internal/web/... && go test -count=1 ./internal/web/...`
- Store slices: also run the integration recipe (memory: autodeploy-progress) for `./internal/store/postgres ./internal/web`.
- Agent slice: `go test -count=1 ./internal/agent/...`

Standard brief footer (paste into every subagent brief):
- RULE-01: NEVER delete any file before confirming with the user AT LEAST TWICE. List files you think should be removed instead of removing them.
- Do not build binaries into the repo root. Do not leave backups in /tmp or the repo. Use the session scratchpad.
- Do not commit. Report the diff summary, test output, and mutation evidence (revert the fix and show the test fails).
- Keep every existing form `name=`, `action=` and URL unless the slice says otherwise. Do not touch files outside the listed set without asking.

### 22-3a Design mock (no Go)
- **Do:**
  - Static HTML and CSS mock in the scratchpad, following the visual system above.
  - Screens:
    1. Overview, populated: 3 environments, one blocked and one failed with a reason.
    2. Overview, empty system: checklist.
    3. Environments.
    4. Runtime config with the strip.
    5. Deployment detail, failed: summary header with class, reason and probe, plus all sections.
    6. Logs: picker, live badge, grouped attempts.
    7. Reconciliation.
  - Each in light, dark and 360px width.
  - Data is modeled on acer attempts 23 and 30.
- **Deliver:** a private artifact (system fonts, D6). The owner approves.
- **Gate: no Go change before approval.**
- The approved CSS becomes the starting point of 22-3c's `app.css`.

### 22-3b Parity guard (tests only)
- **Files:** NEW `internal/web/parity_test.go`.
- **Do:**
  - A table of `{name, setup func(*Handler), target, mustContain []string}` for every current page, using the existing stubs.
  - Pages: landing, projects, deployments, detail (leased plus failed), logs, servers, config, route, enrollment token, github complete, github continue.
  - Fragments are data values, `name=`/`action=`/`href=` attributes and pill values. Never prose sentences.
  - Examples:
    - detail: `name="nonce"`, `action="/deployments/rollback"`, the commit, the digest, the attempt failure class.
    - servers: each server ID, `action="/servers/revoke"`, `name="reason"`.
  - Each entry also records the target *after* 22-3. For example, the reconciling rows move from `/servers` to `/reconciliation`, and the GitHub forms move from `/` to `/github`.
  - Moved entries start out pointing at their current page. The slice that moves the content flips the target in the same commit.
- **Acceptance:** passes on current main. Mutation: delete one field from a template and see it fail. Then restore it.

### 22-3c Shell, tokens, static assets
- **Files:** `templates/layout.html`; all `templates/*.html` (only the `nav` call and the outer wrappers); `static/app.css`; `templates.go`; `web.go` (`securityHeaders`, static serving); NEW `nav.go`; `layout_test.go`.
- **Do:**
  - Sidebar layout and the `navView` partial (see Shared partials).
  - Every page passes `.Nav`: add `Nav navView` to each view struct, set by `h.navFor(key)`.
  - Migrate the older `tmpl.Execute` pages to `renderOperations`.
  - Static serving: an exact-path allowlist map `{"/static/app.css": css, "/static/logs.js": js (22-3k)}`.
    - Content types: text/css, text/javascript.
    - Embedded bytes only, no directory serving.
    - `Cache-Control: no-store` stays.
- **Tests:**
  - Update `wantCSP`.
  - The CSP-clean loop still passes.
  - NEW `TestNavShowsEveryAvailablePage`: with all dependencies set, the nav contains the 10 hrefs. With `ServerAdmin` nil, there is no Servers or Reconciliation link.
  - Parity passes unchanged.
- **Out of scope:** page content changes.
- **Acceptance:**
  - Every existing page renders in the new shell and every test passes.
  - Chrome screenshot of 3 pages, light and dark, taken from a local run (orchestrator).

### 22-3d Store reads: environment overview and failure reason
- **Files:**
  - NEW `internal/store/postgres/environment_overview_read.go` plus `_integration_test.go`.
  - `operations_read.go`: add `FailureReason`/`FailureProbe` to the detail and summary structs.
- **Do (1): environment overview.**
  - Signature: `(r *OperationsReadRepository) ListEnvironmentOverview(ctx, principal) ([]EnvironmentOverview, error)`. Use `beginRead` (RepeatableRead, read-only, live session).
  - `EnvironmentOverview`:
    - Environment: `EnvironmentID string`
    - Project and binding: `ProjectID string; Bound bool; BindingActive bool; DeployRef string`
    - Server: `ServerID string; ServerName string; ServerStatus string; LastHeartbeatAt time.Time`
    - Ownership and route: `Owned bool` (has an agent_jobs row for the owner); `Hostname string; RouteRevision int64; ActiveReleaseID string; ActiveCommit string; ActiveSince time.Time`
    - Config and reconciliation: `ReconciliationRequired bool; ReconcilingAttemptID int64; Strategy string`
    - Latest deployment: `Latest *DeploymentSummary; LatestFailureClass string; LatestFailureReason string`
    - Rollback: `RollbackTargetReleaseID string; RollbackTargetCommit string`
  - Environment set CTE:
    ```
    env AS (
      SELECT environment_id, project_id, server_id, deploy_ref, active, true AS bound
        FROM github_project_bindings
       WHERE owner_id=$1 AND environment_id IS NOT NULL
      UNION ALL
      SELECT DISTINCT environment_id, project_id, server_id, NULL, NULL, false
        FROM agent_jobs
       WHERE owner_id=$1
    )
    ```
    Collapse to one row per `environment_id` (prefer the binding row) and cap at `MaxEnvironmentsListed = 200`, ordered by `environment_id`.
  - `owned` is `EXISTS (agent_jobs WHERE environment_id=e AND owner_id=$1)`. **LEFT JOIN `route_heads`, `environment_slots`, `environment_runtime_configs` and `releases` ONLY `ON ... AND owned`.** This is the hard invariant.
  - Server: `LEFT JOIN servers s ON s.id = COALESCE(binding.server_id, latest.server_id) AND s.owner_id=$1`.
  - Latest deployment: `LEFT JOIN LATERAL (agent_jobs j JOIN deployments d ... WHERE j.owner_id=$1 AND j.environment_id=e ORDER BY d.created_at DESC, d.id DESC LIMIT 1)`.
  - Latest failure: a LATERAL on the latest failed attempt of that job, giving `failure_class`.
  - Reason: a LATERAL over `deployment_log_chunks c WHERE c.attempt_id=a.id AND c.stream='health' AND c.body LIKE 'health gate: failed (reason=%' ORDER BY c.id DESC LIMIT 1`.
  - Rollback target: must mirror `evaluateRollback` (rollback_repository.go ~85-151). That means a `retained` target and an `active` source in the same environment, owned by the principal through the release's deployment job. (CORRECTED 2026-11-20 after review: an earlier rev said `ready`. Releases gain `active`/`retained` states in later migrations.)
  - Reconciling attempt: from `environment_slots.reconciliation_attempt_id` (owned only).
- **Do (2): parse the reason in Go, not SQL.**
  - `parseHealthFailureReason(body string) string` matches `^health gate: failed \(reason=([a-z_]+)( class=[a-z_]+)?\)\n?$` and returns the reason ONLY if it is in the 4-value allowlist. Otherwise it returns "".
  - No free text from logs ever reaches the view. Add the same for the probe line (D5): `^health gate: last probe: (.{1,200})$`, displayed escaped.
- **Do (3): GetDeploymentDetail.**
  - Add `FailureReason` and `FailureProbe` to `AttemptView` (per failed attempt, using the same LATERAL).
  - Add `ActivationSummary string` from the deployment's latest `release_operations.outcome_summary` (already sanitized, at most 512 bytes).
  - `DeploymentSummary` gains `FailureClass` and `FailureReason`. ListDeployments and ListProjects.LatestDeployment get these via the same LATERAL. Keep one query per list, with no per-row round trips.
- **Tests (integration, fail-first):**
  1. Zero environments returns an empty list with no error.
  2. A bound-only environment: `Owned=false`, no route or config fields even if a `route_heads` row exists for that ID.
  3. **Cross-owner collision**: owner B has jobs on `env-x` with a route head. Owner A has only a binding on `env-x`. A must see no hostname, release or config.
  4. Owned environment with route, active release and config: all fields set.
  5. Failed attempt with a health line, then a reason. With a non-allowlisted reason text, the reason is "". With no line (purged), the class only.
  6. The latest deployment is the newest by `(created_at, id)`.
  7. Rollback target excludes the active and non-ready releases.
  8. Revoked session returns the live-session error (existing sentinel).
  9. Ordering and cap.
  - Also a unit test for `parseHealthFailureReason`, table-driven, with all three line formats plus hostile inputs.
- **Verify:** the integration recipe, plus `EXPLAIN (ANALYZE)` of the overview query on a fixture with 50 environments and 500 jobs, pasted in the report.

### 22-3e Agent: ship the last probe (D5 = yes)
- **Files:** `internal/agent/executor/executor.go` (~417), `rollback.go` (~85), `sts.go` (~310), and their tests.
- **Do:**
  - After each `health gate: failed (...)` Linef, emit `logs.Linef(logship.StreamHealth, "health gate: last probe: %s", truncate(gateResult.LastProbe, 200))`.
  - Keep the existing `Diagf` journal lines.
- **Tests:** an executor test asserts the shipped line exists and is redacted (seed a secret into LastProbe and assert it is masked).
- **Acceptance:** acer reinstall later (22-3l).

### 22-3f Overview and Environments pages
- **Files:** NEW `templates/environments.html` and `overview.go`; `templates/landing.html` (becomes the Overview); `web.go` (routes `/` and `/environments`); NEW `overview_test.go`.
- **Do:**
  - NEW optional interface `EnvironmentOverviewReader { ListEnvironmentOverview(ctx, p) ([]postgres.EnvironmentOverview, error) }`, discovered on `Config.OperationsReader` (same pattern as `EnvironmentReleaseReader`).
  - Overview view: `overviewView{Nav, CSRF, ExecNotice, Attention []attentionItem, Board []boardRow, Recent []deploymentRow (ListDeployments limit 10), Servers []serverRow, Checklist *checklistView, CanDeploy bool}`.
  - `boardRow` covers:
    - environment and project, with links to config, route, deployments?environment_id= and logs?id=latest;
    - hostname (`https://` link when set) and server dot (ok if heartbeat < 2 min, else warn);
    - live release (commit, age);
    - last deployment (pill, `reasonView`, time, detail link);
    - inline forms:
      - **Deploy head**: POST `/deployments/manual` with the exact existing field names from projects.html and a fresh nonce. Only when `CanDeploy` and Bound.
      - **Roll back**: POST `/deployments/rollback` with the existing fields and confirm checkbox. Only when `RollbackTargetReleaseID != ""`.
      - **Resolve**: POST `/environments/resolve` with the existing fields including `attempt_id`. Only when `ReconciliationRequired`.
  - Attention items: blocked environments, servers with no heartbeat for more than 5 min or revoked, failed latest deployments, and the activation-disabled notice.
  - Checklist: shown when there are zero environments, OR as a compact "setup" row while any step is undone.
  - Environments page: the full board in table form plus strategy, route revision and the config/route/releases links.
  - Zero environments: the checklist (R3).
  - Redirect targets of `/deployments/manual`, `/deployments/rollback` and `/environments/resolve` stay unchanged for now. Add an optional hidden `return_to`, validated against an exact allowlist (`/`, `/environments`, `/reconciliation`, the detail page), so overview actions return to the overview.
    - **Open-redirect guard:** anything else falls back to the current target, with a test.
- **Tests:**
  - Board renders all fields for owned, bound-only, blocked and failed rows.
  - Forms carry CSRF, nonce and exact names.
  - Rollback and resolve forms are present only when allowed.
  - The `return_to` allowlist, including hostile values (`//evil`, `https://...`, `/\evil`).
  - Empty system shows the checklist.
  - A nil overview reader means the Overview falls back to checklist plus servers (no 500).
  - Add both pages to the CSP-clean loop and the parity table (flip the landing entries).

### 22-3g GitHub and Reconciliation pages
- **Files:** NEW `templates/github.html`, `templates/reconciliation.html`; `web.go`; `templates/servers.html`; `landing_setup_test.go`, `servers_test.go`, `environment_links_test.go`.
- **Do:**
  - `GET /github` renders install, refresh and bind (moved verbatim from landing, same names and actions) plus a bindings table from `ListProjects` (project, repository, ref, environment, server, active).
  - POST `/github/bindings` and `/github/installations/reconcile` redirect to `/github` (was `/`).
  - `GET /reconciliation` renders the reconciling table plus resolve forms (moved from servers).
    - When none are blocked, it shows every owned environment's activation state from the overview reader (R3).
  - `/environments/resolve` redirects to `/reconciliation` (was `/servers`).
  - Servers page: replace the table with an attention banner linking to Reconciliation when count > 0. Its environment-ID GET form is removed, because the strip replaces it. Note it in the commit.
  - Nav: GitHub points to `/github` (was `/#github`).
- **Tests:** update the redirect assertions and flip the parity entries. environment_links_test now expects config/route links on Reconciliation instead of Servers.

### 22-3h First-class Config, Routes, Logs and Deployments filters
- **Files:** `web.go` (config and route GET handlers), `logs.go`, `operations.go` (deployments filters), the templates, and their tests.
- **Do:**
  - Environment choices: `h.environmentChoices(ctx, p) []envChoice` from the overview reader, `Owned` only (the config and route stores deny unowned environments).
  - Config and route: `environment_id` is optional. If absent, select the first owned environment.
    - Unknown or unowned values keep today's response; do not leak existence.
    - Zero owned environments: render the checklist with "Deploy once to create an environment's configuration" plus the Deploy form (R3).
    - A selected environment with no config (`Exists=false`) renders the create form prefilled with defaults, not a sentence. Same for a route with no hostname.
  - Logs: `id` is optional. If absent, use `ListDeployments(limit 1)` and render the picker: a GET form `<select name="id">` of the last 20 deployments (label: env, commit, status, time).
    - Zero deployments: checklist.
    - `after` still requires `id`.
  - Deployments: add a GET filter form with `<select name="project_id">` and `<select name="environment_id">` (options from ListProjects). Keep the existing params and cursor semantics.
  - Add a reason column (`reasonView`) to deployments and projects.
- **Tests:**
  - No-ID GETs return 200 with the first environment selected, and the strip lists all owned environments with `aria-current`.
  - An unowned ID returns the same status as today.
  - Zero environments or deployments shows the checklist.
  - Logs picker. Filter form round-trip.
  - The query allowlist rejects unknown params as today.

### 22-3i Deployment detail summary header
- **Files:** `templates/deployment-detail.html`, `operations.go` (view), `labels.go`, tests.
- **Do:**
  - Summary header with:
    - status pill, live-route pill, rolled-back notice;
    - `reasonView` (latest failed attempt: class, reason, probe) plus `ActivationSummary` (escaped) when set;
    - facts grid (project, environment with config/route links, server, commit, created, updated, drain);
    - actions row (Retry, Roll back, Cancel, Logs), using the existing forms unchanged.
  - Sections: timeline, job and events, attempts (adds reason and probe columns), release, instances.
  - State flags (live on route, rolled back, route reconciliation required) are facts. If reworded, give them a stable marker (e.g. `data-fact="live-route"`) and flip the parity pins in the same commit.
  - Empty sections collapse into one "Not yet" line in the summary facts instead of standalone paragraphs.
  - Refresh behavior is unchanged.
- **Tests:**
  - Failed attempt with a reason: header shows the class and reason labels.
  - Non-allowlisted value: nothing shown.
  - The activation summary is escaped (`<script>` input).
  - Parity passes.

### 22-3j Empty states, text-only guard, nav coverage
- **Files:** remaining templates (enrollment token, github continue, servers empty table), NEW `structure_test.go`.
- **Do:**
  - Enrollment token page:
    - token in a `<pre class="token">` (readonly);
    - expiry shown as an absolute time plus "valid N min";
    - the exact `install-app.sh` enrollment command from docs/operations;
    - next steps with links to Servers and Overview.
  - github continue: a step indicator plus the Continue button.
  - Servers with an empty table: register form plus install hint.
- **Tests (the guard rules, permanent):**
  - `TestNoPageIsTextOnly`: for every nav page and flow page, in BOTH the empty-stub and populated states, `<main>` contains at least one `<form`, `<table` or `class="checklist"`.
  - `TestEveryGetRouteIsInNav`: a list of GET routes kept next to the router (single source: `navRoutes` in nav.go). Every one except `/deployments/detail` and the flow pages appears in the nav when its dependency is set.

### 22-3k Live logs (SSE)
- **Files:** NEW `logs_stream.go` and `static/logs.js`; `logs.go`; `templates/deployment-logs.html`; `templates.go` (embed js); `web.go` (route, allowlist, CSP); tests.
- **Route:** `GET /deployments/logs/stream?id=&after=`. Add it to the query allowlist. Nil `DeploymentLogReader` gives 404. Use `operationsPrelude` and the same param validation as logs.go.
- **Handler loop:**
  - Headers: `Content-Type: text/event-stream`, `Cache-Control: no-store`, `X-Accel-Buffering: no`.
  - Limits: per-session concurrency cap 3 and process cap 64 (a mutex-guarded counter). Over the cap returns 429 with Retry-After 5.
  - Start: `after` from the query or the `Last-Event-ID` header (the larger valid one).
  - Each tick (1s):
    1. `SetWriteDeadline(now+10s)`.
    2. `ListDeploymentLogs(ctx, p, id, after, 0)`. This re-checks the live session every tick, so expiry ends the stream with `event: end` `data: {"reason":"session"}`.
    3. For each line, write `id: <chunkID>`, `event: line`, and `data: <json {attempt, stream, label, clock, stamp, body, marker}>`. Advance `after`.
    4. When the page is empty and the job is terminal (`GetDeploymentDetail`, checked at most every 5s), send `event: end`, then return.
  - Heartbeat `: ping` every 15s.
  - Hard cap of 15 min, then `event: end` `{"reason":"rotate"}` and the client reconnects.
  - Stop on `r.Context().Done()`.
  - Never log body text.
- **logs.js** (no framework, at most about 120 lines):
  - Progressive enhancement: find `[data-stream-url]` on the logs page, open an `EventSource`, append lines with `textContent` only (never innerHTML), and group by attempt.
  - Follow-tail unless the user scrolled up, in which case show a "Jump to live" button.
  - Show a live/paused/ended badge. On `end` with `rotate`, reconnect with the last id.
  - Remove the meta refresh effect by navigating with `?live=1`, which the server answers without the refresh tag. Pages without JS keep refreshing.
    - logs.go `onlyParams` gains `live`, which accepts exactly the value `1`. Anything else returns 400. Test it.
- **Template:** the logs page includes `<script src="/static/logs.js" defer></script>` only when the job is non-terminal.
- **CSP:** add `script-src 'self'; connect-src 'self'`. layout_test bans `<script` except that exact tag.
- **Tests:**
  - httptest server with a fake clock or short tick: streams lines in order with ids.
  - Resumes from Last-Event-ID.
  - Ends on a terminal job.
  - Ends on session invalid.
  - Cap returns 429.
  - Write deadline is extended (no 20s cut-off: run with a test server whose WriteTimeout is 2s and stream for 5s).
  - Body with `<script>` arrives JSON-escaped.
  - Unknown params return 404.
  - JS: a lint-level check that the file contains no `innerHTML`, `eval` or `Function(` (string test).

### 22-3l Validation and docs
- Full sweep: web unit, store integration recipe, agent tests, `go vet ./...`.
- Acer:
  - Build and install (install-app.sh; plus the agent if 22-3e).
  - Run a failing deploy (demo HEALTHCHECK exit 1) and a passing one.
  - Chrome screenshots of every nav page: light, dark and 360px; empty and populated.
  - Live logs during a deploy. Rollback and resolve from the Overview.
- Docs: update docs/IMPLEMENTATION_STATUS.md (UI outcome), tasks/22-plan.md progress, and memory.

## Order and gates
1. 22-3a, then owner approval of the mock.
2. Then 22-3b, 22-3c, 22-3d, (22-3e), 22-3f, 22-3g, 22-3h, 22-3i, 22-3j, 22-3k and 22-3l, one commit each.
- 22-3d (store) and 22-3e (agent) ran in parallel. Owner 2026-11-20: from 22-3f on, slices run strictly one at a time.

## Risks
- Cross-owner leak in the overview join, because the environment namespace is global. Mitigation: the `AND owned` guard on every non-owner-scoped table, plus a mandatory collision test (22-3d test 3).
- Log-derived reason drift if a format string changes. Mitigation: the allowlist parse returns "" safely, and an executor test pins the exact format strings.
- Open redirect through `return_to`. Mitigation: an exact allowlist plus hostile-value tests.
- SSE resource use. Mitigation: caps, a 15-min rotation, a per-tick live-session check, and the context-done exit.
- Test churn, about 100 tests. Mitigation: the parity test first, stable names and actions, and each slice updates only its own tests.
- CSP widening. Mitigation: same-origin only, an exact header pinned by test, and the single script tag allowlisted.
- Unchanged and queued: the 24h retention check (2026-11-21), the public VPS e2e, and the backup drill.

## Progress
- 2026-11-20: inventory and facts gathered (3 read-only sweeps), `.impeccable.md` written, D1-D4 decided, plan rev 2 written. Owner: D5 yes, D6 system fonts. NEXT: 22-3a mock.
- 2026-11-20: 22-3a mock published (private Design canvas https://claude.ai/artifact/5rkZG7V8SkXgtz6UxPSLKh). Eight boards: Overview, Overview empty, Environments, Runtime config, Deployment failed, Logs, Reconciliation, Overview phone. Each has a light/dark tweak. Not mocked: Routes, Projects, Deployments list, Servers, GitHub, enrollment token. These reuse the same components. Awaiting owner approval and feedback.
- 2026-11-20: owner approved the 22-3a mock ("seems alright"). NEXT: 22-3b parity guard.
- 2026-11-20: 22-3b done. internal/web/parity_test.go has 16 entries. Owner-side review added pins for the live-route, rolled-back and reconciliation-required flags, which the subagent had classed as prose. Mutation evidence: blanking the digest or drain_seconds fails the test. NEXT: 22-3c shell.
- 2026-11-20: 22-3c done. Sidebar shell (nav.go; nav lists only pages that exist, later slices add theirs), app.css rewrite, renderOperations migration, static allowlist, env-gated ui_dump_test. Owner-approved adjustments: phone nav wraps instead of a collapsible menu; landing sign-out moved into the sidebar. Review PASS; the side-border guard was tightened to cover logical properties and var(). Screenshots taken with headless Chrome (extension not connected).
- 2026-11-20: 22-3e committed ec914fb. Last probe is shipped on all 3 failure paths through an owner-chosen shape allowlist (safeProbe); anything else ships as 'probe detail withheld', which closes the rollback path's empty-redaction-set gap. Probes ending 'context canceled' or 'TLS handshake timeout' are also withheld (acceptable).
- 2026-11-20: 22-3d review FAIL. H1: rollback target used `ready`, but eligibility needs `retained` (plan error, from facts read only up to 000014). H2: ActiveCommit and the rollback target were not release-owner checked. Also M1-M3 (slot guard test, unused probe lateral, failure fields shown after a successful retry). Fixes are back with the implementer. Lesson: facts about state enums must come from the latest migration, not the creating one.
- 2026-11-20: 22-3d done after 2 reviews. ListEnvironmentOverview: one query; env-level fields gated on environment ownership, release-level fields (ActiveCommit/Since, rollback target) gated on release ownership. Rollback offer mirrors evaluateRollback, tested by construction in both directions with refusal variants. Failure class/reason shown only when the latest attempt failed; probe lateral only on detail. Follow-up: partial index on deployment_log_chunks (attempt_id, id) WHERE stream='health' if needed.
- 2026-11-20: 22-3f done. Overview board with inline Deploy/Roll back/Resolve, attention list, checklist, recent deployments, servers (GitHub forms stay until 22-3g); /environments page; return_to exact allowlist; failure labels; a board read failure degrades to a notice. Review PASS plus follow-ups. Screenshot note: headless Chrome's minimum viewport is 500px, so phone shots use width 500.
- 2026-11-20: 22-3g done. /github page (forms moved verbatim plus a bindings table; GitHub POSTs and setup-complete redirect to /github). /reconciliation page (blocked table plus resolve forms moved from /servers; resolve defaults to /reconciliation; nav badge with aria-label). Servers lost the reconciling table and the env-ID form and gained a banner. Review PASS. Kept one badge query per page for consistency with the Reconciliation list; deriving from board rows can disagree on shared env IDs. Follow-up: /servers and /reconciliation answer a ListReconcilingEnvironments failure with 400 (pre-existing); fix needs auth-denied → /login handling. Owner confirmed twice the deletion of 3 stray sed -E backups.
- 2026-11-20: 22-3h done. Runtime config and Routes work without an id (env strip of owned envs, first selected; unknown/unowned ids keep the old response; zero owned shows checklist plus Deploy forms; >12 envs add a select). Logs works without an id (newest deployment plus a picker of 20; after requires id; the no-id page does not auto-refresh and links to the explicit-id page so a newer deployment can't replace the watched one). Deployments filter selects; reason column on deployments and projects. Nav now lists every planned page. Review PASS.
- 2026-11-20: 22-3i done. Deployment detail summary header: status, data-fact live-route/rolled-back markers (parity pins flipped), a failure block only for status failed (class/reason labels, last probe from the newest attempt only when it failed, activation summary), a facts grid with 'not yet' lines instead of empty paragraphs, and an actions row with danger-styled Roll back/Cancel; attempts gain Reason/Last probe columns. Review PASS; probe gating aligned to the store's latest-attempt rule.
- 2026-11-20: 22-3j done. Enrollment page (token in pre.token shown once, absolute expiry + valid N minute(s), host command mirrored from install-app.sh with allowlist-validated origin/server id, next steps); GitHub step indicators; empty Projects/Deployments/Servers/Reconciliation use the checklist (bind step reflects real binding on Deployments; server step unlinked without ServerAdmin). Permanent guards: TestNoPageIsTextOnly (navRoutes-driven, empty + populated) and TestEveryGetRouteIsInNav (parses router case literals; exemptions carry reasons). Review PASS plus fixes.
- 2026-11-20: 22-3k done. Live logs over SSE (GET /deployments/logs/stream): per-poll session re-check, caps 3/session and 16/process (429 + Retry-After), 2s tick, 15s heartbeat, 15-min rotate, per-write write deadline (server WriteTimeout unchanged), end reasons done/session/notfound/error/rotate. logs.js builds DOM with textContent only and reloads once on done so late lower-id rows appear. The noscript meta refresh keeps no-JS browsers working. CSP adds only script-src 'self'; connect-src 'self'. Security review PASS plus fixes (late-row reload, cap messaging, lower DB load). Production defaults changed from the brief: 2s tick, process cap 16 (was 1s / 64).
