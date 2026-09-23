package postgres

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

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
// revokeInFlightLeases). Both cases get identical effects: the attempt
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
	// combined fence/slot/authority check exactly (line ~937 of this file):
	// attempt must still be the leased, currently-occupying attempt for this
	// fence, with unexpired authority, and no cancellation must have been
	// requested against its job. Unlike Complete there is no "attempt already
	// closed" replay branch here at all: tx1 never closes the attempt (only
	// Complete-on-failure or the not-yet-implemented tx2 do), so an attempt
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
		if existingState != "switch_authorized" {
			// Already resolved by a (not-yet-implemented in this slice) tx2,
			// or otherwise closed: this call is stale, not a live replay.
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
	err = tx.QueryRow(ctx, `INSERT INTO release_operations(release_id, job_id, attempt_id, fence, kind, digest, state, environment_id, expected_active_release_id, expected_route_revision, route_digest)
		VALUES ($1,$2,$3,$4,'activate',$5,'switch_authorized',$6,$7,$8,$9) RETURNING id`,
		releaseID, jobID, attemptID, fence, imageDigest[:], environmentID, expectedActiveReleaseID, routeRevision, routeDigestRaw).Scan(&operationID)
	if err != nil {
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
