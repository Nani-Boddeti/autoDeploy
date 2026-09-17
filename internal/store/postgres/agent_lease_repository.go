package postgres

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"autodeploy/internal/deployment"
	"autodeploy/internal/github"
	"autodeploy/internal/queue"
	"autodeploy/internal/server"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrAgentLeaseStale reports a renewal (or claim-adjacent) request whose
// fence, renewal sequence, or lease/authority deadline no longer matches
// durable state -- an expected at-least-once-delivery race (ADR 0004
// renewal/expiry), not an authorization failure. It is kept distinct from
// ErrServerAuthorizationDenied only so a caller can tell "retry later" from
// "stop"; neither error exposes which specific check failed.
var ErrAgentLeaseStale = errors.New("agent lease stale")

const (
	// leaseDuration is the ADR 0004 V1 default lease/renewal-extension length.
	leaseDuration = 90 * time.Second
	// claimDiscoveryPerJob and claimDiscoveryMax bound the non-locking
	// candidate read (ADR 0004 "bounded" candidate discovery): most
	// candidates are expected to be claimable, but slot contention and
	// stale/occupied environments can require skipping several per claimed
	// job, so discovery reads a small multiple of the caller's limit.
	claimDiscoveryPerJob = 5
	claimDiscoveryMax    = 200
)

// AgentLeaseRepository implements the claim and renew half of the ADR 0004
// lease lifecycle over the agent_jobs/environment_slots/agent_job_attempts
// schema added by migration 000013. Complete, cancel, expire, and reconcile
// are a later slice (Step 4): this repository never moves a job or attempt
// into a terminal state.
type AgentLeaseRepository struct {
	pool        *pgxpool.Pool
	deployments *DeploymentRepository
	failAfter   func(string) error // test-only deterministic rollback seam
}

func NewAgentLeaseRepository(pool *pgxpool.Pool, deployments *DeploymentRepository) *AgentLeaseRepository {
	if deployments == nil {
		deployments = NewDeploymentRepository(pool)
	}
	return &AgentLeaseRepository{pool: pool, deployments: deployments}
}

func (r *AgentLeaseRepository) inject(stage string) error {
	if r.failAfter == nil {
		return nil
	}
	return r.failAfter(stage)
}

// lockActiveServerCredential re-validates the presented credential digest
// against the server's currently active credential inside the caller's own
// transaction, mirroring ServerRepository.Authenticate's constant-time
// dummy-digest comparison exactly. Decision 3 requires this even though the
// HTTP layer already authenticated the caller once: a credential or server
// revoked in the gap between that authentication and this claim/renew
// transaction must still be caught here. It returns the server's owner ID on
// success and the package's one generic denial otherwise.
func lockActiveServerCredential(ctx context.Context, tx pgx.Tx, serverID string, presentedDigest [32]byte) (string, error) {
	var ownerID, status string
	foundServer := true
	if err := tx.QueryRow(ctx, `SELECT owner_id,status FROM servers WHERE id=$1 FOR SHARE`, serverID).Scan(&ownerID, &status); err != nil || status != "active" {
		foundServer = false
	}
	var storedDigest []byte
	foundCredential := false
	if foundServer {
		if err := tx.QueryRow(ctx, `SELECT digest FROM server_credentials WHERE server_id=$1 AND status='active' FOR SHARE`, serverID).Scan(&storedDigest); err == nil {
			foundCredential = true
		}
	}
	if !matchServerCredential(foundServer, foundCredential, storedDigest, presentedDigest) {
		return "", ErrServerAuthorizationDenied
	}
	return ownerID, nil
}

