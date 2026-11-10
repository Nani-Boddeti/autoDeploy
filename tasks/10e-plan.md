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
5. **10e-5 Docs**: ADR 0005 amendment + IMPLEMENTATION_STATUS.

## Guards (must never happen)
- Stop the new release: refuse if predecessor runtime instance/release == new; resolve by container name, then
  require exact labels (managed, runtime kind, owner, environment, server, release id, runtime instance id); exactly
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
