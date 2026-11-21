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
# | lan-upgrade | public-upgrade (second run on the same host after setup: upgrade banner, no fresh steps)
IFS= read -r -d '' DRIVER <<'DRIVEREOF' || true
set -u
SCENARIO="$1"
mode=lan; mutate=0; refuse=0; upgrade=0
case "$SCENARIO" in
  public) mode=public ;;
  lan-upgrade) upgrade=1 ;;
  public-upgrade) mode=public; upgrade=1 ;;
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
echo "systemctl $*" >>/tmp/stubstate/sysctl.log
if [[ "${1:-}" == is-active ]]; then [[ "${*: -1}" == docker.service || -e "/tmp/stubstate/active-${*: -1}" ]]; exit; fi
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
cp /etc/hosts /tmp/hosts-orig
export AUTODEPLOY_HOSTS_FILE=/tmp/hosts-test

# ---- 24-F: --role agent scenarios (public-mode agent-only host: no control plane artifacts) ----
ia() { "$K/install-app.sh" "$@" "$BIN" >/tmp/ia.out 2>&1; IRC=$?; }
AGENT_ARGS=(--mode public --role agent --control-plane-host cp.example.com --acme-email ops@example.com)
R=/var/lib/autodeploy/traefik/dynamic
agent_unit_cp_free() { ! grep -q 'controlplane' /etc/systemd/system/autodeploy-agent.service; }
case "$SCENARIO" in
agent)
  ia "${AGENT_ARGS[@]}"
  sed 's/^/  | /' /tmp/ia.out | tail -n 6
  ck "install-app --role agent exit 0" '((IRC == 0))'
  ck "agent binary installed" '[[ -x /usr/local/lib/autodeploy/app/agent ]]'
  ck "no controlplane/migrate/admin/secrets binaries" '[[ ! -e /usr/local/lib/autodeploy/app/controlplane && ! -e /usr/local/lib/autodeploy/app/migrate && ! -e /usr/local/lib/autodeploy/app/admin && ! -e /usr/local/lib/autodeploy/app/secrets ]]'
  ck "only the traefik and agent units are installed (no controlplane, no postgres)" '[[ "$(ls /etc/systemd/system/autodeploy-*.service | xargs -n1 basename | tr "\n" " ")" == "autodeploy-agent.service autodeploy-traefik.service " ]]'
  ck "no controlplane.env" '[[ ! -e /etc/autodeploy/controlplane.env ]]'
  ck "no controlplane user, no controlplane/secrets directories" '! id -u autodeploy-controlplane >/dev/null 2>&1 && [[ ! -e /etc/autodeploy/controlplane && ! -e /etc/autodeploy/secrets ]]'
  ck "no /etc/hosts block and no CP host mapping (hosts file untouched)" 'cmp -s /tmp/hosts-test /tmp/hosts-orig && ! grep -q "autodeploy-public-cp\|cp.example.com" /tmp/hosts-test'
  ck "agent.env points the agent at https://cp.example.com" 'grep -qx "AUTODEPLOY_AGENT_BASE_URL=https://cp.example.com" /etc/autodeploy/agent.env'
  ck "agent unit has no controlplane dependency (After/Wants/Requires)" 'agent_unit_cp_free'
  ck "agent unit still orders after docker, traefik and the checkout helpers" 'grep -q "^After=.*docker.service.*autodeploy-traefik.service" /etc/systemd/system/autodeploy-agent.service && grep -q "^Requires=autodeploy-checkout-quota.service" /etc/systemd/system/autodeploy-agent.service && grep -q "^Wants=autodeploy-traefik.service" /etc/systemd/system/autodeploy-agent.service'
  ck "traefik.yml has the ACME resolver" 'grep -q letsencrypt-production /etc/autodeploy/traefik/traefik.yml'
  ck "acme.json exists 0600; unit mounts acme, no tls files" '[[ "$(stat -c %a /var/lib/autodeploy/traefik/acme/acme.json)" == 600 ]] && grep -q "^AssertPathExists=/var/lib/autodeploy/traefik/acme/acme.json" /etc/systemd/system/autodeploy-traefik.service && ! grep -q "autodeploy/tls" /etc/systemd/system/autodeploy-traefik.service'
  ck "route dir has the Traefik API route only: no control plane route, no tls file" '[[ -s $R/autodeploy-api.yml && ! -e $R/autodeploy-controlplane.yml && ! -e $R/autodeploy-tls.yml ]] && ! grep -rqi "controlplane\|8081\|cp.example.com" $R'
  ck "ACME resolver letsencrypt-production never referenced by a route (no CP route)" '! grep -rq "certResolver" $R'
  ck "host.env records role=agent, public mode, the control plane host" 'grep -qx AUTODEPLOY_ROLE=agent /etc/autodeploy/host.env && grep -qx AUTODEPLOY_TLS_MODE=public /etc/autodeploy/host.env && grep -qx AUTODEPLOY_CONTROLPLANE_HOST=cp.example.com /etc/autodeploy/host.env'
  ck "Traefik binds 0.0.0.0 (public mode)" 'grep -qx AUTODEPLOY_TRAEFIK_BIND=0.0.0.0 /etc/autodeploy/host.env'
  ck "Docker network created for the apps" '[[ -e /tmp/stubstate/net ]]'
  ck "traefik-secrets and agent dirs exist for create-credentials --traefik-only" '[[ -d /etc/autodeploy/traefik-secrets && -d /etc/autodeploy/agent ]]'
  ck "agent banner: enroll with install.sh / no database or bootstrap steps" 'grep -q "agent-only" /tmp/ia.out && ! grep -q "admin bootstrap\|autodeploy-run migrate\|autodeploy-postgres" /tmp/ia.out'
  # create-credentials.sh --traefik-only on this host: only the Traefik API pair, nothing database or control plane.
  cat >"$S/openssl" <<'X'
