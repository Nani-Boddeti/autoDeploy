#!/usr/bin/env bash
# AutoDeploy host kit, step 18a-2: private certificate authority for a local test
# domain, so the whole TLS path (Traefik serving, the agent's HTTPS probe, the
# agent's HTTPS calls to the control plane) works without public DNS or ACME.
#
# Usage (as root):
#   private-ca.sh [--domain DOMAIN] [--host NAME]... [--days N] [--force]
#
#   --domain   test domain (default autodeploy.test). The CA is name-constrained to
#              it, and the server certificate covers DOMAIN and *.DOMAIN (one label:
#              app.DOMAIN, autodeploy.DOMAIN; not a.b.DOMAIN).
#   --host     extra NAME to map to 127.0.0.1 in /etc/hosts (repeatable). The default
#              mapping is autodeploy.DOMAIN (the control plane). Every NAME must be
#              inside DOMAIN. /etc/hosts has no wildcards: add one per deployed app.
#   --days     server certificate lifetime (default 397). Re-run to renew.
#   --force    reissue the server certificate even if it is still valid.
#
# What it does (idempotent):
#   /etc/autodeploy/ca/ca.key        root 0600  CA private key (never leaves this dir,
#                                               never mounted into a container)
#   /etc/autodeploy/ca/ca.crt        root 0644  CA certificate (ECDSA P-256, 10 years,
#                                               CA:TRUE pathlen:0, name-constrained)
#   /etc/autodeploy/tls/server.{crt,key}        server certificate (0644) and key (0600),
#                                               mounted read-only into Traefik
#   /usr/local/share/ca-certificates/autodeploy-private-ca.crt + update-ca-certificates
#                                    -> system trust store; Go uses the system roots, so the
#                                       agent's probe (internal/runtime/traefik/probe.go,
#                                       RootCAs nil) and control-plane client trust it
#   /etc/hosts                       managed block "BEGIN/END autodeploy-private-ca"
# Browsers on Linux do not read the system store (Chrome: NSS, Firefox: own); import
# /etc/autodeploy/ca/ca.crt there by hand if you browse the control plane.
#
# The probe does not need DNS: it dials --traefik-https-addr (127.0.0.1:443) with
# SNI and verification name = the route hostname, whatever that name resolves to.
# DNS/hosts entries matter only for the agent -> control plane URL and for browsers.
set -euo pipefail

readonly CA_DIR=/etc/autodeploy/ca
readonly TLS_DIR=/etc/autodeploy/tls
readonly CA_KEY="$CA_DIR/ca.key"
readonly CA_CRT="$CA_DIR/ca.crt"
readonly CA_DOMAIN_FILE="$CA_DIR/domain"
readonly SRV_KEY="$TLS_DIR/server.key"
readonly SRV_CRT="$TLS_DIR/server.crt"
readonly TRUST_CRT=/usr/local/share/ca-certificates/autodeploy-private-ca.crt
readonly HOSTS_FILE="${AUTODEPLOY_HOSTS_FILE:-/etc/hosts}"
readonly MARK_BEGIN="# BEGIN autodeploy-private-ca (managed by deploy/host/private-ca.sh)"
readonly MARK_END="# END autodeploy-private-ca"

log() { printf '==> %s\n' "$*"; }
die() { printf 'private-ca: error: %s\n' "$*" >&2; exit 1; }

DOMAIN=autodeploy.test
DAYS=397
FORCE=0
EXTRA_HOSTS=()
while (($#)); do
  case "$1" in
    --domain) DOMAIN="${2:?--domain needs a value}"; shift 2 ;;
    --host) EXTRA_HOSTS+=("${2:?--host needs a value}"); shift 2 ;;
    --days) DAYS="${2:?--days needs a value}"; shift 2 ;;
    --force) FORCE=1; shift ;;
    -h|--help) sed -n '2,36p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done

[[ $EUID -eq 0 ]] || die "must run as root"
command -v openssl >/dev/null || die "openssl required"
label='[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?'
[[ "$DOMAIN" =~ ^($label\.)+$label$ ]] || die "invalid domain: $DOMAIN (lowercase DNS name with at least two labels)"
[[ "$DAYS" =~ ^[1-9][0-9]*$ ]] || die "--days must be 1..825"
((DAYS <= 825)) || die "--days must be 1..825"
for h in "${EXTRA_HOSTS[@]}"; do
  [[ "$h" =~ ^($label\.)+$label$ && "$h" == *".$DOMAIN" ]] || die "--host $h must be a lowercase name inside $DOMAIN"
