#!/usr/bin/env bash
# Verify a downloaded autodeploy release directory (plan 24-A). Needs only openssh + coreutils
# (both preinstalled on Ubuntu 24.04), so install.sh (24-B) can call it before trusting any asset.
#   verify-release.sh DIR --allowed-signers FILE --expect-version VERSION
# DIR holds the release assets: SHA256SUMS, SHA256SUMS.sig, VERSION and every file SHA256SUMS lists.
#
# --allowed-signers is MANDATORY and has no default: the trust anchor must come from the caller (the
# key embedded in install.sh), never from the directory being verified or from the release kit itself.
# --expect-version is MANDATORY (install.sh passes the tag it was fetched from): it binds the signed
# VERSION asset to the requested release and stops a validly signed OLD release being replayed.
#
# Steps (any failure exits non-zero, nothing is installed or modified):
#   1. DIR must be a real directory owned by root or the caller and not group/world-writable.
#   2. allowed_signers must be a regular file, without the PLACEHOLDER marker, with at least one key.
#   3. SHA256SUMS and SHA256SUMS.sig are copied ONCE into a private mktemp -d; everything below uses
#      the copies. The signature must verify (ssh-keygen -Y verify, principal release@autodeploy,
#      namespace autodeploy-release).
#   4. The copy must be well formed ('<sha256>  <basename>' lines, no duplicates, no self listing).
#   5. DIR contains no symlinks, subdirectories or files that SHA256SUMS does not list (fail closed).
#   6. sha256sum --check --strict against the copy; VERSION must be listed and equal --expect-version.
# VERIFY_DEBUG=1 prints ssh-keygen's stderr and the snapshot location.
# Exit codes: 0 verified, 1 verification failed, 2 usage.
set -euo pipefail

readonly PRINCIPAL="release@autodeploy"
readonly NAMESPACE="autodeploy-release"

die() { echo "verify-release: $*" >&2; exit 1; }
usage() { echo "usage: verify-release.sh DIR --allowed-signers FILE --expect-version VERSION" >&2; exit 2; }

allowed="" dir="" expect=""
while (($#)); do
  case "$1" in
    --allowed-signers) (($# >= 2)) || usage; allowed="$2"; shift 2 ;;
    --expect-version) (($# >= 2)) || usage; expect="$2"; shift 2 ;;
    -h | --help) usage ;;
    -*) echo "verify-release: unknown option $1" >&2; usage ;;
    *) [[ -z "$dir" ]] || usage; dir="$1"; shift ;;
  esac
done
[[ -n "$dir" && -n "$allowed" && -n "$expect" ]] || usage
command -v ssh-keygen >/dev/null 2>&1 || die "ssh-keygen not found (install openssh-client)"
debug=0; [[ "${VERIFY_DEBUG:-}" == 1 ]] && debug=1

# 1. directory trust
[[ -d "$dir" && ! -L "$dir" ]] || die "$dir is not a directory"
# stat: GNU first, BSD (macOS) second. Prints "<uid> <octal mode>".
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
else # macOS dev machines without coreutils
  (cd "$dir" && shasum -a 256 -c --quiet "$snap/SHA256SUMS") >/dev/null 2>&1 || die "checksum mismatch"
fi
has_version=0
for name in "${listed[@]}"; do [[ "$name" == VERSION ]] && has_version=1; done
((has_version)) || die "VERSION asset is not listed in the signed SHA256SUMS: refusing a release without a version binding"
got="$(cat -- "$dir/VERSION")"
[[ "$got" == "$expect" ]] || die "version mismatch: release says '$got', expected '$expect'"

echo "verify-release: OK: signature valid, version $got, ${#listed[@]} file(s) match SHA256SUMS in $dir"
