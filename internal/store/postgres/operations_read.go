package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"autodeploy/internal/auth"
	"autodeploy/internal/github"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// OperationsReadRepository is the read-only query surface behind the
// operations UI (step 15b): a derived projects listing, a keyset-paged
// deployments listing and a deployment detail view.
//
// Design rules:
//   - Owner scope ALWAYS comes from agent_jobs.owner_id. deployments has no
//     owner_id, so a deployment with no agent_jobs row (impossible through
//     the enqueue path, which inserts both in one transaction, but possible
//     by schema because the job FK is the deferrable direction) belongs to no
//     owner and is never returned by any method here (inner join on
//     agent_jobs). github_project_bindings is owner-scoped natively.
//   - Plain SELECTs only: no FOR UPDATE/FOR SHARE, no advisory locks, no
//     writes, so reads can never join a wait cycle with the ADR 0004 lock
//     hierarchy. Lists run READ COMMITTED (single statement, own snapshot);
//     GetDeploymentDetail runs one READ ONLY REPEATABLE READ transaction so
//     its several statements observe a single consistent snapshot.
//   - The principal is validated structurally and its admin session is
//     re-checked live in the same transaction as the read, with the same
//     predicate as lockAdministratorSession but as a plain SELECT (no row
//     locks); failure maps to ErrServerAuthorizationDenied.
//   - Only whitelisted non-secret columns are selected. Never selected:
//     payload_digest, idempotency_key, config_digest, result_digest,
//     result_operation_id, frozen secret/literal tables, runtime config
//     bindings, container_name, lease timing, and any free-text failure
//     message (none is stored; failure_class is a closed enum).
type OperationsReadRepository struct {
	pool *pgxpool.Pool
}

func NewOperationsReadRepository(pool *pgxpool.Pool) *OperationsReadRepository {
	return &OperationsReadRepository{pool: pool}
}

// ErrOperationsNotFound is the single not-found outcome of
// GetDeploymentDetail: unknown id, malformed id and another owner's id are
// indistinguishable.
var ErrOperationsNotFound = errors.New("operations record not found")

// ErrOperationsInvalidInput reports a malformed filter, limit or cursor.
var ErrOperationsInvalidInput = errors.New("invalid operations query input")

const (
	// MaxProjectsListed bounds ListProjects.
	MaxProjectsListed = 200
	// DefaultDeploymentPageLimit applies when DeploymentPage.Limit is 0.
	DefaultDeploymentPageLimit = 25
	// MaxDeploymentPageLimit bounds one ListDeployments page.
	MaxDeploymentPageLimit = 100
	// MaxDetailDeploymentEvents and MaxDetailJobEvents bound the newest-first
	// event lists of GetDeploymentDetail.
	MaxDetailDeploymentEvents = 100
	MaxDetailJobEvents        = 100
	// MaxDetailAttempts / MaxDetailRuntimeInstances bound the remaining lists.
	MaxDetailAttempts         = 20
	MaxDetailRuntimeInstances = 20
)