#!/bin/bash
[[ "$1 $2" == "rand -hex" ]] || exit 1
head -c "$3" /dev/urandom | od -An -v -tx1 | tr -d ' \n'
X
  cat >"$S/htpasswd" <<'X'
#!/bin/bash
read -r pw; printf '%s:$2y$10$%s\n' "${@: -1}" "$(printf '%s' "$pw" | od -An -v -tx1 | tr -d ' \n' | head -c 40)"
X
  chmod +x "$S/openssl" "$S/htpasswd"
  "$K/create-credentials.sh" --traefik-only >/tmp/cc.out 2>&1; CRC=$?
  sed 's/^/  | /' /tmp/cc.out | tail -n 4
  ck "create-credentials --traefik-only exit 0" '((CRC == 0))'
  ck "api-users root 0600 and agent credential agent 0600" '[[ "$(stat -c %U:%G:%a /etc/autodeploy/traefik-secrets/api-users)" == root:root:600 && "$(stat -c %U:%G:%a /etc/autodeploy/agent/traefik-api-credentials)" == autodeploy-agent:autodeploy-agent:600 ]]'
  ck "no database, throttle or csrf credential files created" '[[ ! -e /etc/autodeploy/secrets && ! -e /etc/autodeploy/controlplane ]]'
  ck "credential values never printed" '! grep -q "$(cut -d: -f2 /etc/autodeploy/agent/traefik-api-credentials)" /tmp/cc.out'
  before="$(cat /etc/autodeploy/agent/traefik-api-credentials)"
  "$K/create-credentials.sh" --traefik-only >/tmp/cc2.out 2>&1
  ck "rerun keeps the existing credentials" '[[ "$(cat /etc/autodeploy/agent/traefik-api-credentials)" == "$before" ]] && grep -q "exist; keeping" /tmp/cc2.out'
  "$K/create-credentials.sh" >/tmp/cc3.out 2>&1; CRC3=$?
  ck "plain create-credentials.sh on an agent host refuses and points at --traefik-only" '((CRC3 != 0)) && grep -q -- "--traefik-only" /tmp/cc3.out'
  exit "$RC" ;;