done
HOSTS=("autodeploy.$DOMAIN" "${EXTRA_HOSTS[@]}")

install -d -o root -g root -m 0755 /etc/autodeploy "$TLS_DIR"
install -d -o root -g root -m 0700 "$CA_DIR"
# Scratch space (CA and server private keys are generated here) lives in the root-only
# CA directory, not in a shared /tmp.
work="$(mktemp -d "$CA_DIR/work.XXXXXX")"
trap 'rm -rf -- "$work"' EXIT

# ---- CA --------------------------------------------------------------------------
if [[ -e "$CA_KEY" || -e "$CA_CRT" ]]; then
  [[ -f "$CA_KEY" && -f "$CA_CRT" && ! -L "$CA_KEY" && ! -L "$CA_CRT" ]] || die "$CA_DIR holds an incomplete or unexpected CA; refusing to continue"
  have_domain="$(cat "$CA_DOMAIN_FILE" 2>/dev/null || true)"
  [[ "$have_domain" == "$DOMAIN" ]] || die "existing CA is constrained to '${have_domain:-unknown}', not '$DOMAIN'. Remove $CA_DIR (and re-run) to start a new CA."
  openssl x509 -in "$CA_CRT" -noout -checkend $((30 * 86400)) >/dev/null || die "CA certificate expires within 30 days; replace the CA deliberately (remove $CA_DIR)"
  log "CA exists for $DOMAIN; keeping it"
else
  log "creating CA for $DOMAIN (ECDSA P-256, 10 years, name-constrained)"
  cat >"$work/ca.cnf" <<CNF
[req]
distinguished_name = dn
x509_extensions = v3_ca
prompt = no
[dn]
CN = AutoDeploy private CA ($DOMAIN)
[v3_ca]
basicConstraints = critical,CA:TRUE,pathlen:0
keyUsage = critical,keyCertSign,cRLSign
subjectKeyIdentifier = hash
nameConstraints = critical,permitted;DNS:$DOMAIN
CNF
  (umask 077 && openssl ecparam -name prime256v1 -genkey -noout -out "$work/ca.key")
  openssl req -new -x509 -key "$work/ca.key" -days 3650 -sha256 -config "$work/ca.cnf" -out "$work/ca.crt"
  install -o root -g root -m 0600 "$work/ca.key" "$CA_KEY"
  install -o root -g root -m 0644 "$work/ca.crt" "$CA_CRT"
  printf '%s\n' "$DOMAIN" >"$CA_DOMAIN_FILE"
  chmod 0644 "$CA_DOMAIN_FILE"
fi

# ---- system trust store ----------------------------------------------------------
command -v update-ca-certificates >/dev/null || die "update-ca-certificates not found (apt-get install ca-certificates)"
if ! cmp -s "$CA_CRT" "$TRUST_CRT" 2>/dev/null; then
  log "installing the CA into the system trust store"
  install -d -m 0755 "$(dirname "$TRUST_CRT")"
  install -o root -g root -m 0644 "$CA_CRT" "$TRUST_CRT"
  update-ca-certificates >/dev/null
else
  log "CA already in the system trust store"
fi

# ---- server certificate ----------------------------------------------------------
want_san="DNS:*.$DOMAIN, DNS:$DOMAIN"
need_cert=1
CERT_RENEWED=0
if ((FORCE == 0)) && [[ -f "$SRV_CRT" && -f "$SRV_KEY" ]]; then
  have_san="$(openssl x509 -in "$SRV_CRT" -noout -ext subjectAltName 2>/dev/null | sed -n '2p' | tr -d ' ')"
  if [[ "$have_san" == "$(tr -d ' ' <<<"$want_san")" ]] \
    && openssl x509 -in "$SRV_CRT" -noout -checkend $((30 * 86400)) >/dev/null \
    && openssl verify -CAfile "$CA_CRT" "$SRV_CRT" >/dev/null 2>&1; then
    need_cert=0
  fi
fi
if ((need_cert)); then
  log "issuing server certificate for $want_san ($DAYS days)"
  cat >"$work/srv.cnf" <<CNF
