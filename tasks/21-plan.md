# Step 21 — mac-side work while the acer is idle: plan, 2026-11-15 (awaiting approval)

Baseline: origin/main = 88261f3. Gates false in committed code. 20E installed on acer, unvalidated.
Hard gates 5-10, 13, 15 already closed (step 16/17); open: 3 (real Traefik observation, acer), 4, 11 (closed by
20B, doc stale), 12 (downgrade note), 14.

## Sub-steps (order; one subagent per step, review only where logic/SQL/locks change)
- **21A 20F docs + recount.** tasks/todo.md outcome checklist (lines 145-159) is stale: tick only what acer
  evidence covers clause by clause (candidates: checkout, build, runtime container, Traefik route/HTTPS/switch).
  Health policies stay open (HTTP only; no TCP/container policy exists). Blue/green/STS/retention/rollback stays
  open until 20E validates. Status doc: new count + weighted %, gates list (11 closed by 20B), close step 20 code
  items. Docs only.
- **21B failed-candidate 24h removal** (ADR 0005 / B-D5 default). Extend the 20E cleaner + claim/result fence to
  failed releases older than 24h; same rules (exact labels, no force, unverified image kept). Likely migration
  000027 (`failed -> removing` edge or failed-cleanup row). Introspect live schema before SQL. Review: yes.
- **21C cleanup state on the route page.** Show retained/removing/removed + last cleanup result from
  `release_cleanups`. Existing CSS/layout only (no new visual design). Review: light.
- **21D ReserveCheckout / revalidateTx / RevalidateCheckoutBinding lock order** (19 follow-up): takes
  `FOR UPDATE OF b,r,i,a` without the registration-first prefix -> latent cycle against the SHARE chain. Reorder to
  the documented lock order; add a contention test that forces real lock contention (not query-text match).
  Review: yes; concurrency suite `-race -count=20`.
- **21E flagged-head trigger** (gate 14): `validate_route_head_update` refuses a hostname change while
  `reconciliation_required`; migration; test the trigger directly. Review: light.
- **21F gate 4 re-check** (orchestrator, analysis): two resolution paths (`ResolveActivation`,
  `ReconcileActivation`) once activate ops exist — prove they cannot both resolve one op differently (lock order /
  CAS on op state), or add the guard. Result recorded in status doc gates list.

## 21B design (draft 2026-11-15, awaiting owner approval)
Finding: failed-candidate removal is NOT implemented (ADR 0005:327-328, ADR 0004:926-928 say follow-up; cleanup
eligibility is `state='retained'` only, cleanup.go:124-129). Code never writes `releases.state='failed'`: a failed
deployment's release stays `candidate` (failed before activation authorization; image_digest NULL) or `ready`
(failed after; digest set). No release_runtime_instances rows exist; container id = attempt id.
- **D1 (rec.)** reuse the existing legal edge `candidate|ready -> removing` (000018). No release-state migration, no
  loosening of the `failed` terminal check, no new failure writes. Alternative (rejected): start writing `failed` and
  add `failed -> removing` = wider behaviour change across every failure path.
- Eligibility (server, under the slot lock, same claim/result endpoints and fence as 20E, same lock order):
  release state candidate|ready; deployment terminal failed (or cancelled, D3) and terminal >= 24h ago; no job of the
  deployment queued/leased/retry_wait; no non-terminal release_operations for the release; release never activated
  (no succeeded activate op; not any route head's active_release_id); slot idle + not reconciliation_required; route
  head not flagged; claimed only by the server that ran the attempts. Retained eligibility query untouched.
- Claim payload: kind=failed_candidate, release id, and the list of the deployment's attempt ids (each may have left
  a stopped container). Agent: per attempt, exact-label lookup (managed, kind, environment-id, release-id,
  runtime-instance-id), guardedStop, remove force=false; a missing container is fine.
- **D2 image** - `ready` (digest in DB): remove image only if no other non-removed release on the server has the same
  digest, and Docker non-force refuses in-use (kept=true). `candidate` (no digest in DB; common case = health gate
  failed): **rec. keep the image** (ADR 0005:116-120 requires a DB reference check, impossible without a digest);
  images then wait for a later build-cache/image prune step. Alternative: trust the container's own label digest.
- **D3** include cancelled deployments (ADR 0005:50 lists cancellation cleanup) - rec. yes, same rules.
- Migration: likely none for state; maybe 000027 only if release_cleanups needs a `kind` column (decide after
  introspecting). Existing test pinning `failed -> removing` refusal stays valid.
