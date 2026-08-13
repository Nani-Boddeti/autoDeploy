package githubapp

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"autodeploy/internal/auth"
)

type setupStoreStub struct {
	created, begun, cancelled, reserved, completed, bound, disabled int
	lastState, lastHandoff                                          [32]byte
	pending                                                         Installation
	err                                                             error
}

func (s *setupStoreStub) CreateSetupState(_ context.Context, in SetupStateInput) error {
	s.created++
	s.lastState = in.Digest
	return s.err
}
func (s *setupStoreStub) BeginCallback(_ context.Context, _ [32]byte) error {
	s.begun++
	return s.err
}
func (s *setupStoreStub) CancelCallback(_ context.Context, _ [32]byte) error {
	s.cancelled++
	return nil
}
func (s *setupStoreStub) ReserveCallback(_ context.Context, state, handoff [32]byte, _ Installation) error {
	s.reserved++
	s.lastState = state
	s.lastHandoff = handoff
	return s.err
}
func (s *setupStoreStub) PendingInstallation(_ context.Context, _ [32]byte, _ auth.Principal) (Installation, error) {
	if s.err != nil {
		return Installation{}, s.err
	}
	return s.pending, nil
}
func (s *setupStoreStub) CompleteHandoff(_ context.Context, _ [32]byte, _ auth.Principal, _ Installation, _ []Repository) error {
	s.completed++
	return s.err
}
func (s *setupStoreStub) BindRepository(_ context.Context, _ auth.Principal, _ string, _ int64, _ string) error {
	s.bound++
	return s.err
}
func (s *setupStoreStub) DisableInstallation(_ context.Context, _ auth.Principal, _ int64, _ string) error {
	s.disabled++
	return s.err
}
func (s *setupStoreStub) ReconcileInstallation(_ context.Context, _ auth.Principal, _ Installation, _ []Repository, _ string) error {
	return s.err
}

type providerStub struct {
	app                                    App
	installation                           Installation
	repos                                  []Repository
	err                                    error
	appCalls, installationCalls, listCalls int
}

func (p *providerStub) GetApp(context.Context) (App, error) { p.appCalls++; return p.app, p.err }
func (p *providerStub) GetInstallation(context.Context, int64) (Installation, error) {
	p.installationCalls++
	return p.installation, p.err
}
func (p *providerStub) ListInstallationRepositories(context.Context, int64) ([]Repository, error) {
	p.listCalls++
	return p.repos, p.err
}
func (*providerStub) MintDiscoveryToken(context.Context, int64) (Token, error) {
	return Token{}, ErrProvider
}
func (*providerStub) MintCheckoutToken(context.Context, int64, []int64) (Token, error) {
	return Token{}, ErrProvider
}
func (*providerStub) RevokeToken(context.Context, string) error { return nil }
func testPrincipal() auth.Principal {
	return auth.Principal{UserID: "u", OwnerID: "o", SessionID: "s", AuthRevision: 1, Role: auth.RoleOwner}
}
func testProvider() *providerStub {
	owner := Owner{AccountID: 9, Type: "User"}
	return &providerStub{app: App{ID: 7, ClientID: "client", Slug: "app", Owner: owner, Permissions: map[string]string{"contents": "read", "metadata": "read"}, Events: []string{"push"}}, installation: Installation{ID: 11, Account: owner}, repos: []Repository{{ID: 12, OwnerID: 9, Name: "r", FullName: "owner/r"}}}
}
func testService(t *testing.T, store *setupStoreStub, p *providerStub) (*SetupService, *bytes.Reader) {
	t.Helper()
	r := bytes.NewReader(bytes.Repeat([]byte{1}, 128))
	s, e := NewSetupService(store, p, AppIdentity{AppID: 7, ClientID: "client", Slug: "app"}, Owner{AccountID: 9, Type: "User"}, r)
	if e != nil {
		t.Fatal(e)
	}
	return s, r
}

