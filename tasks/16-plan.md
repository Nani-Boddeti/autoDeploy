# Step 16 — pre-activation hardening (host-independent gates): plan, 2026-11-12, awaiting owner approval

Goal: close every `activationEnabled` / `executorEnabled` hard gate (docs/IMPLEMENTATION_STATUS.md "Hard gates")
that does not need a real Linux host. Gates stay false throughout. Outcome count stays 8/18.

## Gate inventory (verified in code 2026-11-12)
| # | Gate | State | In 16? |
|---|------|-------|--------|
| 1 | Privileged Linux smoke | needs host | no |
| 2 | Persistent result retry | done 10d-4 | no (unproven vs real outage: host run) |
| 3 | Traefik API auth | client unauthenticated, loopback-only (`traefik/client.go` NewObserver) | 16f (decision Q3) |
| 4 | Both resolution paths safe only while gate false | check, not code | flip-time checklist item only |
| 5 | Fence `Store.path` >255-byte name | open | 16a |
| 6 | Local `ReconciliationRequired` never cleared | open | 16b (decision Q1) |
| 7 | Recovery only at startup | open | 16c |
| 8 | `validateActivationFlags` misses timeout budget + route-dir writability | open (fails closed) | 16d |
| 9 | Recovery summary truncates from tail | open (low) | 16d |
| 10 | Drain lost if arming fails after direct report | open | 16e |
| 11 | Drain/stop evidence agent-local only | slice 11 | no |
| 12 | 10d binary cannot read 10e records (downgrade) | docs | 16g note |
| — | Admin `AssignRouteHostname` on flagged head blocks `activated` (todo H2 block) | open | 16h (decision Q2) |
| — | 23xxx catch-all | **already done**: `agent_lease_repository.go:1949` -> 409 `constraint_violation` | no |

## Evidence for 16b's clearing rule
- Stale renewal => the attempt was closed server-side. The only fence advances are Claim
  (`agent_lease_repository.go:479`) and `closeAttemptOrdinary` (`:880`), and the latter always sets
  `environment_slots.reconciliation_required=true`. So every stale renewal leaves the server slot flagged.
- Claim hands out nothing for a flagged slot (10d D-E; 16b must pin this with a test, not trust it).
- Therefore a NEW lease for that environment with `fence > HighestAcceptedFence` is proof an operator resolved
  the block server-side. Rule: on such a lease, clear the local flag and proceed. Fence-not-advancing still refuses.
- Conflicts with run.go Decision 5 "never reset locally" -> needs ADR 0004 amendment (16g).

## Sub-steps (one Sonnet subagent each, independent review between, atomic commit each)
- **16a fence path length.** Fence store refuses ids whose encoded name exceeds 255 bytes, same rule/error as
  `routerevision.go` (`maxFileNameBytes`); share the check, don't duplicate it. Tests: 128-byte worst-case id
  refused on Save and Load, normal UUID id unchanged, existing files still load.
- **16b clear local `ReconciliationRequired`** per the rule above (Q1). Tests: stale renew -> flag set -> lease
  with same fence refused -> lease with higher fence clears flag and runs; crash between clear and Save
  (flag stays, retried next lease); integration test: Claim returns nothing while slot flagged.
- **16c between-leases recovery pass.** Run the existing startup `Recover` (authorized -> result_ready failure
  with on-disk evidence) at the top of each poll cycle, only for records whose attempt is not currently running
  (single-process store: no lease is live between cycles). Must not re-report a record the reporter holds.
  Tests: double failure (save + direct report) recovered without restart; live attempt's record untouched.
- **16d flag validation + summary builder.** `validateActivationFlags` checks
  `--activation-timeout` >= probe + verify + 30s when non-zero, and route-dir exists, is a dir, writable
  (create+remove a temp file). Recovery summary via a reason-capped builder: fixed fields (digests) always kept,
  free-text reason truncated, ≤2048 B, valid UTF-8. Tests: table per flag, oversize reason keeps both digests.
