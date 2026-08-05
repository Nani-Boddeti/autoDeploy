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
