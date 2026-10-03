package postgres

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"autodeploy/internal/auth"
	"autodeploy/internal/deployment"
	"autodeploy/internal/github"
	"autodeploy/internal/queue"
	"autodeploy/internal/runtime/dockerid"
	"autodeploy/internal/runtime/traefik"
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

// ErrAgentLeaseIntegrityConflict reports a completion/cancel-acknowledgement
// replay whose operation ID, result digest, or outcome disagrees with the
// terminal result already committed for that attempt (ADR 0004 completion
// "conflicting digest/type/scope ... is rejected and audited as
// stale/integrity failure").
var ErrAgentLeaseIntegrityConflict = errors.New("agent lease integrity conflict")

// ErrAgentLeaseInvalidInput reports a malformed request (bad outcome/
// failure-class combination, oversized operation ID, etc.) rejected before
// any row is read or locked.
var ErrAgentLeaseInvalidInput = errors.New("agent lease invalid input")

// ErrAgentJobTerminal reports a cancellation request against a job that is
// already succeeded/failed/dead -- terminal jobs are never resurrected.
var ErrAgentJobTerminal = errors.New("agent job already terminal")

// ErrAgentJobActivating reports a cancellation request rejected because the
// deployment already committed the activation linearization point (ADR 0004
// "Cancellation while deployment status is activating is rejected").
var ErrAgentJobActivating = errors.New("agent job deployment is activating")

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
	// cancellationGrace is the ADR 0004 V1 cancellation cleanup grace period:
	// a leased attempt's lease/authority deadlines are capped at
	// now+cancellationGrace the moment cancellation is requested.
	cancellationGrace = 60 * time.Second
	// expireBatchMaxLimit bounds one sweeper tick's work (ADR 0004 Step 4
	// sweeper: "bounded batch of 100").
	expireBatchMaxLimit = 100
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
// success and the package's one generic denial otherwise. The denial is
// reserved for a genuinely absent or non-active server/credential; any other
// database error is returned wrapped (never carrying the digest) so callers
// treat it as retryable infrastructure failure, not as a 401.
func lockActiveServerCredential(ctx context.Context, tx pgx.Tx, serverID string, presentedDigest [32]byte) (string, error) {
	var ownerID, status string
	foundServer := true
	if err := tx.QueryRow(ctx, `SELECT owner_id,status FROM servers WHERE id=$1 FOR SHARE`, serverID).Scan(&ownerID, &status); err != nil {
		// Only a genuinely absent row is a denial. Any other error (reset
		// connection, failover, deadline, 57P01) must stay retryable: the
		// agent client never retries a 401, so collapsing it would silently
		// drop a genuine report.
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("lock active server for credential check: %w", err)
		}
		foundServer = false
	} else if status != "active" {
		foundServer = false
	}
	var storedDigest []byte
	foundCredential := false
	if foundServer {
		if err := tx.QueryRow(ctx, `SELECT digest FROM server_credentials WHERE server_id=$1 AND status='active' FOR SHARE`, serverID).Scan(&storedDigest); err == nil {
			foundCredential = true
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("lock active server credential: %w", err)
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
// authorization (SHARE) -> environment runtime config (SHARE, new position
// "1b" added by migration 000014's claim-time freeze -- see
// lockEnvironmentRuntimeConfig's doc comment) -> environment slot (UPDATE)
// -> job (UPDATE ... SKIP LOCKED) -> attempt (INSERT) -> deployment head
// (UPDATE, first attempt only) -> frozen execution config and secret
// versions (INSERT: resolved fresh on attempt 1, copied forward verbatim on
// a retry per ADR 0003) -> release (INSERT on attempt 1, reused and
// digest-verified on a retry) -> events (INSERT). V1 claims at most one job
// per environment; limit caps the total number of leases returned.
// Candidate discovery is a non-locking ordered read outside any
// transaction; every predicate is rechecked after the relevant row is
// locked inside the claim transaction, and a candidate that no longer
// qualifies -- including one whose environment has no runtime config, or
// whose bindings include a secret head with no active version -- is
// skipped rather than failing the whole call.
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
	// Migration 000014 claim-time freeze, ADR 0004 lock hierarchy position
	// "1b" (see lockEnvironmentRuntimeConfig's doc comment for the full
	// deadlock argument): SHARE-lock this environment's runtime config
	// before anything else tryClaim does, including the slot's UPDATE lock
	// immediately below. A missing config row fails only this candidate,
	// exactly like an occupied slot -- never the whole Claim call, never a
	// default. The row's values are used below only if this turns out to be
	// attempt 1; a retry never reads them (see the copy-forward branch
	// after the attempt is inserted).
	runtimeConfig, runtimeConfigFound, err := lockEnvironmentRuntimeConfig(ctx, tx, environmentID)
	if err != nil {
		return queue.ClaimedLease{}, false, err
	}
	if !runtimeConfigFound {
		return queue.ClaimedLease{}, false, nil
	}

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
	err = tx.QueryRow(ctx, `SELECT fence, current_attempt_id, reconciliation_required FROM environment_slots WHERE environment_id=$1 FOR UPDATE SKIP LOCKED`, environmentID).Scan(&slotFence, &currentAttemptID, &reconciling)
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
	var priorAttemptCount int
	var jobAvailableAt time.Time
	var cancelRequestedAt *time.Time
	err = tx.QueryRow(ctx, `SELECT owner_id, project_id, environment_id, server_id, deployment_id, repository_id, full_ref, commit_sha, state, available_at, cancel_requested_at, attempt_count FROM agent_jobs WHERE id=$1 FOR UPDATE SKIP LOCKED`, jobID).
		Scan(&ownerID, &projectID, &jobEnvironmentID, &jobServerID, &deploymentID, &repositoryID, &ref, &commitSHA, &jobState, &jobAvailableAt, &cancelRequestedAt, &priorAttemptCount)
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

	// plannedAttemptNumber is known now, with the job row locked (so nobody
	// else can advance attempt_count before this transaction commits or
	// rolls back), strictly before any mutation below -- fence advance, job
	// lease, or attempt insert. This is what lets the freeze-viability
	// check below fail cleanly (ok=false, no error) without ever leaving a
	// partial write behind: for attempt 1, an unusable binding or missing
	// config is detected here, before this candidate has touched anything.
	// For a retry (attemptNumber>1), current environment_runtime_bindings
	// state is never consulted at all (ADR 0003): only the previous
	// attempt's own already-frozen rows will be read, later, after this
	// candidate's attempt row exists.
	plannedAttemptNumber := priorAttemptCount + 1
	var freshBindings []frozenBinding
	var freshLiterals []frozenLiteral
	var prevAttemptID int64
	if plannedAttemptNumber == 1 {
		var bindingsOK bool
		freshBindings, bindingsOK, err = resolveRuntimeSecretBindings(ctx, tx, environmentID)
		if err != nil {
			return queue.ClaimedLease{}, false, err
		}
		if !bindingsOK {
			return queue.ClaimedLease{}, false, nil
		}
		var literalsOK bool
		freshLiterals, literalsOK, err = resolveRuntimeLiteralBindings(ctx, tx, environmentID)
		if err != nil {
			return queue.ClaimedLease{}, false, err
		}
		if !literalsOK {
			return queue.ClaimedLease{}, false, nil
		}
	} else {
		err = tx.QueryRow(ctx, `SELECT id FROM agent_job_attempts WHERE job_id=$1 AND attempt_number=$2`, jobID, plannedAttemptNumber-1).Scan(&prevAttemptID)
		if errors.Is(err, pgx.ErrNoRows) {
			// A job whose attempt_count is N>0 must have an attempt row for
			// every number up to N: the attempt insert and the attempt_count
			// bump happen in one transaction, and both tables reject deletes.
			// A gap therefore is not a losable race to skip past but a durable
			// inconsistency, and it must never be answered by re-resolving
			// current secret heads (ADR 0003 forbids adopting a version
			// rotated mid-job). Classify it so a caller can tell integrity
			// failure from a transient database error, exactly as
			// ensureRelease does for a digest mismatch.
			return queue.ClaimedLease{}, false, fmt.Errorf("%w: job %d attempt_count implies attempt %d but no such attempt row exists", ErrAgentLeaseCorruptState, jobID, plannedAttemptNumber-1)
		}
		if err != nil {
			return queue.ClaimedLease{}, false, fmt.Errorf("locate previous attempt %d for retry freeze copy-forward: %w", plannedAttemptNumber-1, err)
		}
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
		if err = transitionDeployment(ctx, tx, r.deployments, deploymentID, deployment.StatusAssigned, now, r.inject); err != nil {
			return queue.ClaimedLease{}, false, err
		}
		if err = r.inject("deployment"); err != nil {
			return queue.ClaimedLease{}, false, err
		}
	}

	// Migration 000014 claim-time freeze, ADR 0004 lock hierarchy position
	// 7 ("secret-version and frozen-reference rows"): attempt 1 durably
	// freezes the execution config and secret-version set just resolved
	// above; a retry copies attempt N-1's own frozen rows forward verbatim
	// (ADR 0003) instead of re-resolving anything. Either branch produces
	// the same frozenExecutionSnapshot/[]frozenBinding shape that
	// computeConfigDigest and the ClaimedLease below consume identically.
	var frozenConfig frozenExecutionSnapshot
	var frozenBindings []frozenBinding
	var frozenLiterals []frozenLiteral
	if plannedAttemptNumber == 1 {
		frozenConfig = runtimeConfig
		frozenBindings = freshBindings
		frozenLiterals = freshLiterals
		if err = insertAttemptExecution(ctx, tx, attemptID, frozenConfig); err != nil {
			return queue.ClaimedLease{}, false, err
		}
		if err = insertFrozenSecretVersions(ctx, tx, attemptID, frozenBindings); err != nil {
			return queue.ClaimedLease{}, false, err
		}
		if err = insertFrozenLiteralValues(ctx, tx, attemptID, frozenLiterals); err != nil {
			return queue.ClaimedLease{}, false, err
		}
	} else {
		frozenConfig, err = copyPreviousAttemptExecution(ctx, tx, attemptID, prevAttemptID)
		if err != nil {
			return queue.ClaimedLease{}, false, err
		}
		frozenBindings, err = copyPreviousFrozenSecretVersions(ctx, tx, attemptID, prevAttemptID)
		if err != nil {
			return queue.ClaimedLease{}, false, err
		}
		frozenLiterals, err = copyPreviousFrozenLiteralValues(ctx, tx, attemptID, prevAttemptID)
		if err != nil {
			return queue.ClaimedLease{}, false, err
		}
	}
	if err = r.inject("secret_freeze"); err != nil {
		return queue.ClaimedLease{}, false, err
	}

	// Also create (attempt 1) or reuse-and-verify (a retry) this
	// deployment's one release row. configDigest is recomputed from
	// frozenConfig/frozenBindings every time -- on a retry that recomputes
	// the exact same digest attempt 1 stored, since both were just copied
	// forward verbatim; any disagreement is ErrAgentLeaseCorruptState, not
	// a silently accepted overwrite.
	configDigest := computeConfigDigest(frozenConfig, frozenBindings, frozenLiterals)
	releaseID, err := ensureRelease(ctx, tx, deploymentID, environmentID, commitSHA, frozenConfig.ReleaseStrategy, configDigest)
	if err != nil {
		return queue.ClaimedLease{}, false, err
	}
	if err = r.inject("release"); err != nil {
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
		ContainerPort: frozenConfig.ContainerPort, ContainerScheme: frozenConfig.ContainerScheme,
		ReleaseStrategy: frozenConfig.ReleaseStrategy, HealthPolicyRevision: frozenConfig.HealthPolicyRevision,
		CPUMillis: frozenConfig.CPUMillis, MemoryBytes: frozenConfig.MemoryBytes,
		RestartPolicy: frozenConfig.RestartPolicy, DrainSeconds: frozenConfig.DrainSeconds,
		HealthGatePath: frozenConfig.HealthGatePath, HealthGateHost: frozenConfig.HealthGateHost,
		ReleaseID: releaseID, SecretBindings: toQueueSecretBindings(frozenBindings),
		LiteralBindings: toQueueLiteralBindings(frozenLiterals),
		LeaseExpiresAt:  leaseExpiresAt, AuthorityExpiresAt: leaseExpiresAt,
	}, true, nil
}

// transitionDeployment moves deploymentID to target inside tx, reusing
// DeploymentRepository's own CAS/event-append logic instead of opening a
// second transaction (ADR 0004 atomic claim boundary step 9, and reused by
// every later Step 4 transition that must commit atomically with a job/
// attempt/slot mutation: cancellation, completion, expiry, and revocation).
// If the deployment is already at target (Transition's documented no-op for
// repeating the current status), nothing is persisted -- calling this twice
// for the same terminal effect, e.g. a completion racing an expiry sweep, is
// idempotent rather than a spurious revision conflict.
func transitionDeployment(ctx context.Context, tx pgx.Tx, deployments *DeploymentRepository, deploymentID string, target deployment.Status, at time.Time, hook func(string) error) error {
	snapshot, err := readSnapshot(ctx, tx, deployment.ID(deploymentID))
	if err != nil {
		return fmt.Errorf("load deployment for transition: %w", err)
	}
	current, err := deployment.Rehydrate(snapshot)
	if err != nil {
		return fmt.Errorf("rehydrate deployment for transition: %w", err)
	}
	next, err := current.Transition(target, at)
	if err != nil {
		return fmt.Errorf("transition deployment to %s: %w", target, err)
	}
	if next.Revision() == current.Revision() {
		return nil
	}
	return deployments.saveTxWithHook(ctx, tx, next, hook)
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

// validOperationID mirrors agent_job_attempts.result_operation_id's CHECK
// (octet_length BETWEEN 1 AND 128).
func validOperationID(id string) bool {
	n := len(id)
	return n >= 1 && n <= 128
}

// closeExpiredOrRevokedAttempt closes a leased attempt whose authority has
// ended for a reason the agent never reported: it either authority-expired
// (the sweeper/expireOne) or its server's credential/identity was just
// revoked (ADR 0004's revocation paragraph, called from
// revokeInFlightLeases).
//
// KNOWN GAP, deliberately deferred to Step 10c-2b (step D): this function
// (reached from expireOne and revokeInFlightLeases) still closes an attempt
// that has an unresolved activation operation (see unresolvedActivation),
// which ADR 0005 forbids ("an in-flight syscall cannot be preempted"). 10c-2b
// adds the diversion; do not treat its absence as an oversight.
//
// Both cases get identical effects: the attempt
// closes as cancelled (if the job's cancellation was already requested) or
// expired; the environment fence advances and the slot is marked
// reconciliation_required with its occupant cleared (an already-executing
// host syscall cannot be preempted, so no new work may use the slot until
// the agent reconciles); and the job is terminalized/retried/dead-lettered
// exactly as ADR 0004's expiry section specifies -- attempt_count is never
// incremented here, since it counts started attempts, not closures.
//
// The caller must already hold FOR UPDATE locks (in ADR order: slot, job,
// attempt) and have revalidated that the attempt is still leased and still
// the slot's current occupant; this function only writes.
func closeExpiredOrRevokedAttempt(ctx context.Context, tx pgx.Tx, deployments *DeploymentRepository, inject func(string) error, jobID, attemptID int64, environmentID string, attemptNumber int, deploymentID string, cancelRequested bool, now time.Time) error {
	closedState := "expired"
	if cancelRequested {
		closedState = "cancelled"
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_job_attempts SET state=$2, closed_at=$3 WHERE id=$1`, attemptID, closedState, now); err != nil {
		return fmt.Errorf("close agent job attempt: %w", err)
	}
	if err := inject("attempt_close"); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, `UPDATE environment_slots SET fence=fence+1, current_attempt_id=NULL, reconciliation_required=true, reconciliation_attempt_id=$2, updated_at=$3 WHERE environment_id=$1`,
		environmentID, attemptID, now); err != nil {
		return fmt.Errorf("advance environment slot for closed attempt: %w", err)
	}
	if err := inject("slot_reconcile"); err != nil {
		return err
	}

	var jobState string
	var retryAvailableAt time.Time
	switch {
	case cancelRequested:
		jobState = "cancelled"
	case attemptNumber >= queue.MaxAttempts:
		jobState = "dead"
	default:
		jobState = "retry_wait"
		retryAvailableAt = now.Add(queue.FullJitterDelay(attemptNumber))
	}
	if jobState == "retry_wait" {
		if _, err := tx.Exec(ctx, `UPDATE agent_jobs SET state='retry_wait', available_at=$2, updated_at=$3 WHERE id=$1 AND state='leased'`, jobID, retryAvailableAt, now); err != nil {
			return fmt.Errorf("schedule agent job retry: %w", err)
		}
	} else {
		if _, err := tx.Exec(ctx, `UPDATE agent_jobs SET state=$2, updated_at=$3 WHERE id=$1 AND state='leased'`, jobID, jobState, now); err != nil {
			return fmt.Errorf("close agent job: %w", err)
		}
	}
	if err := inject("job_close"); err != nil {
		return err
	}

	if jobState == "cancelled" || jobState == "dead" {
		target := deployment.StatusCancelled
		if jobState == "dead" {
			target = deployment.StatusFailed
		}
		if err := transitionDeployment(ctx, tx, deployments, deploymentID, target, now, inject); err != nil {
			return err
		}
		if err := inject("deployment"); err != nil {
			return err
		}
	}

	eventType := "expired"
	if cancelRequested {
		eventType = "cancelled"
	}
	if err := appendAgentJobEvent(ctx, tx, jobID, &attemptID, eventType); err != nil {
		return err
	}
	if !cancelRequested {
		switch jobState {
		case "retry_wait":
			if err := appendAgentJobEvent(ctx, tx, jobID, &attemptID, "retry_scheduled"); err != nil {
				return err
			}
		case "dead":
			if err := appendAgentJobEvent(ctx, tx, jobID, &attemptID, "dead"); err != nil {
				return err
			}
		}
	}
	return inject("job_event")
}

// Complete records the first terminal success/failure result for attemptID,
// or -- if the attempt is already closed -- replays the stored terminal
// result when operationID/resultDigest/outcome/failureClass exactly match
// what was already committed (ADR 0004 "Terminal retry uses a separate
// read-only replay branch ... An exact replay returns the stored terminal
// response without reacquiring a slot, extending a lease, or mutating
// state"). A replay is accepted even after authority_expires_at has passed,
// since it never touches authority. A closed attempt with no stored result
// (expired or a cancellation closed by the sweeper without an
// acknowledgement) has nothing to replay against and is rejected as stale --
// this is the "late completion from an expired attempt" case; a closed
// attempt whose stored result disagrees with this request is rejected as an
// integrity conflict. Lock order: authorization (SHARE) -> environment slot
// (UPDATE) -> job (UPDATE) -> attempt (UPDATE), matching Renew.
func (r *AgentLeaseRepository) Complete(ctx context.Context, serverID string, presentedDigest [32]byte, attemptID, fence int64, outcome queue.CompletionOutcome, failureClass queue.FailureClass, operationID string, resultDigest [32]byte) error {
	if r == nil || r.pool == nil || !github.ValidOpaqueID(serverID) || attemptID <= 0 || fence <= 0 || !validOperationID(operationID) {
		return ErrAgentLeaseInvalidInput
	}
	switch outcome {
	case queue.CompletionSucceeded:
		if failureClass != "" {
			return ErrAgentLeaseInvalidInput
		}
	case queue.CompletionFailed:
		if !failureClass.Valid() {
			return ErrAgentLeaseInvalidInput
		}
	default:
		return ErrAgentLeaseInvalidInput
	}

	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin agent lease complete: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err = lockActiveServerCredential(ctx, tx, serverID, presentedDigest); err != nil {
		return err
	}

	var jobID int64
	var environmentID, ownerServerID string
	if err = tx.QueryRow(ctx, `SELECT job_id, environment_id, server_id FROM agent_job_attempts WHERE id=$1`, attemptID).Scan(&jobID, &environmentID, &ownerServerID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrServerAuthorizationDenied
		}
		return fmt.Errorf("locate agent job attempt: %w", err)
	}
	if ownerServerID != serverID {
		return ErrServerAuthorizationDenied
	}

	var slotFence int64
	var slotCurrentAttemptID *int64
	if err = tx.QueryRow(ctx, `SELECT fence, current_attempt_id FROM environment_slots WHERE environment_id=$1 FOR UPDATE`, environmentID).Scan(&slotFence, &slotCurrentAttemptID); err != nil {
		return fmt.Errorf("lock environment slot for complete: %w", err)
	}

	var deploymentID string
	if err = tx.QueryRow(ctx, `SELECT deployment_id FROM agent_jobs WHERE id=$1 FOR UPDATE`, jobID).Scan(&deploymentID); err != nil {
		return fmt.Errorf("lock agent job for complete: %w", err)
	}

	var attemptState, attemptServerID string
	var attemptFence int64
	var attemptNumber int
	var authorityExpiresAt time.Time
	var storedFailureClass, storedOperationID *string
	var storedResultDigest []byte
	if err = tx.QueryRow(ctx, `SELECT state, server_id, fence, attempt_number, authority_expires_at, failure_class, result_operation_id, result_digest FROM agent_job_attempts WHERE id=$1 FOR UPDATE`, attemptID).
		Scan(&attemptState, &attemptServerID, &attemptFence, &attemptNumber, &authorityExpiresAt, &storedFailureClass, &storedOperationID, &storedResultDigest); err != nil {
		return fmt.Errorf("lock agent job attempt for complete: %w", err)
	}
	if attemptServerID != serverID {
		return ErrServerAuthorizationDenied
	}

	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return fmt.Errorf("agent lease complete canonical time: %w", err)
	}

	if attemptState != "leased" {
		if storedOperationID == nil {
			return ErrAgentLeaseStale
		}
		expectedState := "succeeded"
		var expectedFailureClass *string
		if outcome == queue.CompletionFailed {
			expectedState = "failed"
			fc := string(failureClass)
			expectedFailureClass = &fc
		}
		classMatches := (storedFailureClass == nil && expectedFailureClass == nil) ||
			(storedFailureClass != nil && expectedFailureClass != nil && *storedFailureClass == *expectedFailureClass)
		digestMatches := len(storedResultDigest) == 32 && [32]byte(storedResultDigest) == resultDigest
		if attemptState != expectedState || *storedOperationID != operationID || !digestMatches || !classMatches {
			return ErrAgentLeaseIntegrityConflict
		}
		if err = tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit agent lease complete replay: %w", err)
		}
		return nil
	}

	// An open activation owns this attempt's completion: the host switch may
	// already be in flight (switch_authorized), or its outcome is ambiguous
	// and awaiting declared resolution (reconciliation_required). In both
	// cases completing the attempt here would resolve that ambiguity blindly
	// (ADR 0005), so only ReportActivationResult (tx2) may close the
	// attempt, free the slot and close the job, and only reconciliation may
	// resolve a blocked one. Slot, job and attempt
	// locks are held and the attempt is known 'leased' here. A Complete that
	// arrives after a successful tx2 never reaches this point -- the attempt
	// is then closed and takes the replay-or-conflict branch above.
	var activationOpen bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM release_operations WHERE attempt_id=$1 AND fence=$2 AND `+unresolvedActivationSQL+`)`, attemptID, fence).Scan(&activationOpen); err != nil {
		return fmt.Errorf("check open activation for complete: %w", err)
	}
	if activationOpen {
		return ErrAgentLeaseActivationOpen
	}

	if attemptFence != fence || slotFence != fence || slotCurrentAttemptID == nil || *slotCurrentAttemptID != attemptID || !authorityExpiresAt.After(now) {
		return ErrAgentLeaseStale
	}

	var failureClassParam *string
	closedState := "succeeded"
	if outcome == queue.CompletionFailed {
		closedState = "failed"
		fc := string(failureClass)
		failureClassParam = &fc
	}
	if _, err = tx.Exec(ctx, `UPDATE agent_job_attempts SET state=$2, closed_at=$3, failure_class=$4, result_operation_id=$5, result_digest=$6 WHERE id=$1`,
		attemptID, closedState, now, failureClassParam, operationID, resultDigest[:]); err != nil {
		return fmt.Errorf("close agent job attempt: %w", err)
	}
	if err = r.inject("attempt_close"); err != nil {
		return err
	}

	if _, err = tx.Exec(ctx, `UPDATE environment_slots SET current_attempt_id=NULL, updated_at=$2 WHERE environment_id=$1 AND current_attempt_id=$3`, environmentID, now, attemptID); err != nil {
		return fmt.Errorf("free environment slot: %w", err)
	}
	if err = r.inject("slot_free"); err != nil {
		return err
	}

	var newJobState string
	var retryAvailableAt time.Time
	switch {
	case outcome == queue.CompletionSucceeded:
		newJobState = "succeeded"
	case failureClass.Retryable() && attemptNumber < queue.MaxAttempts:
		newJobState = "retry_wait"
		retryAvailableAt = now.Add(queue.FullJitterDelay(attemptNumber))
	case failureClass.Retryable():
		newJobState = "dead"
	default:
		newJobState = "failed"
	}
	if newJobState == "retry_wait" {
		if _, err = tx.Exec(ctx, `UPDATE agent_jobs SET state='retry_wait', available_at=$2, updated_at=$3 WHERE id=$1 AND state='leased'`, jobID, retryAvailableAt, now); err != nil {
			return fmt.Errorf("schedule agent job retry: %w", err)
		}
	} else {
		if _, err = tx.Exec(ctx, `UPDATE agent_jobs SET state=$2, updated_at=$3 WHERE id=$1 AND state='leased'`, jobID, newJobState, now); err != nil {
			return fmt.Errorf("close agent job: %w", err)
		}
	}
	if err = r.inject("job_close"); err != nil {
		return err
	}

	if newJobState == "dead" || newJobState == "failed" {
		if err = transitionDeployment(ctx, tx, r.deployments, deploymentID, deployment.StatusFailed, now, r.inject); err != nil {
			return err
		}
		if err = r.inject("deployment"); err != nil {
			return err
		}
	}

	eventType := "succeeded"
	switch newJobState {
	case "failed":
		eventType = "failed"
	case "retry_wait":
		eventType = "retry_scheduled"
	case "dead":
		eventType = "dead"
	}
	if err = appendAgentJobEvent(ctx, tx, jobID, &attemptID, eventType); err != nil {
		return err
	}
	if err = r.inject("job_event"); err != nil {
		return err
	}

	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit agent lease complete: %w", err)
	}
	return nil
}

// ErrAgentLeaseActivationCandidateNotReady reports that this attempt's
// candidate release is not in a state tx1 can authorize an activation for:
// not 'candidate' or 'ready' (already 'active', 'retained', 'failed',
// 'removing', or 'removed' -- see ADR 0005's release state machine and
// tasks/todo.md Step 10c design §7 on why "re-activate an already-active
// release" is unreachable in the intended flow and only matters as a
// defensive refusal here).
var ErrAgentLeaseActivationCandidateNotReady = errors.New("agent lease activation candidate release is not ready")

// ErrAgentLeaseActivationNoRouteHead reports that environmentID has never
// been assigned a hostname (Step 10a AssignRouteHostname): there is nothing
// for tx1 to render a route document against.
var ErrAgentLeaseActivationNoRouteHead = errors.New("agent lease activation environment has no route head")

// Activate is tx1 of Step 10c's fenced activation (ADR 0005 "Fenced
// activation and reconciliation"; tasks/todo.md Step 10c, the 2026-09-22
// orchestrator decisions binding over the design draft below them). It is
// the control-plane half of the exchange: the agent presents only
// attempt_id/fence/image_digest (never a route digest of its own -- the
// control plane renders the route document and computes its digest itself,
// per the orchestrator's binding decision overriding the design draft's
// agent-renders assumption), and this call authorizes exactly one
// switch-authorized release_operations row before any host mutation is
// permitted to begin (ADR 0005: "Begin the fenced activation operation and
// transition to activating" precedes any write to the host).
//
// Lock order (ADR 0004 hierarchy, 2026-09-22 amendment, position "6b"):
// authorization (SHARE) -> environment slot (UPDATE) -> job (UPDATE) ->
// attempt (UPDATE) -> deployment (UPDATE, via transitionDeployment) ->
// route head (UPDATE, new position 6b) -> candidate release (UPDATE) ->
// append-only inserts (release_operations, agent_job_events).
//
// Environment-scoped refusal: after the attempt-scoped replay check, one
// non-locking read asks whether ANY attempt of this environment holds an
// unresolved activate operation. If so Activate returns the typed,
// non-retryable ErrAgentLeaseActivationEnvironmentBlocked immediately,
// before any deployment write and independent of the route-head flag; the
// INSERT's unique-index violation is a second, defence-in-depth enforcement
// point. A route-head flag set with NO unresolved operation stays
// ErrAgentLeaseStale.
//
// Idempotency: two concurrent /activate calls for the same attempt/fence
// necessarily name the same environment (an attempt belongs to exactly one
// environment_slots row for its whole lifetime), so the earliest lock both
// transactions actually contend on is environment_slots FOR UPDATE
// (position 3, locked a few lines below) -- not the attempt row (position
// 5), which is locked later and is redundant for this specific race, only
// load-bearing for the fence/authority/cancellation rechecks it protects.
// Whichever transaction locks the environment slot row first proceeds
// through every subsequent lock (job, attempt, deployment, route head,
// candidate release) uncontended; the other blocks on that same slot row
// until the first commits or rolls back, then re-reads every value fresh
// under its own locks and only then reaches the release_operations check
// below, observing the row the first transaction just inserted and taking
// the replay branch, never inserting a second one. A presented image_digest
// identical to the one already recorded for this attempt/fence's still-
// switch_authorized operation replays the stored response without mutating
// anything further; a different image_digest is rejected as
// ErrAgentLeaseIntegrityConflict ("a conflicting digest fails", ADR 0005).
func (r *AgentLeaseRepository) Activate(ctx context.Context, serverID string, presentedDigest [32]byte, attemptID, fence int64, imageDigest [32]byte) (queue.ActivationResult, error) {
	if r == nil || r.pool == nil || !github.ValidOpaqueID(serverID) || attemptID <= 0 || fence <= 0 {
		return queue.ActivationResult{}, ErrAgentLeaseInvalidInput
	}

	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return queue.ActivationResult{}, fmt.Errorf("begin agent lease activate: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err = lockActiveServerCredential(ctx, tx, serverID, presentedDigest); err != nil {
		return queue.ActivationResult{}, err
	}

	// Non-locking peek, exactly like Complete/Renew: only used to find which
	// slot to lock before the job/attempt, per the ADR hierarchy. Every
	// value read here is re-verified against the locked rows below.
	var jobID int64
	var environmentID, ownerServerID string
	if err = tx.QueryRow(ctx, `SELECT job_id, environment_id, server_id FROM agent_job_attempts WHERE id=$1`, attemptID).Scan(&jobID, &environmentID, &ownerServerID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return queue.ActivationResult{}, ErrServerAuthorizationDenied
		}
		return queue.ActivationResult{}, fmt.Errorf("locate agent job attempt: %w", err)
	}
	if ownerServerID != serverID {
		return queue.ActivationResult{}, ErrServerAuthorizationDenied
	}

	var slotFence int64
	var slotCurrentAttemptID *int64
	if err = tx.QueryRow(ctx, `SELECT fence, current_attempt_id FROM environment_slots WHERE environment_id=$1 FOR UPDATE`, environmentID).Scan(&slotFence, &slotCurrentAttemptID); err != nil {
		return queue.ActivationResult{}, fmt.Errorf("lock environment slot for activate: %w", err)
	}

	var deploymentID string
	var cancelRequestedAt *time.Time
	if err = tx.QueryRow(ctx, `SELECT deployment_id, cancel_requested_at FROM agent_jobs WHERE id=$1 FOR UPDATE`, jobID).Scan(&deploymentID, &cancelRequestedAt); err != nil {
		return queue.ActivationResult{}, fmt.Errorf("lock agent job for activate: %w", err)
	}

	var attemptState, attemptServerID string
	var attemptFence int64
	var authorityExpiresAt time.Time
	if err = tx.QueryRow(ctx, `SELECT state, server_id, fence, authority_expires_at FROM agent_job_attempts WHERE id=$1 FOR UPDATE`, attemptID).
		Scan(&attemptState, &attemptServerID, &attemptFence, &authorityExpiresAt); err != nil {
		return queue.ActivationResult{}, fmt.Errorf("lock agent job attempt for activate: %w", err)
	}
	if attemptServerID != serverID {
		return queue.ActivationResult{}, ErrServerAuthorizationDenied
	}

	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return queue.ActivationResult{}, fmt.Errorf("agent lease activate canonical time: %w", err)
	}

	// Every ADR 0005 recheck this endpoint owns, mirroring Complete's own
	// combined fence/slot/authority check exactly (see Complete's combined check):
	// attempt must still be the leased, currently-occupying attempt for this
	// fence, with unexpired authority, and no cancellation must have been
	// requested against its job. Unlike Complete there is no "attempt already
	// closed" replay branch here at all: tx1 never closes the attempt (only
	// Complete-on-failure or tx2 do), so an attempt
	// that is not 'leased' when Activate reaches it is simply stale.
	if attemptState != "leased" || attemptFence != fence || slotFence != fence ||
		slotCurrentAttemptID == nil || *slotCurrentAttemptID != attemptID || !authorityExpiresAt.After(now) {
		return queue.ActivationResult{}, ErrAgentLeaseStale
	}
	if cancelRequestedAt != nil {
		return queue.ActivationResult{}, ErrAgentLeaseStale
	}

	// Idempotent replay check. Safe against a concurrent second /activate for
	// this same attempt/fence because the environment_slots row locked above
	// (position 3 -- the earliest lock two such calls actually share, see
	// this function's own doc comment) already serializes them: a racing
	// call blocks on that same slot row until this transaction commits or
	// rolls back, then re-locks every row itself and observes whatever this
	// transaction left behind (including a freshly inserted operation row),
	// never a state this transaction hasn't committed yet.
	var existingOperationID int64
	var existingDigest, existingRouteDigest []byte
	var existingExpectedRouteRevision int64
	var existingExpectedActiveReleaseID *string
	var existingState string
	err = tx.QueryRow(ctx, `SELECT id, digest, route_digest, expected_route_revision, expected_active_release_id, state FROM release_operations WHERE attempt_id=$1 AND fence=$2 AND kind='activate'`, attemptID, fence).
		Scan(&existingOperationID, &existingDigest, &existingRouteDigest, &existingExpectedRouteRevision, &existingExpectedActiveReleaseID, &existingState)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return queue.ActivationResult{}, fmt.Errorf("check existing activation operation: %w", err)
	}
	if err == nil {
		// Three cases, in this order. Authorization liveness ("may this call
		// still be handed the switch?") and "is this activation unresolved?"
		// are different questions: only switch_authorized may replay a full
		// authorization, so it is tested first and by name.
		if existingState != "switch_authorized" {
			if unresolvedActivation(existingState) {
				// Unresolved but no longer authorized (reconciliation_required):
				// the host may already have switched. Never answer with a
				// success-shaped authorization, and a bare stale error would
				// tell the agent nothing true.
				return queue.ActivationResult{}, ErrAgentLeaseActivationEnvironmentBlocked
			}
			// Resolved (tx2 produces succeeded; failed comes only from the
			// operator), or otherwise closed: this call is stale, not a live
			// replay.
			return queue.ActivationResult{}, ErrAgentLeaseStale
		}
		if len(existingDigest) != 32 || [32]byte(existingDigest) != imageDigest {
			return queue.ActivationResult{}, ErrAgentLeaseIntegrityConflict
		}
		// True replay: read the current hostname under lock for an accurate
		// response, but perform no further mutation at all -- no candidate
		// transition, no deployment transition (both already happened on the
		// original call and are not repeated), no new event.
		var hostname string
		if err = tx.QueryRow(ctx, `SELECT hostname FROM route_heads WHERE environment_id=$1 FOR UPDATE`, environmentID).Scan(&hostname); err != nil {
			return queue.ActivationResult{}, fmt.Errorf("lock route head for activate replay: %w", err)
		}
		result := queue.ActivationResult{
			OperationID:              strconv.FormatInt(existingOperationID, 10),
			RouteRevision:            existingExpectedRouteRevision + 1,
			Hostname:                 hostname,
			EntrypointName:           traefik.DefaultEntrypointName,
			CertResolverName:         traefik.DefaultCertResolverName,
			BackendRuntimeInstanceID: dockerid.AttemptRuntimeInstanceID(attemptID),
			RouteDigest:              "sha256:" + hex.EncodeToString(existingRouteDigest),
		}
		if err = tx.Commit(ctx); err != nil {
			return queue.ActivationResult{}, fmt.Errorf("commit agent lease activate replay: %w", err)
		}
		return result, nil
	}

	// Does this ENVIRONMENT hold an unresolved activate operation, from ANY
	// attempt? The query above is keyed on attempt_id and fence, so it cannot
	// see another attempt's stranded row; this one is environment-scoped. It
	// is read here, at the release_operations position of the lock order
	// (after the attempt row, before the deployment and route head), as a
	// plain non-locking SELECT; no release_operations READ happens after the
	// route head lock (the INSERT below is a write at ADR position 8). A plain
	// read is sufficient because the environment_slots row locked above is
	// held by every writer of this environment's activation operations, so the
	// answer cannot change before this transaction ends.
	//
	// The refusal is returned right here, independent of the route-head flag
	// (the sweeper can strand an operation with the flag clear until step D),
	// so no deployment write happens and no later check (candidate readiness,
	// frozen execution read, render) can mask it with a retryable error. The
	// replay block above has already won for the same attempt.
	var environmentHoldsUnresolvedActivation bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM release_operations WHERE environment_id=$1 AND `+unresolvedActivationSQL+`)`, environmentID).
		Scan(&environmentHoldsUnresolvedActivation); err != nil {
		return queue.ActivationResult{}, fmt.Errorf("check environment unresolved activation: %w", err)
	}
	if environmentHoldsUnresolvedActivation {
		return queue.ActivationResult{}, ErrAgentLeaseActivationEnvironmentBlocked
	}

	// Fresh authorization. Deployment first (position 6, via the existing
	// helper every other lease endpoint already uses for this transition).
	if err = transitionDeployment(ctx, tx, r.deployments, deploymentID, deployment.StatusActivating, now, r.inject); err != nil {
		return queue.ActivationResult{}, err
	}
	if err = r.inject("deployment"); err != nil {
		return queue.ActivationResult{}, err
	}

	// Route head, position 6b per the 2026-09-22 ADR 0004 amendment. tx1
	// reads the current route_revision/active_release_id itself under this
	// lock -- the agent has never seen either value before this response, so
	// there is nothing for it to assert as an "expected" value the way
	// AssignRouteHostname's admin-facing CAS does.
	var hostname string
	var routeRevision int64
	var activeReleaseID *string
	var reconciliationRequired bool
	err = tx.QueryRow(ctx, `SELECT hostname, route_revision, active_release_id, reconciliation_required FROM route_heads WHERE environment_id=$1 FOR UPDATE`, environmentID).
		Scan(&hostname, &routeRevision, &activeReleaseID, &reconciliationRequired)
	if errors.Is(err, pgx.ErrNoRows) {
		return queue.ActivationResult{}, ErrAgentLeaseActivationNoRouteHead
	}
	if err != nil {
		return queue.ActivationResult{}, fmt.Errorf("lock route head for activate: %w", err)
	}
	if reconciliationRequired {
		// Flag set with NO unresolved operation (the unresolved case was
		// refused above): a different, so far unexplained condition that keeps
		// the generic stale answer.
		return queue.ActivationResult{}, ErrAgentLeaseStale
	}

	// Candidate release, position 7 (after the route head, per the design's
	// own reasoning: releases postdates ADR 0004's original hierarchy but
	// belongs alongside "secret-version and frozen-reference rows"). One
	// release per deployment (releases.deployment_id UNIQUE), identity
	// looked up non-locking (deployment_id is immutable) then locked.
	var releaseID, releaseState string
	var releaseImageDigest []byte
	err = tx.QueryRow(ctx, `SELECT id, state, image_digest FROM releases WHERE deployment_id=$1 FOR UPDATE`, deploymentID).Scan(&releaseID, &releaseState, &releaseImageDigest)
	if errors.Is(err, pgx.ErrNoRows) {
		return queue.ActivationResult{}, fmt.Errorf("%w: deployment %s has no release row", ErrAgentLeaseCorruptState, deploymentID)
	}
	if err != nil {
		return queue.ActivationResult{}, fmt.Errorf("lock candidate release for activate: %w", err)
	}
	switch releaseState {
	case "candidate":
		// First time this release becomes ready: the write-once image_digest
		// set and the candidate->ready transition happen together, in the
		// one UPDATE releases_validate_update's trigger requires them in
		// (Step 10c design §1's previously-unrecognized 9b/10a/10b gap: this
		// is the first point anywhere in the codebase that ever learns the
		// candidate's built image digest and sets it durably).
		if _, err = tx.Exec(ctx, `UPDATE releases SET image_digest=$2, state='ready', updated_at=$3 WHERE id=$1`, releaseID, imageDigest[:], now); err != nil {
			return queue.ActivationResult{}, fmt.Errorf("transition candidate release to ready: %w", err)
		}
		if err = r.inject("release_ready"); err != nil {
			return queue.ActivationResult{}, err
		}
	case "ready":
		// Already ready from an earlier attempt of the same job (ensureRelease
		// reuses one release row per deployment across retries) or an earlier,
		// lost /activate response for this very attempt that never reached a
		// stored operation row (e.g. a crash between this UPDATE committing
		// and the operation insert below, on some prior call this replay
		// branch above cannot see because no row survived). The presented
		// digest must agree with what is already durably, write-once stored.
		if len(releaseImageDigest) != 32 || [32]byte(releaseImageDigest) != imageDigest {
			return queue.ActivationResult{}, ErrAgentLeaseIntegrityConflict
		}
	default:
		// 'active', 'retained', 'failed', 'removing', 'removed': not
		// authorizable (Step 10c design §7 -- "repeat activation of an
		// already-active release" is unreachable in the intended flow and
		// only matters as this defensive refusal).
		return queue.ActivationResult{}, ErrAgentLeaseActivationCandidateNotReady
	}

	// Render the route document and compute its digest here, in the control
	// plane (2026-09-22 orchestrator decision, overriding the design draft's
	// agent-renders assumption): internal/runtime/traefik has no dependency
	// on internal/runtime/docker (Step 10c's dockerid extraction removed
	// that coupling -- see internal/runtime/dockerid's package doc), so
	// importing it here does not pull any Docker Engine API client into the
	// control-plane binary.
	var containerPort int
	var containerScheme string
	if err = tx.QueryRow(ctx, `SELECT container_port, container_scheme FROM agent_job_attempt_execution WHERE attempt_id=$1`, attemptID).Scan(&containerPort, &containerScheme); err != nil {
		return queue.ActivationResult{}, fmt.Errorf("load frozen execution config for activate: %w", err)
	}
	route := traefik.EnvironmentRoute{
		EnvironmentID:            environmentID,
		RouteRevision:            uint64(routeRevision) + 1,
		Hostname:                 hostname,
		EntrypointName:           traefik.DefaultEntrypointName,
		CertResolverName:         traefik.DefaultCertResolverName,
		BackendRuntimeInstanceID: dockerid.AttemptRuntimeInstanceID(attemptID),
		BackendPort:              containerPort,
		BackendScheme:            containerScheme,
		ReleaseID:                releaseID,
		FenceID:                  strconv.FormatInt(fence, 10),
	}
	_, routeDigestHex, err := traefik.Render(route)
	if err != nil {
		return queue.ActivationResult{}, fmt.Errorf("render activation route document: %w", err)
	}
	routeDigestRaw, err := hex.DecodeString(strings.TrimPrefix(routeDigestHex, "sha256:"))
	if err != nil {
		return queue.ActivationResult{}, fmt.Errorf("decode rendered route digest: %w", err)
	}
	if len(routeDigestRaw) != 32 {
		return queue.ActivationResult{}, fmt.Errorf("decode rendered route digest: got %d bytes, want 32", len(routeDigestRaw))
	}

	var expectedActiveReleaseID *string
	if activeReleaseID != nil && *activeReleaseID != "" {
		expectedActiveReleaseID = activeReleaseID
	}

	var operationID int64
	// A stranded unresolved activation in this environment (any attempt)
	// collides here on the widened unique index; it is a deterministic,
	// non-retryable condition, never a raw 23505.
	err = tx.QueryRow(ctx, `INSERT INTO release_operations(release_id, job_id, attempt_id, fence, kind, digest, state, environment_id, expected_active_release_id, expected_route_revision, route_digest)
		VALUES ($1,$2,$3,$4,'activate',$5,'switch_authorized',$6,$7,$8,$9) RETURNING id`,
		releaseID, jobID, attemptID, fence, imageDigest[:], environmentID, expectedActiveReleaseID, routeRevision, routeDigestRaw).Scan(&operationID)
	if err != nil {
		if isUniqueViolationOf(err, releaseOperationsOpenActivationIndex) {
			return queue.ActivationResult{}, ErrAgentLeaseActivationEnvironmentBlocked
		}
		return queue.ActivationResult{}, fmt.Errorf("insert activation operation: %w", err)
	}
	if err = r.inject("operation_insert"); err != nil {
		return queue.ActivationResult{}, err
	}

	if err = appendAgentJobEvent(ctx, tx, jobID, &attemptID, "activation_authorized"); err != nil {
		return queue.ActivationResult{}, err
	}
	if err = r.inject("job_event"); err != nil {
		return queue.ActivationResult{}, err
	}

	if err = tx.Commit(ctx); err != nil {
		return queue.ActivationResult{}, fmt.Errorf("commit agent lease activate: %w", err)
	}

	return queue.ActivationResult{
		OperationID:              strconv.FormatInt(operationID, 10),
		RouteRevision:            routeRevision + 1,
		Hostname:                 hostname,
		EntrypointName:           traefik.DefaultEntrypointName,
		CertResolverName:         traefik.DefaultCertResolverName,
		BackendRuntimeInstanceID: dockerid.AttemptRuntimeInstanceID(attemptID),
		RouteDigest:              routeDigestHex,
	}, nil
}

// ErrAgentLeaseActivationOpen reports a Complete call refused because a
// unresolved activation operation exists for the attempt/fence:
// only ReportActivationResult (tx2) may close that attempt, free its slot
// and close its job, since the host switch may already be in flight (ADR
// 0005 "An in-flight syscall cannot be preempted").
var ErrAgentLeaseActivationOpen = errors.New("agent lease activation operation is open")

// ErrAgentLeaseActivationEnvironmentBlocked reports that Activate was refused
// because the environment holds an unresolved activation operation (see
// unresolvedActivation): the host may already have switched, so no further
// activation may start until an operator resolves it. Non-retryable.
var ErrAgentLeaseActivationEnvironmentBlocked = errors.New("agent lease activation environment is blocked by an unresolved activation")

// releaseOperationsOpenActivationIndex is the unique index (migration 000021)
// enforcing one unresolved activate operation per environment.
const releaseOperationsOpenActivationIndex = "release_operations_environment_open_activation_idx"

// ErrAgentLeaseActivationRouteMoved reports that the route head no longer
// matches the route_revision/active_release_id tx1 recorded on the
// operation, so tx2 cannot finalize the activation against it.
var ErrAgentLeaseActivationRouteMoved = errors.New("agent lease activation route head moved since authorization")

const (
	// maxActivationFailureSummaryBytes bounds the sanitized failure summary
	// tx2 stores in release_operations.outcome_summary; the column's own
	// CHECK allows 2048, this leaves headroom and keeps rows small.
	maxActivationFailureSummaryBytes = 512
)

// sanitizeOutcomeSummary makes agent-supplied text safe for
// release_operations.outcome_summary: valid UTF-8, no control characters
// (the column's CHECK rejects [\x00-\x1f\x7f]; Unicode control runes are
// dropped as well), trimmed and truncated on a rune boundary.
func sanitizeOutcomeSummary(summary string) string {
	summary = strings.ToValidUTF8(summary, "?")
	summary = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, summary)
	summary = strings.TrimSpace(summary)
	if len(summary) > maxActivationFailureSummaryBytes {
		cut := maxActivationFailureSummaryBytes
		for cut > 0 && !utf8.RuneStart(summary[cut]) {
			cut--
		}
		summary = summary[:cut]
	}
	return summary
}

// activationFinalizeInput is everything finalizeActivationSucceeded needs,
// already loaded and verified by the caller under the slot/job/attempt/
// operation locks. Step 10c-2b's operator-declared "activated" reconciliation
// outcome reuses the helper with its own caller-verified values.
type activationFinalizeInput struct {
	jobID                   int64
	attemptID               int64
	operationID             int64
	environmentID           string
	deploymentID            string
	candidateReleaseID      string
	expectedRouteRevision   int64
	expectedActiveReleaseID *string
	// resultDigest is the route digest recorded as the attempt's
	// result_digest (agent_job_attempts requires one on a succeeded row).
	resultDigest [32]byte
	summary      string
	now          time.Time
	// fromState is the operation state the success close requires (the
	// UPDATE's WHERE state=$). Today's caller passes switch_authorized; the
	// operator-declared "activated" resolution passes reconciliation_required.
	// Anything else is a programming error (validateFinalizeFromState).
	fromState string
}

// validateFinalizeFromState rejects a fromState that could never match a row
// the success close is allowed to start from. Without it a zero value would
// match no row and surface as ErrAgentLeaseStale, which tx2 treats as
// non-transient and would turn a genuine success report into a
// reconciliation block.
func validateFinalizeFromState(fromState string) error {
	switch fromState {
	case "switch_authorized", "reconciliation_required":
		return nil
	}
	return fmt.Errorf("finalizeActivationSucceeded: programming error: fromState %q must be switch_authorized or reconciliation_required", fromState)
}

// finalizeActivationSucceeded is the activation success write sequence, ADR
// 0004 lock order from the deployment onward: deployment (position 6, via
// transitionDeployment) -> route head (6b) -> release rows by id ascending,
// then the writes. The caller must already hold the credential (SHARE),
// environment slot, job and attempt locks and have verified the operation is
// in in.fromState for this attempt/fence. Because it IS the completion
// transaction for a successful activation (ADR 0004 "Transaction
// boundaries": completion, slot release, deployment transition and events
// commit atomically), it also closes the attempt, frees the slot and closes
// the job; Complete is never called afterwards. It does not commit.
func (r *AgentLeaseRepository) finalizeActivationSucceeded(ctx context.Context, tx pgx.Tx, in activationFinalizeInput) error {
	if err := validateFinalizeFromState(in.fromState); err != nil {
		return err
	}
	if err := transitionDeployment(ctx, tx, r.deployments, in.deploymentID, deployment.StatusHealthy, in.now, r.inject); err != nil {
		return err
	}
	if err := r.inject("deployment"); err != nil {
		return err
	}

	var routeRevision int64
	var activeReleaseID *string
	if err := tx.QueryRow(ctx, `SELECT route_revision, active_release_id FROM route_heads WHERE environment_id=$1 FOR UPDATE`, in.environmentID).Scan(&routeRevision, &activeReleaseID); err != nil {
		// Currently unreachable, kept as defence in depth: the caller
		// (reportActivationResult) already looked the route head up FOR UPDATE
		// before the savepoint and routes a missing head through
		// refuseAmbiguous, so by the time this runs the row exists and is
		// locked by this transaction.
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: environment %s has no route head at activation result", ErrAgentLeaseCorruptState, in.environmentID)
		}
		return fmt.Errorf("lock route head for activation result: %w", err)
	}
	sameActive := (activeReleaseID == nil && in.expectedActiveReleaseID == nil) ||
		(activeReleaseID != nil && in.expectedActiveReleaseID != nil && *activeReleaseID == *in.expectedActiveReleaseID)
	if routeRevision != in.expectedRouteRevision || !sameActive {
		return ErrAgentLeaseActivationRouteMoved
	}

	// Release rows, ascending id (ADR 0004 position 7 rule for multiple rows
	// of one class). tx2 is the writer, so FOR UPDATE rather than FOR SHARE;
	// 000018's deferred FOR SHARE trigger stays a redundant second line.
	lockIDs := []string{in.candidateReleaseID}
	if in.expectedActiveReleaseID != nil {
		lockIDs = append(lockIDs, *in.expectedActiveReleaseID)
	}
	rows, err := tx.Query(ctx, `SELECT id, state FROM releases WHERE id = ANY($1) ORDER BY id FOR UPDATE`, lockIDs)
	if err != nil {
		return fmt.Errorf("lock releases for activation result: %w", err)
	}
	states := make(map[string]string, len(lockIDs))
	for rows.Next() {
		var id, state string
		if err = rows.Scan(&id, &state); err != nil {
			rows.Close()
			return fmt.Errorf("scan release for activation result: %w", err)
		}
		states[id] = state
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return fmt.Errorf("lock releases for activation result: %w", err)
	}
	if states[in.candidateReleaseID] != "ready" {
		return fmt.Errorf("%w: candidate release %s is %q at activation result", ErrAgentLeaseCorruptState, in.candidateReleaseID, states[in.candidateReleaseID])
	}
	if in.expectedActiveReleaseID != nil {
		if states[*in.expectedActiveReleaseID] != "active" {
			return fmt.Errorf("%w: predecessor release %s is %q at activation result", ErrAgentLeaseCorruptState, *in.expectedActiveReleaseID, states[*in.expectedActiveReleaseID])
		}
		// Predecessor first: at most one active release per environment is
		// enforced immediately, so it must leave 'active' before the
		// candidate enters it. Skipped entirely on a first deployment.
		if _, err = tx.Exec(ctx, `UPDATE releases SET state='retained', updated_at=$2 WHERE id=$1 AND state='active'`, *in.expectedActiveReleaseID, in.now); err != nil {
			return fmt.Errorf("retain predecessor release: %w", err)
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE releases SET state='active', updated_at=$2 WHERE id=$1 AND state='ready'`, in.candidateReleaseID, in.now); err != nil {
		return fmt.Errorf("activate candidate release: %w", err)
	}
	if err = r.inject("release_active"); err != nil {
		return err
	}

	// Pointer and revision move in ONE update; validate_route_head_update
	// enforces CAS-by-exactly-one.
	tag, err := tx.Exec(ctx, `UPDATE route_heads SET active_release_id=$2, route_revision=route_revision+1, updated_at=GREATEST(updated_at, $3) WHERE environment_id=$1 AND route_revision=$4`,
		in.environmentID, in.candidateReleaseID, in.now, in.expectedRouteRevision)
	if err != nil {
		return fmt.Errorf("advance route head: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrAgentLeaseActivationRouteMoved
	}
	if err = insertRouteEvent(ctx, tx, in.environmentID, "activated", in.expectedRouteRevision+1, nil, &in.candidateReleaseID); err != nil {
		return err
	}
	if err = r.inject("route_head"); err != nil {
		return err
	}

	tag, err = tx.Exec(ctx, `UPDATE release_operations SET state='succeeded', closed_at=$2, outcome_summary=$3 WHERE id=$1 AND state=$4`, in.operationID, in.now, in.summary, in.fromState)
	if err != nil {
		return fmt.Errorf("close activation operation: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrAgentLeaseStale
	}
	if err = r.inject("operation_close"); err != nil {
		return err
	}

	// Completion, mirroring Complete's success branch: attempt, slot, job.
	if _, err = tx.Exec(ctx, `UPDATE agent_job_attempts SET state='succeeded', closed_at=$2, failure_class=NULL, result_operation_id=$3, result_digest=$4 WHERE id=$1`,
		in.attemptID, in.now, strconv.FormatInt(in.operationID, 10), in.resultDigest[:]); err != nil {
		return fmt.Errorf("close agent job attempt: %w", err)
	}
	if err = r.inject("attempt_close"); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE environment_slots SET current_attempt_id=NULL, updated_at=$2 WHERE environment_id=$1 AND current_attempt_id=$3`, in.environmentID, in.now, in.attemptID); err != nil {
		return fmt.Errorf("free environment slot: %w", err)
	}
	if err = r.inject("slot_free"); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE agent_jobs SET state='succeeded', updated_at=$2 WHERE id=$1 AND state='leased'`, in.jobID, in.now); err != nil {
		return fmt.Errorf("close agent job: %w", err)
	}
	if err = r.inject("job_close"); err != nil {
		return err
	}
	if err = appendAgentJobEvent(ctx, tx, in.jobID, &in.attemptID, "activated"); err != nil {
		return err
	}
	if err = appendAgentJobEvent(ctx, tx, in.jobID, &in.attemptID, "succeeded"); err != nil {
		return err
	}
	return r.inject("job_event")
}

