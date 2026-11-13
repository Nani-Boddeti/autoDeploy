# ADR 0005: Support Health-gated Blue/Green and Explicit Stop-then-start Releases

- Status: Accepted
- Date: 2026-08-05

## Context

AutoDeploy must activate Dockerfile-based releases behind Traefik while preserving durable traffic
ownership, safe rollback, and stale-agent fencing. Most applications can run an old and candidate
release concurrently, but some require exclusive ports, volumes, locks, or external resources and
must stop the old process first.

Blue/green can keep a healthy release serving until the candidate passes its gate. Stop-then-start
cannot make that guarantee: its real candidate health check occurs only after the old release stops,
so failure can cause an outage and may require operator recovery.

ADR 0001 assigns target-host Docker and routing mutations to the agent. ADR 0003 constrains secret
materialization and retained-container exposure. ADR 0004 requires fenced, serialized environment
mutations and an atomic transition to `activating` before route changes.

## Decision

Each environment explicitly selects one release strategy:

- Health-gated blue/green is the default.
- Stop-then-start is available only through an explicit environment setting with an outage and
  recovery warning.

PostgreSQL owns desired release/route state and the durable operation journal. The fenced host agent
owns Docker state and atomically replaced Traefik file-provider configuration. Traefik does not
receive the Docker socket.

V1 production application routing requires HTTPS. The HTTP entrypoint serves ACME handling and an
HTTP-to-HTTPS redirect, never an HTTP-only application route. TCP/UDP, HTTP-only production,
weighted/canary, rolling replicas, and cross-host traffic shifting are deferred.

## Deployment, release, runtime, and operation model

A deployment remains immutable intent and lifecycle history. A release is the reusable host/runtime
artifact produced for one deployment. One deployment creates one immutable release identity;
execution retries may create multiple runtime-instance identities for that release.

Release desired states and legal transitions:

- `candidate -> ready`: initial runtime passed the frozen direct health gate.
- `candidate | ready -> failed`: pre-active preparation or health failure.
- `ready -> active`: successful initial activation finalization.
- `active -> retained`: a different release/maintenance recovery target became active.
- `retained -> active`: rollback finalization only, after a fresh health gate and fenced switch.
- `candidate | ready -> removing`: cancellation/supersession cleanup before activation.
- `failed | retained -> removing`: retention cleanup after protection/reference checks.
- `removing -> removed`: exact resources are absent and references are cleared.

`active` cannot transition directly to `removing`; routing must first move safely to another
release or maintenance recovery outcome. `removed` is terminal. A retained rollback target remains
`retained` while it is started and health-checked; failed rollback preparation leaves it retained
and records failure on the rollback operation rather than corrupting release history.

Runtime observation is separate:

```text
created | running | stopped | exited | missing
```

Desired state never claims that Docker or Traefik already converged. Observed state is updated only
from fenced inspection and reconciliation.

Append-only release operations represent `prepare`, `activate`, `rollback`, `retire`, and `cleanup`.
Each operation includes immutable operation ID/type, canonical digest, environment route revision,
job/attempt/fence, source/target release IDs, state, and sanitized outcome.

The route head stores desired `active_release_id`, route revision, hostname, entrypoint, and service
policy. The environment's Traefik file and API-observed router/service are host-observed truth. Any
ambiguous disagreement marks the environment `reconciliation_required` and blocks new activation,
rollback, or cleanup.

Deployment status is not current traffic truth. An old deployment remains historical even if its
retained release later becomes active through rollback.

## Strategy eligibility

Blue/green requires the application to tolerate simultaneous old/candidate execution, including
its database schema, external consumers, singleton tasks, persistent volumes, and locks.

Stop-then-start is required when concurrent execution is unsafe. Strategy selection is frozen into
deployment execution metadata with the service port, network, health-policy revision, route
revision, and required secret-version references.

AutoDeploy does not infer strategy from a Dockerfile. Operators own data-migration backward
compatibility and external-resource semantics. Unsafe or unknown concurrency defaults to explicit
stop-then-start rather than weakening blue/green isolation.

## Immutable identity, names, and labels

Images, containers, networks, route documents, and operations use opaque internal IDs. User input,
domains, branches, repository names, and commit messages never become unvalidated Docker names.

Containers run by immutable image ID/digest, never a mutable tag. Safe container names encode a
collision-checked internal release/runtime ID.

All managed Docker resources carry common creation labels: `io.autodeploy.managed=true`, resource
kind, label-schema version, and immutable owner/project/environment/server IDs where applicable.
Additional ownership is resource-kind-specific:

