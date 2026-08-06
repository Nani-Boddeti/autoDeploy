//go:build !darwin && !linux

package credential

import (
	"errors"
	"testing"
)

func TestUnsupportedCredentialLoadingFailsClosed(t *testing.T) {
	if value, err := LoadDatabaseURL("/credential", "/workspace", 1); value != "" || !errors.Is(err, ErrInvalidCredentialFile) {
		t.Fatalf("database value=%q err=%v", value, err)
	}
	if value, err := LoadKey("/credential", "/workspace", 1); value != nil || !errors.Is(err, ErrInvalidCredentialFile) {
		t.Fatalf("key=%v err=%v", value, err)
	}
	if err := SanitizedError(errors.New("secret")); !errors.Is(err, ErrInvalidCredentialFile) || err.Error() == "secret" {
		t.Fatalf("sanitized error %v", err)
	}
}
