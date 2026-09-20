package agenthttp

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"autodeploy/internal/githubapp"
	"autodeploy/internal/queue"
	"autodeploy/internal/server"
	"autodeploy/internal/store/postgres"
)

// --- fakes -----------------------------------------------------------------

type fakeCheckoutAuthorizer struct {
	// mu guards every field below: the concurrency tests drive many requests
	// through one fake at once, so an unguarded counter is a race in the test
	// rather than in the handler.
	mu     sync.Mutex
	calls  int
	result queue.CheckoutAuthorization
	err    error
	gotID  string
	gotAtt int64
	gotFen int64
}

func (f *fakeCheckoutAuthorizer) AuthorizeCheckout(_ context.Context, serverID string, _ [32]byte, attemptID, fence int64) (queue.CheckoutAuthorization, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.gotID, f.gotAtt, f.gotFen = serverID, attemptID, fence
	return f.result, f.err
}

func (f *fakeCheckoutAuthorizer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeCheckoutStore implements the full githubapp.ReservationStore method set
// so it can back a real *githubapp.CheckoutService (giving handler tests a
// genuine *githubapp.CheckoutLease to exercise, since that type's fields are
// private outside its own package), while also satisfying
// CheckoutReservationStore directly for the Handler.CheckoutStore field.
type fakeCheckoutStore struct {
	mu            sync.Mutex
	reserveResult githubapp.CheckoutReservation
	reserveErr    error
	beginBinding  githubapp.CheckoutBinding
	beginErr      error
	activateErr   error
	startErr      error
	startCalls    int
	closeErr      error
	closeCalls    []string
}

func (f *fakeCheckoutStore) ReserveCheckout(_ context.Context, _, _, _ string) (githubapp.CheckoutReservation, error) {
	return f.reserveResult, f.reserveErr
}
func (f *fakeCheckoutStore) BeginMintAttempt(_ context.Context, _ string) (githubapp.CheckoutBinding, error) {
	return f.beginBinding, f.beginErr
}
func (f *fakeCheckoutStore) ActivateMintAttempt(_ context.Context, _ string, _ githubapp.CheckoutBinding) error {
	return f.activateErr
}
func (f *fakeCheckoutStore) StartCheckout(_ context.Context, _ string, _ githubapp.CheckoutBinding) error {
	f.mu.Lock()
	f.startCalls++
	err := f.startErr
	f.mu.Unlock()
	return err
}
func (f *fakeCheckoutStore) CloseCheckout(_ context.Context, id string) error {
	f.mu.Lock()
	f.closeCalls = append(f.closeCalls, id)
	err := f.closeErr
	f.mu.Unlock()
	return err
}

func (f *fakeCheckoutStore) starts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.startCalls
}

func (f *fakeCheckoutStore) closes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.closeCalls...)
}

// fakeCheckoutProvider implements githubapp.Provider. Only MintCheckoutToken
// and RevokeToken are ever exercised by Activate/Close; the rest exist only
// to satisfy the interface.
type fakeCheckoutProvider struct {
	token     githubapp.Token
	mintErr   error
	revokeErr error
	// mu guards the counters: the concurrency tests below drive several
	// requests at once through one provider.
	mu          sync.Mutex
	mintCalls   int
	revokeCalls []string
}

// mints reports how many checkout tokens were minted, which is the real
// measure of "did activation happen more than once".
func (f *fakeCheckoutProvider) mints() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mintCalls
}

func (f *fakeCheckoutProvider) GetApp(context.Context) (githubapp.App, error) {
	return githubapp.App{}, errors.New("not implemented")
}
func (f *fakeCheckoutProvider) GetInstallation(context.Context, int64) (githubapp.Installation, error) {
	return githubapp.Installation{}, errors.New("not implemented")
}
func (f *fakeCheckoutProvider) ListInstallationRepositories(context.Context, int64) ([]githubapp.Repository, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeCheckoutProvider) MintDiscoveryToken(context.Context, int64) (githubapp.Token, error) {
	return githubapp.Token{}, errors.New("not implemented")
}
func (f *fakeCheckoutProvider) MintCheckoutToken(context.Context, int64, []int64) (githubapp.Token, error) {
	f.mu.Lock()
	f.mintCalls++
	f.mu.Unlock()
	return f.token, f.mintErr
}
func (f *fakeCheckoutProvider) RevokeToken(_ context.Context, token string) error {
	f.mu.Lock()
	f.revokeCalls = append(f.revokeCalls, token)
	f.mu.Unlock()
	return f.revokeErr
}

