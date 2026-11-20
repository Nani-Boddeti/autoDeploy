#!/usr/bin/env bash
# Table tests for deploy/host/netcheck.sh. Pure bash, stubs `ip` and `docker`. Runs on bash 3.2+.
set -u
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../netcheck.sh
. "$here/../netcheck.sh"

t=0 f=0
expect() { # description expected actual
  t=$((t + 1))
  if [[ "$2" == "$3" ]]; then return 0; fi
  f=$((f + 1))
  printf 'FAIL  %s (want %s got %s)\n' "$1" "$2" "$3"
}

# ---- overlap math: a b want (0 overlap, 1 disjoint, 2 malformed) --------------------
while read -r a b want; do
  [[ -n $a ]] || continue
  nc_overlap "$a" "$b"
  expect "overlap $a $b" "$want" "$?"
done <<'TABLE'
172.29.0.0/24 172.29.0.0/24 0
172.29.0.0/24 172.29.1.0/24 1
172.29.0.0/24 172.29.0.0/16 0
172.29.0.128/25 172.29.0.0/24 0
172.29.0.0/25 172.29.0.128/25 1
172.29.0.255/32 172.29.0.0/24 0
172.29.1.0/32 172.29.0.0/24 1
172.29.0.5 172.29.0.5/32 0
172.29.0.5 172.29.0.6 1
10.0.0.0/8 11.0.0.0/8 1
10.255.255.255/32 10.0.0.0/8 0
11.0.0.0/32 10.0.0.0/8 1
0.0.0.0/0 192.168.1.1/32 0
255.255.255.255/32 0.0.0.0/32 1
172.29.0.1/24 172.29.0.200/24 0
1.2.3.4/33 1.2.3.0/24 2
1.2.3.256/24 1.2.3.0/24 2
1.2.3/24 1.2.3.0/24 2
1.2.3.4.5/24 1.2.3.0/24 2
a.b.c.d/24 1.2.3.0/24 2
1.2.3.0/ 1.2.3.0/24 2
1.2.3.0/-1 1.2.3.0/24 2
1.2.3.0/24 garbage 2
TABLE
nc_overlap "" 1.2.3.0/24
expect "empty first arg rejected" 2 "$?"
nc_overlap 1.2.3.0/24 ""
expect "empty second arg rejected" 2 "$?"

nc_parse 172.29.0.130/25
expect "parse ok" 0 "$?"
expect "parse /25 lo is 172.29.0.128" $(((172 << 24) | (29 << 16) | 128)) "$NC_LO"
expect "parse /25 hi is 172.29.0.255" $(((172 << 24) | (29 << 16) | 255)) "$NC_HI"

# ---- check_subnet_collision with stubbed ip / docker ---------------------------------
IP_OUT="" DOCKER_IDS="" DOCKER_INSPECT=""
ip() { printf '%s' "$IP_OUT"; }
docker() {
  case "$1 $2" in
    "network ls") printf '%s' "$DOCKER_IDS" ;;
    "network inspect") printf '%s' "$DOCKER_INSPECT" ;;
    *) return 1 ;;
  esac
}

ERR=""
run() { # description want-rc cidr [exclude]; stderr captured in ERR
  local rc
  ERR="$(check_subnet_collision "$3" ${4:+"$4"} 2>&1 >/dev/null)"
  rc=$?
  expect "$1" "$2" "$rc"
}

IP_OUT=$'default via 10.0.0.1 dev eth0\n10.0.0.0/24 dev eth0 proto kernel scope link src 10.0.0.5\n'
DOCKER_IDS=$'aaa\nbbb\n'
DOCKER_INSPECT=$'bridge 172.17.0.0/16 \nautodeploy-env 172.29.0.0/24 \n'
run "free subnet, own network excluded by default" 0 172.29.0.0/24
run "free subnet, explicit exclude" 0 172.29.0.0/24 autodeploy-env

IP_OUT=$'default via 10.0.0.1 dev eth0\n172.29.0.0/16 dev tun0 scope link\n'
run "overlaps host route" 1 172.29.0.0/24
[[ $ERR == *172.29.0.0/16* && $ERR == *AUTODEPLOY_ENV_SUBNET* && $ERR != *$'\n'* ]]
expect "route message names route + env var, one line" 0 "$?"

IP_OUT=$'default via 10.0.0.1 dev eth0\n172.29.0.77 via 10.0.0.9 dev eth0\n'
run "overlaps host route given as bare IP" 1 172.29.0.0/24
IP_OUT=$'default via 10.0.0.1 dev eth0\n172.29.1.77 via 10.0.0.9 dev eth0\n172.29.1.78/32 dev eth0\n'
run "adjacent host routes do not collide" 0 172.29.0.0/24
IP_OUT=$'default via 10.0.0.1 dev eth0\n0.0.0.0/0 via 10.0.0.1 dev eth0\n'
run "default routes excluded" 0 172.29.0.0/24
IP_OUT=$'blackhole 172.29.0.0/24\n'
run "blackhole route collides (keyword skipped)" 1 172.29.0.0/24
IP_OUT=$'default via 10.0.0.1 dev eth0\nfe80::/64 dev eth0\n'
run "non-IPv4 lines ignored" 0 172.29.0.0/24
IP_OUT=""

DOCKER_INSPECT=$'bridge 172.17.0.0/16 \nother 172.29.0.0/20 fd00::/64 \n'
run "overlaps other docker network" 1 172.29.0.0/24
[[ $ERR == *other* && $ERR == *172.29.0.0/20* && $ERR == *AUTODEPLOY_ENV_SUBNET* && $ERR != *$'\n'* ]]
expect "docker message names network + env var, one line" 0 "$?"
DOCKER_INSPECT=$'bridge 172.17.0.0/16 \nnoipam \nv6only fd00::/64 \n'
run "no-IPAM and IPv6-only networks ignored" 0 172.29.0.0/24
DOCKER_INSPECT=$'autodeploy-env 172.29.0.0/24 \n'
run "own network excluded" 0 172.29.0.0/24
run "same network NOT excluded when another name is excluded" 1 172.29.0.0/24 someother
DOCKER_IDS=""
DOCKER_INSPECT=$'other 172.29.0.0/20 \n'
run "no docker networks listed -> no docker conflicts" 0 172.29.0.0/24
docker() { return 1; }
run "docker unavailable -> only routes checked" 0 172.29.0.0/24
run "malformed CIDR rejected" 2 172.29.0.0/99

printf '%d tests, %d failed\n' "$t" "$f"
((f == 0))
