package postgres

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	"autodeploy/internal/github"
	"autodeploy/internal/runtime/dockerid"

	"github.com/jackc/pgx/v5"
)

// Cleanup results the agent may report (migration 000026).
const (
	CleanupResultRemoved = "removed"
	CleanupResultFailed  = "failed"
	CleanupResultSkipped = "skipped"

	CleanupErrorEngine   = "engine"
	CleanupErrorIdentity = "identity"
	CleanupErrorStop     = "stop"
	CleanupErrorOther    = "other"

	maxCleanupClaimLimit = 16

	// Claim kinds (step 21B-1). A retained claim names one container (the
	// winning activation's); a failed-candidate claim names one container per
	// attempt of a deployment that never activated its release.
	CleanupKindRetained        = "retained"
	CleanupKindFailedCandidate = "failed_candidate"
)

// CleanupPolicy is the retention policy (ADR 0005 "Retention and drain policy",
// owner decision B-D5). The zero value is invalid; use DefaultCleanupPolicy.
type CleanupPolicy struct {
	// KeepRetained is how many of an environment's newest retained releases
	// (by retention time = the release row's updated_at at active -> retained)
	// are always kept, whatever their age.
	KeepRetained int
	// MaxRetainedAge makes a retained release eligible once it has been
	// retained this long, even inside the newest KeepRetained.
	MaxRetainedAge time.Duration
	// RetryBackoff and MaxAttempts bound how often a release left in
	// 'removing' by a failed or skipped cleanup is handed out again.
	RetryBackoff time.Duration
	MaxAttempts  int
	// FailedCandidateAge is how long a failed or cancelled deployment must have
	// been failed, cancelled or superseded before its never-activated release (candidate or ready) is
	// eligible for removal (step 21B-1). It is a floor for post-mortem time.
	FailedCandidateAge time.Duration
}

// DefaultCleanupPolicy: 2 retained releases, 7 days, retry hourly, 5 tries,
// failed candidates after 24 hours.
func DefaultCleanupPolicy() CleanupPolicy {
	return CleanupPolicy{KeepRetained: 2, MaxRetainedAge: 7 * 24 * time.Hour, RetryBackoff: time.Hour, MaxAttempts: 5, FailedCandidateAge: 24 * time.Hour}
}

func (p CleanupPolicy) valid() bool {
	return p.KeepRetained >= 1 && p.MaxRetainedAge > 0 && p.RetryBackoff > 0 && p.MaxAttempts > 0 && p.FailedCandidateAge > 0
}

// SetCleanupPolicy replaces the retention policy (tests and wiring).
func (r *AgentLeaseRepository) SetCleanupPolicy(p CleanupPolicy) { r.cleanupPolicy = &p }

func (r *AgentLeaseRepository) policy() CleanupPolicy {
	if r.cleanupPolicy != nil {
		return *r.cleanupPolicy
	}
	return DefaultCleanupPolicy()
}

// CleanupClaim is one release the agent may now remove. Every field is
// server-derived: the agent only locates the container by these exact values.
type CleanupClaim struct {
	CleanupID         int64
	ReleaseID         string
	EnvironmentID     string
	RuntimeInstanceID string
	ImageDigest       string // hex, 64 characters; empty for a failed candidate whose image is kept
	// Kind is CleanupKindRetained or CleanupKindFailedCandidate.
	Kind string
	// RuntimeInstanceIDs (failed candidates only) lists the runtime instance id
	// of every attempt of the release's deployment, oldest first; each may have
	// left one stopped container. RuntimeInstanceID is empty for that kind.
	RuntimeInstanceIDs []string
}

func validCleanupResult(result string) bool {
	return result == CleanupResultRemoved || result == CleanupResultFailed || result == CleanupResultSkipped
}

func validCleanupErrorClass(class string) bool {
	switch class {
	case CleanupErrorEngine, CleanupErrorIdentity, CleanupErrorStop, CleanupErrorOther:
		return true
	}
	return false
}

// cleanupCandidate is one release a claim may hand out.
type cleanupCandidate struct {
	releaseID, environmentID string
	imageDigest              []byte
	// openCleanupID is set when the release already has an open cleanup that
	// is simply handed out again (the earlier answer or report was lost).
	openCleanupID int64
	// retry is a 'removing' release whose last cleanup failed and whose
	// backoff elapsed: it gets a new cleanup row, no state change.
	retry bool
	// failed marks a failed-candidate release (never activated), as opposed to
	// a retained one.
	failed bool
}

