package githubapp

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSignerProducesCanonicalRS256JWT(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 12, 1, 2, 3, 0, time.UTC)
	s, err := NewSigner(key, "Iv1.client", func() time.Time { return now }, bytes.NewReader(bytes.Repeat([]byte{1}, 512)))
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.Token()
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatal("parts")
	}
	var header map[string]string
	var claims map[string]any
	decode(t, parts[0], &header)
	decode(t, parts[1], &claims)
	if header["alg"] != "RS256" || header["typ"] != "JWT" || claims["iss"] != "Iv1.client" || claims["iat"] != float64(now.Add(-time.Minute).Unix()) || claims["exp"] != float64(now.Add(9*time.Minute).Unix()) {
		t.Fatalf("header=%v claims=%v", header, claims)
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := crypto.SHA256.New()
	_, _ = sum.Write([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum.Sum(nil), sig); err != nil {
		t.Fatal(err)
	}
}

func TestSignerOwnsCopiedRSAKeyAndSupportsConcurrentTokens(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	public := rsa.PublicKey{N: new(big.Int).Set(key.N), E: key.E}
	signer, err := NewSigner(key, "client", time.Now, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key.N.SetInt64(0)
	key.D.SetInt64(0)
	for _, prime := range key.Primes {
		prime.SetInt64(0)
	}
	var group sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		group.Add(1)
		go func() {
			defer group.Done()
			token, tokenErr := signer.Token()
			if tokenErr != nil {
				errs <- tokenErr
				return
			}
			parts := strings.Split(token, ".")
			signature, _ := base64.RawURLEncoding.DecodeString(parts[2])
			digest := crypto.SHA256.New()
			_, _ = digest.Write([]byte(parts[0] + "." + parts[1]))
			errs <- rsa.VerifyPKCS1v15(&public, crypto.SHA256, digest.Sum(nil), signature)
		}()
	}
	group.Wait()
	close(errs)
	for signErr := range errs {
		if signErr != nil {
			t.Fatal(signErr)
		}
	}
}
func TestHTTPProviderUsesFixedSafeContract(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	signer, _ := NewSigner(key, "client", time.Now, rand.Reader)
	p, _ := NewHTTPProvider(signer, "autodeploy-test")
	p.client.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != APIOrigin+"/app" || r.Method != "GET" || r.Header.Get("Accept") != "application/vnd.github+json" || r.Header.Get("X-GitHub-Api-Version") != APIVersion || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || r.Header.Get("User-Agent") != "autodeploy-test" {
			t.Fatalf("request %#v", r)
		}
		return response(http.StatusOK, `{"id":1,"client_id":"client","slug":"app","owner":{"id":2,"type":"User"},"permissions":{"contents":"read"},"events":["push"]}`), nil
	})
	app, err := p.GetApp(context.Background())
	if err != nil || app.ID != 1 || !exactStrings(app.Events, []string{"push"}) {
		t.Fatalf("app=%#v err=%v", app, err)
	}
}
func TestProviderErrorsAreSanitizedAndClassified(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	signer, _ := NewSigner(key, "client", time.Now, rand.Reader)
	p, _ := NewHTTPProvider(signer, "agent")
	p.client.Transport = roundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"7"}, "X-GitHub-Request-Id": []string{"safe-id"}}, Body: io.NopCloser(strings.NewReader("sentinel-token"))}, nil
	})
	_, err := p.GetApp(context.Background())
	var got *ProviderError
	if !errors.As(err, &got) || !errors.Is(err, ErrRateLimited) || got.RetryAfter != 7*time.Second || strings.Contains(err.Error(), "sentinel") {
		t.Fatalf("err=%v got=%#v", err, got)
	}
}

func TestHTTPProviderRetriesOnlyIdempotentTransientGETs(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	signer, _ := NewSigner(key, "client", time.Now, rand.Reader)
	provider, _ := NewHTTPProvider(signer, "agent")
	calls := 0
	provider.client.Transport = roundTrip(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return response(http.StatusBadGateway, ""), nil
		}
		return response(http.StatusOK, `{"id":1,"client_id":"client","slug":"app","owner":{"id":2,"type":"User"},"permissions":{"contents":"read","metadata":"read"},"events":["push"]}`), nil
	})
	if _, err := provider.GetApp(context.Background()); err != nil || calls != 2 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
	calls = 0
	provider.client.Transport = roundTrip(func(*http.Request) (*http.Response, error) { calls++; return response(http.StatusBadGateway, ""), nil })
	if _, err := provider.MintCheckoutToken(context.Background(), 7, []int64{42}); err == nil || calls != 1 {
		t.Fatalf("POST calls=%d err=%v", calls, err)
	}
}

