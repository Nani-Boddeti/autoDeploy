# Step 15g — deployment log channel: plan (owner decisions recorded 2026-11-11)

Owner decision (15-plan D1-log = b): build agent -> control plane -> PostgreSQL -> UI now, although nothing has run on
a real host (`executorEnabled`/`activationEnabled` stay false). UI has no JS (CSP `default-src 'none'; style-src
'self'`), so the view is server-rendered with meta-refresh tailing. No code before owner review of this plan.

## Current state (verified 2026-11-11)
- Only sink today: `executor.Config.BuildOutput` (`internal/agent/executor/executor.go:77-79`, default `io.Discard`
  at :126-128), passed as BOTH `Stdout` and `Stderr` (:216-217); production wiring is `logWriter{logger}`
  (`cmd/agent/main.go:478,487-492`) = one `log.Print` per Write, i.e. agent-local log only, nothing shipped.
- `docker.Build` wraps stdout/stderr in `crypto.RedactingWriter` itself (`internal/runtime/docker/build.go:475-534`);
  redaction values = ONLY the resolved BUILD-scoped frozen bindings (`resolved[i].value`). Marker `[REDACTED]`
  (`internal/crypto/redactor.go:11`); construction failure suppresses output (`ErrRedaction`). Exact-byte matching only.
- Checkout/git output is separately captured + redacted via `lease.RedactingWriter` with its own output budget
  (`internal/gitcheckout/checkout.go:331-345`); it is NOT forwarded anywhere (captured only for error classing).
- Executor phases are reported via `progressReporter` (`internal/agent/executor/progress.go`, fire-and-forget
  `_ = client.ReportProgress`); health gate returns a `HealthGateResult` (`executor.go:277`); activation/drain write
  only `logger.Printf` lines (`executor/activation.go:202-444`, `executor/drain.go:355-487`).
- Wire conventions: `POST /agent/v1/leases/progress` (`internal/agent/http/progress.go`), body cap
  `MaxBodyBytes = 16 KiB` (`agent/http/handler.go:52`), identity = bearer credential + `X-AutoDeploy-Server-ID`,
  route table at `handler.go:206`. Client `post` retries only 503/transport, 5 tries, full-jitter 200ms..5s, 10s
  per-request timeout, 64 KiB response cap (`agent/client/httpclient.go:37-96,181-205`).
- `ReportProgress` store (`store/postgres/agent_lease_materialize.go:269+`): credential lock -> attempt.server_id ->
  slot `FOR SHARE` (fence, current_attempt, !reconciliation_required) -> job `FOR UPDATE` -> attempt `FOR SHARE`
  (leased, fence, authority unexpired) -> deployment `FOR UPDATE`. Refused once `cancel_requested_at` is set.
  A log endpoint must NOT copy this: `FOR UPDATE` on job/deployment per chunk would contend with Complete/cancel.
- Lease ends at hand-off (renewals stop, 10d); activation report/Activate outlive it; the drain runs after the
  200-ack with no lease at all (`tasks/10e-plan.md`).
- Schema: latest migration `000021`; append-only precedent = trigger raising on UPDATE/DELETE
  (`000001_create_deployments.sql:75-83`, `agent_job_attempts` never deleted `000013:235`). Owner path =
  `agent_job_attempts.job_id -> agent_jobs.owner_id` (no owner column on deployments). No log/retention code exists;
  the only background job is `AgentLeaseSweeper` (advisory-lock, `agent_lease_sweeper.go:88`). ADR 0005
  "Retention and drain policy" says logs get "independent bounded retention", durations undecided.

## Surprising findings / gaps
1. Redaction covers build-scoped values only. Runtime-scoped secrets and the checkout credential are not in the
   build redactor's set; a build tool echoing a runtime secret (or any value not frozen as a binding, e.g. derived
   base64/URL-encoded forms, tokens fetched by a build step) is NOT redacted. Matching is exact bytes.
