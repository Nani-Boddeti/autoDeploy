#!/usr/bin/env bash
# Build linux/amd64 host binaries for ANY Ubuntu 24.04 x86_64 host (provider-neutral). Run on the
# development machine (not as root):
#   build-host-binaries.sh --gates enabled|disabled OUTDIR GITDIR
#
#   --gates   REQUIRED, no default. Same logic as build-acer-binaries.sh (which stays unchanged):
#             disabled  builds HEAD exactly as committed: executorEnabled (cmd/agent) and
#                       activationEnabled (cmd/controlplane) stay false (the release posture).
#             enabled   flips both constants to true in a temporary `git worktree` of HEAD
#                       (removed on exit; working tree, index and sources untouched). The result is
#                       GATE-FLIPPED: never release or commit it.
#             (The gates are Go consts rewritten with sed, not ldflags or build tags.)
#   OUTDIR    receives controlplane, agent, admin, checkout-exec, checkout-quota-service,
#             github-askpass, migrate, secrets, gitcheckout.test, git, git-remote-https, SHA256SUMS
#   GITDIR    directory holding the static `git` and `git-remote-https` (build-static-git.sh output)
#
# Uncommitted changes are NOT built: the worktree is HEAD. Nothing is committed or staged.
# Afterwards the sha256 of SHA256SUMS is printed: transfer it to the host over a channel
# SEPARATE from the binaries and compare before running install-*.sh (docs/operations/vps-install.md).
set -euo pipefail

usage() { echo "usage: build-host-binaries.sh --gates enabled|disabled OUTDIR GITDIR" >&2; exit 2; }

gates=""
args=()
while (($#)); do
  case "$1" in
    --gates) (($# >= 2)) || usage; gates="$2"; shift 2 ;;
    --gates=*) gates="${1#--gates=}"; shift ;;
    -h | --help) usage ;;
    -*) echo "build: unknown option $1" >&2; usage ;;
    *) args+=("$1"); shift ;;
  esac
done
[[ "$gates" == enabled || "$gates" == disabled ]] || { echo "build: --gates enabled|disabled is required (no default)" >&2; usage; }
((${#args[@]} == 2)) || usage
outdir="${args[0]}"
gitdir="${args[1]}"

here="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo="$(git -C "$here" rev-parse --show-toplevel)"
[[ -x "$gitdir/git" && -x "$gitdir/git-remote-https" ]] || { echo "build: $gitdir must contain executable git and git-remote-https" >&2; exit 1; }
mkdir -p "$outdir"
outdir="$(cd "$outdir" && pwd)"
gitdir="$(cd "$gitdir" && pwd)"

readonly BINARIES=(admin agent checkout-exec checkout-quota-service controlplane github-askpass migrate secrets)

tmp="$(mktemp -d "${TMPDIR:-/tmp}/autodeploy-host-build.XXXXXX")"
wt="$tmp/worktree"
cleanup() {
  # `git worktree remove` deletes the temporary worktree; the empty mktemp directory is then removed.
  if ! git -C "$repo" worktree remove --force "$wt" >/dev/null 2>&1; then
    echo "build: WARNING: could not remove the temporary worktree; leftover: $wt (remove it by hand: git worktree remove --force '$wt')" >&2
  fi
  git -C "$repo" worktree prune >/dev/null 2>&1 || true
  rmdir "$tmp" >/dev/null 2>&1 || true
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
if [[ "$gates" == enabled ]]; then
  flip cmd/agent/main.go executorEnabled
  flip cmd/controlplane/main.go activationEnabled
fi

export GOOS=linux GOARCH=amd64 CGO_ENABLED=0
# Outputs are overwritten in place (go build -o, install, redirect); nothing is deleted from OUTDIR.
for b in "${BINARIES[@]}"; do
  echo "building $b"
  (cd "$wt" && go build -trimpath -o "$outdir/$b" "./cmd/$b")
done
echo "building gitcheckout.test"
(cd "$wt" && go test -c -trimpath -tags=checkout_integration -o "$outdir/gitcheckout.test" ./internal/gitcheckout)
install -m 0755 "$gitdir/git" "$outdir/git"
install -m 0755 "$gitdir/git-remote-https" "$outdir/git-remote-https"

sum() { sha256sum "$@" 2>/dev/null || shasum -a 256 "$@"; }
# Written under a temp name and renamed over the old file at the end: a failed build never leaves a
# half-written or stale-looking SHA256SUMS next to new binaries.
sums_tmp=".SHA256SUMS.new.$$"
(cd "$outdir" && sum "${BINARIES[@]}" gitcheckout.test git git-remote-https >"$sums_tmp" && mv -f "$sums_tmp" SHA256SUMS)
sums_hash="$(cd "$outdir" && sum SHA256SUMS | awk '{print $1}')"

if [[ "$gates" == enabled ]]; then
  cat <<BANNER

##############################################################################
#  GATE-FLIPPED BINARIES - DO NOT RELEASE OR COMMIT                          #
#  cmd/agent executorEnabled=true, cmd/controlplane activationEnabled=true   #
#  built from HEAD $head_sha in a temporary worktree (now removed)        #
##############################################################################
BANNER
else
  echo
  echo "gates disabled (release posture): built from HEAD $head_sha"
fi
echo "output: $outdir"
echo "sha256 of SHA256SUMS: $sums_hash"
echo "Transfer this hash to the host over a channel separate from the binaries and compare it there."
