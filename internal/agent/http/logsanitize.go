package agenthttp

import (
	"strings"
	"unicode/utf8"

	"autodeploy/internal/logsanitize"
)

// maxSanitizedLogBytes is the store's per-chunk body cap
// (postgres.MaxLogChunkBytes); redaction can grow a short match, so the result
// is re-truncated on a rune boundary.
const maxSanitizedLogBytes = 4096

// sanitizeLogBody strips NUL and C0 control characters (and DEL) except tab
// and newline, replaces invalid UTF-8, redacts the secret patterns above, and
// keeps the result within 4096 bytes without splitting a rune. Newline is kept
// because the shipper's line model may send multi-line chunks. The result may
// be empty (a chunk of only control characters); the caller drops those.
func sanitizeLogBody(body string) string {
	body = strings.ToValidUTF8(body, "�")
	if strings.IndexFunc(body, isStrippedControl) >= 0 {
		body = strings.Map(func(r rune) rune {
			if isStrippedControl(r) {
				return -1
			}
			return r
		}, body)
	}
	body = logsanitize.Redact(body)
	if len(body) > maxSanitizedLogBytes {
		cut := maxSanitizedLogBytes
		for cut > 0 && !utf8.RuneStart(body[cut]) {
			cut--
		}
		body = body[:cut]
	}
	return body
}

func isStrippedControl(r rune) bool {
	return (r < 0x20 && r != '\t' && r != '\n') || r == 0x7f
}
