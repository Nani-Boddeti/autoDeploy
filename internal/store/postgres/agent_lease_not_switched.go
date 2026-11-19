package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"autodeploy/internal/deployment"

	"github.com/jackc/pgx/v5"
)

// Stop-then-start auto-finalize (ADR 0005, slice 22-D).
//
// A stop-then-start candidate that fails BEFORE any route write leaves the
// predecessor stopped and restarted by the agent, and the host route file
// untouched. The agent says so with the not_switched outcome on its failed
// activation-result report. tx2 normally blocks the environment on every
// failure (the switch may have happened); the one exception is this branch,
// which finalizes a DEFINITE not-switched failure in the same transaction:
// the existing block routine, then the existing operator "not_activated"
// writes (resolveNotActivated), so no migration and no forked SQL (migration
// 000021 forbids switch_authorized -> failed, which is why the block comes
// first). The report is never trusted alone: every condition below is
// re-verified under the locks tx2 already holds.

// notSwitchedSummaryPrefix marks an operation this branch finalized, so a
// replay of the same report is recognised as already answered.
const notSwitchedSummaryPrefix = "not_switched: "

// notSwitchedOperationSummary is release_operations.outcome_summary of an
// operation finalized by the not_switched branch. summary is the already
// sanitized report text (at most 512 bytes), so the result fits the column.
func notSwitchedOperationSummary(summary string) string {
	return sanitizeOutcomeSummary(notSwitchedSummaryPrefix + summary)
}

type notSwitchedDisposition int

const (
	// notSwitchedCommitted: the finalize is durable; the report is answered nil.
	notSwitchedCommitted notSwitchedDisposition = iota
	// notSwitchedReturn: return the accompanying error as is (a known-transient
	// failure the agent retries, or a block that could not be written, itself
	// retryable). Nothing durable was written.
	notSwitchedReturn
	// notSwitchedBlockHere: the transaction is usable (rolled back to the
	// savepoint or untouched) and holds the slot/job/attempt/operation locks;
	// the caller records today's plain block in it.
	notSwitchedBlockHere
	// notSwitchedBlockFresh: the transaction is gone (a deterministic COMMIT
	// or savepoint failure); the caller records today's block in a fresh one.
	notSwitchedBlockFresh
)

type notSwitchedInput struct {
	jobID, attemptID, operationID int64
	environmentID, deploymentID   string
	candidateReleaseID            string
	expectedRouteRevision         int64
	expectedActiveReleaseID       *string
	// summary is the sanitized report text.
	summary string
}

const notSwitchedSavepoint = "not_switched_finalize"

