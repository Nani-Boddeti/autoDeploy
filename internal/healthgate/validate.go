// Package healthgate holds the one shared validation for a configured
// health-gate probe target's path and Host: migration 000017's
// environment_runtime_configs.health_gate_path/health_gate_host CHECK
// constraints, mirrored here in Go so the same rule is enforced in three
// places without three independent (and driftable) implementations:
//
//   - internal/store/postgres/environment_runtime_config_repository.go,
//     the admin write path (before any row is written or CHECK ever runs);
//   - internal/web/web.go, the admin form (before the repository call);
//   - internal/runtime/docker/health.go, the agent's own probe (defense in
//     depth: re-validates the frozen lease values immediately before they
//     are used to build the probe request, independent of whatever the
//     control plane already checked).
//
// ValidPath and ValidHost must accept exactly the strings the migration
// 000017 CHECK constraints accept, and reject everything else -- see that
// migration's comments for the exact contract each mirrors.
package healthgate

import (
	"regexp"
	"strings"
)

// MaxPathBytes and MaxHostBytes mirror migration 000017's bounds exactly.
const (
	MaxPathBytes = 2048
	// MaxHostLabelBytes bounds the hostname portion only (before any
	// ":port" suffix), matching RFC 1035's 253-byte total-length limit for
	// a domain name.
	MaxHostLabelBytes = 253
)

// pathPattern requires a leading '/' and then an ASCII-only, allowlisted
// RFC 3986 grammar: path characters (pchar, plus '/' to allow multiple
// segments) each either a literal from the unreserved/sub-delims/":"/"@"
// set or a %-escaped octet, followed by an optional '?'-introduced query
// with the same rule plus a literal '?' allowed inside it too.
//
// This is deliberately an allowlist, not the original denylist (exclude
// whitespace/control/'#') Step 0 shipped with: independent review found
// that a denylist alone does not stop internal/runtime/docker/health.go's
// probe from re-encoding an already-percent-encoded or non-ASCII path when
// it is assigned to url.URL.Path (which net/url treats as the *decoded*
// value) -- "/health%2Fcheck" was sent on the wire as
// "/health%252Fcheck", "/a%zz" as "/a%25zz", and "/hé" as "/h%C3%A9",
// silently probing a different resource than configured (ADR 0005's "exact
// configured ... path"). Restricting the CHECK to exactly the bytes RFC
// 3986 allows in a path/query -- ASCII pchar/query characters and valid
// %XX triplets, nothing else -- makes every accepted path independently
// safe to round-trip through net/url's Path/RawPath/RawQuery split without
// ambiguity, which health.go's buildProbeURL then verifies explicitly
// rather than trusting blindly. A malformed %-triplet (e.g. "%zz", or a
// truncated "%2" at the end of the string) fails to match either
// alternative in the repeating group and is rejected, exactly as a bare
// non-ASCII byte is (it is not itself an allowed literal and does not
// start a valid %XX triplet).
var pathPattern = regexp.MustCompile(`^/(?:[A-Za-z0-9._~!$&'()*+,;=:@/-]|%[0-9A-Fa-f]{2})*(?:\?(?:[A-Za-z0-9._~!$&'()*+,;=:@/?-]|%[0-9A-Fa-f]{2})*)?$`)

// ValidPath reports whether path satisfies migration 000017's
// health_gate_path CHECK: starts with exactly one '/' (never "//..."), at
// most MaxPathBytes bytes, and matches pathPattern's ASCII-only RFC 3986
// path+query grammar exactly (which structurally excludes whitespace,
// control characters, '#', non-ASCII bytes, and any malformed percent
// escape).
func ValidPath(path string) bool {
	if len(path) < 1 || len(path) > MaxPathBytes {
		return false
	}
	if strings.HasPrefix(path, "//") {
		return false
	}
	return pathPattern.MatchString(path)
}

// hostLabelPattern is one DNS label: alphanumeric, optionally hyphenated
// internally, never starting or ending with a hyphen, 1-63 characters.
const hostLabelPattern = `[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?`

// portPattern matches a decimal port in 1..65535 (0 and anything >65535 is
// rejected; leading zeros are not a valid port and are rejected by
// construction, since e.g. "0080" does not match any alternative below).
const portPattern = `(?:6553[0-5]|655[0-2][0-9]|65[0-4][0-9]{2}|6[0-4][0-9]{3}|[1-5][0-9]{4}|[1-9][0-9]{0,3})`

