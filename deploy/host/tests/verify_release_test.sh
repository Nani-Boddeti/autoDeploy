#!/usr/bin/env bash
# Tests for deploy/host/verify-release.sh and package-release.sh (plan 24-A). Local only: throwaway
# ed25519 keys are generated in a temp dir that the test removes itself; nothing touches GitHub or
# a real key. Needs ssh-keygen, git, tar, gzip, sha256sum or shasum. Missing ssh-keygen is a loud
# SKIP (exit 0 locally, exit 1 when CI=true).
# Usage: deploy/host/tests/verify_release_test.sh
set -uo pipefail

TESTS_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
KIT_DIR="$(cd -- "$TESTS_DIR/.." && pwd)"
VERIFY="$KIT_DIR/verify-release.sh"
PACKAGE="$KIT_DIR/package-release.sh"
FAILS=0
pass() { printf 'ok   %s\n' "$1"; }
fail() { printf 'FAIL %s\n' "$1" >&2; FAILS=$((FAILS + 1)); }

if ! command -v ssh-keygen >/dev/null 2>&1; then
  printf '##########\nSKIP verify_release_test: ssh-keygen not available: NOTHING WAS TESTED\n##########\n' >&2
  [[ "${CI:-}" == true ]] && exit 1
  exit 0
fi
sum() { sha256sum "$@" 2>/dev/null || shasum -a 256 "$@"; }

T="$(mktemp -d "${TMPDIR:-/tmp}/verify-release-test.XXXXXX")"
trap 'rm -rf -- "$T"' EXIT

ssh-keygen -q -t ed25519 -N '' -C release@autodeploy -f "$T/key" >/dev/null
ssh-keygen -q -t ed25519 -N '' -C other -f "$T/otherkey" >/dev/null
printf 'release@autodeploy namespaces="autodeploy-release" %s\n' "$(cut -d' ' -f1,2 "$T/key.pub")" >"$T/allowed_signers"
# Placeholder fixture (never the committed file).
printf '# fixture\nrelease@autodeploy namespaces="autodeploy-release" ssh-ed25519 PLACEHOLDER-FIXTURE\n' >"$T/placeholder_signers"
: >"$T/empty_signers"
V=v0.0.1

# mkfiles DIR: fake assets incl. VERSION and SHA256SUMS (unsigned). signrel DIR [KEY] [NS].
mkfiles() {
  local d="$1"
  rm -rf "$d"; mkdir -m 0700 -p "$d"
  printf 'kit bytes\n' >"$d/autodeploy-host-kit-v0.tar.gz"
  printf 'bin bytes\n' >"$d/autodeploy-binaries-v0.tar.gz"
  printf '#!/bin/sh\n' >"$d/install.sh"
  printf '%s\n' "$V" >"$d/VERSION"
  (cd "$d" && sum autodeploy-host-kit-v0.tar.gz autodeploy-binaries-v0.tar.gz install.sh VERSION >SHA256SUMS)
}
signrel() { ssh-keygen -Y sign -q -f "${2:-$T/key}" -n "${3:-autodeploy-release}" "$1/SHA256SUMS"; }
mkrel() { mkfiles "$1"; signrel "$@"; }
A=(--allowed-signers "$T/allowed_signers" --expect-version "$V")

expect_ok() { local l="$1"; shift; if "$VERIFY" "$@" >"$T/out" 2>&1; then pass "$l"; else fail "$l: $(cat "$T/out")"; fi; }
expect_fail() {
  local l="$1" want="$2"; shift 2
  if "$VERIFY" "$@" >"$T/out" 2>&1; then fail "$l: unexpectedly verified"
  elif grep -q -- "$want" "$T/out"; then pass "$l"
  else fail "$l: failed but without expected message '$want': $(cat "$T/out")"; fi
}

mkrel "$T/r1"; expect_ok "valid release verifies" "$T/r1" "${A[@]}"

mkrel "$T/r2"; printf 'evil\n' >"$T/r2/install.sh"
expect_fail "tampered asset" "checksum mismatch" "$T/r2" "${A[@]}"

mkrel "$T/r3"; (cd "$T/r3" && sed '1s/^./g/' SHA256SUMS >S2 && mv S2 SHA256SUMS)
expect_fail "tampered SHA256SUMS" "signature verification FAILED" "$T/r3" "${A[@]}"