// finalizeNotSwitched is the not_switched branch of tx2. The caller has passed
// the credential, fence, operation and occupant checks (op switch_authorized,
// attempt the leased occupant at this fence) and holds, in ADR 0004 order,
// credential SHARE -> environment slot -> job -> attempt -> operation.
//
// Conditions (all must hold, else the disposition is notSwitchedBlockHere and
// nothing was written):
//
//	expected_active_release_id is set (a predecessor existed);
//	the job is not a rollback job and has no cancellation requested;
//	the frozen strategy is stop_then_start;
//	the deployment is activating;
//	the route head is unflagged and still at the authorized revision and
//	active release;
//	the candidate release is ready (a retained release is a rollback-style
//	activation) and the predecessor release is active.
//
// Lock order after the occupant check stays deployment (6) -> route head
// (6b) -> releases ascending (7), all held to commit; the savepoint is taken
// FIRST so any error in the reads or writes can be undone and the plain block
// recorded instead. Never a rejection, never a success: a deterministic error
// (a negative definition, see isDeterministicActivationFailure) degrades to
// today's block; only a known-transient error is returned for the agent to
// retry.
func (r *AgentLeaseRepository) finalizeNotSwitched(ctx context.Context, tx pgx.Tx, in notSwitchedInput) (notSwitchedDisposition, error) {
	if in.expectedActiveReleaseID == nil {
		return notSwitchedBlockHere, nil
	}
	_, err := tx.Exec(ctx, `SAVEPOINT `+notSwitchedSavepoint)
	if err == nil {
		err = r.inject("not_switched_savepoint")
	}
	if err != nil {
		// The transaction may be unusable: a transient error is retried, a
		// deterministic one is recorded from a fresh transaction.
		if !isDeterministicActivationFailure(err) {
			return notSwitchedReturn, fmt.Errorf("savepoint for not_switched finalize: %w", err)
		}
		return notSwitchedBlockFresh, nil
	}
	// undo rolls back to the savepoint (every lock taken before it survives)
	// and degrades to the plain block. A transient cause is returned instead.
	undo := func(cause error) (notSwitchedDisposition, error) {
		if !isDeterministicActivationFailure(cause) {
			return notSwitchedReturn, cause
		}
		if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT `+notSwitchedSavepoint); err != nil {
			return notSwitchedReturn, blockUnrecorded(fmt.Errorf("rollback to savepoint after a failed not_switched finalize: %w", err))
		}
		return notSwitchedBlockHere, nil
	}

	eligible, err := r.notSwitchedEligible(ctx, tx, in)
	if err == nil {
		err = r.inject("not_switched_checks")
	}
	if err != nil {
		return undo(fmt.Errorf("verify not_switched conditions: %w", err))
	}
	if !eligible {
		return notSwitchedBlockHere, nil
	}

	// The block first (the only legal route out of switch_authorized), then the
	// operator not_activated writes, which close everything and clear both
	// flags. The route head is unflagged (verified above), so the block flags
	// it and the resolve clears it again: net, its revision and release are
	// untouched.
	if err = blockActivationForReconciliation(ctx, tx, r.inject, in.jobID, in.attemptID, in.operationID, in.environmentID, in.summary); err != nil {
		return undo(err)
	}
	// The clock is read AFTER the block: the block stamps the slot's updated_at
	// with its own reading and the slot trigger requires monotonic updates, so
	// the resolve's timestamp must not precede it.
	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err == nil {
		err = r.inject("finalize_clock")
	}
	if err != nil {
		return undo(fmt.Errorf("not_switched canonical time: %w", err))
	}
	if err = r.resolveNotActivated(ctx, tx, in.jobID, in.attemptID, in.operationID, in.environmentID, in.deploymentID, true, notSwitchedOperationSummary(in.summary), now); err != nil {
		return undo(err)
	}
	if err = tx.Commit(ctx); err != nil {
		if !isDeterministicActivationFailure(err) {
			// Unknown outcome: stay retryable, the replay path answers it.
			return notSwitchedReturn, fmt.Errorf("commit agent lease not_switched finalize: %w", err)
		}
		return notSwitchedBlockFresh, nil
	}
	return notSwitchedCommitted, nil
}

// notSwitchedEligible reads and checks the conditions listed on
// finalizeNotSwitched. It writes nothing. A false answer is not an error.
func (r *AgentLeaseRepository) notSwitchedEligible(ctx context.Context, tx pgx.Tx, in notSwitchedInput) (bool, error) {
	// The job row is already locked FOR UPDATE by tx2.
	var rollback, cancelRequested bool
	if err := tx.QueryRow(ctx, `SELECT rollback_target_release_id IS NOT NULL, cancel_requested_at IS NOT NULL FROM agent_jobs WHERE id=$1`, in.jobID).Scan(&rollback, &cancelRequested); err != nil {
		return false, fmt.Errorf("read job for not_switched: %w", err)
	}
	if rollback || cancelRequested {
		return false, nil
	}
	var strategy string
	if err := tx.QueryRow(ctx, `SELECT release_strategy FROM agent_job_attempt_execution WHERE attempt_id=$1`, in.attemptID).Scan(&strategy); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("read frozen strategy for not_switched: %w", err)
	}
	if strategy != "stop_then_start" {
		return false, nil
	}
	deploymentStatus, err := lockActivationDeployments(ctx, tx, in.jobID, in.deploymentID, in.expectedActiveReleaseID)
	if err != nil {
		return false, fmt.Errorf("lock deployment for not_switched: %w", err)
	}
	if deploymentStatus != string(deployment.StatusActivating) {
		return false, nil
	}
	var revision int64
	var active *string
	var flagged bool
	if err = tx.QueryRow(ctx, `SELECT route_revision, active_release_id, reconciliation_required FROM route_heads WHERE environment_id=$1 FOR UPDATE`, in.environmentID).Scan(&revision, &active, &flagged); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("lock route head for not_switched: %w", err)
	}
	if flagged || revision != in.expectedRouteRevision || !sameOptionalString(active, in.expectedActiveReleaseID) {
		return false, nil
	}
	rows, err := tx.Query(ctx, `SELECT id, state FROM releases WHERE id = ANY($1) ORDER BY id FOR UPDATE`, []string{in.candidateReleaseID, *in.expectedActiveReleaseID})
	if err != nil {
		return false, fmt.Errorf("lock releases for not_switched: %w", err)
	}
	defer rows.Close()
	states := map[string]string{}
	for rows.Next() {
		var id, state string
		if err = rows.Scan(&id, &state); err != nil {
			return false, fmt.Errorf("scan release for not_switched: %w", err)
		}
		states[id] = state
	}
	if err = rows.Err(); err != nil {
		return false, fmt.Errorf("lock releases for not_switched: %w", err)
	}
	return states[in.candidateReleaseID] == "ready" && states[*in.expectedActiveReleaseID] == "active", nil
}
