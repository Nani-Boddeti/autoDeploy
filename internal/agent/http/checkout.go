package agenthttp

import (
	"context"
	"net/http"
	"sync"
	"time"

	"autodeploy/internal/githubapp"
	"autodeploy/internal/queue"
)

// checkoutRegistryTTL bounds how long an abandoned reservation (begun but
// never started, or started but never closed because the control plane
// restarted) can pin memory in the registry. It generously exceeds
// internal/store/postgres's own 10-minute github_checkout_reservations
// expiry window so this registry never expires an entry the store would
// still honor, while still guaranteeing eventual cleanup.
const checkoutRegistryTTL = 30 * time.Minute

// checkoutRegistryEntry binds one reservation id to the exact server,
// attempt, and fence that created it (begin), so a later start/close call
// presenting a different server credential, attempt id, or fence -- even one
// that is itself perfectly valid for some other attempt -- cannot drive this
// reservation. lease is nil until Start succeeds; closed marks a completed
// close so a retried close call stays idempotent without needing to keep
// re-deriving ownership from a lease that no longer exists.
type checkoutRegistryEntry struct {
	serverID  string
	attemptID int64
	fence     int64
	lease     *githubapp.CheckoutLease
	started   bool
	closed    bool
	expiresAt time.Time
}

// CheckoutRegistry holds the process-local *githubapp.CheckoutLease values
// created by begin/start, keyed by reservation id, because an activated
// lease is a process-local object (docs/adr/0003; tasks/todo.md Step 3c part
// 2's "known wrinkle"). A control-plane restart between start and close
// therefore loses this registry entirely; the reservation's own token still
// expires on schedule, so revocation degrades to token expiry rather than
// failing unsafely -- that gap is deliberate and recorded, not a bug.
type CheckoutRegistry struct {
	mu      sync.Mutex
	entries map[string]*checkoutRegistryEntry
}

// NewCheckoutRegistry constructs an empty registry. Production wiring builds
// exactly one and shares it across every request (Handler is passed by
// value, but this field is a pointer, so every copy of Handler sees the same
// registry).
func NewCheckoutRegistry() *CheckoutRegistry {
	return &CheckoutRegistry{entries: make(map[string]*checkoutRegistryEntry)}
}

func (reg *CheckoutRegistry) sweepLocked(now time.Time) {
	for id, e := range reg.entries {
		if !e.expiresAt.After(now) {
			delete(reg.entries, id)
		}
	}
}

// add records a freshly begun reservation. It is called before any lease
// exists so a begun-but-never-started close still has an entry to match
// against.
func (reg *CheckoutRegistry) add(id, serverID string, attemptID, fence int64, now time.Time) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.sweepLocked(now)
	reg.entries[id] = &checkoutRegistryEntry{serverID: serverID, attemptID: attemptID, fence: fence, expiresAt: now.Add(checkoutRegistryTTL)}
}

// checkoutEntryState is an immutable snapshot of one registry entry, taken
// under the registry lock. match returns this rather than the entry pointer
// itself: handing out the pointer let a caller read lease/closed while another
// request mutated them through setLease/markClosed, which is a data race no
// amount of locking inside those setters can fix.
type checkoutEntryState struct {
	lease   *githubapp.CheckoutLease
	closed  bool
	started bool
}

// match returns a snapshot for id only if it exists, has not expired, and was
// created by exactly this server/attempt/fence. Every other case -- unknown
// id, expired id, or any mismatch -- returns ok=false, deliberately
// indistinguishable to the caller: a valid credential for another server or
// a stale fence must not be able to tell "wrong reservation" from "no such
// reservation".
func (reg *CheckoutRegistry) match(id, serverID string, attemptID, fence int64, now time.Time) (checkoutEntryState, bool) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.sweepLocked(now)
	e, ok := reg.entries[id]
	if !ok || e.serverID != serverID || e.attemptID != attemptID || e.fence != fence {
		return checkoutEntryState{}, false
	}
	return checkoutEntryState{lease: e.lease, closed: e.closed, started: e.started}, true
}

// claimStart atomically reserves the right to activate id exactly once. Two
// concurrent starts for one reservation would otherwise each call Activate and
// mint a token, and only one of them could ever be closed -- the other would
// stay live until it expired. The loser is refused like any other
// unauthorized start.
func (reg *CheckoutRegistry) claimStart(id, serverID string, attemptID, fence int64, now time.Time) bool {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.sweepLocked(now)
	e, ok := reg.entries[id]
	if !ok || e.serverID != serverID || e.attemptID != attemptID || e.fence != fence || e.started || e.closed {
		return false
	}
	e.started = true
	return true
}

// releaseStart undoes claimStart when activation or Start itself failed, so a
// transient failure does not permanently burn the reservation.
func (reg *CheckoutRegistry) releaseStart(id string) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if e, ok := reg.entries[id]; ok && e.lease == nil {
		e.started = false
	}
}

func (reg *CheckoutRegistry) setLease(id string, lease *githubapp.CheckoutLease) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if e, ok := reg.entries[id]; ok {
		e.lease = lease
	}
}

