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

## Phase 2: mac-side code closing outcomes
- 22-1 TCP and container health policies with bounded retries (only HTTP exists) -> Health policies outcome. ~1 day.
- 22-2 Build: exercise secret mounts, resource/time limits, build-output redaction, build cleanup -> Build. ~1-1.5d.
- 22-3 Live log streaming on the deployment page -> UI. ~0.5-1 day.

## Phase 3: observability
- 22-4 Metrics endpoint, structured-log audit, liveness next to /readyz, PostgreSQL + secrets backup/DR docs. ~1 day.

## Phase 4: needs a public host
- 22-5 Hetzner packaging (adapt deploy/host kit) -> Packaging.
- 22-6 ACME automatic HTTPS on Hetzner -> Traefik.

## Phase 5: final validation and launch
- 22-7 Full-codebase security/reliability audit (full-codebase-audit-and-remediate skill), failure-recovery and
  end-to-end on Hetzner, gate 12 (drain-before-downgrade doc), then flip executorEnabled/activationEnabled.

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