// ErrAgentLeaseConstraintViolation reports a database error that no caller
// anticipated and that is not on the known-transient list
// (isRetryableActivationPgError): any PgError outside serialization,
// deadlock, cancellation, connection, shutdown, resource, read-only/failover,
// timeout and stale-statement-cache conditions, e.g. an integrity violation
// (class 23), a trigger exception (P0001) or a deferred trigger failing at
// COMMIT. It is treated as deterministic, so retrying cannot help; it exists
// so such an error is never reported as a retryable 503.
var ErrAgentLeaseConstraintViolation = errors.New("agent lease unexpected integrity violation")

// emptyFailureSummary replaces a blank failure summary so the audit row is
// never empty. release_operations.outcome_summary's own CHECK (live schema:
// octet_length <= 2048 AND no [\x00-\x1f\x7f]) has no minimum length.
const emptyFailureSummary = "activation failure reported without detail"

func failureOutcomeSummary(raw string) string {
	if summary := sanitizeOutcomeSummary(raw); summary != "" {
		return summary
	}
	return emptyFailureSummary
}

// ErrActivationBlockUnrecorded marks an error from writing (or committing) a
// reconciliation block that could not be made durable. It is never terminal:
// the agent must keep reporting until the block exists, so
// classifyActivationResultError returns it untouched (retryable) even when the
// wrapped error is a deterministic PgError. The original finalize failure that
// made the block necessary is carried as text only, never wrapped.
var ErrActivationBlockUnrecorded = errors.New("activation reconciliation block not recorded")

