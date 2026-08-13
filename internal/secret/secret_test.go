package secret

import (
	"context"
	"errors"
	"testing"

	"autodeploy/internal/crypto"
)

type testProvider struct{ key []byte }

func (p testProvider) Active() (string, []byte, error) {
	return "key-1", append([]byte(nil), p.key...), nil
}
func (p testProvider) Resolve(string) ([]byte, error) { return append([]byte(nil), p.key...), nil }

type testAuthorizer struct {
	err   error
	calls int
	last  AuthorizationRequest
}

func (a *testAuthorizer) AuthorizeSecret(_ context.Context, request AuthorizationRequest) error {
	a.calls++
	a.last = request
	return a.err
}

type testRepo struct {
	version                       Version
	reserve, create, replace, get int
	order                         []string
	err                           error
	audits                        []Audit
	auditErr                      error
	createErr                     error
	compromised                   Mutation
}

func (r *testRepo) AppendMaterializationAudit(_ context.Context, audit Audit) error {
	r.order = append(r.order, "audit")
	r.audits = append(r.audits, audit)
	return r.auditErr
}
func (r *testRepo) Rewrap(context.Context, Scope, string, uint64, crypto.Envelope) error {
	return r.err
}

func (r *testRepo) ReserveWrapInvocation(string) error {
	r.reserve++
	r.order = append(r.order, "reserve")
	return r.err
}
func (r *testRepo) Create(_ context.Context, m Mutation) error {
	r.create++
	r.order = append(r.order, "create")
	r.version = m.Version
	if r.createErr != nil {
		return r.createErr
	}
	return r.err
}
func (r *testRepo) Replace(_ context.Context, m Mutation) error {
	r.replace++
	r.order = append(r.order, "replace")
	r.version = m.Version
	return r.err
}
func (r *testRepo) ReplaceCompromised(_ context.Context, m Mutation) error {
	r.replace++
	r.order = append(r.order, "replace_compromised")
	r.version = m.Version
	r.compromised = m
	return r.err
}

func TestReplaceCompromisedCreatesFreshVersionWithoutRewrap(t *testing.T) {
	scope := testScope()
	old, provider := encryptedVersion(t, StateActive)
	repo := &testRepo{version: old}
	got, err := NewService(repo, provider, nil).ReplaceCompromisedWithContext(context.Background(), scope, "replacement", OperationContext{AuditID: "compromise-audit", ActorID: "incident-actor"}, []byte("replacement-value"))
	if err != nil || got.ID != "replacement" || repo.replace != 1 || len(repo.order) != 3 || repo.order[1] != "reserve" || repo.order[2] != "replace_compromised" {
		t.Fatalf("result=%#v err=%v order=%v", got, err, repo.order)
	}
	if repo.compromised.Audit.Action != "replaced_compromised" || repo.compromised.Audit.OldVersionID != "version" || repo.compromised.Audit.ActorID != "incident-actor" || repo.compromised.Audit.Result != "success" || repo.compromised.Version.Number != 2 || repo.compromised.Version.ID == old.ID || string(repo.compromised.Version.Envelope.Ciphertext) == string(old.Envelope.Ciphertext) {
		t.Fatalf("compromise mutation=%#v", repo.compromised)
	}
	if _, err := NewService(repo, provider, nil).ReplaceCompromisedWithContext(context.Background(), scope, "", OperationContext{AuditID: "bad", ActorID: "actor"}, nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("misuse=%v", err)
	}
}
func (r *testRepo) GetVersion(_ context.Context, _ Scope, _ string) (Version, error) {
	r.get++
	r.order = append(r.order, "get")
	if r.err != nil {
		return Version{}, r.err
	}
	return r.version, nil
}

func testScope() Scope {
	return Scope{OwnerID: "owner", ProjectID: "project", EnvironmentID: "env", SecretID: "secret", Purpose: "runtime", DeliveryScope: "runtime"}
}
func testService(repo *testRepo, auth *testAuthorizer) *Service {
	return NewService(repo, testProvider{key: []byte("01234567890123456789012345678901")}, auth)
}

