#!/usr/bin/env bash
# AutoDeploy host kit, part 2 (step 18a-2): control plane, agent, PostgreSQL, Traefik.
#
# Run AFTER install-checkout.sh (it needs the autodeploy-agent identity, the helper
# units and /etc/autodeploy/checkout.env). Installs binaries, identities,
# directories, NON-secret configuration and the four systemd units. It never creates,
# reads or prints a secret: credentials come from create-credentials.sh, run by the
# owner afterwards. It enables but does not start anything.
#
# Usage (as root):
#   install-app.sh BINDIR
#
#   BINDIR  directory with prebuilt linux/amd64 binaries: controlplane, agent, migrate,
#           admin, secrets (GOOS=linux GOARCH=amd64 go build ./cmd/...), and optionally SHA256SUMS
#           (sha256sum format; when present every binary must be listed and match, when
#           absent a loud warning is printed). Inputs are copied to a root-only staging
#           directory first; validation and installation use the copies.
#
# Tunables (environment, all optional, non-secret). IDs only matter on first run.
#   CONTROLPLANE_ID=802                 uid and gid of autodeploy-controlplane
#   AUTODEPLOY_CONTROLPLANE_HOST=autodeploy.autodeploy.test   public hostname (must be inside
#                                       the private-ca.sh domain; Traefik routes it)
#   AUTODEPLOY_ENV_NETWORK=autodeploy-env        Docker network shared by Traefik and apps
#   AUTODEPLOY_ENV_SUBNET=172.29.0.0/24          its subnet
#   AUTODEPLOY_ENV_GATEWAY=172.29.0.1            its gateway: the control plane listens here
#   AUTODEPLOY_ENV_IP_RANGE=172.29.0.128/25      the ONLY range Docker hands to containers
#                                                (apps), so they never get the low addresses
#   AUTODEPLOY_ENV_TRAEFIK_IP=172.29.0.2         Traefik's fixed address (outside the range):
#                                                the control plane trusts forwarded headers from
#                                                it alone and ufw admits only it to the gateway
#   AUTODEPLOY_CONTROLPLANE_PORT=8081            (NOT 8080: Traefik's API owns 127.0.0.1:8080
#                                                and the binary's own default is :8080)
#   AUTODEPLOY_TRAEFIK_BIND=127.0.0.1            host address for published ports 80/443
#
# Idempotent: safe to re-run. Units whose binary or configuration changed are
# restarted only if already active; PostgreSQL is never restarted by this script.
set -euo pipefail

KIT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly KIT_DIR

# ---- fixed layout (units, preflight-app.sh and uninstall.sh assume these paths) ---
readonly AGENT_USER=autodeploy-agent
readonly CP_USER=autodeploy-controlplane
readonly ETC_DIR=/etc/autodeploy
readonly STATE_ROOT=/var/lib/autodeploy
readonly LIB_DIR=/usr/local/lib/autodeploy
readonly APP_DIR="$LIB_DIR/app"
readonly UNIT_DIR=/etc/systemd/system
readonly AGENT_STATE="$STATE_ROOT/agent"
readonly TRAEFIK_STATE="$STATE_ROOT/traefik"
readonly ROUTE_DIR="$TRAEFIK_STATE/dynamic"
readonly CHECKOUT_ENV="$ETC_DIR/checkout.env"
readonly UNITS=(autodeploy-postgres autodeploy-traefik autodeploy-controlplane autodeploy-agent)
readonly BRIDGE_NAME=br-autodeploy # fixed so the firewall rule can name the interface (<= 15 chars)

