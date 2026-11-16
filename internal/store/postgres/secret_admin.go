package postgres

import (
	"context"
	"errors"
	"fmt"

	"autodeploy/internal/secret"
	"github.com/jackc/pgx/v5"
)

// ErrEnvironmentNotFound reports that no deployment job has ever named the
// environment, so its owner and project cannot be derived. The Servers page
// likewise treats an environment as known only after its first deploy.
var ErrEnvironmentNotFound = errors.New("environment not found")

// ErrEnvironmentAmbiguous reports that the environment's jobs name more than
// one owner or project.
var ErrEnvironmentAmbiguous = errors.New("environment has ambiguous owner or project")

// ValidVariableName reports whether name is acceptable as an environment
// runtime binding variable name (the same rule AddEnvironmentRuntimeBinding
// and the schema CHECKs apply).
func ValidVariableName(name string) bool { return validVariableName(name) }

// EnvironmentOwnerProject derives the owner and project of environmentID from
// its deployment jobs (read-only). An environment whose jobs disagree on
// owner or project is refused rather than guessed.
func (r *SecretRepository) EnvironmentOwnerProject(ctx context.Context, environmentID string) (ownerID, projectID string, err error) {
	if r == nil || r.pool == nil || environmentID == "" {
		return "", "", secret.ErrInvalidInput
	}
	rows, err := r.pool.Query(ctx, `SELECT DISTINCT owner_id, project_id FROM agent_jobs WHERE environment_id=$1 LIMIT 2`, environmentID)
	if err != nil {
		return "", "", fmt.Errorf("look up environment: %w", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		if err := rows.Scan(&ownerID, &projectID); err != nil {
			return "", "", fmt.Errorf("scan environment: %w", err)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return "", "", fmt.Errorf("look up environment: %w", err)
	}
	switch n {
	case 0:
		return "", "", ErrEnvironmentNotFound
	case 1:
		return ownerID, projectID, nil
	default:
		return "", "", ErrEnvironmentAmbiguous
	}
}

// SecretHeadScope loads a head's immutable scope (no ciphertext).
func (r *SecretRepository) SecretHeadScope(ctx context.Context, secretID string) (secret.Scope, error) {
	if r == nil || r.pool == nil || secretID == "" {
		return secret.Scope{}, secret.ErrInvalidInput
	}
	s := secret.Scope{SecretID: secretID}
	err := r.pool.QueryRow(ctx, `SELECT owner_id,project_id,environment_id,purpose,delivery_scope FROM secret_heads WHERE id=$1`, secretID).
		Scan(&s.OwnerID, &s.ProjectID, &s.EnvironmentID, &s.Purpose, &s.DeliveryScope)
	if errors.Is(err, pgx.ErrNoRows) {
		return secret.Scope{}, ErrSecretNotFound
	}
	if err != nil {
		return secret.Scope{}, fmt.Errorf("load secret head: %w", err)
	}
	return s, nil
}
