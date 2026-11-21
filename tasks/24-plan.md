# Plan 24 — Self-contained onboarding (Coolify-style install, no SaaS)

Date: 2026-11-21. Status: APPROVED 2026-11-21 (D1–D5 below). Build order: 24-C → 24-D → 24-A → 24-B → 24-F → 24-E → 24-G.
Decision context: AutoDeploy stays self-contained (control plane + agent on the owner's VPS); hosted/multi-tenant deferred.

## Goal

A non-technical owner buys an Ubuntu 24.04 VPS, points a domain at it, runs **one command**, finishes setup **in the browser**,
creates the GitHub App **with one click**, and can add **extra servers with one command** per server.

Target user journey:
1. `curl -fsSLo install.sh https://github.com/<owner>/autoDeploy/releases/latest/download/install.sh && sudo bash install.sh`
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
- GitHub Actions workflow on tag `v*`: build static linux/amd64 binaries (`build-host-binaries.sh --gates disabled|enabled per decision`), static git, `deploy/host` kit tarball, `SHA256SUMS`, signature.
- Signature: **`ssh-keygen -Y sign/verify`** (recommended; openssh is preinstalled on Ubuntu → no extra tool on the host). Public key pinned inside `install.sh`; private key = repo secret (or owner signs locally — decision D1).
- `install.sh` published as a release asset; its own SHA256 published in README/release notes (replaces the "separate channel" rule; ADR/README updated).
- Tests: workflow dry-run on a fork tag; verify script rejects tampered tarball, wrong key, missing signature.

### 24-B Bootstrap installer `install.sh` (single host)
- Preflight: root, x86_64, `/etc/os-release` = Ubuntu 24.04 (clear refusal otherwise), systemd, cgroup v2, ≥2 vCPU/2 GB/40 GB free (30G checkout image), ports 80/443 free, DNS of domain resolves to this host (reuse public preflight).
- Installs Docker CE + buildx from Docker's official apt repo (skip if present & active; refuse `docker.io`/snap docker with a clear message); apt deps (apache2-utils, openssl, curl, xfsprogs, strace, dnsutils).
- Downloads release → verifies signature + SHA256SUMS → runs existing scripts in order: `install-checkout.sh`, `install-app.sh --mode public`, `create-credentials.sh`, start postgres/traefik, `migrate`, start controlplane. Existing scripts stay the source of truth (wrap, don't rewrite).
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
- After admin exists, installer runs new root CLI `autodeploy-run server enroll-local`: registers "this server" under the owner, mints an enrollment token, feeds it to `agent enroll --token-file` (0600 tmp file, shredded), starts agent, runs preflights. Token never displayed.
- Tests: rerun is idempotent (already enrolled → no-op); failure leaves no consumed-but-unused credential.

### 24-E GitHub App manifest flow (amends ADR 0002)
- `/github/create` (admin): choose personal account or org → POST manifest to GitHub (`name`, `url`=origin, `hook_attributes.url`=origin/webhooks/github, `redirect_url`=origin/github/create/callback, `setup_url`, `public:false`, permissions contents:read + metadata:read, event `push`) with one-time `state`.
- Callback: exchange `code` via `POST /app-manifests/{code}/conversions` → App id, slug, client id, PEM, webhook secret.
- Persist without giving the control plane write access to `/etc/autodeploy`: CP writes the result as a staged bundle into a CP-owned spool dir (0700, encrypted with the KEK); a root `systemd .path` + oneshot (`github-apply`) validates and installs files exactly as `enable-github.sh` does, then restarts the CP. UI shows "applying…" then continues into the existing install flow (`/github/setup`).
- `enable-github.sh` is demoted to a break-glass appendix (D3); the `/github` page offers only "Create GitHub App" when no App is configured.
- Verify the manifest API details against current GitHub docs at slice start (no hardcoding from memory).
- Tests: state replay/expiry refused; malformed conversion response rejected; oneshot refuses symlinks/wrong owner/bad PEM; partial apply rolls back; secrets never logged.

### 24-F Agent-only installer for extra servers
- `install-app.sh --role all|agent` (default `all`, golden output unchanged); `install.sh --role agent --control-plane https://<cp> --server-id <id>`.
- Agent role installs: checkout kit, Docker, Traefik (+ its API creds via `create-credentials.sh --traefik-only`), agent unit, network. Skips: CP binary/user, postgres, migrations, controlplane.env, `/etc/hosts` CP block. Agent unit drops `After=controlplane`.
- Servers page: "Add server" → shows the one-line command with server id; token entered at a hidden `/dev/tty` prompt (never in the command line / shell history).
- Public mode only (remote agents trust system roots; lan/private-CA remote agents out of scope).
- App hostnames routed to a server must have DNS pointing at that server (UI hint on bind).
- Tests: golden `lan_mode_identical` unchanged for `--role all`; container harness for `--role agent` asserts no CP artifacts; two-host e2e (VPS + acer or two VPSes): bind env to server 2, push, healthy deploy + route on server 2.

### 24-G Docs + fresh-VPS end-to-end
- Rewrite `docs/operations/vps-install.md` around the one-liner (manual path kept as appendix); README quick start; ADR 0002/0006 amendments; IMPLEMENTATION_STATUS.
- Proof: brand-new VPS, only the one command + browser; record timings; then add a second server.

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
