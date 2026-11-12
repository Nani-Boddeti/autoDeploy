#!/usr/bin/env bash
# AutoDeploy host kit, part 1 (step 18a-1): privileged checkout isolation.
#
# Turns an Ubuntu 24.04 (x86_64, systemd >= 254, cgroup v2) machine into a host
# for the two root helpers described in docs/checkout-isolation.md:
#   identities, root-owned directories, XFS loop volume with project quotas,
#   trusted binary directory, quota ledger, and the helper systemd units.
#
# Usage (as root):
#   install-checkout.sh [--git FILE] [--image-size SIZE] BINDIR
#
#   BINDIR  directory with prebuilt linux/amd64 binaries:
#             checkout-quota-service, checkout-exec, git (see --git),
#             github-askpass (optional; only needed for authenticated fetches)
#           and optionally SHA256SUMS (sha256sum format, basename entries; when present
#           EVERY file above must be listed and match, else the install stops; when
#           absent a loud warning is printed). Inputs are copied to a root-only staging
#           directory first and everything is validated and installed from the copies.
#   --git   use FILE as the trusted git instead of BINDIR/git. checkout-exec only
#           accepts a root-owned ELF of type ET_EXEC (statically linked, non-PIE);
#           the distribution /usr/bin/git is a PIE and is refused. In SHA256SUMS it is
#           looked up under FILE's basename.
#
# Tunables (environment, all optional; non-secret). Identity IDs only matter on
# first run: afterwards the existing accounts are authoritative, because the
# quota ledger binds the checkout UID/GID.
#   CHECKOUT_AGENT_ID=800      uid and gid of autodeploy-agent
#   CHECKOUT_USER_ID=801       uid and gid of autodeploy-checkout
#   CHECKOUT_IMAGE_SIZE=30G    sparse size of the XFS image (first run only)
#   CHECKOUT_INITIAL_PROJECT_ID=1000000   one-time ledger high-water project ID
#   CHECKOUT_QUOTA_BYTES=2147483648       per-checkout hard byte limit (maximum)
#   CHECKOUT_QUOTA_INODES=400000          per-checkout hard inode limit (maximum)
#   CHECKOUT_MEMORY_MAX=1073741824  CHECKOUT_PIDS_MAX=256
#   CHECKOUT_IO_MAX=52428800        (bytes/s read and write on the XFS device)
#   CHECKOUT_CPU_QUOTA=100000       CHECKOUT_CPU_PERIOD=100000
#   CHECKOUT_MAX_DURATION=2m        CHECKOUT_MAX_OUTPUT=1048576
#
# Idempotent: safe to re-run. Never reads or writes credentials.
set -euo pipefail

KIT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly KIT_DIR

# ---- fixed layout (units and preflight-checkout.sh assume these paths) -------
readonly AGENT_USER=autodeploy-agent       # primary group of the same name is the socket group
readonly CHECKOUT_USER=autodeploy-checkout
readonly MOUNT_POINT=/srv/autodeploy/checkout
readonly MOUNT_UNIT=srv-autodeploy-checkout.mount
readonly STATE_ROOT=/var/lib/autodeploy
readonly IMAGE="$STATE_ROOT/checkout.xfs"
readonly QUOTA_STATE_DIR="$STATE_ROOT/checkout-quota"
readonly QUOTA_STATE_FILE="$QUOTA_STATE_DIR/ledger.json"
# Root-only record of what the ledger is bound to (checkout UID/GID, loop device). Lives
# beside the ledger so it survives `uninstall.sh --keep-data` together with it.
readonly BINDING_FILE="$QUOTA_STATE_DIR/binding.env"
readonly LOOP_DEV=/dev/loop200   # pinned by autodeploy-checkout-loop.service; 7:200 never changes
readonly LOOP_MAJMIN=7:200
readonly LOOP_UNIT=autodeploy-checkout-loop.service
readonly EXEC_RUN_DIR="$STATE_ROOT/checkout-exec"   # NOT under /run: see unit comments
readonly LIB_DIR=/usr/local/lib/autodeploy
readonly BIN_DIR="$LIB_DIR/bin"
readonly ETC_DIR=/etc/autodeploy
readonly ENV_FILE="$ETC_DIR/checkout.env"
readonly UNIT_DIR=/etc/systemd/system
readonly QUOTA_SOCKET=/run/autodeploy-checkout-quota/quota.sock
readonly SPAWN_SOCKET="$EXEC_RUN_DIR/spawn.sock"
readonly QUOTA_UNIT=autodeploy-checkout-quota.service
readonly EXEC_UNIT=autodeploy-checkout-exec.service