- Release runtime containers and release-scoped ephemeral volumes: deployment, release, job,
  attempt, fence, runtime-instance, full commit SHA, image digest, and non-secret config digest.
- Built images: immutable image/config/source digests and build-operation identity; images may be
  shared by runtimes and are deleted only after reference checks, not a single-owner label.
- Stable environment networks and persistent environment volumes: environment-infrastructure
  identity/revision only; they never pretend to belong to one release, attempt, or fence.

Labels are creation facts and never encode mutable `active=true` state. They contain no secret,
hostname credential, environment value, or unrestricted user content. Route documents carry their
own revision/operation metadata rather than Docker labels.

Cleanup uses resource-kind-specific ownership. Release-scoped containers/ephemeral volumes require
exact immutable IDs and expected attempt/fence labels. Shared images use exact image ID/digest plus
database and Docker reference checks under the authorized cleanup operation's current fence; they
do not require runtime attempt/fence labels. Stable environment infrastructure is never selected
by release cleanup. Environment/name-only selection is forbidden.

## Network and port model

- Each environment has one stable user-defined bridge network.
- Traefik joins each environment network that it routes.
- Releases join only their environment network plus explicitly approved dependencies.
- Application containers publish no host ports and do not use host networking.
- Traefik routes to an explicitly configured container port and scheme using a unique internal
  Docker DNS name.
- Docker's first exposed port is never used as an implicit routing decision.
- Environment networks are infrastructure and are not removed by attempt/release cleanup.

Traefik spans networks, but application containers cannot directly join or discover other
environment networks. Network attachment changes are fenced host operations.

Persistent application volumes are environment-scoped infrastructure unless explicitly modeled as
release-scoped. Generic release cleanup never deletes environment volumes. Release-scoped ephemeral
volumes require exact immutable labels and reference checks.

## Health-gate contract

Every HTTP/HTTPS production route has an explicit frozen health policy. "Container is running" is
never sufficient.

Accepted default HTTP policy:

- Exact configured container port, scheme, path, and Host header.
- Expected response status: 2xx.
- Redirect following disabled.
- Three consecutive successful probes.
- Two-second probe interval.
- Two-second per-probe connection/request timeout.
- Sixty-second overall deadline.
- Bounded response body that is discarded rather than persisted.

The container must remain running throughout the gate. Exit or explicit unhealthy status is
immediate failure. Probe failures reset the consecutive-success count.

The agent probes the candidate directly over its environment Docker network so candidate health is
independent of production route state. Optional Docker `HEALTHCHECK` or a future TCP/container gate
may add defense in depth but cannot replace the required HTTP gate for an HTTP/HTTPS route.

After route switching, activation additionally requires:

- The secured internal Traefik API reports the exact release/fence-specific router and service with
  no configuration error.
- A bounded HTTPS data-plane request through Traefik succeeds using the production hostname with
  certificate and hostname validation enabled.

Traefik backend health checks are runtime defense in depth, not the deployment gate.

## Traefik configuration ownership

Traefik uses the watched file provider rather than Docker-label discovery. This avoids giving
Traefik Docker API access and represents traffic selection as one independently acknowledged route
document.

There is one complete mutable dynamic route file per environment:

- Stable router/middleware names derive from immutable route IDs.
- Service names include release ID and fence.
- The file points to exactly one application release or the server maintenance service.
- Route content contains no secret values.

The agent renders a deterministic document from authorized desired state and validates its schema
and canonical digest. It writes to a same-directory temporary regular file whose name ends in an
explicit non-provider extension such as `.tmp`; supported Traefik dynamic-config extensions are
forbidden for temporary files and tests prove the provider ignores them. The agent syncs the temp
file, renames it atomically to the final provider-recognized filename, and syncs the parent
directory. Traefik watches a read-only mount of that parent directory so rename-based replacement
presents one complete new inode.

Atomic rename is only the request to switch configuration. Traefik reload is asynchronous and is
not successful until the expected router/service is observed and the HTTPS data-plane probe passes.
The Traefik API and metrics remain private, authenticated management surfaces.

Route documents carry non-secret operation/revision metadata. The agent never edits a partial
shared configuration file in place.

## Fenced activation and reconciliation

Activation is one append-only operation with expected active release ID, expected route revision,
candidate release, attempt/fence, and canonical route digest.

Before a host route switch, a database transaction follows ADR 0004's lock hierarchy and:

- Verifies current authorization, lease authority, operation digest, environment fence, route
  revision, source active release, candidate readiness, and cancellation state.
- Transitions the deployment to `activating`.
- Records the activation operation as switch-authorized.
- Prevents cancellation and replacement work for that environment.

