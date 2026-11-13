//go:build integration

package postgres_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"autodeploy/internal/auth"
	"autodeploy/internal/store/postgres"
	"autodeploy/internal/web"
)

// resolveSessionRepository is the web Repository with only the session
// lookup implemented: the principal under test is the real fixture
// administrator, so the real handler runs against the real
// AgentLeaseRepository without a login round trip.
type resolveSessionRepository struct {
	web.Repository
	principal auth.Principal
}

func (r resolveSessionRepository) ValidateSession(context.Context, auth.SessionToken) (auth.Principal, error) {
	return r.principal, nil
}

// resolveServerAdmin is the production adapter's shape (cmd/controlplane
// serverAdmin) for the two methods this route uses; the others are unused.
type resolveServerAdmin struct {
	web.ServerAdmin
	leases *postgres.AgentLeaseRepository
}

func (a resolveServerAdmin) ListReconcilingEnvironments(ctx context.Context, p auth.Principal) ([]postgres.ReconcilingEnvironment, error) {
	return a.leases.ListReconcilingEnvironments(ctx, p)
}
func (a resolveServerAdmin) ListServers(context.Context, auth.Principal) ([]postgres.ServerSummary, error) {
	return nil, nil
}
func (a resolveServerAdmin) ResolveActivation(ctx context.Context, p auth.Principal, environmentID, outcome, reason string) error {
	return a.leases.ResolveActivation(ctx, p, environmentID, outcome, reason)
}

type resolveHarness struct {
	t       *testing.T
	handler http.Handler
	env     postgres.BlockedEnvironmentForTest
	csrf    string
}

func newResolveHarness(t *testing.T, env postgres.BlockedEnvironmentForTest) resolveHarness {
	t.Helper()
	policy := auth.Argon2Policy{Revision: 1, MemoryKiB: 19 * 1024, Iterations: 2, Lanes: 1}
	dummy, err := auth.BuildDummyPasswordVerifier(policy)
	if err != nil {
		t.Fatal(err)
	}
	keys := auth.CSRFKeyRing{ActiveVersion: "v1", Keys: map[string][]byte{"v1": bytes.Repeat([]byte{1}, 32)}}
	throttle := auth.UsernameThrottleKeyRing{ActiveVersion: "v1", Keys: map[string][]byte{"v1": bytes.Repeat([]byte{2}, 32)}}
	const origin = "https://admin.example.test"
	h, err := web.New(resolveSessionRepository{principal: env.Principal}, web.Config{
		PublicOrigin: origin, CSRFKeys: keys, UsernameThrottleKeys: throttle, AuthThrottleMaxRows: 8,
		PasswordPolicy: policy, DummyPassword: dummy, ServerAdmin: resolveServerAdmin{leases: env.Leases},
		Now: func() time.Time { return time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC) }, Random: bytes.NewReader(bytes.Repeat([]byte{3}, 512)),
	})
	if err != nil {
		t.Fatal(err)
	}
	csrf, err := auth.NewSessionCSRF(keys, env.Principal.SessionID, origin)
	if err != nil {
		t.Fatal(err)
	}
	return resolveHarness{t: t, handler: h, env: env, csrf: csrf}
}

func (h resolveHarness) request(method, target string, form url.Values) *httptest.ResponseRecorder {
	h.t.Helper()
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	r := httptest.NewRequest(method, "https://admin.example.test"+target, body)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", "https://admin.example.test")
	}
	r.AddCookie(&http.Cookie{Name: "__Host-ad_session", Value: "v1.AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"})
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, r)
	return rr
}

func (h resolveHarness) resolve(outcome string) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.request(http.MethodPost, "/environments/resolve", url.Values{"csrf": {h.csrf}, "environment_id": {h.env.EnvironmentID}, "outcome": {outcome}, "reason": {"operator checked the host"}})
}

func (h resolveHarness) listed() bool {
	h.t.Helper()
	rr := h.request(http.MethodGet, "/servers", nil)
	if rr.Code != http.StatusOK {
		h.t.Fatalf("GET /servers = %d", rr.Code)
	}
	return strings.Contains(rr.Body.String(), `<td>`+h.env.EnvironmentID+` (`)
}

