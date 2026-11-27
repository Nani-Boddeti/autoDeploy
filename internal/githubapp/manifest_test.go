package githubapp

import (
	"errors"
	"strings"
	"testing"
)

const testState = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" // 43 base64url chars

func TestBuildManifestGolden(t *testing.T) {
	personal, err := BuildManifest(ManifestInput{Origin: "https://cp.example.com", AccountType: "personal", Name: " AutoDeploy cp "}, testState)
	if err != nil {
		t.Fatal(err)
	}
	wantJSON := `{"name":"AutoDeploy cp","url":"https://cp.example.com","hook_attributes":{"url":"https://cp.example.com/webhooks/github","active":true},"redirect_url":"https://cp.example.com/github/create/callback","setup_url":"https://cp.example.com/github/setup/callback","setup_on_update":true,"public":false,"default_events":["push"],"default_permissions":{"contents":"read","metadata":"read"},"request_oauth_on_install":false}`
	if string(personal.JSON) != wantJSON {
		t.Fatalf("manifest JSON:\n%s", personal.JSON)
	}
	if personal.ActionURL != "https://github.com/settings/apps/new?state="+testState {
		t.Fatalf("action %q", personal.ActionURL)
	}
	org, err := BuildManifest(ManifestInput{Origin: "https://cp.example.com:8443", AccountType: "organization", OrgLogin: "my-org", Name: "X"}, testState)
	if err != nil {
		t.Fatal(err)
	}
	if org.ActionURL != "https://github.com/organizations/my-org/settings/apps/new?state="+testState {
		t.Fatalf("org action %q", org.ActionURL)
	}
	if !strings.Contains(string(org.JSON), `"url":"https://cp.example.com:8443"`) || !strings.Contains(string(org.JSON), `"redirect_url":"https://cp.example.com:8443/github/create/callback"`) {
		t.Fatalf("org manifest %s", org.JSON)
	}
}

func TestBuildManifestInvalid(t *testing.T) {
	good := ManifestInput{Origin: "https://cp.example.com", AccountType: "personal", Name: "App"}
	mut := func(f func(*ManifestInput)) ManifestInput { in := good; f(&in); return in }
	cases := map[string]ManifestInput{
		"http origin":       mut(func(i *ManifestInput) { i.Origin = "http://cp.example.com" }),
		"origin path":       mut(func(i *ManifestInput) { i.Origin = "https://cp.example.com/x" }),
		"origin slash":      mut(func(i *ManifestInput) { i.Origin = "https://cp.example.com/" }),
		"origin uppercase":  mut(func(i *ManifestInput) { i.Origin = "https://CP.example.com" }),
		"origin query":      mut(func(i *ManifestInput) { i.Origin = "https://cp.example.com?a=b" }),
		"origin empty":      mut(func(i *ManifestInput) { i.Origin = "" }),
		"name empty":        mut(func(i *ManifestInput) { i.Name = "" }),
		"name blank":        mut(func(i *ManifestInput) { i.Name = "   " }),
		"name 35":           mut(func(i *ManifestInput) { i.Name = strings.Repeat("a", 35) }),
		"name newline":      mut(func(i *ManifestInput) { i.Name = "a\nb" }),
		"name nul":          mut(func(i *ManifestInput) { i.Name = "a\x00b" }),
		"name bad utf8":     mut(func(i *ManifestInput) { i.Name = "a\xffb" }),
		"bad type":          mut(func(i *ManifestInput) { i.AccountType = "team" }),
		"personal with org": mut(func(i *ManifestInput) { i.OrgLogin = "acme" }),
		"org missing":       mut(func(i *ManifestInput) { i.AccountType = "organization" }),
		"org leading dash":  mut(func(i *ManifestInput) { i.AccountType = "organization"; i.OrgLogin = "-acme" }),
		"org trailing dash": mut(func(i *ManifestInput) { i.AccountType = "organization"; i.OrgLogin = "acme-" }),
		"org double dash":   mut(func(i *ManifestInput) { i.AccountType = "organization"; i.OrgLogin = "ac--me" }),
		"org 40":            mut(func(i *ManifestInput) { i.AccountType = "organization"; i.OrgLogin = strings.Repeat("a", 40) }),
		"org slash":         mut(func(i *ManifestInput) { i.AccountType = "organization"; i.OrgLogin = "a/b" }),
		"org underscore":    mut(func(i *ManifestInput) { i.AccountType = "organization"; i.OrgLogin = "a_b" }),
	}
	for name, in := range cases {
		if _, err := BuildManifest(in, testState); !errors.Is(err, ErrInvalidManifest) {
			t.Errorf("%s: err %v", name, err)
		}
	}
	for _, state := range []string{"", "short", testState + "A", strings.Repeat("!", 43)} {
		if _, err := BuildManifest(good, state); !errors.Is(err, ErrInvalidManifest) {
			t.Errorf("state %q: err %v", state, err)
		}
	}
	// Boundaries that must pass.
	ok := mut(func(i *ManifestInput) {
		i.Name = strings.Repeat("a", 34)
		i.AccountType = "organization"
		i.OrgLogin = strings.Repeat("a", 39)
	})
	if _, err := BuildManifest(ok, testState); err != nil {
		t.Fatalf("boundary: %v", err)
	}
	if _, err := BuildManifest(mut(func(i *ManifestInput) { i.Name = strings.Repeat("é", 34) }), testState); err != nil {
		t.Fatalf("34 runes: %v", err)
	}
}

func TestDefaultAppName(t *testing.T) {
	if got := DefaultAppName("cp.example.com"); got != "AutoDeploy cp.example.com" {
		t.Fatal(got)
	}
	long := DefaultAppName(strings.Repeat("a", 60))
	if len(long) != 34 {
		t.Fatalf("len %d", len(long))
	}
	multi := DefaultAppName(strings.Repeat("é", 30))                  // 11 + 60 bytes
	if len(multi) > 34 || !validAppName(multi) || len(multi)%2 != 1 { // "AutoDeploy " is 11 bytes, then 2-byte runes
		t.Fatalf("multibyte %q (%d)", multi, len(multi))
	}
	if got := DefaultAppName(strings.Repeat("a", 23) + " b"); strings.HasSuffix(got, " ") {
		t.Fatalf("trailing space %q", got)
	}
	if _, err := BuildManifest(ManifestInput{Origin: "https://x.example", AccountType: "personal", Name: multi}, testState); err != nil {
		t.Fatal(err)
	}
}
