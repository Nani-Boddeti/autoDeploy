package githubapp

import (
	"context"
	"encoding/hex"
	"errors"
	"io"

	"autodeploy/internal/auth"
)

var ErrSetupUnavailable = errors.New("GitHub setup unavailable")

// SetupStore is deliberately capability-only: callback reservation cannot
// activate an installation or bind a project.
type SetupStore interface {
	CreateSetupState(context.Context, SetupStateInput) error
	BeginCallback(context.Context, [32]byte) error
	CancelCallback(context.Context, [32]byte) error
	ReserveCallback(context.Context, [32]byte, [32]byte, Installation) error
	PendingInstallation(context.Context, [32]byte, auth.Principal) (Installation, error)
	CompleteHandoff(context.Context, [32]byte, auth.Principal, Installation, []Repository) error
	BindRepository(context.Context, auth.Principal, string, int64, string) error
	DisableInstallation(context.Context, auth.Principal, int64, string) error
	ReconcileInstallation(context.Context, auth.Principal, Installation, []Repository, string) error
}
type SetupStateInput struct {
	Digest    [32]byte
	Principal auth.Principal
}

type SetupService struct {
	store    SetupStore
	provider Provider
	identity AppIdentity
	owner    Owner
	random   io.Reader
}

func NewSetupService(store SetupStore, provider Provider, identity AppIdentity, owner Owner, random io.Reader) (*SetupService, error) {
	if store == nil || provider == nil || !identity.Valid() || !owner.Valid() || random == nil {
		return nil, ErrInvalidIdentity
	}
	return &SetupService{store: store, provider: provider, identity: identity, owner: owner, random: random}, nil
}
func (s *SetupService) Start(ctx context.Context, p auth.Principal) (string, error) {
	if p.Validate() != nil {
		return "", ErrSetupUnavailable
	}
	state, err := NewCapability(s.random)
	if err != nil {
		return "", ErrSetupUnavailable
	}
	d, err := SetupDigest(state)
	if err != nil {
		return "", ErrSetupUnavailable
	}
	if err = s.store.CreateSetupState(ctx, SetupStateInput{Digest: d, Principal: p}); err != nil {
		return "", ErrSetupUnavailable
	}
	return state, nil
}
func (s *SetupService) Callback(ctx context.Context, state string, installationID int64, action string) (string, error) {
	if action != "install" || installationID <= 0 {
		return "", ErrSetupUnavailable
	}
	d, err := SetupDigest(state)
	if err != nil {
		return "", ErrSetupUnavailable
	}
	// Take a short-lived callback lease before provider calls. This bounds
	// replay-driven provider work while preserving retryability on failure.
	if err = s.store.BeginCallback(ctx, d); err != nil {
		return "", ErrSetupUnavailable
	}
	reserved := false
	defer func() {
		if !reserved {
			_ = s.store.CancelCallback(ctx, d)
		}
	}()
	app, err := s.provider.GetApp(ctx)
	if err != nil || VerifyApp(app, s.identity, s.owner) != nil {
		return "", ErrSetupUnavailable
	}
	installation, err := s.provider.GetInstallation(ctx, installationID)
	if err != nil || installation.ID != installationID || installation.Suspended || installation.Account != s.owner {
		return "", ErrSetupUnavailable
	}
	handoff, err := NewCapability(s.random)
	if err != nil {
		return "", ErrSetupUnavailable
	}
	hd, err := HandoffDigest(handoff)
	if err != nil {
		return "", ErrSetupUnavailable
	}
	if err = s.store.ReserveCallback(ctx, d, hd, installation); err != nil {
		return "", ErrSetupUnavailable
	}
	reserved = true
	return handoff, nil
}
func (s *SetupService) Complete(ctx context.Context, handoff string, p auth.Principal) error {
	d, err := HandoffDigest(handoff)
	if err != nil {
		return ErrSetupUnavailable
	}
	installation, err := s.store.PendingInstallation(ctx, d, p)
	if err != nil {
		return ErrSetupUnavailable
	} // Provider work precedes authority consumption so a transient error is retryable.
	app, err := s.provider.GetApp(ctx)
	if err != nil || VerifyApp(app, s.identity, s.owner) != nil {
		return ErrSetupUnavailable
	}
	actual, err := s.provider.GetInstallation(ctx, installation.ID)
	if err != nil || actual.ID != installation.ID || actual.Suspended || actual.Account != s.owner {
		return ErrSetupUnavailable
	}
	repositories, err := s.provider.ListInstallationRepositories(ctx, actual.ID)
	if err != nil {
		return ErrSetupUnavailable
	}
	if err = s.store.CompleteHandoff(ctx, d, p, actual, repositories); err != nil {
		return ErrSetupUnavailable
	}
	return nil
}

// ReconcileInstallation is an explicit, authenticated lifecycle entrypoint.
// It is intended for a trusted control-plane trigger; webhook delivery is
// deliberately out of scope. Provider loss/suspension fails closed by
// disabling the matching owner-scoped installation.
func (s *SetupService) ReconcileInstallation(ctx context.Context, p auth.Principal, installationID int64) error {
	if p.Validate() != nil || installationID <= 0 {
		return ErrSetupUnavailable
	}
	disable := func() error {
		capability, err := NewCapability(s.random)
		if err != nil {
			return ErrSetupUnavailable
		}
		digest, err := HandoffDigest(capability)
		if err != nil {
			return ErrSetupUnavailable
		}
		if err = s.store.DisableInstallation(ctx, p, installationID, lifecycleAuditID(digest)); err != nil {
			return ErrSetupUnavailable
		}
		return nil
	}
	app, err := s.provider.GetApp(ctx)
	if err != nil || VerifyApp(app, s.identity, s.owner) != nil {
		return disable()
	}
	installation, err := s.provider.GetInstallation(ctx, installationID)
	if err != nil || installation.ID != installationID || installation.Suspended || installation.Account != s.owner {
		return disable()
	}
	repositories, err := s.provider.ListInstallationRepositories(ctx, installationID)
	if err != nil {
		return ErrSetupUnavailable
	}
	capability, err := NewCapability(s.random)
	if err != nil {
		return ErrSetupUnavailable
	}
	digest, err := HandoffDigest(capability)
	if err != nil {
		return ErrSetupUnavailable
	}
	if err = s.store.ReconcileInstallation(ctx, p, installation, repositories, lifecycleAuditID(digest)); err != nil {
		return ErrSetupUnavailable
	}
	return nil
}

func lifecycleAuditID(digest [32]byte) string {
	return "gh_lifecycle_" + hex.EncodeToString(digest[:16])
}
func (s *SetupService) HandoffValid(ctx context.Context, handoff string, p auth.Principal) bool {
	d, err := HandoffDigest(handoff)
	if err != nil {
		return false
	}
	_, err = s.store.PendingInstallation(ctx, d, p)
	return err == nil
}
func (s *SetupService) Bind(ctx context.Context, p auth.Principal, projectID string, repositoryID int64, auditID string) error {
	if p.Validate() != nil {
		return ErrSetupUnavailable
	}
	if err := s.store.BindRepository(ctx, p, projectID, repositoryID, auditID); err != nil {
		return ErrSetupUnavailable
	}
	return nil
}
