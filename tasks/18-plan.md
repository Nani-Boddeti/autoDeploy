# Step 18 — host kit + first real-host run on the Ubuntu laptop: plan, 2026-11-12, awaiting owner approval

Goal: a reproducible, reviewable kit that turns the owner's Ubuntu laptop into an AutoDeploy host, then the
privileged checkout smoke, then one end-to-end deploy with both gates on — locally only. First time any code
runs on real Linux / Docker / BuildKit / Traefik. Expect real bugs; each becomes its own fix step.

## Target host (inventoried 2026-11-12 over SSH, read-only)
Ubuntu 24.04.4 LTS, kernel 6.17, x86_64, 8 cores, 7.6 GiB RAM, 344 GB free on ext4 `/`, cgroup v2 with
cpu/io/memory/pids, systemd 255, native Docker Engine 29.5.2 active (`/var/run/docker.sock`; Docker Desktop
4.62 also installed and is the CLI's current context — not used by the agent), apache2 running on :80,
no xfsprogs/Go/Traefik, sudo needs a password (owner runs root steps; access via `ssh autodeploy-laptop`).

## Decisions taken in the plan (owner may veto)
- XFS: loop image (`/var/lib/autodeploy/checkout.xfs`, 30 GB sparse) mounted at `/srv/autodeploy/checkout` with
  `prjquota` via a systemd mount unit — no repartitioning; removable.
- Everything on one machine: PostgreSQL 18 (container), control plane, agent, root helpers, Traefik (container
  with host networking, API on 127.0.0.1:8080 with basic auth, file provider on the agent's route dir).
- TLS: private CA first (installed into the system trust store; Traefik serves certs it signs) — proves the
  switch, probe and drain without public DNS. Real ACME (DNS-01 with an owned domain) later.
- Binaries cross-compiled on the Mac (`GOOS=linux GOARCH=amd64`), copied with scp; Go 1.26.5 installed on the
  laptop only for the privileged integration tests.
- Gates: flipped ONLY in a local, uncommitted build on the laptop (`-ldflags`/patch never committed); the
  committed constants stay false and their guard tests keep passing.

## Sub-steps
- **18a kit files in-repo** (`deploy/host/`): `install.sh` (idempotent; creates `agent`/`checkout` users with
  distinct UID/GID, `agent` socket group, root-owned dirs per docs/checkout-isolation.md, XFS loop image +
  mount unit, ledger init with a documented project ID), systemd units (`autodeploy-checkout-quota`,
  `autodeploy-checkout-exec` with `Delegate=yes` on a dedicated slice and cpu/io/memory/pids enabled,
  `autodeploy-controlplane`, `autodeploy-agent` ordered after both helper sockets), Traefik static config +
  container unit, private-CA script, `preflight.sh` (read-only checks: XFS prjquota on the mount, cgroup
  controllers delegated, distinct IDs, socket modes, ports 80/443 free, docker.sock reachable by `agent`),
  `uninstall.sh`. Linted with shellcheck + `systemd-analyze verify` on the laptop. No secrets in any file;
  credential files created 0600 by the owner from prompts, never in argv.
- **18b runbook** (`deploy/host/README.md`): ordered steps, what each root command does, expected output,
  rollback per step.
- **18c host prep (owner runs root steps, I verify read-only):** stop/disable apache2 (Q1), disable suspend,
  install xfsprogs + Go, run install.sh, run preflight.sh.
- **18d privileged checkout smoke:** `make test-checkout-integration` with the 11 env vars from the kit; record
  output; tick the checkout milestone only on pass.
- **18e end-to-end:** migrate, bootstrap admin, enroll the agent, a tiny sample app repo, deploy with gates
  flipped locally; observe build, run, health gate, preflight, activation, HTTPS probe, drain of a second
  deploy. Capture every divergence from the fakes (Engine field semantics, image addressing by config digest,
  Traefik JSON shapes and reload timing, BuildKit provenance/secret leakage).
- **18f findings -> fixes:** each defect becomes its own planned, reviewed step; docs/status updated.

## Verification
18a: shellcheck clean, `systemd-analyze verify` clean, install.sh idempotent (run twice), uninstall.sh leaves
no users/units/mounts. 18d/18e: the runs themselves are the proof; outputs saved under the scratch area and
summarized in this file. Repo checks unchanged (gates false, `go test ./...` green).

## Risks
- Docker group membership for `agent` is root-equivalent on the host — inherent to driving dockerd; documented.
- Laptop suspend/IP change mid-run -> lease expiry (useful later as a deliberate test).
- 7.6 GiB RAM: builds + Postgres + Traefik fit; keep the sample app tiny.
- Docker Desktop's context could confuse manual commands; the agent uses an explicit socket path.
- The first real run will find bugs the fakes hid; scope 18 ends at "findings recorded", fixes are new steps.

## Owner decisions (2026-11-12)
- Q1: stop and disable apache2. Q2: private CA first. Plan approved; start 18a.
- Q3 (Docker Desktop) not asked separately: left installed, unused by AutoDeploy (default).
- 18a split: 18a-1 identities + XFS + root helpers (install part 1, helper units, preflight checks for them);
  18a-2 control plane + agent + Traefik + private CA + remaining preflight + uninstall; 18b runbook.

## Progress
- 18a-1 written (uncommitted, unreviewed): install-checkout.sh, preflight-checkout.sh, smoke.env.example,
  units (mount, quota, exec), libexec launcher + socket wait. shellcheck clean; systemd-analyze only
  missing-binary warnings. IDs 800/801 fixed (ledger binds checkout UID/GID). Real-host findings:
  (1) checkout-exec accepts only ET_EXEC (static non-PIE) git — distro git is PIE; owner: BUILD STATIC GIT (18a-3).
  (2) checkout-exec socket parent cannot be under /run (openat2 RESOLVE_NO_XDEV vs tmpfs) -> /var/lib/autodeploy.
  (3) RestrictAddressFamilies=AF_UNIX is inherited by git -> https fetch fails; owner: KEEP UNIX-ONLY FOR NOW,
      revisit before 18e. (4) ledger binds loop device MAJ:MIN -> reboot may change it; preflight detects;
      test a reboot in 18c, pin if needed. Exec unit omits ProtectSystem/ReadWritePaths (bind mounts -> EXDEV;
      unverified) and relies on no CAP_DAC_OVERRIDE.
- 18a-3 done (uncommitted): build-static-git.sh + static-git/Dockerfile. git 2.56.1 (kernel.org tarball sha256
  f0b0bf89…607f verified in build), alpine:3.23 pinned by digest, static non-PIE (e_type 2, no PT_INTERP),
  gitexecdir=/usr/local/lib/autodeploy/bin. Also static git-remote-https (curl-static + OpenSSL 3.5). Verified on
  acer (--version, init, od). HTTPS fetch works in a container with checkout-exec's exact env -> feasible later
  without code change; needs: exec unit AF_INET/6, install+preflight git-remote-https, resolv/hosts/nsswitch + CA
  bundle visible in the sandbox. Output sha256: git 82ce0ddf…f39d, git-remote-https da5690aa…cd70 (apk versions
  unpinned -> not bit-reproducible; record hashes). Open: `git init` warns "templates not found" on stderr (exit 0)
  — check whether checkout code treats stderr as failure.
- 18a-2 written (uncommitted): install-app.sh, create-credentials.sh (all secrets from /dev/urandom, 0600, never
  printed/argv), private-ca.sh (P-256 CA name-constrained to autodeploy.test, system trust store, managed
  /etc/hosts block), preflight-app.sh, uninstall.sh (--yes required, --keep-data), libexec/autodeploy-run, units
  postgres/traefik/controlplane/agent, traefik/{traefik,autodeploy-api,autodeploy-tls}.yml. DESIGN CORRECTIONS vs
  this plan: Traefik NOT host networking — renderer emits `http://<container>:<port>` backends, so Traefik joins
  Docker network `autodeploy-env` (172.29.0.0/24) with 80/443 published on 127.0.0.1 and API on 127.0.0.1:8080;
  control plane on 172.29.0.1:8081 + scoped ufw rule (ufw active on acer). LoadCredential unusable (agent needs
  exact 0600 owned by euid; control plane needs owner = process UID). Undefined `letsencrypt-production` resolver:
  Traefik logs ERR per route but router enabled and serves the file-provider cert (tested on pinned image).
  Pins: postgres:18.6@sha256:e3d24f69…3c42, traefik:v3.7.18@sha256:9c437ac4…d635. Control plane also needs a CSRF
  key ring (missing from deploy/README.md). BLOCKER for 18e: GitHub App subsystem (agent checkout routes 503
  otherwise) + secrets KEK need owner-provided config/credentials. Docker Desktop CLI first on acer PATH; units
  pin /usr/bin/docker + DOCKER_HOST.
- Kit security review: FAIL (no critical). H1 app containers could reach the control plane and spoof forwarded
  headers (whole /24 trusted) -> fixed Traefik IP + ip-range + /32 trust + scoped ufw. M1 false exec-unit
  hardening claim; M2 quota CAP_FOWNER; M3 pin loop device (/dev/loop200); M4 /etc/hosts marker safety; M5
  keep ledger binding across --keep-data; M6 stage+validate binaries, optional SHA256SUMS; M7 note: widening
  exec network must pair with IPAddressDeny. Lows L1-L10. All fixed (Traefik 172.29.0.2/32 trust, ip-range
  .128/25, /dev/loop200 pinned unit, binding.env beside ledger, stage-lib.sh + SHA256SUMS, ReadOnlyPaths=/etc -/boot
  after RESOLVE_NO_XDEV inventory). Re-review skipped for speed (owner asked); 18c on the acer is the proof.
  Unverified items listed in the fix report: /etc read-only for exec, loop200 node creation, CAP_FOWNER use,
  ufw row format, --ip outside --ip-range on the acer engine.
