package githubapp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func qaProvider(t *testing.T, transport roundTrip) *HTTPProvider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := NewSigner(key, "Iv1.qa", time.Now, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := NewHTTPProvider(signer, "autodeploy-qa")
	if err != nil {
		t.Fatal(err)
	}
	provider.client.Transport = transport
	return provider
}

func TestQADiscoveryTokenRequestsMetadataOnly(t *testing.T) {
	provider := qaProvider(t, roundTrip(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := string(body), `{"permissions":{"metadata":"read"}}`; got != want {
			t.Fatalf("discovery token body = %s, want %s", got, want)
		}
		return response(http.StatusCreated, fmt.Sprintf(`{"token":"opaque discovery value","expires_at":%q,"permissions":{"metadata":"read"},"repository_selection":"all"}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))), nil
	}))
	if _, err := provider.MintDiscoveryToken(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
}

func TestQACheckoutTokenRejectsMismatchedResponseRepository(t *testing.T) {
	provider := qaProvider(t, roundTrip(func(*http.Request) (*http.Response, error) {
		return response(http.StatusCreated, fmt.Sprintf(`{"token":"opaque checkout value","expires_at":%q,"permissions":{"contents":"read"},"repository_selection":"selected","repositories":[{"id":99}]}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))), nil
	}))
	if _, err := provider.MintCheckoutToken(context.Background(), 7, []int64{42}); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("mismatched repository scope error = %v, want ErrInvalidResponse", err)
	}
}

func TestQATokenRejectsExpiredResponse(t *testing.T) {
	provider := qaProvider(t, roundTrip(func(*http.Request) (*http.Response, error) {
		return response(http.StatusCreated, fmt.Sprintf(`{"token":"opaque checkout value","expires_at":%q,"permissions":{"contents":"read"},"repository_selection":"selected","repositories":[{"id":42}]}`, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339))), nil
	}))
	if _, err := provider.MintCheckoutToken(context.Background(), 7, []int64{42}); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("expired token error = %v, want ErrInvalidResponse", err)
	}
}

func TestQAAppVerificationRejectsAdditionalPermissionsAndEvents(t *testing.T) {
	provider := qaProvider(t, roundTrip(func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, `{"id":1,"client_id":"Iv1.qa","slug":"autodeploy","owner":{"id":2,"type":"Organization"},"permissions":{"metadata":"read","contents":"read","issues":"write"},"events":["push","pull_request"]}`), nil
	}))
	app, err := provider.GetApp(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyApp(app, AppIdentity{AppID: 1, ClientID: "Iv1.qa", Slug: "autodeploy"}, Owner{AccountID: 2, Type: "Organization"}); !errors.Is(err, ErrInvalidIdentity) {
		t.Fatalf("extra App access error = %v, want ErrInvalidIdentity", err)
	}
}

func TestQAInstallationResponseMustMatchRequestedIdentity(t *testing.T) {
	provider := qaProvider(t, roundTrip(func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, `{"id":8,"account":{"id":2,"type":"Organization"},"suspended_at":null}`), nil
	}))
	if _, err := provider.GetInstallation(context.Background(), 7); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("mismatched installation identity error = %v, want ErrInvalidResponse", err)
	}
}

func TestQAMintRequiresDocumentedCreatedStatus(t *testing.T) {
	provider := qaProvider(t, roundTrip(func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, fmt.Sprintf(`{"token":"opaque checkout value","expires_at":%q,"permissions":{"contents":"read"},"repository_selection":"selected","repositories":[{"id":42}]}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))), nil
	}))
	if _, err := provider.MintCheckoutToken(context.Background(), 7, []int64{42}); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("unexpected mint status error = %v, want ErrInvalidResponse", err)
	}
}

func TestQASecondaryRateLimit403WithRetryAfterIsClassified(t *testing.T) {
	provider := qaProvider(t, roundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{"Retry-After": {"9"}}, Body: io.NopCloser(strings.NewReader(`{"message":"secondary rate limit"}`))}, nil
	}))
	_, err := provider.GetApp(context.Background())
	var providerError *ProviderError
	if !errors.As(err, &providerError) || !errors.Is(err, ErrRateLimited) || providerError.RetryAfter != 9*time.Second {
		t.Fatalf("secondary rate-limit error = %#v (%v)", providerError, err)
	}
}

func TestQAPaginationAcceptsDocumentedMaximum(t *testing.T) {
	call := 0
	provider := qaProvider(t, roundTrip(func(request *http.Request) (*http.Response, error) {
		call++
		if call == 1 {
			return response(http.StatusCreated, fmt.Sprintf(`{"token":"discovery","expires_at":%q,"permissions":{"metadata":"read"},"repository_selection":"all"}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))), nil
		}
		if request.Method == http.MethodDelete {
			return response(http.StatusNoContent, ""), nil
		}
		page := call - 1
		repositories := make([]Repository, 100)
		for index := range repositories {
			id := int64((page-1)*100 + index + 1)
			repositories[index] = Repository{ID: id, Name: fmt.Sprintf("r%d", id), FullName: fmt.Sprintf("owner/r%d", id)}
		}
		body, err := json.Marshal(struct {
			TotalCount   int          `json:"total_count"`
			Repositories []Repository `json:"repositories"`
		}{TotalCount: 1000, Repositories: repositories})
		if err != nil {
			t.Fatal(err)
		}
		return response(http.StatusOK, string(body)), nil
	}))
	repositories, err := provider.ListInstallationRepositories(context.Background(), 7)
	if err != nil || len(repositories) != 1000 {
		t.Fatalf("repositories = %d, error = %v; want 1000", len(repositories), err)
	}
}
