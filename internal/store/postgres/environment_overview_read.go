package postgres

import (
	"context"
	"fmt"
	"time"

	"autodeploy/internal/auth"
	"autodeploy/internal/deployment"

	"github.com/jackc/pgx/v5"
)

// MaxEnvironmentsListed bounds ListEnvironmentOverview (silent cap, by
// environment_id order, like MaxProjectsListed).
const MaxEnvironmentsListed = 200

// EnvironmentOverview is one environment row of the operator overview.
//
// Environments are derived (there is no environments table): the set is the
// owner's binding targets plus every environment the owner has a job on.
// Owned is true only when the owner has an agent_jobs row for the
// environment (the same rule as environmentOwnedBy). Route, config,
// reconciliation, active release and rollback fields are populated ONLY for
// owned environments: environment_id is a global free-text namespace and
// route_heads, releases, environment_runtime_configs and environment_slots
// carry no owner column.
type EnvironmentOverview struct {
	EnvironmentID string

	ProjectID     string
	Bound         bool
	BindingActive bool
	DeployRef     string

	ServerID        string
	ServerName      string
	ServerStatus    string
	LastHeartbeatAt time.Time // zero when the server never sent a heartbeat

	Owned           bool
	Hostname        string
	RouteRevision   int64
	ActiveReleaseID string
	ActiveCommit    string
	ActiveSince     time.Time

	ReconciliationRequired bool
	ReconcilingAttemptID   int64
	Strategy               string

	Latest              *DeploymentSummary
	LatestFailureClass  string
	LatestFailureReason string

	RollbackTargetReleaseID string
	RollbackTargetCommit    string
}

// environmentOverviewSQL is the single statement behind
// ListEnvironmentOverview. $1 = owner id, $2 = row cap, $3 = in-flight deployment statuses.
//
// The rb lateral mirrors evaluateRollback (rollback_repository.go): retained
// target of the same environment owned by $1, active source owned by $1 on the
// same server, route head and slot not flagged and not occupied, runtime config
// present, a winning succeeded activation, no revoked frozen secret, and no
// in-flight deployment. The POST still re-evaluates under locks.
//
// Security invariant: every join to a table without an owner column
// (route_heads, environment_slots, environment_runtime_configs, releases) is
// conditioned on oe.environment_id IS NOT NULL, i.e. the environment is owned
// by $1 through agent_jobs. servers is joined with s.owner_id=$1.
var environmentOverviewSQL = `
WITH owned_envs AS (
  SELECT DISTINCT environment_id FROM agent_jobs WHERE owner_id=$1
), env AS (
  SELECT DISTINCT ON (u.environment_id) u.environment_id, u.project_id, u.server_id, u.deploy_ref, u.active, u.bound
  FROM (
    SELECT environment_id, project_id, server_id, deploy_ref, active, true AS bound, 0 AS pri
      FROM github_project_bindings
     WHERE owner_id=$1 AND environment_id IS NOT NULL
    UNION ALL
    SELECT DISTINCT environment_id, project_id, NULL::text, NULL::text, NULL::boolean, false, 1
      FROM agent_jobs
     WHERE owner_id=$1
  ) u
  ORDER BY u.environment_id, u.pri, u.project_id
), capped AS (
  SELECT * FROM env ORDER BY environment_id LIMIT $2
)
SELECT e.environment_id, e.project_id, e.bound, COALESCE(e.active,false), COALESCE(e.deploy_ref,''),
       COALESCE(s.id,''), COALESCE(s.name,''), COALESCE(s.status,''), s.last_heartbeat_at,
       oe.environment_id IS NOT NULL,
       COALESCE(rh.hostname,''), COALESCE(rh.route_revision,0), COALESCE(rh.active_release_id,''), COALESCE(ar.commit_sha,''), ar.updated_at,
       COALESCE(sl.reconciliation_required,false), COALESCE(sl.reconciliation_attempt_id,0), COALESCE(rc.release_strategy,''),
       ld.id, ld.project_id, ld.environment_id, ld.server_id, ld.commit_sha, ld.status, ld.job_id, ld.job_state, ld.cancel_requested, ld.created_at, ld.updated_at,
       ldfa.failure_class, ldfr.body,
       COALESCE(rb.id,''), COALESCE(rb.commit_sha,'')
FROM capped e
LEFT JOIN owned_envs oe ON oe.environment_id=e.environment_id
LEFT JOIN LATERAL (
  SELECT d.id, j.project_id, j.environment_id, j.server_id, d.commit_sha, d.status, j.id AS job_id, j.state AS job_state,
         j.cancel_requested_at IS NOT NULL AS cancel_requested, d.created_at, d.updated_at
  FROM agent_jobs j JOIN deployments d ON d.id=j.deployment_id
  WHERE j.owner_id=$1 AND j.environment_id=e.environment_id
  ORDER BY d.created_at DESC, d.id DESC LIMIT 1
) ld ON true` + jobFailureLaterals("ld", "ld.job_id") + `
LEFT JOIN servers s ON s.id=COALESCE(e.server_id, ld.server_id) AND s.owner_id=$1
LEFT JOIN route_heads rh ON rh.environment_id=e.environment_id AND oe.environment_id IS NOT NULL
LEFT JOIN releases ar ON ar.id=rh.active_release_id AND ar.environment_id=e.environment_id AND oe.environment_id IS NOT NULL
  AND EXISTS (SELECT 1 FROM agent_jobs aj WHERE aj.deployment_id=ar.deployment_id AND aj.owner_id=$1 AND aj.job_kind='deployment_execution')
LEFT JOIN environment_slots sl ON sl.environment_id=e.environment_id AND oe.environment_id IS NOT NULL
LEFT JOIN environment_runtime_configs rc ON rc.environment_id=e.environment_id AND oe.environment_id IS NOT NULL
LEFT JOIN LATERAL (
  SELECT r.id, r.commit_sha FROM releases r
  JOIN agent_jobs tj ON tj.deployment_id=r.deployment_id AND tj.job_kind='deployment_execution' AND tj.owner_id=$1 AND tj.environment_id=e.environment_id
  JOIN LATERAL (
    SELECT ro.attempt_id FROM release_operations ro
    JOIN agent_jobs wj ON wj.id=ro.job_id AND wj.rollback_target_release_id IS NULL
    WHERE ro.release_id=r.id AND ro.kind='activate' AND ro.state='succeeded' ORDER BY ro.id DESC LIMIT 1
  ) w ON true
  WHERE oe.environment_id IS NOT NULL AND r.environment_id=e.environment_id
    AND r.state='retained'
    AND rh.active_release_id IS NOT NULL AND rh.active_release_id <> '' AND r.id <> rh.active_release_id AND NOT rh.reconciliation_required
    AND ar.id IS NOT NULL AND ar.state='active'
    AND EXISTS (SELECT 1 FROM agent_jobs sj WHERE sj.deployment_id=ar.deployment_id AND sj.job_kind='deployment_execution' AND sj.owner_id=$1 AND sj.server_id=tj.server_id)
    AND sl.environment_id IS NOT NULL AND NOT sl.reconciliation_required AND sl.current_attempt_id IS NULL
    AND rc.environment_id IS NOT NULL
    AND NOT EXISTS (SELECT 1 FROM frozen_secret_versions fz JOIN secret_versions sv ON sv.id=fz.secret_version_id WHERE fz.attempt_id=w.attempt_id AND sv.state='revoked')
    AND NOT EXISTS (SELECT 1 FROM deployments dd JOIN agent_jobs dj ON dj.deployment_id=dd.id WHERE dj.owner_id=$1 AND dd.environment_id=e.environment_id AND dd.status = ANY($3))
  ORDER BY r.created_at DESC, r.id DESC LIMIT 1
) rb ON true
ORDER BY e.environment_id`

