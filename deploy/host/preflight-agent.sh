#!/usr/bin/env bash
# AutoDeploy host kit (plan 24-F): read-only verification of an AGENT-ONLY server installed with
# `install-app.sh --role agent` (agent, Traefik with ACME, checkout helpers). Changes nothing, prints no
# secret. Prints PASS/FAIL/SKIP per check and exits non-zero if any check FAILs.
#
# preflight-app.sh verifies a full host (control plane, PostgreSQL, control plane certificate/DNS) and is NOT
# meant for an agent host: use this script there, plus preflight-checkout.sh for the checkout helpers.
# Checks that need root are marked [root] and are SKIPped when run unprivileged.
set -uo pipefail

readonly AGENT_USER=autodeploy-agent
readonly CHECKOUT_USER=autodeploy-checkout
readonly ETC_DIR=/etc/autodeploy
readonly HOST_ENV="$ETC_DIR/host.env"
readonly AGENT_ENV="$ETC_DIR/agent.env"
readonly ROUTE_DIR=/var/lib/autodeploy/traefik/dynamic
readonly AGENT_STATE=/var/lib/autodeploy/agent
readonly ACME_DIR=/var/lib/autodeploy/traefik/acme
readonly API_URL=http://127.0.0.1:8080

pass=0 fail=0 skip=0
ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf 'FAIL  %s\n' "$1"; fail=$((fail + 1)); }
skp() { printf 'SKIP  %s\n' "$1"; skip=$((skip + 1)); }
check() { # description command...
  local d="$1"; shift
  if "$@" >/dev/null 2>&1; then ok "$d"; else bad "$d"; fi
}
is_root() { [[ $EUID -eq 0 ]]; }
# Parsed, never sourced: key=value files written by install-app.sh.
cfg() { sed -n "s/^$2=//p" "$1" 2>/dev/null | head -n1; }
finish() { printf '\n%d passed, %d failed, %d skipped\n' "$pass" "$fail" "$skip"; }

export DOCKER_HOST=unix:///var/run/docker.sock DOCKER_CONFIG=/nonexistent
unset DOCKER_CONTEXT

if [[ ! -r $HOST_ENV || ! -r $AGENT_ENV ]]; then
  bad "$HOST_ENV and $AGENT_ENV readable (run install-app.sh --role agent first)"
  finish; exit 1
fi
if [[ "$(cfg "$HOST_ENV" AUTODEPLOY_ROLE)" != agent ]]; then
  bad "host.env AUTODEPLOY_ROLE is agent (this is not an agent-only host: use preflight-app.sh)"
  finish; exit 1
fi
NETWORK="$(cfg "$HOST_ENV" AUTODEPLOY_ENV_NETWORK)"
SUBNET="$(cfg "$HOST_ENV" AUTODEPLOY_ENV_SUBNET)"
GATEWAY="$(cfg "$HOST_ENV" AUTODEPLOY_ENV_GATEWAY)"
IP_RANGE="$(cfg "$HOST_ENV" AUTODEPLOY_ENV_IP_RANGE)"
TRAEFIK_IP="$(cfg "$HOST_ENV" AUTODEPLOY_ENV_TRAEFIK_IP)"
BIND="$(cfg "$HOST_ENV" AUTODEPLOY_TRAEFIK_BIND)"
CP_HOST="$(cfg "$HOST_ENV" AUTODEPLOY_CONTROLPLANE_HOST)"
TLS_MODE="$(cfg "$HOST_ENV" AUTODEPLOY_TLS_MODE)"
BASE_URL="$(cfg "$AGENT_ENV" AUTODEPLOY_AGENT_BASE_URL)"
readonly BRIDGE_NAME=br-autodeploy
printf 'Role: agent (public mode), control plane: %s\n' "$CP_HOST"
check "host.env is public mode (agent hosts support public mode only)" test "$TLS_MODE" = public
check "agent.env points at https://$CP_HOST" test "$BASE_URL" = "https://$CP_HOST"

# ---- no control plane on this host ---------------------------------------------------
no_cp_units() { ! [[ -e /etc/systemd/system/autodeploy-controlplane.service || -e /etc/systemd/system/autodeploy-postgres.service || -e $ETC_DIR/controlplane.env ]]; }
check "no control plane or PostgreSQL unit or controlplane.env on this host" no_cp_units
no_cp_hosts() { ! grep -Eq "^(127\.|::1)[^#]*[[:space:]]$CP_HOST([[:space:]]|\$)" /etc/hosts; }
check "/etc/hosts does not map $CP_HOST to loopback (the agent must reach the control plane over the internet)" no_cp_hosts
no_cp_route() { ! compgen -G "$ROUTE_DIR/autodeploy-controlplane.yml" >/dev/null; }
check "no control plane route in the Traefik route directory" no_cp_route

