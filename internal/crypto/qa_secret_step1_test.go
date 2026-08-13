package crypto

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestQAPlaintextBoundsAndEmptyRoundTrips(t *testing.T) {
	for _, plaintext := range [][]byte{nil, bytes.Repeat([]byte("x"), MaxPlaintextBytes)} {
		envelope, err := Encrypt(testRing(), &testReservation{}, testScope(), plaintext)
		if err != nil {
			t.Fatalf("encrypt length %d: %v", len(plaintext), err)
		}
		got, err := Decrypt(testRing(), testScope(), envelope)
		if err != nil || !bytes.Equal(got, plaintext) {
			t.Fatalf("round trip length %d: got length %d, err %v", len(plaintext), len(got), err)
		}
	}
	if _, err := Encrypt(testRing(), &testReservation{}, testScope(), bytes.Repeat([]byte("x"), MaxPlaintextBytes+1)); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("oversized plaintext error = %v", err)
	}
}

func TestQARedactorOverlapsAndAdjacentMatches(t *testing.T) {
	var output bytes.Buffer
	writer, err := NewRedactingWriter(&output, [][]byte{[]byte("abcd"), []byte("bc"), []byte("secret")})
	if err != nil {
		t.Fatal(err)
	}
	for _, chunk := range [][]byte{[]byte("xab"), []byte("cdsecret"), []byte("se"), []byte("cretz")} {
		if _, err := writer.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if got, want := output.String(), "x[REDACTED][REDACTED][REDACTED]z"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestQARedactorRejectsShortDownstreamWrite(t *testing.T) {
	writer, err := NewRedactingWriter(shortWriter{}, [][]byte{[]byte("secret")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("public output long enough to flush")); !errors.Is(err, ErrRedaction) {
		t.Fatalf("short downstream write must fail closed; got %v", err)
	}
}

func TestQARedactorFailsClosedWhenReplacementContainsSecret(t *testing.T) {
	secret := []byte("RED")
	var output bytes.Buffer
	writer, err := NewRedactingWriter(&output, [][]byte{secret})
	if err != nil {
		return
	}
	if _, err := writer.Write(secret); err != nil {
		return
	}
	if err := writer.Close(); err != nil {
		return
	}
	if bytes.Contains(output.Bytes(), secret) {
		t.Fatalf("successful redaction output still contains the raw secret: %q", output.Bytes())
	}
}

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return len(p) - 1, nil
}

func TestQAScopeRejectsNonUTF8AADFields(t *testing.T) {
	scope := testScope()
	scope.OwnerID = string([]byte{0xff})
	if utf8.ValidString(scope.OwnerID) {
		t.Fatal("test setup unexpectedly valid UTF-8")
	}
	if _, err := Encrypt(testRing(), &testReservation{}, scope, []byte("secret")); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("non-UTF-8 AAD field accepted: %v", err)
	}
}

func TestQAValueAndWrapOperationDomainsAreNotInterchangeable(t *testing.T) {
	scope := testScope()
	key := bytes.Repeat([]byte{1}, 32)
	ciphertext, err := seal(key, []byte("domain test"), valueAAD(scope))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := open(key, ciphertext, wrapAAD("new", scope)); err == nil {
		t.Fatal("value ciphertext authenticated in wrapping domain")
	}
}

func TestQAKnownAnswerEnvelopeDecryptsWithExactAADAndNoncePrefix(t *testing.T) {
	scope := testScope()
	kek := bytes.Repeat([]byte{0x11}, 32)
	valueNonce := bytes.Repeat([]byte{0x33}, 12)
	wrapNonce := bytes.Repeat([]byte{0x44}, 12)
	ciphertext, err := hex.DecodeString("3333333333333333333333331dada99d1b0c06046984f50470a979f3efb23e1f04037cb757397e8b5d186f")
	if err != nil {
		t.Fatal(err)
	}
	wrappedDEK, err := hex.DecodeString("444444444444444444444444ee676a4c681a0615cbb2bdbf0ac855cebae8ee199a2f1df5f571c3411ee908fe106a7c48210202f2e7028762bfd7d10d")
	if err != nil {
		t.Fatal(err)
	}
	envelope := Envelope{
		Suite:      SuiteAES256GCMRandomNonceV1,
		AADVersion: AADFormatV1,
		KEKID:      "known",
		Ciphertext: ciphertext,
		WrappedDEK: wrappedDEK,
	}
	provider, err := NewKeyRing("known", map[string][]byte{"known": kek})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decrypt(provider, scope, envelope)
	if err != nil || string(got) != "known plaintext" {
		t.Fatalf("known-answer decrypt = %q, %v", got, err)
	}
	if !bytes.Equal(envelope.Ciphertext[:12], valueNonce) || !bytes.Equal(envelope.WrappedDEK[:12], wrapNonce) {
		t.Fatal("nonce prefix serialization mismatch")
	}
}

func TestQAErrorsContainNoInputs(t *testing.T) {
	secret := "qa-secret-marker"
	scope := testScope()
	scope.OwnerID = secret + "\n"
	_, err := Encrypt(testRing(), &testReservation{}, scope, []byte(secret))
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("unsafe encryption error: %v", err)
	}
	if _, err := ParseEnvelope([]byte(secret)); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("unsafe parse error: %v", err)
	}
}

var _ io.Writer = shortWriter{}
