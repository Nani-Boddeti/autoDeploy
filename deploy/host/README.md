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
the UI to install the App and `/github/bindings` to bind the repository.
