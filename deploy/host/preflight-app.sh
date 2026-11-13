#!/usr/bin/env bash
# AutoDeploy host kit, step 18a-2: read-only verification of the application stack
# (PostgreSQL, Traefik, private CA, control plane, agent). Changes nothing, prints no
# secret. Prints PASS/FAIL/SKIP per check and exits non-zero if any check FAILs.
#
# Run it after the services are started. Checks that need root are marked [root] and
# are SKIPped when run unprivileged; run with sudo for full coverage. The checkout
# helpers have their own script: preflight-checkout.sh.
set -uo pipefail

readonly AGENT_USER=autodeploy-agent
readonly CHECKOUT_USER=autodeploy-checkout
readonly CP_USER=autodeploy-controlplane
readonly ETC_DIR=/etc/autodeploy
readonly HOST_ENV="$ETC_DIR/host.env"
readonly CP_ENV="$ETC_DIR/controlplane.env"
readonly ROUTE_DIR=/var/lib/autodeploy/traefik/dynamic
readonly AGENT_STATE=/var/lib/autodeploy/agent
readonly API_URL=http://127.0.0.1:8080

pass=0 fail=0 skip=0
ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf 'FAIL  %s\n' "$1"; fail=$((fail + 1)); }
skp() { printf 'SKIP  %s\n' "$1"; skip=$((skip + 1)); }
wrn() { printf 'WARN  %s\n' "$1"; } # advisory: not counted as a failure
check() { # description command...
  local d="$1"; shift
  if "$@" >/dev/null 2>&1; then ok "$d"; else bad "$d"; fi
}
is_root() { [[ $EUID -eq 0 ]]; }
# Parsed, never sourced: key=value files written by install-app.sh.
cfg() { sed -n "s/^$2=//p" "$1" 2>/dev/null | head -n1; }

# Always the system Engine, not a per-user Docker Desktop context.
export DOCKER_HOST=unix:///var/run/docker.sock DOCKER_CONFIG=/nonexistent
unset DOCKER_CONTEXT

if [[ ! -r $HOST_ENV || ! -r $CP_ENV ]]; then
  bad "$HOST_ENV and $CP_ENV readable (run install-app.sh first)"
  printf '\n%d passed, %d failed, %d skipped\n' "$pass" "$fail" "$skip"
  exit 1
fi
NETWORK="$(cfg "$HOST_ENV" AUTODEPLOY_ENV_NETWORK)"
SUBNET="$(cfg "$HOST_ENV" AUTODEPLOY_ENV_SUBNET)"
GATEWAY="$(cfg "$HOST_ENV" AUTODEPLOY_ENV_GATEWAY)"
IP_RANGE="$(cfg "$HOST_ENV" AUTODEPLOY_ENV_IP_RANGE)"
TRAEFIK_IP="$(cfg "$HOST_ENV" AUTODEPLOY_ENV_TRAEFIK_IP)"
readonly BRIDGE_NAME=br-autodeploy
BIND="$(cfg "$HOST_ENV" AUTODEPLOY_TRAEFIK_BIND)"
CP_HOST="$(cfg "$HOST_ENV" AUTODEPLOY_CONTROLPLANE_HOST)"
CP_PORT="$(cfg "$HOST_ENV" AUTODEPLOY_CONTROLPLANE_PORT)"
AGID="$(cfg "$HOST_ENV" AUTODEPLOY_AGENT_GID)"

# ---- units ------------------------------------------------------------------------
for u in autodeploy-postgres autodeploy-traefik autodeploy-controlplane; do
  check "$u.service is active" systemctl is-active --quiet "$u.service"
done
if [[ -e $AGENT_STATE/credential ]]; then
  check "autodeploy-agent.service is active (agent is enrolled)" systemctl is-active --quiet autodeploy-agent.service
else
  skp "autodeploy-agent.service is active (agent not enrolled yet: $AGENT_STATE/credential absent)"
fi
for u in autodeploy-checkout-quota autodeploy-checkout-exec; do
  check "$u.service is active (agent dependency)" systemctl is-active --quiet "$u.service"
done

# ---- listeners ----------------------------------------------------------------------
# listeners PORT -> "addr" lines of TCP listeners on that port
listeners() { ss -ltnH "sport = :$1" 2>/dev/null | awk '{print $4}' | sed 's/:[0-9]*$//' | sort -u; }
only_on() { # port addr...: every listener on port is exactly one of the addrs, and there is at least one
  local port="$1" a l found=0 good
  shift
  while read -r l; do
    [[ -n "$l" ]] || continue
    found=1
    good=0
    for a in "$@"; do [[ "$l" == "$a" ]] && good=1; done
    ((good)) || return 1
  done < <(listeners "$port")
  ((found))
}
check "5432 listens only on 127.0.0.1 (PostgreSQL)" only_on 5432 127.0.0.1
check "8080 listens only on 127.0.0.1 (Traefik management API)" only_on 8080 127.0.0.1
check "80 listens only on $BIND (Traefik; nothing else, e.g. apache2, on :80)" only_on 80 "$BIND"
check "443 listens only on $BIND (Traefik)" only_on 443 "$BIND"
check "control plane listens only on $GATEWAY:$CP_PORT (Docker bridge gateway)" only_on "$CP_PORT" "$GATEWAY"

