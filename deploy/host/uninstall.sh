#!/usr/bin/env bash
# AutoDeploy host kit: remove everything install-checkout.sh, install-app.sh,
# create-credentials.sh and private-ca.sh created (steps 18a-1 and 18a-2).
#
# Usage (as root):
#   uninstall.sh --yes [--keep-data]
#
#   --yes        required. Without it the script prints what it would remove and exits.
#   --keep-data  keep what is expensive or irreplaceable, remove everything else:
#                  - the PostgreSQL data volume autodeploy-postgres-data
#                  - /etc/autodeploy/secrets and /etc/autodeploy/controlplane (the database
#                    password and URL, throttle and CSRF keys: without them the kept volume
#                    cannot be reopened)
#                  - the checkout XFS image /var/lib/autodeploy/checkout.xfs and the quota
#                    ledger /var/lib/autodeploy/checkout-quota, including binding.env
#                    there (the ledger binds the checkout UID/GID and loop device
#                    7:200, and binding.env records both, so the image, ledger and
#                    binding belong together; /etc/autodeploy/checkout.env is only
#                    a derived copy and is removed)
#                Users and groups ARE removed; a reinstall recreates the same fixed IDs
#                (800/801/802), so the kept files line up again.
#
# Removes: the units (app, helper, loop device, mount) and their runtime state, the containers
# autodeploy-traefik/-postgres, the environment network and the pinned images, the ufw
# rule, the users and groups, /etc/autodeploy, /usr/local/lib/autodeploy,
# /var/lib/autodeploy, /srv/autodeploy, the loop mount and image, the private CA from the
# system trust store, and the managed /etc/hosts block. It does NOT touch Docker itself,
# xfsprogs, apache2, or application containers/images deployed by the agent (stop those
# through the control plane first; leftovers are listed at the end).
set -uo pipefail

readonly ETC_DIR=/etc/autodeploy
readonly STATE_ROOT=/var/lib/autodeploy
readonly LIB_DIR=/usr/local/lib/autodeploy
readonly MOUNT_POINT=/srv/autodeploy/checkout
readonly MOUNT_UNIT=srv-autodeploy-checkout.mount
readonly IMAGE="$STATE_ROOT/checkout.xfs"
readonly LEDGER_DIR="$STATE_ROOT/checkout-quota"
readonly UNIT_DIR=/etc/systemd/system
readonly APP_UNITS=(autodeploy-agent autodeploy-controlplane autodeploy-traefik autodeploy-postgres)
readonly HELPER_UNITS=(autodeploy-checkout-exec autodeploy-checkout-quota)
readonly LOOP_UNIT=autodeploy-checkout-loop.service
readonly USERS=(autodeploy-agent autodeploy-controlplane autodeploy-checkout)
readonly TRUST_CRT=/usr/local/share/ca-certificates/autodeploy-private-ca.crt
readonly HOSTS_FILE=/etc/hosts
readonly MARK_BEGIN="# BEGIN autodeploy-private-ca (managed by deploy/host/private-ca.sh)"
readonly MARK_END="# END autodeploy-private-ca"
readonly PG_VOLUME=autodeploy-postgres-data

log() { printf '==> %s\n' "$*"; }
warn() { printf 'uninstall: warning: %s\n' "$*" >&2; }
die() { printf 'uninstall: error: %s\n' "$*" >&2; exit 1; }

YES=0
KEEP=0
while (($#)); do
  case "$1" in
    --yes) YES=1 ;;
    --keep-data) KEEP=1 ;;
    -h|--help) sed -n '2,31p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *) die "unknown argument: $1 (see --help)" ;;
  esac
  shift
done
[[ $EUID -eq 0 ]] || die "must run as root"
if ((YES == 0)); then
  printf 'uninstall: refusing to run without --yes. This stops all AutoDeploy services and deletes\n'
  printf 'their users, configuration, credentials, the private CA and %s.\n' "$([[ -e $IMAGE ]] && echo "the checkout image $IMAGE" || echo "(no checkout image found)")"
  printf 'Add --keep-data to preserve the PostgreSQL volume, its credentials and the quota ledger/image.\n'
  exit 2
