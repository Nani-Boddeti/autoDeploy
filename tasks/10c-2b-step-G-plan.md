# 10c-2b step G plan — declared resolution of a blocked activation

Source design: `.tmp/10c-2b-design.md` §6, §7, §8 (D4, D8, D10, U1-U7, R2-R5). Facts re-checked 2026-10-19
against `d9582c3`: `Reconcile` (`agent_lease_repository.go:3074`) still denies a leased attempt only
incidentally (`attemptState == "leased" || closedAt == nil` -> `ErrServerAuthorizationDenied`);
`MarkReconciled` (`:3206`) refuses any unresolved activation (`ErrAgentLeaseActivationUnresolved`, step D).

## Goal
An operator or the owning agent declares what really happened to a blocked activation — `activated` or
`not_activated` — and the environment is unblocked in ONE transaction, with every row left exactly as the
corresponding success/failure path would have left it. Until G lands, an activation-caused block has no way out.

## Split (one subagent per step, review + full bar + commit after each)
- **G1 — repository resolution core + operator path (`MarkReconciled`).** The operator path ships first because a
  revoked server can never authenticate, so for a revoke-diverted block it is the ONLY way out (R3).
- **G2 — agent path (`Reconcile` branch B) + the explicit unresolved-activation check (prerequisite 2).**
  Both changes in one step: adding the leased-attempt branch removes `Reconcile`'s incidental protection.
- Step H (wire/HTTP/form) and I (docs) unchanged.

## G1 scope
1. Outcome vocabulary `activated | not_activated | no_activation`, no default (D4). Checked both ways under the
   locks: activation-caused <=> an unresolved activate op exists for the slot's `reconciliation_attempt_id`.
   Wrong kind -> new `ErrAgentLeaseOutcomeMismatch`; unknown/empty -> invalid input. `no_activation` = today's
   behaviour, unchanged.
2. Shared resolution routines (D5/D10 — one definition, used by G1 and G2):
   - `resolveActivationActivated`: calls `finalizeActivationSucceeded(fromState=reconciliation_required)` (step F),
     then appends `reconciled`. `resultDigest = op.route_digest`.
   - `resolveActivationNotActivated`: design §6.3 sequence (deployment `activating -> failed`; head flag cleared
     only if set, nothing else on the head; op `rr -> failed`, closed_at COALESCE; attempt per decision Q1;
     slot freed + flag cleared in one UPDATE, owner-guarded; job `leased -> failed`; events
     `activation_not_activated`, `failed`, `reconciled`). Every write conditional, RowsAffected==1.
   - Pre-verified under locks: slot `reconciliation_attempt_id` == attempt (F's helper only refuses a FOREIGN
     flag); op state is exactly `reconciliation_required` (a `switch_authorized` op is NOT resolvable — the host
     switch may still be in flight, D8); attempt and job still `leased`.
   - Any failure (incl. `ErrAgentLeaseActivationRouteMoved`): roll back, write NO new block — the environment is
     already blocked. Never route through `blockAfterFinalizeFailure`.
   - Clock read AFTER all locks (bare `$now` in slot/job/release `updated_at`).
3. Evidence: op `outcome_summary` keeps the block + late-report evidence; a short ` | declared <outcome>` marker is
   appended when it fits (rune-safe, 2048 cap). The operator's full reason and identity live in the audit row,
   which is not size-constrained by the summary. Evidence is never replaced.
4. Operator entry point: a NEW repository method `ResolveActivation(ctx, principal, environmentID, outcome,
   reason)`; `MarkReconciled` stays byte-identical (it already refuses activation blocks) so G1 touches no web or
   handler code — step H merges the two behind one form with the outcome field. Owner-scoped as today; locks in documented
   order (admin session/user -> slot -> job -> attempt -> op -> deployment -> head -> releases); NO server
   credential (revoked servers). Audit outcome encoding per decision Q4.
5. Idempotency: after resolution the slot no longer names the attempt, so a repeat falls to today's "already
   reconciled" path; add an outcome-agreement check (`succeeded`<->`activated`, `failed`<->`not_activated`, no
   op <-> `no_activation`), disagreement -> `ErrAgentLeaseIntegrityConflict`.

## G1 tests
- Matrix: {activated, not_activated} x block cause {tx2 failure report, ambiguous success, sweeper expiry, REAL
  `RevokeServer`} -> exact post-state tables; `not_activated` leaves head hostname/revision/active/created_at
  byte-identical (only flag + updated_at move); first deployment (no predecessor).
- Refusals: wrong outcome kind both directions, unknown outcome, `switch_authorized` op, wrong owner, foreign
  slot flag, route moved (activated) -> nothing written.
- End to end: strand via the real sweeper -> resolve -> a NEW attempt can Claim and Activate (the wedge is gone).
- Concurrency: two resolutions race (exactly one wins); resolution vs sweeper vs tx2 queued on a slot held by a
  third tx with `pg_stat_activity` waiter confirmation; weaken each lock to prove non-vacuity.
- Mutation proofs on every guard and every conditional write.

