# ADR 0004: Coordinate Agent Work with PostgreSQL Queue Leases

- Status: Accepted
- Date: 2026-08-05

## Context

AutoDeploy must durably coordinate deployment work across control-plane replicas and outbound-only
agents. An agent can pause, disconnect, restart, finish after its lease expires, or perform a host
side effect before the control plane receives its response. PostgreSQL transactions cannot remain
open for the duration of a build, and no database lock can fence a process that already received
work.

ADR 0001 requires expiring scoped leases and stale-agent rejection. ADR 0002 requires webhook
receipt, deployment intent, and queued work to commit durably before success. ADR 0003 requires
exact secret versions to freeze with the authorized lease claim and remain stable across retries.

The queue therefore provides at-least-once delivery. AutoDeploy does not claim exactly-once
execution; correctness depends on fencing, idempotent host operations, durable attempts, and
transactional state changes.

## Decision

PostgreSQL is the V1 durable job queue. Control-plane replicas perform short `READ COMMITTED`
transactions to enqueue, claim, renew, complete, cancel, and recover work. Agents never access
PostgreSQL directly.

Queue ownership is represented by a durable expiring lease plus a monotonic environment-scoped
fence token. Every control-plane and host mutation must prove the current authenticated server,
attempt, fence, and lease authority.

`LISTEN/NOTIFY` may later reduce polling latency but is only a wake-up hint. Polling durable rows
remains the correctness authority.

## Delivery semantics

- Delivery is at least once.
- A committed lease may be delivered even if the claim response is lost.
- An attempt may produce host side effects before its completion response is committed.
- An expired attempt may later report success after a new attempt starts.
- Duplicate requests are idempotent only when their immutable request/result digests match.
- External operations use immutable labels and fence checks so retry/reconciliation can determine
  ownership rather than assume absence of prior work.

Exactly-once execution is rejected as an achievable queue guarantee.

## Job and attempt model

One immutable logical `agent_job` represents V1 execution of one deployment and job kind. Its
typed, versioned payload contains immutable IDs and safe execution metadata, never arbitrary
secret-bearing JSON.

Mutable job-head states:

```text
queued -> leased -> succeeded
                 -> retry_wait -> leased
                 -> failed
                 -> cancelled
                 -> dead
```

- `cancel_requested_at` is a durable flag while leased, not a replacement for lease ownership.
- `failed` is a non-retryable terminal job outcome.
- `dead` means retry attempts were exhausted.
- Terminal jobs are never resurrected. Administrator redeploy creates a distinct deployment
  intent and logical job.

Every lease creates one immutable numbered attempt. Attempt states are:

```text
leased -> succeeded | failed | expired | cancelled
```

Append-only job events record enqueue, claim, renew, retry scheduling, cancellation request and
acknowledgement, expiry, stale rejection, completion, permanent failure, and dead-lettering.
Attempt/event corrections are new events rather than updates or deletes.

## Enqueue idempotency and atomicity

Each job has an immutable idempotency key and canonical payload digest. V1 additionally enforces
one deployment-execution job with `UNIQUE (deployment_id, job_kind)`.

On enqueue conflict:

- Matching immutable scope and payload digest is idempotent success and returns the existing job.
- A mismatched scope or digest is an integrity conflict and creates no new state.

Automatic deployment uniqueness follows ADR 0002's active project binding, repository ID, full
ref, and commit SHA. Manual redeploy uses a distinct authenticated deployment intent.

For webhook-triggered work, these changes share one database transaction:

- Durable verified webhook receipt and processing state.
- Deployment creation and initial deployment event.
- Logical job enqueue and initial queue event.

No successful webhook response is returned if that transaction fails. Implementation requires a
transaction-aware Unit of Work; repositories that always open private transactions cannot
orchestrate this boundary.

## Claiming and approximate fairness

A claim uses a short `READ COMMITTED` transaction. It considers only jobs:

- In `queued` or `retry_wait`.
- Whose `available_at` is no later than database current time.
- Assigned to the authenticated agent's immutable server scope.
- Whose environment execution slot can be acquired.
- Whose project/environment/server authorization remains active.

Candidate ordering is stable:

1. Bounded priority descending.
2. `available_at` ascending.
3. `created_at` ascending.
4. Immutable job ID ascending.

Initial candidate discovery is a non-locking ordered read and does not authorize the claim. After
identifying a candidate, the transaction locks and validates authorization rows, then the
environment slot, then acquires the exact job with `FOR UPDATE ... SKIP LOCKED` in the declared
hierarchy. The job is revalidated against current state after locking. If any candidate row changes
or cannot be locked, the transaction abandons that candidate and retries selection; it never
proceeds from the discovery snapshot.

`SKIP LOCKED` prevents concurrent claimers from waiting on the same exact job at the job-locking
step. It intentionally produces an inconsistent view suitable for queue consumers and provides
approximate FIFO, not strict fairness.

Priority values are bounded and administrator-controlled. Priority aging, weighted tenant
fairness, and batch claims are deferred. V1 monitors oldest runnable age so starvation is visible.

Claim transactions never perform network, filesystem, Docker, or cryptographic materialization
work before commit.

## Atomic claim boundary

One successful claim transaction:

1. Locks owner/project-binding/environment/server/credential authorization rows and validates
   their active revisions.
2. Locks/reserves the environment execution slot.
3. Locks and revalidates the candidate job.
4. Rechecks cancellation and all candidate predicates against current rows.
5. Reserves the next signed-`BIGINT` environment fence token.
6. Creates the next attempt with a 90-second lease and authority deadline.
7. Durably freezes every exact secret-version ID required by the deployment.
8. Associates the active attempt/fence with the job and environment slot.
9. Transitions deployment `queued -> assigned` only on the first claim.
10. Appends claim/deployment/audit events.
11. Commits before returning work to the agent.

Retries and reclaimed leases reuse the already frozen secret-version set and never silently adopt
newer secrets. A new secret set requires a distinct deployment intent. Recovery resumes or
reconciles from durable deployment state and never moves deployment status backward.

All competing queue, authorization, revocation, deployment, and secret-freeze transactions use
this global row-lock hierarchy:

1. Authorization rows in type order: owner, project binding, environment, server, credential;
   within each type, immutable ID ascending.
2. Environment execution-slot rows by environment ID ascending.
3. Logical job rows by job ID ascending.
4. Attempt rows by attempt ID ascending.
5. Deployment aggregate heads by deployment ID ascending.
6. Secret-version and frozen-reference rows by secret/version ID ascending.
7. Append-only event/audit inserts after all required existing rows are locked.

Claim uses shared/key-preserving locks on active authorization revisions; revocation uses
conflicting update locks and increments those revisions before touching later row classes. Thus a
claim linearizes before revocation or observes the revoked revision and fails—revocation cannot
commit invisibly between authorization validation and claim commit.

Bounded deadlock/serialization retries remain defensive but are not a substitute for this lock
order. A retry discards all partial in-memory results.

## Environment execution slots and fencing

V1 permits one active execution per environment. Different environments may execute concurrently,
subject to future server-capacity limits.

Each environment execution slot stores a monotonic signed-`BIGINT` fence counter and the current
job/attempt when occupied. Claim increments the counter while holding the slot lock. Fence values
start positive, never decrement or wrap, and fail closed before overflow.

Every lease response and subsequent operation includes:

- Immutable server, project, environment, deployment, job, and attempt IDs.
- Attempt number.
- Environment fence token.
- Lease expiry and renewal sequence.
- Canonical operation or result digest where applicable.

The fence is required for renewal, progress, secret materialization, completion, failure,
cancellation acknowledgement, release activation, route mutation, and cleanup. Docker resources
and other host artifacts receive immutable ownership labels including environment, deployment,
attempt, and fence.

The singleton privileged host agent durably stores the highest accepted fence per environment and
rejects any lower fence. It also serializes all environment mutations through one local executor.
Before dequeuing each mutation, that executor verifies the exact current fence, operation sequence,
cancellation state, and a locally tracked monotonic authority deadline derived from the lease.

An external syscall cannot be preempted after it starts. Therefore expiry/cancellation during an
in-flight host mutation marks the environment `reconciliation_required`; the control plane does
not lease new work or authorize a higher fence for that environment until the agent reports the
operation outcome and reconciles resource labels/durable state. Acceptance of a higher fence is
serialized after completion/abort and reconciliation of the old local executor operation.

