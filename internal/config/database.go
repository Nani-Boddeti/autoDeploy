// Package config reads non-secret process configuration.
package config

import (
	"errors"
	"net/netip"
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
	PublicOriginEnv                     = "AUTODEPLOY_PUBLIC_ORIGIN"
	CSRFActiveVersionEnv                = "AUTODEPLOY_CSRF_ACTIVE_VERSION"
	CSRFActiveKeyFileEnv                = "AUTODEPLOY_CSRF_ACTIVE_KEY_FILE"
	CSRFRetainedKeyFilesEnv             = "AUTODEPLOY_CSRF_RETAINED_KEY_FILES"
	TrustedProxyCIDRsEnv                = "AUTODEPLOY_TRUSTED_PROXY_CIDRS"
	ListenAddressEnv                    = "AUTODEPLOY_LISTEN_ADDRESS"
	AuthThrottleMaxRowsEnv              = "AUTODEPLOY_AUTH_THROTTLE_MAX_ROWS"
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

// WebConfig contains only public process settings and credential-derived
// signing keys. Credential material is never accepted as an environment value.
type WebConfig struct {
	PublicOrigin        string
	CSRFKeys            auth.CSRFKeyRing
	TrustedProxyCIDRs   []netip.Prefix
	ListenAddress       string
	AuthThrottleMaxRows int
}

func WebConfigFromEnvironment() (WebConfig, error) {
	origin, err := auth.CanonicalHTTPSOrigin(os.Getenv(PublicOriginEnv))
	if err != nil {
		return WebConfig{}, ErrInvalidEnvironment
	}
	keys, err := csrfKeyRingFromEnvironment()
	if err != nil {
		return WebConfig{}, err
	}
	proxies, err := trustedProxyCIDRs(os.Getenv(TrustedProxyCIDRsEnv))
	if err != nil {
		return WebConfig{}, ErrInvalidEnvironment
	}
	listen := os.Getenv(ListenAddressEnv)
	if listen == "" {
		listen = ":8080"
	}
	if len(listen) > 255 || strings.ContainsAny(listen, "\r\n\x00") {
		return WebConfig{}, ErrInvalidEnvironment
	}
	maxRows := 10000
	if raw := os.Getenv(AuthThrottleMaxRowsEnv); raw != "" {
		parsed, parseErr := strconv.Atoi(raw)
		if parseErr != nil || parsed < 8 || parsed > 100000 {
			return WebConfig{}, ErrInvalidEnvironment
		}
		maxRows = parsed
	}
	return WebConfig{PublicOrigin: origin, CSRFKeys: keys, TrustedProxyCIDRs: proxies, ListenAddress: listen, AuthThrottleMaxRows: maxRows}, nil
}

func csrfKeyRingFromEnvironment() (auth.CSRFKeyRing, error) {
	root, err := credentialSettings()
	if err != nil {
		return auth.CSRFKeyRing{}, err
	}
	activeVersion, activePath := os.Getenv(CSRFActiveVersionEnv), os.Getenv(CSRFActiveKeyFileEnv)
	if !validKeyFileEnvironment(activeVersion, activePath) {
		return auth.CSRFKeyRing{}, ErrInvalidEnvironment
	}
	entries, err := keyFileEntries(activeVersion, activePath, os.Getenv(CSRFRetainedKeyFilesEnv))
	if err != nil {
		return auth.CSRFKeyRing{}, err
	}
	ring := auth.CSRFKeyRing{ActiveVersion: activeVersion, Keys: make(map[string][]byte, len(entries))}
	for _, entry := range entries {
		key, loadErr := credential.LoadKey(entry.path, root.path, root.uid)
		if loadErr != nil {
			return auth.CSRFKeyRing{}, credential.SanitizedError(loadErr)
		}
		ring.Keys[entry.version] = key
	}
	if _, err := auth.NewSessionCSRF(ring, "config-check", "https://example.invalid"); err != nil {
		return auth.CSRFKeyRing{}, ErrInvalidEnvironment
	}
	return ring, nil
}

func keyFileEntries(activeVersion, activePath, retained string) ([]keyFileEntry, error) {
	entries := []keyFileEntry{{version: activeVersion, path: filepath.Clean(activePath)}}
	if retained != "" {
		for _, item := range strings.Split(retained, ",") {
			parts := strings.SplitN(item, "=", 2)
			if len(parts) != 2 || strings.Count(item, "=") != 1 || !validKeyFileEnvironment(parts[0], parts[1]) {
				return nil, ErrInvalidEnvironment
			}
			entries = append(entries, keyFileEntry{version: parts[0], path: filepath.Clean(parts[1])})
		}
	}
	if len(entries) > 5 {
		return nil, ErrInvalidEnvironment
	}
	seenVersions, seenPaths := make(map[string]struct{}, len(entries)), make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if _, exists := seenVersions[entry.version]; exists {
			return nil, ErrInvalidEnvironment
		}
		if _, exists := seenPaths[entry.path]; exists {
			return nil, ErrInvalidEnvironment
		}
		seenVersions[entry.version], seenPaths[entry.path] = struct{}{}, struct{}{}
	}
	return entries, nil
}

func validKeyFileEnvironment(version, path string) bool {
	if path == "" || !filepath.IsAbs(path) {
		return false
	}
	return len(version) > 0 && len(version) <= 32 && strings.IndexFunc(version, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-')
	}) == -1
}

func trustedProxyCIDRs(raw string) ([]netip.Prefix, error) {
	if raw == "" {
		return []netip.Prefix{}, nil
	}
	values := strings.Split(raw, ",")
	if len(values) > 32 {
		return nil, ErrInvalidEnvironment
	}
	prefixes := make([]netip.Prefix, 0, len(values))
	seen := make(map[netip.Prefix]struct{}, len(values))
	for _, value := range values {
		if value == "" || strings.TrimSpace(value) != value {
			return nil, ErrInvalidEnvironment
		}
		prefix, err := netip.ParsePrefix(value)
		if err != nil || prefix != prefix.Masked() {
			return nil, ErrInvalidEnvironment
		}
		if prefix.Addr().Is4In6() {
			return nil, ErrInvalidEnvironment
		}
		if _, exists := seen[prefix]; exists {
			return nil, ErrInvalidEnvironment
		}
		seen[prefix] = struct{}{}
		prefixes = append(prefixes, prefix)
	}
	return prefixes, nil
}

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
