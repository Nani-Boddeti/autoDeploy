// Package checkout is the agent-side implementation of gitcheckout.Lease
// (tasks/todo.md Step 3c part 2). It drives the three /agent/v1/checkout RPCs
// -- begin, start, close -- so a host agent can checkout the exact commit its
// lease authorizes without ever holding a GitHub App private key or a
// ReservationStore: those stay control-plane-only (ADR 0001), and this
// package's only capability is a short-lived installation token fetched over
// the same authenticated channel every other agent route uses (ADR 0003).
package checkout

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"autodeploy/internal/crypto"
	"autodeploy/internal/githubapp"
)

// ErrUnavailable is returned for every local failure this package detects on
// its own -- a nil dependency, an out-of-order call, or a call repeated after
// the point it is no longer valid -- as opposed to an error surfaced by the
// control plane itself, which callers see unwrapped from Client.
var ErrUnavailable = errors.New("agent checkout lease unavailable")

// Reservation is the agent-side view of one POST /agent/v1/checkout/begin
// response: a reservation id and the authorized source identity, carrying no
// credential.
type Reservation struct {
	ReservationID string
	Binding       githubapp.CheckoutBinding
}

// Credential is the agent-side view of one POST /agent/v1/checkout/start
// response: the one point where a token crosses the control-plane/agent
// boundary (ADR 0003), delivered only after the control plane has already
// marked the durable lease running.
type Credential struct {
	Token     string
	ExpiresAt time.Time
}

// Client is the narrow control-plane capability this lease needs: the three
// checkout RPCs, keyed by the same attempt id and fence every other agent
// route fences on. autodeploy/internal/agent/client.Client satisfies this
// interface structurally; a fake is used in this package's own tests.
type Client interface {
	CheckoutBegin(ctx context.Context, attemptID, fence int64) (Reservation, error)
	CheckoutStart(ctx context.Context, attemptID, fence int64, reservationID string) (Credential, error)
	CheckoutClose(ctx context.Context, attemptID, fence int64, reservationID string) error
}

// Lease implements gitcheckout.Lease entirely in terms of Client. It never
// holds an App signer or a ReservationStore -- only the reservation id and,
// after Start, the token the control plane minted.
type Lease struct {
	client    Client
	attemptID int64
	fence     int64

	mu              sync.Mutex
	reservationID   string
	binding         githubapp.CheckoutBinding
	started         bool
	closed          bool
	token           string
	expiresAt       time.Time
	brokerDelivered bool
	cancel          context.CancelFunc
}

// Begin reserves a checkout credential for attemptID/fence over the given
// client and returns a *Lease ready to Start. It mints no token: begin is
// reservation only (tasks/todo.md Step 3c part 2).
func Begin(ctx context.Context, client Client, attemptID, fence int64) (*Lease, error) {
	if client == nil || attemptID <= 0 || fence <= 0 {
		return nil, ErrUnavailable
	}
	reservation, err := client.CheckoutBegin(ctx, attemptID, fence)
	if err != nil {
		return nil, err
	}
	if reservation.ReservationID == "" {
		return nil, ErrUnavailable
	}
	return &Lease{
		client:        client,
		attemptID:     attemptID,
		fence:         fence,
		reservationID: reservation.ReservationID,
		binding:       reservation.Binding,
	}, nil
}

// Binding is the authorized source identity captured at Begin.
func (l *Lease) Binding() githubapp.CheckoutBinding {
	if l == nil {
		return githubapp.CheckoutBinding{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.binding
}

// SetCancel installs the hook gitcheckout.Checkout uses to stop Git if this
// lease is invalidated while it runs. This package has no local
// invalidation source of its own (that lives control-plane-side in
// githubapp's leaseRegistry); the hook is stored so a future caller-driven
// cancellation path has somewhere to call, and Close always still runs via
// the caller's own deferred cleanup regardless of whether cancel is ever
// invoked.
func (l *Lease) SetCancel(cancel context.CancelFunc) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.cancel = cancel
	l.mu.Unlock()
}

// Start performs the start RPC, which marks the durable lease running and
// returns the credential in the same response. It fails closed: a caller
// that calls it twice, or after Close, gets ErrUnavailable rather than a
// silently stale token.
func (l *Lease) Start(ctx context.Context) error {
	if l == nil || l.client == nil {
		return ErrUnavailable
	}
	l.mu.Lock()
	if l.started || l.closed {
		l.mu.Unlock()
		return ErrUnavailable
	}
	reservationID := l.reservationID
	l.mu.Unlock()
	if reservationID == "" {
		return ErrUnavailable
	}

	credential, err := l.client.CheckoutStart(ctx, l.attemptID, l.fence, reservationID)
	if err != nil {
		return err
	}
	if credential.Token == "" {
		return ErrUnavailable
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		// Lost the race with a concurrent Close: never adopt a credential
		// for a lease that is already being torn down.
		return ErrUnavailable
	}
	l.token = credential.Token
	l.expiresAt = credential.ExpiresAt
	l.started = true
	return nil
}

// NewAskPassBroker transfers this lease's token through a one-use pipe,
// built from the exported githubapp.NewAskPassBrokerFromToken constructor
// (the one point ADR 0003 permits a fetched token to feed the same pipe
// discipline the control plane's own CheckoutLease uses). It fails closed
// before Start: there is no token yet to hand out, so no broker -- with no
// redaction behind it -- is ever returned.
func (l *Lease) NewAskPassBroker() (*githubapp.AskPassBroker, error) {
	if l == nil {
		return nil, ErrUnavailable
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.started || l.closed || l.token == "" || l.brokerDelivered {
		return nil, ErrUnavailable
	}
	broker, err := githubapp.NewAskPassBrokerFromToken(l.token)
	if err != nil {
		return nil, ErrUnavailable
	}
	l.brokerDelivered = true
	return broker, nil
}

// RedactingWriter wraps dst so the token cannot appear in command output.
// Like NewAskPassBroker, it fails closed before Start: without a token there
// is nothing to redact, so this must never return a writer that would pass
// output through unredacted.
func (l *Lease) RedactingWriter(dst io.Writer) (*crypto.RedactingWriter, error) {
	if l == nil || dst == nil {
		return nil, ErrUnavailable
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.started || l.closed || l.token == "" {
		return nil, ErrUnavailable
	}
	return crypto.NewRedactingWriter(dst, [][]byte{[]byte(l.token)})
}

// ExpiresAt reports when the started credential expires. It is zero before
// Start.
func (l *Lease) ExpiresAt() time.Time {
	if l == nil {
		return time.Time{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.expiresAt
}

// Close performs the close RPC and is idempotent: a second call, or a call
// on a lease that was never started, both succeed without re-contacting the
// control plane a second time beyond what the close RPC itself does (the
// RPC's own idempotency is the control plane's responsibility, matching
// CheckoutLease.Close on the other side of the boundary).
func (l *Lease) Close(ctx context.Context) error {
	if l == nil {
		return ErrUnavailable
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	reservationID := l.reservationID
	client := l.client
	l.mu.Unlock()
	if client == nil || reservationID == "" {
		return ErrUnavailable
	}
	return client.CheckoutClose(ctx, l.attemptID, l.fence, reservationID)
}
