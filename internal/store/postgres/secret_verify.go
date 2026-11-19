package postgres

import (
	"context"
	"fmt"

	"autodeploy/internal/secret"
	"github.com/jackc/pgx/v5"
)

// DatabaseGeneration returns the non-secret database generation binding.
func (r *SecretRepository) DatabaseGeneration(ctx context.Context) (string, error) {
	if r == nil || r.pool == nil {
		return "", secret.ErrInvalidInput
	}
	var id string
	if err := r.pool.QueryRow(ctx, `SELECT id FROM secret_database_generation WHERE singleton=true`).Scan(&id); err != nil {
		return "", fmt.Errorf("load database generation: %w", err)
	}
	return id, nil
}

// ListDecryptableVersions pages, by id, through every version the system can
// still decrypt. 'revoked' is excluded on purpose: MaterializeAuthorized and
// the frozen-secret materialization both refuse it, and rewrap never touches
// it. 'superseded' stays in because frozen_secret_versions (retries) may still
// reference it. Versions are immutable and undeletable, so no destroyed state
// exists. The page runs in a read-only transaction.
func (r *SecretRepository) ListDecryptableVersions(ctx context.Context, cursor string, limit int) ([]secret.Version, error) {
	if r == nil || r.pool == nil || len(cursor) > 128 || limit < 1 || limit > 1000 {
		return nil, secret.ErrInvalidInput
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin secret verify page: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	rows, err := tx.Query(ctx, `SELECT v.id,v.secret_id,v.owner_id,v.project_id,v.environment_id,v.version_number,v.state,v.suite,v.aad_version,v.kek_id,v.ciphertext,v.wrapped_dek,v.envelope_revision,v.created_at,h.purpose,h.delivery_scope FROM secret_versions v JOIN secret_heads h ON h.id=v.secret_id WHERE v.state IN ('active','superseded') AND v.id>$1 ORDER BY v.id ASC LIMIT $2`, cursor, limit)
	if err != nil {
		return nil, fmt.Errorf("list secret verify page: %w", err)
	}
	defer rows.Close()
	versions := make([]secret.Version, 0, limit)
	for rows.Next() {
		var v secret.Version
		var state string
		var revision int64
		if err := rows.Scan(&v.ID, &v.Scope.SecretID, &v.Scope.OwnerID, &v.Scope.ProjectID, &v.Scope.EnvironmentID, &v.Number, &state, &v.Envelope.Suite, &v.Envelope.AADVersion, &v.Envelope.KEKID, &v.Envelope.Ciphertext, &v.Envelope.WrappedDEK, &revision, &v.CreatedAt, &v.Scope.Purpose, &v.Scope.DeliveryScope); err != nil {
			return nil, fmt.Errorf("scan secret verify page: %w", err)
		}
		v.State, v.EnvelopeRevision = secret.State(state), uint64(revision)
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate secret verify page: %w", err)
	}
	return versions, nil
}
