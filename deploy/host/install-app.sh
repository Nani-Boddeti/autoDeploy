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
#   install-app.sh [--insecure-no-checksum] [--mode lan|public] [public-mode options] BINDIR
#
#   --mode lan      (default) private-CA / LAN host. Generated files are unchanged from earlier kits
#                   (pinned by tests/lan_mode_identical.sh); host.env gains AUTODEPLOY_TLS_MODE=lan.
#   --mode public   generic public VPS with Let's Encrypt (ACME) certificates. Requires
#                   AUTODEPLOY_CONTROLPLANE_HOST (a real FQDN pointing at this host; no default),
#                   skips the private CA and autodeploy-tls.yml, binds Traefik 80/443 to 0.0.0.0 by
#                   default and installs traefik/traefik-public.yml (a TEMPLATE rendered here from
#                   the validated flags below) with certificatesResolvers.letsencrypt-production.
#                   An in-place lan -> public switch (private CA already installed) is refused;
#                   public -> lan is refused too: reinstall fresh.
#     --acme-challenge http|tls-alpn   HTTP-01 on :80 (default) or TLS-ALPN-01 on :443
#     --acme-staging                   Let's Encrypt STAGING (manual issuance dry run only: the
#                                      agent probe uses system roots and rejects staging certs)
#     --acme-email ADDR                optional account/expiry-notice address (a warning if omitted)
#     --configure-firewall             ufw: allow detected sshd port(s), 80/tcp, 443/tcp, default
#                                      deny incoming, enable. Never touches ufw without this flag.
#                                      Needs the SSH port typed back at a prompt, or
#                                      --yes-firewall-ssh-port=N (must be a detected sshd port).
#                                      Docker-published ports bypass ufw.
#
#   BINDIR  directory with prebuilt linux/amd64 binaries: controlplane, agent, migrate,
#           admin, secrets (GOOS=linux GOARCH=amd64 go build ./cmd/...) and SHA256SUMS
#           (sha256sum format; every binary must be listed and match; a MISSING SHA256SUMS is
#           fatal unless --insecure-no-checksum is given). Deliver SHA256SUMS, or its hash,
#           over a channel separate from the binaries. Inputs are copied to a root-only
#           staging directory first; validation and installation use the copies.
#
# Database roles (22-7 F1): migrations run as the OWNER role `autodeploy` (superuser created by
# the postgres container; URL in /etc/autodeploy/secrets/database-owner-url, root-only, read
# only by `autodeploy-run migrate`). Everything else (control plane, admin, secrets) uses the
# least-privilege runtime role `autodeploy_app` (DML only) via controlplane/database-url.
# On an existing install (database-url still holds the owner URL) re-running this script
# creates the runtime credential, migrates as the owner, enables the runtime role's LOGIN,
# switches database-url to the runtime URL and restarts the control plane.
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
#                                                (public mode default: 0.0.0.0)
#   AUTODEPLOY_METRICS_LISTEN_ADDRESS=           optional Prometheus /metrics listener, e.g.
#                                                127.0.0.1:9101 (loopback IP literal only; the
#                                                control plane refuses anything else). Unset =
#                                                disabled; pass it on EVERY run like the others.
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
METRICS_LISTEN="${AUTODEPLOY_METRICS_LISTEN_ADDRESS:-}"

CP_HOST_GIVEN="${AUTODEPLOY_CONTROLPLANE_HOST:-}"
TRAEFIK_BIND_GIVEN="${AUTODEPLOY_TRAEFIK_BIND:-}"
HOSTS_FILE="${AUTODEPLOY_HOSTS_FILE:-/etc/hosts}"
readonly ACME_DIR="$TRAEFIK_STATE/acme"
readonly PUBLIC_HOSTS_BEGIN="# BEGIN autodeploy-public-cp (managed by deploy/host/install-app.sh)"
readonly PUBLIC_HOSTS_END="# END autodeploy-public-cp"
MODE=lan
ACME_CHALLENGE=http
ACME_STAGING=0
ACME_EMAIL=""
CONFIGURE_FIREWALL=0
FIREWALL_SSH_PORT_ARG=""

log() { printf '==> %s\n' "$*"; }
die() { printf 'install-app: error: %s\n' "$*" >&2; exit 1; }
# shellcheck source=stage-lib.sh
source "$KIT_DIR/stage-lib.sh"
# Subnet collision helper (check_subnet_collision), provided by netcheck.sh when present.
if [[ -f "$KIT_DIR/netcheck.sh" ]]; then
  # shellcheck source=netcheck.sh
  source "$KIT_DIR/netcheck.sh"
