#!/usr/bin/env bash
# Assemble, checksum and sign the release assets (plan 24-A). Called by .github/workflows/release.yml
# and runnable locally with a throwaway key.
#   package-release.sh --version VERSION --binaries DIR --key KEYFILE --out OUTDIR
#                      [--allowed-signers FILE | --self-check-only]
#   --binaries  output dir of build-host-binaries.sh (binaries + git + its own SHA256SUMS)
#   --key       OpenSSH private key (never printed). Pass the FILE, not the key.
#   --out       must be empty or absent; receives exactly the release assets:
#                 autodeploy-host-kit-VERSION.tar.gz   allowlisted deploy/host runtime files (git archive; no trust anchor)
#                 autodeploy-binaries-VERSION.tar.gz   binaries/ (BINDIR for install-*.sh, with its SHA256SUMS)
#                 VERSION                              the tag
#                 SHA256SUMS, SHA256SUMS.sig           checksums over the files above, signed
#   --allowed-signers FILE  after signing, verify with verify-release.sh against this file (the repo's
#                 release-signing/allowed_signers): proves the key matches the published public key.
#   --self-check-only  (dry runs while allowed_signers is still the placeholder) verify against a key
#                 derived from --key instead; proves the signature round-trips, not key identity.
#   One of the two is required.
#   --commit REF  commit to build the kit from (default HEAD; CI: the triggering commit).
#   VERSION asset: the tag, listed in SHA256SUMS; verify-release.sh --expect-version binds to it.
# Reproducibility: the kit (git archive) is deterministic everywhere; the binaries tarball is
# byte-reproducible with GNU tar only (CI); on macOS it is only content-identical.
set -euo pipefail

die() { echo "package-release: $*" >&2; exit 1; }
usage() { echo "usage: package-release.sh --version V --binaries DIR --key KEYFILE --out OUTDIR [--commit REF] [--allowed-signers FILE | --self-check-only]" >&2; exit 2; }

version="" binaries="" key="" out="" allowed="" selfcheck=0 commit=HEAD
while (($#)); do
  case "$1" in
    --version) (($# >= 2)) || usage; version="$2"; shift 2 ;;
    --binaries) (($# >= 2)) || usage; binaries="$2"; shift 2 ;;
    --key) (($# >= 2)) || usage; key="$2"; shift 2 ;;
    --out) (($# >= 2)) || usage; out="$2"; shift 2 ;;
    --allowed-signers) (($# >= 2)) || usage; allowed="$2"; shift 2 ;;
    --commit) (($# >= 2)) || usage; commit="$2"; shift 2 ;;
    --self-check-only) selfcheck=1; shift ;;
    *) usage ;;
  esac
done
[[ -n "$version" && -n "$binaries" && -n "$key" && -n "$out" ]] || usage
[[ -n "$allowed" || "$selfcheck" == 1 ]] || usage
[[ -z "$allowed" || "$selfcheck" == 0 ]] || usage
[[ "$version" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] || die "invalid version '$version'"
[[ -f "$key" && ! -L "$key" ]] || die "--key must be a regular file"
[[ -d "$binaries" && -f "$binaries/SHA256SUMS" ]] || die "--binaries must be a build-host-binaries.sh output dir with SHA256SUMS"
if [[ -e "$out" ]]; then [[ -d "$out" && -z "$(ls -A "$out")" ]] || die "--out must be an empty directory"; fi
mkdir -p "$out"
chmod 0755 "$out"
out="$(cd "$out" && pwd)"
binaries="$(cd "$binaries" && pwd)"

here="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
work="$(mktemp -d "${TMPDIR:-/tmp}/autodeploy-package.XXXXXX")"
trap 'rm -rf -- "$work"' EXIT

sum() { sha256sum "$@" 2>/dev/null || shasum -a 256 "$@"; }
tar_det() { # deterministic tar | gzip -n: tar_det OUT BASEDIR NAME
  local flags=(--uid 0 --gid 0)
  if tar --version 2>/dev/null | grep -q 'GNU tar'; then flags=(--owner=0 --group=0 --numeric-owner --sort=name --mtime='2020-01-01 00:00Z'); fi
  (cd "$2" && COPYFILE_DISABLE=1 tar "${flags[@]}" -cf - "$3") | gzip -n >"$1"
}

# 1. host kit: `git archive` of COMMIT (default HEAD = the triggering commit) restricted to an explicit
# allowlist. No working-tree content and no umask dependence: modes come from git (0755/0644), owner
# 0, mtime = commit time; tar.umask is pinned (git defaults to 0002). Anything not listed (tests, .bak*, static-git, build-*.sh, smoke.env.example,
# package-release.sh, verify-release.sh and release-signing/) is not in the kit: the kit must never
# carry the trust anchor (install.sh embeds the key; see tasks/24-plan.md 24-B).
KIT_ALLOWLIST=(README.md backup-db.sh create-credentials.sh diagnose.sh enable-github.sh enroll-local.sh
  install-app.sh install-checkout.sh netcheck.sh preflight-app.sh preflight-checkout.sh private-ca.sh
  stage-lib.sh uninstall.sh libexec systemd traefik)
repo="$(git -C "$here" rev-parse --show-toplevel)"
git -C "$repo" -c tar.umask=0022 archive --format=tar --prefix="autodeploy-host-kit-$version/" "$commit:deploy/host" "${KIT_ALLOWLIST[@]}" \
  | gzip -n >"$out/autodeploy-host-kit-$version.tar.gz" || die "git archive of $commit failed (is every allowlisted path committed?)"

# 2. binaries (installer BINDIR), inner SHA256SUMS kept verbatim for stage_verify. Modes are set
# explicitly so the result does not depend on the build machine's umask.
mkdir -p "$work/bin/binaries"
cp "$binaries"/* "$work/bin/binaries/"
chmod 0755 "$work/bin/binaries" "$work/bin/binaries"/*
chmod 0644 "$work/bin/binaries/SHA256SUMS"
tar_det "$out/autodeploy-binaries-$version.tar.gz" "$work/bin" binaries

# install.sh is never a release asset: it is fetched from the git tag ref (tasks/24-plan.md 24-B).

# 3. signed VERSION asset (binds the signature to the tag), SHA256SUMS over every asset, then sign
printf '%s\n' "$version" >"$out/VERSION"
assets=()
for f in "$out"/*; do assets+=("${f##*/}"); done
(cd "$out" && sum "${assets[@]}" >SHA256SUMS)
ssh-keygen -Y sign -q -f "$key" -n autodeploy-release "$out/SHA256SUMS" || die "signing failed (is the key a valid OpenSSH private key?)"
[[ -f "$out/SHA256SUMS.sig" ]] || die "no signature produced"

# 4. verify exactly as a host will
if ((selfcheck)); then
  pub="$(ssh-keygen -y -f "$key")" || die "cannot derive public key"
  allowed="$work/allowed_signers"
  printf 'release@autodeploy namespaces="autodeploy-release" %s\n' "$pub" >"$allowed"
fi
"$here/verify-release.sh" "$out" --allowed-signers "$allowed" --expect-version "$version"
echo "package-release: assets in $out:"
ls -l "$out" | awk 'NR>1 {print "  " $5 "\t" $9}'