// cleanupCandidateEnvironments is the non-locking peek that only decides which
// environment slots to lock: environments of serverID with a retained release
// that could be eligible by count or age, or a removing release with a cleanup.
// Everything is re-evaluated under the locks.
const cleanupCandidateEnvironments = `
SELECT DISTINCT environment_id FROM (
  SELECT r.environment_id FROM releases r JOIN agent_jobs j ON j.deployment_id=r.deployment_id AND j.job_kind='deployment_execution'
   WHERE r.state='retained' AND j.server_id=$1
  UNION
  SELECT c.environment_id FROM release_cleanups c WHERE c.server_id=$1 AND c.completed_at IS NULL
  UNION
  SELECT c.environment_id FROM release_cleanups c JOIN releases r ON r.id=c.release_id WHERE c.server_id=$1 AND r.state='removing'
) e ORDER BY environment_id LIMIT $2`

// cleanupCandidateEnvironmentsWithFailed is the peek for a claim that opted in
// to failed candidates (step 21B-1): the 20E branches plus environments of
// serverID holding a never-activated release of a deployment that has been
// failed, cancelled or superseded for FailedCandidateAge, or a removing release with no
// activation. A separate statement, so a claim without the opt-in peeks
// exactly as before. Still only a hint: everything is re-evaluated under the
// locks.
const cleanupCandidateEnvironmentsWithFailed = `
SELECT DISTINCT environment_id FROM (
  SELECT r.environment_id FROM releases r JOIN agent_jobs j ON j.deployment_id=r.deployment_id AND j.job_kind='deployment_execution'
   WHERE r.state='retained' AND j.server_id=$1
  UNION
  SELECT c.environment_id FROM release_cleanups c WHERE c.server_id=$1 AND c.completed_at IS NULL
  UNION
  SELECT c.environment_id FROM release_cleanups c JOIN releases r ON r.id=c.release_id WHERE c.server_id=$1 AND r.state='removing'
  UNION
  SELECT r.environment_id FROM releases r
    JOIN deployments d ON d.id=r.deployment_id
    JOIN agent_jobs j ON j.deployment_id=r.deployment_id AND j.job_kind='deployment_execution'
   WHERE r.state IN ('candidate','ready') AND d.status IN ('failed','cancelled','superseded') AND j.server_id=$1
     AND EXISTS (SELECT 1 FROM agent_job_attempts a WHERE a.job_id=j.id)
     AND d.updated_at <= (SELECT clock_timestamp()) - make_interval(secs => $3)
) e ORDER BY environment_id LIMIT $2`

// releaseActivatedSQL is true for a release that has a succeeded activate
// operation by a non-rollback job, the release's winning activation (alias r).
// It splits the 'removing' releases of one server into retained-origin ones
// (container identity = the winning activation) and failed candidates (no
// activation: container identities = the attempts of its deployment).
const releaseActivatedSQL = `EXISTS (SELECT 1 FROM release_operations ro JOIN agent_jobs aj ON aj.id=ro.job_id AND aj.rollback_target_release_id IS NULL
	WHERE ro.release_id=r.id AND ro.kind='activate' AND ro.state='succeeded')`

// cleanupEligibleFailed lists the eligible failed candidates of one
// environment, oldest failure first (step 21B-1). Like cleanupEligibleRetained
// it must run under the environment slot lock and the route head FOR SHARE.
//
// A release is eligible when it never went live (state candidate or ready, no
// succeeded activate operation, not any route head's active release), its
// deployment failed, was cancelled or was superseded at least $3 seconds ago, that deployment's
// execution job ran on server $2 and no job of the deployment is queued, leased
// or retrying, no release operation of it is unresolved, and the environment
// slot is idle and not reconciling with an unflagged route head. The single
// clock_timestamp() comes from the one-row subselect.
const cleanupEligibleFailed = `
SELECT r.id, r.image_digest
  FROM releases r
  JOIN deployments d ON d.id=r.deployment_id
 CROSS JOIN (SELECT clock_timestamp() AS now) c
 WHERE r.environment_id=$1 AND r.state IN ('candidate','ready')
   AND d.status IN ('failed','cancelled','superseded')
   AND d.updated_at <= c.now - make_interval(secs => $3)
   AND EXISTS (SELECT 1 FROM agent_jobs j WHERE j.deployment_id=r.deployment_id AND j.job_kind='deployment_execution' AND j.server_id=$2)
   AND NOT EXISTS (SELECT 1 FROM agent_jobs aj WHERE aj.deployment_id=r.deployment_id AND aj.state IN ('queued','leased','retry_wait'))
   AND NOT EXISTS (SELECT 1 FROM release_operations ro WHERE ro.release_id=r.id AND ro.state NOT IN ('succeeded','failed'))
   AND NOT EXISTS (SELECT 1 FROM release_operations ro WHERE ro.release_id=r.id AND ro.kind='activate' AND ro.state='succeeded')
   AND NOT EXISTS (SELECT 1 FROM route_heads h WHERE h.active_release_id=r.id)
   AND EXISTS (SELECT 1 FROM environment_slots s WHERE s.environment_id=$1 AND s.current_attempt_id IS NULL AND NOT s.reconciliation_required)
   AND EXISTS (SELECT 1 FROM route_heads h WHERE h.environment_id=$1 AND NOT h.reconciliation_required)
 ORDER BY d.updated_at, r.id`

