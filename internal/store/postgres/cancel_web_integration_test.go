//go:build integration

package postgres_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"autodeploy/internal/auth"
	"autodeploy/internal/store/postgres"
	"autodeploy/internal/web"
)

type cancelHarness struct {
	t       *testing.T
	fx      *postgres.CancelFixtureForTest
	handler http.Handler
	keys    auth.CSRFKeyRing
	p       auth.Principal
}

// newCancelHarness runs the real handler over the real operations read and
// lease repositories, acting as principal p (owner A or B).
func newCancelHarness(t *testing.T, fx *postgres.CancelFixtureForTest, p auth.Principal) cancelHarness {
	t.Helper()
	policy := auth.Argon2Policy{Revision: 1, MemoryKiB: 19 * 1024, Iterations: 2, Lanes: 1}
	dummy, err := auth.BuildDummyPasswordVerifier(policy)
	if err != nil {
		t.Fatal(err)
	}
	keys := auth.CSRFKeyRing{ActiveVersion: "v1", Keys: map[string][]byte{"v1": bytes.Repeat([]byte{1}, 32)}}
	throttle := auth.UsernameThrottleKeyRing{ActiveVersion: "v1", Keys: map[string][]byte{"v1": bytes.Repeat([]byte{2}, 32)}}
	h, err := web.New(resolveSessionRepository{principal: p}, web.Config{
		PublicOrigin: "https://admin.example.test", CSRFKeys: keys, UsernameThrottleKeys: throttle, AuthThrottleMaxRows: 8,
		PasswordPolicy: policy, DummyPassword: dummy,
		OperationsReader: fx.Ops, DeploymentCanceller: fx.Leases,
		Now: func() time.Time { return time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC) }, Random: bytes.NewReader(bytes.Repeat([]byte{3}, 512)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return cancelHarness{t: t, fx: fx, handler: h, keys: keys, p: p}
}

func (h cancelHarness) do(method, target string, form url.Values) *httptest.ResponseRecorder {
	h.t.Helper()
	var r *http.Request
	if form != nil {
		r = httptest.NewRequest(method, "https://admin.example.test"+target, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", "https://admin.example.test")
	} else {
		r = httptest.NewRequest(method, "https://admin.example.test"+target, nil)
	}
	r.AddCookie(&http.Cookie{Name: "__Host-ad_session", Value: "v1.AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"})
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, r)
	return rr
}

func (h cancelHarness) cancel() *httptest.ResponseRecorder {
	h.t.Helper()
	csrf, err := auth.NewSessionCSRF(h.keys, h.p.SessionID, "https://admin.example.test")
	if err != nil {
		h.t.Fatal(err)
	}
	return h.do(http.MethodPost, "/deployments/cancel", url.Values{"csrf": {csrf}, "deployment_id": {h.fx.DeploymentID}, "confirm": {"cancel"}})
}

func (h cancelHarness) mustRedirect(rr *httptest.ResponseRecorder, what string) {
	h.t.Helper()
	want := "/deployments/detail?id=" + url.QueryEscape(h.fx.DeploymentID)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != want {
		h.t.Fatalf("%s = %d %q %q", what, rr.Code, rr.Header().Get("Location"), rr.Body.String())
	}
}

func TestCancelWebQueuedJobIsCancelled(t *testing.T) {
	fx := postgres.NewCancelFixtureForTest(t, 180)
	h := newCancelHarness(t, fx, fx.Principal)
	page := h.do(http.MethodGet, "/deployments/detail?id="+fx.DeploymentID, nil)
	if !strings.Contains(page.Body.String(), `action="/deployments/cancel"`) {
		t.Fatal("queued job detail page has no Cancel form")
	}
	h.mustRedirect(h.cancel(), "cancel queued")
	s := fx.State(t)
	if s.JobState != "cancelled" || !s.CancelRequested || s.DeploymentStatus != "cancelled" {
		t.Fatalf("state = %+v", s)
	}
	page = h.do(http.MethodGet, "/deployments/detail?id="+fx.DeploymentID, nil)
	if strings.Contains(page.Body.String(), `action="/deployments/cancel"`) || !strings.Contains(page.Body.String(), "cancellation requested") {
		t.Fatal("cancelled job page still offers Cancel or lacks the note")
	}
	// Double submit is idempotent and changes nothing.
	before := s
	h.mustRedirect(h.cancel(), "double submit")
	if after := fx.State(t); after != before {
		t.Fatalf("idempotent resubmit changed state: %+v -> %+v", before, after)
	}
}

func TestCancelWebLeasedJobIsCooperative(t *testing.T) {
	fx := postgres.NewCancelFixtureForTest(t, 182)
	fx.Lease(t)
	h := newCancelHarness(t, fx, fx.Principal)
	h.mustRedirect(h.cancel(), "cancel leased")
	s := fx.State(t)
	if s.JobState != "leased" || !s.CancelRequested {
		t.Fatalf("state = %+v", s)
	}
	h.mustRedirect(h.cancel(), "double submit")
	if after := fx.State(t); after != s {
		t.Fatalf("idempotent resubmit changed state: %+v -> %+v", s, after)
	}
	page := h.do(http.MethodGet, "/deployments/detail?id="+fx.DeploymentID, nil)
	if strings.Contains(page.Body.String(), `action="/deployments/cancel"`) || !strings.Contains(page.Body.String(), "cancellation requested") {
		t.Fatal("cancel-requested page still offers Cancel or lacks the note")
	}
}

func TestCancelWebForeignOwnerGets404AndNothingChanges(t *testing.T) {
	fx := postgres.NewCancelFixtureForTest(t, 184)
	before := fx.State(t)
	h := newCancelHarness(t, fx, fx.PrincipalB)
	rr := h.cancel()
	if rr.Code != http.StatusNotFound {
		t.Fatalf("foreign cancel = %d %q", rr.Code, rr.Body.String())
	}
	if after := fx.State(t); after != before {
		t.Fatalf("foreign cancel changed state: %+v -> %+v", before, after)
	}
}

func TestCancelWebPastActivationPointIsRefused(t *testing.T) {
	fx := postgres.NewCancelFixtureForTest(t, 186)
	fx.Activate(t)
	before := fx.State(t)
	if before.DeploymentStatus != "activating" {
		t.Fatalf("setup: %+v", before)
	}
	h := newCancelHarness(t, fx, fx.Principal)
	page := h.do(http.MethodGet, "/deployments/detail?id="+fx.DeploymentID, nil)
	if strings.Contains(page.Body.String(), `action="/deployments/cancel"`) {
		t.Fatal("activating deployment offers Cancel")
	}
	rr := h.cancel()
	if rr.Code != http.StatusConflict || rr.Body.String() != "this deployment is activating and can no longer be cancelled\n" {
		t.Fatalf("cancel past activation = %d %q", rr.Code, rr.Body.String())
	}
	if after := fx.State(t); after != before {
		t.Fatalf("refused cancel changed state: %+v -> %+v", before, after)
	}
}

// TestCancelWebConcurrentPostsOneTransition fires 10 concurrent cancel POSTs
// at one queued job: exactly one 'cancelled' event, every response a redirect.
func TestCancelWebConcurrentPostsOneTransition(t *testing.T) {
	fx := postgres.NewCancelFixtureForTest(t, 188)
	h := newCancelHarness(t, fx, fx.Principal)
	before := fx.State(t)
	const n = 10
	codes := make([]int, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			codes[i] = h.cancel().Code
		}(i)
	}
	close(start)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("concurrent cancels did not finish: deadlock")
	}
	for i, c := range codes {
		if c != http.StatusSeeOther {
			t.Fatalf("response %d = %d", i, c)
		}
	}
	s := fx.State(t)
	if s.JobState != "cancelled" || s.DeploymentStatus != "cancelled" || s.Events != before.Events+1 {
		t.Fatalf("state = %+v (events before %d)", s, before.Events)
	}
	var cancelledEvents int
	if err := fx.Pool.QueryRow(context.Background(), `SELECT count(*) FROM agent_job_events WHERE job_id=$1 AND event_type='cancelled'`, fx.JobID).Scan(&cancelledEvents); err != nil || cancelledEvents != 1 {
		t.Fatalf("cancelled events = %d err=%v", cancelledEvents, err)
	}
}

// TestCancelWebRacesClaim races the cancel POST against the real Claim of the
// same queued job: either end state must be internally consistent.
func TestCancelWebRacesClaim(t *testing.T) {
	fx := postgres.NewCancelFixtureForTest(t, 190)
	h := newCancelHarness(t, fx, fx.Principal)
	var wg sync.WaitGroup
	var code, claimed int
	var claimErr error
	start := make(chan struct{})
	wg.Add(2)
	go func() { defer wg.Done(); <-start; code = h.cancel().Code }()
	go func() { defer wg.Done(); <-start; claimed, claimErr = fx.Claim(context.Background()) }()
	close(start)
	wg.Wait()
	if claimErr != nil {
		t.Fatalf("claim: %v", claimErr)
	}
	if code != http.StatusSeeOther {
		t.Fatalf("cancel = %d", code)
	}
	s := fx.State(t)
	switch {
	case claimed == 0 && s.JobState == "cancelled" && s.DeploymentStatus == "cancelled":
	case claimed == 1 && s.JobState == "leased" && s.CancelRequested && s.DeploymentStatus != "cancelled":
	default:
		t.Fatalf("inconsistent end state: claimed=%d %+v", claimed, s)
	}
}
