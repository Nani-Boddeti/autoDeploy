// Package crypto implements the bounded, versioned secret-cryptography primitives.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

const (
	SuiteAES256GCMRandomNonceV1 = "AES-256-GCM-RANDOMNONCE-v1"
	AADFormatV1                 = "v1"
	MaxPlaintextBytes           = 64 << 10
)

var (
	ErrInvalidInput      = errors.New("invalid secret cryptography input")
	ErrInvalidEnvelope   = errors.New("invalid secret envelope")
	ErrUnknownKey        = errors.New("unknown secret encryption key")
	ErrCryptographicFail = errors.New("secret cryptographic operation failed")
)

// Scope contains only immutable identifiers required to reconstruct AAD.
type Scope struct {
	OwnerID, ProjectID, EnvironmentID, VariableID, SecretVersionID string
	Purpose, DeliveryScope                                         string
}

// Envelope is the persisted non-plaintext representation of one secret version.
type Envelope struct {
	Suite, AADVersion, KEKID string
	Ciphertext, WrappedDEK   []byte
}

// KeyProvider separates envelope semantics from mounted-file and future KMS providers.
type KeyProvider interface {
	// Active returns a caller-owned key copy. Encrypt clears it before returning.
	Active() (id string, key []byte, err error)
	// Resolve returns a caller-owned key copy. Decrypt clears it before returning.
	Resolve(id string) ([]byte, error)
}

// WrapInvocationReserver is deliberately only a boundary for Step 2's durable ledger.
// Implementations must persist a non-reusable reservation before a KEK encryption operation.
type WrapInvocationReserver interface {
	ReserveWrapInvocation(kekID string) error
}

// Encrypt creates a fresh DEK and wraps it with the provider's active KEK.
func Encrypt(provider KeyProvider, reserver WrapInvocationReserver, scope Scope, plaintext []byte) (Envelope, error) {
	if provider == nil || reserver == nil || len(plaintext) > MaxPlaintextBytes || !validScope(scope) {
		return Envelope{}, ErrInvalidInput
	}
	kekID, kek, err := provider.Active()
	if err != nil || !validKeyID(kekID) || len(kek) != 32 {
		return Envelope{}, ErrCryptographicFail
	}
	defer clear(kek)
	// Reservation is intentionally before all KEK encryption. Step 2 supplies the durable,
	// non-reusable implementation; a failed wrap still leaves its reservation consumed.
	if err := reserver.ReserveWrapInvocation(kekID); err != nil {
		return Envelope{}, ErrCryptographicFail
	}
	dek := make([]byte, 32)
	defer clear(dek) // Go cannot guarantee compiler/runtime copies are cleared.
	if _, err := io.ReadFull(rand.Reader, dek); err != nil {
		return Envelope{}, ErrCryptographicFail
	}
	valueAAD := valueAAD(scope)
	valueCiphertext, err := seal(dek, plaintext, valueAAD)
	if err != nil {
		return Envelope{}, ErrCryptographicFail
	}
	wrappedDEK, err := seal(kek, dek, wrapAAD(kekID, scope))
	if err != nil {
		return Envelope{}, ErrCryptographicFail
	}
	return Envelope{Suite: SuiteAES256GCMRandomNonceV1, AADVersion: AADFormatV1, KEKID: kekID, Ciphertext: valueCiphertext, WrappedDEK: wrappedDEK}, nil
}

// Decrypt authenticates the wrapped DEK and value against the immutable scope.
func Decrypt(provider KeyProvider, scope Scope, envelope Envelope) ([]byte, error) {
	if provider == nil || !validScope(scope) || !validEnvelope(envelope) {
		return nil, ErrInvalidEnvelope
	}
	kek, err := provider.Resolve(envelope.KEKID)
	if err != nil {
		return nil, ErrCryptographicFail
	}
	defer clear(kek)
	if len(kek) != 32 {
		return nil, ErrCryptographicFail
	}
	dek, err := open(kek, envelope.WrappedDEK, wrapAAD(envelope.KEKID, scope))
	if err != nil || len(dek) != 32 {
		return nil, ErrCryptographicFail
	}
	defer clear(dek)
	plaintext, err := open(dek, envelope.Ciphertext, valueAAD(scope))
	if err != nil || len(plaintext) > MaxPlaintextBytes {
		return nil, ErrCryptographicFail
	}
	return plaintext, nil
}

// Rewrap authenticates an existing DEK with its original KEK and wraps it with
// the active KEK.  It deliberately leaves value ciphertext untouched.
func Rewrap(provider KeyProvider, reserver WrapInvocationReserver, scope Scope, envelope Envelope) (Envelope, error) {
	if provider == nil || reserver == nil || !validScope(scope) || !validEnvelope(envelope) {
		return Envelope{}, ErrInvalidInput
	}
	oldKey, err := provider.Resolve(envelope.KEKID)
	if err != nil || len(oldKey) != 32 {
		return Envelope{}, ErrCryptographicFail
	}
	defer clear(oldKey)
	dek, err := open(oldKey, envelope.WrappedDEK, wrapAAD(envelope.KEKID, scope))
	if err != nil || len(dek) != 32 {
		return Envelope{}, ErrCryptographicFail
	}
	defer clear(dek)
	newID, newKey, err := provider.Active()
	if err != nil || !validKeyID(newID) || len(newKey) != 32 || newID == envelope.KEKID {
		return Envelope{}, ErrCryptographicFail
	}
	defer clear(newKey)
	if err := reserver.ReserveWrapInvocation(newID); err != nil {
		return Envelope{}, ErrCryptographicFail
	}
	wrapped, err := seal(newKey, dek, wrapAAD(newID, scope))
	if err != nil {
		return Envelope{}, ErrCryptographicFail
	}
	return Envelope{Suite: envelope.Suite, AADVersion: envelope.AADVersion, KEKID: newID, Ciphertext: append([]byte(nil), envelope.Ciphertext...), WrappedDEK: wrapped}, nil
}

