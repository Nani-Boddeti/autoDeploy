package githubapp

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type checkoutStoreStub struct {
	binding CheckoutBinding
	capture error
	recheck error
	closed  int
}

func (s *checkoutStoreStub) CaptureCheckoutBinding(context.Context, string, string) (CheckoutBinding, error) {
	return s.binding, s.capture
}
func (s *checkoutStoreStub) RevalidateCheckoutBinding(context.Context, CheckoutBinding) error {
	return s.recheck
}
func (s *checkoutStoreStub) ReserveCheckout(context.Context, string, string, string) (CheckoutReservation, error) {
	return CheckoutReservation{}, s.capture
}
func (s *checkoutStoreStub) BeginMintAttempt(context.Context, string) (CheckoutBinding, error) {
	return s.binding, s.capture
}
func (s *checkoutStoreStub) ActivateMintAttempt(context.Context, string, CheckoutBinding) error {
	return s.recheck
}
func (s *checkoutStoreStub) StartCheckout(context.Context, string, CheckoutBinding) error {
	return s.recheck
}
func (s *checkoutStoreStub) CloseCheckout(context.Context, string) error { s.closed++; return nil }

type checkoutProviderStub struct {
	token              Token
	mintErr, revokeErr error
	revoked            []string
}

func (*checkoutProviderStub) GetApp(context.Context) (App, error) {
	return App{}, errors.New("unexpected")
}
func (*checkoutProviderStub) GetInstallation(context.Context, int64) (Installation, error) {
	return Installation{}, errors.New("unexpected")
}
func (*checkoutProviderStub) ListInstallationRepositories(context.Context, int64) ([]Repository, error) {
	return nil, errors.New("unexpected")
}
func (*checkoutProviderStub) MintDiscoveryToken(context.Context, int64) (Token, error) {
	return Token{}, errors.New("unexpected")
}
func (s *checkoutProviderStub) MintCheckoutToken(context.Context, int64, []int64) (Token, error) {
	return s.token, s.mintErr
}
func (s *checkoutProviderStub) RevokeToken(_ context.Context, token string) error {
	s.revoked = append(s.revoked, token)
	return s.revokeErr
}

func TestCheckoutAuthorizesExactTokenAndDeliversPipeOnce(t *testing.T) {
	secret := "checkout-test-sentinel"
	provider := &checkoutProviderStub{token: Token{Value: secret, ExpiresAt: time.Now().Add(time.Minute), RepositoryIDs: []int64{2}, Contents: "read", Permissions: map[string]string{"contents": "read"}}}
	store := &checkoutStoreStub{binding: CheckoutBinding{OwnerID: "owner", ProjectID: "project", InstallationID: 1, RepositoryID: 2, AuthorizationRevision: 3, CloneURL: "https://github.com/owner/repo.git", CommitSHA: "0123456789abcdef0123456789abcdef01234567", ReservationID: "reservation"}}
	s, err := NewCheckoutService(store, provider)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := s.Activate(context.Background(), "reservation")
	if err != nil {
		t.Fatal(err)
	}
	b, err := lease.NewAskPassBroker()
	if err != nil {
		t.Fatal(err)
	}
	reader := b.Reader()
	if reader == nil {
		t.Fatal("missing pipe")
	}
	buf := make([]byte, 128)
	n, err := reader.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(buf[:n]); got != secret+"\n" {
		t.Fatalf("pipe = %q", got)
	}
	if _, err := lease.NewAskPassBroker(); !errors.Is(err, ErrCheckoutUnavailable) {
		t.Fatalf("second delivery: %v", err)
	}
	if err := lease.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(provider.revoked) != 1 || provider.revoked[0] != secret {
		t.Fatal("token was not revoked")
	}
}

func TestCheckoutStaleRevisionRevokesAndDoesNotExposeToken(t *testing.T) {
	secret := "checkout-test-sentinel"
	provider := &checkoutProviderStub{token: Token{Value: secret, ExpiresAt: time.Now().Add(time.Minute), RepositoryIDs: []int64{2}, Contents: "read", Permissions: map[string]string{"contents": "read"}}}
	store := &checkoutStoreStub{binding: CheckoutBinding{OwnerID: "owner", ProjectID: "project", InstallationID: 1, RepositoryID: 2, AuthorizationRevision: 3, CloneURL: "https://github.com/owner/repo.git", CommitSHA: "0123456789abcdef0123456789abcdef01234567", ReservationID: "reservation"}, recheck: errors.New("changed")}
	s, _ := NewCheckoutService(store, provider)
	_, err := s.Activate(context.Background(), "reservation")
	if !errors.Is(err, ErrCheckoutStale) || strings.Contains(err.Error(), secret) {
		t.Fatalf("err = %v", err)
	}
	if len(provider.revoked) != 1 || provider.revoked[0] != secret {
		t.Fatal("stale token not revoked")
	}
}

func TestCheckoutRejectsWrongScopeAndRevokes(t *testing.T) {
	provider := &checkoutProviderStub{token: Token{Value: "sentinel", ExpiresAt: time.Now().Add(time.Minute), RepositoryIDs: []int64{2}, Contents: "read", Permissions: map[string]string{"contents": "write"}}}
	store := &checkoutStoreStub{binding: CheckoutBinding{OwnerID: "owner", ProjectID: "project", InstallationID: 1, RepositoryID: 2, AuthorizationRevision: 3, CloneURL: "https://github.com/owner/repo.git", CommitSHA: "0123456789abcdef0123456789abcdef01234567", ReservationID: "reservation"}}
	s, _ := NewCheckoutService(store, provider)
	if _, err := s.Activate(context.Background(), "reservation"); !errors.Is(err, ErrCheckoutUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if len(provider.revoked) != 1 {
		t.Fatal("invalid token not revoked")
	}
}
