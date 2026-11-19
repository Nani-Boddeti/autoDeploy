package secret

import (
	"context"
	"errors"

	"autodeploy/internal/crypto"
)

// Fixed failure reasons reported by Verify. They are the only failure detail
// an operator sees; error text is never surfaced.
const (
	VerifyKEKMissing      = "kek_missing"
	VerifyKEKInvalid      = "kek_invalid"
	VerifyUnwrapFailed    = "unwrap_failed"
	VerifyDecryptFailed   = "decrypt_failed"
	VerifyEnvelopeInvalid = "envelope_invalid"

	// MaxVerifyFailuresListed bounds memory when a whole database is broken.
	MaxVerifyFailuresListed = 10000
	verifyPageSize          = 100
)

// maxFailuresListed is a test seam for the listing cap.
var maxFailuresListed = MaxVerifyFailuresListed

var ErrGenerationMismatch = errors.New("secret database generation mismatch")

// VerifyRepository is read-only by construction: it exposes neither the wrap
// ledger nor any write, so verification cannot consume wrap invocations,
// append audit rows or change data.
type VerifyRepository interface {
	DatabaseGeneration(context.Context) (string, error)
	// ListDecryptableVersions returns versions the system can still decrypt
	// (active and superseded, which includes everything frozen for a retry),
	// ordered by id, with id > cursor.
	ListDecryptableVersions(ctx context.Context, cursor string, limit int) ([]Version, error)
}

type VerifyFailure struct{ VersionID, KEKID, Reason string }
type VerifyResult struct {
	Checked, Failed int
	// Failures lists at most MaxVerifyFailuresListed entries in id order.
	Failures []VerifyFailure
}

type Verifier struct {
	repository VerifyRepository
	provider   crypto.KeyProvider
}

func NewVerifier(repository VerifyRepository, provider crypto.KeyProvider) *Verifier {
	return &Verifier{repository: repository, provider: provider}
}

// Verify must run with writers stopped (control plane down) for a complete
// result: each page is its own read-only transaction, not one snapshot.
//
// Verify is the non-logging decrypt canary: every decryptable version is
// opened with the real crypto.Decrypt and the plaintext is cleared at once.
// A database generation different from expectedGeneration fails before any
// ciphertext is read.
func (v *Verifier) Verify(ctx context.Context, expectedGeneration string) (VerifyResult, error) {
	if v == nil || v.repository == nil || v.provider == nil || !validID(expectedGeneration) {
		return VerifyResult{}, ErrInvalidInput
	}
	generation, err := v.repository.DatabaseGeneration(ctx)
	if err != nil {
		return VerifyResult{}, ErrCorrupt
	}
	if generation != expectedGeneration {
		return VerifyResult{}, ErrGenerationMismatch
	}
	var result VerifyResult
	cursor := ""
	for {
		versions, err := v.repository.ListDecryptableVersions(ctx, cursor, verifyPageSize)
		if err != nil {
			return VerifyResult{}, ErrCorrupt
		}
		for _, version := range versions {
			result.Checked++
			plaintext, err := crypto.Decrypt(v.provider, cryptoScope(version), version.Envelope)
			clear(plaintext)
			if err == nil {
				continue
			}
			result.Failed++
			if len(result.Failures) < maxFailuresListed {
				result.Failures = append(result.Failures, VerifyFailure{VersionID: version.ID, KEKID: version.Envelope.KEKID, Reason: verifyReason(err)})
			}
		}
		if len(versions) < verifyPageSize {
			return result, nil
		}
		cursor = versions[len(versions)-1].ID
	}
}

func verifyReason(err error) string {
	switch {
	case errors.Is(err, crypto.ErrUnknownKey):
		return VerifyKEKMissing
	case errors.Is(err, crypto.ErrInvalidKey):
		return VerifyKEKInvalid
	case errors.Is(err, crypto.ErrUnwrapFailed):
		return VerifyUnwrapFailed
	case errors.Is(err, crypto.ErrValueOpenFailed):
		return VerifyDecryptFailed
	default:
		return VerifyEnvelopeInvalid
	}
}