func TestHTTPProviderRejectsRedirectResponse(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	signer, _ := NewSigner(key, "client", time.Now, rand.Reader)
	provider, _ := NewHTTPProvider(signer, "agent")
	provider.client.Transport = roundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://elsewhere.invalid"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	if _, err := provider.GetApp(context.Background()); err == nil {
		t.Fatal("redirect accepted")
	}
}

func TestInvalidMintResponseIsRevokedExactlyOnce(t *testing.T) {
	for _, test := range []struct{ name, body string }{
		{"expired", `"expires_at":"2026-01-01T00:00:00Z","permissions":{"contents":"read"},"repository_selection":"selected","repositories":[{"id":42}]`},
		{"extra permission", `"expires_at":"2026-08-12T01:00:00Z","permissions":{"contents":"read","issues":"write"},"repository_selection":"selected","repositories":[{"id":42}]`},
		{"wrong repository", `"expires_at":"2026-08-12T01:00:00Z","permissions":{"contents":"read"},"repository_selection":"selected","repositories":[{"id":99}]`},
		{"wrong selection", `"expires_at":"2026-08-12T01:00:00Z","permissions":{"contents":"read"},"repository_selection":"all","repositories":[{"id":42}]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := fixedProvider(t, time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC))
			calls := 0
			provider.client.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method == http.MethodDelete {
					return response(http.StatusNoContent, ""), nil
				}
				return response(http.StatusCreated, `{"token":"sentinel-mint-token",`+test.body+`}`), nil
			})
			_, err := provider.MintCheckoutToken(context.Background(), 7, []int64{42})
			if !errors.Is(err, ErrInvalidResponse) || calls != 2 || strings.Contains(err.Error(), "sentinel-mint-token") {
				t.Fatalf("err=%v calls=%d", err, calls)
			}
		})
	}
}

func TestInvalidMintResponseJoinsRevocationFailure(t *testing.T) {
	provider := fixedProvider(t, time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC))
	calls := 0
	provider.client.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method == http.MethodDelete {
			return response(http.StatusInternalServerError, "sentinel"), nil
		}
		return response(http.StatusCreated, `{"token":"sentinel-mint-token","expires_at":"2026-01-01T00:00:00Z","permissions":{"contents":"read"},"repository_selection":"selected","repositories":[{"id":42}]}`), nil
	})
	_, err := provider.MintCheckoutToken(context.Background(), 7, []int64{42})
	if !errors.Is(err, ErrInvalidResponse) || !errors.Is(err, ErrRevocationRequired) || calls != 2 || strings.Contains(err.Error(), "sentinel-mint-token") {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestListingRevocationFailureKeepsClassifications(t *testing.T) {
	for _, failedList := range []bool{false, true} {
		t.Run("list", func(t *testing.T) {
			provider := fixedProvider(t, time.Now())
			calls := 0
			provider.client.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				switch r.Method {
				case http.MethodPost:
					return response(http.StatusCreated, discoveryToken(time.Now().Add(time.Hour))), nil
				case http.MethodDelete:
					return response(http.StatusInternalServerError, ""), nil
				default:
					if failedList {
						return response(http.StatusInternalServerError, ""), nil
					}
					return response(http.StatusOK, `{"total_count":0,"repositories":[]}`), nil
				}
			})
			_, err := provider.ListInstallationRepositories(context.Background(), 7)
			wantCalls := 3
			if !errors.Is(err, ErrRevocationRequired) || calls != wantCalls {
				t.Fatalf("err=%v calls=%d", err, calls)
			}
			if failedList && !errors.Is(err, ErrProvider) {
				t.Fatalf("primary missing: %v", err)
			}
		})
	}
}

func TestInstallationTargetTypeMustMatchOwner(t *testing.T) {
	provider := fixedProvider(t, time.Now())
	provider.client.Transport = roundTrip(func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, `{"id":7,"account":{"id":2,"type":"Organization"},"target_type":"User","repository_selection":"all"}`), nil
	})
	if _, err := provider.GetInstallation(context.Background(), 7); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("err=%v", err)
	}
}

func fixedProvider(t *testing.T, now time.Time) *HTTPProvider {
	t.Helper()
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	signer, _ := NewSigner(key, "client", func() time.Time { return now }, rand.Reader)
	provider, _ := NewHTTPProvider(signer, "agent")
	provider.now = func() time.Time { return now }
	return provider
}
func discoveryToken(expires time.Time) string {
	return `{"token":"discovery-token","expires_at":"` + expires.UTC().Format(time.RFC3339) + `","permissions":{"metadata":"read"},"repository_selection":"all"}`
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}
func decode(t *testing.T, s string, out any) {
	t.Helper()
	v, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(v, out); err != nil {
		t.Fatal(err)
	}
}
