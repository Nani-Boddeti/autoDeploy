// Package auth contains non-transport authentication domain primitives.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

var usernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,63}$`)

// CanonicalUsername trims only ASCII spaces and lowercases only ASCII letters.
func CanonicalUsername(input string) (string, error) {
	value := strings.Trim(input, " ")
	if len(value) < 3 || len(value) > 64 {
		return "", errors.New("invalid username")
	}
	canonical := []byte(value)
	for i := range canonical {
		if canonical[i] >= 0x80 {
			return "", errors.New("username must be ASCII")
		}
		if canonical[i] >= 'A' && canonical[i] <= 'Z' {
			canonical[i] += 'a' - 'A'
		}
	}
	value = string(canonical)
	if !usernamePattern.MatchString(value) {
		return "", errors.New("invalid username")
	}
	return value, nil
}

func ValidatePassword(password string) error {
	if !utf8.ValidString(password) {
		return errors.New("password is not valid UTF-8")
	}
	if len(password) < 15 || len(password) > 1024 {
		return errors.New("password must be 15 to 1024 bytes")
	}
	return nil
}

type Argon2Policy struct {
	Revision              uint64
	MemoryKiB, Iterations uint32
	Lanes                 uint8
}

func (p Argon2Policy) Validate() error {
	if p.Revision == 0 || p.MemoryKiB < 19*1024 || p.MemoryKiB > 64*1024 || p.Iterations < 2 || p.Iterations > 5 || p.Lanes != 1 {
		return errors.New("invalid Argon2 policy")
	}
	return nil
}

type PasswordVerifier struct {
	PHC            string
	PolicyRevision uint64
}

type PasswordVerification struct{ Valid, RehashNeeded bool }

const argonSaltBytes = 16
const argonHashBytes = 32
const argonSaltEncodedBytes = 22
const argonHashEncodedBytes = 43
const maxPHCBytes = 128

func HashPassword(password string, policy Argon2Policy, randomness io.Reader) (PasswordVerifier, error) {
	if err := ValidatePassword(password); err != nil {
		return PasswordVerifier{}, err
	}
	if err := policy.Validate(); err != nil {
		return PasswordVerifier{}, err
	}
	if randomness == nil {
		randomness = rand.Reader
	}
	salt := make([]byte, argonSaltBytes)
	if _, err := io.ReadFull(randomness, salt); err != nil {
		return PasswordVerifier{}, fmt.Errorf("generate password salt: %w", err)
	}
	hash := argon2.IDKey([]byte(password), salt, policy.Iterations, policy.MemoryKiB, policy.Lanes, argonHashBytes)
	return PasswordVerifier{PHC: formatPHC(policy.MemoryKiB, policy.Iterations, policy.Lanes, salt, hash), PolicyRevision: policy.Revision}, nil
}

func formatPHC(memory, iterations uint32, lanes uint8, salt, hash []byte) string {
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", memory, iterations, lanes, base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(hash))
}

type parsedPHC struct {
	memory, iterations uint32
	lanes              uint8
	salt, hash         []byte
}

func parsePHC(value string) (parsedPHC, error) {
	if len(value) < 68 || len(value) > maxPHCBytes || value[len(value)-44] != '$' || value[len(value)-67] != '$' {
		return parsedPHC{}, errors.New("invalid Argon2 verifier")
	}
	parts := strings.Split(value, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v=19" || len(parts[3]) > 32 || len(parts[4]) != argonSaltEncodedBytes || len(parts[5]) != argonHashEncodedBytes {
		return parsedPHC{}, errors.New("invalid Argon2 verifier")
	}
	fields := strings.Split(parts[3], ",")
	if len(fields) != 3 {
		return parsedPHC{}, errors.New("invalid Argon2 parameters")
	}
	values := make([]uint64, 3)
	for i, prefix := range []string{"m=", "t=", "p="} {
		if !strings.HasPrefix(fields[i], prefix) {
			return parsedPHC{}, errors.New("noncanonical Argon2 parameters")
		}
		number := strings.TrimPrefix(fields[i], prefix)
		if number == "" || (len(number) > 1 && number[0] == '0') {
			return parsedPHC{}, errors.New("noncanonical Argon2 parameter")
		}
		parsed, err := strconv.ParseUint(number, 10, 32)
		if err != nil {
			return parsedPHC{}, errors.New("invalid Argon2 parameter")
		}
		values[i] = parsed
	}
	if values[0] > 256*1024 || values[1] == 0 || values[1] > 10 || values[2] == 0 || values[2] > 4 || values[0] < 8*values[2] {
		return parsedPHC{}, errors.New("Argon2 parameters out of bounds")
	}
	salt, err := decodeCanonicalRawStd(parts[4])
	if err != nil || len(salt) != argonSaltBytes {
		return parsedPHC{}, errors.New("invalid Argon2 salt")
	}
	hash, err := decodeCanonicalRawStd(parts[5])
	if err != nil || len(hash) != argonHashBytes {
		return parsedPHC{}, errors.New("invalid Argon2 hash")
	}
	return parsedPHC{uint32(values[0]), uint32(values[1]), uint8(values[2]), salt, hash}, nil
}

func decodeCanonicalRawStd(value string) ([]byte, error) {
	if value == "" || strings.Contains(value, "=") {
		return nil, errors.New("invalid base64")
	}
	decoded, err := base64.RawStdEncoding.DecodeString(value)
	if err != nil || base64.RawStdEncoding.EncodeToString(decoded) != value {
		return nil, errors.New("invalid base64")
	}
	return decoded, nil
}

func VerifyPassword(password string, verifier PasswordVerifier, policy Argon2Policy) (PasswordVerification, error) {
	if err := ValidatePassword(password); err != nil {
		return PasswordVerification{}, err
	}
	if err := policy.Validate(); err != nil {
		return PasswordVerification{}, err
	}
	if verifier.PolicyRevision == 0 {
		return PasswordVerification{}, errors.New("invalid password verifier revision")
	}
	if verifier.PolicyRevision > policy.Revision {
		return PasswordVerification{}, errors.New("password verifier policy revision is newer than current policy")
	}
	parsed, err := parsePHC(verifier.PHC)
	if err != nil {
		return PasswordVerification{}, err
	}
	actual := argon2.IDKey([]byte(password), parsed.salt, parsed.iterations, parsed.memory, parsed.lanes, uint32(len(parsed.hash)))
	valid := hmac.Equal(actual, parsed.hash)
	return PasswordVerification{Valid: valid, RehashNeeded: valid && (verifier.PolicyRevision < policy.Revision || parsed.memory != policy.MemoryKiB || parsed.iterations != policy.Iterations || parsed.lanes != policy.Lanes)}, nil
}

type DummyPasswordVerifier struct {
	verifier PasswordVerifier
	policy   Argon2Policy
}

// BuildDummyPasswordVerifier performs startup-time work for an unknown-user verifier.
func BuildDummyPasswordVerifier(policy Argon2Policy) (DummyPasswordVerifier, error) {
	if err := policy.Validate(); err != nil {
		return DummyPasswordVerifier{}, err
	}
	salt := [argonSaltBytes]byte{0x51, 0x9c, 0x27, 0xe4, 0x62, 0x8d, 0x03, 0xb7, 0x44, 0xfa, 0x16, 0xc1, 0x75, 0x2b, 0xde, 0x90}
	hash := argon2.IDKey([]byte("autodeploy-non-account-dummy"), salt[:], policy.Iterations, policy.MemoryKiB, policy.Lanes, argonHashBytes)
	return DummyPasswordVerifier{verifier: PasswordVerifier{PHC: formatPHC(policy.MemoryKiB, policy.Iterations, policy.Lanes, salt[:], hash), PolicyRevision: policy.Revision}, policy: policy}, nil
}

// VerifyDummyPassword always performs one verification but never exposes authentication success.
func VerifyDummyPassword(password string, dummy DummyPasswordVerifier) error {
	_, err := VerifyPassword(password, dummy.verifier, dummy.policy)
	return err
}

type SessionToken string

const sessionTokenPrefix = "v1."

func NewSessionToken(randomness io.Reader) (SessionToken, error) {
	if randomness == nil {
		randomness = rand.Reader
	}
	raw := make([]byte, 32)
	if _, err := io.ReadFull(randomness, raw); err != nil {
		return "", fmt.Errorf("generate session token: %w", err)
	}
	return SessionToken(sessionTokenPrefix + base64.RawURLEncoding.EncodeToString(raw)), nil
}
func (t SessionToken) String() string { return string(t) }
func ParseSessionToken(value string) (SessionToken, error) {
	if len(value) != len(sessionTokenPrefix)+43 || !strings.HasPrefix(value, sessionTokenPrefix) {
		return "", errors.New("invalid session token")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, sessionTokenPrefix))
	if err != nil || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(raw) != strings.TrimPrefix(value, sessionTokenPrefix) {
		return "", errors.New("invalid session token")
	}
	return SessionToken(value), nil
}
func SessionTokenDigest(token SessionToken) ([32]byte, error) {
	parsed, err := ParseSessionToken(token.String())
	if err != nil {
		return [32]byte{}, err
	}
	raw, _ := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(parsed.String(), sessionTokenPrefix))
	return sha256.Sum256(append([]byte("autodeploy/session-token/v1\x00"), raw...)), nil
}

type SessionPolicy struct{ IdleSeconds, AbsoluteSeconds, MaxActive, TouchIntervalSeconds int }

const (
	SessionIdleTimeout   = 30 * time.Minute
	SessionAbsoluteLife  = 8 * time.Hour
	MaxActiveSessions    = 10
	SessionTouchInterval = 5 * time.Minute
)

func DefaultSessionPolicy() SessionPolicy {
	return SessionPolicy{
		IdleSeconds:          int(SessionIdleTimeout.Seconds()),
		AbsoluteSeconds:      int(SessionAbsoluteLife.Seconds()),
		MaxActive:            MaxActiveSessions,
		TouchIntervalSeconds: int(SessionTouchInterval.Seconds()),
	}
}
func (p SessionPolicy) Validate() error {
	if p != DefaultSessionPolicy() {
		return errors.New("invalid session policy")
	}
	return nil
}

type Role string

const RoleOwner Role = "owner"

type Operation string

const OperationAdmin Operation = "admin"

type Principal struct {
	UserID, OwnerID string
	Role            Role
	AuthRevision    uint64
	SessionID       string
}

func (p Principal) Validate() error {
	if p.UserID == "" || p.OwnerID == "" || p.SessionID == "" || p.AuthRevision == 0 || p.Role != RoleOwner {
		return errors.New("invalid principal")
	}
	return nil
}
func CanAccess(principal Principal, resourceOwnerID string, operation Operation) bool {
	return principal.Validate() == nil && resourceOwnerID != "" && principal.OwnerID == resourceOwnerID && principal.Role == RoleOwner && operation == OperationAdmin
}

type CSRFKeyRing struct {
	ActiveVersion string
	Keys          map[string][]byte
}

func (r CSRFKeyRing) validate() error {
	if r.ActiveVersion == "" || !validKeyVersion(r.ActiveVersion) || len(r.Keys[r.ActiveVersion]) < 32 {
		return errors.New("invalid CSRF key ring")
	}
	for v, key := range r.Keys {
		if !validKeyVersion(v) || len(key) < 32 {
			return errors.New("invalid CSRF key")
		}
	}
	return nil
}
func validKeyVersion(v string) bool {
	if len(v) == 0 || len(v) > 32 {
		return false
	}
	for _, c := range v {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
func CanonicalHTTPSOrigin(value string) (string, error) {
	u, err := url.Parse(value)
	if err != nil || strings.ToLower(u.Scheme) != "https" || u.Opaque != "" || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", errors.New("invalid HTTPS origin")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || strings.Contains(host, "%") {
		return "", errors.New("invalid HTTPS origin")
	}
	for i := range len(host) {
		if host[i] >= 0x80 {
			return "", errors.New("invalid HTTPS origin")
		}
	}
	port := u.Port()
	if strings.Contains(u.Host, ":") && port == "" && !strings.HasSuffix(u.Host, "]") {
		return "", errors.New("invalid HTTPS origin")
	}
	if port != "" {
		parsed, parseErr := strconv.ParseUint(port, 10, 16)
		if parseErr != nil || parsed == 0 {
			return "", errors.New("invalid HTTPS origin")
		}
		if parsed != 443 {
			host = net.JoinHostPort(host, strconv.FormatUint(parsed, 10))
		} else if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return "https://" + host, nil
}
func csrfMAC(key []byte, domain, context, origin string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(domain))
	m.Write([]byte{0})
	m.Write([]byte(context))
	m.Write([]byte{0})
	m.Write([]byte(origin))
	return m.Sum(nil)
}
func csrfToken(r CSRFKeyRing, domain, context, origin string) (string, error) {
	if err := r.validate(); err != nil {
		return "", err
	}
	origin, err := CanonicalHTTPSOrigin(origin)
	if err != nil {
		return "", err
	}
	return "v1." + r.ActiveVersion + "." + base64.RawURLEncoding.EncodeToString(csrfMAC(r.Keys[r.ActiveVersion], domain, context, origin)), nil
}
func validateCSRF(r CSRFKeyRing, token, domain, context, origin string) bool {
	if r.validate() != nil {
		return false
	}
	origin, err := CanonicalHTTPSOrigin(origin)
	if err != nil {
		return false
	}
	if len(token) < 46 || len(token) > 79 {
		return false
	}
	p := strings.Split(token, ".")
	if len(p) != 3 || p[0] != "v1" || !validKeyVersion(p[1]) || len(p[2]) != 43 {
		return false
	}
	key, ok := r.Keys[p[1]]
	if !ok {
		return false
	}
	got, err := base64.RawURLEncoding.DecodeString(p[2])
	return err == nil && base64.RawURLEncoding.EncodeToString(got) == p[2] && hmac.Equal(got, csrfMAC(key, domain, context, origin))
}
func NewSessionCSRF(r CSRFKeyRing, sessionID, origin string) (string, error) {
	if sessionID == "" {
		return "", errors.New("empty session ID")
	}
	return csrfToken(r, "autodeploy/session-csrf/v1", sessionID, origin)
}
func ValidateSessionCSRF(r CSRFKeyRing, token, sessionID, origin string) bool {
	return sessionID != "" && validateCSRF(r, token, "autodeploy/session-csrf/v1", sessionID, origin)
}

type LoginNonce [32]byte

func NewLoginNonce(randomness io.Reader) (LoginNonce, error) {
	if randomness == nil {
		randomness = rand.Reader
	}
	var nonce LoginNonce
	_, err := io.ReadFull(randomness, nonce[:])
	if err != nil {
		return LoginNonce{}, err
	}
	return nonce, nil
}
func ParseLoginNonce(value string) (LoginNonce, error) {
	var nonce LoginNonce
	if len(value) != 43 {
		return nonce, errors.New("invalid login nonce")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) != len(nonce) || base64.RawURLEncoding.EncodeToString(decoded) != value {
		return nonce, errors.New("invalid login nonce")
	}
	copy(nonce[:], decoded)
	return nonce, nil
}
func (n LoginNonce) String() string { return base64.RawURLEncoding.EncodeToString(n[:]) }
func LoginNonceDigest(n LoginNonce) [32]byte {
	return sha256.Sum256(append([]byte("autodeploy/login-nonce/v1\x00"), n[:]...))
}
func NewLoginCSRF(r CSRFKeyRing, nonce LoginNonce, origin string) (string, error) {
	return csrfToken(r, "autodeploy/login-csrf/v1", string(nonce[:]), origin)
}
func ValidateLoginCSRF(r CSRFKeyRing, token string, nonce LoginNonce, origin string) bool {
	return validateCSRF(r, token, "autodeploy/login-csrf/v1", string(nonce[:]), origin)
}
