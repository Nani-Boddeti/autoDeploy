package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

var ErrInvalidToken = errors.New("invalid server token")

// CredentialToken is the durable opaque bearer credential an enrolled agent
// presents on every authenticated request: srv.v1.<32 random bytes base64url>.
type CredentialToken string

// EnrollmentToken is the single-use, short-lived token cmd/admin prints once
// and cmd/agent exchanges for a durable credential: enr.v1.<32 random bytes
// base64url>. Its distinct prefix prevents cross-use with CredentialToken:
// parsing either as the other type always fails.
type EnrollmentToken string

const (
	credentialTokenPrefix = "srv.v1."
	enrollmentTokenPrefix = "enr.v1."
	tokenRawBytes         = 32
	tokenEncodedBytes     = 43 // base64.RawURLEncoding length for 32 bytes
)

// NewCredentialToken mints a fresh opaque credential token from randomness
// (crypto/rand.Reader when nil).
func NewCredentialToken(randomness io.Reader) (CredentialToken, error) {
	raw, err := newTokenBytes(randomness)
	if err != nil {
		return "", fmt.Errorf("generate server credential token: %w", err)
	}
	return CredentialToken(credentialTokenPrefix + base64.RawURLEncoding.EncodeToString(raw)), nil
}

// NewEnrollmentToken mints a fresh opaque enrollment token from randomness
// (crypto/rand.Reader when nil).
func NewEnrollmentToken(randomness io.Reader) (EnrollmentToken, error) {
	raw, err := newTokenBytes(randomness)
	if err != nil {
		return "", fmt.Errorf("generate server enrollment token: %w", err)
	}
	return EnrollmentToken(enrollmentTokenPrefix + base64.RawURLEncoding.EncodeToString(raw)), nil
}

func newTokenBytes(randomness io.Reader) ([]byte, error) {
	if randomness == nil {
		randomness = rand.Reader
	}
	raw := make([]byte, tokenRawBytes)
	if _, err := io.ReadFull(randomness, raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func (t CredentialToken) String() string { return string(t) }
func (t EnrollmentToken) String() string { return string(t) }

// ParseCredentialToken strictly validates syntax, length, and prefix. An
// enrollment token (or any other prefix) is always rejected.
func ParseCredentialToken(value string) (CredentialToken, error) {
	if _, err := decodeToken(value, credentialTokenPrefix); err != nil {
		return "", err
	}
	return CredentialToken(value), nil
}

// ParseEnrollmentToken strictly validates syntax, length, and prefix. A
// credential token (or any other prefix) is always rejected.
func ParseEnrollmentToken(value string) (EnrollmentToken, error) {
	if _, err := decodeToken(value, enrollmentTokenPrefix); err != nil {
		return "", err
	}
	return EnrollmentToken(value), nil
}

// decodeToken enforces exact length, exact prefix, and canonical
// (non-alternate) base64url encoding of exactly 32 raw bytes.
func decodeToken(value, prefix string) ([]byte, error) {
	if len(value) != len(prefix)+tokenEncodedBytes || !strings.HasPrefix(value, prefix) {
		return nil, ErrInvalidToken
	}
	encoded := value[len(prefix):]
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(raw) != tokenRawBytes || base64.RawURLEncoding.EncodeToString(raw) != encoded {
		return nil, ErrInvalidToken
	}
	return raw, nil
}

// CredentialTokenDigest returns the domain-separated SHA-256 digest that
// alone is ever persisted for a credential token.
func CredentialTokenDigest(t CredentialToken) ([32]byte, error) {
	raw, err := decodeToken(string(t), credentialTokenPrefix)
	if err != nil {
		return [32]byte{}, err
	}
	return domainDigest("autodeploy/server-credential-token/v1", raw), nil
}

// EnrollmentTokenDigest returns the domain-separated SHA-256 digest that
// alone is ever persisted for an enrollment token.
func EnrollmentTokenDigest(t EnrollmentToken) ([32]byte, error) {
	raw, err := decodeToken(string(t), enrollmentTokenPrefix)
	if err != nil {
		return [32]byte{}, err
	}
	return domainDigest("autodeploy/server-enrollment-token/v1", raw), nil
}

func domainDigest(domain string, raw []byte) [32]byte {
	return sha256.Sum256(append([]byte(domain+"\x00"), raw...))
}

// ConstantTimeEqualDigest compares two digests in constant time. Repository
// authentication/enrollment paths must use this instead of == or a SQL
// WHERE-equality lookup so presented bearer material never influences
// comparison timing.
func ConstantTimeEqualDigest(a, b [32]byte) bool {
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}
