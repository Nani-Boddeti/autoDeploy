// Package postgres contains PostgreSQL-backed repositories.
package postgres

import (
	"errors"
	"fmt"
)

var (
	ErrNotFound            = errors.New("deployment not found")
	ErrAlreadyExists       = errors.New("deployment already exists")
	ErrRevisionConflict    = errors.New("deployment revision conflict")
	ErrRevisionOverflow    = errors.New("deployment revision exceeds PostgreSQL bigint")
	ErrAlreadyBootstrapped = errors.New("administrator authentication already bootstrapped")
	ErrUnauthenticated     = errors.New("unauthenticated")
	ErrSessionRevoked      = errors.New("session already revoked")
	// ErrWebhookIntegrity is a sanitized conflict between immutable webhook
	// delivery, replay, intent, or job identities.
	ErrWebhookIntegrity = errors.New("webhook integrity conflict")
	// ErrWebhookUnauthorized reports an inactive or mismatched durable GitHub
	// authority chain without exposing which authority record failed.
	ErrWebhookUnauthorized = errors.New("webhook authorization denied")
	// ErrManualDeployUnauthorized reports an inactive, suspended, unbound,
	// cross-owner or session-invalid manual deploy without exposing which
	// check failed.
	ErrManualDeployUnauthorized = errors.New("manual deploy authorization denied")
	// ErrManualDeployInFlight reports a non-terminal deployment of the same
	// commit already exists for the binding's environment.
	ErrManualDeployInFlight = errors.New("manual deploy already in flight for this commit")
	// ErrManualDeployConflict reports a request nonce reused with a different
	// payload, an expected-ref mismatch, or a broken immutable identity.
	ErrManualDeployConflict = errors.New("manual deploy integrity conflict")
	// ErrManualDeployInvalid reports a malformed project id, commit SHA or
	// request nonce, rejected before any SQL.
	ErrManualDeployInvalid = errors.New("manual deploy input invalid")
	// ErrRollbackUnauthorized reports an invalid, revoked or mismatched admin
	// session for a rollback.
	ErrRollbackUnauthorized = errors.New("rollback authorization denied")
	// ErrRollbackInvalid reports a malformed release id or request nonce,
	// rejected before any SQL.
	ErrRollbackInvalid = errors.New("rollback input invalid")
	// ErrRollbackNotFound reports an unknown or cross-owner target release.
	ErrRollbackNotFound = errors.New("rollback target not found")
	// ErrRollbackNotEligible reports a target that cannot be rolled back to now:
	// not retained, active, another environment or server than the active
	// release, no succeeded activation, no runtime configuration, or the
	// environment is blocked for reconciliation.
	ErrRollbackNotEligible = errors.New("rollback target is not eligible")
	// ErrRollbackSecretRevoked reports a target one of whose frozen secret
	// versions was revoked (owner decision D-R3).
	ErrRollbackSecretRevoked = errors.New("rollback target has a revoked secret version")
	// ErrRollbackInFlight reports a queued or in-progress deployment in the
	// target's environment.
	ErrRollbackInFlight = errors.New("rollback refused: a deployment is in flight in this environment")
	// ErrRollbackSourceChanged reports a rollback whose source (the active
	// release it was requested from) is no longer the active release. It wraps
	// ErrAgentLeaseActivationCandidateNotReady, so the agent API answers it as the
	// same definite, permanent refusal and the agent stops the candidate.
	ErrRollbackSourceChanged = fmt.Errorf("rollback source release is no longer active: %w", ErrAgentLeaseActivationCandidateNotReady)
	// ErrRollbackConflict reports a request nonce reused with a different
	// target, or a broken immutable identity.
	ErrRollbackConflict = errors.New("rollback integrity conflict")
	// ErrServerAuthorizationDenied reports a denied server registration,
	// enrollment, rotation, revocation, or authentication without exposing
	// which specific check failed.
	ErrServerAuthorizationDenied = errors.New("server authorization denied")
)