// authenticateServerCandidate performs the same constant-time dummy-digest
// check as lockActiveServerCredential, but as a genuinely non-locking read
// against the pool (no FOR SHARE, no transaction) so Claim can reject an
// unknown server or wrong credential before it does any candidate discovery
// or environment_slots writes. The claim transaction still re-validates
// under FOR SHARE via lockActiveServerCredential per Decision 3: a
// revocation between this check and that transaction must still be caught
// there.
func (r *AgentLeaseRepository) authenticateServerCandidate(ctx context.Context, serverID string, presentedDigest [32]byte) error {
	var status string
	foundServer := true
	if err := r.pool.QueryRow(ctx, `SELECT status FROM servers WHERE id=$1`, serverID).Scan(&status); err != nil || status != "active" {
		foundServer = false
	}
	var storedDigest []byte
	foundCredential := false
	if foundServer {
		if err := r.pool.QueryRow(ctx, `SELECT digest FROM server_credentials WHERE server_id=$1 AND status='active'`, serverID).Scan(&storedDigest); err == nil {
			foundCredential = true
		}
	}
	if !matchServerCredential(foundServer, foundCredential, storedDigest, presentedDigest) {
		return ErrServerAuthorizationDenied
	}
	return nil
}

// matchServerCredential is the shared constant-time comparison behind both
// authentication paths above: it always compares against a fixed dummy
// digest when no real credential was found, so an unknown server and a
// known server with a wrong digest take the same code path.
func matchServerCredential(foundServer, foundCredential bool, storedDigest []byte, presentedDigest [32]byte) bool {
	var stored [32]byte
	if foundCredential {
		copy(stored[:], storedDigest)
	} else {
		stored = dummyCredentialDigest
	}
	return foundServer && foundCredential && server.ConstantTimeEqualDigest(stored, presentedDigest)
}

// agentLeaseCandidate is one non-locking discovery result: a runnable job
// and the environment it targets.
type agentLeaseCandidate struct {
	jobID         int64
	environmentID string
}

