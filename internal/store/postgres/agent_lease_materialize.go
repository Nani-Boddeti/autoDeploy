package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"autodeploy/internal/deployment"
	"autodeploy/internal/github"
	"autodeploy/internal/queue"

	"github.com/jackc/pgx/v5"
)

// AuthorizeMaterialization re-derives, from locked rows only, whether
// serverID may right now materialize the secret values attemptID froze at
// claim time, and returns the exact frozen reference set if so. It performs
// no mutation and returns no plaintext: decryption happens in the caller,
// after this transaction commits, so no KEK operation is ever performed
// while holding a row lock (ADR 0003 "Agent authorization and
// materialization" requires the authorization check immediately before
// decryption, not that the two share a transaction).
//
// Lock order is exactly Complete's -- server credential -> environment slot
// -> job -> attempt -- so this route can never invert the ADR 0004 hierarchy
// against a concurrent claim, renew, complete, or sweep. Every lock here is
// FOR SHARE rather than FOR UPDATE: this is a read-only re-authorization, so
// it must not serialize against other readers, and it holds nothing a
// mutating path needs exclusively for longer than its own short transaction.
//
// Every failure returns ErrServerAuthorizationDenied, which the transport
// maps to the same single generic 401 every other agent route uses: an agent
// presenting a stale fence, a closed attempt, an expired authority, a
// cancelled job, or another server's attempt must not be able to tell those
// cases apart.
func (r *AgentLeaseRepository) AuthorizeMaterialization(ctx context.Context, serverID string, presentedDigest [32]byte, attemptID, fence int64) (queue.MaterializationAuthorization, error) {
	if r == nil || r.pool == nil || !github.ValidOpaqueID(serverID) || attemptID <= 0 || fence <= 0 {
		return queue.MaterializationAuthorization{}, ErrAgentLeaseInvalidInput
	}

	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return queue.MaterializationAuthorization{}, fmt.Errorf("begin materialization authorization: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err = lockActiveServerCredential(ctx, tx, serverID, presentedDigest); err != nil {
		return queue.MaterializationAuthorization{}, err
	}

	var jobID int64
	var environmentID, attemptServerID string
	if err = tx.QueryRow(ctx, `SELECT job_id, environment_id, server_id FROM agent_job_attempts WHERE id=$1`, attemptID).Scan(&jobID, &environmentID, &attemptServerID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return queue.MaterializationAuthorization{}, ErrServerAuthorizationDenied
		}
		return queue.MaterializationAuthorization{}, fmt.Errorf("locate agent job attempt for materialization: %w", err)
	}
	if attemptServerID != serverID {
		return queue.MaterializationAuthorization{}, ErrServerAuthorizationDenied
	}

	var slotFence int64
	var slotCurrentAttemptID *int64
	var reconciliationRequired bool
	if err = tx.QueryRow(ctx, `SELECT fence, current_attempt_id, reconciliation_required FROM environment_slots WHERE environment_id=$1 FOR SHARE`, environmentID).
		Scan(&slotFence, &slotCurrentAttemptID, &reconciliationRequired); err != nil {
		return queue.MaterializationAuthorization{}, fmt.Errorf("lock environment slot for materialization: %w", err)
	}
	// The presented fence must be the slot's current fence and this attempt
	// must still be the slot's occupant. A newer fence means this attempt's
	// authority was already withdrawn (expiry, cancellation, or revocation),
	// and an environment awaiting reconciliation has no live occupant at all.
	if slotFence != fence || reconciliationRequired || slotCurrentAttemptID == nil || *slotCurrentAttemptID != attemptID {
		return queue.MaterializationAuthorization{}, ErrServerAuthorizationDenied
	}

	var ownerID, projectID, deploymentID string
	var cancelRequestedAt *time.Time
	if err = tx.QueryRow(ctx, `SELECT owner_id, project_id, deployment_id, cancel_requested_at FROM agent_jobs WHERE id=$1 FOR SHARE`, jobID).
		Scan(&ownerID, &projectID, &deploymentID, &cancelRequestedAt); err != nil {
		return queue.MaterializationAuthorization{}, fmt.Errorf("lock agent job for materialization: %w", err)
	}
	// ADR 0003 lists cancellation state among the checks performed
	// immediately before decryption: work that has been told to stop must not
	// be handed fresh plaintext.
	if cancelRequestedAt != nil {
		return queue.MaterializationAuthorization{}, ErrServerAuthorizationDenied
	}

	var attemptState string
	var attemptFence int64
	var authorityExpiresAt time.Time
	if err = tx.QueryRow(ctx, `SELECT state, fence, authority_expires_at FROM agent_job_attempts WHERE id=$1 FOR SHARE`, attemptID).
		Scan(&attemptState, &attemptFence, &authorityExpiresAt); err != nil {
		return queue.MaterializationAuthorization{}, fmt.Errorf("lock agent job attempt for materialization: %w", err)
	}
	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return queue.MaterializationAuthorization{}, fmt.Errorf("read materialization clock: %w", err)
	}
	if attemptState != string(queue.AttemptLeased) || attemptFence != fence || !authorityExpiresAt.After(now) {
		return queue.MaterializationAuthorization{}, ErrServerAuthorizationDenied
	}

	// The frozen set is the authority on what this attempt may decrypt: the
	// agent never names variables or versions, so it cannot ask for anything
	// this attempt did not freeze at claim time. secret_versions'
	// owner/project/environment come from the version row itself rather than
	// the job, so a scope disagreement cannot be papered over by trusting
	// the caller's context.
	//
	// state IN ('active','superseded') is deliberate: rotation after the
	// freeze supersedes the frozen version, and ADR 0003 says executing work
	// keeps its frozen versions, so a superseded frozen version must still
	// materialize. 'revoked' must not -- that is compromise handling, which
	// the ADR says does withdraw frozen versions.
	rows, err := tx.Query(ctx, `SELECT f.variable_name, f.delivery_scope, f.secret_version_id, sv.secret_id, sv.owner_id, sv.project_id, sv.environment_id, sh.purpose, sh.delivery_scope, sv.state
		FROM frozen_secret_versions f
		JOIN secret_versions sv ON sv.id = f.secret_version_id
		JOIN secret_heads sh ON sh.id = sv.secret_id
		WHERE f.attempt_id=$1 ORDER BY f.variable_name`, attemptID)
	if err != nil {
		return queue.MaterializationAuthorization{}, fmt.Errorf("load frozen secret versions for materialization: %w", err)
	}
	defer rows.Close()
	var refs []queue.FrozenSecretReference
	for rows.Next() {
		var ref queue.FrozenSecretReference
		var versionOwner, versionProject, versionEnvironment, headDeliveryScope, versionState string
		if err := rows.Scan(&ref.VariableName, &ref.DeliveryScope, &ref.SecretVersionID, &ref.SecretID,
			&versionOwner, &versionProject, &versionEnvironment, &ref.Purpose, &headDeliveryScope, &versionState); err != nil {
			return queue.MaterializationAuthorization{}, fmt.Errorf("scan frozen secret version for materialization: %w", err)
		}
		if versionState == "revoked" {
			return queue.MaterializationAuthorization{}, ErrServerAuthorizationDenied
		}
		// Bind-time triggers already enforce these, but this is the
		// pre-decryption recheck ADR 0003 requires, and it must not depend on
		// a trigger having held for rows written earlier by other code.
		if versionOwner != ownerID || versionProject != projectID || versionEnvironment != environmentID || headDeliveryScope != ref.DeliveryScope {
			return queue.MaterializationAuthorization{}, ErrServerAuthorizationDenied
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		return queue.MaterializationAuthorization{}, fmt.Errorf("load frozen secret versions for materialization: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return queue.MaterializationAuthorization{}, fmt.Errorf("commit materialization authorization: %w", err)
	}
	return queue.MaterializationAuthorization{
		AttemptID: attemptID, Fence: fence, JobID: jobID,
		OwnerID: ownerID, ProjectID: projectID, EnvironmentID: environmentID, DeploymentID: deploymentID,
		Secrets: refs,
	}, nil
}

// AuthorizeCheckout re-derives, from locked rows only, whether serverID may
// right now reserve or mint a checkout credential for the exact commit
// attemptID's job was enqueued for, and returns the authority a checkout
// needs if so. It performs no mutation, mints nothing, and matches
// AuthorizeMaterialization's lock order and occupancy checks exactly
// (credential -> slot -> job -> attempt, every lock FOR SHARE): a checkout
// credential is exactly as sensitive as a decrypted secret value (ADR 0003
// "Agent authorization and materialization" draws no distinction), so the
// same re-authorization discipline applies immediately before either is
// minted.
//
// RepositoryID and CommitSHA are read from the job row itself, never
// accepted from the caller: the frozen job is the sole authority on what
// commit this attempt may check out, exactly as the frozen secret set is the
// sole authority on what a materialization request may decrypt.
//
// Every failure returns ErrServerAuthorizationDenied, mapped by the
// transport to the same generic 401 every other agent route uses.
func (r *AgentLeaseRepository) AuthorizeCheckout(ctx context.Context, serverID string, presentedDigest [32]byte, attemptID, fence int64) (queue.CheckoutAuthorization, error) {
	if r == nil || r.pool == nil || !github.ValidOpaqueID(serverID) || attemptID <= 0 || fence <= 0 {
		return queue.CheckoutAuthorization{}, ErrAgentLeaseInvalidInput
	}

	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return queue.CheckoutAuthorization{}, fmt.Errorf("begin checkout authorization: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err = lockActiveServerCredential(ctx, tx, serverID, presentedDigest); err != nil {
		return queue.CheckoutAuthorization{}, err
	}

	var jobID int64
	var environmentID, attemptServerID string
	if err = tx.QueryRow(ctx, `SELECT job_id, environment_id, server_id FROM agent_job_attempts WHERE id=$1`, attemptID).Scan(&jobID, &environmentID, &attemptServerID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return queue.CheckoutAuthorization{}, ErrServerAuthorizationDenied
		}
		return queue.CheckoutAuthorization{}, fmt.Errorf("locate agent job attempt for checkout: %w", err)
	}
	if attemptServerID != serverID {
		return queue.CheckoutAuthorization{}, ErrServerAuthorizationDenied
	}

	var slotFence int64
	var slotCurrentAttemptID *int64
	var reconciliationRequired bool
	if err = tx.QueryRow(ctx, `SELECT fence, current_attempt_id, reconciliation_required FROM environment_slots WHERE environment_id=$1 FOR SHARE`, environmentID).
		Scan(&slotFence, &slotCurrentAttemptID, &reconciliationRequired); err != nil {
		return queue.CheckoutAuthorization{}, fmt.Errorf("lock environment slot for checkout: %w", err)
	}
	if slotFence != fence || reconciliationRequired || slotCurrentAttemptID == nil || *slotCurrentAttemptID != attemptID {
		return queue.CheckoutAuthorization{}, ErrServerAuthorizationDenied
	}

	var ownerID, projectID, deploymentID string
	var repositoryID int64
	var commitSHA string
	var cancelRequestedAt *time.Time
	if err = tx.QueryRow(ctx, `SELECT owner_id, project_id, deployment_id, repository_id, commit_sha, cancel_requested_at FROM agent_jobs WHERE id=$1 FOR SHARE`, jobID).
		Scan(&ownerID, &projectID, &deploymentID, &repositoryID, &commitSHA, &cancelRequestedAt); err != nil {
		return queue.CheckoutAuthorization{}, fmt.Errorf("lock agent job for checkout: %w", err)
	}
	if cancelRequestedAt != nil {
		return queue.CheckoutAuthorization{}, ErrServerAuthorizationDenied
	}

	var attemptState string
	var attemptFence int64
	var authorityExpiresAt time.Time
	if err = tx.QueryRow(ctx, `SELECT state, fence, authority_expires_at FROM agent_job_attempts WHERE id=$1 FOR SHARE`, attemptID).
		Scan(&attemptState, &attemptFence, &authorityExpiresAt); err != nil {
		return queue.CheckoutAuthorization{}, fmt.Errorf("lock agent job attempt for checkout: %w", err)
	}
	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return queue.CheckoutAuthorization{}, fmt.Errorf("read checkout clock: %w", err)
	}
	if attemptState != string(queue.AttemptLeased) || attemptFence != fence || !authorityExpiresAt.After(now) {
		return queue.CheckoutAuthorization{}, ErrServerAuthorizationDenied
	}

	if err := tx.Commit(ctx); err != nil {
		return queue.CheckoutAuthorization{}, fmt.Errorf("commit checkout authorization: %w", err)
	}
	return queue.CheckoutAuthorization{
		AttemptID: attemptID, Fence: fence, JobID: jobID,
		OwnerID: ownerID, ProjectID: projectID, EnvironmentID: environmentID, DeploymentID: deploymentID,
		RepositoryID: repositoryID, CommitSHA: commitSHA,
	}, nil
}

