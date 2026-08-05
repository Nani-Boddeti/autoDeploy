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

## References

- [Traefik file provider](https://doc.traefik.io/traefik/providers/file/)
- [Traefik configuration overview](https://doc.traefik.io/traefik/getting-started/configuration-overview/)
- [Traefik API and dashboard](https://doc.traefik.io/traefik/operations/dashboard/)
- [Traefik TLS overview](https://doc.traefik.io/traefik/reference/routing-configuration/http/tls/overview/)
- [Docker bridge networks](https://docs.docker.com/engine/network/drivers/bridge/)
- [Docker resource labels](https://docs.docker.com/engine/manage-resources/labels/)
- [Dockerfile health checks](https://docs.docker.com/reference/dockerfile/)
- [Docker Buildx metadata](https://docs.docker.com/reference/cli/docker/buildx/build/)
