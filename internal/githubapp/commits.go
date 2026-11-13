package githubapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"autodeploy/internal/github"
)

var (
	ErrCommitNotFound         = errors.New("GitHub commit not found")
	ErrCommitNotOnRef         = errors.New("GitHub commit is not on the branch")
	ErrRefNotFound            = errors.New("GitHub branch not found")
	ErrGitHubUnavailable      = errors.New("GitHub is unavailable")
	ErrRepositoryUnauthorized = errors.New("GitHub repository access unauthorized")
	ErrInvalidRef             = errors.New("invalid branch ref")
	ErrInvalidCommitSHA       = errors.New("invalid commit sha")
)

// CommitResolver resolves and verifies commits with a short-lived token
// narrowed to Contents:read on a single repository.
type CommitResolver interface {
	ResolveBranchHead(ctx context.Context, installationID, repositoryID int64, fullRef string) (string, error)
	VerifyCommitOnRef(ctx context.Context, installationID, repositoryID int64, fullRef, sha string) error
}

var _ CommitResolver = (*HTTPProvider)(nil)

// ResolveBranchHead returns the commit sha at the head of fullRef (refs/heads/<branch>).
func (p *HTTPProvider) ResolveBranchHead(ctx context.Context, installationID, repositoryID int64, fullRef string) (sha string, err error) {
	branch, err := branchFromRef(fullRef)
	if err != nil {
		return "", err
	}
	if repositoryID <= 0 {
		return "", ErrInvalidIdentity
	}
	token, err := p.mintScoped(ctx, installationID, repositoryID)
	if err != nil {
		return "", err
	}
	defer p.revokeInto(ctx, &token, &err)
	var wire struct {
		Object struct {
			Type string `json:"type"`
			SHA  string `json:"sha"`
		} `json:"object"`
	}
	path := fmt.Sprintf("/repositories/%d/git/ref/heads/%s", repositoryID, escapeSegments(branch))
	if err = p.doJSON(ctx, http.MethodGet, path, nil, &wire, token.Value, http.StatusOK); err != nil {
		return "", classifyResolveError(err, ErrRefNotFound)
	}
	if wire.Object.Type != "commit" || !github.ValidCommitSHA(wire.Object.SHA) {
		return "", ErrInvalidResponse
	}
	return wire.Object.SHA, nil
}

// VerifyCommitOnRef proves sha exists in the repository and is reachable from fullRef.
func (p *HTTPProvider) VerifyCommitOnRef(ctx context.Context, installationID, repositoryID int64, fullRef, sha string) (err error) {
	if !github.ValidCommitSHA(sha) {
		return ErrInvalidCommitSHA
	}
	branch, err := branchFromRef(fullRef)
	if err != nil {
		return err
	}
	if repositoryID <= 0 {
		return ErrInvalidIdentity
	}
	token, err := p.mintScoped(ctx, installationID, repositoryID)
	if err != nil {
		return err
	}
	defer p.revokeInto(ctx, &token, &err)
	var commit struct {
		SHA string `json:"sha"`
	}
	if err = p.doJSON(ctx, http.MethodGet, fmt.Sprintf("/repositories/%d/commits/%s", repositoryID, sha), nil, &commit, token.Value, http.StatusOK); err != nil {
		return classifyResolveError(err, ErrCommitNotFound)
	}
	if commit.SHA != sha {
		return ErrInvalidResponse
	}
	// compare base...head with base=sha, head=branch: "ahead" or "identical"
	// means the branch head contains sha; "behind" or "diverged" means it does not.
	var cmp struct {
		Status string `json:"status"`
	}
	path := fmt.Sprintf("/repositories/%d/compare/%s...%s?per_page=1", repositoryID, sha, escapeSegments(branch))
	if err = p.doJSON(ctx, http.MethodGet, path, nil, &cmp, token.Value, http.StatusOK); err != nil {
		var pe *ProviderError
		if errors.As(err, &pe) && pe.StatusCode == http.StatusUnprocessableEntity {
			return ErrCommitNotOnRef
		}
		return classifyResolveError(err, ErrRefNotFound)
	}
	switch cmp.Status {
	case "identical", "ahead":
		return nil
	case "behind", "diverged":
		return ErrCommitNotOnRef
	}
	return ErrInvalidResponse
}

func branchFromRef(fullRef string) (string, error) {
	if !github.ValidBranchRef(fullRef) {
		return "", ErrInvalidRef
	}
	return strings.TrimPrefix(fullRef, "refs/heads/"), nil
}

func escapeSegments(branch string) string {
	parts := strings.Split(branch, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

func (p *HTTPProvider) mintScoped(ctx context.Context, installationID, repositoryID int64) (Token, error) {
	token, err := p.MintCheckoutToken(ctx, installationID, []int64{repositoryID})
	if err != nil {
		return Token{}, classifyResolveError(err, ErrRepositoryUnauthorized)
	}
	return token, nil
}

func (p *HTTPProvider) revokeInto(ctx context.Context, token *Token, err *error) {
	value := token.Value
	token.Value = ""
	if revokeErr := p.RevokeToken(context.WithoutCancel(ctx), value); revokeErr != nil {
		if *err == nil {
			*err = errors.Join(ErrRevocationRequired, revokeErr)
		} else {
			*err = errors.Join(*err, ErrRevocationRequired)
		}
	}
}

// classifyResolveError maps provider failures to stable sentinels. notFound is
// the sentinel for a 404 at the current stage. Errors never carry bodies or tokens.
func classifyResolveError(err error, notFound error) error {
	var pe *ProviderError
	if !errors.As(err, &pe) {
		if errors.Is(err, ErrInvalidResponse) || errors.Is(err, ErrInvalidIdentity) || errors.Is(err, ErrRevocationRequired) {
			return err
		}
		return errors.Join(ErrGitHubUnavailable, err)
	}
	switch {
	case pe.RateLimited, pe.StatusCode == 0, pe.StatusCode >= 500:
		return errors.Join(ErrGitHubUnavailable, err)
	case pe.StatusCode == http.StatusUnauthorized, pe.StatusCode == http.StatusForbidden:
		return errors.Join(ErrRepositoryUnauthorized, err)
	case pe.StatusCode == http.StatusNotFound:
		return errors.Join(notFound, err)
	case pe.StatusCode == http.StatusUnprocessableEntity:
		if notFound == ErrRepositoryUnauthorized {
			return errors.Join(ErrRepositoryUnauthorized, err)
		}
		return errors.Join(notFound, err)
	}
	return errors.Join(ErrGitHubUnavailable, err)
}
