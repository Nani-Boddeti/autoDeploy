#!/usr/bin/env bash
# AutoDeploy bootstrap installer (plan 24-B): one host, Ubuntu 24.04 x86_64, public mode (Let's Encrypt).
#
# Fetch it from the git TAG ref (NOT from a release asset), read it, then run it:
#   download:  https://raw.githubusercontent.com/Nani-Boddeti/autoDeploy/<tag>/deploy/host/install.sh
#   run:       sudo bash install.sh
# Do not pipe it into a shell: prompts are read from the terminal, never from a pipe.
#
# Trust: this file carries the release public key (embedded below) and the release verification logic.
# It downloads the signed release assets of ITS OWN version, verifies signature + SHA256SUMS + version
# binding in a private root-only directory, and only then extracts and runs the host kit from it.
# The key fingerprint is printed so you can compare it with the one published in the README.
#
# Flags (every prompt has one, so the installer also runs unattended):
#   --domain FQDN            control plane hostname; its DNS must point at this host
#   --email ADDR|none        Let's Encrypt account / expiry-notice address
#   --firewall yes|no        configure ufw (SSH, 80, 443; deny the rest). Fresh installs only.
#   --ssh-port N             the SSH port you are connected through (required with --firewall yes
#                            when there is no terminal)
#   --acme-staging           Let's Encrypt STAGING (manual dry run only; deploys will not pass probes)
#   --yes                    continue past warnings (DNS mismatch) and skip the final confirmation
#   --setup-wait DURATION    how long to wait for the first-run browser setup (default 60m)
#   --allow-downgrade        install an older version over a newer installed one
#   --release-dir DIR        use release assets from a local directory (offline install); verified the same way
#   --allow-dev              TESTING ONLY: accept a development build (version contains -dev)
#   --allowed-signers FILE   TESTING ONLY (needs --allow-dev): trust the keys in FILE instead of the embedded key
#   --yes-i-am-testing       TESTING ONLY: the non-interactive form of the typed confirmation a development build asks for
# The three testing flags are accepted ONLY by development builds. On a release version (the one fetched from a tag)
# they are hard errors, so a social-engineered "--allow-dev --allowed-signers attacker.pub" cannot replace the trust anchor.
# Exit codes: 0 done, 1 error/refusal, 2 usage, 4 installed but browser setup not finished (rerun this installer).
#
# Choices (domain, email, staging, firewall, ssh port) are saved in /etc/autodeploy/installer.env (root 0600) once you
# confirm them, so a re-run after a partial install keeps them. Exit 4 still records the installed version: the stack
# is installed and only the browser setup is pending (intentional).
# Re-running is an upgrade: the setup code is skipped when an administrator exists, enrollment when this host
# is already enrolled. lan-mode hosts and a different domain are refused.
#
# EXTRA SERVER (plan 24-F): an agent-only server for an existing control plane. On the Servers page choose "Add
# server"; it shows the two commands (download this installer from the tag, then run it):
#   sudo bash install.sh --role agent --control-plane-host CP.EXAMPLE.COM --server-id server_XXXX [--email ADDR|none] ...
#   --role all|agent           all (default) = this installer as above; agent = agent, Traefik (ACME for the apps),
#                              checkout isolation, Docker. No PostgreSQL, control plane, setup code or /etc/hosts block.
#   --control-plane-host FQDN  (agent) the control plane's public hostname; its https://FQDN/readyz must answer with a
#                              certificate the system trusts. Not a DNS check against THIS host.
#   --server-id ID             (agent) the id the Servers page shows (server_...)
#   --token-file FILE          (agent, no terminal) file holding the one-time enrollment token: a regular file owned by
#                              root, mode 0600, not a symlink. Without it the installer asks for the token at a HIDDEN
#                              prompt on /dev/tty. The token is never accepted on the command line, an environment
#                              variable or a URL; it goes into a private 0600 copy readable only by the agent user,
#                              which is removed on success and on failure.
# The agent role takes --email, --firewall, --ssh-port, --acme-staging and --yes like the default role (the apps on
# this server get certificates for their own hostnames); --domain and --setup-wait are refused. A re-run of an
# enrolled server does not ask for the token. A different control plane host, server id or role is refused.
set -euo pipefail
umask 077

# The release workflow checks this exact line (bare, column 0, one occurrence) against the pushed tag.
AUTODEPLOY_VERSION="v0.0.0-dev"

readonly REPO_SLUG="Nani-Boddeti/autoDeploy"
readonly ETC_DIR=/etc/autodeploy
readonly VERSION_FILE="$ETC_DIR/installed-version"
readonly LOG=/var/log/autodeploy-install.log
readonly RUN=/usr/local/lib/autodeploy/autodeploy-run
readonly AGENT_CREDENTIAL=/var/lib/autodeploy/agent/credential
readonly KIT_COPY=/usr/local/share/autodeploy/kit
readonly AGENT_USER=autodeploy-agent
readonly APP_DIR=/usr/local/lib/autodeploy/app
readonly AGENT_STATE=/var/lib/autodeploy/agent
readonly AGENT_LOCAL_ENV="$ETC_DIR/agent-local.env"
readonly WORK_PARENT=/var/tmp

# Original stdout, kept for the one-time setup code: it goes to the terminal only, never through the log tee.
exec 3>&1

LOG_READY=0
WORK=""
TOKDIR="" REC_TMP="" SECRET=""
say() { printf '==> %s\n' "$*"; if ((LOG_READY)); then printf '==> %s\n' "$*" >>"$LOG"; fi; }
warn() { printf 'install.sh: WARNING: %s\n' "$*" >&2; if ((LOG_READY)); then printf 'WARNING: %s\n' "$*" >>"$LOG"; fi; }
die() {
  printf 'install.sh: error: %s\n' "$*" >&2
  if ((LOG_READY)); then printf 'ERROR: %s\n' "$*" >>"$LOG"; fi
  exit 1
}
usage_die() { printf 'install.sh: %s\n' "$*" >&2; printf 'See the header of install.sh for the flags (sed -n 2,30p install.sh).\n' >&2; exit 2; }

cleanup() {
  SECRET=""
  # The token copy (24-F): removed on every exit path, success or failure. Only this run's own directory.
  case "$TOKDIR" in /run/autodeploy-enroll.*) [[ -d $TOKDIR && ! -L $TOKDIR ]] && rm -rf -- "$TOKDIR" ;; esac
  [[ -z $REC_TMP ]] || rm -f -- "$REC_TMP"
  case "$WORK" in "$WORK_PARENT"/autodeploy-install.*) [[ -d $WORK ]] && rm -rf -- "$WORK" ;; esac
  return 0
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# ---- embedded trust anchor ---------------------------------------------------------------------
# Must byte-equal deploy/host/release-signing/allowed_signers (checked by tests/install_sh_test.sh in CI).
embedded_allowed_signers() {
  cat <<'ALLOWED_SIGNERS_EOF'
# Public key(s) that may sign autodeploy releases (ssh-keygen -Y verify "allowed signers" format).
# PLACEHOLDER: the owner replaces the line below with the real public key
# (docs/operations/releases.md, "One-time owner setup"). verify-release.sh REFUSES to verify while
# the word PLACEHOLDER appears anywhere in this file.
release@autodeploy namespaces="autodeploy-release" ssh-ed25519 PLACEHOLDER-REPLACE-WITH-REAL-PUBLIC-KEY
ALLOWED_SIGNERS_EOF
}

