#!/usr/bin/env bash
# Container harness for deploy/host/enroll-local.sh (plan 24-D). Runs the real script as root in a
# throwaway ubuntu:24.04 container against a fake host layout; stubbed: curl (readyz),
# systemctl, autodeploy-run (the CLI), the agent binary, preflight-app.sh, preflight-checkout.sh.
# runuser, chown, stat, mktemp are real, so token-file ownership/permissions for the agent user
# are genuinely exercised. Every command's argv is recorded and the token value is searched for
# in all argv records and in the script's stdout/stderr.
# Skipped without Docker. Usage: deploy/host/tests/enroll_local_test.sh
set -uo pipefail

TESTS_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
KIT_DIR="$(cd -- "$TESTS_DIR/.." && pwd)"
IMAGE="${STRUCTURE_TEST_IMAGE:-ubuntu:24.04}"
FAILS=0
pass() { printf 'ok   %s\n' "$1"; }
fail() { printf 'FAIL %s\n' "$1" >&2; FAILS=$((FAILS + 1)); }

if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
  printf 'SKIP enroll_local_test: Docker not available\n'
  exit 0
fi

IFS= read -r -d '' DRIVER <<'DRIVEREOF' || true
set -u
RC=0
ck() { if eval "$2"; then echo "CHECK ok   $1"; else echo "CHECK FAIL $1"; RC=1; fi; }
TOKEN="enr.v1.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
ID=server_testid_1

cp -r /kit/deploy/host /tmp/kit
K=/tmp/kit
for p in preflight-app.sh preflight-checkout.sh; do
  printf '#!/bin/bash\necho "$0 $*" >>/tmp/log/preflight.log\nexit "${PREFLIGHT_RC:-0}"\n' >"$K/$p"
  chmod +x "$K/$p"
done
mkdir -p /tmp/log /tmp/ctl /tmp/stubs; chmod 1777 /tmp/log
groupadd --gid 801 autodeploy-agent; useradd --system --uid 801 --gid 801 --no-create-home --shell /usr/sbin/nologin autodeploy-agent
groupadd --gid 802 autodeploy-controlplane; useradd --system --uid 802 --gid 802 --no-create-home --shell /usr/sbin/nologin autodeploy-controlplane
mkdir -p /etc/autodeploy /etc/systemd/system /usr/local/lib/autodeploy/app /var/lib/autodeploy
printf 'AUTODEPLOY_ENV_GATEWAY=172.29.0.1\nAUTODEPLOY_CONTROLPLANE_PORT=8081\n' >/etc/autodeploy/host.env
printf 'AUTODEPLOY_AGENT_BASE_URL=https://cp.example.com\n' >/etc/autodeploy/agent.env
printf '%s\n' '# Owner-edited, never overwritten by install-app.sh.' 'AUTODEPLOY_AGENT_SERVER_ID=' >/etc/autodeploy/agent-local.env
touch /etc/systemd/system/autodeploy-controlplane.service /etc/systemd/system/autodeploy-agent.service
mkdir -p /var/lib/autodeploy/agent; chown autodeploy-agent:autodeploy-agent /var/lib/autodeploy/agent; chmod 0700 /var/lib/autodeploy/agent
chmod 0755 /var/lib/autodeploy /usr/local/lib/autodeploy /usr/local/lib/autodeploy/app
mkdir -p /var/lib/cpvictim; echo secret-db-url >/var/lib/cpvictim/database-url; chown -R autodeploy-controlplane:autodeploy-controlplane /var/lib/cpvictim; chmod 0711 /var/lib/cpvictim; chmod 0600 /var/lib/cpvictim/database-url

cat >/tmp/stubs/curl <<'X'
#!/bin/bash
echo "curl $*" >>/tmp/log/curl.argv
[[ "$*" == *"http://172.29.0.1:8081/readyz"* ]] || exit 7
exit 0
X
cat >/tmp/stubs/systemctl <<'X'
#!/bin/bash
echo "systemctl $*" >>/tmp/log/systemctl.argv
exit 0
X
cat >/tmp/stubs/sleep <<'X'
#!/bin/bash
exit 0
X
# The CLI stub. MODE (from /tmp/ctl/mode): ok | enrolled | enrolledfile | noadmin | disabled | reenroll | gone
# | fail | otherid | symlink | hardlink | nlink2 | panic | plantagent. Like the real autodeploy-run it writes as the
# control-plane user (runuser), so token-file ownership is realistic.
cat >/usr/local/lib/autodeploy/autodeploy-run <<'X'
#!/bin/bash
echo "autodeploy-run $*" >>/tmp/log/run.argv
mode="$(cat /tmp/ctl/mode)"
CP=autodeploy-controlplane
tf=""; prev=""; waitonly=0
for a in "$@"; do [[ $prev == --token-file ]] && tf="$a"; [[ $a == --wait-only ]] && waitonly=1; prev="$a"; done
if ((waitonly)); then
  # Phase marker: who owns the private dir while we only wait for the administrator?
  for d in /run/autodeploy-enroll.*; do [[ -d $d && ! -L $d ]] && stat -c %U "$d" >>/tmp/log/waitphase.owner; done
  case "$mode" in
    noadmin) echo "no administrator exists yet; finish first-run setup first" >&2; exit 20 ;;
    disabled) echo "the administrator account is disabled" >&2; exit 23 ;;
  esac
  exit 0
