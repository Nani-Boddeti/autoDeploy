package githubapp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	commitTestToken = "commit-token-sentinel"
	sha40           = "0123456789abcdef0123456789abcdef01234567"
)

type commitFake struct {
	mu       sync.Mutex
	mintBody string
	paths    []string // non-mint, non-revoke request URIs (escaped)
	revoked  []string
	handler  func(r *http.Request) *http.Response
	onGet    func()
}

func (f *commitFake) provider(t *testing.T) *HTTPProvider {
	return qaProvider(t, roundTrip(func(r *http.Request) (*http.Response, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens"):
			b, _ := io.ReadAll(r.Body)
			f.mintBody = string(b)
			return response(http.StatusCreated, fmt.Sprintf(`{"token":%q,"expires_at":%q,"permissions":{"contents":"read"},"repository_selection":"selected","repositories":[{"id":42}]}`, commitTestToken, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))), nil
		case r.Method == http.MethodDelete && r.URL.Path == "/installation/token":
			f.revoked = append(f.revoked, r.Header.Get("Authorization"))
			return response(http.StatusNoContent, ""), nil
		}
		if r.Header.Get("Authorization") != "Bearer "+commitTestToken {
			return response(http.StatusUnauthorized, ""), nil
		}
		f.paths = append(f.paths, r.URL.EscapedPath())
		if f.onGet != nil {
			f.onGet()
		}
		return f.handler(r), nil
	}))
}

func (f *commitFake) assertRevokedOnce(t *testing.T) {
	t.Helper()
	if len(f.revoked) != 1 || f.revoked[0] != "Bearer "+commitTestToken {
		t.Fatalf("revocations = %v, want exactly one for the minted token", f.revoked)
	}
}

func noSecret(t *testing.T, err error) {
	t.Helper()
	if err != nil && (strings.Contains(err.Error(), commitTestToken) || strings.Contains(err.Error(), "message")) {
		t.Fatalf("error leaks secret or body: %v", err)
	}
}

func jsonResp(status int, body string) *http.Response { return response(status, body) }

func TestResolveBranchHeadSuccessNarrowsTokenAndRevokes(t *testing.T) {
	f := &commitFake{handler: func(*http.Request) *http.Response {
		return jsonResp(200, `{"object":{"type":"commit","sha":"`+sha40+`"}}`)
	}}
	got, err := f.provider(t).ResolveBranchHead(context.Background(), 7, 42, "refs/heads/feature/a#b%")
	if err != nil || got != sha40 {
		t.Fatalf("got %q, %v", got, err)
	}
	if f.mintBody != `{"permissions":{"contents":"read"},"repository_ids":[42]}` {
		t.Fatalf("mint body = %s", f.mintBody)
	}
	if len(f.paths) != 1 || f.paths[0] != "/repositories/42/git/ref/heads/feature/a%23b%25" {
		t.Fatalf("paths = %v", f.paths)
	}
	f.assertRevokedOnce(t)
}

func TestResolveBranchHeadErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		header http.Header
		want   error
	}{
		{"404", 404, nil, ErrRefNotFound},
		{"403", 403, nil, ErrRepositoryUnauthorized},
		{"401", 401, nil, ErrRepositoryUnauthorized},
		{"500", 500, nil, ErrGitHubUnavailable},
		{"429", 429, nil, ErrGitHubUnavailable},
		{"403 rate", 403, http.Header{"X-Ratelimit-Remaining": {"0"}}, ErrGitHubUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &commitFake{handler: func(*http.Request) *http.Response {
				r := jsonResp(tc.status, `{"message":"body-secret"}`)
				for k, v := range tc.header {
					r.Header[k] = v
				}
				return r
			}}
			_, err := f.provider(t).ResolveBranchHead(context.Background(), 7, 42, "refs/heads/main")
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if tc.want == ErrGitHubUnavailable && tc.status == 429 && !errors.Is(err, ErrRateLimited) {
				t.Fatalf("rate limit not preserved: %v", err)
			}
			noSecret(t, err)
			if strings.Contains(fmt.Sprint(err), "body-secret") {
				t.Fatal("body leaked")
			}
			f.assertRevokedOnce(t)
		})
	}
}

func TestResolveBranchHeadRevokesOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &commitFake{handler: func(*http.Request) *http.Response { return jsonResp(200, `{}`) }}
	f.onGet = cancel
	p := f.provider(t)
	inner := p.client.Transport
	p.client.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		resp, err := inner.RoundTrip(r)
		if r.Method == http.MethodGet && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return resp, err
	})
	_, err := p.ResolveBranchHead(ctx, 7, 42, "refs/heads/main")
	if !errors.Is(err, ErrGitHubUnavailable) {
		t.Fatalf("err = %v", err)
	}
	f.assertRevokedOnce(t)
}

