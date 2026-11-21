#!/usr/bin/env bash
# AutoDeploy host kit (plan 24-D): enroll THIS host's agent automatically, as the automatic
# alternative to the manual "register this server in the UI, then `agent enroll`" steps printed
# by install-app.sh. Run as root, after install-app.sh, create-credentials.sh, migrate and
# starting autodeploy-controlplane, and once the first administrator exists (browser /setup or
# `autodeploy-run admin bootstrap`).
#
# Usage (as root):  enroll-local.sh [--wait DURATION] [--name NAME]
#   --wait DURATION  wait up to DURATION (e.g. 30m) for the first administrator to exist before
#                    giving up (default 0: refuse immediately with a clear message).
#   --name NAME      display name of this server in the control plane (default: short hostname).
#
# What it does:
#   1. no-op success if /var/lib/autodeploy/agent/credential already exists (already enrolled);
#   2. preflight: units and binaries installed, control plane answers /readyz;
#   3. waits (--wait) for the first administrator with a call that has no token file, then
#      `autodeploy-run admin server-enroll-local` registers (or reuses) this server under the
#      owner and writes a single-use enrollment token to a file in a root-made 0700 directory
#      under /run (tmpfs), which the control-plane user owns only for that one short call. The
#      token is never printed, never in argv, never in the environment. The file the
#      control-plane user created is NEVER chowned: root verifies it (owner, link count 1,
#      regular, 0600, small), copies it into a NEW agent-owned file and discards the original;
#   4. records AUTODEPLOY_AGENT_SERVER_ID in /etc/autodeploy/agent-local.env (set only when empty
#      or already equal; a different recorded id is refused);
#   5. runs `agent enroll --token-file` as the agent user, then ALWAYS removes the token files and
#      their directory (this script's own temporary files), on success and on failure;
#   6. starts autodeploy-agent and runs preflight-app.sh and preflight-checkout.sh.
# One run at a time (flock). Stale /run/autodeploy-enroll.* directories left by a crashed run are
# swept after the token lifetime. Safe to re-run: a crash between steps leaves a registered-but-unenrolled server that the next
# run reuses with a fresh token (no duplicate server).
set -euo pipefail
umask 077

KIT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly KIT_DIR
readonly ETC_DIR=/etc/autodeploy
readonly LIB_DIR=/usr/local/lib/autodeploy
readonly APP_DIR="$LIB_DIR/app"
readonly RUN="$LIB_DIR/autodeploy-run"
readonly UNIT_DIR=/etc/systemd/system
readonly AGENT_USER=autodeploy-agent
readonly CP_USER=autodeploy-controlplane
readonly AGENT_STATE=/var/lib/autodeploy/agent
readonly LOCAL_ENV="$ETC_DIR/agent-local.env"
readonly TMP_PARENT=/run
# Directly under /run (root-only): /run/lock is world-writable, so anyone could squat the lock there.
# The name cannot match the sweep glob autodeploy-enroll.* below (dash, not dot).
readonly LOCK_FILE=/run/autodeploy-enroll-local.lock
readonly TOKEN_TTL_MIN=15 # server.EnrollmentTTLSeconds (900s)

log() { printf '==> %s\n' "$*"; }
die() { printf 'enroll-local: error: %s\n' "$*" >&2; exit 1; }
# Parsed, never sourced: key=value files written by install-app.sh.
cfg() { sed -n "s/^$2=//p" "$1" 2>/dev/null | head -n1 || true; }
# strict_cfg fails closed: a missing file is empty, an unreadable one is an error (never masked).
strict_cfg() { # file key
  local v
  [[ -e $1 ]] || return 0
  v="$(sed -n "s/^$2=//p" "$1")" || die "cannot read $1"
  printf '%s' "${v%%$'\n'*}"
}

