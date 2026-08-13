package secret

import (
	"context"
	"errors"

	"autodeploy/internal/crypto"
)

const (
	DefaultRewrapBatchSize = 100
	MaxRewrapBatchSize     = 100
)

var (
	ErrRotationInput = errors.New("invalid secret rotation input")
	ErrRetirement    = errors.New("secret encryption key still in use")
)

// RotationRepository is intentionally separate from Repository so ordinary
// secret callers cannot enumerate encrypted envelopes.
type RotationRepository interface {
	crypto.WrapInvocationReserver
	ListVersionsByKEK(context.Context, string, string, int) ([]Version, error)
	Rewrap(context.Context, Scope, string, uint64, crypto.Envelope) error
	CountVersionsByKEK(context.Context, string) (int64, error)
}

type RewrapRequest struct {
	OldKEKID, Cursor string
	BatchSize        int
	DryRun           bool
}
type RewrapResult struct {
	NextCursor                     string
	Selected, Rewrapped, Conflicts int
}

type Rotator struct {
	repository RotationRepository
	provider   crypto.KeyProvider
}

func NewRotator(repository RotationRepository, provider crypto.KeyProvider) *Rotator {
	return &Rotator{repository: repository, provider: provider}
}

func (r *Rotator) RewrapBatch(ctx context.Context, request RewrapRequest) (RewrapResult, error) {
	if r == nil || r.repository == nil || r.provider == nil || !validRotationID(request.OldKEKID) || (request.Cursor != "" && !validID(request.Cursor)) {
		return RewrapResult{}, ErrRotationInput
	}
	if request.BatchSize == 0 {
		request.BatchSize = DefaultRewrapBatchSize
	}
	if request.BatchSize < 1 || request.BatchSize > MaxRewrapBatchSize {
		return RewrapResult{}, ErrRotationInput
	}
	active, key, err := r.provider.Active()
	if err != nil || !validRotationID(active) || len(key) != 32 || active == request.OldKEKID {
		clear(key)
		return RewrapResult{}, ErrRotationInput
	}
	clear(key)
	versions, err := r.repository.ListVersionsByKEK(ctx, request.OldKEKID, request.Cursor, request.BatchSize)
	if err != nil {
		return RewrapResult{}, ErrCorrupt
	}
	result := RewrapResult{Selected: len(versions)}
	for _, version := range versions {
		if version.Envelope.KEKID != request.OldKEKID || !validStoredRotationVersion(version) {
			return RewrapResult{}, ErrCorrupt
		}
		if request.DryRun {
			continue
		}
		envelope, rewrapErr := crypto.Rewrap(r.provider, r.repository, cryptoScope(version), version.Envelope)
		if rewrapErr != nil {
			return RewrapResult{}, ErrCorrupt
		}
		if err = r.repository.Rewrap(ctx, version.Scope, version.ID, version.EnvelopeRevision, envelope); err != nil {
			// The reservation is intentionally burnt even when this optimistic
			// update loses a race. Do not advance past this row: it remains an
			// old-KEK reference and must be retried before later rows.
			result.Conflicts++
			break
		}
		result.Rewrapped++
		result.NextCursor = version.ID
	}
	return result, nil
}

func (r *Rotator) CanRetire(ctx context.Context, kekID string) error {
	if r == nil || r.repository == nil || r.provider == nil || !validRotationID(kekID) {
		return ErrRotationInput
	}
	active, key, err := r.provider.Active()
	clear(key)
	if err != nil || !validRotationID(active) {
		return ErrRotationInput
	}
	if active == kekID {
		return ErrRetirement
	}
	n, err := r.repository.CountVersionsByKEK(ctx, kekID)
	if err != nil {
		return ErrCorrupt
	}
	if n != 0 {
		return ErrRetirement
	}
	return nil
}

func validRotationID(id string) bool { return validID(id) && len(id) <= 64 }
func validStoredRotationVersion(v Version) bool {
	return validScope(v.Scope) && validID(v.ID) && v.EnvelopeRevision > 0 && v.Envelope.Suite == crypto.SuiteAES256GCMRandomNonceV1 && v.Envelope.AADVersion == crypto.AADFormatV1 && len(v.Envelope.Ciphertext) >= 28 && len(v.Envelope.Ciphertext) <= crypto.MaxPlaintextBytes+28 && len(v.Envelope.WrappedDEK) == 60
}
