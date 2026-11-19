package logsanitize

import (
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	pem := "-----BEGIN RSA PRIVATE KEY-----\nMIIEabc\n-----END RSA PRIVATE KEY-----"
	cases := []struct{ name, in, want string }{
		{"plain", "step 1/3 done", "step 1/3 done"},
		{"ghp", "x ghp_" + strings.Repeat("a", 36) + " y", "x [REDACTED] y"},
		{"short ghp kept", "ghp_short", "ghp_short"},
		{"bearer", "Bearer abcdef1234567890", "Bearer [REDACTED]"},
		{"authorization", "Authorization: Basic dXNlcjpwYXNz\nnext", "Authorization: [REDACTED]\nnext"},
		{"pem", "a\n" + pem + "\nb", "a\n[REDACTED]\nb"},
		{"own tokens", "srv.v1." + strings.Repeat("a", 43), "[REDACTED]"},
	}
	for _, tc := range cases {
		if got := Redact(tc.in); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestRedactIsUncapped(t *testing.T) {
	in := strings.Repeat("x", 10000)
	if got := Redact(in); len(got) != 10000 {
		t.Fatalf("len=%d", len(got))
	}
}
