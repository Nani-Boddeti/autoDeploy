package postgres

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// maxHealthProbeBytes bounds the last-probe text surfaced to the UI.
const maxHealthProbeBytes = 200

var (
	healthFailureLineRE = regexp.MustCompile(`^health gate: failed \(reason=([a-z_]+)( class=[a-z_]+)?\)\n?$`)
	healthProbeLineRE   = regexp.MustCompile(`^health gate: last probe: ([^\n]*)\n?$`)

	// healthFailureReasons is the closed reason enum of the agent health gate.
	// Anything else (including future values) is dropped: no free text from a
	// log row ever reaches a view through the reason field.
	healthFailureReasons = map[string]struct{}{
		"deadline_exceeded":     {},
		"context_cancelled":     {},
		"container_not_running": {},
		"container_unhealthy":   {},
	}
)

// parseHealthFailureReason extracts the reason from a health-stream line
// shipped by the agent in one of its three formats
// ("health gate: failed (reason=R class=C)" or without the class, with or
// without a trailing newline). It returns R only when it is allowlisted,
// otherwise "".
func parseHealthFailureReason(body string) string {
	m := healthFailureLineRE.FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	if _, ok := healthFailureReasons[m[1]]; !ok {
		return ""
	}
	return m[1]
}

// Accepted risk (L1): the probe text is free-form, but it is shape-allowlisted
// at the agent, C0 controls are stripped at server ingest, and the UI escapes it.
//
// parseHealthLastProbe extracts the text of a "health gate: last probe: ..."
// line, trimmed and cut to maxHealthProbeBytes on a rune boundary; "" when the
// line does not match or the text is empty. Log rows are redacted at ingest;
// the view layer must still escape the text.
func parseHealthLastProbe(body string) string {
	m := healthProbeLineRE.FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return truncateUTF8(strings.TrimSpace(m[1]), maxHealthProbeBytes)
}

// truncateUTF8 returns s cut to at most max bytes without splitting a rune.
func truncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
