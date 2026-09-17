package agentclient

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"autodeploy/internal/server"
)

// maxEnrollmentTokenStdinBytes bounds a token read from stdin: the encoded
// token itself is fixed-length (see internal/server.token.go), plus a
// generous allowance for a trailing newline and any accidental surrounding
// whitespace, which is rejected rather than silently trimmed beyond one
// newline.
const maxEnrollmentTokenStdinBytes = 256

// ReadEnrollmentToken reads the single-use enrollment token either from a
// protected file (path != "", subject to the same 0600/owner discipline as
// the credential file) or from stdin. It is never accepted from argv or an
// environment variable: both are readable by other local users via the
// process list or shell history, which a short-lived bearer token must
// never risk.
func ReadEnrollmentToken(path string, stdin io.Reader) (server.EnrollmentToken, error) {
	var raw string
	if path != "" {
		contents, err := readProtectedFile(path)
		if err != nil {
			return "", fmt.Errorf("read enrollment token file: %w", err)
		}
		raw = string(contents)
	} else {
		if stdin == nil {
			return "", errors.New("no enrollment token source")
		}
		contents, err := io.ReadAll(io.LimitReader(stdin, maxEnrollmentTokenStdinBytes+1))
		if err != nil {
			return "", fmt.Errorf("read enrollment token from stdin: %w", err)
		}
		if len(contents) > maxEnrollmentTokenStdinBytes {
			return "", errors.New("enrollment token too large")
		}
		raw = string(contents)
	}
	raw = strings.TrimSuffix(strings.TrimSuffix(raw, "\n"), "\r")
	token, err := server.ParseEnrollmentToken(raw)
	if err != nil {
		return "", errors.New("invalid enrollment token")
	}
	return token, nil
}