func blockUnrecorded(err error) error {
	return fmt.Errorf("%w: %w", ErrActivationBlockUnrecorded, err)
}

// classifyActivationResultError turns deterministic database/domain errors
// into typed, non-retryable sentinels. Known-transient failures (see
// isRetryableActivationPgError) and failures to record a block
// (ErrActivationBlockUnrecorded) stay untouched and remain retryable.
func classifyActivationResultError(err error) error {
	if err == nil || errors.Is(err, ErrActivationBlockUnrecorded) {
		return err
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && !isRetryableActivationPgError(pgErr) {
		return fmt.Errorf("%w: sqlstate %s constraint %q: %w", ErrAgentLeaseConstraintViolation, pgErr.Code, pgErr.ConstraintName, err)
	}
	if isDeploymentDomainError(err) {
		return fmt.Errorf("%w: %w", ErrAgentLeaseCorruptState, err)
	}
	return err
}

// isDeploymentDomainError reports a deterministic refusal from the
// deployment aggregate (an unsupported or post-terminal transition, an
// out-of-order timestamp, an unreadable snapshot) or from its repository
// (ErrRevisionConflict, ErrNotFound, ErrRevisionOverflow): a retry reproduces
// it. Inside tx2 the deployment row is already locked, so the repository
// sentinels cannot come from concurrency and indicate corruption.
func isDeploymentDomainError(err error) bool {
	return errors.Is(err, deployment.ErrInvalidTransition) ||
		errors.Is(err, deployment.ErrTerminalState) ||
		errors.Is(err, deployment.ErrInvalidTimestamp) ||
		errors.Is(err, deployment.ErrUnknownStatus) ||
		errors.Is(err, deployment.ErrInvalidSnapshot) ||
		errors.Is(err, deployment.ErrInvalidDeployment) ||
		errors.Is(err, ErrRevisionConflict) ||
		errors.Is(err, ErrNotFound) ||
		errors.Is(err, ErrRevisionOverflow)
}

// isRetryableActivationPgCode reports SQLSTATEs that describe a transient
// condition rather than a property of the data: serialization failure
// (40001), deadlock (40P01), statement-completion-unknown (40003), query
// cancellation (57014), the connection class (08xxx), server shutdown
// (57P01-57P03), session/transaction timeouts that kill the session (57P05,
// 25P03, 25P04), insufficient resources (53xxx), lock-not-available (55P03),
// read-only transaction (25006: failover or standby promotion, where every
// write including a block fails), io_error (58030) and
// invalid_sql_statement_name (26000: a cached prepared statement gone after a
// pooler reset) and internal_error (XX000: "cache lookup failed" / "could not
// open relation" around concurrent DDL; it is a catch-all, so a genuinely
// deterministic XX000 would retry instead of block -- the agent keeps
// reporting rather than the environment being spuriously blocked).
func isRetryableActivationPgCode(code string) bool {
	switch code {
	case "XX000", "40001", "40P01", "40003", "57014", "57P01", "57P02", "57P03", "57P05",
		"55P03", "25006", "25P03", "25P04", "58030", "26000":
		return true
	}
	return strings.HasPrefix(code, "08") || strings.HasPrefix(code, "53")
}

// isRetryableActivationPgError is isRetryableActivationPgCode plus the one
// code too broad to list bare: 0A000 (feature_not_supported) is transient
// only for the pgx statement cache's "cached plan must not change result
// type", which a migration (this slice ships one) or a pooler reset triggers
// until the cached statement is discarded.
func isRetryableActivationPgError(pgErr *pgconn.PgError) bool {
	if isRetryableActivationPgCode(pgErr.Code) {
		return true
	}
	// Matched on the server source routine, not the message text, so a
	// non-English lc_messages cannot make this spuriously terminal.
	return pgErr.Code == "0A000" && pgErr.Routine == "RevalidateCachedQuery"
}

// isTransientActivationFailure is the ONLY list that must be right: an error
// known to be transient, which rolls back and is retried. Context
// cancellation/deadline, transient SQLSTATEs and network-level failures
// (net.Error, EOF) qualify. A network failure on COMMIT has an unknown
// outcome, so it must stay retryable: the replay path answers it.
func isTransientActivationFailure(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, pgconn.ErrConnClosed) {
		return true
	}
	// puddle.ErrClosedPool is deliberately NOT listed, to avoid a direct
	// dependency on pgx's pool internals for no behavioural gain: a closed
	// pool already ends retryable without it. Treated as deterministic, the
	// block is attempted on another pooled connection, that acquire fails on
	// the same closed pool, and the failure is wrapped as
	// ErrActivationBlockUnrecorded -- which is retryable. Same end state.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return isRetryableActivationPgError(pgErr)
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

// isDeterministicActivationFailure decides whether a tx2 finalize error must
// be recorded as a durable reconciliation block instead of rolled back. It is
// a NEGATIVE definition: every non-nil error that is not known to be
// transient is treated as deterministic, so an unrecognized error after the
// occupant check blocks (visible to an operator) rather than retrying forever
// with the operation silently open while the host has already switched.
//
// The cost is stated plainly: this is NOT self-correcting. The block is
// written on a DIFFERENT pooled connection (abortAndBlock, the commit-failure
// paths and recordBlockInFreshTransaction), so a transient error that was
// misjudged on one connection does not stop the block from succeeding -- it
// SPURIOUSLY blocks an environment, which an operator must then clear.
// Blocking by default is still the right direction because losing the record
// of a switch that may have happened is worse than a spurious block, but the
// transient list above should be kept broad to keep that cost rare.
func isDeterministicActivationFailure(err error) bool {
	return err != nil && !isTransientActivationFailure(err)
}

// ReportActivationResult is tx2 of Step 10c's fenced activation: the agent
// reports whether the host switch authorized by Activate succeeded. Lock
// order as shipped (Activate shares the credential -> slot -> job -> attempt
// prefix and the deployment/route head/release suffix, but INSERTs the
// operation row instead of locking it): credential (SHARE, ADR position 1)
// -> environment slot (3) -> job (4) -> attempt (5) -> the activation
// release_operations row -> deployment (6) -> route head (6b) -> release rows
// by id ascending (6b) -> inserts (8). The release_operations row is a lock
// class ADR 0004 did not originally name; it is taken after the attempt and
// before the deployment, and the ADR's 2026-09-22 amendment records that
// position. Every one of those locks is held to commit, so two calls for one operation (or a call racing Complete) cannot
// interleave. The tests prove that each caller blocks on the environment_slots
// row when it is held and that the committed outcome is consistent; they do
// not prove which single row lock decides a race (any of slot, job, attempt
// or operation would suffice), so none is claimed here. The conditional
// UPDATE on the operation's state is defense in depth.
//
// DELIBERATELY NO authority_expires_at.After(now) CHECK, unlike every other
// lease endpoint (Renew/Complete/Activate). The agent may be mid host
// syscall when its lease lapses, and ADR 0005 says such a syscall cannot be
// preempted: the linearization point is the operation row still being
// switch_authorized for this exact attempt/fence (identity columns immutable
// per validate_release_operation_update), not the lease clock. Copying
// Complete's stale check here would reject a legitimately authorized
// activation whose lease lapsed mid-syscall -- the very case ADR 0005
// protects -- and strand the environment. The attempt must still be the
// leased occupant of the slot at this fence.
//
// On success it finalizes the activation and completes the attempt in one
// transaction (finalizeActivationSucceeded). On failure/ambiguity it moves
// the operation to reconciliation_required and blocks the environment
// (route head and slot flagged) without closing the attempt or job, freeing
// the slot, advancing the fence or touching the deployment -- ADR 0005
// forbids resolving the ambiguity by blind rollback. A success report that
// disagrees with what was authorized (the observed route digest differs from
// the one rendered in tx1, or the route head moved since) is itself
// ambiguous: the host may have switched to something unauthorized, so it is
// recorded durably the same way (evidence in outcome_summary) and the typed
// refusal is still returned. Replays: an identical success report for a
// succeeded operation, and an identical failure report (same sanitized
// summary) for a reconciliation_required one, are idempotent successes; any
// other disagreement is ErrAgentLeaseIntegrityConflict.
//
// RULE: once a success report has passed the fence, occupant and authority
// checks, ANY deterministic failure to finalize is recorded as the same
// durable reconciliation block (evidence = the specific discrepancy) and the
// typed non-retryable error is still returned; only failures known to be
// transient (isTransientActivationFailure) roll back and are retried -- the
// definition of "deterministic" is negative, so an unrecognized error blocks.
// A failure raised inside the transaction after the savepoint rolls back to
// it (taken after the deployment and route head locks are held; the locks
// survive) and commits the block there. A failure raised before the savepoint
// exists, or AT COMMIT (a deferred trigger), has already discarded the
// transaction, so the block is written in a fresh one
// (recordBlockInFreshTransaction) that re-takes the same lock order minus the
// credential. If a block cannot be made durable the error returned is always
// retryable (ErrActivationBlockUnrecorded), never the terminal typed error.
//
// Classification is POSITIONAL: only errors raised after the occupant check
// (a block can exist from there on) are passed through
// classifyActivationResultError. An error from the earlier read/lock
// statements (peek, slot, job, attempt, operation) wrote nothing durable, so
// even a non-transient PgError there (class-42 schema skew mid rolling
// deploy, 42501, ...) stays unclassified and retryable: a 503 can succeed on
// retry, a terminal 409 tells the agent to stop with nothing recorded.
func (r *AgentLeaseRepository) ReportActivationResult(ctx context.Context, serverID string, presentedDigest [32]byte, attemptID, fence int64, operationID string, succeeded bool, observedRouteDigest [32]byte, failureSummary string) error {
	var occupantChecked bool
	err := r.reportActivationResult(ctx, serverID, presentedDigest, attemptID, fence, operationID, succeeded, observedRouteDigest, failureSummary, &occupantChecked)
	if !occupantChecked {
		return err
	}
	return classifyActivationResultError(err)
}

func (r *AgentLeaseRepository) reportActivationResult(ctx context.Context, serverID string, presentedDigest [32]byte, attemptID, fence int64, operationID string, succeeded bool, observedRouteDigest [32]byte, failureSummary string, occupantChecked *bool) error {
	if r == nil || r.pool == nil || !github.ValidOpaqueID(serverID) || attemptID <= 0 || fence <= 0 || !validOperationID(operationID) {
		return ErrAgentLeaseInvalidInput
	}
	parsedOperationID, err := strconv.ParseInt(operationID, 10, 64)
	if err != nil || parsedOperationID <= 0 {
		return ErrAgentLeaseInvalidInput
	}

	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin agent lease activation result: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err = lockActiveServerCredential(ctx, tx, serverID, presentedDigest); err != nil {
		return err
	}

	var jobID int64
	var environmentID, ownerServerID string
	if err = tx.QueryRow(ctx, `SELECT job_id, environment_id, server_id FROM agent_job_attempts WHERE id=$1`, attemptID).Scan(&jobID, &environmentID, &ownerServerID); err == nil {
		err = r.inject("attempt_peek")
	}
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrServerAuthorizationDenied
		}
		return fmt.Errorf("locate agent job attempt: %w", err)
	}
	if ownerServerID != serverID {
		return ErrServerAuthorizationDenied
	}

	var slotFence int64
	var slotCurrentAttemptID *int64
	if err = tx.QueryRow(ctx, `SELECT fence, current_attempt_id FROM environment_slots WHERE environment_id=$1 FOR UPDATE`, environmentID).Scan(&slotFence, &slotCurrentAttemptID); err == nil {
		err = r.inject("slot_lock")
	}
	if err != nil {
		return fmt.Errorf("lock environment slot for activation result: %w", err)
	}

	var deploymentID string
	if err = tx.QueryRow(ctx, `SELECT deployment_id FROM agent_jobs WHERE id=$1 FOR UPDATE`, jobID).Scan(&deploymentID); err == nil {
		err = r.inject("job_lock")
	}
	if err != nil {
		return fmt.Errorf("lock agent job for activation result: %w", err)
	}

	var attemptState, attemptServerID string
	var attemptFence int64
	if err = tx.QueryRow(ctx, `SELECT state, server_id, fence FROM agent_job_attempts WHERE id=$1 FOR UPDATE`, attemptID).Scan(&attemptState, &attemptServerID, &attemptFence); err == nil {
		err = r.inject("attempt_lock")
	}
	if err != nil {
		return fmt.Errorf("lock agent job attempt for activation result: %w", err)
	}
	if attemptServerID != serverID {
		return ErrServerAuthorizationDenied
	}
	if attemptFence != fence {
		return ErrAgentLeaseStale
	}

	var op struct {
		id                      int64
		releaseID, state        string
		routeDigest             []byte
		outcomeSummary          string
		expectedRouteRevision   int64
		expectedActiveReleaseID *string
	}
	err = tx.QueryRow(ctx, `SELECT id, release_id, state, route_digest, outcome_summary, expected_route_revision, expected_active_release_id FROM release_operations WHERE attempt_id=$1 AND fence=$2 AND kind='activate' FOR UPDATE`, attemptID, fence).
		Scan(&op.id, &op.releaseID, &op.state, &op.routeDigest, &op.outcomeSummary, &op.expectedRouteRevision, &op.expectedActiveReleaseID)
	if err == nil {
		err = r.inject("operation_lock")
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAgentLeaseStale
	}
	if err != nil {
		return fmt.Errorf("lock activation operation: %w", err)
	}
	if op.id != parsedOperationID {
		return ErrAgentLeaseIntegrityConflict
	}
	if len(op.routeDigest) != 32 {
		return fmt.Errorf("%w: activation operation %d has a malformed route digest", ErrAgentLeaseCorruptState, op.id)
	}
	digestMatches := [32]byte(op.routeDigest) == observedRouteDigest
	summary := failureOutcomeSummary(failureSummary)

	switch op.state {
	case "succeeded":
		// Replay: the original call already finalized everything. Accept
		// only an identical success report.
		if !succeeded || !digestMatches {
			return ErrAgentLeaseIntegrityConflict
		}
		if err = tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit agent lease activation result replay: %w", err)
		}
		return nil
	case "reconciliation_required":
		// Already blocked (by an earlier report or, in 10c-2b, the expiry
		// sweeper). An identical failure report is an idempotent replay so
		// an agent whose response was lost can tell "already recorded" from
		// "lost lease". Nothing is finalized here.
		// 10c-2b insertion point: a late genuine report (especially a
		// success report) should be recorded as reconciliation evidence
		// (outcome_summary + event) instead of being refused.
		if succeeded {
			return ErrAgentLeaseStale
		}
		if summary != op.outcomeSummary {
			return ErrAgentLeaseIntegrityConflict
		}
		if err = tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit agent lease activation result failure replay: %w", err)
		}
		return nil
	case "switch_authorized":
	default:
		return fmt.Errorf("%w: activation operation %d in state %q", ErrAgentLeaseCorruptState, op.id, op.state)
	}

	// Open operation: the attempt must still be the leased occupant of the
	// slot at this fence. Authority expiry is intentionally not consulted
	// (see the doc comment above).
	if attemptState != "leased" || slotFence != fence || slotCurrentAttemptID == nil || *slotCurrentAttemptID != attemptID {
		return ErrAgentLeaseStale
	}
	// From here a block can be recorded, so every error is classified
	// terminally (see ReportActivationResult).
	*occupantChecked = true

	// abortAndBlock handles an error raised before the savepoint exists (the
	// deployment and route head locks, the SAVEPOINT itself). A known-transient
	// error is returned for the agent to retry. Anything else is deterministic
	// and, because a success report has passed the fence/occupant checks, must
	// still be recorded: this transaction is rolled back FIRST (a fresh one
	// re-takes the slot lock and would otherwise wait on this connection
	// forever) and the block is written in a fresh transaction.
	abortAndBlock := func(err error) error {
		if !isDeterministicActivationFailure(err) {
			return err
		}
		_ = tx.Rollback(ctx)
		return r.blockAfterAbortedTransaction(ctx, serverID, attemptID, fence, op.id, "activation finalize failed: ", err)
	}

	// refuseAmbiguous records a success report that disagrees with what was
	// authorized as a durable reconciliation block, commits it, then returns
	// the typed refusal: rolling back would leave the operation open with no
	// flag and no event while the host may already have switched. If the block
	// cannot be written or committed the failure is retryable
	// (blockUnrecorded) -- never the terminal refusal, since nothing durable
	// would exist -- except that a deterministic COMMIT error falls back to a
	// fresh transaction.
	refuseAmbiguous := func(evidence string, refusal error) error {
		evidence = sanitizeOutcomeSummary(evidence)
		if err := r.blockActivationForReconciliation(ctx, tx, jobID, attemptID, op.id, environmentID, evidence); err != nil {
			return blockUnrecorded(err)
		}
		if err := tx.Commit(ctx); err != nil {
			commitErr := fmt.Errorf("commit agent lease activation ambiguity block: %w", err)
			if !isDeterministicActivationFailure(err) {
				return commitErr
			}
			if _, freshErr := r.recordBlockInFreshTransaction(ctx, serverID, attemptID, fence, op.id, evidence); freshErr != nil {
				return fmt.Errorf("%w (after %v)", freshErr, commitErr)
			}
		}
		return refusal
	}

	if !succeeded {
		if err = r.blockActivationForReconciliation(ctx, tx, jobID, attemptID, op.id, environmentID, summary); err != nil {
			return blockUnrecorded(err)
		}
		if err = tx.Commit(ctx); err != nil {
			commitErr := fmt.Errorf("commit agent lease activation result: %w", err)
			if !isDeterministicActivationFailure(err) {
				return commitErr
			}
			outcome, freshErr := r.recordBlockInFreshTransaction(ctx, serverID, attemptID, fence, op.id, summary)
			if freshErr != nil {
				return fmt.Errorf("%w (after %v)", freshErr, commitErr)
			}
			switch outcome {
			case activationBlockRecorded:
				return nil
			case activationBlockNoopSucceeded:
				return ErrAgentLeaseIntegrityConflict
			default:
				return ErrAgentLeaseStale
			}
		}
		return nil
	}

	if !digestMatches {
		return refuseAmbiguous(fmt.Sprintf("route digest mismatch: expected sha256:%s observed sha256:%s", hex.EncodeToString(op.routeDigest), hex.EncodeToString(observedRouteDigest[:])), ErrAgentLeaseIntegrityConflict)
	}

	// Deployment (position 6) then route head (6b), held to commit. What the
	// tests PROVE: the slot, job, attempt and operation locks taken above
	// serialize every tx2/Complete caller (TestReportActivationResultGenuinely
	// Races*), which is what makes the route recheck below reliable against
	// the slot-holding paths. What they do NOT prove: that the deployment and
	// route head locks make the recheck authoritative against a writer that
	// takes neither the slot lock nor these -- the only such writer is the
	// admin AssignRouteHostname path, and no test exercises that contention.
	// The recheck is believed sound (the route head is locked FOR UPDATE
	// before it is read) but that is reasoning, not a tested claim.
	_, err = tx.Exec(ctx, `SELECT 1 FROM deployments WHERE id=$1 FOR UPDATE`, deploymentID)
	if err == nil {
		err = r.inject("deployment_lock")
	}
	if err != nil {
		return abortAndBlock(fmt.Errorf("lock deployment for activation result: %w", err))
	}
	var routeRevision int64
	var activeReleaseID *string
	err = tx.QueryRow(ctx, `SELECT route_revision, active_release_id FROM route_heads WHERE environment_id=$1 FOR UPDATE`, environmentID).Scan(&routeRevision, &activeReleaseID)
	if err == nil {
		err = r.inject("route_head_lock")
	}
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			missing := fmt.Sprintf("environment %s has no route head at activation result", environmentID)
			return refuseAmbiguous(missing, fmt.Errorf("%w: %s", ErrAgentLeaseCorruptState, missing))
		}
		return abortAndBlock(fmt.Errorf("lock route head for activation result: %w", err))
	}
	if !sameOptionalString(activeReleaseID, op.expectedActiveReleaseID) || routeRevision != op.expectedRouteRevision {
		return refuseAmbiguous(fmt.Sprintf("route head moved since authorization: expected revision %d active %s, found revision %d active %s",
			op.expectedRouteRevision, optionalStringForSummary(op.expectedActiveReleaseID), routeRevision, optionalStringForSummary(activeReleaseID)), ErrAgentLeaseActivationRouteMoved)
	}

	// Savepoint after the deployment and route head locks are held: rolling
	// back to it releases only locks taken later (the release rows), never
	// these. A deterministic finalize failure rolls back to here and commits
	// the reconciliation block instead of the whole transaction vanishing.
	_, err = tx.Exec(ctx, `SAVEPOINT `+activationFinalizeSavepoint)
	if err == nil {
		err = r.inject("savepoint")
	}
	if err != nil {
		return abortAndBlock(fmt.Errorf("savepoint for activation finalize: %w", err))
	}
	// Clock read after the slot/job/attempt/operation/deployment/route-head
	// locks above. finalizeActivationSucceeded still takes the release row
	// locks (and releases.updated_at has a monotonic trigger) AFTER this read,
	// so a concurrent writer of those rows could in principle leave
	// updated_at ahead of `now`. Harmless today: nothing outside the
	// slot-holding paths writes releases.
	var now time.Time
	err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now)
	if err == nil {
		err = r.inject("finalize_clock")
	}
	if err != nil {
		return r.blockAfterFinalizeFailure(ctx, tx, serverID, jobID, attemptID, fence, op.id, environmentID, fmt.Errorf("agent lease activation result canonical time: %w", err))
	}
	if err = r.finalizeActivationSucceeded(ctx, tx, activationFinalizeInput{
		jobID:                   jobID,
		attemptID:               attemptID,
		operationID:             op.id,
		environmentID:           environmentID,
		deploymentID:            deploymentID,
		candidateReleaseID:      op.releaseID,
		expectedRouteRevision:   op.expectedRouteRevision,
		expectedActiveReleaseID: op.expectedActiveReleaseID,
		resultDigest:            observedRouteDigest,
		summary:                 "activated route_digest=sha256:" + hex.EncodeToString(observedRouteDigest[:]),
		now:                     now,
		fromState:               "switch_authorized",
	}); err != nil {
		return r.blockAfterFinalizeFailure(ctx, tx, serverID, jobID, attemptID, fence, op.id, environmentID, err)
	}
	if err = tx.Commit(ctx); err != nil {
		commitErr := fmt.Errorf("commit agent lease activation result: %w", err)
		if isDeterministicActivationFailure(err) {
			return r.blockAfterCommitFailure(ctx, serverID, attemptID, fence, op.id, commitErr)
		}
		return commitErr
	}
	return nil
}

