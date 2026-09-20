package githubapp

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"autodeploy/internal/crypto"
)

var (
	ErrCheckoutUnavailable = errors.New("GitHub checkout authorization unavailable")
	ErrCheckoutStale       = errors.New("GitHub checkout authorization changed")
)

// CheckoutBinding is the immutable authorization snapshot captured before a
// checkout token is minted. Numeric GitHub identifiers are the authority.
type CheckoutBinding struct {
	OwnerID              string
	ProjectID            string
	InstallationID       int64
	RepositoryID         int64
	BindingRevision      int64
	RepositoryRevision   int64
	InstallationRevision int64
	CloneURL             string
	CommitSHA            string
	ReservationID        string
	// AuthorizationRevision is retained for source compatibility with callers
	// compiled before the full authority snapshot. New stores populate all three.
	AuthorizationRevision int64
}

func (b CheckoutBinding) CloneURLValid() bool {
	return strings.HasPrefix(b.CloneURL, "https://github.com/") && strings.HasSuffix(b.CloneURL, ".git") && len(b.CloneURL) <= 1024
}

func (b CheckoutBinding) valid() bool {
	legacy := b.AuthorizationRevision > 0 && b.BindingRevision == 0 && b.RepositoryRevision == 0 && b.InstallationRevision == 0
	return b.OwnerID != "" && len(b.OwnerID) <= 128 && b.ProjectID != "" && len(b.ProjectID) <= 128 && b.InstallationID > 0 && b.RepositoryID > 0 && (legacy || (b.BindingRevision > 0 && b.RepositoryRevision > 0 && b.InstallationRevision > 0))
}

type CheckoutStore interface {
	CaptureCheckoutBinding(context.Context, string, string) (CheckoutBinding, error)
	RevalidateCheckoutBinding(context.Context, CheckoutBinding) error
}

type CheckoutReservation struct {
	CheckoutBinding
	ID        string
	ExpiresAt time.Time
	Attempts  int16
	State     string
}
type ReservationStore interface {
	ReserveCheckout(context.Context, string, string, string) (CheckoutReservation, error)
	BeginMintAttempt(context.Context, string) (CheckoutBinding, error)
	ActivateMintAttempt(context.Context, string, CheckoutBinding) error
	StartCheckout(context.Context, string, CheckoutBinding) error
	CloseCheckout(context.Context, string) error
}

