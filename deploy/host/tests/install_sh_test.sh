#!/usr/bin/env bash
# Tests for deploy/host/install.sh (plan 24-B), the bootstrap installer. Local only.
#
# Part A (host side, needs ssh-keygen, git, tar, python3): key drift (embedded allowed_signers block
#   byte-equals release-signing/allowed_signers), release.yml version-guard extraction against the
#   exact AUTODEPLOY_VERSION line, kit allowlist classification of install.sh, static curl rule, and
#   behaviour equivalence of install.sh's embedded verify logic with verify-release.sh on the same
#   fixtures.
# Part B (needs Docker; skipped otherwise): runs the REAL install.sh as root in throwaway containers
#   (ubuntu:24.04 + openssh-client + netcat, no network) against a fixture release built by the REAL
#   package-release.sh from a throwaway git repo whose kit scripts are recording stubs. Throwaway
#   ed25519 keys only. Stubbed on PATH: uname, systemctl, apt-get, curl, docker, ss, ip, dig, nproc, free,
#   df, stat (cgroup query only). Every stubbed command and kit script appends to /tmp/calls, so the
#   ORDER of operations is asserted. Real host behaviour is NOT proven here (see tasks/24-plan.md).
# Usage: deploy/host/tests/install_sh_test.sh
set -uo pipefail

TESTS_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
KIT_DIR="$(cd -- "$TESTS_DIR/.." && pwd)"
REPO="$(cd -- "$KIT_DIR/../.." && pwd)"
INSTALL="${INSTALL_SH_UNDER_TEST:-$KIT_DIR/install.sh}"
VERIFY="$KIT_DIR/verify-release.sh"
FAILS=0
pass() { printf 'ok   %s\n' "$1"; }
fail() { printf 'FAIL %s\n' "$1" >&2; FAILS=$((FAILS + 1)); }

for c in ssh-keygen git tar python3; do
  if ! command -v "$c" >/dev/null 2>&1; then
    printf '##########\nSKIP install_sh_test: %s not available: NOTHING WAS TESTED\n##########\n' "$c" >&2
    [[ "${CI:-}" == true ]] && exit 1
    exit 0
  fi
done
sum() { sha256sum "$@" 2>/dev/null || shasum -a 256 "$@"; }

T="$(mktemp -d "${TMPDIR:-/tmp}/install-sh-test.XXXXXX")"
trap 'rm -rf -- "$T"' EXIT

if [[ ! -f "$INSTALL" ]]; then fail "deploy/host/install.sh does not exist"; printf '%d FAILED\n' "$FAILS" >&2; exit 1; fi

# ------------------------------------------------------------------------------------------ Part A
VERSION_LINE="$(grep -c '^AUTODEPLOY_VERSION="' "$INSTALL" || true)"
[[ "$VERSION_LINE" == 1 ]] && pass "install.sh has exactly one AUTODEPLOY_VERSION line at column 0" || fail "AUTODEPLOY_VERSION line count is $VERSION_LINE"

# The exact extraction command of the release.yml guard. It must be present verbatim in the workflow.
GUARD_SED='s/^AUTODEPLOY_VERSION="\(.*\)"$/\1/p'
grep -qF "have=\"\$(sed -n '$GUARD_SED' \"\$f\")\"" "$REPO/.github/workflows/release.yml" \
  && pass "release.yml guard uses the extraction command this test checks" \
  || fail "release.yml guard extraction command drifted from this test"
grep -qF 'if [[ "$TAG" == *-dev* ]]; then' "$REPO/.github/workflows/release.yml" \
  && pass "release.yml guard rejects -dev tags with the pattern this test checks" || fail "release.yml -dev tag guard missing"
for tg in v1.2.3 v1.0.0-rc.1 v1.2.3-dev v0.0.0-dev v1.0.0-dev.2; do
  if [[ "$tg" == *-dev* ]]; then r=reject; else r=accept; fi
  case "$tg" in *-dev*) want=reject ;; *) want=accept ;; esac
  [[ "$r" == "$want" ]] && pass "-dev tag pattern: $tg -> $r" || fail "-dev tag pattern: $tg"
