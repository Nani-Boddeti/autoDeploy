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
)
