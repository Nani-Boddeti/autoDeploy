package credential

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"testing"
)

func TestParseRSAPrivateKeyPEM(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	good := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if parsed, err := ParseRSAPrivateKeyPEM(good); err != nil || parsed == nil {
		t.Fatalf("good: %v", err)
	}
	for name, in := range map[string][]byte{
		"empty":    nil,
		"trailing": append(append([]byte(nil), good...), 'x'),
		"garbage":  []byte("nope"),
		"oversize": make([]byte, MaxRSAPrivateKeyPEMBytes+1),
	} {
		if _, err := ParseRSAPrivateKeyPEM(in); err != ErrInvalidCredentialFile {
			t.Fatalf("%s: err=%v", name, err)
		}
	}
}