fi
stat -c %U "$(dirname "$tf")" >>/tmp/log/writephase.owner
w() { runuser -u "$CP" -- bash -c 'umask 077; echo "enr.v1.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" >"$0"' "$tf"; }
case "$mode" in
  enrolled) echo server_testid_1; exit 30 ;;
  enrolledfile) w; echo server_testid_1; exit 30 ;;
  panic) echo "panic: runtime error" >&2; exit 2 ;;
  plantagent) w; runuser -u "$CP" -- ln -s /etc/passwd "$(dirname "$tf")/token.agent"; echo server_testid_1; exit 0 ;;
  reenroll) echo "the server was enrolled before but has no active credential" >&2; exit 21 ;;
  gone) echo "recorded server id is not active" >&2; exit 22 ;;
  otherid) w; echo server_other_9; exit 0 ;;
  symlink) runuser -u "$CP" -- ln -s /etc/passwd "$tf"; echo server_testid_1; exit 0 ;;
  hardlink) runuser -u "$CP" -- ln /var/lib/cpvictim/database-url "$tf"; echo server_testid_1; exit 0 ;;
  nlink2) w; runuser -u "$CP" -- ln "$tf" "$tf.extra"; echo server_testid_1; exit 0 ;;
  fail) w; exit 1 ;;
  ok) w; echo server_testid_1; exit 0 ;;
esac
exit 9
X
# The agent stub: must be able to READ the token file as the agent user, like the real one.
cat >/usr/local/lib/autodeploy/app/agent <<'X'
#!/bin/bash
echo "agent $*" >>/tmp/log/agent.argv
tf=""; sd=""; prev=""
for a in "$@"; do
  [[ $prev == --token-file ]] && tf="$a"; [[ $prev == --state-dir ]] && sd="$a"; prev="$a"
