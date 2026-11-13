#!/usr/bin/env bash
# Build GATE-FLIPPED linux/amd64 binaries for the acer ONLY. Run on the development machine
# (not as root):
#   build-acer-binaries.sh OUTDIR GITDIR
#
#   OUTDIR  receives controlplane, agent, admin, checkout-exec, checkout-quota-service,
#           github-askpass, migrate, secrets, gitcheckout.test, git, git-remote-https and SHA256SUMS
#   GITDIR  directory holding the static `git` and `git-remote-https` (build-static-git.sh output)
#
# The two release gates are flipped ONLY in a temporary `git worktree` of HEAD that is removed
# on exit: `const executorEnabled = false` (cmd/agent) and `const activationEnabled = false`
# (cmd/controlplane) become true. The working tree, the index and the committed sources are never
# touched (TestExecutorEnabledStaysFalse / TestActivationEnabledStaysFalse keep guarding HEAD).
# Uncommitted changes are NOT built: the worktree is HEAD. Nothing is committed or staged.
set -euo pipefail

here="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo="$(git -C "$here" rev-parse --show-toplevel)"
outdir="${1:?usage: build-acer-binaries.sh OUTDIR GITDIR}"
gitdir="${2:?usage: build-acer-binaries.sh OUTDIR GITDIR}"
[[ -x "$gitdir/git" && -x "$gitdir/git-remote-https" ]] || { echo "build: $gitdir must contain executable git and git-remote-https" >&2; exit 1; }
mkdir -p "$outdir"
outdir="$(cd "$outdir" && pwd)"
gitdir="$(cd "$gitdir" && pwd)"

readonly BINARIES=(admin agent checkout-exec checkout-quota-service controlplane github-askpass migrate secrets)

tmp="$(mktemp -d "${TMPDIR:-/tmp}/autodeploy-acer-build.XXXXXX")"
wt="$tmp/worktree"
cleanup() {
  git -C "$repo" worktree remove --force "$wt" >/dev/null 2>&1 || true
  git -C "$repo" worktree prune >/dev/null 2>&1 || true
  rm -rf -- "$tmp"
}
trap cleanup EXIT

git -C "$repo" worktree add --detach --quiet "$wt" HEAD
head_sha="$(git -C "$wt" rev-parse --short=12 HEAD)"

# flip FILE CONSTANT: exactly one `const NAME = false` line, replaced in the temporary worktree only.
flip() {
  local file="$wt/$1" name="$2" want="const $2 = false" n
  n="$(grep -c -x -- "$want" "$file" || true)"
  [[ "$n" == 1 ]] || { echo "build: expected exactly one line '$want' in $1, found $n" >&2; exit 1; }
  sed "s/^const $name = false\$/const $name = true/" "$file" >"$file.new"
  mv -f "$file.new" "$file"
  [[ "$(grep -c -x -- "const $name = true" "$file")" == 1 && "$(grep -c -x -- "$want" "$file" || true)" == 0 ]] \
    || { echo "build: flipping $name in $1 failed" >&2; exit 1; }
}
flip cmd/agent/main.go executorEnabled
flip cmd/controlplane/main.go activationEnabled

export GOOS=linux GOARCH=amd64 CGO_ENABLED=0
rm -f -- "$outdir"/gitcheckout.test "$outdir"/SHA256SUMS
for b in "${BINARIES[@]}"; do
  echo "building $b"
  (cd "$wt" && go build -trimpath -o "$outdir/$b" "./cmd/$b")
done
echo "building gitcheckout.test"
(cd "$wt" && go test -c -trimpath -tags=checkout_integration -o "$outdir/gitcheckout.test" ./internal/gitcheckout)
install -m 0755 "$gitdir/git" "$outdir/git"
install -m 0755 "$gitdir/git-remote-https" "$outdir/git-remote-https"

(cd "$outdir" && { sha256sum "${BINARIES[@]}" gitcheckout.test git git-remote-https 2>/dev/null \
  || shasum -a 256 "${BINARIES[@]}" gitcheckout.test git git-remote-https; } >SHA256SUMS)

cat <<BANNER

##############################################################################
#  GATE-FLIPPED BINARIES - FOR THE ACER ONLY - DO NOT RELEASE OR COMMIT      #
#  cmd/agent executorEnabled=true, cmd/controlplane activationEnabled=true   #
#  built from HEAD $head_sha in a temporary worktree (now removed)        #
#  output: $outdir
##############################################################################
BANNER
