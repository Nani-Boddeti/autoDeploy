# Backup and restore (Ubuntu host kit)

Operator runbook for a host installed with `deploy/host`. Every path, unit and command below comes
from the kit scripts and units; anything the repository does not decide is marked **Operator
decision**. The binding policy is ADR 0003, "Backup, restore, and KEK recovery"
(`docs/adr/0003-secret-handling.md`). Run everything as root.

Names used throughout:

| What | Value | Source |
| --- | --- | --- |
| PostgreSQL container, role, database | `autodeploy-postgres`, `autodeploy`, `autodeploy` | `deploy/host/systemd/autodeploy-postgres.service` |
| PostgreSQL data volume | Docker named volume `autodeploy-postgres-data` | same unit |
| Control plane / agent units | `autodeploy-controlplane.service`, `autodeploy-agent.service` | `deploy/host/systemd/` |
| Control-plane key directory | `/etc/autodeploy/controlplane/` (`root:autodeploy-controlplane 0750`) | `deploy/host/install-app.sh` |
| One-shot tools wrapper | `/usr/local/lib/autodeploy/autodeploy-run` | `deploy/host/libexec/autodeploy-run` |
| Agent state directory | `/var/lib/autodeploy/agent` | `deploy/host/install-app.sh` |

## 1. What to back up, and what must never share a location

ADR 0003: the database backup holds only ciphertext and wrapped DEKs. The KEKs that unwrap them are
backed up **separately**, "offline or in a second protected system with narrower access", and the only
KEK backup is **never colocated with the only database backup**. Anyone holding both can read every
stored secret.

| Set | Contents | Backup location rule |
| --- | --- | --- |
| A. Database | Logical dump from `deploy/host/backup-db.sh` (section 2) | Location 1 |
| B. KEK ring | `/etc/autodeploy/controlplane/secret-*` (every KEK file, including retiring ones) and `/etc/autodeploy/controlplane/kek-manifest.json` | Location 2, **different** from location 1, narrower access |
| C. Other control-plane credentials and configuration | see below | Location 2 or a third place; never required to be with A |
| D. Not backed up for restore | see below | Reissued or rebuilt |

**Set C:**

- `/etc/autodeploy/controlplane/csrf-key-1`, `username-throttle-key-1` (32 raw bytes each; losing them only
  invalidates sessions and throttle state, so they are reissuable, but keeping them avoids logging users out)