done
have="$(sed -n "$GUARD_SED" "$INSTALL")"
[[ "$have" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] \
  && pass "guard extracts a single well-formed version from install.sh ($have)" || fail "guard extraction gave '$have'"
printf 'x\nAUTODEPLOY_VERSION="v1.2.3"\nAUTODEPLOY_VERSION="v1.2.4"\n' >"$T/two.sh"
[[ "$(sed -n "$GUARD_SED" "$T/two.sh" | wc -l | tr -d ' ')" == 2 ]] \
  && pass "guard line-count check would reject a duplicated constant" || fail "duplicate constant not detected by extraction"
printf '  readonly AUTODEPLOY_VERSION="v1.2.3"\n' >"$T/ro.sh"
[[ -z "$(sed -n "$GUARD_SED" "$T/ro.sh")" ]] \
  && pass "guard extraction ignores an indented/readonly variant (so the constant must stay a bare line)" || fail "unexpected extraction"

# Key drift: the block between the heredoc markers equals the committed file, byte for byte.
awk "/^  cat <<'ALLOWED_SIGNERS_EOF'\$/ {on=1; next} /^ALLOWED_SIGNERS_EOF\$/ {on=0} on" "$INSTALL" >"$T/embedded_signers"
if [[ -s "$T/embedded_signers" ]] && cmp -s "$T/embedded_signers" "$KIT_DIR/release-signing/allowed_signers"; then
  pass "embedded allowed_signers block byte-equals release-signing/allowed_signers"
else fail "embedded allowed_signers block differs from release-signing/allowed_signers (or markers missing)"; fi

# Kit allowlist: install.sh is fetched from git, never packaged.
eval "$(sed -n '/^KIT_ALLOWLIST=(/,/)$/p' "$KIT_DIR/package-release.sh")"
in_kit=0; for e in "${KIT_ALLOWLIST[@]}"; do [[ "install.sh" == "$e" ]] && in_kit=1; done
((in_kit == 0)) && pass "install.sh is not in package-release.sh KIT_ALLOWLIST" || fail "install.sh is in KIT_ALLOWLIST"
grep -q 'install.sh' <(sed -n '/^EXCLUDED=(/,/)$/p' "$TESTS_DIR/verify_release_test.sh") \
  && pass "install.sh is classified EXCLUDED by the kit drift test" || fail "install.sh missing from verify_release_test.sh EXCLUDED"

# The agent role verifies its two preflight scripts in the kit (L2).
grep -qE 'kit_files\+=\(preflight-agent.sh' "$INSTALL" && grep -qE 'preflight-checkout.sh' "$INSTALL" \
  && pass "install.sh checks preflight-agent.sh and preflight-checkout.sh in the verified kit for the agent role" \
  || fail "install.sh does not check preflight-agent.sh in the kit"

# Static curl rule: every curl invocation in install.sh goes through curl_https (the single wrapper).
bad_curl="$(grep -nE '(^|[^_a-z])curl([[:space:]]|$)' "$INSTALL" | grep -vE '^[0-9]+:[[:space:]]*#' | grep -vE 'pkgs=|install -y|command -v curl|curl_https\(\)|--proto|curl -h|apt-get|install_pkgs|echo|printf|say|die|warn|"[^"]*curl[^"]*"|apt install' || true)"
[[ -z "$bad_curl" ]] && pass "no bare curl invocation outside the https-only wrapper" || fail "bare curl invocation(s): $bad_curl"
grep -qE "curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL" "$INSTALL" \
  && pass "curl wrapper pins --proto =https --tlsv1.2 -fsSL" || fail "curl wrapper flags missing"

# Throwaway keys and fixtures.
ssh-keygen -q -t ed25519 -N '' -C release@autodeploy -f "$T/key" >/dev/null
ssh-keygen -q -t ed25519 -N '' -C other -f "$T/otherkey" >/dev/null
printf 'release@autodeploy namespaces="autodeploy-release" %s\n' "$(cut -d' ' -f1,2 "$T/key.pub")" >"$T/allowed_signers"
printf '# fixture\nrelease@autodeploy namespaces="autodeploy-release" ssh-ed25519 PLACEHOLDER-FIXTURE\n' >"$T/placeholder_signers"
V="$have"

# Behaviour equivalence: source ONLY the marked verify function out of install.sh and compare its
# verdict and message with verify-release.sh on the same fixtures.
sed -n '/^# BEGIN VERIFY-RELEASE LOGIC/,/^# END VERIFY-RELEASE LOGIC/p' "$INSTALL" >"$T/verify_fn.sh"
if [[ -s "$T/verify_fn.sh" ]]; then
  eval "$(cat "$T/verify_fn.sh")"
  mkfiles() {
    local d="$1"
    rm -rf "$d"; mkdir -m 0700 -p "$d"
    printf 'kit bytes\n' >"$d/autodeploy-host-kit-v0.tar.gz"
    printf 'bin bytes\n' >"$d/autodeploy-binaries-v0.tar.gz"
    printf '%s\n' "$V" >"$d/VERSION"
    (cd "$d" && sum autodeploy-host-kit-v0.tar.gz autodeploy-binaries-v0.tar.gz VERSION >SHA256SUMS)
  }
  signrel() { ssh-keygen -Y sign -q -f "${2:-$T/key}" -n "${3:-autodeploy-release}" "$1/SHA256SUMS"; }
  equiv() { # label dir allowed expect
    local label="$1" d="$2" a="$3" e="$4" o1 o2 r1 r2
    o1="$("$VERIFY" "$d" --allowed-signers "$a" --expect-version "$e" 2>&1)"; r1=$?
    o2="$(verify_release_dir "$d" "$a" "$e" 2>&1)"; r2=$?
    if [[ "$r1" == "$r2" && "$o1" == "$o2" ]]; then pass "verify equivalence: $label (rc=$r1)"
    else fail "verify equivalence: $label: script rc=$r1 '$o1' vs embedded rc=$r2 '$o2'"; fi
  }
  mkfiles "$T/e1"; signrel "$T/e1"; equiv "valid release" "$T/e1" "$T/allowed_signers" "$V"
  equiv "wrong expected version" "$T/e1" "$T/allowed_signers" "v9.9.9"
  equiv "placeholder key" "$T/e1" "$T/placeholder_signers" "$V"
  mkfiles "$T/e2"; signrel "$T/e2" "$T/otherkey"; equiv "signed by another key" "$T/e2" "$T/allowed_signers" "$V"
  mkfiles "$T/e3"; signrel "$T/e3" "$T/key" other-namespace; equiv "wrong namespace" "$T/e3" "$T/allowed_signers" "$V"
  mkfiles "$T/e4"; equiv "unsigned release" "$T/e4" "$T/allowed_signers" "$V"
  mkfiles "$T/e5"; signrel "$T/e5"; printf 'evil\n' >"$T/e5/autodeploy-host-kit-v0.tar.gz"; equiv "tampered asset" "$T/e5" "$T/allowed_signers" "$V"
  mkfiles "$T/e6"; signrel "$T/e6"; printf 'x\n' >"$T/e6/extra"; equiv "unlisted file" "$T/e6" "$T/allowed_signers" "$V"
  mkfiles "$T/e7"; signrel "$T/e7"; ln -s /etc/passwd "$T/e7/link"; equiv "symlink in dir" "$T/e7" "$T/allowed_signers" "$V"
  mkfiles "$T/e8"; signrel "$T/e8"; chmod 0777 "$T/e8"; equiv "world-writable dir" "$T/e8" "$T/allowed_signers" "$V"
  mkfiles "$T/e9"; printf 'bad line\n' >"$T/e9/SHA256SUMS"; signrel "$T/e9"; equiv "malformed SHA256SUMS" "$T/e9" "$T/allowed_signers" "$V"
  mkfiles "$T/e10"; printf 'v0.0.0-other\n' >"$T/e10/VERSION"; (cd "$T/e10" && sum autodeploy-host-kit-v0.tar.gz autodeploy-binaries-v0.tar.gz VERSION >SHA256SUMS); signrel "$T/e10"
  equiv "signed VERSION differs from expected" "$T/e10" "$T/allowed_signers" "$V"
  equiv "not a directory" "$T/nonexistent" "$T/allowed_signers" "$V"
else fail "no '# BEGIN VERIFY-RELEASE LOGIC' block in install.sh"; fi

# semver_cmp table (L9): extracted from install.sh and run on the host shell.
sed -n '/^semver_cmp() {/,/^}/p' "$INSTALL" >"$T/semver_fn.sh"
if [[ -s "$T/semver_fn.sh" ]]; then
  eval "$(cat "$T/semver_fn.sh")"
  semver_case() { # a b want
    local got; got="$(semver_cmp "$1" "$2" 2>/dev/null)" || got=ERR
    [[ "$got" == "$3" ]] && pass "semver_cmp $1 vs $2 = $3" || fail "semver_cmp $1 vs $2: got '$got', want $3"
  }
  semver_case v1.0.0-rc.2 v1.0.0-rc.10 -1
  semver_case v1.0.0-rc.10 v1.0.0-rc.2 1
  semver_case v1.0.0-rc.1 v1.0.0 -1
  semver_case v1.0.0 v1.0.0-rc.1 1
  semver_case v1.0.0-alpha v1.0.0-1 1
  semver_case v1.0.0-1 v1.0.0-alpha -1
  semver_case v1.0.0-alpha v1.0.0-alpha.1 -1
  semver_case v1.2.3 v1.2.3 0
  semver_case v1.0.0-rc.1 v1.0.0-rc.1 0
  semver_case v0.0.0-dev v0.0.0 -1
  semver_case v1.10.0 v1.9.9 1
  semver_case v2.0.0 v10.0.0 -1
  semver_case 1.2.3 v1.2.3 ERR
  semver_case v1.2 v1.2.3 ERR
  semver_case v01.2.3 v1.2.3 0
else fail "semver_cmp not found in install.sh"; fi

# ------------------------------------------------------------------------------------------ Part B
if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
  printf 'SKIP part B (container runs of install.sh): Docker not available\n'
  [[ $FAILS -eq 0 ]] || { printf '%d FAILED\n' "$FAILS" >&2; exit 1; }
  exit 0
fi

TEST_IMAGE="autodeploy-install-test:u2404-v1"
if ! docker image inspect "$TEST_IMAGE" >/dev/null 2>&1; then
  printf 'FROM ubuntu:24.04\nRUN apt-get update -qq && apt-get install -y -qq --no-install-recommends openssh-client netcat-openbsd >/dev/null && rm -rf /var/lib/apt/lists/*\n' \
    | docker build -q -t "$TEST_IMAGE" - >/dev/null 2>&1 || { fail "cannot build $TEST_IMAGE (needs network once)"; printf '%d FAILED\n' "$FAILS" >&2; exit 1; }
fi

FX="$T/fx"; mkdir -p "$FX"
# --- fixture repo: real package-release.sh, stubbed kit scripts, committed in a throwaway repo
R="$T/repo"; mkdir -p "$R/deploy"
cp -R "$KIT_DIR" "$R/deploy/host"
rm -rf "$R/deploy/host/static-git" "$R/deploy/host/tests" "$R/deploy/host/release-signing" "$R/deploy/host/install.sh"
H="$R/deploy/host"
stub() { printf '#!/bin/bash\n%s\n' "$2" >"$H/$1"; chmod 0755 "$H/$1"; }
stub install-checkout.sh 'echo "kit install-checkout.sh $*" >>/tmp/calls
id -u autodeploy-agent >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin autodeploy-agent >/dev/null 2>&1'
stub preflight-agent.sh 'echo "kit preflight-agent.sh $*" >>/tmp/calls
[[ -e /tmp/ctl/pffail ]] && { echo "FAIL simulated preflight failure"; exit 1; }
exit 0'
stub preflight-checkout.sh 'echo "kit preflight-checkout.sh $*" >>/tmp/calls; exit 0'
stub create-credentials.sh 'echo "kit create-credentials.sh $*" >>/tmp/calls
[[ -e /tmp/ctl/failcreds ]] && { echo "create-credentials: simulated failure" >&2; exit 1; }
exit 0'
stub enable-github.sh 'echo "kit enable-github.sh $*" >>/tmp/calls'
stub enroll-local.sh 'echo "kit enroll-local.sh $*" >>/tmp/calls
[[ -e /tmp/ctl/noadmin ]] && { echo "no administrator exists yet" >&2; exit 20; }
[[ -e /tmp/ctl/enrollfail ]] && { echo "enroll: simulated failure" >&2; exit 1; }
mkdir -p /var/lib/autodeploy/agent; : >/var/lib/autodeploy/agent/credential; touch /tmp/state-admin'
stub install-app.sh 'K="$(cd -- "$(dirname -- "$0")" && pwd)"
umask 022 # like the real script: traversable directories
echo "kit install-app.sh host=${AUTODEPLOY_CONTROLPLANE_HOST:-} $*" >>/tmp/calls
st=0; [[ " $* " == *" --acme-staging "* ]] && st=1
em=""; prev=""; role=all; cph="$AUTODEPLOY_CONTROLPLANE_HOST"
for a in "$@"; do
  [[ $prev == --acme-email ]] && em="$a"
  [[ $prev == --role ]] && role="$a"
  [[ $prev == --control-plane-host ]] && cph="$a"
  prev="$a"
done
mkdir -p /etc/autodeploy /usr/local/lib/autodeploy
printf "AUTODEPLOY_ENV_GATEWAY=127.0.0.1\nAUTODEPLOY_CONTROLPLANE_PORT=18081\nAUTODEPLOY_CONTROLPLANE_HOST=%s\nAUTODEPLOY_TLS_MODE=public\nAUTODEPLOY_ACME_STAGING=%s\nAUTODEPLOY_ACME_EMAIL=%s\n" "$cph" "$st" "$em" >/etc/autodeploy/host.env
[[ $role == agent ]] && echo AUTODEPLOY_ROLE=agent >>/etc/autodeploy/host.env
[[ -e /etc/autodeploy/agent-local.env ]] || printf "# Owner-edited\nAUTODEPLOY_AGENT_SERVER_ID=\n" >/etc/autodeploy/agent-local.env
if [[ $role == agent ]]; then
  mkdir -p /usr/local/lib/autodeploy/app /var/lib/autodeploy/agent
  chown autodeploy-agent:autodeploy-agent /var/lib/autodeploy/agent; chmod 0700 /var/lib/autodeploy/agent
  install -m 0755 "$K/libexec/fake-agent" /usr/local/lib/autodeploy/app/agent
fi
install -m 0755 "$K/libexec/autodeploy-run" /usr/local/lib/autodeploy/autodeploy-run'
# Fake agent binary (installed by the install-app stub for --role agent). Run as the agent user by install.sh:
# records argv, insists on an agent-owned 0600 token file, keeps what it read ONLY in /tmp/seen-token (test
# observation) and creates the credential unless /tmp/ctl/enrollfail exists.
cat >"$H/libexec/fake-agent" <<'STUBEOF'
#!/bin/bash
echo "agent $*" >>/tmp/calls
if [[ "${1:-}" == enroll ]]; then
  tf=""; prev=""; for a in "$@"; do [[ $prev == --token-file ]] && tf="$a"; prev="$a"; done
  [[ -n $tf && -f $tf && ! -L $tf && "$(stat -c %U:%a "$tf")" == autodeploy-agent:600 ]] || { echo "fake-agent: token file missing or not agent-owned 0600" >&2; exit 3; }
  [[ "$(stat -c %U:%G:%a "$(dirname "$tf")")" == root:autodeploy-agent:710 ]] || { echo "fake-agent: token dir must be root:agent 0710 (agent cannot create or replace entries)" >&2; exit 4; }
  env >/tmp/seen-env
  cat "$tf" >/tmp/seen-token
  [[ -e /tmp/ctl/enrollfail ]] && { echo "enroll: simulated failure" >&2; exit 1; }
  : >/var/lib/autodeploy/agent/credential
  echo "enrolled: credential_version=1" >&2
fi
exit 0
STUBEOF
chmod 0755 "$H/libexec/fake-agent"
cat >"$H/libexec/autodeploy-run" <<'STUBEOF'
#!/bin/bash
echo "run $* stdin=$(readlink /proc/self/fd/0)" >>/tmp/calls
case "$*" in
  migrate*) exit 0 ;;
  "admin setup-token")
    if [[ -e /tmp/state-admin ]]; then echo "an administrator already exists; first-run setup is closed" >&2; exit 1; fi
    echo "setup.v1.TESTCODE0123456789"; echo "setup code valid for 60 minutes; open /setup and enter it." >&2; exit 0 ;;
esac
exit 0
STUBEOF
chmod 0755 "$H/libexec/autodeploy-run"
git -C "$R" init -q . && git -C "$R" add -A && git -C "$R" -c user.name=t -c user.email=t@t commit -q -m fixture \
  || { fail "cannot build the fixture repo"; exit 1; }
BIN="$T/bin"; mkdir -p "$BIN"
for b in controlplane agent migrate admin secrets git; do printf 'fake %s\n' "$b" >"$BIN/$b"; done
(cd "$BIN" && sum controlplane agent migrate admin secrets git >SHA256SUMS)
if "$H/package-release.sh" --version "$V" --binaries "$BIN" --key "$T/key" --out "$FX/rel_ok" --allowed-signers "$T/allowed_signers" >"$T/pkg.out" 2>&1; then
  pass "fixture release built by the real package-release.sh"
else fail "package-release.sh failed: $(cat "$T/pkg.out")"; fi
cp "$T/allowed_signers" "$FX/allowed_signers"
cp "$INSTALL" "$FX/install.sh"
sed "s/^AUTODEPLOY_VERSION=.*/AUTODEPLOY_VERSION=\"v1.2.3\"/" "$INSTALL" >"$FX/install_real_version.sh"
cp "$T/placeholder_signers" "$FX/placeholder_signers"

resign() { # DIR KEY: regenerate SHA256SUMS over every asset and sign it
  local d="$1" k="${2:-$T/key}" f names=()
  rm -f "$d/SHA256SUMS" "$d/SHA256SUMS.sig"
  for f in "$d"/*; do names+=("${f##*/}"); done
  (cd "$d" && sum "${names[@]}" >SHA256SUMS)
  ssh-keygen -Y sign -q -f "$k" -n autodeploy-release "$d/SHA256SUMS"
}
clone_rel() { rm -rf "$FX/$1"; cp -R "$FX/rel_ok" "$FX/$1"; chmod 0755 "$FX/$1"; }
KITTGZ="autodeploy-host-kit-$V.tar.gz"
clone_rel rel_otherkey; resign "$FX/rel_otherkey" "$T/otherkey"
clone_rel rel_vermismatch; printf 'v9.9.9\n' >"$FX/rel_vermismatch/VERSION"; resign "$FX/rel_vermismatch"
clone_rel rel_tampered; printf 'tampered\n' >>"$FX/rel_tampered/$KITTGZ"
clone_rel rel_traversal; clone_rel rel_absolute; clone_rel rel_symlink
python3 -I - "$FX" "$KITTGZ" "$V" <<'PYEOF'
import io, sys, tarfile
fx, name, ver = sys.argv[1:4]
def build(dst, member, link=None):
    with tarfile.open(f"{fx}/{dst}/{name}", "w:gz") as t:
        ok = tarfile.TarInfo(f"autodeploy-host-kit-{ver}/README.md"); ok.size = 2
        t.addfile(ok, io.BytesIO(b"x\n"))
        m = tarfile.TarInfo(member)
        if link:
            m.type = tarfile.SYMTYPE; m.linkname = link; t.addfile(m)
        else:
            m.size = 5; t.addfile(m, io.BytesIO(b"evil\n"))