// Claim atomically leases up to limit runnable jobs scoped to serverID,
// following the ADR 0004 atomic claim boundary and global lock hierarchy:
// authorization (SHARE) -> environment slot (UPDATE) -> job
// (UPDATE ... SKIP LOCKED) -> attempt (INSERT) -> deployment head (UPDATE,
// first attempt only) -> frozen secret versions (INSERT, empty in this
// slice) -> events (INSERT). V1 claims at most one job per environment;
// limit caps the total number of leases returned. Candidate discovery is a
// non-locking ordered read outside any transaction; every predicate is
// rechecked after the relevant row is locked inside the claim transaction,
// and a candidate that no longer qualifies is skipped rather than failing
// the whole call.
func (r *AgentLeaseRepository) Claim(ctx context.Context, serverID string, presentedDigest [32]byte, limit int) ([]queue.ClaimedLease, error) {
	if limit <= 0 {
		// No clamp to 1: a non-positive limit asks for nothing, and asking
		// for nothing must not touch the database at all -- not even to
		// authenticate.
		return nil, nil
	}
	if r == nil || r.pool == nil || !github.ValidOpaqueID(serverID) {
		return nil, ErrServerAuthorizationDenied
	}
	if limit > 50 {
		limit = 50
	}

	// Cheap non-locking authentication before any candidate discovery or
	// environment_slots write: an unknown server or wrong credential must
	// never cause Claim to read job rows or create slot rows, even though
	// the claim transaction below re-authenticates under FOR SHARE anyway.
	if err := r.authenticateServerCandidate(ctx, serverID, presentedDigest); err != nil {
		return nil, err
	}

	discoveryLimit := limit * claimDiscoveryPerJob
	if discoveryLimit > claimDiscoveryMax {
		discoveryLimit = claimDiscoveryMax
	}
	var discoveryNow time.Time
	if err := r.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&discoveryNow); err != nil {
		return nil, fmt.Errorf("agent lease claim discovery time: %w", err)
	}
	rows, err := r.pool.Query(ctx, `SELECT id, environment_id FROM agent_jobs WHERE server_id=$1 AND state IN ('queued','retry_wait') AND available_at<=$2 ORDER BY priority DESC, available_at, created_at, id LIMIT $3`, serverID, discoveryNow, discoveryLimit)
	if err != nil {
		return nil, fmt.Errorf("discover runnable agent jobs: %w", err)
	}
	var candidates []agentLeaseCandidate
	for rows.Next() {
		var c agentLeaseCandidate
		if err = rows.Scan(&c.jobID, &c.environmentID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan runnable agent job: %w", err)
		}
		candidates = append(candidates, c)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("discover runnable agent jobs: %w", err)
	}
	rows.Close()

	// Pre-create every candidate environment's slot row up front, in one
	// statement, in sorted order, strictly before the claim transaction
	// begins -- never inside it. Two concurrent claims (any servers) whose
	// candidate lists both name the same two brand-new environments but in
	// opposite orders would otherwise each hold one uncommitted inserted
	// slot row and wait on the other's, aborting with a Postgres deadlock;
	// a single sorted multi-row insert makes both claims acquire these rows
	// in the same order and removes that race entirely. The claim
	// transaction below only ever locks an existing row with
	// FOR UPDATE SKIP LOCKED; it never inserts one.
	if envs := sortedDistinctEnvironments(candidates); len(envs) > 0 {
		if _, err = r.pool.Exec(ctx, `INSERT INTO environment_slots(environment_id, updated_at) SELECT unnest($1::text[]), clock_timestamp() ON CONFLICT DO NOTHING`, envs); err != nil {
			return nil, fmt.Errorf("ensure environment slots: %w", err)
		}
	}

	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("begin agent lease claim: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err = lockActiveServerCredential(ctx, tx, serverID, presentedDigest); err != nil {
		return nil, err
	}

	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return nil, fmt.Errorf("agent lease claim canonical time: %w", err)
	}
	leaseExpiresAt := now.Add(leaseDuration)

	// Inside this transaction, tryClaim locks each candidate's environment
	// slot in candidate priority order (the order returned by discovery
	// above), not environment_id ascending as ADR 0004's global lock
	// hierarchy lists environment-slot rows for other cross-transaction
	// operations. That is safe here specifically because every slot lock
	// below is FOR UPDATE SKIP LOCKED: this transaction never waits for a
	// slot another transaction already holds, it just skips that candidate.
	// A lock acquisition that can never block cannot participate in a wait
	// cycle, so no ordering discipline is needed to avoid deadlock at this
	// step (unlike the slot pre-create above, which does use blocking
	// inserts and therefore does need the sorted order).
	var claimed []queue.ClaimedLease
	for _, c := range candidates {
		if len(claimed) >= limit {
			break
		}
		lease, ok, err := r.tryClaim(ctx, tx, serverID, c.jobID, c.environmentID, now, leaseExpiresAt)
		if err != nil {
			return nil, err
		}
		if ok {
			claimed = append(claimed, lease)
		}
	}

	if err = tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit agent lease claim: %w", err)
	}
	return claimed, nil
}

// sortedDistinctEnvironments returns the distinct environment IDs named by
// candidates in sorted order, so the pre-create step above always acquires
// environment_slots insert locks in the same deterministic order regardless
// of any caller's candidate ordering.
func sortedDistinctEnvironments(candidates []agentLeaseCandidate) []string {
	seen := make(map[string]struct{}, len(candidates))
	envs := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if _, ok := seen[c.environmentID]; ok {
			continue
		}
		seen[c.environmentID] = struct{}{}
		envs = append(envs, c.environmentID)
	}
	sort.Strings(envs)
	return envs
}

