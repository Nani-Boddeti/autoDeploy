package docker

import (
	"errors"
	"fmt"
	"regexp"
)

// ErrMissingIdentifier is wrapped into the error returned when a required
// identifier field is empty.
var ErrMissingIdentifier = errors.New("docker label: required identifier is missing")

// ErrInvalidIdentifier is wrapped into the error returned when an
// identifier field is not a validated opaque ID, digest, commit SHA, or
// revision. This is the structural refusal that keeps hostnames, env
// values, and other free-form or secret content out of Docker labels: every
// label value passes through one of these validators before it can ever
// reach a label map.
var ErrInvalidIdentifier = errors.New("docker label: identifier is not a validated opaque id, digest, sha, or revision")

var (
	// opaqueIDPattern matches internal opaque identifiers (e.g. ULIDs,
	// UUIDs without hyphens-as-a-requirement, or the store's own ID
	// format): letters, digits, '-', '_' only, bounded length. It never
	// matches a dotted hostname, an email, or a path.
	opaqueIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
	// hexDigestPattern matches an OCI-style content digest, with or
	// without the "sha256:" algorithm prefix.
	hexDigestPattern = regexp.MustCompile(`^(sha256:)?[a-f0-9]{64}$`)
	// commitSHAPattern matches a full (not abbreviated) Git commit SHA-1.
	commitSHAPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)
	// revisionPattern matches a canonical non-negative decimal revision
	// counter: no leading zeros (so "007" and "7" can never be accepted as
	// two different labels for the same revision), bounded so it cannot
	// smuggle arbitrary text.
	revisionPattern = regexp.MustCompile(`^(0|[1-9][0-9]{0,19})$`)
)

type identifierValidator func(field, value string) (string, error)

func requireOpaqueID(field, value string) (string, error) {
	return requirePattern(field, value, opaqueIDPattern)
}

func requireHexDigest(field, value string) (string, error) {
	return requirePattern(field, value, hexDigestPattern)
}

func requireCommitSHA(field, value string) (string, error) {
	return requirePattern(field, value, commitSHAPattern)
}

func requireRevision(field, value string) (string, error) {
	return requirePattern(field, value, revisionPattern)
}

func requirePattern(field, value string, pattern *regexp.Regexp) (string, error) {
	if value == "" {
		return "", fmt.Errorf("%w: %s", ErrMissingIdentifier, field)
	}
	if !pattern.MatchString(value) {
		return "", fmt.Errorf("%w: %s", ErrInvalidIdentifier, field)
	}
	return value, nil
}