build("rel_traversal", f"autodeploy-host-kit-{ver}/../../escape")
build("rel_absolute", "/tmp/escape-abs")
build("rel_symlink", f"autodeploy-host-kit-{ver}/link", "/etc")
PYEOF
for r in rel_traversal rel_absolute rel_symlink; do resign "$FX/$r"; done

IFS= read -r -d '' DRIVER <<'DRIVEREOF' || true
set -u
SCEN="$1"; INSTALLER="$2"; shift 2
RC=0
ck() { if eval "$2"; then echo "CHECK ok   [$SCEN] $1"; else echo "CHECK FAIL [$SCEN] $1"; RC=1; fi; }
S=/tmp/stubs; mkdir -p $S /tmp/ctl /run/systemd/system /etc/apt/keyrings
: >/tmp/calls; chmod 0666 /tmp/calls
cat >$S/uname <<'X'
#!/bin/bash
if [[ "${1:-}" == -m ]]; then [[ -e /tmp/ctl/arch ]] && cat /tmp/ctl/arch || echo x86_64; else exec /usr/bin/uname "$@"; fi
X
cat >$S/systemctl <<'X'
#!/bin/bash
echo "systemctl $*" >>/tmp/calls
case "${1:-}" in
  is-active) [[ -e "/tmp/ctl/active-${*: -1}" ]] ;;
  *) exit 0 ;;
esac
X
cat >$S/apt-get <<'X'
#!/bin/bash
echo "apt-get $*" >>/tmp/calls
echo "needrestart=${NEEDRESTART_MODE:-unset} frontend=${DEBIAN_FRONTEND:-unset}" >>/tmp/apt.env
if [[ " $* " == *" docker-ce "* ]]; then
  cat >/usr/bin/docker <<'D'
#!/bin/bash
echo "docker $*" >>/tmp/calls
exit 0
D
  chmod +x /usr/bin/docker
  echo docker-ce >>/tmp/ctl/pkgs
fi
exit 0
X
cat >$S/dpkg-query <<'X'
#!/bin/bash
pkg="${*: -1}"
grep -qx -- "$pkg" /tmp/ctl/pkgs 2>/dev/null && { printf 'install ok installed'; exit 0; }
exit 1
X
cat >$S/gpg <<'X'
#!/bin/bash
f="${*: -1}"
echo "gpg $*" >>/tmp/calls
if grep -q GOODKEY "$f"; then
  echo "pub:-:4096:1:8D81803C0EBFCD88:1487788586:::-:::escaESCA::::::23::0:"
  echo "fpr:::::::::9DC858229FC7DD38854AE2D88D81803C0EBFCD88:"
  echo "sub:-:4096:1:7EA0A9C3F273FCD8:1487788586::::::s::::::23:"
  echo "fpr:::::::::D3306A018370199E527AE7997EA0A9C3F273FCD8:"
elif grep -q TWOKEYS "$f"; then
  echo "pub:-:4096:1:8D81803C0EBFCD88:1487788586:::-:::escaESCA::::::23::0:"
  echo "fpr:::::::::9DC858229FC7DD38854AE2D88D81803C0EBFCD88:"
  echo "pub:-:4096:1:AAAAAAAAAAAAAAAA:1487788586:::-:::escaESCA::::::23::0:"
  echo "fpr:::::::::0000000000000000000000000000000000000000:"
elif grep -q SUBONLY "$f"; then
  echo "pub:-:4096:1:AAAAAAAAAAAAAAAA:1487788586:::-:::escaESCA::::::23::0:"
  echo "fpr:::::::::0000000000000000000000000000000000000000:"
  echo "sub:-:4096:1:8D81803C0EBFCD88:1487788586::::::s::::::23:"
  echo "fpr:::::::::9DC858229FC7DD38854AE2D88D81803C0EBFCD88:"
else
  echo "pub:-:4096:1:AAAAAAAAAAAAAAAA:1487788586:::-:::escaESCA::::::23::0:"
  echo "fpr:::::::::0000000000000000000000000000000000000000:"
fi
X
cat >$S/date <<'X'
#!/bin/bash
echo "date-stub-called" >>/tmp/calls
exec /usr/bin/date "$@"
X
cat >$S/sleep <<'X'
#!/bin/bash
exit 0
X
cat >$S/curl <<'X'
#!/bin/bash
echo "curl $*" >>/tmp/calls
out=""; url=""; prev=""
for a in "$@"; do [[ $prev == -o ]] && out="$a"; [[ $a == https://* ]] && url="$a"; prev="$a"; done
case "$url" in
  *download.docker.com*) if [[ -e /tmp/ctl/keykind ]]; then cat /tmp/ctl/keykind >"$out"; else echo GOODKEY >"$out"; fi ;;
  *releases/download/*) f="/fx/${RELSERVE:-rel_ok}/${url##*/}"; [[ -f $f ]] || exit 22; cp "$f" "$out" ;;
  */readyz)
    [[ -e /tmp/ctl/cpdown ]] && exit 7
    if [[ -e /tmp/ctl/cpdown-after-first ]]; then
      [[ -e /tmp/ctl/readyz-seen ]] && exit 7
      : >/tmp/ctl/readyz-seen
    fi
    [[ -n $out ]] && : >"$out" ;;
  *) exit 22 ;;
