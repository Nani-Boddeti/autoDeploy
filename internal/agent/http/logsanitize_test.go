package agenthttp

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSanitizeLogBody(t *testing.T) {
	pem := "-----BEGIN RSA PRIVATE KEY-----\nMIIEabc\n-----END RSA PRIVATE KEY-----"
	cases := []struct{ name, in, want string }{
		{"plain", "step 1/3 done\n\tok", "step 1/3 done\n\tok"},
		{"nul and c0", "a\x00b\x01c\x1bd\re\x7ff", "abcdef"},
		{"keeps tab newline unicode", "héllo\t世界\n", "héllo\t世界\n"},
		{"ghp", "x ghp_" + strings.Repeat("a", 36) + " y", "x [REDACTED] y"},
		{"ghs gho ghu ghr", "ghs_" + strings.Repeat("1", 30) + " gho_" + strings.Repeat("2", 30) + " ghu_" + strings.Repeat("3", 30) + " ghr_" + strings.Repeat("4", 30), "[REDACTED] [REDACTED] [REDACTED] [REDACTED]"},
		{"fine grained", "github_pat_" + strings.Repeat("A1_", 20), "[REDACTED]"},
		{"short ghp kept", "ghp_short", "ghp_short"},
		{"bearer", "curl -H 'Bearer abcdef1234567890'", "curl -H 'Bearer [REDACTED]'"},
		{"bearer lower", "bearer abcdef1234567890", "bearer [REDACTED]"},
		{"authorization header", "Authorization: Basic dXNlcjpwYXNz\nnext", "Authorization: [REDACTED]\nnext"},
		{"authorization bearer", "authorization=Bearer abcdef1234567890", "authorization=[REDACTED]"},
		{"authorization json", `{"authorization": "Basic dXNlcjpwYXNz", "x": 1}`, `{"authorization": [REDACTED], "x": 1}`},
		{"authorization json upper key", `{"Authorization":"token abc123"}`, `{"Authorization":[REDACTED]}`},
		{"authorization quoted eq", `authorization="Basic dXNlcjpwYXNz" rest`, `authorization=[REDACTED] rest`},
		{"authorization single quoted", `Authorization: 'Bearer x'`, `Authorization: [REDACTED]`},
		{"pem", "before\n" + pem + "\nafter", "before\n[REDACTED]\nafter"},
		{"pem unterminated", "k -----BEGIN PRIVATE KEY-----\nMIIE", "k [REDACTED]"},
		{"own tokens", "srv.v1." + strings.Repeat("a", 43) + " enr.v1." + strings.Repeat("b", 43), "[REDACTED] [REDACTED]"},
		{"only control", "\x00\x01", ""},
		{"invalid utf8", "a\xffb", "a�b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeLogBody(tc.in); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestSanitizeLogBodyKeepsCapAndRunes(t *testing.T) {
	// Redaction grows short matches (Bearer + 8 chars -> +2 bytes); 4096 bytes
	// of them must still fit, never splitting a rune.
	grow := strings.Repeat("Bearer abcdefgh ", 256)
	for _, in := range []string{grow, strings.Repeat("世", 1365) + "ab", strings.Repeat("é", 2048+10)} {
		got := sanitizeLogBody(in)
		if len(got) > 4096 || !utf8.ValidString(got) {
			t.Fatalf("len=%d valid=%v", len(got), utf8.ValidString(got))
		}
	}
	if got := sanitizeLogBody(strings.Repeat("é", 2048+10)); len(got) != 4096 {
		t.Fatalf("truncate len=%d", len(got))
	}
}
