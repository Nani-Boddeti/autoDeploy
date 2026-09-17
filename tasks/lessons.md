# Lessons

## PostgreSQL aggregate persistence

- Read aggregate heads and event streams inside one repeatable-read transaction; separate pool
  queries can observe different committed revisions.
- Serialize competing saves by locking the head row, then enforce revision CAS. Equal candidate
  revisions are conflicts, not idempotent successes.
- Hold a PostgreSQL advisory lock across migration ledger checks, DDL, and ledger writes using
  the same transaction/connection.
- Store migration checksums and reject changed files after application.
- Register cleanup immediately for tests that mutate shared database schema.
- CI integration targets must include every integration-tagged package, not only the repository
  package.
- Assert full aggregate snapshot equality after create/load; revision counts alone miss identity,
  timestamp, status, and event corruption.

## Administrator authentication architecture

- Strict session cookies need an explicit one-time same-origin handoff after cross-site provider
  callbacks; the callback itself must not mutate authorization.
- Durable abuse controls must bound attacker-controlled row and audit cardinality, including when
  cleanup is delayed.
- Versioned keyed throttle identifiers need overlap and retirement rules so key rotation cannot
  reset or split live limits.
- Calibrate password hashing once through an operator workflow and persist one shared policy;
  per-replica startup calibration creates divergent rehash behavior.
- Invalid forwarding chains from trusted proxies need isolated accounting and rejection, not a
  fallback that merges all clients into the proxy's normal throttle bucket.
- Reject stored password-policy revisions newer than the running policy; treating every mismatch
  as rehashable lets stale replicas request a downgrade.
- Build unknown-user dummy verifiers outside request handling and bind them to the exact current
  password policy; the request API must never expose dummy credential validity.
- Bound attacker-controlled strings before normalization, splitting, or base64 decoding; crypto
  parameter ceilings alone do not prevent parser allocation abuse.
- Digest-domain tests must use identical raw input in each domain or they do not prove separation.
- Exponential username backoff needs durable non-window state; carrying history through fixed
  windows breaks progression across boundaries, rotation aliases, and shared overflow.
- Cardinality limits must count every retained row globally under one cross-replica admission lock;
  counting only the current window makes delayed cleanup an unbounded-storage path.
- Reserve overflow capacity and read shared overflow counters before admitting work; write-only
  overflow buckets silently restore unlimited attempts.
- New password verifiers must match both the durable policy revision and exact Argon2 parameters;
  a revision label alone can disguise a weak legacy PHC string.
- Composite `(id, owner_id)` foreign keys prevent cross-owner session/audit corruption before
  application authorization runs.
- Execute evolving migrations against a fresh real database: checksum protection correctly rejects
  edited already-applied development migrations, and live constraints expose invalid fixtures and
  ordering bugs that compile-only tagged tests cannot.
- Foreign keys participate in lock ordering: a throttle transaction that writes a user reference
  can lock the user even without selecting it. Recovery must therefore take throttle admission
  before the user/session locks, and concurrency tests must exercise first-time FK tagging.
- Recovery cannot enumerate client-specific pair digests from a username. Persist a non-secret
  known-user recovery association when the pair is created; remove un-attributable pre-feature rows
  in an additive migration rather than pretending username digests can clear them.
- A secure mounted-file read needs same-descriptor pre/post identity and metadata validation, not
  only no-follow open plus a pre-read `fstat`; otherwise an in-place partial rewrite can be accepted.
- Reject non-interactive operator commands before loading credentials, connecting, migrating, or
  calibrating. Terminal-only is an execution boundary, not merely a password-reader check.
- Integration suites sharing one database must reset durable singleton configuration as well as
  ordinary rows; otherwise a second normal/race run inherits incompatible limits.
- Cookie `Expires` and `Max-Age` attributes are not returned to servers. A stateless pre-login
  cookie therefore needs an authenticated issued-at value (or server-side persistence) to enforce
  its lifetime consistently across replicas.