2. Build output is delivered per `Write` of the redactor (arbitrary chunk, may end mid-line, mid-UTF-8, and the
   redactor holds back up to `maxPattern-1` bytes until Close); stdout and stderr share one sink, so the sink must be
   goroutine-safe and must not assume line boundaries.
3. Checkout output is dropped, not captured for display; phase/health/activation/drain text exists only as agent
   `Printf` lines containing no structured data. Container runtime logs have no capture path at all (no Engine
   logs call in `internal/runtime/docker`).
4. `MaxBodyBytes` (16 KiB) is too small/wrong shape to reuse blindly; `Complete`/`activation-result` are the only
   paths the agent can still call after the lease ends, and they are lease-fenced.

## Owner decisions (2026-11-11)
- Owner chose: L1 (a) structured deploy-step lines only, container runtime logs deferred; L4 purge attempts closed
  > 14 days, always keep newest 20 per environment, 256 KiB per attempt cap.
- Orchestrator defaults (recommended options): L2 caps as R; L3 30-min post-close window, no slot/job/deployment
  locks; L5 pattern sanitizer only; L6 separate logs page, 3s tail refresh.

## Options as drafted (R = recommended)
- L1 Scope. (a) R: agent-generated, structured lines only: build stdout/stderr (redacted), executor phase markers,
  checkout result line (no git output), health-gate summary (attempts, status class, elapsed; no body), activation
  steps, drain outcome. (b) also container runtime logs: DEFER (new Engine API logs-follow client, unbounded volume,
  runtime secrets appear in app output, redaction cannot be exact; revisit after the real-host run).
- L2 Storage caps. R: 256 KiB per attempt (~ build tail matters most), 4 KiB per chunk text, 4000 chunks per
  attempt; on cap a single `truncated` marker row, further chunks dropped at the server (count only). Alternatives:
  1 MiB. Postgres 18 TOAST not needed at these sizes.
- L3 Post-lease phases. R: accept chunks for an attempt while it is leased OR closed < 30 min ago (covers activation
  report + 300s drain + retries); no slot/job/deployment locks; fence must equal the attempt's own fence; server
  derives phase ordering from nothing the agent claims. Alternative: lease-only (loses activation + drain lines).
- L4 Retention. R: purge chunks of attempts closed > 14 days ago AND keep only the newest 20 attempts per
  environment, run by a new bounded batch step on the existing sweeper tick (advisory lock, LIMIT batch). Alternative:
  defer purge with the hard per-attempt cap only (growth then bounded by attempts x 256 KiB; ~ 2.5 MB per 10 deploys).
