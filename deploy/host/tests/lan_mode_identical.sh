#!/usr/bin/env bash
# Pins install-app.sh's generated output. LAN mode must stay byte-identical to the pre-22-5 kit
# (plus the single new host.env line AUTODEPLOY_TLS_MODE=lan); public mode is checked structurally.
#
# install-app.sh cannot run end-to-end off a target host (root, systemd, Docker), so this sources it
# with AUTODEPLOY_INSTALL_APP_SOURCE_ONLY=1 (defines the pure render_* functions, runs nothing) and
# compares their output with tests/golden/*, which was captured from the PRE-change install-app.sh
# (commit f9900ba) by evaluating its heredocs with the same variable values. The LAN unit and static
# config are not generated at all (installed verbatim), so they are compared to that commit's files.
# Usage: deploy/host/tests/lan_mode_identical.sh        (no root, no Docker needed)
set -euo pipefail

TESTS_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
KIT_DIR="$(cd -- "$TESTS_DIR/.." && pwd)"
GOLDEN="$TESTS_DIR/golden"
BASELINE=f9900ba
FAILS=0
SCRATCH="$(mktemp -d)" # left in place (the kit never deletes files); lives under $TMPDIR

pass() { printf 'ok   %s\n' "$1"; }
fail() { printf 'FAIL %s\n' "$1" >&2; FAILS=$((FAILS + 1)); }
check_not() { local name="$1"; shift; if "$@" >/dev/null 2>&1; then fail "$name"; else pass "$name"; fi; }
check() { local name="$1"; shift; if "$@" >/dev/null 2>&1; then pass "$name"; else fail "$name"; fi; }

export AUTODEPLOY_INSTALL_APP_SOURCE_ONLY=1 AUTODEPLOY_HOSTS_FILE="$SCRATCH/hosts"
# shellcheck source=../install-app.sh
source "$KIT_DIR/install-app.sh"
set +e # install-app.sh turned errexit on; the checks below handle failures themselves

# Same values the golden files were captured with (defaults of the kit).
AGENT_GID=801
CP_HOST=autodeploy.autodeploy.test

# ---- LAN: byte identical ----------------------------------------------------------------
MODE=lan
{ cat "$GOLDEN/lan-host.env.head"; echo "AUTODEPLOY_TLS_MODE=lan"; } >"$SCRATCH/host.env.want"
render_host_env >"$SCRATCH/host.env.got"
check "lan host.env = pre-change output + AUTODEPLOY_TLS_MODE=lan" cmp "$SCRATCH/host.env.want" "$SCRATCH/host.env.got"
render_cp_route >"$SCRATCH/route.got"
check "lan control-plane router yaml identical" cmp "$GOLDEN/lan-cp-route.yml" "$SCRATCH/route.got"
render_traefik_unit "$KIT_DIR/systemd/autodeploy-traefik.service" >"$SCRATCH/unit.got"
check "lan traefik unit rendered = source file" cmp "$KIT_DIR/systemd/autodeploy-traefik.service" "$SCRATCH/unit.got"

if git -C "$KIT_DIR" rev-parse --verify -q "$BASELINE^{commit}" >/dev/null 2>&1; then
  for f in systemd/autodeploy-traefik.service traefik/traefik.yml traefik/autodeploy-tls.yml traefik/autodeploy-api.yml; do
    git -C "$KIT_DIR" show "$BASELINE:deploy/host/$f" >"$SCRATCH/base.file" 2>/dev/null
    check "lan file unchanged since $BASELINE: $f" cmp "$SCRATCH/base.file" "$KIT_DIR/$f"
  done
else
  echo "skip baseline file comparison: commit $BASELINE not available"
fi

# ---- public: structure ------------------------------------------------------------------
MODE=public CP_HOST=cp.example.com TRAEFIK_BIND=0.0.0.0
ACME_CHALLENGE=http ACME_STAGING=0 ACME_EMAIL=ops@example.com
render_host_env >"$SCRATCH/pub.env"
check "public host.env has mode/challenge/staging/email" bash -c \
  "grep -qx AUTODEPLOY_TLS_MODE=public '$SCRATCH/pub.env' && grep -qx AUTODEPLOY_ACME_CHALLENGE=http '$SCRATCH/pub.env' && grep -qx AUTODEPLOY_ACME_STAGING=0 '$SCRATCH/pub.env' && grep -qx AUTODEPLOY_ACME_EMAIL=ops@example.com '$SCRATCH/pub.env'"
