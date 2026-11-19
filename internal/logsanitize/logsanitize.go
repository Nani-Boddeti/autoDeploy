// Package logsanitize holds the regex second-pass redaction shared by the
// control plane's log pipeline. It is a leaf package (stdlib only) so both
// internal/logging and internal/agent/http can use it without import cycles.
package logsanitize

import "regexp"

// Redacted is the replacement for every pattern match. It is the same marker
// internal/crypto's RedactingWriter writes, so there is one convention.
const Redacted = "[REDACTED]"

// patterns are defense in depth only: exact-value redaction is primary. Order
// matters: the Authorization header rule runs before the bare Bearer rule so
// the whole header value is consumed.
var patterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	// PEM private key block; an unterminated block is redacted to the end.
	{regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|\z)`), Redacted},
	// Authorization header, key=value or JSON form: redact the value (any
	// scheme) up to its closing quote, or to end of line when unquoted.
	{regexp.MustCompile(`(?i)(["']?authorization["']?[ \t]*[:=][ \t]*)(?:"[^"\n]*"|'[^'\n]*'|[^\n]+)`), "${1}" + Redacted},
	// Bearer <token>.
	{regexp.MustCompile(`(?i)(\bbearer[ \t]+)[A-Za-z0-9._~+/=-]{8,}`), "${1}" + Redacted},
	// GitHub token shapes and this system's own credential/enrollment tokens.
	{regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`), Redacted},
	{regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`), Redacted},
	{regexp.MustCompile(`(?:srv|enr)\.v1\.[A-Za-z0-9_-]{20,}`), Redacted},
}

// Redact applies every secret pattern to s. The result is uncapped; callers
// that need a size bound truncate themselves.
func Redact(s string) string {
	for _, p := range patterns {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	return s
}
