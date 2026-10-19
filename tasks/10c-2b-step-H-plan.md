# 10c-2b step H plan — wire declared resolution to the agent API and the operator form

Source: `.tmp/10c-2b-design.md` §9 step H, `tasks/10c-2b-step-G-plan.md`, touch-point map 2026-10-19.
Gates stay false: `activationEnabled` (`cmd/controlplane/main.go:42`), `executorEnabled` (`cmd/agent/main.go:60`).

## Facts that shape it
- No agent code calls `Client.Reconcile`; nothing agent-side decides an outcome (that is 10d). H wires the wire
  contract and the client method only.
- Agent `mapError` has no case for `ErrAgentLeaseOutcomeMismatch` / `ErrAgentLeaseActivationUnresolved`: both
  fall to the retryable 503 today — wrong for terminal refusals.
- `postgres.ReconcilingEnvironment` cannot say whether a block is activation-caused (design U7), so the operator
  cannot pick an outcome.
- The web UI is unstyled; CSP `default-src 'none'` blocks CSS.

## Owner decisions (2026-10-19)
- Delete the old `Reconcile` and `MarkReconciled` repository methods in H (their behaviour is the `no_activation`
  outcome of the new methods), with the removal documented and their unique assertions migrated first.
- Plain HTML, no CSP change: a required `<select>` with an empty first option + an "activation" column. Styling
  is a separate later step (would need `style-src 'self'`).

## H1 — agent API
- `reconcileRequest` gains required `outcome`; `parse()` allow-lists the three `postgres.ActivationOutcome*`
  values; missing/unknown -> 400 `invalid_request` (no default). `decodeStrict` already rejects unknown keys.
- `EnvironmentReconciler` becomes `ReconcileActivation(...)` with the repository signature, so `main.go` keeps
  passing the repository directly (fix the stale "no adapter needed" comment if wording changes).
- `mapError`: `ErrAgentLeaseOutcomeMismatch` -> 409 `outcome_mismatch`; `ErrAgentLeaseActivationUnresolved` ->
  409 `activation_unresolved`. Both terminal for the client (any non-`stale_lease` 409). Placed after
  `ErrActivationBlockUnrecorded`, which must stay first.
- `Client.Reconcile` gains `outcome string` and sends it; still no caller.
- Tests: fake + `TestErrorMapping` rows for both sentinels; request validation (missing / unknown / each valid
  outcome); client sends the field and classifies both 409s non-retryable; the HTTP integration test
  (`activation_blocked_integration_test.go`) sends an outcome and keeps proving step C; an end-to-end HTTP test:
  block via the real sweeper -> agent declares over HTTP -> environment unblocked.

## H2 — operator form
- `ServerAdmin.MarkReconciled` -> `ResolveActivation(ctx, principal, environmentID, outcome, reason)`; adapter in
  `main.go` calls `leases.ResolveActivation`.
- Handler: exact 4 keys (`csrf`, `environment_id`, `outcome`, `reason`), outcome allow-listed, CSRF unchanged.
  Reason bounded so `outcome=<x>; <reason>` fits 512 bytes (form `maxlength` and server check agree).
- Fixed, distinct operator messages (never `err.Error()`): outcome mismatch ("this block was / was not caused
  by an activation"), not yet resolvable (switch still authorized), route moved (`activated` impossible, choose
  `not_activated`), corrupt state, generic bad request.
- `ReconcilingEnvironment` gains an activation field (none | `switch_authorized` | `reconciliation_required`)
  from the `unresolvedActivationSQL` rule; the row shows it so the operator can choose.
- Tests: 3-key and 5-key bodies refused, missing/unknown outcome refused, each outcome reaches the repository,
  each error renders its fixed text, rendered form has `required` select with no preselected option and the
  activation column.

## H3 — delete the old methods
- Before deleting: list every assertion in the `Reconcile` / `MarkReconciled` tests; each must already exist for
  the new methods or be ported. Then delete the methods and their tests, and record removed behaviour,
  replacement and proof in `tasks/todo.md` (CLAUDE.md "Don't delete working code" rule).

Each of H1/H2/H3: one subagent, independent review, full bar, commit.
