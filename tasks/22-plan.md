# Step 22+ roadmap to completion (written 2026-11-16, owner approved the order)

Baseline: origin/main = 22fe8c7. 11/18 outcomes (~61%), weighted ~81%, hard gates 13/15 (open 3, 12).
Each task below gets its own design section here (investigate -> design -> owner approval -> implement with a
failing test first -> full bar -> independent review -> owner sees diff -> commit), then acer validation.

## Phase 1: acer-provable, little code
- 22-A 24h check: rt-19 removed (after ~2026-11-17 16:00); Releases table shows it removed.
- 22-B crash recovery: kill agent mid-deploy (and one control-plane restart mid-deploy); recovery completes or
  blocks correctly, nothing lost. Closes the main validation gap; required before flipping gates.
- 22-C gate 3: observe real Traefik reload/route timing on the acer.
- 22-D owner decision: ADR 0005 STS failure with predecessor restart OK -> finalize failed without operator step
  (agent "definitely not switched" report kind + control-plane branch requiring unchanged route head). ~2-3h if yes.

## Phase 2: mac-side code closing outcomes (owner 2026-11-16: finish the application first; UI work 22-3 moves to
## the very end, after everything else reaches 100%)
- 22-1 TCP and container health policies with bounded retries (only HTTP exists) -> Health policies outcome. ~1 day.
- 22-2 Build: exercise secret mounts, resource/time limits, build-output redaction, build cleanup -> Build. ~1-1.5d.
- 22-3 UI outcome (owner 2026-11-16): (a) show the failure reason (e.g. container_unhealthy, deadline_exceeded +
  last probe) on the deployment page next to the failure class, not only under Logs; (b) overall UI rework to a more
  productive theme with fewer nested clicks (dashboard-style overview: environments, current release, last deploy
  and its reason, deploy/rollback/resolve actions inline); (c) live log streaming. Design first with the
  impeccable skill and pastel palette (CLAUDE.md), owner approves mock/design before code. ~2-3 days.

## Phase 3: observability
- 22-4 Metrics endpoint, structured-log audit, liveness next to /readyz, PostgreSQL + secrets backup/DR docs. ~1 day.

## Phase 4: needs a public host
(owner 2026-11-19: vendor-agnostic. Target = any VPS from any provider (generic Ubuntu LTS host with a public
IPv4/IPv6 and a DNS name), never a provider-specific API, image, or tool. Hetzner is just one possible test host.)
- 22-5 VPS packaging: adapt the deploy/host kit for a generic public VPS (no provider assumptions) -> Packaging.
- 22-6 ACME automatic HTTPS on a public VPS (HTTP-01/TLS-ALPN-01, no provider DNS plugin required) -> Traefik.

## Phase 5: final validation and launch
- 22-7 Full-codebase security/reliability audit (full-codebase-audit-and-remediate skill), failure-recovery and
  end-to-end on a public VPS, gate 12 (drain-before-downgrade doc), then flip executorEnabled/activationEnabled.

Expected: after Phase 2 -> 14/18 (~90% weighted); Phase 3 -> 15/18; Phases 4-5 -> 18/18.

## 22-1 design: additive health policies (2026-11-16, APPROVED: D1-D4 as recommended)
Owner scope decision: additive. ADR 0005:161 keeps the HTTP gate mandatory for every HTTPS route (every V1
environment has one); TCP/container checks add defense in depth, never replace it. No ADR change beyond a short note.

**Today:** HTTP gate constants in internal/runtime/docker/health.go:30-36 (3 successes, 2s interval, 2s probe
timeout, 60s deadline); container must stay running (State.Running/ExitCode/OOMKilled decoded); image HEALTHCHECK
status is NOT read, although ADR 0005:156 already says "explicit unhealthy status is immediate failure".
health_policy_revision CHECK = 1, frozen per attempt with path/host (000014, 000017); config_digest tag "3".

**Change (four slices, one subagent each, bar after each):**
- 22-1a migration 000028 + store + digest. Both environment_runtime_configs and agent_job_attempt_execution gain:
  health_success_threshold SMALLINT 1..10 (default 3), health_interval_seconds 1..30 (2),
  health_timeout_seconds 1..30 (2), health_deadline_seconds 10..600 (60), health_tcp_port INTEGER NULL 1..65535;
  cross CHECKs timeout <= interval and deadline >= threshold * interval. Live table keeps defaults; frozen table:
  ADD COLUMN ... DEFAULT (fills existing rows without firing the immutability trigger) then DROP DEFAULT.
  Freeze path (agent_lease_execution.go live read / frozen insert / frozen read / snapshot) carries them.
  config_digest: tag stays "3" and the new parts are appended ONLY when the policy differs from the defaults, so
  every existing environment's digest is byte-identical (ensureRelease compares digests; real acer rows exist).
  health_policy_revision stays 1.
- 22-1b web: config form fields with defaults and bounds; validator mirrors the CHECKs (internal/healthgate).
- 22-1c wire: lease/claim carry an optional health policy object only to agents that opt in
  (capability in the claim request, ClaimCleanups precedent; one fallback on 400). Non-opted-in responses
  byte-identical. A job whose frozen policy is non-default is NOT leased to a non-opted-in agent (D3).
- 22-1d agent gate: runHealthGate takes the policy (defaults when absent). Each round: HTTP probe, plus TCP dial to
  containerIP:health_tcp_port when set; both must succeed to count (D4); any failure resets the streak.
  ContainerInspect decodes State.Health: unhealthy -> immediate failure (reason container_unhealthy, ADR 0005:156);
  starting -> cannot pass yet, keep probing until deadline (D1); healthy or none -> no effect. New reasons classified
  permanent_configuration like deadline_exceeded. Blue/green, stop-then-start and rollback all pass the frozen
  policy of their own attempt.

**Decisions for owner:**
- D1 image HEALTHCHECK "starting": block passing until healthy (recommended; waits within the deadline) vs only
  "unhealthy" fails.
- D2 bounds as above (recommended) or different limits.
- D3 non-opted-in (older) agent and a non-default policy: do not lease (recommended; never silently weaken) vs lease
  with defaults.
- D4 TCP check counted together with HTTP in each round (recommended) vs a separate pass after HTTP succeeds.

**Tests (fail before, pass after):** migration bounds/cross-CHECKs/frozen backfill; digest of a default policy equals
the pre-change digest (golden computed from the current code) and every new field changes it when non-default;
web form bounds; wire opt-in (non-opted bytes unchanged, non-default policy withheld from non-opted agent);
agent loop: unhealthy -> immediate fail, starting -> waits then healthy passes, none ignored, TCP failure resets
streak, tuned threshold/interval/deadline honoured; STS/blue-green/rollback pass the policy.
**Bar:** gofmt, build, vet x2, make check, race unit, linux amd64 cross-build, fresh PG18 sweep, race x20 on
claim/lease/execution tests. Review: yes (migration + wire + agent). Estimate ~1.5 days.
**Acer validation:** set tcp port 80 + tuned deadline, deploy (pass); demo Dockerfile with a failing HEALTHCHECK
-> fast container_unhealthy failure (blue/green, so no reconciliation block).
**Risks:** digest drift (guarded by the golden test); version skew (opt-in + D3); downgrade: reset non-default
policies before installing an older build (gate 12 class, document in status doc).

## 22-2 design: build outcome (2026-11-16, APPROVED: D1-D5 as recommended)
Outcome text: "Docker/BuildKit build execution, secret mounts, resource/time limits, labels, log redaction, and
scoped cleanup." Investigation (orchestrator-verified):
- EXISTS, fake-docker tested only: buildx argv with --provenance=false --sbom=false, --label io.autodeploy.*,
  secrets as `--secret id=build-secret-N,env=VAR` (value only in the subprocess env, zeroed after), fail-closed buildx
  probe, three-layer redaction of build output (build.go RedactingWriter, logship, server sanitizer).
- MISSING: build time limit (build.go:545 bounded only by the lease ctx); build CPU/memory limits; build-scoped
  literal bindings (executor/secrets.go:147 "not delivered"); build cache / dangling image cleanup.
