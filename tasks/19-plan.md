# Step 19 — admin manual deploy: plan, 2026-11-13, awaiting owner approval

Why: the acer has no public address, the GitHub App's webhook is inactive, and deployments are created only from
verified push webhooks. Owner chose an admin-only "deploy" action. ADR 0004:80-95 already anticipates "Manual
redeploy uses a distinct authenticated deployment intent". Gates stay false in committed code.

## Design (from code trace; file:line in the design report)
- Same authority chain as a push: `lockWebhookAuthorityForPush` (webhook_job_repository.go:472) FOR SHARE locks
  app registration (active) -> installation (active, not suspended) -> repository (active, accessible) -> project
  binding (active, deploy_ref, environment_id/server_id set). Manual path adds `owner_id = principal.OwnerID`.
- Environment/server/ref come from the binding — the form only picks the project. Never accept owner,
  installation, environment or ref from the form.
- Commit: resolved BEFORE the tx with an App installation token narrowed to Contents:read on that one repo (same as
  `MintCheckoutToken`), revoked on every path, never logged: branch head of the bound ref (default), or a given
  40-hex SHA that must exist AND be reachable from the bound branch (compare API). No new App permissions.
- New table `manual_deployment_intents` (migration 000023): actor user, owner, binding, repo, ref, sha,
  request_nonce, deployment_id UNIQUE, created_at; UNIQUE(owner_id, request_nonce); immutable (no update/delete
  triggers). It is also the audit record. `deployments`/`agent_jobs` unchanged.
- One tx: lock chain -> in-flight check (non-terminal deployment for same environment + sha -> 409) -> intent +
  deployment + event + agent job + job event, via a helper extracted from `enqueueVerifiedPushTx` (no webhook
  behaviour change). Deterministic ids from domain "autodeploy:manual:v1" + nonce: double submit is idempotent,
  a deliberate redeploy (new form render = new nonce) is allowed.
- Web: POST /deployments/manual — provenance check, CSRF, owner session, exact field count; one form per bound
  project on /projects (hidden server-generated nonce; "branch head" or "specific commit"); 303 to the deployment
  detail. Errors: 400 bad input, 403 not authorized/suspended/unbound, 409 in flight, 422 commit unknown/not on
  branch, 503 GitHub unavailable, 500 generic; never echo GitHub bodies or tokens.

## Sub-steps (one implementer each, review between, atomic commits)
- 19a migration 000023 + migration test (PG18).
- 19b githubapp ResolveBranchHead / VerifyCommit (+reachability) — httptest unit tests (narrowing, revoke on all
  paths, 404/422/403/rate limit, path escaping). Parallel with 19a.
- 19c store: extract shared persistence helper (webhook tests unchanged, green) + EnqueueManualDeploy with
  owner-scoped lock chain, in-flight 409, nonce idempotency, cross-owner denial with zero rows, injected-failure
  rollback, real lock contention vs concurrent installation disable. Full PG18 integration + concurrency stage.
- 19d web route + projects form + wiring in cmd/controlplane; web tests (CSRF, provenance, non-owner, field count,
  each error mapping) + one PG18 end-to-end.
- 19e ADR 0002 + 0004 amendments, deploy/host README note (webhooks inactive supported).

## Risks
Authorization bypass (mitigated: owner scope in SQL, revalidate in tx, form carries nothing authoritative);
duplicate deployments vs a real push (in-flight check + environment slots serialize); token leakage (in-memory,
revoked, sanitized errors); touching shared webhook code (refactor first, existing tests must stay green).

## Owner decisions (2026-11-13)
- Q1: branch head (default) OR a specific SHA reachable from the bound branch. Q2: plan approved.

## Progress
(empty)
- 19a migration 000023 committed 521fc0e (two FKs to bindings are separate: repo/project could in theory be different
  binding rows; harmless because 19c reads repo from the binding row; tightening needs an ALTER on bindings).
- 19b commit resolution committed 84a42fd (compare path uses per-segment escaping like the ref lookup).
- 19c store committed 82e5862: lock order registration S -> admin session/users X -> installation -> repository ->
  binding S -> advisory xact lock (env, sha) (session-first would deadlock vs DisableInstallation/BindRepository);
  replay before in-flight; ON CONFLICT DO NOTHING + re-resolve. Full PG18 + concurrency (-race x20) green.
  Review PASS WITH NOTES -> fix round after 19d: (1) required ExpectedRepositoryID/InstallationID/Ref compared in tx
  (rebind between resolve and enqueue); (2) healthy no longer blocks redeploy — only queued/in-progress
  (owner-intent: deliberate redeploy allowed); list from deployment package; (3) map only pgx.ErrNoRows to
  unauthorized, wrap others; (4) webhook+manual same commit concurrently: accepted, environment slots serialise —
  document; lows: invariant failures -> internal error, owner-scope the in-flight check, deterministic advisory-lock
  test, rebind-race test, healthy test. FOLLOW-UP (pre-existing): ReserveCheckout/revalidateTx/
  RevalidateCheckoutBinding lock FOR UPDATE OF b,r,i,a without registration first -> latent cycle vs SHARE chain.