The serialized host route executor then rechecks the same fence/operation and replaces the route
file. A duplicate operation with the same digest compares the existing file/observed state and is
idempotent; a conflicting digest fails.

After Traefik/config/data-plane observation, a second database transaction rechecks the operation,
route revision, and fence, then atomically:

- Advances the desired route revision and active release pointer.
- Marks the candidate active and the predecessor retained where applicable.
- Finalizes deployment `healthy` or the rollback operation.
- Appends release, route, deployment, and audit events.

A crash can occur before/after file replacement or before/after database finalization. Ambiguity is
never resolved by blind automatic rollback. The environment remains blocked while reconciliation
compares the operation/digest, desired route revision, file digest, Traefik API, HTTPS data plane,
Docker resources, and fence. It then resumes/finalizes the same operation, restores the last
verified route when safe, or requires operator recovery.

An in-flight syscall cannot be preempted. ADR 0004's reconciliation-required and no-slot-reuse
rules apply through the entire activation operation.

## Blue/green procedure

1. Build the immutable image and record image/config digests.
2. Create and start a uniquely identified candidate without public host ports.
3. Run the complete direct health gate while the current active release keeps serving.
4. Begin the fenced activation operation and transition to `activating`.
5. Atomically replace the environment route file to select the candidate.
6. Verify Traefik configuration and HTTPS data-plane health.
7. Finalize the new active release and retain the predecessor transactionally.
8. Drain for the accepted 30-second window, then stop—but do not remove—the predecessor.

Failure before activation leaves the current route/release untouched. The failed candidate is
stopped and retained temporarily for sanitized diagnostics.

Failure or ambiguity after switch authorization blocks the environment for reconciliation. It does
not permit a different attempt to activate concurrently.

## Stop-then-start procedure

The host provides a pre-provisioned, independently health-checked maintenance service that returns
HTTP 503 without application secrets. Maintenance routing must work before an environment may use
stop-then-start.

1. Build and create—but do not start—the candidate while the old release serves.
2. Validate image/config digests, network/port/volume compatibility, resource capacity, and frozen
   health policy.
3. Begin the fenced activation operation and transition to `activating`; cancellation is now
   rejected and the outage operation is durable.
4. Route to maintenance and verify the expected 503 service through Traefik.
5. If maintenance switching fails, do not stop the predecessor; restore/verify its route when
   unambiguous or block for reconciliation.
6. Drain for 30 seconds, request bounded predecessor shutdown, and inspect until Docker reports it
   stopped/exited and its process is no longer running.
7. If stop times out, fails, or remains ambiguous, do not start the candidate; keep verified old or
   maintenance routing and reconcile.
8. Only after observed predecessor termination, start the candidate and run the real direct health
   gate.
9. Switch the route to the healthy candidate and verify Traefik plus HTTPS data plane.
10. Finalize candidate active, predecessor retained, and deployment healthy.

If the candidate fails:

1. Stop the candidate.
2. Restart and health-check the prior release with its retained configuration/secret eligibility.
3. Restore and verify its route.
4. Finalize the candidate deployment as failed while the prior release remains active.

If prior-release recovery also fails, maintenance routing remains indefinitely. The environment is
marked degraded/reconciliation-required, no new activation starts, and operator recovery is
required.

This strategy preserves the prior release as the recovery target but explicitly accepts bounded or
unbounded outage. It never claims blue/green's continuous-serving guarantee.

## Rollback

Rollback is a new authenticated durable operation/job, not resurrection of a terminal deployment
job. It identifies the current source release and one retained target release in the same
environment and server.

The target must:

- Have passed its original health gate.
- Retain or reproducibly recreate its immutable image/config/runtime resources.
- Match recorded digests.
- Reference secret versions that are still eligible under ADR 0003.
- Pass a fresh health gate before receiving traffic.

Retired but non-revoked secret versions may be used only for this explicit rollback during the
retained-release lifetime. Deleted, compromised, missing, or otherwise non-materializable versions
block rollback. Emergency secret recovery removes affected retained containers even if that
eliminates rollback.

Blue/green rollback starts/recreates and health-checks the target beside the current release, then
uses the same fenced activation procedure. Stop-then-start rollback uses the same maintenance,
outage, restart, and recovery semantics as stop-then-start deployment.

After a successful rollback, the source deployment transitions to `rolled_back` where permitted.
The target's original deployment history is not rewritten. PostgreSQL route/release heads and the
rollback operation remain desired/audit truth; serving truth comes from the route file plus
Traefik/data-plane observation, and disagreement requires reconciliation.

