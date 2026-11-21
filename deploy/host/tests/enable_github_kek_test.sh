#!/usr/bin/env bash
# Container harness for `enable-github.sh --kek-only` (plan 24-B installer hook) and for the
# unchanged full path. Runs the REAL script as root in a throwaway ubuntu:24.04 container; stubbed:
# docker (psql answers), systemctl, openssl, autodeploy-run (records argv and whether it had a tty).
# Skipped without Docker. Usage: deploy/host/tests/enable_github_kek_test.sh
set -uo pipefail

TESTS_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
KIT_DIR="$(cd -- "$TESTS_DIR/.." && pwd)"
IMAGE="${STRUCTURE_TEST_IMAGE:-ubuntu:24.04}"
FAILS=0
pass() { printf 'ok   %s\n' "$1"; }
fail() { printf 'FAIL %s\n' "$1" >&2; FAILS=$((FAILS + 1)); }

if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
  printf 'SKIP enable_github_kek_test: Docker not available\n'
  exit 0
fi

IFS= read -r -d '' DRIVER <<'DRIVEREOF' || true
set -u
RC=0
ck() { if eval "$2"; then echo "CHECK ok   $1"; else echo "CHECK FAIL $1"; RC=1; fi; }
groupadd --gid 802 autodeploy-controlplane; useradd --system --uid 802 --gid 802 --no-create-home --shell /usr/sbin/nologin autodeploy-controlplane
S=/tmp/stubs; mkdir -p $S /tmp/log /etc/autodeploy/controlplane /usr/local/lib/autodeploy/app
: >/etc/autodeploy/controlplane.env
echo "postgres://x" >/etc/autodeploy/controlplane/database-url
install -m 0755 /dev/null /usr/local/lib/autodeploy/app/secrets
cat >/usr/local/lib/autodeploy/autodeploy-run <<'X'
#!/bin/bash
echo "run $* tty_in=$([[ -t 0 ]] && echo y || echo n) tty_out=$([[ -t 1 ]] && echo y || echo n)" >>/tmp/log/run
X
chmod 0755 /usr/local/lib/autodeploy/autodeploy-run
# The script insists on /usr/bin/docker; this is a throwaway container.
cat >/usr/bin/docker <<'X'
#!/bin/bash
sql="${*: -1}"
case "$sql" in
  "SELECT 1 FROM"*) echo 1 ;;
  "SELECT id FROM secret_database_generation"*) echo ABCDEFGHIJKLMNOP1234 ;;
  "SELECT count(*)"*) [[ -e /tmp/ledger ]] && echo 1 || echo 0 ;;
esac
X
chmod 0755 /usr/bin/docker
cat >$S/systemctl <<'X'
#!/bin/bash
echo "systemctl $*" >>/tmp/log/systemctl
[[ "$1" == is-active ]] && echo active
exit 0
X
cat >$S/openssl <<'X'
#!/bin/bash
[[ "$1" == rand ]] && { echo 00112233445566778899aabbccddeeff; exit 0; }
exit 0
X
cat >$S/journalctl <<'X'
#!/bin/bash
exit 0
X
cat >$S/sleep <<'X'
#!/bin/bash
exit 0
X
chmod +x $S/*
export PATH="$S:$PATH"
E=/etc/autodeploy; CPD=$E/controlplane
K=/kit/deploy/host/enable-github.sh

echo "== --kek-only"
"$K" --kek-only </dev/null >/tmp/out 2>&1 | cat; RUNRC=${PIPESTATUS[0]}
ck "exit 0 without a terminal" '((RUNRC == 0))'
ck "KEK is 32 bytes 0600 control-plane-owned" '[[ "$(stat -c "%s %a %U" $CPD/secret-kek-1)" == "32 600 autodeploy-controlplane" ]]'
ck "manifest is the single-key manifest" 'grep -q "\"active_key_id\":\"kek-1\",\"database_generation_id\":\"ABCDEFGHIJKLMNOP1234\"" $CPD/kek-manifest.json'
ck "env file has only the secret subsystem lines" '[[ "$(grep -vc "^#" $E/controlplane-github.env)" == 2 ]] && grep -qx "AUTODEPLOY_SECRET_SUBSYSTEM_ENABLED=true" $E/controlplane-github.env && grep -qx "AUTODEPLOY_KEK_MANIFEST_FILE=$CPD/kek-manifest.json" $E/controlplane-github.env && ! grep -q GITHUB_APP $E/controlplane-github.env'
ck "no App key and no webhook secret created" '[[ ! -e $CPD/github-app-private-key && ! -e $CPD/github-webhook-secret-1 ]]'
ck "ledger init ran once, with no terminal" '[[ "$(grep -c "secrets secret init-active-kek --kek kek-1 tty_in=n tty_out=n" /tmp/log/run)" == 1 ]]'
ck "control plane NOT restarted" '! grep -q "restart" /tmp/log/systemctl 2>/dev/null'
touch /tmp/ledger; : >/tmp/log/run
"$K" --kek-only </dev/null >/tmp/out 2>&1; RUNRC=$?
ck "rerun is idempotent: ok, no second ledger init, KEK unchanged" '((RUNRC == 0)) && [[ ! -s /tmp/log/run ]] && [[ "$(stat -c %s $CPD/secret-kek-1)" == 32 ]]'
printf 'AUTODEPLOY_GITHUB_APP_ENABLED=true\n' >$E/controlplane-github.env
"$K" --kek-only </dev/null >/tmp/out 2>&1
ck "an existing controlplane-github.env is kept" '[[ "$(cat $E/controlplane-github.env)" == "AUTODEPLOY_GITHUB_APP_ENABLED=true" ]]'
"$K" --kek-only --app-id 5 </dev/null >/tmp/out 2>&1; RUNRC=$?
ck "--kek-only with GitHub flags refused" '((RUNRC != 0)) && grep -q "no GitHub App flags" /tmp/out'

echo "== full path unchanged (no terminal: still stops at the TTY guard, after writing the files)"
rm -f /tmp/ledger $CPD/secret-kek-1 $CPD/kek-manifest.json $E/controlplane-github.env; : >/tmp/log/run
printf -- '-----BEGIN RSA PRIVATE KEY-----\nAAAA\n-----END RSA PRIVATE KEY-----\n' >/tmp/app.pem
"$K" --app-id 5 --client-id Iv1.abc --slug my-app --owner-id 7 --owner-type User --private-key /tmp/app.pem </dev/null >/tmp/out 2>&1; RUNRC=$?
ck "full mode still refuses without a terminal" '((RUNRC != 0)) && grep -q "interactive terminal" /tmp/out && [[ ! -s /tmp/log/run ]]'
ck "full mode wrote the App key, webhook secret and the full env" '[[ -s $CPD/github-app-private-key && -s $CPD/github-webhook-secret-1 ]] && grep -qx "AUTODEPLOY_GITHUB_APP_ENABLED=true" $E/controlplane-github.env && grep -qx "AUTODEPLOY_GITHUB_APP_ID=5" $E/controlplane-github.env'
"$K" --private-key /tmp/app.pem </dev/null >/tmp/out 2>&1; RUNRC=$?
ck "full mode still validates its flags" '((RUNRC != 0)) && grep -q -- "--app-id" /tmp/out'
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
