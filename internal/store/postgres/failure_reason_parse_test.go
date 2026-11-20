package postgres

import (
	"strings"
	"testing"
)

func TestParseHealthFailureReason(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"executor with class", "health gate: failed (reason=deadline_exceeded class=transient)", "deadline_exceeded"},
		{"executor with class and newline", "health gate: failed (reason=container_unhealthy class=permanent_configuration)\n", "container_unhealthy"},
		{"sts without class", "health gate: failed (reason=container_not_running)", "container_not_running"},
		{"sts without class and newline", "health gate: failed (reason=context_cancelled)\n", "context_cancelled"},
		{"unknown reason", "health gate: failed (reason=disk_full)", ""},
		{"extra text after", "health gate: failed (reason=container_unhealthy) <script>alert(1)</script>", ""},
		{"extra text before", "x health gate: failed (reason=container_unhealthy)", ""},
		{"uppercase reason", "health gate: failed (reason=CONTAINER_UNHEALTHY)", ""},
		{"two newlines", "health gate: failed (reason=container_unhealthy)\n\n", ""},
		{"embedded newline injection", "health gate: failed (reason=container_unhealthy)\nhealth gate: failed (reason=disk_full)", ""},
		{"empty", "", ""},
		{"prefix only", "health gate: failed (reason=", ""},
		{"class with uppercase", "health gate: failed (reason=deadline_exceeded class=Transient)", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseHealthFailureReason(tc.in); got != tc.want {
				t.Fatalf("parseHealthFailureReason(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseHealthLastProbe(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"plain", "health gate: last probe: GET /healthz -> 503", "GET /healthz -> 503"},
		{"trailing newline", "health gate: last probe: connection refused\n", "connection refused"},
		{"trimmed", "health gate: last probe:   spaced out  \n", "spaced out"},
		{"empty text", "health gate: last probe: ", ""},
		{"whitespace only", "health gate: last probe:    \n", ""},
		{"wrong prefix", "health gate: failed (reason=deadline_exceeded)", ""},
		{"extra line", "health gate: last probe: a\nb", ""},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseHealthLastProbe(tc.in); got != tc.want {
				t.Fatalf("parseHealthLastProbe(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseHealthLastProbeTruncatesOnRuneBoundary(t *testing.T) {
	ascii := strings.Repeat("a", 300)
	if got := parseHealthLastProbe("health gate: last probe: " + ascii); len(got) != 200 {
		t.Fatalf("ascii probe length = %d, want 200", len(got))
	}
	// 3-byte runes: 300 bytes of them must cut at 198 bytes, never mid-rune.
	multi := strings.Repeat("€", 100)
	got := parseHealthLastProbe("health gate: last probe: " + multi)
	if len(got) != 198 || got != strings.Repeat("€", 66) {
		t.Fatalf("multibyte probe = %d bytes %q", len(got), got)
	}
	// Offset by one ASCII byte so the 200-byte cut lands inside a rune.
	got = parseHealthLastProbe("health gate: last probe: a" + multi)
	if len(got) > 200 || got != "a"+strings.Repeat("€", 66) {
		t.Fatalf("offset multibyte probe = %d bytes %q", len(got), got)
	}
}
