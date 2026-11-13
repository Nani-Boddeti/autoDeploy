#!/usr/bin/env bash
# AutoDeploy host kit: one-shot diagnostic for the two privileged checkout helpers.
#
# Usage (as root, from the kit directory; arguments go to install-checkout.sh verbatim,
# the LAST one must be BINDIR, the directory with the prebuilt binaries):
#   sudo deploy/host/diagnose.sh [--git FILE] [--image-size SIZE] BINDIR
#
# What it does, continuing past failures so a single run answers as much as possible:
#   1. install-checkout.sh <args>                      -> install.out
#   2. preflight-checkout.sh                           -> preflight.out
#   3. if a helper unit is not active: re-run ONLY that unit once under strace through a
#      temporary systemd drop-in (SystemCallFilter=ptrace, CAP_SYS_PTRACE, Restart=no)
#   4. if both are active: run the privileged smoke test (BINDIR/gitcheckout.test, as
#      autodeploy-agent, env from smoke.env or smoke.env.example). If it fails, restart both
#      helpers under strace and run the smoke once more to capture the spawn path.
#   5. everything lands in ONE directory /var/tmp/autodeploy-diag/<timestamp>/ (world readable)
#      and a short summary is printed. Drop-ins and temp files are always removed (trap).
#
# No secrets are read or written: the units carry only IDs, paths and limits, and strace is
# run without -v, so environment blocks are not dumped.
set -uo pipefail

KIT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly KIT_DIR
readonly QUOTA_UNIT=autodeploy-checkout-quota.service
readonly EXEC_UNIT=autodeploy-checkout-exec.service
readonly UNIT_FILE_DIR=/etc/systemd/system
readonly DROPIN_DIR=/run/systemd/system
readonly AGENT_USER=autodeploy-agent
readonly STATE_ROOT=/var/lib/autodeploy
# Trace files must live where the unit's own sandbox lets it write and PrivateTmp does not hide
# them: the exec unit has no ProtectSystem; the quota unit is ProtectSystem=strict with only
# its ledger directory writable.
readonly EXEC_TRACE_DIR=$STATE_ROOT
readonly QUOTA_TRACE_DIR=$STATE_ROOT/checkout-quota