done
[[ "$(id -un)" == autodeploy-agent ]] || { echo "not run as the agent user" >&2; exit 5; }
[[ "$(stat -c %a "$tf")" == 600 && "$(stat -c %U "$tf")" == autodeploy-agent ]] || { echo "bad token file perms" >&2; exit 6; }
tok="$(cat "$tf")" || exit 3
[[ "$tok" == "enr.v1.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" ]] || exit 4
[[ -e /tmp/ctl/agent-fails ]] && exit 1
(umask 077; echo credential >"$sd/credential")
exit 0
X
# install wrapper: records who owns the token directory at the moment root creates the agent copy.
cat >/tmp/stubs/install <<'X'
#!/bin/bash
d="$(dirname "${*: -1}")"
[[ $d == /run/autodeploy-enroll.* ]] && stat -c %U "$d" >>/tmp/log/install.dirowner
exec /usr/bin/install "$@"
X
chmod +x /tmp/stubs/* /usr/local/lib/autodeploy/autodeploy-run /usr/local/lib/autodeploy/app/agent
export PATH="/tmp/stubs:$PATH"

reset() { # between scenarios: fresh log dir, no credential, pristine env file
  rm -f /var/lib/autodeploy/agent/credential /tmp/ctl/agent-fails
  rm -rf /tmp/log; mkdir -p /tmp/log; chmod 1777 /tmp/log # the stubbed agent logs as its own user
  printf '%s\n' '# Owner-edited, never overwritten by install-app.sh.' 'AUTODEPLOY_AGENT_SERVER_ID=' >/etc/autodeploy/agent-local.env
  unset PREFLIGHT_RC
}
tmpdirs() { ls -d /run/autodeploy-enroll.* 2>/dev/null | wc -l; }
run() { # mode args...
  echo "$1" >/tmp/ctl/mode; shift
  "$K/enroll-local.sh" "$@" >/tmp/out.txt 2>&1; RUNRC=$?
}
token_leaked() { grep -rqF "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" /tmp/log /tmp/out.txt; }

echo "== success"
reset; run ok --name edge1 --wait 5m
ck "exit 0" '((RUNRC == 0))'
ck "credential written by the agent enroll" '[[ -s /var/lib/autodeploy/agent/credential ]]'
ck "token file and its directory removed" '((`tmpdirs` == 0))'
ck "token never in any recorded argv or in script output" '! token_leaked'
ck "agent enroll argv has no token, has --token-file" 'grep -q -- "--token-file /run/autodeploy-enroll\." /tmp/log/agent.argv && ! grep -q "enr.v1" /tmp/log/agent.argv'
ck "agent enroll got base-url, server id and state dir" 'grep -q -- "--base-url https://cp.example.com --server-id $ID --state-dir /var/lib/autodeploy/agent" /tmp/log/agent.argv'
ck "CLI got --name and --wait" 'grep -q -- "--name edge1" /tmp/log/run.argv && grep -q -- "--wait 5m" /tmp/log/run.argv'
ck "no recorded id yet: CLI not given --server-id" '! grep -q -- "--server-id" /tmp/log/run.argv'
ck "agent-local.env records the id exactly once, comment kept" '[[ "$(grep -c "^AUTODEPLOY_AGENT_SERVER_ID=$ID\$" /etc/autodeploy/agent-local.env)" == 1 && "$(grep -c "^AUTODEPLOY_AGENT_SERVER_ID=" /etc/autodeploy/agent-local.env)" == 1 ]] && grep -q "^# Owner-edited" /etc/autodeploy/agent-local.env'
ck "agent-local.env mode 0644 root" '[[ "$(stat -c "%a %U" /etc/autodeploy/agent-local.env)" == "644 root" ]]'
ck "agent started" 'grep -q "systemctl start autodeploy-agent.service" /tmp/log/systemctl.argv'
ck "both preflights ran" 'grep -q preflight-app /tmp/log/preflight.log && grep -q preflight-checkout /tmp/log/preflight.log'
ck "dir is root-owned during the wait phase, CP-owned only for the write call" '[[ "$(sort -u /tmp/log/waitphase.owner)" == root && "$(sort -u /tmp/log/writephase.owner)" == autodeploy-controlplane ]]'
ck "root took the directory back before touching the file" '[[ "$(sort -u /tmp/log/install.dirowner)" == root ]]'
ck "wait-only call carries no token file" 'grep -- "--wait-only" /tmp/log/run.argv | grep -vq -- "--token-file"'
ck "agent got a NEW file, not the CP-created inode" 'grep -q -- "--token-file /run/autodeploy-enroll\.[A-Za-z0-9]*/token\.agent" /tmp/log/agent.argv'
ck "readyz probed on the gateway address" 'grep -q "http://172.29.0.1:8081/readyz" /tmp/log/curl.argv'

echo "== rerun after enrollment is a no-op"
sum1="$(sha256sum /etc/autodeploy/agent-local.env)"; : >/tmp/log/run.argv
run ok
ck "exit 0" '((RUNRC == 0))'
ck "no-op message" 'grep -q "already enrolled" /tmp/out.txt'
ck "CLI not invoked" '[[ ! -s /tmp/log/run.argv ]]'
ck "env file unchanged" '[[ "$sum1" == "$(sha256sum /etc/autodeploy/agent-local.env)" ]]'
ck "no temp dir left" '((`tmpdirs` == 0))'

echo "== rerun with a recorded id and a lost credential (crash recovery)"
rm -f /var/lib/autodeploy/agent/credential; : >/tmp/log/run.argv
run ok
ck "exit 0" '((RUNRC == 0))'
ck "CLI given the recorded --server-id" 'grep -q -- "--server-id $ID" /tmp/log/run.argv'
ck "env key still present exactly once" '[[ "$(grep -c "^AUTODEPLOY_AGENT_SERVER_ID=" /etc/autodeploy/agent-local.env)" == 1 ]]'
ck "token never leaked" '! token_leaked'

echo "== agent enroll fails: token file gone, nonzero"
reset; touch /tmp/ctl/agent-fails; run ok
ck "nonzero exit" '((RUNRC != 0))'
ck "token directory removed after failure" '((`tmpdirs` == 0))'
ck "no credential" '[[ ! -e /var/lib/autodeploy/agent/credential ]]'
ck "agent not started" '! grep -q "start autodeploy-agent" /tmp/log/systemctl.argv 2>/dev/null'
ck "token never leaked" '! token_leaked'
ck "message tells to re-run" 'grep -q "Re-run" /tmp/out.txt'