# Who owns the listeners. Docker-published ports are held by docker-proxy (or the
# daemon); PostgreSQL/Traefik themselves are inside containers, so the expected owner
# of 5432/80/443/8080 is docker-proxy/dockerd and of the control plane port the binary.
port_owner_in() { # port pattern  ([root]: needs -p)
  ss -ltnpH "sport = :$1" 2>/dev/null | grep -Eq "users:\(\(\"($2)\""
}
if is_root; then
  for p in 5432 8080 80 443; do
    check "port $p is held by docker-proxy/dockerd (a published container port) [root]" port_owner_in "$p" 'docker-proxy|dockerd'
  done
  check "port $CP_PORT is held by the controlplane binary [root]" port_owner_in "$CP_PORT" controlplane
else
  skp "listener owners (docker-proxy for 5432/8080/80/443, controlplane for $CP_PORT) [root]"
fi

docker_ok() { docker info >/dev/null 2>&1; }
if docker_ok; then
  published() { # container container-port expected-host-addr:port
    [[ "$(docker port "$1" "$2" 2>/dev/null | head -n1)" == "$3" ]]
  }
  check "container autodeploy-postgres publishes 5432 on 127.0.0.1:5432 only" published autodeploy-postgres 5432/tcp 127.0.0.1:5432
  check "container autodeploy-traefik publishes 8080 on 127.0.0.1:8080 only" published autodeploy-traefik 8080/tcp 127.0.0.1:8080
  check "container autodeploy-traefik publishes 443 on $BIND:443" published autodeploy-traefik 443/tcp "$BIND:443"
  check "container autodeploy-traefik publishes 80 on $BIND:80" published autodeploy-traefik 80/tcp "$BIND:80"
  net_ok() {
    [[ "$(docker network inspect -f '{{range .IPAM.Config}}{{.Subnet}} {{.IPRange}} {{.Gateway}}{{end}}' "$NETWORK" 2>/dev/null)" == "$SUBNET $IP_RANGE $GATEWAY" ]] \
      && [[ "$(docker network inspect -f '{{index .Options "com.docker.network.bridge.name"}}' "$NETWORK" 2>/dev/null)" == "$BRIDGE_NAME" ]]
  }
  check "Docker network $NETWORK has subnet $SUBNET, ip-range $IP_RANGE, gateway $GATEWAY and bridge $BRIDGE_NAME" net_ok
  # Privilege boundary: trust and the firewall rule are keyed on Traefik's source address, so
  # only Traefik may hold it and every other container must sit inside the app range.
  ip2int() { local a b c d; IFS=. read -r a b c d <<<"$1"; echo $(((a << 24) | (b << 16) | (c << 8) | d)); }
  in_cidr() { local m=$((32 - ${2#*/})) n; n="$(ip2int "$1")"; (((n >> m) == ($(ip2int "${2%/*}") >> m))); } # ip cidr
  traefik_ip_ok() {
    [[ "$(docker inspect -f "{{(index .NetworkSettings.Networks \"$NETWORK\").IPAddress}}" autodeploy-traefik 2>/dev/null)" == "$TRAEFIK_IP" ]]
  }
  check "Traefik holds its fixed address $TRAEFIK_IP on $NETWORK" traefik_ip_ok
  others_in_range() {
    local name ip
    while read -r name ip; do
      [[ -n "$name" && "$name" != autodeploy-traefik ]] || continue
      in_cidr "${ip%/*}" "$IP_RANGE" || return 1
    done < <(docker network inspect -f '{{range .Containers}}{{.Name}} {{.IPv4Address}}{{"\n"}}{{end}}' "$NETWORK")
  }
  check "every other container on $NETWORK has an address inside $IP_RANGE (never the trusted Traefik address)" others_in_range
  pg_off_network() { ! docker inspect -f '{{json .NetworkSettings.Networks}}' autodeploy-postgres 2>/dev/null | grep -q "\"$NETWORK\""; }
  check "PostgreSQL container is NOT on the environment network $NETWORK" pg_off_network
else
  skp "docker port/network checks (this user cannot reach /var/run/docker.sock; run with sudo)"
fi

# ---- firewall -------------------------------------------------------------------------
# The control plane listens on the bridge gateway; the ufw rule restricts that port to Traefik's
# address. With ufw inactive, ANY container on the bridge (untrusted apps included) reaches it.
if is_root; then
  if command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q '^Status: active'; then
    # Row shape (ufw status, to-gateway rule): "<gw> <port>/tcp on <bridge>  ALLOW  <src>  # comment".
    rule_ok() {
      local out
      out="$(ufw status)" || return 1
      grep -Eq "$GATEWAY +$CP_PORT/tcp.*ALLOW +$TRAEFIK_IP +# autodeploy: traefik" <<<"$out" \
        && ! grep -q '# autodeploy: traefik to control plane' <<<"$out" # the older, bridge-wide rule must be gone
    }
    check "ufw is active and allows only $TRAEFIK_IP to $GATEWAY:$CP_PORT (comment 'autodeploy: traefik') [root]" rule_ok
  else
    wrn "ufw is INACTIVE or missing: $GATEWAY:$CP_PORT is reachable from every container on $NETWORK. Enable ufw and re-run install-app.sh, or add an equivalent firewall rule."
  fi
else
  skp "ufw rule restricting $GATEWAY:$CP_PORT to $TRAEFIK_IP [root]"
fi

# ---- docker.sock access --------------------------------------------------------------
sock_reachable_by() { # user
  runuser -u "$1" -- env HOME=/nonexistent DOCKER_CONFIG=/nonexistent DOCKER_HOST=unix:///var/run/docker.sock \
    /usr/bin/docker version --format '{{.Server.Version}}'
}
if is_root; then
  check "/var/run/docker.sock is reachable by $AGENT_USER [root]" sock_reachable_by "$AGENT_USER"
  check "/var/run/docker.sock is NOT reachable by $CHECKOUT_USER [root]" bash -c "! runuser -u $CHECKOUT_USER -- env HOME=/nonexistent DOCKER_CONFIG=/nonexistent /usr/bin/docker -H unix:///var/run/docker.sock version"
  check "/var/run/docker.sock is NOT reachable by $CP_USER [root]" bash -c "! runuser -u $CP_USER -- env HOME=/nonexistent DOCKER_CONFIG=/nonexistent /usr/bin/docker -H unix:///var/run/docker.sock version"
else
  skp "docker.sock reachable by $AGENT_USER, not by $CHECKOUT_USER/$CP_USER [root]"
fi
docker_group_exact() { # only the agent (plus, harmlessly, admins) may hold docker; never checkout/controlplane
  local u
  for u in "$CHECKOUT_USER" "$CP_USER"; do
    if id -nG "$u" | tr ' ' '\n' | grep -qx docker; then return 1; fi
  done
  id -nG "$AGENT_USER" | tr ' ' '\n' | grep -qx docker
}
check "$AGENT_USER is in group docker; $CHECKOUT_USER and $CP_USER are not" docker_group_exact

# ---- Traefik management API --------------------------------------------------------
# Credentials come from the agent's file and are fed to curl on stdin (-K -), so they
# never reach argv; nothing is printed.
CRED_FILE="$(cfg "$ETC_DIR/agent.env" AUTODEPLOY_AGENT_TRAEFIK_CREDENTIALS_FILE)"
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
  check "Traefik dashboard is off (/dashboard/ is 404 with credentials) [root]" status_is 404 api_status /dashboard/ auth
else
  skp "Traefik API 200 with credentials; dashboard off [root: the credentials file is agent-owned 0600]"
fi

# ---- private CA and TLS --------------------------------------------------------------
CA_CRT=$ETC_DIR/ca/ca.crt
SRV_CRT=$ETC_DIR/tls/server.crt
tls_verifies() { # connect to Traefik on 127.0.0.1:443 with SNI; verify against system roots and the hostname
  local out
  out="$(openssl s_client -connect "127.0.0.1:443" -servername "$CP_HOST" -verify_return_error -verify_hostname "$CP_HOST" </dev/null 2>&1)" || return 1
  grep -q 'Verification: OK' <<<"$out"
}
check "Traefik serves a certificate for $CP_HOST that the SYSTEM trust store verifies (SNI, hostname, openssl -verify_return_error)" tls_verifies
check "server certificate verifies against the system trust store (openssl verify -CApath /etc/ssl/certs)" openssl verify -CApath /etc/ssl/certs "$SRV_CRT"
check "CA certificate is installed in /usr/local/share/ca-certificates" cmp -s "$CA_CRT" /usr/local/share/ca-certificates/autodeploy-private-ca.crt
cert_not_expiring() { openssl x509 -in "$SRV_CRT" -noout -checkend $((14 * 86400)); }
check "server certificate is valid for at least 14 more days" cert_not_expiring
hosts_ok() { grep -Eq "^127\.0\.0\.1[[:space:]]+$CP_HOST([[:space:]]|\$)" /etc/hosts; }
check "/etc/hosts maps $CP_HOST to 127.0.0.1 (agent -> control plane URL, browsers)" hosts_ok
cp_via_traefik() { # control plane answers through Traefik over verified TLS
  local code
  code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 --resolve "$CP_HOST:443:127.0.0.1" "https://$CP_HOST/")" || return 1
  [[ "$code" =~ ^[23][0-9][0-9]$ ]]
}
check "control plane answers 2xx/3xx at https://$CP_HOST/ through Traefik (system CA, no -k)" cp_via_traefik
if is_root; then
  ca_key_ok() { [[ "$(stat -c '%u:%g:%a' "$ETC_DIR/ca/ca.key")" == 0:0:600 && "$(stat -c '%a' "$ETC_DIR/ca")" == 700 ]]; }
  check "CA key is root:root 0600 in a 0700 directory, outside any container mount [root]" ca_key_ok
else
  skp "CA key permissions [root]"
fi

# ---- credential files and directories -----------------------------------------------
stat_is() { [[ "$(stat -c '%U:%G:%a' "$1" 2>/dev/null)" == "$2" ]]; }
if is_root; then
  check "postgres-password is root:root 0600 [root]" stat_is "$ETC_DIR/secrets/postgres-password" root:root:600
  for f in database-url username-throttle-key-1 csrf-key-1; do
    check "controlplane/$f is $CP_USER:$CP_USER 0600 [root]" stat_is "$ETC_DIR/controlplane/$f" "$CP_USER:$CP_USER:600"
  done
  key_size() { [[ "$(stat -c %s "$ETC_DIR/controlplane/$1")" == 32 ]]; }
  check "username-throttle-key-1 is exactly 32 bytes [root]" key_size username-throttle-key-1
  check "csrf-key-1 is exactly 32 bytes [root]" key_size csrf-key-1
  check "traefik-secrets/api-users is root:root 0600 [root]" stat_is "$ETC_DIR/traefik-secrets/api-users" root:root:600
  check "agent/traefik-api-credentials is $AGENT_USER:$AGENT_USER 0600 (exactly; protectedfile.go) [root]" stat_is "$CRED_FILE" "$AGENT_USER:$AGENT_USER:600"
  check "agent state dir is $AGENT_USER:$AGENT_USER 0700 [root]" stat_is "$AGENT_STATE" "$AGENT_USER:$AGENT_USER:700"
  if [[ -e $AGENT_STATE/credential ]]; then
    check "agent enrollment credential is $AGENT_USER:$AGENT_USER 0600 [root]" stat_is "$AGENT_STATE/credential" "$AGENT_USER:$AGENT_USER:600"
  fi
else
  skp "credential file owners/modes [root]"
fi
check "route directory is root:$AGENT_USER 1770 (sticky; internal/runtime/traefik checkDirectoryPolicy)" stat_is "$ROUTE_DIR" "root:$AGENT_USER:1770"
route_files_ok() {
  local f
  for f in autodeploy-api.yml autodeploy-tls.yml autodeploy-controlplane.yml; do
    [[ "$(stat -c '%U:%a' "$ROUTE_DIR/$f" 2>/dev/null)" == root:644 ]] || return 1
  done
}
check "installer route files (api, tls, controlplane) are root-owned 0644 in the route directory" route_files_ok
bins_ok() {
  local b
  for b in controlplane agent migrate admin secrets; do
    [[ "$(stat -c '%u:%a' "/usr/local/lib/autodeploy/app/$b" 2>/dev/null)" == 0:755 ]] || return 1
  done
}
check "app binaries are root-owned 0755 in /usr/local/lib/autodeploy/app" bins_ok
ids_ok() {
  [[ "$(id -u "$CP_USER")" != 0 && "$(id -u "$CP_USER")" != "$(id -u "$AGENT_USER")" && "$(id -u "$CP_USER")" != "$(id -u "$CHECKOUT_USER")" ]] \
    && [[ "$(id -g "$AGENT_USER")" == "$AGID" ]] && [[ "$(id -G "$CP_USER")" == "$(id -g "$CP_USER")" ]]
}
check "control plane identity is distinct, non-root, with no supplementary groups; agent gid matches host.env" ids_ok

# ---- units match the kit ------------------------------------------------------------
for u in autodeploy-postgres autodeploy-traefik autodeploy-controlplane autodeploy-agent; do
  check "$u.service is enabled" systemctl is-enabled --quiet "$u.service"
done

printf '\n%d passed, %d failed, %d skipped\n' "$pass" "$fail" "$skip"
if ((skip > 0)); then printf 'note: %d check(s) skipped; re-run with sudo for full coverage\n' "$skip"; fi
((fail == 0))
