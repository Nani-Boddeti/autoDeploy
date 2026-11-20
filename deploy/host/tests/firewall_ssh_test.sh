#!/usr/bin/env bash
# Stub tests for the --configure-firewall SSH-port logic of install-app.sh (lockout safety).
# Extracts the detection block and the ufw apply block from the real script and runs them with
# stubbed sshd/ss/ufw on PATH. Scratch files live in a fixed dir under TMPDIR and are overwritten
# in place (nothing is deleted). Pure bash, no Docker.
set -u
here="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$here/../install-app.sh"
W="${TMPDIR:-/tmp}/autodeploy-fw-test"
mkdir -p "$W/bin"
sed -n '/^# Firewall flag:/,/^# Always the system Engine/p' "$SCRIPT" >"$W/detect.sh"
sed -n '/^# ---- 3b\./,/^fi$/p' "$SCRIPT" >"$W/apply.sh"
[[ -s $W/detect.sh && -s $W/apply.sh ]] || { echo "FAIL could not extract firewall blocks" >&2; exit 1; }

cat >"$W/bin/sshd" <<'S'
#!/usr/bin/env bash
for p in $STUB_SSHD_PORTS; do echo "port $p"; done
S
cat >"$W/bin/ss" <<'S'
#!/usr/bin/env bash
for p in $STUB_SS_SSHD; do echo "LISTEN 0 128 0.0.0.0:$p 0.0.0.0:* users:((\"sshd\",pid=7,fd=3))"; done
for p in $STUB_SS_SYSTEMD; do echo "LISTEN 0 128 *:$p *:* users:((\"systemd\",pid=1,fd=40))"; done
for p in $STUB_SS_OTHER; do echo "LISTEN 0 128 0.0.0.0:$p 0.0.0.0:* users:((\"nginx\",pid=9,fd=3))"; done
S
cat >"$W/bin/ufw" <<'S'
#!/usr/bin/env bash
echo "$*" >>"$STUB_UFW_LOG"
[[ -n ${STUB_UFW_FAIL:-} && "$*" == *"$STUB_UFW_FAIL"* ]] && exit 1
exit 0
S
chmod +x "$W/bin/"*

t=0 f=0
# run NAME EXPECT(ok|die) SSHD_PORTS SS_SSHD SS_SYSTEMD SSH_CONNECTION CONFIRM [UFW_FAIL] -> sets $log, $out
run() {
  local name="$1" want="$2" rc
  : >"$W/ufw.log"
  out="$(env -i PATH="$W/bin:/usr/bin:/bin" STUB_SSHD_PORTS="$3" STUB_SS_SSHD="$4" STUB_SS_SYSTEMD="$5" \
    STUB_SS_OTHER="${STUB_SS_OTHER:-}" STUB_UFW_LOG="$W/ufw.log" STUB_UFW_FAIL="${8:-}" SSH_CONNECTION="$6" \
    CONFIRM="$7" DETECT="$W/detect.sh" APPLY="$W/apply.sh" bash -c '
      set -euo pipefail
      die() { printf "install-app: error: %s\n" "$*" >&2; exit 1; }
      log() { :; }
      CONFIGURE_FIREWALL=1; FIREWALL_SSH_PORT_ARG="$CONFIRM"; TRAEFIK_BIND=0.0.0.0
      [[ -n "$SSH_CONNECTION" ]] || unset SSH_CONNECTION
      source "$DETECT"
      source "$APPLY"
      echo "allowed: ${FIREWALL_SSH_PORTS[*]}"
    ' 2>&1)"
  rc=$?
  log="$(cat "$W/ufw.log")"
  t=$((t + 1))
  if { [[ $want == ok && $rc == 0 ]] || [[ $want == die && $rc != 0 ]]; }; then return 0; fi
  f=$((f + 1)); printf 'FAIL  %s (rc=%s) %s\n' "$name" "$rc" "$out"; return 1
}
has() { grep -qx -- "$2" <<<"$1"; }
assert() { t=$((t + 1)); if ! "${@:2}"; then f=$((f + 1)); printf 'FAIL  %s\n' "$1"; fi; }
line_no() { grep -n -x -- "$2" <<<"$1" | head -1 | cut -d: -f1; }

# 1. normal: sshd on 22, connected via 22
run normal ok "22" "22" "" "10.0.0.1 5000 10.0.0.2 22" 22
assert "normal: allow 22" has "$log" "allow 22/tcp comment autodeploy: ssh"
assert "normal: deny+enable after allows" test "$(line_no "$log" 'default deny incoming')" -gt "$(line_no "$log" 'allow 443/tcp comment autodeploy: https')"

# 2. socket override: sshd -T says 22, ss shows systemd on 2222, session on 2222
run socket ok "22" "" "2222" "10.0.0.1 5000 10.0.0.2 2222" 2222
assert "socket: allow 22" has "$log" "allow 22/tcp comment autodeploy: ssh"
assert "socket: allow 2222" has "$log" "allow 2222/tcp comment autodeploy: ssh"

# 3. socket override, operator types the config port 22 while on 2222: accepted only because 22 is in the
#    allowed set, and 2222 (the session port) is allowed too, so no lockout.
run socket22 ok "22" "" "2222" "10.0.0.1 5000 10.0.0.2 2222" 22
assert "socket22: 2222 still allowed" has "$log" "allow 2222/tcp comment autodeploy: ssh"

# 4. session port not detected anywhere -> die before any ufw call
run undetected die "22" "22" "" "10.0.0.1 5000 10.0.0.2 2200" 2200
assert "undetected: no ufw call" test -z "$log"

# 5. typed port not detected -> die, no ufw call
run badtyped die "22" "22" "" "" 2200
assert "badtyped: no ufw call" test -z "$log"

# 6. non-sshd/systemd listeners are ignored
STUB_SS_OTHER="8080" run other ok "22" "22" "" "" 22
assert "other: 8080 not allowed" test -z "$(grep 8080 <<<"$log")"

# 7. malformed SSH_CONNECTION -> die
run malformed die "22" "22" "" "garbage" 22
assert "malformed: no ufw call" test -z "$log"

# 8. an allow rule fails -> never default deny / enable
run allowfail die "22" "22" "" "" 22 "allow 443/tcp"
assert "allowfail: no default deny" test -z "$(grep 'default deny' <<<"$log")"
assert "allowfail: no enable" test -z "$(grep 'enable' <<<"$log")"

printf '%s tests, %s failed\n' "$t" "$f"
((f == 0))