const activationFinalizeSavepoint = "activation_finalize"

// blockAfterFinalizeFailure handles an error raised inside the transaction,
// after the savepoint, before COMMIT. A known-transient error is returned
// untouched (the caller's deferred rollback discards everything and the agent
// retries). Anything else rolls back to the savepoint (every lock taken
// before it survives), records the reconciliation block with the discrepancy
// as evidence, commits, and returns cause so the caller still sees the typed
// non-retryable error. If recording the block itself fails, nothing durable
// exists, so that failure is returned as a retryable error (cause kept as
// text only, so it can never be misclassified as the terminal error); a
// deterministic COMMIT error of the block transaction falls back to a fresh
// transaction.
func (r *AgentLeaseRepository) blockAfterFinalizeFailure(ctx context.Context, tx pgx.Tx, serverID string, jobID, attemptID, fence, operationID int64, environmentID string, cause error) error {
	if !isDeterministicActivationFailure(cause) {
		return cause
	}
	if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT `+activationFinalizeSavepoint); err != nil {
		return blockUnrecorded(fmt.Errorf("rollback to savepoint for reconciliation block failed: %w (after %v)", err, cause))
	}
	evidence := sanitizeOutcomeSummary("activation finalize failed: " + cause.Error())
	if err := r.blockActivationForReconciliation(ctx, tx, jobID, attemptID, operationID, environmentID, evidence); err != nil {
		return blockUnrecorded(fmt.Errorf("record activation reconciliation block after %v: %w", cause, err))
	}
	if err := tx.Commit(ctx); err != nil {
		if !isDeterministicActivationFailure(err) {
			return blockUnrecorded(fmt.Errorf("commit activation reconciliation block after %v: %w", cause, err))
		}
		return r.blockAfterAbortedTransaction(ctx, serverID, attemptID, fence, operationID, "activation finalize failed: ", cause)
	}
	return cause
}

// blockAfterCommitFailure handles a deterministic error raised by COMMIT
// itself (a deferred constraint trigger): that transaction is gone, so a
// fresh one writes the block (see recordBlockInFreshTransaction). Results:
// the block was recorded or this report has nothing to record -> cause (the
// typed non-retryable error); another writer already finalized this very
// operation as succeeded -> nil (this report's intent is satisfied, the
// idempotent answer); the fresh transaction failed for any reason ->
// that failure, retryable, with cause as text only.
func (r *AgentLeaseRepository) blockAfterCommitFailure(ctx context.Context, serverID string, attemptID, fence, operationID int64, cause error) error {
	return r.blockAfterAbortedTransaction(ctx, serverID, attemptID, fence, operationID, "activation finalize failed at commit: ", cause)
}

// blockAfterAbortedTransaction is blockAfterCommitFailure's body, shared with
// the failures that occur before the savepoint exists. evidencePrefix says
// where the failure happened.
func (r *AgentLeaseRepository) blockAfterAbortedTransaction(ctx context.Context, serverID string, attemptID, fence, operationID int64, evidencePrefix string, cause error) error {
	evidence := sanitizeOutcomeSummary(evidencePrefix + cause.Error())
	outcome, err := r.recordBlockInFreshTransaction(ctx, serverID, attemptID, fence, operationID, evidence)
	if err != nil {
		return fmt.Errorf("%w (after %v)", err, cause)
	}
	if outcome == activationBlockNoopSucceeded {
		return nil
	}
	return cause
}

type activationBlockOutcome int

const (
	// activationBlockRecorded: the block was written and committed.
	activationBlockRecorded activationBlockOutcome = iota
	// activationBlockNoopSucceeded: another writer already finalized this
	// operation as succeeded.
	activationBlockNoopSucceeded
	// activationBlockNoopOther: already blocked/closed by a concurrent
	// writer, or the attempt no longer occupies the slot at this fence.
	activationBlockNoopOther
)

// recordBlockInFreshTransaction writes the reconciliation block in a new
// transaction, re-taking the documented lock order minus the credential:
// environment slot -> job -> attempt -> operation -> route head (inside
// blockActivationForReconciliation). That is a strict subsequence of
// tx2's order (credential SHARE -> slot -> job -> attempt -> operation ->
// deployment -> route head -> releases), so it cannot introduce a deadlock.
//
// The credential is deliberately NOT revalidated: authentication already
// happened in tx1, and the block grants the agent nothing -- it only records
// that something ambiguous occurred so an operator looks. A block written for
// a since-revoked server is harmless; a lost one is not, so this must fail
// safe. Ownership (server_id on the attempt), occupant and operation-state
// checks stay. It is idempotent and tolerant: a no-op is a distinct outcome,
// not an error. EVERY failure of the fresh transaction is returned
// (blockUnrecorded, retryable), never swallowed, because nothing durable
// exists.
//
// INVARIANT: the `server_id=$2` predicate on the attempt lookup is defence in
// depth, and serverID MUST come from the authenticated request. It becomes a
// TAUTOLOGY (always true) if a future caller (10c-2b, e.g. the sweeper) passes
// a serverID read from the attempt row itself; such a caller must establish
// authority some other way.
func (r *AgentLeaseRepository) recordBlockInFreshTransaction(ctx context.Context, serverID string, attemptID, fence, operationID int64, evidence string) (activationBlockOutcome, error) {
	fail := func(what string, err error) (activationBlockOutcome, error) {
		return activationBlockNoopOther, blockUnrecorded(fmt.Errorf("%s: %w", what, err))
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fail("begin activation reconciliation block", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var jobID int64
	var environmentID string
	if err = tx.QueryRow(ctx, `SELECT job_id, environment_id FROM agent_job_attempts WHERE id=$1 AND server_id=$2`, attemptID, serverID).Scan(&jobID, &environmentID); err != nil {
		return fail("locate attempt for activation reconciliation block", err)
	}
	var slotFence int64
	var slotCurrentAttemptID *int64
	if err = tx.QueryRow(ctx, `SELECT fence, current_attempt_id FROM environment_slots WHERE environment_id=$1 FOR UPDATE`, environmentID).Scan(&slotFence, &slotCurrentAttemptID); err != nil {
		return fail("lock environment slot for activation reconciliation block", err)
	}
	if _, err = tx.Exec(ctx, `SELECT 1 FROM agent_jobs WHERE id=$1 FOR UPDATE`, jobID); err != nil {
		return fail("lock agent job for activation reconciliation block", err)
	}
	var attemptState string
	if err = tx.QueryRow(ctx, `SELECT state FROM agent_job_attempts WHERE id=$1 FOR UPDATE`, attemptID).Scan(&attemptState); err != nil {
		return fail("lock agent job attempt for activation reconciliation block", err)
	}
	var opState string
	if err = tx.QueryRow(ctx, `SELECT state FROM release_operations WHERE id=$1 AND attempt_id=$2 AND fence=$3 AND kind='activate' FOR UPDATE`, operationID, attemptID, fence).Scan(&opState); err != nil {
		return fail("lock activation operation for reconciliation block", err)
	}
	if opState == "succeeded" {
		return activationBlockNoopSucceeded, nil
	}
	if opState != "switch_authorized" || attemptState != "leased" || slotFence != fence || slotCurrentAttemptID == nil || *slotCurrentAttemptID != attemptID {
		// Already blocked by a concurrent writer, or the slot moved on:
		// nothing left for this report to record.
		return activationBlockNoopOther, nil
	}
	if err = r.blockActivationForReconciliation(ctx, tx, jobID, attemptID, operationID, environmentID, evidence); err != nil {
		return activationBlockNoopOther, blockUnrecorded(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return fail("commit activation reconciliation block", err)
	}
	return activationBlockRecorded, nil
}

func sameOptionalString(a, b *string) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func optionalStringForSummary(v *string) string {
	if v == nil {
		return "<none>"
	}
	return *v
}

// blockActivationForReconciliation is tx2's failure/ambiguous path. Only the
// operation, the two reconciliation flags and one event are written. The
// slot keeps current_attempt_id and its fence; the attempt, job and
// deployment are left exactly as they are, because the switch may have
// happened and ADR 0005 forbids resolving that by blind rollback or blind
// success. The route head's revision does not move (validate_route_head_update
// exempts a flag-only update, migration 000020): the desired route state did
// not change. The caller must hold the slot, job, attempt and operation locks.
func (r *AgentLeaseRepository) blockActivationForReconciliation(ctx context.Context, tx pgx.Tx, jobID, attemptID, operationID int64, environmentID, summary string) error {
	// Lock the route head (position 6b) and read its flag. The operation row
	// is already locked by the caller, so taking this lock first changes
	// nothing about lock ORDER; what it does is (1) serialize this block
	// with any concurrent route head writer before the clock is read, and
	// (2) let the flag write below be decided from the locked value. The
	// deployment row (position 6) is not written here (the success-ambiguity
	// caller may already hold it, which is harmless). A missing route head
	// is tolerated (the block must still be recorded on the operation and
	// slot; "no route head" is itself the evidence in that case).
	headExists, headFlagged := true, false
	if err := tx.QueryRow(ctx, `SELECT reconciliation_required FROM route_heads WHERE environment_id=$1 FOR UPDATE`, environmentID).Scan(&headFlagged); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("lock route head for activation reconciliation: %w", err)
		}
		headExists = false
	}
	// Clock read after the blocking locks so updated_at cannot trip the
	// route head's monotonicity rule against a concurrent writer; GREATEST
	// is the belt to that brace.
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return fmt.Errorf("agent lease activation block canonical time: %w", err)
	}
	tag, err := tx.Exec(ctx, `UPDATE release_operations SET state='reconciliation_required', closed_at=$2, outcome_summary=$3 WHERE id=$1 AND state='switch_authorized'`, operationID, now, summary)
	if err != nil {
		return fmt.Errorf("close activation operation for reconciliation: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrAgentLeaseStale
	}
	if err = r.inject("operation_close"); err != nil {
		return err
	}
	// Migration 000020 exempts a flag-only update from the revision bump only
	// if the flag actually CHANGES, so an already-flagged head must not be
	// updated at all (it would raise P0001). The row is locked above, so the
	// guard and the pre-read agree and exactly one row is affected whenever
	// the write is attempted.
	if headExists && !headFlagged {
		tag, err = tx.Exec(ctx, `UPDATE route_heads SET reconciliation_required=true, updated_at=GREATEST(updated_at, $2) WHERE environment_id=$1 AND NOT reconciliation_required`, environmentID, now)
		if err != nil {
			return fmt.Errorf("flag route head for reconciliation: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("%w: route head for environment %s vanished during activation reconciliation", ErrAgentLeaseCorruptState, environmentID)
		}
	}
	tag, err = tx.Exec(ctx, `UPDATE environment_slots SET reconciliation_required=true, reconciliation_attempt_id=$2, updated_at=$3 WHERE environment_id=$1`, environmentID, attemptID, now)
	if err != nil {
		return fmt.Errorf("flag environment slot for reconciliation: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: environment slot %s vanished during activation reconciliation", ErrAgentLeaseCorruptState, environmentID)
	}
	if err = r.inject("slot_reconcile"); err != nil {
		return err
	}
	if err = appendAgentJobEvent(ctx, tx, jobID, &attemptID, "activation_reconciliation_required"); err != nil {
		return err
	}
	return r.inject("job_event")
}

// RequestCancellation cancels jobID on behalf of an owner-scoped, live
// administrator session (ADR 0004 cancellation). Queued/retry-wait
// cancellation is immediate. Leased cancellation is cooperative: it sets
// cancel_requested_at and caps the current attempt's lease/authority
// deadlines at now+60s in the same transaction (Step 3's carry-over fix --
// previously these could be set in separate transactions, letting a renewal
// race between them observe an uncapped deadline), and is rejected outright
// if the deployment already committed the activating linearization point.
// Requesting cancellation twice, or against an already-cancelled job, is
// idempotent; requesting it against any other terminal job is an error.
func (r *AgentLeaseRepository) RequestCancellation(ctx context.Context, principal auth.Principal, jobID int64) error {
	if r == nil || r.pool == nil || principal.Validate() != nil || jobID <= 0 {
		return ErrServerAuthorizationDenied
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin agent job cancellation request: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if err = lockAdministratorSession(ctx, tx, principal); err != nil {
		return err
	}

	// Non-locking peek at the job's environment purely so the slot can be
	// locked before the job below, per the ADR hierarchy, even on the
	// immediate queued/retry_wait path that never touches an attempt. A
	// never-claimed environment has no slot row yet; locking zero rows is a
	// harmless no-op.
	var peekEnvironmentID string
	if err = tx.QueryRow(ctx, `SELECT environment_id FROM agent_jobs WHERE id=$1`, jobID).Scan(&peekEnvironmentID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrServerAuthorizationDenied
		}
		return fmt.Errorf("locate agent job for cancellation: %w", err)
	}
	if _, err = tx.Exec(ctx, `SELECT 1 FROM environment_slots WHERE environment_id=$1 FOR UPDATE`, peekEnvironmentID); err != nil {
		return fmt.Errorf("lock environment slot for cancellation: %w", err)
	}

	var ownerID, jobState, environmentID, deploymentID string
	var cancelRequestedAt *time.Time
	if err = tx.QueryRow(ctx, `SELECT owner_id, state, environment_id, deployment_id, cancel_requested_at FROM agent_jobs WHERE id=$1 FOR UPDATE`, jobID).
		Scan(&ownerID, &jobState, &environmentID, &deploymentID, &cancelRequestedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrServerAuthorizationDenied
		}
		return fmt.Errorf("lock agent job for cancellation: %w", err)
	}
	if ownerID != principal.OwnerID {
		return ErrServerAuthorizationDenied
	}

	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return fmt.Errorf("agent job cancellation canonical time: %w", err)
	}

	switch jobState {
	case "cancelled":
		if err = tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit agent job cancellation no-op: %w", err)
		}
		return nil
	case "succeeded", "failed", "dead":
		return ErrAgentJobTerminal
	case "queued", "retry_wait":
		if _, err = tx.Exec(ctx, `UPDATE agent_jobs SET state='cancelled', cancel_requested_at=$2, updated_at=$2 WHERE id=$1`, jobID, now); err != nil {
			return fmt.Errorf("cancel agent job: %w", err)
		}
		if err = r.inject("job_cancel"); err != nil {
			return err
		}
		if err = transitionDeployment(ctx, tx, r.deployments, deploymentID, deployment.StatusCancelled, now, r.inject); err != nil {
			return err
		}
		if err = r.inject("deployment"); err != nil {
			return err
		}
		if err = appendAgentJobEvent(ctx, tx, jobID, nil, "cancelled"); err != nil {
			return err
		}
		if err = r.inject("job_event"); err != nil {
			return err
		}
		if err = tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit agent job cancellation: %w", err)
		}
		return nil
	case "leased":
		if cancelRequestedAt != nil {
			// Idempotent: the first request already capped the attempt's
			// deadlines; a second request mutates nothing further.
			if err = tx.Commit(ctx); err != nil {
				return fmt.Errorf("commit agent job cancellation no-op: %w", err)
			}
			return nil
		}
		var attemptID int64
		if err = tx.QueryRow(ctx, `SELECT id FROM agent_job_attempts WHERE job_id=$1 AND state='leased' FOR UPDATE`, jobID).Scan(&attemptID); err != nil {
			return fmt.Errorf("lock leased agent job attempt for cancellation: %w", err)
		}
		var deploymentStatus string
		if err = tx.QueryRow(ctx, `SELECT status FROM deployments WHERE id=$1 FOR UPDATE`, deploymentID).Scan(&deploymentStatus); err != nil {
			return fmt.Errorf("lock deployment for cancellation: %w", err)
		}
		if deploymentStatus == string(deployment.StatusActivating) {
			return ErrAgentJobActivating
		}
		deadline := now.Add(cancellationGrace)
		if _, err = tx.Exec(ctx, `UPDATE agent_jobs SET cancel_requested_at=$2, updated_at=$2 WHERE id=$1`, jobID, now); err != nil {
			return fmt.Errorf("mark agent job cancel requested: %w", err)
		}
		if err = r.inject("job_cancel_request"); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE agent_job_attempts SET cancel_deadline_at=$2, lease_expires_at=LEAST(lease_expires_at,$2), authority_expires_at=LEAST(authority_expires_at,$2) WHERE id=$1`, attemptID, deadline); err != nil {
			return fmt.Errorf("cap agent job attempt deadlines: %w", err)
		}
		if err = r.inject("attempt_cancel_cap"); err != nil {
			return err
		}
		if err = appendAgentJobEvent(ctx, tx, jobID, &attemptID, "cancel_requested"); err != nil {
			return err
		}
		if err = r.inject("job_event"); err != nil {
			return err
		}
		if err = tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit agent job cancellation request: %w", err)
		}
		return nil
	default:
		return ErrServerAuthorizationDenied
	}
}