[req]
distinguished_name = dn
prompt = no
[dn]
CN = *.$DOMAIN
[v3_srv]
basicConstraints = critical,CA:FALSE
keyUsage = critical,digitalSignature
extendedKeyUsage = serverAuth
subjectAltName = $want_san
subjectKeyIdentifier = hash
authorityKeyIdentifier = keyid
CNF
  (umask 077 && openssl ecparam -name prime256v1 -genkey -noout -out "$work/srv.key")
  openssl req -new -key "$work/srv.key" -config "$work/srv.cnf" -out "$work/srv.csr"
  openssl x509 -req -in "$work/srv.csr" -CA "$CA_CRT" -CAkey "$CA_KEY" -CAcreateserial \
    -CAserial "$work/ca.srl" -days "$DAYS" -sha256 -extfile "$work/srv.cnf" -extensions v3_srv \
    -out "$work/srv.crt" 2>/dev/null
  openssl verify -CAfile "$CA_CRT" "$work/srv.crt" >/dev/null || die "issued certificate does not verify against the CA"
  # Both new files are fully written beside the targets first, then renamed into place
  # key first, certificate second (each rename is atomic; the pair is not, hence the
  # Traefik restart below).
  new_key="$(mktemp "$SRV_KEY.XXXXXX")"
  new_crt="$(mktemp "$SRV_CRT.XXXXXX")"
  install -o root -g root -m 0600 "$work/srv.key" "$new_key"
  install -o root -g root -m 0644 "$work/srv.crt" "$new_crt"
  mv -f "$new_key" "$SRV_KEY"
  mv -f "$new_crt" "$SRV_CRT"
  CERT_RENEWED=1
else
  log "server certificate is valid for $want_san; keeping it (use --force to reissue)"
fi

# Traefik reads the certificate files at startup and does not watch them (only its dynamic
# configuration files are watched), so a renewed pair takes effect after a restart.
if ((CERT_RENEWED)) && command -v systemctl >/dev/null && systemctl is-active --quiet autodeploy-traefik.service; then
  log "restarting autodeploy-traefik to load the renewed certificate"
  systemctl restart autodeploy-traefik.service
fi

# ---- /etc/hosts ------------------------------------------------------------------
# Rewritten only if the managed block is intact: BEGIN and END each exactly once and in
# order, or both absent (first run: the block is appended). Anything else is refused.
block="$MARK_BEGIN"$'\n'
for h in "${HOSTS[@]}"; do block+="127.0.0.1 $h"$'\n'; done
block+="$MARK_END"
nb="$(grep -cxF "$MARK_BEGIN" "$HOSTS_FILE" || true)"
ne="$(grep -cxF "$MARK_END" "$HOSTS_FILE" || true)"
if [[ "$nb" == 1 && "$ne" == 1 ]]; then
  [[ "$(grep -nxF "$MARK_BEGIN" "$HOSTS_FILE" | cut -d: -f1)" -lt "$(grep -nxF "$MARK_END" "$HOSTS_FILE" | cut -d: -f1)" ]] \
    || die "$HOSTS_FILE: the autodeploy END marker precedes the BEGIN marker; fix it by hand and re-run. Nothing was changed."
elif [[ "$nb" != 0 || "$ne" != 0 ]]; then
  die "$HOSTS_FILE has a damaged autodeploy block (BEGIN x$nb, END x$ne; each must appear exactly once); fix it by hand and re-run. Nothing was changed."
fi
current="$(awk -v b="$MARK_BEGIN" -v e="$MARK_END" '$0==b{p=1} p{print} $0==e{p=0}' "$HOSTS_FILE")"
if [[ "$current" == "$block" ]]; then
  log "$HOSTS_FILE already maps: ${HOSTS[*]}"
else
  log "updating the managed block in $HOSTS_FILE: ${HOSTS[*]}"
  [[ -e "$HOSTS_FILE.autodeploy.bak" ]] || cp -p "$HOSTS_FILE" "$HOSTS_FILE.autodeploy.bak"
  tmp="$(mktemp "$HOSTS_FILE.XXXXXX")"
  awk -v b="$MARK_BEGIN" -v e="$MARK_END" '$0==b{skip=1} !skip{print} $0==e{skip=0}' "$HOSTS_FILE" >"$tmp"
  printf '%s\n' "$block" >>"$tmp"
  chmod --reference="$HOSTS_FILE" "$tmp"
  chown --reference="$HOSTS_FILE" "$tmp"
  mv -f "$tmp" "$HOSTS_FILE"
fi

log "done. CA: $CA_CRT   server cert: $SRV_CRT"
log "Browsers: import $CA_CRT manually. A renewed server certificate is loaded by restarting Traefik (done above when it was running)."