esac
X
cat >$S/ss <<'X'
#!/bin/bash
[[ -e /tmp/ctl/ss ]] && cat /tmp/ctl/ss; exit 0
X
cat >$S/ip <<'X'
#!/bin/bash
echo "1.1.1.1 via 10.0.0.1 dev eth0 src 203.0.113.10 uid 0"
X
cat >$S/dig <<'X'
#!/bin/bash
if [[ " $* " == *" AAAA "* ]]; then exit 0; fi
[[ -e /tmp/ctl/dns ]] && cat /tmp/ctl/dns || echo 203.0.113.10
X
cat >$S/nproc <<'X'
#!/bin/bash
[[ -e /tmp/ctl/nproc ]] && cat /tmp/ctl/nproc || echo 4
X
cat >$S/free <<'X'
#!/bin/bash
echo "               total        used        free"
echo "Mem:           $( [[ -e /tmp/ctl/memmb ]] && cat /tmp/ctl/memmb || echo 4096 )        100        3000"
X
cat >$S/df <<'X'
#!/bin/bash
echo "Filesystem 1024-blocks Used Available Capacity Mounted"
echo "/dev/vda1 99999999 1000 $( [[ -e /tmp/ctl/diskkb ]] && cat /tmp/ctl/diskkb || echo 90000000 ) 1% /"
X
cat >$S/stat <<'X'
#!/bin/bash
if [[ "$*" == "-fc %T /sys/fs/cgroup" ]]; then [[ -e /tmp/ctl/cgroup1 ]] && echo tmpfs || echo cgroup2fs; else exec /usr/bin/stat "$@"; fi
X
chmod +x $S/*
export PATH="$S:$PATH"
# docker-ce installed, active, at /usr/bin/docker by default (the Docker-install scenarios remove it).
# install.sh resets PATH; the stubs are reachable only through the dev-build test hook below.
cp /dev/null /tmp/ctl/active-docker
echo docker-ce >/tmp/ctl/pkgs
cat >/usr/bin/docker <<'X'
#!/bin/bash
echo "docker $*" >>/tmp/calls
[[ "${1:-}" == buildx && -e /tmp/ctl/nobuildx ]] && exit 1
[[ "${1:-}" == exec && -e /tmp/ctl/pgdown ]] && exit 1
exit 0
X
chmod +x /usr/bin/docker
export AUTODEPLOY_INSTALL_TEST_PATH=/tmp/stubs
printf 'HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n' >/tmp/readyz.resp
nohup bash -c 'while true; do nc -l 127.0.0.1 18081 </tmp/readyz.resp >>/tmp/readyz.req 2>/dev/null; done' >/dev/null 2>&1 &
printf 'HTTP/1.1 401 Unauthorized\r\nContent-Length: 0\r\n\r\n' >/tmp/api401.resp
nohup bash -c 'while true; do nc -l 127.0.0.1 8080 </tmp/api401.resp >/dev/null 2>&1; done' >/dev/null 2>&1 &
mkdir -p /fxrw
BASE=(--domain cp.example.com --email ops@example.com --firewall no --allow-dev --yes-i-am-testing --allowed-signers /fx/allowed_signers --release-dir /fx/rel_ok)
run() { # args...: run the installer, capture rc/out/err
  bash "/fx/$INSTALLER" "$@" >/tmp/out 2>/tmp/err </dev/null; IRC=$?
  cat /tmp/out /tmp/err | sed 's/^/  | /' | tail -n 6
}
feed() { # TEXT: wait for the "Proceed?" prompt, answer y, wait for the token prompt, then type TEXT (hidden prompt)
  local i
  for i in $(seq 1 150); do grep -q "Proceed? " /tmp/ttyout 2>/dev/null && break; /usr/bin/sleep 0.1; done
  /usr/bin/sleep 0.3; printf 'y\n'
  for i in $(seq 1 150); do grep -q "Paste the one-time enrollment token" /tmp/ttyout 2>/dev/null && break; /usr/bin/sleep 0.1; done
  /usr/bin/sleep 0.4; printf '%s\n' "$1"
}
nocall() { ! grep -qE "$1" /tmp/calls; }
nothing_changed() {
  ! grep -qE '^(kit |apt-get|run |systemctl start|systemctl enable)' /tmp/calls \
    && ! grep -q 'download.docker.com' /tmp/calls \
    && [[ ! -e /etc/apt/keyrings/docker.asc && ! -e /etc/apt/sources.list.d/docker.sources ]] \
    && [[ ! -e /etc/autodeploy/installer.env ]]
}
TOK=enr.v1.AbCdEfGhIjKlMnOpQrStUvWxYz0123456789-_ABCDE
AB=(--role agent --control-plane-host cp.example.com --server-id server_abc123 --email ops@example.com --firewall no --allow-dev --yes-i-am-testing --allowed-signers /fx/allowed_signers --release-dir /fx/rel_ok)
nothing_changed_except_env() {
  ! grep -qE '^(kit |apt-get|run |systemctl start|systemctl enable)' /tmp/calls && ! grep -q 'download.docker.com' /tmp/calls
}
tmpgone() { ! ls -d /var/tmp/autodeploy-install.* >/dev/null 2>&1; }
msg() { grep -qi -- "$1" /tmp/out /tmp/err; }
order_ok() { # patterns in order (first match of each must be strictly increasing)
  local last=0 n p
  for p in "$@"; do
    n="$(grep -nE -- "$p" /tmp/calls | head -n1 | cut -d: -f1)"
    [[ -n $n ]] && ((n > last)) || { echo "order broken at: $p (line '${n:-none}' after $last)"; return 1; }
    last=$n
  done
}

case "$SCEN" in
os-debian)
  printf 'ID=debian\nVERSION_ID="12"\nPRETTY_NAME="Debian GNU/Linux 12"\n' >/etc/os-release
  run "${BASE[@]}"
  ck "refused" '((IRC != 0))'; ck "message names Ubuntu 24.04" 'msg "Ubuntu 24.04"'; ck "nothing ran" 'nothing_changed'; ck "tmp clean" 'tmpgone' ;;
os-2204)
  printf 'ID=ubuntu\nVERSION_ID="22.04"\n' >/etc/os-release
  run "${BASE[@]}"
  ck "refused" '((IRC != 0))'; ck "message names 24.04" 'msg "24.04"'; ck "nothing ran" 'nothing_changed' ;;
arch-arm)
  echo aarch64 >/tmp/ctl/arch
  run "${BASE[@]}"
  ck "refused" '((IRC != 0))'; ck "message names x86_64" 'msg "x86_64"'; ck "nothing ran" 'nothing_changed' ;;
not-root)
  useradd -m nobody2 >/dev/null 2>&1; cp /fx/install.sh /tmp/i.sh; chmod 644 /tmp/i.sh
  su nobody2 -s /bin/bash -c 'bash /tmp/i.sh --domain a.example.com' >/tmp/out 2>/tmp/err; IRC=$?
  ck "refused when not root" '((IRC != 0)) && msg "root"' ;;
res-ram)
  echo 1024 >/tmp/ctl/memmb; run "${BASE[@]}"
  ck "RAM refusal" '((IRC != 0)) && msg "2 GB"'; ck "nothing ran" 'nothing_changed' ;;
res-cpu)
  echo 1 >/tmp/ctl/nproc; run "${BASE[@]}"
  ck "vCPU refusal" '((IRC != 0)) && msg "vCPU"'; ck "nothing ran" 'nothing_changed' ;;
res-disk)
  echo 1000000 >/tmp/ctl/diskkb; run "${BASE[@]}"
  ck "disk refusal" '((IRC != 0)) && msg "40 GB"'; ck "nothing ran" 'nothing_changed' ;;
res-cgroup)
  : >/tmp/ctl/cgroup1; run "${BASE[@]}"
  ck "cgroup v2 refusal" '((IRC != 0)) && msg "cgroup v2"'; ck "nothing ran" 'nothing_changed' ;;
res-systemd)
  rmdir /run/systemd/system; run "${BASE[@]}"
  ck "systemd refusal" '((IRC != 0)) && msg "systemd"'; ck "nothing ran" 'nothing_changed' ;;
ports-busy)
  echo 'LISTEN 0 511 0.0.0.0:80 0.0.0.0:* users:(("nginx",pid=812,fd=6))' >/tmp/ctl/ss
  run "${BASE[@]}"
  ck "refused" '((IRC != 0))'; ck "names the holder" 'msg "nginx"'; ck "nothing ran" 'nothing_changed' ;;
docker-io)
  echo docker.io >>/tmp/ctl/pkgs
  run "${BASE[@]}"
  ck "docker.io refused" '((IRC != 0)) && msg "docker.io"'; ck "explains the official repo" 'msg "docker-ce"'; ck "nothing ran" 'nothing_changed' ;;
docker-snap)
  cat >$S/snap <<'X'
#!/bin/bash
[[ "$1 $2" == "list docker" ]] && { echo "docker 27 snap"; exit 0; }; exit 1
X
  chmod +x $S/snap
  run "${BASE[@]}"
  ck "snap docker refused" '((IRC != 0)) && msg "snap"'; ck "nothing ran" 'nothing_changed' ;;
docker-install)
  rm -f /usr/bin/docker /tmp/ctl/active-docker; : >/tmp/ctl/pkgs
  run "${BASE[@]}"
  ck "success" '((IRC == 0))'
  ck "key fingerprint checked with gpg before install" 'grep -q "^gpg .*--show-keys --with-colons" /tmp/calls && order_ok "^curl .*download.docker.com/linux/ubuntu/gpg" "^gpg .*--show-keys" "^apt-get .*install.* docker-ce "'
  ck "every apt-get call has the lock timeout, needrestart is automatic" '! grep "^apt-get " /tmp/calls | grep -v -- "-o DPkg::Lock::Timeout=600" | grep -q . && ! grep -v "needrestart=a frontend=noninteractive" /tmp/apt.env | grep -q .'
  ck "docker used only as /usr/bin/docker" '[[ -x /usr/bin/docker ]]'
  ck "official key fetched over https into /etc/apt/keyrings/docker.asc" '[[ -s /etc/apt/keyrings/docker.asc ]] && grep -q "curl .*--proto =https.*download.docker.com/linux/ubuntu/gpg" /tmp/calls'
  ck "deb822 source with Signed-By" 'grep -q "^URIs: https://download.docker.com/linux/ubuntu" /etc/apt/sources.list.d/docker.sources && grep -q "^Signed-By: /etc/apt/keyrings/docker.asc" /etc/apt/sources.list.d/docker.sources && grep -q "^Suites: noble" /etc/apt/sources.list.d/docker.sources'
  ck "installs docker-ce, cli, containerd.io, buildx" 'grep -E "^apt-get -o DPkg::Lock::Timeout=600 install" /tmp/calls | grep -q "docker-ce docker-ce-cli containerd.io docker-buildx-plugin"'
  ck "does not install docker.io" '! grep -q "install.*docker\.io" /tmp/calls'
  ck "docker enabled" 'grep -q "systemctl enable --now docker" /tmp/calls' ;;
docker-usrlocal)
  rm -f /usr/bin/docker; : >/tmp/ctl/pkgs; mkdir -p /usr/local/bin; printf '#!/bin/bash\nexit 0\n' >/usr/local/bin/docker; chmod +x /usr/local/bin/docker
  run "${BASE[@]}"
  ck "docker only at /usr/local/bin refused with an explanation" '((IRC != 0)) && msg "/usr/bin/docker"'; ck "nothing changed (no docker repo, no apt)" 'nothing_changed' ;;
docker-podman)
  echo podman-docker >>/tmp/ctl/pkgs
  run "${BASE[@]}"
  ck "podman-docker shim refused" '((IRC != 0)) && msg "podman"'; ck "nothing changed" 'nothing_changed' ;;
docker-not-ce)
  : >/tmp/ctl/pkgs
  run "${BASE[@]}"
  ck "/usr/bin/docker without the docker-ce package refused" '((IRC != 0)) && msg "docker-ce"'; ck "nothing changed" 'nothing_changed' ;;
docker-badkey)
  rm -f /usr/bin/docker /tmp/ctl/active-docker; : >/tmp/ctl/pkgs
  for kind in BADKEY TWOKEYS SUBONLY; do
    echo "$kind" >/tmp/ctl/keykind; : >/tmp/calls
    run "${BASE[@]}"
    ck "$kind: refused" '((IRC != 0)) && msg "fingerprint"'
    ck "$kind: nothing written under /etc/apt, docker not installed, no kit script" '[[ ! -e /etc/apt/keyrings/docker.asc && ! -e /etc/apt/sources.list.d/docker.sources ]] && ! grep -q "install.* docker-ce " /tmp/calls && ! grep -q "^kit " /tmp/calls'
  done ;;
docker-nobuildx)
  : >/tmp/ctl/nobuildx; run "${BASE[@]}"
  ck "active docker without buildx triggers the CE install" 'grep -E "^apt-get -o DPkg::Lock::Timeout=600 install" /tmp/calls | grep -q docker-buildx-plugin' ;;
noninteractive-missing)
  run --allow-dev --yes-i-am-testing --allowed-signers /fx/allowed_signers --release-dir /fx/rel_ok
  ck "refused" '((IRC != 0))'
  ck "lists the flags" 'msg "--domain" && msg "--email" && msg "--firewall"'
  ck "nothing ran" 'nothing_changed'; ck "tmp clean" 'tmpgone' ;;
noninteractive-fw-yes-noport)
  run --domain cp.example.com --email ops@example.com --firewall yes --allow-dev --yes-i-am-testing --allowed-signers /fx/allowed_signers --release-dir /fx/rel_ok
  ck "ssh port required when unattended" '((IRC != 0)) && msg "--ssh-port"'; ck "nothing ran" 'nothing_changed' ;;
unattended)
  run "${BASE[@]}"
  ck "exit 0" '((IRC == 0))'
  ck "kit/stack call order matches vps-install.md section 4" 'order_ok "^kit install-checkout.sh" "^kit install-app.sh" "^kit create-credentials.sh" "^systemctl start autodeploy-postgres autodeploy-traefik" "^run migrate" "^kit enable-github.sh --kek-only" "^systemctl start autodeploy-controlplane" "^run admin setup-token" "^kit enroll-local.sh"'
  ck "install-app got mode public, host, email, BINDIR" 'grep -E "^kit install-app.sh host=cp.example.com --mode public --acme-email ops@example.com .*/binaries$" /tmp/calls'
  ck "no firewall flag when --firewall no" '! grep -q -- "--configure-firewall" /tmp/calls'
  ck "apt deps installed" 'grep -E "^apt-get -o DPkg::Lock::Timeout=600 install" /tmp/calls | grep -q "apache2-utils" && grep -E "^apt-get -o DPkg::Lock::Timeout=600 install" /tmp/calls | grep -q dnsutils'
  ck "enroll-local default wait 60m" 'grep -q "^kit enroll-local.sh --wait 60m" /tmp/calls'
  ck "setup URL printed" 'grep -q "https://cp.example.com/setup" /tmp/out'
  ck "setup code printed exactly once to the terminal" '[[ "$(grep -c "setup.v1.TESTCODE0123456789" /tmp/out)" == 1 ]] && ! grep -q "setup.v1.TESTCODE0123456789" /tmp/err'
  ck "setup code absent from the log file" '! grep -q "TESTCODE0123456789" /var/log/autodeploy-install.log && [[ -s /var/log/autodeploy-install.log ]]'
  ck "setup code absent from the call log" '! grep -q TESTCODE /tmp/calls'
  ck "log is root-only 0600" '[[ "$(stat -c %a /var/log/autodeploy-install.log)" == 600 ]]'
  ck "installed-version recorded" '[[ "$(cat /etc/autodeploy/installed-version)" == "'"$(sed -n 's/^AUTODEPLOY_VERSION="\(.*\)"$/\1/p' /fx/install.sh)"'" ]]'
  ck "key fingerprint printed" 'grep -q "SHA256:" /tmp/out'
  ck "final summary names the dashboard URL" 'grep -q "https://cp.example.com" /tmp/out'
  ck "verified kit kept on the host, root-owned, no leftovers" '[[ -x /usr/local/share/autodeploy/kit/enroll-local.sh ]] && [[ "$(stat -c "%u %a" /usr/local/share/autodeploy)" == "0 755" ]] && [[ "$(stat -c %u /usr/local/share/autodeploy/kit/enroll-local.sh)" == 0 ]] && [[ ! -e /usr/local/share/autodeploy/kit.new && ! -e /usr/local/share/autodeploy/kit.old ]]'
  ck "apt-get calls carry the lock timeout; needrestart automatic" '! grep "^apt-get " /tmp/calls | grep -v -- "-o DPkg::Lock::Timeout=600" | grep -q . && grep -q "needrestart=a frontend=noninteractive" /tmp/apt.env'
  ck "readyz probe sends Host: host:port" 'for _i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do grep -q "^Host: 127.0.0.1:18081" /tmp/readyz.req 2>/dev/null && break; /usr/bin/sleep 0.2; done; grep -q "^Host: 127.0.0.1:18081" /tmp/readyz.req || { echo "DEBUG req: $(od -c /tmp/readyz.req 2>&1 | head -5)"; false; }'
  ck "choices persisted in installer.env (0600)" '[[ "$(stat -c %a /etc/autodeploy/installer.env)" == 600 ]] && grep -qx "INSTALLER_DOMAIN=cp.example.com" /etc/autodeploy/installer.env && grep -qx "INSTALLER_FIREWALL=no" /etc/autodeploy/installer.env && ! grep -q TESTCODE /etc/autodeploy/installer.env'
  ck "tmp dir removed after success" 'tmpgone' ;;