fi

# ---- generators (pure: write to stdout / take paths; also exercised by tests/lan_mode_identical.sh) ----
render_host_env() {
  cat <<ENVEOF
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
AUTODEPLOY_TLS_MODE=$MODE
ENVEOF
  if [[ "$MODE" == public ]]; then
    cat <<ENVEOF
AUTODEPLOY_ACME_CHALLENGE=$ACME_CHALLENGE
AUTODEPLOY_ACME_STAGING=$ACME_STAGING
AUTODEPLOY_ACME_EMAIL=$ACME_EMAIL
ENVEOF
  fi
}

render_cp_route() {
  if [[ "$MODE" == public ]]; then
    cat <<ROUTEEOF
# Generated by deploy/host/install-app.sh: route to the control plane. The certificate comes
# from the ACME resolver letsencrypt-production (traefik.yml, public mode).
http:
  routers:
    autodeploy-controlplane:
      rule: "Host(\`$CP_HOST\`)"
      entryPoints:
        - websecure
      service: autodeploy-controlplane
      tls:
        certResolver: "letsencrypt-production"
  services:
    autodeploy-controlplane:
      loadBalancer:
        servers:
          - url: "http://$ENV_GATEWAY:$CP_PORT"
ROUTEEOF
  else
    cat <<ROUTEEOF
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
  fi
}

# render_traefik_unit SRC: lan = SRC verbatim. public = SRC with the three tls lines replaced by
# the acme mount/assert (every pattern must match exactly once, else die).
render_traefik_unit() {
  if [[ "$MODE" != public ]]; then cat "$1"; return; fi
  local line n_crt=0 n_key=0 n_mnt=0
  while IFS= read -r line || [[ -n "$line" ]]; do
    case "$line" in
      "AssertPathExists=/etc/autodeploy/tls/server.crt")
        printf '%s\n' "AssertPathExists=$ACME_DIR/acme.json"; n_crt=$((n_crt + 1)) ;;
      "AssertPathExists=/etc/autodeploy/tls/server.key") n_key=$((n_key + 1)) ;;
      "  -v /etc/autodeploy/tls:/etc/traefik/tls:ro \\")
        printf '%s\n' "  -v $ACME_DIR:/etc/traefik/acme \\"; n_mnt=$((n_mnt + 1)) ;;
      *) printf '%s\n' "$line" ;;
    esac
  done <"$1"
  [[ $n_crt == 1 && $n_key == 1 && $n_mnt == 1 ]] || die "traefik unit template changed: tls lines not found exactly once ($n_crt/$n_key/$n_mnt)"
}

# render_traefik_static TEMPLATE: fills the @ACME_*@ placeholders of traefik-public.yml from the
# already-validated ACME_EMAIL / ACME_CHALLENGE / ACME_STAGING (no sed, no shell expansion of values).
render_traefik_static() {
  local line ca
  if ((ACME_STAGING)); then ca=https://acme-staging-v02.api.letsencrypt.org/directory
  else ca=https://acme-v02.api.letsencrypt.org/directory; fi
  while IFS= read -r line || [[ -n "$line" ]]; do
    case "$line" in
      "@ACME_EMAIL_LINE@") if [[ -n "$ACME_EMAIL" ]]; then printf '      email: "%s"\n' "$ACME_EMAIL"; fi ;;
      "@ACME_CHALLENGE_BLOCK@")
        if [[ "$ACME_CHALLENGE" == tls-alpn ]]; then
          printf '%s\n' '      tlsChallenge: {}'
        else
          printf '%s\n' '      httpChallenge:' '        entryPoint: web'
        fi ;;
      "#"*) printf '%s\n' "$line" ;;
      *) printf '%s\n' "${line//@ACME_CA_SERVER@/$ca}" ;;
    esac
  done <"$1"
}

