# Host kit: enabling GitHub and building for the acer

## build-acer-binaries.sh (on the development machine, not root)

    deploy/host/build-acer-binaries.sh OUTDIR GITDIR

Builds linux/amd64 binaries of every `cmd/*`, the checkout integration test binary
(`gitcheckout.test`), copies the static `git` and `git-remote-https` from GITDIR (the
`build-static-git.sh` output) and writes `SHA256SUMS`. It works in a temporary `git worktree` of
HEAD where `executorEnabled` (cmd/agent) and `activationEnabled` (cmd/controlplane) are flipped to
`true`; the working tree is untouched and the worktree is removed on exit. The result is
gate-flipped: for the acer only, never release or commit it. Uncommitted changes are not built.

## Installing binaries: SHA256SUMS is mandatory

`install-app.sh` and `install-checkout.sh` refuse to run when `BINDIR/SHA256SUMS` is missing (the
binaries run as root); `--insecure-no-checksum` is the explicit, loud escape hatch for throwaway hosts.
A SHA256SUMS that travels in the same directory/channel as the binaries proves nothing against an
attacker who can change that channel: deliver `SHA256SUMS` (or at least its own sha256, e.g.
`sha256sum SHA256SUMS`) separately (different transport, or read out to the operator) and compare before
installing.

## Database roles

Migrations run as the owner role `autodeploy` through `autodeploy-run migrate` (root-only
`/etc/autodeploy/secrets/database-owner-url`). Control plane, `admin` and `secrets` use the runtime role
`autodeploy_app` (DML only) from `controlplane/database-url`. Re-running `install-app.sh` on an existing
install creates the missing credentials, migrates, enables the runtime role and switches the URL.

## diagnose.sh

Reports go to a root-only directory `/var/lib/autodeploy/diag/<timestamp>.XXXXXX` (0700, files 0600);
strace does not capture read/write payloads. Nothing is copied out for you: review the report and copy it
explicitly (`sudo tar -C /var/lib/autodeploy/diag -czf ~/diag.tgz <dir>`).

## uninstall.sh

`uninstall.sh --yes` keeps the PostgreSQL volume, credentials, KEK files and the checkout image/ledger.
`--destroy-data` deletes them too, lists what is lost (including KEK files) and aborts before deleting any
secret if the Docker volume cannot be removed.

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
command prints `secret head id: <id>` and `version: 1` (never the value). The audit actor records the
sudo operator, e.g. `terminal:alice(as autodeploy-controlplane)` (names outside `^[a-z_][a-z0-9_-]{0,31}$`,
such as `john.doe`, are recorded as `unknown`; this is attribution, not authentication). Paste the id into the
environment's "Add binding" form on the Servers page: kind secret, the same variable name and the same
scope. `--name` is only validated, not stored: the variable name is set in the binding form, and both
must match what the Dockerfile or app expects. To rotate:

    sudo /usr/local/lib/autodeploy/autodeploy-run secrets secret replace --secret <id> [--compromised]

This adds a new version; running deployments keep the version frozen at their claim. `--compromised`
also revokes the previous version (it can no longer materialize, even for a frozen deployment).

## Verify secrets after a restore or key change

    sudo /usr/local/lib/autodeploy/autodeploy-run secrets secret verify

Read-only decrypt canary (ADR 0003 restore step): checks the database generation against the key
manifest, then decrypts every active and superseded version (retained for frozen retries) and discards
the plaintext. It needs no terminal, writes no rows and spends no wrap budget. It prints
`secret verify completed checked=N failed=M` and, for each failure, `failed version=<id> kek=<id>
reason=kek_missing|kek_invalid|unwrap_failed|decrypt_failed|envelope_invalid`, exiting non-zero if any fail. Revoked
versions are not checked: no read path can decrypt them. Ids that are not plain are printed quoted.
Each page is its own read-only transaction, so run it with writers stopped (control plane down, as
in the restore order) for a complete result; under concurrent rewrap or replace it is best-effort.

## Backups and restore

    sudo deploy/host/backup-db.sh [--out-dir DIR] [--container NAME] [--database NAME] [--user NAME]

Logical `pg_dump -Fc` of the control-plane database. Defaults: `--out-dir /var/backups/autodeploy`
(created 0700, dump file 0600), `--container autodeploy-postgres`, `--database autodeploy`,
`--user autodeploy`. It prints only the path, size and sha256. KEK files and the key manifest are NOT
in this backup: back them up separately, never next to the only database backup (ADR 0003).
Encryption of the dump and the off-host copy are the operator's job. Full procedure:
docs/operations/backup-restore.md.

If a restore leaves the agent's credential unknown to the database, re-enroll the same server id (UI
"Re-enroll agent" for an enrolled server, then stop the agent, remove the stale credential file, run
`agent enroll` with the token and start the agent): backup-restore.md section 3, step 10.

Metrics, health endpoints and log format: docs/operations/observability.md. The optional
`install-app.sh` tunable `AUTODEPLOY_METRICS_LISTEN_ADDRESS` (loopback IP literal plus port, for
example `127.0.0.1:9101`; pass it on every run) enables the metrics listener.

## First deploy order (UI)

Bind (project ID, environment ID, branch, server) -> Projects page "Deploy" (this creates the job that
makes the environment known) -> Servers page: environment runtime configuration -> route hostname ->
only then enroll and start the agent, so the first claim finds a complete configuration. The agent
reads the finished checkout through the `autodeploy-checkout` group (workspace mode 0750); the checkout
helper fetches over HTTPS through `git-remote-https` (install-checkout.sh installs it next to git).
