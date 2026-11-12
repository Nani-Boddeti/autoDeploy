#!/usr/bin/env bash
# AutoDeploy host kit, part 1 (step 18a-1): read-only verification of the
# privileged checkout stack (docs/checkout-isolation.md "Verify this before
# enabling the agent"). Changes nothing. Prints PASS/FAIL/SKIP per check and
# exits non-zero if any check FAILs.
#
# Most checks work as an ordinary user. Checks that need root are marked
# [root] and are SKIPped (not failed) when run unprivileged; run the script with
# sudo for full coverage before enabling the agent.
set -uo pipefail

readonly AGENT_USER=autodeploy-agent
readonly CHECKOUT_USER=autodeploy-checkout
readonly ENV_FILE=/etc/autodeploy/checkout.env
readonly QUOTA_UNIT=autodeploy-checkout-quota.service
readonly EXEC_UNIT=autodeploy-checkout-exec.service

pass=0 fail=0 skip=0
ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf 'FAIL  %s\n' "$1"; fail=$((fail + 1)); }
skp() { printf 'SKIP  %s\n' "$1"; skip=$((skip + 1)); }
check() { # description command...
  local d="$1"; shift
  if "$@" >/dev/null 2>&1; then ok "$d"; else bad "$d"; fi
}
is_root() { [[ $EUID -eq 0 ]]; }

# Non-secret key=value file written by install-checkout.sh. Parsed, not sourced.
cfg() { sed -n "s/^$1=//p" "$ENV_FILE" 2>/dev/null | head -n1; }

if [[ ! -r $ENV_FILE ]]; then
  bad "$ENV_FILE readable (run install-checkout.sh first)"
  printf '\n%d passed, %d failed, %d skipped\n' "$pass" "$fail" "$skip"
  exit 1
fi
ROOT="$(cfg AUTODEPLOY_CHECKOUT_ROOT)"
QSOCK="$(cfg AUTODEPLOY_CHECKOUT_QUOTA_SOCKET)"
SSOCK="$(cfg AUTODEPLOY_CHECKOUT_SPAWN_SOCKET)"
STATE="$(cfg AUTODEPLOY_CHECKOUT_STATE_FILE)"
BIN="$(cfg AUTODEPLOY_CHECKOUT_BIN_ROOT)"
AUID="$(cfg AUTODEPLOY_CHECKOUT_AGENT_UID)"
AGID="$(cfg AUTODEPLOY_CHECKOUT_AGENT_GID)"
CUID="$(cfg AUTODEPLOY_CHECKOUT_UID)"
CGID="$(cfg AUTODEPLOY_CHECKOUT_GID)"
LEDGER_DEV="$(cfg AUTODEPLOY_CHECKOUT_LEDGER_DEV)"

# ---- mount: XFS + prjquota ------------------------------------------------------
mount_is_xfs_prjquota() {
  [[ "$(findmnt -n -o TARGET --target "$ROOT")" == "$ROOT" ]] || return 1
  [[ "$(findmnt -n -o FSTYPE --target "$ROOT")" == xfs ]] || return 1
  case ",$(findmnt -n -o OPTIONS --target "$ROOT")," in *,prjquota,* | *,pquota,*) return 0 ;; esac
  return 1
}
check "checkout root $ROOT is its own XFS mount with prjquota (findmnt)" mount_is_xfs_prjquota

quota_enforced() { # [root]
  local out
  out="$(xfs_quota -x -c 'state -p' "$ROOT" 2>/dev/null)" || return 1
  grep -q 'Accounting: ON' <<<"$out" && grep -q 'Enforcement: ON' <<<"$out"
}
if is_root; then check "project quota accounting and enforcement ON (xfs_quota) [root]" quota_enforced
else skp "project quota accounting and enforcement ON (xfs_quota) [root]"; fi

