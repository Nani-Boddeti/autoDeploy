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