# ---- embedded copy of deploy/host/verify-release.sh logic ----------------------------------------
# Keep in sync with verify-release.sh; tests/install_sh_test.sh compares both on the same fixtures.
# verify_release_dir DIR ALLOWED_SIGNERS_FILE EXPECTED_VERSION  (runs in a subshell; 0 = verified)
# BEGIN VERIFY-RELEASE LOGIC
verify_release_dir() (
  set -euo pipefail
  readonly PRINCIPAL="release@autodeploy"
  readonly NAMESPACE="autodeploy-release"
  die() { echo "verify-release: $*" >&2; exit 1; }
  dir="$1" allowed="$2" expect="$3"
  command -v ssh-keygen >/dev/null 2>&1 || die "ssh-keygen not found (install openssh-client)"
  debug=0; [[ "${VERIFY_DEBUG:-}" == 1 ]] && debug=1

  # 1. directory trust
  [[ -d "$dir" && ! -L "$dir" ]] || die "$dir is not a directory"
  dir_info="$(stat -c '%u %a' "$dir" 2>/dev/null || stat -f '%u %Lp' "$dir")"
  dir_uid="${dir_info%% *}"; dir_mode="${dir_info##* }"
  [[ "$dir_uid" == 0 || "$dir_uid" == "$(id -u)" ]] || die "$dir is owned by uid $dir_uid (not root or the current user): refusing"
  mode_oct=$((8#$dir_mode))
  ((mode_oct & 8#022)) && die "$dir is group- or world-writable (mode $dir_mode): refusing; use a private directory"

  # 2. trust anchor
  [[ -f "$allowed" && ! -L "$allowed" ]] || die "allowed_signers $allowed is not a regular file"
  if grep -q 'PLACEHOLDER' "$allowed"; then
    die "$allowed still contains the PLACEHOLDER key: the owner must install the real release public key first (docs/operations/releases.md)"
  fi
  grep -q -v -e '^[[:space:]]*#' -e '^[[:space:]]*$' "$allowed" || die "$allowed contains no keys"

  sums="$dir/SHA256SUMS"
  sig="$dir/SHA256SUMS.sig"
  [[ -f "$sums" && ! -L "$sums" ]] || die "SHA256SUMS missing or not a regular file in $dir"
  [[ -f "$sig" && ! -L "$sig" ]] || die "SHA256SUMS.sig missing or not a regular file in $dir: refusing an unsigned release"

  # 3. snapshot once; verify and parse only the copy
  snap="$(mktemp -d "${TMPDIR:-/tmp}/verify-release.XXXXXX")"
  trap 'rm -rf -- "$snap"' EXIT
  chmod 0700 "$snap"
  cp -- "$sums" "$snap/SHA256SUMS"
  cp -- "$sig" "$snap/SHA256SUMS.sig"
  ((debug)) && echo "verify-release: debug: verifying private snapshot $snap" >&2

  exec 3>/dev/null
  ((debug)) && exec 3>&2
  ssh-keygen -Y verify -f "$allowed" -I "$PRINCIPAL" -n "$NAMESPACE" -s "$snap/SHA256SUMS.sig" <"$snap/SHA256SUMS" >/dev/null 2>&3 \
    || die "signature verification FAILED (wrong key, wrong namespace, or SHA256SUMS was altered)"

  # 4. the signed copy is trusted: parse strictly
  listed=()
  while IFS= read -r line || [[ -n "$line" ]]; do
    [[ "$line" =~ ^[0-9a-f]{64}\ \ ([A-Za-z0-9][A-Za-z0-9._+-]*)$ ]] || die "malformed SHA256SUMS line (want '<sha256>  <basename>')"
    name="${BASH_REMATCH[1]}"
    [[ "$name" != SHA256SUMS && "$name" != SHA256SUMS.sig ]] || die "SHA256SUMS must not list itself or its signature"
    for seen in ${listed[@]+"${listed[@]}"}; do [[ "$seen" != "$name" ]] || die "SHA256SUMS lists $name twice"; done
    listed+=("$name")
  done <"$snap/SHA256SUMS"
  ((${#listed[@]} > 0)) || die "SHA256SUMS lists no files"

  for name in "${listed[@]}"; do
    [[ -f "$dir/$name" && ! -L "$dir/$name" ]] || die "listed file $name is missing or not a regular file"
  done

  # 5. everything in DIR must be accounted for
  shopt -s dotglob nullglob
  for p in "$dir"/*; do
    base="${p##*/}"
    [[ "$base" == SHA256SUMS || "$base" == SHA256SUMS.sig ]] && continue
    [[ ! -L "$p" ]] || die "$base is a symlink: refusing"
    [[ -f "$p" ]] || die "$base is not a regular file: refusing"
    found=0
    for name in "${listed[@]}"; do [[ "$name" == "$base" ]] && { found=1; break; }; done
    ((found)) || die "$base is not listed in SHA256SUMS: refusing an unlisted file"
  done
  shopt -u dotglob nullglob

  # 6. hashes against the signed copy, then the version binding
  if command -v sha256sum >/dev/null 2>&1; then
    (cd "$dir" && sha256sum --check --strict --quiet "$snap/SHA256SUMS") >/dev/null 2>&1 || die "checksum mismatch"
  else
    (cd "$dir" && shasum -a 256 -c --quiet "$snap/SHA256SUMS") >/dev/null 2>&1 || die "checksum mismatch"
  fi
  has_version=0
  for name in "${listed[@]}"; do [[ "$name" == VERSION ]] && has_version=1; done
  ((has_version)) || die "VERSION asset is not listed in the signed SHA256SUMS: refusing a release without a version binding"
  got="$(cat -- "$dir/VERSION")"
  [[ "$got" == "$expect" ]] || die "version mismatch: release says '$got', expected '$expect'"

  echo "verify-release: OK: signature valid, version $got, ${#listed[@]} file(s) match SHA256SUMS in $dir"
)
# END VERIFY-RELEASE LOGIC

# ---- arguments -----------------------------------------------------------------------------------
DOMAIN="" EMAIL="" FIREWALL="" SSH_PORT="" ACME_STAGING="" ASSUME_YES=0 SETUP_WAIT=60m SETUP_WAIT_GIVEN=0
ALLOW_DOWNGRADE=0 ALLOW_DEV=0 ALLOWED_FILE="" RELEASE_DIR="" TESTING_OK=0
ROLE=all CP_HOST_FLAG="" SERVER_ID="" TOKEN_FILE=""
while (($#)); do
  case "$1" in
    --domain | --email | --firewall | --ssh-port | --setup-wait | --release-dir | --allowed-signers \
      | --role | --control-plane-host | --server-id | --token-file)
      (($# >= 2)) || usage_die "$1 needs a value"
      case "$1" in
        --domain) DOMAIN="$2" ;; --email) EMAIL="$2" ;; --firewall) FIREWALL="$2" ;; --ssh-port) SSH_PORT="$2" ;;
        --setup-wait) SETUP_WAIT="$2"; SETUP_WAIT_GIVEN=1 ;; --release-dir) RELEASE_DIR="$2" ;; --allowed-signers) ALLOWED_FILE="$2" ;;
        --role) ROLE="$2" ;; --control-plane-host) CP_HOST_FLAG="$2" ;; --server-id) SERVER_ID="$2" ;; --token-file) TOKEN_FILE="$2" ;;
      esac
      shift 2 ;;
    --acme-staging) ACME_STAGING=1; shift ;;
    --yes) ASSUME_YES=1; shift ;;
    --allow-downgrade) ALLOW_DOWNGRADE=1; shift ;;
    --allow-dev) ALLOW_DEV=1; shift ;;
    --yes-i-am-testing) TESTING_OK=1; shift ;;
    -h | --help) sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed '$d' | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) usage_die "unknown argument: $1" ;;
  esac
done
[[ -z $FIREWALL || $FIREWALL == yes || $FIREWALL == no ]] || usage_die "--firewall must be yes or no"
[[ -z $SSH_PORT || ( $SSH_PORT =~ ^[1-9][0-9]{0,4}$ && $SSH_PORT -le 65535 ) ]] || usage_die "--ssh-port must be a port number"
[[ $SETUP_WAIT =~ ^[0-9]+(ns|us|ms|s|m|h)([0-9]+(ns|us|ms|s|m|h))*$ ]] || usage_die "--setup-wait must be a duration such as 30m"
[[ $ROLE == all || $ROLE == agent ]] || usage_die "--role must be all or agent"
if [[ $ROLE == agent ]]; then
  [[ -n $CP_HOST_FLAG ]] || usage_die "--role agent needs --control-plane-host FQDN (the control plane's public hostname)"
  [[ -n $SERVER_ID ]] || usage_die "--role agent needs --server-id ID (the id the Servers page shows for this server)"
  [[ -z $DOMAIN ]] || usage_die "--domain is for the control plane (--role all); an agent server names its control plane with --control-plane-host"
  ((!SETUP_WAIT_GIVEN)) || usage_die "--setup-wait belongs to the control plane's first-run setup and has no meaning with --role agent"
  DOMAIN="$CP_HOST_FLAG" # from here on DOMAIN is the control plane host the agent reports to
elif [[ -n $CP_HOST_FLAG || -n $SERVER_ID || -n $TOKEN_FILE ]]; then
  usage_die "--control-plane-host, --server-id and --token-file are only valid with --role agent"
fi

# ---- preflight (nothing is changed below this line until the release is verified) ----------------
[[ $EUID -eq 0 ]] || die "must run as root: use 'sudo bash install.sh'"
umask 077
# Fixed environment: nothing inherited from the caller decides which binaries run.
unset BASH_ENV ENV CDPATH
export LC_ALL=C
# Test hook for stubbed commands: honoured ONLY by development builds (version contains -dev) and only when
# AUTODEPLOY_INSTALL_TEST_PATH is set; a release build ignores it. Never set it for a real install.
if [[ $AUTODEPLOY_VERSION == *-dev* && -n ${AUTODEPLOY_INSTALL_TEST_PATH:-} ]]; then
  PATH="$AUTODEPLOY_INSTALL_TEST_PATH:/usr/sbin:/usr/bin:/sbin:/bin"
else
  PATH=/usr/sbin:/usr/bin:/sbin:/bin
