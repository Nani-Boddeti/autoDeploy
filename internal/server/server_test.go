package server

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

func validServer() Server {
	return Server{ID: "server_1", OwnerID: "owner-1", Name: "hetzner-1", ProtocolVersion: ProtocolVersion, Status: StatusActive, CreatedAt: time.Unix(1, 0)}
}

func TestServerValidateAccepts(t *testing.T) {
	if err := validServer().Validate(); err != nil {
		t.Fatal(err)
	}
	revoked := validServer()
	revoked.Status = StatusRevoked
	revoked.RevokedAt = time.Unix(2, 0)
	if err := revoked.Validate(); err != nil {
		t.Fatal(err)
	}
	heartbeat := validServer()
	heartbeat.LastHeartbeatAt = time.Unix(2, 0)
	if err := heartbeat.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestServerValidateRejects(t *testing.T) {
	cases := map[string]Server{
		"empty id":               func() Server { s := validServer(); s.ID = ""; return s }(),
		"oversized id":           func() Server { s := validServer(); s.ID = strings.Repeat("a", 129); return s }(),
		"nul in id":              func() Server { s := validServer(); s.ID = "a\x00b"; return s }(),
		"empty owner":            func() Server { s := validServer(); s.OwnerID = ""; return s }(),
		"empty name":             func() Server { s := validServer(); s.Name = ""; return s }(),
		"oversized name":         func() Server { s := validServer(); s.Name = strings.Repeat("a", 129); return s }(),
		"control char in name":   func() Server { s := validServer(); s.Name = "a\nb"; return s }(),
		"leading space name":     func() Server { s := validServer(); s.Name = " a"; return s }(),
		"trailing space name":    func() Server { s := validServer(); s.Name = "a "; return s }(),
		"wrong protocol version": func() Server { s := validServer(); s.ProtocolVersion = 2; return s }(),
		"unknown status":         func() Server { s := validServer(); s.Status = "pending"; return s }(),
		"zero created_at":        func() Server { s := validServer(); s.CreatedAt = time.Time{}; return s }(),
		"active with revoked_at": func() Server { s := validServer(); s.RevokedAt = time.Unix(2, 0); return s }(),
		"revoked without revoked_at": func() Server {
			s := validServer()
			s.Status = StatusRevoked
			return s
		}(),
		"revoked_at before created_at": func() Server {
			s := validServer()
			s.Status = StatusRevoked
			s.RevokedAt = time.Unix(0, 0)
			return s
		}(),
		"heartbeat before created_at": func() Server {
			s := validServer()
			s.LastHeartbeatAt = time.Unix(0, 0)
			return s
		}(),
	}
	for name, s := range cases {
		if err := s.Validate(); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

func TestValidName(t *testing.T) {
	if !ValidName("hetzner-1") {
		t.Fatal("plain name rejected")
	}
	if ValidName("") || ValidName(strings.Repeat("a", 129)) || ValidName(" a") || ValidName("a ") || ValidName("a\tb") || ValidName("a\x7fb") {
		t.Fatal("invalid name accepted")
	}
}

func TestNewIDIsOpaqueAndBoundedAndFailsClosedOnShortRandomness(t *testing.T) {
	id, err := NewID(bytes.NewReader(bytes.Repeat([]byte{9}, idRawBytes)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(id, idPrefix) || len(id) > 128 {
		t.Fatalf("id=%q", id)
	}
	other, err := NewID(bytes.NewReader(bytes.Repeat([]byte{10}, idRawBytes)))
	if err != nil {
		t.Fatal(err)
	}
	if id == other {
		t.Fatal("distinct randomness produced identical ids")
	}
	if _, err = NewID(bytes.NewReader(nil)); err == nil {
		t.Fatal("empty randomness accepted")
	}
	if _, err = NewID(errReader{}); err == nil {
		t.Fatal("failing randomness accepted")
	}
}

// errReader always fails, proving ID generation fails closed rather than
// falling back to zero/weak bytes.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrClosedPipe }
