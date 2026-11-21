package auth

import (
	"bytes"
	"crypto/sha256"
	"strings"
	"testing"
)

func TestSetupTokenRoundTrip(t *testing.T) {
	token, err := NewSetupToken(bytes.NewReader(bytes.Repeat([]byte{7}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token.String(), "setup.v1.") || len(token.String()) != len("setup.v1.")+43 {
		t.Fatalf("token shape = %q", token)
	}
	parsed, err := ParseSetupToken(token.String())
	if err != nil || parsed != token {
		t.Fatalf("parsed=%q err=%v", parsed, err)
	}
	first, err := SetupTokenDigest(token)
	if err != nil {
		t.Fatal(err)
	}
	second, err := SetupTokenDigest(parsed)
	if err != nil || first != second {
		t.Fatal("digest not deterministic")
	}
	other, err := NewSetupToken(bytes.NewReader(bytes.Repeat([]byte{8}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	otherDigest, err := SetupTokenDigest(other)
	if err != nil || otherDigest == first {
		t.Fatal("distinct tokens share a digest")
	}
}

func TestSetupTokenDigestIsDomainSeparated(t *testing.T) {
	raw := bytes.Repeat([]byte{9}, 32)
	token, err := NewSetupToken(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	digest, err := SetupTokenDigest(token)
	if err != nil {
		t.Fatal(err)
	}
	// The digest must not equal a plain or enrollment-domain digest of the same bytes.
	for _, domain := range []string{"", "autodeploy/server-enrollment-token/v1", "autodeploy/server-credential-token/v1"} {
		if digest == testDomainDigest(domain, raw) {
			t.Fatalf("digest collides with domain %q", domain)
		}
	}
}

func TestParseSetupTokenRejectsOtherShapes(t *testing.T) {
	good, err := NewSetupToken(bytes.NewReader(bytes.Repeat([]byte{1}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	body := strings.TrimPrefix(good.String(), "setup.v1.")
	session, err := NewSessionToken(bytes.NewReader(bytes.Repeat([]byte{1}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"empty":                "",
		"enrollment prefix":    "enr.v1." + body,
		"credential prefix":    "srv.v1." + body,
		"session token":        session.String(),
		"bare body":            body,
		"wrong version":        "setup.v2." + body,
		"uppercase prefix":     "SETUP.v1." + body,
		"short":                "setup.v1." + body[:42],
		"long":                 "setup.v1." + body + "A",
		"padded":               "setup.v1." + body + "=",
		"non-canonical base64": "setup.v1." + body[:42] + nonCanonicalTail(body[42]),
		"whitespace":           " " + good.String(),
		"trailing newline":     good.String() + "\n",
		"standard alphabet":    "setup.v1." + strings.Repeat("+", 43),
	}
	for name, value := range cases {
		if _, err := ParseSetupToken(value); err == nil {
			t.Errorf("%s: parse accepted %q", name, value)
		}
	}
	// Conversely a setup token is never a session token.
	if _, err := ParseSessionToken(good.String()); err == nil {
		t.Error("setup token parsed as session token")
	}
}

// nonCanonicalTail returns a last character that decodes to the same 32 bytes
// but is not the canonical encoding (the final base64 char carries 4 spare bits).
func nonCanonicalTail(last byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	index := strings.IndexByte(alphabet, last)
	return string(alphabet[(index+1)%64])
}

func TestNewSetupTokenFailsOnShortRandomness(t *testing.T) {
	if _, err := NewSetupToken(bytes.NewReader([]byte{1, 2, 3})); err == nil {
		t.Fatal("short randomness accepted")
	}
}

func testDomainDigest(domain string, raw []byte) [32]byte {
	if domain == "" {
		return sha256Sum(raw)
	}
	return sha256Sum(append([]byte(domain+"\x00"), raw...))
}

func sha256Sum(b []byte) [32]byte { return sha256.Sum256(b) }
