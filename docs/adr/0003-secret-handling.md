# ADR 0003: Handle Secrets with Versioned Envelope Encryption

- Status: Accepted
- Date: 2026-08-05

## Context

AutoDeploy must store build and runtime values that are later materialized for an authorized
deployment. It also consumes bootstrap credentials such as the GitHub App private key, webhook
secret, PostgreSQL credential, and a future agent credential authority. These secret classes have
different lifecycle and recovery requirements.

Database encryption alone does not protect a database dump when the decryption key is available
inside the same database. Build and runtime delivery also cross the control-plane/agent boundary
and enter a Docker-capable trusted host where complete secrecy from administrators cannot be
promised.

This ADR defines storage, materialization, rotation, redaction, backup, deletion, and failure
invariants. Concrete schemas, APIs, and Docker/BuildKit adapters remain separate implementation
slices.

## Threat model and accepted boundaries

The design protects against:

- Disclosure of PostgreSQL data files, logical dumps, ordinary database backups, and application
  query access without the external key-encryption keys.
- Accidental disclosure through APIs, UI, audit events, logs, metrics, traces, command lines,
  durable jobs, image metadata, and ordinary build output.
- Cross-project, cross-environment, cross-server, cross-lease, or stale-version materialization.
- Partial rotation, missing keys, altered ciphertext or AAD-bound envelope fields, and unsafe
  credential files.

The design does not protect plaintext from:

- A compromised control-plane process or host while it can use the active key provider.
- A compromised Docker-capable agent or deployment host receiving an authorized secret.
- A trusted application after runtime materialization.
- A malicious trusted Dockerfile that copies, transforms, prints, or exfiltrates a mounted build
  secret.
- Root, debugger, core-dump, swap, or runtime memory inspection on a trusted host.

Co-locating the control plane and Docker-capable agent shares the physical host compromise
boundary as established by ADR 0001.

## Secret classification

### External provider credentials

These remain outside PostgreSQL and are not encrypted using a KEK stored with database data:

- Root KEK versions and active-key metadata.
- GitHub App private keys and webhook secrets.
- PostgreSQL client credentials.
- Control-plane and Traefik TLS/ACME private state where applicable.
- Backup-encryption and recovery keys.
- Future CA, signing, agent-bootstrap, and external-provider credentials.
- Future administrator bootstrap and session-signing credentials where required.

### Database-encrypted recoverable values

- Immutable build-secret versions.
- Immutable runtime-secret versions.
- Future reversible provider credentials only after explicit approval for database storage.

### One-way verifiers

Administrator passwords and bearer/session/agent tokens use approved password hashing or token
digests when verification or indexed lookup does not require plaintext recovery. Private signing
keys and mTLS private keys remain provider-held rather than hashed.

### Transient material

- GitHub App JWTs and installation access tokens.
- Plaintext DEKs and decrypted values.
- Lease-scoped agent materialization payloads.
- BuildKit secret-session values and runtime injection values.
- In-memory redaction dictionaries.

### Non-secret protected metadata

Stable scope IDs, names, purpose/scope, version, algorithm suite, KEK ID, timestamps, status,
ciphertext, wrapped DEK, revision, and audit events are not secret plaintext. Ciphertext and
wrapped keys remain excluded from ordinary UI/API responses.

## Decision

AutoDeploy uses application-layer, versioned envelope encryption for database-recoverable
secrets. V1 uses an external mounted-file KEK provider behind an interface that can later be
replaced by KMS, HSM, or Vault without changing stored envelope semantics.

Production does not accept bootstrap secret values from command-line arguments, repository files,
`.env` files, or process environment variables. Development adapters must be explicitly marked
non-production and cannot become a silent production fallback.

## Key hierarchy and envelope format

The KEK provider exposes a versioned ring:

- Exactly one active 256-bit AES KEK for new writes.
- Zero or more retiring KEKs allowed only for reads and controlled rotation.
- Stable, non-secret KEK IDs that never contain key material.

Every immutable secret version receives a fresh 256-bit DEK from `crypto/rand`.

The versioned `AES-256-GCM-RANDOMNONCE-v1` suite:

1. Encrypts the secret value under its DEK using a fresh random nonce.
2. Wraps the DEK under the active KEK using a separate fresh random nonce.
3. Authenticates canonical associated data for both operations.

The implementation may use Go's `cipher.NewGCMWithRandomNonce` when available in the approved Go
baseline. It must not reuse nonce/key pairs or substitute deterministic nonces.

