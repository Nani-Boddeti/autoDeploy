// Package agenthttp implements the isolated agent HTTP transport boundary
// (docs/adr/0001 control-plane/agent separation; docs/adr/0004 fenced lease
// protocol). It deliberately has no browser session/CSRF dependency, mirrors
// internal/github/http's trusted-HTTPS provenance and bounded-body-read
// discipline, and never observes a raw bearer credential, digest, or
// request/response body outside this package: every dependency is a narrow
// interface over the real postgres repositories, never a concrete type, so
// the boundary is testable with fakes and cannot leak persistence internals.
package agenthttp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"autodeploy/internal/github"
	"autodeploy/internal/queue"
	"autodeploy/internal/server"
	"autodeploy/internal/store/postgres"
)

const (
	// Prefix is the mount point for every route this handler serves.
	Prefix = "/agent/v1/"

	pathEnroll     = "/agent/v1/enroll"
	pathHeartbeat  = "/agent/v1/heartbeat"
	pathClaim      = "/agent/v1/jobs/claim"
	pathRenew      = "/agent/v1/leases/renew"
	pathComplete   = "/agent/v1/leases/complete"
	pathCancelAck  = "/agent/v1/leases/cancel-ack"
	pathReconcile  = "/agent/v1/environments/reconcile"
	authHeaderName = "Authorization"
	serverIDHeader = "X-AutoDeploy-Server-ID"
	bearerPrefix   = "Bearer "

	// MaxBodyBytes bounds every request body except heartbeat, which is
	// tighter because its fixed-schema capability struct never needs more.
	MaxBodyBytes = 16 * 1024
	// MaxHeartbeatBodyBytes bounds the heartbeat capability struct.
	MaxHeartbeatBodyBytes = 1024

	// HeartbeatIntervalSeconds is the fixed interval this slice tells every
	// agent to heartbeat at (owner decision; see tasks/todo.md Step 5).
	HeartbeatIntervalSeconds = 20
	// defaultRequestTimeout bounds how long one request may run so a slow or
	// stalled agent connection cannot hold a database transaction open
	// indefinitely.
	defaultRequestTimeout = 10 * time.Second
)

// Clock lets tests control request-deadline computation deterministically;
// production wiring leaves it nil and gets time.Now.
type Clock interface{ Now() time.Time }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// Enroller consumes a presented enrollment/credential digest pair and mints
// the first durable credential version. It matches
// (*postgres.ServerRepository).Enroll's signature exactly.
type Enroller interface {
	Enroll(ctx context.Context, serverID string, enrollmentDigest, credentialDigest [32]byte) (int, error)
}

// Authenticator verifies a presented credential digest without mutating
// anything. It is used only where a route must gate business logic (claim's
// executor_available=false short-circuit) without performing that logic.
type Authenticator interface {
	Authenticate(ctx context.Context, serverID string, presentedDigest [32]byte) error
}

// Heartbeater authenticates and durably records agent liveness in one
// repository call, mirroring how Claim/Renew/Complete re-authenticate
// internally rather than trusting an earlier check.
type Heartbeater interface {
	Heartbeat(ctx context.Context, serverID string, presentedDigest [32]byte) error
}

// LeaseClaimer, LeaseRenewer, LeaseCompleter, LeaseCanceller, and
// EnvironmentReconciler match (*postgres.AgentLeaseRepository)'s method
// signatures exactly, so production wiring can pass that repository
// directly without an adapter.
type LeaseClaimer interface {
	Claim(ctx context.Context, serverID string, presentedDigest [32]byte, limit int) ([]queue.ClaimedLease, error)
}
type LeaseRenewer interface {
	Renew(ctx context.Context, serverID string, presentedDigest [32]byte, attemptID, fence int64, sequence int) (queue.RenewalResult, error)
}
type LeaseCompleter interface {
	Complete(ctx context.Context, serverID string, presentedDigest [32]byte, attemptID, fence int64, outcome queue.CompletionOutcome, failureClass queue.FailureClass, operationID string, resultDigest [32]byte) error
}
type LeaseCanceller interface {
	AcknowledgeCancellation(ctx context.Context, serverID string, presentedDigest [32]byte, attemptID, fence int64, operationID string, resultDigest [32]byte) error
}
type EnvironmentReconciler interface {
	Reconcile(ctx context.Context, serverID string, presentedDigest [32]byte, environmentID string, attemptID, fence int64, outcomeDigest [32]byte) error
}

// Handler deliberately has no browser session or CSRF dependency and takes
// only narrow interfaces, never a concrete repository, so it can be
// constructed with fakes in tests. A nil dependency makes its route return
// 503 rather than panicking.
type Handler struct {
	Enroller          Enroller
	Authenticator     Authenticator
	Heartbeater       Heartbeater
	Claimer           LeaseClaimer
	Renewer           LeaseRenewer
	Completer         LeaseCompleter
	Canceller         LeaseCanceller
	Reconciler        EnvironmentReconciler
	TrustedProxyCIDRs []netip.Prefix
	Clock             Clock
	RequestTimeout    time.Duration
}

func (h Handler) clock() Clock {
	if h.Clock == nil {
		return systemClock{}
	}
	return h.Clock
}

func (h Handler) requestTimeout() time.Duration {
	if h.RequestTimeout <= 0 {
		return defaultRequestTimeout
	}
	return h.RequestTimeout
}