- `/etc/autodeploy/controlplane/github-app-private-key` and `github-webhook-secret-1` (written by
  `enable-github.sh`; the App key can also be re-downloaded from the GitHub App settings, the webhook
  secret must then be re-pasted into the App's webhook settings)
- `/etc/autodeploy/controlplane/database-url` and `/etc/autodeploy/secrets/postgres-password` (the role
  password only seeds the role on a first start of an empty volume, see `create-credentials.sh`; on a new
  host `create-credentials.sh` generates a matching pair, so these are optional)
- `/etc/autodeploy/*.env`: `controlplane.env`, `controlplane-github.env`, `host.env`, `agent.env`,
  `agent-local.env`, `checkout.env` (non-secret, but `agent-local.env` holds the server id and
  `controlplane-github.env` the GitHub App ids)
- `/etc/autodeploy/agent/traefik-api-credentials` and `/etc/autodeploy/traefik-secrets/api-users` (can be
  regenerated: delete both and re-run `create-credentials.sh`, then restart `autodeploy-traefik`)

**Set D (not restored from backup):**

- **Agent credential** (`/var/lib/autodeploy/agent/credential`): never restore it from a file backup.
  If the restored database does not recognise it, re-enroll (section 3, step 10).
- **Traefik dynamic route files** (`/var/lib/autodeploy/traefik/dynamic`) and **application containers**:
  rebuilt by the next deploy of each environment. Until then, what is still on the host keeps running.
- **Agent state directory** (fence state, pending activation/drain records): host-local runtime state,
  not a restore source; restoring it against a different database is exactly the conflict described in
  section 3 ("Restoring an older database").
- **Private CA** (`/etc/autodeploy/ca/ca.key`, `ca.crt`, from `private-ca.sh`): **Operator decision.**
  Either back up `ca.key` with the same care as a KEK (separate location), or accept that a lost CA is
  regenerated with `private-ca.sh --force`, after which every browser and client must trust the new
  `ca.crt` again.
- Docker image build cache and the Postgres volume itself: not backed up; the logical dump is the
  supported artifact. (Volume snapshots are **Operator decision** and are not documented here.)

## 2. Backup procedure

```
sudo deploy/host/backup-db.sh                       # default out dir /var/backups/autodeploy
sudo deploy/host/backup-db.sh --out-dir /some/dir   # must be absolute; created 0700 root:root
```

What it does: `pg_dump -Fc` inside the `autodeploy-postgres` container (one consistent snapshot; the
control plane may keep running), written to a temp file in the output directory, checked with `pg_restore --list`
(which only proves the archive header and table of contents parse), then
renamed to `autodeploy-db-<UTC timestamp>.dump`. It prints only `path`, `size` and `sha256`. The role
password is never read: the database role connects over the container's local unix socket. The script
does not touch any KEK file by design and prints a one-line reminder.
Completeness rests on `pg_dump`'s exit status (the script aborts and keeps nothing on failure); integrity
after that rests on the sha256, so **recording the printed sha256 is mandatory**: store it with, but
separately from the readable dump (for example in your backup log), and verify it after every copy and
before every restore.

Then, as an explicit operator responsibility (the repository decides none of this):

1. **Encrypt the dump** before it leaves the host, with a key that is not a KEK in set B. (**Operator
   decision**: tool and key custody. ADR 0003: database backups "are encrypted independently".)
2. **Copy it off the host** to location 1 and record the sha256 printed by the script; verify it after
   the copy. (**Operator decision**: medium, destination, schedule.)
3. **Back up set B** (and C) to location 2 whenever a KEK is added, retired or the manifest changes, and
   after every restore (section 3 creates a new KEK). Verify the copy by listing the files and confirming
   `kek-manifest.json` parses; the ring is validated for real by `secret verify` during a drill.
4. **Retention is coupled to KEK retirement.** A retiring KEK stays in the manifest, and in the set B
   backups, for as long as any retained database backup contains a version wrapped by it. Before deleting
   a KEK, `autodeploy-run secrets secret can-retire --kek <id>` must succeed; it checks the live database
   only. **Rule:** while any retained dump is older than the rewrap that moved its versions off `<id>`,
   keep `<id>` (this is ADR 0003: "Retain retiring KEKs until every required database backup is migrated
   or expires"). Do not run `can-retire` as the decision on its own. Backup retention period and
   frequency are an **Operator decision** (the ADR leaves them open).

## 3. Restore procedure (strict order)

Do the steps in order. Do not start the control plane or the agent before step 8: ADR 0003 requires the
KEKs installed and a decrypt canary passed "before enabling administrative traffic or agent jobs".

Prerequisites: the dump (decrypted, sha256 matches the recorded value), the set B backup, a terminal
(`init-active-kek` refuses a non-terminal), `autodeploy-postgres` running
(`systemctl start autodeploy-postgres`).

1. Stop the consumers, agent first:

   ```
   systemctl stop autodeploy-agent autodeploy-controlplane
   ```

2. Restore the database into a fresh database; the current one is kept under a new name, not dropped:

   ```
   D=/path/to/autodeploy-db-<UTC timestamp>.dump
   sha256sum "$D"        # compare with the recorded value
   OLD=autodeploy_prerestore_$(date -u +%Y%m%d%H%M)
   echo "$OLD"            # WRITE THIS NAME DOWN now; the shell variable is lost if the session ends
   docker exec autodeploy-postgres psql -U autodeploy -d postgres -X -v ON_ERROR_STOP=1 \
     -c "ALTER DATABASE autodeploy RENAME TO $OLD" \
     -c "CREATE DATABASE autodeploy OWNER autodeploy"
   docker exec -i autodeploy-postgres pg_restore -U autodeploy -d autodeploy \
     --no-owner --exit-on-error < "$D"
   ```

   If `pg_restore` fails midway, the new `autodeploy` is partial: put the original back and retry:

   ```
   docker exec autodeploy-postgres psql -U autodeploy -d postgres -X -v ON_ERROR_STOP=1 \
     -c "DROP DATABASE autodeploy" -c "ALTER DATABASE $OLD RENAME TO autodeploy"
   ```

   (If the session ended, list the name with `docker exec autodeploy-postgres psql -U autodeploy -d postgres -c '\l'`.)
   Then fix the cause and repeat from the `ALTER DATABASE autodeploy RENAME` command.

   `ALTER DATABASE ... RENAME` fails while anything is connected; step 1 stopped the only clients. The
   renamed copy still holds ciphertext and wrapped DEKs: it is a backup of the failed state. Drop it
   (`DROP DATABASE`) only when you no longer need it, and count it in your retention policy. Do **not**
   run `autodeploy-run migrate` first; the dump carries the schema and `schema_migrations`. A restored
   database from an older release is brought forward afterwards by running `autodeploy-run migrate`
   (migrations are forward-only, section 6).

3. Check the database generation the dump carries; the KEK manifest must be bound to it:

   ```
   docker exec autodeploy-postgres psql -U autodeploy -d autodeploy -X -tA \
     -c "SELECT id FROM secret_database_generation WHERE singleton"
   docker exec autodeploy-postgres psql -U autodeploy -d autodeploy -X -tA \
     -c "SELECT kek_id FROM secret_wrap_invocations ORDER BY kek_id"
   ```

   Keep both outputs: the first must equal `database_generation_id` in the backed-up
   `kek-manifest.json`; the second is the list of KEK ids already used (a new id must not be in it).
   If the generation differs, that manifest belongs to a different database lineage: stop and decide
   by hand (every secret command refuses with "database generation does not match the key manifest").

4. Install set B from its separate backup into `/etc/autodeploy/controlplane/`: every `secret-<id>` file and
   `kek-manifest.json`, owner `autodeploy-controlplane:autodeploy-controlplane`, mode `0600`, created
   through a temp file and renamed as in `enable-github.sh`. Also restore set C (`*.env`,
   key files) if this is a fresh host; **`/etc/autodeploy/controlplane-github.env` is required** for the
   secret subsystem: it carries `AUTODEPLOY_SECRET_SUBSYSTEM_ENABLED=true` and
   `AUTODEPLOY_KEK_MANIFEST_FILE` (written by `enable-github.sh`). Without it `autodeploy-run` and the
   control plane run with secrets disabled, and `secret verify` cannot check anything. On a fresh host, run
   `install-app.sh` and `create-credentials.sh` first.

5. **Install a fresh active KEK id.** Project rule: every restore installs a new active KEK id, because the
   restored key ledger (`secret_wrap_invocations`, the wrap budget counters) may be stale relative to
   wraps performed after the backup. The old KEK(s) stay, decrypt-only (`retiring`).

   ```
   CP=/etc/autodeploy/controlplane
   NEW=kek-2        # any [A-Za-z0-9_-]{1,64} id NOT listed by the second query in step 3
   GEN=<id from step 3>
   umask 077
   tmp=$(mktemp "$CP/secret-$NEW.XXXXXX")
   head -c 32 /dev/urandom > "$tmp"
   chown autodeploy-controlplane:autodeploy-controlplane "$tmp"; chmod 0600 "$tmp"
   mv -f "$tmp" "$CP/secret-$NEW"
   [ "$(stat -c %s "$CP/secret-$NEW")" = 32 ] || echo "KEK file is not 32 bytes"
   ```

   A failed run can leave `$CP/secret-*.XXXXXX` temp files behind; delete them (`ls $CP`), otherwise the
   `secret-*` glob sweeps them into the KEK backup set.

   `enable-github.sh --kek-id` does not do this step: it refuses when a manifest with another active id
   exists, because adding a fresh id next to `retiring` keys is only supported by hand, as below.

   Then write the manifest (non-secret JSON; same ownership/mode and temp-file-and-rename way). Format
   from `internal/crypto/manifest.go` and `enable-github.sh`; exactly one `active` key, every older key
   `retiring`, absolute paths, no unknown fields. Example with old `kek-1`:

   ```
   {"active_key_id":"kek-2","database_generation_id":"<GEN>","keys":[{"id":"kek-2","state":"active","path":"/etc/autodeploy/controlplane/secret-kek-2"},{"id":"kek-1","state":"retiring","path":"/etc/autodeploy/controlplane/secret-kek-1"}]}
   ```

   Write it (replacing the restored manifest; temp file in the same directory, then rename):

   ```
   tmp=$(mktemp "$CP/kek-manifest.json.XXXXXX")
   printf '%s\n' '{"active_key_id":"kek-2","database_generation_id":"'"$GEN"'","keys":[{"id":"kek-2","state":"active","path":"'"$CP"'/secret-kek-2"},{"id":"kek-1","state":"retiring","path":"'"$CP"'/secret-kek-1"}]}' > "$tmp"
   chown autodeploy-controlplane:autodeploy-controlplane "$tmp"; chmod 0600 "$tmp"
   mv -f "$tmp" "$CP/kek-manifest.json"
   ```

   Remove any leftover `kek-manifest.json.*` temp file the same way.

   Include every retained KEK file, one entry each. Then initialise the ledger for the new id
   (it refuses an id that any stored version references):

   ```
   /usr/local/lib/autodeploy/autodeploy-run secrets secret init-active-kek --kek kek-2
   ```

   Output: `active secret key initialized`. The control plane does not start with the secret subsystem
   enabled until this ledger row exists for the active id.

6. Decrypt canary (read-only, no terminal needed):

   ```
   /usr/local/lib/autodeploy/autodeploy-run secrets secret verify
   ```

   Required result: `secret verify completed checked=N failed=0`. A failure prints one line per version,
   `failed version=<id> kek=<kek> reason=kek_missing|kek_invalid|unwrap_failed|decrypt_failed|envelope_invalid`
   (`kek_missing`: that KEK file is not in the manifest; `kek_invalid`: the KEK file is present but has the wrong length; the others mean wrong or damaged key material
   or data). Revoked versions are not checked. Do not proceed on `failed>0`: fix set B, or if a KEK is
   truly gone see section 4.

7. Optional now, mandatory before ever retiring the old KEK: move versions to the new KEK with
   `secrets secret rewrap --from-kek kek-1`, then see section 2 item 4 before `can-retire`.

8. If the dump predates the installed binaries, bring the schema forward with
   `/usr/local/lib/autodeploy/autodeploy-run migrate`. Then start the control plane and check readiness:

   ```
   systemctl start autodeploy-controlplane
   curl -fsS http://<AUTODEPLOY_LISTEN_ADDRESS from /etc/autodeploy/controlplane.env>/readyz   # "ok"
   ```

   (Default listen address `172.29.0.1:8081`, plain HTTP behind Traefik.) `503 unavailable` means the
   database is unreachable or, with the secret subsystem enabled, the manifest/ring cannot be loaded or
   the active KEK has no ledger row for this database generation. `journalctl -u autodeploy-controlplane`
   shows the failing startup stage.

9. Take a fresh set B backup now (new KEK file and manifest) and a fresh database backup; store them per
   section 1. The previous set B is no longer sufficient for databases written after this restore.

10. Start the agent:

    ```
    systemctl start autodeploy-agent
    ```

    The unit has `ConditionPathExists=/var/lib/autodeploy/agent/credential`, so it will not start if the
    credential file is absent. If the agent cannot authenticate because the restored database does not
    contain its credential (the server was registered or enrolled after the backup), re-enroll: in the UI
    register the server or reissue its enrollment token, then, as in `install-app.sh`, remove the stale
    `credential` file (`agent enroll` never overwrites it) and run `agent enroll` with the new token typed at
    a hidden prompt (`install-app.sh` prints the exact command). The UI **refuses** a fresh token for a
    server that is already enrolled in the restored database (migration `000012`: a consumed token cannot be
    reissued); if the restored database says "enrolled" but the credential differs from the agent's, the
    repository has no supported re-enrollment for that server id. **Operator decision**: register a new
    server and move its environments, or restore a newer backup.

### Restoring an older database than the host's local state

If the dump is older than work the agent has already done, the database and the host disagree.

- **Pending activation records.** The agent writes `/var/lib/autodeploy/agent/activations/attempt-<id>.json`
  before it touches a Traefik route and removes it only after the control plane acknowledges the result;
  pending drain obligations live in `/var/lib/autodeploy/agent/drains/`. A record for an attempt the
  restored database does not know, or in a different phase, can conflict: expect the environment to show
  **reconciliation required**. Resolve it in the UI with the resolve form
  (`POST /environments/resolve`, rendered on the Servers page; the administrator states what really
  happened: `activated`, `not_activated` or `no_activation`, with a reason, which is audited). Do not
  delete the agent's records by hand to silence it; first list them
  (`ls /var/lib/autodeploy/agent/activations /var/lib/autodeploy/agent/drains`) and compare the attempt ids
  with the UI. The metric `autodeploy_environments_reconciliation_required` counts flagged environments.
- **Releases and containers newer than the backup** are unknown to the control plane. They are not deleted
  by the restore. Inspect them with
  `docker ps -a --filter label=io.autodeploy.managed --format 'table {{.Names}}\t{{.Label "io.autodeploy.environment-id"}}\t{{.Label "io.autodeploy.release-id"}}\t{{.Status}}'`
  and decide per container: leave it (it keeps serving; the next deploy of that environment supersedes
  it), or remove it with `docker rm -f` once you confirmed the environment's current release in the UI.
  The label keys are defined in `internal/runtime/docker/labels.go`. Redeploy affected environments
  from the control plane so route files and containers match the restored history.
- **Deployment logs and audit entries** written after the backup are lost with the dump.

## 4. Total KEK loss

If every KEK that wrapped stored versions is gone (no set B anywhere), stored secret values are
**unrecoverable** (ADR 0003). The control plane preserves the currently healthy release where it can, but
blocks new secret materialisation and deployment work that needs secrets.

1. Stop `autodeploy-agent` and `autodeploy-controlplane`. If the database was lost too, restore it first
   (section 3 step 2).
2. Create a new KEK file and a manifest with a single `active` key (the format in section 3 step 5, without
   the retiring entries), for an id that is not in `secret_wrap_invocations`. The generation id is
   `SELECT id FROM secret_database_generation WHERE singleton`. If the old manifest is gone (remove the
   stale one first, together with any `secret-*` files that no longer exist elsewhere), run
   `enable-github.sh --kek-id <new-id>` with the usual App arguments, including `--private-key` with
   the same App key file (see its header). Prerequisites: `autodeploy-postgres` running, migrations
   applied, and an interactive terminal (it runs `init-active-kek`). It restarts the control plane at
   the end, so step 4 below is then only the `/readyz` check. It creates `secret-<new-id>`, the single-key manifest and, in
   step 3, the ledger row. It refuses, changing nothing, if a manifest with a different active id still
   exists, and never overwrites an existing KEK file; in that case use the manual steps above.
3. `enable-github.sh` already ran `init-active-kek` for the new id. With the manual steps, run
   `autodeploy-run secrets secret init-active-kek --kek <new-id>`.
4. Start the control plane; `/readyz` must answer `ok`.
5. `autodeploy-run secrets secret verify` will list every old version as `reason=kek_missing`. That is
   expected; the new KEK covers only values written from now on. Re-enter every secret:
   list the heads and their scope in the database,

   ```
   docker exec autodeploy-postgres psql -U autodeploy -d autodeploy -X \
     -c "SELECT h.id, h.project_id, h.environment_id, h.purpose, h.delivery_scope, h.current_version_id, v.state AS current_state FROM secret_heads h LEFT JOIN secret_versions v ON v.id = h.current_version_id ORDER BY h.project_id, h.environment_id, h.id"
   ```

   then for each head whose `current_state` is `active` `autodeploy-run secrets secret replace --secret <id>` (hidden prompt, typed twice;
   it adds a new version under the new KEK and does not decrypt the old one). Bindings keep pointing at the
   same head ids. A head with no current version, or whose current version is not `active` (for example
   revoked), cannot be replaced (the command refuses); create such a secret anew with `secret put` and
   rebind it. Superseded old versions remain and continue to show as `kek_missing` in `verify`; **Operator
   decision**: whether and when to purge them (ADR 0003 deletion and retention rules).
6. Redeploy the environments that use secrets. Deployments without secrets are unaffected. Containers that
   are already running keep the environment they were started with until they are redeployed.
7. Take a new set B and database backup (section 3 step 9).

If a GitHub App key or webhook secret was also lost, use `enable-github.sh --replace-key` (App key) and
re-paste the webhook secret; those are separate runbooks per ADR 0003.

## 5. Restore drill checklist (required before production)

ADR 0003: "Restore and total-KEK-loss exercises are required before production readiness." Use a scratch
database; never the live one. Record the date, the dump sha256 and the result of each line.

Restore drill:

- [ ] Take a dump: `backup-db.sh` (or with `--out-dir` set to a scratch directory).
- [ ] Encrypt, copy off host, retrieve, decrypt, and confirm the sha256 matches (this tests your medium).
- [ ] Retrieve set B from location 2 without using location 1's credentials (proves the separation).
- [ ] Create the scratch database and restore into it:
  `docker exec autodeploy-postgres psql -U autodeploy -d postgres -c "CREATE DATABASE autodeploy_drill"` then
  `docker exec -i autodeploy-postgres pg_restore -U autodeploy -d autodeploy_drill --no-owner --exit-on-error < "$D"`.
- [ ] Compare per-table row counts between `autodeploy` and `autodeploy_drill` (they must match for a dump
  taken just before; allow for writes since otherwise).
- [ ] Rehearse the full section 3 (steps 1 to 10, including the fresh KEK id) on a **non-production** host
  (a throwaway VM built from `deploy/host`), where the restored database takes the name `autodeploy`;
  confirm `secret verify` reports `failed=0` with the fresh KEK id active and the old KEK `retiring`.
  `secret verify` always reads the database named in `database-url`, so the in-place `autodeploy_drill`
  database above only proves the dump restores; it does not exercise the KEK steps.
- [ ] Negative check: remove the old KEK file from the scratch manifest and confirm `verify` reports
  `reason=kek_missing` and a non-zero exit, then put it back.
- [ ] Drop the scratch database: `... -d postgres -c "DROP DATABASE autodeploy_drill"`. Delete any copied
  dump and KEK files from the drill host.

Total-KEK-loss drill (on the same scratch copy, never on production):

- [ ] Remove all KEKs and the manifest; create a new KEK and single-key manifest; run `init-active-kek`.
- [ ] Confirm `verify` lists `kek_missing` for the old versions.
- [ ] Run `secret replace` for one head and confirm a deployment using it succeeds.
- [ ] Confirm a deployment without secrets still succeeds.
- [ ] Record elapsed time; this is your recovery-time figure.

## 6. Downgrade rule

- Migrations are **forward-only**: `migrations/migrations.go` applies each embedded `*.sql` once and
  records its checksum; `cmd/migrate` has no down or rollback mode, and there are no down scripts in
  `migrations/`. An older binary must never be run against a database a newer release has migrated.
- Builds before and after drain-obligation support write different pending activation records: newer ones
  carry a `drain_seconds` field (and related drain inputs), and the agent decodes records with
  `DisallowUnknownFields` (`internal/agent/state/activation.go`), so an older agent build rejects a newer
  record.

Therefore, **before installing an older build** (agent or control plane):

1. No deployment is in flight (no job claimed or executing; check the UI and `autodeploy_agent_jobs`).
2. The agent state directory has no pending records:
   `ls /var/lib/autodeploy/agent/activations /var/lib/autodeploy/agent/drains` shows no `attempt-*.json`
   (stop the agent first, so none can be created while you look).
3. No environment is flagged reconciliation required
   (`autodeploy_environments_reconciliation_required == 0`).

A database downgrade is done by **restoring a backup taken before the upgrade** (section 3), then
installing the matching older binaries, never by running old binaries against the newer schema. Take and
verify a database backup (and set B) immediately before every upgrade; that is the only downgrade path.
