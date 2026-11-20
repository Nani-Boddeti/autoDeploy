#!/usr/bin/env bash
# Regression guard for "leftover heredoc body" bugs (the 22-5 install-app.sh H1 defect: the
# `<<ENVEOF` opener was removed but its body and a bare `ENVEOF` terminator stayed, so the shell ran
# `ENVEOF` as a command: exit 127 at step 5, invisible to `bash -n`).
#
# Part A (static, any host): every deploy/host/*.sh and libexec/* script is scanned for heredocs.
#   Openers: <<[-]WORD, <<[-]'WORD', <<[-]"WORD" (WORD = [A-Z_][A-Z0-9_]*; <<< and arithmetic
#   shifts are ignored). A terminator-looking line (a delimiter name used anywhere in the file, or
#   ^[A-Z0-9_]*EOF$) seen OUTSIDE a heredoc is an orphan; a heredoc still open at EOF is unterminated.
# Part B (needs Docker, skipped otherwise): runs install-app.sh PAST argument parsing, through step 5
#   (host.env, controlplane.env, agent.env, traefik config, route files, hosts block) and step 6
#   (unit install), in a throwaway ubuntu:24.04 container (the container IS the root filesystem: the
#   script's absolute /etc/autodeploy paths are real there). The repo is mounted read-only and copied
#   to /tmp inside. Stubbed on PATH: uname (x86_64), systemctl, docker, ip, ufw. Real: useradd,
#   groupadd, usermod, getent, id, install, sha256sum, stat. Fake BINDIR: five minimal ELF-header
#   files + SHA256SUMS. NOT executed: postgres/migrations (postgres reported inactive), service
#   restarts, ufw (--configure-firewall not passed), real Docker network creation, ACME.
#   Mutation checks reinsert the H1 leftover block into a COPY inside the container and require the
#   run to FAIL, proving the test catches the defect class.
# Usage: deploy/host/tests/script_structure_test.sh     (Docker needed for part B only)
set -uo pipefail

TESTS_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
KIT_DIR="$(cd -- "$TESTS_DIR/.." && pwd)"
IMAGE="${STRUCTURE_TEST_IMAGE:-ubuntu:24.04}"
FAILS=0
pass() { printf 'ok   %s\n' "$1"; }
fail() { printf 'FAIL %s\n' "$1" >&2; FAILS=$((FAILS + 1)); }

