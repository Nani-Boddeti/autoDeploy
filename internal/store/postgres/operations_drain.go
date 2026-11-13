package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// DrainResultView is the recorded outcome of a deployment's predecessor drain
// (step 20B; visibility only).
type DrainResultView struct {
	Outcome              string
	Summary              string
	PredecessorReleaseID string
	ReportedAt           time.Time
}

// readDrainResult returns the drain result recorded for any attempt of jobID
// (the deployment's activating attempt), nil when none exists. The caller has
// already established that the job belongs to the principal's owner.
func readDrainResult(ctx context.Context, tx pgx.Tx, jobID int64) (*DrainResultView, error) {
	var v DrainResultView
	err := tx.QueryRow(ctx, `SELECT r.outcome, r.outcome_summary, r.predecessor_release_id, r.reported_at
FROM release_drain_results r JOIN agent_job_attempts a ON a.id=r.attempt_id WHERE a.job_id=$1 ORDER BY r.id DESC LIMIT 1`, jobID).
		Scan(&v.Outcome, &v.Summary, &v.PredecessorReleaseID, &v.ReportedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read drain result: %w", err)
	}
	return &v, nil
}
