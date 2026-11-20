#!/usr/bin/env bash
# AutoDeploy host kit, step 18a-2: create every credential file the services read.
#
# Run as root, once, after install-app.sh. Values are generated from /dev/urandom here;
# no human-chosen secret, and a secret value never appears in argv, the environment,
# a log or the terminal. They exist only in shell variables / pipes and in the 0600
# files below. An existing file is never overwritten (rotation is a deliberate, manual
# step, see deploy/host/README or the notes below), so the script is idempotent.
#
#   /etc/autodeploy/secrets/postgres-password             root:root 0600  (autodeploy-postgres: POSTGRES_PASSWORD_FILE)
#   /etc/autodeploy/secrets/database-owner-url            root:root 0600  (OWNER URL, role autodeploy; migrations only)
#   /etc/autodeploy/secrets/runtime-db-password           root:root 0600  (password of runtime role autodeploy_app)
#   /etc/autodeploy/controlplane/database-url             cp:cp     0600  (RUNTIME URL, role autodeploy_app, DML only)
#   /etc/autodeploy/controlplane/username-throttle-key-1  cp:cp     0600  exactly 32 raw bytes
#   /etc/autodeploy/controlplane/csrf-key-1               cp:cp     0600  exactly 32 raw bytes
#   /etc/autodeploy/traefik-secrets/api-users             root:root 0600  bcrypt htpasswd for the Traefik API
#   /etc/autodeploy/agent/traefik-api-credentials         agent:agent 0600 "username:password", one line
#
# Modes/owners are what the code demands: internal/credential (control plane: owned by
# the process UID, no group/world bits, no symlinks in the path) and
# internal/agent/client/protectedfile.go (agent: owned by its UID, mode EXACTLY 0600).
#
# `--database-only` creates just the three database files above (used by install-app.sh on
# upgrade; needs no htpasswd). The runtime role is created NOLOGIN by migration 000030;
# `autodeploy-run migrate` enables LOGIN with the password in runtime-db-password and, on an
# existing install whose database-url still holds the owner URL, replaces it with the runtime URL.
# create-credentials.sh itself never overwrites an existing database-url.
#
# The Traefik API pair is created together (the same password, in two forms). The
# enrollment token is NOT created here: it is issued once by the control plane UI and is
# typed at a hidden prompt (see the end of install-app.sh).
#
# Rotation notes: the PostgreSQL password in postgres-password only seeds the role on the
# first start of an EMPTY data volume; to rotate it later run ALTER ROLE inside the
# database and then replace database-owner-url (write a complete new file and rename it; never
# rewrite in place, per deploy/README.md). The runtime password rotates by replacing
# runtime-db-password and database-url together, then `autodeploy-run migrate` (re-applies ALTER ROLE). To rotate the Traefik API credential delete both
# files and re-run this script, then restart autodeploy-traefik.
set -euo pipefail
umask 077

DB_ONLY=0
case "${1:-}" in
  "") ;;
  --database-only) DB_ONLY=1 ;;
  *) printf 'usage: create-credentials.sh [--database-only]\n' >&2; exit 2 ;;
esac

readonly ETC_DIR=/etc/autodeploy
readonly CP_USER=autodeploy-controlplane
readonly AGENT_USER=autodeploy-agent
readonly API_USER=autodeploy-agent # Traefik management API basic-auth username (not secret)

