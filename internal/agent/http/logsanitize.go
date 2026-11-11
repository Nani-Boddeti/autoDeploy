package agenthttp

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// logRedacted is the replacement for every pattern match. It is the same
// marker internal/crypto's RedactingWriter writes, so the UI shows one
// convention.
const logRedacted = "[REDACTED]"

// maxSanitizedLogBytes is the store's per-chunk body cap
// (postgres.MaxLogChunkBytes); redaction can grow a short match, so the result
// is re-truncated on a rune boundary.
const maxSanitizedLogBytes = 4096

// logSecretPatterns are the L5 control-plane second pass (tasks/15g-plan.md).
// Defense in depth only: the agent's exact-value redaction is primary, and
// the control plane deliberately does not hold secret values here. Order
// matters: the Authorization header rule runs before the bare Bearer rule so
// the whole header value is consumed.
//
// Each entry keeps `keep` (a regexp replacement template) around the part
// that is not secret.
var logSecretPatterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	// PEM private key block; an unterminated block (the end marker lives in a
	// later chunk) is redacted to the end of the body.
	{regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|\z)`), logRedacted},
	// Authorization header, key=value or JSON form (optional quotes around key
	// and value, ':' or '='): redact the value (any scheme) up to its closing
	// quote, or to end of line when unquoted. Quotes around the value go too.
	{regexp.MustCompile(`(?i)(["']?authorization["']?[ \t]*[:=][ \t]*)(?:"[^"\n]*"|'[^'\n]*'|[^\n]+)`), "${1}" + logRedacted},
	// Bearer <token>.
	{regexp.MustCompile(`(?i)(\bbearer[ \t]+)[A-Za-z0-9._~+/=-]{8,}`), "${1}" + logRedacted},
	// GitHub token shapes and this system's own credential/enrollment tokens.
	{regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`), logRedacted},
	{regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`), logRedacted},
	{regexp.MustCompile(`(?:srv|enr)\.v1\.[A-Za-z0-9_-]{20,}`), logRedacted},
}

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
	for _, p := range logSecretPatterns {
		body = p.re.ReplaceAllString(body, p.repl)
	}
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