// cleanupEligibleRetained lists the eligible retained releases of one
// environment, oldest retention first. It must run under the environment slot
// lock (and the route head FOR SHARE). The rank is computed over ALL retained
// releases of the environment, so protected releases still occupy a "kept"
// position.
//
// A release is NEVER eligible when it is the target of a queued, leased or
// retrying job (an in-flight rollback), when a release operation of it is not
// terminal (an unresolved activation), or when its environment slot is
// occupied or reconciling or its route head is flagged.
const cleanupEligibleRetained = `
SELECT id, image_digest FROM (
  SELECT r.id, r.image_digest, r.updated_at,
         row_number() OVER (ORDER BY r.updated_at DESC, r.id DESC) AS rn
    FROM releases r
   WHERE r.environment_id=$1 AND r.state='retained'
) ranked
JOIN LATERAL (SELECT j.server_id FROM releases rr JOIN agent_jobs j ON j.deployment_id=rr.deployment_id AND j.job_kind='deployment_execution' WHERE rr.id=ranked.id) own ON own.server_id=$2
WHERE (ranked.rn > $3 OR ranked.updated_at <= clock_timestamp() - make_interval(secs => $4))
  AND ranked.image_digest IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM agent_jobs aj WHERE aj.rollback_target_release_id=ranked.id AND aj.state IN ('queued','leased','retry_wait'))
  AND NOT EXISTS (SELECT 1 FROM release_operations ro WHERE ro.release_id=ranked.id AND ro.state NOT IN ('succeeded','failed'))
  AND EXISTS (SELECT 1 FROM environment_slots s WHERE s.environment_id=$1 AND s.current_attempt_id IS NULL AND NOT s.reconciliation_required)
  AND EXISTS (SELECT 1 FROM route_heads h WHERE h.environment_id=$1 AND NOT h.reconciliation_required)
ORDER BY ranked.updated_at, ranked.id`

// ClaimCleanups hands the calling server at most limit releases to remove
// (step 20E, owner decisions B-D5/B-D6). The server is the credential's, never
// a request field.
//
// The fence is database state: in ONE READ COMMITTED transaction, in the ADR 0004
// amendment 2026-11-13 ("Retention cleanup") order -- server credential (SHARE,
// position 1), the candidate environments' slots (FOR UPDATE, position 3, by
// environment id ascending), their route heads (FOR SHARE, 6b), the chosen
// release rows (FOR UPDATE, 6b, by id ascending), then the append-only insert
// (8) -- it re-evaluates eligibility, moves each release retained -> removing
// and inserts its release_cleanups row. Because EnqueueRollback, Activate and
// the activation finalizers all take the same slot first, a rollback enqueue
// and a claim racing on one release serialize: either the rollback commits
// first (its queued job protects the release) or the claim does (the release is
// 'removing', which EnqueueRollback refuses).
//
// Already-open cleanups of this server are handed out again unchanged (their
// report or answer was lost; removal is idempotent). A release left 'removing'
// by a failed or skipped cleanup gets a new cleanup row once RetryBackoff has
// passed, at most MaxAttempts times in all.
func (r *AgentLeaseRepository) ClaimCleanups(ctx context.Context, serverID string, presentedDigest [32]byte, limit int) ([]CleanupClaim, error) {
	return r.ClaimCleanupsWithKinds(ctx, serverID, presentedDigest, limit, false)
}

