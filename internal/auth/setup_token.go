package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ErrInvalidSetupToken is returned for any malformed setup code.
var ErrInvalidSetupToken = errors.New("invalid setup token")

// SetupToken is the single-use, short-lived first-run code the host operator
// mints with `admin setup-token` and presents once at /setup:
// setup.v1.<32 random bytes base64url>. Its prefix is distinct from the
// server enrollment (enr.v1.), server credential (srv.v1.), and session (v1.)
// tokens, so parsing any of those as a SetupToken (or the reverse) fails.
type SetupToken string

const (
	setupTokenPrefix  = "setup.v1."
	setupTokenRaw     = 32
	setupTokenEncoded = 43 // base64.RawURLEncoding length for 32 bytes
)

// NewSetupToken mints a fresh setup code from randomness (crypto/rand.Reader
// when nil).
func NewSetupToken(randomness io.Reader) (SetupToken, error) {
	if randomness == nil {
		randomness = rand.Reader
	}
	raw := make([]byte, setupTokenRaw)
	if _, err := io.ReadFull(randomness, raw); err != nil {
		return "", fmt.Errorf("generate setup token: %w", err)
	}
	return SetupToken(setupTokenPrefix + base64.RawURLEncoding.EncodeToString(raw)), nil
}

func (t SetupToken) String() string { return string(t) }

// ParseSetupToken strictly validates prefix, length, and canonical base64url
// of exactly 32 raw bytes. It does not trim: callers decide how to normalize.
func ParseSetupToken(value string) (SetupToken, error) {
	if _, err := decodeSetupToken(value); err != nil {
		return "", err
	}
	return SetupToken(value), nil
}

func decodeSetupToken(value string) ([]byte, error) {
	if len(value) != len(setupTokenPrefix)+setupTokenEncoded || !strings.HasPrefix(value, setupTokenPrefix) {
		return nil, ErrInvalidSetupToken
	}
	encoded := value[len(setupTokenPrefix):]
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(raw) != setupTokenRaw || base64.RawURLEncoding.EncodeToString(raw) != encoded {
		return nil, ErrInvalidSetupToken
	}
	return raw, nil
}

// SetupTokenDigest returns the domain-separated SHA-256 digest that alone is
// ever persisted for a setup code.
func SetupTokenDigest(t SetupToken) ([32]byte, error) {
	raw, err := decodeSetupToken(string(t))
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(append([]byte("autodeploy/installation-setup-token/v1\x00"), raw...)), nil
}