// markClosed records that id's close already ran, without removing the
// entry: a retried close must still match and observe idempotent success
// until the registry's own TTL sweep eventually reclaims it.
func (reg *CheckoutRegistry) markClosed(id string) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if e, ok := reg.entries[id]; ok {
		e.closed = true
	}
}

// CheckoutAuthorizer re-derives an attempt's right to reserve or mint a
// checkout credential from locked rows. It matches
// (*postgres.AgentLeaseRepository).AuthorizeCheckout exactly.
type CheckoutAuthorizer interface {
	AuthorizeCheckout(ctx context.Context, serverID string, presentedDigest [32]byte, attemptID, fence int64) (queue.CheckoutAuthorization, error)
}

// CheckoutReservationStore is the subset of githubapp.ReservationStore this
// transport calls directly, rather than through CheckoutActivator: reserving
// a credential-free exact-commit reservation, and closing one (idempotently,
// including one that was never activated).
type CheckoutReservationStore interface {
	ReserveCheckout(ctx context.Context, owner, project, sha string) (githubapp.CheckoutReservation, error)
	CloseCheckout(ctx context.Context, id string) error
}

// CheckoutActivator matches (*githubapp.CheckoutService).Activate exactly.
// It is the only capability that can turn a reservation into a live,
// token-bearing lease, and it never touches the App private key itself --
// only the provider's already-derived installation-token minting.
type CheckoutActivator interface {
	Activate(ctx context.Context, reservationID string) (*githubapp.CheckoutLease, error)
}

type checkoutBindingView struct {
	OwnerID        string `json:"owner_id"`
	ProjectID      string `json:"project_id"`
	InstallationID int64  `json:"installation_id"`
	RepositoryID   int64  `json:"repository_id"`
	CloneURL       string `json:"clone_url"`
	CommitSHA      string `json:"commit_sha"`
	ReservationID  string `json:"reservation_id"`
}

type checkoutBeginRequest struct {
	AttemptID int64 `json:"attempt_id"`
	Fence     int64 `json:"fence"`
}

func (r checkoutBeginRequest) valid() bool { return r.AttemptID > 0 && r.Fence > 0 }

type checkoutBeginResponse struct {
	ReservationID string              `json:"reservation_id"`
	Binding       checkoutBindingView `json:"binding"`
}

