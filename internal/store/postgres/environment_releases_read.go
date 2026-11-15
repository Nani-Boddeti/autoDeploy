package postgres

import (
	"context"
	"fmt"
	"time"

	"autodeploy/internal/auth"
	"autodeploy/internal/github"

	"github.com/jackc/pgx/v5"
)

// MaxEnvironmentReleases bounds the per-environment release listing.
const MaxEnvironmentReleases = 10

// EnvironmentRelease is one release of an environment with its latest
// cleanup (nil when it was never claimed), for the route page (step 21C).
type EnvironmentRelease struct {
	ID        string
	State     string
	Strategy  string
	CreatedAt time.Time
	Cleanup   *ReleaseCleanup
}

// ListEnvironmentReleases returns the newest MaxEnvironmentReleases releases
// of environmentID, newest first. Owner scope is per release: only releases
// whose deployment has an agent_jobs row owned by the principal are returned
// (environment ids are not owner-bound), so another owner's or an unknown
// environment yields an empty list. Read-only; same live-session check and
// snapshot as GetDeploymentDetail.
func (r *OperationsReadRepository) ListEnvironmentReleases(ctx context.Context, principal auth.Principal, environmentID string) ([]EnvironmentRelease, error) {
	if err := r.ready(principal); err != nil {
		return nil, err
	}
	if !github.ValidOpaqueID(environmentID) {
		return []EnvironmentRelease{}, nil
	}
	tx, err := r.beginRead(ctx, principal, pgx.RepeatableRead)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	out, err := queryAll(ctx, tx, func(rows pgx.Rows) (EnvironmentRelease, error) {
		var e EnvironmentRelease
		var cid *int64
		var claimed *time.Time
		var result, class *string
		var kept *bool
		var summary *string
		var completed *time.Time
		var attempts int
		if err := rows.Scan(&e.ID, &e.State, &e.Strategy, &e.CreatedAt, &cid, &claimed, &result, &class, &kept, &summary, &completed, &attempts); err != nil {
			return e, err
		}
		if cid != nil {
			c := &ReleaseCleanup{ID: *cid, CompletedAt: completed, Attempts: attempts, Result: derefString(result), ErrorClass: derefString(class), Summary: derefString(summary)}
			if claimed != nil {
				c.ClaimedAt = *claimed
			}
			if kept != nil {
				c.ImageKept = *kept
			}
			e.Cleanup = c
		}
		return e, nil
	}, `SELECT r.id, r.state, r.strategy, r.created_at,
       c.id, c.claimed_at, c.result, c.error_class, c.image_kept, c.outcome_summary, c.completed_at,
       (SELECT count(*) FROM release_cleanups WHERE release_id=r.id)
FROM releases r
LEFT JOIN LATERAL (SELECT id, claimed_at, result, error_class, image_kept, outcome_summary, completed_at
                   FROM release_cleanups WHERE release_id=r.id ORDER BY id DESC LIMIT 1) c ON true
WHERE r.environment_id=$1
  AND EXISTS (SELECT 1 FROM agent_jobs j WHERE j.deployment_id=r.deployment_id AND j.owner_id=$2)
ORDER BY r.created_at DESC, r.id DESC LIMIT $3`, environmentID, principal.OwnerID, MaxEnvironmentReleases)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit environment releases: %w", err)
	}
	return out, nil
}
