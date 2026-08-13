package crypto

import (
	"bytes"
	"encoding/json"
	"io"
	"path/filepath"
)

const MaxKEKFiles = 64

// KeyManifest is non-secret configuration. Paths must be validated and loaded by the caller.
type KeyManifest struct {
	ActiveKeyID          string             `json:"active_key_id"`
	DatabaseGenerationID string             `json:"database_generation_id"`
	Keys                 []KeyManifestEntry `json:"keys"`
}
type KeyManifestEntry struct {
	ID    string `json:"id"`
	State string `json:"state"`
	Path  string `json:"path"`
}

// ParseKeyManifest strictly accepts the active/retiring mounted-file key-ring schema.
func ParseKeyManifest(data []byte) (KeyManifest, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var manifest KeyManifest
	if err := decoder.Decode(&manifest); err != nil {
		return KeyManifest{}, ErrInvalidInput
	}
	if err := decoder.Decode(new(struct{})); err != io.EOF {
		return KeyManifest{}, ErrInvalidInput
	}
	if len(manifest.Keys) == 0 || len(manifest.Keys) > MaxKEKFiles || !validKeyID(manifest.ActiveKeyID) || !validGenerationID(manifest.DatabaseGenerationID) {
		return KeyManifest{}, ErrInvalidInput
	}
	ids, paths, active := make(map[string]struct{}, len(manifest.Keys)), make(map[string]struct{}, len(manifest.Keys)), 0
	for _, entry := range manifest.Keys {
		if !validKeyID(entry.ID) || entry.Path == "" || len(entry.Path) > 4096 || !filepath.IsAbs(entry.Path) || filepath.Clean(entry.Path) != entry.Path || (entry.State != "active" && entry.State != "retiring") {
			return KeyManifest{}, ErrInvalidInput
		}
		if _, exists := ids[entry.ID]; exists {
			return KeyManifest{}, ErrInvalidInput
		}
		ids[entry.ID] = struct{}{}
		if _, exists := paths[entry.Path]; exists {
			return KeyManifest{}, ErrInvalidInput
		}
		paths[entry.Path] = struct{}{}
		if entry.State == "active" {
			active++
			if entry.ID != manifest.ActiveKeyID {
				return KeyManifest{}, ErrInvalidInput
			}
		}
	}
	if active != 1 {
		return KeyManifest{}, ErrInvalidInput
	}
	return manifest, nil
}

func validGenerationID(id string) bool { return len(id) >= 16 && len(id) <= 128 }
