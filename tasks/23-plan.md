# 23 Small dev follow-ups (2026-11-21, owner approved)

Goal: close the remaining code follow-ups so that what remains is real-host validation only (public VPS e2e,
backup drill, 24h retention).
- Out of scope: flipping executorEnabled/activationEnabled. That waits for the VPS e2e.
- Dropped: secret rewrap generation binding, already fixed in d02edb3 (internal/secret/rotation.go:58, tests
  rotation_test.go:156/160).

Process (as in 22-3):
- One slice at a time: Sonnet implements, then a fresh review, then fixes, then the orchestrator verifies, then
  Haiku commits.
- Verify bar per slice:
  - always: vet + unit tests;
  - store/web changes: `go test -tags=integration` on the store, web and agent http packages;
  - web-handler changes: also `-run Web ./internal/store/postgres` (lesson 22-3l);
  - capture real exit codes (no `| tail` masking).
- Every brief carries RULE-01, no rm, no `sed -i`, scratch only in the scratchpad, no commits by the implementer.

## Slices (small first, then the two design slices, then quota)

### 23-A Web error mapping (tiny)
- Today `/servers` (ListServers and ListReconcilingEnvironments errors, web.go:883-891) and `/reconciliation`
  (reconciliation.go:48-52) answer `badRequest` (400). Replace each with `operationsFailure(w, r, err)`:
  - ErrServerAuthorizationDenied redirects to /login;
  - 55P03/57014 give 503 with Retry-After;
  - anything else gives 500.
- Tests: an auth-denied error redirects to /login; a generic error gives 500; a lock timeout gives 503.

### 23-B Windows vet (tiny)
- `GOOS=windows go vet ./internal/gitcheckout` fails because checkout.go/production.go use x/sys/unix untagged.
- Fix: add `//go:build linux` to checkout.go, production.go and the three untagged tests, plus a `!linux` stub
  file so the package still builds elsewhere.
- Verify: `GOOS=windows go vet ./...` and `GOOS=darwin go vet ./...` are clean, and linux tests are unchanged.

### 23-C Installer upgrade banner (small, shell)
- install-app.sh always prints the fresh-install next steps (public :792-818, LAN :821-847).
- Detect an upgrade before the banner: any of WAS_CP/WAS_AGENT/WAS_TRAEFIK, or an existing
  `$ETC_DIR/controlplane/database-url` / `$ETC_DIR/secrets/postgres-password`.
- On upgrade print a short banner (upgraded binaries, units refreshed, migrations applied, services restarted,
  run preflight-app.sh) and exit 0. The fresh path is unchanged.
- Test: extend deploy/host/tests/script_structure_test.sh run_scenario (Docker) with a second run asserting
  the upgrade banner, and the fresh banner on the first run. Execution test, not `bash -n` (lesson 22-5).

### 23-D Partial index for health-line lookups (small, migration)
- `migrations/000031_deployment_log_chunks_health_idx.sql`:
  `CREATE INDEX deployment_log_chunks_health_idx ON deployment_log_chunks (attempt_id, id DESC) WHERE stream = 'health';`
  - Up-only.
  - Apply runs in one transaction, so no CONCURRENTLY. The table is bounded by retention, so the build is short.
- A migration integration test asserts the index exists and the reason/probe lookups use it (EXPLAIN contains
  the index name on a seeded fixture).
- Acer: the next install-app run applies it.

### 23-E Checkout exit 125 diagnostics (small)
- cmd/checkout-exec/main_linux.go returns 125 for three distinct helper failures (:450 cleanup failed,
  :455 cancelled, :460 wait/start error incl. a signal kill).
- Emit a distinct, fixed diagnostic line for each, naming the signal for signal kills via WaitStatus, so the
  next occurrence is self-explaining in the attempt log. No behaviour change.
- Test: unit tests for each path's diagnostic text, with no secrets or paths beyond the existing redaction rules.

### 23-F Narrow the reconciliation block to host-mutating phases (design, owner decision D1)
- Today closeAttemptOrdinary (agent_lease_repository.go:962, slot UPDATE :974) sets
  `reconciliation_required` on every expiry or revocation, whatever the phase. ADR 0004 itself says
  "in-flight host mutation".
- Proposal:
  - Inside the existing slot/job/attempt locks, read `deployments.status`.
  - Block (as today) only when the status is `starting`, `health_checking` or `activating` (a candidate
    container may exist), or an activate operation exists (already diverted).
  - For `assigned`/`fetching`/`building`: vacate the slot (fence+1, current_attempt_id NULL) WITHOUT the flag,
    so the retry or next deploy can claim immediately.