// revokes returns a copy of the revoked tokens, taken under the lock.
func (f *fakeCheckoutProvider) revokes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.revokeCalls...)
}

// --- fixture -----------------------------------------------------------------

const (
	fixtureOwnerID   = "owner"
	fixtureProjectID = "project"
	fixtureEnvID     = "env"
	fixtureDeployID  = "dep"
	fixtureRepoID    = int64(201)
	fixtureCommitSHA = "0123456789abcdef0123456789abcdef01234567"
	fixtureAttemptID = int64(7)
	fixtureFence     = int64(3)
	fixtureReservID  = "reservation-1"
	fixtureInstallID = int64(101)
)

func checkoutAuthorization() queue.CheckoutAuthorization {
	return queue.CheckoutAuthorization{
		AttemptID: fixtureAttemptID, Fence: fixtureFence, JobID: 1,
		OwnerID: fixtureOwnerID, ProjectID: fixtureProjectID, EnvironmentID: fixtureEnvID, DeploymentID: fixtureDeployID,
		RepositoryID: fixtureRepoID, CommitSHA: fixtureCommitSHA,
	}
}

func checkoutBinding() githubapp.CheckoutBinding {
	return githubapp.CheckoutBinding{
		OwnerID: fixtureOwnerID, ProjectID: fixtureProjectID, InstallationID: fixtureInstallID, RepositoryID: fixtureRepoID,
		CloneURL: "https://github.com/owner/repo.git", CommitSHA: fixtureCommitSHA, ReservationID: fixtureReservID,
		AuthorizationRevision: 1,
	}
}

// checkoutFixture wires a real *githubapp.CheckoutService over fake
// dependencies so tests exercise the genuine *githubapp.CheckoutLease type
// Handler.CheckoutActivator returns, rather than a hand-rolled substitute.
type checkoutFixture struct {
	authorizer *fakeCheckoutAuthorizer
	store      *fakeCheckoutStore
	provider   *fakeCheckoutProvider
	service    *githubapp.CheckoutService
	registry   *CheckoutRegistry
}

func newCheckoutFixture(t *testing.T) *checkoutFixture {
	t.Helper()
	store := &fakeCheckoutStore{
		reserveResult: githubapp.CheckoutReservation{CheckoutBinding: checkoutBinding(), ID: fixtureReservID, ExpiresAt: time.Now().Add(10 * time.Minute), State: "reserved"},
		beginBinding:  checkoutBinding(),
	}
	provider := &fakeCheckoutProvider{token: githubapp.Token{
		Value: "installation-token-value", ExpiresAt: time.Now().Add(50 * time.Minute),
		RepositoryIDs: []int64{fixtureRepoID}, Contents: "read", Permissions: map[string]string{"contents": "read"},
	}}
	service, err := githubapp.NewCheckoutService(store, provider)
	if err != nil {
		t.Fatal(err)
	}
	return &checkoutFixture{
		authorizer: &fakeCheckoutAuthorizer{result: checkoutAuthorization()},
		store:      store,
		provider:   provider,
		service:    service,
		registry:   NewCheckoutRegistry(),
	}
}

func (f *checkoutFixture) handler() Handler {
	return Handler{CheckoutAuthorizer: f.authorizer, CheckoutStore: f.store, CheckoutActivator: f.service, CheckoutRegistry: f.registry}
}

