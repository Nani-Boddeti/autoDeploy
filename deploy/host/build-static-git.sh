#!/usr/bin/env bash
# Build a static, non-PIE (ELF ET_EXEC) linux/amd64 git (+ git-remote-https)
# for checkout-exec and write them to OUTDIR (plus SHA256SUMS next to them, which
# install-checkout.sh verifies against its staged copies). Requires Docker.
#   build-static-git.sh OUTDIR
#
# NOT verified here (out-of-band step): the git source tarball is checked against the
# sha256 pinned in static-git/Dockerfile, but that pin was taken from kernel.org's
# sha256sums.asc without checking its PGP signature. Verify sha256sums.asc against the
# git maintainers' signing key by hand before changing the pinned hash.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
outdir="${1:?usage: build-static-git.sh OUTDIR}"
mkdir -p "$outdir"
outdir="$(cd "$outdir" && pwd)"
tag=autodeploy-static-git-report:local

docker build --platform linux/amd64 --target export --output "type=local,dest=$outdir" "$here/static-git"
docker build --platform linux/amd64 --target report --tag "$tag" "$here/static-git"

echo "--- container checks (readelf) ---"
docker run --rm --platform linux/amd64 "$tag"

echo "--- host checks ---"
for f in git git-remote-https; do
  p="$outdir/$f"
  [[ -f "$p" ]] || { echo "missing $p" >&2; exit 1; }
  desc="$(file -b "$p")"
  echo "$f: $desc"
  [[ "$desc" == *"statically linked"* ]] || { echo "$f: not statically linked" >&2; exit 1; }
  etype="$(od -An -tx1 -j16 -N1 "$p" | tr -d ' \n')"
  echo "$f: e_type byte 16 = $etype"
  [[ "$etype" == 02 ]] || { echo "$f: e_type is not ET_EXEC" >&2; exit 1; }
  shasum -a 256 "$p"
done

# SHA256SUMS next to the outputs; entries for other files already in OUTDIR are kept.
sums="$outdir/SHA256SUMS"
{
  if [[ -f "$sums" ]]; then grep -v -E ' \*?(git|git-remote-https)$' "$sums" || true; fi
  (cd "$outdir" && { sha256sum git git-remote-https 2>/dev/null || shasum -a 256 git git-remote-https; })
} >"$sums.tmp"
mv -f "$sums.tmp" "$sums"
echo "wrote $sums"