## Retention and drain policy

Accepted V1 defaults:

- Keep the active release without time limit while active.
- Keep at most two prior successful releases, each no longer than seven days after deactivation.
- Stop predecessors after a 30-second drain; retained containers remain stopped unless needed for
  rollback.
- Keep failed candidate resources for at most 24 hours for sanitized diagnostics.
- Retain compact sanitized release/operation/audit metadata according to the future audit policy.

Cleanup runs in bounded idempotent batches. Active, in-progress, rollback-target,
reconciliation-owned, and route-referenced resources are protected regardless of age/count.

If disk/capacity pressure remains after safe cleanup, reject new builds/deployments and alert rather
than deleting protected resources. Bulky build cache, stopped failed candidates, unreferenced
images, and logs have independent bounded retention.

Long-lived connections may exceed the default drain and be interrupted when the predecessor stops.
The drain duration is environment-configurable and frozen into the operation.

## Cancellation and supersession

- Supersession is allowed only while the older deployment/job is queued.
- Blue/green cancellation before `activating` stops/removes only exact candidate resources and
  preserves the active release/route.
- Stop-then-start enters `activating` before maintenance routing or stopping the old release, so
  cancellation is rejected once outage begins.
- Expiry, cancellation, or revocation during a host syscall blocks slot reuse pending
  reconciliation under ADR 0004.
- Rollback and stop are distinct post-healthy intents, not deployment cancellation.

## Cleanup and idempotency

- Every cleanup request has an immutable operation ID/digest, expected fence, and exact resource
  identity set.
- Absent exact resources are idempotent success only when no conflicting newer ownership exists.
- A route reference or protected release state prevents removal.
- Cleanup cannot delete environment networks, maintenance infrastructure, persistent environment
  volumes, active route documents, or newer-fence resources.
- Release-scoped container/volume deletion rechecks exact immutable ownership/fence labels
  immediately before mutation. Shared-image deletion rechecks exact digest/ID and zero live or
  protected references under the current cleanup-operation fence.
- Failed partial cleanup records observed residual resources and enters reconciliation rather than
  broadening selectors.

## Domains, TLS, and servers

- One route belongs to one environment and canonical hostname.
- An authenticated owner/administrator explicitly assigns the hostname and acknowledges authority
  to use it; repository content, deployment payloads, and DNS resolution alone cannot claim it.
- Hostname binding is audited and unique across concurrently active control-plane routes. Future
  multi-tenant self-service requires an independent DNS/HTTP ownership challenge.
- Each environment/release is pinned to one immutable server; moving servers requires a fresh
  deployment.
- DNS A/AAAA must resolve to that server's Traefik before certificate issuance/production
  activation, but resolution is readiness evidence rather than ownership authorization.
- Route/TLS readiness is provisioned independently, initially pointing to maintenance until a
  healthy active release exists.
- The HTTP entrypoint redirects application requests to HTTPS; production success always uses the
  HTTPS entrypoint with certificate and hostname validation.
- Traefik derives certificate domains from explicit Host rules; certificate/hostname validation is
  required in the post-switch probe.
- ACME state is persisted and backed up per server with restrictive access; one writable ACME store
  is not shared across independent Traefik instances.

DNS automation, wildcard certificates, cross-host activation, shared certificates, global
failover/load balancing, and zero-downtime server migration are deferred.

## Secrets, logs, and audit

- Route documents, Docker names/labels, image metadata, operation payloads, probes, and logs contain
  no secret values.
- Runtime values use Docker API injection, never command arguments or generated environment files.
- Agent and control plane redact independently; failure suppresses output under ADR 0003.
- Health response bodies are bounded and discarded; persist only sanitized status class, timing,
  attempt, and failure category.
- Traefik access logs drop or redact authorization, cookies, query strings, and sensitive headers.
- Audit records contain immutable IDs, route revisions, operation/fence/digests, strategy, health
  policy revision, safe results, and timestamps—never credentials or unrestricted bodies.

## Failure invariants

- Candidate build/start/direct-health failure in blue/green leaves current serving route untouched.
- Stop-then-start candidate failure attempts fenced prior-release recovery; dual failure leaves
  verified maintenance routing and blocks the environment.
- Invalid or rejected Traefik configuration never counts as activation success.
- Route-file replacement without observation/finalization is reconciliation, not success or blind
  rollback.
- Traefik/agent/Docker restart is reconciled from desired route, operation journal, immutable labels,
  and observed state.
- Stale fence activation/cleanup cannot operate after a newer fence is accepted; in-flight old
  syscalls block reuse until reconciliation.