// ClaimCleanupsWithKinds is ClaimCleanups; includeFailedCandidates additionally
// lets failed, cancelled and superseded deployments' never-activated releases be claimed
// (step 21B-1). Without it the behaviour is exactly the step 20E claim.
func (r *AgentLeaseRepository) ClaimCleanupsWithKinds(ctx context.Context, serverID string, presentedDigest [32]byte, limit int, includeFailedCandidates bool) ([]CleanupClaim, error) {
	if r == nil || r.pool == nil || !github.ValidOpaqueID(serverID) || limit <= 0 || limit > maxCleanupClaimLimit {
		return nil, ErrAgentLeaseInvalidInput
	}
	policy := r.policy()
	if !policy.valid() {
		return nil, ErrAgentLeaseInvalidInput
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("begin cleanup claim: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err = lockActiveServerCredential(ctx, tx, serverID, presentedDigest); err != nil {
		return nil, err
	}
	var environments []string
	if includeFailedCandidates {
		environments, err = queryStrings(ctx, tx, cleanupCandidateEnvironmentsWithFailed, serverID, limit, policy.FailedCandidateAge.Seconds())
	} else {
		environments, err = queryStrings(ctx, tx, cleanupCandidateEnvironments, serverID, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("list cleanup environments: %w", err)
	}
	// Slots ascending (position 3). Other transactions hold at most one slot
	// at a time, so ascending acquisition cannot cycle.
	for _, environmentID := range environments {
		if err = tx.QueryRow(ctx, `SELECT 1 FROM environment_slots WHERE environment_id=$1 FOR UPDATE`, environmentID).Scan(new(int)); err != nil {
			return nil, fmt.Errorf("lock cleanup environment slot: %w", err)
		}
	}
	if len(environments) > 0 {
		// Route heads (6b) FOR SHARE, ascending: a flagged head vetoes the environment.
		rows, qerr := tx.Query(ctx, `SELECT environment_id FROM route_heads WHERE environment_id = ANY($1) ORDER BY environment_id FOR SHARE`, environments)
		if qerr != nil {
			return nil, fmt.Errorf("lock cleanup route heads: %w", qerr)
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return nil, fmt.Errorf("lock cleanup route heads: %w", err)
		}
	}

	var candidates []cleanupCandidate
	for _, environmentID := range environments {
		found, ferr := cleanupCandidatesUnderLock(ctx, tx, serverID, environmentID, policy, includeFailedCandidates)
		if ferr != nil {
			return nil, ferr
		}
		candidates = append(candidates, found...)
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].releaseID < candidates[j].releaseID })
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	if len(candidates) == 0 {
		if err = tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("commit empty cleanup claim: %w", err)
		}
		return nil, nil
	}

	// Release rows (6b) FOR UPDATE, ascending; the state is re-checked.
	ids := make([]string, len(candidates))
	for i, c := range candidates {
		ids[i] = c.releaseID
	}
	states := map[string]string{}
	rows, err := tx.Query(ctx, `SELECT id, state FROM releases WHERE id = ANY($1) ORDER BY id FOR UPDATE`, ids)
	if err != nil {
		return nil, fmt.Errorf("lock cleanup releases: %w", err)
	}
	for rows.Next() {
		var id, state string
		if err = rows.Scan(&id, &state); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan cleanup release: %w", err)
		}
		states[id] = state
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("lock cleanup releases: %w", err)
	}

	claims := make([]CleanupClaim, 0, len(candidates))
	for _, c := range candidates {
		if c.failed {
			claim, ok, ferr := r.claimFailedCandidate(ctx, tx, serverID, c, states[c.releaseID])
			if ferr != nil {
				return nil, ferr
			}
			if ok {
				claims = append(claims, claim)
			}
			continue
		}
		want := "retained"
		if c.openCleanupID != 0 || c.retry {
			want = "removing"
		}
		if states[c.releaseID] != want {
			continue // changed between the eligibility read and the row lock: definite skip
		}
		attemptID, ok, werr := winningActivationAttempt(ctx, tx, c.releaseID)
		if werr != nil {
			return nil, werr
		}
		if !ok {
			continue // no known container identity: never hand out a guess
		}
		cleanupID := c.openCleanupID
		if cleanupID == 0 {
			if !c.retry {
				if _, err = tx.Exec(ctx, `UPDATE releases SET state='removing', updated_at=clock_timestamp() WHERE id=$1 AND state='retained'`, c.releaseID); err != nil {
					return nil, classifyActivationResultError(fmt.Errorf("release retained->removing: %w", err))
				}
			}
			if err = tx.QueryRow(ctx, `INSERT INTO release_cleanups(release_id, server_id, environment_id) VALUES ($1,$2,$3) RETURNING id`, c.releaseID, serverID, c.environmentID).Scan(&cleanupID); err != nil {
				return nil, classifyActivationResultError(fmt.Errorf("insert release cleanup: %w", err))
			}
		}
		claims = append(claims, CleanupClaim{
			CleanupID: cleanupID, ReleaseID: c.releaseID, EnvironmentID: c.environmentID,
			RuntimeInstanceID: dockerid.AttemptRuntimeInstanceID(attemptID), ImageDigest: hex.EncodeToString(c.imageDigest),
			Kind: CleanupKindRetained,
		})
	}
	if err = r.inject("cleanup_claim"); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, classifyActivationResultError(fmt.Errorf("commit cleanup claim: %w", err))
	}
	return claims, nil
}