func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL == nil || !strings.HasPrefix(r.URL.Path, Prefix) || r.URL.RawPath != "" || r.URL.RawQuery != "" {
		h.writeError(w, http.StatusNotFound, "not_found")
		return
	}
	if r.Method != http.MethodPost {
		h.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if !h.https(r) {
		h.writeError(w, http.StatusNotFound, "not_found")
		return
	}
	if !validContentType(r.Header) {
		h.writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	ctx, cancel := context.WithDeadline(r.Context(), h.clock().Now().Add(h.requestTimeout()))
	defer cancel()
	r = r.WithContext(ctx)

	switch r.URL.Path {
	case pathEnroll:
		h.handleEnroll(w, r)
	case pathHeartbeat:
		h.handleHeartbeat(w, r)
	case pathClaim:
		h.handleClaim(w, r)
	case pathRenew:
		h.handleRenew(w, r)
	case pathComplete:
		h.handleComplete(w, r)
	case pathCancelAck:
		h.handleCancelAck(w, r)
	case pathReconcile:
		h.handleReconcile(w, r)
	default:
		h.writeError(w, http.StatusNotFound, "not_found")
	}
}

func validContentType(header http.Header) bool {
	values := header.Values("Content-Type")
	return len(values) == 1 && values[0] == "application/json"
}

// identity extracts and strictly validates the bearer credential and server
// ID this request presents, without contacting any repository. It never
// returns partial success: a malformed Authorization header, a duplicated
// header, an enrollment token presented as a bearer credential, or a
// missing/invalid server-ID header are all indistinguishable failures to
// the caller.
func (h Handler) identity(r *http.Request) (serverID string, digest [32]byte, ok bool) {
	authValues := r.Header.Values(authHeaderName)
	if len(authValues) != 1 || !strings.HasPrefix(authValues[0], bearerPrefix) {
		return "", digest, false
	}
	token, err := server.ParseCredentialToken(strings.TrimPrefix(authValues[0], bearerPrefix))
	if err != nil {
		return "", digest, false
	}
	d, err := server.CredentialTokenDigest(token)
	if err != nil {
		return "", digest, false
	}
	idValues := r.Header.Values(serverIDHeader)
	if len(idValues) != 1 || !github.ValidOpaqueID(idValues[0]) {
		return "", digest, false
	}
	return idValues[0], d, true
}

// readBody bounds one full body read to max bytes, exactly once, using the
// same http.MaxBytesReader discipline as internal/github/http.
func readBody(w http.ResponseWriter, r *http.Request, max int64) ([]byte, bool) {
	if r.Body == nil {
		return nil, false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, max))
	if err != nil || int64(len(body)) > max {
		return nil, false
	}
	return body, true
}

// decodeStrict rejects unknown fields and any trailing JSON value, matching
// internal/github/http's parse discipline.
func decodeStrict(body []byte, v any) bool {
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return false
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return false
	}
	return true
}

// readJSON combines the bounded read, strict decode, and their shared
// failure responses (413 for an oversized body, 400 for anything else) so
// every handler follows the same order: read, then decode.
func (h Handler) readJSON(w http.ResponseWriter, r *http.Request, max int64, v any) bool {
	body, ok := readBody(w, r, max)
	if !ok {
		h.writeError(w, http.StatusRequestEntityTooLarge, "request_entity_too_large")
		return false
	}
	if !decodeStrict(body, v) {
		h.writeError(w, http.StatusBadRequest, "invalid_request")
		return false
	}
	return true
}

func (h Handler) writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

type errorBody struct {
	Error string `json:"error"`
}

func (h Handler) writeError(w http.ResponseWriter, status int, code string) {
	h.writeJSON(w, status, errorBody{Error: code})
}

// mapError translates a repository error into the fixed, sanitized response
// shapes ADR 0004/tasks/todo.md Step 5 require. It never echoes err's text:
// an infrastructure failure the caller cannot classify is reported as a
// generic retryable 503.
func (h Handler) mapError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, postgres.ErrServerAuthorizationDenied):
		h.writeError(w, http.StatusUnauthorized, "unauthorized")
	case errors.Is(err, postgres.ErrAgentLeaseStale):
		h.writeError(w, http.StatusConflict, "stale_lease")
	case errors.Is(err, postgres.ErrAgentLeaseIntegrityConflict):
		h.writeError(w, http.StatusConflict, "integrity_conflict")
	case errors.Is(err, postgres.ErrAgentLeaseInvalidInput):
		h.writeError(w, http.StatusBadRequest, "invalid_request")
	default:
		h.writeError(w, http.StatusServiceUnavailable, "service_unavailable")
	}
}

// https requires the same trusted-HTTPS provenance as internal/github/http:
// either a direct TLS connection, or a single X-Forwarded-Proto: https
// value from a peer address inside a configured trusted-proxy CIDR. A
// direct untrusted peer can never assert HTTPS this way.
func (h Handler) https(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	trusted := false
	for _, cidr := range h.TrustedProxyCIDRs {
		if cidr.Contains(peer.Unmap()) {
			trusted = true
			break
		}
	}
	if !trusted {
		return false
	}
	values := r.Header.Values("X-Forwarded-Proto")
	return len(values) == 1 && values[0] == "https"
}