mkrel "$T/r4" "$T/otherkey"; expect_fail "wrong key" "signature verification FAILED" "$T/r4" "${A[@]}"
mkrel "$T/r5" "$T/key" "some-other-namespace"; expect_fail "wrong namespace" "signature verification FAILED" "$T/r5" "${A[@]}"
mkrel "$T/r6"; rm "$T/r6/SHA256SUMS.sig"; expect_fail "missing signature" "unsigned" "$T/r6" "${A[@]}"
mkrel "$T/r7"; rm "$T/r7/install.sh"; expect_fail "listed file missing" "missing or not a regular file" "$T/r7" "${A[@]}"
mkrel "$T/r8"; printf 'x\n' >"$T/r8/extra.txt"; expect_fail "unlisted extra file refused (fail closed)" "not listed in SHA256SUMS" "$T/r8" "${A[@]}"

mkrel "$T/r9"; mv "$T/r9/install.sh" "$T/real-install.sh"; ln -s "$T/real-install.sh" "$T/r9/install.sh"
expect_fail "symlinked listed asset" "missing or not a regular file" "$T/r9" "${A[@]}"
mkrel "$T/r10"; ln -s "$T/r10/install.sh" "$T/r10/link"; expect_fail "extra symlink" "symlink" "$T/r10" "${A[@]}"
mkrel "$T/r11"; mkdir "$T/r11/sub"; expect_fail "subdirectory" "not a regular file" "$T/r11" "${A[@]}"
mkrel "$T/r12"; mv "$T/r12/SHA256SUMS.sig" "$T/sigreal"; ln -s "$T/sigreal" "$T/r12/SHA256SUMS.sig"
expect_fail "symlinked signature" "SHA256SUMS.sig missing or not a regular file" "$T/r12" "${A[@]}"

mkrel "$T/r13"
expect_fail "placeholder allowed_signers refused" "PLACEHOLDER" "$T/r13" --allowed-signers "$T/placeholder_signers" --expect-version "$V"
expect_fail "empty allowed_signers" "contains no keys" "$T/r13" --allowed-signers "$T/empty_signers" --expect-version "$V"
expect_fail "nonexistent dir" "not a directory" "$T/nope" "${A[@]}"
"$VERIFY" >/dev/null 2>&1; [[ $? == 2 ]] && pass "no args: usage exit 2" || fail "no args: usage exit 2"
"$VERIFY" "$T/r13" --expect-version "$V" >/dev/null 2>&1; [[ $? == 2 ]] && pass "--allowed-signers is mandatory (no silent default)" || fail "--allowed-signers mandatory"
"$VERIFY" "$T/r13" --allowed-signers "$T/allowed_signers" >/dev/null 2>&1; [[ $? == 2 ]] && pass "--expect-version is mandatory" || fail "--expect-version mandatory"

# M3 version binding
expect_fail "version mismatch" "version mismatch" "$T/r1" --allowed-signers "$T/allowed_signers" --expect-version v9.9.9
mkfiles "$T/r14"; (cd "$T/r14" && rm VERSION && sum autodeploy-host-kit-v0.tar.gz autodeploy-binaries-v0.tar.gz install.sh >SHA256SUMS); signrel "$T/r14"
expect_fail "VERSION asset missing from a signed release" "VERSION" "$T/r14" "${A[@]}"

# Strict SHA256SUMS parsing (each case signed with the right key, so only parsing can reject it)
mkfiles "$T/r15"; printf '%s  ../x\n' "$(printf 'x' | sum | cut -d' ' -f1)" >>"$T/r15/SHA256SUMS"; signrel "$T/r15"
expect_fail "../x name in SHA256SUMS" "malformed" "$T/r15" "${A[@]}"
mkfiles "$T/r16"; (cd "$T/r16" && while IFS= read -r l; do printf '%s\r\n' "$l"; done <SHA256SUMS >S2 && mv S2 SHA256SUMS && [ "$(head -c 200 SHA256SUMS | tr -cd '\r' | wc -c)" -gt 0 ]); signrel "$T/r16"
expect_fail "CRLF lines" "malformed" "$T/r16" "${A[@]}"
mkfiles "$T/r17"; (cd "$T/r17" && sum install.sh >>SHA256SUMS); signrel "$T/r17"
expect_fail "duplicate listing" "twice" "$T/r17" "${A[@]}"
mkfiles "$T/r18"; (cd "$T/r18" && sum SHA256SUMS >>SHA256SUMS 2>/dev/null; true); signrel "$T/r18"
expect_fail "SHA256SUMS listing itself" "must not list itself" "$T/r18" "${A[@]}"

