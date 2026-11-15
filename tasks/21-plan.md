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
