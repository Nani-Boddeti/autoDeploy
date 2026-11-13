// Package postgres contains PostgreSQL-backed repositories.
package postgres

import "errors"

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
	// ErrServerAuthorizationDenied reports a denied server registration,
	// enrollment, rotation, revocation, or authentication without exposing
	// which specific check failed.
	ErrServerAuthorizationDenied = errors.New("server authorization denied")
)
