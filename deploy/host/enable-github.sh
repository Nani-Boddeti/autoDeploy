#!/usr/bin/env bash
# AutoDeploy host kit: enable the GitHub App and the secret (KEK) subsystem on the control
# plane. Run as root, once, after install-app.sh + create-credentials.sh + migrate, with the
# GitHub App already created by the owner on github.com.
#
# Usage (as root):
#   enable-github.sh --app-id N --client-id ID --slug SLUG --owner-id N \
#       --owner-type User|Organization --private-key FILE [--replace-key] [--bindir DIR]
#
#   --private-key FILE  the .pem downloaded from the App settings, copied to the host. It is
#                       copied (never printed) to the control plane's credential directory.
#   --replace-key       replace a DIFFERENT, already installed App private key (default: refuse).
#   --bindir DIR        run install-app.sh DIR first (installs the cmd/secrets binary and the unit
#                       change on a host installed before they existed). Not needed once
#                       install-app.sh has been re-run with a BINDIR that contains `secrets`.
#
# Every input is non-secret and validated exactly as internal/config/database.go
# GitHubAppConfigFromEnvironment does (plus a stricter character set, since the slug becomes
# part of a URL). What it does:
#   1. installs the App private key  /etc/autodeploy/controlplane/github-app-private-key
#   2. creates the webhook secret    /etc/autodeploy/controlplane/github-webhook-secret-1
#      (32 random bytes, hex text so the owner can paste it into the App's webhook settings)
#   3. creates the KEK               /etc/autodeploy/controlplane/secret-kek-1 (32 random bytes)
#      and its manifest              /etc/autodeploy/controlplane/kek-manifest.json (non-secret
#      JSON bound to the database generation read from PostgreSQL)
#   4. initialises the key ledger once: `autodeploy-run secrets secret init-active-kek --kek kek-1`
#      (cmd/secrets needs an interactive terminal; a ledger that already exists is detected and
#      left alone)
#   5. writes /etc/autodeploy/controlplane-github.env (root 0644, paths and ids only), which the
#      control plane unit reads after controlplane.env, and restarts autodeploy-controlplane.
# Secret files are owned by autodeploy-controlplane, mode 0600, created by /dev/urandom or copied
# from FILE; no secret appears in argv, the environment, logs or the terminal. An existing secret
# file is never overwritten (the App key only with --replace-key).
set -euo pipefail
umask 077

KIT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly KIT_DIR
readonly ETC_DIR=/etc/autodeploy
readonly CP_DIR="$ETC_DIR/controlplane"
readonly CP_USER=autodeploy-controlplane
readonly LIB_DIR=/usr/local/lib/autodeploy
readonly APP_DIR="$LIB_DIR/app"
readonly RUN="$LIB_DIR/autodeploy-run"
readonly GITHUB_ENV="$ETC_DIR/controlplane-github.env"
readonly KEY_DST="$CP_DIR/github-app-private-key"
readonly WEBHOOK_FILE="$CP_DIR/github-webhook-secret-1"
readonly KEK_ID=kek-1
readonly KEK_FILE="$CP_DIR/secret-$KEK_ID"
readonly MANIFEST="$CP_DIR/kek-manifest.json"
readonly PG_CONTAINER=autodeploy-postgres
# Always the system Engine, never a per-user Docker Desktop context.
export DOCKER_HOST=unix:///var/run/docker.sock DOCKER_CONFIG=/nonexistent
unset DOCKER_CONTEXT

log() { printf '==> %s\n' "$*"; }
die() { printf 'enable-github: error: %s\n' "$*" >&2; exit 1; }

app_id="" client_id="" slug="" owner_id="" owner_type="" key_src="" replace_key=0 bindir=""
while (($#)); do
  case "$1" in
    --replace-key) replace_key=1; shift ;;
    --app-id | --client-id | --slug | --owner-id | --owner-type | --private-key | --bindir)
      (($# >= 2)) || die "$1 needs a value"
      case "$1" in
        --app-id) app_id="$2" ;; --client-id) client_id="$2" ;; --slug) slug="$2" ;;
        --owner-id) owner_id="$2" ;; --owner-type) owner_type="$2" ;;
        --private-key) key_src="$2" ;; --bindir) bindir="$2" ;;
        *) die "unreachable" ;;
      esac
      shift 2 ;;
    *) die "unknown argument: $1 (see the header of this script)" ;;
  esac
done