const (
	mismatchCaused    = "this block was caused by an activation; declare activated or not_activated\n"
	mismatchNotCaused = "this block was not caused by an activation; declare no_activation\n"
)

// TestResolveFormEndToEndActivationBlock drives the real handler over the real
// repository for a block the real Activate and sweeper produced.
func TestResolveFormEndToEndActivationBlock(t *testing.T) {
	h := newResolveHarness(t, postgres.NewActivationBlockForTest(t, 160, true))
	if !h.listed() {
		t.Fatal("setup: the activation-blocked environment is not listed")
	}
	rr := h.request(http.MethodGet, "/servers", nil)
	if !strings.Contains(rr.Body.String(), "<td>reconciliation_required</td>") {
		t.Fatal("the Activation column does not show reconciliation_required")
	}

	rr = h.resolve("no_activation")
	if rr.Code != http.StatusConflict || rr.Body.String() != mismatchCaused {
		t.Fatalf("no_activation on an activation block = %d %q", rr.Code, rr.Body.String())
	}
	if !h.listed() {
		t.Fatal("a refused resolution unblocked the environment")
	}

	rr = h.resolve("not_activated")
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/servers" {
		t.Fatalf("not_activated = %d location=%q body=%q", rr.Code, rr.Header().Get("Location"), rr.Body.String())
	}
	if h.listed() {
		t.Fatal("the environment is still listed after not_activated")
	}
}

// TestResolveFormEndToEndPlainBlock: a block no activation caused takes
// no_activation and refuses a declared activation outcome.
func TestResolveFormEndToEndPlainBlock(t *testing.T) {
	h := newResolveHarness(t, postgres.NewPlainBlockForTest(t, 170))
	if !h.listed() {
		t.Fatal("setup: the plain-blocked environment is not listed")
	}
	if rr := h.request(http.MethodGet, "/servers", nil); !strings.Contains(rr.Body.String(), "<td>none</td>") {
		t.Fatal("the Activation column does not show none")
	}
	rr := h.resolve("not_activated")
	if rr.Code != http.StatusConflict || rr.Body.String() != mismatchNotCaused {
		t.Fatalf("not_activated on a plain block = %d %q", rr.Code, rr.Body.String())
	}
	if !h.listed() {
		t.Fatal("a refused resolution unblocked the environment")
	}
	rr = h.resolve("no_activation")
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/servers" {
		t.Fatalf("no_activation = %d %q", rr.Code, rr.Body.String())
	}
	if h.listed() {
		t.Fatal("the environment is still listed after no_activation")
	}
}

