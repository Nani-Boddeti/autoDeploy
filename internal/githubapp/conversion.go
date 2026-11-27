package githubapp

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// ErrManifestCodeRejected means GitHub refused the manifest code (expired,
// already used, or unknown). The owner must start the flow again.
var ErrManifestCodeRejected = errors.New("GitHub manifest code expired or already used")

const (
	manifestUserAgent  = "autodeploy-controlplane"
	conversionTimeout  = 15 * time.Second
	maxManifestHTMLURL = 512
	maxWebhookSecret   = 255
)

var slugPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

var manifestCodePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// ManifestOrphanError reports that GitHub created an App but its conversion
// response failed validation, so the App exists on GitHub and is unusable here.
// It carries only the App id and html_url (when parseable and valid) so the
// owner can delete it. It never holds secret material or response bodies.
type ManifestOrphanError struct {
	AppID   int64
	HTMLURL string
}

func (e *ManifestOrphanError) Error() string { return ErrInvalidResponse.Error() }
func (e *ManifestOrphanError) Unwrap() error { return ErrInvalidResponse }

// ManifestConversion is the validated result of exchanging a manifest code.
// client_secret is intentionally never retained.
type ManifestConversion struct {
	AppID         int64
	Slug          string
	ClientID      string
	Name          string
	HTMLURL       string
	OwnerID       int64
	OwnerLogin    string
	OwnerType     string
	PEM           []byte
	WebhookSecret []byte
}

// ManifestClient performs the unauthenticated manifest code conversion.
type ManifestClient struct {
	base   string
	client *http.Client
}

// NewManifestClient returns a client for api.github.com.
func NewManifestClient() *ManifestClient { return newManifestClient(APIOrigin) }

