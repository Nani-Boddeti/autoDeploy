package traefik

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// This file renders one environment's Traefik dynamic configuration
// document by hand, as YAML text, rather than through a YAML library.
//
// Why YAML, and why hand-rendered: Traefik's file provider accepts YAML or
// TOML dynamic configuration files -- never JSON; JSON is not one of the
// two formats the provider recognizes by extension (verified against
// Traefik's own provider documentation while implementing this step). This
// package picks YAML over TOML because YAML's block-mapping/list shape
// reads closer to Traefik's own dynamic-configuration examples, but the
// choice is otherwise arbitrary between the two accepted formats -- what
// matters structurally is that JSON is not an option here at all.
//
// go.mod carries no YAML library (or any dependency at all beyond pgx and
// golang.org/x/*), and this package's document shape is small and fixed:
// exactly one router and one service, five scalar values total. Adding a
// third-party YAML dependency for that fixed shape is a larger surface
// (arbitrary Go value marshaling, its own escaping bugs, a supply-chain
// addition) than writing the handful of lines this format needs directly
// and proving the one escaping primitive (quoteYAMLString) correct in
// isolation.
//
// Every value written into the document is quoted with quoteYAMLString,
// with no exception -- there is no "safe to leave bare" scalar anywhere in
// renderDocument, even for values (opaque IDs, the validated hostname) that
// EnvironmentRoute.validate has already restricted to a charset with no
// YAML metacharacters. This is deliberate belt-and-suspenders: validation
// is what actually keeps a hostile value out, but quoting is what keeps a
// value that validation ever regresses on from corrupting document
// structure instead of merely rendering as an unexpected (but contained)
// string. render_test.go proves quoteYAMLString itself is injection-proof
// against adversarial input independent of whether validate would ever let
// that input reach here.

// ErrNonASCIIValue is returned by quoteYAMLString (and therefore by
// renderDocument and Render) when a value contains any byte 0x80 or above.
// This is an enforced hard precondition, not a best-effort escape: an
// independent review of this package found that plain byte-range escaping
// (the original version of this function) let non-ASCII bytes through
// unescaped, and specific YAML-significant non-ASCII characters
// round-trip through a real YAML parser (PyYAML, yaml.v3) as something
// other than the literal bytes written -- U+0085 (NEL, UTF-8 C2 85),
// U+2028 (LINE SEPARATOR, UTF-8 E2 80 A8), and U+2029 (PARAGRAPH
// SEPARATOR, UTF-8 E2 80 A9) are all YAML line-break characters per the
// YAML spec and decode as a plain space in flow/double-quoted scalar
// content, silently changing what the rendered document actually says
// without changing its bytes' "shape" in any way this package's earlier,
// ASCII-blind escaping could see. A byte sequence that is not valid UTF-8
// at all can make the whole document unparseable to some YAML
// implementations. Every field this package validates (opaque IDs, the
// route hostname, entrypoint/certResolver names) is already restricted to
// a strict ASCII charset by EnvironmentRoute.validate, so this can never
// fire through the normal Render path today -- it exists so that if any
// future field or validator regression ever let a non-ASCII byte through,
// rendering fails closed (ErrInvalidRoute, permanent_configuration)
// instead of silently producing a document a YAML parser could
// misinterpret.
var ErrNonASCIIValue = errors.New("traefik: value contains a byte outside the ASCII range")