fi

export DOCKER_HOST=unix:///var/run/docker.sock DOCKER_CONFIG=/nonexistent
unset DOCKER_CONTEXT
have_docker() { command -v docker >/dev/null && docker info >/dev/null 2>&1; }

# The managed /etc/hosts block is edited only if its markers are intact: BEGIN and END each
# exactly once, in order. Checked up front so a damaged file stops the run before anything
# is removed.
hosts_state() { # -> none | ok ; dies on anything else
  local nb ne lb le
  nb="$(grep -cxF "$MARK_BEGIN" "$HOSTS_FILE" 2>/dev/null || true)"
  ne="$(grep -cxF "$MARK_END" "$HOSTS_FILE" 2>/dev/null || true)"
  if [[ "$nb" == 0 && "$ne" == 0 ]]; then echo none; return; fi
  lb="$(grep -nxF "$MARK_BEGIN" "$HOSTS_FILE" | cut -d: -f1)"
  le="$(grep -nxF "$MARK_END" "$HOSTS_FILE" | cut -d: -f1)"
  if [[ "$nb" == 1 && "$ne" == 1 ]] && ((lb < le)); then echo ok; return; fi
  die "$HOSTS_FILE has a damaged autodeploy block (BEGIN x$nb, END x$ne, or out of order); fix it by hand and re-run. Nothing was changed."
}
HOSTS_STATE="$(hosts_state)" || exit 1

# ---- 1. stop and remove units --------------------------------------------------------
log "stopping and disabling services (agent first)"
for u in "${APP_UNITS[@]}" "${HELPER_UNITS[@]}"; do
  systemctl disable --now "$u.service" >/dev/null 2>&1 || true
  rm -f -- "$UNIT_DIR/$u.service"
done
if have_docker; then
  docker rm -f autodeploy-traefik autodeploy-postgres >/dev/null 2>&1 || true
fi
systemctl daemon-reload

# ---- 2. docker objects, firewall -----------------------------------------------------
if have_docker; then
  if [[ -r $ETC_DIR/host.env ]]; then
    NETWORK="$(sed -n 's/^AUTODEPLOY_ENV_NETWORK=//p' "$ETC_DIR/host.env" | head -n1)"
  fi
  NETWORK="${NETWORK:-autodeploy-env}"
  # Application containers the agent attached are not removed here; the network cannot be
  # deleted while any is attached.
  if docker network inspect "$NETWORK" >/dev/null 2>&1; then
    log "removing Docker network $NETWORK"
    docker network rm "$NETWORK" >/dev/null 2>&1 || warn "network $NETWORK is still in use (application containers attached?); remove them and run: docker network rm $NETWORK"
  fi
  if ((KEEP)); then
    log "keeping Docker volume $PG_VOLUME"
  else
    docker volume rm "$PG_VOLUME" >/dev/null 2>&1 || true
    # Images pinned by the units; ignore failure when something else still uses them.
    docker image rm "postgres:18.6@sha256:e3d24f6906d0760bf4da0dae872415391ededefad23fd471c47dc53b0ddf3c42" >/dev/null 2>&1 || true
    docker image rm "traefik:v3.7.18@sha256:9c437ac4f09ce6f4980a8544902d3475fdf70a1a7c578729581d2535dfa7d635" >/dev/null 2>&1 || true
  fi
else
  warn "Docker Engine not reachable: containers, network, volume and images were NOT removed"
fi
if command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q '^Status: active'; then
  # Rules added by install-app.sh carry the comment "autodeploy: traefik" (the older kit's
  # wider rule had "autodeploy: traefik to control plane"; the prefix matches both).
  while read -r n; do
    if [[ -n "$n" ]]; then ufw --force delete "$n" >/dev/null 2>&1; fi
  done < <(ufw status numbered | sed -n 's/^\[ *\([0-9][0-9]*\)\].*# autodeploy: traefik.*/\1/p' | sort -rn)
fi