unattended-options)
  run "${BASE[@]}" --acme-staging --firewall yes --ssh-port 2222 --setup-wait 5m
  ck "exit 0" '((IRC == 0))'
  ck "staging flag mapped" 'grep -E "^kit install-app.sh .*--acme-staging" /tmp/calls'
  ck "firewall mapped with the confirmed ssh port" 'grep -E "^kit install-app.sh .*--configure-firewall .*--yes-firewall-ssh-port=2222" /tmp/calls'
  ck "ufw installed with the apt deps" 'grep -E "^apt-get -o DPkg::Lock::Timeout=600 install" /tmp/calls | grep -q ufw'
  ck "setup wait mapped" 'grep -q "^kit enroll-local.sh --wait 5m" /tmp/calls' ;;
download)
  run --domain cp.example.com --email ops@example.com --firewall no --allow-dev --yes-i-am-testing --allowed-signers /fx/allowed_signers
  ck "exit 0" '((IRC == 0))'
  ck "downloaded the five derived assets from the release URL" '[[ "$(grep -c "releases/download/'"$VER"'/" /tmp/calls)" == 5 ]] && grep -q "https://github.com/Nani-Boddeti/autoDeploy/releases/download/'"$VER"'/SHA256SUMS.sig" /tmp/calls'
  ck "every curl call pins --proto =https and --tlsv1.2" '! grep "^curl " /tmp/calls | grep -v -- "--proto =https" | grep -q . && ! grep "^curl " /tmp/calls | grep -v -- "--tlsv1.2" | grep -q .'
  ck "curl was used" 'grep -q "^curl " /tmp/calls' ;;
sig-fail|ver-fail|tampered|traversal|absolute|symlink)
  run "${BASE[@]/\/fx\/rel_ok//fx/$REL}"
  ck "refused" '((IRC != 0))'
  ck "no kit script, apt step, or systemctl ran" 'nothing_changed'
  ck "no escape file created" '[[ ! -e /tmp/escape-abs && ! -e /fx/escape && ! -e /escape ]]'
  ck "tmp clean after failure" 'tmpgone'
  case "$SCEN" in
    traversal|absolute) ck "refused by the installer's own member-path check" 'msg "unsafe member path"' ;;
    symlink) ck "refused by the installer's own member-type check" 'msg "unsafe member type"' ;;
    *) ck "explains" 'msg "verification failed"' ;;
  esac ;;
dl-sig-fail)
  run --domain cp.example.com --email ops@example.com --firewall no --allow-dev --yes-i-am-testing --allowed-signers /fx/allowed_signers
  ck "download-mode signature failure refused" '((IRC != 0)) && msg "verification failed"'
  ck "assets were fetched over https only, then nothing ran" 'grep -q "^curl --proto =https" /tmp/calls && nothing_changed'
  ck "tmp clean" 'tmpgone' ;;
placeholder)
  run --domain cp.example.com --email ops@example.com --firewall no --release-dir /fx/rel_ok
  ck "real-version installer with the placeholder key refused" '((IRC != 0)) && msg "PLACEHOLDER"'
  ck "nothing ran" 'nothing_changed' ;;
placeholder-allow-dev-nokey)
  run --domain cp.example.com --email ops@example.com --firewall no --allow-dev --yes-i-am-testing --release-dir /fx/rel_ok
  ck "allow-dev alone does not bypass the placeholder" '((IRC != 0)) && msg "PLACEHOLDER"'; ck "nothing ran" 'nothing_changed' ;;
dev-version-refused)
  run --domain cp.example.com --email ops@example.com --firewall no --release-dir /fx/rel_ok
  ck "dev version refused without --allow-dev" '((IRC != 0)) && msg "allow-dev"'; ck "nothing ran" 'nothing_changed' ;;
signers-without-dev)
  run --domain cp.example.com --email ops@example.com --firewall no --allowed-signers /fx/allowed_signers --release-dir /fx/rel_ok
  ck "--allowed-signers needs --allow-dev" '((IRC != 0)) && msg "allow-dev"'; ck "nothing ran" 'nothing_changed' ;;
rerun)
  run "${BASE[@]}"; ck "first run ok" '((IRC == 0))'
  : >/tmp/calls; cp /tmp/out /tmp/out1; touch /usr/local/share/autodeploy/kit/stale-marker
  run "${BASE[@]}"
  ck "rerun ok" '((IRC == 0))'
  ck "rerun does not mint a setup token" '! grep -q "admin setup-token" /tmp/calls && ! grep -q TESTCODE /tmp/out'
  ck "rerun does not call enroll-local (already enrolled)" '! grep -q "enroll-local.sh" /tmp/calls'
  ck "rerun still runs install-app (upgrade) and migrate" 'grep -q "^kit install-app.sh" /tmp/calls && grep -q "^run migrate" /tmp/calls'
  ck "rerun mentions upgrade" 'msg "upgrade"'
  ck "kit copy replaced atomically (stale file gone, no leftovers)" '[[ ! -e /usr/local/share/autodeploy/kit/stale-marker && -x /usr/local/share/autodeploy/kit/install-app.sh && ! -e /usr/local/share/autodeploy/kit.new && ! -e /usr/local/share/autodeploy/kit.old ]]' ;;
rerun-admin-exists)
  touch /tmp/state-admin; run "${BASE[@]}"
  ck "ok" '((IRC == 0))'
  ck "setup-token attempted but refused: no code printed" 'grep -q "admin setup-token" /tmp/calls && ! grep -q TESTCODE /tmp/out /tmp/err'
  ck "enroll-local still called" 'grep -q "^kit enroll-local.sh" /tmp/calls' ;;
rerun-noargs)
  run "${BASE[@]}"; : >/tmp/calls
  run --allow-dev --yes-i-am-testing --allowed-signers /fx/allowed_signers --release-dir /fx/rel_ok
  ck "rerun without domain/email/firewall reuses the recorded values" '((IRC == 0)) && grep -q "^kit install-app.sh host=cp.example.com .*--acme-email ops@example.com" /tmp/calls' ;;
downgrade)
  mkdir -p /etc/autodeploy; echo v9.0.0 >/etc/autodeploy/installed-version
  run "${BASE[@]}"
  ck "downgrade refused" '((IRC != 0)) && msg "downgrade"'; ck "nothing ran" 'nothing_changed'
  run "${BASE[@]}" --allow-downgrade
  ck "allowed with --allow-downgrade" '((IRC == 0))'
  ck "version file now the installed one" '[[ "$(cat /etc/autodeploy/installed-version)" != v9.0.0 ]]' ;;
other-domain)
  mkdir -p /etc/autodeploy; printf 'AUTODEPLOY_CONTROLPLANE_HOST=old.example.org\nAUTODEPLOY_TLS_MODE=public\n' >/etc/autodeploy/host.env
  run "${BASE[@]}"
  ck "different domain refused" '((IRC != 0)) && msg "old.example.org"'; ck "nothing ran" 'nothing_changed' ;;
lan-host)
  mkdir -p /etc/autodeploy; printf 'AUTODEPLOY_CONTROLPLANE_HOST=cp.example.com\nAUTODEPLOY_TLS_MODE=lan\n' >/etc/autodeploy/host.env
  run "${BASE[@]}"
  ck "lan host refused" '((IRC != 0)) && msg "lan"'; ck "nothing ran" 'nothing_changed' ;;
dns-mismatch)
  echo 198.51.100.7 >/tmp/ctl/dns
  run "${BASE[@]}"
  ck "unattended without --yes aborts on DNS mismatch" '((IRC != 0)) && msg "198.51.100.7" && msg "--yes"'; ck "nothing ran" 'nothing_changed'
  run "${BASE[@]}" --yes
  ck "continues with --yes and warns" '((IRC == 0)) && msg "DNS"' ;;
dns-none)
  : >/tmp/ctl/dns
  run "${BASE[@]}"
  ck "no record: abort without --yes" '((IRC != 0)) && msg "no A record\|does not resolve"' ;;
bad-domain)
  run --domain 10.0.0.1 --email ops@example.com --firewall no --allow-dev --yes-i-am-testing --allowed-signers /fx/allowed_signers --release-dir /fx/rel_ok
  ck "IP literal refused" '((IRC != 0)) && msg "domain"'
  run --domain app.test --email ops@example.com --firewall no --allow-dev --yes-i-am-testing --allowed-signers /fx/allowed_signers --release-dir /fx/rel_ok
  ck "*.test refused" '((IRC != 0)) && msg "domain"'; ck "nothing ran" 'nothing_changed' ;;
interactive)
  command -v script >/dev/null || { echo "CHECK ok   [interactive] (script not available: skipped)"; echo "RESULT rc=0"; exit 0; }
  printf 'I AM TESTING\ncp.example.com\nops@example.com\nno\ny\n' >/fxrw/answers
  script -qec "bash /fx/$INSTALLER --allow-dev --allowed-signers /fx/allowed_signers --release-dir /fx/rel_ok" /dev/null </fxrw/answers >/tmp/out 2>/tmp/err; IRC=$?
  sed 's/^/  | /' /tmp/out | tail -n 5
  ck "prompted run succeeds" '((IRC == 0))'
  ck "answers reached install-app" 'grep -q "^kit install-app.sh host=cp.example.com --mode public --acme-email ops@example.com" /tmp/calls'
  ck "prompts were shown on the terminal" 'grep -q "Control plane domain" /tmp/out'
  ck "dev build asked for the typed confirmation on the terminal" 'grep -q "I AM TESTING" /tmp/out'
  ck "setup-token and migrate run with stdin from /dev/null (not the terminal)" 'grep "^run admin setup-token" /tmp/calls | grep -q "stdin=/dev/null" && grep "^run migrate" /tmp/calls | grep -q "stdin=/dev/null"' ;;
real-version-bypass)
  # H1: a RELEASE-version installer must never accept the testing flags, in any order.
  run --domain cp.example.com --email ops@example.com --firewall no --allow-dev --allowed-signers /fx/allowed_signers --yes-i-am-testing
  ck "release version + --allow-dev + --allowed-signers refused" '((IRC != 0)) && msg "development builds"'
  ck "refused before any download, verify, apt or kit step" 'nothing_changed && ! grep -q "^curl " /tmp/calls'
  run --allowed-signers /fx/allowed_signers --allow-dev --release-dir /fx/rel_ok --domain cp.example.com --email ops@example.com --firewall no
  ck "other flag order refused too" '((IRC != 0)) && msg "development builds" && nothing_changed'
  run --allow-dev --domain cp.example.com --email ops@example.com --firewall no --release-dir /fx/rel_ok
  ck "--allow-dev alone refused on a release version" '((IRC != 0)) && msg "development builds"'
  AUTODEPLOY_ALLOW_DEV=1 AUTODEPLOY_ALLOWED_SIGNERS=/fx/allowed_signers run --domain cp.example.com --email ops@example.com --firewall no --release-dir /fx/rel_ok
  ck "environment variables do not unlock anything" '((IRC != 0)) && msg "PLACEHOLDER" && nothing_changed' ;;
