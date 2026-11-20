# shellcheck shell=bash
# AutoDeploy host kit: IPv4 CIDR overlap helpers and the environment-subnet collision check.
# Sourced (not executed) by install-app.sh before it creates the Docker network, and by
# preflight-app.sh. Pure bash integer math (no python, no declare -A), so it also runs on
# bash 3.2. All names are nc_-prefixed to avoid clashing with the caller's helpers.

# nc_parse CIDR|IP: sets NC_LO and NC_HI (inclusive integer bounds). A bare IP is a /32.
# Returns 1 on anything that is not a strict dotted-quad with an optional /0../32 prefix.
nc_parse() {
  local s="$1" ip pfx a b c d o m
  [[ -n $s ]] || return 1
  if [[ $s == */* ]]; then ip="${s%/*}" pfx="${s#*/}"; else ip="$s" pfx=32; fi
  [[ $pfx =~ ^[0-9]{1,2}$ ]] || return 1
  ((10#$pfx <= 32)) || return 1
  [[ $ip =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]] || return 1
  IFS=. read -r a b c d <<<"$ip"
  for o in "$a" "$b" "$c" "$d"; do ((10#$o <= 255)) || return 1; done
  m=$((32 - 10#$pfx))
  NC_LO=$(((((10#$a << 24) | (10#$b << 16) | (10#$c << 8) | 10#$d) >> m) << m))
  NC_HI=$((NC_LO | ((1 << m) - 1)))
}

# nc_overlap CIDR_A CIDR_B: 0 if the ranges share any address, 1 if disjoint, 2 if malformed.
nc_overlap() {
  local lo1 hi1
  nc_parse "$1" || return 2
  lo1=$NC_LO hi1=$NC_HI
  nc_parse "$2" || return 2
  ((lo1 <= NC_HI && NC_LO <= hi1))
}

# nc_route_dests: IPv4 route destinations (CIDR or bare host IP), one per line, default route
# excluded. Skips the leading route-type keyword (blackhole, unreachable, ...) when present.
nc_route_dests() {
  local first second rest
  while read -r first second rest; do
    case "$first" in
      blackhole | unreachable | prohibit | throw | local | broadcast | multicast | anycast | nat) first="$second" ;;
    esac
    case "$first" in '' | default | 0.0.0.0/0) continue ;; esac
    nc_parse "$first" || continue
    printf '%s\n' "$first"
  done < <(ip -4 route show 2>/dev/null)
}

# nc_docker_subnets EXCLUDE_NETWORK: "<network-name> <ipv4-subnet>" lines for every Docker
# network except EXCLUDE_NETWORK. IPv6 subnets and networks without IPAM config are skipped.
nc_docker_subnets() {
  local ids name rest s
  # shellcheck disable=SC2207
  ids=($(docker network ls -q 2>/dev/null)) || return 0
  ((${#ids[@]})) || return 0
  while read -r name rest; do
    [[ -n $name && $name != "$1" ]] || continue
    for s in $rest; do
      [[ $s == */* && $s != *:* ]] && printf '%s %s\n' "$name" "$s"
    done
  done < <(docker network inspect -f '{{.Name}} {{range .IPAM.Config}}{{.Subnet}} {{end}}' "${ids[@]}" 2>/dev/null)
}

# check_subnet_collision CIDR [EXCLUDE_NETWORK]: 0 if CIDR overlaps no IPv4 route (default route
# excluded) and no existing Docker network subnet (EXCLUDE_NETWORK, default $ENV_NETWORK or
# autodeploy-env, is skipped). Otherwise 1 with a single-line message on stderr; 2 if CIDR is
# malformed. The message names the conflict and the variable that overrides the subnet.
check_subnet_collision() {
  local cidr="$1" excl="${2:-${ENV_NETWORK:-autodeploy-env}}" dest name sub
  local hint="choose a free range with AUTODEPLOY_ENV_SUBNET (and matching AUTODEPLOY_ENV_GATEWAY, AUTODEPLOY_ENV_IP_RANGE, AUTODEPLOY_ENV_TRAEFIK_IP)"
  nc_parse "$cidr" || {
    echo "invalid subnet '$cidr' (expected IPv4 CIDR such as 172.29.0.0/24)" >&2
    return 2
  }
  while read -r dest; do
    [[ -n $dest ]] || continue
    if nc_overlap "$cidr" "$dest"; then
      echo "subnet $cidr overlaps host route $dest; $hint" >&2
      return 1
    fi
  done < <(nc_route_dests)
  while read -r name sub; do
    [[ -n $name ]] || continue
    if nc_overlap "$cidr" "$sub"; then
      echo "subnet $cidr overlaps Docker network $name ($sub); $hint" >&2
      return 1
    fi
  done < <(nc_docker_subnets "$excl")
  return 0
}
