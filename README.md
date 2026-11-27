# AutoDeploy

AutoDeploy is a self-hosted control plane and outbound deployment-agent platform for deploying private GitHub Dockerfile applications to any Linux VPS, independent of the hosting provider. It uses Go, PostgreSQL, Docker/BuildKit, and Traefik; it does not give its public control plane a Docker socket or broad SSH access.

Implementation is active. The authoritative continuation brief is [docs/IMPLEMENTATION_STATUS.md](docs/IMPLEMENTATION_STATUS.md). The working plan is [tasks/todo.md](tasks/todo.md); accepted architecture decisions are in [docs/adr](docs/adr).

## Quick start

Status: **pre-release.** The one-command installer and the signed release pipeline are built and tested locally only (unit tests, PostgreSQL integration tests, container harness). They are unproven on a real public VPS, and no signed release has been published yet. Browser one-click GitHub App creation (plan 24-E) is not built.

What you need: an Ubuntu 24.04 x86_64 VPS (2 vCPU, 2 GB RAM, 40 GB disk) from any provider, a domain with an A record pointing at it, and an email address for certificates.

1. Download `install.sh` from a release tag: `curl --proto '=https' --tlsv1.2 -fsSLo install.sh https://raw.githubusercontent.com/Nani-Boddeti/autoDeploy/vX.Y.Z/deploy/host/install.sh`
2. Check the release key fingerprint it embeds against the one published with the release, then run `sudo bash install.sh`.
3. Open the `/setup` address and one-time code it prints, create the administrator, then connect GitHub.

Details, upgrades, extra servers and troubleshooting: [docs/operations/vps-install.md](docs/operations/vps-install.md#quick-install). Release fingerprint: not published yet (owner setup pending, see [docs/operations/releases.md](docs/operations/releases.md)).

Do not place credentials in this repository, environment files, logs, commands, migrations, or documentation. Production credentials are protected mounted files outside the checkout.

.tmp/ is intentionally untracked but not gitignored. Exclude it manually and do not use blanket git add.
