#!/usr/bin/env bash
# Stub tests for the public-mode DNS and TLS checks of preflight-app.sh. Extracts those blocks from
# the real script and runs them with stubbed dig/ip/openssl/curl. Scratch dir is fixed under TMPDIR,
# overwritten in place. Pure bash, no Docker.
set -u
here="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$here/../preflight-app.sh"
W="${TMPDIR:-/tmp}/autodeploy-pf-test"
mkdir -p "$W/bin"
sed -n '/^  # CP host DNS == /,/^  # Ports 80\/443:/p' "$SCRIPT" | sed '$d' >"$W/dns.sh"
sed -n '/^  # Issued certificate\./,/^# ---- credential files/p' "$SCRIPT" | sed '$d' | { echo 'if true; then'; cat; } >"$W/tls.sh"
[[ -s $W/dns.sh && -s $W/tls.sh ]] || { echo "FAIL could not extract blocks" >&2; exit 1; }

cat >"$W/bin/dig" <<'S'
#!/usr/bin/env bash
case "$*" in *" AAAA "*) for a in $STUB_AAAA; do echo "$a"; done ;; *" A "*) for a in $STUB_A; do echo "$a"; done ;; esac
S
cat >"$W/bin/ip" <<'S'
#!/usr/bin/env bash
if [[ "$*" == *-6* ]]; then [[ -n ${STUB_SRC6:-} ]] && echo "$* src $STUB_SRC6 uid 0"; else [[ -n ${STUB_SRC4:-} ]] && echo "$* src $STUB_SRC4 uid 0"; fi
exit 0
S
cat >"$W/bin/openssl" <<'S'
#!/usr/bin/env bash
case "$1" in
  s_client) if [[ "$*" == *-verify_return_error* ]]; then
              if [[ ${STUB_VERIFY:-ok} == ok ]]; then echo "Verify return code: 0 (ok)"; exit 0; else echo "Verify return code: 18 (self-signed certificate)"; exit 1; fi
            else echo CERT; fi ;;
  x509) [[ -n ${STUB_ISSUER:-} ]] && echo "issuer=$STUB_ISSUER" ;;
esac
S
cat >"$W/bin/curl" <<'S'
#!/usr/bin/env bash
echo "${STUB_HTTP:-200}"; [[ ${STUB_HTTP:-200} != 000 ]]
S
chmod +x "$W/bin/"*

t=0 f=0
# harness FILE -> output; vars from env
runblock() {
  env -i PATH="$W/bin:/usr/bin:/bin" CP_HOST=cp.example.org ACME_STAGING="${ACME_STAGING:-0}" \
    STUB_A="${STUB_A:-}" STUB_AAAA="${STUB_AAAA:-}" STUB_SRC4="${STUB_SRC4:-}" STUB_SRC6="${STUB_SRC6:-}" \
    STUB_ISSUER="${STUB_ISSUER:-}" STUB_VERIFY="${STUB_VERIFY:-ok}" STUB_HTTP="${STUB_HTTP:-200}" \
    BLOCK="$1" bash -c '
      ok() { echo "PASS $1"; }; bad() { echo "FAIL $1"; }; wrn() { echo "WARN $1"; }; skp() { echo "SKIP $1"; }
      check() { local d="$1"; shift; if "$@" >/dev/null 2>&1; then ok "$d"; else bad "$d"; fi; }
      is_root() { return 1; }; ACME_JSON=/nonexistent
      nc_overlap() { [[ "$1" == 10.* || "$1" == 192.168.* ]]; }
      source "$BLOCK"' 2>&1
}
expect() { # name want-prefix-regex output
  t=$((t + 1))
  if ! grep -Eq -- "$2" <<<"$3"; then f=$((f + 1)); printf 'FAIL  %s: want /%s/ got: %s\n' "$1" "$2" "$3"; fi
}

dns() { STUB_A="$1" STUB_AAAA="$2" STUB_SRC4="$3" STUB_SRC6="$4" runblock "$W/dns.sh"; }
expect "dns all match"        '^PASS'                 "$(dns "203.0.113.5" "2001:db8::5" 203.0.113.5 2001:db8::5)"
expect "dns A only match"     '^PASS'                 "$(dns "203.0.113.5" "" 203.0.113.5 "")"
expect "dns AAAA ok A wrong"  '^FAIL.*198\.51\.100\.9' "$(dns "198.51.100.9" "2001:db8::5" 203.0.113.5 2001:db8::5)"
expect "dns extra foreign A"  '^FAIL.*198\.51\.100\.9' "$(dns "203.0.113.5 198.51.100.9" "" 203.0.113.5 "")"
expect "dns wrong AAAA"       '^FAIL.*2001:db8::9'     "$(dns "203.0.113.5" "2001:db8::9" 203.0.113.5 2001:db8::5)"
expect "dns AAAA but no host v6 ignored" '^PASS'      "$(dns "203.0.113.5" "2001:db8::9" 203.0.113.5 "")"
expect "dns no A record"      '^FAIL.*no public A'     "$(dns "" "2001:db8::5" 203.0.113.5 2001:db8::5)"
expect "dns no records"       '^FAIL.*no public A'     "$(dns "" "" 203.0.113.5 "")"
expect "dns NAT stays WARN"   '^WARN.*NAT'             "$(dns "203.0.113.5" "" 10.1.2.3 "")"

tls() { STUB_ISSUER="$1" runblock "$W/tls.sh"; }
out="$(tls "O=Let's Encrypt, CN=R11")"
expect "tls verified pass"   'PASS certificate for cp.example.org verifies' "$out"
expect "tls cp via traefik"  'PASS control plane answers 2xx/3xx'           "$out"
out="$(STUB_VERIFY=bad STUB_ISSUER="CN=Other CA" runblock "$W/tls.sh")"
expect "tls verify error FAIL" 'FAIL certificate for cp.example.org verifies' "$out"
out="$(STUB_HTTP=000 STUB_ISSUER="O=Let's Encrypt, CN=R11" runblock "$W/tls.sh")"
expect "tls cp unreachable FAIL" 'FAIL control plane answers'                "$out"
out="$(STUB_ISSUER="CN=(STAGING) Fake LE Intermediate" runblock "$W/tls.sh")"
expect "tls staging WARN skip" 'WARN skipped: control plane through Traefik' "$out"
expect "tls staging no verify" '^WARN staging cert' "$out"
out="$(STUB_ISSUER="CN=TRAEFIK DEFAULT CERT" runblock "$W/tls.sh")"
expect "tls default cert WARN" '^WARN Traefik still serves its default' "$out"
t=$((t + 1)); if grep -q 'answers 2xx' <<<"$out"; then f=$((f + 1)); echo "FAIL  default cert must not run cp check"; fi

printf '%s tests, %s failed\n' "$t" "$f"
((f == 0))