AGENT_ID="${CHECKOUT_AGENT_ID:-800}"
USER_ID="${CHECKOUT_USER_ID:-801}"
IMAGE_SIZE="${CHECKOUT_IMAGE_SIZE:-30G}"
# One-time ledger floor. The XFS volume is created fresh by this script, so no
# project ID can exist on it yet; 1000000 also stays clear of any /etc/projid
# convention. Project IDs are monotonic and never reused (docs/checkout-isolation.md).
INITIAL_PROJECT_ID="${CHECKOUT_INITIAL_PROJECT_ID:-1000000}"
QUOTA_BYTES="${CHECKOUT_QUOTA_BYTES:-2147483648}"
QUOTA_INODES="${CHECKOUT_QUOTA_INODES:-400000}"
MEMORY_MAX="${CHECKOUT_MEMORY_MAX:-1073741824}"
PIDS_MAX="${CHECKOUT_PIDS_MAX:-256}"
IO_MAX="${CHECKOUT_IO_MAX:-52428800}"
CPU_QUOTA="${CHECKOUT_CPU_QUOTA:-100000}"
CPU_PERIOD="${CHECKOUT_CPU_PERIOD:-100000}"
MAX_DURATION="${CHECKOUT_MAX_DURATION:-2m}"
MAX_OUTPUT="${CHECKOUT_MAX_OUTPUT:-1048576}"

CHANGED=0 # set when a binary, wrapper or unit file was replaced

log() { printf '==> %s\n' "$*"; }
die() { printf 'install-checkout: error: %s\n' "$*" >&2; exit 1; }
# shellcheck source=stage-lib.sh
source "$KIT_DIR/stage-lib.sh"

# ---- arguments ----------------------------------------------------------------
GIT_SRC=""
BINDIR=""
while (($#)); do
  case "$1" in
    --git) GIT_SRC="${2:?--git needs a file}"; shift 2 ;;
    --image-size) IMAGE_SIZE="${2:?--image-size needs a value}"; shift 2 ;;
    -h|--help) sed -n '2,37p' "${BASH_SOURCE[0]}"; exit 0 ;;
    -*) die "unknown option: $1" ;;
    *) [[ -z "$BINDIR" ]] || die "only one BINDIR allowed"; BINDIR="$1"; shift ;;
  esac
done
[[ -n "$BINDIR" ]] || die "usage: install-checkout.sh [--git FILE] [--image-size SIZE] BINDIR"

[[ $EUID -eq 0 ]] || die "must run as root"
[[ "$(uname -s)" == Linux && "$(uname -m)" == x86_64 ]] || die "Linux x86_64 only"
[[ "$(stat -fc %T /sys/fs/cgroup)" == cgroup2fs ]] || die "/sys/fs/cgroup is not cgroup v2"
command -v systemctl >/dev/null || die "systemd required"
for v in "$AGENT_ID" "$USER_ID"; do
  [[ "$v" =~ ^[1-9][0-9]*$ ]] || die "invalid uid/gid: $v"
  ((v < 65534)) || die "invalid uid/gid: $v"
done
[[ "$AGENT_ID" != "$USER_ID" ]] || die "agent and checkout IDs must differ"
for v in "$INITIAL_PROJECT_ID" "$QUOTA_BYTES" "$QUOTA_INODES" "$MEMORY_MAX" "$PIDS_MAX" \
  "$IO_MAX" "$CPU_QUOTA" "$CPU_PERIOD" "$MAX_OUTPUT"; do
  [[ "$v" =~ ^[1-9][0-9]*$ ]] || die "numeric setting must be a positive integer: $v"
