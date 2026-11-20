package agenthttp

import (
	"container/list"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"autodeploy/internal/github"
	"autodeploy/internal/server"
	"autodeploy/internal/store/postgres"
)

// enrollRequest is deliberately never sent a raw credential token. The agent
// generates its own credential token locally (crypto/rand, via
// server.NewCredentialToken) and keeps the raw value only in its own 0600
// credential file; it presents this request's CredentialDigest instead --
// the domain-separated digest of that locally-generated token, computed
// with server.CredentialTokenDigest. The control plane therefore never
// observes, transmits, or stores the raw credential; it only ever sees and
// stores its digest, exactly like every credential this repository knows
// about (see internal/store/postgres/server_repository.go). EnrollmentToken
// is the single-use token an administrator printed out of band and gave to
// whoever is provisioning the agent; it authorizes this one enroll call and
// is itself never persisted, only its digest.
type enrollRequest struct {
	ServerID         string `json:"server_id"`
	EnrollmentToken  string `json:"enrollment_token"`
	CredentialDigest string `json:"credential_digest"`
}

func (req enrollRequest) parse() (serverID string, enrollmentDigest, credentialDigest [32]byte, ok bool) {
	if !github.ValidOpaqueID(req.ServerID) {
		return "", enrollmentDigest, credentialDigest, false
	}
	token, err := server.ParseEnrollmentToken(req.EnrollmentToken)
	if err != nil {
		return "", enrollmentDigest, credentialDigest, false
	}
	enrollmentDigest, err = server.EnrollmentTokenDigest(token)
	if err != nil {
		return "", enrollmentDigest, credentialDigest, false
	}
	credentialDigest, ok = decodeDigest(req.CredentialDigest)
	if !ok {
		return "", enrollmentDigest, credentialDigest, false
	}
	return req.ServerID, enrollmentDigest, credentialDigest, true
}

type enrollResponse struct {
	CredentialVersion int `json:"credential_version"`
}

func (h Handler) handleEnroll(w http.ResponseWriter, r *http.Request) {
	if h.Enroller == nil {
		h.writeError(w, http.StatusServiceUnavailable, "service_unavailable")
		return
	}
	var req enrollRequest
	if !h.readJSON(w, r, MaxBodyBytes, &req) {
		return
	}
	serverID, enrollmentDigest, credentialDigest, ok := req.parse()
	if !ok {
		h.writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	// Throttling policy (M2): the limiter is keyed on a caller-supplied
	// server id, so charging BEFORE the token check would let anyone who
	// knows an id hold that server's enrollment at 429. Instead every request
	// reaches the repository (its fast reject is lock-free and one query, so
	// an attacker's cost to us is bounded by that), a request whose token
	// verifies always succeeds regardless of attacker traffic, and only
	// failed attempts (ErrServerAuthorizationDenied) are charged. Once an id
	// has exhausted its failure budget, further failures answer 429 with
	// Retry-After instead of 401. Buckets are only created on failure, and
	// the map is LRU-bounded.
	version, err := h.Enroller.Enroll(r.Context(), serverID, enrollmentDigest, credentialDigest)
	if err != nil {
		if errors.Is(err, postgres.ErrServerAuthorizationDenied) {
			if allowed, wait := h.enrollLimiter().Allow(serverID); !allowed {
				w.Header().Set("Retry-After", retryAfterSeconds(wait))
				h.writeError(w, http.StatusTooManyRequests, "rate_limited")
				return
			}
		}
		h.mapError(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, enrollResponse{CredentialVersion: version})
}

// decodeDigest parses a lowercase-or-uppercase hex-encoded 32-byte digest.
// It is deliberately strict about length: a truncated or padded value is
// rejected rather than silently zero-extended.
func decodeDigest(s string) ([32]byte, bool) {
	var d [32]byte
	if len(s) != hex.EncodedLen(len(d)) {
		return d, false
	}
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != len(d) {
		return d, false
	}
	copy(d[:], b)
	return d, true
}

const (
	// enrollAttemptsPerMinute is the sustained per-server-id FAILED enroll rate
	// (successful enrolls are never charged).
	enrollAttemptsPerMinute = 10
	// enrollLimiterMaxEntries bounds limiter memory against random ids.
	enrollLimiterMaxEntries = 4096
)

// EnrollLimiter is an in-memory per-server-id token bucket for the
// unauthenticated enroll endpoint. Memory is bounded: at most maxEntries
// buckets are kept, evicting the least recently used when full (an attacker
// flooding random ids can at worst reset another id's bucket, never grow
// memory). Process-local by design: V1 runs one control-plane instance.
type EnrollLimiter struct {
	mu         sync.Mutex
	capacity   float64
	perSecond  float64
	maxEntries int
	now        func() time.Time
	order      *list.List // front = most recently used; values are *enrollBucket
	byID       map[string]*list.Element
}

type enrollBucket struct {
	id     string
	tokens float64
	last   time.Time
}

// NewEnrollLimiter allows burst attempts per id, refilled at perMinute.
func NewEnrollLimiter(burst, perMinute, maxEntries int, now func() time.Time) *EnrollLimiter {
	if now == nil {
		now = time.Now
	}
	return &EnrollLimiter{capacity: float64(burst), perSecond: float64(perMinute) / 60, maxEntries: maxEntries, now: now, order: list.New(), byID: make(map[string]*list.Element)}
}

// defaultEnrollLimiter is used when Handler.EnrollLimiter is nil so the
// protection is on without extra wiring.
var defaultEnrollLimiter = NewEnrollLimiter(enrollAttemptsPerMinute, enrollAttemptsPerMinute, enrollLimiterMaxEntries, nil)

// Allow charges one failed attempt against id. When refused it returns how long until
// one token is available.
func (l *EnrollLimiter) Allow(id string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	var b *enrollBucket
	if el, ok := l.byID[id]; ok {
		b = el.Value.(*enrollBucket)
		if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
			b.tokens = min(l.capacity, b.tokens+elapsed*l.perSecond)
			b.last = now
		}
		l.order.MoveToFront(el)
	} else {
		for l.order.Len() >= l.maxEntries {
			oldest := l.order.Back()
			delete(l.byID, oldest.Value.(*enrollBucket).id)
			l.order.Remove(oldest)
		}
		b = &enrollBucket{id: id, tokens: l.capacity, last: now}
		l.byID[id] = l.order.PushFront(b)
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / l.perSecond * float64(time.Second))
}

// Len reports the number of tracked ids (test hook for the memory bound).
func (l *EnrollLimiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.order.Len()
}

func (h Handler) enrollLimiter() *EnrollLimiter {
	if h.EnrollLimiter == nil {
		return defaultEnrollLimiter
	}
	return h.EnrollLimiter
}

func retryAfterSeconds(d time.Duration) string {
	secs := int((d + time.Second - 1) / time.Second)
	return strconv.Itoa(max(secs, 1))
}
