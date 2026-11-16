# Host kit: enabling GitHub and building for the acer

## build-acer-binaries.sh (on the development machine, not root)

    deploy/host/build-acer-binaries.sh OUTDIR GITDIR

Builds linux/amd64 binaries of every `cmd/*`, the checkout integration test binary
(`gitcheckout.test`), copies the static `git` and `git-remote-https` from GITDIR (the
`build-static-git.sh` output) and writes `SHA256SUMS`. It works in a temporary `git worktree` of
HEAD where `executorEnabled` (cmd/agent) and `activationEnabled` (cmd/controlplane) are flipped to
`true`; the working tree is untouched and the worktree is removed on exit. The result is
gate-flipped: for the acer only, never release or commit it. Uncommitted changes are not built.

## enable-github.sh (root, on the host)

Prerequisites: install-app.sh, create-credentials.sh, the database running and migrated, the GitHub
App created, its private key `.pem` copied to the host (e.g. your home directory). One command:

    sudo deploy/host/enable-github.sh --app-id N --client-id ID --slug SLUG --owner-id N \
      --owner-type User|Organization --private-key ~/app.pem [--bindir DIR] [--replace-key]

`--bindir DIR` runs `install-app.sh DIR` first (needed once, to install `cmd/secrets` and the unit
change). It installs the App key, creates the webhook secret, the KEK and its manifest, initialises
the key ledger once (`autodeploy-run secrets secret init-active-kek --kek kek-1`, interactive
terminal required), writes `/etc/autodeploy/controlplane-github.env` (non-secret) and restarts the
control plane, printing the last journal lines. Re-running is safe; existing secrets are never
overwritten. Afterwards `shred -u` the `.pem`, set the App webhook (`/webhooks/github`, secret from
`/etc/autodeploy/controlplane/github-webhook-secret-1`, event `push`), then use `/github/setup` in
the UI to install the App (landing page button) and bind the repository with the "Bind a repository"
form on the landing page (POST `/github/bindings`).

## Add a secret

Secret values are entered only on the host terminal (hidden prompt, typed twice; never argv, env or a
file; refused when stdin is not a terminal). The environment must already be known (first Deploy done).

    sudo /usr/local/lib/autodeploy/autodeploy-run secrets secret put --environment uat --name NPM_TOKEN --scope build

`--scope` is `build` (build-time `RUN --mount=type=secret`) or `runtime` (container environment). The
command prints `secret head id: <id>` and `version: 1` (never the value). Paste the id into the
environment's "Add binding" form on the Servers page: kind secret, the same variable name and the same
scope. `--name` is only validated, not stored: the variable name is set in the binding form, and both
must match what the Dockerfile or app expects. To rotate:

    sudo /usr/local/lib/autodeploy/autodeploy-run secrets secret replace --secret <id> [--compromised]

This adds a new version; running deployments keep the version frozen at their claim. `--compromised`
also revokes the previous version (it can no longer materialize, even for a frozen deployment).

## First deploy order (UI)

Bind (project ID, environment ID, branch, server) -> Projects page "Deploy" (this creates the job that
makes the environment known) -> Servers page: environment runtime configuration -> route hostname ->
only then enroll and start the agent, so the first claim finds a complete configuration. The agent
reads the finished checkout through the `autodeploy-checkout` group (workspace mode 0750); the checkout
helper fetches over HTTPS through `git-remote-https` (install-checkout.sh installs it next to git).