done
((INITIAL_PROJECT_ID < 4294967295)) || die "project ID out of range"
((CPU_QUOTA <= CPU_PERIOD)) || die "CPU quota must not exceed the period"

log "staging and validating inputs"
stage_init
GIT_SRC="${GIT_SRC:-$BINDIR/git}"
stage_copy "$BINDIR/checkout-quota-service" checkout-quota-service checkout-quota-service
stage_copy "$BINDIR/checkout-exec" checkout-exec checkout-exec
stage_copy "$GIT_SRC" git git
[[ ! -e "$BINDIR/github-askpass" ]] || stage_copy "$BINDIR/github-askpass" github-askpass github-askpass
stage_verify "$BINDIR"
for b in checkout-quota-service checkout-exec git github-askpass; do
  if [[ -e "$STAGE/$b" ]]; then stage_require_elf "$b" "$b"; fi
done
if [[ "$(elf_bytes "$STAGE/git" 16 2)" != 0200 ]]; then
  die "git: $GIT_SRC is not a statically linked, non-PIE executable (ELF type ET_EXEC). checkout-exec rejects any other
       type (internal/checkoutcontain/trusted_linux.go isELF, cmd/checkout-exec validExecFD), including the distribution git.
       Provide one with --git (build with deploy/host/build-static-git.sh)."
fi
for f in systemd/$MOUNT_UNIT systemd/$LOOP_UNIT systemd/$QUOTA_UNIT systemd/$EXEC_UNIT libexec/checkout-loop \
  libexec/checkout-exec-launch libexec/checkout-wait-socket; do
  [[ -f "$KIT_DIR/$f" ]] || die "kit file missing: $KIT_DIR/$f"
done

# ---- 1. packages ---------------------------------------------------------------
if ! command -v mkfs.xfs >/dev/null || ! command -v xfs_quota >/dev/null; then
  log "installing xfsprogs"
  apt-get update -qq
  DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends xfsprogs
fi

# ---- 2. identities -------------------------------------------------------------
# Fixed IDs (not --system allocation): the quota ledger permanently binds the
# checkout UID/GID, so they must be reproducible across reinstalls and chosen
# once. 800/801 sit in Debian's dynamic system range (100-999) but far below the
# region adduser --system allocates from first, and the script refuses to take
# an ID already held by another name.
ensure_group() { # name gid
  if getent group "$1" >/dev/null; then return; fi
  if getent group "$2" >/dev/null; then die "gid $2 is already used by group $(getent group "$2" | cut -d: -f1)"; fi
  groupadd --system --gid "$2" "$1"
}
ensure_user() { # name uid
  if id -u "$1" >/dev/null 2>&1; then return; fi
  if getent passwd "$2" >/dev/null; then die "uid $2 is already used by $(getent passwd "$2" | cut -d: -f1)"; fi
  useradd --system --uid "$2" --gid "$1" --no-create-home --home-dir /nonexistent \
    --shell /usr/sbin/nologin "$1"
}
log "ensuring identities"
ensure_group "$AGENT_USER" "$AGENT_ID"
ensure_group "$CHECKOUT_USER" "$USER_ID"
ensure_user "$AGENT_USER" "$AGENT_ID"
ensure_user "$CHECKOUT_USER" "$USER_ID"

AGENT_UID="$(id -u "$AGENT_USER")";      AGENT_GID="$(id -g "$AGENT_USER")"
CHECKOUT_UID="$(id -u "$CHECKOUT_USER")"; CHECKOUT_GID="$(id -g "$CHECKOUT_USER")"
[[ "$(id -gn "$AGENT_USER")" == "$AGENT_USER" ]] || die "$AGENT_USER primary group must be $AGENT_USER"
[[ "$(id -gn "$CHECKOUT_USER")" == "$CHECKOUT_USER" ]] || die "$CHECKOUT_USER primary group must be $CHECKOUT_USER"
for v in "$AGENT_UID" "$AGENT_GID" "$CHECKOUT_UID" "$CHECKOUT_GID"; do
  ((v != 0)) || die "identities must be non-root"