agent-upgrade)
  ia "${AGENT_ARGS[@]}"
  ck "first run ok" '((IRC == 0))'
  : >/tmp/stubstate/active-autodeploy-traefik.service; : >/tmp/stubstate/active-autodeploy-agent.service; : >/tmp/stubstate/sysctl.log
  { printf '\177ELF\002\001\001\000\000\000\000\000\000\000\000\000\002\000\076\000'; echo "fake agent v2"; } >"$BIN/agent"
  (cd "$BIN" && sha256sum controlplane agent migrate admin secrets >SHA256SUMS)
  ia "${AGENT_ARGS[@]}"
  sed 's/^/  | /' /tmp/ia.out | tail -n 6
  ck "upgrade over a running agent host exit 0 (no postgres/migration gate on an agent host)" '((IRC == 0))'
  ck "upgrade banner names the agent role" 'grep -q "^==> upgraded (public mode, agent role)" /tmp/ia.out'
  ck "running agent restarted for its new binary" 'grep -q "systemctl restart autodeploy-agent.service" /tmp/stubstate/sysctl.log'
  ck "no controlplane unit enabled, started or restarted" '! grep -E "^systemctl (enable|start|restart|disable) .*controlplane" /tmp/stubstate/sysctl.log'
  ck "still no controlplane artifacts" '[[ ! -e /etc/autodeploy/controlplane.env && ! -e /etc/systemd/system/autodeploy-controlplane.service ]]'
  exit "$RC" ;;
agent-preflight)
  ia "${AGENT_ARGS[@]}"
  ck "agent install ok" '((IRC == 0))'
  "$K/preflight-agent.sh" >/tmp/pf.out 2>&1; sed 's/^/  | /' /tmp/pf.out | tail -n 4
  ck "preflight-agent recognises the host (role agent, base URL, no control plane artifacts)" 'grep -q "^PASS  agent.env points at https://cp.example.com" /tmp/pf.out && grep -q "^PASS  no control plane or PostgreSQL unit" /tmp/pf.out && grep -q "^PASS  agent unit has no control plane dependency" /tmp/pf.out && grep -q "^PASS  host.env is public mode" /tmp/pf.out'
  ck "no loopback mapping of the control plane host: PASS" 'grep -q "^PASS  /etc/hosts does not map cp.example.com to loopback" /tmp/pf.out'
  echo "127.0.0.1 cp.example.com" >>/etc/hosts
  "$K/preflight-agent.sh" >/tmp/pf2.out 2>&1; PRC=$?
  ck "a loopback mapping of the control plane host FAILS the preflight" '((PRC != 0)) && grep -q "^FAIL  /etc/hosts does not map cp.example.com to loopback" /tmp/pf2.out'
  cp /tmp/hosts-orig /etc/hosts
  printf '# note: controlplane is not a dependency here\n' >>/etc/systemd/system/autodeploy-agent.service
  "$K/preflight-agent.sh" >/tmp/pf3.out 2>&1
  ck "a comment mentioning controlplane in the agent unit does not fail the dependency check" 'grep -q "^PASS  agent unit has no control plane dependency" /tmp/pf3.out'
  ck "route/ACME checks ran (route dir, acme resolver)" 'grep -q "^PASS  route directory is root:autodeploy-agent 1770" /tmp/pf.out && grep -q "^PASS  traefik.yml has the letsencrypt-production resolver" /tmp/pf.out'
  "$K/preflight-app.sh" >/tmp/pa.out 2>&1; PARC=$?
  ck "preflight-app.sh on an agent host says to use preflight-agent.sh" '((PARC != 0)) && grep -q "preflight-agent.sh" /tmp/pa.out'
  exit "$RC" ;;
agent-uninstall)
  ia "${AGENT_ARGS[@]}"
  ck "agent install ok" '((IRC == 0))'
  cat >>/etc/hosts <<'H'
203.0.113.9 unrelated.example.com
H
  "$K/uninstall.sh" --yes >/tmp/un.out 2>&1; URC=$?
  sed 's/^/  | /' /tmp/un.out | tail -n 6
  ck "uninstall.sh on an agent-role host exits 0" '((URC == 0))'
  ck "no uninstall error lines (no assumption that control plane pieces exist)" '! grep -q "^uninstall: error" /tmp/un.out'
  ck "agent and traefik units removed" '! ls /etc/systemd/system/autodeploy-*.service >/dev/null 2>&1'
  ck "agent user removed" '! id -u autodeploy-agent >/dev/null 2>&1'
  ck "kept message does not list controlplane or secrets that never existed on an agent host" '! grep "^==> kept" /tmp/un.out | grep -q "controlplane\|secrets"'
  ck "tells the owner to revoke the server on the Servers page" 'grep -qi "revoke" /tmp/un.out'
  ck "ACME state kept without --destroy-data" '[[ -f /var/lib/autodeploy/traefik/acme/acme.json ]]'
  ck "/usr/local/lib/autodeploy removed, hosts file keeps its unrelated line" '[[ ! -e /usr/local/lib/autodeploy ]] && grep -q unrelated.example.com /etc/hosts'
  exit "$RC" ;;
