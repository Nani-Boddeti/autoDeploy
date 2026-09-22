package healthgate

import (
	"strings"
	"testing"
)

func TestValidPathAcceptsGoodShapes(t *testing.T) {
	cases := []string{
		"/", "/healthz", "/a/b/c", "/" + strings.Repeat("a", 2047),
		"/health%2Fcheck", // a valid percent-encoded triplet
		"/a;b",            // ';' is an RFC 3986 sub-delim, allowed literally
		"/p?x=1&y=%20",    // query string, including a percent-encoded space
		"/a:b@c",          // ':' and '@' are allowed pchar literals
		"/foo?",           // empty query
		"/a?b?c",          // literal '?' inside the query
	}
	for _, p := range cases {
		if !ValidPath(p) {
			t.Fatalf("ValidPath(%q) = false, want true", p)
		}
	}
}

func TestValidPathRejectsBadShapes(t *testing.T) {
	cases := map[string]string{
		"empty":                  "",
		"missing leading slash":  "healthz",
		"double leading slash":   "//evil",
		"space":                  "/a b",
		"fragment":               "/a#b",
		"control character":      "/a\tb",
		"newline":                "/a\nb",
		"oversized":              "/" + strings.Repeat("a", 2048),
		"non-ASCII literal byte": "/hé",
		"malformed percent (zz)": "/a%zz",
		"truncated percent":      "/a%2",
		"double quote":           `/a"b`,
		"angle bracket open":     "/a<b",
		"angle bracket close":    "/a>b",
		"backslash":              `/a\b`,
		"caret":                  "/a^b",
		"pipe":                   "/a|b",
		"curly brace":            "/a{b",
		"backtick":               "/a`b",
	}
	for name, p := range cases {
		if ValidPath(p) {
			t.Fatalf("%s: ValidPath(%q) = true, want false", name, p)
		}
	}
}

func TestValidHostAcceptsGoodShapes(t *testing.T) {
	label := strings.Repeat("a", 63)
	host253 := label + "." + label + "." + label + "." + strings.Repeat("a", 253-3*64)
	if len(host253) != 253 {
		t.Fatalf("test bug: constructed host is %d bytes, want 253", len(host253))
	}
	cases := []string{"h", "app.internal", "app.internal:8443", "app.internal:1", "app.internal:65535", host253}
	for _, h := range cases {
		if !ValidHost(h) {
			t.Fatalf("ValidHost(%q) = false, want true", h)
		}
	}
}

func TestValidHostRejectsBadShapes(t *testing.T) {
	label := strings.Repeat("a", 63)
	host253 := label + "." + label + "." + label + "." + strings.Repeat("a", 253-3*64)
	cases := map[string]string{
		"empty":           "",
		"userinfo":        "user@host",
		"embedded path":   "host/path",
		"ipv6 brackets":   "[::1]",
		"port zero":       "app.internal:0",
		"port too large":  "app.internal:65536",
		"254 bytes":       host253 + "a",
		"leading hyphen":  "-bad.internal",
		"trailing hyphen": "bad-.internal",
		"empty label":     "bad..internal",
		"whitespace":      "app internal",
		"control byte":    "app\x00internal",
	}
	for name, h := range cases {
		if ValidHost(h) {
			t.Fatalf("%s: ValidHost(%q) = true, want false", name, h)
		}
	}
}
