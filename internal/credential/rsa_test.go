//go:build darwin || linux

package credential_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"autodeploy/internal/credential"
)

func TestLoadRSAPrivateKeyAcceptsOnlySecureUnencryptedRSA(t *testing.T) {
	root, outside := secureTempDir(t), secureTempDir(t)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(outside, "github.pem")
	writeRSAPEM(t, path, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(key))
	got, err := credential.LoadRSAPrivateKey(path, root, os.Geteuid())
	if err != nil || got.N.Cmp(key.N) != 0 {
		t.Fatalf("err=%v", err)
	}
	for _, value := range [][]byte{pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte{1}}), append(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), []byte("trailing")...), pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Headers: map[string]string{"Proc-Type": "4,ENCRYPTED"}, Bytes: x509.MarshalPKCS1PrivateKey(key)})} {
		if err := os.WriteFile(path, value, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := credential.LoadRSAPrivateKey(path, root, os.Geteuid()); !errors.Is(err, credential.ErrInvalidCredentialFile) {
			t.Fatalf("err=%v", err)
		}
	}
}
func writeRSAPEM(t *testing.T, path, kind string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
}