func TestCreateReservesBeforePersistentWriteAndSupportsEmptyAndBound(t *testing.T) {
	for _, plaintext := range [][]byte{nil, make([]byte, crypto.MaxPlaintextBytes)} {
		repo := &testRepo{}
		_, err := testService(repo, nil).CreateWithContext(context.Background(), testScope(), "version", OperationContext{AuditID: "audit", ActorID: "actor"}, plaintext)
		if err != nil {
			t.Fatal(err)
		}
		if got := len(repo.order); got != 2 || repo.order[0] != "reserve" || repo.order[1] != "create" {
			t.Fatalf("order=%v", repo.order)
		}
	}
}
func TestCreateRejectsInvalidScopeAndOversize(t *testing.T) {
	repo := &testRepo{}
	scope := testScope()
	scope.Purpose = "bad"
	if _, err := testService(repo, nil).CreateWithContext(context.Background(), scope, "version", OperationContext{AuditID: "audit", ActorID: "actor"}, nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("purpose: %v", err)
	}
	if _, err := testService(repo, nil).CreateWithContext(context.Background(), testScope(), "version", OperationContext{AuditID: "audit", ActorID: "actor"}, make([]byte, crypto.MaxPlaintextBytes+1)); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("bound: %v", err)
	}
}
func TestMaterializeAuthorizesBeforeRepositoryAndDecrypts(t *testing.T) {
	scope := testScope()
	provider := testProvider{key: []byte("01234567890123456789012345678901")}
	seed := &testRepo{}
	envelope, err := crypto.Encrypt(provider, seed, crypto.Scope{OwnerID: scope.OwnerID, ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID, VariableID: scope.SecretID, SecretVersionID: "version", Purpose: scope.Purpose, DeliveryScope: scope.DeliveryScope}, []byte("value"))
	if err != nil {
		t.Fatal(err)
	}
	repo := &testRepo{version: Version{ID: "version", Scope: scope, Number: 1, State: StateActive, Envelope: envelope}}
	auth := &testAuthorizer{err: errors.New("no")}
	if _, err := testService(repo, auth).MaterializeAuthorized(context.Background(), AuthorizationRequest{Scope: scope, VersionID: "version", ActorID: "actor"}, "audit-materialize"); !errors.Is(err, ErrUnauthorized) || repo.get != 1 {
		t.Fatalf("authorization order err=%v gets=%d", err, repo.get)
	}
	auth.err = nil
	p, err := testService(repo, auth).MaterializeAuthorized(context.Background(), AuthorizationRequest{Scope: scope, VersionID: "version", ActorID: "actor"}, "audit-materialize")
	if err != nil || string(p) != "value" {
		t.Fatalf("materialize=%q,%v", p, err)
	}
}
func TestMaterializeSanitizesRepositoryAndCryptoFailures(t *testing.T) {
	scope := testScope()
	auth := &testAuthorizer{}
	repo := &testRepo{err: errors.New("ciphertext secret-value")}
	if _, err := testService(repo, auth).MaterializeAuthorized(context.Background(), AuthorizationRequest{Scope: scope, VersionID: "version", ActorID: "actor"}, "audit-materialize"); !errors.Is(err, ErrCorrupt) || err.Error() != "secret unavailable" {
		t.Fatalf("repo err=%v", err)
	}
	repo.err = nil
	repo.version = Version{ID: "version", Scope: scope, Number: 1, State: StateActive, Envelope: crypto.Envelope{Suite: crypto.SuiteAES256GCMRandomNonceV1, AADVersion: crypto.AADFormatV1, KEKID: "key-1", Ciphertext: make([]byte, 28), WrappedDEK: make([]byte, 60)}}
	if _, err := testService(repo, auth).MaterializeAuthorized(context.Background(), AuthorizationRequest{Scope: scope, VersionID: "version", ActorID: "actor"}, "audit-materialize"); !errors.Is(err, ErrCorrupt) || err.Error() != "secret unavailable" {
		t.Fatalf("crypto err=%v", err)
	}
}

func TestMaterializeRejectsNonActiveVersion(t *testing.T) {
	scope := testScope()
	repo := &testRepo{version: Version{ID: "version", Scope: scope, Number: 1, State: StateSuperseded}}
	if _, err := testService(repo, &testAuthorizer{}).MaterializeAuthorized(context.Background(), AuthorizationRequest{Scope: scope, VersionID: "version", ActorID: "actor"}, "audit-materialize"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("superseded materialization = %v", err)
	}
}