die() { printf 'diagnose: %s\n' "$*" >&2; exit 2; }
[[ $EUID -eq 0 ]] || die "run as root (sudo $0 [install args] BINDIR)"
(($# >= 1)) || die "usage: sudo $0 [--git FILE] [--image-size SIZE] BINDIR"
BINDIR="${!#}"
[[ -d $BINDIR ]] || die "BINDIR '$BINDIR' is not a directory (it must be the last argument)"
HAVE_STRACE=1
command -v strace >/dev/null || { HAVE_STRACE=0; printf 'diagnose: strace not installed (apt install strace); traces will be skipped\n' >&2; }

TS="$(date +%Y%m%d-%H%M%S)"
REPORT="/var/tmp/autodeploy-diag/$TS"
mkdir -p "$REPORT" || die "cannot create $REPORT"
chmod 0755 /var/tmp/autodeploy-diag "$REPORT"
SUMMARY="$REPORT/summary.txt"
: >"$SUMMARY"

DROPINS=()
TRACES=()
SMOKE_TMP=""
# shellcheck disable=SC2317  # invoked through the EXIT trap
cleanup() {
  local d reload=0
  for d in "${DROPINS[@]}"; do
    if [[ -e $d ]]; then
      rm -f -- "$d"
      rmdir --ignore-fail-on-non-empty -- "$(dirname -- "$d")" 2>/dev/null
      reload=1
    fi
  done
  if ((reload)); then systemctl daemon-reload; fi
  if ((${#TRACES[@]})); then rm -f -- "${TRACES[@]}"; fi
  if [[ -n $SMOKE_TMP ]]; then rm -rf -- "$SMOKE_TMP"; fi
  chmod -R a+rX "$REPORT" 2>/dev/null
  return 0
}
trap cleanup EXIT
trap 'exit 130' INT TERM

unit_state() { systemctl is-active "$1" 2>/dev/null || true; }

# ---- 1. install ---------------------------------------------------------------------
bash "$KIT_DIR/install-checkout.sh" "$@" >"$REPORT/install.out" 2>&1
INSTALL_RC=$?

# ---- 2. preflight -------------------------------------------------------------------
bash "$KIT_DIR/preflight-checkout.sh" >"$REPORT/preflight.out" 2>&1
PREFLIGHT_RC=$?

# ---- host facts (read-only, no environment dump) -------------------------------------
facts() {
  echo "## $(date -Is) $(uname -srm)"
  systemctl --version | head -n1
  echo "## units"
  systemctl show "$QUOTA_UNIT" "$EXEC_UNIT" -p Id -p ActiveState -p SubState -p Result -p ExecMainStatus -p NRestarts
  echo "## modes"
  ls -ld /usr/local/lib/autodeploy /usr/local/lib/autodeploy/bin "$STATE_ROOT" "$STATE_ROOT/checkout-exec" \
    "$STATE_ROOT/checkout-quota" /srv/autodeploy/checkout /run/autodeploy-checkout-quota 2>&1
  ls -l /usr/local/lib/autodeploy/bin "$STATE_ROOT/checkout-exec" /run/autodeploy-checkout-quota 2>&1
  echo "## checkout mount"
  findmnt -n -o TARGET,SOURCE,MAJ:MIN,FSTYPE,OPTIONS --target /srv/autodeploy/checkout 2>&1
  echo "## exec cgroup"
  local cg=/sys/fs/cgroup/system.slice/$EXEC_UNIT f
  for f in cgroup.controllers cgroup.subtree_control cgroup.procs leaves/cgroup.subtree_control; do
    printf '%s: ' "$f"
    tr '\n' ' ' <"$cg/$f" 2>&1
    echo
  done
  ls -l "$cg/leaves/io.max" "$cg/leaves/cgroup.kill" 2>&1
  echo "## identities"
  id "$AGENT_USER" 2>&1
  id autodeploy-checkout 2>&1
}
facts >"$REPORT/host-facts.txt" 2>&1

save_unit_logs() { # suffix
  local u
  for u in "$QUOTA_UNIT" "$EXEC_UNIT"; do
    journalctl -u "$u" -n 200 --no-pager >"$REPORT/journal-${u%.service}$1.txt" 2>&1
    systemctl status "$u" --no-pager -l >"$REPORT/status-${u%.service}$1.txt" 2>&1
  done
}

# ---- strace drop-ins -----------------------------------------------------------------
# Original ExecStart (continuation lines joined) read from the installed unit, so the traced
# command cannot drift from the real one. Environment substitution is done by systemd.
unit_execstart() { # unit
  awk '
    /^ExecStart=/ { p = 1 }
    p {
      l = $0
      sub(/^ExecStart=/, "", l)
      if (l ~ /\\$/) { sub(/\\$/, "", l); printf "%s ", l } else { printf "%s\n", l; exit }
    }
  ' "$UNIT_FILE_DIR/$1"
}

trace_file_for() { # unit
  if [[ $1 == "$QUOTA_UNIT" ]]; then echo "$QUOTA_TRACE_DIR/diag-quota-$TS.strace"; else echo "$EXEC_TRACE_DIR/diag-exec-$TS.strace"; fi
}

install_dropin() { # unit
  local u=$1 d="$DROPIN_DIR/$1.d" trace cmd
  trace="$(trace_file_for "$u")"
  if [[ $u == "$EXEC_UNIT" ]]; then
    cmd=/usr/local/lib/autodeploy/checkout-exec-launch # the unit's own ExecStart
  else
    cmd="$(unit_execstart "$u")"
  fi
  mkdir -p "$d"
  DROPINS+=("$d/zz-diagnose.conf")
  TRACES+=("$trace")
  cat >"$d/zz-diagnose.conf" <<EOF
# Temporary (diagnose.sh). Removed automatically.
[Service]
Restart=no
SystemCallFilter=ptrace
CapabilityBoundingSet=CAP_SYS_PTRACE
ExecStart=
ExecStart=$(command -v strace) -f -s 128 -o $trace -- $cmd
EOF
}

reset_unit() {
  systemctl stop "$1" 2>/dev/null
  systemctl reset-failed "$1" 2>/dev/null
  return 0
}

# Trim one raw strace file into the report.
analyse_trace() { # unit
  local u=$1 f out pid cpids c
  f="$(trace_file_for "$u")"
  out="$REPORT/trace-${u%.service}"
  if [[ ! -s $f ]]; then echo "no trace written ($f)" >"$out.note.txt"; return; fi
  pid="$(grep -m1 -E 'execve\("[^"]*/(checkout-exec|checkout-quota-service)"' "$f" | awk '{print $1}')"
  [[ -n $pid ]] || pid="$(head -n1 "$f" | awk '{print $1}')"
  grep -E "^$pid " "$f" | tail -n 400 >"$out.helper-tail.txt"
  grep -E ' = -1 ' "$f" | tail -n 600 >"$out.errors.txt"
  # Spawn children: every pid that tried to exec the checkout child.
  cpids="$(grep -E 'execve\("/proc/self/exe"' "$f" | awk '{print $1}' | sort -u | head -n 3)"
  : >"$out.child.txt"
  for c in $cpids; do
    printf '### pid %s\n' "$c" >>"$out.child.txt"
    grep -E "^$c " "$f" | tail -n 150 >>"$out.child.txt"
  done
  printf 'helper pid %s; raw trace lines %s\n' "$pid" "$(wc -l <"$f")" >"$out.note.txt"
}

trace_units() { # unit...   (stop, drop-in, start once)
  local u
  ((HAVE_STRACE)) || return 1
  for u in "$@"; do
    reset_unit "$u"
    install_dropin "$u"
  done
  systemctl daemon-reload
  for u in "$@"; do systemctl start "$u" >>"$REPORT/trace-start.out" 2>&1; done
  return 0
}

untrace_units() { # unit...  (cleanup also removes drop-ins; here a normal instance is restored)
  local u
  for u in "$@"; do
    rm -f -- "$DROPIN_DIR/$u.d/zz-diagnose.conf"
    rmdir --ignore-fail-on-non-empty "$DROPIN_DIR/$u.d" 2>/dev/null
  done
  systemctl daemon-reload
  for u in "$@"; do
    if [[ $(unit_state "$u") == active ]]; then systemctl restart "$u" >/dev/null 2>&1; else reset_unit "$u"; fi
  done
}

# ---- smoke ----------------------------------------------------------------------------
SMOKE_RC=-1
SMOKE_NOTE=""
run_smoke() { # outfile
  local out=$1 envfile line
  local -a envargs=()
  if [[ ! -x $BINDIR/gitcheckout.test ]]; then
    SMOKE_NOTE="$BINDIR/gitcheckout.test missing (go test -c -tags=checkout_integration -o gitcheckout.test ./internal/gitcheckout)"
    echo "$SMOKE_NOTE" >"$out"
    SMOKE_RC=-2
    return
  fi
  envfile="$KIT_DIR/smoke.env"
  [[ -r $envfile ]] || envfile="$KIT_DIR/smoke.env.example"
  while IFS= read -r line; do
    [[ -z $line || $line == \#* ]] && continue
    envargs+=("$line")
  done <"$envfile"
  [[ -n $SMOKE_TMP ]] || SMOKE_TMP="$(mktemp -d)" # root-owned 0700
  install -m 0755 -o root -g root "$BINDIR/gitcheckout.test" "$SMOKE_TMP/gitcheckout.test"
  chmod 0711 "$SMOKE_TMP" # the agent must be able to traverse it
  timeout 240 runuser -u "$AGENT_USER" -- env "${envargs[@]}" "$SMOKE_TMP/gitcheckout.test" \
    -test.run TestPrivilegedHelperIsolation -test.count=1 -test.v >"$out" 2>&1
  SMOKE_RC=$?
}

# ---- 3/4. decide ----------------------------------------------------------------------
QSTATE="$(unit_state "$QUOTA_UNIT")"
ESTATE="$(unit_state "$EXEC_UNIT")"
TRACED=""
SMOKE1_RC=-1
SMOKE2_RC=""
if [[ $QSTATE == active && $ESTATE == active ]]; then
  run_smoke "$REPORT/smoke.out"
  SMOKE1_RC=$SMOKE_RC
  if ((SMOKE1_RC > 0)); then
    TRACED="quota exec (smoke failed)"
    if trace_units "$QUOTA_UNIT" "$EXEC_UNIT"; then
      if [[ $(unit_state "$QUOTA_UNIT") == active && $(unit_state "$EXEC_UNIT") == active ]]; then
        run_smoke "$REPORT/smoke-traced.out"
        SMOKE2_RC=$SMOKE_RC
      fi
      save_unit_logs -traced
      analyse_trace "$QUOTA_UNIT"
      analyse_trace "$EXEC_UNIT"
      untrace_units "$QUOTA_UNIT" "$EXEC_UNIT"
    fi
  fi
else
  failed=()
  [[ $QSTATE == active ]] || failed+=("$QUOTA_UNIT")
  [[ $ESTATE == active ]] || failed+=("$EXEC_UNIT")
  save_unit_logs -before
  TRACED="${failed[*]} (not active)"
  if trace_units "${failed[@]}"; then
    # Both helpers may come up under strace (e.g. the failure was transient): still exercise the spawn path.
    if [[ $(unit_state "$QUOTA_UNIT") == active && $(unit_state "$EXEC_UNIT") == active ]]; then
      run_smoke "$REPORT/smoke-traced.out"
      SMOKE1_RC=$SMOKE_RC
    fi
    save_unit_logs -traced
    for u in "${failed[@]}"; do analyse_trace "$u"; done
    untrace_units "${failed[@]}"
  fi
fi
save_unit_logs ""
find "/sys/fs/cgroup/system.slice/$EXEC_UNIT/leaves" -maxdepth 1 -name 'checkout-*' 2>/dev/null | wc -l >"$REPORT/leaked-leaves-count.txt"

# ---- summary --------------------------------------------------------------------------
summary() {
  local t e
  echo "diagnose $TS  report: $REPORT"
  echo "install rc=$INSTALL_RC | preflight rc=$PREFLIGHT_RC | quota unit: $QSTATE | exec unit: $ESTATE"
  [[ -z $TRACED ]] || echo "strace pass on: $TRACED"
  if [[ -n $SMOKE_NOTE ]]; then
    echo "smoke: ERROR: $SMOKE_NOTE"
  elif ((SMOKE1_RC == 0)); then
    echo "smoke: PASS"
  elif ((SMOKE1_RC > 0)); then
    echo "smoke: FAIL rc=$SMOKE1_RC (smoke.out; traced rerun rc=${SMOKE2_RC:-n/a})"
  else
    echo "smoke: not run (a helper unit was not active)"
  fi
  echo "-- failed preflight checks:"
  grep -E '^FAIL' "$REPORT/preflight.out" | head -n 5
  echo "-- install problems:"
  grep -iE 'error|fail|refus|die:' "$REPORT/install.out" | tail -n 2
  echo "-- last journal lines (exec / quota):"
  tail -n 3 "$REPORT/journal-autodeploy-checkout-exec.txt" | cut -c1-200
  tail -n 2 "$REPORT/journal-autodeploy-checkout-quota.txt" | cut -c1-200
  for t in smoke.out smoke-traced.out; do
    [[ -f $REPORT/$t ]] || continue
    echo "-- $t tail:"
    tail -n 4 "$REPORT/$t" | cut -c1-200
  done
  for t in exec quota; do
    e="$REPORT/trace-autodeploy-checkout-$t.errors.txt"
    [[ -s $e ]] || continue
    echo "-- last failing syscalls ($t, ENOENT/EAGAIN/ENOTTY noise filtered):"
    grep -vE 'ENOENT|EAGAIN|ENOTTY|ERESTART|ECHILD' "$e" | tail -n 4 | cut -c1-200
  done
}
summary 2>&1 | head -n 30 | tee -a "$SUMMARY"
chmod -R a+rX "$REPORT"
echo "full report: $REPORT"
if ((INSTALL_RC != 0 || PREFLIGHT_RC != 0 || SMOKE1_RC != 0)); then exit 1; fi
exit 0