// ReportProgress records that attemptID has entered phase, advancing the
// deployment's own status so an operator can see where a running deployment
// is. It is the only way an agent influences deployment status short of
// completing, and it can only move forward along the execution path.
//
// Authorization is the same re-derivation from locked rows that
// AuthorizeMaterialization performs, in the same ADR 0004 order (credential ->
// slot -> job -> attempt); the job is locked FOR UPDATE because this path does
// mutate the deployment, which serializes concurrent progress reports for one
// job against each other and against Complete.
//
// Deliberately appends no agent_job_events row. ADR 0004's amendment gives the
// same reasoning for renewals: a long build would otherwise write unbounded
// rows. Phase history already lives in the deployment's own append-only event
// log.
func (r *AgentLeaseRepository) ReportProgress(ctx context.Context, serverID string, presentedDigest [32]byte, attemptID, fence int64, phase deployment.Status) error {
	if r == nil || r.pool == nil || !github.ValidOpaqueID(serverID) || attemptID <= 0 || fence <= 0 || !deployment.AgentReportablePhase(phase) {
		return ErrAgentLeaseInvalidInput
	}

	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin agent lease progress: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err = lockActiveServerCredential(ctx, tx, serverID, presentedDigest); err != nil {
		return err
	}

	var jobID int64
	var environmentID, attemptServerID string
	if err = tx.QueryRow(ctx, `SELECT job_id, environment_id, server_id FROM agent_job_attempts WHERE id=$1`, attemptID).Scan(&jobID, &environmentID, &attemptServerID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrServerAuthorizationDenied
		}
		return fmt.Errorf("locate agent job attempt for progress: %w", err)
	}
	if attemptServerID != serverID {
		return ErrServerAuthorizationDenied
	}

	var slotFence int64
	var slotCurrentAttemptID *int64
	var reconciliationRequired bool
	if err = tx.QueryRow(ctx, `SELECT fence, current_attempt_id, reconciliation_required FROM environment_slots WHERE environment_id=$1 FOR SHARE`, environmentID).
		Scan(&slotFence, &slotCurrentAttemptID, &reconciliationRequired); err != nil {
		return fmt.Errorf("lock environment slot for progress: %w", err)
	}
	if slotFence != fence || reconciliationRequired || slotCurrentAttemptID == nil || *slotCurrentAttemptID != attemptID {
		return ErrServerAuthorizationDenied
	}

	var deploymentID string
	var cancelRequestedAt *time.Time
	if err = tx.QueryRow(ctx, `SELECT deployment_id, cancel_requested_at FROM agent_jobs WHERE id=$1 FOR UPDATE`, jobID).Scan(&deploymentID, &cancelRequestedAt); err != nil {
		return fmt.Errorf("lock agent job for progress: %w", err)
	}
	// Cancellation of a leased job is cooperative: the attempt stays leased
	// and the deployment stays on the execution path until the agent
	// acknowledges. Progress must still be refused from that moment, both for
	// consistency with materialization and because advancing a deployment's
	// visible status toward healthy while it is being cancelled would tell an
	// operator the opposite of what is happening.
	if cancelRequestedAt != nil {
		return ErrServerAuthorizationDenied
	}

	var attemptState string
	var attemptFence int64
	var authorityExpiresAt time.Time
	if err = tx.QueryRow(ctx, `SELECT state, fence, authority_expires_at FROM agent_job_attempts WHERE id=$1 FOR SHARE`, attemptID).
		Scan(&attemptState, &attemptFence, &authorityExpiresAt); err != nil {
		return fmt.Errorf("lock agent job attempt for progress: %w", err)
	}
	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return fmt.Errorf("read progress clock: %w", err)
	}
	if attemptState != string(queue.AttemptLeased) || attemptFence != fence || !authorityExpiresAt.After(now) {
		return ErrServerAuthorizationDenied
	}

	var currentStatus string
	if err = tx.QueryRow(ctx, `SELECT status FROM deployments WHERE id=$1 FOR UPDATE`, deploymentID).Scan(&currentStatus); err != nil {
		return fmt.Errorf("lock deployment for progress: %w", err)
	}
	current := deployment.Status(currentStatus)
	currentOrder, onPath := deployment.ExecutionOrder(current)
	if !onPath {
		// The deployment already left the execution path (cancelled, failed,
		// superseded, rolled back). Its history must not be rewritten by a
		// report from work that is winding down.
		return ErrServerAuthorizationDenied
	}
	targetOrder, _ := deployment.ExecutionOrder(phase)
	switch {
	case targetOrder <= currentOrder:
		// Already at or past this phase: absorb the duplicate. A progress
		// report retried after a network failure, or a phase re-entered by a
		// resumed executor, must not fail a build that is otherwise fine.
		return tx.Commit(ctx)
	case targetOrder > currentOrder+1:
		// A skip would claim a phase that never happened. The executor reports
		// each phase as it enters it, so this is a caller bug, not a race, and
		// inventing the intermediate transitions would forge history in an
		// append-only event log.
		return ErrAgentLeaseInvalidInput
	}
	if err = transitionDeployment(ctx, tx, r.deployments, deploymentID, phase, now, r.inject); err != nil {
		return err
	}
	if err = r.inject("progress"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