Every KEK has a durable, monotonic wrap-invocation ledger. Before each KEK encryption operation,
the writer reserves one invocation using a transaction/CAS; reservations are never decremented or
reused, including after a failed write. New wrapping fails closed at `2^31` reserved invocations
and requires a new active KEK, remaining well below GCM's hard `2^32` random-nonce invocation
limit. A restored system that cannot prove ledger continuity makes the restored write KEK
read-only and activates a fresh KEK before accepting new writes.

Associated data has two mandatory schemas. Each field is encoded in the listed order as a
four-byte big-endian byte length followed by the exact UTF-8 bytes; empty or omitted fields are
invalid for stored build/runtime secrets.

Value-encryption AAD fields:

1. Operation label `autodeploy:secret-value:v1`.
2. Algorithm-suite ID.
3. AAD-format version.
4. Immutable owner ID.
5. Immutable project ID.
6. Immutable environment ID.
7. Immutable variable ID.
8. Immutable secret-version ID.
9. Secret-purpose enum.
10. Build/runtime-scope enum.

DEK-wrapping AAD fields:

1. Operation label `autodeploy:secret-dek-wrap:v1`.
2. Algorithm-suite ID.
3. AAD-format version.
4. KEK ID.
5. The same immutable owner, project, environment, variable, and secret-version IDs.
6. The same secret-purpose and build/runtime-scope enums.

The operation labels prevent cross-use of value and wrapping ciphertext. KEK ID is deliberately
absent from value AAD and mandatory in wrapping AAD so routine DEK rewrapping does not require
value re-encryption. Any future secret class needing different scope fields introduces a new
explicit AAD version rather than optional fields.

For this suite, each random 96-bit nonce is serialized as the prefix of its sealed ciphertext.
The value ciphertext and wrapped-DEK blob each carry their own nonce. Parsers reject truncated,
oversized, unknown-suite, or trailing-data representations.

Mutable display names are never part of authorization or required AAD reconstruction.

Persisted envelope metadata contains:

- Secret-version ID, purpose, scope, status, and revision/CAS metadata.
- Ciphertext and wrapped DEK.
- KEK ID, algorithm-suite ID, and AAD-format version.
- Creation, activation, retirement, and logical-deletion timestamps.

Do not persist equality hashes, previews, masked fragments, or explicit plaintext lengths. The
unavoidable approximate length leakage from ciphertext is accepted for V1.

Authorization occurs before decryption. Unknown key IDs, malformed envelopes, substitution of an
AAD-bound field, or AEAD failure are corruption/security errors. They never produce plaintext,
plaintext fallback, or a misleading "not configured" result.

Lifecycle status, revision, and timestamps are not cryptographically authenticated by the
envelope. Database constraints, authorization, CAS, and audit protect ordinary application access
but do not prevent a malicious database writer from altering those fields or replaying a complete
older envelope under the same immutable IDs. That stronger database-integrity threat is outside
V1 and must not be claimed as an AEAD guarantee.

## Immutable versions and materialization freeze point

- Setting or replacing a secret creates a new immutable encrypted version.
- Activation, retirement, and deletion use revision/CAS checks; ciphertext is not overwritten.
- Durable queued jobs contain authorized secret references, never plaintext.
- One database transaction claims the authorized agent lease and durably freezes every exact
  secret-version ID before any decryption or delivery occurs.
- Rotation before lease claim/freeze affects queued work; executing work retains its frozen
  versions unless compromise handling revokes them.
- Retry or lease reclamation for the same deployment reuses the already frozen version set; using
  newer values requires a distinct deployment intent.
- Releases retain version references for their configured rollback lifetime.
- Ordinary rotation applies to running applications only after a new deployment or restart.

Manual redeploy and rollback must record the exact version references used. A retired or deleted
version cannot be newly materialized unless an explicit recovery policy permits it.

## Rotation

Routine KEK rotation:

1. Add and validate a new KEK version.
2. Mark it active so all new writes use it.
3. Rewrap live DEKs transactionally in bounded, restartable CAS batches.
4. Verify that no live envelope or required backup references the old KEK.
5. Retire, and later remove, the old KEK according to backup retention policy.

Interrupted rotation leaves old and new keys readable and resumes idempotently.

If a KEK is compromised, immediately disable it for writes and restrict any remaining reads to
controlled incident recovery. Treat every secret version ever wrapped by that KEK as potentially
disclosed, including retired/deleted rows, WAL, replicas, dumps, and retained or stolen backups.
Inventory every affected historical copy and rotate or revoke every still-valid underlying
credential.