// inFlightStatuses lists the deployment statuses that make evaluateRollback
// refuse with ErrRollbackInFlight.
func inFlightStatuses() []string {
	out := make([]string, 0, 8)
	for _, s := range deployment.ActiveStatuses() {
		out = append(out, string(s))
	}
	return out
}

// ListEnvironmentOverview returns at most MaxEnvironmentsListed environment
// rows for the owner in one read-only REPEATABLE READ statement (live session
// checked in the same transaction). See EnvironmentOverview for scoping.
func (r *OperationsReadRepository) ListEnvironmentOverview(ctx context.Context, principal auth.Principal) ([]EnvironmentOverview, error) {
	if err := r.ready(principal); err != nil {
		return nil, err
	}
	tx, err := r.beginRead(ctx, principal, pgx.RepeatableRead)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	rows, err := tx.Query(ctx, environmentOverviewSQL, principal.OwnerID, MaxEnvironmentsListed, inFlightStatuses())
	if err != nil {
		return nil, fmt.Errorf("list environment overview: %w", err)
	}
	defer rows.Close()
	out := []EnvironmentOverview{}
	for rows.Next() {
		var o EnvironmentOverview
		var (
			heartbeat, activeSince                                           *time.Time
			ldID, ldProject, ldEnv, ldServer, ldCommit, ldStatus, ldJobState *string
			ldJobID                                                          *int64
			ldCancel                                                         *bool
			ldCreated, ldUpdated                                             *time.Time
			ldClass, ldReasonBody                                            *string
		)
		if err = rows.Scan(&o.EnvironmentID, &o.ProjectID, &o.Bound, &o.BindingActive, &o.DeployRef,
			&o.ServerID, &o.ServerName, &o.ServerStatus, &heartbeat,
			&o.Owned,
			&o.Hostname, &o.RouteRevision, &o.ActiveReleaseID, &o.ActiveCommit, &activeSince,
			&o.ReconciliationRequired, &o.ReconcilingAttemptID, &o.Strategy,
			&ldID, &ldProject, &ldEnv, &ldServer, &ldCommit, &ldStatus, &ldJobID, &ldJobState, &ldCancel, &ldCreated, &ldUpdated,
			&ldClass, &ldReasonBody,
			&o.RollbackTargetReleaseID, &o.RollbackTargetCommit); err != nil {
			return nil, fmt.Errorf("scan environment overview: %w", err)
		}
		if heartbeat != nil {
			o.LastHeartbeatAt = *heartbeat
		}
		if activeSince != nil {
			o.ActiveSince = *activeSince
		}
		if ldID != nil {
			d := DeploymentSummary{DeploymentID: *ldID, ProjectID: *ldProject, EnvironmentID: *ldEnv, ServerID: *ldServer,
				CommitSHA: *ldCommit, Status: *ldStatus, JobID: *ldJobID, JobState: *ldJobState, CancelRequested: *ldCancel,
				CreatedAt: *ldCreated, UpdatedAt: *ldUpdated,
				FailureClass: derefString(ldClass), FailureReason: parseHealthFailureReason(derefString(ldReasonBody))}
			o.Latest = &d
			o.LatestFailureClass, o.LatestFailureReason = d.FailureClass, d.FailureReason
		}
		out = append(out, o)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("list environment overview: %w", err)
	}
	rows.Close()
	if err = tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit list environment overview: %w", err)
	}
	return out, nil
}
