//go:build !darwin && !linux

// Package credential loads small, owner-controlled secret files without exposing their contents.
package credential

import "errors"

const MaxDatabaseURLBytes = 4096

var (
	ErrInvalidCredentialFile = errors.New("invalid credential file")
	ErrInvalidDatabaseURL    = errors.New("invalid database credential")
)

// Credential file access is intentionally unavailable until a platform-safe,
// descriptor-relative implementation exists for this operating system.
func LoadDatabaseURL(string, string, int) (string, error) { return "", ErrInvalidCredentialFile }
func LoadKey(string, string, int) ([]byte, error)         { return nil, ErrInvalidCredentialFile }
func SanitizedError(error) error                          { return ErrInvalidCredentialFile }