Recovery never rewrites value ciphertext under an existing immutable version ID. For every
currently recoverable value that must remain available, create a replacement immutable version
with a new version ID, new DEK, fresh value ciphertext, and the fresh active KEK; record a
sanitized `replaces_compromised_version` audit link and revoke the compromised version. Prefer a
new upstream credential value rather than merely re-encrypting an exposed value. Any queued,
claimed, retrying, or reclaimed deployment frozen to a revoked version fails/cancels and cannot
silently switch versions; a distinct deployment intent must freeze the replacements. Remove or
recreate running and retained containers carrying exposed values as described below.

Replacement versions and re-encryption do not repair ciphertext/key copies already held by an
attacker. Preserve a sanitized incident record and the current healthy release only where doing
so does not continue using a revoked credential.

GitHub App private-key and webhook-secret overlap follows ADR 0002. Other provider credentials
must define comparable issue, overlap, verification, revocation, and retirement procedures.

## Mounted-file credential provider

Production mounted credential files must satisfy all of these invariants:

- Absolute paths outside the repository and workspace.
- Dedicated unprivileged service identity and restrictive parent directories.
- Root/operator ownership where applicable; service-readable only and never group/world writable.
- A read-only trusted parent-directory mount when the service runs in a container, so reopening a
  safely replaced path observes the new inode.
- Regular bounded files only; reject directories, devices, sockets, FIFOs, symlinks, and magic
  links.
- Descriptor-relative, no-follow opening with post-open metadata verification on Linux; a
  pre-open path check alone is insufficient.
- Strict type-specific parsing with no value-bearing error messages.
- KEK material decodes to exactly 32 random bytes; PEM keys are parsed and fingerprint-checked;
  webhook-secret bytes are not silently trimmed or normalized.
- Missing, unreadable, empty, oversized, incorrectly owned, or incorrectly permissioned files fail
  startup/readiness closed.

Rotation uses same-filesystem atomic replacement: prepare a complete new regular file with final
ownership/permissions, sync it, rename atomically, sync the directory, reopen through the trusted
directory, reload explicitly, and verify the new non-secret version/fingerprint. A container bind
mount of one individual file remains attached to the old inode after host rename and is therefore
forbidden for live reload; such deployments must recreate/remount the service. Retain any bounded
old/new overlap required by the provider. The service must never observe a partially written file.

The KEK ring's active-key selection is non-secret configuration, but every referenced key file is
subject to these checks. Environment-variable fallback is forbidden in production.

## Agent authorization and materialization

Immediately before decryption and delivery, the control plane rechecks:

- Authenticated lease owner and unexpired lease revision.
- Immutable deployment, project, environment, and server scope.
- Secret purpose, active/frozen version status, and cancellation/revocation state.
- That the deployment requires each requested value.

Only required values cross the authenticated, confidentiality-protected agent channel with server
identity verification. Intermediaries must not log payloads or authorization material. Lease or
transport loss discards plaintext and requires complete reauthorization; stale materialization
results are rejected.

The agent installs its redactor before invoking checkout/build/runtime tools, holds plaintext only
in bounded short-lived memory, and never places it in durable jobs or persistent temporary files.
Memory clearing is best effort only and is not represented as guaranteed zeroization.

## Build-secret delivery

Build secrets use a BuildKit session secret provider with opaque IDs and Dockerfile
`RUN --mount=type=secret`. AutoDeploy must not downgrade to:

- Docker `ARG` or Dockerfile `ENV`.
- Build context files.
- Command-line arguments.
- Image labels, provenance, or persistent temporary files.

If BuildKit secret support is unavailable, the build fails closed. BuildKit reduces accidental
persistence in layers and history; it does not contain a hostile trusted Dockerfile.

## Runtime-secret delivery

V1 injects general runtime secret values directly through the Docker Engine API rather than CLI
arguments or `--env-file`. Values are therefore visible to Docker administrators and retained in
container configuration. That exposure is inside the accepted deployment-host trust boundary and
must be documented to operators.

Applications that support file credentials should eventually receive read-only `/run/secrets`
material. A general standalone-Docker file-secret adapter is deferred until tmpfs, permissions,
container lifecycle, and cleanup behavior receive a dedicated review.

Retained rollback containers retain their runtime configuration. Ordinary rotation does not erase
old values from them. Emergency compromise recovery revokes the upstream credential and removes
or recreates every retained container carrying it, accepting loss of rollback to those containers.

## Redaction, logging, APIs, UI, and audit

