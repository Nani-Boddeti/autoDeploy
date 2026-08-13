// Package secret coordinates authorized encrypted secret version operations.
package secret

import (
	"context"
	"errors"
	"time"

	"autodeploy/internal/crypto"
)

var (
	ErrInvalidInput = errors.New("invalid secret input")
	ErrUnauthorized = errors.New("secret access denied")
	ErrCorrupt      = errors.New("secret unavailable")
)

type State string

const (
	StateActive     State = "active"
	StateSuperseded State = "superseded"
	StateRevoked    State = "revoked"
)

type Scope struct {
	OwnerID, ProjectID, EnvironmentID, SecretID string
	Purpose, DeliveryScope                      string
}
type Version struct {
	ID               string
	Scope            Scope
	Number           uint64
	State            State
	Envelope         crypto.Envelope
	EnvelopeRevision uint64
	CreatedAt        time.Time
}

// Metadata deliberately excludes encrypted envelope fields and is the only
// mutation result exposed beyond this package's persistence boundary.
type Metadata struct {
	ID     string
	Scope  Scope
	Number uint64
	State  State
}
type Audit struct {
	ID                                                                     string
	OwnerID                                                                string
	SecretID                                                               string
	VersionID                                                              string
	Action                                                                 string
	ActorID, ServiceID, DeploymentID, LeaseID, Result, OldVersionID, KEKID string
}
type Mutation struct {
	Version Version
	Audit   Audit
}

// OperationContext is authenticated request metadata; it contains no secret
// value and makes audit identity/correlation explicit for every mutation.
type OperationContext struct{ AuditID, ActorID, ServiceID, DeploymentID, LeaseID string }

// Repository keeps ciphertext only. ReserveWrapInvocation must commit before Encrypt is invoked.
type Repository interface {
	crypto.WrapInvocationReserver
	Create(context.Context, Mutation) error
	Replace(context.Context, Mutation) error
	ReplaceCompromised(context.Context, Mutation) error
	GetVersion(context.Context, Scope, string) (Version, error)
	Rewrap(context.Context, Scope, string, uint64, crypto.Envelope) error
	AppendMaterializationAudit(context.Context, Audit) error
}
type AuthorizationRequest struct {
	Scope                                                Scope
	VersionID, ActorID, ServiceID, DeploymentID, LeaseID string
	Frozen                                               bool
}
type Authorizer interface {
	AuthorizeSecret(context.Context, AuthorizationRequest) error
}

type Service struct {
	repository Repository
	provider   crypto.KeyProvider
	authorizer Authorizer
}

func NewService(repository Repository, provider crypto.KeyProvider, authorizer Authorizer) *Service {
	return &Service{repository: repository, provider: provider, authorizer: authorizer}
}

func (s *Service) CreateWithContext(ctx context.Context, scope Scope, versionID string, operation OperationContext, plaintext []byte) (Metadata, error) {
	if !validOperation(operation) {
		return Metadata{}, ErrInvalidInput
	}
	return s.create(ctx, scope, versionID, operation, plaintext)
}
func (s *Service) create(ctx context.Context, scope Scope, versionID string, operation OperationContext, plaintext []byte) (Metadata, error) {
	if err := validNew(s, scope, versionID, operation.AuditID, plaintext); err != nil {
		return Metadata{}, err
	}
	v := Version{ID: versionID, Scope: scope, Number: 1, State: StateActive}
	envelope, err := crypto.Encrypt(s.provider, s.repository, cryptoScope(v), plaintext)
	if err != nil {
		return Metadata{}, ErrCorrupt
	}
	v.Envelope = envelope
	if err := s.repository.Create(ctx, Mutation{Version: v, Audit: audit(operation, scope, versionID, "", "created", envelope.KEKID)}); err != nil {
		return Metadata{}, ErrCorrupt
	}
	return metadata(v), nil
}
func (s *Service) ReplaceWithContext(ctx context.Context, scope Scope, versionID string, operation OperationContext, plaintext []byte) (Metadata, error) {
	if !validOperation(operation) || !validScope(scope) || !validID(versionID) || len(plaintext) > crypto.MaxPlaintextBytes {
		return Metadata{}, ErrInvalidInput
	}
	return s.replace(ctx, scope, versionID, operation, plaintext)
}