// tryClaim attempts to claim one candidate job inside tx. ok=false with a
// nil error means the candidate turned out not to be claimable once locked
// (an occupied or reconciling slot, a job another claimer already holds, or
// a job whose revalidated state no longer matches the discovery snapshot);
// Claim moves on to its next candidate rather than failing the whole call. A
// non-nil error is reserved for a genuine database failure.
func (r *AgentLeaseRepository) tryClaim(ctx context.Context, tx pgx.Tx, serverID string, jobID int64, environmentID string, now, leaseExpiresAt time.Time) (queue.ClaimedLease, bool, error) {
	// The slot row itself is never inserted here -- Claim pre-creates every
	// discovered candidate's environment_slots row, in sorted order, as its
	// own committed statement before this transaction even begins (see the
	// deadlock note on Claim). This transaction only locks an existing row.
	//
	// SKIP LOCKED (rather than a blocking FOR UPDATE) matches the same
	// job-locking choice below: two claimers racing different candidates
	// that happen to share an environment must never block on each other,
	// only skip. Losing the race and finding no unlocked row is
	// indistinguishable from finding the slot already occupied, and from
	// the (should-not-happen, since it was just pre-created) case of the
	// row genuinely not existing.
	var slotFence int64
	var currentAttemptID *int64
	var reconciling bool
	err := tx.QueryRow(ctx, `SELECT fence, current_attempt_id, reconciliation_required FROM environment_slots WHERE environment_id=$1 FOR UPDATE SKIP LOCKED`, environmentID).Scan(&slotFence, &currentAttemptID, &reconciling)
	if errors.Is(err, pgx.ErrNoRows) {
		return queue.ClaimedLease{}, false, nil
	}
	if err != nil {
		return queue.ClaimedLease{}, false, fmt.Errorf("lock environment slot: %w", err)
	}
	if currentAttemptID != nil || reconciling {
		return queue.ClaimedLease{}, false, nil
	}

	var ownerID, projectID, deploymentID, ref, commitSHA, jobState, jobEnvironmentID, jobServerID string
	var repositoryID int64
	var jobAvailableAt time.Time
	var cancelRequestedAt *time.Time
	err = tx.QueryRow(ctx, `SELECT owner_id, project_id, environment_id, server_id, deployment_id, repository_id, full_ref, commit_sha, state, available_at, cancel_requested_at FROM agent_jobs WHERE id=$1 FOR UPDATE SKIP LOCKED`, jobID).
		Scan(&ownerID, &projectID, &jobEnvironmentID, &jobServerID, &deploymentID, &repositoryID, &ref, &commitSHA, &jobState, &jobAvailableAt, &cancelRequestedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return queue.ClaimedLease{}, false, nil
	}
	if err != nil {
		return queue.ClaimedLease{}, false, fmt.Errorf("lock agent job: %w", err)
	}
	// Re-validate the locked row against the discovery candidate rather
	// than trusting it: environment_id and server_id are immutable per the
	// job update trigger, so this can only fail if tryClaim were ever
	// called with a mismatched candidate, but the check costs nothing and
	// closes that off explicitly rather than by invariant alone.
	if jobEnvironmentID != environmentID || jobServerID != serverID {
		return queue.ClaimedLease{}, false, nil
	}
	if (jobState != "queued" && jobState != "retry_wait") || jobAvailableAt.After(now) || cancelRequestedAt != nil {
		return queue.ClaimedLease{}, false, nil
	}

	// Claim ordering per migration 000013's attempt-insert trigger: bump the
	// slot fence, then lease the job with attempt_count+1, then insert the
	// attempt carrying that same fence and the job's now-current
	// attempt_count -- only then can the slot's current_attempt_id (the
	// circular reference the migration defers) be set.
	var newFence int64
	if err = tx.QueryRow(ctx, `UPDATE environment_slots SET fence=fence+1, updated_at=$2 WHERE environment_id=$1 RETURNING fence`, environmentID, now).Scan(&newFence); err != nil {
		return queue.ClaimedLease{}, false, fmt.Errorf("advance environment slot fence: %w", err)
	}
	if err = r.inject("slot_fence"); err != nil {
		return queue.ClaimedLease{}, false, err
	}

	var attemptNumber int
	err = tx.QueryRow(ctx, `UPDATE agent_jobs SET state='leased', attempt_count=attempt_count+1, updated_at=$2 WHERE id=$1 AND state=$3 RETURNING attempt_count`, jobID, now, jobState).Scan(&attemptNumber)
	if errors.Is(err, pgx.ErrNoRows) || isCheckViolation(err) {
		// The five-attempt ceiling (agent_jobs.attempt_count CHECK) is
		// enforced by Step 4's dead-lettering before a job can reach it;
		// tripping it here means the candidate is not actually claimable.
		return queue.ClaimedLease{}, false, nil
	}
	if err != nil {
		return queue.ClaimedLease{}, false, fmt.Errorf("lease agent job: %w", err)
	}
	if err = r.inject("job_leased"); err != nil {
		return queue.ClaimedLease{}, false, err
	}

	var attemptID int64
	err = tx.QueryRow(ctx, `INSERT INTO agent_job_attempts(job_id, attempt_number, server_id, environment_id, fence, state, lease_expires_at, authority_expires_at) VALUES ($1,$2,$3,$4,$5,'leased',$6,$6) RETURNING id`,
		jobID, attemptNumber, serverID, environmentID, newFence, leaseExpiresAt).Scan(&attemptID)
	if err != nil {
		return queue.ClaimedLease{}, false, fmt.Errorf("insert agent job attempt: %w", err)
	}
	if err = r.inject("attempt_insert"); err != nil {
		return queue.ClaimedLease{}, false, err
	}

	if _, err = tx.Exec(ctx, `UPDATE environment_slots SET current_attempt_id=$2, updated_at=$3 WHERE environment_id=$1`, environmentID, attemptID, now); err != nil {
		return queue.ClaimedLease{}, false, fmt.Errorf("occupy environment slot: %w", err)
	}
	if err = r.inject("slot_occupy"); err != nil {
		return queue.ClaimedLease{}, false, err
	}

	if attemptNumber == 1 {
		if err = r.transitionDeploymentAssigned(ctx, tx, deploymentID, now); err != nil {
			return queue.ClaimedLease{}, false, err
		}
		if err = r.inject("deployment"); err != nil {
			return queue.ClaimedLease{}, false, err
		}
	}

	if err = freezeSecretVersions(ctx, tx, attemptID, nil); err != nil {
		return queue.ClaimedLease{}, false, err
	}
	if err = r.inject("secret_freeze"); err != nil {
		return queue.ClaimedLease{}, false, err
	}

	if err = appendAgentJobEvent(ctx, tx, jobID, &attemptID, "claimed"); err != nil {
		return queue.ClaimedLease{}, false, err
	}
	if err = r.inject("job_event"); err != nil {
		return queue.ClaimedLease{}, false, err
	}

	return queue.ClaimedLease{
		JobID: jobID, AttemptID: attemptID, AttemptNumber: attemptNumber, Fence: newFence,
		OwnerID: ownerID, ProjectID: projectID, EnvironmentID: environmentID, DeploymentID: deploymentID,
		RepositoryID: repositoryID, Ref: ref, CommitSHA: commitSHA,
		LeaseExpiresAt: leaseExpiresAt, AuthorityExpiresAt: leaseExpiresAt,
	}, true, nil
}