Redaction is defense in depth:

- The agent performs streaming redaction before transmission.
- The control plane redacts again before persistence, structured logging, API/UI output, metrics,
  traces, or diagnostic forwarding.
- Matching covers raw values and only deterministic encodings AutoDeploy itself creates.
- Matching works across output chunk boundaries.
- Transformed, partial, hashed, compressed, encrypted, or attacker-obfuscated values cannot be
  guaranteed detectable.
- Redactor initialization or processing failure suppresses affected output and emits only a
  sanitized diagnostic; it never passes data through.

Prohibited from logs and diagnostics:

- Plaintext values, ciphertext, wrapped keys, and redactor match content.
- Authorization headers, database URLs, request bodies, environment blocks, and command arguments.
- Credential-provider content, panic/core dumps, and value-bearing cryptographic errors.

Secret APIs and UI are write-only for values: set, replace, and delete. Reads return only
`configured`, immutable version ID, purpose/scope, status, and timestamps. There is no reveal
endpoint, prefix/suffix preview, length, or value-shaped placeholder. Omitted, empty, unchanged,
replace, and delete are distinct explicit operations.

Audit records include actor/service identity, immutable scope IDs, action, result, old/new version
IDs, KEK ID, deployment/lease correlation, and timestamp. They exclude plaintext, plaintext
hashes, ciphertext, wrapped keys, request bodies, and redactor matches. A secret mutation and its
audit record commit atomically or the mutation fails.

## Backup, restore, and KEK recovery

- Database backups contain only ciphertext and wrapped DEKs and are encrypted independently.
- Back up the KEK ring separately, offline or in a second protected system with narrower access.
- Never colocate the only KEK backup with the only database backup.
- External GitHub, database, agent, ACME, and provider credentials have separate backup or
  reissuance runbooks.
- Retain retiring KEKs until every required database backup is migrated or expires.
- Restore installs and validates required KEKs before enabling administrative traffic or agent
  jobs, then performs a non-logging decrypt canary.
- Restore and total-KEK-loss exercises are required before production readiness.

Losing every applicable KEK makes stored secrets unrecoverable. AutoDeploy preserves the current
healthy release where possible but blocks new secret materialization and deployment work.

The operator must select and test a separate KEK recovery location and database/KEK backup
retention policy before production deployment. Those operational values are intentionally not
guessed in this ADR.

## Deletion and compromise recovery

Normal deletion immediately makes secret versions non-materializable in the live system, waits for
queued, executing, and retained-release references according to approved retention policy,
deletes live ciphertext and wrapped DEKs, and retains only a non-value metadata tombstone. UI/API
expose a pending-retention state until historical copies containing wrapped DEKs have expired or
been destroyed.

PostgreSQL row deletion is not immediate physical erasure: old row versions, WAL, PITR, replicas,
and backups may retain both prior ciphertext and wrapped DEKs while applicable KEKs remain
available. AutoDeploy promises immediate logical non-materializability followed by eventual loss
of recoverability after all relevant copies and keys expire or are destroyed—not immediate
physical or cryptographic erasure.

Upstream revocation is the immediate response for a compromised external credential. Emergency
eradication also removes affected retained containers and rotates encryption/provider keys as
required.

## Failure invariants

- Missing or unsafe provider/KEK files: not ready; no decrypt, token mint, or deployment.
- Unknown/retired KEK, invalid metadata, or AEAD failure: sanitized security/corruption error; no
  fallback.
- Rotation interruption: maintain readable old/new keys and resume idempotently.
- Secret retirement before materialization: fail or cancel deployment and preserve active release.
- Lease or transport loss: discard plaintext and require fresh authorization.
- BuildKit secret facility unavailable: fail; never downgrade to ARG/ENV.
- Redaction failure: suppress affected logs.
- Audit persistence failure: reject the secret mutation.
- Runtime creation failure: remove scoped failed material/container and preserve active release.
- Restore without required KEKs: stop before processing administrative traffic or jobs.

## Minimal implementation boundaries

Future approved slices should preserve these interfaces or equivalent responsibility boundaries:

- Credential provider: load a bounded typed external credential plus non-secret version ID.
- Key provider: identify the active write KEK and resolve permitted decrypt KEK IDs.
- Envelope cipher: encrypt/decrypt using immutable typed scope/AAD input.
- Secret repository: immutable versions, CAS activation/retirement, and reference tracking.
- Secret materializer: authorize and resolve exact versions for one lease/server/deployment.
- Streaming redactor: chunk-boundary-safe, fail-closed transformation.
- Build-secret provider and runtime-secret injector: consume ephemeral material without durable
  plaintext serialization.