wait_arg="" name_arg=""
while (($#)); do
  case "$1" in
    --wait | --name)
      (($# >= 2)) || die "$1 needs a value"
      if [[ $1 == --wait ]]; then wait_arg="$2"; else name_arg="$2"; fi
      shift 2 ;;
    -h | --help) sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed '$d' | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) die "unknown argument: $1 (usage: enroll-local.sh [--wait DURATION] [--name NAME])" ;;
  esac
done
[[ -z $wait_arg || $wait_arg =~ ^[0-9]+(ns|us|ms|s|m|h)([0-9]+(ns|us|ms|s|m|h))*$ ]] || die "--wait must be a duration such as 30s or 10m"
[[ -z $name_arg || $name_arg =~ ^[A-Za-z0-9]([A-Za-z0-9._\ -]{0,126}[A-Za-z0-9._-])?$ ]] || die "--name: 1-128 characters of letters, digits, space . _ - (no leading or trailing space)"

[[ $EUID -eq 0 ]] || die "must run as root"

# One enrollment at a time (root-only lock file; released when the script exits). Never follow a
# symlink; the file must be a root-owned regular file.
[[ ! -L $LOCK_FILE ]] || die "$LOCK_FILE is a symlink; refusing"
exec 9>>"$LOCK_FILE"
[[ -f $LOCK_FILE && ! -L $LOCK_FILE && "$(stat -c %u "$LOCK_FILE")" == 0 ]] || die "$LOCK_FILE is not a root-owned regular file; refusing"
flock -n 9 || die "another enrollment is running (lock $LOCK_FILE); wait for it or check 'ps'"

# 1. Already enrolled on this host: nothing to do.
if [[ -e $AGENT_STATE/credential ]]; then
  log "agent credential already present in $AGENT_STATE: already enrolled, nothing to do"
  exit 0
fi

# 2. Preflight.
for f in "$UNIT_DIR/autodeploy-controlplane.service" "$UNIT_DIR/autodeploy-agent.service" "$RUN" "$APP_DIR/agent"; do
  [[ -e $f ]] || die "$f not found: run install-app.sh first"
done
id -u "$AGENT_USER" >/dev/null 2>&1 || die "user $AGENT_USER not found: run install-checkout.sh first"
CP_UID="$(id -u "$CP_USER" 2>/dev/null)" || die "user $CP_USER not found: run install-app.sh first"
gateway="$(cfg "$ETC_DIR/host.env" AUTODEPLOY_ENV_GATEWAY)"
port="$(cfg "$ETC_DIR/host.env" AUTODEPLOY_CONTROLPLANE_PORT)"
[[ $gateway =~ ^[0-9.]+$ && $port =~ ^[0-9]+$ ]] || die "$ETC_DIR/host.env lacks a valid control-plane address (run install-app.sh)"
base_url="$(cfg "$ETC_DIR/agent.env" AUTODEPLOY_AGENT_BASE_URL)"
[[ $base_url =~ ^https://[A-Za-z0-9.-]+(:[0-9]+)?$ ]] || die "$ETC_DIR/agent.env AUTODEPLOY_AGENT_BASE_URL is missing or malformed (run install-app.sh)"
recorded="$(strict_cfg "$LOCAL_ENV" AUTODEPLOY_AGENT_SERVER_ID)"
if [[ -n $recorded ]]; then
  [[ $recorded =~ ^server_[A-Za-z0-9_-]{1,100}$ ]] || die "$LOCAL_ENV holds a malformed AUTODEPLOY_AGENT_SERVER_ID"
fi
ready=0
for _ in $(seq 1 30); do
  if curl -fsS --max-time 5 -o /dev/null "http://$gateway:$port/readyz" 2>/dev/null; then ready=1; break; fi
  sleep 2
done
((ready)) || die "control plane did not answer http://$gateway:$port/readyz: start it (systemctl start autodeploy-controlplane) and check 'journalctl -u autodeploy-controlplane'"

# 3. Temp artifacts of THIS script only: a root-made private directory and the env-file temp,
# removed on every exit path. Stale directories of a crashed earlier run (exact prefix, real
# directories only, never symlinks, owned by root or the two service users, older than the token
# lifetime) are swept first; we hold the lock, so none belongs to a live run.
TMPD="" REC_TMP=""
cleanup() {
  if [[ -n $TMPD ]]; then rm -rf -- "$TMPD"; fi
  if [[ -n $REC_TMP ]]; then rm -f -- "$REC_TMP"; fi
}
trap cleanup EXIT
find "$TMP_PARENT" -maxdepth 1 -mindepth 1 -type d -name 'autodeploy-enroll.*' -mmin "+$TOKEN_TTL_MIN" \
  \( -user root -o -user "$CP_USER" -o -user "$AGENT_USER" \) -exec rm -rf --one-file-system -- {} + 2>/dev/null || true
TMPD="$(mktemp -d "$TMP_PARENT/autodeploy-enroll.XXXXXX")"
chmod 0700 "$TMPD" # root-owned while we only wait
token_file="$TMPD/token"
agent_file="$TMPD/token.agent"

# 3a. Wait for the first administrator with a call that has NO token file and no directory access.
wait_cli=("$RUN" admin server-enroll-local --wait-only)
[[ -z $wait_arg ]] || wait_cli+=(--wait "$wait_arg")
log "checking for the first administrator (waits if --wait is given)"
rc=0
"${wait_cli[@]}" || rc=$?
# Dedicated CLI codes 20-23 and 30 sit outside the Go runtime (2 = panic) and shell (126-165) ranges;
# any other nonzero code is a generic failure.
cli_failure() { # rc: map the CLI's exit codes to operator guidance
  case "$1" in
    20) die "no administrator exists yet. Create one first (browser /setup with the code from 'autodeploy-run admin setup-token', or 'autodeploy-run admin bootstrap --username administrator'), then re-run, or pass --wait DURATION" ;;
    21) die "this server was enrolled before but has no active credential in the control plane. Use Servers > Re-enroll in the UI, then enroll with the one-time token as in the manual steps" ;;
    22) die "the server id recorded in $LOCAL_ENV is revoked or removed in the control plane. Clear AUTODEPLOY_AGENT_SERVER_ID in $LOCAL_ENV (leave it empty) and re-run" ;;
    23) die "the administrator account is disabled; re-enable it or reset its password, then re-run" ;;
    *) die "registering this server failed (exit $1); nothing was enrolled. Re-run to retry" ;;
  esac
}
((rc == 0)) || cli_failure "$rc"