- Missing/revoked rollback resources fail closed and do not disturb the current active release.
- Cleanup racing activation/rollback yields to protected route/release references.
- Missing authenticated hostname assignment or DNS/ACME failure blocks first production activation
  while maintenance remains explicit.

## Minimal implementation boundaries

Future approved slices should preserve these interfaces or equivalent responsibilities:

- Release and append-only release-event repositories.
- Release planner and strategy selector.
- Runtime: build, create, start, stop, inspect, remove.
- Health gate.
- Route store with expected-route-revision CAS.
- Traefik adapter: render, validate, switch, observe.
- Activation and rollback coordinators.
- Retention planner and exact cleanup executor.
- Host reconciler.

## Consequences

Benefits:

- Default blue/green preserves a healthy serving release through candidate validation.
- Stop-then-start supports exclusive applications without pretending zero downtime.
- Traefik file routing preserves the Docker-socket trust boundary and exposes an auditable pointer
  switch.
- Immutable identities, digests, labels, fencing, and operation journals make crash recovery
  deterministic enough to reconcile.
- Explicit retained releases enable health-gated rollback.

Costs:

- Blue/green temporarily doubles runtime capacity and requires concurrent-safe application/data
  behavior.
- Stop-then-start introduces deliberate outage and can remain on maintenance indefinitely.
- Traefik reload is asynchronous, so activation needs observation and data-plane verification.
- Retained containers/images consume disk and may retain runtime secrets.
- Host routing/runtime state requires ongoing reconciliation with PostgreSQL desired state.

## Alternatives considered

- In-place same-name container replacement: rejected because crash recovery and rollback ownership
  are ambiguous.
- Traefik Docker-label discovery: rejected because it requires Docker API access and does not expose
  one independently acknowledged active-release pointer.
- Published ephemeral host ports: rejected due to collision, exposure, and cleanup complexity.
- Always blue/green: rejected for exclusive/single-writer applications.
- Stop-then-start default: rejected because it creates unnecessary outage.
- Route switch before candidate health: rejected.
- Blind automatic rollback after ambiguous activation: rejected because it may reverse a successful
  live switch.
- Mutable image tags as release identity: rejected.

## Deferred decisions

- Concrete Docker/BuildKit and Traefik clients and version pins.
- Dynamic-config serialization library and schema validation implementation.
- Probe implementation, alternate status policies, and application-specific warm-up semantics.
- Fine-grained connection draining and protocol-aware shutdown.
- Resource limits, build cache policy, and server-capacity scheduling.
- UI/API workflows and operator reconciliation tools.
- Retention batching and compact audit-retention durations.
- Runtime file-secret adapter and persistent-volume lifecycle automation.
- Weighted/canary/rolling releases, replica sets, TCP/UDP routes, orchestrators, registries, and
  cross-server/global routing.

## Validation criteria

- Blue/green build/start/health failure leaves the old HTTPS release serving.
- Stop-then-start tests cover candidate failure with successful old recovery and dual failure with
  persistent maintenance/degraded state.
- Health tests enforce exact host/path/port/status, redirects off, consecutive successes, timeouts,
  deadline, container exit, and bounded discarded bodies.
- Crash tests cover every boundary before/after route rename, Traefik observation, and database
  finalization.
- Invalid/delayed/coalesced file-provider reload and Traefik restart cannot produce false success.
- Two control-plane replicas racing route CAS yield one activation operation.
- Stale fence activation/cleanup fails; in-flight syscalls block environment reuse until
  reconciliation.
- Docker/agent restart and container-IP change reconcile by immutable identity/DNS, not cached IP.
- Rollback rejects missing images/config, digest mismatch, wrong environment/server, and
  missing/deleted/compromised secrets.
- Cleanup racing rollback/activation preserves active, retained target, and route-referenced
  resources.
- Unauthorized hostname assignment, DNS mismatch, ACME failure/rate limit, expired/wrong
  certificate, HTTP redirect failure, and private Traefik API failure block production success
  safely.
- Drain tests document long-lived connection interruption after the 30-second default.
- Labels, route files, health diagnostics, access logs, API, audit, and metrics contain no secrets.

## Amendments (2026-11-11)

Steps 10d (agent activation) and 10e (predecessor drain) implemented the blue/green procedure above. Each
statement was checked against `internal/agent/executor` (`activation.go`, `recovery.go`, `drain.go`),
`internal/agent/client/reporter.go`, `internal/runtime/traefik/probe.go` and
`internal/store/postgres/agent_lease_repository.go` (`derivePredecessor`). Decisions are recorded in
`tasks/10d-plan.md` and `tasks/10e-plan.md`. Nothing here has run on a real host.