func encryptedVersion(t *testing.T, state State) (Version, testProvider) {
	t.Helper()
	scope := testScope()
	provider := testProvider{key: []byte("01234567890123456789012345678901")}
	seed := &testRepo{}
	envelope, err := crypto.Encrypt(provider, seed, crypto.Scope{OwnerID: scope.OwnerID, ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID, VariableID: scope.SecretID, SecretVersionID: "version", Purpose: scope.Purpose, DeliveryScope: scope.DeliveryScope}, []byte("secret-value"))
	if err != nil {
		t.Fatal(err)
	}
	return Version{ID: "version", Scope: scope, Number: 1, State: state, Envelope: envelope}, provider
}
func TestMaterializeRequestAndAuditOutcomes(t *testing.T) {
	for name, testCase := range map[string]struct {
		state     State
		authErr   error
		tamper    bool
		wantAudit string
	}{
		"denied": {StateActive, errors.New("deny"), false, "denied"}, "failed": {StateActive, nil, true, "failed"}, "frozen": {StateSuperseded, nil, false, "success"},
	} {
		t.Run(name, func(t *testing.T) {
			v, p := encryptedVersion(t, testCase.state)
			if testCase.tamper {
				v.Envelope.Ciphertext[0] ^= 1
			}
			repo := &testRepo{version: v}
			auth := &testAuthorizer{err: testCase.authErr}
			request := AuthorizationRequest{Scope: v.Scope, VersionID: v.ID, ActorID: "actor", DeploymentID: "deployment", LeaseID: "lease", Frozen: name == "frozen"}
			got, err := NewService(repo, p, auth).MaterializeAuthorized(context.Background(), request, "audit-outcome")
			if name == "frozen" {
				if err != nil || string(got) != "secret-value" {
					t.Fatalf("frozen=%q,%v", got, err)
				}
			} else if err == nil {
				t.Fatal("materialization succeeded")
			}
			if auth.last != request {
				t.Fatalf("authorization request=%#v", auth.last)
			}
			if len(repo.audits) != 1 || repo.audits[0].Result != testCase.wantAudit {
				t.Fatalf("audits=%#v", repo.audits)
			}
		})
	}
}
func TestMaterializeAuditFailureSuppressesPlaintextAndCreateFailureConsumesReservation(t *testing.T) {
	v, p := encryptedVersion(t, StateActive)
	repo := &testRepo{version: v, auditErr: errors.New("audit failure secret-value")}
	got, err := NewService(repo, p, &testAuthorizer{}).MaterializeAuthorized(context.Background(), AuthorizationRequest{Scope: v.Scope, VersionID: v.ID, ActorID: "actor"}, "audit-fail")
	if got != nil || !errors.Is(err, ErrCorrupt) {
		t.Fatalf("audit=%q,%v", got, err)
	}
	repo = &testRepo{createErr: errors.New("write secret-value")}
	_, err = NewService(repo, p, nil).CreateWithContext(context.Background(), testScope(), "new-version", OperationContext{AuditID: "audit-create", ActorID: "actor"}, []byte("secret-value"))
	if !errors.Is(err, ErrCorrupt) || repo.reserve != 1 || repo.create != 1 {
		t.Fatalf("create failure=%v reserve=%d create=%d", err, repo.reserve, repo.create)
	}
}

func TestReplaceSanitizesRepositoryErrorsAndRejectsInvalidInput(t *testing.T) {
	operation := OperationContext{AuditID: "audit", ActorID: "actor"}
	for name, testCase := range map[string]struct {
		scope     Scope
		id        string
		plaintext []byte
	}{
		"scope": {Scope{}, "version", nil}, "id": {testScope(), "", nil}, "bound": {testScope(), "version", make([]byte, crypto.MaxPlaintextBytes+1)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := testService(&testRepo{}, nil).ReplaceWithContext(context.Background(), testCase.scope, testCase.id, operation, testCase.plaintext); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("invalid=%v", err)
			}
		})
	}
	repo := &testRepo{err: errors.New("postgres secret-value")}
	_, err := testService(repo, nil).ReplaceWithContext(context.Background(), testScope(), "version", operation, nil)
	if !errors.Is(err, ErrCorrupt) || err.Error() != "secret unavailable" {
		t.Fatalf("raw replace error=%v", err)
	}
}
