package github

import (
	"strings"
	"unicode/utf8"
)

// ValidOpaqueID preserves opaque identifier compatibility: values are bounded
// but otherwise unconstrained, except NUL which cannot safely cross storage
// boundaries.
func ValidOpaqueID(value string) bool {
	return len(value) > 0 && len(value) <= 128 && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

// ValidBranchRef applies Git's branch-name restrictions to the one allowed
// deployment ref namespace.
func ValidBranchRef(ref string) bool {
	const prefix = "refs/heads/"
	if !strings.HasPrefix(ref, prefix) || len(ref) <= len(prefix) || len(ref) > 255 || strings.HasSuffix(ref, "/") || strings.Contains(ref, "//") || strings.Contains(ref, "..") || strings.Contains(ref, "@{") || ref == "@" {
		return false
	}
	for _, part := range strings.Split(strings.TrimPrefix(ref, prefix), "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	for _, c := range ref {
		if c <= 0x20 || c == 0x7f || strings.ContainsRune("~^:?*[\\", c) {
			return false
		}
	}
	return true
}

func ValidCommitSHA(value string) bool {
	if len(value) != 40 || value == strings.Repeat("0", 40) {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