done
((AGENT_UID != CHECKOUT_UID)) || die "agent and checkout UIDs must differ"
((AGENT_GID != CHECKOUT_GID)) || die "agent and checkout primary GIDs must differ"
for u in "$AGENT_USER" "$CHECKOUT_USER"; do
  [[ "$(getent passwd "$u" | cut -d: -f7)" == /usr/sbin/nologin ]] || die "$u must have the nologin shell"
  for g in root sudo admin wheel; do
    if id -nG "$u" | tr ' ' '\n' | grep -qx "$g"; then die "$u must not be in group $g"; fi
  done
done
# checkout: no supplementary groups at all (so never in the socket group).
[[ "$(id -G "$CHECKOUT_USER")" == "$CHECKOUT_GID" ]] || die "$CHECKOUT_USER must have no supplementary groups"
# The socket group is the agent's primary group (both helpers chown the sockets
# to AgentGID); nobody but the agent may belong to it.
members="$(getent group "$AGENT_USER" | cut -d: -f4)"
[[ -z "$members" || "$members" == "$AGENT_USER" ]] || die "group $AGENT_USER has extra members: $members"
others="$(getent passwd | awk -F: -v g="$AGENT_GID" -v a="$AGENT_USER" '$4==g && $1!=a {print $1}')"
[[ -z "$others" ]] || die "other users have the socket group as primary group: $others"
others="$(getent passwd | awk -F: -v g="$CHECKOUT_GID" -v a="$CHECKOUT_USER" '$4==g && $1!=a {print $1}')"
[[ -z "$others" ]] || die "other users share the checkout primary group: $others"

# Changing either checkout identity after ledger creation needs a migration. The facts
# the ledger is bound to are recorded in $BINDING_FILE (beside the ledger, so they
# survive uninstall.sh --keep-data); /etc/autodeploy/checkout.env is only a derived copy.
# Written by step 7 BEFORE --initialize as STATE=pending and finalized to STATE=bound.
bind_get() { sed -n "s/^$1=//p" "$BINDING_FILE" 2>/dev/null | head -n1; }
BIND_STATE=""
if [[ -e "$QUOTA_STATE_FILE" ]]; then
  [[ -f "$BINDING_FILE" && ! -L "$BINDING_FILE" ]] || die "ledger $QUOTA_STATE_FILE exists but $BINDING_FILE does not, so the
       identity/device the ledger is bound to cannot be verified. Recovery: if you know the checkout UID, GID and loop device
       MAJ:MIN it was initialized with, create $BINDING_FILE (root, 0600) containing the lines STATE=bound,
       CHECKOUT_UID=<uid>, CHECKOUT_GID=<gid>, LEDGER_DEV=<maj:min> and re-run. Otherwise the ledger and image are not
       reusable: remove $QUOTA_STATE_DIR and $IMAGE (all checkouts are lost) and re-run."
  [[ "$(stat -c '%u:%g:%a' "$BINDING_FILE")" == 0:0:600 ]] || die "$BINDING_FILE must be root:root 0600"
  BIND_STATE="$(bind_get STATE)"
  [[ "$BIND_STATE" == bound || "$BIND_STATE" == pending ]] || die "$BINDING_FILE has no valid STATE"
  if [[ "$(bind_get CHECKOUT_UID)" != "$CHECKOUT_UID" || "$(bind_get CHECKOUT_GID)" != "$CHECKOUT_GID" ]]; then
    die "checkout identity changed since the ledger was created ($BINDING_FILE); this requires a deliberate ledger migration"
  fi
fi

