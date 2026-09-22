-- Slice 10 ("health-gate configuration + blue/green activation"), Step 10a.
-- See tasks/todo.md "Approved Slice 10" -> "Step 10a -- route head + release
-- states", docs/adr/0005-release-strategies.md ("Deployment, release,
-- runtime, and operation model" for the route head's desired fields and the
-- release state machine, "Domains, TLS, and servers" for hostname
-- assignment/authority acknowledgement, "Immutable identity, names, and
-- labels" for why route documents carry their own revision metadata rather
-- than Docker labels), and docs/adr/0004-postgresql-queue-leases.md's global
-- row-lock hierarchy (Decision D5: route-head rows are inserted immediately
-- after deployment aggregate heads (hierarchy position 6) and before
-- secret-version/frozen-reference rows (position 7); this migration does not
-- edit that ADR text, and internal/store/postgres/route_head_repository.go
-- documents the same slot against the unmodified hierarchy in code).
--
-- Scope is schema-only, matching 000014's own precedent: no activation
-- transaction, no Traefik code, no agent wiring ships here. This step adds
-- exactly what Step 10a's plan calls for -- the route head table, its
-- append-only event log, an audit table for the one write this slice
-- exposes (hostname assignment), and the release state widening blue/green
-- activation needs -- and nothing else. releases has never had a production
-- row (tasks/todo.md: executorEnabled=false, and 000014/000016/000017's own
-- comments make the same claim about their tables), so every ALTER TABLE
-- below is safe additive schema surgery, not a data migration.
--
-- Same-environment enforcement for route_heads.active_release_id: a
-- composite FOREIGN KEY (active_release_id, environment_id) REFERENCES
-- releases(id, environment_id), backed by a new UNIQUE(id, environment_id)
-- constraint on releases (safe to add: releases.id is already a PRIMARY KEY,
-- so it is already unique on its own -- pairing it with environment_id can
-- never fail against any existing row). PostgreSQL foreign keys default to
-- MATCH SIMPLE, under which a NULL in any referencing column exempts the row
-- from the check entirely; active_release_id is the only nullable column in
-- the pair (environment_id is NOT NULL, it is route_heads' own primary key),
-- so the constraint is enforced exactly when it matters -- whenever
-- active_release_id is set -- and trivially satisfied while it is NULL. This
-- is a structural constraint the database itself enforces on every future
-- write, not a trigger that could be bypassed by a future direct SQL path;
-- it is chosen over a BEFORE INSERT/UPDATE trigger (like
-- validate_environment_runtime_binding_scope in 000014) because the
-- relationship here is a plain "these two columns must jointly exist as a
-- row elsewhere," which a composite FK expresses natively, unlike the
-- cross-table purpose/scope agreement 000014 needed a trigger for.
ALTER TABLE releases ADD CONSTRAINT releases_id_environment_id_key UNIQUE (id, environment_id);

-- Desired route state for one environment. One route per environment
-- (PRIMARY KEY environment_id, matching environment_runtime_configs'
-- shape). hostname is normalized to lowercase by the Go layer
-- (internal/healthgate.ValidRouteHostname / route_head_repository.go)
-- before it ever reaches this CHECK, which itself only accepts a lowercase
-- charset -- an uppercase byte is rejected here even if some future caller
-- forgot to normalize, so the UNIQUE constraint below can never be defeated
-- by case variation (e.g. "App.example.com" vs "app.example.com" landing as
-- two different rows). No port, no trailing dot: the charset has no ':' and
-- the repeating-label grammar requires the final label to end on an
-- alphanumeric, exactly like health_gate_host's grammar (000017) minus the
-- optional ":port" suffix that a Host-header probe target needs but a
-- routable production hostname does not.
--
-- Two further restrictions this CHECK adds beyond health_gate_host's
-- grammar, because a route hostname (unlike a Host-header probe target)
-- must be a name ACME/Let's Encrypt and the public DNS will actually issue
-- a certificate for and route to (ADR 0005 "Domains, TLS, and servers":
-- "V1 production application routing requires HTTPS" and derives
-- certificate domains from this hostname):
--
--   1. At least two labels (the pattern below requires one-or-more
--      "label." prefixes before the final label, not zero-or-more), so a
--      bare single-label name like "localhost" or "app" -- never a
--      routable public hostname -- is rejected structurally.
--   2. The final label must not be all-digits, checked separately via
--      substring(...) since RE2 (PostgreSQL's regex engine) has no
--      negative-lookahead to express this inside one pattern. This is the
--      standard heuristic for "not a bare IPv4 literal": every public DNS
--      TLD is all-letters (some IDN TLDs are punycode "xn--..." labels,
--      still not all-digits), while every dotted-quad IPv4 literal's last
--      octet is all-digits by construction -- so "1.2.3.4" and
--      "192.168.0.1" are rejected by this rule alone, without needing a
--      separate IPv4-literal parser. IPv6 literals are already excluded by
--      the charset (no ':' or '[' ']').
--
-- route_revision is a CAS counter exactly like
-- environment_runtime_configs.revision (000014): every UPDATE must advance
-- it by exactly one, enforced below by the same trigger shape as
-- validate_environment_runtime_config_update.
--
-- active_release_id is nullable and, in this slice, never set by any code
-- path -- Step 10a ships no activation logic (tasks/todo.md: "no activation
-- logic" is explicit scope). It exists now so the column, its FK, and the
-- CAS trigger's hostname-immutability guard (below) are all in place before
-- Step 10c's fenced activation transactions start writing it.
--
-- reconciliation_required mirrors environment_slots' own flag (000013):
-- Step 10c/10d fenced activation failures mark it true and this slice's
-- scope note says resolution is the existing operator "mark-reconciled"
-- flow, not new code here.
CREATE TABLE route_heads (
  environment_id TEXT PRIMARY KEY CHECK (octet_length(environment_id) BETWEEN 1 AND 128),
  hostname TEXT NOT NULL UNIQUE CHECK (
    octet_length(hostname) BETWEEN 1 AND 253 AND
    hostname ~ '^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$' AND
    substring(hostname from '[^.]+$') !~ '^[0-9]+$'
  ),
  route_revision BIGINT NOT NULL DEFAULT 0 CHECK (route_revision >= 0),
  active_release_id TEXT,
  reconciliation_required BOOLEAN NOT NULL DEFAULT false,
  created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  CHECK (updated_at >= created_at),
  CONSTRAINT route_heads_active_release_env_fk FOREIGN KEY (active_release_id, environment_id)
    REFERENCES releases(id, environment_id) ON DELETE RESTRICT
);

-- Same CAS shape as validate_environment_runtime_config_update (000014),
-- plus one Step 10a policy this slice's Go layer must not be able to bypass
-- via a future direct-SQL path: once a route has an active release, its
-- hostname can never change through this table alone -- a live route change
-- requires the agent to move traffic first (deferred to Step 10c/10d), so
-- AssignHostname refuses it at the application layer (typed error) and this
-- trigger refuses it again at the database layer as defense in depth,
-- exactly the same two-layer discipline health_gate_path/health_gate_host
-- already use (CHECK mirrored independently in Go).
CREATE FUNCTION validate_route_head_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
  IF NEW.environment_id <> OLD.environment_id THEN
    RAISE EXCEPTION 'route head identity is immutable';
  END IF;
  IF NEW.route_revision <> OLD.route_revision + 1 THEN
    RAISE EXCEPTION 'route head revision must advance by exactly one; expected % got %', OLD.route_revision + 1, NEW.route_revision;
  END IF;
  IF NEW.updated_at < OLD.updated_at THEN
    RAISE EXCEPTION 'route head updated_at must be monotonic';
  END IF;
  IF OLD.active_release_id IS NOT NULL AND NEW.hostname <> OLD.hostname THEN
    RAISE EXCEPTION 'route head hostname cannot change while a release is active';
  END IF;
  RETURN NEW;
END; $$;
CREATE TRIGGER route_heads_validate_update BEFORE UPDATE ON route_heads FOR EACH ROW EXECUTE FUNCTION validate_route_head_update();
CREATE FUNCTION reject_route_head_delete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'route heads are retained'; END; $$;
CREATE TRIGGER route_heads_no_delete BEFORE DELETE ON route_heads FOR EACH ROW EXECUTE FUNCTION reject_route_head_delete();

-- Append-only route event log: the durable, per-environment history of
-- route-head changes. This slice only ever appends 'hostname_assigned'
-- events; 'activated' is named now (like release_operations named
-- 'activate'/'rollback'/'retire'/'cleanup' ahead of use in 000014) so Step
-- 10c's fenced activation transactions need no further migration to start
-- recording their own route-revision advances here.
CREATE TABLE route_events (
  id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  environment_id TEXT NOT NULL REFERENCES route_heads(environment_id) ON DELETE RESTRICT,
  event_kind TEXT NOT NULL CHECK (event_kind IN ('hostname_assigned', 'activated')),
  route_revision BIGINT NOT NULL CHECK (route_revision >= 0),
  hostname TEXT CHECK (hostname IS NULL OR octet_length(hostname) BETWEEN 1 AND 253),
  active_release_id TEXT REFERENCES releases(id) ON DELETE RESTRICT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  CHECK ((event_kind = 'hostname_assigned') = (hostname IS NOT NULL)),
  CHECK ((event_kind = 'activated') = (active_release_id IS NOT NULL))
);
CREATE INDEX route_events_environment_idx ON route_events(environment_id, created_at);
CREATE FUNCTION reject_route_event_mutation() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'route events are append-only'; END; $$;
CREATE TRIGGER route_events_no_update BEFORE UPDATE ON route_events FOR EACH ROW EXECUTE FUNCTION reject_route_event_mutation();
CREATE TRIGGER route_events_no_delete BEFORE DELETE ON route_events FOR EACH ROW EXECUTE FUNCTION reject_route_event_mutation();

-- Audit trail for the one route_heads mutation this slice exposes
-- (hostname assignment), mirroring environment_runtime_config_audit_events
-- (000016) exactly -- same actor/owner/result/reason shape, append-only via
-- the same trigger pattern. A hostname is not a secret (ADR 0005: "Route
-- documents ... contain no secret values"), so recording it here in plain
-- text is intentional and matches route_events above.
CREATE TABLE route_head_audit_events (
  id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  actor_type TEXT NOT NULL CHECK (actor_type = 'administrator'),
  actor_id TEXT NOT NULL CHECK (octet_length(actor_id) BETWEEN 1 AND 128),
  owner_id TEXT NOT NULL CHECK (octet_length(owner_id) BETWEEN 1 AND 128),
  environment_id TEXT NOT NULL CHECK (octet_length(environment_id) BETWEEN 1 AND 128),
  action TEXT NOT NULL CHECK (action = 'hostname_assigned'),
  hostname TEXT CHECK (hostname IS NULL OR octet_length(hostname) BETWEEN 1 AND 253),
  resulting_revision BIGINT CHECK (resulting_revision IS NULL OR resulting_revision >= 0),
  result TEXT NOT NULL CHECK (result IN ('success', 'failure')),
  reason TEXT NOT NULL DEFAULT '' CHECK (octet_length(reason) <= 512 AND reason !~ '[\x00-\x1f\x7f]'),
  created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  CHECK ((result = 'success') = (resulting_revision IS NOT NULL))
);
CREATE INDEX route_head_audit_events_environment_idx ON route_head_audit_events(environment_id, created_at);
CREATE FUNCTION reject_route_head_audit_event_mutation() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'route head audit events are append-only'; END; $$;
CREATE TRIGGER route_head_audit_events_no_update BEFORE UPDATE ON route_head_audit_events FOR EACH ROW EXECUTE FUNCTION reject_route_head_audit_event_mutation();
CREATE TRIGGER route_head_audit_events_no_delete BEFORE DELETE ON route_head_audit_events FOR EACH ROW EXECUTE FUNCTION reject_route_head_audit_event_mutation();

-- Widen releases.state (000014) with 'active' and 'retained' and the three
-- new edges ADR 0005 names for first activation and rollback:
-- ready->active, active->retained, retained->active. 000014's own comment
-- said this migration would do exactly this ("active/retained do not exist
-- yet (the activation slice adds them)"); 000014 itself is never edited --
-- this DROP/ADD CONSTRAINT and CREATE OR REPLACE FUNCTION are new
-- statements in a new migration, matching 000013's precedent for widening
-- agent_jobs.state the same way.
--
-- Decision: 'active'/'retained' -> 'removing' is NOT added here, even
-- though ADR 0005 ("Deployment, release, runtime, and operation model")
-- separately describes "failed | retained -> removing: retention cleanup
-- after protection/reference checks" as part of the eventual full state
-- machine. tasks/todo.md's Slice 10 "Scope" section explicitly defers
-- "retention/cleanup batches" to slice 11+, and no code in this slice (or
-- Step 10a's approved scope) ever performs a reference/protection check
-- before a removal -- the check the ADR's own sentence conditions the edge
-- on. Adding a reachable 'retained'->'removing' edge now, with no cleanup
-- transaction anywhere in the codebase that enforces the ADR's protection
-- check before taking it, would let some future direct write silently
-- delete-eligible a retained release with no safeguard in place. The ADR
-- is not silent on the destination edge, but it is inseparable from
-- cleanup logic this slice does not ship; the edge is deferred alongside
-- that logic, not added speculatively. 'active'->'removing' is never
-- legal at all: ADR 0005 states plainly that "`active` cannot transition
-- directly to `removing`; routing must first move safely to another
-- release."
ALTER TABLE releases DROP CONSTRAINT releases_state_check;
ALTER TABLE releases ADD CONSTRAINT releases_state_check CHECK (state IN ('candidate', 'ready', 'failed', 'removing', 'removed', 'active', 'retained'));

CREATE OR REPLACE FUNCTION validate_release_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
  IF NEW.id <> OLD.id OR NEW.deployment_id <> OLD.deployment_id OR NEW.environment_id <> OLD.environment_id
     OR NEW.config_digest <> OLD.config_digest OR NEW.commit_sha <> OLD.commit_sha
     OR NEW.strategy <> OLD.strategy OR NEW.created_at <> OLD.created_at THEN
    RAISE EXCEPTION 'release identity is immutable';
  END IF;
  IF OLD.image_digest IS NOT NULL AND NEW.image_digest IS DISTINCT FROM OLD.image_digest THEN
    RAISE EXCEPTION 'release image digest is write-once';
  END IF;
  IF NEW.state = 'ready' AND NEW.image_digest IS NULL THEN
    RAISE EXCEPTION 'release cannot become ready without an image digest';
  END IF;
  IF OLD.state IN ('failed','removed') THEN
    RAISE EXCEPTION 'release is terminal in state %', OLD.state;
  END IF;
  IF NEW.state <> OLD.state AND NOT (
    (OLD.state = 'candidate' AND NEW.state IN ('ready','failed','removing')) OR
    (OLD.state = 'ready' AND NEW.state IN ('failed','removing','active')) OR
    (OLD.state = 'removing' AND NEW.state = 'removed') OR
    (OLD.state = 'active' AND NEW.state = 'retained') OR
    (OLD.state = 'retained' AND NEW.state = 'active')
  ) THEN
    RAISE EXCEPTION 'invalid release transition % -> %', OLD.state, NEW.state;
  END IF;
  IF NEW.updated_at < OLD.updated_at THEN
    RAISE EXCEPTION 'release updated_at must be monotonic';
  END IF;
  RETURN NEW;
END; $$;

-- At most one 'active' release per environment (ADR 0005: exactly one route
-- head active pointer per environment implies exactly one active release).
-- A partial unique index, not a CHECK, because the invariant spans rows.
CREATE UNIQUE INDEX releases_environment_active_idx ON releases(environment_id) WHERE state = 'active';

-- Structural invariant tying route_heads.active_release_id to a release
-- actually in 'active' state, in both directions:
--
--   (a) a non-NULL route_heads.active_release_id must reference a release
--       whose state is 'active';
--   (b) a release whose state is NOT 'active' must not be referenced by any
--       route head's active_release_id.
--
-- These are enforced as two DEFERRABLE INITIALLY DEFERRED constraint
-- triggers -- checked at COMMIT, not per-statement -- because Step 10c's
-- fenced finalization transaction performs, in one transaction: predecessor
-- active->retained, candidate ready->active, then repoints the head's
-- active_release_id and advances its route_revision. Every intermediate
-- statement in that sequence momentarily violates one half of this
-- invariant (e.g. immediately after the predecessor becomes 'retained' but
-- before the head is repointed, the head still names a non-active release;
-- immediately after the head is repointed but before the predecessor's
-- state settles, ordering could differ) -- only the state at COMMIT matters.
-- An IMMEDIATE (per-statement) trigger would make that legitimate,
-- correctly-ordered-at-commit sequence impossible to express in one
-- transaction. This mirrors 000004's validate_current_secret_version_state
-- / secret_versions_current_state_after_update, which enforces the same
-- "only a row in the required state may be referenced as current" shape
-- for secret_versions/secret_heads.current_version_id, deferred for the
-- same reason (a version rotation and the head repoint that follows it
-- happen in one transaction there too).
--
-- These are independent of, and in addition to, the composite FK above:
-- the FK only proves cross-environment identity; it says nothing about
-- release *state*, and a FK cannot express "must currently equal 'active'"
-- or the converse ownership-release-side check at all.
-- Both functions below deliberately re-read their target row(s) fresh via a
-- plain SELECT rather than trusting the trigger's own NEW value. This
-- matters specifically because these are DEFERRABLE triggers: if the same
-- row is written more than once for the same column in one transaction
-- (e.g. a release walking candidate->ready->active, or a route head's
-- active_release_id being set and later re-pointed), PostgreSQL queues one
-- deferred firing per write, and ALL of them execute at COMMIT -- not just
-- the last. A firing that trusted its own captured NEW value would validate
-- against a value that is no longer current by commit time (e.g. the
-- candidate->ready firing's NEW.state='ready' is stale once that same row
-- later reaches 'active' before commit), producing a false rejection on a
-- transaction that is actually consistent at commit. Re-reading the row
-- fresh inside the function body means every firing -- however many queue
-- up for the same row -- evaluates the same final, as-of-commit state, so
-- the result is independent of how many times the row was written earlier
-- in the same transaction.
-- validate_route_head_active_release's SELECT on releases below takes a
-- FOR SHARE row lock, not a plain read. Without it there is a write-skew
-- window: transaction T1 points a route head at release R1 (still 'active')
-- and passes this check (a plain, unlocked SELECT sees R1 as 'active', its
-- own snapshot as of that statement); concurrently, transaction T2 updates
-- R1 to 'retained' and passes releases_active_reference_after_update's own
-- check (a plain, unlocked EXISTS sees no committed reference yet, since
-- T1 hasn't committed) -- each transaction validates against a snapshot
-- that does not yet reflect the other's still-uncommitted write, and both
-- commit, leaving the head pointing at a retained release. This was proven
-- live against PG18 (both orderings) before this fix existed, and is
-- reproduced permanently by TestRouteHeadActiveReleaseWriteSkewPrevented in
-- internal/store/postgres/route_head_repository_test.go.
--
-- FOR SHARE forces R1's row lock to participate in whichever transaction
-- gets there first: an UPDATE of releases.state (T2) cannot proceed while
-- another transaction (T1) holds FOR SHARE on that same row, and conversely
-- a FOR SHARE acquisition (T1) cannot proceed while another transaction
-- (T2) holds the row's write lock from an in-progress UPDATE. Either
-- ordering therefore serializes through this one row:
--
--   * T1 checks first: it acquires FOR SHARE while R1 is still 'active' and
--     passes, holding the lock until it commits or rolls back. T2's UPDATE
--     of R1 then blocks until T1 finishes. If T1 commits, T2's UPDATE
--     proceeds and its own deferred check (still to come, at T2's own
--     SET CONSTRAINTS/COMMIT) now sees the just-committed route head
--     reference and raises.
--   * T2's UPDATE happens first: T1's FOR SHARE blocks until T2 finishes.
--     If T2 commits, T1's FOR SHARE then observes the committed 'retained'
--     state and raises in this function directly.
--
-- In both orderings exactly one of the two transactions fails; the
-- surviving one's write is the only one ever visible after both have
-- settled, so the invariant can never be observed broken by a concurrent
-- reader either.
--
-- The route_heads table itself needs no analogous lock on the release
-- side (releases_active_reference_after_update's EXISTS query below is
-- deliberately left as a plain, unlocked read). At most one route_heads
-- row can ever reference a given release: the composite FK above ties
-- active_release_id to (id, environment_id), and route_heads.environment_id
-- is that table's own primary key, so only the single route head for
-- exactly R1's environment could ever name R1. The FOR SHARE lock on R1
-- above is therefore already the one synchronization point both
-- transactions must pass through -- T2's UPDATE of R1 and T1's read of R1
-- conflict directly on that same row, which is what forces the ordering.
-- Locking route_heads as well would add a second lock acquisition with no
-- additional write-skew window to close: by the time
-- releases_active_reference_after_update's EXISTS check runs (after T2's
-- own UPDATE of R1, which by Postgres's ordinary MVCC rules already holds
-- R1's row lock for the rest of T2's transaction), the relative ordering
-- between T1 and T2 has already been fully decided by that R1 lock
-- conflict -- there is no remaining window in which the EXISTS check could
-- observe a "stale mid-air" route_heads value that FOR SHARE on R1 has not
-- already serialized against.
CREATE FUNCTION validate_route_head_active_release() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE current_active_release_id TEXT;
DECLARE target_state TEXT;
BEGIN
  SELECT active_release_id INTO current_active_release_id FROM route_heads WHERE environment_id = NEW.environment_id;
  IF current_active_release_id IS NULL THEN
    RETURN NEW;
  END IF;
  SELECT state INTO target_state FROM releases WHERE id = current_active_release_id FOR SHARE;
  IF NOT FOUND OR target_state <> 'active' THEN
    RAISE EXCEPTION 'route head active_release_id % does not reference an active release', current_active_release_id;
  END IF;
  RETURN NEW;
END; $$;
CREATE CONSTRAINT TRIGGER route_heads_active_release_after_write
AFTER INSERT OR UPDATE OF active_release_id ON route_heads DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION validate_route_head_active_release();

CREATE FUNCTION validate_release_not_referenced_unless_active() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE current_state TEXT;
BEGIN
  SELECT state INTO current_state FROM releases WHERE id = NEW.id;
  IF current_state <> 'active' AND EXISTS (SELECT 1 FROM route_heads WHERE active_release_id = NEW.id) THEN
    RAISE EXCEPTION 'release % is referenced by a route head but is not active', NEW.id;
  END IF;
  RETURN NEW;
END; $$;
CREATE CONSTRAINT TRIGGER releases_active_reference_after_update
AFTER UPDATE OF state ON releases DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION validate_release_not_referenced_unless_active();