- L5 Server-side second pass. R: cheap pattern sanitizer only (token shapes `ghs_`/`ghp_`/`github_pat_`, `Bearer ...`,
  PEM blocks, this system's own credential/enrollment token format, control chars), NOT value-based: the control plane
  would have to decrypt every secret per chunk (ADR 0003 plaintext exposure) — rejected.
- L6 UI. R: separate page `/deployments/logs?id=&after=` (linked from detail), not an inline section (keeps the
  5 s detail refresh cheap); 200 rows/page, keyset by chunk `id`; page meta-refreshes 3 s only while the job is
  non-terminal AND the page is the tail (no `after` or `after` = last id), with a "load more" link when more exist.

## Design

### Table (migration 000022, forward-only)
- `deployment_log_chunks`: `id BIGINT GENERATED ALWAYS AS IDENTITY PK` (UI cursor), `attempt_id BIGINT NOT NULL
  REFERENCES agent_job_attempts(id) ON DELETE RESTRICT`, `stream TEXT CHECK IN ('build','phase','health','activation',
  'drain','system')`, `writer_epoch BIGINT NOT NULL`, `seq BIGINT NOT NULL CHECK (seq>0)`, `body TEXT NOT NULL CHECK
  (octet_length(body) BETWEEN 1 AND 4096)` (valid UTF-8 guaranteed by `text` type), `created_at TIMESTAMPTZ DEFAULT
  clock_timestamp()`, `UNIQUE (attempt_id, stream, writer_epoch, seq)` (idempotent retry key; `writer_epoch` = agent
  process start so a restarted agent resuming activation/drain cannot collide with its earlier seq), index
  `(attempt_id, id)`.
- `deployment_log_attempt_totals(attempt_id PK FK, bytes, chunks, truncated bool, updated_at)`: the only mutable
  table; one writer per attempt in practice; the insert + counter update are one tx; cap check inside the tx.
- Append-only triggers: UPDATE always rejected; DELETE rejected unless `current_setting('autodeploy.log_purge', true)
  = 'on'` (set `SET LOCAL` only by the purge function). Totals row deleted with its chunks.
- Owner scoping: read query joins chunks -> attempts -> agent_jobs and filters `agent_jobs.owner_id = principal`;
  unknown/other-owner id = the same generic 404 as detail (15b convention). No owner column duplicated.

### Agent shipper (new `internal/agent/logship`)
- `Shipper` with bounded in-memory ring (256 KiB cap) and one goroutine per active attempt; `Write` never blocks and
  never errors (returns len(p)); line-assembles (flush partial line on 1 s idle or 4 KiB), `strings.ToValidUTF8`,
  strips control chars except `\n\t`, splits to <= 4 KiB chunks, assigns `(stream, epoch, seq)`.
- Batch: up to 16 chunks or 24 KiB or 2 s, one `POST /agent/v1/leases/logs`. Own short retry (2 tries, 5 s
  timeout) on the same client transport; NOT `post`'s 5x loop (must not hold up exit). Any non-retryable answer
  (stale/403/404/409/cap) stops that attempt's shipping silently.
- Overflow: oldest-drop is wrong for build tails; R: drop NEWEST when full, count drops, emit one
  `[logs dropped: N bytes]` marker as the next chunk when space returns. Flush at attempt end with a hard 2 s budget;
  `Close` is bounded; shipper failure only logs a counter via the agent logger, never an error to Execute.
- Phase/health/activation/drain lines are written through a small `Lines.Printf(stream, ...)` API that takes only
  fixed formats with enum/numeric args (never error text from the Engine, URLs, or env), so redaction risk there is
  structural, not value-based. Build and git-result text pass through the existing redactors BEFORE reaching the
  shipper (the shipper is downstream of `crypto.RedactingWriter`, never a bypass). A final defensive pass in the
  shipper re-runs a `RedactingWriter` over the lease's full known secret set (build + runtime scoped values from the
  attempt's `secretMaterializer`, `executor/secrets.go`) so runtime values are also masked on the wire (closes gap 1
  for exact values); encoded forms remain a documented residual.

### Endpoint `POST /agent/v1/leases/logs`
- Auth: `h.identity` as progress. Body: `{attempt_id, fence, writer_epoch, chunks:[{stream,seq,body}]}`; dedicated
  cap 64 KiB (`MaxLogBodyBytes`), <= 32 chunks, strict decode (unknown fields rejected like other handlers), stream
  allowlist, body 1..4096 bytes valid UTF-8, seq > 0.