func (h resolveHarness) count(query string, args ...any) int {
	h.t.Helper()
	var n int
	if err := h.env.Pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

func (h resolveHarness) auditReasons() []string {
	h.t.Helper()
	rows, err := h.env.Pool.Query(context.Background(), `SELECT reason FROM server_audit_events WHERE action='environment_mark_reconciled' AND result='success' ORDER BY id`)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			h.t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

const eventsQuery = `SELECT count(*) FROM agent_job_events`
const reconciledQuery = `SELECT count(*) FROM agent_job_events WHERE event_type='reconciled'`
const auditQuery = `SELECT count(*) FROM server_audit_events WHERE action='environment_mark_reconciled'`

// activated through the form on a real activation block unblocks it, writes
// exactly one audit row carrying the declared outcome, and a refused
// resolution before it wrote neither an audit row nor a job event.
func TestResolveFormEndToEndActivatedAndAudit(t *testing.T) {
	h := newResolveHarness(t, postgres.NewActivationBlockForTest(t, 180, true))
	events, audits := h.count(eventsQuery), h.count(auditQuery)
	if rr := h.resolve("no_activation"); rr.Code != http.StatusConflict {
		t.Fatalf("mismatch = %d", rr.Code)
	}
	if h.count(eventsQuery) != events || h.count(auditQuery) != audits {
		t.Fatal("a refused resolution wrote an audit row or a job event")
	}
	rr := h.resolve("activated")
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/servers" {
		t.Fatalf("activated = %d %q", rr.Code, rr.Body.String())
	}
	if h.listed() {
		t.Fatal("the environment is still listed after activated")
	}
	if got := h.auditReasons(); len(got) != 1 || got[0] != "outcome=activated; operator checked the host" {
		t.Fatalf("audit reasons = %q", got)
	}
	if got := h.count(reconciledQuery); got != 1 {
		t.Fatalf("reconciled events = %d, want 1", got)
	}
}

// A repeat that names a different outcome than the recorded one is an
// integrity conflict (confirmResolvedOutcome -> agreeResolvedOutcome); the
// same outcome is an idempotent success. Neither writes.
func TestResolveFormEndToEndReplay(t *testing.T) {
	h := newResolveHarness(t, postgres.NewActivationBlockForTest(t, 190, true))
	if rr := h.resolve("not_activated"); rr.Code != http.StatusSeeOther {
		t.Fatalf("not_activated = %d %q", rr.Code, rr.Body.String())
	}
	if got := h.auditReasons(); len(got) != 1 || got[0] != "outcome=not_activated; operator checked the host" {
		t.Fatalf("audit reasons = %q", got)
	}
	events, audits := h.count(eventsQuery), h.count(auditQuery)
	rr := h.resolve("activated")
	if rr.Code != http.StatusConflict || rr.Body.String() != "this environment was already resolved with a different outcome\n" {
		t.Fatalf("conflicting replay = %d %q", rr.Code, rr.Body.String())
	}
	if rr = h.resolve("not_activated"); rr.Code != http.StatusSeeOther {
		t.Fatalf("same-outcome replay = %d %q", rr.Code, rr.Body.String())
	}
	if h.count(eventsQuery) != events || h.count(auditQuery) != audits {
		t.Fatal("a replay wrote an audit row or a job event")
	}
}

// RouteMoved: since step 16h AssignRouteHostname refuses a hostname change
// while the head is flagged, so the move is forced in SQL (a revision bump with
// the flag untouched, the shape any future writer of a flagged head would
// leave); the refusal itself is covered by
// TestAssignRouteHostnameRefusedWhileReconciliationRequired.
func TestResolveFormEndToEndRouteMoved(t *testing.T) {
	env := postgres.NewActivationBlockForTest(t, 200, false)
	h := newResolveHarness(t, env)
	heads := postgres.NewRouteHeadRepository(env.Pool)
	head, found, err := heads.GetRouteHead(context.Background(), env.Principal, env.EnvironmentID)
	if err != nil || !found {
		t.Fatalf("GetRouteHead found=%v err=%v", found, err)
	}
	rev := head.RouteRevision
	if _, err = heads.AssignRouteHostname(context.Background(), env.Principal, env.EnvironmentID, "moved.example.com", &rev); !errors.Is(err, postgres.ErrRouteHeadReconciliationRequired) {
		t.Fatalf("AssignRouteHostname on the blocked head: %v, want ErrRouteHeadReconciliationRequired", err)
	}
	if _, err = env.Pool.Exec(context.Background(), `UPDATE route_heads SET hostname='moved.example.com', route_revision=route_revision+1, updated_at=clock_timestamp() WHERE environment_id=$1`, env.EnvironmentID); err != nil {
		t.Fatalf("force the route move: %v", err)
	}
	events, audits := h.count(eventsQuery), h.count(auditQuery)
	rr := h.resolve("activated")
	if rr.Code != http.StatusConflict || rr.Body.String() != "the route moved since the activation was authorized, so activated is impossible; declare not_activated\n" {
		t.Fatalf("activated after a moved route = %d %q", rr.Code, rr.Body.String())
	}
	if !h.listed() || h.count(eventsQuery) != events || h.count(auditQuery) != audits {
		t.Fatal("the refusal changed state")
	}
	if rr = h.resolve("not_activated"); rr.Code != http.StatusSeeOther || h.listed() {
		t.Fatalf("not_activated after a moved route = %d, listed=%v", rr.Code, h.listed())
	}
}