func checkoutTestCredential(t *testing.T) string {
	t.Helper()
	tok, err := server.NewCredentialToken(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return bearerPrefix + string(tok)
}

func checkoutBeginBody(attemptID, fence int64) string {
	b, _ := json.Marshal(checkoutBeginRequest{AttemptID: attemptID, Fence: fence})
	return string(b)
}
func checkoutStartBody(attemptID, fence int64, reservationID string) string {
	b, _ := json.Marshal(checkoutStartRequest{AttemptID: attemptID, Fence: fence, ReservationID: reservationID})
	return string(b)
}
func checkoutCloseBody(attemptID, fence int64, reservationID string) string {
	b, _ := json.Marshal(checkoutCloseRequest{AttemptID: attemptID, Fence: fence, ReservationID: reservationID})
	return string(b)
}

// --- begin -------------------------------------------------------------------

func TestCheckoutBeginNeverReturnsAToken(t *testing.T) {
	f := newCheckoutFixture(t)
	cred := checkoutTestCredential(t)
	r := newRequestTLS(newRequest(t, pathCheckoutBegin, checkoutBeginBody(fixtureAttemptID, fixtureFence), cred, "server_1"))
	w := do(f.handler(), r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "token") {
		t.Fatalf("begin response must never carry a token: %s", w.Body.String())
	}
	var resp checkoutBeginResponse
	decodeBody(t, w, &resp)
	if resp.ReservationID != fixtureReservID || resp.Binding.CloneURL == "" || resp.Binding.CommitSHA != fixtureCommitSHA {
		t.Fatalf("response=%+v", resp)
	}
	if f.authorizer.gotAtt != fixtureAttemptID || f.authorizer.gotFen != fixtureFence || f.authorizer.gotID != "server_1" {
		t.Fatalf("authorizer got attempt=%d fence=%d server=%s", f.authorizer.gotAtt, f.authorizer.gotFen, f.authorizer.gotID)
	}
}

func TestCheckoutBeginRefusesRepositoryMismatch(t *testing.T) {
	f := newCheckoutFixture(t)
	mismatched := checkoutBinding()
	mismatched.RepositoryID = 999
	f.store.reserveResult = githubapp.CheckoutReservation{CheckoutBinding: mismatched, ID: fixtureReservID, State: "reserved"}
	cred := checkoutTestCredential(t)
	r := newRequestTLS(newRequest(t, pathCheckoutBegin, checkoutBeginBody(fixtureAttemptID, fixtureFence), cred, "server_1"))
	w := do(f.handler(), r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503 on a repository/commit mismatch", w.Code)
	}
	if len(f.store.closes()) != 1 || f.store.closes()[0] != fixtureReservID {
		t.Fatalf("closeCalls=%v, want the mismatched reservation closed", f.store.closes())
	}
}

func TestCheckoutBeginMapsAuthorizationDenial(t *testing.T) {
	f := newCheckoutFixture(t)
	f.authorizer.err = postgres.ErrServerAuthorizationDenied
	cred := checkoutTestCredential(t)
	r := newRequestTLS(newRequest(t, pathCheckoutBegin, checkoutBeginBody(fixtureAttemptID, fixtureFence), cred, "server_1"))
	w := do(f.handler(), r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", w.Code)
	}
}

func TestCheckoutBeginRejectsInvalidRequest(t *testing.T) {
	f := newCheckoutFixture(t)
	cred := checkoutTestCredential(t)
	for _, body := range []string{
		`{"attempt_id":0,"fence":1}`,
		`{"attempt_id":1,"fence":0}`,
		`{"attempt_id":1,"fence":1,"bogus":true}`,
	} {
		r := newRequestTLS(newRequest(t, pathCheckoutBegin, body, cred, "server_1"))
		w := do(f.handler(), r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("body=%s status=%d, want 400", body, w.Code)
		}
	}
	if f.authorizer.callCount() != 0 {
		t.Fatalf("authorizer calls=%d, want 0 for rejected requests", f.authorizer.callCount())
	}
}

// --- start ---------------------------------------------------------------

// beginThenReturnFixture runs a real begin call and returns the fixture with
// its registry populated, mirroring the caller sequence start/close expect.
func beginThenReturnFixture(t *testing.T, serverID string) *checkoutFixture {
	t.Helper()
	f := newCheckoutFixture(t)
	cred := checkoutTestCredential(t)
	r := newRequestTLS(newRequest(t, pathCheckoutBegin, checkoutBeginBody(fixtureAttemptID, fixtureFence), cred, serverID))
	w := do(f.handler(), r)
	if w.Code != http.StatusOK {
		t.Fatalf("begin status=%d body=%s", w.Code, w.Body.String())
	}
	return f
}

func TestCheckoutStartReturnsTokenOnlyAfterSuccessfulStart(t *testing.T) {
	f := beginThenReturnFixture(t, "server_1")
	cred := checkoutTestCredential(t)
	r := newRequestTLS(newRequest(t, pathCheckoutStart, checkoutStartBody(fixtureAttemptID, fixtureFence, fixtureReservID), cred, "server_1"))
	w := do(f.handler(), r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp checkoutStartResponse
	decodeBody(t, w, &resp)
	if resp.Token == "" {
		t.Fatalf("expected a token once Start succeeded")
	}
	if resp.ExpiresAt == "" {
		t.Fatalf("expected an expiry once Start succeeded")
	}
	if f.store.starts() != 1 {
		t.Fatalf("startCalls=%d, want exactly 1", f.store.starts())
	}
}

func TestCheckoutStartReturnsNoTokenWhenStartFails(t *testing.T) {
	f := beginThenReturnFixture(t, "server_1")
	f.store.startErr = errors.New("boom")
	cred := checkoutTestCredential(t)
	r := newRequestTLS(newRequest(t, pathCheckoutStart, checkoutStartBody(fixtureAttemptID, fixtureFence, fixtureReservID), cred, "server_1"))
	w := do(f.handler(), r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503 when Start fails", w.Code)
	}
	if strings.Contains(w.Body.String(), "installation-token-value") {
		t.Fatalf("token must never appear when Start failed: %s", w.Body.String())
	}
	// The activated lease was closed, not left live with nothing that can use it.
	if len(f.provider.revokes()) != 1 {
		t.Fatalf("revokes=%v, want the never-started token revoked", f.provider.revokes())
	}
}

func TestCheckoutStartRejectsMismatchedCaller(t *testing.T) {
	f := beginThenReturnFixture(t, "server_1")
	cred := checkoutTestCredential(t)

	cases := []struct {
		name             string
		serverID         string
		attemptID, fence int64
	}{
		{"wrong server", "server_2", fixtureAttemptID, fixtureFence},
		{"wrong attempt", "server_1", fixtureAttemptID + 1, fixtureFence},
		{"stale fence", "server_1", fixtureAttemptID, fixtureFence + 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRequestTLS(newRequest(t, pathCheckoutStart, checkoutStartBody(tc.attemptID, tc.fence, fixtureReservID), cred, tc.serverID))
			w := do(f.handler(), r)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("%s: status=%d, want 401", tc.name, w.Code)
			}
		})
	}
	if f.store.starts() != 0 {
		t.Fatalf("startCalls=%d, want 0: no mismatched caller may drive this reservation", f.store.starts())
	}
}