- Store `AppendLogChunks`: lock active credential (existing helper) -> `SELECT` attempt (server_id must equal caller,
  fence must equal, state leased OR closed_at > now()-30 min) -> insert with `ON CONFLICT (attempt_id,stream,
  writer_epoch,seq) DO NOTHING` -> upsert totals, enforce caps (partial accept: stop at cap, write one truncated
  marker, set `truncated`). No `FOR UPDATE` on job/slot/deployment; no slot fence compare (slot may already belong
  to a newer attempt after close — the attempt's own fence is the authority). Cancel requested: still accepted.
  Errors: same `ErrServerAuthorizationDenied` -> generic 403/404 mapping as progress; duplicates return 200.
- Server pass L5 sanitizer applied to each body before insert.

### Retention
- `PurgeLogChunks(ctx, batch)` on the sweeper tick under its advisory lock, bounded `DELETE ... WHERE id IN (SELECT
  ... LIMIT n)`; sets the purge GUC inside the tx; both predicates of L4.

### UI
- Store read `ListDeploymentLogs(principal, deploymentID, afterID, limit)` mirroring 15b (owner join, no locks,
  keyset, newest attempt first by default? R: oldest-first, grouped by attempt number, all attempts of the job).
- Template `deployment-logs.html` in `internal/web/templates`, new file `internal/web/logs.go` (web.go is 1800+ lines);
  rows rendered in `<pre>` via `html/template` auto-escape only; `Cache-Control: no-store`; query whitelist `id`,
  `after`; "truncated" / "dropped" markers shown as plain text; honesty banner "executor disabled: no logs yet"
  driven by data (zero chunks + job terminal/never-leased), never implying live capture.

## Sub-steps (one subagent each, independent review, atomic commit; run gates false throughout)
1. **15g-1 Schema + store** (full bar): `migrations/000022_deployment_log_chunks.sql` + `_test.go`;
   `store/postgres/deployment_logs.go` (`AppendLogChunks`, `ListDeploymentLogs`, `PurgeLogChunks`) + integration
   tests on fresh PG18: two-owner isolation, idempotent replay, cap/truncation, UPDATE/DELETE rejected (purge GUC
   only), concurrent same-seq inserts, no-lock proof vs concurrent Complete, EXPLAIN uses indexes. Risk: HIGH.
2. **15g-2 Endpoint + client** (full bar for auth): `agent/http/logs.go` + route, `agent/client` `AppendLogs`
   (no 5x retry), handler tests (bad fence, wrong server, expired >30 min, cap, oversize, unknown field, stream
   allowlist, L5 sanitizer). Risk: HIGH (new authorization surface).
3. **15g-3 Shipper + wiring** (agent-only; race): `internal/agent/logship`, executor `Lines` markers, tee of
   `BuildOutput` (additive: keep `logWriter` fast path/local log), health/activation/drain lines. Race tests,
   fuzz of redaction with known secrets (incl. split across Writes and across chunks), overflow, blocked-server,
   memory-bound, flush-budget test. Risk: MEDIUM-HIGH.
4. **15g-4 Retention** (SQL, full bar): sweeper hook, purge tests (protects newest N, age, batch bound, concurrent
   sweepers via advisory lock). Risk: HIGH-medium.
5. **15g-5 UI** (low): `/deployments/logs`, link from detail, escaping/auth/404/paging/refresh tests, two-owner web
   test. Risk: LOW.
6. **15g-6 Docs**: ADR 0004 amendment (new lease-adjacent endpoint, post-close window, no-lock rule) and ADR 0003
   amendment (log redaction scope and its limits), ADR 0005 retention-durations note; IMPLEMENTATION_STATUS; 15-plan
   progress; todo UI item stays partial (no real-host validation). Container runtime logs = future 15g-7, not planned.

## Guards (must never happen)
- A secret value in `deployment_log_chunks` (tests with known secrets through the real redactors, plus a DB scan).
- Logging blocks, delays beyond its 2 s flush budget, or fails an attempt/Complete/activation/drain (shipper errors
  swallowed; test with hung/erroring server; `Write` never blocks).
- Cross-owner read; chunk accepted for another server's attempt, wrong fence, or long-closed attempt.
- Unbounded growth: DB per attempt (caps), agent memory (ring cap, drop-newest), request size, purge keeps pace.
- Lock contention: no `FOR UPDATE` on job/slot/deployment; no GET side effects.
- Gates untouched; no Docker/control-plane coupling (ADR 0001/0005); no JS/CSP change.

## Verification by risk
15g-1/-2/-4: gofmt, vet, `make check`, `go test -race ./...`, PG18 integration (real contention, two owners), vacuous
test audit, independent review loop. 15g-3: race tests, fuzz, Linux build. 15g-5: render/escape/auth tests, low bar.

## Risks
- Residual redaction gap: encoded/derived secret forms and non-frozen tokens in build output; mitigated by
  runtime-value pass + L5 patterns + small cap, not eliminated. Owner may prefer showing only phase/health lines
  until a real run (L1 narrow option).
- Volume, ordering under interleaved build stdout/stderr, and real buildx output shape unvalidated without a host.
- Post-close window widens what a stolen agent credential can write (bounded: own attempts, caps, 30 min).
- Tail UX via 3 s refresh on large attempts; page size 200 mitigates.

## Cannot be proven without a real host
Real buildx/Engine output volume and line shape, flush behaviour on agent kill mid-build, drain/activation timing
inside the 30 min window, network failure modes on a real link, actual secret-leak surface of real build tools.

## Progress
- 15g-1 migration 000022 (`deployment_log_chunks`, `deployment_log_attempt_totals`, append-only triggers, purge GUC
  `autodeploy.log_purge` accident guard, index `agent_job_attempts(environment_id, id DESC)`) + `DeploymentLogRepository`
  (AppendLogChunks via shared `lockActiveServerCredential`; totals-row upsert serializes same-attempt caps; first
  write wins on replay conflict; ListDeploymentLogs owner-scoped + live session; PurgeLogChunks). DEVIATION: no FK to
  agent_job_attempts (FK insert takes FOR KEY SHARE, conflicts with lease FOR UPDATE; attempts are undeletable by
  trigger). Review PASS with notes. Carried:
  - 15g-2/15g-3: store rejects a whole batch on a NUL byte -> shipper must strip NUL/control chars before send.
  - 15g-5: chunk-id keyset can miss a lower id committed late by an overlapping previous attempt's post-close
    window -> render from after=0 once the job is terminal (or document).
  - 15g-6: docs must state no FK (plan Design text still shows REFERENCES).
- 15g-2 `POST /agent/v1/leases/logs` (auth = progress route; route-only 64 KiB body cap; ≤32 chunks; L5 sanitizer:
  control chars, PEM, Authorization header/JSON/quoted forms, Bearer, GitHub token shapes, srv/enr tokens; all-empty
  request -> 400) + client `AppendLogs` (single attempt; 403/404 permanent locally; shared mapStatus untouched;
  ctx cancel classifies retryable -> shipper must check ctx.Err() itself). Review PASS with notes, both fixed.
- 15g-3 `internal/agent/logship` (per-Start strictly increasing epoch; seq per stream/epoch, stable across retries;
  256 KiB queue incl. in-flight, drop-newest + one marker per episode; batches ≤32 chunks/~60 KiB encoded; 6 tries
  capped backoff; 2s Close budget) + executor wiring (build tee, phase/health/activation lines). Redaction: controls
  stripped -> crypto.RedactingWriter on raw stream -> assembly; armed by `secretMaterializer.onFetched` (no extra
  materialize calls; call counts/classes identical, tested); no build-scoped bindings -> empty set (build holds no
  secrets); fetch failure -> build output suppressed. Review 1 PASS with notes (eager fetch, post-redaction strip)
  -> fixed -> re-review PASS. Gaps for 15g-6 docs: drain lines not shipped (drain record lacks fence); checkout
  credential not reachable (checkout output not shipped); runtime-only secret in build output not masked (cannot
  reach build); encoded forms residual; Execute may wait up to ~2.1s on a hung log server.
- 15g-4 `LogPurgeScheduler` triggered async after each sweeper tick (non-blocking; ≤ every 10 min; 500 × ≤40
  batches, 30s budget; own session try-lock key 0x61676e745f6c7033 on a dedicated conn, conn destroyed if unlock
  fails); env `AUTODEPLOY_LOG_RETENTION_MAX_AGE` (336h, 1h..365d) / `AUTODEPLOY_LOG_RETENTION_KEEP_NEWEST` (20,
  1..10000). Review PASS with notes (pool of 1 would starve; no recover, matches repo).