// transitionDeploymentAssigned moves deployment_id queued->assigned inside
// tx (ADR 0004 atomic claim boundary step 9, first claim only), reusing
// DeploymentRepository's own CAS/event-append logic instead of opening a
// second transaction.
func (r *AgentLeaseRepository) transitionDeploymentAssigned(ctx context.Context, tx pgx.Tx, deploymentID string, at time.Time) error {
	snapshot, err := readSnapshot(ctx, tx, deployment.ID(deploymentID))
	if err != nil {
		return fmt.Errorf("load deployment for assignment: %w", err)
	}
	current, err := deployment.Rehydrate(snapshot)
	if err != nil {
		return fmt.Errorf("rehydrate deployment for assignment: %w", err)
	}
	next, err := current.Transition(deployment.StatusAssigned, at)
	if err != nil {
		return fmt.Errorf("transition deployment to assigned: %w", err)
	}
	return r.deployments.saveTxWithHook(ctx, tx, next, r.inject)
}

// freezeSecretVersions durably freezes the exact secret-version set required
// by this attempt (ADR 0003/0004 atomic claim boundary step 7). No secret
// materialization exists yet in this slice, so versionIDs is always empty
// today; the write path still runs so this call site needs no change when
// the execution slice starts passing real versions.
func freezeSecretVersions(ctx context.Context, tx pgx.Tx, attemptID int64, versionIDs []string) error {
	for _, versionID := range versionIDs {
		if _, err := tx.Exec(ctx, `INSERT INTO frozen_secret_versions(attempt_id, secret_version_id) VALUES ($1,$2)`, attemptID, versionID); err != nil {
			return fmt.Errorf("freeze secret version: %w", err)
		}
	}
	return nil
}

