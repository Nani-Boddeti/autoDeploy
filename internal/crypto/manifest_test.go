package crypto

import "testing"

func TestParseKeyManifest(t *testing.T) {
	valid := []byte(`{"active_key_id":"k2","database_generation_id":"0123456789abcdef","keys":[{"id":"k2","state":"active","path":"/keys/k2"},{"id":"k1","state":"retiring","path":"/keys/k1"}]}`)
	manifest, err := ParseKeyManifest(valid)
	if err != nil || manifest.ActiveKeyID != "k2" {
		t.Fatalf("manifest %#v err %v", manifest, err)
	}
	for _, raw := range [][]byte{[]byte(`{}`), []byte(`{"active_key_id":"k2","keys":[{"id":"k2","state":"retiring","path":"/keys/k2"}]}`), []byte(`{"active_key_id":"k2","keys":[{"id":"k2","state":"active","path":"/keys/k2"},{"id":"k2","state":"retiring","path":"/keys/k1"}]}`), append(valid, []byte(`x`)...)} {
		if _, err := ParseKeyManifest(raw); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}
