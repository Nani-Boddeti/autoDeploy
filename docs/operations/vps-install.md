# Installing AutoDeploy on a fresh public VPS (provider-neutral)

**Status: pre-release.** Everything below was built and tested locally only (unit tests, a PostgreSQL integration
suite and an Ubuntu 24.04 container harness). Nothing has been run on a real VPS yet, no signed release exists
yet (the owner still has to complete the release setup in `docs/operations/releases.md`), and one-click GitHub App
creation (plan 24-E) is not built. Until the first signed release is published, the Quick install cannot work:
use the manual install in the Appendix.

Contents: [What you need](#what-you-need) · [Quick install](#quick-install) · [First login](#first-login) ·
[Connect GitHub](#connect-github) · [Add another server](#add-another-server) ·
[Upgrade, uninstall, troubleshooting](#upgrade) · [Appendix: manual install](#appendix-manual-install)

## What you need

- One server (a "VPS") from any provider, running **Ubuntu 24.04 LTS, x86_64**, with at least **2 vCPU, 2 GB RAM and
  40 GB free disk**. It must be a full virtual machine, not a container-style plan.
- A **domain name** you control with an **A record** pointing at the server's public IP address (for example
  `autodeploy.example.com`). Create the record before you start; it can take a while to spread.
- An **email address** for Let's Encrypt certificate notices (optional, but you get no expiry warnings without it).
- Ports **22, 80 and 443** reachable from the internet (also in the provider's own firewall). Nothing else may use
  80 or 443 on the server.
- A login on the server that can use `sudo`.

TLS certificates come from Let's Encrypt through Traefik (public mode). A private LAN host with a private CA is a
different setup (`--mode lan`, see `deploy/host/README.md`); switching an existing LAN install to public is not
supported: install fresh.

## Quick install

Replace `vX.Y.Z` with the release tag you are installing (see the project's releases page). Run both commands on
the server:

    curl --proto '=https' --tlsv1.2 -fsSLo install.sh https://raw.githubusercontent.com/Nani-Boddeti/autoDeploy/vX.Y.Z/deploy/host/install.sh
    sudo bash install.sh

Do not pipe the download into `bash`: the installer asks its questions on the terminal and refuses to read
answers from a pipe.

**Check the key fingerprint first.** `install.sh` comes from the git tag, not from a release asset. Its trust rests
on the repository and tag protection plus the signing key it carries. Before you run it, compare the key it
embeds with the fingerprint the owner published (README or release notes, a different channel than the
download). Reading the script is also a good idea (`less install.sh`):

    awk '/^release@autodeploy/ {print $3, $4}' install.sh >embedded.pub && ssh-keygen -lf embedded.pub

The installer prints the same fingerprint again before it downloads anything, and stops if the key is still the
placeholder. You can also compare `sha256sum install.sh` with a hash published by the owner.

**What the installer asks** (each question has a flag, so it can also run unattended):

| Question | Flag |
|---|---|
| Control plane domain (its A record must point at this server) | `--domain autodeploy.example.com` |
| Let's Encrypt email, or `none` | `--email ops@example.com` |
| Configure the firewall (ufw: allow SSH, 80, 443; deny the rest)? | `--firewall yes` or `--firewall no` |
| With `--firewall yes` and no terminal: the SSH port you use | `--ssh-port N` |
| Final "Proceed?" confirmation (and DNS-mismatch warnings) | `--yes` |

Unattended example: `sudo bash install.sh --domain autodeploy.example.com --email ops@example.com --firewall no --yes`.
Other flags: `--setup-wait 30m` (how long to wait for the browser step, default 60m), `--acme-staging` (Let's
Encrypt test certificates; a manual dry run only, deploys will not pass their health probe),
`--allow-downgrade`, `--release-dir DIR` (a local copy of the release for offline installs, verified the same way).
Your answers are saved in `/etc/autodeploy/installer.env` (root-only), so a re-run after an interrupted install
keeps them.

**What it does.** It checks the server (OS, CPU, memory, disk, ports 80/443, DNS), downloads the signed release for
its own version over HTTPS only, verifies signature, checksums and version in a private root-only directory
before it extracts anything, installs Docker CE and buildx from Docker's official apt repository if needed (the
apt key is accepted only if its fingerprint equals the one pinned in the installer), then installs and starts
the stack. A DNS record that does not point at this server is a warning you confirm (or accept with `--yes`).

**What it prints at the end**, once, on the terminal only (it is never written to the log):

      Finish setup in your browser (valid 60 minutes):
        URL : https://<your domain>/setup
        Code: setup.v1.<random characters>

The installer then waits for you to finish the browser step, enrolls this server's own agent, and prints a
summary. If you do not finish within the wait it exits with code 4: the stack is installed, only the browser
setup is pending; run `sudo bash install.sh` again for a new code.

Useful files: log `/var/log/autodeploy-install.log` (root-only, never contains the setup code or any secret);
installed version `/etc/autodeploy/installed-version`; the verified kit scripts (`preflight-app.sh`,
`uninstall.sh`, `enable-github.sh`, `backup-db.sh`, ...) in `/usr/local/share/autodeploy/kit`.

## First login

1. Open the `https://<your domain>/setup` address the installer printed, enter the code, choose an administrator
   username and password (twice), and submit. Then sign in at `https://<your domain>/login`.
2. **Lost or expired code** (it lives 60 minutes): on the server run
   `sudo /usr/local/lib/autodeploy/autodeploy-run admin setup-token`. It prints a new code, invalidates older
   unused ones, and refuses once an administrator exists. Re-running the installer also issues a new code.
3. **Alternative without the browser:** create the administrator in the terminal with
   `sudo /usr/local/lib/autodeploy/autodeploy-run admin bootstrap --username administrator` (it asks for the
   password), then `sudo /usr/local/share/autodeploy/kit/enroll-local.sh --wait 30m` to enroll this server's agent.
   Use one path or the other, not both. `/setup` returns 404 as soon as an administrator exists.

## Connect GitHub

**Coming (plan 24-E, not built yet): one-click "Create GitHub App" in the browser will replace this step.** Until
it ships, creating the App is manual and uses the break-glass script `enable-github.sh`. Do not expect a "Create
GitHub App" button today.

1. On github.com create a GitHub App for your account or organization and download its private key (`.pem`) to
   the server. The App needs read access to repository contents and metadata, and the `push` event.
2. On the server, as root:

       sudo /usr/local/share/autodeploy/kit/enable-github.sh --app-id N --client-id ID --slug SLUG --owner-id N \
         --owner-type User|Organization --private-key ~/app.pem

   Full description and what it does: `deploy/host/README.md`, section "enable-github.sh". Afterwards `shred -u`
   the `.pem`.
3. Set the App's webhook URL to `https://<your domain>/webhooks/github` with the secret from
   `/etc/autodeploy/controlplane/github-webhook-secret-1` and the `push` event. Then in the UI open **GitHub**
   (`/github`), install the App on your repositories, and bind a repository with the **Bind a repository** form.
   Push to deploy. (Remember: release builds ship with the deployment gates off until the VPS round, so deploys do
   not run yet.)

## Add another server

An extra server runs only the agent (plus Traefik for the apps you deploy to it); the control plane, PostgreSQL
and the setup code stay on the first server. Requirements: a second Ubuntu 24.04 x86_64 VPS (same sizing as
"What you need"), ports 80/443 open to the internet, a working control plane reachable at `https://<control plane host>`
with a certificate trusted by system roots (public mode; lan/private-CA agents are out of scope), and the
installer's release tag (`cat /etc/autodeploy/installed-version` on the control plane).

1. On the control plane's **Servers** page, enter a name and press **Add server**. The page that follows shows
   the two commands for the new VPS and, once, the one-time enrollment token (valid 15 minutes):

       curl --proto '=https' --tlsv1.2 -fsSLo install.sh https://raw.githubusercontent.com/Nani-Boddeti/autoDeploy/<version>/deploy/host/install.sh
       sudo bash install.sh --role agent --control-plane-host <control plane host> --server-id <server id>

   The page fills in `<version>` from `/etc/autodeploy/installed-version` when that file holds a valid release tag; otherwise replace it by hand. The token is **not** part of the command. A mistyped server id or control plane host can be corrected by re-running until the server is enrolled.
2. Run them on the new VPS. The installer first checks that `https://<control plane host>/readyz` answers, then
   installs Docker, the checkout isolation, the agent and Traefik (Let's Encrypt for the apps), and asks
   `Paste the one-time enrollment token from the Servers page` at a **hidden** prompt on the terminal. Paste the token
   there. Without a terminal, create a file owned by root with mode 0600 holding the token and pass
   `--token-file FILE` (then delete the file; the token is spent). The token is never accepted on the command line,
   an environment variable or a URL, is copied to a private 0600 file readable only by the agent user for the
   single `agent enroll` call, and that copy is removed on success and on failure. The installer then starts the
   agent and runs `preflight-agent.sh` and `preflight-checkout.sh` (kept in `/usr/local/share/autodeploy/kit`).
   `--email`, `--firewall`, `--ssh-port`, `--acme-staging` and `--yes` work as for the first server.
3. If the token expired or enrollment failed, use **Reissue enrollment token** on the Servers page and run the
   installer again; it resumes. A server that is already enrolled is not asked for a token again; re-running
   upgrades it. A different control plane host, server id or role on an installed server is refused.
4. Bind an environment to the new server (the **Server** field of the bind form). **DNS:** each app hostname
   deployed to that server must point at the *new server's* IP address, not at the control plane.

After uninstalling an extra server (`uninstall.sh --yes`), revoke it on the Servers page, or it stays listed as active.

Differences from the first server (`install-app.sh --role agent`):

| | first server (`--role all`) | extra server (`--role agent`) |
|---|---|---|
| installed | control plane, agent, PostgreSQL, Traefik | agent, Traefik, checkout isolation |
| `/etc/hosts` entry for the control plane host | yes (127.0.0.1) | never |
| control plane route in Traefik | yes | none |
| ACME resolver in Traefik | yes | yes (for the apps) |
| `controlplane.env`, migrations, KEK, setup code | yes | no |
| agent unit ordered after the control plane unit | yes | no |
| credentials | `create-credentials.sh` | `create-credentials.sh --traefik-only` |
| `host.env` role line | none (= all) | `AUTODEPLOY_ROLE=agent` |
| preflight | `preflight-app.sh` | `preflight-agent.sh` |

Not proven yet (needs two real hosts, see plan 24, 24-F): the two-host end to end.

## Upgrade

Download `install.sh` from the newer tag (same `curl` command as in Quick install, with the new `vX.Y.Z`), check the
fingerprint again, and run `sudo bash install.sh`. A re-run is an upgrade: it keeps your saved answers, skips the
setup code when an administrator exists and skips enrollment when this server is already enrolled. It refuses to
install an older version over a newer installed one (`refusing the downgrade`); `--allow-downgrade` overrides that on
purpose. The installed version is in `/etc/autodeploy/installed-version`. Changing the domain, switching between
Let's Encrypt staging and production, or switching a host's role is refused (see below).

## Uninstall

On the server: `sudo /usr/local/share/autodeploy/kit/uninstall.sh --yes`. It keeps your data (the PostgreSQL
volume, credentials, KEK files, ACME certificates and the checkout image) unless you add `--destroy-data`, which
lists what will be lost, including the KEK files. Without `--yes` it only prints what it would remove. It leaves the
ufw allow rules in place on purpose.

For an **extra server**, also open the control plane's **Servers** page and **Revoke** that server afterwards, or it
stays listed as active. Back up first (section 6 of the appendix and `docs/operations/backup-restore.md`).

## Troubleshooting

Start with the log: `/var/log/autodeploy-install.log` (root-only; it never contains the setup code, enrollment token
or any secret). The installer stops before changing anything when a check fails. Common refusals (messages abbreviated):

| The installer says | What it means / what to do |
|---|---|
| `must run as root: use 'sudo bash install.sh'` | Run it with `sudo`. |
| `Ubuntu 24.04 LTS only`, `x86_64 only` | Wrong OS or CPU; use an Ubuntu 24.04 x86_64 image. |
| `at least 2 vCPU` / `2 GB RAM` / `40 GB free disk` | The server is too small. |
| `cgroup v2 is required`, `systemd is required` | Not a normal Ubuntu 24.04 VM image; use a full VM. |
| `this is a development build ... not a release` | You did not download `install.sh` from a release tag. |
| `release public key ... is still the PLACEHOLDER` | The owner has not finished the release setup (`docs/operations/releases.md`). No installer from this tag can work. |
| `--allow-dev, --allowed-signers and --yes-i-am-testing are accepted only by development builds` | Testing flags are refused on a release by design; remove them. |
| `release verification failed ... Nothing was installed.` | Signature, checksum or version did not match. Do not work around it; re-download and compare the fingerprint, and tell the owner. |
| `port 80 and/or 443 is already in use by: ...` | Stop the other web server first; Traefik needs both ports. |
| `has no A record yet` / DNS mismatch warning | Fix the domain's A record, wait for DNS, re-run (or `--yes` to continue anyway; certificates will not issue until DNS is right). |
| `docker.io`, `podman-docker` or a snap Docker is installed | Remove it yourself; the installer wants Docker CE from Docker's apt repository. |
| `the Docker apt key does not match the pinned fingerprint` | Nothing was installed from it; do not override, report it. |
| `refusing the downgrade` | The installed version is newer; use `--allow-downgrade` only if you mean it. |
| `a previous install used the domain ..., not ...` | Changing the domain is not supported; re-run with the old domain, or uninstall and install fresh. |
| `lan-mode (private CA) AutoDeploy install` | This installer only handles public hosts; install fresh. |
| `an earlier run ... chose --role ...` / `installed with role=...` | A host's role cannot be switched; uninstall and install fresh. |
| `cannot reach the control plane at https://.../readyz` (extra server) | Check the `--control-plane-host` spelling, that the control plane is up, and that its certificate is trusted by this server. |
| `PostgreSQL did not become ready within 60 s` | See `docker logs autodeploy-postgres` and `journalctl -u autodeploy-postgres`. |
| `the control plane did not answer .../readyz` | See `journalctl -u autodeploy-controlplane`. |
| exit code 4 | Installed, but you did not finish the browser setup in time; re-run for a new code. |

More on certificates, DNS, rate limits and NAT: appendix section 7. Host diagnostics: `sudo ./diagnose.sh` (see
`deploy/host/README.md`).

# Appendix: manual install

The step-by-step path the installer automates. It stays valid, for example until the first signed release exists
or when you want to see every step. The sections keep their numbers 1 to 7 on purpose, because installer messages
refer to them (for example "section 4, step 2").

## 1. Requirements

- Ubuntu 24.04 LTS, x86_64 only. systemd >= 254 (`systemctl --version`), cgroup v2
  (`stat -fc %T /sys/fs/cgroup` prints `cgroup2fs`).
- Kernel loop-device support: `/dev/loop-control` exists (the checkout volume is a loop-mounted XFS image).
  Some container-based VPS offerings do not provide it; use a full VM.
- Disk: the checkout image is a sparse 30G file (`CHECKOUT_IMAGE_SIZE`) under `/var/lib`; plan for it, the
  PostgreSQL volume, Docker images and build cache on top.
- Public IPv4 (IPv6 optional) on the host itself. If the address is NATed (1:1 NAT) the preflight warns.
- Inbound TCP 22, 80 and 443 reachable from the internet (provider firewall / security group included).
  Port 80 may be blocked by the provider: see "TLS-ALPN-01" below.
- DNS records, pointing at the VPS address, before you start:
  - the control plane hostname (a real FQDN, e.g. `autodeploy.example.com`)
  - one A/AAAA record **per deployed application hostname**. There are no wildcard certificates (they need
    DNS-01, out of scope). The first deploy of a new hostname waits for certificate issuance inside the
    health-probe window.

## 2. Prerequisites on the host

Docker Engine and the buildx plugin from Docker's official apt repository (not the distribution
`docker.io`); follow https://docs.docker.com/engine/install/ubuntu/ and install `docker-ce docker-ce-cli
containerd.io docker-buildx-plugin`. Then:

    sudo apt-get update
    sudo apt-get install -y apache2-utils openssl curl ca-certificates xfsprogs strace dnsutils
    docker buildx version && sudo docker info >/dev/null

## 3. Build and transfer the binaries

On the development machine (Go toolchain, Docker, a clean `git` checkout at the commit to deploy;
uncommitted changes are not built):

    deploy/host/build-static-git.sh GITDIR
    deploy/host/build-host-binaries.sh --gates disabled OUTDIR GITDIR

`--gates` is mandatory. `disabled` builds the committed release posture (`executorEnabled` and
`activationEnabled` false). `enabled` flips both in a temporary worktree and prints a DO NOT RELEASE banner:
only for hosts where the owner has decided to run with the gates open.

The script writes `OUTDIR/SHA256SUMS` and prints its sha256. Copy `OUTDIR` and the repository's `deploy/host`
directory to the VPS (e.g. `scp -r`). **Deliver the printed hash over a different channel** (read it out,
messaging, another session). On the VPS:

    sha256sum BINDIR/SHA256SUMS      # must equal the hash printed on the build machine

The installers refuse to run without `SHA256SUMS`; do not use `--insecure-no-checksum` on a public host.

## 4. Install order (as root on the VPS, from the copied `deploy/host`)

1. Checkout isolation:

       sudo ./install-checkout.sh BINDIR

2. Application stack, staging first. First a dry run against Let's Encrypt staging to prove DNS and the
   challenge path without spending production rate limits:

       sudo AUTODEPLOY_CONTROLPLANE_HOST=autodeploy.example.com ./install-app.sh --mode public \
         --acme-email ops@example.com --acme-staging --configure-firewall BINDIR

   Flags (public mode): `--acme-challenge http|tls-alpn` (default `http`), `--acme-staging`, `--acme-email ADDR`
   (optional; a warning is printed without it, and you get no expiry notices), `--configure-firewall`.
   `AUTODEPLOY_CONTROLPLANE_HOST` is required and must be a real FQDN (no default). Traefik binds `0.0.0.0` by
   default in public mode. Run `install-app.sh` on an existing install again with the same flags every time.

   **Staging is for the manual issuance check only.** The agent's health probe uses system roots and does not
   trust staging certificates, so deploys would fail their probe. Check that the control plane host serves a
   staging-issued certificate (`openssl s_client -connect HOST:443 -servername HOST </dev/null 2>/dev/null |
   openssl x509 -noout -issuer`), then switch to production, in exactly this order (staging registrations and
   certificates must not be reused, and the unit asserts `acme.json` exists, so it must be recreated before the
   start):

       sudo systemctl stop autodeploy-traefik
       sudo mv /var/lib/autodeploy/traefik/acme/acme.json /var/lib/autodeploy/traefik/acme/acme.json.staging   # keep it
       sudo ./install-app.sh --mode public ... BINDIR        # same flags as before, minus --acme-staging
       sudo systemctl start autodeploy-traefik

   The re-run rewrites `traefik.yml` for production and creates a new empty `acme.json` (0600). `install-app.sh`
   never starts a service, and it only restarts Traefik if it is already active, so with Traefik stopped you
   must start it yourself. Do not re-run the installer while Traefik is still active on the old `acme.json`:
   it would restart onto the staging registration.

   Switching an existing install from `lan` to `public` in place is refused (a leftover
   `dynamic/autodeploy-tls.yml` also blocks it); uninstall and install fresh.

   **A stale `autodeploy-tls.yml` refusal.** `install-app.sh --mode public` stops if
   `/etc/autodeploy/traefik/dynamic/autodeploy-tls.yml` exists, and no script ever deletes it. That file is a
   `lan` artifact (it points Traefik at the private-CA server certificate). Inspect it first
   (`cat` it; a lan install references `/etc/autodeploy/ca/` certificate files and `host.env` says
   `AUTODEPLOY_TLS_MODE=lan` or has no mode). Only for a fresh public install, where nothing should be served
   by the lan certificate, remove it by hand as root (`rm /etc/autodeploy/traefik/dynamic/autodeploy-tls.yml`)
   and re-run the installer. If the host is an actual lan install, do not remove it: uninstall and install fresh.

3. Credentials, services, migrations, administrator, enrollment (the installer prints these steps with your
   exact paths; in public mode there is no private CA step):

       sudo ./create-credentials.sh
       sudo systemctl start autodeploy-postgres autodeploy-traefik
       sudo /usr/local/lib/autodeploy/autodeploy-run migrate
       sudo /usr/local/lib/autodeploy/autodeploy-run admin bootstrap --username administrator
       sudo systemctl start autodeploy-controlplane

   Alternative to `admin bootstrap` (browser first-run): start the control plane, then on the host run
   `sudo /usr/local/lib/autodeploy/autodeploy-run admin setup-token`. It prints a one-time setup code (valid
   60 minutes; running it again invalidates older unused codes; it refuses once an administrator exists).
   Open `https://autodeploy.example.com/setup`, enter the code, choose a username and password, then sign in at
   `/login`. `/setup` returns 404 once an administrator exists. Use one path or the other, not both.

   Sign in at `https://autodeploy.example.com`, register the server, put its id in
   `/etc/autodeploy/agent-local.env` (`AUTODEPLOY_AGENT_SERVER_ID=`), enroll with the one-time token typed at
   the hidden prompt exactly as `install-app.sh` printed (`agent enroll --base-url https://HOST ...`), then
   `sudo systemctl start autodeploy-agent`.

   Automatic alternative to that register/enroll/start sequence (and to step 4's preflights): once the first
   administrator exists, run `sudo ./enroll-local.sh [--wait 30m] [--name NAME]`. It registers this server under
   the owner (default name: short hostname), writes the one-time token to a root-made 0700 directory under
   `/run` (never printed, never in argv, never in the environment), records `AUTODEPLOY_AGENT_SERVER_ID` in
   `agent-local.env`, runs `agent enroll --token-file` as the agent user, always removes the token file, starts
   `autodeploy-agent` and runs both preflights. `--wait` polls for the first administrator (use it while you
   finish `/setup`). It is idempotent: an existing agent credential makes it a no-op, and a crash between steps
   is resumed with a fresh token and no duplicate server. If this host's server is enrolled in the control plane
   but the host has no credential, it stops and tells you to use Servers > Re-enroll. The audit actor is the
   owner's single administrator user, the same shape as UI registration (its `register` audit row carries the
   reason `local-enroll`; only servers registered this way are ever reused by name, never ones you registered in
   the UI). One run at a time (flock); stale `/run/autodeploy-enroll.*` directories of a crashed run are swept
   after 15 minutes.

   Restores: a host that still has `/var/lib/autodeploy/agent/credential` but whose control plane lost the
   matching credential (database restore) is a no-op for this script (the credential file exists); use
   Servers > Re-enroll in the UI and enroll as in the manual steps. Likewise, if the control plane has the server
   enrolled but the host has no credential, the script stops with the same guidance; if the recorded id was
   revoked or removed it tells you to clear `AUTODEPLOY_AGENT_SERVER_ID` in `agent-local.env` and re-run.

4. Verify: `sudo ./preflight-app.sh` and `sudo ./preflight-checkout.sh`. In public mode `preflight-app.sh`
   checks: DNS of the control plane host resolves to this host's public address (warns under NAT), the hosts
   line `CP_HOST -> 127.0.0.1`, ports 80/443 owned by Traefik, the ACME directory is reachable, `acme.json`
   permissions (0600) and the issuer of the certificate actually served. In both modes: Docker network subnet
   collision with existing routes, `/dev/loop-control`, free disk and buildx.

The managed `/etc/hosts` line makes the agent reach the control plane at `127.0.0.1` without depending on
hairpin NAT.

## 5. Firewall

`--configure-firewall` (opt-in) sets up ufw: allows the detected SSH port(s), 80 and 443, default deny
incoming; you must type the SSH port back to confirm (guards against locking yourself out). Without the flag
the kit never touches ufw and you must allow 22/80/443 yourself, plus the provider's own firewall.
Docker-published ports bypass ufw: the kit binds everything except Traefik 80/443 to loopback or the Docker
bridge; do not publish extra container ports on `0.0.0.0` expecting ufw to block them.

`uninstall.sh` removes only the `autodeploy: traefik` ufw rule. The ssh/80/443 allow rules and the default-deny
policy are left in place on purpose (removing them could lock you out of the host); change them with `ufw`
yourself if you want them gone.

The ACME directory is bind-mounted into the Traefik container, which runs without extra capabilities. This
relies on host root being container root: it does not work with Docker user-namespace remapping
(`userns-remap`). Do not enable it on this host.

## LAN hosts (`--mode lan`, existing installs)

Re-running `install-app.sh` on a host installed by an earlier kit adds `AUTODEPLOY_TLS_MODE=lan` to
`/etc/autodeploy/host.env`; that counts as a Traefik configuration change, so an active Traefik is restarted
once. Nothing else in the generated LAN files changes (pinned by `deploy/host/tests/lan_mode_identical.sh`).
`preflight-app.sh` in lan mode also gained checks: `/dev/loop-control`, `docker buildx` (FAIL if missing), free
disk (WARN) and the environment subnet (SKIP/OK/WARN).

## 6. Backups

- Database: `sudo ./backup-db.sh`, then encrypt and copy off-host.
- KEK files and `kek-manifest.json`, to a **different** location than the dump (ADR 0003).
- `/var/lib/autodeploy/traefik/acme/acme.json` (0600): the ACME account key and certificates. Back it up
  encrypted, root-only. Let's Encrypt limits (5 duplicate certificates per week, 50 certificates per registered
  domain per week) make losing it expensive.

Full procedure and restore order: `docs/operations/backup-restore.md`.

## 7. Troubleshooting

- **DNS not propagated / wrong address:** issuance fails and the preflight DNS check fails. Compare
  `dig +short HOST` with the VPS address from every resolver you can; wait for the TTL; create the record
  before the first deploy of that hostname.
- **Port 80 blocked by the provider or an upstream firewall:** HTTP-01 cannot validate. Reinstall with
  `--acme-challenge tls-alpn` (validates over 443 only). Port 80 is then only needed for the HTTP to HTTPS redirect.
- **Rate limits:** `too many certificates already issued` / duplicate-certificate errors in
  `journalctl -u autodeploy-traefik`. Wait out the weekly window; never delete `acme.json` to "retry". Use
  staging for experiments.
- **First deploy times out on a new hostname:** Traefik v3 requests the certificate shortly after the route is loaded (not on the first request; verify on the VPS), and issuance can take longer than
  the health-probe window when DNS is slow. Check `journalctl -u autodeploy-traefik` for the ACME result, then
  redeploy; the certificate is now cached.
- **Hairpin / NAT:** if the server cannot reach its own public address, the probe and issuance checks can
  fail while the outside world works. The control plane path is covered by the `/etc/hosts` line; for app hosts
  confirm from outside the host (`curl -I https://app-host` from another machine).
- **Subnet collision:** the preflight reports a conflict with `172.29.0.0/24`; set `AUTODEPLOY_ENV_SUBNET`,
  `AUTODEPLOY_ENV_GATEWAY`, `AUTODEPLOY_ENV_IP_RANGE`, `AUTODEPLOY_ENV_TRAEFIK_IP` on a fresh install.
- Anything else: `sudo ./diagnose.sh` (see `deploy/host/README.md`).