release-hook-ignored)
  # L9: the stub PATH hook is honoured only by dev builds. `date` is called (LOG header) before the policy gate,
  # so a stub date that logs proves whether the hook put /tmp/stubs on PATH.
  run --domain cp.example.com --email ops@example.com --firewall no --release-dir /fx/rel_ok
  ck "release build refused at the placeholder gate" '((IRC != 0)) && msg "PLACEHOLDER"'
  ck "release build never ran the stubbed date (hook ignored)" '! grep -q "^date-stub-called" /tmp/calls'
  : >/tmp/calls
  bash /fx/install.sh "${BASE[@]}" </dev/null >/tmp/out 2>/tmp/err
  ck "positive control: the dev build with the hook DID run the stubbed date" 'grep -q "^date-stub-called" /tmp/calls' ;;
dns-abort-rerun)
  echo 198.51.100.7 >/tmp/ctl/dns
  run "${BASE[@]}"
  ck "aborted at the DNS check" '((IRC != 0)) && nothing_changed'
  rm -f /tmp/ctl/dns
  run "${BASE[@]/cp.example.com/other.example.com}"
  ck "rerun with a different (corrected) domain succeeds" '((IRC == 0)) && grep -q "^kit install-app.sh host=other.example.com" /tmp/calls' ;;
ports-after-abort)
  echo 198.51.100.7 >/tmp/ctl/dns
  run "${BASE[@]}"
  rm -f /tmp/ctl/dns
  echo 'LISTEN 0 511 0.0.0.0:443 0.0.0.0:* users:(("nginx",pid=9,fd=6))' >/tmp/ctl/ss
  run "${BASE[@]}"
  ck "ports check still runs on the rerun after an early abort" '((IRC != 0)) && msg "nginx" && nothing_changed'
  mkdir -p /etc/autodeploy; printf 'INSTALLER_DOMAIN=old.example.org\nINSTALLER_EMAIL=none\nINSTALLER_ACME_STAGING=0\nINSTALLER_FIREWALL=no\nINSTALLER_SSH_PORT=\n' >/etc/autodeploy/installer.env
  run "${BASE[@]}"
  ck "a leftover installer.env does not skip the ports check" '((IRC != 0)) && msg "nginx"'
  rm -f /tmp/ctl/ss
  run "${BASE[@]}"
  ck "its domain is only a default, not binding (no host.env)" '((IRC == 0))' ;;
installer-env-symlink)
  mkdir -p /etc/autodeploy /elsewhere; ln -s /elsewhere/target /etc/autodeploy/installer.env.new
  run "${BASE[@]}"
  ck "symlinked installer.env.new refused, target untouched" '((IRC != 0)) && msg "symlink" && [[ ! -e /elsewhere/target ]]'
  ck "no kit script ran" '! grep -q "^kit " /tmp/calls' ;;
verify-fail-no-env)
  run --domain cp.example.com --email ops@example.com --firewall no --allow-dev --yes-i-am-testing --allowed-signers /fx/allowed_signers --release-dir /fx/rel_otherkey
  ck "after a verification failure no installer.env exists" '((IRC != 0)) && [[ ! -e /etc/autodeploy/installer.env ]]' ;;
dev-needs-confirmation)
  run --domain cp.example.com --email ops@example.com --firewall no --allow-dev --allowed-signers /fx/allowed_signers --release-dir /fx/rel_ok
  ck "dev build without --yes-i-am-testing refused when there is no terminal" '((IRC != 0)) && msg "yes-i-am-testing"'; ck "nothing ran" 'nothing_changed' ;;
partial-rerun)
  : >/tmp/ctl/failcreds
  run --domain cp.example.com --email ops@example.com --firewall yes --ssh-port 2222 --allow-dev --yes-i-am-testing --allowed-signers /fx/allowed_signers --release-dir /fx/rel_ok
  ck "first run fails midway" '((IRC != 0))'
  ck "choices were saved before the failure" 'grep -qx "INSTALLER_FIREWALL=yes" /etc/autodeploy/installer.env && grep -qx "INSTALLER_SSH_PORT=2222" /etc/autodeploy/installer.env'
  rm -f /tmp/ctl/failcreds; : >/tmp/calls
  run --domain cp.example.com --email ops@example.com --allow-dev --yes-i-am-testing --allowed-signers /fx/allowed_signers --release-dir /fx/rel_ok
  ck "rerun without --firewall keeps firewall=yes and the ssh port" '((IRC == 0)) && grep -E "^kit install-app.sh .*--configure-firewall .*--yes-firewall-ssh-port=2222" /tmp/calls' ;;
pg-timeout)
  : >/tmp/ctl/pgdown
  run "${BASE[@]}"
  ck "postgres wait times out with a hint" '((IRC != 0)) && msg "PostgreSQL did not become ready" && msg "docker logs autodeploy-postgres"'
  ck "migrate never ran" '! grep -q "^run migrate" /tmp/calls' ;;
enroll-fail)
  : >/tmp/ctl/enrollfail
  run "${BASE[@]}"
  ck "enroll failure is an error" '((IRC != 0))'
  ck "kit copy was made BEFORE enrollment" '[[ -x /usr/local/share/autodeploy/kit/enroll-local.sh ]]'
  ck "installed-version NOT written on a real failure" '[[ ! -e /etc/autodeploy/installed-version ]]' ;;
kit-symlink)
  mkdir -p /usr/local/share /elsewhere; ln -s /elsewhere /usr/local/share/autodeploy
  run "${BASE[@]}"
  ck "symlinked kit parent refused, link target untouched" '((IRC != 0)) && msg "symlink" && [[ -z "$(ls -A /elsewhere)" ]]' ;;
version-file-unreadable)
  mkdir -p /etc/autodeploy/installed-version
  run "${BASE[@]}"
  ck "unreadable installed-version gives a clear message" '((IRC != 0)) && msg "installed-version" && nothing_changed' ;;
# ---- plan 24-F: --role agent (extra server) ------------------------------------------------------------
agent-flags)
  AB=(--role agent --control-plane-host cp.example.com --server-id server_abc123 --email ops@example.com --firewall no --allow-dev --yes-i-am-testing --allowed-signers /fx/allowed_signers --release-dir /fx/rel_ok)
  printf '%s\n' "$TOK" >/tmp/tokfile; chmod 0600 /tmp/tokfile
  TF=(--token-file /tmp/tokfile)
  COMMON=(--email ops@example.com --firewall no --allow-dev --yes-i-am-testing --allowed-signers /fx/allowed_signers --release-dir /fx/rel_ok)
  run --role agent --server-id server_abc123 "${COMMON[@]}" "${TF[@]}"
  ck "missing --control-plane-host: usage error (exit 2)" '((IRC == 2)) && msg "control-plane-host"'; ck "nothing ran" 'nothing_changed'
  run --role agent --control-plane-host cp.example.com "${COMMON[@]}" "${TF[@]}"
  ck "missing --server-id: usage error (exit 2)" '((IRC == 2)) && msg "server-id"'; ck "nothing ran" 'nothing_changed'
  for bad in 'bad id' 'srv_1' 'server_' '../x' 'server_a/b' "server_$(printf 'a%.0s' $(seq 1 101))"; do
    run "${AB[@]/server_abc123/$bad}" "${TF[@]}"
    ck "server id '${bad:0:20}' refused (same format as enroll-local.sh)" '((IRC != 0)) && msg "server id"'
  done
  ck "nothing ran after the id refusals" 'nothing_changed'
  run "${AB[@]/cp.example.com/10.0.0.1}" "${TF[@]}"
  ck "IP literal control plane host refused" '((IRC != 0)) && msg "control-plane-host"'
  run "${AB[@]/cp.example.com/cp.test}" "${TF[@]}"
  ck "reserved suffix refused" '((IRC != 0)) && msg "control-plane-host"'
  run "${AB[@]/cp.example.com/https://cp.example.com}" "${TF[@]}"
  ck "a URL instead of a hostname refused" '((IRC != 0)) && msg "control-plane-host"'
  run "${AB[@]}" "${TF[@]}" --domain cp.example.com
  ck "--domain refused with --role agent" '((IRC == 2)) && msg "domain"'
  run "${AB[@]}" "${TF[@]}" --setup-wait 5m
  ck "--setup-wait refused with --role agent" '((IRC == 2)) && msg "setup-wait"'
  run "${BASE[@]}" --server-id server_abc123
  ck "--server-id refused with the default role" '((IRC == 2)) && msg "role agent"'
  run "${BASE[@]}" --control-plane-host cp.example.com
  ck "--control-plane-host refused with the default role" '((IRC == 2)) && msg "role agent"'
  run "${BASE[@]}" --token-file /tmp/tokfile
  ck "--token-file refused with the default role" '((IRC == 2)) && msg "role agent"'
  run "${AB[@]/agent/bogus}" "${TF[@]}"
  ck "unknown role refused" '((IRC == 2)) && msg "role"'
  run "${AB[@]}"
  ck "no terminal and no --token-file: refused up front, naming both ways to give the token" '((IRC != 0)) && msg "--token-file" && msg "terminal"'
  ck "...and nothing was changed" 'nothing_changed'
  run "${AB[@]}" --token-file /tmp/nonexistent
  ck "missing token file refused" '((IRC != 0)) && msg "token-file"'
  ln -s /tmp/tokfile /tmp/toklink
  run "${AB[@]}" --token-file /tmp/toklink
  ck "symlinked token file refused" '((IRC != 0)) && msg "symlink"'
  cp /tmp/tokfile /tmp/tok644; chmod 0644 /tmp/tok644
  run "${AB[@]}" --token-file /tmp/tok644
  ck "token file with group/other access refused" '((IRC != 0)) && msg "0600"'
  cp /tmp/tokfile /tmp/tokown; chown nobody /tmp/tokown 2>/dev/null; chmod 0600 /tmp/tokown
  run "${AB[@]}" --token-file /tmp/tokown
  ck "token file not owned by root refused" '((IRC != 0)) && msg "root"'
  echo "not-a-token" >/tmp/tokbad; chmod 0600 /tmp/tokbad
  run "${AB[@]}" --token-file /tmp/tokbad
  ck "malformed token refused WITHOUT echoing it" '((IRC != 0)) && msg "format" && ! grep -q "not-a-token" /tmp/out /tmp/err /var/log/autodeploy-install.log'
  ck "no refusal installed or changed anything" 'nothing_changed'
  ck "no token copy directory was ever created" '! ls -d /run/autodeploy-enroll.* >/dev/null 2>&1' ;;
