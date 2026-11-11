# Step 10e — drain and stop the predecessor: plan (owner decisions recorded 2026-11-10)

Settled (slice-10 plan + ADR 0005 §"Retention and drain policy"): blue/green only; after the switch, wait the
frozen `drain_seconds` (1-300, frozen per attempt, already delivered in the lease), then STOP — never remove — the
predecessor; a stop failure never undoes the activation. Retention/cleanup/rollback are slice 11. Gates stay false.

## Current state (verified 2026-11-10)
- `Lease.DrainSeconds` reaches the agent; nothing uses it.
- Lease is 90s and renewals stop at hand-off (10d); the success report's `finalizeActivationSucceeded` closes the
  attempt/slot/job and moves the predecessor release `active -> retained` in the same transaction. So after the
  report there is no lease; the server never touches Docker (ADR 0005).
- Agent does not know the predecessor: `ActivateResponse` carries only the new backend. Server knows it
  (`release_operations.expected_active_release_id`; its own succeeded activate op's attempt -> runtime instance id).
- Engine client has no stop call. `release_runtime_instances.observed_state` exists, unwritten. No server channel
  for a stop result (job closed; no event type; `retire` op kind unused).

## Sub-steps (one subagent each, review between, atomic commits)
1. **10e-1 Engine stop** (`internal/runtime/docker`): `StopContainer(ctx, id, grace)` (204/304 ok, 404 typed),
   bounded `WaitStopped` via inspect. Unit + race.
2. **10e-2 Predecessor identity from the server** (D-b): activate response gains `predecessor_release_id` and
   `predecessor_runtime_instance_id` (empty on first activation), derived server-side, also on the replay branch;
   threaded through queue/http/client. No migration. Affected integration packages on fresh PG18.
3. **10e-3 Durable drain record** (`internal/agent/state`, own small store, same rules as the activation store):
   armed ONLY when the server answers 200 to a SUCCESS report (never on conflict/late-report/failure); written before
   the activation record is deleted (crash between = replay re-arms). Fields: attempt, env, new + predecessor release
   and runtime instance, drain_seconds, armed_at, observed route digest, phase, summary.
4. **10e-4 Drainer** (agent goroutine + startup resume): wait `armed_at + drain_seconds` (resume remaining time after
   restart); guards; stop; wait stopped; delete record. Failure -> keep record as `stop_failed` evidence + loud log;
   bounded retry for transient errors only.
   10e-4 must also: make the drain store REQUIRED in production wiring (reporter + ActivationConfig; fail agent
   startup if nil); no tombstone (decided 2026-11-11): a late re-arm after a completed drain only re-stops an
   already-stopped container (304 = success) and the route guard still applies.
5. **10e-5 Docs**: ADR 0005 amendment + IMPLEMENTATION_STATUS + note: the drainer runs only while the executor is enabled (leftover drains wait); the route guard is checked
   before stop, so a route change during the stop window is not re-checked (no second activation exists until slice 11)
   + note that a 10d binary cannot read 10e-written activation records (strict decode; drain them
   before any downgrade) + ADR 0004 note: Activate now does a plain non-locking
   `release_operations` read (predecessor derivation) after the route head lock.

## Guards (must never happen)
- Stop the new release: refuse if predecessor runtime instance/release == new; resolve by container name, then
  require exact labels (managed, runtime kind, environment, release id, runtime instance id — owner/server NOT
  checked: deviation recorded 2026-11-11, the runtime instance id is the globally unique attempt id and owner/server
  are not in the drain record); stop by the inspected container ID; exactly
  one match; never an empty selector.
- Stop without a verified switch: arm only from a 200-acknowledged success with a non-zero observed digest.
- Stop what the route still serves: before stopping, on-disk route digest must equal the recorded observed digest;
  else skip ("route moved") and record.
- Remove: the interface gains only Stop; no remove reachable.
- First deployment: no predecessor -> no drain record.

## Owner decisions (2026-11-10, all as recommended)
- D-a: drain after the 200-acknowledged success report, as an agent-local durable obligation outside any lease.
- D-b: the server supplies predecessor release id + runtime instance id in the activate response.
- D-c: stop failures are agent-local evidence (durable record + loud log) in 10e; server record with slice 11.

## Verification (by risk)
Agent-only steps: gofmt, vets, `make check`, `go test -race ./...`, Linux builds. 10e-2 (server response change, no
SQL/locks): + affected integration packages on fresh PG18. Independent review per step.

## Risks
Label/identity confusion (exact match + self-refusal); later activation racing the drain (route-digest check);
long agent outage (predecessor keeps running, record survives); failed stop invisible to control plane until slice
11 (D-c); restart policy vs explicit stop and real Engine semantics unprovable on macOS; clock jumps (persist
wall-clock armed_at, bound the wait by drain_seconds).

## Progress
- 10e-1 `3e83f79` Engine StopContainer (ID or AutoDeploy name only; grace 1-300s) + fail-closed WaitStopped.
- 10e-2 activate response names the predecessor (release id + runtime instance = attempt id of its newest succeeded
  activate op; plain read after the route head lock; replay derives from the stored op). Corrupt state refused
  (no succeeded op; identity equal to candidate). Client requires both keys (absent = malformed; empty = none).
  Review PASS with notes, applied. Integration on fresh PG18 green.
- 10e-3 durable drain record: armed (ArmDrain: create-or-no-op, never rewinds drainer progress, ArmedAt preserved)
  only on HTTP 200 to a success report with a predecessor, before the activation record is deleted; reporter and
  executor direct-report fallback (bounded retry). PendingActivation carries release/backend/predecessor/drain
  fields (old-shape files still load). Review PASS with notes, applied.
- 10e-4 drainer + wiring. Review 1 FAILED: Docker inspect 404 was never ErrContainerNotFound (fakes masked it),
  so a gone predecessor became permanent stop_failed -> engine now wraps it (still *EngineError 404). Also fixed:
  backward clock step (monotonic first-seen bound), Drainer.Validate at startup, *docker.Client has no Remove/Kill
  (reflection test). Re-review PASS. Drain store required; drainer started after Recover; --drain-stop-grace 30s.