# ---------------------------------------------------------------- Part A: static heredoc scan
scan_heredocs() { # file -> prints problems, exit 1 if any
  awk '
    function trim_tabs(s) { sub(/^\t+/, "", s); return s }
    # Pass 1 (NR==FNR) collects delimiters; pass 2 checks.
    function openers(line,    rest, w, q, d, out) {
      out = ""
      rest = line
      while (match(rest, /<<-?["'\'']?[A-Z_][A-Z0-9_]*["'\'']?/)) {
        w = substr(rest, RSTART, RLENGTH)
        rest = substr(rest, RSTART + RLENGTH)
        if (substr(w, 3, 1) == "<") continue
        sub(/^<<-?["'\'']?/, "", w); sub(/["'\'']?$/, "", w)
        out = out " " w
      }
      return out
    }
    NR == FNR {
      if ($0 !~ /^[ \t]*#/) { n = split(openers($0), a, " "); for (i = 1; i <= n; i++) known[a[i]] = 1 }
      next
    }
    {
      if (inhd != "") {
        t = $0; if (dash) t = trim_tabs(t)
        if (t == inhd) { inhd = ""; dash = 0 }
        next
      }
      if ($0 ~ /^[ \t]*#/) next
      o = openers($0)
      if (o != "") {
        split(o, a, " ")
        inhd = a[1]; dash = ($0 ~ /<<-/) ? 1 : 0; start = FNR
        # a same-line second heredoc is not used in this kit; only the first is tracked
        next
      }
      if ($0 in known || $0 ~ /^[A-Z][A-Z0-9_]*EOF$/) {
        printf "%s:%d: orphan heredoc terminator \"%s\" outside any heredoc\n", FILENAME, FNR, $0; bad = 1
      }
    }
    END { if (inhd != "") { printf "%s: heredoc \"%s\" opened at line %d never terminated\n", FILENAME, inhd, start; bad = 1 } exit bad }
  ' "$1" "$1"
}

scripts=("$KIT_DIR"/*.sh "$KIT_DIR"/libexec/*)
for f in "${scripts[@]}"; do
  [[ -f "$f" ]] || continue
  if out="$(scan_heredocs "$f" 2>&1)"; then pass "heredocs balanced: ${f#"$KIT_DIR"/}"; else fail "heredoc structure: ${f#"$KIT_DIR"/}"; printf '%s\n' "$out" >&2; fi
done

# Self-test of the scanner: it must flag the exact H1 shape.
tmp="$(mktemp -d)"
printf '%s\n' 'f() {' '  cat <<ENVEOF' 'A=1' 'ENVEOF' '}' 'x=1' 'B=2' 'ENVEOF' >"$tmp/orphan.sh"
if scan_heredocs "$tmp/orphan.sh" >/dev/null 2>&1; then fail "scanner flags a leftover ENVEOF"; else pass "scanner flags a leftover ENVEOF"; fi
printf '%s\n' 'cat <<-X' '	body' '	X' 'cat <<EOF' 'open' >"$tmp/unterminated.sh"
if scan_heredocs "$tmp/unterminated.sh" >/dev/null 2>&1; then fail "scanner flags an unterminated heredoc"; else pass "scanner flags an unterminated heredoc"; fi

# ---------------------------------------------------------------- Part B: executed install
if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
  printf 'SKIP part B (executed install-app.sh): Docker not available\n'
  [[ $FAILS -eq 0 ]] || { printf '%d FAILED\n' "$FAILS" >&2; exit 1; }
  exit 0
fi

# Driver, executed inside the container: SCENARIO = lan | public | public-refuse | lan-h1 | public-h1
IFS= read -r -d '' DRIVER <<'DRIVEREOF' || true
set -u
SCENARIO="$1"
mode=lan; mutate=0; refuse=0
case "$SCENARIO" in
  public) mode=public ;;
  public-refuse) mode=public; refuse=1 ;;
  lan-h1) mutate=1 ;;
  public-h1) mode=public; mutate=1 ;;
esac
RC=0
ck() { if eval "$2"; then echo "CHECK ok   $1"; else echo "CHECK FAIL $1"; RC=1; fi; }

cp -r /kit /tmp/kit
K=/tmp/kit/deploy/host
[[ -d $K ]] || K=/tmp/kit
if ((mutate)); then
  # Reinsert the H1 leftover: old heredoc body + bare terminator after the render_host_env line.
  awk '{print} /^render_host_env \| put_generated/ {
    print "# Generated by deploy/host/install-app.sh. Non-secret. Do not edit by hand."
    print "AUTODEPLOY_ENV_NETWORK=$ENV_NETWORK"
    print "AUTODEPLOY_TLS_MODE=$MODE"
    print "ENVEOF" }' "$K/install-app.sh" >"$K/install-app.sh.new" && mv "$K/install-app.sh.new" "$K/install-app.sh"
  chmod +x "$K/install-app.sh"
fi

S=/tmp/stubs; mkdir -p "$S" /tmp/stubstate
cat >"$S/uname" <<'X'
#!/bin/bash
case "${1:-}" in -s) echo Linux ;; -m) echo x86_64 ;; *) echo Linux ;; esac
X
cat >"$S/systemctl" <<'X'
#!/bin/bash
if [[ "${1:-}" == is-active ]]; then [[ "${*: -1}" == docker.service ]]; exit; fi
exit 0
X
cat >"$S/ip" <<'X'
#!/bin/bash
exit 0
X
cat >"$S/ufw" <<'X'
#!/bin/bash
exit 0
X
cat >"$S/docker" <<'X'
#!/bin/bash
if [[ "${1:-}" == network ]]; then
  case "${2:-}" in
    create) touch /tmp/stubstate/net; exit 0 ;;
    ls) exit 0 ;;
    inspect)
      [[ -e /tmp/stubstate/net ]] || exit 1
      if [[ "${3:-}" == -f ]]; then
        case "$4" in
          *Subnet*) echo "172.29.0.0/24" ;;
          *IPRange*) echo "172.29.0.128/25" ;;
          *Gateway*) echo "172.29.0.1" ;;
          *bridge.name*) echo "br-autodeploy" ;;
        esac
      fi
      exit 0 ;;
  esac
fi
exit 0
X
chmod +x "$S"/*
export PATH="$S:$PATH"

groupadd docker; groupadd autodeploy-checkout; groupadd --gid 801 autodeploy-agent
useradd --system --uid 801 --gid 801 --no-create-home --shell /usr/sbin/nologin autodeploy-agent
mkdir -p /etc/autodeploy
echo "AUTODEPLOY_CHECKOUT_AGENT_GID=801" >/etc/autodeploy/checkout.env
BIN=/tmp/bindir; mkdir -p "$BIN"
for b in controlplane agent migrate admin secrets; do
  { printf '\177ELF\002\001\001\000\000\000\000\000\000\000\000\000\002\000\076\000'; echo "fake $b"; } >"$BIN/$b"
done
(cd "$BIN" && sha256sum controlplane agent migrate admin secrets >SHA256SUMS)
cp /etc/hosts /tmp/hosts-test
export AUTODEPLOY_HOSTS_FILE=/tmp/hosts-test

args=(--mode "$mode")
if [[ $mode == public ]]; then export AUTODEPLOY_CONTROLPLANE_HOST=cp.example.com; args+=(--acme-email ops@example.com); fi
if ((refuse)); then mkdir -p /var/lib/autodeploy/traefik/dynamic; : >/var/lib/autodeploy/traefik/dynamic/autodeploy-tls.yml; fi

OUT=/tmp/install.out
"$K/install-app.sh" "${args[@]}" "$BIN" >"$OUT" 2>&1
IRC=$?
sed 's/^/  | /' "$OUT" | tail -n 8

if ((refuse)); then
  ck "install refused (rc != 0)" '((IRC != 0))'
  ck "refusal names autodeploy-tls.yml" 'grep -q "autodeploy-tls.yml exists" "$OUT"'
  ck "autodeploy-tls.yml was NOT deleted" '[[ -e /var/lib/autodeploy/traefik/dynamic/autodeploy-tls.yml ]]'
  ck "nothing generated (no host.env)" '[[ ! -e /etc/autodeploy/host.env ]]'
  exit "$RC"
fi
ck "no 'command not found'" '! grep -q "command not found" "$OUT"'
ck "install-app.sh exit 0" '((IRC == 0))'
ck "installer reached the final summary" 'grep -q "^==> installed" "$OUT"'
for f in host.env controlplane.env agent.env agent-local.env traefik/traefik.yml; do
  ck "generated /etc/autodeploy/$f" "[[ -s /etc/autodeploy/$f ]]"
done
R=/var/lib/autodeploy/traefik/dynamic
ck "route file autodeploy-controlplane.yml" "grep -q 'Host(.*${AUTODEPLOY_CONTROLPLANE_HOST:-autodeploy.autodeploy.test}' $R/autodeploy-controlplane.yml"
ck "route file autodeploy-api.yml" "[[ -s $R/autodeploy-api.yml ]]"
ck "host.env has AUTODEPLOY_TLS_MODE=$mode" "grep -qx 'AUTODEPLOY_TLS_MODE=$mode' /etc/autodeploy/host.env"
ck "controlplane.env has AUTODEPLOY_PUBLIC_ORIGIN" "grep -q '^AUTODEPLOY_PUBLIC_ORIGIN=https://' /etc/autodeploy/controlplane.env"
ck "agent.env has AUTODEPLOY_AGENT_BASE_URL" "grep -q '^AUTODEPLOY_AGENT_BASE_URL=https://' /etc/autodeploy/agent.env"
ck "4 systemd units installed" '(( $(ls /etc/systemd/system/autodeploy-*.service | wc -l) == 4 ))'
if [[ $mode == public ]]; then
  ck "no autodeploy-tls.yml in public mode" "[[ ! -e $R/autodeploy-tls.yml ]]"
  ck "acme.json exists 0600" '[[ "$(stat -c %a /var/lib/autodeploy/traefik/acme/acme.json)" == 600 ]]'
  ck "traefik.yml has the ACME resolver" "grep -q letsencrypt-production /etc/autodeploy/traefik/traefik.yml"
  ck "hosts block maps cp.example.com" "grep -qx '127.0.0.1 cp.example.com' /tmp/hosts-test"
else
  ck "autodeploy-tls.yml installed in lan mode" "[[ -s $R/autodeploy-tls.yml ]]"
fi
exit "$RC"
DRIVEREOF

run_expect_pass() {
  run_scenario "$1"
  if [[ $SC_RC -eq 0 ]]; then pass "executed install-app.sh scenario: $1"; else fail "executed install-app.sh scenario: $1"; printf '%s\n' "$SC_OUT" >&2; fi
}
run_expect_fail() {
  run_scenario "$1"
  if [[ $SC_RC -ne 0 ]] && grep -q 'command not found' <<<"$SC_OUT"; then pass "mutation caught (H1 leftover reinserted): $1"
  else fail "mutation NOT caught: $1 (rc=$SC_RC)"; printf '%s\n' "$SC_OUT" >&2; fi
}
# Driver expects /kit/deploy/host: mount the repo root read-only.
KIT_DIR_PARENT="$(cd -- "$KIT_DIR/../.." && pwd)"
run_scenario() {
  SC_OUT="$(printf '%s\n' "$DRIVER" | docker run --rm -i --network none -v "$KIT_DIR_PARENT":/kit:ro "$IMAGE" bash -s -- "$1" 2>&1)"
  SC_RC=$?
}
run_expect_pass lan
run_expect_pass public
run_expect_pass public-refuse
run_expect_fail lan-h1
run_expect_fail public-h1

if [[ $FAILS -eq 0 ]]; then printf 'all passed\n'; exit 0; fi
printf '%d FAILED\n' "$FAILS" >&2
exit 1