- Not covered (note only): failed earlier attempts of a deployment that later SUCCEEDED leave stopped containers
  that neither rule removes.
- Tests: PG integration for every eligibility exclusion (each must fail if its predicate is deleted), agent cleaner
  tests per attempt list, all 20E tests unchanged and green; full bar + concurrency suite.

**Owner decisions 2026-11-15:** D1 reuse `candidate|ready -> removing`; D2 keep image when DB has no digest; D3
include cancelled deployments. Split: **21B-1** control plane (eligibility, claim payload, tests; agent untouched),
**21B-2** agent cleaner (per-attempt removal; empty claim digest = identity by release-id + runtime-instance-id
labels, image always kept). Each: implement -> full bar -> independent review -> owner sees diff -> commit.

## 21D design (2026-11-16, APPROVED: D1 keep FOR UPDATE, D2 add StartCheckout cross-check)
**Premise correction.** b,r,i,a are GitHub tables (project_bindings, repositories, installations,
app_registrations id=1), not the server-credential chain. The relevant order is ADR 0004 (manual deploy section):
registration -> admin session -> installation -> repository -> binding -> then dependents (checkout reservations
last: DisableInstallation/ReconcileInstallation invalidate them after b).

**Today (github_repository.go):** one statement `... FOR UPDATE OF b,r,i,a` in RevalidateCheckoutBinding (:86),
ReserveCheckout (:118), revalidateTx (:213). Rowmarks lock in range-table order b,r,i,a, so registration is LAST.
BeginMintAttempt (:145) and StartCheckout (:187) also lock the reservation row FOR UPDATE BEFORE revalidateTx.
Every other writer/locker takes registration first: activeRegistration FOR UPDATE (Disable, Reconcile, Bind,
CompleteHandoff, CreateSetupState, ReserveCallback), lockLifecycleAuthority/lockGitHubAuthorityChain FOR SHARE.

**Cycles (verified against code; Postgres resolves each with 40P01 after deadlock_timeout, which the checkout
paths swallow into ErrGitHubDenied -> spurious checkout/build failure, or a failed webhook/admin action):**
- A: webhook push/manual deploy holds a SHARE, wants i SHARE; checkout holds b,r,i FOR UPDATE, wants a FOR UPDATE.
- B: Disable/Reconcile/Bind hold a FOR UPDATE, want i/r/b; checkout holds b,r,i, wants a.
- C: BeginMintAttempt/StartCheckout hold reservation, want b,r,i,a; Disable/Reconcile hold a,i,r,b, want the
  reservation (invalidatedCheckoutIDs).
- Not a cycle: checkout vs checkout (same order).

**Change (one shared helper, separate statements so order is by construction, not planner-dependent):**
1. `lockCheckoutAuthority(ctx, tx, binding)` : `SELECT .. github_app_registrations id=1 AND active FOR UPDATE`
   -> installation FOR UPDATE (active, not suspended) -> repository FOR UPDATE (active) -> binding FOR UPDATE
   (active), each scoped by owner; returns the three revisions (+ clone_url for Reserve). Lock modes unchanged
   (see D1). Any lock/no-row error -> ErrGitHubDenied exactly as today.
2. ReserveCheckout: helper keyed by (owner, project) -> expire + insert reservation (already last).
3. revalidateTx -> helper + revision compare. RevalidateCheckoutBinding -> own tx + helper (no prod caller;
   interface method kept, not deleted).
4. BeginMintAttempt: unlocked read of the reservation to learn the binding -> helper -> reservation
   `FOR UPDATE` re-read with `state='reserved' AND expires_at>now AND attempts<1` AND binding/revision columns equal
   to the unlocked read -> update. StartCheckout: helper with passed b -> reservation FOR UPDATE + state check ->
   update. ActivateMintAttempt: already authority-then-reservation; only switches to the helper.
5. ADR 0004: add the checkout paths to the GitHub order paragraph.

**Decisions for owner:**
- D1 lock modes: keep FOR UPDATE on registration (recommended: pure reorder, zero behavior change; registration
  acts as a mutex so the i/r/b sub-order cannot matter while it is held) vs FOR SHARE (matches webhook chain, lets
  checkouts run concurrently; separate-statement i->r->b order keeps it deadlock-free; a behavior change ->
  separate slice).
- D2 StartCheckout does not cross-check the reservation's own binding/revision columns against the passed b
  (ActivateMintAttempt does). Add the same predicate to the reservation re-lock? Recommended yes (tightening only;
  internal caller always passes the matching b).