- Safety precondition, verified first and pinned by a test: the executor never creates a container unless
  ReportProgress(starting) succeeded under the current fence and authority. The store refuses progress once
  the slot moved, so a partitioned old agent cannot start one after expiry.
  - If the executor does not guarantee this today, the slice adds that guard first.
- Builds left behind by a dead agent (images, build cache) are not traffic-affecting; the existing cleanup and
  retention handle images.
- ADR 0004 amendment (dated) states the predicate and why.
- Tests:
  - expiry in fetching/building: no flag, fence advanced, slot claimable, job retry/dead semantics unchanged;
  - expiry in starting/health_checking: flag as today;
  - revocation path: same split;
  - existing sweeper tests updated where they pinned health_checking (keeps blocking) vs building
    (new: no block).

### 23-G Restarted agent acknowledges lost attempts (design, owner decision D2)
- Today leases are memory-only. A SIGKILLed agent's attempt silently expires (K1).
- Proposal:
  - Persist a minimal attempt record in the agent state dir at claim: {attempt_id, job_id, environment_id,
    fence, phase}.
  - Update `phase` BEFORE each phase starts. Delete on Complete/CancelAck/activation hand-off.
  - On startup, for each leftover record (with no durable activation record for that attempt):
    - phase ≤ building: call Complete(failed, class transient, fixed summary "agent restarted during <phase>").
      If accepted, the attempt closes normally and the job retries per the queue rules, with no expiry and no
      block. If refused (authority lapsed or already expired), log and delete the record.
    - phase ≥ starting: log "lost attempt in <phase>" and leave it to expiry, which blocks per 23-F.
  - Always log one fixed line per lost attempt.
- State-file integrity rules match the existing fence files: corrupt means fail closed.
- Tests:
  - agent restart in building leads to a Complete(failed, transient) call with the right fence/attempt;
  - in starting: no call, log only;
  - refused Complete deletes the record;
  - corrupt record: agent refuses to act;
  - activation record present: untouched (activation recovery owns it).

### 23-H Reclaim stranded checkout quota allocations (design, owner decision D3)
- Today an `active` allocation whose holder died stays until the quota service restarts (reconcile only in
  New, quota_linux.go:1143). The agent logs "allocation may be stranded" when its single Close fails.
- Proposal:
  - Agent side (F3): retry Close with bounded backoff (e.g. 3 tries within checkoutCloseBudget).
  - Quota side: a periodic sweep in Serve (every 10 min) that releases `active` entries older than a TTL
    (D3: default 6h, well above checkout + build time limits). Uses the existing cleanupLocked/completeLocked
    path and is logged.
  - Verify ledger semantics first: Terminal replay, a handle reused after release, and an in-use workspace
    past the TTL (its build would fail; the TTL must exceed the maximum job wall time).
- Tests (Linux, checkout_integration tag where XFS is needed; unit tests with the existing fakes otherwise):
  - stale active reclaimed;
  - fresh active untouched;
  - releasing/terminal unaffected;
  - concurrent Release vs sweep is safe.

## Order and gates
1. 23-A, 23-B, 23-C, 23-D, 23-E (independent, small).
2. Then 23-F, then 23-G (23-G's phase ≤ building rule relies on 23-F's split).
3. Then 23-H.
4. Acer round 8 at the end: install, K1 again (kill during build: expect no block and a clean retry), a
   restart during build (expect Complete failed + retry), the index present, and the upgrade banner.

## Risks
- 23-F loosens a safety block. Its precondition test is mandatory, and the review uses a shell-capable
  reviewer across store and executor.
- 23-G adds durable agent state. Fail closed on corruption; never declare an activation outcome
  (owner decision D-B unchanged).
- 23-H could reclaim a workspace in use if the TTL is too short. The TTL is above the job wall-time limit, and
  the sweep is logged.

## Progress
- 2026-11-21: facts gathered (2 read-only sweeps), plan drafted. Owner approved. Decisions: D1 narrow the block to container phases (starting/health_checking/activating); D2 a restarted agent reports lost checkout/build attempts as failed, transient; D3 quota reclaim TTL 6h.
- 2026-11-21: 23-A cb29c69 (web error mapping; old tests that pinned 400 replaced, reasoning in the commit). 23-B a9d3a05 (cross-GOOS vet via unix helpers plus !unix stubs; darwin still runs all 157 tests). 23-C (installer upgrade banner): Docker scenarios lan-upgrade/public-upgrade fail-first then pass; lan_mode_identical passes on debian:bookworm (fails on the BusyBox bash image for both HEAD and the change, a diff implementation difference; run it on Debian/Ubuntu).