## Consequences

Benefits:

- Database compromise alone does not reveal stored secret plaintext.
- Per-version envelopes limit nonce/key reuse and support controlled rotation.
- Immutable versions make deployments and audits reproducible.
- Provider interfaces permit later KMS/HSM/Vault adoption.
- Explicit redaction and delivery boundaries reduce accidental exposure.

Costs:

- KEK custody, recovery, rotation, and backup compatibility become operational requirements.
- The control plane remains capable of decryption and is therefore a high-value trust boundary.
- Docker administrators and authorized applications can inspect runtime material.
- Retained releases complicate deletion and emergency rotation.
- Streaming redaction cannot detect arbitrary transformations or malicious exfiltration.

## Alternatives considered

- Plaintext PostgreSQL: rejected.
- PostgreSQL-side `pgcrypto`: rejected because decryption keys/plaintext enter the database trust
  boundary.
- One master key directly encrypting every value: rejected because it expands rotation blast
  radius and weakens per-version lifecycle control.
- Production environment variables or `.env` bootstrap secrets: rejected as an avoidable exposure
  and accidental logging boundary.
- Vault/KMS/HSM in V1: deferred because it adds an external bootstrap and availability dependency;
  the provider interface preserves migration to it.
- Permanent per-agent secret copies: rejected.
- Build `ARG` or `ENV`: rejected because values can persist in history, metadata, cache, or output.
- Swarm/Kubernetes secrets: deferred because those orchestrators are outside the V1 stack.
- Hash every secret: impossible for values AutoDeploy must later supply; hashing is used only when
  verification rather than recovery is sufficient.

## Deferred decisions

- SQL schema and repository implementation.
- HTTP representation, authorization handlers, and maximum value sizes.
- Password-hashing and session/token design for administrator authentication.
- Rotation batch sizes, schedule, retention periods, and operational dashboards.
- KMS/HSM/Vault adapters and sign-only GitHub key providers.
- BuildKit SDK, Docker Engine API, and agent transport implementations.
- Runtime file-secret support.
- Final KEK recovery medium/location and backup-retention duration.

## Validation criteria

- Known-answer, round-trip, and tamper tests cover ciphertext, wrapped DEK, both exact AAD schemas,
  key ID, algorithm, operation-domain separation, nonce serialization, and AAD-bound substitution.
- Property/statistical tests exercise random envelope generation without claiming proof of random
  uniqueness; deterministic tests enforce the per-KEK invocation budget and CAS-safe activation.
- Rotation tests cover interruption, mixed keys, rewrap, full compromise re-encryption, and key
  retirement guards.
- Unsafe file owner, mode, type, symlink, oversized content, and partial replacement fail closed.
- Rotation tests prove directory-mounted reload observes the replacement inode/value and that an
  individual-file bind mount requires service recreation.
- Database dumps, WAL-derived restore artifacts, API/UI/audit output, logs, metrics, traces,
  command lines, and durable jobs contain no plaintext.
- Streaming redaction tests split matches across every chunk boundary and cover fail-closed errors.
- Wrong project, environment, server, lease, purpose, or version cannot materialize a secret.
- Build tests prove AutoDeploy-owned transport and benign approved Dockerfile fixtures do not
  place secrets in image history, layers, cache metadata, provenance, or persisted output; hostile
  Dockerfiles remain capable of deliberate exfiltration and are tested/documented as a trust limit.
- Runtime tests document Docker-administrator visibility and retained-container cleanup.
- Backup restore succeeds with the separate KEK set and fails safely without it.
- Deletion tests cover live references, tombstones, WAL/backup retention, and upstream revocation.

## References

- [Go crypto/cipher package](https://pkg.go.dev/crypto/cipher)
- [Docker build secrets](https://docs.docker.com/build/building/secrets/)
- [Docker secrets in Compose](https://docs.docker.com/compose/how-tos/use-secrets/)
- [OWASP Secrets Management Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Secrets_Management_Cheat_Sheet.html)
- [OWASP Logging Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Logging_Cheat_Sheet.html)
- [PostgreSQL encryption options](https://www.postgresql.org/docs/18/encryption-options.html)
- [PostgreSQL routine vacuuming](https://www.postgresql.org/docs/18/routine-vacuuming.html)
- [PostgreSQL write-ahead logging](https://www.postgresql.org/docs/18/wal-intro.html)