# M2 directory trust
mkrel "$T/r19"; chmod 0770 "$T/r19"; expect_fail "group-writable dir" "writable" "$T/r19" "${A[@]}"
mkrel "$T/r20"; chmod 0777 "$T/r20"; expect_fail "world-writable dir" "writable" "$T/r20" "${A[@]}"
if [[ "$(id -u)" == 0 ]]; then
  mkrel "$T/r21"; chown 65534 "$T/r21"; expect_fail "dir owned by another user" "owned by" "$T/r21" "${A[@]}"
else
  echo "note: 'dir owned by another user' only runs as root (chown needs it); run it in the ubuntu:24.04 container as root"
fi
# M2 snapshot: the signed SHA256SUMS is read once into a private copy (debug shows the copy is used).
VERIFY_DEBUG=1 "$VERIFY" "$T/r1" "${A[@]}" >"$T/out" 2>&1 && grep -q 'snapshot' "$T/out" && pass "VERIFY_DEBUG=1 reports the private snapshot" || fail "VERIFY_DEBUG snapshot: $(cat "$T/out")"

# ---- package-release.sh end to end (kit built by git archive from HEAD, under umask 077)
mkdir "$T/bin"
for f in controlplane agent git; do printf 'fake %s\n' "$f" >"$T/bin/$f"; done
(cd "$T/bin" && sum controlplane agent git >SHA256SUMS)
chmod 0600 "$T/bin"/*   # umask-hostile inputs: output modes must not depend on them
( umask 077; "$PACKAGE" --version v9.9.9-test --binaries "$T/bin" --key "$T/key" --out "$T/outA" --allowed-signers "$T/allowed_signers" >"$T/pout" 2>&1 ) \
  && pass "package-release signs and self-verifies (umask 077)" || fail "package-release: $(cat "$T/pout")"
expect_ok "packaged release verifies independently" "$T/outA" --allowed-signers "$T/allowed_signers" --expect-version v9.9.9-test
[[ "$(cat "$T/outA/VERSION" 2>/dev/null)" == v9.9.9-test ]] && pass "signed VERSION asset holds the version" || fail "VERSION asset"
kit="$T/outA/autodeploy-host-kit-v9.9.9-test.tar.gz"
listing="$(tar -tzf "$kit" 2>/dev/null)"
if grep -q 'install-app.sh$' <<<"$listing" && grep -q 'libexec/autodeploy-run$' <<<"$listing" \
  && ! grep -q -e '/tests' -e '\.bak' -e 'static-git' -e 'build-' -e 'package-release' -e 'smoke.env.example' -e 'verify-release' -e 'release-signing' -e 'allowed_signers' <<<"$listing"; then
  pass "kit contents: runtime items in; tests, .bak*, build/dev items, verify-release.sh and the trust anchor out"
else fail "kit contents: $listing"; fi
tv="$(tar -tvzf "$kit" 2>/dev/null)"
grep -E ' autodeploy-host-kit-[^ ]*/install-app\.sh$' <<<"$tv" | grep -q '^-rwxr-xr-x' \
  && grep -E 'traefik/traefik\.yml$' <<<"$tv" | grep -q '^-rw-r--r--' \
  && grep -E 'libexec/checkout-loop$' <<<"$tv" | grep -q '^-rwxr-xr-x' \
  && pass "kit modes are 0755/0644 regardless of umask" || fail "kit modes: $(grep -e install-app -e traefik.yml <<<"$tv")"
bt="$(tar -tvzf "$T/outA/autodeploy-binaries-v9.9.9-test.tar.gz" 2>/dev/null)"
grep -E 'binaries/controlplane$' <<<"$bt" | grep -q '^-rwxr-xr-x' && grep -E 'binaries/SHA256SUMS$' <<<"$bt" | grep -q '^-rw-r--r--' \
  && pass "binaries tarball modes normalized, inner SHA256SUMS kept" || fail "binaries tarball: $bt"