echo "== CLI fails after writing the token file"
reset; run fail
ck "nonzero exit" '((RUNRC != 0))'
ck "token directory removed" '((`tmpdirs` == 0))'
ck "agent enroll not attempted" '[[ ! -e /tmp/log/agent.argv ]]'
ck "env file untouched" '! grep -q "^AUTODEPLOY_AGENT_SERVER_ID=server" /etc/autodeploy/agent-local.env'

echo "== no administrator yet"
reset; run noadmin
ck "nonzero exit" '((RUNRC != 0))'
ck "message names /setup and --wait" 'grep -q "no administrator exists yet" /tmp/out.txt && grep -q -- "--wait" /tmp/out.txt'
ck "temp dir removed" '((`tmpdirs` == 0))'

echo "== server enrolled in the control plane but this host has no credential"
reset; run enrolled
ck "nonzero exit with re-enroll guidance" '((RUNRC != 0)) && grep -q "Re-enroll" /tmp/out.txt'
ck "agent enroll not attempted" '[[ ! -e /tmp/log/agent.argv ]]'
ck "temp dir removed" '((`tmpdirs` == 0))'

echo "== different server id than the recorded one is refused"
reset; printf 'AUTODEPLOY_AGENT_SERVER_ID=server_recorded_5\n' >/etc/autodeploy/agent-local.env; run otherid
ck "nonzero exit" '((RUNRC != 0)) && grep -q "different server id" /tmp/out.txt'
ck "recorded id untouched" 'grep -q "^AUTODEPLOY_AGENT_SERVER_ID=server_recorded_5\$" /etc/autodeploy/agent-local.env'
ck "temp dir removed" '((`tmpdirs` == 0))'

echo "== token path swapped for a symlink is refused"
reset; run symlink
ck "nonzero exit, agent not run" '((RUNRC != 0)) && [[ ! -e /tmp/log/agent.argv ]]'
ck "/etc/passwd ownership untouched" '[[ "$(stat -c %U /etc/passwd)" == root ]]'
ck "temp dir removed" '((`tmpdirs` == 0))'

echo "== M1: control-plane user cannot make the script chown or consume a foreign inode"
reset; run hardlink
ck "hardlinked CP-owned file refused" '((RUNRC != 0)) && grep -qi "hard link\|link count" /tmp/out.txt && [[ ! -e /tmp/log/agent.argv ]]'
ck "target file still owned by the control-plane user, content intact" '[[ "$(stat -c %U /var/lib/cpvictim/database-url)" == autodeploy-controlplane && "$(cat /var/lib/cpvictim/database-url)" == secret-db-url ]]'
ck "temp dir removed" '((`tmpdirs` == 0))'
reset; run nlink2
ck "token file with link count 2 refused" '((RUNRC != 0)) && grep -qi "link" /tmp/out.txt && [[ ! -e /tmp/log/agent.argv ]]'
reset; run enrolledfile
ck "a file present in already-enrolled mode is refused, not consumed" '((RUNRC != 0)) && grep -qi "unexpected" /tmp/out.txt && [[ ! -e /tmp/log/agent.argv ]]'

echo "== M2: distinct guidance"
reset; run reenroll
ck "needs-reenroll: Servers > Re-enroll guidance" '((RUNRC != 0)) && grep -q "Re-enroll" /tmp/out.txt'
reset; printf 'AUTODEPLOY_AGENT_SERVER_ID=server_gone_7\n' >/etc/autodeploy/agent-local.env; run gone
ck "revoked/removed recorded id: tells to clear AUTODEPLOY_AGENT_SERVER_ID and re-run" '((RUNRC != 0)) && grep -q "revoked or removed" /tmp/out.txt && grep -q "AUTODEPLOY_AGENT_SERVER_ID" /tmp/out.txt && grep -q "agent-local.env" /tmp/out.txt'
reset; run disabled
ck "disabled administrator: clear message" '((RUNRC != 0)) && grep -qi "disabled" /tmp/out.txt'

echo "== M3: concurrent runs"
reset; (flock /run/autodeploy-enroll-local.lock /bin/sleep 15 &); /bin/sleep 1
run ok
ck "second run refused while another holds the lock" '((RUNRC != 0)) && grep -q "another enrollment is running" /tmp/out.txt && [[ ! -e /tmp/log/run.argv ]]'
pkill -f "/bin/sleep 15" 2>/dev/null; /bin/sleep 1

