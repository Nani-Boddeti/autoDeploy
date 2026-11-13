# Step 20 — UI fixes + slice 11: plan, 2026-11-13, approved 2026-11-13

Research trace (file:line) in the session report; key findings kept here. Gates stay false in committed code.
One implementer + one review per sub-step, atomic commits.

## Key findings
1. The agent never reads `release_strategy` (executor.go:159-345 hard-codes blue/green).
2. Server `Activate` (agent_lease_repository.go:1213) does not depend on the health gate -> stop-then-start
   needs no migration (report `health_checking` before Activate to satisfy the state machine).
3. Any failure on a `switch_authorized` op -> `reconciliation_required`; operator declares (D-B kept).
4. 000018 already allows `retained -> active` (rollback); not `retained -> removing`.
5. Activation machinery is keyed to `kind='activate'` -> rollback reuses `kind='activate'`.
6. `release_runtime_instances` never written; runtime instance id is derived (derivePredecessor).
7. **Failed candidate containers are never stopped** (unless-stopped restarts them) — real leak on the acer.
8. No maintenance service/route exists (ADR 0005 STS prerequisite).
9. `agent_jobs.job_kind` CHECK = 'deployment_execution' -> rollback needs a new kind + deployment row.
10. Control plane cannot see the agent executor gate; banner can only reflect `activationEnabled`.

## Sub-steps (order)
- **20A-1** nav links to route/config pages (projects, deployment detail, servers reconciling table, cross-links).
- **20A-2** banner shown only when `!ActivationEnabled` (web.Config flag); reword; fix logs empty-state text.
- **20A-3** Retry = form on a failed deployment's detail page posting the existing `/deployments/manual` with
  mode=sha, the same commit, a fresh nonce; shown only when the deployment's environment = current binding.
- **20E-0** stop (never remove) failed candidate containers after any post-start failure (guarded helper).
- **20B** server-side drain/stop-failure record: migration 000024 `release_drain_results` (immutable, one row per
  activation attempt), `POST /agent/v1/drains/result` (credential-only, derived authority, idempotent), agent drain
  record gains `report_pending`; detail/route page show the result.
- **20C** stop-then-start: branch in Execute on `lease.ReleaseStrategy`; build while old serves -> Activate ->
  persist record (`PredecessorStopSent`) -> guarded stop + wait -> start + health gate -> switch/verify/report.
  Failure after stop: restart predecessor best-effort, report failure, environment blocks for operator. Recovery
  restarts the predecessor if `PredecessorStopSent`. No maintenance page in V1 (502/503 during the outage).
- **20D** rollback (R1: restart the retained container): migration 000025 (job_kind 'rollback_execution',
  `agent_jobs.rollback_target_release_id`, immutable `rollback_intents`), `EnqueueRollback` (nonce idempotent,
  in-flight 409), claim freezes the target's config with live strategy/drain, Activate/finalize accept a `retained`
  candidate, source deployment -> `rolled_back`; agent rollback branch (no checkout/build; start, gate, activate;
  stop again on failure); UI button with confirmation. Full activation race suite re-run.
- **20E** retention/cleanup (after rollback is proven on the acer): migration 000026 (`retained -> removing` edge,
  `release_cleanups`), agent-initiated `cleanup/claim` + `cleanup/result` with DB-state fence, agent Cleaner with
  exact-label, no-force container/image removal.
- **20F** ADR 0005/0004 amendments, status/todo, real-host validation on the acer in order: links/banner, retry,
  stop record, STS, rollback, retention.

## Owner decisions — ALL APPROVED AS RECOMMENDED (2026-11-13)
- A-D1 banner [show only when activation gate is off] · A-D2 retry target [current binding only]
- B-D1 stop failure blocks? [visibility only] · B-D2 STS without maintenance page [yes for V1]
- B-D3 failure after predecessor stop [restart it, report failure, env blocks for operator]
- D-R1 rollback [restart retained container now; recreate-from-image later] · D-R2 rollback strategy/drain [live env]
- D-R3 rollback with revoked secret [refuse] · B-D5 retention [ADR defaults: 2 retained, 7 days, 24h failed]
- B-D6 cleanup fence [DB-state fence, recorded in ADR]

## Deferred
Maintenance service; rollback by image recreate; container runtime logs; build-cache pruning; ReserveCheckout
lock-order fix; flagged-head trigger; agent_jobs owner index; persisting executor_available.

## Progress
(empty)