// hostPattern is a DNS hostname (one or more dot-separated labels) with an
// optional ":port" suffix. There is no allowance anywhere in this pattern
// for '@' (userinfo), '/' (a path), or '[' ']' (an IPv6 literal) -- the
// character class every label draws from is strictly [A-Za-z0-9-], so none
// of those can ever appear.
var hostPattern = regexp.MustCompile(`^(?:` + hostLabelPattern + `)(?:\.(?:` + hostLabelPattern + `))*(?::` + portPattern + `)?$`)

// ValidHost reports whether host satisfies migration 000017's
// health_gate_host CHECK: a DNS hostname (strict [A-Za-z0-9.-] label
// charset, each label 1-63 bytes, no leading/trailing hyphen) with an
// optional ":port" suffix (port 1-65535), the hostname portion (excluding
// any ":port" suffix) at most MaxHostLabelBytes bytes, and nothing else --
// no userinfo, no path, no brackets.
func ValidHost(host string) bool {
	if host == "" || !hostPattern.MatchString(host) {
		return false
	}
	hostPart := host
	// hostPattern's character class guarantees at most one ':' can appear
	// in the whole string (only the optional trailing ":port" may contain
	// one), so splitting on the first ':' isolates the hostname portion
	// unambiguously.
	if idx := strings.IndexByte(host, ':'); idx >= 0 {
		hostPart = host[:idx]
	}
	return len(hostPart) <= MaxHostLabelBytes
}

// routeHostLabelPattern mirrors migration 000018's route_heads.hostname
// CHECK: the same DNS label grammar as hostLabelPattern above, but with two
// differences a health-gate Host header does not need and a production
// route hostname must not have -- no optional ":port" suffix (a route
// belongs to one canonical hostname, never a host:port pair), and a
// strictly lowercase charset ([a-z0-9-], not [A-Za-z0-9-]) so two hostnames
// differing only in case can never be treated as distinct by the database's
// UNIQUE(hostname) constraint. ValidRouteHostname's caller
// (route_head_repository.go) is responsible for lowercasing the operator's
// input before calling this function; this pattern intentionally rejects an
// uppercase byte rather than silently accepting and folding it, so a caller
// that forgot to normalize fails loudly instead of writing a
// case-sensitive duplicate of an existing route.
const routeHostLabelPattern = `[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?`

// routeHostnamePattern additionally requires at least two labels (one or
// more "label." prefixes, not zero-or-more, before the final label), unlike
// hostPattern above: a route hostname is used for public HTTPS/ACME (ADR
// 0005 "Domains, TLS, and servers"), where a bare single-label name like
// "localhost" or "app" is never routable or issuable a certificate for.
var routeHostnamePattern = regexp.MustCompile(`^(?:` + routeHostLabelPattern + `\.)+` + routeHostLabelPattern + `$`)

// routeHostFinalLabelAllDigits matches a final label consisting entirely of
// decimal digits. This is the standard heuristic for rejecting a bare IPv4
// dotted-quad literal (e.g. "1.2.3.4", "192.168.0.1") without a separate
// IPv4 parser: every public DNS TLD is all-letters (including punycode IDN
// TLDs like "xn--..."), while an IPv4 literal's last octet is all-digits by
// construction. IPv6 literals are already excluded by routeHostnamePattern's
// charset, which has no ':' or '[' ']'.
var routeHostFinalLabelAllDigits = regexp.MustCompile(`^[0-9]+$`)

// ValidRouteHostname reports whether host satisfies migration 000018's
// route_heads.hostname CHECK: a lowercase DNS hostname (each label 1-63
// bytes, no leading/trailing hyphen), at least two labels, a final label
// that is not all-digits, no port, no trailing dot, at most
// MaxHostLabelBytes bytes overall. Callers must lowercase the operator's
// input before calling this (see the comment above); this function does not
// normalize case itself, it only validates it.
func ValidRouteHostname(host string) bool {
	if host == "" || len(host) > MaxHostLabelBytes || !routeHostnamePattern.MatchString(host) {
		return false
	}
	// routeHostnamePattern guarantees at least one '.', so idx >= 0 always
	// holds here.
	idx := strings.LastIndexByte(host, '.')
	finalLabel := host[idx+1:]
	return !routeHostFinalLabelAllDigits.MatchString(finalLabel)
}