echo "== L8: stale temp dirs swept, symlinks never followed, record temp under the trap"
reset; mkdir -p /run/autodeploy-enroll.stale1 /tmp/keepdir; echo x >/run/autodeploy-enroll.stale1/token; touch -d '3 hours ago' /run/autodeploy-enroll.stale1
ln -s /tmp/keepdir /run/autodeploy-enroll.link1; touch -h -d '3 hours ago' /run/autodeploy-enroll.link1
mkdir -p /run/autodeploy-enroll.fresh1
run ok
ck "success" '((RUNRC == 0))'
ck "stale dir older than the token TTL swept" '[[ ! -e /run/autodeploy-enroll.stale1 ]]'
ck "symlink matching the prefix not followed or removed" '[[ -L /run/autodeploy-enroll.link1 && -d /tmp/keepdir ]]'
ck "fresh dir (younger than the TTL) left alone" '[[ -d /run/autodeploy-enroll.fresh1 ]]'
rm -f /run/autodeploy-enroll.link1; rmdir /run/autodeploy-enroll.fresh1

echo "== L6/L7: unreadable agent-local.env fails closed; names match ValidName"
reset; rm -f /etc/autodeploy/agent-local.env; mkdir /etc/autodeploy/agent-local.env; run ok
ck "unreadable agent-local.env refused, CLI not called for enrollment" '((RUNRC != 0)) && grep -q "agent-local.env" /tmp/out.txt && ! grep -q -- "--token-file" /tmp/log/run.argv 2>/dev/null'
rmdir /etc/autodeploy/agent-local.env
reset; run ok --name " lead"
ck "leading-space name refused" '((RUNRC != 0)) && [[ ! -e /tmp/log/run.argv ]]'
reset; run ok --name "trail "
ck "trailing-space name refused" '((RUNRC != 0)) && [[ ! -e /tmp/log/run.argv ]]'
reset; run ok --name "my host-1"
ck "inner space accepted" '((RUNRC == 0))'

echo "== hardening: lock path, token.agent plant, exit-code collision"
reset; rm -f /run/autodeploy-enroll-local.lock; ln -s /tmp/keepdir /run/autodeploy-enroll-local.lock; run ok
ck "symlink planted at the lock path refused" '((RUNRC != 0)) && grep -q "lock" /tmp/out.txt && [[ ! -e /tmp/log/run.argv ]]'
rm -f /run/autodeploy-enroll-local.lock
reset; run plantagent
ck "symlink planted at token.agent refused, target untouched" '((RUNRC != 0)) && [[ ! -e /tmp/log/agent.argv ]] && [[ "$(stat -c %U /etc/passwd)" == root ]] && grep -q "^root:" /etc/passwd && ! grep -q "enr.v1" /etc/passwd'
ck "temp dir removed" '((`tmpdirs` == 0))'
reset; run panic
ck "Go panic exit 2 is a generic failure, not 'no administrator'" '((RUNRC != 0)) && grep -q "failed" /tmp/out.txt && ! grep -q "no administrator exists yet" /tmp/out.txt'
ck "lock file never matched by the stale sweep prefix" '[[ "/run/autodeploy-enroll-local.lock" != /run/autodeploy-enroll.* ]]'

echo "== argument validation and preflight"
reset; run ok --wait banana
ck "bad --wait refused before any work" '((RUNRC != 0)) && [[ ! -e /tmp/log/run.argv ]]'
reset; run ok --bogus
ck "unknown flag refused" '((RUNRC != 0))'
reset; PREFLIGHT_RC=1 run ok
ck "preflight failure is reported but enrollment stands" '((RUNRC != 0)) && [[ -s /var/lib/autodeploy/agent/credential ]] && grep -q "preflight reported failures" /tmp/out.txt'
reset; rm /etc/systemd/system/autodeploy-agent.service; run ok
ck "missing unit refused before any CLI call" '((RUNRC != 0)) && [[ ! -e /tmp/log/run.argv ]] && grep -q "install-app.sh" /tmp/out.txt'
exit "$RC"
DRIVEREOF

KIT_PARENT="$(cd -- "$KIT_DIR/../.." && pwd)"
OUT="$(printf '%s\n' "$DRIVER" | docker run --rm -i --network none -v "$KIT_PARENT":/kit:ro "$IMAGE" bash -s 2>&1)"
RC=$?
printf '%s\n' "$OUT" | grep -E '^(CHECK|==)' | sed 's/^CHECK ok   /ok   /; s/^CHECK FAIL /FAIL /'
if ((RC != 0)); then
  fail "container driver reported failures (rc=$RC)"
  printf '%s\n' "$OUT" | grep -vE '^(CHECK ok|==)' >&2
fi
[[ $FAILS -eq 0 ]] && { printf 'all passed\n'; exit 0; }
printf '%d FAILED\n' "$FAILS" >&2
exit 1