// claimFailedCandidate turns one failed-candidate candidate into a claim, in
// the claim transaction with its release row locked (state is the locked
// state). ok=false is a definite skip: the release changed after the
// eligibility read, or no container identity is known.
//
// A fresh candidate moves candidate|ready -> removing (the legal edges of
// migration 000018; 'failed' is untouched and never reached) and gets its
// release_cleanups row exactly as a retained release does; an open cleanup is
// handed out unchanged and a retry gets a new row without a state change.
// The container identities are every attempt of the deployment's execution
// job(s): each may have left one stopped container, named by the same
// AttemptRuntimeInstanceID derivation the executor uses.
//
// Image (owner decision D2): a release without an image digest hands none (the
// agent keeps the image). With a digest, it is handed only when no other release
// of this server that is not yet 'removed' carries the same digest, whatever its
// state; otherwise the image is kept.
func (r *AgentLeaseRepository) claimFailedCandidate(ctx context.Context, tx pgx.Tx, serverID string, c cleanupCandidate, state string) (CleanupClaim, bool, error) {
	fresh := c.openCleanupID == 0 && !c.retry
	if fresh {
		if state != "candidate" && state != "ready" {
			return CleanupClaim{}, false, nil
		}
	} else if state != "removing" {
		return CleanupClaim{}, false, nil
	}
	// Removal is irreversible: for every item, fresh, open or retry, re-check
	// under the locked release row that it never went live. Stricter than the
	// retained/failed split (any job's succeeded activate operation counts).
	var live bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM release_operations ro WHERE ro.release_id=$1 AND ro.kind='activate' AND ro.state='succeeded')
		OR EXISTS (SELECT 1 FROM route_heads h WHERE h.active_release_id=$1)`, c.releaseID).Scan(&live); err != nil {
		return CleanupClaim{}, false, fmt.Errorf("check failed candidate never activated: %w", err)
	}
	if live {
		return CleanupClaim{}, false, nil
	}
	rows, err := tx.Query(ctx, `SELECT a.id FROM agent_job_attempts a JOIN agent_jobs j ON j.id=a.job_id
		JOIN releases r ON r.deployment_id=j.deployment_id
		WHERE r.id=$1 AND j.job_kind='deployment_execution' AND j.server_id=$2 ORDER BY a.id`, c.releaseID, serverID)
	if err != nil {
		return CleanupClaim{}, false, fmt.Errorf("read failed candidate attempts: %w", err)
	}
	var instances []string
	for rows.Next() {
		var attemptID int64
		if err = rows.Scan(&attemptID); err != nil {
			rows.Close()
			return CleanupClaim{}, false, fmt.Errorf("scan failed candidate attempt: %w", err)
		}
		instances = append(instances, dockerid.AttemptRuntimeInstanceID(attemptID))
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return CleanupClaim{}, false, fmt.Errorf("read failed candidate attempts: %w", err)
	}
	if len(instances) == 0 {
		return CleanupClaim{}, false, nil // no known container identity: never hand out a guess
	}

	imageDigest := ""
	if len(c.imageDigest) > 0 {
		var shared bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM releases o JOIN deployments od ON od.id=o.deployment_id
			WHERE o.id<>$1 AND o.image_digest=$2 AND o.state<>'removed' AND od.server_id=$3)`, c.releaseID, c.imageDigest, serverID).Scan(&shared); err != nil {
			return CleanupClaim{}, false, fmt.Errorf("check failed candidate image sharing: %w", err)
		}
		if !shared {
			imageDigest = hex.EncodeToString(c.imageDigest)
		}
	}

	cleanupID := c.openCleanupID
	if cleanupID == 0 {
		if fresh {
			tag, uerr := tx.Exec(ctx, `UPDATE releases SET state='removing', updated_at=clock_timestamp() WHERE id=$1 AND state IN ('candidate','ready')`, c.releaseID)
			if uerr != nil {
				return CleanupClaim{}, false, classifyActivationResultError(fmt.Errorf("release candidate->removing: %w", uerr))
			}
			if tag.RowsAffected() != 1 {
				return CleanupClaim{}, false, nil
			}
		}
		if err = tx.QueryRow(ctx, `INSERT INTO release_cleanups(release_id, server_id, environment_id) VALUES ($1,$2,$3) RETURNING id`, c.releaseID, serverID, c.environmentID).Scan(&cleanupID); err != nil {
			return CleanupClaim{}, false, classifyActivationResultError(fmt.Errorf("insert failed candidate cleanup: %w", err))
		}
	}
	return CleanupClaim{
		CleanupID: cleanupID, ReleaseID: c.releaseID, EnvironmentID: c.environmentID,
		ImageDigest: imageDigest, Kind: CleanupKindFailedCandidate, RuntimeInstanceIDs: instances,
	}, true, nil
}