# ---- 3. directories ------------------------------------------------------------
mkdir_root() { install -d -o root -g root -m "$1" "$2"; }
log "creating root-owned directories"
mkdir_root 0755 /srv/autodeploy
mkdir_root 0711 "$MOUNT_POINT"          # no listing for others; workspaces are reached by descriptor
mkdir_root 0755 "$STATE_ROOT"
mkdir_root 0700 "$QUOTA_STATE_DIR"      # ledger: only root may even list it
mkdir_root 0711 "$EXEC_RUN_DIR"         # spawn socket parent: traverse-only for others
mkdir_root 0755 /usr/local/lib
mkdir_root 0755 "$LIB_DIR"
mkdir_root 0755 "$BIN_DIR"
mkdir_root 0755 "$ETC_DIR"

# ---- 4. XFS loop volume --------------------------------------------------------
if [[ ! -e "$IMAGE" ]]; then
  log "creating sparse image $IMAGE ($IMAGE_SIZE)"
  (umask 077 && truncate -s "$IMAGE_SIZE" "$IMAGE")
fi
[[ -f "$IMAGE" && ! -L "$IMAGE" ]] || die "$IMAGE must be a regular file"
[[ "$(stat -c '%u:%g' "$IMAGE")" == 0:0 ]] || chown root:root "$IMAGE"
chmod 0600 "$IMAGE"
fstype="$(blkid -p -o value -s TYPE "$IMAGE" 2>/dev/null || true)"
if [[ -z "$fstype" ]]; then
  log "formatting $IMAGE as XFS"
  mkfs.xfs -q -L adcheckout "$IMAGE"
elif [[ "$fstype" != xfs ]]; then
  die "$IMAGE already holds a $fstype filesystem; refusing to overwrite"
fi

install_file() { # src dst mode  (atomic replace; sets CHANGED when content differs)
  if [[ -f "$2" ]] && cmp -s "$1" "$2" && [[ "$(stat -c '%u:%g:%a' "$2")" == "0:0:${3#0}" ]]; then return 1; fi
  local tmp
  tmp="$(mktemp "$2.XXXXXX")"
  install -o root -g root -m "$3" "$1" "$tmp"
  mv -f "$tmp" "$2"
  CHANGED=1
}
was_active() { systemctl is-active --quiet "$1"; }
WAS_QUOTA=0; WAS_EXEC=0
was_active "$QUOTA_UNIT" && WAS_QUOTA=1
was_active "$EXEC_UNIT" && WAS_EXEC=1

log "installing and enabling $LOOP_UNIT and $MOUNT_UNIT"
install_file "$KIT_DIR/libexec/checkout-loop" "$LIB_DIR/checkout-loop" 0755 || true
install_file "$KIT_DIR/systemd/$LOOP_UNIT" "$UNIT_DIR/$LOOP_UNIT" 0644 || true
install_file "$KIT_DIR/systemd/$MOUNT_UNIT" "$UNIT_DIR/$MOUNT_UNIT" 0644 || true
systemctl daemon-reload
# A mount left over from the old Options=loop unit sits on a dynamically allocated loop device.
if mountpoint -q "$MOUNT_POINT" && [[ "$(findmnt -n -o SOURCE --target "$MOUNT_POINT")" != "$LOOP_DEV" ]]; then
  die "$MOUNT_POINT is mounted from $(findmnt -n -o SOURCE --target "$MOUNT_POINT"), not $LOOP_DEV (older kit version). Stop the helpers and run: umount $MOUNT_POINT; losetup -j $IMAGE; losetup -d <device>; then re-run. If a ledger exists it is bound to the old device number and the helper will refuse it: see docs/checkout-isolation.md."
fi
systemctl enable --now "$LOOP_UNIT"
systemctl enable --now "$MOUNT_UNIT"
[[ "$(findmnt -n -o SOURCE --target "$MOUNT_POINT")" == "$LOOP_DEV" ]] || die "$MOUNT_POINT is not mounted from $LOOP_DEV"
[[ "$(findmnt -n -o MAJ:MIN --target "$MOUNT_POINT" | tr -d ' ')" == "$LOOP_MAJMIN" ]] || die "$MOUNT_POINT is not on device $LOOP_MAJMIN"
[[ "$(findmnt -n -o TARGET --target "$MOUNT_POINT")" == "$MOUNT_POINT" ]] || die "$MOUNT_POINT is not a mount point"
[[ "$(findmnt -n -o FSTYPE --target "$MOUNT_POINT")" == xfs ]] || die "$MOUNT_POINT is not XFS"
case ",$(findmnt -n -o OPTIONS --target "$MOUNT_POINT")," in
  *,prjquota,* | *,pquota,*) ;;
  *) die "$MOUNT_POINT is mounted without prjquota; stop helpers and remount: systemctl restart $MOUNT_UNIT" ;;
