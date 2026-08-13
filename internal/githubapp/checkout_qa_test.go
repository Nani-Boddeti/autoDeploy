package githubapp

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func validQACheckoutToken(value string) Token {
	return Token{
		Value:         value,
		ExpiresAt:     time.Now().Add(time.Minute),
		RepositoryIDs: []int64{2},
		Contents:      "read",
		Permissions:   map[string]string{"contents": "read"},
	}
}

func validQACheckoutStore() *checkoutStoreStub {
	return &checkoutStoreStub{binding: CheckoutBinding{
		OwnerID:               "owner",
		ProjectID:             "project",
		InstallationID:        1,
		RepositoryID:          2,
		AuthorizationRevision: 3,
		CloneURL:              "https://github.com/owner/repo.git",
		CommitSHA:             "0123456789abcdef0123456789abcdef01234567",
		ReservationID:         "reservation",
	}}
}

func TestQACheckoutInvalidMintSurfacesRequiredRevocation(t *testing.T) {
	provider := &checkoutProviderStub{
		token: Token{
			Value:         "qa-invalid-token-sentinel",
			ExpiresAt:     time.Now().Add(time.Minute),
			RepositoryIDs: []int64{99},
			Contents:      "read",
			Permissions:   map[string]string{"contents": "read"},
		},
		revokeErr: errors.New("provider revoke failed"),
	}
	service, err := NewCheckoutService(validQACheckoutStore(), provider)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Activate(context.Background(), "reservation")
	if !errors.Is(err, ErrRevocationRequired) {
		t.Fatalf("invalid minted credential with failed revocation must surface ErrRevocationRequired: %v", err)
	}
}

func TestQAActivationFailureAlwaysClosesDurableReservation(t *testing.T) {
	for _, tc := range []struct {
		name        string
		invalid     bool
		activateErr error
	}{
		{name: "invalid mint and revoke failure", invalid: true},
		{name: "stale activation and revoke failure", activateErr: errors.New("stale")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := validQACheckoutStore()
			store.recheck = tc.activateErr
			token := validQACheckoutToken("qa-close-token-sentinel")
			if tc.invalid {
				token.RepositoryIDs = []int64{99}
			}
			provider := &checkoutProviderStub{token: token, revokeErr: errors.New("provider revoke failed")}
			service, err := NewCheckoutService(store, provider)
			if err != nil {
				t.Fatal(err)
			}
			_, err = service.Activate(context.Background(), "reservation")
			if !errors.Is(err, ErrRevocationRequired) {
				t.Fatalf("error = %v", err)
			}
			if store.closed != 1 {
				t.Fatalf("durable reservation close calls=%d", store.closed)
			}
		})
	}
}

func TestQACheckoutLeaseConcurrentConsumeAndRevoke(t *testing.T) {
	provider := &checkoutProviderStub{token: validQACheckoutToken("qa-concurrent-token-sentinel")}
	service, err := NewCheckoutService(validQACheckoutStore(), provider)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := service.Activate(context.Background(), "reservation")
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		broker, brokerErr := lease.NewAskPassBroker()
		if brokerErr == nil {
			_ = broker.Close()
		}
	}()
	go func() {
		defer wg.Done()
		_ = lease.Revoke(context.Background())
	}()
	wg.Wait()
}
