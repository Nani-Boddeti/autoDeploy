package server

import (
	"bytes"
	"strings"
	"testing"
)

func TestCredentialTokenRoundTrip(t *testing.T) {
	token, err := NewCredentialToken(bytes.NewReader(bytes.Repeat([]byte{1}, tokenRawBytes)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token.String(), credentialTokenPrefix) {
		t.Fatalf("token=%q", token)
	}
	parsed, err := ParseCredentialToken(token.String())
	if err != nil || parsed != token {
		t.Fatalf("parsed=%q err=%v", parsed, err)
	}
	digest, err := CredentialTokenDigest(token)
	if err != nil {
		t.Fatal(err)
	}
	again, err := CredentialTokenDigest(parsed)
	if err != nil || digest != again {
		t.Fatal("digest not deterministic")
	}
}

func TestEnrollmentTokenRoundTrip(t *testing.T) {
	token, err := NewEnrollmentToken(bytes.NewReader(bytes.Repeat([]byte{2}, tokenRawBytes)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token.String(), enrollmentTokenPrefix) {
		t.Fatalf("token=%q", token)
	}
	parsed, err := ParseEnrollmentToken(token.String())
	if err != nil || parsed != token {
		t.Fatalf("parsed=%q err=%v", parsed, err)
	}
	if _, err = EnrollmentTokenDigest(token); err != nil {
		t.Fatal(err)
	}
}

// TestCrossPrefixConfusionRejected proves a credential token can never parse
// as an enrollment token and vice versa, even though both encode 32 random
// bytes at the same total length.
func TestCrossPrefixConfusionRejected(t *testing.T) {
	credential, err := NewCredentialToken(bytes.NewReader(bytes.Repeat([]byte{3}, tokenRawBytes)))
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := NewEnrollmentToken(bytes.NewReader(bytes.Repeat([]byte{4}, tokenRawBytes)))
	if err != nil {
		t.Fatal(err)
	}
	if len(credential.String()) != len(enrollment.String()) {
		t.Fatalf("prefixes are not length-comparable: %d vs %d", len(credential.String()), len(enrollment.String()))
	}
	if _, err = ParseEnrollmentToken(credential.String()); err == nil {
		t.Fatal("credential token parsed as enrollment token")
	}
	if _, err = ParseCredentialToken(enrollment.String()); err == nil {
		t.Fatal("enrollment token parsed as credential token")
	}
	if _, err = CredentialTokenDigest(CredentialToken(enrollment.String())); err == nil {
		t.Fatal("enrollment token digested as credential token")
	}
	if _, err = EnrollmentTokenDigest(EnrollmentToken(credential.String())); err == nil {
		t.Fatal("credential token digested as enrollment token")
	}
}

func TestParseTokenRejectsMalformedAndOversizedInput(t *testing.T) {
	base, err := NewCredentialToken(bytes.NewReader(bytes.Repeat([]byte{5}, tokenRawBytes)))
	if err != nil {
		t.Fatal(err)
	}
	withLastCharReplaced := base.String()[:len(base.String())-1] + "!"
	cases := []string{
		"",
		"srv.v1.",
		withLastCharReplaced,                 // invalid base64 character
		base.String() + "A",                  // oversized by one char
		base.String()[:len(base.String())-1], // undersized by one char
		"srv.v2." + base.String()[len(credentialTokenPrefix):],
		strings.Repeat("A", 10000),                             // grossly oversized
		"SRV.V1." + base.String()[len(credentialTokenPrefix):], // wrong case prefix
	}
	for _, c := range cases {
		if _, err := ParseCredentialToken(c); err == nil {
			t.Fatalf("accepted malformed token %q", c)
		}
	}
}

func TestNewTokenFailsClosedOnShortOrFailingRandomness(t *testing.T) {
	if _, err := NewCredentialToken(bytes.NewReader(nil)); err == nil {
		t.Fatal("empty randomness accepted")
	}
	if _, err := NewCredentialToken(bytes.NewReader(make([]byte, tokenRawBytes-1))); err == nil {
		t.Fatal("short randomness accepted")
	}
	if _, err := NewEnrollmentToken(errReader{}); err == nil {
		t.Fatal("failing randomness accepted")
	}
}

func TestConstantTimeEqualDigest(t *testing.T) {
	a := [32]byte{1, 2, 3}
	b := [32]byte{1, 2, 3}
	c := [32]byte{1, 2, 4}
	if !ConstantTimeEqualDigest(a, b) {
		t.Fatal("equal digests reported unequal")
	}
	if ConstantTimeEqualDigest(a, c) {
		t.Fatal("unequal digests reported equal")
	}
}

// TestNonCanonicalBase64PaddingBitsRejected proves a last-character mutation
// that flips only the unused low padding bits of a 32-byte RawURLEncoding
// (so it still decodes to the same 32 bytes) is rejected because it does not
// round-trip back to the identical encoded string. For 32 bytes the last
// char's 6-bit value must have its low 2 bits zero to be canonical.
func TestNonCanonicalBase64PaddingBitsRejected(t *testing.T) {
	token, err := NewCredentialToken(bytes.NewReader(bytes.Repeat([]byte{6}, tokenRawBytes)))
	if err != nil {
		t.Fatal(err)
	}
	encoded := token.String()[len(credentialTokenPrefix):]
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	lastIndex := strings.IndexByte(alphabet, encoded[len(encoded)-1])
	if lastIndex < 0 {
		t.Fatalf("unexpected last character %q", encoded[len(encoded)-1])
	}
	if lastIndex%4 != 0 {
		t.Fatalf("token's own last character is already non-canonical: index=%d", lastIndex)
	}
	nonCanonical := alphabet[lastIndex+1] // sets a nonzero low padding bit
	mutated := credentialTokenPrefix + encoded[:len(encoded)-1] + string(nonCanonical)
	if _, err := ParseCredentialToken(mutated); err == nil {
		t.Fatal("non-canonical padding-bit token accepted")
	}
}