## G2 scope (outline; detailed after G1 lands)
`Reconcile` common prefix moved to the legal order (creds SHARE -> peek -> slot -> job -> attempt -> op); explicit
unresolved-activation check on the existing closed-attempt path (corrupt state, not a generic denial); new branch B
for `leased` + `closed_at IS NULL` with server/env/fence/slot-flag/op checks, authority NOT checked, outcome
`activated` requires presented digest per decision Q2; reuses G1's routines.

## Risks
- R2: the outcome field changes the agent wire contract (H); acceptable while `executorEnabled=false`.
- R5: `AssignRouteHostname` can move the head while blocked -> `activated` refused `route_moved`, only
  `not_activated` remains. Pre-existing; recorded.
- Recurring hazard: a resolution that frees the attempt while leaving any flag set, or closes the op while the
  attempt stays leased (sweeper then runs the ordinary close) — prerequisite 1. Every test asserts all six rows.

## Owner decisions (2026-10-19)
- Q1 `not_activated`: attempt `leased -> expired` (failure_class/result NULL), job `leased -> failed`. No migration.
- Q2 agent `activated` (G2): presented digest must equal `op.route_digest`; mismatch -> `ErrAgentLeaseIntegrityConflict`,
  nothing written.
- Q3 `not_activated`: candidate release stays `ready` (matches `Complete(failed)`); releases not locked on that path.
- Q4 audit: keep action `environment_mark_reconciled`, reason stored as `outcome=<x>; <reason>`, `validReason`
  (<=512 bytes, no controls) applied to the COMBINED string.

## G2 detailed plan (2026-10-19, after G1 `d359810`)

**Deviation from the outline (orchestrator call):** a NEW method `ReconcileActivation(ctx, serverID,
presentedDigest, environmentID, attemptID, fence, outcome, outcomeDigest)` instead of a branch inside `Reconcile`.
Why: adding branch B inside `Reconcile` removes its incidental protection (`attemptState == "leased"` denial) in the
same edit that must replace it; a separate method leaves `Reconcile` protected while the new path is proven, mirrors
G1 (`ResolveActivation` beside `MarkReconciled`), and keeps G2 repository-only. Step H rewires the agent endpoint
to the new method (wire gains `outcome`) and retires the old one.

### G2a scope
1. **`Reconcile` hardening (prerequisite 2), behaviour otherwise identical:** on its existing closed-attempt path,
   an unresolved activate op for this attempt -> `ErrAgentLeaseCorruptState` (explicit, by rule via
   `unresolvedActivationSQL`), never the generic denial and never a flag clear.
2. **Shared core:** factor G1's post-lock verify + resolve (`activated` / `not_activated` routines, summary marker,
   RowsAffected checks) so `ResolveActivation` and `ReconcileActivation` call ONE implementation (design D5/D10).
   `ResolveActivation` behaviour must stay identical (its tests unchanged and green).
3. **`ReconcileActivation`:** lock order creds SHARE (`lockActiveServerCredential`) -> slot -> job -> attempt -> op
   -> deployment -> head -> releases; clock after locks. Accepts all three outcomes:
   - `no_activation`: today's `Reconcile` semantics (closed attempt, slot flagged by it, no unresolved op).
   - `activated` / `not_activated` (branch B): attempt `leased`, `closed_at IS NULL`, `server_id == serverID`,
     `environment_id` and `fence` match the request and `op.fence`; slot `reconciliation_attempt_id == attempt`;
     op exactly `reconciliation_required`. Authority deliberately NOT checked (it has usually lapsed).
   - `activated` requires `outcomeDigest == op.route_digest` (owner decision Q2); mismatch ->
     `ErrAgentLeaseIntegrityConflict`, nothing written.
   - Revoked server: credential lock fails -> denied; operator path is the only way out (by design).
   - No server audit row (today's agent `Reconcile` writes none); events as G1 (`reconciled` last).
   - Any failure: rollback, no new block.
4. **Idempotency:** the attempt is explicit, so a repeat is anchored to IT: attempt closed + `reconciled` event for
   it + outcome agrees with its op (for `activated`, digest agrees too) -> nil; disagreement -> integrity conflict;
   no event -> denied. Nothing written.

### G2a tests
Post-state matrix {activated, not_activated} x {tx2 failure, ambiguous success, sweeper expiry} (revoke: agent is
refused; operator path covered by G1); digest mismatch refused; refusal matrix (wrong server, env, fence, foreign
slot flag, `switch_authorized`, outcome kind both ways, unknown outcome, revoked credential); `Reconcile` explicit
check (closed attempt + unresolved op, raw SQL) -> corrupt, nothing written, plus a mutation proving it is the
explicit check and not the old incidental denial that refuses; end to end: block -> agent resolves -> new Claim +
Activate; races: agent vs operator `ResolveActivation`, agent vs sweeper, agent vs tx2 late report, all queued on
the slot with waiter confirmation; mutation proofs on every guard.

### G2b (separate step, after G2a): trace a late success against a declared `not_activated`
tx2's `failed` arm answers `integrity_conflict` with no record while traffic may have switched. Append ONE
`activation_late_report` event per attempt after the `reconciled` event (dedup: none with a higher sequence than
that attempt's `reconciled` event), commit, still answer `integrity_conflict`. Op row untouched (closed).