agent-lan)
  ia --mode lan --role agent --control-plane-host cp.example.com
  ck "--role agent with --mode lan refused" '((IRC != 0)) && grep -qi "public mode" /tmp/ia.out && grep -q -- "--mode public" /tmp/ia.out'
  ia --role agent --control-plane-host cp.example.com
  ck "--role agent without --mode (default lan) refused the same way" '((IRC != 0)) && grep -qi "public mode" /tmp/ia.out'
  ck "nothing generated" '[[ ! -e /etc/autodeploy/host.env && ! -e /etc/autodeploy/agent.env ]]'
  exit "$RC" ;;
agent-flags)
  ia --mode public --role agent --acme-email ops@example.com
  ck "--role agent needs --control-plane-host" '((IRC != 0)) && grep -q -- "--control-plane-host" /tmp/ia.out'
  ia --mode public --role agent --control-plane-host 10.0.0.1 --acme-email ops@example.com
  ck "IP literal host refused like valid_public_host" '((IRC != 0)) && grep -qi "invalid" /tmp/ia.out'
  ia --mode public --role agent --control-plane-host cp.test --acme-email ops@example.com
  ck "reserved suffix refused" '((IRC != 0)) && grep -qi "invalid" /tmp/ia.out'
  ia --mode public --role all --control-plane-host cp.example.com --acme-email ops@example.com
  ck "--control-plane-host with --role all refused" '((IRC != 0)) && grep -q -- "--role agent" /tmp/ia.out'
  ia --mode public --role bogus --control-plane-host cp.example.com
  ck "unknown role refused" '((IRC != 0)) && grep -qi "invalid --role" /tmp/ia.out'
  AUTODEPLOY_CONTROLPLANE_HOST=other.example.com ia "${AGENT_ARGS[@]}"
  ck "an env control plane host that differs from the flag is refused" '((IRC != 0)) && grep -qi "differs" /tmp/ia.out'
  ck "nothing generated by any refusal" '[[ ! -e /etc/autodeploy/host.env && ! -e /etc/autodeploy/agent.env ]]'
  exit "$RC" ;;
agent-switch)
  ia "${AGENT_ARGS[@]}"
  ck "agent install ok" '((IRC == 0))'
  AUTODEPLOY_CONTROLPLANE_HOST=cp.example.com ia --mode public --acme-email ops@example.com
  ck "agent host -> role all refused (no in-place role switch)" '((IRC != 0)) && grep -qi "role" /tmp/ia.out && grep -q "agent" /tmp/ia.out'
  ck "host.env still role=agent after the refusal" 'grep -qx AUTODEPLOY_ROLE=agent /etc/autodeploy/host.env'
  ia --mode public --role agent --control-plane-host other.example.com --acme-email ops@example.com
  ck "NOT enrolled yet: a corrected control plane host is accepted (mistyped host must be fixable)" '((IRC == 0))'
  ck "host.env and agent.env follow the correction" 'grep -qx AUTODEPLOY_CONTROLPLANE_HOST=other.example.com /etc/autodeploy/host.env && grep -qx "AUTODEPLOY_AGENT_BASE_URL=https://other.example.com" /etc/autodeploy/agent.env'
  mkdir -p /var/lib/autodeploy/agent; : >/var/lib/autodeploy/agent/credential
  ia --mode public --role agent --control-plane-host third.example.com --acme-email ops@example.com
  ck "ENROLLED: a different control plane host is refused" '((IRC != 0)) && grep -q "other.example.com" /tmp/ia.out'
  ck "agent.env unchanged by the refusal" 'grep -qx "AUTODEPLOY_AGENT_BASE_URL=https://other.example.com" /etc/autodeploy/agent.env'
  exit "$RC" ;;
