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
