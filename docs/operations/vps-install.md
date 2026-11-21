# Installing on a fresh public VPS (provider-neutral)

Target: one Ubuntu 24.04 LTS x86_64 server with a public address and DNS you control, any provider.
TLS comes from Let's Encrypt through Traefik (`--mode public`). For a private LAN host with a private CA,
use the default `--mode lan` (see `deploy/host/README.md`). Switching an existing LAN install to public
in place is not supported: install fresh.

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