# 3b. Register and mint. The control-plane user owns the directory ONLY for this one call; root
# takes it back immediately after.
cli=("$RUN" admin server-enroll-local --token-file "$token_file")
[[ -z $name_arg ]] || cli+=(--name "$name_arg")
[[ -z $recorded ]] || cli+=(--server-id "$recorded")
chown "$CP_USER:$CP_USER" "$TMPD"
log "registering this server with the control plane"
rc=0
server_id="$("${cli[@]}")" || rc=$?
chown root:root "$TMPD"
if ((rc != 0 && rc != 30)); then cli_failure "$rc"; fi
[[ $server_id =~ ^server_[A-Za-z0-9_-]{1,100}$ ]] || die "unexpected server id from the control plane tools"

# 4. Record the id exactly where the manual steps record it (set only when empty or equal).
record_server_id() {
  local cur
  cur="$(strict_cfg "$LOCAL_ENV" AUTODEPLOY_AGENT_SERVER_ID)"
  [[ $cur != "$server_id" ]] || return 0
  [[ -z $cur ]] || die "$LOCAL_ENV already records a different server id; refusing to overwrite it"
  REC_TMP="$(mktemp "$LOCAL_ENV.XXXXXX")"
  if [[ -e $LOCAL_ENV ]]; then
    grep -v '^AUTODEPLOY_AGENT_SERVER_ID=' "$LOCAL_ENV" >"$REC_TMP" || [[ $? -eq 1 ]] || die "cannot read $LOCAL_ENV"
  else
    printf '%s\n' '# Owner-edited, never overwritten by install-app.sh. Set after registering this server in the control plane UI.' >"$REC_TMP"
  fi
  printf 'AUTODEPLOY_AGENT_SERVER_ID=%s\n' "$server_id" >>"$REC_TMP"
  chmod 0644 "$REC_TMP"
  chown root:root "$REC_TMP"
  mv -f "$REC_TMP" "$LOCAL_ENV"
  REC_TMP=""
}
record_server_id
log "server id $server_id recorded in $LOCAL_ENV"

# 5. Already enrolled in the control plane (exit 30) means NO token was minted and no file may
# exist: anything present is refused, never consumed. The host has no credential, so the
# operator must re-enroll in the UI.
if ((rc == 30)); then
  if [[ -e $token_file || -L $token_file ]]; then die "unexpected file where no token should exist; refusing (a service user planted it)"; fi
  die "server $server_id is already enrolled in the control plane but this host has no agent credential (for example after a database restore). Use Servers > Re-enroll in the UI for $server_id, then enroll with the one-time token as in the manual steps"
fi
[[ -e $token_file || -L $token_file ]] || die "the control plane tools reported success but wrote no token file"

# Never chown or consume the inode the control-plane user created. Verify it, copy the bytes into
# a NEW file that root creates for the agent user, and discard the original.
[[ -f $token_file && ! -L $token_file ]] || die "token file is not a regular file; refusing"
read -r f_uid f_links f_mode f_size < <(stat -c '%u %h %a %s' "$token_file")
[[ $f_uid == "$CP_UID" ]] || die "token file is not owned by the control-plane user; refusing"
[[ $f_links == 1 ]] || die "token file has a hard link (link count $f_links); refusing"
[[ $f_mode == 600 ]] || die "token file mode is not 0600; refusing"
((f_size > 0 && f_size <= 256)) || die "token file size is implausible; refusing"
[[ ! -e $agent_file && ! -L $agent_file ]] || die "$agent_file already exists (planted?); refusing"
install -m 0600 -o "$AGENT_USER" -g "$AGENT_USER" /dev/null "$agent_file"
head -c 256 -- "$token_file" >"$agent_file"
rm -f -- "$token_file" # the control-plane-created original: this script's own temp artifact
chown "$AGENT_USER:$AGENT_USER" "$TMPD"
log "enrolling the agent"
runuser -u "$AGENT_USER" -- env -i "PATH=/usr/bin:/bin" "$APP_DIR/agent" enroll \
  --base-url "$base_url" --server-id "$server_id" --state-dir "$AGENT_STATE" --token-file "$agent_file" \
  || die "agent enroll failed; the token file is removed. Re-run this script to mint a fresh token"
cleanup
TMPD=""

# 6. Start and verify.
systemctl start autodeploy-agent.service || die "could not start autodeploy-agent (journalctl -u autodeploy-agent)"
log "agent enrolled and started; running the preflights"
pre_rc=0
"$KIT_DIR/preflight-app.sh" || pre_rc=1
"$KIT_DIR/preflight-checkout.sh" || pre_rc=1
((pre_rc == 0)) || die "agent enrolled and started, but a preflight reported failures (see above)"
log "done: server $server_id enrolled"