esac
# The XFS root directory is what the quota service opens as CheckoutRoot.
chown root:root "$MOUNT_POINT"
chmod 0711 "$MOUNT_POINT"
# With a ledger present the volume must be the very device the ledger was bound to.
if [[ -e "$QUOTA_STATE_FILE" && "$(bind_get LEDGER_DEV)" != "$LOOP_MAJMIN" ]]; then
  die "ledger is bound to device '$(bind_get LEDGER_DEV)' but the volume is now $LOOP_MAJMIN ($BINDING_FILE); the quota helper would refuse it. This needs a deliberate ledger migration."
fi

# ---- 5. trusted binaries -------------------------------------------------------
log "installing helper binaries into $BIN_DIR (root:root 0755)"
install_file "$STAGE/checkout-quota-service" "$BIN_DIR/checkout-quota-service" 0755 || true
install_file "$STAGE/checkout-exec" "$BIN_DIR/checkout-exec" 0755 || true
install_file "$STAGE/git" "$BIN_DIR/git" 0755 || true
if [[ -e "$STAGE/github-askpass" ]]; then
  # checkout-exec hard-codes this path as GIT_ASKPASS for authenticated fetches.
  install_file "$STAGE/github-askpass" "$LIB_DIR/github-askpass" 0755 || true
fi
install_file "$KIT_DIR/libexec/checkout-exec-launch" "$LIB_DIR/checkout-exec-launch" 0755 || true
install_file "$KIT_DIR/libexec/checkout-wait-socket" "$LIB_DIR/checkout-wait-socket" 0755 || true

# ---- 6. non-secret configuration consumed by the units, preflight and smoke ----
# Derived from $BINDING_FILE below (that file, not this one, is authoritative).
ledger_dev=""

# ---- 7. ledger (exactly once) ---------------------------------------------------
quota_args=("$MOUNT_POINT" "$QUOTA_SOCKET" "$QUOTA_STATE_FILE" "$AGENT_UID" "$AGENT_GID"
  "$CHECKOUT_UID" "$CHECKOUT_GID" "$QUOTA_BYTES" "$QUOTA_INODES")
write_binding() { # state
  local tmp
  tmp="$(mktemp "$BINDING_FILE.XXXXXX")"
  {
    echo "# Written by deploy/host/install-checkout.sh. The ledger is bound to these. Root only."
    echo "STATE=$1"
    echo "CHECKOUT_UID=$CHECKOUT_UID"
    echo "CHECKOUT_GID=$CHECKOUT_GID"
    echo "LEDGER_DEV=$LOOP_MAJMIN"
  } >"$tmp"
  chown root:root "$tmp"
  chmod 0600 "$tmp"
  mv -f "$tmp" "$BINDING_FILE"
}
if [[ ! -e "$QUOTA_STATE_FILE" ]]; then
  log "initializing quota ledger (one time; project ID floor $INITIAL_PROJECT_ID)"
  systemctl stop "$QUOTA_UNIT" "$EXEC_UNIT" 2>/dev/null || true
  # Pending marker FIRST: a crash between here and the finalize below leaves a
  # detectable STATE=pending (the identity guard above then re-verifies on the next run).
  write_binding pending
  "$BIN_DIR/checkout-quota-service" --initialize --initial-project-id "$INITIAL_PROJECT_ID" "${quota_args[@]}" \
    || die "ledger initialization failed (is $MOUNT_POINT empty and mounted with prjquota?); $BINDING_FILE stays STATE=pending"
  [[ -f "$QUOTA_STATE_FILE" ]] || die "ledger was not created"
  write_binding bound