valid_public_host() { # FQDN, not an IP literal, not a reserved/private suffix
  local label='[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?'
  [[ "$1" =~ ^($label\.)+$label$ && ${#1} -le 253 ]] || return 1
  [[ ! "$1" =~ ^[0-9.]+$ && ! "${1##*.}" =~ ^[0-9]+$ ]] || return 1
  case "$1" in *.test|*.local|*.localhost|*.internal|localhost) return 1 ;; *) return 0 ;; esac
}

# Public mode: managed /etc/hosts block CP_HOST -> 127.0.0.1 so agent -> control plane never needs
# hairpin NAT. Markers differ from private-ca.sh's. Same intactness rule: BEGIN/END once each, in
# order, or both absent; anything else is refused with nothing changed.
manage_public_hosts_block() {
  local block nb ne current tmp
  block="$PUBLIC_HOSTS_BEGIN"$'\n'"127.0.0.1 $CP_HOST"$'\n'"$PUBLIC_HOSTS_END"
  nb="$(grep -cxF "$PUBLIC_HOSTS_BEGIN" "$HOSTS_FILE" || true)"
  ne="$(grep -cxF "$PUBLIC_HOSTS_END" "$HOSTS_FILE" || true)"
  if [[ "$nb" == 1 && "$ne" == 1 ]]; then
    [[ "$(grep -nxF "$PUBLIC_HOSTS_BEGIN" "$HOSTS_FILE" | cut -d: -f1)" -lt "$(grep -nxF "$PUBLIC_HOSTS_END" "$HOSTS_FILE" | cut -d: -f1)" ]] \
      || die "$HOSTS_FILE: the autodeploy-public-cp END marker precedes BEGIN; fix it by hand and re-run. Nothing was changed."
  elif [[ "$nb" != 0 || "$ne" != 0 ]]; then
    die "$HOSTS_FILE has a damaged autodeploy-public-cp block (BEGIN x$nb, END x$ne); fix it by hand and re-run. Nothing was changed."
  fi
  current="$(awk -v b="$PUBLIC_HOSTS_BEGIN" -v e="$PUBLIC_HOSTS_END" '$0==b{p=1} p{print} $0==e{p=0}' "$HOSTS_FILE")"
  if [[ "$current" == "$block" ]]; then
    log "$HOSTS_FILE already maps $CP_HOST -> 127.0.0.1"
    return
  fi
  log "updating the managed block in $HOSTS_FILE: $CP_HOST -> 127.0.0.1"
  [[ -e "$HOSTS_FILE.autodeploy.bak" ]] || cp -p "$HOSTS_FILE" "$HOSTS_FILE.autodeploy.bak"
  tmp="$(mktemp "$HOSTS_FILE.XXXXXX")"
  awk -v b="$PUBLIC_HOSTS_BEGIN" -v e="$PUBLIC_HOSTS_END" '$0==b{skip=1} !skip{print} $0==e{skip=0}' "$HOSTS_FILE" >"$tmp"
  printf '%s\n' "$block" >>"$tmp"
  chmod --reference="$HOSTS_FILE" "$tmp"
  chown --reference="$HOSTS_FILE" "$tmp"
  mv -f "$tmp" "$HOSTS_FILE"
}

# Test hook: define everything above, run nothing (tests/lan_mode_identical.sh).
if [[ "${AUTODEPLOY_INSTALL_APP_SOURCE_ONLY:-0}" == 1 ]]; then return 0 2>/dev/null || exit 0; fi

BINDIR=""
ACME_OPT=0
while (($#)); do
  case "$1" in
    -h|--help) awk 'NR==1{next} /^#/{print; next} {exit}' "${BASH_SOURCE[0]}"; exit 0 ;;
    --insecure-no-checksum) INSECURE_NO_CHECKSUM=1; shift ;;
    --mode) (($# >= 2)) || die "--mode needs lan|public"; MODE="$2"; shift 2 ;;
    --mode=*) MODE="${1#*=}"; shift ;;
    --acme-challenge) (($# >= 2)) || die "--acme-challenge needs http|tls-alpn"; ACME_CHALLENGE="$2"; ACME_OPT=1; shift 2 ;;
    --acme-challenge=*) ACME_CHALLENGE="${1#*=}"; ACME_OPT=1; shift ;;
    --acme-staging) ACME_STAGING=1; ACME_OPT=1; shift ;;
    --acme-email) (($# >= 2)) || die "--acme-email needs an address"; ACME_EMAIL="$2"; ACME_OPT=1; shift 2 ;;
    --acme-email=*) ACME_EMAIL="${1#*=}"; ACME_OPT=1; shift ;;
    --configure-firewall) CONFIGURE_FIREWALL=1; ACME_OPT=1; shift ;;
    --yes-firewall-ssh-port=*) FIREWALL_SSH_PORT_ARG="${1#*=}"; ACME_OPT=1; shift ;;
    -*) die "unknown option: $1" ;;
    *) [[ -z "$BINDIR" ]] || die "usage: install-app.sh [--insecure-no-checksum] [--mode lan|public] BINDIR"; BINDIR="$1"; shift ;;
  esac
