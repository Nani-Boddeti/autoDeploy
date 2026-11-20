# 22-7 Full-codebase audit (2026-11-20)

Scope: cmd/, internal/, migrations/, deploy/host/. Five parallel read-only audits (security, leaks, PII,
reliability, host kit + migrations). Result: no P0/P1. Findings below are medium/low hardening + reliability.
"V" = verified by orchestrator against code; others cited by auditor with file:line, verify during implementation.

## Execution order
1. F1 App DB role is Postgres superuser; append-only triggers bypassable (no TRUNCATE triggers) — V — needs D1
2. F2 Release containers run with Docker default caps, no no-new-privileges, no pids limit — V — needs D2
3. F3 Checkout Close failure never retried -> workspace fd + quota allocation stranded — V
4. F4 diagnose.sh writes as root into predictable /var/tmp path, world-readable report incl. strace buffers — V
5. F5 stage_verify: missing SHA256SUMS = warning only, unverified root binaries installed — V
6. F6 uninstall.sh --yes deletes KEKs + DB with no distinct destroy flag
7. F7 Missing indexes: release_operations(attempt_id, fence), (job_id); agent_jobs(rollback_target_release_id), (environment_id, project_id) — V (release_operations)
8. F8 No statement_timeout/lock_timeout on pool; lease sweeper has no per-tick deadline (silent wedge)
9. F9 Checkout failure ships raw git stderr line to tenant-visible log chunks — V
10. F10 install-app.sh: migration silently skipped when Postgres down / URL empty, then services restart on old schema
11. F11 Startup/cmd/migrate DB calls have no deadline; cmd/migrate log.Fatalf prints raw pgx error (DETAIL may carry row values)
12. F12 LOW batch (see section)
13. F13 Retention for append-only tables — needs D3

## Findings

### F1 DB superuser (MEDIUM, defense in depth) — V
- deploy/host/systemd/autodeploy-postgres.service:32 POSTGRES_USER=autodeploy -> image makes it superuser;
  create-credentials.sh:69 puts it in the control plane URL; no migration has CREATE ROLE/GRANT.
- Scenario: any control-plane compromise / SQL injection -> DISABLE TRIGGER, session_replication_role, COPY PROGRAM
  (host shell in PG container). Append-only/no-delete triggers on events/audit/release_operations are advisory;
  TRUNCATE bypasses row triggers entirely.
- Remedy: migration owner role (superuser, used only by cmd/migrate) + runtime role (DML only, no TRUNCATE, not
  owner of tables). New migration: BEFORE TRUNCATE statement triggers on append-only tables + GRANTs. Installer:
  create runtime role + separate credential file; control plane uses runtime URL, migrate uses owner URL.
  Existing installs (acer): upgrade path in install-app.sh (idempotent role creation).

### F2 Container hardening (MEDIUM) — V
- internal/runtime/docker/run.go:207-224 HostConfig = NetworkMode, RestartPolicy, NanoCPUs, Memory only.
- Scenario: fork bomb in a deployed image takes down the host; default caps (NET_RAW, MKNOD, ...) widen kernel surface.
- Remedy: SecurityOpt no-new-privileges, PidsLimit, CapDrop of non-essential default caps (see D2). Pin payload by test.

### F3 Checkout cleanup not retried (MEDIUM) — V
- internal/gitcheckout/checkout.go:94-117 retains fd on retryable ReleaseAndCleanup failure "for a later attempt";
  internal/agent/executor/executor.go:255-262 + executor/checkout.go:58 call it once (closeOnce) and discard error.
- Scenario: quota service slow/down at job end -> one workspace + Active allocation stranded per job until agent
  restart; no reclaim sweep found in checkoutquota.
- Remedy: on retryable failure hand the Result to a bounded retry (backoff, cap N) owned by the agent loop or cleaner;
  log class; quota service startup reclaim of Active entries with no live holder (verify ledger semantics first).

### F4 diagnose.sh root in /var/tmp (MEDIUM) — V
- deploy/host/diagnose.sh:46-48,68,154,311: predictable /var/tmp/autodeploy-diag/$TS, mkdir -p + chmod + truncate as
  root without owner/symlink check; chmod -R a+rX; strace -s 128 may capture askpass token bytes.
