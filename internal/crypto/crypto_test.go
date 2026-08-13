package crypto

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func testScope() Scope {
	return Scope{OwnerID: "owner", ProjectID: "project", EnvironmentID: "environment", VariableID: "variable", SecretVersionID: "version", Purpose: "runtime", DeliveryScope: "runtime"}
}
func testRing() KeyRing {
	ring, err := NewKeyRing("new", map[string][]byte{"new": bytes.Repeat([]byte{1}, 32), "old": bytes.Repeat([]byte{2}, 32)})
	if err != nil {
		panic(err)
	}
	return ring
}

type testReservation struct {
	ids []string
	err error
}

func (r *testReservation) ReserveWrapInvocation(id string) error {
	r.ids = append(r.ids, id)
	return r.err
}

func TestEnvelopeRoundTripAndSerialization(t *testing.T) {
	plain := []byte("correct horse battery staple")
	envelope, err := Encrypt(testRing(), &testReservation{}, testScope(), plain)
	if err != nil {
		t.Fatal(err)
	}
	if envelope.Suite != SuiteAES256GCMRandomNonceV1 || bytes.Contains(envelope.Ciphertext, plain) {
		t.Fatal("invalid envelope")
	}
	encoded, err := envelope.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseEnvelope(encoded)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decrypt(testRing(), testScope(), parsed)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("got %q, err %v", got, err)
	}
	second, err := Encrypt(testRing(), &testReservation{}, testScope(), plain)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(envelope.Ciphertext, second.Ciphertext) || bytes.Equal(envelope.WrappedDEK, second.WrappedDEK) {
		t.Fatal("fresh encryption reused output")
	}
}

func TestEnvelopeRejectsAADSubstitutionAndMalformedData(t *testing.T) {
	envelope, err := Encrypt(testRing(), &testReservation{}, testScope(), []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	mutations := []func(*Scope){
		func(s *Scope) { s.OwnerID = "other" }, func(s *Scope) { s.ProjectID = "other" }, func(s *Scope) { s.EnvironmentID = "other" },
		func(s *Scope) { s.VariableID = "other" }, func(s *Scope) { s.SecretVersionID = "other" }, func(s *Scope) { s.Purpose = "build" }, func(s *Scope) { s.DeliveryScope = "build" },
	}
	for _, mutate := range mutations {
		scope := testScope()
		mutate(&scope)
		if _, err := Decrypt(testRing(), scope, envelope); !errors.Is(err, ErrCryptographicFail) {
			t.Fatalf("AAD error %v", err)
		}
	}
	wrong, _ := NewKeyRing("new", map[string][]byte{"new": bytes.Repeat([]byte{9}, 32), "old": bytes.Repeat([]byte{2}, 32)})
	if _, err := Decrypt(wrong, testScope(), envelope); !errors.Is(err, ErrCryptographicFail) {
		t.Fatalf("wrong key %v", err)
	}
	unknown, _ := NewKeyRing("old", map[string][]byte{"old": bytes.Repeat([]byte{2}, 32)})
	if _, err := Decrypt(unknown, testScope(), envelope); !errors.Is(err, ErrCryptographicFail) {
		t.Fatalf("unknown key %v", err)
	}
	for _, data := range [][]byte{nil, []byte{0, 0, 0, 1}, append(mustMarshal(t, envelope), 0)} {
		if _, err := ParseEnvelope(data); !errors.Is(err, ErrInvalidEnvelope) {
			t.Fatalf("malformed %v", err)
		}
	}
	bad := envelope
	bad.Suite = "unknown"
	if _, err := Decrypt(testRing(), testScope(), bad); !errors.Is(err, ErrInvalidEnvelope) {
		t.Fatalf("suite %v", err)
	}
}

func TestCryptoErrorsDoNotExposePlaintext(t *testing.T) {
	secret := "never-expose-this-value"
	_, err := Encrypt(nil, &testReservation{}, testScope(), []byte(secret))
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("unsafe error %v", err)
	}
	if _, err := Decrypt(testRing(), testScope(), Envelope{}); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("unsafe decrypt error %v", err)
	}
}

func TestKeyRingCopiesCallerKeys(t *testing.T) {
	keys := map[string][]byte{"active": bytes.Repeat([]byte{7}, 32)}
	ring, err := NewKeyRing("active", keys)
	if err != nil {
		t.Fatal(err)
	}
	if got := keys["active"][0]; got != 7 {
		t.Fatalf("caller key was cleared: %d", got)
	}
	keys["active"][0] = 9
	got, err := ring.Resolve("active")
	if err != nil || got[0] != 7 {
		t.Fatalf("ring did not retain an independent copy: %v, %v", got, err)
	}
}

type recordingProvider struct {
	events []string
	key    []byte
}

func (provider *recordingProvider) Active() (string, []byte, error) {
	provider.events = append(provider.events, "active")
	return "selected", append([]byte(nil), provider.key...), nil
}

func (provider *recordingProvider) Resolve(string) ([]byte, error) {
	provider.events = append(provider.events, "resolve")
	return nil, ErrUnknownKey
}

type recordingReserver struct {
	events *[]string
	id     string
	err    error
}

func (reserver *recordingReserver) ReserveWrapInvocation(id string) error {
	*reserver.events = append(*reserver.events, "reserve")
	reserver.id = id
	return reserver.err
}

func TestEncryptReservesSelectedKEKBeforeWrapping(t *testing.T) {
	provider := &recordingProvider{key: bytes.Repeat([]byte{5}, 32)}
	reserver := &recordingReserver{events: &provider.events}
	if _, err := Encrypt(provider, reserver, testScope(), []byte("plaintext")); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(provider.events, ","), "active,reserve"; got != want {
		t.Fatalf("operation order = %q, want %q", got, want)
	}
	if reserver.id != "selected" {
		t.Fatalf("reserved KEK = %q", reserver.id)
	}

	provider = &recordingProvider{key: bytes.Repeat([]byte{5}, 32)}
	reserver = &recordingReserver{events: &provider.events, err: errors.New("refused")}
	envelope, err := Encrypt(provider, reserver, testScope(), []byte("plaintext"))
	if !errors.Is(err, ErrCryptographicFail) || envelope.Suite != "" || envelope.AADVersion != "" || envelope.KEKID != "" || envelope.Ciphertext != nil || envelope.WrappedDEK != nil {
		t.Fatalf("refusal = %#v, %v", envelope, err)
	}
	if got, want := strings.Join(provider.events, ","), "active,reserve"; got != want {
		t.Fatalf("refusal operation order = %q, want no further activity after %q", got, want)
	}
}
func mustMarshal(t *testing.T, envelope Envelope) []byte {
	t.Helper()
	encoded, err := envelope.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