agent-tokenfile)
  echo 198.51.100.7 >/tmp/ctl/dns   # DNS of the control plane host points elsewhere: an agent host must not care
  printf '%s\n' "$TOK" >/tmp/tokfile; chmod 0600 /tmp/tokfile
  run "${AB[@]}" --token-file /tmp/tokfile
  ck "exit 0" '((IRC == 0))'
  ck "step order: reachability, checkout, app (agent), traefik-only credentials, traefik, enroll, agent start, preflights" 'order_ok "^curl .*https://cp.example.com/readyz" "^kit install-checkout.sh" "^kit install-app.sh" "^kit create-credentials.sh --traefik-only" "^systemctl start autodeploy-traefik" "^agent enroll" "^systemctl start autodeploy-agent" "^kit preflight-agent.sh" "^kit preflight-checkout.sh"'
  ck "no control plane step ran: no postgres, migrate, kek, setup code, enroll-local, controlplane unit" '! grep -qE "^run |enroll-local|enable-github|autodeploy-postgres|autodeploy-controlplane|setup-token|pg_isready" /tmp/calls'
  ck "no setup URL or setup code printed" '! grep -q "/setup" /tmp/out && ! grep -q "setup.v1" /tmp/out'
  ck "install-app got --mode public --role agent --control-plane-host and no host env" 'grep -E "^kit install-app.sh host= --mode public --role agent --control-plane-host cp.example.com --acme-email ops@example.com .*/binaries$" /tmp/calls'
  ck "install-app got no --configure-firewall (--firewall no)" '! grep -q -- "--configure-firewall" /tmp/calls'
  ck "agent enroll argv: base url, server id, state dir and a token FILE path only" 'grep -E "^agent enroll --base-url https://cp.example.com --server-id server_abc123 --state-dir /var/lib/autodeploy/agent --token-file /run/autodeploy-enroll\.[A-Za-z0-9]+/token$" /tmp/calls'
  ck "readyz checked twice over https with system roots (no -k, no --cacert), once up front and once right before enrolling" '[[ "$(grep -c "^curl .*https://cp.example.com/readyz" /tmp/calls)" == 2 ]] && ! grep "readyz" /tmp/calls | grep -qE -- "-k|--insecure|--cacert|--capath" && grep "readyz" /tmp/calls | grep -q -- "--proto =https" && grep "readyz" /tmp/calls | grep -q -- "--tlsv1.2"'
  ck "the second readyz check comes right before the agent enroll" 'l="$(grep -n "^curl .*readyz" /tmp/calls | tail -n1 | cut -d: -f1)"; e="$(grep -n "^agent enroll" /tmp/calls | cut -d: -f1)"; ((l < e)) && ((l > $(grep -n "^systemctl start autodeploy-traefik" /tmp/calls | cut -d: -f1)))'
  ck "the agent read the token from its 0600 copy (fake agent checked owner and mode)" '[[ "$(cat /tmp/seen-token)" == "$TOK" ]]'
  ck "the token is in NO argv, call log, output, error, installer log, config or kit copy" '! grep -rqF -- "$TOK" /tmp/calls /tmp/out /tmp/err /var/log/autodeploy-install.log /etc/autodeploy /usr/local/share/autodeploy /var/lib/autodeploy 2>/dev/null'
  ck "the token copy and its directory are gone after success" '! ls -d /run/autodeploy-enroll.* >/dev/null 2>&1'
  ck "the operator-supplied token file is left alone" '[[ -f /tmp/tokfile ]]'
  ck "the agent's environment never held the token" '! grep -qF -- "$TOK" /tmp/seen-env'
  ck "server id recorded in agent-local.env for the agent unit" 'grep -qx "AUTODEPLOY_AGENT_SERVER_ID=server_abc123" /etc/autodeploy/agent-local.env'
  ck "credential exists" '[[ -e /var/lib/autodeploy/agent/credential ]]'
  ck "choices persisted with the role and the control plane host" 'grep -qx "INSTALLER_ROLE=agent" /etc/autodeploy/installer.env && grep -qx "INSTALLER_DOMAIN=cp.example.com" /etc/autodeploy/installer.env && ! grep -q "$TOK" /etc/autodeploy/installer.env'
  ck "installed-version recorded; kit copy kept" '[[ -s /etc/autodeploy/installed-version && -x /usr/local/share/autodeploy/kit/preflight-agent.sh ]]'
  ck "the DNS mismatch of the control plane host was not an issue (no DNS comparison on an agent host)" '! msg "198.51.100.7"'
  ck "summary tells the owner about app DNS and the next step" 'msg "this server" && msg "DNS"'
  ck "tmp dir removed" 'tmpgone' ;;
agent-tty)
  command -v script >/dev/null || { echo "CHECK ok   [agent-tty] (script not available: skipped)"; echo "RESULT rc=0"; exit 0; }
  : >/tmp/ttyout
  feed "$TOK" \
    | script -qec "bash /fx/$INSTALLER ${AB[*]}" /tmp/ttyout >/tmp/out 2>/tmp/err; IRC=$?
  sed 's/^/  | /' /tmp/ttyout | tail -n 4
  ck "exit 0 with the token pasted at the terminal prompt" '((IRC == 0))'
  ck "the hidden prompt was shown on the terminal" 'grep -q "Paste the one-time enrollment token from the Servers page" /tmp/ttyout'
  ck "the pasted token was NOT echoed to the terminal" '! grep -qF -- "$TOK" /tmp/ttyout'
  ck "the agent read it from the 0600 copy" '[[ "$(cat /tmp/seen-token)" == "$TOK" ]]'
  ck "the token is in no argv, call log, output, error, log or config" '! grep -rqF -- "$TOK" /tmp/calls /tmp/out /tmp/err /var/log/autodeploy-install.log /etc/autodeploy /usr/local/share/autodeploy 2>/dev/null'
  ck "token copy removed" '! ls -d /run/autodeploy-enroll.* >/dev/null 2>&1'
  ck "enroll came after the reachability check" 'order_ok "^curl .*readyz" "^agent enroll"' ;;
agent-tty-badtoken)
  command -v script >/dev/null || { echo "CHECK ok   [agent-tty-badtoken] (script not available: skipped)"; echo "RESULT rc=0"; exit 0; }
  : >/tmp/ttyout
  feed "this-is-not-a-token-SECRETISH" \
    | script -qec "bash /fx/$INSTALLER ${AB[*]}" /tmp/ttyout >/tmp/out 2>/tmp/err; IRC=$?
  sed 's/^/  | /' /tmp/ttyout | tail -n 5
  ck "a malformed pasted token is refused" '((IRC != 0)) && grep -qi "format\|look like" /tmp/ttyout /tmp/err /tmp/out'
  ck "the pasted text is not echoed anywhere" '! grep -rqF "SECRETISH" /tmp/ttyout /tmp/out /tmp/err /var/log/autodeploy-install.log /tmp/calls'
  ck "no agent enroll, no token copy" '! grep -q "^agent enroll" /tmp/calls && ! ls -d /run/autodeploy-enroll.* >/dev/null 2>&1' ;;
agent-enroll-fail)
  : >/tmp/ctl/enrollfail
  printf '%s\n' "$TOK" >/tmp/tokfile; chmod 0600 /tmp/tokfile
  run "${AB[@]}" --token-file /tmp/tokfile
  ck "enroll failure is an error" '((IRC != 0))'
  ck "the agent was invoked, and its failure explained with the reissue hint" 'grep -q "^agent enroll" /tmp/calls && msg "Reissue"'
  ck "the token copy is removed on failure too" '! ls -d /run/autodeploy-enroll.* >/dev/null 2>&1'
  ck "the token is in no argv, call log, output, error or log" '! grep -rqF -- "$TOK" /tmp/calls /tmp/out /tmp/err /var/log/autodeploy-install.log /etc/autodeploy 2>/dev/null'
  ck "agent not started, no credential, version not recorded" '! grep -q "^systemctl start autodeploy-agent" /tmp/calls && [[ ! -e /var/lib/autodeploy/agent/credential && ! -e /etc/autodeploy/installed-version ]]'
  ck "kit copy was made BEFORE enrollment" '[[ -x /usr/local/share/autodeploy/kit/preflight-agent.sh ]]'
  rm -f /tmp/ctl/enrollfail; : >/tmp/calls
  run "${AB[@]}" --token-file /tmp/tokfile
  ck "a rerun after the failure enrolls (fresh token copy)" '((IRC == 0)) && [[ -e /var/lib/autodeploy/agent/credential ]]' ;;
agent-cp-unreachable)
  : >/tmp/ctl/cpdown
  printf '%s\n' "$TOK" >/tmp/tokfile; chmod 0600 /tmp/tokfile
  run "${AB[@]}" --token-file /tmp/tokfile
  ck "refused" '((IRC != 0))'
  ck "the message names the readiness URL and the TLS/DNS hint" 'msg "https://cp.example.com/readyz"'
  ck "refused before any change: nothing changed, no token copy, no enroll" 'nothing_changed && ! grep -q "^agent " /tmp/calls && ! ls -d /run/autodeploy-enroll.* >/dev/null 2>&1'
  ck "the check is https only" 'grep "readyz" /tmp/calls | grep -q -- "--proto =https"' ;;
agent-cp-unreachable-tty)
  command -v script >/dev/null || { echo "CHECK ok   [agent-cp-unreachable-tty] (script not available: skipped)"; echo "RESULT rc=0"; exit 0; }
  : >/tmp/ctl/cpdown; : >/tmp/ttyout
  ( /usr/bin/sleep 1; printf 'y\n'; /usr/bin/sleep 0.5; printf '%s\n' "$TOK" ) | script -qec "bash /fx/$INSTALLER ${AB[*]}" /tmp/ttyout >/tmp/out 2>/tmp/err; IRC=$?
  ck "refused" '((IRC != 0)) && grep -q "readyz" /tmp/ttyout /tmp/err /tmp/out'
  ck "the token prompt was never shown" '! grep -q "Paste the one-time enrollment token" /tmp/ttyout'
  ck "nothing changed" 'nothing_changed' ;;
agent-cp-lost-midway)
  : >/tmp/ctl/cpdown-after-first
  printf '%s\n' "$TOK" >/tmp/tokfile; chmod 0600 /tmp/tokfile
  run "${AB[@]}" --token-file /tmp/tokfile
  ck "control plane lost between the first check and the enrollment: refused before enrolling" '((IRC != 0)) && msg "readyz"'
  ck "the install steps ran, but no enroll, no token copy, no agent start" 'grep -q "^kit install-app.sh" /tmp/calls && ! grep -q "^agent " /tmp/calls && ! ls -d /run/autodeploy-enroll.* >/dev/null 2>&1 && ! grep -q "^systemctl start autodeploy-agent" /tmp/calls' ;;
agent-xtrace)
  command -v script >/dev/null || { echo "CHECK ok   [agent-xtrace] (script not available: skipped)"; echo "RESULT rc=0"; exit 0; }
  printf '%s\n' "$TOK" >/tmp/tokfile; chmod 0600 /tmp/tokfile
  bash -x "/fx/$INSTALLER" "${AB[@]}" --token-file /tmp/tokfile >/tmp/out 2>/tmp/err </dev/null; IRC=$?
  ck "bash -x with --token-file: exit 0" '((IRC == 0))'
  ck "bash -x trace and output never hold the token (file route)" '! grep -rqF -- "$TOK" /tmp/out /tmp/err /var/log/autodeploy-install.log /tmp/calls /tmp/seen-env'
  ck "the trace really ran (positive control)" 'grep -q "^+ " /tmp/err'
  rm -rf /var/lib/autodeploy/agent/credential /etc/autodeploy/installer.env; : >/tmp/ttyout; : >/tmp/calls
  feed "$TOK" | script -qec "bash -x /fx/$INSTALLER ${AB[*]}" /tmp/ttyout >/tmp/out2 2>/tmp/err2; IRC=$?
  ck "bash -x on a pty: exit 0" '((IRC == 0))'
  ck "bash -x trace and output never hold the typed token (pty route)" '! grep -rqF -- "$TOK" /tmp/ttyout /tmp/out2 /tmp/err2 /var/log/autodeploy-install.log /tmp/calls /tmp/seen-env'
  ck "the agent did get it" '[[ "$(cat /tmp/seen-token)" == "$TOK" ]]' ;;
agent-wrong-id)
  : >/tmp/ctl/enrollfail
  printf '%s\n' "$TOK" >/tmp/tokfile; chmod 0600 /tmp/tokfile
  run "${AB[@]/server_abc123/server_wrong1}" --token-file /tmp/tokfile
  ck "enroll with the mistyped id fails" '((IRC != 0))'
  ck "the mistyped id was NOT locked in (agent-local.env untouched)" '! grep -q "server_wrong1" /etc/autodeploy/agent-local.env'
  rm -f /tmp/ctl/enrollfail; : >/tmp/calls
  run "${AB[@]}" --token-file /tmp/tokfile
  ck "rerun with the right id succeeds, not refused" '((IRC == 0)) && [[ -e /var/lib/autodeploy/agent/credential ]]'
  ck "the right id is recorded after enrollment" 'grep -qx "AUTODEPLOY_AGENT_SERVER_ID=server_abc123" /etc/autodeploy/agent-local.env' ;;