func TestCheckoutStartRejectsUnknownReservation(t *testing.T) {
	f := newCheckoutFixture(t) // no begin call, so the registry is empty
	cred := checkoutTestCredential(t)
	r := newRequestTLS(newRequest(t, pathCheckoutStart, checkoutStartBody(fixtureAttemptID, fixtureFence, "never-begun"), cred, "server_1"))
	w := do(f.handler(), r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401 for an unknown reservation", w.Code)
	}
}

// --- close -----------------------------------------------------------------

func TestCheckoutCloseIsIdempotent(t *testing.T) {
	f := beginThenReturnFixture(t, "server_1")
	cred := checkoutTestCredential(t)
	// Start first so Close has a real lease to close.
	do(f.handler(), newRequestTLS(newRequest(t, pathCheckoutStart, checkoutStartBody(fixtureAttemptID, fixtureFence, fixtureReservID), cred, "server_1")))

	r1 := newRequestTLS(newRequest(t, pathCheckoutClose, checkoutCloseBody(fixtureAttemptID, fixtureFence, fixtureReservID), cred, "server_1"))
	if w := do(f.handler(), r1); w.Code != http.StatusOK {
		t.Fatalf("first close status=%d body=%s", w.Code, w.Body.String())
	}
	r2 := newRequestTLS(newRequest(t, pathCheckoutClose, checkoutCloseBody(fixtureAttemptID, fixtureFence, fixtureReservID), cred, "server_1"))
	if w := do(f.handler(), r2); w.Code != http.StatusOK {
		t.Fatalf("second (retried) close status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestCheckoutCloseSucceedsForBegunButNeverStarted(t *testing.T) {
	f := beginThenReturnFixture(t, "server_1")
	cred := checkoutTestCredential(t)
	r := newRequestTLS(newRequest(t, pathCheckoutClose, checkoutCloseBody(fixtureAttemptID, fixtureFence, fixtureReservID), cred, "server_1"))
	w := do(f.handler(), r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want a clean close for a reservation that was never started", w.Code, w.Body.String())
	}
	if len(f.store.closes()) != 1 {
		t.Fatalf("closeCalls=%v, want exactly one direct store close (no token to revoke)", f.store.closes())
	}
	if len(f.provider.revokes()) != 0 {
		t.Fatalf("revokes=%v, want none: no token was ever minted", f.provider.revokes())
	}
}

func TestCheckoutCloseRejectsMismatchedCaller(t *testing.T) {
	f := beginThenReturnFixture(t, "server_1")
	cred := checkoutTestCredential(t)
	r := newRequestTLS(newRequest(t, pathCheckoutClose, checkoutCloseBody(fixtureAttemptID, fixtureFence, fixtureReservID), cred, "server_2"))
	w := do(f.handler(), r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401 for another server's reservation", w.Code)
	}
	if len(f.store.closes()) != 0 {
		t.Fatalf("closeCalls=%v, want 0: a mismatched caller must not close someone else's reservation", f.store.closes())
	}
}

// --- dependency-nil paths ----------------------------------------------------

func TestCheckoutDependencyNilPathsReturn503(t *testing.T) {
	cred := checkoutTestCredential(t)
	f := newCheckoutFixture(t)
	full := f.handler()

	cases := []struct {
		name string
		h    Handler
		path string
		body string
	}{
		{"begin without authorizer", withoutCheckoutAuthorizer(full), pathCheckoutBegin, checkoutBeginBody(fixtureAttemptID, fixtureFence)},
		{"begin without store", withoutCheckoutStore(full), pathCheckoutBegin, checkoutBeginBody(fixtureAttemptID, fixtureFence)},
		{"begin without registry", withoutCheckoutRegistry(full), pathCheckoutBegin, checkoutBeginBody(fixtureAttemptID, fixtureFence)},
		{"start without authorizer", withoutCheckoutAuthorizer(full), pathCheckoutStart, checkoutStartBody(fixtureAttemptID, fixtureFence, fixtureReservID)},
		{"start without activator", withoutCheckoutActivator(full), pathCheckoutStart, checkoutStartBody(fixtureAttemptID, fixtureFence, fixtureReservID)},
		{"start without registry", withoutCheckoutRegistry(full), pathCheckoutStart, checkoutStartBody(fixtureAttemptID, fixtureFence, fixtureReservID)},
		{"close without authorizer", withoutCheckoutAuthorizer(full), pathCheckoutClose, checkoutCloseBody(fixtureAttemptID, fixtureFence, fixtureReservID)},
		{"close without store", withoutCheckoutStore(full), pathCheckoutClose, checkoutCloseBody(fixtureAttemptID, fixtureFence, fixtureReservID)},
		{"close without registry", withoutCheckoutRegistry(full), pathCheckoutClose, checkoutCloseBody(fixtureAttemptID, fixtureFence, fixtureReservID)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRequestTLS(newRequest(t, tc.path, tc.body, cred, "server_1"))
			w := do(tc.h, r)
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("status=%d, want 503", w.Code)
			}
		})
	}
}