Route activation has an additional database linearization point: before the local route executor
can switch traffic, a fenced transaction must validate current authority and atomically transition
the deployment to `activating`. If cancellation committed first, this transition fails. If
`activating` committed first, cancellation is rejected and any lost/late activation result enters
reconciliation; no replacement attempt is leased meanwhile.

Cleanup operations address only immutable resources carrying their deployment/attempt/fence
labels. They cannot select resources merely by mutable environment/name labels.

Database fencing rejects stale control-plane requests but cannot revoke an already executing host
syscall. Safety across that gap comes from per-environment host serialization, activation
linearization, scoped labels, and blocking slot reuse until reconciliation—not an instant-preemption
claim. If local fence state is lost or corrupt, the agent fails closed and reconciles before
accepting work; it never resets the counter from local assumptions.

## Clock authority and lease defaults

PostgreSQL time is authoritative for availability, claim, renewal, cancellation deadlines, and
database-side expiry. Agent wall clocks do not extend authority; the host uses a local monotonic
deadline only to stop work no later than the authority duration it received.

Each transaction obtains database current time using `clock_timestamp()` and derives its deadlines
from that value. `now()`/`CURRENT_TIMESTAMP` are not used when elapsed real time inside a
transaction matters because they remain fixed at transaction start.

Accepted V1 defaults:

- Lease duration: 90 seconds.
- Agent renewal interval: 30 seconds.
- Cancellation cleanup grace: 60 seconds.
- Maximum attempts: 5, including expired attempts.
- Retry base: 5 seconds.
- Retry cap: 5 minutes.

Configuration may tighten these values after validation but cannot disable expiry, fencing, or a
bounded retry limit.

`clock_timestamp()` is wall-clock time, not monotonic. Production PostgreSQL nodes require
synchronized, monitored clocks with maximum skew below the lease renewal safety margin. A detected
backward jump or failover with uncertain skew pauses new claims, renewals, and activations until
the database clock is healthy and affected leases are reconciled. Failover preserves durable lease
rows but may shorten or extend their effective wall duration within the bounded clock-skew
assumption; this ADR does not claim otherwise.

## Renewal

Renewal requires all of these to match current durable state:

- Authenticated server identity.
- Active job and attempt ID.
- Environment fence token.
- Unexpired authority at database current time.
- Expected monotonic renewal sequence.

For sequence `N + 1`, the control plane calculates normal expiry as database current time plus 90
seconds. Without cancellation, both `lease_expires_at` and `authority_expires_at` become that
value. With cancellation, both remain capped at the earlier cleanup deadline. It persists the new
sequence/deadlines atomically. Replaying the already accepted sequence returns the stored deadlines
without extending them again. Lower sequences and gaps are rejected.

Renewal responses include the durable cancellation/revocation state. Authority that already
expired cannot be renewed even if no recovery worker has processed it yet.

## Completion and failure

First-time completion, failure, and cancellation acknowledgement require the same current
owner/attempt/fence/unexpired-authority predicates. They atomically:

- Validate the typed, size-bounded, sanitized result.
- Compare its canonical result digest.
- Close the attempt and job as appropriate.
- Release the environment execution slot only if it still names that attempt/fence.
- Commit the corresponding deployment transition and audit/job events.

Terminal retry uses a separate read-only replay branch. It requires the same authenticated server
and immutable owner/project/environment/deployment/job scope, exact attempt ID/fence, operation
type, request/operation ID, and terminal result digest previously committed for that attempt. An
exact replay returns the stored terminal response without reacquiring a slot, extending a lease,
or mutating state. This applies to success, permanent failure, and cancellation acknowledgement.
If no matching terminal operation was previously accepted, a late request is stale rather than
idempotent. A conflicting digest/type/scope, expired nonterminal lease, replaced fence, or different
attempt is rejected and audited as stale/integrity failure.

An ambiguous database/network response is reconciled by reading durable attempt state before
retrying; the caller does not assume commit or rollback.

## Cancellation and revocation

