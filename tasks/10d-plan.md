# Step 10d — agent activation execution: plan (owner decisions recorded 2026-11-10)

Settled, not re-opened: D1 blue/green only; D3 activation under the same lease; D4 failures transient only
BEFORE switch authorization, reconciliation-required after (never retried as a fresh attempt, never an
automatic route rollback — ADR 0005 forbids blind resolution); control plane renders route + digest, agent
re-renders and refuses on mismatch; Activate replay key (attempt, fence, image_digest); `executorEnabled` and
`activationEnabled` stay false throughout 10d.

## Current state (verified in code 2026-11-10)
- `executor.Execute` stops after the health gate; returns `agentclient.Outcome`; `run.go reportOutcome` calls
  `Complete`, logs and DROPS on error (only `post`'s 5 tries, ~3s).
- Client has no `Activate` / `ReportActivationResult`; `Reconcile` exists, no caller.
- `mapStatus`: 409 `stale_lease` -> ErrStaleLease, every other 409 -> ErrIntegrityConflict (code discarded);
  server sends 12 distinct 409 codes (`agent/http/handler.go mapError`).
- `agent/state` persists only `Fence`; nothing survives a restart to resume or re-report. `runLease` rejects a
  non-advancing fence, so recovery can never come from re-claiming.
- `ReportActivationResult` deliberately has NO authority-expiry check; after the sweeper blocks the operation a
  report is recorded as evidence (409 `late_report_recorded`), never finalizes.
- `runtime/traefik`: Render/Digest/Write/ReadDigest/Observer/Verify exist; no HTTPS data-plane probe; no
  route-dir / API URL / probe address config in the agent.

## Sub-steps (one subagent each, review between, atomic commit each; order matters)
1. **10d-1 client.** `ConflictError{Code}` (still `errors.Is(ErrIntegrityConflict)` for non-stale codes) +
   `ConflictCode(err)`; `Activate(...) (ActivateResponse, error)`; `ReportActivationResult(...)`. Existing
   `post` behaviour unchanged for other routes. Tests: httptest table over all 12 codes + bad body.
2. **10d-2 HTTPS probe** (`runtime/traefik/probe.go`). Dial configurable local address, SNI = hostname, full
   chain + hostname verification, Host header, bounded poll with its own deadline (first-deploy ACME). Tests:
   good SAN, wrong SAN, expired cert, timeout. Injectable roots/dialer for tests only.
3. **10d-3 durable pending-activation store** (`agent/state`, one file per attempt under `activations/`,
   existing `writeFileAtomic`, fail closed on corruption). Record: attempt, fence, environment, operation_id,
   authorized route_digest, image_digest, phase (`authorized` | `result_ready`), result (success digest |
   failure summary + observed evidence), created_at. Load/List/Save/Delete.
4. **10d-4 persistent reporter.** Loads pending records at startup and retries each until a TERMINAL answer,
   capped backoff (5s→60s). Delete only when the server durably holds the report or a final decision (allow-list): 200,
   `late_report_recorded`, `route_moved`, `integrity_conflict`. Every other 409 (incl. `stale_lease`, `corrupt_state`,
   `constraint_violation`, unknown codes: server rolled back, nothing recorded) is kept and parked at MaxBackoff. Does NOT set the
   local `ReconciliationRequired` (D-E). Retry: 503, transport, unknown status, 401 (slowly). Never calls `Complete`
   and never fails the job after any 409. Loud log once past authority expiry. Tests: restart (new store, same
   dir), outage then recovery, each terminal code deletes, retryable codes keep.
5. **10d-5 executor activation + config.** After a passing gate: `Activate` (failures here classify transient)
   -> persist record `authorized` BEFORE any route write -> re-render from response + lease (port, scheme,
   release, fence); digest mismatch = FAILURE REPORT, not a silent return -> local revision guard (D-C) ->
   `traefik.Write` -> `Observer.Verify` -> `ProbeDataPlane` -> persist `result_ready` -> hand to reporter.
   Failure summary carries evidence (on-disk digest vs authorized, observer verdict, probe error), ≤2048 B,
   valid digest format. New outcome shape so `reportOutcome` never calls `Complete` once an activation is
   authorized (success closes the attempt server-side). Agent flags with safe defaults (D-D). Tests: fakes,
   one sub-test per failure point (transient before / reconcile after), digest mismatch, crash points.

## 10d-1 review finding binding on 10d-5
A non-409 `Activate` error (transport/503 after retries, undecodable 200, `ErrMalformedResponse`) does NOT mean
"not authorized": tx1 may have committed and the response been lost. 10d-5 must not call `Complete(failed,
transient)` on it as if pre-authorization. Instead: re-call `Activate` (server replays on attempt/fence/digest)
while authority lasts; if still unknown, write nothing to the route and send no `Complete` — the sweeper blocks
an authorized op and the operator declares `not_activated` (host untouched). Only a 409 is a definite refusal
(and then the pre-authorization transient classification applies).

## Crash windows (accepted / closed)
- Crash after Activate response, before the record is persisted: host untouched; sweeper blocks; operator
  declares `not_activated`. Accepted (no traffic change; replaying Activate on restart adds machinery).
- Crash after record, before/after Write, before report: record exists -> reporter reports failure with the
  on-disk digest as evidence on restart. Closed.
- Crash after report accepted, before record delete: replay is idempotent server-side. Closed.

## Owner decisions (2026-11-10, all as recommended)
- D-A: retry the result report until a terminal answer, across restarts; loud log past authority expiry.
- D-B: no agent-side `Reconcile` caller in 10d; resolution stays operator-only; agent sends evidence only.
- D-C: add the local route-revision guard, persisted in agent state.
- D-D: Traefik route dir / API URL / probe address as agent flags with safe defaults.
- D-E (2026-11-10): reporter never sets the local `ReconciliationRequired` flag; nothing clears it today ("never
  reset locally"), so it would wedge the environment on this agent forever. The server's slot block (Claim returns
  nothing until resolved) is the single authority.
- Not in 10d: admin `AssignRouteHostname` on a flagged head (gate-review item, recorded in todo H2 block).

## Verification (scaled to risk)
Steps 1-5 touch only agent-side Go, no SQL/locks/migrations: per step gofmt, both vets, `make check`,
`go test -race ./...`, Linux amd64/arm64 build. PG integration only if a step touches `internal/agent/http`
or the store (none planned). Independent review per step.

## Not provable on macOS
Real Traefik reload, ACME, HTTPS probe against real certs, fsync/rename on the host FS. 10d stays behind the
gates; the Hetzner end-to-end bar remains the proof.

## Risks
- Lost report after switch (the project's recurring class): mitigated by persist-before-write + terminal-only
  deletion + retry-by-default for unknown statuses.
- False probe failure on first-deploy ACME -> spurious block with route rolled forward: generous deadline,
  stated in summary.
- `activation_open` from a stray `Complete`: outcome shape prevents the call.

## Progress
- 10d-1 `59e116d` client typed conflicts + Activate/ReportActivationResult.
- 10d-2 `cfec942` HTTPS data-plane probe (reuses healthgate.ValidRouteHostname).
- 10d-3 durable activation + route-revision stores. Review 1 FAILED (invalid-UTF-8 summary self-corrupted on
  reload -> List fails closed for all records; new dirs not fsynced into parent); fixed, re-review PASS. Shared
  per-directory lock (EvalSymlinks-canonical dir), concurrency tests mutation-proved (per-call lock fails both).
  Single agent process per state dir (no cross-process exclusion). Missing route-revision file reads 0 (accepted:
  guard is defence in depth over server CAS).

- 10d-4 persistent reporter. Review 1 FAILED: server answered integrity_conflict on an operation-id mismatch after
  a rollback (nothing recorded) and the reporter deleted on it -> fixed server-side as 409 `operation_mismatch`
  (kept by the reporter). Delete allow-list: 200, late_report_recorded, route_moved, integrity_conflict. Corrupt
  record files no longer block good ones (state.ScanActivations; corrupt files untouched, logged hourly). Outage
  stops a pass; deferred records not charged; round-robin by last try. Re-review PASS.

## Follow-ups
- Fence `Store.path`: ValidOpaqueID allows 128 bytes and encodeName can expand x3, exceeding the 255-byte file
  name limit for long environment ids (route-revision store already refuses these). Pre-existing; fix before
  activationEnabled flips.
- Local `ReconciliationRequired` set on stale renewal (run.go) is never cleared: that environment is refused by this
  agent forever, even after an operator resolves. Pre-existing; design a clearing rule before activationEnabled flips.