### Agent activation sequence (10d)

After a passing health gate the executor (`activate`) runs, in order:

1. **Activate** (`authorize`). A 409 is a definite refusal: nothing was authorized, and the failure is
   classified (`classifyActivateRefusal`: `candidate_not_ready` transient, `no_route_head`
   permanent_configuration, every other code corrupt_state). Any other error (transport, 503, undecodable
   body) leaves the authorization state UNKNOWN, because the control plane may have committed it and the answer
   been lost. The call is replayed (the server replays on attempt, fence and image digest); a 409 after an
   earlier unknown try is still unknown, since it may be a consequence of the agent's own authorization. When
   the state stays unknown the agent writes no route and sends no `Complete`; the sweeper blocks the operation
   and an operator declares the outcome.
2. **Durable `authorized` record** (`state.PendingActivation`) BEFORE any route write. If it cannot be saved the
   route is untouched and a failure is reported directly.
3. **`switchRoute`**: re-render from the response and lease and refuse on any entrypoint, cert-resolver or
   digest mismatch with the authorized route digest; local route-revision guard (refuse a revision below the
   stored one, record the new one before writing); write; Traefik API verify (`Observer.Verify`, polled while
   the reload is pending); HTTPS data-plane probe (`traefik.ProbeDataPlane`: literal loopback dial, SNI =
   hostname, full chain and hostname verification, Host header, bounded poll for first-deploy ACME); re-read
   the on-disk digest and compare.
4. **Durable `result_ready` record** (success with the on-disk digest, or a failure summary of at most 2048
   bytes carrying the step, reason and on-disk versus authorized digests), then the persistent reporter is
   kicked. The outcome handed back to the lease loop never causes `Complete` or a cancel acknowledgement.

Everything after step 1 runs under a context detached from lease cancellation, so post-authorization work
survives cancellation and a stale renewal. A failure is never rolled back: no route restore and no
`Complete(failed)`, which is this ADR's own blind-resolution rule. The agent never declares a reconciliation
outcome itself (owner decision D-B in `tasks/10d-plan.md`); the server blocks the operation and an operator
resolves it. The reporter never sets the agent-local `ReconciliationRequired` flag either (D-E).

### Persistent reporter and startup recovery (10d)

`ActivationReporter` delivers every `result_ready` record and retries (capped backoff 5s to 60s) across outages
and restarts. It deletes a record only on an allow-list of answers meaning the server holds the report or a
final decision: HTTP 200, `late_report_recorded`, `route_moved`, `integrity_conflict`. Every other 409
(`stale_lease`, `corrupt_state`, `constraint_violation`, unknown codes, and `operation_mismatch`) can follow a
server rollback that recorded nothing, so the record is kept and retried at the maximum backoff.
`RecoverActivations` runs once at agent startup, before any claim: every stranded `authorized` record becomes a
`result_ready` FAILURE carrying the on-disk digest (or `absent` / `unreadable`) as evidence, even when the
on-disk digest equals the authorized one.

### Predecessor drain (10e)

- **Arming.** A drain is armed only on HTTP 200 to a SUCCESS report of an activation that has a predecessor
  (`ActivationReporter` and the executor's direct-report fallback, via `state.DrainStore.ArmDrain`, saved before
  the activation record is deleted). No 409, even an allow-list one, ever arms it. The drain is an agent-local
  durable obligation OUTSIDE any lease (owner decision D-a): the success report's finalize closes the attempt,
  slot and job, so no lease exists afterwards.
- **Predecessor identity** comes from the control plane in the activate response (D-b):
  `predecessor_release_id` and `predecessor_runtime_instance_id`, empty on a first activation. The server
  derives them (`derivePredecessor`) from the release the route serves (`expected_active_release_id`) and the
  newest succeeded activate operation of that release, whose attempt id names the runtime instance. A
  predecessor with no succeeded activate operation, or one equal to the candidate release or runtime instance,
  is corrupt state and is refused.
- **Stop, never remove.** After the frozen `drain_seconds` (the wait resumes after a restart from the persisted
  `armed_at` and is bounded on the monotonic clock against wall-clock steps) the `Drainer` stops the container.
  The drain interface and the engine client have no remove or kill call. Guards, each failing closed (the
  predecessor keeps running and the record stays as evidence): identity sanity (target never equals the new
  release or runtime instance); the on-disk route digest must equal the digest the switch left; the container
  resolved by its deterministic name must be the single container matching the exact identity labels (managed,
  runtime kind, environment, release, runtime instance id) and must not carry the new release or instance;
  the stop targets the inspected container ID, not the name. Owner and server labels are deliberately NOT
  checked: the runtime instance id is the globally unique attempt id and neither label is in the drain record.
  The stop grace is whole seconds within 1..300 (`--drain-stop-grace`, default 30s; never 0, which would be an
  immediate SIGKILL). A container already stopped or gone discharges the obligation and the record is deleted.