Queued or retry-wait cancellation is immediate: one transaction locks current authorization,
environment/job, and deployment rows in the global order; verifies cancellation is legal; marks
the job/deployment `cancelled`; and appends events. No attempt, lease, cleanup deadline, or
environment slot is created.

Cancellation of leased work before activation is cooperative:

1. Persist `cancel_requested_at`, a database-time cleanup deadline 60 seconds later, and a queue
   event; atomically cap both `lease_expires_at` and `authority_expires_at` at that deadline.
2. Return cancellation on the next agent poll/renewal.
3. Permit only fenced, scoped cleanup during the 60-second grace.
4. Accept a matching cancellation acknowledgement and close the attempt/job/deployment.
5. If grace expires, authority is already invalid; recovery advances fencing, closes the attempt
   and job/deployment as cancelled, and requires reconciliation before new work uses the slot.

Renewal during cancellation cannot extend authority beyond the cleanup deadline. New secret
materialization, build, start, or activation is forbidden after cancellation/revocation.

Cancellation while deployment status is `activating` is rejected because the accepted deployment
lifecycle permits only `activating -> healthy | failed`. Cancellation after `healthy` is a separate
rollback/stop decision, not queue cancellation.

Credential or authorization revocation immediately blocks new claims, renewals, materialization,
and host mutations that have not begun execution. It advances database fencing and marks any
environment with an in-flight host syscall `reconciliation_required`; already-running syscalls
cannot be preempted, and slot reuse/higher-fence authorization remains blocked until reconciliation.
Revocation preserves the currently healthy release unless that release itself uses a compromised
credential requiring ADR 0003 emergency handling.

## Expiry and recovery

Recovery first performs a non-locking ordered discovery of leased attempt IDs whose
`authority_expires_at` is no later than database current time. For each candidate, a bounded
transaction locks authorization rows, environment slot, job, and then the exact attempt with
`FOR UPDATE ... SKIP LOCKED`, following the global hierarchy. It revalidates expiry and ownership
after locking; changed or unavailable candidates are skipped. `authority_expires_at` equals the
normal lease expiry until cancellation caps both deadlines at the earlier cleanup deadline.

For each confirmed expiry it atomically:

- Closes the attempt as `cancelled` when cancellation was requested, otherwise as `expired`.
- Advances or invalidates the environment fence before accepting replacement work.
- Records the outcome and increments retry accounting only for expiry, never cancellation.
- Releases or marks the environment slot as requiring reconciliation.
- Schedules `retry_wait` or terminal `dead` for expiry; cancellation terminalizes the job and
  deployment without retry.

Lease expiry does not prove that host side effects stopped. Before reusing the environment, the
current agent reconciles fenced resource labels and durable deployment state. Cleanup is scoped so
an older attempt cannot delete a newer release.

Database restart/failover preserves lease rows. Effective duration remains subject to the bounded
database wall-clock skew and pause/reconciliation rules defined above.

## Retry and dead-letter policy

The retry classifier permits only transient failures, such as bounded infrastructure/provider
unavailability that remains safe to repeat. These do not retry automatically:

- Authorization or scope failure.
- Invalid input or unsupported payload version.
- Cancellation or credential revocation.
- Corrupt persisted state, digest conflict, or invariant violation.
- Permanent repository/build/runtime configuration failure.

Lease expiry counts as an attempt. Retry delay uses full jitter:

```text
uniform(0, min(5 minutes, 5 seconds * 2^(attempt_number - 1)))
```

Delay is selected once and persisted as `available_at`; retries do not recompute it. After five
attempts, the job becomes `dead` and the deployment transitions to a sanitized `failed` state in
the same transaction. A permanently classified error uses terminal `failed` without consuming
unused retry opportunities.

Dead jobs remain queryable/auditable but cannot be requeued. Administrator redeploy creates a new
deployment, idempotency key, job, and fence sequence use.

## Authorization and concurrency

- Agents claim only jobs for their authenticated immutable server ID.
- Every operation rechecks owner, project, environment, deployment, and server scope.
- Owner scope persists even in single-administrator V1 so multi-tenant boundaries are not removed
  from durable state.
- Multiple control-plane replicas are stateless with respect to lease ownership; PostgreSQL rows,
  transactions, and constraints are authoritative.