# ---- 3. private CA and hosts ---------------------------------------------------------
if [[ -e $TRUST_CRT ]]; then
  log "removing the private CA from the system trust store"
  rm -f -- "$TRUST_CRT"
  update-ca-certificates >/dev/null 2>&1 || warn "update-ca-certificates failed"
fi
if [[ "$HOSTS_STATE" == ok ]]; then
  log "removing the managed block from $HOSTS_FILE"
  [[ -e "$HOSTS_FILE.autodeploy.bak" ]] || cp -p "$HOSTS_FILE" "$HOSTS_FILE.autodeploy.bak"
  tmp="$(mktemp "$HOSTS_FILE.XXXXXX")"
  awk -v b="$MARK_BEGIN" -v e="$MARK_END" '$0==b{skip=1} !skip{print} $0==e{skip=0}' "$HOSTS_FILE" >"$tmp"
  chmod --reference="$HOSTS_FILE" "$tmp"
  chown --reference="$HOSTS_FILE" "$tmp"
  mv -f "$tmp" "$HOSTS_FILE"
fi

# ---- 4. loop mount and image ---------------------------------------------------------
# Units (and their cgroups/sockets) are gone, so nothing holds the volume. The mount unit
# file is removed in every case; the image only without --keep-data.
log "unmounting the checkout volume and detaching its loop device"
systemctl disable --now "$MOUNT_UNIT" >/dev/null 2>&1 || true
if mountpoint -q "$MOUNT_POINT"; then umount "$MOUNT_POINT" || warn "could not unmount $MOUNT_POINT (busy?)"; fi
# The loop unit detaches /dev/loop200 on stop, strictly after the mount (Requires=/After=).
systemctl disable --now "$LOOP_UNIT" >/dev/null 2>&1 || true
rm -f -- "$UNIT_DIR/$MOUNT_UNIT" "$UNIT_DIR/$LOOP_UNIT"
systemctl daemon-reload
if [[ -e $IMAGE ]]; then
  while read -r dev; do
    if [[ -n "$dev" ]]; then losetup -d "$dev" 2>/dev/null || true; fi
  done < <(losetup -j "$IMAGE" 2>/dev/null | cut -d: -f1)
fi
if mountpoint -q "$MOUNT_POINT"; then die "$MOUNT_POINT is still mounted; refusing to delete anything under /srv or $STATE_ROOT"; fi
if ((KEEP)); then
  log "keeping $IMAGE and $LEDGER_DIR"
else
  rm -f -- "$IMAGE"
fi

# ---- 5. users and groups -------------------------------------------------------------
for u in "${USERS[@]}"; do
  if id -u "$u" >/dev/null 2>&1; then
    log "removing user $u"
    userdel "$u" 2>/dev/null || warn "could not remove user $u (still has processes?)"
  fi
  getent group "$u" >/dev/null && groupdel "$u" 2>/dev/null
done

# ---- 6. directories ------------------------------------------------------------------
log "removing directories"
rm -rf -- "${LIB_DIR:?}"
rm -rf -- /run/autodeploy-checkout-quota
rmdir "$MOUNT_POINT" 2>/dev/null || true
rmdir /srv/autodeploy 2>/dev/null || true
if ((KEEP)); then
  # Remove everything under /etc/autodeploy and /var/lib/autodeploy except the kept items.
  find "$ETC_DIR" -mindepth 1 -maxdepth 1 ! -name secrets ! -name controlplane -exec rm -rf -- {} +
  find "$STATE_ROOT" -mindepth 1 -maxdepth 1 ! -name checkout.xfs ! -name checkout-quota -exec rm -rf -- {} +
  log "kept: $ETC_DIR/secrets $ETC_DIR/controlplane $IMAGE $LEDGER_DIR (and Docker volume $PG_VOLUME)"
else
  rm -rf -- "${ETC_DIR:?}" "${STATE_ROOT:?}"
fi

# ---- 7. report leftovers -------------------------------------------------------------
log "done."
if have_docker; then
  left="$(docker ps -a --filter 'name=^autodeploy-rt-' --format '{{.Names}}' 2>/dev/null | tr '\n' ' ')"
  [[ -z "$left" ]] || warn "application containers created by the agent remain (not touched): $left"
fi
exit 0