- Remedy: report under root-only 0700 dir (mktemp -d under /var/lib/autodeploy/diag), refuse pre-existing non-root
  path, files 0600, strace exclude read/write payloads (-e trace=!read,write or -s 0 on data syscalls).

### F5 Optional checksum (MEDIUM) — V
- deploy/host/stage-lib.sh:36-42 returns 0 when SHA256SUMS missing.
- Remedy: fatal unless explicit --insecure-no-checksum; README: transfer SUMS (or its hash) over a separate channel.

### F6 uninstall data destruction (MEDIUM)
- deploy/host/uninstall.sh:30,118,123-124,189-194: --yes without --keep-data rm -rf /etc/autodeploy /var/lib/autodeploy
  (KEKs, manifest, DB password, PG volume). Docker down -> secrets deleted, volume orphaned, warning only.
- Remedy: require --destroy-data (default keeps data); refuse when KEK files exist unless --destroy-data; abort before
  deleting secrets if volume removal fails.

### F7 Missing indexes (MEDIUM) — V (release_operations)
- release_operations only (release_id, created_at) + partial unique environment_id. Queries on attempt_id/fence:
  agent_lease_repository.go (~1158, 1416, 2499, 2948, sweeper NOT EXISTS ~3379), activation_resolution.go:517.
  agent_jobs: cleanup.go:214 (rollback_target_release_id), operations_read.go:284 (environment_id, project_id).
- Remedy: one migration with the indexes; confirm with EXPLAIN on a seeded table that the planner uses them.

### F8 DB timeouts (MEDIUM)
- cmd/controlplane/main.go:105 pgxpool.New with no runtime params; agent_lease_sweeper.go:86-108 SweepOnce on
  process ctx. Stuck row lock -> sweeper wedges silently (lease expiry + log purge stop).
- Remedy: pool RuntimeParams lock_timeout (e.g. 10s) + statement_timeout (e.g. 60s); per-sweep context.WithTimeout
  reported via onError + metric. Check lock_timeout errors (55P03) are already transient in classification (they are,
  per transient list) and that no existing test relies on unbounded waits.

### F9 Checkout error text in tenant logs (LOW-MEDIUM) — V
- internal/agent/executor/executor.go:250 Linef("checkout: failed (%s): %v", class, err); err carries git stderr last
  line (gitcheckout/checkout.go:379). Violates logship.go:323-326 contract.
- Remedy: Linef class only; %v to Diagf (journal). Test pins tenant line.

### F10 install-app.sh migration skip (LOW-MEDIUM)
- install-app.sh:270-274, 426-429: migrate skipped silently if Postgres down or database-url empty; failure after
  binaries installed. Remedy: run migrate before restart; fail loudly if skipped while restart pending.

### F11 Startup deadlines + migrate output (LOW)
- cmd/controlplane/main.go:113,125,129 signal ctx only; cmd/migrate/main.go:19-25 context.Background + log.Fatalf("%v").
- Remedy: 30s startup ctx; migrate timeout flag; print fixed message + SQLSTATE + constraint name; add cmd/migrate to
  srcguard.

### F12 LOW batch
- a. internal/githubapp/checkout.go:180 go Revoke(context.Background()), error swallowed -> log class, bounded ctx.
- b. internal/githubapp/checkout.go:37-39 clone URL prefix/suffix check -> url.Parse, host github.com, no user/query/
  fragment, path regex.
- c. internal/web/web.go:1949-1973 no Strict-Transport-Security (add unless Traefik sets it; check traefik config).
- d. internal/agent/http enroll: no rate limit; FOR UPDATE on server row before cheap checks
  (server_repository.go:329-378) -> cheap checks first, simple per-server-id limiter.
