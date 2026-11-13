package postgres

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"unicode/utf8"

	"autodeploy/internal/github"
	"autodeploy/internal/runtime/dockerid"

	"github.com/jackc/pgx/v5"
)

// Predecessor drain outcomes the agent may report (migration 000024).
const (
	DrainOutcomeStopped        = "stopped"
	DrainOutcomeAlreadyStopped = "already_stopped"
	DrainOutcomeGone           = "gone"
	DrainOutcomeStopFailed     = "stop_failed"
	DrainOutcomeRouteMoved     = "route_moved"
	DrainOutcomeIdentityError  = "identity_error"

	maxDrainResultSummaryBytes = 2048
)

func validDrainOutcome(outcome string) bool {
	switch outcome {
	case DrainOutcomeStopped, DrainOutcomeAlreadyStopped, DrainOutcomeGone,
		DrainOutcomeStopFailed, DrainOutcomeRouteMoved, DrainOutcomeIdentityError:
		return true
	}
	return false
}

// validDrainSummary mirrors release_drain_results.outcome_summary's CHECK:
// at most 2048 bytes, valid UTF-8, no ASCII control characters or DEL.
func validDrainSummary(s string) bool {
	if len(s) > maxDrainResultSummaryBytes || !utf8.ValidString(s) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return false
		}
	}
	return true
}

// RecordDrainResult records what the agent's predecessor drain for the
// activating attempt ended in (step 20B). It is visibility only (owner
// decision B-D1): nothing reads the row to act, a stop failure never undoes or
// blocks anything.
//
// The call is credential-authenticated only: by the time the agent reports,
// the attempt is closed and no lease or fence exists, so authority is
// DERIVED. The attempt must belong to this server and be succeeded; its
// succeeded activate operation must have expected_active_release_id equal to
// the reported predecessor release; and the reported predecessor runtime
// instance must equal derivePredecessor's answer for that operation (10e-2).
// Any disagreement is ErrAgentLeaseIntegrityConflict, so an agent can never
// attach a drain result to a release pair the server did not authorize.
//
// Lock order: server credential (SHARE) only; every other read is a plain
// non-locking SELECT (attempt identity columns and a succeeded operation's
// identity are immutable), so ADR 0004's lock order is unchanged. The write is
// INSERT ... ON CONFLICT (attempt_id) DO NOTHING: a replay with an identical
// payload is a no-op success, a different payload is an integrity conflict
// (first report wins; rows are immutable).
func (r *AgentLeaseRepository) RecordDrainResult(ctx context.Context, serverID string, presentedDigest [32]byte, attemptID int64, predecessorReleaseID, predecessorRuntimeInstanceID, outcome, summary string) error {
	if r == nil || r.pool == nil || !github.ValidOpaqueID(serverID) || attemptID <= 0 ||
		!github.ValidOpaqueID(predecessorReleaseID) || !validDrainRuntimeInstanceID(predecessorRuntimeInstanceID) ||
		!validDrainOutcome(outcome) || !validDrainSummary(summary) {
		return ErrAgentLeaseInvalidInput
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin drain result record: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err = lockActiveServerCredential(ctx, tx, serverID, presentedDigest); err != nil {
		return err
	}
	var attemptServerID, environmentID, attemptState string
	var jobID int64
	if err = tx.QueryRow(ctx, `SELECT server_id, environment_id, state, job_id FROM agent_job_attempts WHERE id=$1`, attemptID).Scan(&attemptServerID, &environmentID, &attemptState, &jobID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrServerAuthorizationDenied
		}
		return fmt.Errorf("read drain result attempt: %w", err)
	}
	if attemptServerID != serverID {
		return ErrServerAuthorizationDenied
	}
	if attemptState != "succeeded" {
		return ErrAgentLeaseIntegrityConflict
	}
	var releaseID string
	var expectedActiveReleaseID *string
	if err = tx.QueryRow(ctx, `SELECT release_id, expected_active_release_id FROM release_operations WHERE attempt_id=$1 AND kind='activate' AND state=$2 ORDER BY id DESC LIMIT 1`,
		attemptID, activationStateSucceeded).Scan(&releaseID, &expectedActiveReleaseID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAgentLeaseIntegrityConflict
		}
		return fmt.Errorf("read drain result activate operation: %w", err)
	}
	if expectedActiveReleaseID == nil || *expectedActiveReleaseID != predecessorReleaseID {
		return ErrAgentLeaseIntegrityConflict
	}
	candidateInstanceID, err := activationCandidateInstance(ctx, tx, jobID, releaseID, attemptID)
	if err != nil {
		return err
	}
	_, derivedRuntimeInstanceID, err := derivePredecessor(ctx, tx, expectedActiveReleaseID, releaseID, candidateInstanceID)
	if err != nil {
		return err
	}
	if derivedRuntimeInstanceID != predecessorRuntimeInstanceID {
		return ErrAgentLeaseIntegrityConflict
	}
	tag, err := tx.Exec(ctx, `INSERT INTO release_drain_results(attempt_id, environment_id, release_id, predecessor_release_id, predecessor_runtime_instance_id, outcome, outcome_summary)
		VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (attempt_id) DO NOTHING`,
		attemptID, environmentID, releaseID, predecessorReleaseID, predecessorRuntimeInstanceID, outcome, summary)
	if err != nil {
		return classifyActivationResultError(fmt.Errorf("insert drain result: %w", err))
	}
	if tag.RowsAffected() == 0 {
		var storedOutcome, storedSummary string
		if err = tx.QueryRow(ctx, `SELECT outcome, outcome_summary FROM release_drain_results WHERE attempt_id=$1`, attemptID).Scan(&storedOutcome, &storedSummary); err != nil {
			return fmt.Errorf("read existing drain result: %w", err)
		}
		// Release and predecessor columns were derived from the same immutable
		// operation as above, so equality of outcome and summary is the whole
		// replay comparison.
		if storedOutcome != outcome || storedSummary != summary {
			return ErrAgentLeaseIntegrityConflict
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return classifyActivationResultError(fmt.Errorf("commit drain result: %w", err))
	}
	return nil
}

// validDrainRuntimeInstanceID accepts exactly the decimal attempt id
// dockerid.AttemptRuntimeInstanceID produces (no sign, no leading zero).
func validDrainRuntimeInstanceID(s string) bool {
	n, err := strconv.ParseInt(s, 10, 64)
	return err == nil && n > 0 && dockerid.AttemptRuntimeInstanceID(n) == s
}