- **Failures** are agent-local evidence only (D-c): the record becomes `stop_failed` with a bounded summary and
  a loud log; transient engine errors retry a bounded number of times, others are permanent. There is no
  control-plane record of a stop failure until slice 11. No tombstone is kept after a completed drain: a late
  re-arm only re-stops an already-stopped container (Engine 304 counts as success) and the route guard still
  applies.
- **Limits to remember.** The drainer (and the reporter and startup recovery) are wired only while the executor
  is enabled, so leftover drains wait while it is disabled. The route guard is checked before the stop, not
  during it; a route change inside the stop window is not re-checked (no second activation exists until slice
  11). If arming fails after a direct report the drain is lost and logged; the predecessor keeps running until
  an operator stops it. A 10d binary cannot read activation records written by 10e (strict JSON decode rejects
  the new fields): drain pending activations and drains before any downgrade.
- **Still slice 11.** Stop-then-start, retention and cleanup, rollback, and a server-side record of stop
  failures.

### Deployment log retention (step 15g)

- **Caps and retention.** Per attempt: 256 KiB and 4000 rows, then one truncation marker and no further rows.
  Chunks are purged when their attempt closed more than 14 days ago, but the newest 20 attempts per
  environment are always kept (both predicates must allow deletion).
- **Configuration.** `AUTODEPLOY_LOG_RETENTION_MAX_AGE` (default 336h, 1h..365d) and
  `AUTODEPLOY_LOG_RETENTION_KEEP_NEWEST` (default 20, 1..10000).
- **Schedule.** Purge runs at most every 10 minutes, in batches of 500 rows, at most 40 batches, within a 30 s
  budget, under its own advisory key (see ADR 0004). Container runtime logs and drain log lines are not part of
  this channel.

## Amendments (2026-11-12)

Step 16 (local commits fd9b8a1..2edaa98, plan 34b86e8, 16h pending commit); checked against the agent executor,
reporter, recovery and Traefik observer code. Nothing here has run on a real host.

### Recovery and drain hand-off (16c, 16d, 16e)

- **Recovery cadence (16c).** Stranded `authorized` activation records are converted to result_ready failure
  reports at startup and after every lease returns. No executor runs at that point: leases are serial and
  `runLease` waits for the executor on every exit path. The failure summary now reads "activation result was
  neither saved nor reported (agent restart or failed result save)" at step "recovery". The summary builder
  keeps digests intact and truncates only the reason (16d).
- **Drain hand-off (16e).** On the direct-report path (entered because the result save failed), if the report is
  acknowledged 200 but arming the predecessor drain fails, the executor re-saves the real result as result_ready
  success so the reporter owns it. The reporter re-reports (the server answers an identical success replay 200
  before any occupant check) and arms the drain through its durable retry. Only if that save also fails is the
  drain lost, logged loudly ("drain obligation LOST ... must be stopped by an operator").
- **Startup flag validation (16d).** `--activation-timeout` must be at least probe + verify + 30s when non-zero
  (one shared rule, `executor.ValidateActivationTimeout`); `--traefik-route-dir` must be writable.

### Traefik API authentication (16f)

- Optional `--traefik-api-credentials-file`: one `username:password` line in an owner-only (0600) regular file,
  read with `O_NOFOLLOW|O_NONBLOCK` under a size cap; never via argv or env. The observer sends HTTP basic auth
  only to the loopback base URL; redirects and proxies are refused; base URLs with userinfo are now rejected; the
  credential is redacted in all formatting. An absent flag keeps the unauthenticated loopback behaviour. This
  closes the code half of "authenticated management surface"; real Traefik behaviour is unverified.
- **Pre-authorization Traefik API check (step 17, 2026-11-12; replaces the earlier "known gap").** Before
  requesting authorization the executor calls `Observer.Preflight` (one GET `/api/version` through the same
  loopback-only, authenticated client). 401/403 (credentials rejected) and other 4xx (API not at the configured
  URL) fail the attempt as `permanent_configuration`; 408/425/429, 5xx, dial errors and timeouts fail it as
  `transient`; a non-object 200 or a refused redirect as `corrupt_state`. All of these happen before
  authorization, so no route is written and no reconciliation is needed. After the switch, router/service
  observation now classifies 401/403 as configuration, so Verify fails immediately instead of polling for the
  verify timeout (still reconciliation-required: the route was written). Unverified against a real Traefik.

