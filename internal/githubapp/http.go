package githubapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	maxBodyBytes         = 1 << 20
	maxRepositories      = 1000
	maxPages             = 10
	maxTokenBytes        = 8192
	tokenSafetyMargin    = 30 * time.Second
	tokenExpiryTolerance = time.Minute
)

type HTTPProvider struct {
	client    *http.Client
	signer    *Signer
	userAgent string
	now       func() time.Time
}

func NewHTTPProvider(signer *Signer, userAgent string) (*HTTPProvider, error) {
	if signer == nil || !validText(userAgent, 255) {
		return nil, ErrInvalidIdentity
	}
	return &HTTPProvider{signer: signer, userAgent: userAgent, now: time.Now, client: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (p *HTTPProvider) GetApp(ctx context.Context) (App, error) {
	var wire struct {
		ID       int64  `json:"id"`
		ClientID string `json:"client_id"`
		Slug     string `json:"slug"`
		Owner    struct {
			ID   int64  `json:"id"`
			Type string `json:"type"`
		} `json:"owner"`
		Permissions map[string]string `json:"permissions"`
		Events      []string          `json:"events"`
	}
	if err := p.doJSON(ctx, http.MethodGet, "/app", nil, &wire, "", http.StatusOK); err != nil {
		return App{}, err
	}
	owner := Owner{AccountID: wire.Owner.ID, Type: wire.Owner.Type}
	if wire.ID <= 0 || !validText(wire.ClientID, 255) || !validText(wire.Slug, 100) || !owner.Valid() {
		return App{}, ErrInvalidResponse
	}
	return App{ID: wire.ID, ClientID: wire.ClientID, Slug: wire.Slug, Owner: Owner{wire.Owner.ID, wire.Owner.Type}, Permissions: wire.Permissions, Events: wire.Events}, nil
}
func (p *HTTPProvider) GetInstallation(ctx context.Context, id int64) (Installation, error) {
	if id <= 0 {
		return Installation{}, ErrInvalidIdentity
	}
	var wire struct {
		ID                  int64      `json:"id"`
		SuspendedAt         *time.Time `json:"suspended_at"`
		TargetType          string     `json:"target_type"`
		RepositorySelection string     `json:"repository_selection"`
		Account             struct {
			ID   int64  `json:"id"`
			Type string `json:"type"`
		} `json:"account"`
	}
	if err := p.doJSON(ctx, http.MethodGet, fmt.Sprintf("/app/installations/%d", id), nil, &wire, "", http.StatusOK); err != nil {
		return Installation{}, err
	}
	account := Owner{wire.Account.ID, wire.Account.Type}
	if wire.ID != id || !account.Valid() || wire.TargetType != account.Type || (wire.RepositorySelection != "all" && wire.RepositorySelection != "selected") {
		return Installation{}, ErrInvalidResponse
	}
	return Installation{ID: wire.ID, Account: account, Suspended: wire.SuspendedAt != nil, TargetType: wire.TargetType, RepositorySelection: wire.RepositorySelection}, nil
}
func (p *HTTPProvider) ListInstallationRepositories(ctx context.Context, id int64) (repositories []Repository, err error) {
	token, err := p.MintDiscoveryToken(ctx, id)
	if err != nil {
		return nil, err
	}
	defer func() {
		value := token.Value
		token.Value = ""
		revokeErr := p.RevokeToken(context.WithoutCancel(ctx), value)
		if revokeErr != nil {
			if err == nil {
				err = errors.Join(ErrRevocationRequired, revokeErr)
			} else {
				err = errors.Join(err, ErrRevocationRequired)
			}
		}
	}()
	seen := make(map[int64]struct{}, maxRepositories)
	for page := 1; page <= maxPages; page++ {
		var wire struct {
			TotalCount   int `json:"total_count"`
			Repositories []struct {
				ID            int64  `json:"id"`
				NodeID        string `json:"node_id"`
				Name          string `json:"name"`
				FullName      string `json:"full_name"`
				DefaultBranch string `json:"default_branch"`
				Owner         struct {
					ID int64 `json:"id"`
				} `json:"owner"`
			} `json:"repositories"`
		}
		path := fmt.Sprintf("/installation/repositories?per_page=100&page=%d", page)
		if err = p.doJSON(ctx, http.MethodGet, path, nil, &wire, token.Value, http.StatusOK); err != nil {
			return nil, err
		}
		if wire.TotalCount < 0 || wire.TotalCount > maxRepositories || len(wire.Repositories) > 100 {
			return nil, ErrInvalidResponse
		}
		for _, r := range wire.Repositories {
			if !validRepository(r.ID, r.Name, r.FullName) || (r.NodeID != "" && !validText(r.NodeID, 255)) || (r.DefaultBranch != "" && !validText(r.DefaultBranch, 255)) {
				return nil, ErrInvalidResponse
			}
			if _, exists := seen[r.ID]; exists {
				return nil, ErrInvalidResponse
			}
			seen[r.ID] = struct{}{}
			repositories = append(repositories, Repository{ID: r.ID, NodeID: r.NodeID, OwnerID: r.Owner.ID, Name: r.Name, FullName: r.FullName, DefaultBranch: r.DefaultBranch})
			if len(repositories) > maxRepositories {
				return nil, ErrInvalidResponse
			}
		}
		if len(repositories) == wire.TotalCount {
			return repositories, nil
		}
		if len(repositories) > wire.TotalCount || len(wire.Repositories) < 100 {
			return nil, ErrInvalidResponse
		}
	}
	return nil, ErrInvalidResponse
}
func validRepository(id int64, name, full string) bool {
	if id <= 0 || !validText(name, 255) || !validText(full, 512) {
		return false
	}
	owner, repo, ok := strings.Cut(full, "/")
	return ok && owner != "" && repo == name && validText(owner, 255) && !strings.Contains(repo, "/")
}
func (p *HTTPProvider) MintDiscoveryToken(ctx context.Context, id int64) (Token, error) {
	return p.mint(ctx, id, nil, true)
}
func (p *HTTPProvider) MintCheckoutToken(ctx context.Context, id int64, repos []int64) (Token, error) {
	if len(repos) != 1 || repos[0] <= 0 {
		return Token{}, ErrInvalidIdentity
	}
	return p.mint(ctx, id, repos, false)
}
func (p *HTTPProvider) mint(ctx context.Context, id int64, repos []int64, discovery bool) (Token, error) {
	if id <= 0 {
		return Token{}, ErrInvalidIdentity
	}
	permissions := map[string]string{"metadata": "read"}
	if !discovery {
		permissions = map[string]string{"contents": "read"}
	}
	payload := map[string]any{"permissions": permissions}
	if !discovery {
		payload["repository_ids"] = repos
	}
	var wire struct {
		Token               string            `json:"token"`
		ExpiresAt           time.Time         `json:"expires_at"`
		Permissions         map[string]string `json:"permissions"`
		RepositorySelection string            `json:"repository_selection"`
		Repositories        []struct {
			ID int64 `json:"id"`
		} `json:"repositories"`
	}
	if err := p.doJSON(ctx, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", id), payload, &wire, "", http.StatusCreated); err != nil {
		return Token{}, err
	}
	if !validToken(wire.Token, wire.ExpiresAt, p.now()) {
		return Token{}, p.revokeInvalidMint(ctx, &wire.Token)
	}
	if discovery {
		if wire.RepositorySelection != "all" || !exactPermissions(wire.Permissions, map[string]string{"metadata": "read"}) {
			return Token{}, p.revokeInvalidMint(ctx, &wire.Token)
		}
		return Token{Value: wire.Token, ExpiresAt: wire.ExpiresAt, Permissions: wire.Permissions}, nil
	}
	if wire.RepositorySelection != "selected" || !exactPermissions(wire.Permissions, map[string]string{"contents": "read"}) || len(wire.Repositories) != 1 || wire.Repositories[0].ID != repos[0] {
		return Token{}, p.revokeInvalidMint(ctx, &wire.Token)
	}
	return Token{Value: wire.Token, ExpiresAt: wire.ExpiresAt, RepositoryIDs: []int64{wire.Repositories[0].ID}, Contents: "read", Permissions: wire.Permissions}, nil
}
func (p *HTTPProvider) revokeInvalidMint(ctx context.Context, value *string) error {
	token := *value
	*value = ""
	if token == "" {
		return ErrInvalidResponse
	}
	if err := p.RevokeToken(context.WithoutCancel(ctx), token); err != nil {
		return errors.Join(ErrInvalidResponse, ErrRevocationRequired)
	}
	return ErrInvalidResponse
}
func validToken(value string, expiresAt, now time.Time) bool {
	return value != "" && len(value) <= maxTokenBytes && !strings.ContainsAny(value, "\r\n\x00") && !expiresAt.IsZero() && expiresAt.After(now.Add(tokenSafetyMargin)) && expiresAt.Before(now.Add(time.Hour+tokenExpiryTolerance).Add(time.Nanosecond))
}
func (p *HTTPProvider) RevokeToken(ctx context.Context, token string) error {
	if token == "" || len(token) > maxTokenBytes || strings.ContainsAny(token, "\r\n\x00") {
		return ErrInvalidIdentity
	}
	return p.doJSON(ctx, http.MethodDelete, "/installation/token", nil, nil, token, http.StatusNoContent)
}
func (p *HTTPProvider) doJSON(ctx context.Context, method, path string, payload any, out any, installationToken string, wantStatus int) error {
	var encoded []byte
	if payload != nil {
		var err error
		encoded, err = json.Marshal(payload)
		if err != nil {
			return ErrInvalidIdentity
		}
	}
	jwt, err := p.signer.Token()
	if err != nil {
		return err
	}
	for attempt := 0; attempt < 2; attempt++ {
		var body io.Reader
		if encoded != nil {
			body = bytes.NewReader(encoded)
		}
		req, err := http.NewRequestWithContext(ctx, method, APIOrigin+path, body)
		if err != nil {
			return ErrProvider
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", APIVersion)
		req.Header.Set("User-Agent", p.userAgent)
		if installationToken != "" {
			req.Header.Set("Authorization", "Bearer "+installationToken)
		} else {
			req.Header.Set("Authorization", "Bearer "+jwt)
		}
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := p.client.Do(req)
		if err != nil {
			if method == http.MethodGet && attempt == 0 {
				continue
			}
			return &ProviderError{Temporary: true}
		}
		err = p.decode(resp, out, wantStatus)
		if err == nil {
			return nil
		}
		pe, ok := err.(*ProviderError)
		if method == http.MethodGet && attempt == 0 && ok && pe.Temporary {
			continue
		}
		return err
	}
	return ErrProvider
}
func (p *HTTPProvider) decode(resp *http.Response, out any, wantStatus int) error {
	defer resp.Body.Close()
	requestID := safeRequestID(resp.Header.Get("X-GitHub-Request-Id"))
	if resp.StatusCode != wantStatus {
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return ErrInvalidResponse
		}
		pe := &ProviderError{StatusCode: resp.StatusCode, RequestID: requestID, Temporary: resp.StatusCode == 502 || resp.StatusCode == 503 || resp.StatusCode == 504}
		if resp.StatusCode == 429 || (resp.StatusCode == 403 && rateLimited(resp.Header, p.now())) {
			pe.RateLimited = true
			pe.RetryAfter = retryAfter(resp.Header, p.now())
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes+1))
		return pe
	}
	if out == nil {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil || len(body) > maxBodyBytes {
		return ErrInvalidResponse
	}
	if err = json.Unmarshal(body, out); err != nil {
		return ErrInvalidResponse
	}
	return nil
}
func safeRequestID(v string) string {
	if len(v) > 128 {
		return ""
	}
	for _, r := range v {
		if r < '!' || r > '~' {
			return ""
		}
	}
	return v
}
func rateLimited(h http.Header, now time.Time) bool {
	if h.Get("Retry-After") != "" || h.Get("X-RateLimit-Remaining") == "0" {
		return true
	}
	_, err := rateReset(h, now)
	return err == nil
}
func retryAfter(h http.Header, now time.Time) time.Duration {
	if n, err := strconv.Atoi(h.Get("Retry-After")); err == nil && n >= 0 && n <= 3600 {
		return time.Duration(n) * time.Second
	}
	d, _ := rateReset(h, now)
	return d
}
func rateReset(h http.Header, now time.Time) (time.Duration, error) {
	n, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64)
	if err != nil {
		return 0, err
	}
	d := time.Unix(n, 0).Sub(now)
	if d <= 0 || d > time.Hour {
		return 0, errors.New("invalid rate reset")
	}
	return d, nil
}