- e. internal/agent/state/state.go:285-318 stale .fence-*.tmp never swept -> sweep at open.
- f. internal/runtime/docker/builder.go:128-144 uncapped buffers for buildx ls/inspect -> capped writer.
- g. internal/store/postgres/deployment_log_purge_scheduler.go:208,219 Hijack().Close(context.Background()) -> short ctx.
- h. systemd: agent ProtectSystem=strict + ReadWritePaths; CapabilityBoundingSet on postgres/traefik units (test on acer).
- i. Literal (non-secret) bindings: UI warning that literals are visible (config page + argv). Pairs with 22-3 UI.

### F13 Retention (needs D3)
- No purge for webhook_deliveries, agent_job_events, deployment_events, route_events, audit tables, agent_job_attempts,
  release_operations, release_cleanups, enrollment tokens; backup-db.sh no dump retention.

## Owner decisions
- D1 DB role split: (a) now, in 22-7 (pre-launch, cheapest time; acer upgrade path required) — RECOMMENDED;
  (b) defer post-launch with documented risk.
- D2 Container defaults: (a) no-new-privileges + PidsLimit 1024 + drop NET_RAW, MKNOD, SYS_CHROOT, AUDIT_WRITE, SETFCAP
  (keeps CHOWN/SETUID/SETGID/NET_BIND_SERVICE so common root-entrypoint images still work) — RECOMMENDED;
  (b) CapDrop ALL + add-back list (breaks images like nginx unless configured).
- D3 Retention: (a) purge webhook_deliveries + agent_job_events/route_events older than 90d via 000022 GUC pattern; keep
  audit tables + release_operations forever, documented — RECOMMENDED; (b) defer entirely, document growth.

## Rejected / out of scope
- "Cookies lack __Host- prefix" — false: web.go:30-32 already __Host-.
- Production fit (test domain defaults, private CA only, no ACME, fixed subnet) -> 22-5/22-6.
- Secret deletion/tombstones (ADR 0003:357) not implemented -> separate feature, ciphertext only.
- Literal-in-argv by design (ADR 0003); only UI warning (F12i).
- Windows build (not a target).

## Confirmed clean (do not re-audit)
- Web: exact-method router, provenance + session CSRF (hmac.Equal) on every POST, strict forms, html/template, CSP,
  __Host- cookies Secure/HttpOnly/Strict, session digests, idle/absolute expiry, login throttle (pair/user/IP),
  trusted-proxy XFF, enrollment token shown once + no-store.
- Agent API: constant-time digest auth, every repository path scoped by server_id, checkout re-authorization + one-start
  token registry, single-use expiring enrollment tokens.
- Webhook: HMAC before parse, MaxBytesReader, secret ring. GitHub client fixed origin, no redirects (no SSRF).
- No SQL string building. Owner scoping on operations/logs/rollback/manual deploy/runtime config.
- Traefik writes: Lstat dir checks, O_EXCL|O_NOFOLLOW, fsync+rename, strict hostname charset, quoted YAML.
- Build: argv only, secret names validated, secrets via --secret env, Dockerfile path symlink walk.
- Crypto: AES-256-GCM random nonce, full AAD binding, zero-key reject. Askpass via pipe fd.
- Logging/redaction: agent RedactingWriter before logship + server sanitize; audit rows store ids only; metrics label
  allow-list, loopback listener; backup-db.sh 0700/0600, umask 077.
- Feature flags executorEnabled/activationEnabled: no bypass path. Atomic state writes (fence, activation, traefik).
  Agent + control plane shutdown joins all goroutines. External call timeouts (docker, agent client, GitHub 10s).
  Log shipper byte-budgeted. Migration runner single tx + advisory lock + checksums.
- Host kit: set -euo pipefail, quoting, mktemp 0700 staging, ELF check, secrets 0600, Traefik dashboard off, digest
  pins, PG on 127.0.0.1, control plane + quota units strictly hardened.
- Not examined in depth (no verdict): checkoutquota request handling, checkout-exec handlers, cmd/admin recovery,
  RedactingWriter split-write edge cases, per-site tx.Rollback across ~35 Begin sites.

