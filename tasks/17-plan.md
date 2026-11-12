# Step 17 — Traefik API pre-authorization check: plan, 2026-11-12, awaiting owner approval

Gate item 13 (IMPLEMENTATION_STATUS): a wrong/missing Traefik API credential answers 401, the observer classifies
any non-2xx/non-404 as transient, and `Verify` runs only AFTER the route switch
(`executor/activation.go` switchRoute). So credential misconfiguration (either direction: wrong file, or Traefik
requires auth and no file is set) turns every activation into a post-authorization failure -> reconciliation
block an operator must clear. Nothing before `authorize` touches Traefik (`validateActivationFlags` and
`NewObserver` do no I/O). Gates stay false; outcome count stays 8/18.

## Design
- **17a `Observer.Preflight(ctx) error`** (`internal/runtime/traefik/client.go`): one GET `/api/version` through
  the existing `get` (same loopback-only client, basic auth when configured, no redirects/proxy, bounded timeout).
  Typed result via the existing `ObservationError` classes:
  - 200 (body not parsed beyond being a JSON object) -> nil.
  - 401 / 403 -> `ErrClassConfiguration` with a fixed message ("traefik api rejected the agent's credentials").
  - 404 / other 4xx -> `ErrClassConfiguration` ("traefik api not found at the configured url").
  - transport error, timeout, 5xx -> `ErrClassTransient`.
  `/api/version` is a public Traefik API endpoint in v2 and v3 (docs knowledge; unverified against a real Traefik).
- **17b executor** (`activate`, before `a.authorize`): call Preflight. `RouteObserver` interface gains `Preflight`.
  - nil -> continue as today.
  - Configuration -> `e.outcome(lease, false, queue.FailureClassPermanentConfiguration)`: pre-authorization,
    no traffic change, job not retried (Q1).
  - Transient (Traefik down/restarting) -> `FailureClassTransient`: retried later; nothing authorized.
  - Structural log line `activation: traefik api check failed (<class>)`; ERROR log with attempt id + class
    only (never the credential, never the URL userinfo — already rejected).
- **17c Verify fail-fast** (`Observe*`): classify 401/403 as `ErrClassConfiguration` instead of transient, so if
  the credential breaks between preflight and verify the post-switch failure is immediate with a clear summary
  instead of polling 30s. Still a reconciliation block (correct: the route was written). Q2.

## Files
`internal/runtime/traefik/client.go` (+tests), `internal/agent/executor/activation.go` (+tests, fake observer
gains Preflight), status/ADR 0005 note. ~80-120 lines incl. tests. No SQL, locks or migrations.

## Edge cases
Traefik restarting at preflight (transient, retried); credential rotated between preflight and verify (17c);
Traefik API disabled entirely (404 -> permanent_configuration, operator fixes config); preflight slow (bounded
by the observer's per-request timeout, inside the lease, before authorization); preflight passes but router
never loads (unchanged: post-switch verify failure — a Traefik-side problem, not config).

## Verification (agent-only per subagent-verification-scope)
gofmt, both vets, `make check`, `go test -race ./...`, `-race -count=10` on executor + traefik, Linux
amd64/arm64 builds, `go list -deps ./internal/runtime/traefik` (no Docker client). Tests: httptest 200/401/403/
404/500/timeout -> class; executor: preflight config error -> permanent outcome and `authorize` NEVER called;
transient -> transient outcome, `authorize` never called; nil -> authorize called. Mutation-check each.
Independent review.

## Risks
- Wrongly permanent-failing a deployment on a transient Traefik hiccup that answers 4xx — mitigated by mapping
  only 401/403/404-class to configuration, everything else transient.
- Real Traefik `/api/version` shape/auth behaviour unverified until the host run.

## Owner decisions (2026-11-12, both as recommended)
- Q1: preflight 401/403/404 -> `permanent_configuration`.
- Q2: include 17c (Verify 401/403 fail-fast).
Order: 17a+17c (traefik package, one subagent) -> review -> 17b (executor, one subagent) -> review -> docs.

## Progress
- 17a+17c (uncommitted): Preflight GET /api/version via `get`; 401/403 -> Configuration (credentials), 408/425/429
  -> Transient (load shedding; added after review — would otherwise permanently fail a job on a rate limit),
  other 4xx -> Configuration (not found), 5xx -> Transient, 200 non-object -> Malformed; redirect refusal stays
  Malformed (corrupt_state, also non-retryable; consistent with `get`). ObserveRouter/Service 401/403 ->
  Configuration; executor verify loop returns on any non-transient class (activation.go verify), so post-switch
  auth failure is immediate. Only external callers of ClassifyObservation/Verify are in executor (verified).
  Review PASS with notes: 408/429 fix + stale ErrClass docs applied (mutation-proved). Accepted lows: 4xx body
  >64 KiB reads as Malformed (fail-closed); 2xx-not-200 Transient.
- 17b: `RouteObserver.Preflight`; activate calls it with the lease ctx before authorize; failure ->
  `e.outcome(lease,false,ClassifyObservation(err))` -> run.go Complete(failed, class) (pre-authorization; no
  SaveActivation). Tests use a real Observer on httptest (401/403/404 permanent, 500 transient; Activate never
  called, nothing written) + ordering test; both mutations (skip, move after authorize) proved. Orchestrator
  review PASS. Combined tree: make check exit 0, `go test -race ./...` exit 0 / 0 FAIL, vets, Linux builds,
  diff --check green.
- Docs: gate 13 struck; ADR 0005 2026-11-12 "known gap" replaced by the step 17 description.