done
[[ -n "$BINDIR" ]] || die "usage: install-app.sh [--insecure-no-checksum] [--mode lan|public] BINDIR"
[[ "$MODE" == lan || "$MODE" == public ]] || die "invalid --mode: $MODE (lan|public)"
if [[ "$MODE" == lan && "$ACME_OPT" == 1 ]]; then die "--acme-*/--configure-firewall/--yes-firewall-ssh-port only apply with --mode public"; fi
if [[ "$MODE" == public ]]; then
  [[ -n "$CP_HOST_GIVEN" ]] || die "--mode public requires AUTODEPLOY_CONTROLPLANE_HOST (the real FQDN that points at this host; there is no default)"
  valid_public_host "$CP_HOST" || die "invalid public AUTODEPLOY_CONTROLPLANE_HOST: $CP_HOST (need a lowercase FQDN; IP literals and *.test/.local/.localhost/.internal are rejected)"
  [[ "$ACME_CHALLENGE" == http || "$ACME_CHALLENGE" == tls-alpn ]] || die "invalid --acme-challenge: $ACME_CHALLENGE (http|tls-alpn)"
  if [[ -n "$ACME_EMAIL" ]]; then
    [[ "$ACME_EMAIL" =~ ^[A-Za-z0-9._%+-]{1,64}@[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)+$ && ${#ACME_EMAIL} -le 254 ]] || die "invalid --acme-email: $ACME_EMAIL"
  else
    echo "install-app: WARNING: no --acme-email: Let's Encrypt cannot send expiry or account notices." >&2
  fi
  if [[ -z "$TRAEFIK_BIND_GIVEN" ]]; then TRAEFIK_BIND=0.0.0.0; fi
fi

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
# Loopback IP literal + port only (the control plane re-validates); also keeps the value a single
# safe line in controlplane.env.
[[ -z "$METRICS_LISTEN" || "$METRICS_LISTEN" =~ ^(127(\.[0-9]{1,3}){3}|\[::1\]|\[::ffff:127(\.[0-9]{1,3}){3}\]):[1-9][0-9]{0,4}$ ]] \
  || die "invalid AUTODEPLOY_METRICS_LISTEN_ADDRESS: must be a loopback IP literal and port, e.g. 127.0.0.1:9101"
# Range checks too: a value the control plane rejects would stop it from starting on the restart below.
if [[ -n "$METRICS_LISTEN" ]]; then
  metrics_port="${METRICS_LISTEN##*:}"
  ((10#$metrics_port <= 65535)) || die "invalid AUTODEPLOY_METRICS_LISTEN_ADDRESS: port $metrics_port > 65535"
  metrics_v4="${METRICS_LISTEN%:*}"
  metrics_v4="${metrics_v4#\[::ffff:}"
  metrics_v4="${metrics_v4%\]}"
  if [[ "$metrics_v4" == 127.* ]]; then
    IFS=. read -r -a metrics_octets <<<"$metrics_v4"
    for octet in "${metrics_octets[@]}"; do
      [[ "$octet" =~ ^(0|[1-9][0-9]*)$ ]] && ((octet <= 255)) \
        || die "invalid AUTODEPLOY_METRICS_LISTEN_ADDRESS: octet $octet (0-255, no leading zeros)"
    done
  fi
  # Traefik's API owns 127.0.0.1:8080 (traefik/autodeploy-api.yml).
  ((10#$metrics_port != 8080)) || die "invalid AUTODEPLOY_METRICS_LISTEN_ADDRESS: port 8080 is Traefik's API"
fi
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

# No in-place mode switch: lan -> public with a private CA installed, or public -> lan, is a
# fresh install (the TLS material and Traefik static config differ). host.env without a mode line = lan.
prev_mode=""
if [[ -r "$ETC_DIR/host.env" ]]; then prev_mode="$(sed -n 's/^AUTODEPLOY_TLS_MODE=//p' "$ETC_DIR/host.env" | head -n1)"; fi
if [[ "$MODE" == public && -e "$ETC_DIR/host.env" && "${prev_mode:-lan}" == lan ]]; then
  if [[ -e "$ETC_DIR/ca/ca.crt" || -e "$ETC_DIR/tls/server.crt" || -e /usr/local/share/ca-certificates/autodeploy-private-ca.crt ]]; then
    die "$ETC_DIR/host.env says lan and a private CA is installed: switching to --mode public in place is unsupported. Uninstall (deploy/host/uninstall.sh --yes) and install fresh with --mode public."
  fi
fi
# A lan install that never ran private-ca.sh can still reach this point; its autodeploy-tls.yml would
# then point Traefik at /etc/autodeploy/tls files that public mode does not mount. Never deleted here.
if [[ "$MODE" == public && -e "$ROUTE_DIR/autodeploy-tls.yml" ]]; then
  die "$ROUTE_DIR/autodeploy-tls.yml exists (left by a lan install): switching to --mode public in place is unsupported. Do a fresh install (uninstall.sh --yes first), or remove that file manually after reading docs/operations/vps-install.md. Nothing was changed."
fi
if [[ "$MODE" == lan && "$prev_mode" == public ]]; then
  die "$ETC_DIR/host.env says public: re-run with --mode public (public -> lan in place is unsupported; uninstall and install fresh)."
fi

# Firewall flag: detect sshd port(s) and get the typed confirmation BEFORE anything is changed.
FIREWALL_SSH_PORTS=()
if ((CONFIGURE_FIREWALL)); then
  command -v ufw >/dev/null || die "--configure-firewall needs ufw (apt install ufw)"
  # Candidate SSH ports = union of: sshd -T Port lines, listening sshd/systemd (ssh.socket) ports,
  # and the server port of the current SSH session. A socket-activated sshd can listen elsewhere
  # than sshd -T reports, so none of the sources is trusted alone.
  detected=""
  if command -v sshd >/dev/null; then detected+="$(sshd -T 2>/dev/null | awk 'tolower($1)=="port"{print $2}' | tr '\n' ' ') "; fi
  if command -v ss >/dev/null; then
    detected+="$(ss -ltnpH 2>/dev/null | awk '/"sshd"|"systemd"/{n=split($4,a,":"); print a[n]}' | tr '\n' ' ')"
  fi
  det_ports=()
  for dp in $detected; do
    [[ "$dp" =~ ^[1-9][0-9]{0,4}$ ]] && ((dp <= 65535)) || continue
    already=0
    for x in ${det_ports[@]+"${det_ports[@]}"}; do if [[ "$x" == "$dp" ]]; then already=1; fi; done
    ((already)) || det_ports+=("$dp")
  done
  conn_port=""
  if [[ -n "${SSH_CONNECTION:-}" ]]; then
    read -r -a conn_f <<<"$SSH_CONNECTION"
    conn_port="${conn_f[3]:-}"
    [[ "$conn_port" =~ ^[1-9][0-9]{0,4}$ ]] && ((conn_port <= 65535)) || die "cannot parse the server port from SSH_CONNECTION ('$SSH_CONNECTION'); refusing to configure ufw"
    conn_found=0
    for dp in ${det_ports[@]+"${det_ports[@]}"}; do if [[ "$dp" == "$conn_port" ]]; then conn_found=1; fi; done
    ((conn_found)) || die "this SSH session uses server port $conn_port, which is not among the detected sshd ports (${det_ports[*]:-none}); refusing to configure ufw (lockout risk)"
  fi
  confirmed=""
  if [[ -n "$FIREWALL_SSH_PORT_ARG" ]]; then
    [[ "$FIREWALL_SSH_PORT_ARG" =~ ^[1-9][0-9]{0,4}$ ]] && ((FIREWALL_SSH_PORT_ARG <= 65535)) || die "invalid --yes-firewall-ssh-port: $FIREWALL_SSH_PORT_ARG"
    confirmed="$FIREWALL_SSH_PORT_ARG"
  else
    [[ -t 0 ]] || die "--configure-firewall needs the SSH port confirmed: run on a terminal, or pass --yes-firewall-ssh-port=N"
    echo "install-app: ufw will deny all incoming except SSH (detected sshd port(s): ${det_ports[*]:-none}; this session: ${conn_port:-unknown}), 80/tcp and 443/tcp." >&2
    read -r -p "Type the SSH port you are connected through to confirm: " confirmed
    [[ "$confirmed" =~ ^[1-9][0-9]{0,4}$ ]] && ((confirmed <= 65535)) || die "not a valid port: $confirmed"
  fi
  if ((${#det_ports[@]})); then
    found=0
    for dp in "${det_ports[@]}"; do if [[ "$dp" == "$confirmed" ]]; then found=1; fi; done
    ((found)) || die "port $confirmed is not a detected sshd port (${det_ports[*]}); refusing to configure ufw"
    FIREWALL_SSH_PORTS=("${det_ports[@]}")
  else
    echo "install-app: WARNING: could not detect sshd's port; allowing only the confirmed port $confirmed." >&2
    FIREWALL_SSH_PORTS=("$confirmed")
  fi
fi

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
if [[ "$MODE" == public ]]; then tls_kit_file=traefik/traefik-public.yml; else tls_kit_file=traefik/autodeploy-tls.yml; fi
for f in traefik/traefik.yml "$tls_kit_file" traefik/autodeploy-api.yml libexec/autodeploy-run \
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
# checkout group: READ-ONLY access to the finished checkout. The quota helper grants workspaces
# as checkout:checkout 0750 and the git child runs with umask 027, so group members can read but
# never write. `docker buildx build` runs as the agent and sends the workspace as build context;
# with checkout-only access (the earlier 0700) it could not even chdir into it. The checkout
# identity itself must stay out of the agent's own groups (checked by install-checkout.sh).
CHECKOUT_GROUP=autodeploy-checkout
getent group "$CHECKOUT_GROUP" >/dev/null || die "group $CHECKOUT_GROUP not found: run install-checkout.sh first"
log "adding $AGENT_USER to group $CHECKOUT_GROUP (read-only build-context access)"
if ! id -nG "$AGENT_USER" | tr ' ' '\n' | grep -qx "$CHECKOUT_GROUP"; then
  usermod -aG "$CHECKOUT_GROUP" "$AGENT_USER"
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
if [[ "$MODE" == public ]]; then
  # ACME state (holds the account key and certificate keys): root-only, NOT under the agent-writable
  # dynamic/ directory. acme.json must be 0600 (Traefik refuses wider) and is created empty.
  mkdir_as root root 0700 "$ACME_DIR"
  chmod 0700 "$ACME_DIR"
  if [[ ! -e "$ACME_DIR/acme.json" ]]; then (umask 077 && : >"$ACME_DIR/acme.json"); fi
  chown root:root "$ACME_DIR/acme.json"
  chmod 0600 "$ACME_DIR/acme.json"
else
  mkdir_as root root 0755 "$ETC_DIR/tls"
fi
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

# ---- 3b. host firewall (public mode, only with --configure-firewall) ----------------------
# Allow rules first, then default deny incoming, then enable: never a window without SSH.
if ((CONFIGURE_FIREWALL)); then
  log "configuring ufw: ssh ${FIREWALL_SSH_PORTS[*]}, 80/tcp, 443/tcp; default deny incoming"
  # Every allow must succeed before default-deny/enable: a failed allow aborts with ufw untouched.
  for sp in "${FIREWALL_SSH_PORTS[@]}"; do ufw allow "$sp/tcp" comment 'autodeploy: ssh' >/dev/null || die "ufw allow $sp/tcp failed; ufw left unchanged beyond earlier allow rules"; done
  ufw allow 80/tcp comment 'autodeploy: http' >/dev/null || die "ufw allow 80/tcp failed"
  ufw allow 443/tcp comment 'autodeploy: https' >/dev/null || die "ufw allow 443/tcp failed"
  ufw default deny incoming >/dev/null
  ufw --force enable >/dev/null
  echo "install-app: WARNING: ports published by Docker bypass ufw; only the Traefik bind address ($TRAEFIK_BIND) and the loopback-only publications keep other ports closed." >&2
fi

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
  if declare -F check_subnet_collision >/dev/null; then
    check_subnet_collision "$ENV_SUBNET" || die "subnet $ENV_SUBNET collides with an existing route or Docker network; set AUTODEPLOY_ENV_SUBNET/GATEWAY/IP_RANGE/TRAEFIK_IP to a free range and re-run"
  fi
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
render_host_env | put_generated "$ETC_DIR/host.env" TRAEFIK_CHANGED
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
${METRICS_LISTEN:+AUTODEPLOY_METRICS_LISTEN_ADDRESS=$METRICS_LISTEN}
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
if [[ "$MODE" == public ]]; then
  # traefik-public.yml is a template (placeholders @ACME_*@); rendered from the validated flags.
  render_traefik_static "$KIT_DIR/traefik/traefik-public.yml" | put_generated "$ETC_DIR/traefik/traefik.yml" TRAEFIK_CHANGED
else
  if install_file "$KIT_DIR/traefik/traefik.yml" "$ETC_DIR/traefik/traefik.yml" 0644; then mark TRAEFIK_CHANGED; fi
fi
install_file "$KIT_DIR/traefik/autodeploy-api.yml" "$ROUTE_DIR/autodeploy-api.yml" 0644 || true
if [[ "$MODE" != public ]]; then
  install_file "$KIT_DIR/traefik/autodeploy-tls.yml" "$ROUTE_DIR/autodeploy-tls.yml" 0644 || true
fi
cp_route="$(mktemp)"
render_cp_route >"$cp_route"
install_file "$cp_route" "$ROUTE_DIR/autodeploy-controlplane.yml" 0644 || true
rm -f "$cp_route"

if [[ "$MODE" == public ]]; then manage_public_hosts_block; fi

# ---- 6. units -----------------------------------------------------------------------
log "installing units"
for u in "${UNITS[@]}"; do
  unit_src="$KIT_DIR/systemd/$u.service"
  if [[ "$MODE" == public && "$u" == autodeploy-traefik ]]; then
    unit_src="$(mktemp)"
    render_traefik_unit "$KIT_DIR/systemd/$u.service" >"$unit_src"
  fi
  if install_file "$unit_src" "$UNIT_DIR/$u.service" 0644; then
    case "$u" in
      autodeploy-postgres) mark PG_UNIT_CHANGED ;;
      autodeploy-traefik) mark TRAEFIK_CHANGED ;;
      autodeploy-controlplane) mark CP_CHANGED ;;
      autodeploy-agent) mark AGENT_CHANGED ;;
      *) ;;
    esac
  fi
  if [[ "$unit_src" != "$KIT_DIR/systemd/$u.service" ]]; then rm -f "$unit_src"; fi
done
systemctl daemon-reload
for u in "${UNITS[@]}"; do systemctl enable "$u.service" >/dev/null 2>&1; done
# ---- 7. database: credentials, migrate as owner, activate runtime role ------------------
# Binaries from a newer commit can need schema the running database lacks (found on the acer:
# the manual deploy table from migration 000023 was missing). Migration ALWAYS runs before any
# restart, and a restart that is pending while it cannot run is an error, never a silent skip.
restart_pending() {
  ((WAS_TRAEFIK && TRAEFIK_CHANGED)) || ((WAS_CP && (CP_CHANGED || CP_RESTART_DUE))) || ((WAS_AGENT && AGENT_CHANGED))
}
state_report() {
  echo "install-app: host state: binaries, units and configuration are INSTALLED; no service was restarted," >&2
  echo "install-app: so running services still use their previous binaries. Fix the cause, then re-run install-app.sh." >&2
}
# A previous run (or autodeploy-run migrate) switched database-url to the runtime role but may have
# died before restarting the control plane, which then still holds owner-credential connections.
CP_MARKER=/var/lib/autodeploy/controlplane-restart-pending
CP_RESTART_DUE=0
if [[ -e $CP_MARKER ]]; then
  CP_RESTART_DUE=1
  log "restart marker present: the control plane must restart onto the runtime database role"
fi
# Swap marker is written by autodeploy-run migrate before the URL is replaced; re-check after it.
PG_UP=0
if systemctl is-active --quiet autodeploy-postgres.service; then PG_UP=1; fi
if ((PG_UP)); then
  [[ -s "$ETC_DIR/secrets/postgres-password" ]] \
    || { state_report; die "autodeploy-postgres is running but $ETC_DIR/secrets/postgres-password is missing: cannot migrate"; }
  # Idempotent: creates only what is missing (owner URL, runtime password, runtime URL if absent).
  "$KIT_DIR/create-credentials.sh" --database-only || { state_report; die "creating database credentials failed"; }
  # An existing install still has the owner URL in database-url: the running control plane must
  # restart onto the runtime URL after the switch that autodeploy-run migrate performs.
  if grep -q '^postgres://autodeploy:' "$ETC_DIR/controlplane/database-url" 2>/dev/null; then
    log "database-url holds the owner credential: it will be switched to the runtime role"
    mark CP_CHANGED
  fi
  log "applying pending database migrations (as the owner role) and enabling the runtime role"
  "$LIB_DIR/autodeploy-run" migrate || { state_report; die "database migration/role activation failed"; }
  if [[ -e $CP_MARKER ]]; then CP_RESTART_DUE=1; fi
elif restart_pending; then
  state_report
  die "autodeploy-postgres is not running but a service restart is pending: cannot migrate first. Start it (systemctl start autodeploy-postgres), then re-run install-app.sh"
else
  log "autodeploy-postgres is not running: migrations skipped (nothing to restart; migrate after starting it, see below)"
fi

# A running service keeps its old binary/config until restarted. PostgreSQL is left alone.
if ((WAS_TRAEFIK && TRAEFIK_CHANGED)); then log "restarting autodeploy-traefik"; systemctl restart autodeploy-traefik.service; fi
if ((WAS_CP && (CP_CHANGED || CP_RESTART_DUE))); then
  log "restarting autodeploy-controlplane"
  # set -e is active: a failed restart aborts here and the marker is KEPT for the next run.
  systemctl restart autodeploy-controlplane.service
  # Type=simple: restart returning proves nothing. Require active, then still active after a
  # stable window (a crash-looping unit flaps), within a bounded wait. Marker KEPT on failure.
  cp_ok=0; cp_streak=0
  for ((i = 0; i < 30; i++)); do
    if systemctl is-active --quiet autodeploy-controlplane.service; then
      cp_streak=$((cp_streak + 1))
      if ((cp_streak >= 5)); then cp_ok=1; break; fi
    else
      cp_streak=0
    fi
    sleep 1
  done
  ((cp_ok)) || die "autodeploy-controlplane did not stay active for 5s within 30s after restart; restart marker kept ($CP_MARKER); see: journalctl -u autodeploy-controlplane"
  rm -f "$CP_MARKER" # marker this installer/autodeploy-run created; restart verified
elif ((CP_RESTART_DUE)); then
  # Control plane not running: nothing holds old credentials; its next start uses the runtime URL.
  rm -f "$CP_MARKER"
fi
if ((WAS_AGENT && AGENT_CHANGED)); then log "restarting autodeploy-agent"; systemctl restart autodeploy-agent.service; fi
if ((PG_UNIT_CHANGED)); then log "note: the PostgreSQL unit changed; restart it yourself when convenient (systemctl restart autodeploy-postgres)"; fi

if [[ "$MODE" == public ]]; then
  cat <<PUBEOF

==> installed (public mode). Nothing was started and no secret exists yet. Next, as root, in order:

 1. DNS: an A/AAAA record for $CP_HOST (and one per app hostname) must point at this host, and
    80/tcp + 443/tcp must be reachable from the internet (${ACME_CHALLENGE} challenge; ACME staging=$ACME_STAGING). There is no private CA step.
    ACME state (account key, certificates) lives in $ACME_DIR/acme.json: back it up.
 2. Generate every credential file (random values, never printed, never in argv/env):
      sudo $KIT_DIR/create-credentials.sh
 3. Start the database and the proxy, apply migrations, start the control plane:
      sudo systemctl start autodeploy-postgres autodeploy-traefik
      sudo $LIB_DIR/autodeploy-run migrate
      sudo $LIB_DIR/autodeploy-run admin bootstrap --username administrator   # prompts on the terminal
      sudo systemctl start autodeploy-controlplane
 4. Sign in at https://$CP_HOST (certificate requested shortly after the route is loaded; verify on the VPS), register this server, put its
    id in $ETC_DIR/agent-local.env (AUTODEPLOY_AGENT_SERVER_ID=...), then enroll with the one-time token:
      read -rs -p 'enrollment token: ' T; echo
      printf '%s\n' "\$T" | sudo -u $AGENT_USER $APP_DIR/agent enroll \\
        --base-url https://$CP_HOST --server-id <SERVER_ID> --state-dir $AGENT_STATE
      unset T
 5. sudo systemctl start autodeploy-agent
 6. sudo $KIT_DIR/preflight-app.sh            # read-only; reads the mode from $ETC_DIR/host.env

 Note: docker group membership of $AGENT_USER is root-equivalent on this host. Docker-published
 ports bypass ufw; Traefik 80/443 are bound to $TRAEFIK_BIND.
PUBEOF
  exit 0
fi

cat <<NEXTEOF

==> installed. Nothing was started and no secret exists yet. Next, as root, in order:

 1. Private CA, certificate and hosts entries (add one --host per app hostname you will deploy):
      sudo $KIT_DIR/private-ca.sh --host demo.${CP_HOST#*.}
 2. Generate every credential file (random values, never printed, never in argv/env):
      sudo $KIT_DIR/create-credentials.sh
 3. Start the database and the proxy, apply migrations, start the control plane:
      sudo systemctl start autodeploy-postgres autodeploy-traefik
      sudo $LIB_DIR/autodeploy-run migrate   # as the owner role; then enables the runtime role
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