- An integration command exiting zero is not proof that integration ran. Use verbose output or an
  explicit required-environment preflight and record non-skipped test names before claiming live
  database coverage.
- Generic authentication redirects make status-only regression tests non-discriminating. Assert
  forbidden side effects too: no repository calls, authority minting, revocation, or cookie change.
- Streaming redaction bounds must cover derived-pattern expansion, candidate/output-link work, and
  counter overflow—not only source-byte transitions or retained-buffer size.
- Immutable bounded audit admission must dynamically reserve capacity for every currently active
  authority's future revocation, not consume a fixed lifecycle reserve. Edit an uncommitted migration
  only against a fresh database: checksum protection rejects changed already-applied migrations.
- A process running under the UID that owns a delegated cgroup subtree can move into sibling cgroups;
  use a distinct fixed payload UID/GID with no cgroup write access and atomic cgroup-FD admission.
- Preventive project quotas require helper-created, root-inaccessible workspaces and a durable
  mount-bound non-reusing ID ledger. Caller-created directories leave a pre-quota mutation window.
- FD-based protocols need one explicit owner per descriptor, exact direction/count/uniqueness checks,
  and durable indeterminate/replay states; compilation does not expose double-close or ACK-loss bugs.
- GitHub lifecycle payloads may carry repository arrays; normalize, bound, sort, and persist the full
  set atomically. A child-table immutability trigger must lock and recheck the parent terminal state,
  otherwise a concurrent insert can append after the receipt is sealed.
- HMAC rejection must not cross the database boundary. For verified malformed input, persist the
  sanitized rejection and return a retryable 5xx if that persistence fails so provider retry remains
  possible.
- Integration fixtures that derive identifiers from `t.Name()` need bounded deterministic IDs;
  nested descriptive subtest names can exceed production identifier limits only at live runtime.
- Never record "fresh PostgreSQL passed" evidence without the exact command output from a full
  integration run of every tagged package; focused runs hid four failures (2026-09-16).
- In standard (non-E) SQL strings backslashes are literal: write regex `\.`, not `\\.`. Prove every
  regex CHECK with a positive canonical value, not only rejections.
- Adding an FK to an existing table breaks other tests' TRUNCATE lists; grep all TRUNCATEs of the
  referenced table when adding a referencing table.
- Concurrency tests must force overlap after the real read (barrier that reads, then waits for both);
  releasing on arrival still lets the scheduler serialize, especially under -race.

## Server agent, credentials, and leases

- A same-host `https://` -> `http://` redirect still resends the `Authorization` header; an HTTP
  client used for bearer-credential requests must refuse to follow any redirect, not just
  cross-host ones.
- Auditing unauthenticated-request rejections (bad/missing/expired credential or token) gives an
  unauthenticated caller a way to drive unbounded audit-row write amplification; audit only
  authenticated mutations and authenticated-but-invalid operations, not failed authentication itself.
- Integration test packages that share one database must run with `go test -p 1` across every
  tagged package, and each package must apply/own its own migrations; running them as separate
  concurrent test binaries against the same live database produces nondeterministic failures.
- A per-key local state file is not enough to detect loss: without a separate manifest
  (`.fence-index.json`) recording which keys should exist, a missing file is indistinguishable from
  "never claimed anything for this environment," which silently defeats fail-closed fence recovery.
  A crash between writing the file and updating the manifest must also fail closed.
- A trigger that must be monotonic (e.g., a never-decreasing fence) cannot read a volatile
  `DEFAULT`/`now()`-style column for its comparison value; it must use the same captured
  transaction-time value the surrounding statement used, or concurrent rows can compare against
  different instants and let the monotonic guarantee slip.
- `SKIP LOCKED` only applies to explicit row locking clauses (`SELECT ... FOR UPDATE SKIP LOCKED`);
  it does not apply to `INSERT ... ON CONFLICT`, so slot/row pre-creation that might race on insert
  needs its own separate, deterministically ordered statement rather than relying on `SKIP LOCKED`
  semantics during the insert.
