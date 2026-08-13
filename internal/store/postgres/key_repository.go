package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

const maxWrapInvocations int64 = 1 << 31
const maxReservedWrapInvocations int64 = maxWrapInvocations - 1

var ErrWrapInvocationExhausted = errors.New("secret wrap invocation limit reached")
var ErrKeyLedgerContinuity = errors.New("secret key ledger continuity unavailable")

// KeyRepository persists only invocation reservations. Key lifecycle and active
// selection stay exclusively in the external KeyProvider.
type KeyRepository struct{ pool *pgxpool.Pool }

func NewKeyRepository(pool *pgxpool.Pool) *KeyRepository { return &KeyRepository{pool: pool} }

// ReserveWrapInvocation commits before returning, so a later encryption or
// secret-write failure can never recycle a GCM invocation.
func (r *KeyRepository) ReserveWrapInvocation(kekID string) error {
	if r == nil || r.pool == nil || !validKEKID(kekID) {
		return ErrWrapInvocationExhausted
	}
	ctx := context.Background()
	command, err := r.pool.Exec(ctx, `UPDATE secret_wrap_invocations i SET reserved_invocations=i.reserved_invocations+1, updated_at=now() FROM secret_database_generation g WHERE i.kek_id=$1 AND i.database_generation_id=g.id AND i.reserved_invocations < $2`, kekID, maxReservedWrapInvocations)
	if err != nil {
		return fmt.Errorf("reserve secret wrap invocation: %w", err)
	}
	if command.RowsAffected() != 1 {
		return ErrWrapInvocationExhausted
	}
	return nil
}

// HasWrapInvocationLedger verifies that this database contains the durable
// reservation ledger for an externally selected active KEK. Callers use this
// only as a readiness dependency; absence fails closed rather than allowing a
// restored database to reuse an unknown nonce history.
func (r *KeyRepository) HasWrapInvocationLedger(ctx context.Context, kekID string) (bool, error) {
	if r == nil || r.pool == nil || !validKEKID(kekID) {
		return false, ErrWrapInvocationExhausted
	}
	var exists bool
	if err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM secret_wrap_invocations WHERE kek_id=$1)`, kekID).Scan(&exists); err != nil {
		return false, fmt.Errorf("load secret wrap ledger: %w", err)
	}
	return exists, nil
}

// LedgerReady requires both the immutable database generation binding and the
// active writer ledger. It deliberately does not create missing rows.
func (r *KeyRepository) LedgerReady(ctx context.Context, kekID, generationID string) (bool, error) {
	if r == nil || r.pool == nil || !validKEKID(kekID) || !validGenerationID(generationID) {
		return false, ErrKeyLedgerContinuity
	}
	var exists bool
	err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM secret_wrap_invocations i JOIN secret_database_generation g ON g.singleton=true WHERE i.kek_id=$1 AND i.database_generation_id=g.id AND g.id=$2)`, kekID, generationID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check secret key continuity: %w", err)
	}
	return exists, nil
}

// InitializeActiveLedger is terminal-only recovery/bootstrap. It never resets
// or reuses a KEK ledger and rejects IDs referenced by any envelope.
func (r *KeyRepository) InitializeActiveLedger(ctx context.Context, kekID, generationID string) error {
	if r == nil || r.pool == nil || !validKEKID(kekID) || !validGenerationID(generationID) {
		return ErrKeyLedgerContinuity
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin key ledger init: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var dbGeneration string
	if err = tx.QueryRow(ctx, `SELECT id FROM secret_database_generation WHERE singleton=true FOR UPDATE`).Scan(&dbGeneration); err != nil {
		return fmt.Errorf("load database generation: %w", err)
	}
	if dbGeneration != generationID {
		return ErrKeyLedgerContinuity
	}
	var referenced bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM secret_versions WHERE kek_id=$1)`, kekID).Scan(&referenced); err != nil || referenced {
		return ErrKeyLedgerContinuity
	}
	command, err := tx.Exec(ctx, `INSERT INTO secret_wrap_invocations(kek_id,reserved_invocations,database_generation_id) VALUES($1,0,$2) ON CONFLICT DO NOTHING`, kekID, generationID)
	if err != nil {
		return fmt.Errorf("initialize key ledger: %w", err)
	}
	if command.RowsAffected() != 1 {
		return ErrKeyLedgerContinuity
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit key ledger init: %w", err)
	}
	return nil
}
func validKEKID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}
func validGenerationID(id string) bool { return len(id) >= 16 && len(id) <= 128 }

var _ interface{ ReserveWrapInvocation(string) error } = (*KeyRepository)(nil)