// AcknowledgeCancellation closes attemptID as cancelled once the agent
// reports its cleanup outcome (ADR 0004 cancellation step 4). It is
// replay-safe exactly like Complete: an identical replay against an
// already-closed attempt returns success without mutation, a mismatch is an
// integrity conflict, and a closed attempt with no stored result (e.g. the
// sweeper already closed it as cancelled/expired without ever recording an
// acknowledgement) is stale.
func (r *AgentLeaseRepository) AcknowledgeCancellation(ctx context.Context, serverID string, presentedDigest [32]byte, attemptID, fence int64, operationID string, resultDigest [32]byte) error {
	if r == nil || r.pool == nil || !github.ValidOpaqueID(serverID) || attemptID <= 0 || fence <= 0 || !validOperationID(operationID) {
		return ErrAgentLeaseInvalidInput
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin agent lease cancel acknowledge: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err = lockActiveServerCredential(ctx, tx, serverID, presentedDigest); err != nil {
		return err
	}

	var jobID int64
	var environmentID, ownerServerID string
	if err = tx.QueryRow(ctx, `SELECT job_id, environment_id, server_id FROM agent_job_attempts WHERE id=$1`, attemptID).Scan(&jobID, &environmentID, &ownerServerID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrServerAuthorizationDenied
		}
		return fmt.Errorf("locate agent job attempt: %w", err)
	}
	if ownerServerID != serverID {
		return ErrServerAuthorizationDenied
	}

	var slotFence int64
	var slotCurrentAttemptID *int64
	if err = tx.QueryRow(ctx, `SELECT fence, current_attempt_id FROM environment_slots WHERE environment_id=$1 FOR UPDATE`, environmentID).Scan(&slotFence, &slotCurrentAttemptID); err != nil {
		return fmt.Errorf("lock environment slot for cancel acknowledge: %w", err)
	}

	var deploymentID string
	var jobCancelRequestedAt *time.Time
	if err = tx.QueryRow(ctx, `SELECT deployment_id, cancel_requested_at FROM agent_jobs WHERE id=$1 FOR UPDATE`, jobID).Scan(&deploymentID, &jobCancelRequestedAt); err != nil {
		return fmt.Errorf("lock agent job for cancel acknowledge: %w", err)
	}

	var attemptState, attemptServerID string
	var attemptFence int64
	var authorityExpiresAt time.Time
	var storedOperationID *string
	var storedResultDigest []byte
	if err = tx.QueryRow(ctx, `SELECT state, server_id, fence, authority_expires_at, result_operation_id, result_digest FROM agent_job_attempts WHERE id=$1 FOR UPDATE`, attemptID).
		Scan(&attemptState, &attemptServerID, &attemptFence, &authorityExpiresAt, &storedOperationID, &storedResultDigest); err != nil {
		return fmt.Errorf("lock agent job attempt for cancel acknowledge: %w", err)
	}
	if attemptServerID != serverID {
		return ErrServerAuthorizationDenied
	}

	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return fmt.Errorf("agent lease cancel acknowledge canonical time: %w", err)
	}

	if attemptState != "leased" {
		if storedOperationID == nil {
			return ErrAgentLeaseStale
		}
		digestMatches := len(storedResultDigest) == 32 && [32]byte(storedResultDigest) == resultDigest
		if attemptState != "cancelled" || *storedOperationID != operationID || !digestMatches {
			return ErrAgentLeaseIntegrityConflict
		}
		if err = tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit agent lease cancel acknowledge replay: %w", err)
		}
		return nil
	}

	if jobCancelRequestedAt == nil {
		return ErrAgentLeaseInvalidInput
	}
	if attemptFence != fence || slotFence != fence || slotCurrentAttemptID == nil || *slotCurrentAttemptID != attemptID || !authorityExpiresAt.After(now) {
		return ErrAgentLeaseStale
	}

	if _, err = tx.Exec(ctx, `UPDATE agent_job_attempts SET state='cancelled', closed_at=$2, result_operation_id=$3, result_digest=$4 WHERE id=$1`,
		attemptID, now, operationID, resultDigest[:]); err != nil {
		return fmt.Errorf("close agent job attempt as cancelled: %w", err)
	}
	if err = r.inject("attempt_close"); err != nil {
		return err
	}

	if _, err = tx.Exec(ctx, `UPDATE environment_slots SET current_attempt_id=NULL, updated_at=$2 WHERE environment_id=$1 AND current_attempt_id=$3`, environmentID, now, attemptID); err != nil {
		return fmt.Errorf("free environment slot: %w", err)
	}
	if err = r.inject("slot_free"); err != nil {
		return err
	}

	if _, err = tx.Exec(ctx, `UPDATE agent_jobs SET state='cancelled', updated_at=$2 WHERE id=$1 AND state='leased'`, jobID, now); err != nil {
		return fmt.Errorf("cancel agent job: %w", err)
	}
	if err = r.inject("job_close"); err != nil {
		return err
	}

	if err = transitionDeployment(ctx, tx, r.deployments, deploymentID, deployment.StatusCancelled, now, r.inject); err != nil {
		return err
	}
	if err = r.inject("deployment"); err != nil {
		return err
	}

	if err = appendAgentJobEvent(ctx, tx, jobID, &attemptID, "cancelled"); err != nil {
		return err
	}
	if err = r.inject("job_event"); err != nil {
		return err
	}

	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit agent lease cancel acknowledge: %w", err)
	}
	return nil
}

