// Package githubapp provides the narrow GitHub App authentication boundary.
package githubapp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"time"
)

const (
	APIVersion = "2026-03-10"
	APIOrigin  = "https://api.github.com"
)

var (
	ErrInvalidIdentity    = errors.New("invalid GitHub App identity")
	ErrInvalidResponse    = errors.New("invalid GitHub provider response")
	ErrProvider           = errors.New("GitHub provider request failed")
	ErrRateLimited        = errors.New("GitHub provider rate limited")
	ErrRevocationRequired = errors.New("GitHub discovery token revocation required")
)

type AppIdentity struct {
	AppID    int64
	ClientID string
	Slug     string
}

func (v AppIdentity) Valid() bool {
	return v.AppID > 0 && validText(v.ClientID, 255) && validText(v.Slug, 100)
}

type Owner struct {
	AccountID int64
	Type      string
}

func (v Owner) Valid() bool { return v.AccountID > 0 && (v.Type == "User" || v.Type == "Organization") }

type App struct {
	ID          int64
	ClientID    string
	Slug        string
	Owner       Owner
	Permissions map[string]string
	Events      []string
}

type Installation struct {
	ID                  int64
	Account             Owner
	Suspended           bool
	TargetType          string
	RepositorySelection string
}

type Repository struct {
	ID            int64  `json:"id"`
	NodeID        string `json:"node_id"`
	OwnerID       int64  `json:"-"`
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
}

// SetupDigest is a domain-separated, comparison-only capability digest.
func SetupDigest(value string) ([32]byte, error) { return capabilityDigest("setup-state/v1", value) }
func HandoffDigest(value string) ([32]byte, error) {
	return capabilityDigest("setup-handoff/v1", value)
}
func NewCapability(random io.Reader) (string, error) {
	if random == nil {
		return "", ErrInvalidIdentity
	}
	b := make([]byte, 32)
	if _, err := io.ReadFull(random, b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func capabilityDigest(domain, value string) ([32]byte, error) {
	if len(value) != 43 {
		return [32]byte{}, ErrInvalidIdentity
	}
	b, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(b) != 32 {
		return [32]byte{}, ErrInvalidIdentity
	}
	return sha256.Sum256(append(append([]byte(domain+":"), b...), byte(0))), nil
}

type Token struct {
	Value         string
	ExpiresAt     time.Time
	RepositoryIDs []int64
	Contents      string
	Permissions   map[string]string
}

// ProviderError deliberately retains only safe classification and request metadata.
type ProviderError struct {
	StatusCode  int
	RequestID   string
	RetryAfter  time.Duration
	RateLimited bool
	Temporary   bool
}

func (e *ProviderError) Error() string { return "GitHub provider request failed" }
func (e *ProviderError) Unwrap() error {
	if e.RateLimited {
		return ErrRateLimited
	}
	return ErrProvider
}

type Provider interface {
	GetApp(context.Context) (App, error)
	GetInstallation(context.Context, int64) (Installation, error)
	ListInstallationRepositories(context.Context, int64) ([]Repository, error)
	MintDiscoveryToken(context.Context, int64) (Token, error)
	MintCheckoutToken(context.Context, int64, []int64) (Token, error)
	RevokeToken(context.Context, string) error
}

func VerifyApp(app App, want AppIdentity, owner Owner) error {
	if !want.Valid() || !owner.Valid() || app.ID != want.AppID || app.ClientID != want.ClientID || app.Slug != want.Slug || app.Owner != owner || !exactPermissions(app.Permissions, map[string]string{"contents": "read", "metadata": "read"}) || !exactStrings(app.Events, []string{"push"}) {
		return ErrInvalidIdentity
	}
	return nil
}

func exactPermissions(got, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for key, value := range want {
		if got[key] != value {
			return false
		}
	}
	return true
}
func exactStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[string]struct{}, len(got))
	for _, value := range got {
		if _, exists := seen[value]; exists {
			return false
		}
		seen[value] = struct{}{}
	}
	for _, value := range want {
		if _, ok := seen[value]; !ok {
			return false
		}
	}
	return true
}

func validText(s string, limit int) bool {
	if s == "" || len(s) > limit {
		return false
	}
	for _, r := range s {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}