ledger_dev_matches() {
  [[ -n "$LEDGER_DEV" ]] || return 0 # unknown (ledger not created by this kit): nothing to compare
  [[ "$(findmnt -n -o MAJ:MIN --target "$ROOT" | tr -d ' ')" == "$LEDGER_DEV" ]]
}
# The loop device is pinned (autodeploy-checkout-loop.service) so the ledger's device binding is stable.
readonly LOOP_DEV=/dev/loop200 IMAGE=/var/lib/autodeploy/checkout.xfs
check "autodeploy-checkout-loop.service is active" systemctl is-active --quiet autodeploy-checkout-loop.service
loop_pinned() {
  [[ "$(findmnt -n -o SOURCE --target "$ROOT")" == "$LOOP_DEV" ]] || return 1
  [[ "$(findmnt -n -o MAJ:MIN --target "$ROOT" | tr -d ' ')" == 7:200 ]] || return 1
  [[ "$(losetup --noheadings --output NAME --associated "$IMAGE")" == "$LOOP_DEV" ]]
}
check "checkout volume is mounted from $LOOP_DEV (7:200), the only device bound to $IMAGE" loop_pinned
binding_ok() { # [root] the ledger's recorded bindings match the installed identities and device
  local f
  f="$(dirname "$STATE")/binding.env"
  bg() { sed -n "s/^$1=//p" "$f" | head -n1; }
  [[ -f "$f" && ! -L "$f" && "$(stat -c '%u:%g:%a' "$f")" == 0:0:600 ]] || return 1
  [[ "$(bg STATE)" == bound && "$(bg CHECKOUT_UID)" == "$CUID" && "$(bg CHECKOUT_GID)" == "$CGID" && "$(bg LEDGER_DEV)" == "$(findmnt -n -o MAJ:MIN --target "$ROOT" | tr -d ' ')" ]]
}
if is_root; then check "ledger binding file (STATE=bound, checkout uid/gid, device) matches the host [root]" binding_ok
else skp "ledger binding file matches the host [root]"; fi
check "XFS device number equals the one recorded at ledger init ($LEDGER_DEV); a changed loop device makes the quota helper refuse" ledger_dev_matches

# ---- identities -----------------------------------------------------------------
identities_distinct() {
  [[ "$(id -u "$AGENT_USER")" == "$AUID" && "$(id -g "$AGENT_USER")" == "$AGID" ]] || return 1
  [[ "$(id -u "$CHECKOUT_USER")" == "$CUID" && "$(id -g "$CHECKOUT_USER")" == "$CGID" ]] || return 1
  [[ "$AUID" != 0 && "$AGID" != 0 && "$CUID" != 0 && "$CGID" != 0 ]] || return 1
  [[ "$AUID" != "$CUID" && "$AGID" != "$CGID" ]]
}
check "agent and checkout have distinct non-root UIDs and primary GIDs, matching $ENV_FILE" identities_distinct

no_privilege_groups() {
  local u g
  for u in "$AGENT_USER" "$CHECKOUT_USER"; do
    for g in root sudo admin wheel; do
      if id -nG "$u" | tr ' ' '\n' | grep -qx "$g"; then return 1; fi
    done
  done
}
check "neither identity is in root/sudo/admin/wheel" no_privilege_groups

socket_group_agent_only() {
  [[ "$(id -G "$CHECKOUT_USER")" == "$CGID" ]] || return 1 # no supplementary groups, so not in the socket group
  local members
  members="$(getent group "$AGID" | cut -d: -f4)"
  [[ -z "$members" || "$members" == "$AGENT_USER" ]] || return 1
  [[ -z "$(getent passwd | awk -F: -v g="$AGID" -v a="$AGENT_USER" '$4==g && $1!=a')" ]]
}
check "socket group (agent primary gid $AGID) contains only $AGENT_USER; checkout is not a member" socket_group_agent_only