elif [[ "$BIND_STATE" == pending ]]; then
  # Crash between --initialize and the finalize: the ledger file exists (it is created by
  # an atomic rename at the end of initialization) and the identity/device were already
  # verified against the pending record above.
  log "ledger exists with a pending binding record; finalizing it"
  write_binding bound
else
  log "ledger exists; not re-initializing"
fi
ledger_dev="$(bind_get LEDGER_DEV)"

env_tmp="$(mktemp "$ENV_FILE.XXXXXX")"
{
  echo "# Generated by deploy/host/install-checkout.sh. Non-secret. Do not edit by hand."
  echo "AUTODEPLOY_CHECKOUT_ROOT=$MOUNT_POINT"
  echo "AUTODEPLOY_CHECKOUT_QUOTA_SOCKET=$QUOTA_SOCKET"
  echo "AUTODEPLOY_CHECKOUT_SPAWN_SOCKET=$SPAWN_SOCKET"
  echo "AUTODEPLOY_CHECKOUT_STATE_FILE=$QUOTA_STATE_FILE"
  echo "AUTODEPLOY_CHECKOUT_BIN_ROOT=$BIN_DIR"
  echo "AUTODEPLOY_CHECKOUT_AGENT_UID=$AGENT_UID"
  echo "AUTODEPLOY_CHECKOUT_AGENT_GID=$AGENT_GID"
  echo "AUTODEPLOY_CHECKOUT_UID=$CHECKOUT_UID"
  echo "AUTODEPLOY_CHECKOUT_GID=$CHECKOUT_GID"
  echo "AUTODEPLOY_CHECKOUT_QUOTA_BYTES=$QUOTA_BYTES"
  echo "AUTODEPLOY_CHECKOUT_QUOTA_INODES=$QUOTA_INODES"
  echo "AUTODEPLOY_CHECKOUT_MEMORY_MAX=$MEMORY_MAX"
  echo "AUTODEPLOY_CHECKOUT_PIDS_MAX=$PIDS_MAX"
  echo "AUTODEPLOY_CHECKOUT_IO_MAX=$IO_MAX"
  echo "AUTODEPLOY_CHECKOUT_CPU_QUOTA=$CPU_QUOTA"
  echo "AUTODEPLOY_CHECKOUT_CPU_PERIOD=$CPU_PERIOD"
  echo "AUTODEPLOY_CHECKOUT_MAX_DURATION=$MAX_DURATION"
  echo "AUTODEPLOY_CHECKOUT_MAX_OUTPUT=$MAX_OUTPUT"
  echo "AUTODEPLOY_CHECKOUT_LEDGER_DEV=$ledger_dev"
} >"$env_tmp"
chmod 0644 "$env_tmp"
chown root:root "$env_tmp"
if [[ -f "$ENV_FILE" ]] && cmp -s "$env_tmp" "$ENV_FILE"; then rm -f "$env_tmp"; else mv -f "$env_tmp" "$ENV_FILE"; CHANGED=1; fi

# ---- 8. helper units -----------------------------------------------------------
log "installing helper units"
install_file "$KIT_DIR/systemd/$QUOTA_UNIT" "$UNIT_DIR/$QUOTA_UNIT" 0644 || true
install_file "$KIT_DIR/systemd/$EXEC_UNIT" "$UNIT_DIR/$EXEC_UNIT" 0644 || true
systemctl daemon-reload
systemctl enable "$QUOTA_UNIT" "$EXEC_UNIT"
systemctl start "$QUOTA_UNIT" "$EXEC_UNIT"
if ((CHANGED)); then
  # A running helper keeps its old binary/config until restarted.
  if ((WAS_QUOTA)); then systemctl restart "$QUOTA_UNIT"; fi
  if ((WAS_EXEC)); then systemctl restart "$EXEC_UNIT"; fi
fi

log "done. Verify with: deploy/host/preflight-checkout.sh   (use sudo for the root-only checks)"
log "Restart the application agent after any helper restart (ordering is wired in the agent unit, step 18a-2)."