fi
export PATH
[[ ! -L $LOG ]] || die "$LOG is a symlink: refusing"
: >>"$LOG"
chmod 0600 "$LOG"
LOG_READY=1
printf '=== install.sh %s started %s\n' "$AUTODEPLOY_VERSION" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >>"$LOG"

INTERACTIVE=0
if [[ -t 0 ]] && { : </dev/tty; } 2>/dev/null; then INTERACTIVE=1; fi
tty_ask() { # prompt -> stdout (the answer). Reads /dev/tty only, never a piped stdin.
  local ans
  printf '%s' "$1" >/dev/tty
  IFS= read -r ans </dev/tty || die "no answer on the terminal"
  printf '%s' "$ans"
}
# Every function that touches the token starts with `local -; set +x`: under `bash -x` the trace of its commands (which
# expand $SECRET) would otherwise put the token into stderr and any captured log.
tty_secret() { # prompt -> SECRET (hidden input, /dev/tty only; never echoed, never argv, never stdout)
  local -; set +x
  printf '%s' "$1" >/dev/tty
  IFS= read -rs SECRET </dev/tty || { printf '\n' >/dev/tty; SECRET=""; die "no answer on the terminal"; }
  printf '\n' >/dev/tty
}
confirm() { # question: --yes accepts; a terminal asks; otherwise refuse with the flag to use
  if ((ASSUME_YES)); then warn "$1 (continuing because of --yes)"; return 0; fi
  ((INTERACTIVE)) || die "$1 Re-run with --yes to continue anyway."
  [[ "$(tty_ask "$1 Continue? [y/N] ")" =~ ^[Yy] ]] || die "aborted by the operator"
}