- One active execution per environment prevents competing releases from mutating the same route.
- Future server-capacity scheduling may further limit parallel environments without changing
  lease/fence semantics.

## Transaction boundaries

These state groups must commit atomically:

- Webhook receipt, deployment creation, and job enqueue.
- Claim, attempt creation, fence increment, environment-slot reservation, secret-version freeze,
  initial assignment, and related events.
- Renewal sequence and lease expiry.
- Completion/failure, slot release, deployment transition, and events.
- Cancellation request/deadline and event.
- Expiry, attempt closure, fencing, retry/dead scheduling, deployment failure, and events.

No transaction spans agent network calls or host work. PostgreSQL/pgx transaction helpers must
explicitly commit or roll back; context cancellation alone is not treated as rollback completion.

## Schema, constraints, and indexes

Future migrations should provide typed constrained tables for logical jobs, attempts, append-only
events, environment execution slots/fences, and frozen secret-version references.

Required constraints include:

- Positive signed revisions, attempt numbers, renewal sequences, and fence values.
- Checked job/attempt states and legal terminal shapes.
- Unique deployment/job kind, immutable idempotency key, attempt ID, and
  `(job_id, attempt_number)`.
- One occupied execution slot per environment.
- Foreign keys and immutable owner/project/environment/server/deployment scope when those domain
  tables exist.

Required index shapes include:

- Partial runnable index on
  `(server_id, priority DESC, available_at, created_at, id)` for `queued/retry_wait`.
- Partial authority-expiry index on `(authority_expires_at, id)` for `leased`, including
  cancellation-capped attempts.
- Current attempt/fence lookup and idempotency/digest lookup indexes.

Query plans must be tested against representative backlog sizes. Exact retention durations are
deferred to the operations/retention slice. Sanitized job, attempt, event, and result metadata
follows deployment-audit retention; bulky logs/results are purged separately in bounded batches.

## Audit and observability

Queue/audit records may contain immutable IDs, typed states, revisions, attempt/fence numbers,
timings, classifications, safe digests, and sanitized errors. They never contain secret values,
ciphertext, wrapped keys, credential/token material, unrestricted payload bodies, environment
blocks, command lines, or redactor matches.

Metrics include:

- Runnable backlog and oldest age by server.
- Claim latency and active lease count.
- Renewal failures, expiry count, retries, and dead jobs.
- Cancellation latency and reconciliation backlog.
- Stale attempt/fence rejection count.
- Queue transaction/query latency and database error classifications.

Metric labels use bounded identifiers/classes and never project-controlled free text.

## Failure invariants

- Crash before claim commit: no lease exists and the job remains runnable.
- Crash after claim commit or lost claim response: lease expires and is reconciled/retried.
- Lost renewal response: retry the same sequence and receive the stored expiry.
- Lost completion response: reconcile durable terminal state and digest before retrying.
- Agent pause/partition beyond expiry: database-side requests fail; an in-flight host syscall makes
  the environment reconciliation-required and blocks newer work rather than claiming preemption.
- Two claimers: row/slot locks permit at most one current attempt per environment.
- Authorization revocation races: shared/update authorization locks linearize claim or force it to
  observe the revoked revision.
- Poison job: bounded retries end in durable `dead`.
- Offline server: jobs remain durable and visible without reassignment outside authorization.
- Database ambiguity/deadlock/failover: discard partial results and reconcile/retry whole bounded
  transactions.
- Fence overflow or lost local fence state: fail closed and require operator/reconciliation flow.
- Route activation or cleanup race: highest environment fence and immutable labels protect the
  newer release, and a reconciliation-required environment cannot lease replacement work.

## Minimal implementation boundaries

Future approved slices should preserve these interfaces or equivalent responsibilities:

- Enqueuer: enqueue a typed intent inside a caller-owned transaction.
- Leaser: claim, renew, complete, fail, request cancellation, and recover expiry.
- Lease authorizer: validate server/scope/attempt/fence/expiry for every operation.
- Attempt/event repositories: immutable attempts and append-only audit history.
- Fence store: durable environment counter/slot plus host synchronization contract.
- Retry classifier and backoff policy.
- Queue observer for metrics without payload leakage.
- Transaction-aware deployment and secret-freeze repositories under one Unit of Work.