// DeploymentSummary is the latest/listed deployment projection. JobID,
// JobState and CancelRequested come from the owning agent_jobs row.
type DeploymentSummary struct {
	DeploymentID    string
	ProjectID       string
	EnvironmentID   string
	ServerID        string
	CommitSHA       string
	Status          string
	JobID           int64
	JobState        string
	CancelRequested bool
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// ShortCommit returns the first 12 characters of the commit id.
func (d DeploymentSummary) ShortCommit() string {
	if len(d.CommitSHA) > 12 {
		return d.CommitSHA[:12]
	}
	return d.CommitSHA
}

// ProjectSummary is one derived project row.
type ProjectSummary struct {
	ProjectID string
	// HasBinding is true when a github_project_bindings row exists. The
	// Binding* fields are meaningful only then.
	HasBinding           bool
	BindingActive        bool
	RepositoryFullName   string
	RepositoryDefaultRef string // deploy_ref, "" when the binding has no deploy target
	BindingEnvironmentID string
	BindingServerID      string
	EnvironmentIDs       []string // sorted, distinct: binding target plus every job environment
	LatestDeployment     *DeploymentSummary
}

// DeploymentFilter narrows ListDeployments; empty fields mean "any".
type DeploymentFilter struct {
	ProjectID     string
	EnvironmentID string
}

// DeploymentCursor is the keyset position (created_at, id) of the last row of
// the previous page.
type DeploymentCursor struct {
	CreatedAt    time.Time
	DeploymentID string
}

// DeploymentPage selects a page. After nil starts from the newest.
type DeploymentPage struct {
	Limit int
	After *DeploymentCursor
}

// DeploymentList is one page, newest first. Next is non-nil iff more rows
// exist after this page.
type DeploymentList struct {
	Items []DeploymentSummary
	Next  *DeploymentCursor
}

type DeploymentStatusEvent struct {
	Revision   int64
	FromStatus string
	ToStatus   string
	OccurredAt time.Time
}

type JobEventView struct {
	Sequence   int
	EventType  string
	AttemptID  *int64
	OccurredAt time.Time
}

// JobView is the agent job head. Phase is not persisted anywhere in the
// schema; the deployment status is the durable phase signal.
type JobView struct {
	ID                int64
	State             string
	AttemptCount      int
	CancelRequestedAt *time.Time
	ServerID          string
	EnvironmentID     string
	ProjectID         string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type AttemptView struct {
	ID              int64
	AttemptNumber   int
	State           string
	FailureClass    string // "" when none
	Fence           int64
	ServerID        string
	ReleaseStrategy string // from agent_job_attempt_execution; "" if the row does not exist
	CreatedAt       time.Time
	ClosedAt        *time.Time
}

type ReleaseView struct {
	ID          string
	State       string
	Strategy    string
	CommitSHA   string
	ImageDigest string // lowercase hex of the 32-byte digest; "" until the build finishes
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type RuntimeInstanceView struct {
	ID            int64
	AttemptID     int64
	Fence         int64
	ObservedState string
	CreatedAt     time.Time
}

// DeploymentDetail aggregates one owned deployment. Release is nil when none
// exists yet. RouteActiveReleaseID is the environment route head's active
// release ("" when none / no route head); ReleaseIsActive is true only when
// that id equals this deployment's release id.
type DeploymentDetail struct {
	Deployment           DeploymentSummary
	StatusEvents         []DeploymentStatusEvent // newest first, <= MaxDetailDeploymentEvents
	Job                  JobView
	JobEvents            []JobEventView // newest first, <= MaxDetailJobEvents
	Attempts             []AttemptView  // newest first, <= MaxDetailAttempts
	Release              *ReleaseView
	RuntimeInstances     []RuntimeInstanceView // newest first, <= MaxDetailRuntimeInstances
	RouteActiveReleaseID string
	ReleaseIsActive      bool
}

// requireLiveSession mirrors lockAdministratorSession's predicate without
// FOR UPDATE, so a revoked, expired or mismatched session is denied but the
// read never blocks (or is blocked by) a session-row writer.
func requireLiveSession(ctx context.Context, tx pgx.Tx, principal auth.Principal) error {
	if requireLiveSessionRow(ctx, tx, principal) != nil {
		return ErrServerAuthorizationDenied
	}
	return nil
}

// requireLiveSessionRow returns the raw database error (pgx.ErrNoRows when no
// live session matches) for callers that must not fold operational failures
// into an authorization denial.
func requireLiveSessionRow(ctx context.Context, tx pgx.Tx, principal auth.Principal) error {
	var live int
	err := tx.QueryRow(ctx, `SELECT 1 FROM admin_sessions s JOIN users u ON u.id=s.user_id AND u.owner_id=s.owner_id WHERE s.id=$1 AND s.user_id=$2 AND s.owner_id=$3 AND s.auth_revision=$4 AND u.auth_revision=$4 AND u.status='active' AND s.revoked_at IS NULL AND s.idle_expires_at>clock_timestamp() AND s.absolute_expires_at>clock_timestamp()`,
		principal.SessionID, principal.UserID, principal.OwnerID, int64(principal.AuthRevision)).Scan(&live)
	if err != nil {
		return err
	}
	if live != 1 {
		return pgx.ErrNoRows
	}
	return nil
}

// beginRead opens a read-only transaction and verifies the session in it.
func (r *OperationsReadRepository) beginRead(ctx context.Context, principal auth.Principal, iso pgx.TxIsoLevel) (pgx.Tx, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: iso, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin operations read: %w", err)
	}
	if err = requireLiveSession(ctx, tx, principal); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return tx, nil
}

func (r *OperationsReadRepository) ready(principal auth.Principal) error {
	if r == nil || r.pool == nil || principal.Validate() != nil {
		return ErrServerAuthorizationDenied
	}
	return nil
}

// ListProjects returns at most MaxProjectsListed derived project rows for the
// owner (the cap is silent: rows beyond it, by project_id order, are omitted
// with no truncation signal): every project id with a binding or with at least one job.
func (r *OperationsReadRepository) ListProjects(ctx context.Context, principal auth.Principal) ([]ProjectSummary, error) {
	if err := r.ready(principal); err != nil {
		return nil, err
	}
	tx, err := r.beginRead(ctx, principal, pgx.ReadCommitted)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	rows, err := tx.Query(ctx, `
WITH ids AS (
  SELECT project_id FROM github_project_bindings WHERE owner_id=$1
  UNION
  SELECT project_id FROM agent_jobs WHERE owner_id=$1
), limited AS (
  SELECT project_id FROM ids ORDER BY project_id LIMIT $2
)
SELECT l.project_id,
       b.project_id IS NOT NULL, COALESCE(b.active,false), COALESCE(rp.full_name,''), COALESCE(b.deploy_ref,''),
       COALESCE(b.environment_id,''), COALESCE(b.server_id,''),
       ARRAY(SELECT e FROM (
               SELECT j.environment_id AS e FROM agent_jobs j WHERE j.owner_id=$1 AND j.project_id=l.project_id
               UNION SELECT b.environment_id WHERE b.environment_id IS NOT NULL
             ) s ORDER BY e),
       ld.id, ld.environment_id, ld.server_id, ld.commit_sha, ld.status, ld.job_id, ld.job_state, ld.cancel_requested, ld.created_at, ld.updated_at
FROM limited l
LEFT JOIN github_project_bindings b ON b.owner_id=$1 AND b.project_id=l.project_id
LEFT JOIN github_repositories rp ON rp.repository_id=b.repository_id AND rp.owner_id=b.owner_id
LEFT JOIN LATERAL (
  SELECT d.id, d.environment_id, d.server_id, d.commit_sha, d.status, j.id AS job_id, j.state AS job_state,
         j.cancel_requested_at IS NOT NULL AS cancel_requested, d.created_at, d.updated_at
  FROM agent_jobs j JOIN deployments d ON d.id=j.deployment_id
  WHERE j.owner_id=$1 AND j.project_id=l.project_id
  ORDER BY d.created_at DESC, d.id DESC LIMIT 1
) ld ON true
ORDER BY l.project_id`, principal.OwnerID, MaxProjectsListed)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()
	out := []ProjectSummary{}
	for rows.Next() {
		var p ProjectSummary
		var (
			ldID, ldEnv, ldServer, ldCommit, ldStatus, ldJobState *string
			ldJobID                                               *int64
			ldCancel                                              *bool
			ldCreated, ldUpdated                                  *time.Time
		)
		if err = rows.Scan(&p.ProjectID, &p.HasBinding, &p.BindingActive, &p.RepositoryFullName, &p.RepositoryDefaultRef,
			&p.BindingEnvironmentID, &p.BindingServerID, &p.EnvironmentIDs,
			&ldID, &ldEnv, &ldServer, &ldCommit, &ldStatus, &ldJobID, &ldJobState, &ldCancel, &ldCreated, &ldUpdated); err != nil {
			return nil, fmt.Errorf("scan project: %w", err)
		}
		if ldID != nil {
			p.LatestDeployment = &DeploymentSummary{DeploymentID: *ldID, ProjectID: p.ProjectID, EnvironmentID: *ldEnv, ServerID: *ldServer,
				CommitSHA: *ldCommit, Status: *ldStatus, JobID: *ldJobID, JobState: *ldJobState, CancelRequested: *ldCancel,
				CreatedAt: *ldCreated, UpdatedAt: *ldUpdated}
		}
		out = append(out, p)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	rows.Close()
	if err = tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit list projects: %w", err)
	}
	return out, nil
}

// ListDeployments returns one newest-first page of the owner's deployments
// ordered by (created_at, id) descending (a total, stable order even with
// equal timestamps).
func (r *OperationsReadRepository) ListDeployments(ctx context.Context, principal auth.Principal, filter DeploymentFilter, page DeploymentPage) (DeploymentList, error) {
	if err := r.ready(principal); err != nil {
		return DeploymentList{}, err
	}
	limit := page.Limit
	if limit == 0 {
		limit = DefaultDeploymentPageLimit
	}
	if limit < 0 || limit > MaxDeploymentPageLimit ||
		(filter.ProjectID != "" && !github.ValidOpaqueID(filter.ProjectID)) ||
		(filter.EnvironmentID != "" && !github.ValidOpaqueID(filter.EnvironmentID)) ||
		(page.After != nil && (page.After.CreatedAt.IsZero() || !github.ValidOpaqueID(page.After.DeploymentID))) {
		return DeploymentList{}, ErrOperationsInvalidInput
	}
	var afterAt *time.Time
	afterID := ""
	if page.After != nil {
		afterAt, afterID = &page.After.CreatedAt, page.After.DeploymentID
	}
	tx, err := r.beginRead(ctx, principal, pgx.ReadCommitted)
	if err != nil {
		return DeploymentList{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	rows, err := tx.Query(ctx, `
SELECT d.id, j.project_id, j.environment_id, j.server_id, d.commit_sha, d.status, j.id, j.state,
       j.cancel_requested_at IS NOT NULL, d.created_at, d.updated_at
FROM agent_jobs j JOIN deployments d ON d.id=j.deployment_id
WHERE j.owner_id=$1
  AND ($2='' OR j.project_id=$2)
  AND ($3='' OR j.environment_id=$3)
  AND ($4::timestamptz IS NULL OR (d.created_at, d.id) < ($4::timestamptz, $5::text))
ORDER BY d.created_at DESC, d.id DESC
LIMIT $6`, principal.OwnerID, filter.ProjectID, filter.EnvironmentID, afterAt, afterID, limit+1)
	if err != nil {
		return DeploymentList{}, fmt.Errorf("list deployments: %w", err)
	}
	defer rows.Close()
	list := DeploymentList{Items: []DeploymentSummary{}}
	for rows.Next() {
		var d DeploymentSummary
		if err = rows.Scan(&d.DeploymentID, &d.ProjectID, &d.EnvironmentID, &d.ServerID, &d.CommitSHA, &d.Status, &d.JobID, &d.JobState,
			&d.CancelRequested, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return DeploymentList{}, fmt.Errorf("scan deployment: %w", err)
		}
		list.Items = append(list.Items, d)
	}
	if err = rows.Err(); err != nil {
		return DeploymentList{}, fmt.Errorf("list deployments: %w", err)
	}
	rows.Close()
	if err = tx.Commit(ctx); err != nil {
		return DeploymentList{}, fmt.Errorf("commit list deployments: %w", err)
	}
	if len(list.Items) > limit {
		list.Items = list.Items[:limit]
		last := list.Items[limit-1]
		list.Next = &DeploymentCursor{CreatedAt: last.CreatedAt, DeploymentID: last.DeploymentID}
	}
	return list, nil
}

// GetDeploymentDetail returns the aggregate for one owned deployment, or
// ErrOperationsNotFound for an unknown, malformed or foreign id.
func (r *OperationsReadRepository) GetDeploymentDetail(ctx context.Context, principal auth.Principal, deploymentID string) (DeploymentDetail, error) {
	if err := r.ready(principal); err != nil {
		return DeploymentDetail{}, err
	}
	if !github.ValidOpaqueID(deploymentID) {
		return DeploymentDetail{}, ErrOperationsNotFound
	}
	tx, err := r.beginRead(ctx, principal, pgx.RepeatableRead)
	if err != nil {
		return DeploymentDetail{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var d DeploymentDetail
	err = tx.QueryRow(ctx, `
SELECT d.id, j.project_id, j.environment_id, j.server_id, d.commit_sha, d.status, j.id, j.state,
       j.cancel_requested_at IS NOT NULL, d.created_at, d.updated_at,
       j.attempt_count, j.cancel_requested_at, j.created_at, j.updated_at
FROM agent_jobs j JOIN deployments d ON d.id=j.deployment_id
WHERE j.owner_id=$1 AND d.id=$2`, principal.OwnerID, deploymentID).Scan(
		&d.Deployment.DeploymentID, &d.Deployment.ProjectID, &d.Deployment.EnvironmentID, &d.Deployment.ServerID, &d.Deployment.CommitSHA,
		&d.Deployment.Status, &d.Deployment.JobID, &d.Deployment.JobState, &d.Deployment.CancelRequested, &d.Deployment.CreatedAt, &d.Deployment.UpdatedAt,
		&d.Job.AttemptCount, &d.Job.CancelRequestedAt, &d.Job.CreatedAt, &d.Job.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return DeploymentDetail{}, ErrOperationsNotFound
	}
	if err != nil {
		return DeploymentDetail{}, fmt.Errorf("read deployment detail: %w", err)
	}
	d.Job.ID, d.Job.State = d.Deployment.JobID, d.Deployment.JobState
	d.Job.ServerID, d.Job.EnvironmentID, d.Job.ProjectID = d.Deployment.ServerID, d.Deployment.EnvironmentID, d.Deployment.ProjectID

	if d.StatusEvents, err = queryAll(ctx, tx, func(rows pgx.Rows) (DeploymentStatusEvent, error) {
		var e DeploymentStatusEvent
		return e, rows.Scan(&e.Revision, &e.FromStatus, &e.ToStatus, &e.OccurredAt)
	}, `SELECT revision, from_status, to_status, occurred_at FROM deployment_events WHERE deployment_id=$1 ORDER BY revision DESC LIMIT $2`,
		deploymentID, MaxDetailDeploymentEvents); err != nil {
		return DeploymentDetail{}, err
	}
	if d.JobEvents, err = queryAll(ctx, tx, func(rows pgx.Rows) (JobEventView, error) {
		var e JobEventView
		return e, rows.Scan(&e.Sequence, &e.EventType, &e.AttemptID, &e.OccurredAt)
	}, `SELECT sequence, event_type, attempt_id, occurred_at FROM agent_job_events WHERE job_id=$1 ORDER BY sequence DESC LIMIT $2`,
		d.Job.ID, MaxDetailJobEvents); err != nil {
		return DeploymentDetail{}, err
	}
	if d.Attempts, err = queryAll(ctx, tx, func(rows pgx.Rows) (AttemptView, error) {
		var a AttemptView
		return a, rows.Scan(&a.ID, &a.AttemptNumber, &a.State, &a.FailureClass, &a.Fence, &a.ServerID, &a.ReleaseStrategy, &a.CreatedAt, &a.ClosedAt)
	}, `SELECT a.id, a.attempt_number, a.state, COALESCE(a.failure_class,''), a.fence, a.server_id, COALESCE(x.release_strategy,''), a.created_at, a.closed_at
FROM agent_job_attempts a LEFT JOIN agent_job_attempt_execution x ON x.attempt_id=a.id
WHERE a.job_id=$1 ORDER BY a.attempt_number DESC LIMIT $2`, d.Job.ID, MaxDetailAttempts); err != nil {
		return DeploymentDetail{}, err
	}

	var rel ReleaseView
	err = tx.QueryRow(ctx, `SELECT id, state, strategy, commit_sha, COALESCE(encode(image_digest,'hex'),''), created_at, updated_at FROM releases WHERE deployment_id=$1`, deploymentID).
		Scan(&rel.ID, &rel.State, &rel.Strategy, &rel.CommitSHA, &rel.ImageDigest, &rel.CreatedAt, &rel.UpdatedAt)
	switch {
	case err == nil:
		d.Release = &rel
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return DeploymentDetail{}, fmt.Errorf("read release: %w", err)
	}
	if d.Release != nil {
		if d.RuntimeInstances, err = queryAll(ctx, tx, func(rows pgx.Rows) (RuntimeInstanceView, error) {
			var i RuntimeInstanceView
			return i, rows.Scan(&i.ID, &i.AttemptID, &i.Fence, &i.ObservedState, &i.CreatedAt)
		}, `SELECT id, attempt_id, fence, observed_state, created_at FROM release_runtime_instances WHERE release_id=$1 ORDER BY id DESC LIMIT $2`,
			d.Release.ID, MaxDetailRuntimeInstances); err != nil {
			return DeploymentDetail{}, err
		}
	}
	// Environment ids are not owner-bound: the active release id is surfaced
	// only when that release's deployment belongs to the principal.
	var active *string
	err = tx.QueryRow(ctx, `SELECT rh.active_release_id FROM route_heads rh
JOIN releases r ON r.id=rh.active_release_id
JOIN agent_jobs j ON j.deployment_id=r.deployment_id AND j.owner_id=$2
WHERE rh.environment_id=$1`, d.Job.EnvironmentID, principal.OwnerID).Scan(&active)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return DeploymentDetail{}, fmt.Errorf("read route head: %w", err)
	}
	if active != nil {
		d.RouteActiveReleaseID = *active
		d.ReleaseIsActive = d.Release != nil && d.Release.ID == *active
	}
	if err = tx.Commit(ctx); err != nil {
		return DeploymentDetail{}, fmt.Errorf("commit deployment detail: %w", err)
	}
	return d, nil
}

func queryAll[T any](ctx context.Context, tx pgx.Tx, scan func(pgx.Rows) (T, error), sql string, args ...any) ([]T, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("read detail list: %w", err)
	}
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("scan detail list: %w", err)
		}
		out = append(out, v)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("read detail list: %w", err)
	}
	return out, nil
}