# Version, dev builds and trust-anchor policy. Evaluated after ALL arguments are parsed, so flag order cannot
# matter, and it reads no environment variable.
[[ $AUTODEPLOY_VERSION =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || die "malformed AUTODEPLOY_VERSION '$AUTODEPLOY_VERSION' in this installer"
if [[ $AUTODEPLOY_VERSION != *-dev* ]]; then
  if ((ALLOW_DEV)) || ((TESTING_OK)) || [[ -n $ALLOWED_FILE ]]; then
    die "--allow-dev, --allowed-signers and --yes-i-am-testing are accepted only by development builds, and this is release $AUTODEPLOY_VERSION: real installs trust only the signing key embedded in this installer. Refusing."
  fi
else
  ((ALLOW_DEV)) || die "this is a development build ($AUTODEPLOY_VERSION), not a release. Download install.sh from a release tag; --allow-dev is for testing only."
  if ((TESTING_OK)); then
    :
  elif ((INTERACTIVE)); then
    [[ "$(tty_ask "DEVELOPMENT BUILD $AUTODEPLOY_VERSION: for testing only. Type I AM TESTING to continue: ")" == "I AM TESTING" ]] || die "aborted by the operator"
  else
    die "development builds need a typed confirmation on the terminal, or --yes-i-am-testing when there is no terminal"
  fi
fi
if [[ -n $ALLOWED_FILE ]]; then
  [[ -f $ALLOWED_FILE && ! -L $ALLOWED_FILE ]] || die "--allowed-signers $ALLOWED_FILE is not a regular file"
elif embedded_allowed_signers | grep -q PLACEHOLDER; then
  die "the release public key embedded in this installer is still the PLACEHOLDER: this build cannot verify any release. Use install.sh from an official release tag."
fi
if [[ -n $RELEASE_DIR ]]; then
  [[ -d $RELEASE_DIR && ! -L $RELEASE_DIR ]] || die "--release-dir $RELEASE_DIR is not a directory"
fi
KIT_PARENT=${KIT_COPY%/*}
check_kit_parent() { # the directory holding the kit copy: root-owned 0755, never a symlink
  [[ ! -e $KIT_PARENT && ! -L $KIT_PARENT ]] && return 0
  [[ ! -L $KIT_PARENT && -d $KIT_PARENT ]] || die "$KIT_PARENT is a symlink or not a directory: refusing"
  [[ "$(stat -c '%u %a' "$KIT_PARENT")" == "0 755" ]] || die "$KIT_PARENT must be a root-owned 0755 directory"
}
check_kit_parent

# ---- host preflight (read-only) ----
[[ "$(uname -m)" == x86_64 ]] || die "x86_64 only (this host reports $(uname -m))"
os_id="" os_ver=""
if [[ -r /etc/os-release ]]; then
  os_id="$(sed -n 's/^ID=//p' /etc/os-release | head -n1 | tr -d '"')"
  os_ver="$(sed -n 's/^VERSION_ID=//p' /etc/os-release | head -n1 | tr -d '"')"
fi
[[ $os_id == ubuntu && $os_ver == 24.04 ]] || die "Ubuntu 24.04 LTS only (this host reports ID=${os_id:-unknown} VERSION_ID=${os_ver:-unknown}). Use an Ubuntu 24.04 server image."
[[ -d /run/systemd/system ]] || die "systemd is required (this host is not running systemd as PID 1)"
[[ "$(stat -fc %T /sys/fs/cgroup 2>/dev/null)" == cgroup2fs ]] || die "cgroup v2 is required (stat -fc %T /sys/fs/cgroup must print cgroup2fs)"
vcpu="$(nproc)"
((vcpu >= 2)) || die "at least 2 vCPU are required (found $vcpu)"
mem_mb="$(free -m | awk '/^Mem:/ {print $2; exit}')"
# A "2 GB" machine reports slightly less than 2048 MB.
[[ $mem_mb =~ ^[0-9]+$ ]] && ((mem_mb >= 1800)) || die "at least 2 GB RAM is required (found ${mem_mb:-unknown} MB)"
disk_kb="$(df -Pk /var/lib 2>/dev/null | awk 'NR==2 {print $4}')"
[[ $disk_kb =~ ^[0-9]+$ ]] && ((disk_kb >= 40 * 1024 * 1024)) || die "at least 40 GB free disk is required under /var/lib (the checkout image goes there); found $(( ${disk_kb:-0} / 1024 / 1024 )) GB"
for tool in ssh-keygen tar gzip sha256sum; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool not found (try: apt install openssh-client tar gzip coreutils)"
done
[[ -n $RELEASE_DIR && $ROLE == all ]] || command -v curl >/dev/null 2>&1 || die "curl not found (apt install curl)"

# semver compare: prints -1, 0 or 1 for A vs B (both vX.Y.Z[-pre]); a pre-release sorts below its release.
semver_cmp() {
  local re='^v([0-9]+)\.([0-9]+)\.([0-9]+)(-([0-9A-Za-z.-]+))?$' a b i x y
  [[ $1 =~ $re ]] || return 2
  a=("${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}" "${BASH_REMATCH[3]}"); local pa="${BASH_REMATCH[5]:-}"
  [[ $2 =~ $re ]] || return 2
  b=("${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}" "${BASH_REMATCH[3]}"); local pb="${BASH_REMATCH[5]:-}"
  for i in 0 1 2; do
    x=$((10#${a[i]})); y=$((10#${b[i]}))
    if ((x < y)); then echo -1; return 0; elif ((x > y)); then echo 1; return 0; fi
  done
  if [[ -z $pa && -z $pb ]]; then echo 0; return 0; fi
  if [[ -z $pa ]]; then echo 1; return 0; fi
  if [[ -z $pb ]]; then echo -1; return 0; fi
  local -a ia ib; IFS=. read -r -a ia <<<"$pa"; IFS=. read -r -a ib <<<"$pb"
  for ((i = 0; i < ${#ia[@]} || i < ${#ib[@]}; i++)); do
    x="${ia[i]-}"; y="${ib[i]-}"
    if [[ -z $x ]]; then echo -1; return 0; fi
    if [[ -z $y ]]; then echo 1; return 0; fi
    if [[ $x =~ ^[0-9]+$ && $y =~ ^[0-9]+$ ]]; then
      if ((10#$x < 10#$y)); then echo -1; return 0; elif ((10#$x > 10#$y)); then echo 1; return 0; fi
    elif [[ $x =~ ^[0-9]+$ ]]; then echo -1; return 0
    elif [[ $y =~ ^[0-9]+$ ]]; then echo 1; return 0
    elif [[ $x < $y ]]; then echo -1; return 0
    elif [[ $x > $y ]]; then echo 1; return 0; fi
  done
  echo 0
}

# Previous install (parsed, never sourced).
cfg() { sed -n "s/^$2=//p" "$1" 2>/dev/null | head -n1 || true; }
INSTALLER_ENV="$ETC_DIR/installer.env"
UPGRADE=0 RERUN=0 PREV_DOMAIN="" PREV_EMAIL="" PREV_STAGING=""
S_DOMAIN="" S_EMAIL="" S_STAGING="" S_FIREWALL="" S_SSH_PORT="" S_ROLE=""
if [[ -e $INSTALLER_ENV ]]; then
  RERUN=1
  [[ -f $INSTALLER_ENV && ! -L $INSTALLER_ENV ]] || die "$INSTALLER_ENV is not a regular file"
  S_DOMAIN="$(cfg "$INSTALLER_ENV" INSTALLER_DOMAIN)"
  S_EMAIL="$(cfg "$INSTALLER_ENV" INSTALLER_EMAIL)"
  S_STAGING="$(cfg "$INSTALLER_ENV" INSTALLER_ACME_STAGING)"
  S_FIREWALL="$(cfg "$INSTALLER_ENV" INSTALLER_FIREWALL)"
  S_SSH_PORT="$(cfg "$INSTALLER_ENV" INSTALLER_SSH_PORT)"
  S_ROLE="$(cfg "$INSTALLER_ENV" INSTALLER_ROLE)"
  [[ ${S_ROLE:-all} == "$ROLE" ]] || die "an earlier run of this installer on this host chose --role ${S_ROLE:-all}, not --role $ROLE: switching a host's role is not supported. If that run never got past its first steps, remove $INSTALLER_ENV yourself and re-run."
fi
if [[ -e $ETC_DIR/host.env ]]; then
  UPGRADE=1 RERUN=1
  PREV_DOMAIN="$(cfg "$ETC_DIR/host.env" AUTODEPLOY_CONTROLPLANE_HOST)"
  prev_mode="$(cfg "$ETC_DIR/host.env" AUTODEPLOY_TLS_MODE)"
  PREV_EMAIL="$(cfg "$ETC_DIR/host.env" AUTODEPLOY_ACME_EMAIL)"
  PREV_STAGING="$(cfg "$ETC_DIR/host.env" AUTODEPLOY_ACME_STAGING)"
  prev_role="$(cfg "$ETC_DIR/host.env" AUTODEPLOY_ROLE)"
  [[ ${prev_role:-all} == "$ROLE" ]] || die "this host was installed with role=${prev_role:-all} (host.env), not --role $ROLE: switching a host's role in place is not supported. Uninstall (uninstall.sh --yes) and install fresh."
  [[ $prev_mode == public ]] || die "this host has a lan-mode (private CA) AutoDeploy install (AUTODEPLOY_TLS_MODE='${prev_mode:-lan}'). This installer only handles public hosts; switching an install in place is not supported."
fi
# Only once enrolled: before that a mistyped id or host can be corrected on a re-run.
if [[ $ROLE == agent && -e $AGENT_LOCAL_ENV && -e $AGENT_CREDENTIAL ]]; then # the server id this host's agent unit runs with (parsed, never sourced)
  rec_id="$(cfg "$AGENT_LOCAL_ENV" AUTODEPLOY_AGENT_SERVER_ID)"
  [[ -z $rec_id || $rec_id == "$SERVER_ID" ]] || die "this host is already set up as server $rec_id, not $SERVER_ID: one host is one server. Re-run with --server-id $rec_id, or uninstall and install fresh."
fi
if [[ -e $VERSION_FILE ]]; then
  installed="$(head -n1 "$VERSION_FILE" 2>/dev/null)" || die "cannot read the installed version file $VERSION_FILE: fix its permissions or content (it must hold one line such as v1.2.3)"
  if ! cmp_res="$(semver_cmp "$installed" "$AUTODEPLOY_VERSION")"; then
    ((ALLOW_DOWNGRADE)) || die "cannot read the installed version '$installed' in $VERSION_FILE; pass --allow-downgrade to proceed anyway"
  elif [[ $cmp_res == 1 ]] && ((!ALLOW_DOWNGRADE)); then
    die "installed version $installed is newer than $AUTODEPLOY_VERSION: refusing the downgrade (pass --allow-downgrade to force it)"
  fi
fi

# ---- inputs ----------------------------------------------------------------------------------------
valid_public_host() { # same rule as install-app.sh
  local label='[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?'
  [[ "$1" =~ ^($label\.)+$label$ && ${#1} -le 253 ]] || return 1
  [[ ! "$1" =~ ^[0-9.]+$ && ! "${1##*.}" =~ ^[0-9]+$ ]] || return 1
  case "$1" in *.test | *.local | *.localhost | *.internal | localhost) return 1 ;; *) return 0 ;; esac
}
# Precedence: flags, then what this host recorded (host.env for what install-app wrote, installer.env for the
# choices of an earlier, possibly partial, run), then prompts.
# Only host.env (written by install-app.sh) makes the domain binding; installer.env of an aborted run is a default.
[[ -n $DOMAIN || -z $S_DOMAIN ]] || DOMAIN="$S_DOMAIN"
REF_DOMAIN="$PREV_DOMAIN"
if [[ -n $REF_DOMAIN ]]; then
  [[ -n $DOMAIN ]] || DOMAIN="$REF_DOMAIN"
  if [[ $DOMAIN != "$REF_DOMAIN" && $ROLE == agent && -e $AGENT_CREDENTIAL ]]; then
    die "this agent server is enrolled with the control plane $REF_DOMAIN, not $DOMAIN: pointing an enrolled agent at another control plane is not supported. Re-run with --control-plane-host $REF_DOMAIN, or uninstall and install fresh."
  fi
  if [[ $DOMAIN != "$REF_DOMAIN" && $ROLE == all ]]; then
    die "a previous install used the domain $REF_DOMAIN, not $DOMAIN. Changing the domain is not supported by this installer: re-run with --domain $REF_DOMAIN (or without --domain)."
  fi
fi
if ((RERUN)); then
  [[ -n $EMAIL ]] || EMAIL="${S_EMAIL:-${PREV_EMAIL:-none}}"
  [[ -n $FIREWALL ]] || FIREWALL="${S_FIREWALL:-no}"
  [[ -n $SSH_PORT || $FIREWALL != yes ]] || SSH_PORT="$S_SSH_PORT"
  [[ -n $ACME_STAGING ]] || ACME_STAGING="${PREV_STAGING:-${S_STAGING:-0}}"
  if [[ -n $PREV_STAGING && $PREV_STAGING != "$ACME_STAGING" ]]; then
    die "this host was installed with ACME staging=$PREV_STAGING; switching between staging and production is a manual procedure (docs/operations/vps-install.md section 4, step 2). Re-run without changing --acme-staging."
  fi
else
  : "${ACME_STAGING:=0}"
fi
[[ $FIREWALL == no ]] && SSH_PORT=""
[[ -z $FIREWALL || $FIREWALL == yes || $FIREWALL == no ]] || die "$INSTALLER_ENV holds an invalid firewall choice"
[[ -z $SSH_PORT || ( $SSH_PORT =~ ^[1-9][0-9]{0,4}$ && $SSH_PORT -le 65535 ) ]] || die "$INSTALLER_ENV holds an invalid SSH port"

missing=()
[[ -n $DOMAIN ]] || missing+=("--domain FQDN       control plane hostname (DNS must point at this host)")
[[ -n $EMAIL ]] || missing+=("--email ADDR|none  Let's Encrypt account / expiry-notice address")
[[ -n $FIREWALL ]] || missing+=("--firewall yes|no  configure ufw (SSH, 80, 443)")
if [[ $FIREWALL == yes && -z $SSH_PORT ]] && ((!INTERACTIVE)); then
  missing+=("--ssh-port N        the SSH port you are connected through (needed with --firewall yes)")
fi
if ((${#missing[@]})) && ((!INTERACTIVE)); then
  {
    printf 'install.sh: error: required answers are missing and there is no terminal to ask them on. Pass these flags:\n'
    printf '  %s\n' "${missing[@]}"
    printf 'Optional: --acme-staging --yes --setup-wait DURATION --allow-downgrade --release-dir DIR\n'
  } >&2
  printf 'ERROR: missing flags\n' >>"$LOG"
  exit 1
fi
if [[ -z $DOMAIN ]]; then DOMAIN="$(tty_ask 'Control plane domain (e.g. autodeploy.example.com, DNS must point at this host): ')"; fi
if [[ $ROLE == agent ]]; then
  valid_public_host "$DOMAIN" || die "invalid --control-plane-host '$DOMAIN': need the control plane's lowercase FQDN, not a URL (IP literals and *.test/.local/.localhost/.internal are rejected)"
  [[ $SERVER_ID =~ ^server_[A-Za-z0-9_-]{1,100}$ ]] || die "invalid --server-id: use the server id shown on the Servers page (server_ followed by letters, digits, - or _)"
else
  valid_public_host "$DOMAIN" || die "invalid --domain '$DOMAIN': need a lowercase FQDN (IP literals and *.test/.local/.localhost/.internal are rejected)"
fi
if [[ -z $EMAIL ]]; then EMAIL="$(tty_ask "ACME email for Let's Encrypt notices (or 'none'): ")"; fi
if [[ $EMAIL == none ]]; then EMAIL=""; fi
if [[ -n $EMAIL ]]; then
  [[ "$EMAIL" =~ ^[A-Za-z0-9._%+-]{1,64}@[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)+$ && ${#EMAIL} -le 254 ]] || die "invalid --email '$EMAIL'"
else
  warn "no ACME email: Let's Encrypt cannot send expiry or account notices"
fi
if [[ -z $FIREWALL ]]; then
  FIREWALL="$(tty_ask 'Configure the firewall (ufw: allow SSH, 80, 443; deny the rest)? [yes/no]: ')"
  [[ $FIREWALL =~ ^[Yy] ]] && FIREWALL=yes || FIREWALL=no
fi

# ---- agent role: enrollment token handling (24-F) --------------------------------------------------------
# The token is a one-time secret: it is read at a hidden /dev/tty prompt or from a root-owned 0600 file, held in the
# shell variable SECRET only for as long as it takes to write it into a private 0600 copy for the agent user, and it
# is never printed, logged, passed in argv, exported or put in a URL. Error messages never include its value.
accept_secret() { # trims surrounding whitespace from the global SECRET, then 0 only if it looks like an enrollment token
  local -; set +x
  SECRET="${SECRET#"${SECRET%%[![:space:]]*}"}"
  SECRET="${SECRET%"${SECRET##*[![:space:]]}"}"
  [[ $SECRET =~ ^enr\.v1\.[A-Za-z0-9_-]{20,200}$ ]]
}
read_token_file() { # FILE -> SECRET. One open file descriptor: every check is on the file actually read (no swap race).
  local -; set +x
  local f="$1" ftype uid mode size
  [[ ! -L $f ]] || die "--token-file $f is a symlink: refusing"
  [[ -f $f ]] || die "--token-file $f is not a regular file"
  exec 8<"$f" || die "cannot open --token-file $f"
  IFS='|' read -r ftype uid mode size < <(stat -L -c '%F|%u|%a|%s' /proc/self/fd/8) || { exec 8<&-; die "cannot inspect --token-file $f"; }
  if [[ $ftype != "regular file" || $uid != 0 ]]; then exec 8<&-; die "--token-file $f must be a regular file owned by root (it is not): copy the token into a file created by root with mode 0600"; fi
  if [[ $mode != 600 && $mode != 400 ]]; then exec 8<&-; die "--token-file $f must have mode 0600 (it has $mode): chmod 0600 it first"; fi
  if ((size > 512)); then exec 8<&-; die "--token-file $f is too large to hold an enrollment token"; fi
  IFS= read -r SECRET <&8 || true
  exec 8<&-
  accept_secret || { SECRET=""; die "the token in --token-file $f has an unexpected format (the value is not shown): copy the token from the Servers page again"; }
}
if [[ $ROLE == agent ]]; then
  if [[ -e $AGENT_CREDENTIAL ]]; then
    [[ -z $TOKEN_FILE ]] || warn "this host is already enrolled: --token-file is ignored"
  elif [[ -n $TOKEN_FILE ]]; then
    read_token_file "$TOKEN_FILE"; SECRET="" # validated now (before any change); read again when it is needed
  elif ((!INTERACTIVE)); then
    die "this installer needs the one-time enrollment token from the Servers page, but there is no terminal to ask on: run it from a terminal (it asks at a hidden prompt) or pass --token-file FILE (a file owned by root, mode 0600)."
  fi
fi

if [[ $ROLE == agent ]]; then
  say "installing the AutoDeploy agent $AUTODEPLOY_VERSION on this server (control plane https://$DOMAIN, server $SERVER_ID)$( ((UPGRADE)) && echo ' (upgrade of an existing install)')"
else
  say "installing AutoDeploy $AUTODEPLOY_VERSION on $DOMAIN$( ((UPGRADE)) && echo ' (upgrade of an existing install)')"
fi
if ((INTERACTIVE)) && ((!ASSUME_YES)); then
  if [[ $ROLE == agent ]]; then
    printf 'Role: agent-only server\nControl plane: https://%s\nServer id: %s\nACME email: %s\nFirewall: %s\n' "$DOMAIN" "$SERVER_ID" "${EMAIL:-none}" "$FIREWALL" >/dev/tty
  else
    printf 'Domain: %s\nACME email: %s\nFirewall: %s\n' "$DOMAIN" "${EMAIL:-none}" "$FIREWALL" >/dev/tty
  fi
  [[ "$(tty_ask 'Proceed? [y/N] ')" =~ ^[Yy] ]] || die "aborted by the operator"
fi

# ---- ports and DNS (read-only checks) ---------------------------------------------------------------
if ((!UPGRADE)); then
  if command -v ss >/dev/null 2>&1; then
    busy="$(ss -H -ltnp 2>/dev/null | awk '$4 ~ /:(80|443)$/ {print}' || true)"
    if [[ -n $busy ]]; then
      holders="$(sed -n 's/.*users:(("\([^"]*\)",pid=\([0-9]*\).*/\1 (pid \2)/p' <<<"$busy" | sort -u | paste -sd, -)"
      die "port 80 and/or 443 is already in use by: ${holders:-another process}. Stop it first (Traefik needs both ports)."
    fi
  else
    warn "ss not found: cannot check whether ports 80/443 are free"
  fi
fi

curl_https() { curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL --retry 3 --connect-timeout 20 --max-time 900 "$@"; }
check_cp_reachable() { # the agent role's counterpart of check_dns: can THIS host reach the control plane over HTTPS?
  local url="https://$DOMAIN/readyz"
  # System trust store only (no -k, no --cacert): the agent itself talks to the control plane the same way.
  curl_https --max-time 15 --retry 2 -o /dev/null "$url" \
    || die "cannot reach the control plane at $url from this server (it must answer over HTTPS with a certificate this server trusts). Check the --control-plane-host spelling, that the control plane is running (its own installer finished) and that this server has outbound HTTPS and working DNS. $1"
  say "the control plane answers $url"
}
check_dns() {
  local src4 resolved r has_a=0 mism="" shown
  src4="$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for (i = 1; i < NF; i++) if ($i == "src") {print $(i + 1); exit}}' || true)"
  if command -v dig >/dev/null 2>&1; then
    resolved="$({ dig +short +time=3 +tries=1 A "$DOMAIN"; dig +short +time=3 +tries=1 AAAA "$DOMAIN"; } 2>/dev/null | grep -E '^[0-9a-fA-F:.]+$' || true)"
  else
    resolved="$(getent ahostsv4 "$DOMAIN" 2>/dev/null | awk '{print $1}' | sort -u || true)"
  fi
  if [[ -z $resolved ]]; then
    confirm "$DOMAIN has no A record yet (DNS does not resolve it): Let's Encrypt cannot issue a certificate until it points at this host."
    return 0
  fi
  shown="$(tr '\n' ' ' <<<"$resolved" | sed 's/ $//')"
  while read -r r; do
    [[ -n $r ]] || continue
    if [[ $r == *:* ]]; then continue; fi
    has_a=1
    [[ -z $src4 || $r == "$src4" ]] || mism+=" $r"
  done <<<"$resolved"
  if ((!has_a)); then
    confirm "$DOMAIN has no A record ($shown): IPv4 clients and Let's Encrypt cannot reach this host."
  elif [[ $src4 =~ ^(10\.|192\.168\.|172\.(1[6-9]|2[0-9]|3[01])\.|100\.(6[4-9]|[7-9][0-9]|1[01][0-9]|12[0-7])\.|169\.254\.) ]]; then
    warn "this host's address ($src4) is private: it is behind NAT, so DNS for $DOMAIN ($shown) cannot be compared from here; it must be the provider's public IP"
  elif [[ -n $mism ]]; then
    confirm "DNS for $DOMAIN ($shown) has record(s) that are not this host's address ($src4):$mism."
  else
    say "DNS for $DOMAIN resolves to this host ($shown)"
  fi
}
if [[ $ROLE == all ]]; then
  check_dns
elif [[ ! -e $AGENT_CREDENTIAL ]]; then
  check_cp_reachable "Nothing was changed on this server."
fi

# ---- Docker state (read-only) -------------------------------------------------------------------------
# AutoDeploy needs Docker CE from Docker's apt repository at exactly /usr/bin/docker (the units pin it).
DOCKER_BIN=/usr/bin/docker
pkg_installed() { dpkg-query -W -f='${Status}' "$1" 2>/dev/null | grep -q 'install ok installed'; }
if pkg_installed docker.io; then
  die "the Ubuntu package docker.io is installed. AutoDeploy needs Docker Engine and the buildx plugin from Docker's official apt repository (docker-ce, docker-buildx-plugin); docker.io conflicts with them. Remove it yourself (apt remove docker.io), then re-run."
fi
if pkg_installed podman-docker; then
  die "the podman-docker package (a 'docker' shim over Podman) is installed. AutoDeploy needs Docker CE; remove podman-docker yourself, then re-run."
fi
if command -v snap >/dev/null 2>&1 && snap list docker >/dev/null 2>&1; then
  die "Docker is installed as a snap. The snap's confinement breaks the checkout/build integration; remove it (snap remove docker) and re-run so Docker CE is installed from Docker's official apt repository."
fi
DOCKER_READY=0
if [[ -x $DOCKER_BIN ]]; then
  case "$(readlink -f "$DOCKER_BIN" 2>/dev/null)" in *podman*) die "$DOCKER_BIN is a Podman shim: AutoDeploy needs Docker CE (docker-ce package)" ;; esac
  pkg_installed docker-ce || die "$DOCKER_BIN exists but the docker-ce package is not installed: this is not Docker CE from Docker's apt repository. Remove it yourself and re-run so the installer can set up docker-ce."
  if systemctl is-active --quiet docker && "$DOCKER_BIN" buildx version >/dev/null 2>&1; then DOCKER_READY=1; fi
else
  for other in /usr/local/bin/docker /usr/local/sbin/docker /snap/bin/docker "$(command -v docker 2>/dev/null || true)"; do
    if [[ -n $other && -e $other ]]; then die "a docker binary exists at $other, but AutoDeploy needs Docker CE at exactly $DOCKER_BIN (the services pin that path). Remove or rename it and re-run so the installer can install docker-ce from Docker's apt repository."; fi
  done
fi

# ---- trust: private directory, key, fingerprint ------------------------------------------------------
WORK="$(mktemp -d "$WORK_PARENT/autodeploy-install.XXXXXX")"
[[ "$(stat -c '%u %a' "$WORK")" == "0 700" ]] || die "$WORK is not a root-owned 0700 directory"
export TMPDIR="$WORK"
mkdir -m 0700 "$WORK/release" "$WORK/kit-x" "$WORK/bin-x"
SIGNERS="$WORK/allowed_signers"
if [[ -n $ALLOWED_FILE ]]; then cat -- "$ALLOWED_FILE" >"$SIGNERS"; else embedded_allowed_signers >"$SIGNERS"; fi
chmod 0600 "$SIGNERS"
say "release signing key(s) trusted by this installer (compare with the fingerprint published in the README):"
key_re='(ssh-[a-z0-9-]+|ecdsa-sha2-[a-z0-9]+|sk-[a-z0-9@.-]+)[[:space:]]+([A-Za-z0-9+/=]+)'
nkeys=0
while IFS= read -r line; do
  [[ $line =~ ^[[:space:]]*# || -z ${line//[[:space:]]/} ]] && continue
  if [[ $line =~ $key_re ]]; then
    printf '%s %s\n' "${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}" >"$WORK/pub.tmp"
    ssh-keygen -lf "$WORK/pub.tmp" | tee -a "$LOG"
    nkeys=$((nkeys + 1))
  fi
done <"$SIGNERS"
((nkeys > 0)) || die "no usable public key in the trust anchor"
if [[ -n $ALLOWED_FILE ]]; then warn "TESTING ONLY: trusting $ALLOWED_FILE instead of the embedded release key"; fi

# ---- download (or copy), verify, then extract ------------------------------------------------------
V="$AUTODEPLOY_VERSION"
ASSETS=("autodeploy-host-kit-$V.tar.gz" "autodeploy-binaries-$V.tar.gz" VERSION SHA256SUMS SHA256SUMS.sig)
if [[ -n $RELEASE_DIR ]]; then
  say "using local release assets from $RELEASE_DIR"
  for a in "${ASSETS[@]}"; do
    [[ -f $RELEASE_DIR/$a && ! -L $RELEASE_DIR/$a ]] || die "$RELEASE_DIR lacks the regular file $a"
    cp -- "$RELEASE_DIR/$a" "$WORK/release/$a"
  done
else
  base="https://github.com/$REPO_SLUG/releases/download/$V"
  say "downloading release $V"
  for a in "${ASSETS[@]}"; do
    curl_https -o "$WORK/release/$a" "$base/$a" || die "download of $base/$a failed"
  done
fi
say "verifying the release (signature, SHA256SUMS, version binding) before anything is extracted"
if ! verify_out="$(verify_release_dir "$WORK/release" "$SIGNERS" "$V" 2>&1)"; then
  printf '%s\n' "$verify_out" >>"$LOG"
  die "release verification failed: $verify_out. Nothing was installed."
fi
printf '%s\n' "$verify_out" | tee -a "$LOG"

extract_safe() { # TARBALL DEST PREFIX: refuse absolute/.. members, links, devices, foreign top-level names
  local tb="$1" dest="$2" prefix="$3" n t
  while IFS= read -r n; do
    [[ -n $n ]] || continue
    [[ $n != /* && $n != ".." && $n != ../* && $n != */../* && $n != */.. ]] || die "unsafe member path in $tb: $n"
    [[ $n == "$prefix" || $n == "$prefix/"* ]] || die "unexpected member in $tb: $n (not under $prefix/)"
  done < <(tar -tzf "$tb")
  while IFS= read -r t; do
    [[ ${t:0:1} == - || ${t:0:1} == d ]] || die "unsafe member type in $tb: only regular files and directories are allowed"
  done < <(tar -tvzf "$tb")
  tar -xzf "$tb" -C "$dest" --no-same-owner
  if [[ -n "$(find "$dest" \( -type l -o ! -type f ! -type d \) -print -quit)" ]]; then die "extracted tree contains links or special files"; fi
}
extract_safe "$WORK/release/autodeploy-host-kit-$V.tar.gz" "$WORK/kit-x" "autodeploy-host-kit-$V"
extract_safe "$WORK/release/autodeploy-binaries-$V.tar.gz" "$WORK/bin-x" binaries
KIT="$WORK/kit-x/autodeploy-host-kit-$V"
BINDIR="$WORK/bin-x/binaries"
kit_files=(install-checkout.sh install-app.sh create-credentials.sh enable-github.sh enroll-local.sh)
[[ $ROLE == agent ]] && kit_files+=(preflight-agent.sh preflight-checkout.sh)
for f in "${kit_files[@]}"; do
  [[ -f $KIT/$f && -x $KIT/$f ]] || die "kit file $f is missing or not executable in the verified release"
done
[[ -f $BINDIR/SHA256SUMS ]] || die "the binaries archive has no SHA256SUMS"

# ---- from here on the host is modified ---------------------------------------------------------------
export DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=a
APT=(apt-get -o DPkg::Lock::Timeout=600)
STEP_RC=0
run_step_rc() { # label cmd...: output to terminal and log; sets STEP_RC, never exits
  local label="$1"; shift
  say "$label"
  set +e
  # stdin is /dev/null unless the step really prompts (STEP_TTY=1, only install-app.sh's firewall confirmation).
  if ((INTERACTIVE)) && [[ ${STEP_TTY:-0} == 1 ]]; then "$@" 2>&1 | tee -a "$LOG"; else "$@" 2>&1 </dev/null | tee -a "$LOG"; fi
  STEP_RC=${PIPESTATUS[0]}
  set -e
}
run_step() { run_step_rc "$@"; ((STEP_RC == 0)) || die "step failed (exit $STEP_RC): $1. See $LOG"; }

# Docker's apt signing key, pinned. Fingerprint of "Docker Release (CE deb) <docker@docker.com>", read from the
# key served at https://download.docker.com/linux/ubuntu/gpg and parsed with gpg --show-keys --with-colons.
readonly DOCKER_KEY_FPR=9DC858229FC7DD38854AE2D88D81803C0EBFCD88
install_docker_ce() { # Docker's documented apt repository method (https://docs.docker.com/engine/install/ubuntu/)
  local codename arch gout npub fpr
  run_step "installing prerequisites for Docker's apt repository" "${APT[@]}" install -y ca-certificates curl gnupg
  say "fetching Docker's apt signing key and checking its pinned fingerprint"
  curl_https -o "$WORK/docker.asc" https://download.docker.com/linux/ubuntu/gpg || die "could not download Docker's apt key"
  mkdir -m 0700 -p "$WORK/gnupg"
  gout="$(GNUPGHOME="$WORK/gnupg" gpg --batch --show-keys --with-colons "$WORK/docker.asc" 2>/dev/null)" || die "gpg could not read Docker's apt key"
  npub="$(grep -c '^pub:' <<<"$gout" || true)"
  fpr="$(awk -F: '/^pub:/ {p = 1} p && /^fpr:/ {print $10; exit}' <<<"$gout")"
  if [[ $npub != 1 || $fpr != "$DOCKER_KEY_FPR" ]]; then
    die "the Docker apt key does not match the pinned fingerprint (got '${fpr:-none}', $npub key(s); expected $DOCKER_KEY_FPR). Nothing was installed from it."
  fi
  install -m 0755 -d /etc/apt/keyrings
  install -m 0644 "$WORK/docker.asc" /etc/apt/keyrings/docker.asc
  codename="$(. /etc/os-release && echo "${UBUNTU_CODENAME:-$VERSION_CODENAME}")"
  arch="$(dpkg --print-architecture)"
  cat >/etc/apt/sources.list.d/docker.sources <<DOCKER_SOURCES_EOF
Types: deb
URIs: https://download.docker.com/linux/ubuntu
Suites: $codename
Components: stable
Architectures: $arch
Signed-By: /etc/apt/keyrings/docker.asc
DOCKER_SOURCES_EOF
  chmod 0644 /etc/apt/sources.list.d/docker.sources
  run_step "refreshing apt" "${APT[@]}" update
  run_step "installing Docker Engine and the buildx plugin" "${APT[@]}" install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin
  run_step "enabling Docker" systemctl enable --now docker
  [[ -x $DOCKER_BIN ]] && pkg_installed docker-ce || die "docker-ce was installed but $DOCKER_BIN is missing"
}

run_step "refreshing apt" "${APT[@]}" update
pkgs=(apache2-utils openssl curl ca-certificates xfsprogs strace dnsutils)
[[ $FIREWALL == yes ]] && pkgs+=(ufw)
run_step "installing host dependencies" "${APT[@]}" install -y "${pkgs[@]}"
if ((DOCKER_READY)); then
  say "Docker CE is active and buildx is present: keeping it"
else
  install_docker_ce
fi

# ---- the install order of docs/operations/vps-install.md section 4 -------------------------------------
# Persist the confirmed choices only now (verified and extracted, nothing else changed yet), so a re-run after a partial install keeps them (parsed with sed, never sourced).
install -d -m 0755 "$ETC_DIR"
[[ ! -L $INSTALLER_ENV && ! -L $INSTALLER_ENV.new ]] || die "$INSTALLER_ENV or $INSTALLER_ENV.new is a symlink: refusing"
printf '%s\n' "# Written by install.sh: the choices you confirmed. Non-secret. Re-runs reuse them." \
  "INSTALLER_DOMAIN=$DOMAIN" "INSTALLER_EMAIL=${EMAIL:-none}" "INSTALLER_ACME_STAGING=$ACME_STAGING" \
  "INSTALLER_FIREWALL=$FIREWALL" "INSTALLER_SSH_PORT=$SSH_PORT" "INSTALLER_ROLE=$ROLE" >"$INSTALLER_ENV.new"
chmod 0600 "$INSTALLER_ENV.new"
mv -f "$INSTALLER_ENV.new" "$INSTALLER_ENV"

# Keep the verified kit on the host (uninstall.sh, preflight-*.sh, backup-db.sh, enable-github.sh live there). Done
# BEFORE enrollment so an enrollment failure or a long wait still leaves the tools in place. The copy is replaced
# atomically: build KIT_COPY.new root-owned and not group/world-writable, move the old one aside, move the new one
# in, then remove the old one (all installer-owned artifacts).
install_kit_copy() {
  local new="$KIT_COPY.new" old="$KIT_COPY.old" d
  install -d -m 0755 -o root -g root "$KIT_PARENT"
  check_kit_parent
  [[ ! -L $KIT_COPY ]] || die "$KIT_COPY is a symlink: refusing"
  for d in "$new" "$old"; do
    [[ ! -L $d ]] || die "$d is a symlink: refusing"
    if [[ -e $d ]]; then rm -rf -- "$d"; fi # leftover of an interrupted earlier install.sh run
  done
  mkdir -m 0755 "$new"
  cp -a "$KIT/." "$new/"
  chown -R root:root "$new"
  chmod -R go-w "$new"
  if [[ -e $KIT_COPY ]]; then mv -- "$KIT_COPY" "$old"; fi
  mv -- "$new" "$KIT_COPY"
  if [[ -e $old ]]; then rm -rf -- "$old"; fi
}

# ==== agent role (plan 24-F): checkout kit, Docker, Traefik with ACME, agent; no PostgreSQL, control plane or setup ====
record_server_id() { # the unit starts the agent with the id in agent-local.env (set only when empty or equal)
  local cur
  [[ -e $AGENT_LOCAL_ENV ]] || die "$AGENT_LOCAL_ENV is missing: install-app.sh did not create it"
  cur="$(cfg "$AGENT_LOCAL_ENV" AUTODEPLOY_AGENT_SERVER_ID)"
  [[ $cur != "$SERVER_ID" ]] || return 0
  [[ -z $cur ]] || die "$AGENT_LOCAL_ENV already records server $cur, not $SERVER_ID; refusing to overwrite it"
  REC_TMP="$(mktemp "$AGENT_LOCAL_ENV.XXXXXX")"
  grep -v '^AUTODEPLOY_AGENT_SERVER_ID=' "$AGENT_LOCAL_ENV" >"$REC_TMP" || [[ $? -eq 1 ]] || die "cannot read $AGENT_LOCAL_ENV"
  printf 'AUTODEPLOY_AGENT_SERVER_ID=%s\n' "$SERVER_ID" >>"$REC_TMP"
  chmod 0644 "$REC_TMP"
  chown root:root "$REC_TMP"
  mv -f "$REC_TMP" "$AGENT_LOCAL_ENV"
  REC_TMP=""
}
traefik_api_up() { # Traefik's management API answers 401 on loopback (plain HTTP over bash /dev/tcp, like http_ready)
  local line
  line="$( (exec 4<>/dev/tcp/127.0.0.1/8080 && printf 'GET /api/version HTTP/1.0\r\nHost: 127.0.0.1:8080\r\n\r\n' >&4 && read -r -t 5 line <&4 && printf '%s' "$line") 2>/dev/null)" || return 1
  [[ $line == HTTP/*" 401"* ]]
}
write_token_copy() { # SECRET -> $TOKDIR/token. The directory stays root-owned 0700 while root creates and writes the file.
  local -; set +x
  TOKDIR="$(mktemp -d /run/autodeploy-enroll.XXXXXX)"
  chmod 0700 "$TOKDIR"
  install -m 0600 -o "$AGENT_USER" -g "$AGENT_USER" /dev/null "$TOKDIR/token"
  printf '%s\n' "$SECRET" >"$TOKDIR/token"
  SECRET=""
  # Only now hand the agent access: group agent, 0710 = it can traverse to its own file but can never create,
  # rename or replace entries in the directory, so no swap is possible while it runs.
  chgrp "$AGENT_USER" "$TOKDIR"
  chmod 0710 "$TOKDIR"
}
enroll_agent() {
  local rc
  if [[ -e $AGENT_CREDENTIAL ]]; then
    say "this server is already enrolled: no token needed"
    record_server_id # normally already done; covers a run that stopped right after the enrollment
    return 0
  fi
  # Stale token directories of a crashed earlier run (exact prefix, real directories, root or agent owned, older than
  # the token lifetime), swept like enroll-local.sh does.
  find /run -maxdepth 1 -mindepth 1 -type d -name 'autodeploy-enroll.*' -mmin +15 \
    \( -user root -o -user "$AGENT_USER" \) -exec rm -rf --one-file-system -- {} + 2>/dev/null || true
  # The control plane could have gone away during the install: check again BEFORE asking for the token.
  check_cp_reachable "Nothing was enrolled; re-run this installer when it is back."
  if [[ -n $TOKEN_FILE ]]; then
    read_token_file "$TOKEN_FILE"
  else
    ((INTERACTIVE)) || die "no terminal to ask for the enrollment token on, and no --token-file given"
    tty_secret "Paste the one-time enrollment token from the Servers page (input is hidden), then press Enter: "
    accept_secret || { SECRET=""; die "that does not look like an enrollment token (the value is not shown). Copy it again from the Servers page; it is shown once and valid for 15 minutes."; }
  fi
  # A private copy the agent user can read (write_token_copy). printf is a shell builtin, so the token never reaches an
  # argv. cleanup() removes it on every exit path; it is also removed right after the enroll call.
  write_token_copy
  run_step_rc "enrolling this server's agent with the control plane" \
    runuser -u "$AGENT_USER" -- env -i PATH=/usr/bin:/bin "$APP_DIR/agent" enroll \
      --base-url "https://$DOMAIN" --server-id "$SERVER_ID" --state-dir "$AGENT_STATE" --token-file "$TOKDIR/token"
  rc=$STEP_RC
  rm -rf -- "$TOKDIR"
  TOKDIR=""
  ((rc == 0)) || die "agent enroll failed (exit $rc); the token copy was removed. The token is single use and expires after 15 minutes: on the control plane's Servers page use 'Reissue enrollment token' for $SERVER_ID, then re-run this installer (a mistyped --server-id or --control-plane-host can be corrected on the re-run). See $LOG"
  # Recorded only after the control plane accepted this id: a mistyped id must not stick.
  record_server_id
}

if [[ $ROLE == agent ]]; then
  run_step "1/6 checkout isolation (install-checkout.sh)" "$KIT/install-checkout.sh" "$BINDIR"
  app_args=(--mode public --role agent --control-plane-host "$DOMAIN")
  [[ -z $EMAIL ]] || app_args+=(--acme-email "$EMAIL")
  [[ $ACME_STAGING != 1 ]] || app_args+=(--acme-staging)
  if [[ $FIREWALL == yes ]]; then
    app_args+=(--configure-firewall)
    [[ -z $SSH_PORT ]] || app_args+=("--yes-firewall-ssh-port=$SSH_PORT")
  fi
  app_args+=("$BINDIR")
  STEP_TTY=1 # only this step may prompt (the firewall SSH-port confirmation)
  run_step "2/6 agent-only stack (install-app.sh --role agent)" "$KIT/install-app.sh" "${app_args[@]}"
  STEP_TTY=0
  run_step "3/6 Traefik API credentials (create-credentials.sh --traefik-only)" "$KIT/create-credentials.sh" --traefik-only
  run_step "4/6 starting Traefik" systemctl start autodeploy-traefik
  install_kit_copy # before enrollment, so a failure still leaves the tools in place
  say "waiting for Traefik's management API"
  api_ready=0
  for _ in $(seq 1 60); do
    if traefik_api_up; then api_ready=1; break; fi
    sleep 2
  done
  ((api_ready)) || warn "Traefik's management API did not answer within 2 minutes (the Traefik image may still be downloading); the preflight below will report it"
  say "5/6 enrolling this server"
  enroll_agent
  run_step "6/6 starting the agent" systemctl start autodeploy-agent.service

  install -d -m 0755 "$ETC_DIR"
  printf '%s\n' "$AUTODEPLOY_VERSION" >"$VERSION_FILE.new"
  chmod 0644 "$VERSION_FILE.new"
  mv -f "$VERSION_FILE.new" "$VERSION_FILE"

  pre_rc=0
  run_step_rc "preflight (preflight-agent.sh)" "$KIT_COPY/preflight-agent.sh"; ((STEP_RC == 0)) || pre_rc=1
  run_step_rc "preflight (preflight-checkout.sh)" "$KIT_COPY/preflight-checkout.sh"; ((STEP_RC == 0)) || pre_rc=1
  ((pre_rc == 0)) || die "this server is enrolled and its agent started, but a preflight reported failures (see above and $LOG). Fix them and re-run this installer."

  say "AutoDeploy agent $AUTODEPLOY_VERSION is installed: server $SERVER_ID reports to https://$DOMAIN"
  cat <<SUMMARY_EOF

  This server:      $SERVER_ID (control plane https://$DOMAIN)
  Installer log:    $LOG
  Kit scripts:      $KIT_COPY (preflight-agent.sh, preflight-checkout.sh, uninstall.sh, ...)

  Next steps
    1. On the control plane, bind an environment to this server (Projects / GitHub page, "Server" field).
    2. DNS: the hostname of every app you deploy to this server must have an A/AAAA record pointing at THIS
       server's IP address (not at the control plane); ports 80 and 443 must be reachable from the internet.
       Traefik requests each app's certificate from Let's Encrypt when its first deployment is routed here.
    3. If you used --token-file, delete that file now (the token is spent).
SUMMARY_EOF
  exit 0
fi

run_step "1/9 checkout isolation (install-checkout.sh)" "$KIT/install-checkout.sh" "$BINDIR"

app_args=(--mode public)
[[ -z $EMAIL ]] || app_args+=(--acme-email "$EMAIL")
[[ $ACME_STAGING != 1 ]] || app_args+=(--acme-staging)
if [[ $FIREWALL == yes ]]; then
  app_args+=(--configure-firewall)
  [[ -z $SSH_PORT ]] || app_args+=("--yes-firewall-ssh-port=$SSH_PORT")
fi
app_args+=("$BINDIR")
STEP_TTY=1 # only this step may prompt (the firewall SSH-port confirmation)
run_step "2/9 application stack (install-app.sh --mode public)" env AUTODEPLOY_CONTROLPLANE_HOST="$DOMAIN" "$KIT/install-app.sh" "${app_args[@]}"
STEP_TTY=0

run_step "3/9 credentials (create-credentials.sh)" "$KIT/create-credentials.sh"
run_step "4/9 starting PostgreSQL and Traefik" systemctl start autodeploy-postgres autodeploy-traefik
pg_ready=0
for _ in $(seq 1 30); do
  if "$DOCKER_BIN" exec autodeploy-postgres pg_isready -U autodeploy -q >/dev/null 2>&1; then pg_ready=1; break; fi
  sleep 2
done
((pg_ready)) || die "PostgreSQL did not become ready within 60 s: check 'docker logs autodeploy-postgres' and 'journalctl -u autodeploy-postgres'"
run_step "5/9 database migrations" "$RUN" migrate
run_step "6/9 secret-key (KEK) initialisation" "$KIT/enable-github.sh" --kek-only
run_step "7/9 starting the control plane" systemctl start autodeploy-controlplane

gateway="$(cfg "$ETC_DIR/host.env" AUTODEPLOY_ENV_GATEWAY)"
port="$(cfg "$ETC_DIR/host.env" AUTODEPLOY_CONTROLPLANE_PORT)"
[[ $gateway =~ ^[0-9.]+$ && $port =~ ^[0-9]+$ ]] || die "$ETC_DIR/host.env lacks a valid control-plane address"
http_ready() { # plain HTTP GET /readyz over bash /dev/tcp (the https-only wrapper is for downloads)
  local line
  line="$( (exec 4<>"/dev/tcp/$gateway/$port" && printf 'GET /readyz HTTP/1.0\r\nHost: %s:%s\r\n\r\n' "$gateway" "$port" >&4 && read -r -t 5 line <&4 && printf '%s' "$line") 2>/dev/null)" || return 1
  [[ $line == HTTP/*" 200"* ]]
}
say "8/9 waiting for the control plane to answer /readyz"
ready=0
for _ in $(seq 1 60); do
  if http_ready; then ready=1; break; fi
  sleep 2
done
((ready)) || die "the control plane did not answer http://$gateway:$port/readyz: check 'journalctl -u autodeploy-controlplane'"

install_kit_copy

SETUP_INCOMPLETE=0
if [[ -e $AGENT_CREDENTIAL ]]; then
  say "this host is already enrolled: no setup code and no enrollment needed"
else
  say "9/9 first-run setup code"
  code="" setup_rc=0
  code="$("$RUN" admin setup-token 2>"$WORK/setup.err" </dev/null)" || setup_rc=$?
  if ((setup_rc != 0)); then
    if grep -q 'administrator already exists' "$WORK/setup.err"; then
      say "an administrator already exists: no setup code needed"
    else
      cat "$WORK/setup.err" >>"$LOG"
      die "could not create the setup code (exit $setup_rc); see $LOG"
    fi
  else
    [[ $code =~ ^setup\.v1\.[A-Za-z0-9_-]+$ ]] || die "unexpected output from 'admin setup-token' (not shown)"
    # Terminal only (fd 3 = the original stdout): never through the log tee.
    {
      printf '\n'
      printf '  ============================================================\n'
      printf '  Finish setup in your browser (valid 60 minutes):\n'
      printf '    URL : https://%s/setup\n' "$DOMAIN"
      printf '    Code: %s\n' "$code"
      printf '  This code is shown once and is not written to any log.\n'
      printf '  ============================================================\n\n'
    } >&3
    code=""
  fi
  run_step_rc "enrolling this host's agent (waits up to $SETUP_WAIT for you to finish the browser setup)" "$KIT/enroll-local.sh" --wait "$SETUP_WAIT"
  if ((STEP_RC == 20)); then
    SETUP_INCOMPLETE=1
    warn "first-run setup was not finished in time; the stack is installed but this host's agent is not enrolled yet"
  elif ((STEP_RC != 0)); then
    die "enrollment failed (exit $STEP_RC); see $LOG"
  fi
fi

# Recorded on success AND on exit 4 (setup pending): the stack itself is installed either way. Intentional.
install -d -m 0755 "$ETC_DIR"
printf '%s\n' "$AUTODEPLOY_VERSION" >"$VERSION_FILE.new"
chmod 0644 "$VERSION_FILE.new"
mv -f "$VERSION_FILE.new" "$VERSION_FILE"

say "AutoDeploy $AUTODEPLOY_VERSION is installed"
cat <<SUMMARY_EOF

  Dashboard:        https://$DOMAIN
  Installer log:    $LOG
  Kit scripts:      $KIT_COPY (preflight-app.sh, preflight-checkout.sh, backup-db.sh, uninstall.sh, ...)

  Next steps
    1. Sign in at https://$DOMAIN with the administrator you created in the browser.
    2. Create the GitHub App. A one-click flow is planned (plan 24-E); until it ships, use the manual
       break-glass path: sudo $KIT_COPY/enable-github.sh (see its header) after creating the App on github.com.
    3. Bind a repository to a project and push to deploy.
SUMMARY_EOF
if ((SETUP_INCOMPLETE)); then
  cat <<SUMMARY_EOF

  First-run setup is NOT finished: re-run 'sudo bash install.sh' to get a new setup code and enroll this host.
SUMMARY_EOF
  exit 4
fi
exit 0