# ---- units ------------------------------------------------------------------------
check "autodeploy-traefik.service is active" systemctl is-active --quiet autodeploy-traefik.service
if [[ -e $AGENT_STATE/credential ]]; then
  check "autodeploy-agent.service is active (agent is enrolled)" systemctl is-active --quiet autodeploy-agent.service
else
  skp "autodeploy-agent.service is active (agent not enrolled yet: $AGENT_STATE/credential absent)"
fi
for u in autodeploy-checkout-quota autodeploy-checkout-exec; do
  check "$u.service is active (agent dependency)" systemctl is-active --quiet "$u.service"
done
agent_unit_cp_free() { ! grep -v '^[[:space:]]*#' /etc/systemd/system/autodeploy-agent.service | grep -q controlplane; }
check "agent unit has no control plane dependency" agent_unit_cp_free
for u in autodeploy-traefik autodeploy-agent; do check "$u.service is enabled" systemctl is-enabled --quiet "$u.service"; done

# ---- listeners and containers ---------------------------------------------------------
listeners() { ss -ltnH "sport = :$1" 2>/dev/null | awk '{print $4}' | sed 's/:[0-9]*$//' | sort -u; }
only_on() { # port addr...: every listener on port is one of the addrs, and there is at least one
  local port="$1" a l found=0 good
  shift
  while read -r l; do
    [[ -n "$l" ]] || continue
    found=1; good=0
    for a in "$@"; do [[ "$l" == "$a" ]] && good=1; done
    ((good)) || return 1
  done < <(listeners "$port")
  ((found))
}
check "8080 listens only on 127.0.0.1 (Traefik management API)" only_on 8080 127.0.0.1
for p in 80 443; do
  if ! is_root && listeners "$p" | grep -qxE '\*|\[::\]'; then
    printf 'WARN  %s listens on all interfaces and the owner is unknown: run as root to verify it is Traefik\n' "$p"
  else
    check "$p listens only on $BIND/[::] (Traefik)" only_on "$p" "$BIND" '[::]' '*'
  fi