func withoutCheckoutAuthorizer(h Handler) Handler { h.CheckoutAuthorizer = nil; return h }
func withoutCheckoutStore(h Handler) Handler      { h.CheckoutStore = nil; return h }
func withoutCheckoutActivator(h Handler) Handler  { h.CheckoutActivator = nil; return h }
func withoutCheckoutRegistry(h Handler) Handler   { h.CheckoutRegistry = nil; return h }

func TestCheckoutRequiresCredential(t *testing.T) {
	f := newCheckoutFixture(t)
	r := newRequestTLS(newRequest(t, pathCheckoutBegin, checkoutBeginBody(fixtureAttemptID, fixtureFence), "", ""))
	w := do(f.handler(), r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401 without a credential", w.Code)
	}
	if f.authorizer.callCount() != 0 {
		t.Fatalf("authorizer calls=%d, want 0 without a credential", f.authorizer.callCount())
	}
}

// TestCheckoutStartIsOneShotPerReservation proves a second start cannot mint a
// second credential. Two tokens for one reservation would leave the loser live
// with nothing able to close it, so it would only stop working when it
// expired.
func TestCheckoutStartIsOneShotPerReservation(t *testing.T) {
	f := beginThenReturnFixture(t, "server_1")
	cred := checkoutTestCredential(t)
	body := checkoutStartBody(fixtureAttemptID, fixtureFence, fixtureReservID)

	first := do(f.handler(), newRequestTLS(newRequest(t, pathCheckoutStart, body, cred, "server_1")))
	if first.Code != http.StatusOK {
		t.Fatalf("first start status=%d body=%s", first.Code, first.Body.String())
	}
	second := do(f.handler(), newRequestTLS(newRequest(t, pathCheckoutStart, body, cred, "server_1")))
	if second.Code != http.StatusUnauthorized {
		t.Fatalf("second start status=%d, want 401", second.Code)
	}
	if f.provider.mints() != 1 {
		t.Fatalf("tokens minted=%d, want exactly 1", f.provider.mints())
	}
}