log() { printf '==> %s\n' "$*"; }
die() { printf 'create-credentials: error: %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "must run as root"
command -v openssl >/dev/null || die "openssl required"
((DB_ONLY)) || command -v htpasswd >/dev/null || die "htpasswd required (apt-get install apache2-utils)"
for u in "$CP_USER" "$AGENT_USER"; do id -u "$u" >/dev/null 2>&1 || die "identity $u missing: run install-app.sh first"; done
for d in secrets controlplane traefik-secrets agent; do [[ -d "$ETC_DIR/$d" ]] || die "$ETC_DIR/$d missing: run install-app.sh first"; done

rand_hex() { openssl rand -hex 24; } # 192 bits; hex so it needs no URL escaping

write_file() { # dst owner group  (content on stdin; written to a 0600 temp file, then renamed into place)
  local tmp
  tmp="$(mktemp "$1.XXXXXX")"
  cat >"$tmp"
  chown "$2:$3" "$tmp"
  chmod 0600 "$tmp"
  mv -f "$tmp" "$1"
}

# ---- PostgreSQL passwords and database URLs ----------------------------------------
# Owner role `autodeploy` (superuser, created by the postgres container from postgres-password):
# used ONLY by cmd/migrate through autodeploy-run (root-only URL file). Runtime role
# `autodeploy_app` (migration 000030): control plane, admin, secrets.
PW_FILE="$ETC_DIR/secrets/postgres-password"
OWNER_URL_FILE="$ETC_DIR/secrets/database-owner-url"
RT_PW_FILE="$ETC_DIR/secrets/runtime-db-password"
URL_FILE="$ETC_DIR/controlplane/database-url"
if [[ ! -e "$PW_FILE" ]]; then
  [[ ! -e "$URL_FILE" ]] || die "$URL_FILE exists but $PW_FILE does not; the owner password cannot be recovered. Remove $URL_FILE (and the Postgres volume if it was initialised) to start over"
  log "creating PostgreSQL owner password"
  pg_pw="$(rand_hex)"
  printf '%s' "$pg_pw" | write_file "$PW_FILE" root root
  unset pg_pw
fi
if [[ ! -e "$OWNER_URL_FILE" ]]; then
  log "creating owner database URL (migrations only, root-only)"
  pg_pw="$(<"$PW_FILE")"
  [[ "$pg_pw" =~ ^[0-9a-f]{48}$ ]] || die "$PW_FILE is not a generated 48-hex password; write $OWNER_URL_FILE by hand (root:root 0600)"
  printf 'postgres://autodeploy:%s@127.0.0.1:5432/autodeploy?sslmode=disable\n' "$pg_pw" | write_file "$OWNER_URL_FILE" root root
  unset pg_pw
fi
if [[ ! -e "$RT_PW_FILE" ]]; then
  if [[ -e "$URL_FILE" ]] && grep -q '^postgres://autodeploy_app:' "$URL_FILE"; then
    die "$URL_FILE holds a runtime URL but $RT_PW_FILE is missing; remove $URL_FILE and re-run"
  fi
  log "creating runtime role password"
  rt_pw="$(rand_hex)"
  printf '%s' "$rt_pw" | write_file "$RT_PW_FILE" root root
  unset rt_pw
fi
if [[ -e "$URL_FILE" ]]; then
  if grep -q '^postgres://autodeploy:' "$URL_FILE"; then
    log "database-url still holds the owner credential; 'autodeploy-run migrate' switches it to the runtime role"
  else
    log "database-url exists; keeping it"
  fi
else
  log "creating runtime database URL"
  rt_pw="$(<"$RT_PW_FILE")"
  [[ "$rt_pw" =~ ^[0-9a-f]{48}$ ]] || die "$RT_PW_FILE is malformed"
  printf 'postgres://autodeploy_app:%s@127.0.0.1:5432/autodeploy?sslmode=disable\n' "$rt_pw" | write_file "$URL_FILE" "$CP_USER" "$CP_USER"
  unset rt_pw
fi
if ((DB_ONLY)); then log "database credentials ready (names only: $OWNER_URL_FILE $RT_PW_FILE $URL_FILE)"; exit 0; fi

# ---- username-throttle and CSRF keys: exactly 32 raw bytes ----------------------------
for k in username-throttle-key-1 csrf-key-1; do
  f="$ETC_DIR/controlplane/$k"
  if [[ -e "$f" ]]; then log "$k exists; keeping it"; continue; fi
  log "creating $k (32 random bytes)"
  head -c 32 /dev/urandom | write_file "$f" "$CP_USER" "$CP_USER"
  [[ "$(stat -c %s "$f")" == 32 ]] || die "$f is not exactly 32 bytes"
done

# ---- Traefik management API: bcrypt hash for Traefik, plaintext pair for the agent ---
USERS_FILE="$ETC_DIR/traefik-secrets/api-users"
CRED_FILE="$ETC_DIR/agent/traefik-api-credentials"
if [[ -e "$USERS_FILE" && -e "$CRED_FILE" ]]; then
  log "Traefik API credentials exist; keeping them"
elif [[ -e "$USERS_FILE" || -e "$CRED_FILE" ]]; then
  die "only one of $USERS_FILE / $CRED_FILE exists; they must match. Remove both and re-run"
else
  log "creating Traefik API credentials (bcrypt cost 10)"
  api_pw="$(rand_hex)"
  # htpasswd -i reads the password from stdin: nothing secret in argv.
  printf '%s' "$api_pw" | htpasswd -niB -C 10 "$API_USER" | write_file "$USERS_FILE" root root
  printf '%s:%s\n' "$API_USER" "$api_pw" | write_file "$CRED_FILE" "$AGENT_USER" "$AGENT_USER"
  unset api_pw
  grep -q "^$API_USER:\\\$2" "$USERS_FILE" || die "generated htpasswd entry is not bcrypt"
fi

log "done. Credential files (names only):"
find "$ETC_DIR/secrets" "$ETC_DIR/controlplane" "$ETC_DIR/traefik-secrets" "$ETC_DIR/agent" -maxdepth 1 -type f \
  -printf '    %m %u:%g %p\n' 
log "Restart autodeploy-traefik if it was already running (it reads api-users at start)."