- BUG: any build error -> ErrBuildFailed -> permanent_configuration (classify.go:77), even a kill caused by job
  cancel or lease loss (should be transient/cancelled).
- GAP: secret ids are positional (build-secret-N) and shown nowhere, so a Dockerfile author cannot know them, and
  they may shift when bindings change unless the order is deterministic.

**Change (slices, one subagent each, bar after each):**
- 22-2a build time limit + classification fix: agent flag --build-timeout (default 30m, bounds 1m..2h, validated at
  startup) wraps the build in context.WithTimeout; kill the whole process group, cmd.WaitDelay. Our own deadline ->
  new ErrBuildTimeout -> permanent_configuration (the build is too slow for its limit); parent ctx cancel/lease loss
  -> transient (or the existing cancelled path), never permanent.
- 22-2b dedicated builder for limits and scoped cache: the agent ensures (idempotently, at startup, no root) a buildx
  builder "autodeploy-build" with the docker-container driver and caps from flags --build-memory (default 2GiB) and
  --build-cpus (default 2), and every build uses --builder autodeploy-build. Fail closed if it cannot be created or
  probed. Secrets transport unchanged (BuildKit session). Images still --load into dockerd.
- 22-2c scoped cleanup: after every build (success or failure) prune ONLY that builder's cache to a keep-storage bound
  (flag --build-cache-keep, default 5GiB): `buildx prune --builder autodeploy-build --max-used-space N --force`
  (flag present in buildx v0.31.1..v0.39; --keep-storage is a deprecated alias). NO image prune: the built image is
  itself an untagged image carrying our labels and is the artifact about to be deployed (found in 22-2c), so image
  removal stays with the 20E/21B cleaners (verified digest only). Never touches other builders. Errors are logged,
  never fail the deployment.
- 22-2d build-scoped literal bindings: delivered as `--build-arg NAME=value` (non-secret by classification; ADR 0003
  forbids ARG only for secrets); names checked against the reserved list; the config page notes that build literals
  are visible in image history.
- 22-2e secret id discoverability: build secret ids become deterministic (sorted by variable name, frozen order) and
  the config page shows "Dockerfile id: build-secret-N" next to each build-scoped secret binding (minimal text, no
  redesign; UI rework stays 22-3).

**Decisions for owner:** D1 limits via a dedicated capped builder (recommended; also makes cache cleanup scoped) vs
only documenting a daemon-level cap. D2 build timeout as an agent flag, default 30m (recommended) vs a per-environment
setting (migration). D3 build literals via --build-arg (recommended) vs refusing build-scoped literals. D4 cache bound
5GiB per host (flag). D5 deterministic secret ids shown on the config page (recommended) vs letting the id equal the
variable name (simpler for authors, but ADR 0003 asks for opaque ids).
**Tests:** fake-docker unit tests for each slice failing first (timeout kill + classification, parent cancel ->
transient, builder flags + fail-closed, prune argv scoped to the builder/label, --build-arg delivery and reserved
names, deterministic id order); existing build/redaction tests unchanged.
**Acer validation (one session, one sudo command per round):** build secret via RUN --mount (passes, value absent from
`docker history --no-trunc` and `docker save`), build output echoing the secret shows [REDACTED] in the Logs page,
labels on the image, a `RUN sleep` build hitting a short --build-timeout, a memory-hungry RUN hitting the cap, build
cache bounded after builds, a build literal reaching the build.
**Estimate:** ~1.5-2 days + acer session. Review: yes.

## 22-2f design: create/replace secrets from the terminal (2026-11-16, APPROVED: D1-D4 as recommended)
**Gap (found during the 22-2 acer prep):** no entry point creates a secret value. SecretRepository.Create/Replace and
secret.Service.CreateWithContext/ReplaceWithContext (encrypt with the active KEK, audit) exist and are tested, but
nothing outside tests calls them; cmd/secrets only does KEK rotation (init-active-kek, rewrap, can-retire). The
binding form asks for a "Secret head ID" nobody can obtain, so runtime and build secrets are unusable in practice.
**Change:** extend the existing terminal-only tool (same preflight: interactive TTY, root-run via autodeploy-run, same
config and key ring as rotation):
- `secrets secret put --environment ENV --name VAR --scope runtime|build` -> creates a secret head + version 1 for
  that environment, prints `secret head id: <id>` and the version number (never the value).
- `secrets secret replace --secret ID [--compromised]` -> new version on an existing head (ReplaceWithContext /
  ReplaceCompromisedWithContext); running deployments keep their frozen version.
- Owner/project are derived from the environment row (no hand-typed owner ids); unknown environment -> clear error.
- Value entry: hidden prompt, typed twice and compared; never argv, env or a file; bounded length; the buffer zeroed
  after use (best effort, as elsewhere). Refuse when stdin is not a terminal (D3).
- Audit: OperationContext with a generated audit id and actor "terminal:<unix user>"; no value in any log line.
- deploy/host/README: a short "add a secret" section (put -> paste the id into the binding form).
**Decisions for owner:** D1 terminal command now (recommended; matches the existing terminal-only secrets tool; a web
form joins the UI rework at the end) vs a web form now. D2 owner/project derived from the environment (recommended).
D3 interactive hidden prompt only, no piped input (recommended, no plaintext in files or shell history) vs also
accepting stdin for automation. D4 include `replace` and `--compromised` now (recommended).
**Tests (fail first):** argument parsing (good/bad), non-TTY refused, mismatched confirmation refused, put calls
CreateWithContext with the derived scope and never prints the value, replace/compromised paths, unknown environment;
integration: put then the binding form accepts the printed id and a claim freezes that version.
**Acer:** owner runs put for a build secret, binds it, then the deferred 22-2 checks (RUN --mount secret, absent from
`docker history --no-trunc` and `docker save`, [REDACTED] in Logs). Estimate ~0.5 day. Review: yes (secret handling).

## 22-4 design: observability outcome (2026-11-19, APPROVED: D1-D6 as recommended)
**Outcome text (tasks/todo.md:164):** metrics, structured sanitized logs, readiness/liveness endpoints, retention
cleanup, backup instructions, disaster-recovery documentation.

**Baseline (investigated 2026-11-19, spot-checked):**
- Liveness/readiness: control plane already serves unauthenticated GET `/livez` (process) and `/readyz` (DB ping;
  plus KEK ring reload + ledger/generation check when the secret subsystem is on, cmd/controlplane secretReadyHandler).
  Agent has no HTTP listener (outbound only); its liveness is visible only as `servers.last_heartbeat_at`.
- Retention cleanup: exists (deployment log purge scheduler, 20E release retention, 21B failed-candidate cleanup).
- Metrics: none; go.mod has 4 direct deps (pgx, x/crypto, x/sys, x/term), no prometheus/expvar/OTel.
- Logs: stdlib `log` text, no levels/fields. Control plane logger goes through crypto.RedactingWriter and logs
  fixed messages + failure site (error text could carry a DSN). Agent logger `log.New(stderr,...)` has NO redactor;
  ~53 Printf sites across agent client/executor/reporter/cleaner, docker builder, both mains. Stray direct stderr
  prints bypass the control-plane logger: internal/web/rollback.go:35,100 and manual_deploy.go:139 (`%v` err),
  web.go:655,706 (stage only), internal/agent/http/checkout.go:312,318,335 (`%v` err), docker/builder.go:68
  (`log.Printf`). logship.go:350,383 tee already-redacted lines (fine).
- Backup/DR: no scripts or docs. ADR 0003 "Backup, restore, and KEK recovery" sets the rules (DB backup holds only
  ciphertext; KEK ring backed up separately, never colocated; restore installs + validates KEKs before admin traffic
  or agent jobs, then a non-logging decrypt canary; restore and total-KEK-loss drills before production). MEMORY.md:
  every restore installs a FRESH active KEK id; an uncertain restored ledger makes the old KEK read-only. cmd/secrets
  has init-active-kek / rewrap / can-retire / put / replace, but NO decrypt canary.

