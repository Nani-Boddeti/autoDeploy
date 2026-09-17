package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"

	"autodeploy/internal/deployment"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DeploymentRepository persists deployment aggregates.
type DeploymentRepository struct{ pool *pgxpool.Pool }

func NewDeploymentRepository(pool *pgxpool.Pool) *DeploymentRepository {
	return &DeploymentRepository{pool: pool}
}

// Create persists a new aggregate and its complete event history atomically.
func (r *DeploymentRepository) Create(ctx context.Context, value deployment.Deployment) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin deployment create: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = r.CreateTx(ctx, tx, value); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit deployment create: %w", err)
	}
	return nil
}

// CreateTx persists a new deployment in a transaction owned by the caller.
// It deliberately does not commit or roll back that transaction.
func (r *DeploymentRepository) CreateTx(ctx context.Context, tx pgx.Tx, value deployment.Deployment) error {
	return r.createTxWithHook(ctx, tx, value, nil)
}

// createTxWithHook is the repository-owned transaction seam used only by the
// webhook unit of work. The callback runs after the deployment head and after
// its initial event, so rollback tests exercise both real write stages.
func (r *DeploymentRepository) createTxWithHook(ctx context.Context, tx pgx.Tx, value deployment.Deployment, hook func(string) error) error {
	if tx == nil {
		return errors.New("deployment transaction is required")
	}
	snapshot, err := checkedSnapshot(value)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO deployments (id, project_id, environment_id, server_id, commit_sha, schema_version, status, created_at, updated_at, revision) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, snapshot.Identity.DeploymentID, snapshot.Identity.ProjectID, snapshot.Identity.EnvironmentID, snapshot.Identity.ServerID, snapshot.Identity.CommitSHA, int16(snapshot.Version), snapshot.Status, snapshot.CreatedAt, snapshot.UpdatedAt, int64(snapshot.Revision)); err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: %s", ErrAlreadyExists, snapshot.Identity.DeploymentID)
		}
		return fmt.Errorf("insert deployment: %w", err)
	}
	if hook != nil {
		if err := hook("deployment"); err != nil {
			return err
		}
	}
	if err = appendEvents(ctx, tx, snapshot.Identity.DeploymentID, snapshot.Events); err != nil {
		return err
	}
	if hook != nil {
		if err := hook("deployment_event"); err != nil {
			return err
		}
	}
	return nil
}

// GetByID loads and validates a complete aggregate, including its append-only event history.
func (r *DeploymentRepository) GetByID(ctx context.Context, id deployment.ID) (deployment.Deployment, error) {
	if id == "" {
		return deployment.Deployment{}, fmt.Errorf("%w: empty ID", ErrNotFound)
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return deployment.Deployment{}, fmt.Errorf("begin deployment read: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	snapshot, err := readSnapshot(ctx, tx, id)
	if err != nil {
		return deployment.Deployment{}, err
	}
	value, err := deployment.Rehydrate(snapshot)
	if err != nil {
		return deployment.Deployment{}, fmt.Errorf("rehydrate deployment %s: %w", id, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return deployment.Deployment{}, fmt.Errorf("commit deployment read: %w", err)
	}
	return value, nil
}

// Save conditionally replaces a deployment head and appends only events not already persisted.
// It returns ErrRevisionConflict when another writer has saved first.
func (r *DeploymentRepository) Save(ctx context.Context, value deployment.Deployment) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin deployment save: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = r.saveTxWithHook(ctx, tx, value, nil); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit deployment save: %w", err)
	}
	return nil
}

// saveTxWithHook is Save's transaction-aware core, reused by the agent lease
// claim transaction's queued->assigned transition so that mutation shares
// the claim transaction's atomicity instead of opening a private one (ADR
// 0004 atomic claim boundary step 9). It deliberately does not commit or
// roll back tx. The optional hook runs after the head update and after its
// appended event, mirroring createTxWithHook's rollback-test seam.
func (r *DeploymentRepository) saveTxWithHook(ctx context.Context, tx pgx.Tx, value deployment.Deployment, hook func(string) error) error {
	snapshot, err := checkedSnapshot(value)
	if err != nil {
		return err
	}
	var lockedID deployment.ID
	if err := tx.QueryRow(ctx, `SELECT id FROM deployments WHERE id=$1 FOR UPDATE`, snapshot.Identity.DeploymentID).Scan(&lockedID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrNotFound, snapshot.Identity.DeploymentID)
		}
		return fmt.Errorf("lock deployment %s: %w", snapshot.Identity.DeploymentID, err)
	}
	stored, err := readSnapshot(ctx, tx, snapshot.Identity.DeploymentID)
	if err != nil {
		return err
	}
	if stored.Identity != snapshot.Identity {
		return fmt.Errorf("%w: immutable identity", ErrRevisionConflict)
	}
	if _, err := deployment.Rehydrate(stored); err != nil {
		return fmt.Errorf("rehydrate stored deployment %s: %w", snapshot.Identity.DeploymentID, err)
	}
	if stored.Revision > snapshot.Revision || !matchingPrefix(stored.Events, snapshot.Events) {
		return fmt.Errorf("%w: %s", ErrRevisionConflict, snapshot.Identity.DeploymentID)
	}
	if stored.Revision == snapshot.Revision {
		// Save is a CAS operation, not an idempotent upsert. A snapshot at the
		// stored revision has no new event to append and is therefore stale.
		return fmt.Errorf("%w: %s", ErrRevisionConflict, snapshot.Identity.DeploymentID)
	}
	command, err := tx.Exec(ctx, `UPDATE deployments SET schema_version=$2, status=$3, updated_at=$4, revision=$5 WHERE id=$1 AND revision=$6`, snapshot.Identity.DeploymentID, int16(snapshot.Version), snapshot.Status, snapshot.UpdatedAt, int64(snapshot.Revision), int64(stored.Revision))
	if err != nil {
		return fmt.Errorf("update deployment: %w", err)
	}
	if command.RowsAffected() != 1 {
		return fmt.Errorf("%w: %s", ErrRevisionConflict, snapshot.Identity.DeploymentID)
	}
	if hook != nil {
		if err := hook("deployment_update"); err != nil {
			return err
		}
	}
	if err = appendEvents(ctx, tx, snapshot.Identity.DeploymentID, snapshot.Events[stored.Revision:]); err != nil {
		return err
	}
	if hook != nil {
		if err := hook("deployment_update_event"); err != nil {
			return err
		}
	}
	return nil
}

