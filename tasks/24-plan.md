# Plan 24 — Self-contained onboarding (Coolify-style install, no SaaS)

Date: 2026-11-21. Status: APPROVED 2026-11-21 (D1–D5 below). Build order: 24-C → 24-D → 24-A → 24-B → 24-F → 24-E → 24-G.
Decision context: AutoDeploy stays self-contained (control plane + agent on the owner's VPS); hosted/multi-tenant deferred.

## Goal

A non-technical owner buys an Ubuntu 24.04 VPS, points a domain at it, runs **one command**, finishes setup **in the browser**,
creates the GitHub App **with one click**, and can add **extra servers with one command** per server.

Target user journey:
1. `curl -fsSLo install.sh https://raw.githubusercontent.com/Nani-Boddeti/autoDeploy/<tag>/deploy/host/install.sh && sudo bash install.sh` (fetched from a git tag ref, not a release asset; the installer embeds the signing public key and verifies the release before using it)
2. Installer asks: control plane domain, ACME email, open firewall? (reads answers from /dev/tty)
3. Installer installs Docker + everything, starts services, prints `https://<domain>/setup` + one-time setup code
4. Browser: enter code, create admin → "Create GitHub App" → install it → bind repo → push deploys
5. Extra server: Servers page → "Add server" → copy the one-line agent command → paste on new VPS

## Facts this plan rests on (from exploration, 2026-11-21)

- No release/download mechanism; binaries built on the dev mac, scp'd, verified via SHA256SUMS sent out of band. Repo is PUBLIC (GitHub Releases downloadable without auth).
- No `/etc/os-release` check anywhere; Docker checked only by `command -v docker` + `systemctl is-active`.
- Interactive TTY steps: firewall SSH-port confirm, `admin bootstrap` password, `agent enroll` token, `enable-github.sh` KEK init (`[[ -t 0 && -t 1 ]]`). `curl | bash` breaks all of these (stdin is the pipe) → download-then-run, prompts via /dev/tty.
- `admin bootstrap` is terminal-only **by ADR 0006** ("no public bootstrap ... not available over HTTP"). Bootstrap tx: `installation_auth` singleton row `FOR UPDATE`, owner_id NULL until bootstrap.
- One-time-token pattern exists: enrollment tokens `enr.v1.`+32B, SHA-256 digest stored, `consumed_at`, immutability triggers, 900s TTL.
- GitHub App config = root-installed files + `/etc/autodeploy/controlplane-github.env`, loaded once at startup; restart required. ADR 0002: mounted files, no UI/API credential storage. App creation not implemented; install/bind flow exists (`internal/githubapp/setup.go`).
- `AUTODEPLOY_PUBLIC_ORIGIN` (HTTPS) known to the control plane → manifest redirect/webhook URLs derivable.
- `install-app.sh` always installs both roles. Agent needs locally: checkout kit, Docker+buildx, own Traefik (API at 127.0.0.1:8080 hardcoded), route dir, Traefik API creds. Agent → CP is outbound HTTPS only (ADR 0001 allows separate hosts). Public mode writes an `/etc/hosts` block mapping CP host → 127.0.0.1 (must NOT be applied on a remote agent host).
- Committed gates `executorEnabled`/`activationEnabled` = false → a release build cannot deploy until the gates are flipped.

## Sequencing (dependency)

Owner decision D5: build plan 24 now; all public-VPS proofs (remaining ~8%, gate flip, 24-F/24-G proofs) run together once the VPS is bought. Release builds stay gate-disabled until that round.

## Slices (one subagent per slice; Sonnet implements → fresh review → fix → orchestrator verify → Haiku commit)

### 24-A Release pipeline + signing
Status 2026-11-21: **implemented; review 1 FAILED, fixes applied (C1 C2 H1-H3 M1-M7 + lows), pending re-review** (nothing committed). Files: `.github/workflows/release.yml` (jobs build / dry-run / sign[environment `release`] / publish[`contents: write`]), `deploy/host/package-release.sh`, `verify-release.sh`, `release-signing/allowed_signers` (PLACEHOLDER, canonical source that 24-B embeds), `tests/verify_release_test.sh` (also a step in ci.yml), `docs/operations/releases.md`. The kit carries NO trust anchor (no verify-release.sh, no allowed_signers); `--allowed-signers` and `--expect-version` are mandatory; a signed `VERSION` asset binds the tag; dry runs sign with a throwaway key and never read the real secret; publishing uses `gh release create` (no third-party action); static git is built fresh (no cache). Release builds `--gates disabled`.
- Owner actions (final list, do none until the VPS round is near; none performed): (1) generate the key (`ssh-keygen -t ed25519 -C release@autodeploy`); (2) create environment `release` with required reviewer = owner and deployment refs restricted to `v*` tags; (3) `gh secret set AUTODEPLOY_RELEASE_SIGNING_KEY --env release < key` (environment secret, no repo secret); (4) commit the real public key into `allowed_signers`; (5) publish the fingerprint (`ssh-keygen -lf`) in README/release notes out of band; (6) tag ruleset on `v*` (restrict creation, block update and delete); (7) protect `main`; (8) offline key backup, delete the local copy only after confirming it. Commands: docs/operations/releases.md.
- Deferred to the VPS round (not proven): first real tagged release on GitHub, environment approval flow, `gh release create` path, fresh static-git build in CI, `release` job re-verify of the downloaded artifact on Actions, signature check of a downloaded release on a real host, gate flip before the first production release.
- Residual risk: repo write access is the root of trust (install.sh is fetched from a git ref and embeds the key): protect `main` and `v*` tags with rules.
- GitHub Actions workflow on tag `v*`: build static linux/amd64 binaries (`build-host-binaries.sh --gates disabled|enabled per decision`), static git, `deploy/host` kit tarball, `SHA256SUMS`, signature.
- Signature: **`ssh-keygen -Y sign/verify`** (recommended; openssh is preinstalled on Ubuntu → no extra tool on the host). Public key pinned inside `install.sh`; private key = GitHub ENVIRONMENT secret (D1; environment `release`, required reviewer).
- `install.sh` is NOT a release asset (a same-channel asset could be swapped together with the key): it is fetched from the git tag ref; its trust is the repo/tag protection plus the published key fingerprint (README/release notes; ADR/README updated).
- Tests: workflow dry-run on a fork tag; verify script rejects tampered tarball, wrong key, missing signature.

### 24-B Bootstrap installer `install.sh` (single host)
Status 2026-11-21: **implemented; review 1 FAILED (H1 testing flags could replace the trust anchor on a release; M2-M6, L7-L13), fixes applied, pending re-review** (nothing committed). Review fixes: testing flags (`--allow-dev --allowed-signers --yes-i-am-testing`) are hard errors on release versions and dev builds need a typed or flag confirmation; Docker must be docker-ce at /usr/bin/docker; Docker apt key pinned (fpr 9DC858229FC7DD38854AE2D88D81803C0EBFCD88); apt lock timeout; choices persisted in /etc/autodeploy/installer.env; postgres wait dies with a hint; kit copy atomic and before enrollment; PATH/LC_ALL/TMPDIR fixed (stub PATH hook only for dev builds + AUTODEPLOY_INSTALL_TEST_PATH). Files: `deploy/host/install.sh`, `tests/install_sh_test.sh` (also a ci.yml step; includes key-drift, release.yml guard extraction, verify-logic equivalence with verify-release.sh, ubuntu:24.04 container runs against a fixture release built by the real package-release.sh), `tests/enable_github_kek_test.sh`. Small hooks in existing code: `enable-github.sh --kek-only` (KEK + manifest + ledger without GitHub inputs, no restart, minimal env file when none exists) and `cmd/secrets` `init-active-kek` no longer requires a terminal (it reads no secret; same precedent as `verify`). Choices: version recorded in `/etc/autodeploy/installed-version`; dev builds and the PLACEHOLDER key refused unless `--allow-dev` (+ `--allowed-signers` for tests); DNS mismatch/no record = warning needing `--yes` or a terminal confirm, NAT = info only; log `/var/log/autodeploy-install.log` 0600 with no setup code (the code goes to the original stdout only); verified kit copied to `/usr/local/share/autodeploy/kit` so uninstall/preflight/enable-github remain available (not in the spec: owner may veto); exit 4 = installed but browser setup not finished; readyz polled over bash /dev/tcp so curl stays https-only.
- Deferred to the VPS round (not proven): real apt/Docker CE install from Docker's repo and its key over the network (the pinned fingerprint was read from the live key with gpg 2.4 in ubuntu:24.04, not from docs.docker.com/engine/install/ubuntu, which prints none; cross-checked 2026-11-21 against a second independent source, keyserver.ubuntu.com `pks/lookup?op=get&search=0x9DC858229FC7DD38854AE2D88D81803C0EBFCD88`: gpg --show-keys gives the same primary fpr 9DC858229FC7DD38854AE2D88D81803C0EBFCD88, uid Docker Release (CE deb) <docker@docker.com>, plus the same signing subkey D3306A018370199E527AE7997EA0A9C3F273FCD8); real `dpkg-query`/`dpkg --print-architecture`/gpg behaviour and the apt lock timeout under unattended-upgrades/needrestart; kit copy ownership on a real fs; re-run after a partial real install reading installer.env; real `curl` download from GitHub Releases (redirects to objects host under `--proto-redir =https`); `ssh-keygen -Y verify` with the real release key; real systemd/cgroup/ss/dig/ip behaviour of the preflight (container stubs only); DNS check on a real public/NATed host; real kit scripts end to end in the installer order (kit scripts were stubs in the harness), including postgres readiness before migrate, `enable-github.sh --kek-only` against real Postgres and a real `secrets` binary, and the control plane starting with the secret subsystem enabled but no GitHub App; `/readyz` wait; the real `/setup` + `enroll-local.sh --wait` hand-off; firewall prompt and `--yes-firewall-ssh-port` with a real ufw/sshd; port-holder reporting on a real host; upgrade rerun over a real install (install-app upgrade path, ACME staging/production guard); interactive prompts on a real tty (only a `script` pty is exercised); `uninstall.sh` against an installer-made host.
- Preflight: root, x86_64, `/etc/os-release` = Ubuntu 24.04 (clear refusal otherwise), systemd, cgroup v2, ≥2 vCPU/2 GB/40 GB free (30G checkout image), ports 80/443 free, DNS of domain resolves to this host (reuse public preflight).
- Installs Docker CE + buildx from Docker's official apt repo (skip if present & active; refuse `docker.io`/snap docker with a clear message); apt deps (apache2-utils, openssl, curl, xfsprogs, strace, dnsutils).
- `install.sh` is fetched from `https://raw.githubusercontent.com/Nani-Boddeti/autoDeploy/<tag>/deploy/host/install.sh` (git tag ref, not a release asset). It embeds the `allowed_signers` lines literally and the verify logic (or fetches `verify-release.sh` from the same git ref), writes the key to a 0600 file in a private `mktemp -d`, ALWAYS passes `--allowed-signers` and `--expect-version <tag>`, verifies BEFORE extracting or executing anything from the release, and prints the key fingerprint. Residual risk: repo write access is the root of trust, so protect `main` and `v*` tags with rules.
- **24-B tightening (review 2):**
  - Version: `install.sh` carries `AUTODEPLOY_VERSION="vX.Y.Z"` as a committed constant. The release `build` guard (already in release.yml; skips while install.sh is absent) fails when that constant != the pushed tag.
  - Downgrade: refuse to install an older version over a newer installed one unless `--allow-downgrade`.
  - Verify logic is embedded in `install.sh`, not fetched.
  - Downloads: `curl --proto '=https' --tlsv1.2 -fsSL` only; asset names derived from `AUTODEPLOY_VERSION`, never globbed; into a root-owned 0700 `mktemp -d`; extraction only after verification, from that dir, refusing absolute or `..` member paths.
  - Key drift: the embedded key block must byte-equal `deploy/host/release-signing/allowed_signers`; add this CI check in 24-B.
- Downloads release → verifies signature + SHA256SUMS (`verify-release.sh`) → runs existing scripts in order: `install-checkout.sh`, `install-app.sh --mode public`, `create-credentials.sh`, start postgres/traefik, `migrate`, start controlplane. Existing scripts stay the source of truth (wrap, don't rewrite).
- Prompts via `/dev/tty`; every prompt also has a flag for unattended use (`--domain --email --firewall yes|no --yes`).
- KEK init done here non-interactively-but-root (new `secrets secret init-active-kek --installer` path or run with /dev/tty) so 24-E needs no TTY later.
- Idempotent rerun = upgrade (existing upgrade banner); refuses lan installs.
- Ends by generating the setup code (24-C) and printing URL + code; then waits for admin creation and runs 24-D.
- Tests: extend `script_structure_test.sh` container harness (ubuntu:24.04 + stubs): OS refusal (debian, 22.04), docker.io refusal, flag-only unattended run, rerun-as-upgrade, signature failure aborts before any change.

### 24-C Browser first-run setup (amends ADR 0006)
Status 2026-11-21: **implemented and reviewed (PASS after fixes M1 M2 L1-L3)**; unit + Postgres integration + race proven on the mac; host proof deferred to the VPS round. Migration 000032; `/setup` + `admin setup-token`; ADR 0006 amendment; control plane now starts pre-bootstrap by calibrating the password policy.
- New table `installation_setup_tokens` (digest only, `expires_at` 60 min, `consumed_at`, immutability triggers like enrollment tokens). Format `setup.v1.`+32B.
- Token minted by a root-only host CLI (`autodeploy-run admin setup-token`), printed to the installer terminal only. Possession of the code proves host access → keeps ADR 0006's trusted-host intent; amend ADR 0006 text accordingly.
- `/setup` route: exists only while `installation_auth.owner_id IS NULL`; 404 forever after bootstrap. Form: code, username, password ×2. Same tx: lock singleton row, verify+consume token, `Bootstrap`, audit row `bootstrap_web`. Durable throttling like `/login`; CSRF via login-style stateless envelope; same CSP (no scripts needed).
- Terminal `admin bootstrap` stays as the alternative path.
- Tests: consumed/expired/wrong code refused; concurrent double submit → exactly one admin (real lock contention test); `/setup` 404 after bootstrap; token never logged; throttling.

### 24-D Local agent auto-enroll (single host)
Status 2026-11-21: **implemented and reviewed (FAIL → fixes M1–M4, L5–L11 → PASS; final hardening: lock path, token.agent pre-plant guard, exit codes 20–30).** Proven locally only (Go unit, Postgres integration as the runtime role, ubuntu:24.04 container harness with stubs); no real host.
- Built: `autodeploy-run admin server-enroll-local --token-file P [--name N] [--server-id ID] [--wait D]` (cmd/admin, runtime DB role, no TTY) + `deploy/host/enroll-local.sh [--wait D] [--name N]`. Differs from the original sketch (`server enroll-local`): lives in `admin` because that is the existing runtime-role, root-wrapped entry point (`autodeploy-run admin`).
- Shared code: `registerServerTx`/`issueEnrollmentTx` extracted from the UI path (`RegisterServer`/`IssueEnrollment`); same validation, token constructors, digest, TTL (`server.EnrollmentTTLSeconds`=900, now also used by web) and audit rows. New host-trusted repo methods resolve the owner from the database instead of a browser session.
- Audit actor: `administrator` + the owner's single active 'owner'-role user id (identical shape to UI registration; no constraint changed).
- Local-server identity (no schema): recorded `AUTODEPLOY_AGENT_SERVER_ID` in `/etc/autodeploy/agent-local.env` is authoritative (script passes `--server-id`); with none recorded, the newest active server with the exact name whose token is unconsumed and has no credential is reused (crash recovery); an enrolled same-name server is never adopted. Enrolled + credential -> mint nothing; enrolled in DB but no host credential -> script stops, points to UI Re-enroll.
- Token handling: CLI creates the file first (O_EXCL|O_NOFOLLOW, 0600, absolute clean path) and removes its own file if a later step fails; script uses a root-made 0700 dir in `/run` (tmpfs), chowns the dir to the control-plane user only while the CLI writes, takes it back to root, verifies regular 0600, hands file+dir to the agent user, and always removes both via trap.
- Review round 1 fixes (M1-M4, L5-L11): script never chowns the control-plane-created inode (verifies owner/nlink 1/0600/size, copies into a root-created agent-owned file, discards the original) and the CP user owns the temp dir only during the write call (admin wait is a separate `--wait-only` call); exit codes 20 no admin, 21 needs re-enroll, 22 recorded id revoked/removed, 23 admin disabled, 30 already enrolled (chosen outside Go's panic code 2 and the shell's 126+; others = generic failure) (id printed, no token; script refuses any file present); script flock on root-only `/run/autodeploy-enroll-local.lock` (symlink and non-root-owned refused; token.agent pre-plant refused) + DB `pg_advisory_xact_lock` (key 0x4144454e4c4f4341 "ADENLOCA"; other advisory keys in the repo: fixed 7314928456316941{22,23,24}, ASCII agnt_lp3/agnt_ls2/agnt_rt3, hashtextextended keys in manual_deploy_repository.go; no equality with the fixed/ASCII ones) around find+register/issue in one tx; local registrations carry audit reason `local-enroll` and only those are adopted by name; stale `/run/autodeploy-enroll.*` sweep; strict agent-local.env reads.
- Deferred to the VPS round (not proven): real `autodeploy-run` runuser as the control-plane user writing into the /run dir and root re-chown; real `agent enroll --token-file` as the agent user against a live control plane (ownership/0600 check in `ReadProtectedFile`); `/readyz` on the real gateway address; systemd start of the agent with the credential; real `preflight-app.sh`/`preflight-checkout.sh` after auto-enroll; `--wait` while the owner completes browser `/setup`; lan-mode (private CA) agent trust during auto-enroll; real flock/`/run` semantics, hard-link behaviour with the real fs.protected_hardlinks, and `find -user` sweep on a real host; installer (24-B) wiring that calls `enroll-local.sh --wait` after printing the `/setup` URL (24-B not built yet).
- After admin exists, installer runs new root CLI `autodeploy-run server enroll-local`: registers "this server" under the owner, mints an enrollment token, feeds it to `agent enroll --token-file` (0600 tmp file, shredded), starts agent, runs preflights. Token never displayed.
- Tests: rerun is idempotent (already enrolled → no-op); failure leaves no consumed-but-unused credential.

### 24-E GitHub App manifest flow (amends ADR 0002)
Status 2026-11-27: **implemented locally (E1-E5), uncommitted; real-GitHub proof pending (VPS checklist step 10).** Storage changed from the spool/root-unit design below to DB + self-restart; see tasks/24-E-plan.md and the ADR 0002 amendment. The spool/`github-apply` bullets below are superseded.
- `/github/create` (admin): choose personal account or org → POST manifest to GitHub (`name`, `url`=origin, `hook_attributes.url`=origin/webhooks/github, `redirect_url`=origin/github/create/callback, `setup_url`, `public:false`, permissions contents:read + metadata:read, event `push`) with one-time `state`.
- Callback: exchange `code` via `POST /app-manifests/{code}/conversions` → App id, slug, client id, PEM, webhook secret.
- Persist without giving the control plane write access to `/etc/autodeploy`: CP writes the result as a staged bundle into a CP-owned spool dir (0700, encrypted with the KEK); a root `systemd .path` + oneshot (`github-apply`) validates and installs files exactly as `enable-github.sh` does, then restarts the CP. UI shows "applying…" then continues into the existing install flow (`/github/setup`).
- `enable-github.sh` is demoted to a break-glass appendix (D3); the `/github` page offers only "Create GitHub App" when no App is configured.
- Verify the manifest API details against current GitHub docs at slice start (no hardcoding from memory).
- Tests: state replay/expiry refused; malformed conversion response rejected; oneshot refuses symlinks/wrong owner/bad PEM; partial apply rolls back; secrets never logged.

### 24-F Agent-only installer for extra servers
Status 2026-11-21: **implemented, pending review** (nothing committed). Proven locally only (Go unit tests, ubuntu:24.04 container harness with stubs); no real host, no second host.
- Built: `install-app.sh --role all|agent [--control-plane-host FQDN]` (agent = agent binary/unit, Docker network, Traefik with the ACME resolver, route dir; no controlplane binary/user/unit, postgres, migrations, controlplane.env, CP route, `/etc/hosts` block; agent unit rendered without `autodeploy-controlplane.service` in `After=`; `AUTODEPLOY_ROLE=agent` recorded in host.env, absent for `all` so its output is byte-identical; public mode only (D4); role switch and a different control plane host refused); `create-credentials.sh --traefik-only` (plain form refused on an agent host); `preflight-agent.sh` (new kit file; `preflight-app.sh` on an agent host says to use it); `install.sh --role agent --control-plane-host FQDN --server-id ID [--token-file FILE]` (readiness check of `https://<cp>/readyz` before changes and again right before the token prompt; token via hidden `/dev/tty` prompt or root-owned 0600 file, copied to a 0600 agent-owned file in a root-made `/run/autodeploy-enroll.*` directory, removed on success and failure; server id recorded in agent-local.env; no DNS comparison of the CP host with this host; rerun of an enrolled server asks for nothing); Servers page "Add server" + token page with the two commands, DNS reminder and the token shown once; bind-form DNS hint. `uninstall.sh` needed no change (proven on an agent host).
- Version in the download URL: the control plane binary carries no release tag (`buildVersion` is the VCS revision), so the page shows the literal `<version>` and says to read the tag from `/etc/autodeploy/installed-version` on the control plane host (written by install.sh). No new version mechanism was added.
- Deferred to the VPS round (not proven): **the two-host end to end: bind an environment to server 2, push, healthy deploy and route on server 2** (agent on server 2 polling the real control plane, Traefik on server 2 obtaining the app certificate by ACME HTTP-01, app DNS pointing at server 2); real `agent enroll --token-file` as the agent user against a live control plane over public TLS (ownership/0600 check in `ReadProtectedFile`, agent-owned file in a root-made /run directory); the hidden `/dev/tty` prompt on a real SSH terminal (only a `script` pty is exercised); real `https://<cp>/readyz` through Traefik and Let's Encrypt trust from a second host; `create-credentials.sh --traefik-only`, `install-app.sh --role agent` and `preflight-agent.sh` with real systemd/Docker/ss/ufw (container stubs only); the Traefik image pull and the wait for its management API on first start; `agent` unit start without the control plane unit on a real host; a token that expires during a slow Docker install (15 min TTL; the prompt comes after the install on purpose); ufw rules on the agent host (no Traefik->control plane rule); `uninstall.sh` on a real agent host; the Servers-page copy/paste of the two commands in a real browser.
- Residual risks: `preflight-agent.sh` duplicates checks of `preflight-app.sh` (drift risk, chosen over gating ~15 control plane blocks in a 500-line script); `<version>` must be filled in by hand; a re-enroll of an existing host still uses the manual command (the token page keeps it).
- `install-app.sh --role all|agent` (default `all`, golden output unchanged); `install.sh --role agent --control-plane https://<cp> --server-id <id>`.
- Agent role installs: checkout kit, Docker, Traefik (+ its API creds via `create-credentials.sh --traefik-only`), agent unit, network. Skips: CP binary/user, postgres, migrations, controlplane.env, `/etc/hosts` CP block. Agent unit drops `After=controlplane`.
- Servers page: "Add server" → shows the one-line command with server id; token entered at a hidden `/dev/tty` prompt (never in the command line / shell history).
- Public mode only (remote agents trust system roots; lan/private-CA remote agents out of scope).
- App hostnames routed to a server must have DNS pointing at that server (UI hint on bind).
- Tests: golden `lan_mode_identical` unchanged for `--role all`; container harness for `--role agent` asserts no CP artifacts; two-host e2e (VPS + acer or two VPSes): bind env to server 2, push, healthy deploy + route on server 2.

### 24-G Docs + fresh-VPS end-to-end
Status 2026-11-21: **docs done (2026-11-21); VPS proof pending.** Docs: `docs/operations/vps-install.md` restructured around the one-command install (manual path kept as the appendix), README quick start, `deploy/host/README.md` script index, `releases.md` and IMPLEMENTATION_STATUS refreshed. ADR 0002/0006 amendments: 0006 was amended in 24-C; 0002 amended 2026-11-27 with 24-E.
- Rewrite `docs/operations/vps-install.md` around the one-liner (manual path kept as appendix); README quick start; ADR 0002/0006 amendments; IMPLEMENTATION_STATUS.
- Proof: brand-new VPS, only the one command + browser; record timings; then add a second server.

## VPS round checklist (run top to bottom; nothing here is proven yet)

Order matters: owner setup unlocks the release, the release unlocks the installer, the installer unlocks everything after. Mark each item with the date and evidence when done.

1. **Owner GitHub setup** (docs/operations/releases.md): generate the signing key; environment `release` (required reviewer, `v*` tag rule); environment secret `AUTODEPLOY_RELEASE_SIGNING_KEY`; commit the real public key (`allowed_signers` and the block in `install.sh`); publish the fingerprint out of band; `v*` tag ruleset; protect `main`; offline key backup (delete the local key only after confirming it).
2. **Buy the VPS and DNS**: Ubuntu 24.04 x86_64, domain A record, ports 22/80/443 open at the provider. Also a second host (or second VPS) for step 11.
3. **Gate flip decision** (D5): flip `executorEnabled`/`activationEnabled` in committed code for the first production release (release builds are `--gates disabled` until then); the first release must not be cut earlier.
4. **First signed release**: set `AUTODEPLOY_VERSION` in `install.sh` to the tag, dry run first, tag, reviewer approval of the `release` environment, `gh release create` path, fresh static-git build in CI, `release` job re-verify on Actions; verify the downloaded release with `verify-release.sh` on the VPS using the real key.
5. **Fresh-VPS one-command install (24-B, 24-G proof)**: only the two commands plus the browser; record timings; real Docker CE apt install and pinned key, apt lock timeout, `curl` from GitHub Releases, `ssh-keygen -Y verify`, preflight, DNS check, firewall prompt with real ufw/sshd, port-holder report, `/readyz` wait, interactive prompts on a real tty, kit copy ownership.
6. **Browser first run and local enroll (24-C, 24-D)**: `/setup` with the printed code (and a lost code via `admin setup-token`), `enroll-local.sh --wait` hand-off, real runuser/`/run` token file handling, agent start, `preflight-app.sh` and `preflight-checkout.sh` after auto-enroll, flock and hard-link behaviour on a real fs.
7. **Public ACME end to end (gate 3)**: Let's Encrypt via the installer (staging then production, per vps-install.md appendix section 4), certificate for the control plane host and for an app hostname, first deploy waiting on issuance.
8. **First deploy and the open dev checks**: GitHub App via the browser "Create GitHub App" flow (24-E; `enable-github.sh` only as break-glass fallback), bind, push, healthy deploy; **build secret mounts** (`RUN --mount=type=secret`) on a real build; blue/green and rollback on the VPS.
9. **Upgrade and downgrade**: re-run `install.sh` of a newer tag over the install; confirm the downgrade refusal; `uninstall.sh` against an installer-made host.
10. **24-E manifest flow**: manifest creation against real GitHub on the public origin (confirm manifest permission keys `contents`/`metadata`, the real conversion response, webhook active), state replay and expiry, restart under systemd (exit 75, `Restart=on-failure`, App loaded after ~10 s), env-file precedence note, and the wrong-App recovery SQL in vps-install.md.
11. **Two-host test (24-F)**: Add server on the Servers page, `install.sh --role agent` on server 2 (hidden `/dev/tty` prompt, `--token-file`, Let's Encrypt trust from a second host, `create-credentials.sh --traefik-only`, `install-app.sh --role agent`, `preflight-agent.sh`, ufw rules), bind an environment to server 2, push, healthy deploy and route with the app certificate via ACME HTTP-01 on server 2, `uninstall.sh` on an agent host, revoke on the Servers page.
12. **24h retention check**: leave the host running continuously for 24 hours, then run the retention check as defined in IMPLEMENTATION_STATUS (see the 24h retention item there).
13. **Backup/restore drill (last, owner decision)**: `backup-db.sh`, KEK files and `acme.json` to separate locations, restore per docs/operations/backup-restore.md, `secrets secret verify`, re-enroll if the credential is unknown.
14. **Close out**: update IMPLEMENTATION_STATUS (progress numbers, gates), mark the slices proven here, flip nothing else.

## Owner decisions (2026-11-21)

- **D1 Signing key custody:** CI repo secret. DECIDED.
- **D2 ADR 0006 amendment:** allow browser first-run admin creation, gated by host-printed one-time code. DECIDED.
- **D3 ADR 0002 amendment (manifest flow):** DECIDED — include 24-E; the manual App-creation path stops being the documented/default path. Not deleted: `enable-github.sh` is retired to a documented break-glass appendix (no UI link, not in the main install docs) and stays until the manifest flow is proven on the public VPS, because the acer and the VPS-round fallback still rely on it (CLAUDE.md "don't delete working code"). Removal, if wanted, is a separate decision after that proof.
- **D4 Scope of 24-F:** public-mode remote agents only. DECIDED.
- **D5 Order:** build plan 24 now; the real VPS is bought at the end and the public VPS e2e (remaining 8% + gate 3 + 24-G proof + two-host 24-F proof) runs together then. DECIDED.

### Consequences of D5
- **The acer is no longer available (owner, 2026-11-21).** No real-host checks happen until the VPS round; every host-level validation (including what earlier plans would have done on the acer) runs once, together, on the VPS.
- Proof split: everything provable without a host is proven now (unit/integration tests against local Postgres, container harness for install.sh, real-lock tests for `/setup`). Public-only proofs (ACME via installer, fresh-VPS one-liner, remote agent, manifest flow against real GitHub callback on a public origin) are deferred to the VPS round and listed as "not yet proven" — never marked done before then.
- Gates: release builds stay `--gates disabled` until the gate flip in the VPS round; installer tests use stub binaries. The gate flip + first signed release happen in the VPS round.
- Manifest flow can be partly exercised on the acer only if GitHub can reach a public origin; otherwise deferred to the VPS round.

## Edge cases

- Domain DNS not yet propagated → installer waits/retries ACME staging first, explains clearly, rerun-safe.
- Port 80/443 already used (existing nginx/apache) → refuse with what's using it.
- Low-RAM VPS (1 GB) → refuse below minimum.
- Setup code lost/expired → `autodeploy-run admin setup-token` mints a new one (only while no admin).
- GitHub conversion code expires (1 h) or user abandons → state expires; retry button.
- Agent host clock skew → existing lease/fencing; preflight warns on NTP off.
- Rerun of installer after partial failure → idempotent resume.

## Risks & mitigations

- `curl` trust → signed releases, pinned key, script hash published; download-then-run (not pipe) so the user can read it.
- Web bootstrap weakens ADR 0006 → code proves host access, single-use, short TTL, route disappears after use; terminal path retained.
- CP gaining config-write power → CP writes only to its own spool; root helper validates; no CP write to /etc.
- Regressions in existing host kit → wrap, don't rewrite; golden tests for `--role all`; full bar before each commit.
- Release builds with gates off → sequencing (D5).

## Rough size (estimates)

24-A M · 24-B M · 24-C M · 24-D S · 24-E L · 24-F M · 24-G S.