// ReplaceCompromisedWithContext creates a wholly new immutable version and
// revokes the active compromised version. It never rewraps existing ciphertext.
func (s *Service) ReplaceCompromisedWithContext(ctx context.Context, scope Scope, versionID string, operation OperationContext, plaintext []byte) (Metadata, error) {
	if !validOperation(operation) || !validScope(scope) || !validID(versionID) || len(plaintext) > crypto.MaxPlaintextBytes {
		return Metadata{}, ErrInvalidInput
	}
	if err := validNew(s, scope, versionID, operation.AuditID, plaintext); err != nil {
		return Metadata{}, err
	}
	old, err := s.repository.GetVersion(ctx, scope, "")
	if err != nil || old.State != StateActive {
		return Metadata{}, ErrCorrupt
	}
	v := Version{ID: versionID, Scope: scope, Number: old.Number + 1, State: StateActive}
	envelope, err := crypto.Encrypt(s.provider, s.repository, cryptoScope(v), plaintext)
	if err != nil {
		return Metadata{}, ErrCorrupt
	}
	v.Envelope = envelope
	if err := s.repository.ReplaceCompromised(ctx, Mutation{Version: v, Audit: audit(operation, scope, versionID, old.ID, "replaced_compromised", envelope.KEKID)}); err != nil {
		return Metadata{}, ErrCorrupt
	}
	return metadata(v), nil
}
func (s *Service) replace(ctx context.Context, scope Scope, versionID string, operation OperationContext, plaintext []byte) (Metadata, error) {
	if err := validNew(s, scope, versionID, operation.AuditID, plaintext); err != nil {
		return Metadata{}, err
	}
	old, err := s.repository.GetVersion(ctx, scope, "")
	if err != nil {
		return Metadata{}, ErrCorrupt
	}
	if old.State != StateActive {
		return Metadata{}, ErrInvalidInput
	}
	v := Version{ID: versionID, Scope: scope, Number: old.Number + 1, State: StateActive}
	envelope, err := crypto.Encrypt(s.provider, s.repository, cryptoScope(v), plaintext)
	if err != nil {
		return Metadata{}, ErrCorrupt
	}
	v.Envelope = envelope
	if err := s.repository.Replace(ctx, Mutation{Version: v, Audit: audit(operation, scope, versionID, old.ID, "replaced", envelope.KEKID)}); err != nil {
		return Metadata{}, ErrCorrupt
	}
	return metadata(v), nil
}
func (s *Service) MaterializeAuthorized(ctx context.Context, request AuthorizationRequest, auditID string) ([]byte, error) {
	if s == nil || s.repository == nil || s.authorizer == nil || s.provider == nil || !validScope(request.Scope) || !validID(request.VersionID) || !validID(auditID) || !((validID(request.ActorID) && request.ServiceID == "") || (validID(request.ServiceID) && request.ActorID == "")) {
		return nil, ErrInvalidInput
	}
	if err := s.authorizer.AuthorizeSecret(ctx, request); err != nil {
		v, loadErr := s.repository.GetVersion(ctx, request.Scope, request.VersionID)
		if loadErr == nil && v.ID == request.VersionID && v.Scope == request.Scope {
			_ = s.repository.AppendMaterializationAudit(ctx, Audit{ID: auditID, OwnerID: request.Scope.OwnerID, SecretID: request.Scope.SecretID, VersionID: v.ID, Action: "materialized", ActorID: request.ActorID, ServiceID: request.ServiceID, DeploymentID: request.DeploymentID, LeaseID: request.LeaseID, Result: "denied", KEKID: v.Envelope.KEKID})
		}
		return nil, ErrUnauthorized
	}
	v, err := s.repository.GetVersion(ctx, request.Scope, request.VersionID)
	if err != nil || v.ID != request.VersionID || v.Scope != request.Scope {
		return nil, ErrCorrupt
	}
	if v.State != StateActive && !(request.Frozen && v.State == StateSuperseded) {
		return nil, ErrCorrupt
	}
	p, err := crypto.Decrypt(s.provider, cryptoScope(v), v.Envelope)
	if err != nil {
		_ = s.repository.AppendMaterializationAudit(ctx, Audit{ID: auditID, OwnerID: request.Scope.OwnerID, SecretID: request.Scope.SecretID, VersionID: v.ID, Action: "materialized", ActorID: request.ActorID, ServiceID: request.ServiceID, DeploymentID: request.DeploymentID, LeaseID: request.LeaseID, Result: "failed", KEKID: v.Envelope.KEKID})
		return nil, ErrCorrupt
	}
	if err := s.repository.AppendMaterializationAudit(ctx, Audit{ID: auditID, OwnerID: request.Scope.OwnerID, SecretID: request.Scope.SecretID, VersionID: v.ID, Action: "materialized", ActorID: request.ActorID, ServiceID: request.ServiceID, DeploymentID: request.DeploymentID, LeaseID: request.LeaseID, Result: "success", KEKID: v.Envelope.KEKID}); err != nil {
		clear(p)
		return nil, ErrCorrupt
	}
	return p, nil
}
func validNew(s *Service, scope Scope, versionID, auditID string, plaintext []byte) error {
	if s == nil || s.repository == nil || s.provider == nil || !validScope(scope) || !validID(versionID) || !validID(auditID) || len(plaintext) > crypto.MaxPlaintextBytes {
		return ErrInvalidInput
	}
	return nil
}
func validScope(s Scope) bool {
	return validID(s.OwnerID) && validID(s.ProjectID) && validID(s.EnvironmentID) && validID(s.SecretID) && (s.Purpose == "build" || s.Purpose == "runtime" || s.Purpose == "provider") && (s.DeliveryScope == "build" || s.DeliveryScope == "runtime")
}
func validID(v string) bool { return len(v) > 0 && len(v) <= 128 }
func cryptoScope(v Version) crypto.Scope {
	return crypto.Scope{OwnerID: v.Scope.OwnerID, ProjectID: v.Scope.ProjectID, EnvironmentID: v.Scope.EnvironmentID, VariableID: v.Scope.SecretID, SecretVersionID: v.ID, Purpose: v.Scope.Purpose, DeliveryScope: v.Scope.DeliveryScope}
}
func metadata(v Version) Metadata {
	return Metadata{ID: v.ID, Scope: v.Scope, Number: v.Number, State: v.State}
}
func validOperation(o OperationContext) bool {
	return validID(o.AuditID) && ((validID(o.ActorID) && o.ServiceID == "") || (validID(o.ServiceID) && o.ActorID == "")) && (o.DeploymentID == "" || validID(o.DeploymentID)) && (o.LeaseID == "" || validID(o.LeaseID))
}
func audit(o OperationContext, s Scope, version, old, action, kek string) Audit {
	return Audit{ID: o.AuditID, OwnerID: s.OwnerID, SecretID: s.SecretID, VersionID: version, Action: action, ActorID: o.ActorID, ServiceID: o.ServiceID, DeploymentID: o.DeploymentID, LeaseID: o.LeaseID, Result: "success", OldVersionID: old, KEKID: kek}
}