**Tests (fail before, pass after; hold the real conflicting lock, detect blocking via pg_blocking_pids, not
query text):** table-driven over Reserve, BeginMint, ActivateMint, StartCheckout, RevalidateCheckoutBinding:
tx1 holds registration FOR SHARE (webhook-chain stand-in); run the op in a goroutine; wait until
`tx1pid = ANY(pg_blocking_pids(pid))` for some backend; probe from tx3 with NOWAIT: installation, repository,
binding and (mint/start) reservation rows are lockable FOR UPDATE -> the op holds nothing below registration while
waiting. Today the probes fail with 55P03. Release tx1 -> op succeeds. Plus a variant with tx1 = DisableInstallation
-style FOR UPDATE on registration -> after release the op is denied (revision bumped), no 40P01. New tests at
-race -count=20. Reuse webhook_job_repository_test helpers (assertPgSQLState) and qaCheckoutBinding.

**Bar:** gofmt, build, vet both tag sets, make check, `go test -race ./...`, fresh PG18 integration sweep, activation
concurrency suite `-race -count=20`. Independent review: yes (locks). Estimate 2-3h.
**Risks:** an unlocked reservation read then re-lock adds a window: closed by the equality re-check under lock.
Helper must not be called by a tx that already holds a reservation (comment + test via StartCheckout/BeginMint).

## 21E design (2026-11-16, APPROVED: D1 = OLD only)
**Gate 14:** app refuses a hostname change on a flagged head (AssignRouteHostname, route_head_repository.go:~306,
ErrRouteHeadReconciliationRequired, under FOR UPDATE); the trigger `validate_route_head_update` (000018 + 000020
narrow exemption) does not. Trigger today already refuses hostname change when OLD.active_release_id IS NOT NULL,
so the gap is only a flagged head with no active release (an ambiguous FIRST activation).
**Every production route_heads UPDATE (introspected):** AssignRouteHostname (hostname + revision+1, only when not
flagged and no active release); activation success agent_lease_repository.go:~2068 (active_release_id + revision+1 +
clear flag, hostname unchanged); flag set :~2971 and flag clear activation_resolution.go:~405 (flag only). None
changes hostname while or when touching the flag -> the new rule refuses nothing production does today.
**Change:** migration 000027 `CREATE OR REPLACE FUNCTION validate_route_head_update()` = the live body (copied from
introspection, not reasoned) + one rule:
`IF OLD.reconciliation_required AND NEW.hostname <> OLD.hostname THEN RAISE
EXCEPTION 'route head hostname cannot change while reconciliation is required'`.
D1: OLD OR NEW (recommended: also refuses setting the flag and changing hostname in one statement, which nothing does
and would be meaningless) vs OLD only (exact mirror of the app check).
Trigger binding unchanged (BEFORE UPDATE). Forward-only; older binaries never issue such an UPDATE (gate 12 safe).
**Tests (migrations/000027_*_test.go, direct SQL against the trigger, existing 000018/000020 test helpers):**
flagged head, no active release: hostname change refused (even with revision+1); clear flag + change hostname in one
statement refused; flag set/clear alone without revision bump still allowed;
unflagged hostname change with revision+1 still allowed; existing active-release refusal unchanged. Each refusal
test fails before the migration. Run the 000018/000020/000021 migration tests + full store sweep (activation and
resolution paths write route_heads).
**Bar:** gofmt, build, vet x2, make check, race unit, fresh PG18 sweep; race x20 on route/activation/resolution tests
(GenuinelyRaces|Races|ConcurrentAssign|TestAssignRouteHostnameRefused|ResolveActivation|ReconcileActivation).
Review: light. Estimate 1-1.5h.

## 21G design (2026-11-16, APPROVED: D1 keep latest + require equality, D2 409 reload message)
**Hazard (21F):** the operator resolve form posts (environment_id, outcome, reason) only. ResolveActivation locks the
slot and resolves WHATEVER attempt is pending. A stale form for attempt A (lost response, back/refresh) submitted
after a newer attempt B blocked resolves B with A's outcome (e.g. "activated" when the host never switched).
The replay path (nothing pending) is already safe: confirmResolvedOutcome is anchored to the environment's latest
attempt by design (TestResolveActivationReplayIsAnchoredToLatestAttempt: a duplicate for A after B was claimed is
Denied; an old resolution never reports success for a call that did nothing). Keep that.
**Change:**
1. Form (templates/servers.html): hidden `attempt_id` = the row's AttemptID (already rendered in the table; fed by
   ListReconcilingEnvironments a.id = the pending attempt). No query/view change.