### Amendment 2026-11-13 (step 20C): stop-then-start V1, no maintenance route

- **Scope.** The agent now honours `release_strategy = stop_then_start` (blue/green is unchanged). The
  maintenance-service prerequisite above is deferred: V1 has no maintenance route, so the outage is visible
  as 502/503 from Traefik for as long as the predecessor is stopped. Owner decision B-D2.
- **Sequence.** Build while the old release serves; report `health_checking` (an early label, kept so the
  state machine stays valid without a migration); Traefik preflight; server `Activate` (starts the authorized
  window); persist the activation record with `PredecessorStopSent=true`; identity-guarded stop of the
  predecessor (exact labels, never the candidate) and wait until stopped; create + start the candidate; health
  gate; then the unchanged switch -> verify -> probe -> report. The first deployment skips the stop.
- **No drain wait, no post-success drain.** Nothing is routed ahead of the stop, so there is no traffic to
  drain; `drain_seconds` is ignored for this strategy. Because the predecessor is already stopped, no drain is
  armed after success (`DrainFromActivation` owes nothing when `PredecessorStopSent`).
- **Failure policy (B-D3).** On a failure after the stop (stop error, candidate start, health gate, or a switch
  failure before the route file changed) the agent stops the candidate, then restarts the predecessor best
  effort (guarded identity), and reports an activation FAILURE whose summary states what happened (candidate
  state, restart ok/failed). The server blocks the environment; the operator declares the outcome. The agent
  never declares one. The predecessor is not restarted while the candidate could not be confirmed stopped
  (running both is what this strategy avoids). If the route file may already point at the candidate (on-disk
  digest equals the authorized one, or is unreadable), both containers are left as found.
- **Recovery.** A stranded `authorized` record with `PredecessorStopSent` triggers the same rollback (candidate
  stop, predecessor restart) under the same route-digest rule before the failure is reported; a record without
  the flag still means the host was untouched. The flag is the only activation field that may change on an
  existing record (false -> true once). It is omitted when false, so blue/green records keep their old shape;
  downgrade caveat: an older agent rejects a record carrying it as corrupt, so let such records report first.
- **Timeout.** The detached authorized-window deadline gains a fixed 8 minute allowance for stop, create,
  gate and rollback; `ValidateActivationTimeout` and blue/green timeouts are unchanged.
- **Switch window gate.** After the health gate, the agent refuses to write the route when the remaining
  authorized-window time is below `ProbeTimeout + verifyTimeout + 30s`; it rolls back (stop candidate, restart
  predecessor) and reports a failure instead of risking a switch cut off with the candidate serving. Rollback
  and the predecessor restart run on detached contexts, so an expired window cannot strand the predecessor
  down; the predecessor stop stays bound to the window (a stop cut off mid-way counts as unknown and is rolled
  back). The rollback outcome leads the failure reason so truncation cannot drop it.
- **Server window.** No server change: `ReportActivationResult` has no authority check by design, and the lease
  loop keeps renewing (authority = now+90s) while the executor runs; job cancel is refused while `activating`.
  A report is only late if renewals lapse, exactly as for blue/green (then `late_report_recorded`).
- **Coexistence.** A pending blue/green drain from an earlier deployment of the environment can briefly run
  alongside a stop-then-start deployment (the drain is a separate durable obligation). Restart policy: the
  permitted values are `unless-stopped` and `on-failure`, so Docker does not auto-restart the stopped
  predecessor; were `always` ever allowed, a daemon or host restart would resurrect the stopped predecessor
  next to the candidate.
- **Unverified.** Fakes only; not run against a real Docker engine or Traefik.

## References

- [Traefik file provider](https://doc.traefik.io/traefik/providers/file/)
- [Traefik configuration overview](https://doc.traefik.io/traefik/getting-started/configuration-overview/)
- [Traefik API and dashboard](https://doc.traefik.io/traefik/operations/dashboard/)
- [Traefik TLS overview](https://doc.traefik.io/traefik/reference/routing-configuration/http/tls/overview/)
- [Docker bridge networks](https://docs.docker.com/engine/network/drivers/bridge/)
- [Docker resource labels](https://docs.docker.com/engine/manage-resources/labels/)
- [Dockerfile health checks](https://docs.docker.com/reference/dockerfile/)
- [Docker Buildx metadata](https://docs.docker.com/reference/cli/docker/buildx/build/)
