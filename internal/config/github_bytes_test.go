package config_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"testing"

	"autodeploy/internal/config"
)

func rsaPEM(t *testing.T, bits int, pkcs8 bool) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatal(err)
	}
	if pkcs8 {
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

func TestGitHubAppSignerFromPEM(t *testing.T) {
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecDER, err := x509.MarshalPKCS8PrivateKey(ecKey)
	if err != nil {
		t.Fatal(err)
	}
	valid := rsaPEM(t, 2048, false)
	cases := []struct {
		name     string
		pem      []byte
		clientID string
		ok       bool
	}{
		{"pkcs1 accepted", valid, "Iv1.client", true},
		{"pkcs8 accepted", rsaPEM(t, 2048, true), "Iv1.client", true},
		{"small key rejected", rsaPEM(t, 1024, false), "Iv1.client", false},
		{"non-rsa rejected", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecDER}), "Iv1.client", false},
		{"empty rejected", nil, "Iv1.client", false},
		{"garbage rejected", []byte("not pem"), "Iv1.client", false},
		{"trailing data rejected", append(append([]byte(nil), valid...), []byte("junk")...), "Iv1.client", false},
		{"two blocks rejected", append(append([]byte(nil), valid...), valid...), "Iv1.client", false},
		{"encrypted headers rejected", pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Headers: map[string]string{"Proc-Type": "4,ENCRYPTED", "DEK-Info": "AES-128-CBC,00"}, Bytes: []byte{1}}), "Iv1.client", false},
		{"unknown block type rejected", pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(mustKey(t))}), "Iv1.client", false},
		{"empty client id rejected", valid, "", false},
		{"oversize rejected", bytes.Repeat([]byte("A"), 16<<10+1), "Iv1.client", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := append([]byte(nil), tc.pem...)
			signer, err := config.GitHubAppSignerFromPEM(tc.clientID, tc.pem)
			if (err == nil) != tc.ok || (err == nil) != (signer != nil) {
				t.Fatalf("err=%v signer=%v, want ok=%v", err, signer != nil, tc.ok)
			}
			if !bytes.Equal(before, tc.pem) {
				t.Fatal("input mutated")
			}
			if err != nil && err != config.ErrInvalidEnvironment {
				t.Fatalf("error %v not the sanitized sentinel", err)
			}
		})
	}
}

func mustKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestGitHubWebhookSecretRingFromBytes(t *testing.T) {
	secret := bytes.Repeat([]byte{7}, 32)
	ring, err := config.GitHubWebhookSecretRingFromBytes(secret)
	if err != nil || ring.ActiveVersion != "1" || !bytes.Equal(ring.Active, secret) || len(ring.Retained) != 0 {
		t.Fatalf("ring=%+v err=%v", ring, err)
	}
	if _, err := config.GitHubWebhookSecretRingFromBytes(bytes.Repeat([]byte{1}, 1024)); err != nil {
		t.Fatalf("1 KiB must be accepted: %v", err)
	}
	clear(secret)
	if ring.Active[0] != 7 {
		t.Fatal("ring aliases the caller buffer")
	}
	for _, bad := range [][]byte{nil, {}, bytes.Repeat([]byte{1}, 31), bytes.Repeat([]byte{1}, 1025)} {
		if _, err := config.GitHubWebhookSecretRingFromBytes(bad); err != config.ErrInvalidEnvironment {
			t.Fatalf("len %d: err=%v", len(bad), err)
		}
	}
}
