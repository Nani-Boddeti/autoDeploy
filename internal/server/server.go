// Package server contains non-transport domain primitives for durable
// deployment-agent identity: the Server value type, its opaque ID, and
// bearer credential/enrollment tokens. It mirrors internal/auth's token
// shape (prefix + random bytes + domain-separated digest) but keeps agent
// bearer material in a distinct namespace so administrator and agent
// credentials can never be confused with each other.
package server

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"autodeploy/internal/github"
)

var ErrInvalidServer = errors.New("invalid server")

// Status is the durable lifecycle of a registered deployment-agent server.
// Only active->revoked is a legal transition; there is no delete.
type Status string

const (
	StatusActive  Status = "active"
	StatusRevoked Status = "revoked"
)

// ProtocolVersion is the only agent protocol version this slice accepts.
const ProtocolVersion = 1

// Server is an immutable-identity, mutable-status deployment-agent record.
// OwnerID, ID, Name, and ProtocolVersion never change after creation; only
// Status (active->revoked once) and LastHeartbeatAt (monotonic) do.
type Server struct {
	ID              string
	OwnerID         string
	Name            string
	ProtocolVersion int
	Status          Status
	CreatedAt       time.Time
	RevokedAt       time.Time // zero when not revoked
	LastHeartbeatAt time.Time // zero when no heartbeat has been recorded
}

// Validate checks owner scope, bounded name, protocol version, status, and
// the timestamp invariants the servers table also enforces.
func (s Server) Validate() error {
	if !github.ValidOpaqueID(s.ID) || !github.ValidOpaqueID(s.OwnerID) || !ValidName(s.Name) || s.ProtocolVersion != ProtocolVersion {
		return ErrInvalidServer
	}
	if s.Status != StatusActive && s.Status != StatusRevoked {
		return ErrInvalidServer
	}
	if s.CreatedAt.IsZero() {
		return ErrInvalidServer
	}
	if (s.Status == StatusRevoked) != !s.RevokedAt.IsZero() {
		return ErrInvalidServer
	}
	if !s.RevokedAt.IsZero() && s.RevokedAt.Before(s.CreatedAt) {
		return ErrInvalidServer
	}
	if !s.LastHeartbeatAt.IsZero() && s.LastHeartbeatAt.Before(s.CreatedAt) {
		return ErrInvalidServer
	}
	return nil
}

const maxNameBytes = 128

// ValidName bounds a display name: 1-128 UTF-8 bytes, no control characters,
// and no leading/trailing whitespace (a name equal to its own trim).
func ValidName(name string) bool {
	if len(name) == 0 || len(name) > maxNameBytes || !utf8.ValidString(name) {
		return false
	}
	if strings.TrimSpace(name) != name {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

const idPrefix = "server_"
const idRawBytes = 16

// NewID generates a system-generated opaque server ID. It never derives an
// ID from caller-controlled content: registration always mints a fresh
// random identity.
func NewID(randomness io.Reader) (string, error) {
	if randomness == nil {
		randomness = rand.Reader
	}
	raw := make([]byte, idRawBytes)
	if _, err := io.ReadFull(randomness, raw); err != nil {
		return "", fmt.Errorf("generate server id: %w", err)
	}
	return idPrefix + hex.EncodeToString(raw), nil
}