// TestCheckoutStartConcurrentCallsActivateOnce is the race the registry's
// claim exists for: without it both callers pass the ownership check, both
// activate, and one minted token is orphaned. Run this with -race.
func TestCheckoutStartConcurrentCallsActivateOnce(t *testing.T) {
	f := beginThenReturnFixture(t, "server_1")
	cred := checkoutTestCredential(t)
	body := checkoutStartBody(fixtureAttemptID, fixtureFence, fixtureReservID)
	handler := f.handler()

	const callers = 8
	codes := make(chan int, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := do(handler, newRequestTLS(newRequest(t, pathCheckoutStart, body, cred, "server_1")))
			codes <- w.Code
		}()
	}
	wg.Wait()
	close(codes)
	okCount := 0
	for code := range codes {
		switch code {
		case http.StatusOK:
			okCount++
		case http.StatusUnauthorized:
		default:
			t.Fatalf("unexpected status %d", code)
		}
	}
	if okCount != 1 {
		t.Fatalf("successful starts=%d, want exactly 1", okCount)
	}
	if f.provider.mints() != 1 {
		t.Fatalf("tokens minted=%d, want exactly 1", f.provider.mints())
	}
}

// TestCheckoutCloseConcurrentWithStartIsRaceFree exercises close and start
// against one reservation concurrently, which is what forced match to return
// an immutable snapshot rather than the registry entry itself. Run with -race.
func TestCheckoutCloseConcurrentWithStartIsRaceFree(t *testing.T) {
	f := beginThenReturnFixture(t, "server_1")
	cred := checkoutTestCredential(t)
	handler := f.handler()
	startBody := checkoutStartBody(fixtureAttemptID, fixtureFence, fixtureReservID)
	closeBody := checkoutCloseBody(fixtureAttemptID, fixtureFence, fixtureReservID)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			do(handler, newRequestTLS(newRequest(t, pathCheckoutStart, startBody, cred, "server_1")))
		}()
		go func() {
			defer wg.Done()
			do(handler, newRequestTLS(newRequest(t, pathCheckoutClose, closeBody, cred, "server_1")))
		}()
	}
	wg.Wait()
	if f.provider.mints() > 1 {
		t.Fatalf("tokens minted=%d, want at most 1", f.provider.mints())
	}
}