CP_ID="${CONTROLPLANE_ID:-802}"
CP_HOST="${AUTODEPLOY_CONTROLPLANE_HOST:-autodeploy.autodeploy.test}"
ENV_NETWORK="${AUTODEPLOY_ENV_NETWORK:-autodeploy-env}"
ENV_SUBNET="${AUTODEPLOY_ENV_SUBNET:-172.29.0.0/24}"
ENV_GATEWAY="${AUTODEPLOY_ENV_GATEWAY:-172.29.0.1}"
ENV_IP_RANGE="${AUTODEPLOY_ENV_IP_RANGE:-172.29.0.128/25}"
TRAEFIK_IP="${AUTODEPLOY_ENV_TRAEFIK_IP:-172.29.0.2}"
CP_PORT="${AUTODEPLOY_CONTROLPLANE_PORT:-8081}"
TRAEFIK_BIND="${AUTODEPLOY_TRAEFIK_BIND:-127.0.0.1}"

log() { printf '==> %s\n' "$*"; }
die() { printf 'install-app: error: %s\n' "$*" >&2; exit 1; }
# shellcheck source=stage-lib.sh
source "$KIT_DIR/stage-lib.sh"

case "${1:-}" in
  -h|--help) sed -n '2,38p' "${BASH_SOURCE[0]}"; exit 0 ;;
  "") die "usage: install-app.sh BINDIR" ;;
  -*) die "unknown option: $1" ;;