## Implementation workstreams (disjoint files, after approval)
- WS-A migrations: 000030 indexes (F7) + TRUNCATE triggers + runtime role grants (F1 DB half) [+ F13 purge if D3a].
- WS-B host kit: diagnose.sh (F4), stage-lib.sh (F5), uninstall.sh (F6), install-app.sh (F10 + F1 role/creds),
  create-credentials.sh, systemd units (F1, F12h). Depends on WS-A role names.
- WS-C docker runtime: run.go (F2), builder.go (F12f).
- WS-D agent: executor.go + executor/checkout.go (F3, F9), agent/state/state.go (F12e); quota reclaim if needed.
- WS-E control plane: cmd/controlplane/main.go (F8, F11), agent_lease_sweeper.go (F8), cmd/migrate (F11),
  deployment_log_purge_scheduler.go (F12g), githubapp/checkout.go (F12a, F12b).
- WS-F web/agent http: web.go HSTS (F12c), enroll limiter + server_repository.go ordering (F12d).
- Each: fail-first test or mutation evidence, package tests + integration sweep where SQL; then shell-capable
  independent review; acer proof for F1, F2, F4-F6, F10, F12h.

## Approval (owner 2026-11-20)
- D1 (a) split now; D2 (a) compatible set; D3 (a) purge 90d (webhook_deliveries, agent_job_events, route_events), keep
  audit + release_operations; plan approved, implementation via Sonnet subagents, no commits until review.
- Orchestrator refinement: drop BEFORE TRUNCATE triggers. Real boundary = runtime role lacks TRUNCATE/ownership; test
  fixtures TRUNCATE tables, so triggers would need a bypass GUC (advisory again) for no gain.
- Sequencing: WS-A, WS-C, WS-D, WS-E(F8/F11/F12a,b,g), WS-F in parallel (own PG containers: A 55441, E 55442, F 55443);
  then WS-B (needs WS-A role name) and WS-E2 (F13 purge Go job + main.go wiring, needs WS-A GUC/migration).

## Review round 1 (independent, 2026-11-20): FAIL -> fixes
- H1 cmd/admin/main.go:40 runs migrations.Apply on runtime-role URL -> admin bootstrap/reset-password break (V). Fix: admin
  never migrates; read-only schema-current check; test as autodeploy_app.
- Owner: M1 stop purging webhook_deliveries (replay protection; keep forever); M3 HSTS max-age=86400 (raise in 22-6);
  M5 PidsLimit 4096. Also fix M2 (limiter charges failed attempts only, verified digest never throttled), M4 (restart
  marker after URL swap), L1 (two un-retried checkout Close paths), L3 (web 55P03/57014 -> 503), L4 (uninstall inspect).
- Accepted: L2 (slog default set in mains), L5 (migrate as root), L6 (default privileges bound to migrating role), L7.

## Review round 2 + result (2026-11-20)
- Round 2: PASS WITH NOTES (0 HIGH/MEDIUM). Shell LOWs fixed: temp runtime-URL file removed on failed swap (scoped trap,
  owner-approved), marker cleared only after control plane stays active 5s (30s bound), marker+PG down -> die.
- Mac verification: make check green; integration sweep (store, agent/http, web, migrations, cmd/...) green on fresh
  PG18; race x10 on new enroll/limiter/close-retry/sweeper/retention tests green; GOOS=linux build green.
  TestConcurrentCallsOnOneClient (docker) flakes only under heavy parallel load (200 dials vs somaxconn 128); passes
  idle at HEAD and tree.
- Follow-ups: shell test harness for install/uninstall (M4/L4 have stub tests only); Checkout wiring test for L1
  (needs fakeable gitcheckout.Result); CheckCurrent NULL-checksum + non-integration test; per-IP/edge limiter for
  enroll floods (22-5/22-6); stale marker after manual migrate (one extra restart); quota-side reclaim of stranded
  Active allocations (F3 remainder); retention metric counter; HSTS 1y in 22-6; existing containers keep old HostConfig
  until redeploy.
- Acer proof pending: install upgrade (role split, URL swap, marker), runtime role refusals, container hardening on a
  real deploy, agent ProtectSystem=strict, empty CapabilityBoundingSet on postgres/traefik, diagnose.sh, checksum
  enforcement, uninstall default keep-data.
