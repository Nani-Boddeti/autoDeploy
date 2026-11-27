package githubapp

import (
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"autodeploy/internal/auth"
)

const (
	// MaxAppNameLength is GitHub's App name limit (characters).
	MaxAppNameLength = 34
	maxOrgLoginLen   = 39
	accountPersonal  = "personal"
	accountOrg       = "organization"
	githubWebOrigin  = "https://github.com"
)

// ErrInvalidManifest reports invalid manifest builder input.
var ErrInvalidManifest = errors.New("invalid GitHub App manifest input")

var orgLoginPattern = regexp.MustCompile(`^[A-Za-z0-9]+(-[A-Za-z0-9]+)*$`)

// ManifestInput is the owner-chosen data for the manifest registration form.
type ManifestInput struct {
	Origin      string // canonical https origin of this control plane
	AccountType string // "personal" or "organization"
	OrgLogin    string // required iff AccountType is "organization"
	Name        string // 1..34 printable characters after trimming
}

// ManifestForm is what the auto-submit page needs: the manifest JSON (form
// field "manifest") and the GitHub URL to POST it to (already carrying state).
type ManifestForm struct {
	JSON      []byte
	ActionURL string
}

type manifestHook struct {
	URL    string `json:"url"`
	Active bool   `json:"active"`
}

type manifestPermissions struct {
	Contents string `json:"contents"`
	Metadata string `json:"metadata"`
}

type manifestDoc struct {
	Name                  string              `json:"name"`
	URL                   string              `json:"url"`
	HookAttributes        manifestHook        `json:"hook_attributes"`
	RedirectURL           string              `json:"redirect_url"`
	SetupURL              string              `json:"setup_url"`
	SetupOnUpdate         bool                `json:"setup_on_update"`
	Public                bool                `json:"public"`
	DefaultEvents         []string            `json:"default_events"`
	DefaultPermissions    manifestPermissions `json:"default_permissions"`
	RequestOAuthOnInstall bool                `json:"request_oauth_on_install"`
}

// BuildManifest validates input and returns the manifest JSON and form action
// URL. state must be a 43-character base64url capability (see NewCapability).
func BuildManifest(in ManifestInput, state string) (ManifestForm, error) {
	origin, err := auth.CanonicalHTTPSOrigin(in.Origin)
	if err != nil || origin != in.Origin {
		return ManifestForm{}, ErrInvalidManifest
	}
	name := strings.TrimSpace(in.Name)
	if !validAppName(name) {
		return ManifestForm{}, ErrInvalidManifest
	}
	if _, err = SetupDigest(state); err != nil {
		return ManifestForm{}, ErrInvalidManifest
	}
	var action string
	switch in.AccountType {
	case accountPersonal:
		if in.OrgLogin != "" {
			return ManifestForm{}, ErrInvalidManifest
		}
		action = githubWebOrigin + "/settings/apps/new"
	case accountOrg:
		if len(in.OrgLogin) > maxOrgLoginLen || !orgLoginPattern.MatchString(in.OrgLogin) {
			return ManifestForm{}, ErrInvalidManifest
		}
		action = githubWebOrigin + "/organizations/" + url.PathEscape(in.OrgLogin) + "/settings/apps/new"
	default:
		return ManifestForm{}, ErrInvalidManifest
	}
	doc := manifestDoc{
		Name:               name,
		URL:                origin,
		HookAttributes:     manifestHook{URL: origin + "/webhooks/github", Active: true},
		RedirectURL:        origin + "/github/create/callback",
		SetupURL:           origin + "/github/setup/callback",
		SetupOnUpdate:      true,
		DefaultEvents:      []string{"push"},
		DefaultPermissions: manifestPermissions{Contents: "read", Metadata: "read"},
	}
	encoded, err := json.Marshal(doc)
	if err != nil {
		return ManifestForm{}, ErrInvalidManifest
	}
	return ManifestForm{JSON: encoded, ActionURL: action + "?state=" + state}, nil
}

// DefaultAppName returns "AutoDeploy <host>" truncated to 34 bytes on a rune
// boundary (trailing spaces trimmed).
func DefaultAppName(host string) string {
	name := "AutoDeploy " + host
	if len(name) > MaxAppNameLength {
		cut := MaxAppNameLength
		for cut > 0 && !utf8.RuneStart(name[cut]) {
			cut--
		}
		name = name[:cut]
	}
	return strings.TrimSpace(name)
}

// validAppName accepts 1..34 valid-UTF-8 printable characters (spaces allowed,
// no control or format characters). The caller trims first.
func validAppName(s string) bool {
	if s == "" || !utf8.ValidString(s) || utf8.RuneCountInString(s) > MaxAppNameLength {
		return false
	}
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}