esac
(($# == 1)) || die "usage: install-app.sh BINDIR"
BINDIR="$1"

[[ $EUID -eq 0 ]] || die "must run as root"
[[ "$(uname -s)" == Linux && "$(uname -m)" == x86_64 ]] || die "Linux x86_64 only"
command -v systemctl >/dev/null || die "systemd required"
command -v docker >/dev/null || die "docker CLI required"
systemctl is-active --quiet docker.service || die "docker.service is not active"
[[ "$CP_ID" =~ ^[1-9][0-9]*$ ]] || die "invalid CONTROLPLANE_ID: $CP_ID"
((CP_ID < 65534)) || die "invalid CONTROLPLANE_ID: $CP_ID"
[[ "$CP_PORT" =~ ^[1-9][0-9]*$ ]] || die "invalid AUTODEPLOY_CONTROLPLANE_PORT: $CP_PORT"
((CP_PORT < 65536 && CP_PORT != 8080 && CP_PORT != 80 && CP_PORT != 443 && CP_PORT != 5432)) \
  || die "invalid AUTODEPLOY_CONTROLPLANE_PORT: $CP_PORT (must not collide with 80/443/5432/8080)"
[[ "$ENV_NETWORK" =~ ^[a-z0-9][a-z0-9_.-]{0,62}$ ]] || die "invalid network name: $ENV_NETWORK"
[[ "$ENV_SUBNET" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}/[0-9]{1,2}$ && "$ENV_IP_RANGE" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}/[0-9]{1,2}$ ]] || die "invalid subnet/ip-range"
[[ "$ENV_GATEWAY" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ && "$TRAEFIK_IP" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]] || die "invalid gateway/Traefik IP"
# IPv4 arithmetic for the network layout checks.
ip2int() { local a b c d; IFS=. read -r a b c d <<<"$1"; echo $(((a << 24) | (b << 16) | (c << 8) | d)); }
cidr_lo() { local m=$((32 - ${1#*/})); echo $(($(ip2int "${1%/*}") >> m << m)); }
cidr_hi() { local m=$((32 - ${1#*/})); echo $(($(cidr_lo "$1") | ((1 << m) - 1))); }
in_cidr() { local n; n="$(ip2int "$1")"; ((n >= $(cidr_lo "$2") && n <= $(cidr_hi "$2"))); } # ip cidr
for p in "$ENV_SUBNET" "$ENV_IP_RANGE"; do ((${p#*/} >= 8 && ${p#*/} <= 30)) || die "invalid prefix length in $p"; done
cidr_within() { (($(cidr_lo "$1") >= $(cidr_lo "$2") && $(cidr_hi "$1") <= $(cidr_hi "$2"))); } # inner outer
cidr_within "$ENV_IP_RANGE" "$ENV_SUBNET" || die "AUTODEPLOY_ENV_IP_RANGE $ENV_IP_RANGE is not inside $ENV_SUBNET"
in_cidr "$ENV_GATEWAY" "$ENV_SUBNET" || die "gateway must be inside $ENV_SUBNET"
in_cidr "$TRAEFIK_IP" "$ENV_SUBNET" || die "Traefik IP must be inside $ENV_SUBNET"
! in_cidr "$ENV_GATEWAY" "$ENV_IP_RANGE" || die "gateway $ENV_GATEWAY must be outside the container range $ENV_IP_RANGE"
! in_cidr "$TRAEFIK_IP" "$ENV_IP_RANGE" || die "Traefik IP $TRAEFIK_IP must be outside the container range $ENV_IP_RANGE (apps must never share its address space)"
[[ "$TRAEFIK_IP" != "$ENV_GATEWAY" ]] || die "Traefik IP must differ from the gateway"
[[ "$TRAEFIK_BIND" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]] || die "invalid AUTODEPLOY_TRAEFIK_BIND (IPv4 address)"
label='[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?'
[[ "$CP_HOST" =~ ^($label\.)+$label$ ]] || die "invalid AUTODEPLOY_CONTROLPLANE_HOST: $CP_HOST"

# Always the system Engine, never a per-user Docker Desktop context.
export DOCKER_HOST=unix:///var/run/docker.sock DOCKER_CONFIG=/nonexistent
unset DOCKER_CONTEXT

[[ -f "$CHECKOUT_ENV" ]] || die "$CHECKOUT_ENV not found: run install-checkout.sh first"
id -u "$AGENT_USER" >/dev/null 2>&1 || die "user $AGENT_USER not found: run install-checkout.sh first"
getent group docker >/dev/null || die "group docker not found (is Docker Engine installed?)"
AGENT_GID="$(id -g "$AGENT_USER")"
[[ "$(sed -n 's/^AUTODEPLOY_CHECKOUT_AGENT_GID=//p' "$CHECKOUT_ENV")" == "$AGENT_GID" ]] || die "agent gid differs from $CHECKOUT_ENV"

log "staging and validating inputs"
stage_init
for b in controlplane agent migrate admin secrets; do stage_copy "$BINDIR/$b" "$b" "$b"; done
stage_verify "$BINDIR"
for b in controlplane agent migrate admin secrets; do stage_require_elf "$b" "$b"; done
for f in traefik/traefik.yml traefik/autodeploy-api.yml traefik/autodeploy-tls.yml libexec/autodeploy-run \
  systemd/autodeploy-postgres.service systemd/autodeploy-traefik.service \
  systemd/autodeploy-controlplane.service systemd/autodeploy-agent.service; do
  [[ -f "$KIT_DIR/$f" ]] || die "kit file missing: $KIT_DIR/$f"
done

# ---- helpers ------------------------------------------------------------------------
install_file() { # src dst mode  -> 0 if replaced, 1 if already identical (atomic replace)
  if [[ -f "$2" ]] && cmp -s "$1" "$2" && [[ "$(stat -c '%u:%g:%a' "$2")" == "0:0:${3#0}" ]]; then return 1; fi
  local tmp
  tmp="$(mktemp "$2.XXXXXX")"
  install -o root -g root -m "$3" "$1" "$tmp"
  mv -f "$tmp" "$2"
}
mark() { printf -v "$1" 1; } # mark VAR: set a change flag
CP_CHANGED=0 AGENT_CHANGED=0 TRAEFIK_CHANGED=0 PG_UNIT_CHANGED=0

put_generated() { # dst flagvar  (content on stdin; world-readable, non-secret)
  local tmp
  tmp="$(mktemp "$1.XXXXXX")"
  cat >"$tmp"
  chmod 0644 "$tmp"
  chown root:root "$tmp"
  if [[ -f "$1" ]] && cmp -s "$tmp" "$1"; then rm -f "$tmp"; else mv -f "$tmp" "$1"; mark "$2"; fi
}

was_active() { systemctl is-active --quiet "$1.service"; }
WAS_CP=0 WAS_AGENT=0 WAS_TRAEFIK=0
if was_active autodeploy-controlplane; then WAS_CP=1; fi
if was_active autodeploy-agent; then WAS_AGENT=1; fi
if was_active autodeploy-traefik; then WAS_TRAEFIK=1; fi

# ---- 1. identities ------------------------------------------------------------------
# 802: next free number after the checkout kit's 800/801, same fixed-ID convention and
# the same Debian dynamic-system range below adduser --system's allocation region. The
# control plane needs no stable numeric identity for any on-disk binding (unlike the
# checkout UID/GID in the quota ledger); a fixed ID just keeps credential-file ownership
# reproducible across reinstalls.
log "ensuring identity $CP_USER ($CP_ID)"
if ! getent group "$CP_USER" >/dev/null; then
  if getent group "$CP_ID" >/dev/null; then die "gid $CP_ID is already used by group $(getent group "$CP_ID" | cut -d: -f1)"; fi
  groupadd --system --gid "$CP_ID" "$CP_USER"
fi
if ! id -u "$CP_USER" >/dev/null 2>&1; then
  if getent passwd "$CP_ID" >/dev/null; then die "uid $CP_ID is already used by $(getent passwd "$CP_ID" | cut -d: -f1)"; fi
  useradd --system --uid "$CP_ID" --gid "$CP_USER" --no-create-home --home-dir /nonexistent \
    --shell /usr/sbin/nologin "$CP_USER"
fi
CP_UID="$(id -u "$CP_USER")"
[[ "$(id -gn "$CP_USER")" == "$CP_USER" && "$CP_UID" != 0 ]] || die "$CP_USER must be a non-root user with its own primary group"
[[ "$(getent passwd "$CP_USER" | cut -d: -f7)" == /usr/sbin/nologin ]] || die "$CP_USER must have the nologin shell"
[[ "$(id -G "$CP_USER")" == "$(id -g "$CP_USER")" ]] || die "$CP_USER must have no supplementary groups"
[[ "$CP_UID" != "$(id -u "$AGENT_USER")" ]] || die "control plane and agent UIDs must differ"

# docker group: root-equivalent (the agent can start privileged containers through
# dockerd). Inherent to driving dockerd; only the agent gets it, and ONLY the agent:
# the control plane and the checkout identity must never be members.
log "adding $AGENT_USER to the docker group (root-equivalent; inherent to driving dockerd)"
if ! id -nG "$AGENT_USER" | tr ' ' '\n' | grep -qx docker; then
  usermod -aG docker "$AGENT_USER"
  # Supplementary groups are read at process start: a running agent needs a restart to get docker.
  mark AGENT_CHANGED
fi
for u in "$CP_USER" autodeploy-checkout; do
  if id -u "$u" >/dev/null 2>&1 && id -nG "$u" | tr ' ' '\n' | grep -qx docker; then die "$u must not be in the docker group"; fi
done

# ---- 2. directories -----------------------------------------------------------------
# Layout and the code that forces it:
#  /etc/autodeploy/controlplane   root:cp 0750. internal/credential/open_linux.go safeDirectory:
#       every ancestor owned by root or the process UID and not group/world-writable.
#       Files inside: owned by cp, 0600 (stat_linux.go safeCredentialStat + file.go safeFileMode).
#  /etc/autodeploy/agent          root:agent 0750. The Traefik API credentials file must be
#       owned by the agent with mode EXACTLY 0600 (internal/agent/client/protectedfile.go).
#  /var/lib/autodeploy/agent      agent:agent 0700. cmd/agent state dir: credential at
#       <dir>/credential (0600, agent-owned) and fence state below it; state.New does
#       MkdirAll(0700) but the agent cannot create it under root-owned /var/lib/autodeploy.
#  .../agent/home                 agent:agent 0700: HOME/DOCKER_CONFIG for the docker/buildx subprocess.
#  /var/lib/autodeploy/traefik/dynamic  root:agent 1770: Traefik's file-provider directory and the
#       agent's route directory. internal/runtime/traefik/write.go checkDirectoryPolicy accepts
#       an owner of the agent UID or root, rejects world-writable, accepts group-writable for a
#       trusted owner. Root-owned + group-writable + STICKY lets the agent create and replace
#       its own route files but not delete or overwrite the root-owned installer files
#       (management-API auth, TLS, control-plane route). Traefik reads it as group agent
#       (--group-add), route files are written 0640 (write.go routeFileMode).
mkdir_as() { install -d -o "$1" -g "$2" -m "$3" "$4"; }
log "creating directories"
mkdir_as root root 0755 "$ETC_DIR"
mkdir_as root root 0755 "$STATE_ROOT"
mkdir_as root root 0755 /usr/local/lib
mkdir_as root root 0755 "$LIB_DIR"
mkdir_as root root 0755 "$APP_DIR"
mkdir_as root "$CP_USER" 0750 "$ETC_DIR/controlplane"
mkdir_as root "$AGENT_USER" 0750 "$ETC_DIR/agent"
mkdir_as root root 0700 "$ETC_DIR/secrets"
mkdir_as root root 0700 "$ETC_DIR/traefik-secrets"
mkdir_as root root 0755 "$ETC_DIR/traefik"
mkdir_as root root 0755 "$ETC_DIR/tls"
mkdir_as "$AGENT_USER" "$AGENT_USER" 0700 "$AGENT_STATE"
mkdir_as "$AGENT_USER" "$AGENT_USER" 0700 "$AGENT_STATE/home"
mkdir_as root root 0755 "$TRAEFIK_STATE"
mkdir_as root "$AGENT_USER" 1770 "$ROUTE_DIR"
chmod 1770 "$ROUTE_DIR" # install -d does not change the mode of an existing directory
chown root:"$AGENT_USER" "$ROUTE_DIR"

# ---- 3. binaries --------------------------------------------------------------------
log "installing binaries into $APP_DIR (root:root 0755)"
for b in controlplane agent migrate admin secrets; do
  if install_file "$STAGE/$b" "$APP_DIR/$b" 0755; then
    case "$b" in controlplane) mark CP_CHANGED ;; agent) mark AGENT_CHANGED ;; *) ;; esac
  fi
done
install_file "$KIT_DIR/libexec/autodeploy-run" "$LIB_DIR/autodeploy-run" 0755 || true

# ---- 4. Docker network, firewall ----------------------------------------------------
# Environment network: created here, never by the agent (internal/runtime/docker/engine.go
# InspectNetwork: "environment networks are provisioned outside this package"). Traefik
# joins it to resolve application containers by name (see autodeploy-traefik.service).
# PostgreSQL is deliberately not on it.
#
# Privilege boundary: apps share this network with Traefik, and both the ufw rule and the
# control plane's trusted-proxy list are keyed on SOURCE ADDRESS. So Docker may hand
# containers only $ENV_IP_RANGE (--ip-range): the agent starts app containers with a
# network ID only, never a static IP (internal/runtime/docker/run.go), hence they are
# allocated inside that range; Traefik alone gets the fixed low address $TRAEFIK_IP.
net_inspect() { docker network inspect -f "$1" "$ENV_NETWORK"; }
check_network() {
  local have_subnet have_range have_gw have_bridge
  have_subnet="$(net_inspect '{{range .IPAM.Config}}{{.Subnet}} {{end}}' | tr -d ' ')"
  have_range="$(net_inspect '{{range .IPAM.Config}}{{.IPRange}} {{end}}' | tr -d ' ')"
  have_gw="$(net_inspect '{{range .IPAM.Config}}{{.Gateway}} {{end}}' | tr -d ' ')"
  have_bridge="$(net_inspect '{{index .Options "com.docker.network.bridge.name"}}')"
  if [[ "$have_subnet" != "$ENV_SUBNET" || "$have_range" != "$ENV_IP_RANGE" || "$have_gw" != "$ENV_GATEWAY" || "$have_bridge" != "$BRIDGE_NAME" ]]; then
    die "network $ENV_NETWORK exists with subnet '$have_subnet', ip-range '${have_range:-none}', gateway '$have_gw', bridge '${have_bridge:-default}';
       expected subnet $ENV_SUBNET, ip-range $ENV_IP_RANGE, gateway $ENV_GATEWAY, bridge $BRIDGE_NAME.
       Stop autodeploy-traefik and every app container on it, then: docker network rm $ENV_NETWORK, and re-run."
  fi
}
if docker network inspect "$ENV_NETWORK" >/dev/null 2>&1; then
  check_network
  log "network $ENV_NETWORK exists"
else
  log "creating Docker network $ENV_NETWORK ($ENV_SUBNET, container range $ENV_IP_RANGE, gateway $ENV_GATEWAY, bridge $BRIDGE_NAME)"
  docker network create --driver bridge --subnet "$ENV_SUBNET" --ip-range "$ENV_IP_RANGE" --gateway "$ENV_GATEWAY" \
    -o "com.docker.network.bridge.name=$BRIDGE_NAME" "$ENV_NETWORK" >/dev/null
  check_network
fi
# Only Traefik (fixed address) may reach the control plane on the bridge gateway. With ufw
# active, container -> host traffic is dropped by default, so allow exactly that source.
# The rule is found again by its comment (uninstall.sh). Any older, wider rule from a previous
# kit version (whole bridge) is removed first.
ufw_active() { command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q '^Status: active'; }
ufw_delete_by_comment() { # fixed comment text; deletes highest rule numbers first
  local n
  while read -r n; do
    if [[ -n "$n" ]]; then ufw --force delete "$n" >/dev/null 2>&1; fi
  done < <(ufw status numbered | sed -n "s/^\[ *\([0-9][0-9]*\)\].*# $1.*/\1/p" | sort -rn)
}
if ufw_active; then
  ufw_delete_by_comment 'autodeploy: traefik'
  log "ufw is active: allowing $TRAEFIK_IP -> $ENV_GATEWAY port $CP_PORT/tcp on $BRIDGE_NAME"
  ufw allow in on "$BRIDGE_NAME" from "$TRAEFIK_IP" to "$ENV_GATEWAY" port "$CP_PORT" proto tcp comment 'autodeploy: traefik' >/dev/null
else
  # Without ufw the host-side port is reachable from ANY container on the bridge, including
  # untrusted apps (the source-address restriction exists only as the ufw rule).
  echo "install-app: WARNING: ufw is not active: $ENV_GATEWAY:$CP_PORT is reachable from every container on $ENV_NETWORK." >&2
  echo "install-app: WARNING: enable ufw (and re-run this script) or add an equivalent iptables/nftables rule before running untrusted apps." >&2
fi

# ---- 5. non-secret configuration ----------------------------------------------------
log "writing non-secret configuration"
put_generated "$ETC_DIR/host.env" TRAEFIK_CHANGED <<ENVEOF
# Generated by deploy/host/install-app.sh. Non-secret. Do not edit by hand.
AUTODEPLOY_ENV_NETWORK=$ENV_NETWORK
AUTODEPLOY_ENV_SUBNET=$ENV_SUBNET
AUTODEPLOY_ENV_GATEWAY=$ENV_GATEWAY
AUTODEPLOY_ENV_IP_RANGE=$ENV_IP_RANGE
AUTODEPLOY_ENV_TRAEFIK_IP=$TRAEFIK_IP
AUTODEPLOY_TRAEFIK_BIND=$TRAEFIK_BIND
AUTODEPLOY_AGENT_GID=$AGENT_GID
AUTODEPLOY_CONTROLPLANE_HOST=$CP_HOST
AUTODEPLOY_CONTROLPLANE_PORT=$CP_PORT
ENVEOF
# Credential paths are in the environment, secret values never are
# (internal/config/database.go). AUTODEPLOY_REPOSITORY_ROOT only has to be a directory the
# credential files are NOT inside; the checkout parent is the "workspace root".
# GitHub App and the secret (KEK) subsystem are left disabled: cmd/controlplane serves the
# agent checkout routes only with the GitHub App enabled (503 otherwise), so enable both
# (see deploy/README.md) before the 18e end-to-end deploy.
put_generated "$ETC_DIR/controlplane.env" CP_CHANGED <<ENVEOF
# Generated by deploy/host/install-app.sh. Non-secret. Do not edit by hand.
AUTODEPLOY_REPOSITORY_ROOT=/srv/autodeploy
AUTODEPLOY_DATABASE_URL_FILE=$ETC_DIR/controlplane/database-url
AUTODEPLOY_USERNAME_THROTTLE_ACTIVE_VERSION=1
AUTODEPLOY_USERNAME_THROTTLE_ACTIVE_KEY_FILE=$ETC_DIR/controlplane/username-throttle-key-1
AUTODEPLOY_CSRF_ACTIVE_VERSION=1
AUTODEPLOY_CSRF_ACTIVE_KEY_FILE=$ETC_DIR/controlplane/csrf-key-1
AUTODEPLOY_PUBLIC_ORIGIN=https://$CP_HOST
AUTODEPLOY_LISTEN_ADDRESS=$ENV_GATEWAY:$CP_PORT
AUTODEPLOY_TRUSTED_PROXY_CIDRS=$TRAEFIK_IP/32
AUTODEPLOY_AGENT_ENABLED=true
ENVEOF
put_generated "$ETC_DIR/agent.env" AGENT_CHANGED <<ENVEOF
# Generated by deploy/host/install-app.sh. Non-secret. Do not edit by hand.
AUTODEPLOY_AGENT_BASE_URL=https://$CP_HOST
AUTODEPLOY_AGENT_STATE_DIR=$AGENT_STATE
AUTODEPLOY_AGENT_ROUTE_DIR=$ROUTE_DIR
AUTODEPLOY_AGENT_TRAEFIK_CREDENTIALS_FILE=$ETC_DIR/agent/traefik-api-credentials
AUTODEPLOY_AGENT_PROBE_TIMEOUT=3m
AUTODEPLOY_AGENT_DRAIN_STOP_GRACE=30s
ENVEOF
if [[ ! -e "$ETC_DIR/agent-local.env" ]]; then
  printf '%s\n' '# Owner-edited, never overwritten by install-app.sh. Set after registering this server in the control plane UI.' \
    'AUTODEPLOY_AGENT_SERVER_ID=' >"$ETC_DIR/agent-local.env"
  chmod 0644 "$ETC_DIR/agent-local.env"
  chown root:root "$ETC_DIR/agent-local.env"
fi

# Traefik: static config (container restart to apply) and the root-owned dynamic files
# (hot-reloaded by the file provider; no restart).
if install_file "$KIT_DIR/traefik/traefik.yml" "$ETC_DIR/traefik/traefik.yml" 0644; then mark TRAEFIK_CHANGED; fi
install_file "$KIT_DIR/traefik/autodeploy-api.yml" "$ROUTE_DIR/autodeploy-api.yml" 0644 || true
install_file "$KIT_DIR/traefik/autodeploy-tls.yml" "$ROUTE_DIR/autodeploy-tls.yml" 0644 || true
cp_route="$(mktemp)"
cat >"$cp_route" <<ROUTEEOF
# Generated by deploy/host/install-app.sh: route to the control plane. No certResolver:
# the certificate comes from autodeploy-tls.yml (private CA).
http:
  routers:
    autodeploy-controlplane:
      rule: "Host(\`$CP_HOST\`)"
      entryPoints:
        - websecure
      service: autodeploy-controlplane
      tls: {}
  services:
    autodeploy-controlplane:
      loadBalancer:
        servers:
          - url: "http://$ENV_GATEWAY:$CP_PORT"
ROUTEEOF
install_file "$cp_route" "$ROUTE_DIR/autodeploy-controlplane.yml" 0644 || true
rm -f "$cp_route"

# ---- 6. units -----------------------------------------------------------------------
log "installing units"
for u in "${UNITS[@]}"; do
  if install_file "$KIT_DIR/systemd/$u.service" "$UNIT_DIR/$u.service" 0644; then
    case "$u" in
      autodeploy-postgres) mark PG_UNIT_CHANGED ;;
      autodeploy-traefik) mark TRAEFIK_CHANGED ;;
      autodeploy-controlplane) mark CP_CHANGED ;;
      autodeploy-agent) mark AGENT_CHANGED ;;
      *) ;;
    esac
  fi
done
systemctl daemon-reload
for u in "${UNITS[@]}"; do systemctl enable "$u.service" >/dev/null 2>&1; done

# A running service keeps its old binary/config until restarted. PostgreSQL is left alone.
if ((WAS_TRAEFIK && TRAEFIK_CHANGED)); then log "restarting autodeploy-traefik"; systemctl restart autodeploy-traefik.service; fi
if ((WAS_CP && CP_CHANGED)); then log "restarting autodeploy-controlplane"; systemctl restart autodeploy-controlplane.service; fi
if ((WAS_AGENT && AGENT_CHANGED)); then log "restarting autodeploy-agent"; systemctl restart autodeploy-agent.service; fi
if ((PG_UNIT_CHANGED)); then log "note: the PostgreSQL unit changed; restart it yourself when convenient (systemctl restart autodeploy-postgres)"; fi

cat <<NEXTEOF

==> installed. Nothing was started and no secret exists yet. Next, as root, in order:

 1. Private CA, certificate and hosts entries (add one --host per app hostname you will deploy):
      sudo $KIT_DIR/private-ca.sh --host demo.${CP_HOST#*.}
 2. Generate every credential file (random values, never printed, never in argv/env):
      sudo $KIT_DIR/create-credentials.sh
 3. Start the database and the proxy, apply migrations, start the control plane:
      sudo systemctl start autodeploy-postgres autodeploy-traefik
      sudo $LIB_DIR/autodeploy-run migrate
      sudo $LIB_DIR/autodeploy-run admin bootstrap --username administrator   # prompts on the terminal
      sudo systemctl start autodeploy-controlplane
 4. Sign in at https://$CP_HOST (import /etc/autodeploy/ca/ca.crt into your browser), register this
    server, put its id in $ETC_DIR/agent-local.env (AUTODEPLOY_AGENT_SERVER_ID=...), then enroll
    with the one-time token typed at a hidden prompt (held in a shell variable, piped on stdin;
    never in argv or the environment):
      read -rs -p 'enrollment token: ' T; echo
      printf '%s\n' "\$T" | sudo -u $AGENT_USER $APP_DIR/agent enroll \\
        --base-url https://$CP_HOST --server-id <SERVER_ID> --state-dir $AGENT_STATE
      unset T
 5. sudo systemctl start autodeploy-agent     # refuses to start until the credential exists
 6. sudo $KIT_DIR/preflight-app.sh            # read-only; also run preflight-checkout.sh

 Note: docker group membership of $AGENT_USER is root-equivalent on this host. Docker-published
 ports bypass ufw; Traefik 80/443 are bound to $TRAEFIK_BIND only.
NEXTEOF