[[ $EUID -eq 0 ]] || die "must run as root"
# ---- 1. validate inputs (mirrors internal/config/database.go) -------------------------
[[ $app_id =~ ^[1-9][0-9]{0,17}$ ]] || die "--app-id must be a positive integer"
[[ $owner_id =~ ^[1-9][0-9]{0,17}$ ]] || die "--owner-id must be a positive integer"
[[ $owner_type == User || $owner_type == Organization ]] || die "--owner-type must be User or Organization"
# safeGitHubText: printable ASCII without space, 1..N bytes; narrowed to URL-safe characters.
[[ $client_id =~ ^[A-Za-z0-9._-]{1,255}$ ]] || die "--client-id must be 1-255 characters of A-Za-z0-9._-"
[[ $slug =~ ^[A-Za-z0-9._-]{1,100}$ ]] || die "--slug must be 1-100 characters of A-Za-z0-9._-"
[[ -n $key_src ]] || die "--private-key FILE is required"
[[ -f $key_src && ! -L $key_src ]] || die "--private-key must be a regular file (not a symlink)"
# MaxRSAPrivateKeyPEMBytes = 16 KiB; exactly one unencrypted RSA PKCS#1 or PKCS#8 PEM block.
(($(stat -c %s "$key_src") > 0 && $(stat -c %s "$key_src") <= 16384)) || die "--private-key is empty or larger than 16 KiB"
[[ "$(grep -c -- '^-----BEGIN ' "$key_src")" == 1 ]] || die "--private-key must contain exactly one PEM block"
grep -q -- '^-----BEGIN \(RSA \)\?PRIVATE KEY-----' "$key_src" || die "--private-key is not an unencrypted RSA/PKCS#8 private key PEM"
command -v openssl >/dev/null || die "openssl required"
openssl rsa -in "$key_src" -noout -passin pass: >/dev/null 2>&1 || die "--private-key does not parse as an RSA private key"
installation_url="https://github.com/apps/$slug/installations/new"

# ---- 2. preconditions -------------------------------------------------------------------
if [[ -n $bindir ]]; then
  log "running install-app.sh $bindir"
  "$KIT_DIR/install-app.sh" "$bindir"
fi
id -u "$CP_USER" >/dev/null 2>&1 || die "identity $CP_USER missing: run install-app.sh first"
[[ -d $CP_DIR && -r $ETC_DIR/controlplane.env ]] || die "$CP_DIR or controlplane.env missing: run install-app.sh first"
[[ -x $APP_DIR/secrets && -x $RUN ]] || die "$APP_DIR/secrets missing: re-run install-app.sh with a BINDIR containing 'secrets' (or pass --bindir DIR)"
[[ -f $CP_DIR/database-url ]] || die "run create-credentials.sh first"
systemctl is-active --quiet autodeploy-postgres.service || die "autodeploy-postgres is not running (sudo systemctl start autodeploy-postgres)"
[[ -x /usr/bin/docker ]] || die "/usr/bin/docker required (the units pin it)"
psql_cp() { /usr/bin/docker exec "$PG_CONTAINER" psql -U autodeploy -d autodeploy -X -tA -v ON_ERROR_STOP=1 -c "$1"; }
psql_cp "SELECT 1 FROM secret_database_generation" >/dev/null 2>&1 \
  || die "database has no secret tables: run '$RUN migrate' first"

write_file() { # dst owner group mode (content on stdin; 0600 temp file renamed into place)
  local tmp
  tmp="$(mktemp "$1.XXXXXX")"
  cat >"$tmp"
  chown "$2:$3" "$tmp"
  chmod "$4" "$tmp"
  mv -f "$tmp" "$1"
}

# ---- 3. GitHub App private key -------------------------------------------------------------
if [[ -e $KEY_DST ]]; then
  if cmp -s "$key_src" "$KEY_DST"; then
    log "App private key already installed (identical)"
  elif ((replace_key)); then
    log "replacing the installed App private key (--replace-key)"
    write_file "$KEY_DST" "$CP_USER" "$CP_USER" 0600 <"$key_src"
  else
    die "$KEY_DST exists and differs from --private-key; pass --replace-key to replace it"
  fi
else
  log "installing the App private key"
  write_file "$KEY_DST" "$CP_USER" "$CP_USER" 0600 <"$key_src"
fi

# ---- 4. webhook secret (32 bytes: LoadKey demands exactly 32; hex text is pasteable) -----
if [[ -e $WEBHOOK_FILE ]]; then
  log "webhook secret exists; keeping it"
else
  log "creating the webhook secret"
  openssl rand -hex 16 | tr -d '\n' | write_file "$WEBHOOK_FILE" "$CP_USER" "$CP_USER" 0600
fi
[[ "$(stat -c %s "$WEBHOOK_FILE")" == 32 ]] || die "$WEBHOOK_FILE is not exactly 32 bytes"

# ---- 5. KEK and manifest ---------------------------------------------------------------------
generation="$(psql_cp "SELECT id FROM secret_database_generation WHERE singleton")"
[[ $generation =~ ^[A-Za-z0-9]{16,128}$ ]] || die "unexpected database generation id"
if [[ -e $KEK_FILE ]]; then
  log "KEK exists; keeping it"
