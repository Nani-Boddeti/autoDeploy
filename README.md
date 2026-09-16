# AutoDeploy

AutoDeploy is a self-hosted control plane and outbound deployment-agent platform for deploying private GitHub Dockerfile applications to Hetzner. It uses Go, PostgreSQL, Docker/BuildKit, and Traefik; it does not give its public control plane a Docker socket or broad SSH access.

Implementation is active. The authoritative continuation brief is [docs/IMPLEMENTATION_STATUS.md](docs/IMPLEMENTATION_STATUS.md). The working plan is [tasks/todo.md](tasks/todo.md); accepted architecture decisions are in [docs/adr](docs/adr).

Do not place credentials in this repository, environment files, logs, commands, migrations, or documentation. Production credentials are protected mounted files outside the checkout.

.tmp/ is intentionally untracked but not gitignored. Exclude it manually and do not use blanket git add.
