-- Slice 10, Step 10c-1 ("fenced activation, control plane" -- schema half).
-- See tasks/todo.md "Step 10c -- fenced activation, control plane" and its
-- "orchestrator decisions on the design (2026-09-22)" section (binding,
-- overrides the draft design below it wherever the two conflict),
-- docs/adr/0004-postgresql-queue-leases.md's 2026-09-22 amendment (route
-- head/release lock class), and docs/adr/0005-release-strategies.md
-- ("Fenced activation and reconciliation"). Scope is schema-only, matching
-- 000014/000018's own precedent: 10c-1 widens release_operations/
-- agent_job_events for the new POST /agent/v1/leases/activate transaction
-- (tx1) this step's Go half adds; 10c-2 (the separate /activation-result
-- transaction, the Complete guard, and the sweeper diversion) is a later
-- step and adds its own migration if it needs one.
--
-- release_operations (000014) is reused rather than reinvented: it has zero
-- application writers today (grep -rln release_operations over non-test
-- .go files: no hits, confirmed again before writing this migration), so
-- every ALTER TABLE below is additive schema surgery, not a data migration,
-- exactly like every other 000014/000018 table at this point in the
-- project's history.

-- New columns tx1 needs. environment_id is denormalized from
-- releases.environment_id (validate_release_operation_insert below
-- enforces the match) purely so a partial unique index can express "at
-- most one open activation per environment" without a cross-table join
-- (Postgres partial indexes cannot join). expected_active_release_id is
-- nullable independent of kind (first deployment has no predecessor to
-- expect -- see route_heads.active_release_id's own nullability);
-- expected_route_revision and route_digest are NOT NULL exactly when
-- kind='activate', enforced by the two CHECKs below. digest (existing,
-- NOT NULL for every kind already) holds the presented candidate image
-- digest for an 'activate' row -- the table's own header comment already
-- calls this column the operation's "canonical digest", and an activate
-- operation's canonical digest is the image it is switching traffic to.
ALTER TABLE release_operations ADD COLUMN environment_id TEXT CHECK (environment_id IS NULL OR octet_length(environment_id) BETWEEN 1 AND 128);
ALTER TABLE release_operations ADD COLUMN expected_active_release_id TEXT REFERENCES releases(id) ON DELETE RESTRICT;
ALTER TABLE release_operations ADD COLUMN expected_route_revision BIGINT CHECK (expected_route_revision IS NULL OR expected_route_revision >= 0);
-- route_digest is the control-plane-computed traefik.Digest(...) of the
-- route document tx1 rendered (Step 10c orchestrator decision: the control
-- plane renders and digests, not the agent -- see tasks/todo.md). It is
-- stored raw (32 bytes), not as the "sha256:<hex>" display string
-- internal/runtime/traefik.Digest returns; the Go layer strips/reapplies
-- that prefix at the boundary.
ALTER TABLE release_operations ADD COLUMN route_digest BYTEA CHECK (route_digest IS NULL OR octet_length(route_digest) = 32);

ALTER TABLE release_operations ADD CONSTRAINT release_operations_activate_environment_check CHECK ((kind = 'activate') = (environment_id IS NOT NULL));
ALTER TABLE release_operations ADD CONSTRAINT release_operations_activate_route_revision_check CHECK ((kind = 'activate') = (expected_route_revision IS NOT NULL));
ALTER TABLE release_operations ADD CONSTRAINT release_operations_activate_route_digest_check CHECK ((kind = 'activate') = (route_digest IS NOT NULL));

-- At most one open ('switch_authorized') activation per environment. This
-- is a second, structural line of defense on top of the application-level
-- ADR 0004 lock ordering (route_heads FOR UPDATE, position "6b" per the
-- 2026-09-22 ADR amendment below): even a future direct-SQL or
-- differently-ordered writer can never leave two open activations for the
-- same environment, because the second INSERT simply fails the index.
CREATE UNIQUE INDEX release_operations_environment_open_activation_idx ON release_operations(environment_id) WHERE kind = 'activate' AND state = 'switch_authorized';

-- State widening: switch_authorized (open, tx1's own terminal insert state
-- for an 'activate' row -- there is no separate "started, not yet
-- authorized" moment for this kind, see tasks/todo.md Step 10c design §2)
-- and reconciliation_required (closed, terminal; 10c-2's failure/ambiguous
-- path and the expiry-sweeper diversion both close to this state). The
-- inline, unnamed CHECKs 000014 wrote for this table were introspected
-- against a live database before this migration was written (do not trust
-- the design draft's guessed names): the state enum is
-- release_operations_state_check (matches the draft), but the
-- "(state='started') = (closed_at IS NULL)" CHECK is actually named
-- release_operations_check1, not release_operations_closed_at_check as
-- the design draft assumed -- Postgres's default naming for a second
-- unnamed table-level CHECK on the same table.
ALTER TABLE release_operations DROP CONSTRAINT release_operations_state_check;
ALTER TABLE release_operations ADD CONSTRAINT release_operations_state_check CHECK (state IN ('started','succeeded','failed','switch_authorized','reconciliation_required'));
ALTER TABLE release_operations DROP CONSTRAINT release_operations_check1;
ALTER TABLE release_operations ADD CONSTRAINT release_operations_check1 CHECK ((state IN ('started','switch_authorized')) = (closed_at IS NULL));

-- validate_release_operation_insert (000014) additionally validates
-- environment_id against releases.environment_id when it is present, using
-- the same no-lock reasoning 000014's own comment already gives for the
-- fence/job_id check just above it: releases.environment_id is immutable
-- (validate_release_update, 000014/000018), so an unlocked SELECT cannot
-- observe a value that later changes out from under this check.
CREATE OR REPLACE FUNCTION validate_release_operation_insert() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  attempt_fence BIGINT;
  attempt_job_id BIGINT;
  release_environment_id TEXT;
BEGIN
  SELECT fence, job_id INTO attempt_fence, attempt_job_id FROM agent_job_attempts WHERE id = NEW.attempt_id;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'release operation attempt % does not exist', NEW.attempt_id;
  END IF;
  IF NEW.fence <> attempt_fence THEN
    RAISE EXCEPTION 'release operation fence % does not match attempt % fence %', NEW.fence, NEW.attempt_id, attempt_fence;
  END IF;
  IF NEW.job_id <> attempt_job_id THEN
    RAISE EXCEPTION 'release operation job_id % does not match attempt % job_id %', NEW.job_id, NEW.attempt_id, attempt_job_id;
  END IF;
  IF NEW.environment_id IS NOT NULL THEN
    SELECT environment_id INTO release_environment_id FROM releases WHERE id = NEW.release_id;
    IF NOT FOUND OR release_environment_id <> NEW.environment_id THEN
      RAISE EXCEPTION 'release operation environment_id % does not match release % environment', NEW.environment_id, NEW.release_id;
    END IF;
  END IF;
  RETURN NEW;
END; $$;

-- validate_release_operation_update becomes kind-aware: 'activate' rows
-- open at switch_authorized (never 'started') and close to succeeded or
-- reconciliation_required; every other kind keeps exactly its original
-- started -> {succeeded,failed} behavior, unchanged. 10c-1 never performs
-- an UPDATE on this table itself (tx1 only inserts, already as
-- switch_authorized) -- this widening ships now, ahead of 10c-2's own use
-- of the switch_authorized -> {succeeded,reconciliation_required} edges,
-- matching 000014/000018's repeated precedent of naming a kind/state
-- literal one step ahead of the code that first writes it. The four new
-- columns are added to the immutable-identity check alongside the
-- pre-existing ones, so a future writer cannot silently repoint an
-- activation's expectations after the row is created.
CREATE OR REPLACE FUNCTION validate_release_operation_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
  IF NEW.id <> OLD.id OR NEW.release_id <> OLD.release_id OR NEW.job_id <> OLD.job_id OR NEW.attempt_id <> OLD.attempt_id
     OR NEW.fence <> OLD.fence OR NEW.kind <> OLD.kind OR NEW.digest <> OLD.digest
     OR NEW.created_at <> OLD.created_at
     OR NEW.environment_id IS DISTINCT FROM OLD.environment_id
     OR NEW.expected_active_release_id IS DISTINCT FROM OLD.expected_active_release_id
     OR NEW.expected_route_revision IS DISTINCT FROM OLD.expected_route_revision
     OR NEW.route_digest IS DISTINCT FROM OLD.route_digest THEN
    RAISE EXCEPTION 'release operation identity is immutable';
  END IF;
  IF OLD.kind = 'activate' THEN
    IF OLD.state <> 'switch_authorized' THEN
      RAISE EXCEPTION 'release operation % is closed', OLD.state;
    END IF;
    IF NEW.state NOT IN ('succeeded','reconciliation_required') THEN
      RAISE EXCEPTION 'activate release operation must close to succeeded or reconciliation_required';
    END IF;
  ELSE
    IF OLD.state <> 'started' THEN
      RAISE EXCEPTION 'release operation % is closed', OLD.state;
    END IF;
    IF NEW.state NOT IN ('succeeded','failed') THEN
      RAISE EXCEPTION 'release operation must close to succeeded or failed';
    END IF;
  END IF;
  RETURN NEW;
END; $$;

-- agent_job_events (000013) widened with the one new event literal 10c-1
-- itself appends (tx1's own audit trail entry once it authorizes an
-- activation). 10c-2's own 'activated'/'activation_reconciliation_required'
-- literals are deliberately NOT added here -- 10c-1 does not implement
-- 10c-2, and widening this CHECK again later is the same cheap, additive
-- surgery this migration itself performs on it now. The real constraint
-- name (agent_job_events_event_type_check) was confirmed by introspecting
-- a live database, not assumed from the design draft.
ALTER TABLE agent_job_events DROP CONSTRAINT agent_job_events_event_type_check;
ALTER TABLE agent_job_events ADD CONSTRAINT agent_job_events_event_type_check CHECK (event_type IN ('enqueued','claimed','cancel_requested','cancelled','expired','retry_scheduled','succeeded','failed','dead','reconciled','activation_authorized'));