"$PACKAGE" --version v9 --binaries "$T/bin" --key "$T/key" --out "$T/outA" --allowed-signers "$T/allowed_signers" >/dev/null 2>&1 \
  && fail "package into non-empty out dir accepted" || pass "package refuses a non-empty out dir"
"$PACKAGE" --version v9 --binaries "$T/bin" --key "$T/otherkey" --out "$T/outB" --allowed-signers "$T/allowed_signers" >/dev/null 2>&1 \
  && fail "package accepted a key that is not the published one" || pass "package fails when key does not match allowed_signers"
"$PACKAGE" --version v9 --binaries "$T/bin" --key "$T/otherkey" --out "$T/outC" --self-check-only >/dev/null 2>&1 \
  && pass "package --self-check-only (dry run)" || fail "package --self-check-only"
"$PACKAGE" --version 'v9;x' --binaries "$T/bin" --key "$T/key" --out "$T/outD" --self-check-only >/dev/null 2>&1 \
  && fail "invalid version accepted" || pass "package refuses an invalid version"
printf 'not a key\n' >"$T/badkey"; chmod 600 "$T/badkey"
"$PACKAGE" --version v9 --binaries "$T/bin" --key "$T/badkey" --out "$T/outE" --self-check-only >/dev/null 2>&1 \
  && fail "garbage key accepted" || pass "package fails closed on an invalid key"

# ---- kit allowlist drift: every tracked deploy/host file is in KIT_ALLOWLIST or explicitly EXCLUDED
eval "$(sed -n '/^KIT_ALLOWLIST=(/,/)$/p' "$PACKAGE")"
EXCLUDED=(tests/ static-git/ release-signing/ build-acer-binaries.sh build-host-binaries.sh build-static-git.sh
  package-release.sh verify-release.sh smoke.env.example install.sh)
in_list() { # path list...: exact match, or path under a "dir/" or bare-dir entry
  local p="$1" e; shift
  for e in "$@"; do
    [[ "$p" == "$e" || "$p" == "${e%/}/"* ]] && return 0
  done
  return 1
}
unclassified=""
if repo="$(git -C "$KIT_DIR" rev-parse --show-toplevel 2>/dev/null)"; then
  while IFS= read -r f; do
    rel="${f#deploy/host/}"
    in_list "$rel" "${KIT_ALLOWLIST[@]}" || in_list "$rel" "${EXCLUDED[@]}" || unclassified+=" $rel"
  done < <(git -C "$repo" ls-files deploy/host)
  [[ -z "$unclassified" ]] && pass "every tracked deploy/host file is in KIT_ALLOWLIST or EXCLUDED" \
    || fail "unclassified deploy/host files (add to KIT_ALLOWLIST in package-release.sh or EXCLUDED here):$unclassified"
else fail "not a git checkout: cannot run the allowlist drift check"; fi
# Best-effort: literal "$KIT_DIR/<path>" references in the kit's scripts must land inside the kit. A
# reference containing a variable ("$KIT_DIR/systemd/$u.service") is checked down to its directory part
# only. NOT covered: paths built from other variables, "$f" loops (their lists are checked at run time
# by the scripts' own "kit file missing" test), here/dirname-relative paths, sourced files' own refs.
missing=""
for s in install-app.sh install-checkout.sh enroll-local.sh; do
  while IFS= read -r ref; do
    path="${ref#\$KIT_DIR/}"
    [[ "$path" == *'$'* ]] && { path="${path%%\$*}"; [[ "$path" == */ || -z "$path" ]] || path="${path%/*}/"; [[ -n "$path" ]] || continue; }
    in_list "${path%/}" "${KIT_ALLOWLIST[@]}" || missing+=" $s:$ref"
  done < <(grep -oE '\$KIT_DIR/[A-Za-z0-9_./$-]+' "$KIT_DIR/$s" | sort -u)
done
[[ -z "$missing" ]] && pass "KIT_DIR references in install-app/install-checkout/enroll-local resolve inside the kit" || fail "KIT_DIR references outside the kit:$missing"

if ((FAILS)); then printf '%d FAILED\n' "$FAILS" >&2; exit 1; fi
echo "all verify_release tests passed"
