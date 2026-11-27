package credential

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
)

// ParseRSAPrivateKeyPEM parses exactly one unencrypted RSA PKCS#1 or PKCS#8
// PEM block of at least 2048 bits. It does not retain or clear contents;
// the caller owns that buffer. Every rejection is ErrInvalidCredentialFile.
func ParseRSAPrivateKeyPEM(contents []byte) (*rsa.PrivateKey, error) {
	if len(contents) == 0 || len(contents) > MaxRSAPrivateKeyPEMBytes {
		return nil, ErrInvalidCredentialFile
	}
	block, rest := pem.Decode(contents)
	if block == nil || len(rest) != 0 || block.Headers["Proc-Type"] != "" || block.Headers["DEK-Info"] != "" {
		return nil, ErrInvalidCredentialFile
	}
	var key *rsa.PrivateKey
	var err error
	switch block.Type {
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		var parsed any
		parsed, err = x509.ParsePKCS8PrivateKey(block.Bytes)
		if rsaKey, ok := parsed.(*rsa.PrivateKey); ok {
			key = rsaKey
		} else if err == nil {
			err = errors.New("non-RSA private key")
		}
	default:
		err = errors.New("unsupported private key encoding")
	}
	if err != nil || key == nil || key.N.BitLen() < 2048 || key.Validate() != nil {
		return nil, ErrInvalidCredentialFile
	}
	return key, nil
}