// cleanupCandidatesUnderLock runs with the environment slot and route head
// locked. Order of preference inside one environment: open cleanups (redeliver),
// failed-removal retries, then fresh retained releases.
func cleanupCandidatesUnderLock(ctx context.Context, tx pgx.Tx, serverID, environmentID string, policy CleanupPolicy, includeFailed bool) ([]cleanupCandidate, error) {
	var out []cleanupCandidate
	// Slot and route head veto everything in the environment that is not an
	// already-open cleanup (which was claimed while the environment was quiet
	// and is idempotent to redo).
	var quiet bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM environment_slots s WHERE s.environment_id=$1 AND s.current_attempt_id IS NULL AND NOT s.reconciliation_required)
		AND EXISTS (SELECT 1 FROM route_heads h WHERE h.environment_id=$1 AND NOT h.reconciliation_required)`, environmentID).Scan(&quiet); err != nil {
		return nil, fmt.Errorf("read cleanup environment: %w", err)
	}

	rows, err := tx.Query(ctx, `SELECT c.id, c.release_id, r.image_digest FROM release_cleanups c JOIN releases r ON r.id=c.release_id
		WHERE c.server_id=$1 AND c.environment_id=$2 AND c.completed_at IS NULL AND r.state='removing' AND r.image_digest IS NOT NULL
		  AND `+releaseActivatedSQL+` ORDER BY c.id`, serverID, environmentID)
	if err != nil {
		return nil, fmt.Errorf("read open cleanups: %w", err)
	}
	for rows.Next() {
		c := cleanupCandidate{environmentID: environmentID}
		if err = rows.Scan(&c.openCleanupID, &c.releaseID, &c.imageDigest); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan open cleanup: %w", err)
		}
		out = append(out, c)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("read open cleanups: %w", err)
	}
	if includeFailed {
		// Open cleanups of failed candidates (no activation): redelivered
		// unchanged under the same rule as the retained ones above.
		rows, err = tx.Query(ctx, `SELECT c.id, c.release_id, r.image_digest FROM release_cleanups c JOIN releases r ON r.id=c.release_id
			WHERE c.server_id=$1 AND c.environment_id=$2 AND c.completed_at IS NULL AND r.state='removing'
			  AND NOT `+releaseActivatedSQL+` ORDER BY c.id`, serverID, environmentID)
		if err != nil {
			return nil, fmt.Errorf("read open failed-candidate cleanups: %w", err)
		}
		for rows.Next() {
			c := cleanupCandidate{environmentID: environmentID, failed: true}
			if err = rows.Scan(&c.openCleanupID, &c.releaseID, &c.imageDigest); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan open failed-candidate cleanup: %w", err)
			}
			out = append(out, c)
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return nil, fmt.Errorf("read open failed-candidate cleanups: %w", err)
		}
	}
	if !quiet {
		return out, nil
	}

	// Failed or skipped last cleanup, backoff elapsed, attempts left.
	rows, err = tx.Query(ctx, `SELECT r.id, r.image_digest FROM releases r
		WHERE r.environment_id=$1 AND r.state='removing' AND r.image_digest IS NOT NULL AND `+releaseActivatedSQL+`
		  AND NOT EXISTS (SELECT 1 FROM release_cleanups o WHERE o.release_id=r.id AND o.completed_at IS NULL)
		  AND EXISTS (SELECT 1 FROM release_cleanups l WHERE l.release_id=r.id AND l.server_id=$2 AND l.id=(SELECT max(id) FROM release_cleanups WHERE release_id=r.id)
		              AND l.result IN ('failed','skipped') AND l.completed_at <= clock_timestamp() - make_interval(secs => $3))
		  AND (SELECT count(*) FROM release_cleanups WHERE release_id=r.id) < $4
		ORDER BY r.id`, environmentID, serverID, policy.RetryBackoff.Seconds(), policy.MaxAttempts)
	if err != nil {
		return nil, fmt.Errorf("read cleanup retries: %w", err)
	}
	for rows.Next() {
		c := cleanupCandidate{environmentID: environmentID, retry: true}
		if err = rows.Scan(&c.releaseID, &c.imageDigest); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan cleanup retry: %w", err)
		}
		out = append(out, c)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("read cleanup retries: %w", err)
	}

	rows, err = tx.Query(ctx, cleanupEligibleRetained, environmentID, serverID, policy.KeepRetained, policy.MaxRetainedAge.Seconds())
	if err != nil {
		return nil, fmt.Errorf("read eligible retained releases: %w", err)
	}
	for rows.Next() {
		c := cleanupCandidate{environmentID: environmentID}
		if err = rows.Scan(&c.releaseID, &c.imageDigest); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan eligible retained release: %w", err)
		}
		out = append(out, c)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("read eligible retained releases: %w", err)
	}
	if !includeFailed {
		return out, nil
	}

	// Failed candidates (step 21B-1): a removing release with no activation
	// whose last cleanup failed or was skipped, backoff elapsed, attempts left;
	// then fresh eligible ones.
	rows, err = tx.Query(ctx, `SELECT r.id, r.image_digest FROM releases r
		WHERE r.environment_id=$1 AND r.state='removing' AND NOT `+releaseActivatedSQL+`
		  AND NOT EXISTS (SELECT 1 FROM release_cleanups o WHERE o.release_id=r.id AND o.completed_at IS NULL)
		  AND EXISTS (SELECT 1 FROM release_cleanups l WHERE l.release_id=r.id AND l.server_id=$2 AND l.id=(SELECT max(id) FROM release_cleanups WHERE release_id=r.id)
		              AND l.result IN ('failed','skipped') AND l.completed_at <= (SELECT clock_timestamp()) - make_interval(secs => $3))
		  AND (SELECT count(*) FROM release_cleanups WHERE release_id=r.id) < $4
		ORDER BY r.id`, environmentID, serverID, policy.RetryBackoff.Seconds(), policy.MaxAttempts)
	if err != nil {
		return nil, fmt.Errorf("read failed-candidate cleanup retries: %w", err)
	}
	for rows.Next() {
		c := cleanupCandidate{environmentID: environmentID, retry: true, failed: true}
		if err = rows.Scan(&c.releaseID, &c.imageDigest); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan failed-candidate cleanup retry: %w", err)
		}
		out = append(out, c)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("read failed-candidate cleanup retries: %w", err)
	}

	rows, err = tx.Query(ctx, cleanupEligibleFailed, environmentID, serverID, policy.FailedCandidateAge.Seconds())
	if err != nil {
		return nil, fmt.Errorf("read eligible failed candidates: %w", err)
	}
	for rows.Next() {
		c := cleanupCandidate{environmentID: environmentID, failed: true}
		if err = rows.Scan(&c.releaseID, &c.imageDigest); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan eligible failed candidate: %w", err)
		}
		out = append(out, c)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("read eligible failed candidates: %w", err)
	}
	return out, nil
}

func queryStrings(ctx context.Context, tx pgx.Tx, sql string, args ...any) ([]string, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err = rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// RecordCleanupResult records what the agent's removal of one claimed release
// ended in (step 20E). Credential-authenticated only; authority is DERIVED: the
// cleanup row must belong to the credential's server (any other or unknown id
// is ErrServerAuthorizationDenied, never distinguished).
//
// Lock order: server credential (SHARE, 1) -> the cleanup's environment slot
// (FOR UPDATE, 3) -> the release row (FOR UPDATE, 6b) -> the cleanup row
// (FOR UPDATE, 8's class, locked last). The slot lock serializes it with a
// claim that might retry the same release.
//
// Idempotent by cleanup id: a replay of the identical report is a no-op
// success; a different report for a completed cleanup is
// ErrAgentLeaseIntegrityConflict (first report wins; rows are write-once).
// Only result 'removed' moves the release removing -> removed; failed and
// skipped leave it 'removing', operator visible, retried by a later claim after
// the backoff. The server never marks a release removed on its own.
func (r *AgentLeaseRepository) RecordCleanupResult(ctx context.Context, serverID string, presentedDigest [32]byte, cleanupID int64, result, errorClass string, imageKept bool, summary string) error {
	if r == nil || r.pool == nil || !github.ValidOpaqueID(serverID) || cleanupID <= 0 || !validCleanupResult(result) ||
		(result == CleanupResultRemoved) != (errorClass == "") || (errorClass != "" && !validCleanupErrorClass(errorClass)) ||
		(imageKept && result != CleanupResultRemoved) || !validDrainSummary(summary) {
		return ErrAgentLeaseInvalidInput
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin cleanup result: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err = lockActiveServerCredential(ctx, tx, serverID, presentedDigest); err != nil {
		return err
	}
	var cleanupServerID, environmentID, releaseID string
	if err = tx.QueryRow(ctx, `SELECT server_id, environment_id, release_id FROM release_cleanups WHERE id=$1`, cleanupID).Scan(&cleanupServerID, &environmentID, &releaseID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrServerAuthorizationDenied
		}
		return fmt.Errorf("read cleanup: %w", err)
	}
	if cleanupServerID != serverID {
		return ErrServerAuthorizationDenied
	}
	if err = tx.QueryRow(ctx, `SELECT 1 FROM environment_slots WHERE environment_id=$1 FOR UPDATE`, environmentID).Scan(new(int)); err != nil {
		return fmt.Errorf("lock cleanup result slot: %w", err)
	}
	var releaseState string
	if err = tx.QueryRow(ctx, `SELECT state FROM releases WHERE id=$1 FOR UPDATE`, releaseID).Scan(&releaseState); err != nil {
		return fmt.Errorf("lock cleanup result release: %w", err)
	}
	var storedResult, storedClass, storedSummary *string
	var storedKept bool
	if err = tx.QueryRow(ctx, `SELECT result, error_class, outcome_summary, image_kept FROM release_cleanups WHERE id=$1 FOR UPDATE`, cleanupID).Scan(&storedResult, &storedClass, &storedSummary, &storedKept); err != nil {
		return fmt.Errorf("lock cleanup row: %w", err)
	}
	if storedResult != nil {
		same := *storedResult == result && derefString(storedClass) == errorClass && derefString(storedSummary) == summary && storedKept == imageKept
		if !same {
			return ErrAgentLeaseIntegrityConflict
		}
		if err = tx.Commit(ctx); err != nil {
			return classifyActivationResultError(fmt.Errorf("commit cleanup result replay: %w", err))
		}
		return nil
	}
	if releaseState != "removing" {
		return ErrAgentLeaseCorruptState
	}
	var class any
	if errorClass != "" {
		class = errorClass
	}
	if _, err = tx.Exec(ctx, `UPDATE release_cleanups SET result=$2, error_class=$3, image_kept=$4, outcome_summary=$5, completed_at=clock_timestamp() WHERE id=$1 AND result IS NULL`,
		cleanupID, result, class, imageKept, summary); err != nil {
		return classifyActivationResultError(fmt.Errorf("record cleanup result: %w", err))
	}
	if result == CleanupResultRemoved {
		if _, err = tx.Exec(ctx, `UPDATE releases SET state='removed', updated_at=clock_timestamp() WHERE id=$1 AND state='removing'`, releaseID); err != nil {
			return classifyActivationResultError(fmt.Errorf("release removing->removed: %w", err))
		}
	}
	if err = r.inject("cleanup_result"); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return classifyActivationResultError(fmt.Errorf("commit cleanup result: %w", err))
	}
	return nil
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ReleaseCleanup is the latest cleanup of a release, for the web detail page.
type ReleaseCleanup struct {
	ID          int64
	ClaimedAt   time.Time
	Result      string // empty while open
	ErrorClass  string
	ImageKept   bool
	Summary     string
	CompletedAt *time.Time
	Attempts    int
}

// readLatestCleanup returns the newest cleanup of releaseID (nil when the
// release was never claimed) and how many cleanups it has had. The caller has
// already established that the release belongs to the principal.
func readLatestCleanup(ctx context.Context, tx pgx.Tx, releaseID string) (*ReleaseCleanup, error) {
	var c ReleaseCleanup
	var result, class *string
	err := tx.QueryRow(ctx, `SELECT id, claimed_at, result, error_class, image_kept, outcome_summary, completed_at, (SELECT count(*) FROM release_cleanups WHERE release_id=$1)
FROM release_cleanups WHERE release_id=$1 ORDER BY id DESC LIMIT 1`, releaseID).
		Scan(&c.ID, &c.ClaimedAt, &result, &class, &c.ImageKept, &c.Summary, &c.CompletedAt, &c.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read release cleanup: %w", err)
	}
	c.Result, c.ErrorClass = derefString(result), derefString(class)
	return &c, nil
}
