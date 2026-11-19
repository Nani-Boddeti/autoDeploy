#!/usr/bin/env bash
# AutoDeploy host kit: logical backup of the control-plane PostgreSQL database.
#
# Run as root, on the host, while autodeploy-postgres is running. The control plane may keep
# running: pg_dump reads one consistent snapshot.
#
# Usage (as root):
#   backup-db.sh [--out-dir DIR] [--container NAME] [--database NAME] [--user NAME]
#
#   --out-dir DIR     default /var/backups/autodeploy; created 0700 root:root if absent, refused if it
#                     exists with any other owner or mode, or is a symlink.
#   --container NAME  default autodeploy-postgres (deploy/host/systemd/autodeploy-postgres.service)
#   --database NAME   default autodeploy (POSTGRES_DB of that unit)
#   --user NAME       default autodeploy (POSTGRES_USER of that unit)
# The three overrides exist for restore drills against a scratch database.
#
# What it does: runs `pg_dump -Fc` INSIDE the container (docker exec) as the database role, over the
# container's local unix socket, which the official postgres image trusts; so no password is read,
# passed or printed. The dump goes to a temp file in --out-dir and is renamed to
#   autodeploy-db-<UTC timestamp>.dump
# only after pg_dump exited 0 (the completeness guard) and `pg_restore --list` (run in the container,
# dump on stdin) parsed it; note that --list only proves the archive header and table of contents
# parse. Prints only the path, size in bytes and sha256 on stdout. RECORD the sha256: it is the
# integrity check for every later copy and for the restore.
#
# The dump holds ciphertext and wrapped DEKs only. It does NOT include the KEK files or
# kek-manifest.json; those must be backed up to a DIFFERENT place (ADR 0003). Encryption of the dump
# and the off-host copy are the operator's job; this script only makes the local file.
#
# Drill only: AUTODEPLOY_BACKUP_ALLOW_NONROOT=1 skips the root requirement (and uses the invoking
# user as owner of --out-dir). Never set it on the production host.
set -euo pipefail
umask 077

# Always the system Engine, never a per-user Docker Desktop context (same as enable-github.sh).
# A drill on a workstation may pre-set DOCKER_HOST to point elsewhere.
export DOCKER_HOST="${DOCKER_HOST:-unix:///var/run/docker.sock}" DOCKER_CONFIG="${DOCKER_CONFIG:-/nonexistent}"
unset DOCKER_CONTEXT

log() { printf '==> %s\n' "$*" >&2; }
die() { printf 'backup-db: error: %s\n' "$*" >&2; exit 1; }

out_dir=/var/backups/autodeploy container=autodeploy-postgres database=autodeploy dbuser=autodeploy
while (($#)); do
  case "$1" in
    --out-dir | --container | --database | --user)
      (($# >= 2)) || die "$1 needs a value"
      case "$1" in
        --out-dir) out_dir="$2" ;; --container) container="$2" ;;
        --database) database="$2" ;; --user) dbuser="$2" ;;
        *) die "unreachable" ;;
      esac
      shift 2 ;;
    *) die "unknown argument: $1 (see the header of this script)" ;;
  esac
done

if [[ $EUID -ne 0 && ${AUTODEPLOY_BACKUP_ALLOW_NONROOT:-} != 1 ]]; then
  die "must run as root"
fi
[[ $out_dir == /* ]] || die "--out-dir must be an absolute path"
[[ $container =~ ^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$ ]] || die "invalid --container"
[[ $database =~ ^[A-Za-z0-9_]{1,63}$ ]] || die "invalid --database"
[[ $dbuser =~ ^[A-Za-z0-9_]{1,63}$ ]] || die "invalid --user"
command -v docker >/dev/null || die "docker not found"

sha256_of() { # file
  if command -v sha256sum >/dev/null; then sha256sum -- "$1" | awk '{print $1}'; else shasum -a 256 -- "$1" | awk '{print $1}'; fi
}

# ---- output directory: 0700, owned by the invoking user (root in production) ------------
[[ ! -L $out_dir ]] || die "$out_dir is a symlink"
if [[ -e $out_dir ]]; then
  [[ -d $out_dir ]] || die "$out_dir is not a directory"
else
  mkdir -m 0700 -p -- "$out_dir"
fi
perm_owner="$(stat -c '%a %u' -- "$out_dir" 2>/dev/null || stat -f '%Lp %u' -- "$out_dir")"
[[ $perm_owner == "700 $EUID" ]] || die "$out_dir must be mode 0700 and owned by uid $EUID (found: $perm_owner)"

# ---- the container must be running ------------------------------------------------------
[[ "$(docker inspect -f '{{.State.Running}}' -- "$container" 2>/dev/null || true)" == true ]] \
  || die "container $container is not running"

stamp="$(date -u +%Y%m%dT%H%M%SZ)"
final="$out_dir/autodeploy-db-$stamp.dump"
[[ ! -e $final ]] || die "$final already exists (two backups in the same second?)"
tmp="$(mktemp "$out_dir/.autodeploy-db.XXXXXX")"
trap 'rm -f -- "$tmp"' EXIT

# ---- dump (custom format, consistent snapshot) -------------------------------------------
log "dumping database $database from container $container"
docker exec "$container" pg_dump -Fc -U "$dbuser" -d "$database" >"$tmp" || die "pg_dump failed"
[[ -s $tmp ]] || die "pg_dump produced an empty file"

# ---- sanity check: pg_restore must be able to parse the archive header and TOC ---------
docker exec -i "$container" pg_restore --list >/dev/null <"$tmp" || die "pg_restore --list could not parse the archive header/TOC; not keeping it"

sync -- "$tmp" 2>/dev/null || sync
mv -- "$tmp" "$final"
sync -- "$out_dir" 2>/dev/null || true

printf 'path:   %s\n' "$final"
printf 'size:   %s\n' "$(wc -c <"$final" | tr -d ' ')"
printf 'sha256: %s\n' "$(sha256_of "$final")"
log "reminder: KEK files and kek-manifest.json are NOT in this backup; keep them in a separate location (docs/operations/backup-restore.md)"
