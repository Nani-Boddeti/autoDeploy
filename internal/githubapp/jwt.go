package githubapp

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"time"
)

type Signer struct {
	key      *rsa.PrivateKey
	clientID string
	now      func() time.Time
	random   io.Reader
}

// NewSigner deep-copies key into signer-owned storage. Callers may clear or
// mutate their input key after this function returns.
func NewSigner(key *rsa.PrivateKey, clientID string, now func() time.Time, random io.Reader) (*Signer, error) {
	if key == nil || !validText(clientID, 255) {
		return nil, ErrInvalidIdentity
	}
	owned, err := cloneRSAKey(key)
	if err != nil || owned.N.BitLen() < 2048 || owned.Validate() != nil {
		return nil, ErrInvalidIdentity
	}
	owned.Precompute()
	if now == nil {
		now = time.Now
	}
	if random == nil {
		random = rand.Reader
	}
	return &Signer{key: owned, clientID: clientID, now: now, random: random}, nil
}

func cloneRSAKey(source *rsa.PrivateKey) (*rsa.PrivateKey, error) {
	if source.N == nil || source.D == nil || len(source.Primes) < 2 {
		return nil, errors.New("invalid RSA key")
	}
	key := &rsa.PrivateKey{PublicKey: rsa.PublicKey{N: new(big.Int).Set(source.N), E: source.E}, D: new(big.Int).Set(source.D), Primes: make([]*big.Int, len(source.Primes))}
	for index, prime := range source.Primes {
		if prime == nil {
			return nil, errors.New("invalid RSA prime")
		}
		key.Primes[index] = new(big.Int).Set(prime)
	}
	return key, nil
}

func (s *Signer) Token() (string, error) {
	now := s.now().UTC()
	header, _ := json.Marshal(struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}{"RS256", "JWT"})
	claims, err := json.Marshal(struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}{s.clientID, now.Add(-time.Minute).Unix(), now.Add(9 * time.Minute).Unix()})
	if err != nil {
		return "", ErrInvalidIdentity
	}
	encoded := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := crypto.SHA256.New()
	_, _ = digest.Write([]byte(encoded))
	signature, err := rsa.SignPKCS1v15(s.random, s.key, crypto.SHA256, digest.Sum(nil))
	if err != nil {
		return "", errors.New("GitHub App JWT signing failed")
	}
	return encoded + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}