done
check "5432 is NOT listening (no PostgreSQL on an agent host)" bash -c "! ss -ltnH 'sport = :5432' 2>/dev/null | grep -q ."
docker_ok() { docker info >/dev/null 2>&1; }
if docker_ok; then
  published() { [[ "$(docker port "$1" "$2" 2>/dev/null | head -n1)" == "$3" ]]; }
  check "container autodeploy-traefik publishes 8080 on 127.0.0.1:8080 only" published autodeploy-traefik 8080/tcp 127.0.0.1:8080
  net_ok() {
    [[ "$(docker network inspect -f '{{range .IPAM.Config}}{{.Subnet}} {{.IPRange}} {{.Gateway}}{{end}}' "$NETWORK" 2>/dev/null)" == "$SUBNET $IP_RANGE $GATEWAY" ]] \
      && [[ "$(docker network inspect -f '{{index .Options "com.docker.network.bridge.name"}}' "$NETWORK" 2>/dev/null)" == "$BRIDGE_NAME" ]]
  }
  check "Docker network $NETWORK has subnet $SUBNET, ip-range $IP_RANGE, gateway $GATEWAY and bridge $BRIDGE_NAME" net_ok
  traefik_ip_ok() { [[ "$(docker inspect -f "{{(index .NetworkSettings.Networks \"$NETWORK\").IPAddress}}" autodeploy-traefik 2>/dev/null)" == "$TRAEFIK_IP" ]]; }
  check "Traefik holds its fixed address $TRAEFIK_IP on $NETWORK" traefik_ip_ok
  check "docker buildx is installed (image builds)" docker buildx version
else
  skp "docker port/network/buildx checks (this user cannot reach /var/run/docker.sock; run with sudo)"
fi

# ---- docker.sock access and groups -----------------------------------------------------
sock_reachable_by() {
  runuser -u "$1" -- env HOME=/nonexistent DOCKER_CONFIG=/nonexistent DOCKER_HOST=unix:///var/run/docker.sock \
    /usr/bin/docker version --format '{{.Server.Version}}'
}
if is_root; then
  check "/var/run/docker.sock is reachable by $AGENT_USER [root]" sock_reachable_by "$AGENT_USER"
  check "/var/run/docker.sock is NOT reachable by $CHECKOUT_USER [root]" bash -c "! runuser -u $CHECKOUT_USER -- env HOME=/nonexistent DOCKER_CONFIG=/nonexistent /usr/bin/docker -H unix:///var/run/docker.sock version"
else
  skp "docker.sock reachable by $AGENT_USER, not by $CHECKOUT_USER [root]"
fi
in_group() { id -nG "$1" 2>/dev/null | tr ' ' '\n' | grep -qx "$2"; }
check "$AGENT_USER is in group docker; $CHECKOUT_USER is not" bash -c "$(declare -f in_group); in_group $AGENT_USER docker && ! in_group $CHECKOUT_USER docker"
check "$AGENT_USER is in group $CHECKOUT_USER (read-only build context)" bash -c "$(declare -f in_group); in_group $AGENT_USER $CHECKOUT_USER"

# ---- Traefik management API (credentials on stdin of curl, never argv) -----------------
CRED_FILE="$(cfg "$AGENT_ENV" AUTODEPLOY_AGENT_TRAEFIK_CREDENTIALS_FILE)"
api_status() { # path [auth]
  if [[ "${2:-}" == auth ]]; then
    local c
    c="$(<"$CRED_FILE")" || return 1
    printf 'user = "%s"\n' "$c" | curl -s -o /dev/null -w '%{http_code}' --max-time 5 -K - "$API_URL$1"
  else
    curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$API_URL$1"
  fi
}
status_is() { [[ "$("${@:2}")" == "$1" ]]; }
check "Traefik API answers 401 on /api/version without credentials" status_is 401 api_status /api/version
if is_root; then
  check "Traefik API answers 200 on /api/version with the agent's credentials [root]" status_is 200 api_status /api/version auth
else
  skp "Traefik API 200 with credentials [root: the credentials file is agent-owned 0600]"
fi

# ---- the control plane is reachable from here, over verified TLS -------------------------
cp_reachable() { curl --proto '=https' --tlsv1.2 -fsS --max-time 10 -o /dev/null "https://$CP_HOST/readyz"; }
check "control plane answers https://$CP_HOST/readyz (system roots, no -k)" cp_reachable

# ---- ACME for the apps on this server ----------------------------------------------------
stat_is() { [[ "$(stat -c '%U:%G:%a' "$1" 2>/dev/null)" == "$2" ]]; }
check "traefik.yml has the letsencrypt-production resolver" grep -q letsencrypt-production "$ETC_DIR/traefik/traefik.yml"
if is_root; then
  check "$ACME_DIR is a root:root 0700 directory [root]" stat_is "$ACME_DIR" root:root:700
  check "$ACME_DIR/acme.json is a regular file, root:root 0600 [root]" bash -c "[[ -f $ACME_DIR/acme.json && ! -L $ACME_DIR/acme.json && \$(stat -c '%U:%G:%a' $ACME_DIR/acme.json) == root:root:600 ]]"
  check "traefik-secrets/api-users is root:root 0600 [root]" stat_is "$ETC_DIR/traefik-secrets/api-users" root:root:600
  check "agent/traefik-api-credentials is $AGENT_USER:$AGENT_USER 0600 (exactly) [root]" stat_is "$CRED_FILE" "$AGENT_USER:$AGENT_USER:600"
  check "agent state dir is $AGENT_USER:$AGENT_USER 0700 [root]" stat_is "$AGENT_STATE" "$AGENT_USER:$AGENT_USER:700"
  if [[ -e $AGENT_STATE/credential ]]; then
    check "agent enrollment credential is $AGENT_USER:$AGENT_USER 0600 [root]" stat_is "$AGENT_STATE/credential" "$AGENT_USER:$AGENT_USER:600"
  fi
else
  skp "ACME and credential file owners/modes [root]"
fi
printf 'note: a certificate for an app hostname is requested when the first deployment routes it here; its DNS must point at this server.\n'

# ---- files ---------------------------------------------------------------------------
check "route directory is root:$AGENT_USER 1770 (sticky)" stat_is "$ROUTE_DIR" "root:$AGENT_USER:1770"
check "installer route file autodeploy-api.yml is root-owned 0644" bash -c "[[ \$(stat -c '%U:%a' $ROUTE_DIR/autodeploy-api.yml 2>/dev/null) == root:644 ]]"
check "agent binary is root-owned 0755 in /usr/local/lib/autodeploy/app" bash -c "[[ \$(stat -c '%u:%a' /usr/local/lib/autodeploy/app/agent 2>/dev/null) == 0:755 ]]"
check "/dev/loop-control exists (checkout XFS image needs loop devices)" test -e /dev/loop-control

finish
if ((skip > 0)); then printf 'note: %d check(s) skipped; re-run with sudo for full coverage\n' "$skip"; fi
((fail == 0))