func TestSetupCapabilityDigestsAreDomainSeparated(t *testing.T) {
	v := "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"
	a, e := SetupDigest(v)
	if e != nil {
		t.Fatal(e)
	}
	b, e := HandoffDigest(v)
	if e != nil {
		t.Fatal(e)
	}
	if a == b {
		t.Fatal("state and handoff digests are interchangeable")
	}
	if _, e = SetupDigest("short"); e == nil {
		t.Fatal("malformed capability accepted")
	}
}
func TestSetupStartPersistsOnlyDigest(t *testing.T) {
	store := &setupStoreStub{}
	s, _ := testService(t, store, testProvider())
	state, e := s.Start(context.Background(), testPrincipal())
	if e != nil || len(state) != 43 || store.created != 1 {
		t.Fatalf("state=%q calls=%d err=%v", state, store.created, e)
	}
	if string(store.lastState[:]) == state {
		t.Fatal("raw state persisted")
	}
	if _, e = s.Start(context.Background(), auth.Principal{}); e == nil {
		t.Fatal("invalid principal started setup")
	}
}
func TestSetupCallbackVerifiesAppOwnerAndDoesNotComplete(t *testing.T) {
	store := &setupStoreStub{}
	p := testProvider()
	s, _ := testService(t, store, p)
	state := "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"
	handoff, e := s.Callback(context.Background(), state, 11, "install")
	if e != nil || len(handoff) != 43 || store.begun != 1 || store.reserved != 1 || store.completed != 0 {
		t.Fatalf("handoff=%q begun=%d reserved=%d completed=%d err=%v", handoff, store.begun, store.reserved, store.completed, e)
	}
	p.installation.Suspended = true
	if _, e = s.Callback(context.Background(), state, 11, "install"); e == nil {
		t.Fatal("suspended installation accepted")
	}
	p.installation.Suspended = false
	p.installation.Account.AccountID = 99
	if _, e = s.Callback(context.Background(), state, 11, "install"); e == nil {
		t.Fatal("wrong owner accepted")
	}
	if _, e = s.Callback(context.Background(), state, 11, "update"); e == nil {
		t.Fatal("wrong action accepted")
	}
}
func TestSetupCallbackRejectsInvalidStateBeforeProviderCalls(t *testing.T) {
	store := &setupStoreStub{err: errors.New("invalid state")}
	p := testProvider()
	s, _ := testService(t, store, p)
	if _, err := s.Callback(context.Background(), "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE", 11, "install"); err == nil {
		t.Fatal("invalid state accepted")
	}
	if p.appCalls != 0 || p.installationCalls != 0 || store.reserved != 0 || store.cancelled != 0 {
		t.Fatalf("invalid state called provider app=%d installation=%d reserve=%d cancel=%d", p.appCalls, p.installationCalls, store.reserved, store.cancelled)
	}
}
func TestSetupCallbackProviderFailureReleasesLease(t *testing.T) {
	store := &setupStoreStub{}
	p := testProvider()
	p.err = errors.New("network")
	s, _ := testService(t, store, p)
	if _, err := s.Callback(context.Background(), "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE", 11, "install"); err == nil {
		t.Fatal("provider failure accepted")
	}
	if store.begun != 1 || store.cancelled != 1 || store.reserved != 0 {
		t.Fatalf("lease lifecycle begin=%d cancel=%d reserve=%d", store.begun, store.cancelled, store.reserved)
	}
}
func TestSetupCallbackProviderFailureDoesNotReserve(t *testing.T) {
	store := &setupStoreStub{}
	p := testProvider()
	p.err = errors.New("network")
	s, _ := testService(t, store, p)
	if _, e := s.Callback(context.Background(), "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE", 11, "install"); e == nil || store.reserved != 0 {
		t.Fatalf("err=%v reserve=%d", e, store.reserved)
	}
}
func TestSetupCompleteDiscoversThenConsumes(t *testing.T) {
	store := &setupStoreStub{pending: testProvider().installation}
	p := testProvider()
	s, _ := testService(t, store, p)
	if e := s.Complete(context.Background(), "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE", testPrincipal()); e != nil || store.completed != 1 || p.listCalls != 1 {
		t.Fatalf("err=%v complete=%d list=%d", e, store.completed, p.listCalls)
	}
	store.completed = 0
	p.err = errors.New("transient")
	if e := s.Complete(context.Background(), "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE", testPrincipal()); e == nil || store.completed != 0 {
		t.Fatal("provider failure consumed handoff")
	}
}
func TestSetupReconcileDisablesOnProviderAuthorityLoss(t *testing.T) {
	store := &setupStoreStub{}
	provider := testProvider()
	provider.app.Permissions = map[string]string{"metadata": "read"}
	service, _ := testService(t, store, provider)
	if err := service.ReconcileInstallation(context.Background(), testPrincipal(), 11); err != nil {
		t.Fatalf("reconcile authority loss: %v", err)
	}
	if store.disabled != 1 || provider.installationCalls != 0 {
		t.Fatalf("disable=%d installation calls=%d", store.disabled, provider.installationCalls)
	}
}
