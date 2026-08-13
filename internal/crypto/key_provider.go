package crypto

import (
	"crypto/subtle"
	"errors"
)

// KeyRing is an immutable in-memory snapshot loaded from protected key files.
type KeyRing struct {
	activeID string
	keys     map[string][]byte
}

// NewKeyRing deep-copies an already verified key snapshot. The caller retains
// ownership of keys and must clear its originals after this call. KEK entropy is
// an operator generation responsibility; V1 rejects only visibly degenerate
// all-zero material.
func NewKeyRing(activeID string, keys map[string][]byte) (KeyRing, error) {
	ring := KeyRing{activeID: activeID, keys: make(map[string][]byte, len(keys))}
	for id, key := range keys {
		ring.keys[id] = append([]byte(nil), key...)
	}
	if err := ring.Validate(); err != nil {
		clearKeyMap(ring.keys)
		return KeyRing{}, err
	}
	return ring, nil
}

func clearKeyMap(keys map[string][]byte) {
	for id, key := range keys {
		clear(key)
		keys[id] = nil
	}
}

func (ring KeyRing) Active() (string, []byte, error) {
	key, err := ring.Resolve(ring.activeID)
	if err != nil {
		return "", nil, err
	}
	return ring.activeID, key, nil
}
func (ring KeyRing) Resolve(id string) ([]byte, error) {
	key, ok := ring.keys[id]
	if !ok || len(key) != 32 {
		return nil, ErrUnknownKey
	}
	return append([]byte(nil), key...), nil
}
func (ring KeyRing) Validate() error {
	if !validKeyID(ring.activeID) || len(ring.keys) == 0 || len(ring.keys) > 64 {
		return ErrInvalidInput
	}
	for id, key := range ring.keys {
		if !validKeyID(id) || len(key) != 32 || subtle.ConstantTimeCompare(key, make([]byte, 32)) == 1 {
			return ErrInvalidInput
		}
	}
	if _, ok := ring.keys[ring.activeID]; !ok {
		return errors.New("active key missing")
	}
	return nil
}
