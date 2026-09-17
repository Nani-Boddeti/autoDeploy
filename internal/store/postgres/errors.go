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
	// ErrServerAuthorizationDenied reports a denied server registration,
	// enrollment, rotation, revocation, or authentication without exposing
	// which specific check failed.
	ErrServerAuthorizationDenied = errors.New("server authorization denied")
)