else
  [[ ! -e $MANIFEST ]] || die "$MANIFEST exists without $KEK_FILE; refusing to continue"
  log "creating the KEK (32 random bytes)"
  head -c 32 /dev/urandom | write_file "$KEK_FILE" "$CP_USER" "$CP_USER" 0600
fi
[[ "$(stat -c %s "$KEK_FILE")" == 32 ]] || die "$KEK_FILE is not exactly 32 bytes"
manifest_body="$(printf '{"active_key_id":"%s","database_generation_id":"%s","keys":[{"id":"%s","state":"active","path":"%s"}]}\n' \
  "$KEK_ID" "$generation" "$KEK_ID" "$KEK_FILE")"
if [[ -e $MANIFEST ]]; then
  [[ "$(cat "$MANIFEST")" == "$manifest_body" ]] || die "$MANIFEST exists and differs from the expected single-key manifest (rotated or restored? handle by hand)"
  log "KEK manifest exists; keeping it"
else
  log "writing the KEK manifest"
  printf '%s\n' "$manifest_body" | write_file "$MANIFEST" "$CP_USER" "$CP_USER" 0600
fi

# ---- 6. non-secret environment file --------------------------------------------------------
log "writing $GITHUB_ENV"
tmp="$(mktemp "$GITHUB_ENV.XXXXXX")"
cat >"$tmp" <<ENVEOF
# Generated by deploy/host/enable-github.sh. Non-secret (ids and paths only). Read by
# autodeploy-controlplane.service after controlplane.env and by autodeploy-run.
AUTODEPLOY_GITHUB_APP_ENABLED=true
AUTODEPLOY_GITHUB_APP_ID=$app_id
AUTODEPLOY_GITHUB_APP_CLIENT_ID=$client_id
AUTODEPLOY_GITHUB_APP_SLUG=$slug
AUTODEPLOY_GITHUB_APP_OWNER_ID=$owner_id
AUTODEPLOY_GITHUB_APP_OWNER_TYPE=$owner_type
AUTODEPLOY_GITHUB_APP_INSTALLATION_URL=$installation_url
AUTODEPLOY_GITHUB_APP_PRIVATE_KEY_FILE=$KEY_DST
AUTODEPLOY_GITHUB_WEBHOOK_ACTIVE_VERSION=1
AUTODEPLOY_GITHUB_WEBHOOK_ACTIVE_SECRET_FILE=$WEBHOOK_FILE
AUTODEPLOY_SECRET_SUBSYSTEM_ENABLED=true
AUTODEPLOY_KEK_MANIFEST_FILE=$MANIFEST
ENVEOF
chown root:root "$tmp"
chmod 0644 "$tmp"
mv -f "$tmp" "$GITHUB_ENV"

# ---- 7. key ledger (once) -----------------------------------------------------------------
ledger="$(psql_cp "SELECT count(*) FROM secret_wrap_invocations WHERE kek_id='$KEK_ID' AND database_generation_id='$generation'")"
if [[ $ledger == 1 ]]; then
  log "key ledger already initialised for $KEK_ID"
else
  [[ $ledger == 0 ]] || die "unexpected ledger state ($ledger rows)"
  [[ -t 0 && -t 1 ]] || die "cmd/secrets needs an interactive terminal: run this script from a terminal, not a pipe"
  log "initialising the key ledger for $KEK_ID"
  "$RUN" secrets secret init-active-kek --kek "$KEK_ID"
fi

# ---- 8. restart and report -------------------------------------------------------------------
log "restarting autodeploy-controlplane"
systemctl restart autodeploy-controlplane.service || true
sleep 6
state="$(systemctl is-active autodeploy-controlplane.service || true)"
log "autodeploy-controlplane is: $state; last 3 journal lines:"
journalctl -u autodeploy-controlplane.service -n 3 --no-pager -o cat || true
[[ $state == active ]] || die "control plane is not active; see the journal lines above (the failing startup line is logged)"

cat <<EOF

GitHub App and secret subsystem are enabled.

Next steps (owner):
  1. Delete the downloaded key from your home directory:   shred -u $key_src
  2. In the GitHub App settings set the webhook (if not already) to:
       URL     https://<control plane host>/webhooks/github
       secret  the contents of $WEBHOOK_FILE
               (read it once with sudo, paste it into GitHub; it is 32 hex characters)
       events  push        (permissions: Contents read, Metadata read)
  3. Sign in to the AutoDeploy UI and open  /github/setup  ("Install GitHub App"). It sends you to
       $installation_url
     to install the App on the repository, and GitHub returns to /github/setup/callback.
  4. Then bind the repository to a project under /github/bindings.
EOF