// quoteYAMLString renders s as a YAML double-quoted scalar: wraps it in
// `"`, and escapes exactly the bytes that would otherwise let it break out
// of that scalar or be reinterpreted -- backslash, double-quote, and every
// ASCII control character (0x00-0x1F, 0x7F), using YAML's own
// double-quoted escape sequences (\\, \", \n, \r, \t, and \xHH for any
// other control byte). Any byte 0x80 or above -- valid UTF-8 continuation
// bytes included -- is rejected outright as ErrNonASCIIValue rather than
// passed through or escaped; see that error's doc comment for why. YAML
// has no separate "backtick is special" rule -- a literal backtick is an
// ordinary character inside a double-quoted scalar and needs no escaping
// -- but Traefik's rule value template (renderDocument's ruleValue) wraps
// the hostname in backticks *inside* the YAML string's content on purpose
// (Host(`hostname`) is Traefik's own rule-language syntax, not YAML
// syntax), so this function must not mangle a backtick even though the
// hostname itself can never contain one (healthgate.ValidRouteHostname's
// charset excludes it structurally; see route_test.go).
func quoteYAMLString(s string) (string, error) {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x80 {
			return "", fmt.Errorf("%w: 0x%02X at offset %d", ErrNonASCIIValue, c, i)
		}
		switch c {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if c < 0x20 || c == 0x7F {
				fmt.Fprintf(&b, `\x%02X`, c)
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteByte('"')
	return b.String(), nil
}

// ruleValue builds Traefik's Host() rule expression for hostname. This is
// injection-proof by construction, not merely by the caller's diligence:
// healthgate.ValidRouteHostname's charset is exactly
// `[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?` labels joined by literal dots --
// it contains no backtick, parenthesis, whitespace, or newline byte in its
// accepted alphabet, so no hostname this function is ever called with can
// close the Host(` ... `) template early, append a second predicate, or
// escape into the surrounding YAML scalar. route_test.go asserts this
// charset exclusion directly (TestEnvironmentRouteHostnameRejectsBacktick
// and friends) rather than trusting the regex's shape by inspection alone.
func ruleValue(hostname string) string {
	return "Host(`" + hostname + "`)"
}

// renderDocument builds the complete YAML document body for r as a fixed
// sequence of lines, in a fixed order, with every scalar quoted. There is
// no map literal anywhere in this function and therefore no map-iteration
// order to stabilize: Go's `range` over a map is the classic source of
// nondeterministic serialization, and this function structurally cannot
// have that problem because it never ranges over one. render_test.go's
// TestRenderDeterministic proves the byte-for-byte-identical output this
// gives across repeated calls and independent EnvironmentRoute value
// constructions.
//
// Every quoted value goes through the local q() helper, which forwards to
// quoteYAMLString and latches the first error it returns: this function
// fails closed (returns that error, and no partial/inconsistent document)
// the instant any single field fails the ASCII-only precondition, rather
// than rendering a document with one field silently different from what
// was asked for.
func renderDocument(r EnvironmentRoute, router, service, backendURL string) ([]byte, error) {
	var b strings.Builder
	var firstErr error
	q := func(s string) string {
		out, err := quoteYAMLString(s)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		return out
	}

	b.WriteString("# Generated by autodeploy internal/runtime/traefik. Do not edit by hand;\n")
	b.WriteString("# this file is replaced atomically on every activation.\n")
	fmt.Fprintf(&b, "# environment: %s\n", r.EnvironmentID)
	fmt.Fprintf(&b, "# route_revision: %s\n", strconv.FormatUint(r.RouteRevision, 10))
	b.WriteString("http:\n")
	b.WriteString("  routers:\n")
	fmt.Fprintf(&b, "    %s:\n", q(router))
	fmt.Fprintf(&b, "      rule: %s\n", q(ruleValue(r.Hostname)))
	b.WriteString("      entryPoints:\n")
	fmt.Fprintf(&b, "        - %s\n", q(r.EntrypointName))
	fmt.Fprintf(&b, "      service: %s\n", q(service))
	b.WriteString("      tls:\n")
	fmt.Fprintf(&b, "        certResolver: %s\n", q(r.CertResolverName))
	b.WriteString("  services:\n")
	fmt.Fprintf(&b, "    %s:\n", q(service))
	b.WriteString("      loadBalancer:\n")
	b.WriteString("        servers:\n")
	fmt.Fprintf(&b, "          - url: %s\n", q(backendURL))

	if firstErr != nil {
		return nil, firstErr
	}
	return []byte(b.String()), nil
}

// Digest returns the canonical SHA-256 digest of doc, hex-encoded with a
// "sha256:" prefix (the same OCI-style shape
// internal/runtime/docker/validate.go's hexDigestPattern accepts).
// Rendered document bytes are exactly what is digested -- there is no
// separate canonicalization step, because renderDocument already produces
// a single, fully deterministic byte sequence for a given EnvironmentRoute
// and there is nothing left to canonicalize.
func Digest(doc []byte) string {
	sum := sha256.Sum256(doc)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Render validates r and returns its deterministic rendered document bytes
// together with their canonical digest. Two calls with equal
// EnvironmentRoute values (by Go value equality) always return
// byte-identical documents and therefore equal digests; any single field
// difference changes the digest (render_test.go's
// TestRenderDigestChangesPerField proves this exhaustively, one field at a
// time).
func Render(r EnvironmentRoute) ([]byte, string, error) {
	if err := r.validate(); err != nil {
		return nil, "", err
	}
	backendURL, err := backendServerURL(r)
	if err != nil {
		// validate() already calls docker.ContainerName and would have
		// returned first on any error, so this branch is unreachable under
		// correct operation; it exists only so this function never trusts
		// that invariant blindly.
		return nil, "", fmt.Errorf("%w: %v", ErrInvalidRoute, err)
	}
	doc, err := renderDocument(r, routerName(r), serviceName(r), backendURL)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrInvalidRoute, err)
	}
	return doc, Digest(doc), nil
}