func newManifestClient(base string, timeout ...time.Duration) *ManifestClient {
	limit := conversionTimeout
	if len(timeout) > 0 {
		limit = timeout[0]
	}
	return &ManifestClient{base: base, client: &http.Client{
		Timeout:       limit,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

type conversionWire struct {
	ID       int64  `json:"id"`
	Slug     string `json:"slug"`
	ClientID string `json:"client_id"`
	Name     string `json:"name"`
	HTMLURL  string `json:"html_url"`
	Owner    *struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"owner"`
	PEM           string  `json:"pem"`
	WebhookSecret *string `json:"webhook_secret"`
}

// ConvertManifestCode exchanges a manifest code for the new App's credentials.
// The code is a bearer-like secret: it never appears in returned errors.
func (c *ManifestClient) ConvertManifestCode(ctx context.Context, code string) (ManifestConversion, error) {
	if !manifestCodePattern.MatchString(code) {
		return ManifestConversion{}, ErrInvalidIdentity
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/app-manifests/"+url.PathEscape(code)+"/conversions", nil)
	if err != nil {
		return ManifestConversion{}, ErrProvider
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", APIVersion)
	req.Header.Set("User-Agent", manifestUserAgent)
	resp, err := c.client.Do(req)
	if err != nil {
		// url.Error embeds the request URL (which holds the code): drop it.
		if ctx.Err() != nil {
			return ManifestConversion{}, errors.Join(ErrProvider, ctx.Err())
		}
		return ManifestConversion{}, ErrProvider
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusCreated:
	case http.StatusNotFound, http.StatusUnprocessableEntity:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes+1))
		return ManifestConversion{}, ErrManifestCodeRejected
	case http.StatusForbidden, http.StatusTooManyRequests:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes+1))
		if resp.StatusCode == http.StatusTooManyRequests || rateLimited(resp.Header, time.Now()) {
			return ManifestConversion{}, ErrRateLimited
		}
		return ManifestConversion{}, ErrProvider
	default:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes+1))
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return ManifestConversion{}, ErrInvalidResponse
		}
		return ManifestConversion{}, ErrProvider
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	defer clear(body)
	if err != nil || len(body) > maxBodyBytes {
		// A 201 means an App may exist: report whatever identity is readable.
		return ManifestConversion{}, orphanError(body)
	}
	result, err := parseConversion(body)
	if err != nil {
		return ManifestConversion{}, orphanError(body)
	}
	return result, nil
}

// orphanError builds the post-201 validation failure, attaching id and
// html_url only when each independently parses and validates.
func orphanError(body []byte) error {
	orphan := &ManifestOrphanError{}
	dec := json.NewDecoder(bytes.NewReader(body))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return orphan
	}
	// Scan top-level members one at a time so a truncated body still yields
	// the members that arrived complete.
	for dec.More() {
		keyTok, err := dec.Token()
		key, isString := keyTok.(string)
		if err != nil || !isString {
			break
		}
		var raw json.RawMessage
		if dec.Decode(&raw) != nil {
			break
		}
		switch key {
		case "id":
			var id int64
			if json.Unmarshal(raw, &id) == nil && id > 0 {
				orphan.AppID = id
			}
		case "html_url":
			var htmlURL string
			if json.Unmarshal(raw, &htmlURL) == nil && validGitHubHTMLURL(htmlURL) {
				orphan.HTMLURL = htmlURL
			}
		}
	}
	return orphan
}

func parseConversion(body []byte) (ManifestConversion, error) {
	var wire conversionWire
	if err := json.Unmarshal(body, &wire); err != nil {
		return ManifestConversion{}, ErrInvalidResponse
	}
	identity := AppIdentity{AppID: wire.ID, ClientID: wire.ClientID, Slug: wire.Slug}
	if !identity.Valid() || !slugPattern.MatchString(wire.Slug) || !printableText(wire.Name, 255) || !validGitHubHTMLURL(wire.HTMLURL) || wire.Owner == nil {
		return ManifestConversion{}, ErrInvalidResponse
	}
	owner := Owner{AccountID: wire.Owner.ID, Type: wire.Owner.Type}
	if !owner.Valid() || !validText(wire.Owner.Login, 100) {
		return ManifestConversion{}, ErrInvalidResponse
	}
	if wire.WebhookSecret == nil || *wire.WebhookSecret == "" || len(*wire.WebhookSecret) > maxWebhookSecret {
		return ManifestConversion{}, ErrInvalidResponse
	}
	canonical, ok := canonicalRSAPEM(wire.PEM)
	if !ok {
		return ManifestConversion{}, ErrInvalidResponse
	}
	return ManifestConversion{
		AppID: wire.ID, Slug: wire.Slug, ClientID: wire.ClientID, Name: wire.Name, HTMLURL: wire.HTMLURL,
		OwnerID: wire.Owner.ID, OwnerLogin: wire.Owner.Login, OwnerType: wire.Owner.Type,
		PEM: canonical, WebhookSecret: []byte(*wire.WebhookSecret),
	}, nil
}

// canonicalRSAPEM requires exactly one PEM block (no leading or trailing junk)
// holding an RSA private key of at least 2048 bits (the floor NewSigner
// enforces) and returns its canonical re-encoding.
func canonicalRSAPEM(s string) ([]byte, bool) {
	if !strings.HasPrefix(strings.TrimSpace(s), "-----BEGIN ") {
		return nil, false
	}
	block, rest := pem.Decode([]byte(s))
	if block == nil || strings.TrimSpace(string(rest)) != "" {
		return nil, false
	}
	defer clear(block.Bytes)
	var key *rsa.PrivateKey
	switch block.Type {
	case "RSA PRIVATE KEY":
		k, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, false
		}
		key = k
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, false
		}
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, false
		}
		key = rk
	default:
		return nil, false
	}
	if key.N.BitLen() < 2048 || key.Validate() != nil {
		return nil, false
	}
	return pem.EncodeToMemory(&pem.Block{Type: block.Type, Bytes: block.Bytes}), true
}

func validGitHubHTMLURL(s string) bool {
	if len(s) == 0 || len(s) > maxManifestHTMLURL || !strings.HasPrefix(s, githubWebOrigin+"/") {
		return false
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.Fragment != "" {
		return false
	}
	for _, r := range s {
		if r <= 0x20 || r >= 0x7f {
			return false
		}
	}
	return true
}

func printableText(s string, maxBytes int) bool {
	if s == "" || len(s) > maxBytes || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}