all-switch)
  AUTODEPLOY_CONTROLPLANE_HOST=cp.example.com ia --mode public --acme-email ops@example.com
  ck "role all install ok" '((IRC == 0))'
  ck "host.env of role all has NO role line (byte-identical to earlier kits)" '! grep -q AUTODEPLOY_ROLE /etc/autodeploy/host.env'
  # Refactor guard: the plain create-credentials.sh of a full host still creates every credential file.
  cat >"$S/openssl" <<'X'
#!/bin/bash
[[ "$1 $2" == "rand -hex" ]] || exit 1
head -c "$3" /dev/urandom | od -An -v -tx1 | tr -d ' \n'
X
  cat >"$S/htpasswd" <<'X'
#!/bin/bash
read -r pw; printf '%s:$2y$10$%s\n' "${@: -1}" "$(printf '%s' "$pw" | od -An -v -tx1 | tr -d ' \n' | head -c 40)"
X
  chmod +x "$S/openssl" "$S/htpasswd"
  groupadd --gid 802 autodeploy-controlplane 2>/dev/null; id -u autodeploy-controlplane >/dev/null 2>&1 || useradd --system --uid 802 --gid 802 --no-create-home --shell /usr/sbin/nologin autodeploy-controlplane
  "$K/create-credentials.sh" >/tmp/cc.out 2>&1; CRC=$?
  ck "full create-credentials.sh exit 0 on a role-all host" '((CRC == 0))'
  ck "full host gets database, throttle, csrf and Traefik credentials" 'for f in secrets/postgres-password secrets/database-owner-url secrets/runtime-db-password controlplane/database-url controlplane/username-throttle-key-1 controlplane/csrf-key-1 traefik-secrets/api-users agent/traefik-api-credentials; do [[ -s /etc/autodeploy/$f ]] || exit 1; done'
  ck "csrf key is 32 bytes" '[[ "$(stat -c %s /etc/autodeploy/controlplane/csrf-key-1)" == 32 ]]'
  ia "${AGENT_ARGS[@]}"
  ck "role all host -> agent refused" '((IRC != 0)) && grep -qi "role" /tmp/ia.out'
  ck "controlplane artifacts untouched" '[[ -s /etc/autodeploy/controlplane.env && -e /etc/systemd/system/autodeploy-controlplane.service ]] && grep -q "cp.example.com" /tmp/hosts-test'
  exit "$RC" ;;
esac

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
ck "fresh run prints the fresh banner" 'grep -q "Nothing was started and no secret exists yet" "$OUT"'
ck "fresh run does not print the upgrade banner" '! grep -q "^==> upgraded" "$OUT"'
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
if ((upgrade)); then
  # What a real host has after setup: create-credentials.sh wrote the database credentials.
  mkdir -p /etc/autodeploy/secrets /etc/autodeploy/controlplane
  echo placeholder >/etc/autodeploy/secrets/postgres-password
  OUT2=/tmp/install2.out
  "$K/install-app.sh" "${args[@]}" "$BIN" >"$OUT2" 2>&1
  IRC2=$?
  sed 's/^/  | /' "$OUT2" | tail -n 8
  ck "upgrade run exit 0" '((IRC2 == 0))'
  ck "upgrade run prints the upgrade banner" 'grep -q "^==> upgraded ($mode mode)" "$OUT2"'
  ck "upgrade banner points at preflight-app.sh" 'grep -q "preflight-app.sh" "$OUT2"'
  ck "upgrade run does not print fresh next steps" '! grep -q "Nothing was started and no secret exists yet" "$OUT2" && ! grep -q "create-credentials.sh" "$OUT2" && ! grep -q "admin bootstrap" "$OUT2"'
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
run_expect_pass lan-upgrade
run_expect_pass public-upgrade
for sc in agent agent-upgrade agent-preflight agent-uninstall agent-lan agent-flags agent-switch all-switch; do run_expect_pass "$sc"; done
run_expect_fail lan-h1
run_expect_fail public-h1

if [[ $FAILS -eq 0 ]]; then printf 'all passed\n'; exit 0; fi
printf '%d FAILED\n' "$FAILS" >&2
exit 1