func seal(key, plaintext, aad []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, err
	}
	return aead.Seal(nil, nil, plaintext, aad), nil
}

func open(key, ciphertext, aad []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, err
	}
	return aead.Open(nil, nil, ciphertext, aad)
}

func valueAAD(scope Scope) []byte {
	return encodeFields("autodeploy:secret-value:v1", SuiteAES256GCMRandomNonceV1, AADFormatV1, scope.OwnerID, scope.ProjectID, scope.EnvironmentID, scope.VariableID, scope.SecretVersionID, scope.Purpose, scope.DeliveryScope)
}

func wrapAAD(kekID string, scope Scope) []byte {
	return encodeFields("autodeploy:secret-dek-wrap:v1", SuiteAES256GCMRandomNonceV1, AADFormatV1, kekID, scope.OwnerID, scope.ProjectID, scope.EnvironmentID, scope.VariableID, scope.SecretVersionID, scope.Purpose, scope.DeliveryScope)
}

func encodeFields(fields ...string) []byte {
	length := 0
	for _, field := range fields {
		length += 4 + len(field)
	}
	encoded := make([]byte, 0, length)
	for _, field := range fields {
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(field)))
		encoded = append(encoded, size[:]...)
		encoded = append(encoded, field...)
	}
	return encoded
}

func validScope(scope Scope) bool {
	for _, field := range []string{scope.OwnerID, scope.ProjectID, scope.EnvironmentID, scope.VariableID, scope.SecretVersionID, scope.Purpose, scope.DeliveryScope} {
		if field == "" || len(field) > 255 || !utf8.ValidString(field) || strings.ContainsAny(field, "\x00\r\n") {
			return false
		}
	}
	return (scope.Purpose == "build" || scope.Purpose == "runtime" || scope.Purpose == "provider") && (scope.DeliveryScope == "build" || scope.DeliveryScope == "runtime")
}

func validKeyID(id string) bool {
	if id == "" || len(id) > 64 || !utf8.ValidString(id) {
		return false
	}
	return strings.IndexFunc(id, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-')
	}) == -1
}

func validEnvelope(envelope Envelope) bool {
	return utf8.ValidString(envelope.Suite) && utf8.ValidString(envelope.AADVersion) && envelope.Suite == SuiteAES256GCMRandomNonceV1 && envelope.AADVersion == AADFormatV1 && validKeyID(envelope.KEKID) && len(envelope.Ciphertext) >= 28 && len(envelope.Ciphertext) <= MaxPlaintextBytes+28 && len(envelope.WrappedDEK) == 60
}

// MarshalBinary uses exact length-prefixing to make envelope parsing unambiguous.
func (envelope Envelope) MarshalBinary() ([]byte, error) {
	if !validEnvelope(envelope) {
		return nil, ErrInvalidEnvelope
	}
	return marshalFields([][]byte{[]byte(envelope.Suite), []byte(envelope.AADVersion), []byte(envelope.KEKID), envelope.Ciphertext, envelope.WrappedDEK}), nil
}

// ParseEnvelope rejects unknown, truncated, oversized, and trailing representations.
func ParseEnvelope(encoded []byte) (Envelope, error) {
	if len(encoded) > maxSerializedEnvelopeBytes {
		return Envelope{}, ErrInvalidEnvelope
	}
	fields, err := parseFields(encoded, 5)
	if err != nil {
		return Envelope{}, ErrInvalidEnvelope
	}
	envelope := Envelope{Suite: string(fields[0]), AADVersion: string(fields[1]), KEKID: string(fields[2]), Ciphertext: fields[3], WrappedDEK: fields[4]}
	if !validEnvelope(envelope) {
		return Envelope{}, ErrInvalidEnvelope
	}
	return envelope, nil
}

func marshalFields(fields [][]byte) []byte {
	length := 0
	for _, field := range fields {
		length += 4 + len(field)
	}
	encoded := make([]byte, 0, length)
	for _, field := range fields {
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(field)))
		encoded = append(encoded, size[:]...)
		encoded = append(encoded, field...)
	}
	return encoded
}
func parseFields(encoded []byte, want int) ([][]byte, error) {
	fields := make([][]byte, 0, want)
	for len(fields) < want {
		if len(encoded) < 4 {
			return nil, fmt.Errorf("truncated")
		}
		n := binary.BigEndian.Uint32(encoded[:4])
		encoded = encoded[4:]
		if uint64(n) > uint64(len(encoded)) || n > maxFieldLength(len(fields)) {
			return nil, fmt.Errorf("truncated")
		}
		fields, encoded = append(fields, append([]byte(nil), encoded[:n]...)), encoded[n:]
	}
	if len(encoded) != 0 {
		return nil, fmt.Errorf("trailing")
	}
	return fields, nil
}

const maxSerializedEnvelopeBytes = 4 + len(SuiteAES256GCMRandomNonceV1) + 4 + len(AADFormatV1) + 4 + 64 + 4 + MaxPlaintextBytes + 28 + 4 + 60

func maxFieldLength(index int) uint32 {
	switch index {
	case 0:
		return uint32(len(SuiteAES256GCMRandomNonceV1))
	case 1:
		return uint32(len(AADFormatV1))
	case 2:
		return 64
	case 3:
		return MaxPlaintextBytes + 28
	case 4:
		return 60
	}
	return 0
}