2. Handler environmentsResolve: exactly 5 fields; attempt_id parsed int64 > 0, else badRequest. An old open form
   without attempt_id -> 400 (reload).
3. Store ResolveActivation(ctx, principal, environmentID, expectedAttemptID, outcome, reason): expectedAttemptID <= 0
   -> ErrAgentLeaseInvalidInput up front. Pending path: right after the slot lock, pending != expected -> new
   `ErrActivationResolveStale`, before any other lock, nothing written (code convention: refusals write nothing).
   Replay path: keep latest-attempt anchoring AND require latest == expected (D1); mismatch -> ErrServerAuthorizationDenied
   as today.
4. web resolveError: ErrActivationResolveStale -> 409 "this form is for a different activation than the one now
   blocking the environment; reload the servers page and review it".
5. Signature ripple: web interface + cmd/controlplane adapter + 2 test fakes; ~30 test call sites (fixture helper
   a.resolve passes a.attemptID; harness h.resolve posts attempt_id; BlockedEnvironmentForTest gains AttemptID).
   Agent ReconcileActivation unchanged (already attempt-anchored).
**Decisions:** D1 replay: keep latest anchoring + require equality (recommended: zero behaviour change, existing
replay tests keep their assertions) vs anchor replay to the expected attempt (duplicate for an older resolved attempt
would then return success; rewrites TestResolveActivationReplayIsAnchoredToLatestAttempt). D2: 409 + reload message.
**Tests (fail before, pass after):** store: resolve A, claim B, block B (plain-block expire path), replay with A's id
-> ErrActivationResolveStale, slot still pending on B, resolveState/audit/events unchanged; positive control with B's
id succeeds. expected <= 0 -> invalid input. Web end-to-end: stale form 409 + nothing written; form missing attempt_id
-> 400; rendered servers page contains the hidden attempt_id. Existing resolve/reconcile/replay suites unchanged in
assertions (only call sites pass the id).
**Bar:** gofmt, build, vet x2, make check, race unit, fresh PG18 sweep; race x20 on ResolveActivation|ReconcileActivation|
ResolveForm. Review: yes (resolution path). Estimate 1.5-2h.

## Out of scope here — each needs its own plan + owner call
Health TCP/container policies; observability (metrics, structured logs audit, backup/DR docs; readiness already
exists); Hetzner packaging; UI gaps (variables, domains); server-side claim idempotency, register dedupe.

## Then one acer session (batched, one diagnose run)
Install 21B/21E build, 4th deploy -> 20E cleanup removes oldest retained, force a failed candidate (24h cutoff
overridable? decide in 21B: no test-only knob in prod; validate by DB timestamp check instead), crash-recovery
check, environment-variable injection check (set a literal binding, `docker inspect` the container), gates decision.

## Verification
Per step: gofmt, build, vet both tag sets, `go test -race ./...`; steps with SQL/migrations: PG18 fresh-db sweeps;
21D: concurrency suite. Commit per step (no Co-Authored-By per CLAUDE.md; verify message bodies).

## Risks
- 21B touches the cleanup fence: reuse 20E's fence, do not add a second path.
- 21D lock reorder can deadlock if wrong: contention test must hold the real conflicting lock.
- 21A over-claiming outcomes: tick only with a cited acer attempt per clause.

## Progress
- 21A DONE 2026-11-15 (docs only): evidence per clause gathered; only checkout + runtime container tickable
  (already counted in 10/18, checklist was stale); env-var injection lacks acer evidence -> acer session. .gitignore
  now ignores /admin-credentials.txt.
- 21B-1 DONE 2026-11-15 (uncommitted -> commit pending owner OK): control-plane failed-candidate claim behind
  `kinds:["failed_candidate"]` opt-in (non-opted-in bytes unchanged); failed|cancelled|superseded (owner added
  superseded) >= 24h; claim-time never-live guard; attempts limited to claiming server; no migration. Review PASS WITH
  NOTES -> one fix round (guard, peek EXISTS(attempt), disjointness + rollback-job tests). Bar: gofmt/build/vet x2/
  make check/race clean; integration sweep 3/3 fresh PG18.