// ExpireBatch recovers up to limit (capped at 100) authority-expired leases
// (ADR 0004 recovery): a non-locking ordered discovery followed by one
// short, independently committed transaction per candidate that locks
// slot -> job -> attempt with SKIP LOCKED and revalidates before closing
// anything. It returns the number of attempts actually closed; a candidate
// that a concurrent renewal, completion, or another sweeper run already
// resolved is silently skipped, not counted, and not an error.
func (r *AgentLeaseRepository) ExpireBatch(ctx context.Context, limit int) (int, error) {
	if r == nil || r.pool == nil || limit <= 0 {
		return 0, nil
	}
	if limit > expireBatchMaxLimit {
		limit = expireBatchMaxLimit
	}

	rows, err := r.pool.Query(ctx, `SELECT id FROM agent_job_attempts WHERE state='leased' AND authority_expires_at <= clock_timestamp() ORDER BY authority_expires_at, id LIMIT $1`, limit)
	if err != nil {
		return 0, fmt.Errorf("discover expired agent job attempts: %w", err)
	}
	var candidates []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan expired agent job attempt: %w", err)
		}
		candidates = append(candidates, id)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("discover expired agent job attempts: %w", err)
	}
	rows.Close()

	processed := 0
	for _, attemptID := range candidates {
		ok, err := r.expireOne(ctx, attemptID)
		if err != nil {
			return processed, err
		}
		if ok {
			processed++
		}
	}
	return processed, nil
}