// appendAgentJobEvent appends the next sequenced event for jobID. The caller
// must already hold the job row's lock, so no concurrent writer can also be
// computing the next sequence for it.
func appendAgentJobEvent(ctx context.Context, tx pgx.Tx, jobID int64, attemptID *int64, eventType string) error {
	var sequence int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM agent_job_events WHERE job_id=$1`, jobID).Scan(&sequence); err != nil {
		return fmt.Errorf("compute agent job event sequence: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO agent_job_events(job_id, sequence, event_type, attempt_id) VALUES ($1,$2,$3,$4)`, jobID, sequence, eventType, attemptID); err != nil {
		return fmt.Errorf("append agent job event: %w", err)
	}
	return nil
}

// Renew extends attemptID's lease/authority to database time plus 90
// seconds, or -- for an exact replay of the already accepted sequence --
// returns the persisted deadlines unchanged (ADR 0004 renewal). It locks in
// the ADR's declared order: authorization (SHARE) -> environment slot
// (UPDATE) -> job (UPDATE) -> attempt (UPDATE). Every predicate is
// rechecked against the locked rows; the non-locking lookup that finds which
// slot/job an attempt belongs to is never itself treated as authoritative.
func (r *AgentLeaseRepository) Renew(ctx context.Context, serverID string, presentedDigest [32]byte, attemptID, fence int64, sequence int) (queue.RenewalResult, error) {
	if r == nil || r.pool == nil || !github.ValidOpaqueID(serverID) || attemptID <= 0 || fence <= 0 || sequence < 0 {
		return queue.RenewalResult{}, ErrServerAuthorizationDenied
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return queue.RenewalResult{}, fmt.Errorf("begin agent lease renew: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err = lockActiveServerCredential(ctx, tx, serverID, presentedDigest); err != nil {
		return queue.RenewalResult{}, err
	}

	// Non-locking discovery of which job/environment this attempt belongs
	// to, purely so the slot can be locked before the job and attempt per
	// the ADR hierarchy; every value read here is re-verified against the
	// locked rows below rather than trusted.
	var jobID int64
	var environmentID, ownerServerID string
	if err = tx.QueryRow(ctx, `SELECT job_id, environment_id, server_id FROM agent_job_attempts WHERE id=$1`, attemptID).Scan(&jobID, &environmentID, &ownerServerID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return queue.RenewalResult{}, ErrServerAuthorizationDenied
		}
		return queue.RenewalResult{}, fmt.Errorf("locate agent job attempt: %w", err)
	}
	if ownerServerID != serverID {
		return queue.RenewalResult{}, ErrServerAuthorizationDenied
	}

	var slotFence int64
	var slotCurrentAttemptID *int64
	if err = tx.QueryRow(ctx, `SELECT fence, current_attempt_id FROM environment_slots WHERE environment_id=$1 FOR UPDATE`, environmentID).Scan(&slotFence, &slotCurrentAttemptID); err != nil {
		return queue.RenewalResult{}, fmt.Errorf("lock environment slot for renew: %w", err)
	}

	var jobCancelRequestedAt *time.Time
	if err = tx.QueryRow(ctx, `SELECT cancel_requested_at FROM agent_jobs WHERE id=$1 FOR UPDATE`, jobID).Scan(&jobCancelRequestedAt); err != nil {
		return queue.RenewalResult{}, fmt.Errorf("lock agent job for renew: %w", err)
	}

	var attemptServerID, attemptState string
	var attemptFence int64
	var attemptRenewalSequence int
	var leaseExpiresAt, authorityExpiresAt time.Time
	var cancelDeadlineAt *time.Time
	if err = tx.QueryRow(ctx, `SELECT server_id, state, fence, renewal_sequence, lease_expires_at, authority_expires_at, cancel_deadline_at FROM agent_job_attempts WHERE id=$1 FOR UPDATE`, attemptID).
		Scan(&attemptServerID, &attemptState, &attemptFence, &attemptRenewalSequence, &leaseExpiresAt, &authorityExpiresAt, &cancelDeadlineAt); err != nil {
		return queue.RenewalResult{}, fmt.Errorf("lock agent job attempt for renew: %w", err)
	}

	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return queue.RenewalResult{}, fmt.Errorf("agent lease renew canonical time: %w", err)
	}

	if attemptServerID != serverID {
		return queue.RenewalResult{}, ErrServerAuthorizationDenied
	}
	// slot.current_attempt_id must still name this attempt: defense in depth
	// for the ADR's one-occupant-per-environment invariant, independent of
	// the fence check above.
	if attemptState != "leased" || attemptFence != fence || slotFence != fence ||
		slotCurrentAttemptID == nil || *slotCurrentAttemptID != attemptID || !authorityExpiresAt.After(now) {
		return queue.RenewalResult{}, ErrAgentLeaseStale
	}

	cancelRequested := jobCancelRequestedAt != nil
	if sequence == attemptRenewalSequence {
		// Idempotent replay (ADR 0004: "Replaying the already accepted
		// sequence returns the stored deadlines without extending them
		// again"). No row changes; commit only to release the locks taken
		// above.
		if err = tx.Commit(ctx); err != nil {
			return queue.RenewalResult{}, fmt.Errorf("commit agent lease renew replay: %w", err)
		}
		return queue.RenewalResult{LeaseExpiresAt: leaseExpiresAt, AuthorityExpiresAt: authorityExpiresAt, CancelRequested: cancelRequested}, nil
	}
	if sequence != attemptRenewalSequence+1 {
		return queue.RenewalResult{}, ErrAgentLeaseStale
	}

	// ADR 0004 renewal: "Without cancellation, both lease_expires_at and
	// authority_expires_at become [now+90s]. With cancellation, both remain
	// capped at the earlier cleanup deadline."
	newExpiresAt := now.Add(leaseDuration)
	if cancelDeadlineAt != nil && cancelDeadlineAt.Before(newExpiresAt) {
		newExpiresAt = *cancelDeadlineAt
	}
	if _, err = tx.Exec(ctx, `UPDATE agent_job_attempts SET renewal_sequence=$2, lease_expires_at=$3, authority_expires_at=$3 WHERE id=$1`, attemptID, sequence, newExpiresAt); err != nil {
		return queue.RenewalResult{}, fmt.Errorf("extend agent job attempt lease: %w", err)
	}
	if err = r.inject("attempt_renew"); err != nil {
		return queue.RenewalResult{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return queue.RenewalResult{}, fmt.Errorf("commit agent lease renew: %w", err)
	}
	return queue.RenewalResult{LeaseExpiresAt: newExpiresAt, AuthorityExpiresAt: newExpiresAt, CancelRequested: cancelRequested}, nil
}

func isCheckViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514"
}