# ---- root-owned, non-group/world-writable paths --------------------------------
root_safe_dir() { # path: root:root dir, no group/world write
  local m
  [[ -d "$1" && ! -L "$1" && "$(stat -c '%u:%g' "$1")" == 0:0 ]] || return 1
  m="$(stat -c '%a' "$1")"
  (((8#$m & 8#022) == 0))
}
root_safe_chain() { # path: the directory and every ancestor
  local p="$1"
  while :; do
    root_safe_dir "$p" || return 1
    [[ "$p" == / ]] && return 0
    p="$(dirname "$p")"
  done
}
for d in "$ROOT" "$(dirname "$QSOCK")" "$(dirname "$SSOCK")" "$BIN" "$(dirname "$STATE")"; do
  check "$d and all ancestors are root:root and not group/world-writable" root_safe_chain "$d"
done

mode_is() { [[ "$(stat -c '%a' "$1")" == "$2" ]]; }
check "$(dirname "$STATE") mode is 0700 (checkout user cannot list the ledger)" mode_is "$(dirname "$STATE")" 700
check "$ROOT mode is 0711 (no listing for others)" mode_is "$ROOT" 711
ledger_ok() { [[ -f "$STATE" && ! -L "$STATE" && "$(stat -c '%u:%g:%a' "$STATE")" == 0:0:600 ]]; }
if is_root; then check "ledger $STATE exists, regular, root:root 0600 [root]" ledger_ok
else skp "ledger $STATE exists, regular, root:root 0600 [root]"; fi

# ---- trusted binary directory ---------------------------------------------------
bin_files_trusted() {
  local f
  for f in checkout-quota-service checkout-exec git; do
    [[ -f "$BIN/$f" && ! -L "$BIN/$f" && "$(stat -c '%u' "$BIN/$f")" == 0 ]] || return 1
    (((8#$(stat -c '%a' "$BIN/$f") & 8#6022) == 0)) || return 1
    [[ -x "$BIN/$f" ]] || return 1
  done
}
check "helper binaries and git in $BIN are root-owned regular files, executable, no group/world write, no set-id" bin_files_trusted
git_is_et_exec() { # checkout-exec only accepts ELF type ET_EXEC (static, non-PIE)
  [[ "$(od -An -v -tx1 -j16 -N2 "$BIN/git" | tr -d ' \n')" == 0200 ]]
}
check "trusted git is ELF ET_EXEC (static, non-PIE), as checkout-exec requires" git_is_et_exec

# ---- sockets --------------------------------------------------------------------
socket_ok() { # path
  [[ -S "$1" && ! -L "$1" && "$(stat -c '%u:%g:%a' "$1")" == "0:$AGID:620" ]]
}
check "quota socket $QSOCK exists, root:$AGID mode 620" socket_ok "$QSOCK"
check "spawn socket $SSOCK exists, root:$AGID mode 620" socket_ok "$SSOCK"

# ---- cgroup delegation ----------------------------------------------------------
cg_path() { systemctl show -p ControlGroup --value "$EXEC_UNIT" 2>/dev/null; }
cgroup_delegated() {
  local cg base c f
  [[ "$(stat -fc %T /sys/fs/cgroup)" == cgroup2fs ]] || return 1
  cg="$(cg_path)"
  [[ -n "$cg" ]] || return 1
  base="/sys/fs/cgroup$cg"
  [[ -d "$base/leaves" ]] || return 1
  for f in "$base" "$base/leaves"; do
    for c in cpu io memory pids; do
      grep -qw "$c" "$f/cgroup.subtree_control" || return 1
    done
  done
  [[ -z "$(tr -d '[:space:]' <"$base/leaves/cgroup.procs")" ]] || return 1 # root must hold no processes
  [[ "$(stat -c '%u:%a' "$base/leaves")" == 0:755 ]]
}
check "cgroup v2; exec unit cgroup and its 'leaves' root have cpu io memory pids enabled, empty, root-owned 0755" cgroup_delegated

# ---- units ----------------------------------------------------------------------
check "$QUOTA_UNIT is active" systemctl is-active --quiet "$QUOTA_UNIT"
check "$EXEC_UNIT is active" systemctl is-active --quiet "$EXEC_UNIT"
no_leaked_leaves() {
  local base
  base="/sys/fs/cgroup$(cg_path)/leaves"
  [[ -d "$base" && -z "$(find "$base" -mindepth 1 -maxdepth 1 -type d -name 'checkout-*')" ]]
}
check "no leaked checkout cgroup leaves (idle host)" no_leaked_leaves

printf '\n%d passed, %d failed, %d skipped\n' "$pass" "$fail" "$skip"
if ((skip > 0)); then printf 'note: %d root-only check(s) skipped; re-run with sudo for full coverage\n' "$skip"; fi
((fail == 0))