render_cp_route >"$SCRATCH/pub-route.yml"
check "public router requests letsencrypt-production" grep -q 'certResolver: "letsencrypt-production"' "$SCRATCH/pub-route.yml"
render_traefik_unit "$KIT_DIR/systemd/autodeploy-traefik.service" >"$SCRATCH/pub.unit"
check "public unit has no tls path" bash -c "! grep -q 'autodeploy/tls\\|traefik/tls' '$SCRATCH/pub.unit'"
check "public unit mounts acme rw and asserts acme.json" bash -c \
  "grep -qxF -- '  -v /var/lib/autodeploy/traefik/acme:/etc/traefik/acme \\' '$SCRATCH/pub.unit' && grep -qx 'AssertPathExists=/var/lib/autodeploy/traefik/acme/acme.json' '$SCRATCH/pub.unit'"
diff "$KIT_DIR/systemd/autodeploy-traefik.service" "$SCRATCH/pub.unit" | grep -c '^[<>]' >"$SCRATCH/ndiff" || true
check "public unit differs from lan in exactly 5 diff lines (3 tls lines out, 2 acme lines in)" test "$(cat "$SCRATCH/ndiff")" = 5

for combo in "http 0 ops@example.com" "tls-alpn 1 ops@example.com" "http 1 " "tls-alpn 0 "; do
  read -r ACME_CHALLENGE ACME_STAGING ACME_EMAIL <<<"$combo"
  out="$SCRATCH/static-$ACME_CHALLENGE-$ACME_STAGING-${ACME_EMAIL:+email}.yml"
  render_traefik_static "$KIT_DIR/traefik/traefik-public.yml" >"$out"
  name="static [$ACME_CHALLENGE staging=$ACME_STAGING email=${ACME_EMAIL:-none}]"
  check "$name no placeholder left" bash -c "! grep -v '^#' '$out' | grep -q '@ACME_'"
  if ((ACME_STAGING)); then check "$name staging CA" grep -qx '      caServer: https://acme-staging-v02.api.letsencrypt.org/directory' "$out"
  else check "$name production CA" grep -qx '      caServer: https://acme-v02.api.letsencrypt.org/directory' "$out"; fi
  if [[ "$ACME_CHALLENGE" == tls-alpn ]]; then check "$name tlsChallenge" grep -qx '      tlsChallenge: {}' "$out"
  else check "$name httpChallenge web" grep -qx '        entryPoint: web' "$out"; fi
  if [[ -n "$ACME_EMAIL" ]]; then check "$name email present" grep -qx '      email: "ops@example.com"' "$out"
  else check "$name email omitted" bash -c "! grep -q '^      email:' '$out'"; fi
done

# ---- public host validation ---------------------------------------------------------------
for h in cp.example.com a.b.example.org; do check "host accepted: $h" valid_public_host "$h"; done
for h in autodeploy.test x.local x.localhost x.internal localhost 10.0.0.1 1.2.3.4 example .com Bad.Example.com 'a b.example.com'; do
  check_not "host rejected: $h" valid_public_host "$h"
done

# ---- public hosts block ---------------------------------------------------------------------
printf '127.0.0.1 localhost\n' >"$AUTODEPLOY_HOSTS_FILE"
HOSTS_FILE="$AUTODEPLOY_HOSTS_FILE" CP_HOST=cp.example.com
(manage_public_hosts_block) >/dev/null 2>&1
check "hosts block added" grep -qx '127.0.0.1 cp.example.com' "$HOSTS_FILE"
cp "$HOSTS_FILE" "$SCRATCH/hosts.1"
(manage_public_hosts_block) >/dev/null 2>&1
check "hosts block idempotent" cmp "$SCRATCH/hosts.1" "$HOSTS_FILE"
CP_HOST=cp2.example.com
(manage_public_hosts_block) >/dev/null 2>&1
check "hosts block updated in place, one block" bash -c \
  "grep -qx '127.0.0.1 cp2.example.com' '$HOSTS_FILE' && ! grep -q cp.example.com '$HOSTS_FILE' && test \$(grep -c 'BEGIN autodeploy-public-cp' '$HOSTS_FILE') = 1 && grep -qx '127.0.0.1 localhost' '$HOSTS_FILE'"
grep -v 'END autodeploy-public-cp' "$HOSTS_FILE" >"$SCRATCH/hosts.dmg" || true
cp "$SCRATCH/hosts.dmg" "$HOSTS_FILE"
if (manage_public_hosts_block) >/dev/null 2>&1; then fail "damaged hosts block refused"; else pass "damaged hosts block refused"; fi

echo
if ((FAILS)); then echo "$FAILS check(s) FAILED" >&2; exit 1; fi
echo "all checks passed"
