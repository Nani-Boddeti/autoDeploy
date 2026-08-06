// Package config reads non-secret process configuration.
package config

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"autodeploy/internal/auth"
	"autodeploy/internal/credential"
)

const (
	DatabaseURLFileEnv                  = "AUTODEPLOY_DATABASE_URL_FILE"
	RepositoryRootEnv                   = "AUTODEPLOY_REPOSITORY_ROOT"
	CredentialOwnerUIDEnv               = "AUTODEPLOY_CREDENTIAL_OWNER_UID"
	UsernameThrottleActiveVersionEnv    = "AUTODEPLOY_USERNAME_THROTTLE_ACTIVE_VERSION"
	UsernameThrottleActiveKeyFileEnv    = "AUTODEPLOY_USERNAME_THROTTLE_ACTIVE_KEY_FILE"
	UsernameThrottleRetainedKeyFilesEnv = "AUTODEPLOY_USERNAME_THROTTLE_RETAINED_KEY_FILES"
)

var ErrInvalidEnvironment = errors.New("invalid database configuration")

// DatabaseURLFromEnvironment reads a database URL only from the configured credential file.
// AUTODEPLOY_REPOSITORY_ROOT is an explicit absolute repository/workspace root and is required
// so deployed credentials cannot accidentally be stored in the source tree.
func DatabaseURLFromEnvironment() (string, error) {
	if os.Getenv("AUTODEPLOY_DATABASE_URL") != "" {
		return "", ErrInvalidEnvironment
	}
	path, root := os.Getenv(DatabaseURLFileEnv), os.Getenv(RepositoryRootEnv)
	if path == "" || root == "" || !filepath.IsAbs(path) || !filepath.IsAbs(root) {
		return "", ErrInvalidEnvironment
	}
	uid := os.Geteuid()
	if raw := os.Getenv(CredentialOwnerUIDEnv); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			return "", ErrInvalidEnvironment
		}
		uid = parsed
	}
	value, err := credential.LoadDatabaseURL(path, root, uid)
	if err != nil {
		return "", credential.SanitizedError(err)
	}
	return value, nil
}

// UsernameThrottleKeyRingFromEnvironment loads only key-file paths from the
// environment. Key material itself remains in protected raw credential files.
func UsernameThrottleKeyRingFromEnvironment() (auth.UsernameThrottleKeyRing, error) {
	root, err := credentialSettings()
	if err != nil {
		return auth.UsernameThrottleKeyRing{}, err
	}
	activeVersion := os.Getenv(UsernameThrottleActiveVersionEnv)
	activePath := os.Getenv(UsernameThrottleActiveKeyFileEnv)
	if activeVersion == "" || activePath == "" || !filepath.IsAbs(activePath) {
		return auth.UsernameThrottleKeyRing{}, ErrInvalidEnvironment
	}
	entries := []keyFileEntry{{version: activeVersion, path: filepath.Clean(activePath)}}
	rawRetained := os.Getenv(UsernameThrottleRetainedKeyFilesEnv)
	if rawRetained != "" {
		for _, item := range strings.Split(rawRetained, ",") {
			if item == "" || strings.Count(item, "=") != 1 {
				return auth.UsernameThrottleKeyRing{}, ErrInvalidEnvironment
			}
			parts := strings.SplitN(item, "=", 2)
			if parts[0] == "" || parts[1] == "" || !filepath.IsAbs(parts[1]) {
				return auth.UsernameThrottleKeyRing{}, ErrInvalidEnvironment
			}
			entries = append(entries, keyFileEntry{version: parts[0], path: filepath.Clean(parts[1])})
		}
	}
	if len(entries) > 5 {
		return auth.UsernameThrottleKeyRing{}, ErrInvalidEnvironment
	}
	ring := auth.UsernameThrottleKeyRing{ActiveVersion: activeVersion, Keys: make(map[string][]byte, len(entries))}
	seenVersions, seenPaths := make(map[string]struct{}, len(entries)), make(map[string]struct{}, len(entries))
	for index, entry := range entries {
		if _, duplicate := seenVersions[entry.version]; duplicate {
			return auth.UsernameThrottleKeyRing{}, ErrInvalidEnvironment
		}
		if _, duplicate := seenPaths[entry.path]; duplicate {
			return auth.UsernameThrottleKeyRing{}, ErrInvalidEnvironment
		}
		key, loadErr := credential.LoadKey(entry.path, root.path, root.uid)
		if loadErr != nil {
			return auth.UsernameThrottleKeyRing{}, credential.SanitizedError(loadErr)
		}
		seenVersions[entry.version], seenPaths[entry.path] = struct{}{}, struct{}{}
		ring.Keys[entry.version] = key
		if index > 0 {
			ring.RetainedVersions = append(ring.RetainedVersions, entry.version)
		}
	}
	if _, err := ring.UsernameDigests("admin"); err != nil {
		return auth.UsernameThrottleKeyRing{}, ErrInvalidEnvironment
	}
	return ring, nil
}

type keyFileEntry struct{ version, path string }
type credentialConfig struct {
	path string
	uid  int
}

func credentialSettings() (credentialConfig, error) {
	root := os.Getenv(RepositoryRootEnv)
	if root == "" || !filepath.IsAbs(root) {
		return credentialConfig{}, ErrInvalidEnvironment
	}
	uid := os.Geteuid()
	if raw := os.Getenv(CredentialOwnerUIDEnv); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			return credentialConfig{}, ErrInvalidEnvironment
		}
		uid = parsed
	}
	return credentialConfig{path: root, uid: uid}, nil
}