func checkedSnapshot(value deployment.Deployment) (deployment.Snapshot, error) {
	snapshot, err := value.Snapshot()
	if err != nil {
		return deployment.Snapshot{}, err
	}
	if _, err := postgresRevision(snapshot.Revision); err != nil {
		return deployment.Snapshot{}, err
	}
	return snapshot, nil
}

func postgresRevision(revision deployment.Revision) (int64, error) {
	if uint64(revision) > math.MaxInt64 {
		return 0, ErrRevisionOverflow
	}
	return int64(revision), nil
}

type queryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func readSnapshot(ctx context.Context, q queryer, id deployment.ID) (deployment.Snapshot, error) {
	var snapshot deployment.Snapshot
	var version int16
	if err := q.QueryRow(ctx, `SELECT id, project_id, environment_id, server_id, commit_sha, schema_version, status, created_at, updated_at, revision FROM deployments WHERE id=$1`, id).Scan(&snapshot.Identity.DeploymentID, &snapshot.Identity.ProjectID, &snapshot.Identity.EnvironmentID, &snapshot.Identity.ServerID, &snapshot.Identity.CommitSHA, &version, &snapshot.Status, &snapshot.CreatedAt, &snapshot.UpdatedAt, &snapshot.Revision); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return deployment.Snapshot{}, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return deployment.Snapshot{}, fmt.Errorf("load deployment %s: %w", id, err)
	}
	snapshot.Version = deployment.SchemaVersion(version)
	rows, err := q.Query(ctx, `SELECT from_status, to_status, occurred_at FROM deployment_events WHERE deployment_id=$1 ORDER BY revision`, id)
	if err != nil {
		return deployment.Snapshot{}, fmt.Errorf("load deployment events %s: %w", id, err)
	}
	defer rows.Close()
	for rows.Next() {
		var event deployment.Event
		if err := rows.Scan(&event.From, &event.To, &event.At); err != nil {
			return deployment.Snapshot{}, fmt.Errorf("scan deployment event %s: %w", id, err)
		}
		snapshot.Events = append(snapshot.Events, event)
	}
	if err := rows.Err(); err != nil {
		return deployment.Snapshot{}, fmt.Errorf("load deployment events %s: %w", id, err)
	}
	return snapshot, nil
}

func appendEvents(ctx context.Context, tx pgx.Tx, id deployment.ID, events []deployment.Event) error {
	var existing int64
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM deployment_events WHERE deployment_id=$1`, id).Scan(&existing); err != nil {
		return fmt.Errorf("count deployment events: %w", err)
	}
	for offset, event := range events {
		if _, err := tx.Exec(ctx, `INSERT INTO deployment_events (deployment_id, revision, from_status, to_status, occurred_at) VALUES ($1,$2,$3,$4,$5)`, id, existing+int64(offset)+1, event.From, event.To, event.At); err != nil {
			return fmt.Errorf("append deployment event: %w", err)
		}
	}
	return nil
}

func matchingPrefix(stored, candidate []deployment.Event) bool {
	if len(stored) > len(candidate) {
		return false
	}
	for index := range stored {
		if stored[index].From != candidate[index].From || stored[index].To != candidate[index].To || !stored[index].At.Equal(candidate[index].At) {
			return false
		}
	}
	return true
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
