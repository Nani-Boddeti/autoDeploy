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
#   /etc/autodeploy/controlplane/database-url             cp:cp     0600  (postgres:// URL, same password)
#   /etc/autodeploy/controlplane/username-throttle-key-1  cp:cp     0600  exactly 32 raw bytes
#   /etc/autodeploy/controlplane/csrf-key-1               cp:cp     0600  exactly 32 raw bytes
#   /etc/autodeploy/traefik-secrets/api-users             root:root 0600  bcrypt htpasswd for the Traefik API
#   /etc/autodeploy/agent/traefik-api-credentials         agent:agent 0600 "username:password", one line
#
# Modes/owners are what the code demands: internal/credential (control plane: owned by
# the process UID, no group/world bits, no symlinks in the path) and
# internal/agent/client/protectedfile.go (agent: owned by its UID, mode EXACTLY 0600).
#
# The Traefik API pair is created together (the same password, in two forms). The
# enrollment token is NOT created here: it is issued once by the control plane UI and is
# typed at a hidden prompt (see the end of install-app.sh).
#
# Rotation notes: the PostgreSQL password in postgres-password only seeds the role on the
# first start of an EMPTY data volume; to rotate it later run ALTER ROLE inside the
# database and then replace database-url (write a complete new file and rename it; never
# rewrite in place, per deploy/README.md). To rotate the Traefik API credential delete both
# files and re-run this script, then restart autodeploy-traefik.
set -euo pipefail
umask 077

readonly ETC_DIR=/etc/autodeploy
readonly CP_USER=autodeploy-controlplane
readonly AGENT_USER=autodeploy-agent
readonly API_USER=autodeploy-agent # Traefik management API basic-auth username (not secret)

log() { printf '==> %s\n' "$*"; }
die() { printf 'create-credentials: error: %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "must run as root"
command -v openssl >/dev/null || die "openssl required"
command -v htpasswd >/dev/null || die "htpasswd required (apt-get install apache2-utils)"
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

# ---- PostgreSQL password and database URL ------------------------------------------
PW_FILE="$ETC_DIR/secrets/postgres-password"
URL_FILE="$ETC_DIR/controlplane/database-url"
if [[ -e "$PW_FILE" && -e "$URL_FILE" ]]; then
  log "PostgreSQL credentials exist; keeping them"
elif [[ -e "$PW_FILE" || -e "$URL_FILE" ]]; then
  die "only one of $PW_FILE / $URL_FILE exists; they must match. Remove both (and the Postgres volume if it was initialised) to start over"
else
  log "creating PostgreSQL password and database URL"
  pg_pw="$(rand_hex)"
  printf '%s' "$pg_pw" | write_file "$PW_FILE" root root
  printf 'postgres://autodeploy:%s@127.0.0.1:5432/autodeploy?sslmode=disable\n' "$pg_pw" | write_file "$URL_FILE" "$CP_USER" "$CP_USER"
  unset pg_pw
fi

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