func TestResolveBranchHeadRejectsBadObjects(t *testing.T) {
	for name, body := range map[string]string{
		"tag object":    `{"object":{"type":"tag","sha":"` + sha40 + `"}}`,
		"short sha":     `{"object":{"type":"commit","sha":"abc123"}}`,
		"uppercase sha": `{"object":{"type":"commit","sha":"` + strings.ToUpper(sha40) + `"}}`,
		"empty":         `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := &commitFake{handler: func(*http.Request) *http.Response { return jsonResp(200, body) }}
			_, err := f.provider(t).ResolveBranchHead(context.Background(), 7, 42, "refs/heads/main")
			if !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("err = %v", err)
			}
			f.assertRevokedOnce(t)
		})
	}
}

func TestResolveBranchHeadRejectsBadRefWithoutHTTP(t *testing.T) {
	for _, ref := range []string{"main", "refs/tags/v1", "refs/heads/", "refs/heads/a/../b", "refs/heads/a//b", "heads/main"} {
		f := &commitFake{handler: func(*http.Request) *http.Response { t.Fatal("unexpected request"); return nil }}
		_, err := f.provider(t).ResolveBranchHead(context.Background(), 7, 42, ref)
		if !errors.Is(err, ErrInvalidRef) || f.mintBody != "" {
			t.Fatalf("ref %q err = %v", ref, err)
		}
	}
}

func compareFake(commitStatus, cmpStatus int, cmpBody string) *commitFake {
	return &commitFake{handler: func(r *http.Request) *http.Response {
		if strings.Contains(r.URL.Path, "/commits/") {
			if commitStatus != 200 {
				return jsonResp(commitStatus, `{"message":"body-secret"}`)
			}
			return jsonResp(200, `{"sha":"`+sha40+`"}`)
		}
		return jsonResp(cmpStatus, cmpBody)
	}}
}

func TestVerifyCommitOnRefBadSHAMakesNoCall(t *testing.T) {
	for _, sha := range []string{"", "abc", strings.ToUpper(sha40), strings.Repeat("0", 40), sha40 + "0", "../" + sha40[:37]} {
		f := &commitFake{handler: func(*http.Request) *http.Response { t.Fatal("unexpected request"); return nil }}
		err := f.provider(t).VerifyCommitOnRef(context.Background(), 7, 42, "refs/heads/main", sha)
		if !errors.Is(err, ErrInvalidCommitSHA) || f.mintBody != "" {
			t.Fatalf("sha %q err = %v", sha, err)
		}
	}
}

func TestVerifyCommitOnRefReachability(t *testing.T) {
	for status, want := range map[string]error{"identical": nil, "ahead": nil, "behind": ErrCommitNotOnRef, "diverged": ErrCommitNotOnRef, "weird": ErrInvalidResponse} {
		t.Run(status, func(t *testing.T) {
			f := compareFake(200, 200, `{"status":"`+status+`"}`)
			err := f.provider(t).VerifyCommitOnRef(context.Background(), 7, 42, "refs/heads/rel/1.0", sha40)
			if want == nil && err != nil || want != nil && !errors.Is(err, want) {
				t.Fatalf("err = %v, want %v", err, want)
			}
			if f.mintBody != `{"permissions":{"contents":"read"},"repository_ids":[42]}` {
				t.Fatalf("mint body = %s", f.mintBody)
			}
			if len(f.paths) != 2 || f.paths[0] != "/repositories/42/commits/"+sha40 || f.paths[1] != "/repositories/42/compare/"+sha40+"...rel/1.0" {
				t.Fatalf("paths = %v", f.paths)
			}
			f.assertRevokedOnce(t)
		})
	}
}

func TestVerifyCommitOnRefErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		commitStatus, cmpCode int
		want                  error
	}{
		{"commit 404", 404, 200, ErrCommitNotFound},
		{"commit 422", 422, 200, ErrCommitNotFound},
		{"commit 403", 403, 200, ErrRepositoryUnauthorized},
		{"commit 500", 500, 200, ErrGitHubUnavailable},
		{"commit 429", 429, 200, ErrGitHubUnavailable},
		{"compare 404", 200, 404, ErrRefNotFound},
		{"compare 403", 200, 403, ErrRepositoryUnauthorized},
		{"compare 500", 200, 500, ErrGitHubUnavailable},
		{"compare 422", 200, 422, ErrCommitNotOnRef},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := compareFake(tc.commitStatus, tc.cmpCode, `{"message":"body-secret"}`)
			err := f.provider(t).VerifyCommitOnRef(context.Background(), 7, 42, "refs/heads/main", sha40)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			noSecret(t, err)
			if strings.Contains(err.Error(), "body-secret") {
				t.Fatal("body leaked")
			}
			f.assertRevokedOnce(t)
		})
	}
}

func TestVerifyCommitOnRefRejectsMismatchedCommitSHA(t *testing.T) {
	f := &commitFake{handler: func(*http.Request) *http.Response {
		return jsonResp(200, `{"sha":"`+strings.Repeat("a", 40)+`"}`)
	}}
	err := f.provider(t).VerifyCommitOnRef(context.Background(), 7, 42, "refs/heads/main", sha40)
	if !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("err = %v", err)
	}
	f.assertRevokedOnce(t)
}

func TestMintFailureMapsToUnauthorizedAndUnavailable(t *testing.T) {
	for status, want := range map[int]error{403: ErrRepositoryUnauthorized, 404: ErrRepositoryUnauthorized, 500: ErrGitHubUnavailable} {
		p := qaProvider(t, roundTrip(func(*http.Request) (*http.Response, error) { return response(status, `{"message":"x"}`), nil }))
		_, err := p.ResolveBranchHead(context.Background(), 7, 42, "refs/heads/main")
		if !errors.Is(err, want) {
			t.Fatalf("status %d err = %v", status, err)
		}
	}
}