// handleCheckoutBegin authorizes, then reserves a credential-free exact-
// commit reservation for the authorized owner/project/commit -- never for
// anything the caller names. No token is minted here and none is ever
// present in this response: minting happens only in handleCheckoutStart,
// immediately before the durable lease is marked running.
func (h Handler) handleCheckoutBegin(w http.ResponseWriter, r *http.Request) {
	serverID, digest, ok := h.identity(r)
	if !ok {
		h.writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if h.CheckoutAuthorizer == nil || h.CheckoutStore == nil || h.CheckoutRegistry == nil {
		h.writeError(w, http.StatusServiceUnavailable, "service_unavailable")
		return
	}
	var req checkoutBeginRequest
	if !h.readJSON(w, r, MaxBodyBytes, &req) {
		return
	}
	if !req.valid() {
		h.writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	authorization, err := h.CheckoutAuthorizer.AuthorizeCheckout(r.Context(), serverID, digest, req.AttemptID, req.Fence)
	if err != nil {
		h.mapError(w, err)
		return
	}
	reservation, err := h.CheckoutStore.ReserveCheckout(r.Context(), authorization.OwnerID, authorization.ProjectID, authorization.CommitSHA)
	if err != nil {
		h.writeError(w, http.StatusServiceUnavailable, "service_unavailable")
		return
	}
	// Defense in depth, the same discipline AuthorizeMaterialization applies
	// to frozen secret scope: the reservation resolves owner/project to a
	// repository through its own independent binding lookup, and that
	// resolution must agree with what the frozen job actually references
	// before any token is ever minted against it.
	if reservation.RepositoryID != authorization.RepositoryID || reservation.CommitSHA != authorization.CommitSHA {
		_ = h.CheckoutStore.CloseCheckout(r.Context(), reservation.ID)
		h.writeError(w, http.StatusServiceUnavailable, "service_unavailable")
		return
	}
	h.CheckoutRegistry.add(reservation.ID, serverID, req.AttemptID, req.Fence, h.clock().Now())
	h.writeJSON(w, http.StatusOK, checkoutBeginResponse{
		ReservationID: reservation.ID,
		Binding: checkoutBindingView{
			OwnerID:        reservation.OwnerID,
			ProjectID:      reservation.ProjectID,
			InstallationID: reservation.InstallationID,
			RepositoryID:   reservation.RepositoryID,
			CloneURL:       reservation.CloneURL,
			CommitSHA:      reservation.CommitSHA,
			ReservationID:  reservation.ID,
		},
	})
}

type checkoutStartRequest struct {
	AttemptID     int64  `json:"attempt_id"`
	Fence         int64  `json:"fence"`
	ReservationID string `json:"reservation_id"`
}

func (r checkoutStartRequest) valid() bool {
	return r.AttemptID > 0 && r.Fence > 0 && r.ReservationID != "" && len(r.ReservationID) <= 128
}

type checkoutStartResponse struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
}

// handleCheckoutStart re-authorizes, activates the reservation (which mints
// the installation token server-side), and only then calls the lease's own
// Start -- the durable checkout lease is marked running before the response
// carrying the token is ever written, preserving the invariant that the
// credential and the running state ship together. If Start fails the
// activated lease is closed immediately and no token is ever returned.
func (h Handler) handleCheckoutStart(w http.ResponseWriter, r *http.Request) {
	serverID, digest, ok := h.identity(r)
	if !ok {
		h.writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if h.CheckoutAuthorizer == nil || h.CheckoutActivator == nil || h.CheckoutRegistry == nil {
		h.writeError(w, http.StatusServiceUnavailable, "service_unavailable")
		return
	}
	var req checkoutStartRequest
	if !h.readJSON(w, r, MaxBodyBytes, &req) {
		return
	}
	if !req.valid() {
		h.writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if _, err := h.CheckoutAuthorizer.AuthorizeCheckout(r.Context(), serverID, digest, req.AttemptID, req.Fence); err != nil {
		h.mapError(w, err)
		return
	}
	// Claim the single start this reservation gets. A retried or concurrent
	// start is refused rather than minting a second token that nothing could
	// ever close; an agent needing another credential begins a new
	// reservation.
	if !h.CheckoutRegistry.claimStart(req.ReservationID, serverID, req.AttemptID, req.Fence, h.clock().Now()) {
		h.writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	lease, err := h.CheckoutActivator.Activate(r.Context(), req.ReservationID)
	if err != nil {
		h.CheckoutRegistry.releaseStart(req.ReservationID)
		h.writeError(w, http.StatusServiceUnavailable, "service_unavailable")
		return
	}
	if err := lease.Start(r.Context()); err != nil {
		// Start failed: the durable lease was never marked running, so the
		// credential must never be returned. Close what Activate opened so
		// no minted token is left live with nothing that can use it.
		_ = lease.Close(r.Context())
		h.CheckoutRegistry.releaseStart(req.ReservationID)
		h.writeError(w, http.StatusServiceUnavailable, "service_unavailable")
		return
	}
	h.CheckoutRegistry.setLease(req.ReservationID, lease)

	// Deliver the token through the lease's explicit one-use delivery method.
	// An earlier version drained the askpass pipe instead, which worked but
	// misused a mechanism built to feed a local Git subprocess; delivery to a
	// remote agent is its own operation and says so.
	token, err := lease.DeliverToken()
	if err != nil {
		h.writeError(w, http.StatusServiceUnavailable, "service_unavailable")
		return
	}
	h.writeJSON(w, http.StatusOK, checkoutStartResponse{Token: token, ExpiresAt: lease.ExpiresAt().Format(time.RFC3339Nano)})
}

type checkoutCloseRequest struct {
	AttemptID     int64  `json:"attempt_id"`
	Fence         int64  `json:"fence"`
	ReservationID string `json:"reservation_id"`
}

func (r checkoutCloseRequest) valid() bool {
	return r.AttemptID > 0 && r.Fence > 0 && r.ReservationID != "" && len(r.ReservationID) <= 128
}

// handleCheckoutClose releases the reservation and, if a credential was ever
// minted, revokes it. It is idempotent: a reservation already closed by an
// earlier call (or one that was begun but never started, so no token exists
// to revoke) both succeed without error. A reservation this caller never
// owned -- wrong server, wrong attempt, or a stale fence -- is refused
// exactly like an unknown reservation id.
func (h Handler) handleCheckoutClose(w http.ResponseWriter, r *http.Request) {
	serverID, digest, ok := h.identity(r)
	if !ok {
		h.writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if h.CheckoutAuthorizer == nil || h.CheckoutStore == nil || h.CheckoutRegistry == nil {
		h.writeError(w, http.StatusServiceUnavailable, "service_unavailable")
		return
	}
	var req checkoutCloseRequest
	if !h.readJSON(w, r, MaxBodyBytes, &req) {
		return
	}
	if !req.valid() {
		h.writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if _, err := h.CheckoutAuthorizer.AuthorizeCheckout(r.Context(), serverID, digest, req.AttemptID, req.Fence); err != nil {
		h.mapError(w, err)
		return
	}
	entry, matched := h.CheckoutRegistry.match(req.ReservationID, serverID, req.AttemptID, req.Fence, h.clock().Now())
	if !matched {
		h.writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if entry.closed {
		h.writeJSON(w, http.StatusOK, struct{}{})
		return
	}
	var closeErr error
	if entry.lease != nil {
		closeErr = entry.lease.Close(r.Context())
	} else {
		closeErr = h.CheckoutStore.CloseCheckout(r.Context(), req.ReservationID)
	}
	if closeErr != nil {
		h.writeError(w, http.StatusServiceUnavailable, "service_unavailable")
		return
	}
	h.CheckoutRegistry.markClosed(req.ReservationID)
	h.writeJSON(w, http.StatusOK, struct{}{})
}
