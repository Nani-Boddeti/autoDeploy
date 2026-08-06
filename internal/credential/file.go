//go:build darwin || linux

// Package credential loads small, owner-controlled secret files without exposing their contents.
package credential

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// MaxDatabaseURLBytes bounds database credential files. PostgreSQL URLs are normally far smaller.
const MaxDatabaseURLBytes = 4096

var (
	ErrInvalidCredentialFile = errors.New("invalid credential file")
	ErrInvalidDatabaseURL    = errors.New("invalid database credential")
)

// afterCredentialPreReadForTest deterministically exercises mutation between
// validation and read. It is nil in production.
var afterCredentialPreReadForTest func()

// LoadDatabaseURL loads and validates a PostgreSQL URL from path. workspaceRoot must be an
// explicit absolute repository/workspace root; credential files in that tree are rejected.
func LoadDatabaseURL(path, workspaceRoot string, expectedUID int) (string, error) {
	contents, err := loadCredential(path, workspaceRoot, expectedUID, MaxDatabaseURLBytes)
	if err != nil {
		return "", err
	}
	value, err := normalizeDatabaseURL(contents)
	if err != nil {
		return "", err
	}
	return value, nil
}

// LoadKey loads an exact 32-byte raw key. Unlike URL credentials it performs
// no whitespace trimming or text normalization.
func LoadKey(path, workspaceRoot string, expectedUID int) ([]byte, error) {
	contents, err := loadCredential(path, workspaceRoot, expectedUID, 32)
	if err != nil || len(contents) != 32 {
		return nil, ErrInvalidCredentialFile
	}
	return contents, nil
}

func loadCredential(path, workspaceRoot string, expectedUID, limit int) ([]byte, error) {
	if path == "" || !filepath.IsAbs(path) || workspaceRoot == "" || !filepath.IsAbs(workspaceRoot) {
		return nil, ErrInvalidCredentialFile
	}
	cleanPath, cleanRoot := filepath.Clean(path), filepath.Clean(workspaceRoot)
	if cleanPath == cleanRoot || strings.HasPrefix(cleanPath, cleanRoot+string(filepath.Separator)) {
		return nil, ErrInvalidCredentialFile
	}
	fd, err := openCredentialFile(cleanPath, expectedUID)
	if err != nil {
		return nil, ErrInvalidCredentialFile
	}
	defer unix.Close(fd) //nolint:errcheck

	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || !safeCredentialStat(stat, expectedUID) || stat.Size < 0 || stat.Size > int64(limit) {
		return nil, ErrInvalidCredentialFile
	}
	fingerprint := credentialStatFingerprint(stat)
	if afterCredentialPreReadForTest != nil {
		afterCredentialPreReadForTest()
	}
	contents, err := readBounded(fd, limit)
	if err != nil || int64(len(contents)) != stat.Size {
		return nil, ErrInvalidCredentialFile
	}
	var after unix.Stat_t
	if err := unix.Fstat(fd, &after); err != nil || !safeCredentialStat(after, expectedUID) || credentialStatFingerprint(after) != fingerprint {
		return nil, ErrInvalidCredentialFile
	}
	return contents, nil
}

func safeFileMode(mode uint32) bool {
	permissions := mode & 0777
	return mode&07000 == 0 && permissions&0177 == 0 && permissions&0400 != 0
}

func readBounded(fd int, limit int) ([]byte, error) {
	contents := make([]byte, 0, limit+1)
	buffer := make([]byte, 512)
	for {
		read, err := unix.Read(fd, buffer)
		if read > 0 {
			contents = append(contents, buffer[:read]...)
			if len(contents) > limit {
				return nil, errors.New("credential file size")
			}
		}
		if err != nil {
			return nil, err
		}
		if read == 0 {
			return contents, nil
		}
	}
}

func normalizeDatabaseURL(contents []byte) (string, error) {
	value := string(contents)
	if strings.HasSuffix(value, "\n") {
		value = strings.TrimSuffix(value, "\n")
	}
	if value == "" || strings.ContainsAny(value, "\r\n\x00") {
		return "", ErrInvalidDatabaseURL
	}
	parsed, err := url.ParseRequestURI(value)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") || parsed.Host == "" || parsed.User == nil {
		return "", ErrInvalidDatabaseURL
	}
	return value, nil
}

// SanitizedError makes configuration failures safe to log.
func SanitizedError(err error) error {
	if errors.Is(err, ErrInvalidDatabaseURL) {
		return ErrInvalidDatabaseURL
	}
	if errors.Is(err, ErrInvalidCredentialFile) {
		return ErrInvalidCredentialFile
	}
	return fmt.Errorf("credential configuration failed")
}