// Reserve admits a credential-free exact-commit reservation. Minting is a
// separate action so provider credentials never enter durable state.
func Reserve(ctx context.Context, store ReservationStore, owner, project, sha string) (CheckoutReservation, error) {
	if store == nil || len(sha) != 40 || strings.ToLower(sha) != sha || !isLowerHex(sha) {
		return CheckoutReservation{}, ErrCheckoutUnavailable
	}
	r, err := store.ReserveCheckout(ctx, owner, project, sha)
	if err != nil {
		return CheckoutReservation{}, ErrCheckoutUnavailable
	}
	return r, nil
}
func isLowerHex(v string) bool {
	for _, c := range v {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// CheckoutLease owns one minted token. It must be revoked by the caller on
// every path; the token is deliberately neither serializable nor cacheable.
type CheckoutLease struct {
	mu          sync.Mutex
	provider    Provider
	store       ReservationStore
	token       string
	binding     CheckoutBinding
	expires     time.Time
	delivered   bool
	invalidated bool
	cancel      context.CancelFunc
}

func (l *CheckoutLease) Binding() CheckoutBinding { return l.binding }
func (l *CheckoutLease) ExpiresAt() time.Time     { return l.expires }

// Revoke is idempotent locally. A provider revocation failure is surfaced so
// callers fail closed instead of silently continuing with an exposed token.
func (l *CheckoutLease) Revoke(ctx context.Context) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.token == "" {
		return nil
	}
	if err := l.provider.RevokeToken(context.WithoutCancel(ctx), l.token); err != nil {
		return err
	}
	l.token = ""
	return nil
}

// Start atomically changes the durable lease to running immediately before
// credential delivery. A concurrent authority disable wins by invalidating it.
func (l *CheckoutLease) Start(ctx context.Context) error {
	if l == nil || l.store == nil || l.binding.ReservationID == "" {
		return ErrCheckoutUnavailable
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.invalidated || l.token == "" || l.delivered {
		return ErrCheckoutStale
	}
	if err := l.store.StartCheckout(ctx, l.binding.ReservationID, l.binding); err != nil {
		return ErrCheckoutStale
	}
	return nil
}

// SetCancel installs the in-process cancellation hook used while Git runs.
func (l *CheckoutLease) SetCancel(cancel context.CancelFunc) {
	l.mu.Lock()
	l.cancel = cancel
	l.mu.Unlock()
}

// InvalidateCheckoutReservation is called after a durable disable/reconcile
// commit. It only reaches same-process leases; cross-process result acceptance
// remains prevented by CloseCheckout's invalidated-state check.
func InvalidateCheckoutReservation(id string) { activeLeases.invalidate(id) }

type leaseRegistry struct {
	mu     sync.Mutex
	leases map[string]*CheckoutLease
}

var activeLeases = leaseRegistry{leases: make(map[string]*CheckoutLease)}

func (r *leaseRegistry) add(l *CheckoutLease) {
	r.mu.Lock()
	r.leases[l.binding.ReservationID] = l
	r.mu.Unlock()
}
func (r *leaseRegistry) remove(id string) { r.mu.Lock(); delete(r.leases, id); r.mu.Unlock() }
func (r *leaseRegistry) invalidate(id string) {
	r.mu.Lock()
	l := r.leases[id]
	r.mu.Unlock()
	if l != nil {
		l.invalidate()
	}
}
func (l *CheckoutLease) invalidate() {
	l.mu.Lock()
	l.invalidated = true
	cancel := l.cancel
	l.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	go func() { _ = l.Revoke(context.Background()) }()
}

// Close consumes the durable reservation and revokes its ephemeral token.
// Both actions are attempted even when the other fails; callers must treat a
// joined ErrRevocationRequired as a failed checkout boundary.
func (l *CheckoutLease) Close(ctx context.Context) error {
	if l == nil {
		return ErrCheckoutUnavailable
	}
	var errs []error
	defer activeLeases.remove(l.binding.ReservationID)
	if l.store == nil || l.binding.ReservationID == "" {
		errs = append(errs, ErrCheckoutUnavailable)
	} else if err := withCleanupContext(ctx, func(cleanupCtx context.Context) error {
		return l.store.CloseCheckout(cleanupCtx, l.binding.ReservationID)
	}); err != nil {
		errs = append(errs, ErrCheckoutStale)
	}
	if err := withCleanupContext(ctx, l.Revoke); err != nil {
		errs = append(errs, ErrRevocationRequired)
	}
	return errors.Join(errs...)
}

func withCleanupContext(ctx context.Context, fn func(context.Context) error) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return fn(cleanupCtx)
}

// DeliverToken hands this lease's token to the caller for transfer over the
// control plane's authenticated, confidentiality-protected agent channel,
// which ADR 0003 permits for transient material once the lease is running. It
// is the deliberate counterpart to NewAskPassBroker: a local checkout feeds
// Git through the one-use pipe, while a remote checkout on the agent host
// needs the token itself so it can build that pipe on its own side.
//
// Delivery is one-use and shares the same flag as the pipe, so a lease can
// either feed a local Git or be delivered once to one agent, never both and
// never twice. An invalidated or already-revoked lease delivers nothing.
func (l *CheckoutLease) DeliverToken() (string, error) {
	if l == nil {
		return "", ErrCheckoutUnavailable
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.token == "" || l.delivered || l.invalidated {
		return "", ErrCheckoutUnavailable
	}
	l.delivered = true
	return l.token, nil
}

// NewAskPassBroker transfers this lease's token through a one-use pipe. The
// lease becomes incapable of creating another broker after transfer.
func (l *CheckoutLease) NewAskPassBroker() (*AskPassBroker, error) {
	if l == nil {
		return nil, ErrCheckoutUnavailable
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.token == "" || l.delivered || l.invalidated {
		return nil, ErrCheckoutUnavailable
	}
	b, err := newAskPassBroker(l.token)
	if err != nil {
		return nil, ErrCheckoutUnavailable
	}
	l.delivered = true
	return b, nil
}

// RedactingWriter makes the checkout token safe before command output can be
// returned to a caller. It is the only output boundary that sees the token.
func (l *CheckoutLease) RedactingWriter(dst io.Writer) (*crypto.RedactingWriter, error) {
	if l == nil || dst == nil {
		return nil, ErrCheckoutUnavailable
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.token == "" {
		return nil, ErrCheckoutUnavailable
	}
	return crypto.NewRedactingWriter(dst, [][]byte{[]byte(l.token)})
}

type CheckoutService struct {
	store    ReservationStore
	provider Provider
	now      func() time.Time
}

func NewCheckoutService(store ReservationStore, provider Provider) (*CheckoutService, error) {
	if store == nil || provider == nil {
		return nil, ErrInvalidIdentity
	}
	return &CheckoutService{store: store, provider: provider, now: time.Now}, nil
}

// Activate turns a previously durable, exact-commit reservation into a
// one-use lease. The reservation is always closed if activation cannot finish.
func (s *CheckoutService) Activate(ctx context.Context, reservationID string) (lease *CheckoutLease, err error) {
	if s == nil || reservationID == "" {
		return nil, ErrCheckoutUnavailable
	}
	b, err := s.store.BeginMintAttempt(ctx, reservationID)
	if err != nil || !b.valid() || b.ReservationID != reservationID || !b.CloneURLValid() || len(b.CommitSHA) != 40 || !isLowerHex(b.CommitSHA) {
		return nil, errors.Join(ErrCheckoutUnavailable, closeError(withCleanupContext(ctx, func(cleanupCtx context.Context) error {
			return s.store.CloseCheckout(cleanupCtx, reservationID)
		})))
	}
	t, err := s.provider.MintCheckoutToken(ctx, b.InstallationID, []int64{b.RepositoryID})
	if err != nil || !validCheckoutToken(t, b.RepositoryID, s.now()) {
		var revokeErr error
		if t.Value != "" {
			revokeErr = withCleanupContext(ctx, func(cleanupCtx context.Context) error {
				return s.provider.RevokeToken(cleanupCtx, t.Value)
			})
		}
		closeErr := withCleanupContext(ctx, func(cleanupCtx context.Context) error {
			return s.store.CloseCheckout(cleanupCtx, reservationID)
		})
		return nil, errors.Join(ErrCheckoutUnavailable, revocationError(revokeErr), closeError(closeErr))
	}
	if err = s.store.ActivateMintAttempt(ctx, reservationID, b); err != nil {
		revokeErr := withCleanupContext(ctx, func(cleanupCtx context.Context) error {
			return s.provider.RevokeToken(cleanupCtx, t.Value)
		})
		closeErr := withCleanupContext(ctx, func(cleanupCtx context.Context) error {
			return s.store.CloseCheckout(cleanupCtx, reservationID)
		})
		return nil, errors.Join(ErrCheckoutStale, revocationError(revokeErr), closeError(closeErr))
	}
	lease = &CheckoutLease{provider: s.provider, store: s.store, token: t.Value, binding: b, expires: t.ExpiresAt}
	activeLeases.add(lease)
	return lease, nil
}
func closeError(err error) error {
	if err != nil {
		return ErrCheckoutUnavailable
	}
	return nil
}
func revocationError(err error) error {
	if err != nil {
		return ErrRevocationRequired
	}
	return nil
}

func validCheckoutToken(t Token, repositoryID int64, now time.Time) bool {
	return validToken(t.Value, t.ExpiresAt, now) && t.Contents == "read" && exactPermissions(t.Permissions, map[string]string{"contents": "read"}) && len(t.RepositoryIDs) == 1 && t.RepositoryIDs[0] == repositoryID
}