agent-wrong-host)
  : >/tmp/ctl/enrollfail
  printf '%s\n' "$TOK" >/tmp/tokfile; chmod 0600 /tmp/tokfile
  run "${AB[@]/cp.example.com/cpwrong.example.com}" --token-file /tmp/tokfile
  ck "enroll against the mistyped host fails" '((IRC != 0))'
  ck "host.env holds the mistyped host for now" 'grep -qx AUTODEPLOY_CONTROLPLANE_HOST=cpwrong.example.com /etc/autodeploy/host.env'
  rm -f /tmp/ctl/enrollfail; : >/tmp/calls
  run "${AB[@]}" --token-file /tmp/tokfile
  ck "rerun with the right host succeeds (not enrolled yet, so correctable)" '((IRC == 0))'
  ck "host.env now names the right host and the install-app got it" 'grep -qx AUTODEPLOY_CONTROLPLANE_HOST=cp.example.com /etc/autodeploy/host.env && grep -q "install-app.sh .*--control-plane-host cp.example.com" /tmp/calls'
  ck "installer.env follows" 'grep -qx INSTALLER_DOMAIN=cp.example.com /etc/autodeploy/installer.env' ;;
agent-token-whitespace)
  command -v script >/dev/null || { echo "CHECK ok   [agent-token-whitespace] (script not available: skipped)"; echo "RESULT rc=0"; exit 0; }
  : >/tmp/ttyout
  feed "  $TOK  " | script -qec "bash /fx/$INSTALLER ${AB[*]}" /tmp/ttyout >/tmp/out 2>/tmp/err; IRC=$?
  ck "a pasted token with surrounding spaces is accepted" '((IRC == 0))'
  ck "the agent got the trimmed token" '[[ "$(cat /tmp/seen-token)" == "$TOK" ]]'
  rm -rf /var/lib/autodeploy/agent/credential /etc/autodeploy/installer.env
  printf '  %s \r\n' "$TOK" >/tmp/tokfile; chmod 0600 /tmp/tokfile
  run "${AB[@]}" --token-file /tmp/tokfile
  ck "a token file with spaces and CRLF is accepted too" '((IRC == 0))' ;;
agent-stale-sweep)
  mkdir -m 0700 /run/autodeploy-enroll.OLD123 /run/autodeploy-enroll.NEW123
  touch -d '2 hours ago' /run/autodeploy-enroll.OLD123
  printf '%s\n' "$TOK" >/tmp/tokfile; chmod 0600 /tmp/tokfile
  run "${AB[@]}" --token-file /tmp/tokfile
  ck "exit 0" '((IRC == 0))'
  ck "a stale token directory of a crashed run is swept, a fresh one is left" '[[ ! -e /run/autodeploy-enroll.OLD123 && -d /run/autodeploy-enroll.NEW123 ]]' ;;
agent-rerun)
  printf '%s\n' "$TOK" >/tmp/tokfile; chmod 0600 /tmp/tokfile
  run "${AB[@]}" --token-file /tmp/tokfile; ck "first run ok" '((IRC == 0))'
  : >/tmp/calls; rm -f /tmp/seen-token
  run "${AB[@]}"
  ck "rerun without any token or terminal succeeds (already enrolled)" '((IRC == 0))'
  ck "rerun does not enroll, does not copy a token, does not prompt" '! grep -q "^agent " /tmp/calls && ! grep -q "Paste the one-time" /tmp/out && [[ ! -e /tmp/seen-token ]] && ! ls -d /run/autodeploy-enroll.* >/dev/null 2>&1'
  ck "rerun says it is already enrolled" 'msg "already enrolled"'
  ck "rerun is an upgrade: install-app --role agent again, agent started, preflights run" 'grep -q "^kit install-app.sh .*--role agent" /tmp/calls && grep -q "^systemctl start autodeploy-agent" /tmp/calls && grep -q "^kit preflight-agent.sh" /tmp/calls'
  ck "rerun asks nothing of the control plane: no setup code, no control plane step" '! grep -qE "^run |enroll-local|setup-token" /tmp/calls'
  ck "rerun mentions upgrade" 'msg "upgrade"' ;;
agent-rerun-mismatch)
  printf '%s\n' "$TOK" >/tmp/tokfile; chmod 0600 /tmp/tokfile
  run "${AB[@]}" --token-file /tmp/tokfile; ck "first run ok" '((IRC == 0))'
  quiet() { ! grep -qE "^(kit |apt-get|run |systemctl start|systemctl enable|agent |curl .*releases)" /tmp/calls; }
  : >/tmp/calls
  run "${AB[@]/cp.example.com/other.example.com}"
  ck "a different control plane host is refused, naming the recorded one" '((IRC != 0)) && msg "cp.example.com" && msg "other.example.com"'
  ck "...before anything ran" 'quiet'
  run "${AB[@]/server_abc123/server_zzz999}"
  ck "a different server id is refused, naming the recorded one" '((IRC != 0)) && msg "server_abc123" && msg "server_zzz999"'
  ck "...before anything ran" 'quiet'
  run "${BASE[@]}"
  ck "the default role on an agent host is refused (no in-place role switch)" '((IRC != 0)) && msg "role" && msg "agent"'
  ck "...before anything ran" 'quiet'
  ck "host.env and agent-local.env unchanged" 'grep -qx AUTODEPLOY_ROLE=agent /etc/autodeploy/host.env && grep -qx AUTODEPLOY_AGENT_SERVER_ID=server_abc123 /etc/autodeploy/agent-local.env' ;;
agent-role-all-then-agent)
  run "${BASE[@]}"; ck "full install ok" '((IRC == 0))'
  ck "host.env of the full install has no role line" '! grep -q AUTODEPLOY_ROLE /etc/autodeploy/host.env'
  : >/tmp/calls
  printf '%s\n' "$TOK" >/tmp/tokfile; chmod 0600 /tmp/tokfile
  run "${AB[@]}" --token-file /tmp/tokfile
  ck "agent role over a full install refused" '((IRC != 0)) && msg "role"'
  ck "...before any kit script or agent ran" '! grep -qE "^(kit |agent |systemctl start)" /tmp/calls' ;;
agent-installer-env-role)
  mkdir -p /etc/autodeploy; printf 'INSTALLER_DOMAIN=cp.example.com\nINSTALLER_EMAIL=none\nINSTALLER_ACME_STAGING=0\nINSTALLER_FIREWALL=no\nINSTALLER_SSH_PORT=\nINSTALLER_ROLE=agent\n' >/etc/autodeploy/installer.env
  run "${BASE[@]}"
  ck "an aborted agent run's installer.env blocks the default role" '((IRC != 0)) && msg "role" && nothing_changed_except_env' ;;
agent-lan-host)
  mkdir -p /etc/autodeploy; printf 'AUTODEPLOY_CONTROLPLANE_HOST=cp.example.com\nAUTODEPLOY_TLS_MODE=lan\nAUTODEPLOY_ROLE=agent\n' >/etc/autodeploy/host.env
  printf '%s\n' "$TOK" >/tmp/tokfile; chmod 0600 /tmp/tokfile
  run "${AB[@]}" --token-file /tmp/tokfile
  ck "lan host refused for the agent role too" '((IRC != 0)) && msg "lan"' ;;
agent-preflight-fail)
  : >/tmp/ctl/pffail
  printf '%s\n' "$TOK" >/tmp/tokfile; chmod 0600 /tmp/tokfile
  run "${AB[@]}" --token-file /tmp/tokfile
  ck "a failing agent preflight makes the installer exit nonzero" '((IRC != 0)) && msg "preflight"'
  ck "...after enrolling and starting the agent, with the version recorded" '[[ -e /var/lib/autodeploy/agent/credential ]] && grep -q "^systemctl start autodeploy-agent" /tmp/calls && [[ -s /etc/autodeploy/installed-version ]]'
  ck "token copy removed" '! ls -d /run/autodeploy-enroll.* >/dev/null 2>&1' ;;
setup-incomplete)
  : >/tmp/ctl/noadmin; run "${BASE[@]}"
  ck "exit 4: installed, first-run setup not finished" '((IRC == 4))'
  ck "tells the owner to rerun for a new code" 'msg "re-run"'
  ck "version still recorded" '[[ -s /etc/autodeploy/installed-version ]]' ;;
esac
echo "RESULT rc=$RC"
exit "$RC"
DRIVEREOF

# VER is substituted into the driver (download scenario) from the host-side constant.
DRIVER="VER=$V"$'\n'"$DRIVER"
run_sc() { # scenario installer-file [REL name] -- extra env
  local scen="$1" inst="${2:-install.sh}" rel="${3:-rel_ok}" out rc
  out="$(printf '%s\n' "$DRIVER" | docker run --rm -i --network none -e REL="$rel" -e RELSERVE="$rel" -v "$FX":/fx:ro "$TEST_IMAGE" bash -s -- "$scen" "$inst" 2>&1)"; rc=$?
  printf '%s\n' "$out" | grep -E '^CHECK' | sed 's/^CHECK ok   /ok   /; s/^CHECK FAIL /FAIL /' | while IFS= read -r l; do
    case "$l" in FAIL*) printf '%s\n' "$l" >&2 ;; *) printf '%s\n' "$l" ;; esac
  done
  if ((rc != 0)); then fail "scenario $scen reported failures (rc=$rc)"; printf '%s\n' "$out" | grep -vE '^CHECK ok' | tail -n 40 >&2; fi
}

for s in os-debian os-2204 arch-arm not-root res-ram res-cpu res-disk res-cgroup res-systemd ports-busy docker-io docker-snap \
  docker-install docker-usrlocal docker-podman docker-not-ce docker-badkey docker-nobuildx noninteractive-missing noninteractive-fw-yes-noport unattended unattended-options download \
  placeholder-allow-dev-nokey dev-version-refused signers-without-dev rerun rerun-admin-exists rerun-noargs downgrade other-domain lan-host \
  dns-mismatch dns-none bad-domain interactive setup-incomplete \
  agent-flags agent-tokenfile agent-tty agent-tty-badtoken agent-enroll-fail agent-cp-unreachable agent-cp-unreachable-tty agent-cp-lost-midway \
  agent-xtrace agent-wrong-id agent-wrong-host agent-token-whitespace agent-stale-sweep agent-rerun agent-rerun-mismatch agent-role-all-then-agent agent-installer-env-role agent-lan-host agent-preflight-fail \
  dev-needs-confirmation dns-abort-rerun ports-after-abort installer-env-symlink verify-fail-no-env partial-rerun pg-timeout enroll-fail kit-symlink version-file-unreadable; do
  run_sc "$s"
done
run_sc placeholder install_real_version.sh
run_sc real-version-bypass install_real_version.sh
run_sc release-hook-ignored install_real_version.sh
run_sc sig-fail install.sh rel_otherkey
run_sc dl-sig-fail install.sh rel_otherkey
run_sc ver-fail install.sh rel_vermismatch
run_sc tampered install.sh rel_tampered
run_sc traversal install.sh rel_traversal
run_sc absolute install.sh rel_absolute
run_sc symlink install.sh rel_symlink

[[ $FAILS -eq 0 ]] && { printf 'all passed\n'; exit 0; }
printf '%d FAILED\n' "$FAILS" >&2
exit 1