- 21B-2 DONE 2026-11-15 (commit pending owner OK): agent opts in with kinds (one fallback on 400 invalid_request);
  per-id exact name+label match, stop on mismatch/error, non-forced removal, image only with verified digest; unknown
  kind refused. Review PASS WITH NOTES -> fix (vanished-mid-stop keeps image) + branch tests. Bar clean (race, vet x2,
  make check, linux amd64/arm64). Not validated on a real host yet -> acer session.
- 21C DONE 2026-11-16: route page "Releases" table (10 newest, state + latest cleanup via newCleanupView), read-only
  RR tx with session + owner check, omitted on read failure; no new CSS. Bar clean + PG18 integration. Not yet seen in a
  browser -> acer session. NEXT (2026-11-17): 21D investigation + design for owner approval; consider a deliberate
  failing deploy on the acer early so the 24h failed-candidate window has elapsed by the main acer session.
- 21D DONE 2026-11-16 (commit pending owner OK): lockCheckoutAuthority locks registration -> installation ->
  repository -> binding FOR UPDATE as separate statements; reservation row always last (BeginMint: unlocked read,
  authority, re-lock with full equality). revalidateTx denies zero/empty ids (orchestrator caught a fail-open the
  helper's id-learning introduced; test proved it). Review PASS WITH NOTES -> fixes: StartCheckout requires
  reservation owner/project (D2), ActivateMintAttempt requires owner/project/installation/repository (owner OK,
  pre-existing gap), test cleanup releases blocker before join, fixture ids from b, probes fail on 0 rows.
  Every new test failed on old code (55P03 probes / err=nil). Bar: gofmt, build, vet x2, make check, race unit;
  fresh PG18 sweep x2 (store ~272s); concurrency suite race x20 (1058s); final LockOrder|GitHub|Checkout|
  ManualDeploy|WebhookJobRepository race x20 (688s). Not exercised on acer (no checkout path change visible
  there beyond normal deploys). Known: rebind racing ReserveCheckout now denies (safe); 40P01/conn errors still
  surface as ErrGitHubDenied (pre-existing).
- 21E DONE 2026-11-16 (commit with 21D): migration 000027 replaces validate_route_head_update with the introspected
  live body + `IF OLD.reconciliation_required AND NEW.hostname <> OLD.hostname THEN RAISE` (body diff = 3 added lines;
  binding unchanged). Refusal tests failed before the migration. The rule broke 3 existing tests that forced a hostname
  move on a flagged head in one UPDATE (state unreachable via the app since 16h): owner chose clear flag -> move
  (revision+1) -> re-flag, each asserted 1 row, helper forceRouteMovedWhileFlagged; stale comments fixed. Light review
  PASS WITH NOTES -> confirmed all 4 production route_heads UPDATEs unaffected, allowed cases assert 1 row, OLD-only
  edge pinned. Bar: gofmt, build, vet x2, make check, race unit; fresh PG18 sweep; migrations 77 PASS; route/
  activation/resolution race x20 (2070s). Gate 14 closed in status doc. NEXT: 21F gate 4 analysis, then acer session.
- 21F DONE 2026-11-16 (analysis, verified by orchestrator): gate 4 SAFE (slot FOR UPDATE in both paths
  activation_resolution.go:143/:560, under-lock re-check in resolveDeclared, typed replay/conflict, 000021 closed-op
  trigger, cross-path race test both orders x 4 outcome pairs). Found: operator ResolveActivation takes only
  (environment, outcome); replay anchored to latest attempt -> stale resubmit can resolve a newer blocked activation.
  Owner: fix now as 21G.
- 21G DONE 2026-11-16 (commit pending owner OK): resolve form carries hidden attempt_id; handler requires exactly 5
  fields, attempt_id int64 > 0 (parsed after session/CSRF); ResolveActivation(expectedAttemptID): <= 0 invalid input;
  pending path: owner check, THEN pending != expected -> ErrActivationResolveStale (409 reload), nothing written;
  replay path: latest-attempt anchoring kept AND latest == expected. Orchestrator moved the stale check after the owner
  check (design placed it before: foreign owner could learn another owner's environment has a blocked activation);
  test failed with the old order. Stale-form test failed on old code. Only existing test touched beyond the id
  argument: foreign slot flag now names the corrupt attempt (still asserts CorruptState, no writes). Review PASS WITH
  NOTES. Deliberate: a stale form when nothing is pending gets the generic 400 (replay refusals reveal nothing); a
  double-click straddling a new claim gets 400 (safe). ParseInt accepts "+5"/"007" (numeric compare, harmless).