- **16e drain arming durability.** Persist drain intent (predecessor ids, observed digest) in the `result_ready`
  record BEFORE the direct report; on 200 arm from the record; if arming fails, the record stays as
  `reported_success` (new phase) and recovery/reporter retries ArmDrain only, never re-reports as failure.
  Tests: arming failure then restart -> drain armed, no failure report sent; old-shape records load.
- **16f Traefik API credential (Q3).** Optional `--traefik-api-credentials-file` (basic auth, file 0600 checked,
  never argv/env, never logged); Observer sends `Authorization` only to the loopback base URL; no-redirect stays.
  Absent flag = today's behaviour. Tests: header sent, file perms refused, value absent from logs/errors.
- **16h hostname assignment on flagged head (Q2).** `AssignRouteHostname` refuses with a typed error while
  `route_heads.reconciliation_required`; admin UI shows the reason. Lock order unchanged (route head row only).
  Tests: integration refusal on flagged head, success after resolution.
- **16g docs.** ADR 0004 amendment (16b clearing rule replaces "never reset locally"), ADR 0005 (16e, 16f),
  IMPLEMENTATION_STATUS gate list (strike closed, add flip-time checklist incl. #4 and #12 downgrade note).

Order: 16a, 16d (small, warm-up) -> 16b -> 16c -> 16e (16c/16e share recovery code; serial) -> 16f -> 16h -> 16g.

## Verification (by risk, per subagent-verification-scope)
- 16a/16c/16d/16e/16f agent-only: gofmt, both vet tag sets, `make check`, `go test -race ./...`,
  Linux amd64/arm64 builds.
- 16b, 16h touch store/handler: + fresh-PG18 integration for `internal/store/postgres internal/agent/http internal/web`
  (recipe in memory); 16h also the activation concurrency stage (`-race -count=20`, `-timeout 60m`) since it
  adds a route-head refusal path.
- Every step: independent `code-reviewer` pass; fix -> re-review until PASS. Review fakes against real error
  shapes (10e-4 lesson).

## Edge cases to cover
Long env ids on existing fence files (refuse, never silently rename); flag cleared but Save fails (stays set,
fails closed); recovery racing the reporter on one record; drain record old-shape decode; credential file
missing/world-readable/empty; hostname assignment racing resolution (route head lock serializes).

## Risks
- 16b wrongly clears after a stale renew that the server did NOT flag -> agent acts on an environment with
  lingering side effects. Mitigation: the evidence above + an integration test pinning Claim-refuses-flagged.
- 16c/16e touch the lost-report class (6 prior instances). Mitigation: no delete-path changes; reviewers told
  to audit every failure path of the new phase.
- 16f misconfig only surfaces on a real Traefik (accepted; host run proves it).

## Owner decisions (2026-11-12, all as recommended)
- Q1: 16b clear the local flag on a higher-fence lease (amends ADR 0004 Decision 5).
- Q2: 16h refuse hostname assignment on a flagged head.
- Q3: 16f Traefik credential now.
- Q4: 16e in this step.

## Progress
- 16a (uncommitted): shared `fileNameFor` in state.go; fence `Store.path` refuses names >255 B; route-revision
  store reuses it. Test incl. exact-255 boundary; mutation-proved (OS ENAMETOOLONG alone fails the assertion).
  gofmt, vets, make check, agent race tests, Linux builds green. Review PASS with notes (256-byte refusal case,
  refused Save leaves no file/index entry) — both applied; 256 case mutation-proved (`<= max+1` fails it).
- 16d: exported `executor.ValidateActivationTimeout(activation, probe, verify)` (single rule; NewExecutor and
  validateActivationFlags both call it; agent has no verify flag -> 0 = default). Route-dir writability probe
  (`.autodeploy-writable-*`, no Traefik-loadable extension, removed). `buildFailureSummary` budgets the reason
  after fixed fields (no parallel helper). 5 mutations proved. Orchestrator review: PASS (agent-only, fails closed).
  Combined tree: gofmt, both vets, `go test -race ./...`, Linux builds, diff --check green.