// expireOne is ExpireBatch's per-candidate short transaction.
func (r *AgentLeaseRepository) expireOne(ctx context.Context, attemptID int64) (bool, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, fmt.Errorf("begin agent lease expiry: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var jobID int64
	var environmentID string
	if err = tx.QueryRow(ctx, `SELECT job_id, environment_id FROM agent_job_attempts WHERE id=$1`, attemptID).Scan(&jobID, &environmentID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("locate expiring agent job attempt: %w", err)
	}

	var slotCurrentAttemptID *int64
	err = tx.QueryRow(ctx, `SELECT current_attempt_id FROM environment_slots WHERE environment_id=$1 FOR UPDATE SKIP LOCKED`, environmentID).Scan(&slotCurrentAttemptID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock environment slot for expiry: %w", err)
	}
	if slotCurrentAttemptID == nil || *slotCurrentAttemptID != attemptID {
		return false, nil
	}

	var jobState, deploymentID string
	var jobCancelRequestedAt *time.Time
	err = tx.QueryRow(ctx, `SELECT state, deployment_id, cancel_requested_at FROM agent_jobs WHERE id=$1 FOR UPDATE SKIP LOCKED`, jobID).Scan(&jobState, &deploymentID, &jobCancelRequestedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock agent job for expiry: %w", err)
	}
	if jobState != "leased" {
		return false, nil
	}

	var attemptState string
	var attemptNumber int
	var authorityExpiresAt time.Time
	err = tx.QueryRow(ctx, `SELECT state, attempt_number, authority_expires_at FROM agent_job_attempts WHERE id=$1 FOR UPDATE SKIP LOCKED`, attemptID).Scan(&attemptState, &attemptNumber, &authorityExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock expiring agent job attempt: %w", err)
	}

	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return false, fmt.Errorf("agent lease expiry canonical time: %w", err)
	}
	if attemptState != "leased" || authorityExpiresAt.After(now) {
		return false, nil
	}

	cancelRequested := jobCancelRequestedAt != nil
	if err = closeExpiredOrRevokedAttempt(ctx, tx, r.deployments, r.inject, jobID, attemptID, environmentID, attemptNumber, deploymentID, cancelRequested, now); err != nil {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit agent lease expiry: %w", err)
	}
	return true, nil
}