**Slices (one subagent each, bar after each):**
- 22-4a stray-print cleanup: inject the control-plane logger into web.Handler and the agent HTTP handler; replace the
  8 direct stderr prints with logger lines carrying a fixed message + stage + error class (no `%v` of raw errors,
  matching main.go's failure-site rule); builder.go uses the injected logger. Test: capture logger output on each
  failure path, assert no DSN/token/secret fixture appears and the stage is present.
- 22-4b structured logs (D2): both binaries move to log/slog with a key=value text handler (journald-friendly),
  levels INFO/WARN/ERROR, stable fields `component`, `attempt_id`, `deployment_id`, `release_id`, `cleanup_id`,
  `failure_class`. Control plane handler writes through the existing RedactingWriter; agent handler gets one too,
  seeded with the agent credential token and per-attempt secret set where available, plus the logsanitize regex pass.
  Migrate the ~53 sites mechanically; message text kept so existing journal greps (`grep attempt`) still work.
- 22-4c metrics (D1, D3, D4): `/metrics` in Prometheus text format 0.0.4, hand-rolled (counters + gauges only, ~150
  lines + tests, no new dependency), on a SEPARATE listener `AUTODEPLOY_METRICS_LISTEN_ADDRESS`, empty = disabled
  (default), startup refuses a non-loopback address. Not routed by Traefik, no auth logic needed. Metrics:
  - process: `autodeploy_build_info{version}`, `autodeploy_up`.
  - in-process counters: `autodeploy_http_requests_total{surface=web|agent|webhook,code=2xx|3xx|4xx|5xx}`,
    `autodeploy_lease_sweep_runs_total{result}`, `autodeploy_leases_expired_total`,
    `autodeploy_log_purge_runs_total{result}`.
  - DB gauges, one read-only query batch at scrape with a 2s timeout, cached 15s (scrape storms cannot load the DB):
    `autodeploy_db_up`, `autodeploy_agent_jobs{state}`, `autodeploy_agent_heartbeat_age_seconds{server_id}` (agent
    liveness), `autodeploy_environments_reconciliation_required`, `autodeploy_release_operations_open`,
    `autodeploy_webhook_jobs{state}`, `autodeploy_releases{state}`.
  - Label rule: only bounded enums and server ids; never owner/project/environment names, hostnames, or repo names.
  Agent-side counters stay in the journal (D3); no agent listener.
- 22-4d decrypt canary (D5): `secrets secret verify` decrypts every live secret version with the current ring,
  discards plaintext immediately, prints only `checked=N failed=M` plus failing version ids, exit non-zero on any
  failure; read-only, no audit write, never prints values or lengths. Required by ADR 0003 restore step.
- 22-4e backup/DR docs + script (D6): `docs/operations/backup-restore.md` and `deploy/host/backup-db.sh`
  (`docker exec autodeploy-postgres pg_dump -Fc` to a 0600 file under a root-only dir; encryption/off-host copy left
  to the operator per ADR 0003). Doc covers: what to back up (DB dump; KEK files + manifest separately; the rest of
  /etc/autodeploy/controlplane keys; agent credential is reissued, not restored), retention/KEK-retire coupling,
  restore order (stop agent + control plane -> restore DB -> install KEKs -> fresh active KEK id via init-active-kek
  -> old KEK kept for decrypt only (no read-only flag exists in code today: the manifest's active id
  is the only write key, so 22-4e confirms init-active-kek is sufficient or raises it) -> `secrets secret verify` -> /readyz -> start control plane -> start agent), what a restored
  DB older than the agent's pending activation records does (reconciliation block, operator resolve), total-KEK-loss
  procedure (secrets unrecoverable: replace every secret value, deployments without secrets keep running), Traefik
  route files and app containers are reconstructed by the next deploy. Also the gate 12 drain-before-downgrade
  note (closes gate 12 on paper; Phase 5 keeps the real-host check).

**Decisions for owner:**
- D1 metrics library: hand-rolled text format, no new dependency (recommended; we need ~10 series) vs
  prometheus/client_golang (standard, adds a sizeable dependency tree).
- D2 log format: slog key=value text (recommended; journald + grep friendly) vs slog JSON (better for a log shipper
  later) vs keep stdlib log and only fix the stray prints (cheapest; "structured" not met).
- D3 agent metrics: derive agent liveness and job state from the DB on the control plane (recommended; no new agent
  listener or attack surface) vs a loopback /metrics listener on the agent too.
- D4 exposure: separate loopback-only listener, disabled by default (recommended) vs `/metrics` on the public
  listener behind a bearer token file.
- D5 add `secrets secret verify` now (recommended; ADR 0003 makes it part of every restore) vs document a manual
  canary.
- D6 ship a backup script + a restore drill on the acer (recommended; ADR 0003 requires a tested restore before
  production) vs docs only.

**Tests (fail first):** 22-4a/b log capture per failure path asserting redaction (DSN, bearer, enrollment token,
secret fixture) and required fields; 22-4c exposition format golden test, counters increment on real handler paths,
gauge query against PG18 (integration), cache honoured, scrape timeout returns `autodeploy_db_up 0` not a hang,
non-loopback address refused, listener absent when unset; 22-4d integration: put two secrets -> verify ok; corrupt
one ciphertext -> verify fails naming that id, output never contains the value; 22-4e shellcheck + a mac drill
(dump PG18 container, drop DB, restore, verify passes).
**Acer validation:** enable the metrics listener, `curl 127.0.0.1:<port>/metrics` during a deploy (heartbeat age,
job states move); `journalctl -u autodeploy-agent` shows key=value lines; full backup -> restore drill per the doc
on the acer (one owner sudo round per step), `secrets secret verify` passes, a deploy succeeds after restore.
**Risks:** slog migration touches many agent files (mechanical, but review every redaction seam; journal greps in
the acer recipe change shape -> update the recipe in memory); restore drill on the acer touches the live DB (take a
dump first, drill on a scratch database name before the real one); cardinality creep (label rule above, enforced
by a test listing allowed label names).
**Estimate:** ~1.5 days mac + one acer session. Review: yes (logging redaction, secret canary, new listener).
**Outcome effect:** +1 outcome (observability) after acer proof, independent of the build outcome.

## 22-D design: ADR 0005 stop-then-start auto-finalize (2026-11-19, owner chose auto-finalize; APPROVED 2026-11-19: D1 A, D2 probe, D3 exclude rollback)
**Today:** STS candidate failure before any route write (agent beforeSwitch: stop predecessor -> start candidate ->
gate fails -> stop candidate -> restart predecessor) is reported as plain `succeeded=false` + free text; tx2
(agent_lease_repository.go ~2626) calls blockActivationForReconciliation unconditionally -> environment blocked until
an operator resolves not_activated (acer attempt 19). ADR 0005 :274-278 wants: stop candidate, restart AND
health-check prior release, finalize failed while prior stays active; ambiguity is never auto-resolved (:226, :248).
Gaps found: (1) the agent never health-checks the restarted predecessor (only re-inspects Running); (2) trigger
000021 forbids switch_authorized -> failed ("tx2 never reports a definite failure"); (3) strict decoding both sides.
**Change:**
- Agent (sts.go/activation.go/state): a `not_switched` outcome ONLY when all hold: failure inside beforeSwitch
  (switchRoute never entered), predecessor existed and stop was sent, candidate confirmed stopped, predecessor
  restart ok AND a bounded predecessor health probe passes (reuse the env health policy; new), route-revision file
  below the authorized revision, on-disk route digest readable and equal to a pre-stop snapshot recorded in the
  pending record. Everything else (switch failures, restart/probe failure, stop mismatch, already-stopped/gone
  predecessor, persist failures, crash-recovered records, rollback/retained jobs) keeps today's plain failure ->
  block. New omitempty record field, immutable once result_ready.
- Wire: capability opt-in at claim (health_policy precedent, claim.go), server echoes support only to opted-in
  claims (non-opted-in bytes pinned by golden test); agent sends `outcome:"not_switched"` only when the lease said
  supported; reporter strips the field and retries once as a plain failure on 400 invalid_request (server
  downgrade). Activation-result decoder accepts the new field.
- Control plane (branch at the !succeeded site, after occupantChecked): ALL must hold under existing locks/order:
  op switch_authorized; occupant/fence/not cancelled; expected_active_release_id non-nil; frozen strategy
  stop_then_start; not a rollback job; deployment activating; route_heads FOR UPDATE: revision ==
  expected_route_revision, active_release_id == expected_active_release_id, not flagged; predecessor release active.
  Then, in ONE transaction (option A, no migration): blockActivationForReconciliation then the resolveNotActivated
  writes (deployment failed, op failed, attempt closed, slot freed + unflagged, job failed, head/releases untouched,
  events). Any failed condition or deterministic error in the branch -> today's block (never reject, never success);
  transient -> rollback + agent retry; savepoint like the success path; commit failure -> existing fresh-tx block.
  Replay of not_switched on a failed op answered like the success replay (check reporter delete rules).
- Docs: ADR 0005 (predecessor health probe, auto-finalize conditions), ADR 0004 amendment (tx2 may finalize a
  definite not-switched failure; otherwise unchanged).
**Decisions for owner:** D1 option A (block+resolve in one tx, no migration; reuses tested resolveNotActivated;
noisier events) (recommended) vs B (migration 000029 allowing switch_authorized -> failed + new event). D2 require
a bounded predecessor health probe before claiming not_switched (recommended; ADR 0005 step 2; otherwise
auto-finalize could hide an outage) vs Running-only. D3 rollback/retained STS jobs excluded in V1 (recommended).
**Tests:** agent outcome matrix (positive: gate fail/start fail/gate error + restart ok + probe ok; negative: each
exclusion), record round-trip + old-record byte pin, reporter 400 fallback; http strict shapes + claim golden;
store post-state matrix, negative matrix (each -> block), replay, races (Complete, sweeper, cancel, ResolveActivation)
at -race -count=20, atomicity via inject hooks, lock order, new Activate possible afterwards; acer: rerun the
attempt-19 scenario -> deployment failed, environment not blocked, predecessor serving.
**Estimate:** ~1.5-2 days + reviews (revised up from 2-3h: wire opt-in, predecessor probe, concurrency suite).

## 22-8 design: re-enroll an existing server after a restore (2026-11-19, APPROVED: D1 (a) token, D2 leave open + fencing, D3 no --replace)
**Gap (found in 22-4e):** nothing lets an already-enrolled server get a new credential. server_enrollment_tokens
has PK server_id and a frozen consumed row (000012:82-110: "an already enrolled server can never receive a fresh
enrollment token"); IssueEnrollment refuses consumed servers (server_repository.go:222); Enroll inserts credential
version 1 only (:289); RotateCredential (:334) has no caller (agent generates its own credential; ADR 0001 :141-146
says operator rotation is impossible and revoke-then-re-enroll is no substitute). After a restore where the agent
holds a credential the DB doesn't know, the agent logs "agent credential rejected" every heartbeat forever and the
only path is revoke + new server + rebind every environment (orphans queued jobs on the old id, strands open
release_cleanups, forces reconciliation on every environment with an open attempt).
**Change (option a):**
- Migration 000029: server_enrollment_tokens gets a surrogate id PK + `purpose` ('enroll'|'reenroll'); a partial
  unique index keeps at most one UNCONSUMED token per server; consumed rows stay immutable; existing rows migrate as
  purpose='enroll'. Audit action `reenrollment_issued` added to the server_audit_events CHECK. Append-only/no-delete
  triggers kept.
- Store: `IssueReenrollment(ctx, principal, serverID, reason)` (active server only, owner-scoped, single-use token,
  same TTL cap as enrollment, digest only, audit with reason). `Enroll` consumes either purpose: for 'reenroll' it
  revokes the active credential and inserts MAX(version)+1 in the same transaction (reuses RotateCredential logic),
  audits `enrolled` + `credential_rotated` (actor agent). Lock order per ADR 0004 (owner -> server -> credential).
- Web: POST /servers/reenroll (session + CSRF + reason + confirm=reenroll, mirroring /servers/revoke), token shown
  once via the existing renderEnrollmentToken. No UI redesign (22-3 stays last).
- Agent: unchanged; operator removes the stale credential file then runs `agent enroll` with the new token (the
  os.Link create-if-absent refusal stays).
- Docs: ADR 0001 amendment (re-enrollment token), backup-restore.md re-enroll steps, deploy/host/README.md.
**Decisions for owner:** D1 option (a) re-enrollment token (recommended) vs (b) revoke + new server + rebind (no
code, heavy, loses continuity). D2 open attempts on re-enroll: leave open, fence-checked (recommended; ADR 0004
:583 already keeps attempts across credential changes; the restored DB is the authority) vs revoke them
(forces reconciliation). D3 no `agent enroll --replace` now (recommended; manual file removal is documented) vs add it.
**Tests (fail first):** migration on a DB with existing consumed tokens (rows preserved, constraints hold);
issue refused for revoked/foreign-owner servers and while an unconsumed token exists; reenroll consume -> old
credential rejected, new version N+1 authenticates, heartbeat works; token single-use + expiry; concurrent consume
(one wins); audit rows; web route CSRF/confirm/reason; enroll purpose='enroll' behaviour unchanged.
**Side finding (separate, small):** upsertBinding (github_repository.go:484) repoints github_project_bindings.
server_id without checking the target server exists/is active/same owner — verify and add a guard.
**Estimate:** ~0.5-1 day + review (security-relevant: credentials, migration).

## Progress
- 2026-11-16: roadmap written; 22-1 investigation started.
- 2026-11-16: 22-1a DONE (uncommitted): migration 000028 (5 columns both tables, named CHECKs + cross CHECKs,
  frozen ADD COLUMN DEFAULT then DROP DEFAULT, real backfill test), internal/healthgate/policy.go (Policy, defaults,
  bounds, WithDefaults, IsDefault, Valid), store read/insert/update + freeze path, computeConfigDigest appends "hp1"
  parts only for non-default (golden digests from old code still match). Zero means default (0 invalid for every
  field). 12 raw test INSERTs into the frozen table gained 3,2,2,60. Bar clean; fresh sweep clean.
- Pre-existing (not 22-1a, reproduced on HEAD 22fe8c7): 7 store tests use fixed secret-head ids and fail on
  -count>=2 (secret_heads_pkey 23505). Race x20 for 22-1a run with -skip for those 7. Follow-up: make them
  re-runnable (unique ids) in a small test-only slice.
- 2026-11-16: 22-1b DONE (web form: 5 fields, ranges from healthgate, strict 17-field form, missing field -> 400).
  22-1c DONE (capability "health_policy"; lease health_policy object only for opted-in agents; non-opted bytes
  golden-identical; tryClaim guard before the first write skips non-default policy jobs for non-opted agents,
  covering attempt 1, retries and rollbacks). 22-1c must never ship without 22-1d. Follow-up: skipped jobs are
  rediscovered each claim (5 candidates per lease) -> could crowd out others with 5+ stuck jobs on an old agent.
- 22-1d Stage 0 found: STS gate runs inside dctx = a.timeout + 8m; after the gate beforeSwitch needs probe+verify+30s;
  max safe gate ~390s (default flags), 300s at minimum --activation-timeout -> a 600s deadline could be cut off.
  No server-side activation expiry (lease renews every 30s). Owner decision D5: grow the STS window per job:
  stsExtra = 8m + max(0, deadline - 60s); default policy byte-identical; 10..600 range kept.
- 2026-11-16: 22-1d DONE: gate honours the frozen policy (policyTimings from healthgate defaults; State.Health:
  unhealthy -> container_unhealthy immediate fail, starting blocks passing, none ignored; andProbes HTTP+TCP per
  round); STS window = 8m + max(0, deadline-60s) (D5); blue/green, STS and rollback pass lease.HealthPolicy.
  Review PASS WITH NOTES -> fixed: agent refused a lease without health_policy only when the capability was
  declared (was fail-open to defaults); 2 client fixtures now send the policy like the real server. Confirmed:
  State.Health is a value struct (no nil panic); summaries bounded by boundSummary (2048); blue/green 600s gate on
  renewed lease ctx. Bar: gofmt, build, vet x2, make check, linux amd64/arm64, race ./... ; fresh PG18 sweep clean;
  22-1a race x20 (skip 7 pre-existing fixed-id tests) clean. Low follow-ups: skipped-job crowding; cosmetic
  trailing "; " in LastProbe when HTTP errors first. NEXT: commit, then acer validation (tcp port 80 + tuned
  deadline pass; failing HEALTHCHECK image fast fail with blue/green).
- ACER 2026-11-16 (build 1818b54, install-app.sh applied 000028; helpers byte-identical; previous build kept as
  ~/autodeploy-stage/bin-gated.prev-e0f2319): attempt 21 STS with tuned policy (deadline 90, tcp 80) passed and
  switched. Attempt 22 blue/green with tcp port 81: gate 20:10:10 -> 20:11:40 (exactly the tuned 90s) failed
  deadline_exceeded/permanent_configuration, last_probe="status=200; tcp error: dial tcp 172.29.0.129:81: connect:
  connection refused" -> TCP check per round and tuned deadline PROVEN; candidate stopped, no activation, site 200.
  Pending: image HEALTHCHECK unhealthy/starting on a real host (needs a Dockerfile HEALTHCHECK in the demo repo).
- ACER 2026-11-16 attempt 23 (blue/green, demo Dockerfile HEALTHCHECK exit 1): gate 20:16:26 -> 20:16:30 failed
  container_unhealthy/permanent_configuration with last_probe="status=200; tcp ok" -> image HEALTHCHECK PROVEN.
  Health-policies outcome ticked (12/18, weighted ~84%). Owner asked: failure reason on the deployment page + overall
  productive UI rework with fewer nested clicks -> folded into 22-3.
- 2026-11-16: owner order: 22-2 build next; 22-3 UI last (after 100% of the rest). 22-2 investigation started.
- 2026-11-16: 22-2b implemented (uncommitted): internal/runtime/docker/builder.go (ensureBuilder: ls json -> match driver+exact
  driver-opts else rm+create, then inspect --bootstrap; fail closed ErrBuilderUnavailable), BuildkitImage pinned moby/buildkit
  v0.35.0 @ index digest sha256:aa632b92... (GitHub latest release + `buildx imagetools inspect`), memory-swap=memory (hard cap),
  cpu-quota=cpus*100000/period 100000, BUILDX_CONFIG=<state-dir>/buildx, --builder autodeploy-build on every build,
  flags --build-memory (512MiB..32GiB, def 2GiB) --build-cpus (1..32, def 2). ProbeBuildx now takes BuilderConfig. No host kit change.
- 22-2a DONE (uncommitted): --build-timeout (30m, 1m..2h); build in WithTimeout; process-group SIGKILL + WaitDelay
  10s (unix); ErrBuildTimeout -> permanent_configuration; parent cancel -> ErrBuildInterrupted -> transient (also for
  a cancel during probeBuildx, which used to be permanent); timeout reason line in the build log.
- 22-2b DONE (uncommitted): builder "autodeploy-build" (docker-container driver, moby/buildkit:v0.35.0@sha256:
  aa632b92...52bb), caps from --build-memory (2GiB, 512MiB..32GiB, memory-swap = memory) and --build-cpus (2, 1..32);
  BUILDX_CONFIG=<state>/buildx; drift -> rm + create (only that name); fail closed (ErrBuilderUnavailable); every
  build uses --builder autodeploy-build. Caps verified on the mac via docker inspect. Acer CLI plugin order: /usr/lib
  (Docker Desktop buildx v0.31.1) shadows /usr/libexec (CE v0.34.0) -> agent likely uses v0.31.1. Follow-up: a builder
  removed after agent start fails builds until restart (no per-build re-ensure).
- 22-2c DONE (uncommitted): --build-cache-keep (5GiB, 1GiB..500GiB); after every build (success/failure/timeout/
  cancel) prune only autodeploy-build to --max-used-space on WithoutCancel ctx with its own 2m timeout; error -> fixed
  phase line "build: cache cleanup failed (the deployment is not affected)", raw error to the journal only. Design
  correction: no dangling-image prune (would delete the just-built release image). Verified on the mac (cache 33MB
  -> 0 with a 10MB cap); acer runs buildx v0.31.1 (flag present in its source).
- 22-2d DONE (uncommitted): build-scoped literals -> `--build-arg NAME=value` (sorted) via BuildRequest.Literals; refused
  (ErrInvalidBuildLiteral, wraps ErrInvalidBuildRequest -> permanent_configuration, phase line names the variable, never
  the value): invalid/duplicate name, reserved list, BUILDKIT_*/BUILDX_*, proxy and platform built-in args
  (case-insensitive), name also bound as build secret. Runtime literals/secrets unchanged. Config page hint line added.
- 2026-11-16: 22-2d DONE (build literals -> sorted --build-arg; reserved/built-in names and secret/literal clashes
  refused). 22-2e DONE (internal/buildsecret.ID: build-secret-N by byte-order name index; shown on the config page;
  duplicate names refused). Review PASS WITH NOTES -> fix round: builder lost after start -> inspect, re-ensure,
  ErrBuilderUnavailable (transient, classified); drift recreation reason logged; flaky timeout test fixed (2s, no
  t.Fatal in goroutine, -race -count=5 stable); BUILDKIT_/BUILDX_/DOCKER_ prefixes refused for build secret names;
  no cache prune when the build never started; buildx config dir tightened to 0700. Bar: gofmt, build, vet x3 tag
  sets, make check, race ./..., linux amd64/arm64, windows vet; fresh PG18 sweep clean. NEXT: commit, acer validation.
- 2026-11-16: 22-2f DONE (uncommitted): `secrets secret put --environment ENV --name VAR --scope runtime|build` and
  `secrets secret replace --secret ID [--compromised]` in cmd/secrets via secret.Service (active KEK, audited);
  owner/project from agent_jobs for the environment (ambiguous -> refused; unknown until the first deploy); hidden
  prompt twice on /dev/tty, piped stdin refused before any DB access; value bounded by crypto.MaxPlaintextBytes,
  constant-time compare, cleared; output = head id + version only. Review PASS WITH NOTES -> fixed: leak assertions
  on every failure path, terminal echo restored on all returns and on SIGINT/SIGTERM (seam-tested), ambiguous
  environment message, usage sentinel, README note that --name is not stored. Follow-ups: audit actor is
  terminal:autodeploy-controlplane under autodeploy-run (env -i drops SUDO_USER); very long pasted values may hit
  terminal line limits.
- END OF DAY 2026-11-16: origin/main 369175a. Acer runs build 14bb581 (installed 21:54; 22-2 code, builder caps not
  yet confirmed: owner did not paste the docker inspect line). Build 369175a (adds secrets put/replace) is STAGED in
  ~/autodeploy-stage/bin-gated, NOT installed; backups bin-gated.prev-{20261116,e0f2319,1818b54,14bb581}.
  RESUME (2026-11-17): (1) 24h check: rt-19 gone (`sudo docker ps -a`) and Releases table shows it removed;
  (2) Round 1: install 369175a + `autodeploy-run secrets secret put --environment uat --name AUTODEPLOY_BUILD_TOKEN
  --scope build` with throwaway value acer-test-secret-7f3a9c; confirm hidden prompt + terminal restored; get the
  head id; (3) Round 2: bind it (expect "Dockerfile id: build-secret-0"), add build literal, demo Dockerfile with
  RUN --mount=type=secret,id=build-secret-0 (+ a cat of it for [REDACTED]) and ARG for the literal; deploy; one
  sudo command: builder caps (docker inspect buildx_buildkit_autodeploy-build0), `docker history --no-trunc` and
  `docker save | grep -c` for the value (expect 0), buildx du under the cap; (4) build timeout and memory-cap checks
  need agent flag overrides (systemd drop-in, owner sudo) -> plan them; (5) crash-recovery test; then tick the build
  outcome (13/18) and move to Phase 3 (observability). UI (22-3) stays last.
- 2026-11-19: 22-4 design APPROVED (D1-D6 as recommended). 22-4a DONE (uncommitted; owner: commit 22-4a+22-4b
  together): 8 direct stderr prints in internal/web + internal/agent/http now go through the injected redacting
  logger as fixed message + stage + errclass token (internal/errclass: context_canceled, deadline_exceeded,
  postgres_<SQLSTATE>, postgres, other; typed-nil safe); nil logger discards; internal/testutil/srcguard AST guard
  (os.Stderr/Stdout, log.Print*/Fatal*/Panic*, print/println) in both packages; agent builder Logf wired explicitly.
  Review PASS WITH NOTES -> fixed (vacuous checkout tests reworked, discard assertions, AST guard, typed-nil).
  Orchestrator mutation checks: error text re-added -> 3 checkout tests fail; os.Stderr re-added -> guard fails.
  Limits: lease-start step always class=other (githubapp flattens to ErrCheckoutStale); builder.go:281 logs buildx
  error text to the agent journal (covered by 22-4b agent redaction).
- 2026-11-19: 22-4b split: (1) internal/logging + control plane, (2) agent sites. Finding: crypto.RedactingWriter
  streams and holds back maxPattern-1 bytes while values are loaded -> a long-lived writer under slog would delay
  each line's tail; design = per-record redaction (format record, fresh writer Write+Close, regex pass, one Write
  under mutex). 22-4b-1 in progress.
- 2026-11-19: 22-4b DONE (uncommitted, with 22-4a). internal/logging: slog key=value, no time attr (journald),
  INFO; per-record redaction = exact values + base64 + slog-escaped forms (derived via slog's own TextHandler) via a
  fresh RedactingWriter, then internal/logsanitize regex pass (moved out of agenthttp, cap kept there), one Write
  under a mutex, fixed drop line on failure. Control plane: fixed msg + stage/class attrs, SetDefault at start of
  run(), ErrorLog shares the process logger. Agent: ~70 sites moved; NewAgent redacts the credential token;
  logging.Err allowed agent-side only; logship tees + build output through the logger (per-attempt redaction still
  upstream); log.Default fallbacks removed (nil -> discard). srcguard import-aware (renamed/dot imports, fmt.Print*,
  log setters, os.NewFile, syscall.Stderr) on 7 packages. Reviews: 22-4b-1 PASS WITH NOTES (HIGH escaped-secret gap
  fixed, ErrorLog, guard, SetDefault order); final whole-change review PASS WITH NOTES (no critical/high; empty
  build-output chunks now skipped). Mutation checks: token values nil -> redaction test fails; escaping disabled ->
  8 subtests fail; Close removed -> holdback tests fail. Windows vet fails at HEAD already (internal/gitcheckout),
  pre-existing. Acer journal greps change shape (attempt_id=N) -> update acer recipe after install.
- 2026-11-19: 22-4c DONE (uncommitted). internal/metrics (hand-rolled 0.0.4 exposition, label allow-list, GaugeSet =
  one collect per WriteText, Cache with 15s success / 5s failure TTL + single in-flight); MetricsSnapshot = one
  UNION ALL in a READ ONLY tx with SET LOCAL statement_timeout sent as its own statement first (batched, pgx
  prepares before the SET and a migration lock hung it; test: ACCESS EXCLUSIVE -> postgres_57014 fast); state lists
  mirrored from CHECKs + integration test vs pg_get_constraintdef; webhook gauge pending-only (partial index);
  never-heartbeated/revoked servers omitted. AUTODEPLOY_METRICS_LISTEN_ADDRESS loopback IP literal only (4in6 ok),
  GET/HEAD /metrics only, bind failure fails startup, later Serve failure only logs (optional endpoint never kills
  the control plane). Review PASS WITH NOTES -> fixed (non-fatal metrics Serve, one snapshot per scrape, failure
  TTL, panic-path + 4in6 tests). Owner-side decision (orchestrator): no indexes/migration for agent_jobs/releases
  GROUP BY scans (single-host scale; document limit + alert on db_up in 22-4e). Mutation: loopback check removed ->
  config table test fails.
- 2026-11-19: 22-4d DONE (uncommitted). `secrets secret verify`: generation check before any read; pages
  secret_versions state active|superseded (revoked excluded: materialization refuses it at SQL level; can-retire
  still counts revoked) by id in READ ONLY txs; real crypto.Decrypt + production cryptoScope; plaintext cleared;
  no ledger/audit/row writes (integration snapshots identical); output counts + `failed version= kek= reason=`
  (kek_missing|kek_invalid|unwrap_failed|decrypt_failed|envelope_invalid), unsafe ids %q-quoted, failure list capped
  at 10000; no TTY preflight for verify only. crypto.Decrypt failures now also match stage markers (stageError, same
  text; only the verifier branches on them). Run with writers stopped (not one snapshot). Review PASS WITH NOTES ->
  fixed (kek_invalid, snapshot caveat, cap test, id quoting). Mutation: decrypt errors ignored -> verifier test
  fails. Follow-up (pre-existing): rewrap has no generation-binding check (only init-active-kek does).
- 2026-11-19: 22-4e DONE (uncommitted). docs/operations/backup-restore.md (what to back up; KEK set separate per
  ADR 0003; restore order: stop agent+CP -> rename live DB (record name) -> pg_restore into fresh DB (failure ->
  drop partial, rename back) -> KEKs+manifest (controlplane-github.env required) -> fresh active KEK + init-active-kek
  -> `secret verify` failed=0 with writers stopped -> CP + /readyz -> agent; total KEK loss; drill checklist;
  downgrade rule = gate 12 closed on paper: forward-only migrations, drain before older build), observability.md,
  deploy/host/backup-db.sh (pg_dump -Fc via docker exec, 0700/0600, temp+mv, TOC check; sha256 recording mandatory).
  install-app.sh: AUTODEPLOY_METRICS_LISTEN_ADDRESS tunable (was a commented line lost on every regenerate),
  validated (loopback literal, octets/leading zeros, port<=65535, 8080 refused). Mac drill: dump/restore row counts
  match (54 tables). Accuracy review PASS WITH NOTES -> fixed. README backups section + IMPLEMENTATION_STATUS refresh
  (86% weighted, gates 14/15). Unproven until acer drill: restored-DB-older-than-agent-state behaviour, KEK restore
  steps end to end, `secret put` + rebind guidance for heads without an active version.
  NEXT: commit 22-4 (all slices) -> acer: install, metrics during a deploy, journal format (update acer recipe greps
  to attempt_id=N), backup/restore drill on a scratch DB, then tick observability.
- 2026-11-19: owner: 22-5/22-6 are vendor-agnostic (any provider's public VPS; no provider APIs/images/DNS
  plugins); README/status wording updated, ADR 0001 left as historical context.
- 2026-11-19: small fixes DONE (uncommitted): (1) Rotator.RewrapBatch/CanRetire refuse ErrGenerationMismatch before
  any repository access (manifest generation vs secret_database_generation; same fixed CLI text as verify);
  (2) enable-github.sh --kek-id (default kek-1 byte-identical; validKeyID rule; refuses before App key/webhook/KEK/
  manifest writes if an existing manifest's active id differs; whitespace-tolerant id read; warns on orphan KEK
  file); runbook total-KEK-loss uses it with prerequisites; (3) audit actor `terminal:<operator>(as <user>)` from
  autodeploy-run AUTODEPLOY_OPERATOR (SUDO_USER -> logname -> root, strict regex else unknown; set after env-file
  vars so it can't be overridden). Review PASS WITH NOTES -> fixed. Mutation-style: generation compare disabled ->
  integration rewrap succeeded (test fails).
- 2026-11-19: 22-D + 22-8 designs APPROVED (22-8 D2: leave attempts open, fencing). 22-8 DONE mac-side (uncommitted):
  migration 000029 (surrogate id, purpose enroll|reenroll, one-unconsumed + one-enroll partial unique indexes,
  reenroll insert trigger: active + already enrolled + born unconsumed; audit action reenrollment_issued);
  IssueReenrollment (owner/active first, refuses live pending token, replaces expired), Enroll consumes either
  purpose (reenroll: revoke active + MAX+1 via shared replaceActiveCredential, audits enrolled+credential_rotated);
  POST /servers/reenroll (CSRF, provenance, reason, confirm=reenroll, token shown once); BindRepository target
  server guard (exists, active, same owner; FOR SHARE after binding upsert). Integration sweep green, race x20
  green, 33 mutations killed, make check green. Review PASS WITH NOTES; lock order verified by hand (no path
  holds servers then bindings except compatible SHARE locks); runbook fixed (stop agent -> rm -> enroll -> start);
  ADR 0001 bearer-token risk note. Unproven until acer: real agent enroll after a restore.
- 2026-11-20: 22-8 committed 894a48e. 22-D DONE mac-side (committed with this entry). Control plane: claim capability
  activation_not_switched, lease echo only for opted-in STS non-rollback leases (non-opted bytes golden-pinned);
  activation-result `outcome:"not_switched"` (only succeeded=false, no observed digest); finalizeNotSwitched
  (agent_lease_not_switched.go): savepoint -> conditions (expected active set, not rollback/cancel, frozen STS,
  deployment activating, route head unflagged at expected revision/active, candidate ready, predecessor active) ->
  blockActivationForReconciliation + resolveNotActivated in one tx (no migration); any doubt/deterministic error ->
  today's block; transient -> retry; commit failure -> fresh-tx block. Opt-in NOT persisted per attempt (needs a
  migration); trust note in ADR 0004 (control plane cannot see host routes; agent trusted for host state as for success).
  Agent: 11-fact proof (echo, not retained, predecessor, stop sent+confirmed, candidate stopped, restart started,
  pre-stop route digest snapshot, bounded predecessor probe via same RunHealthGate/policy on detached ctx, revision
  file below authorized, on-disk digest == snapshot); recovered records never claim; record fields omitempty
  (old bytes pinned); reporter 400 -> durable DowngradeNotSwitched + one plain retry; claim ladder both -> health_policy
  -> none. Verified: integration sweeps, race x20 (new + existing suites), agent -race x3, make check, linux build;
  two independent reviews PASS (0 HIGH/MEDIUM); mutations killed both sides; L1/L2 pin tests added.
  Implementer wrote agent code before tests (mutation runs substitute for fail-first).
  Known limits: predecessor probe uses the new lease's port/path (mismatch -> plain failure, safe); stop-unknown never
  eligible; older agent binary cannot read records with the new fields (drain before downgrade).
  NEXT: acer session (install, rerun attempt-19 STS failure scenario -> deployment failed, env not blocked,
  predecessor serving; re-enroll drill; plus earlier list), then 22-7 audit, 22-5/22-6 vendor-agnostic VPS, 22-3 UI last.
- 2026-11-20: acer unreachable (host down) -> mac-side first (owner). Rewrap generation follow-up already closed (d02edb3,
  rotation.go:58). 22-7 audit run: 5 parallel audits, no P0/P1; 13 findings + decisions D1-D3 in tasks/22-7-audit.md
  (key: DB superuser role, container hardening, checkout cleanup no retry, diagnose.sh /var/tmp as root, optional
  checksum, uninstall destroys KEKs, missing release_operations indexes, no DB timeouts). Awaiting owner approval.
- 2026-11-20: 22-7 mac-side DONE: 13 audit findings + 2 review rounds fixed (round 1 FAIL: admin migrated as runtime
  role -> CheckCurrent; round 2 PASS WITH NOTES). Owner: D1 role split, D2 compatible caps + PidsLimit 4096, D3 purge
  90d job/route events only (webhook deliveries kept: replay protection), HSTS 1 day. Migration 000030. Full bar green.
  Follow-ups + acer checklist in tasks/22-7-audit.md. RULE-01 (no file deletion without double confirm) adopted.

## 22-5 + 22-6 design: generic public VPS + ACME (2026-11-20, APPROVED: D1-D5 as recommended)
**Baseline (investigated, spot-checked):** kit is LAN-only: CP host default autodeploy.autodeploy.test (install-app.sh:71),
private-ca.sh wildcard cert via traefik/autodeploy-tls.yml, /etc/hosts 127.0.0.1 block, TRAEFIK_BIND=127.0.0.1, subnet
172.29.0.0/24 with no host-route collision check, preflight-app.sh CA/hosts/--resolve checks, traefik unit
AssertPathExists tls/server.*. traefik.yml has NO certificatesResolvers. Renderer ALREADY emits
tls.certResolver "letsencrypt-production" (render.go:170, defaults.go:25; observer requires it, client.go ErrTLSMismatch);
LAN works because an undefined resolver falls back to the file cert. CP config (PublicOrigin, trusted proxy) host-agnostic.
Kit never enables ufw; 5432/8080/metrics/CP port are loopback/bridge-bound. Docker is assumed preinstalled; binaries only
via build-acer-binaries.sh (gate-flipped). Agent probe uses system roots -> LE staging certs never pass the probe.

**Design:**
- `--mode lan|public` on install-app.sh / preflight-app.sh / uninstall.sh, persisted AUTODEPLOY_TLS_MODE in host.env.
  lan = default, generated files byte-identical to today (pinned by a test). In-place lan->public switch unsupported
  (fresh install documented).
- public: no private CA / tls file; CP host required (real FQDN, no default); TRAEFIK_BIND default 0.0.0.0;
  traefik/traefik-public.yml = same entrypoints/providers + certificatesResolvers.letsencrypt-production.acme
  (storage /etc/traefik/acme/acme.json, challenge per D1, caServer per D2, email per D3). Unit: rw mount
  /var/lib/autodeploy/traefik/acme (0700 dir, acme.json 0600, NOT under agent-writable dynamic/), tls mount +
  AssertPathExists mode-conditional. CP router (install-app.sh heredoc) gets certResolver in public mode.
  No Go change: per-app routes already request the resolver (no golden/digest churn).
- Keep a managed /etc/hosts line CP_HOST -> 127.0.0.1 in public mode (no CA) so agent->CP never depends on hairpin NAT.
- Preflight public: DNS A/AAAA of CP host == host public IP (from `ip route get`; no external what-is-my-ip call),
  nothing else on 80/443, outbound to ACME directory, acme.json 0600, served cert issuer != Traefik default;
  subnet collision check vs `ip -4 route` + docker networks before network creation (both modes, fail with env var hint).
- Firewall per D4. acme.json backup note in docs/operations/backup-restore.md (holds ACME account key).
- Packaging per D5: docs for Docker Engine + buildx + required apt packages (apache2-utils, openssl, curl, xfsprogs),
  preflight for /dev/loop-control + free disk; generic build script name (new build-host-binaries.sh wrapping existing
  logic; old script kept); x86_64 + Ubuntu 24.04 stay hard requirements.

**Decisions:** D1 challenge: HTTP-01 default, TLS-ALPN-01 opt-in flag (rec). D2 `--acme-staging` for manual issuance dry
run only (probe can't trust staging) (rec). D3 ACME email optional + warning (rec). D4 firewall: opt-in
`--configure-firewall` (ufw 22/80/443, explicit SSH port confirm) (rec) vs docs only. D5 include packaging docs + generic
build script in 22-5 (rec).
**Risks:** LE rate limits (5 dup certs/week) -> back up acme.json; first issuance inside 3m probe window; one DNS record
per app host (no wildcard without DNS-01); HTTP-01 vs entrypoint redirect + undefined-resolver behaviour unverified live;
Docker ports bypass ufw; provider blocks on :80.
**Test plan:** Mac: bash -n/shellcheck, lan-mode byte-identical generated files test, traefik-public.yml validated by the
pinned Traefik image under Docker (`traefik --configFile` startup, no ports), CIDR-collision helper tests, Go suite
unchanged. VPS (later, any provider): DNS + 80/443 reachability, staging then prod issuance for CP + one app host, redirect
interplay, acme.json persistence/renewal, ufw+Docker, full deploy e2e (22-7 gate).
**Slices:** 22-5a mode switch + public config + unit/router + hosts line; 22-5b preflight (DNS, ports, subnet, loop/disk);
22-5c firewall + packaging docs + build script; 22-6 resolver config/challenge flags/staging + backup doc + validation.
- 2026-11-20: 22-5 + 22-6 mac-side DONE. install-app.sh --mode lan|public (lan byte-identical + AUTODEPLOY_TLS_MODE=lan);
  public: FQDN required, Traefik resolver letsencrypt-production (renderer already requested it: no Go change),
  HTTP-01 default / tls-alpn opt-in, --acme-staging, optional --acme-email, opt-in --configure-firewall (SSH ports =
  sshd -T U ss U $SSH_CONNECTION; die before any ufw change if session port undetected), acme.json 0700/0600 rw mount,
  CP /etc/hosts 127.0.0.1 block, refuses lan<->public switch and leftover autodeploy-tls.yml. netcheck.sh subnet
  collision; preflight public checks (all DNS addrs match, chain+hostname verify, CP via Traefik, ACME reachability,
  loop/disk/buildx). build-host-binaries.sh --gates enabled|disabled; docs/operations/vps-install.md. Reviews: round 1
  FAIL (orphan heredoc body aborted install-app.sh step 5 in both modes; bash -n + render-only test missed it ->
  new script_structure_test executes the installer in a container + heredoc scan), round 2 PASS WITH NOTES, LOWs fixed.
  Shell tests: netcheck 45, firewall_ssh 19, preflight_public 17, script_structure, lan_mode_identical all green.
  Unproven until a public VPS: real issuance (staging then prod), HTTP-01 vs entrypoint redirect, renewal, DNS, ufw +
  Docker, first-issuance vs 3m probe. deploy/host/uninstall.sh.bak0 = stray untracked backup, owner: keep, never commit.

## 22-D2 design: probe the restarted predecessor with ITS OWN health target (2026-11-20, APPROVED)
Acer attempt 27: STS candidate failed (health path changed to /autodeploy-missin), predecessor restarted and serving 200,
but probePredecessor (sts.go ~529-560) used the NEW lease's host/path/port/scheme/policy -> 404 -> no not_switched ->
environment blocked. Owner: probe with the target the predecessor was deployed with; fail closed when unknown.
- Source: control plane (authority). Health target is frozen per attempt in agent_job_attempt_execution (000017 path/host,
  000028 tuning). Agent state and Docker labels carry no health target (labels closed set, digest only).
- Wire: Activate request gains optional `predecessor_health: true`, sent only when lease.ActivationNotSwitched; response
  gains `predecessor_health {host,path,port,scheme,policy_revision,policy}` only when requested (non-opted bytes golden-
  pinned unchanged). Server lookup = one read-only SELECT inside Activate after the route_heads lock (and on replay,
  same answer): expected_active_release_id -> its WINNING activation attempt (same predicate as 21B retained identity)
  -> frozen execution row. Absent/ambiguous -> no block. No new locks/writes, lock order unchanged, no migration.
- Agent: strict decode; missing/partial/invalid block or server rejecting the flag -> "target unavailable" -> no probe ->
  plain failure (today's block), never a deploy error or retry loop. New fact predecessorTargetKnown in notSwitchedFacts.
  Probe uses predecessor host/path/port/scheme/policy and its deadline for the bound. Kept in memory (crash recovery never
  claims not_switched).
- Tests: agent (lease path wrong + predecessor path right -> not_switched; mutation: revert to lease fields -> fails;
  absent -> no probe; fact veto table); wire golden both directions + strict decoder + client malformed/partial; store
  integration (older revision + different path returns predecessor's values not current env config, replay, rollback-
  release predecessor, missing row, lock-order race suite unchanged). Then acer rerun of section C.
- 2026-11-20: 22-D2 DONE mac-side: Activate request opt-in `predecessor_health:true` (sent only for ActivationNotSwitched
  leases) -> response block with the predecessor's winning attempt's frozen health target (read in a savepoint after the
  route_heads lock; any read error -> no block, outer tx intact). Agent probes the restarted predecessor with it; absent/
  invalid/older-server -> fail closed (operator block). Wiring pinned at compile time (cmd/controlplane/wiring_test.go);
  real handler <-> real client contract test. Review PASS WITH NOTES, LOWs fixed. make check, integration sweep, agent
  race x3 green. Acer: rerun section C (health path change on STS) -> expect deployment failed, env NOT blocked.
- 2026-11-20 acer (4eba3b3 then 4bb1bb5, install-app.sh lan upgrade): PROVEN runtime role split (control plane connects as
  autodeploy_app; TRUNCATE/DISABLE TRIGGER/session_replication_role/CREATE/COPY PROGRAM refused), restart marker cleared,
  agent ProtectSystem=strict (deploy + route write + drain OK), empty CapabilityBoundingSet on postgres/traefik (postgres
  restarted on new unit), metrics on 127.0.0.1:9101, preflight 64/0/1, container hardening on a real deploy (rt-26:
  no-new-privileges, PidsLimit 4096, 5 caps dropped). 22-D: attempt 27 (4eba3b3) fell back to block (probe used new path)
  -> 22-D2; attempt 30 (4bb1bb5) STS + wrong path: predecessor probe passed with its own path, not_switched, deployment
  failed without operator, env not blocked, demo 200. Anomaly: attempt 28 checkout git fetch exit 125 (one-off; 29/30 OK)
  -> diagnose.sh if it recurs. Installer upgrade output still prints fresh-install next steps (cosmetic follow-up).
  NEXT acer: re-enroll drill, backup/restore drill on scratch DB, crash-recovery test, 24h retention check.
- 2026-11-20 acer re-enroll drill (22-8) PROVEN: UI reenrollment_issued -> agent stopped, old credential MOVED aside
  (credential.pre-reenroll-20261120, owner-approved RULE-01 x2) -> agent enroll -> credential v1 revoked, v2 active in one
  tx, audit reenrollment_issued + enrolled(2) + credential_rotated(2); heartbeat resumed; attempt 31 blue/green deploy OK
  with the re-enrolled agent. Old-credential refusal shown via DB revocation (live presentation not exercised).
  Remaining acer: backup/restore drill (scratch DB), crash-recovery (kill agent mid-deploy), 24h rt retention check.
- 2026-11-20 acer retention (20E/21B) PROVEN: 11-16 failed candidates (incl. attempt 19) not removed at ~6h, removed at
  first pass after boot 11-18 (cleanups 14-17); successful releases beyond keep-2 removed (18, 19 at 16:07/16:17, 20 =
  rt-25 at 16:31:55, 10 min after agent start); today's failed candidates (27, 28, 30) kept <24h. Pending: 24h removal on a
  continuously running host (rt-27 ~13:12, rt-30 ~16:12 on 11-21) via `ssh -t autodeploy-laptop 'sudo bash
  ~/autodeploy-stage/retention.sh'`.
- 2026-11-20 acer crash test K1 (SIGKILL agent during build, attempt 32): systemd restarted agent in 5s; an operator
  Cancel from the UI arrived at 16:39:00 (only writer of cancel_requested is POST /deployments/cancel); no ack possible
  (new process has no in-memory lease) -> lease capped 60s -> expired -> closeAttemptOrdinary flags slot
  reconciliation_required (ADR 0004: expiry/cancel always reconciles, phase-independent). Owner: keep design; follow-ups:
  (1) narrow the block to attempts that reached a host-mutating phase (ADR amendment + predicate), (2) restarted agent
  should log attempts it lost / persist a minimal attempt-started record to ack or fail cleanly. Clear via Servers page
  resolve (attempt 32, no_activation). K2 (kill after activation authorized) still to run.