## Consequences

Benefits:

- PostgreSQL provides durable queue and domain atomicity without a second datastore.
- Multiple control-plane replicas and agents can contend safely.
- Fencing rejects late work after lease reassignment.
- Durable attempts/events make ambiguity, recovery, and operator diagnosis auditable.
- Secret versions and deployment state remain consistent with lease authority.

Costs:

- Host operations must be idempotent, labeled, fenced, and reconcilable.
- `SKIP LOCKED` provides throughput rather than strict fairness.
- Environment serialization can reduce throughput for applications sharing one environment.
- Recovery must inspect external Docker/routing state; database leases alone cannot undo side
  effects.
- Queue tables and indexes require backlog, bloat, retention, and query-plan operations.

## Alternatives considered

- Redis queue in V1: rejected because PostgreSQL already owns transactional deployment state and
  the approved design intentionally excludes Redis.
- Session/advisory locks as job ownership: rejected because connection loss releases them and they
  do not provide durable attempts, expiry, or external-operation fencing.
- Hold a database transaction throughout execution: rejected because builds are long-running and
  network/host failures would retain locks and connections.
- Agent PostgreSQL access: rejected by ADR 0001 because it bypasses API authorization and expands
  database exposure.
- `LISTEN/NOTIFY` as the queue: rejected because notifications are not durable ownership.
- Lease without attempts/events: rejected because ambiguity and stale results become unauditable.
- Optimistic claim without row locking: rejected because it complicates fairness and slot/fence
  atomicity without platform value.
- Exactly-once queue claims: rejected as an inaccurate guarantee across database and host side
  effects.

## Deferred decisions

- Exact SQL schema and repository implementation.
- Agent transport and HTTP message formats.
- Batch claiming and `LISTEN/NOTIFY` wake-up implementation.
- Server capacity/labels and multi-environment scheduling.
- Advanced priority aging and multi-tenant weighted fairness.
- Exact terminal metadata, event, result, and log retention durations.
- Administrator queue/retry/cancellation UI.

## Validation criteria

- Concurrent claimers across control-plane replicas produce one current attempt per environment.
- Webhook receipt, deployment creation, and enqueue either all commit or all roll back.
- First claim, fence increment, secret freeze, assignment, attempt, slot, and events are atomic.
- Expired attempts recover with a higher fence; stale renewal/completion/materialization fails.
- Renewal replay returns the stored expiry without extending; gaps/lower sequences fail.
- Duplicate completion with the same digest is idempotent; a different digest is rejected.
- Exact terminal success/failure/cancellation replay returns stored response without active lease;
  unmatched late terminal requests remain stale.
- Retries/reclaims reuse frozen secret versions and never move deployment status backward.
- Cancellation races at claim, materialization, build, start, health check, and activation obey the
  lifecycle and 60-second cleanup bound.
- Queued/retry-wait cancellation is terminal immediately; leased cancellation caps authority and
  is recovered through the authority-expiry index.
- Claim and authorization/credential revocation serialize through the documented lock hierarchy.
- Retry classification, persisted full-jitter delay, five-attempt exhaustion, and dead-lettering
  are deterministic under controlled randomness.
- Database restart/failover and ambiguous responses reconcile from durable state.
- Lost/corrupt host fence state fails closed; stale cleanup cannot delete a newer release.
- In-flight same-fence host mutation during expiry/cancellation blocks environment reuse until
  reconciliation; activation uses the atomic `activating` transition as its linearization point.
- Database clock regression/failover beyond the monitored bound pauses queue authority changes and
  requires reconciliation.
- Representative PostgreSQL 18 `EXPLAIN` plans use runnable and expiry indexes at backlog scale.
- Queue rows, events, metrics, traces, and logs contain no secret material.

## References

- [PostgreSQL SELECT locking and SKIP LOCKED](https://www.postgresql.org/docs/18/sql-select.html)
- [PostgreSQL transaction isolation](https://www.postgresql.org/docs/18/transaction-iso.html)
- [PostgreSQL date/time functions](https://www.postgresql.org/docs/18/functions-datetime.html)
- [pgxpool transaction behavior](https://pkg.go.dev/github.com/jackc/pgx/v5/pgxpool)