// Reconcile clears environmentID's reconciliation flag once the owning
// server reports the closed reconciliation attempt's outcome (ADR 0004
// Decision 6). Only the server that holds attemptID, at the exact fence it
// closed with, may clear the flag it left behind; any other caller is
// denied generically. outcomeDigest is accepted as the sanitized-outcome
// input the wire protocol (Step 5) will carry, but this schema slice has no
// column to durably record it without a new migration -- reconciliation
// itself is proven by the attempt already being closed, and the outcome
// digest is validated shape-only here.
//
// Idempotency: once cleared, environment_slots.reconciliation_attempt_id no
// longer names this attempt, so a second call cannot re-check "is this still
// the pending attempt". Instead it is detected by the durable 'reconciled'
// job event this call appends the first time: if one already exists for
// this exact attempt, the call is an idempotent no-op; otherwise a caller
// naming an attempt that was never the pending one is denied.
func (r *AgentLeaseRepository) Reconcile(ctx context.Context, serverID string, presentedDigest [32]byte, environmentID string, attemptID, fence int64, outcomeDigest [32]byte) error {
	if r == nil || r.pool == nil || !github.ValidOpaqueID(serverID) || !github.ValidOpaqueID(environmentID) || attemptID <= 0 || fence <= 0 {
		return ErrAgentLeaseInvalidInput
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin agent lease reconcile: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err = lockActiveServerCredential(ctx, tx, serverID, presentedDigest); err != nil {
		return err
	}

	var slotReconciliationAttemptID *int64
	if err = tx.QueryRow(ctx, `SELECT reconciliation_attempt_id FROM environment_slots WHERE environment_id=$1 FOR UPDATE`, environmentID).Scan(&slotReconciliationAttemptID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrServerAuthorizationDenied
		}
		return fmt.Errorf("lock environment slot for reconcile: %w", err)
	}

	var attemptJobID int64
	var attemptServerID, attemptEnvironmentID string
	var attemptFence int64
	var attemptState string
	var closedAt *time.Time
	if err = tx.QueryRow(ctx, `SELECT job_id, server_id, environment_id, fence, state, closed_at FROM agent_job_attempts WHERE id=$1 FOR UPDATE`, attemptID).
		Scan(&attemptJobID, &attemptServerID, &attemptEnvironmentID, &attemptFence, &attemptState, &closedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrServerAuthorizationDenied
		}
		return fmt.Errorf("lock agent job attempt for reconcile: %w", err)
	}
	if attemptServerID != serverID || attemptEnvironmentID != environmentID || attemptFence != fence || attemptState == "leased" || closedAt == nil {
		return ErrServerAuthorizationDenied
	}

	if slotReconciliationAttemptID == nil || *slotReconciliationAttemptID != attemptID {
		var alreadyReconciled bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM agent_job_events WHERE job_id=$1 AND attempt_id=$2 AND event_type='reconciled')`, attemptJobID, attemptID).Scan(&alreadyReconciled); err != nil {
			return fmt.Errorf("check prior agent lease reconciliation: %w", err)
		}
		if !alreadyReconciled {
			return ErrServerAuthorizationDenied
		}
		if err = tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit agent lease reconcile no-op: %w", err)
		}
		return nil
	}

	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return fmt.Errorf("agent lease reconcile canonical time: %w", err)
	}
	if _, err = tx.Exec(ctx, `UPDATE environment_slots SET reconciliation_required=false, reconciliation_attempt_id=NULL, updated_at=$2 WHERE environment_id=$1`, environmentID, now); err != nil {
		return fmt.Errorf("clear environment slot reconciliation: %w", err)
	}
	if err = r.inject("slot_reconcile_clear"); err != nil {
		return err
	}
	if err = appendAgentJobEvent(ctx, tx, attemptJobID, &attemptID, "reconciled"); err != nil {
		return err
	}
	if err = r.inject("job_event"); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit agent lease reconcile: %w", err)
	}
	return nil
}

// ReconcilingEnvironment is one environment currently blocked on
// reconciliation, as the web admin /servers page renders it: the pending
// attempt and job it is waiting on, the server that must reconcile it (or be
// broken-glassed via MarkReconciled), and since (the slot's last update,
// i.e. when reconciliation became required).
type ReconcilingEnvironment struct {
	EnvironmentID string
	AttemptID     int64
	JobID         int64
	ServerID      string
	Since         time.Time
}

// ListReconcilingEnvironments returns every environment currently requiring
// reconciliation whose pending attempt's job belongs to principal.OwnerID --
// the same ownership derivation MarkReconciled uses, so a caller can never
// list (or later break-glass) an environment it could not already resolve.
func (r *AgentLeaseRepository) ListReconcilingEnvironments(ctx context.Context, principal auth.Principal) ([]ReconcilingEnvironment, error) {
	if r == nil || r.pool == nil || principal.Validate() != nil {
		return nil, ErrServerAuthorizationDenied
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("begin list reconciling environments: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if err = lockAdministratorSession(ctx, tx, principal); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT es.environment_id, a.id, a.job_id, a.server_id, es.updated_at FROM environment_slots es JOIN agent_job_attempts a ON a.id = es.reconciliation_attempt_id JOIN agent_jobs j ON j.id = a.job_id WHERE es.reconciliation_required AND j.owner_id=$1 ORDER BY es.updated_at ASC, es.environment_id`, principal.OwnerID)
	if err != nil {
		return nil, fmt.Errorf("query reconciling environments: %w", err)
	}
	defer rows.Close()
	var environments []ReconcilingEnvironment
	for rows.Next() {
		var e ReconcilingEnvironment
		if err = rows.Scan(&e.EnvironmentID, &e.AttemptID, &e.JobID, &e.ServerID, &e.Since); err != nil {
			return nil, fmt.Errorf("scan reconciling environment row: %w", err)
		}
		environments = append(environments, e)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate reconciling environments: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit list reconciling environments: %w", err)
	}
	return environments, nil
}

// MarkReconciled is the audited administrator break-glass path for a
// wedged environment (ADR 0004 Decision 6 owner decisions). environment_slots
// carries no owner column, so ownership is derived from the job that the
// pending reconciliation attempt belongs to; an environment with nothing
// pending, or whose pending attempt's job belongs to a different owner, is
// denied generically either way.
func (r *AgentLeaseRepository) MarkReconciled(ctx context.Context, principal auth.Principal, environmentID, reason string) error {
	if r == nil || r.pool == nil || principal.Validate() != nil || !github.ValidOpaqueID(environmentID) || !validReason(reason) {
		return ErrServerAuthorizationDenied
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin environment mark-reconciled: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if err = lockAdministratorSession(ctx, tx, principal); err != nil {
		return err
	}

	var reconciliationAttemptID *int64
	if err = tx.QueryRow(ctx, `SELECT reconciliation_attempt_id FROM environment_slots WHERE environment_id=$1 FOR UPDATE`, environmentID).Scan(&reconciliationAttemptID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrServerAuthorizationDenied
		}
		return fmt.Errorf("lock environment slot for mark-reconciled: %w", err)
	}
	if reconciliationAttemptID == nil {
		return ErrServerAuthorizationDenied
	}

	var attemptJobID int64
	var jobOwnerID string
	if err = tx.QueryRow(ctx, `SELECT a.job_id, j.owner_id FROM agent_job_attempts a JOIN agent_jobs j ON j.id = a.job_id WHERE a.id=$1`, *reconciliationAttemptID).Scan(&attemptJobID, &jobOwnerID); err != nil {
		return fmt.Errorf("locate agent lease reconciliation attempt job: %w", err)
	}
	if jobOwnerID != principal.OwnerID {
		return ErrServerAuthorizationDenied
	}

	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return fmt.Errorf("environment mark-reconciled canonical time: %w", err)
	}
	if _, err = tx.Exec(ctx, `UPDATE environment_slots SET reconciliation_required=false, reconciliation_attempt_id=NULL, updated_at=$2 WHERE environment_id=$1`, environmentID, now); err != nil {
		return fmt.Errorf("clear environment slot reconciliation: %w", err)
	}
	if err = r.inject("slot_reconcile_clear"); err != nil {
		return err
	}

	actor := administratorActor(principal)
	if err = insertServerAuditEvent(ctx, tx, actor, "environment_mark_reconciled", nil, nil, "success", reason); err != nil {
		return err
	}
	if err = r.inject("audit_insert"); err != nil {
		return err
	}

	if err = appendAgentJobEvent(ctx, tx, attemptJobID, reconciliationAttemptID, "reconciled"); err != nil {
		return err
	}
	if err = r.inject("job_event"); err != nil {
		return err
	}

	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit environment mark-reconciled: %w", err)
	}
	return nil
}

// revokeInFlightLeases closes every currently leased attempt of serverID
// inside RevokeServer's own transaction (ADR 0004's revocation paragraph):
// an already-executing host syscall cannot be preempted, so each affected
// environment gets exactly the same fence/reconciliation/job/deployment
// effects ExpireBatch would give it, and the environment cannot accept new
// work until the agent (or break-glass) reconciles. Lock order matches
// Claim/Renew/the sweeper -- slot, then job, then attempt, environments
// visited in ascending ID order -- so this can never deadlock against them;
// RevokeServer has already locked the servers row first, ahead of all of
// these per the global hierarchy. Decision 3: RotateCredential never calls
// this, only RevokeServer -- rotation must not strand in-flight work.
func revokeInFlightLeases(ctx context.Context, tx pgx.Tx, deployments *DeploymentRepository, inject func(string) error, serverID string, now time.Time) error {
	rows, err := tx.Query(ctx, `SELECT id, job_id, environment_id FROM agent_job_attempts WHERE server_id=$1 AND state='leased' ORDER BY environment_id, id`, serverID)
	if err != nil {
		return fmt.Errorf("discover in-flight agent job attempts: %w", err)
	}
	type inFlightAttempt struct {
		attemptID, jobID int64
		environmentID    string
	}
	var candidates []inFlightAttempt
	for rows.Next() {
		var c inFlightAttempt
		if err = rows.Scan(&c.attemptID, &c.jobID, &c.environmentID); err != nil {
			rows.Close()
			return fmt.Errorf("scan in-flight agent job attempt: %w", err)
		}
		candidates = append(candidates, c)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("discover in-flight agent job attempts: %w", err)
	}
	rows.Close()

	for _, c := range candidates {
		var slotCurrentAttemptID *int64
		if err = tx.QueryRow(ctx, `SELECT current_attempt_id FROM environment_slots WHERE environment_id=$1 FOR UPDATE`, c.environmentID).Scan(&slotCurrentAttemptID); err != nil {
			return fmt.Errorf("lock environment slot for revocation: %w", err)
		}
		if slotCurrentAttemptID == nil || *slotCurrentAttemptID != c.attemptID {
			continue // already closed/reassigned by a concurrent completion or expiry
		}
		var jobState, deploymentID string
		var jobCancelRequestedAt *time.Time
		if err = tx.QueryRow(ctx, `SELECT state, deployment_id, cancel_requested_at FROM agent_jobs WHERE id=$1 FOR UPDATE`, c.jobID).Scan(&jobState, &deploymentID, &jobCancelRequestedAt); err != nil {
			return fmt.Errorf("lock agent job for revocation: %w", err)
		}
		if jobState != "leased" {
			continue
		}
		var attemptState string
		var attemptNumber int
		if err = tx.QueryRow(ctx, `SELECT state, attempt_number FROM agent_job_attempts WHERE id=$1 FOR UPDATE`, c.attemptID).Scan(&attemptState, &attemptNumber); err != nil {
			return fmt.Errorf("lock agent job attempt for revocation: %w", err)
		}
		if attemptState != "leased" {
			continue
		}
		cancelRequested := jobCancelRequestedAt != nil
		if err = closeExpiredOrRevokedAttempt(ctx, tx, deployments, inject, c.jobID, c.attemptID, c.environmentID, attemptNumber, deploymentID, cancelRequested, now); err != nil {
			return err
		}
	}
	return nil
}
