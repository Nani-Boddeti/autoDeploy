-- Durable lease lifecycle over agent_jobs from 000010. See
-- docs/adr/0004-postgresql-queue-leases.md for job/attempt states, the
-- global lock hierarchy, fencing, retry, cancellation, and expiry rules.
-- This migration only adds schema/constraints/indexes; claim/renew/
-- complete/cancel/expire/reconcile repository logic is a later slice.

-- agent_jobs: widen from the single 'queued' state installed by 000010 to
-- the full job-head lifecycle, and replace the blanket no-update trigger
-- with a transition-validating one.
ALTER TABLE agent_jobs ADD COLUMN attempt_count SMALLINT NOT NULL DEFAULT 0 CHECK (attempt_count BETWEEN 0 AND 5);
ALTER TABLE agent_jobs ADD COLUMN cancel_requested_at TIMESTAMPTZ CHECK (cancel_requested_at IS NULL OR isfinite(cancel_requested_at));
-- clock_timestamp() is volatile, so PG cannot back-fill this default as
-- catalog-only metadata (the fast path for constant defaults since PG11);
-- adding it rewrites agent_jobs under ACCESS EXCLUSIVE lock. Accepted here
-- because no deployment has populated this table before this migration.
ALTER TABLE agent_jobs ADD COLUMN updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp();

ALTER TABLE agent_jobs DROP CONSTRAINT agent_jobs_state_check;
ALTER TABLE agent_jobs ADD CONSTRAINT agent_jobs_state_check CHECK (state IN ('queued','leased','retry_wait','succeeded','failed','cancelled','dead'));

DROP TRIGGER agent_jobs_no_update ON agent_jobs;

-- Legal state transitions (job head, ADR 0004 job/attempt model):
--   queued     -> leased | cancelled
--   leased     -> leased (cancel_requested_at only) | succeeded | failed | retry_wait | dead | cancelled
--   retry_wait -> leased | cancelled | dead
-- succeeded/failed/cancelled/dead are terminal and never resurrected.
-- Identity/payload columns are immutable for the row's lifetime.
-- attempt_count increments by exactly one only when a *new* attempt starts
-- (transition into 'leased' from a non-leased state); it never decreases.
-- available_at changes only when entering retry_wait, and must advance.
-- cancel_requested_at may be set once (NULL -> value) and is then immutable.
CREATE FUNCTION validate_agent_job_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
  IF NEW.id <> OLD.id OR NEW.schema_version <> OLD.schema_version OR NEW.job_kind <> OLD.job_kind
     OR NEW.owner_id <> OLD.owner_id OR NEW.project_id <> OLD.project_id OR NEW.environment_id <> OLD.environment_id
     OR NEW.server_id <> OLD.server_id OR NEW.deployment_id <> OLD.deployment_id OR NEW.repository_id <> OLD.repository_id
     OR NEW.full_ref <> OLD.full_ref OR NEW.commit_sha <> OLD.commit_sha OR NEW.priority <> OLD.priority
     OR NEW.idempotency_key <> OLD.idempotency_key OR NEW.payload_digest <> OLD.payload_digest OR NEW.created_at <> OLD.created_at THEN
    RAISE EXCEPTION 'agent job identity is immutable';
  END IF;
  IF OLD.state IN ('succeeded','failed','cancelled','dead') THEN
    RAISE EXCEPTION 'agent job % is terminal', OLD.state;
  END IF;
  IF NOT (
    (OLD.state = 'queued' AND NEW.state IN ('leased','cancelled')) OR
    (OLD.state = 'leased' AND NEW.state IN ('leased','succeeded','failed','retry_wait','dead','cancelled')) OR
    (OLD.state = 'retry_wait' AND NEW.state IN ('leased','cancelled','dead'))
  ) THEN
    RAISE EXCEPTION 'invalid agent job transition % -> %', OLD.state, NEW.state;
  END IF;
  IF OLD.state = 'leased' AND NEW.state = 'leased' AND (
    NEW.attempt_count <> OLD.attempt_count OR NEW.available_at <> OLD.available_at
  ) THEN
    RAISE EXCEPTION 'agent job leased-state update may only set cancel_requested_at';
  END IF;
  IF NEW.state = 'leased' AND OLD.state <> 'leased' THEN
    IF NEW.attempt_count <> OLD.attempt_count + 1 THEN
      RAISE EXCEPTION 'agent job attempt_count must increment by one when a new attempt is leased';
    END IF;
  ELSIF NEW.attempt_count <> OLD.attempt_count THEN
    RAISE EXCEPTION 'agent job attempt_count is immutable outside a new lease';
  END IF;
  IF NEW.state = 'retry_wait' THEN
    IF NEW.available_at <= OLD.available_at THEN
      RAISE EXCEPTION 'agent job retry available_at must advance';
    END IF;
  ELSIF NEW.available_at <> OLD.available_at THEN
    RAISE EXCEPTION 'agent job available_at may change only when entering retry_wait';
  END IF;
  IF OLD.cancel_requested_at IS NOT NULL AND NEW.cancel_requested_at IS DISTINCT FROM OLD.cancel_requested_at THEN
    RAISE EXCEPTION 'agent job cancel_requested_at is immutable once set';
  END IF;
  IF NEW.updated_at < OLD.updated_at THEN
    RAISE EXCEPTION 'agent job updated_at must be monotonic';
  END IF;
  RETURN NEW;
END; $$;
CREATE TRIGGER agent_jobs_validate_update BEFORE UPDATE ON agent_jobs FOR EACH ROW EXECUTE FUNCTION validate_agent_job_update();
-- agent_jobs_no_delete (reject_agent_job_mutation) from 000010 is retained unchanged.

DROP INDEX agent_jobs_runnable_idx;
-- Column order matches ADR 0004's required runnable index shape exactly
-- (server_id, priority DESC, available_at, created_at, id): created_at
-- breaks ties within the same available_at before falling back to id.
CREATE INDEX agent_jobs_runnable_idx ON agent_jobs(server_id, priority DESC, available_at, created_at, id) WHERE state IN ('queued','retry_wait');

-- One execution slot per environment. Created (upserted) by the claim
-- transaction before an attempt is recorded against it; current_attempt_id
-- and reconciliation_attempt_id are added below once agent_job_attempts
-- exists, resolving the circular reference.
CREATE TABLE environment_slots (
  environment_id TEXT PRIMARY KEY CHECK (octet_length(environment_id) BETWEEN 1 AND 128),
  fence BIGINT NOT NULL DEFAULT 0 CHECK (fence >= 0),
  reconciliation_required BOOLEAN NOT NULL DEFAULT false,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

-- One immutable numbered attempt per lease (ADR 0004 job/attempt model).
-- Decision 11: agent_jobs.server_id has no FK (bindings predate servers),
-- but every attempt FKs to the authenticated server that holds it.
CREATE TABLE agent_job_attempts (
  id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  job_id BIGINT NOT NULL REFERENCES agent_jobs(id) ON DELETE RESTRICT,
  attempt_number SMALLINT NOT NULL CHECK (attempt_number BETWEEN 1 AND 5),
  server_id TEXT NOT NULL REFERENCES servers(id) ON DELETE RESTRICT,
  environment_id TEXT NOT NULL REFERENCES environment_slots(environment_id) ON DELETE RESTRICT,
  fence BIGINT NOT NULL CHECK (fence > 0),
  state TEXT NOT NULL CHECK (state IN ('leased','succeeded','failed','expired','cancelled')),
  renewal_sequence INTEGER NOT NULL DEFAULT 0 CHECK (renewal_sequence >= 0),
  lease_expires_at TIMESTAMPTZ NOT NULL CHECK (isfinite(lease_expires_at)),
  authority_expires_at TIMESTAMPTZ NOT NULL CHECK (isfinite(authority_expires_at)),
  cancel_deadline_at TIMESTAMPTZ CHECK (cancel_deadline_at IS NULL OR isfinite(cancel_deadline_at)),
  -- Provisional closed enum; exact terminal metadata is a deferred ADR 0004
  -- decision. Unknown/absent classes are treated as non-retryable by the
  -- retry classifier (Step 4), so this list only needs to name known
  -- transient vs. permanent buckets, not be exhaustive of every cause.
  failure_class TEXT CHECK (failure_class IS NULL OR failure_class IN ('transient','authorization','invalid_input','cancelled','corrupt_state','permanent_configuration')),
  result_operation_id TEXT CHECK (result_operation_id IS NULL OR octet_length(result_operation_id) BETWEEN 1 AND 128),
  result_digest BYTEA CHECK (result_digest IS NULL OR octet_length(result_digest) = 32),
  closed_at TIMESTAMPTZ CHECK (closed_at IS NULL OR isfinite(closed_at)),
  created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  UNIQUE (job_id, attempt_number),
  CHECK (authority_expires_at <= lease_expires_at),
  CHECK ((result_operation_id IS NULL) = (result_digest IS NULL)),
  CHECK ((state <> 'leased') = (closed_at IS NOT NULL)),
  -- Closure shape per ADR 0004 completion/cancel-ack/expiry: leased and
  -- expired never carry failure_class or a result; succeeded never carries
  -- failure_class but always carries a result; failed always carries both
  -- (completion/failure share the same validated-result predicate); an
  -- agent cancel acknowledgement may carry a result (cancelled by the
  -- agent mid-cleanup) while an expiry-swept cancellation carries none
  -- (cancelled never carries failure_class either way).
  CHECK (
    (state = 'leased'    AND failure_class IS NULL AND result_operation_id IS NULL AND result_digest IS NULL) OR
    (state = 'succeeded' AND failure_class IS NULL AND result_operation_id IS NOT NULL AND result_digest IS NOT NULL) OR
    (state = 'failed'    AND failure_class IS NOT NULL AND result_operation_id IS NOT NULL AND result_digest IS NOT NULL) OR
    (state = 'expired'   AND failure_class IS NULL AND result_operation_id IS NULL AND result_digest IS NULL) OR
    (state = 'cancelled' AND failure_class IS NULL)
  ),
  -- cancel_deadline_at, once set, caps authority_expires_at (ADR 0004
  -- cancellation step 1); whether it may be set at all, and whether it may
  -- move, is a prior-state question the update trigger below enforces.
  CHECK (cancel_deadline_at IS NULL OR authority_expires_at <= cancel_deadline_at)
);
-- At most one leased (occupying) attempt per job.
CREATE UNIQUE INDEX agent_job_attempts_one_leased_uq ON agent_job_attempts(job_id) WHERE state = 'leased';
-- Sweeper discovery of authority-expired leases (ADR 0004 recovery).
CREATE INDEX agent_job_attempts_authority_expiry_idx ON agent_job_attempts(authority_expires_at, id) WHERE state = 'leased';

-- Fencing (ADR 0004 atomic claim boundary + global lock hierarchy: slot,
-- then job, then attempt). The claim transaction must, in order: (1)
-- increment the environment slot's fence, (2) transition the job into
-- 'leased' with attempt_count incremented, (3) insert this attempt row
-- carrying that same fence and the job's now-current attempt_count. This
-- trigger locks the slot FOR UPDATE (the claim already holds that lock
-- from step 1 in the same transaction; re-acquiring is a no-op) and the
-- job FOR SHARE (read-only revalidation; the claim already holds an
-- update lock on it from step 2), then rejects an attempt whose fence,
-- attempt_number, server_id, or environment_id disagree with those rows.
CREATE FUNCTION validate_agent_job_attempt_insert() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  slot_fence BIGINT;
  job_server_id TEXT;
  job_environment_id TEXT;
  job_attempt_count SMALLINT;
BEGIN
  SELECT fence INTO slot_fence FROM environment_slots WHERE environment_id = NEW.environment_id FOR UPDATE;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'agent job attempt environment slot % does not exist', NEW.environment_id;
  END IF;
  IF NEW.fence <> slot_fence THEN
    RAISE EXCEPTION 'agent job attempt fence % does not match environment slot fence %', NEW.fence, slot_fence;
  END IF;

  SELECT server_id, environment_id, attempt_count INTO job_server_id, job_environment_id, job_attempt_count
    FROM agent_jobs WHERE id = NEW.job_id FOR SHARE;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'agent job attempt job % does not exist', NEW.job_id;
  END IF;
  IF job_server_id <> NEW.server_id OR job_environment_id <> NEW.environment_id THEN
    RAISE EXCEPTION 'agent job attempt server/environment must match job %', NEW.job_id;
  END IF;
  IF NEW.attempt_number <> job_attempt_count THEN
    RAISE EXCEPTION 'agent job attempt_number % must equal job attempt_count %', NEW.attempt_number, job_attempt_count;
  END IF;
  RETURN NEW;
END; $$;
CREATE TRIGGER agent_job_attempts_validate_insert BEFORE INSERT ON agent_job_attempts FOR EACH ROW EXECUTE FUNCTION validate_agent_job_attempt_insert();

-- Update trigger: identity is immutable; a row can only be mutated while it
-- is still 'leased' (closed rows are fully frozen); the only legal target
-- states from 'leased' are 'leased' itself (renewal/cancel-deadline update)
-- or one of the four terminal attempt states; renewal_sequence is
-- monotonic; result operation id/digest and failure_class, once recorded,
-- are immutable.
CREATE FUNCTION validate_agent_job_attempt_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
  IF NEW.id <> OLD.id OR NEW.job_id <> OLD.job_id OR NEW.attempt_number <> OLD.attempt_number
     OR NEW.server_id <> OLD.server_id OR NEW.environment_id <> OLD.environment_id OR NEW.fence <> OLD.fence
     OR NEW.created_at <> OLD.created_at THEN
    RAISE EXCEPTION 'agent job attempt identity is immutable';
  END IF;
  IF OLD.state <> 'leased' THEN
    RAISE EXCEPTION 'agent job attempt % is closed', OLD.state;
  END IF;
  IF NEW.state <> 'leased' AND NEW.state NOT IN ('succeeded','failed','expired','cancelled') THEN
    RAISE EXCEPTION 'invalid agent job attempt transition % -> %', OLD.state, NEW.state;
  END IF;
  IF NEW.renewal_sequence < OLD.renewal_sequence THEN
    RAISE EXCEPTION 'agent job attempt renewal_sequence must be monotonic';
  END IF;
  IF OLD.result_operation_id IS NOT NULL AND (
    NEW.result_operation_id IS DISTINCT FROM OLD.result_operation_id OR NEW.result_digest IS DISTINCT FROM OLD.result_digest
  ) THEN
    RAISE EXCEPTION 'agent job attempt result is immutable once recorded';
  END IF;
  IF OLD.failure_class IS NOT NULL AND NEW.failure_class IS DISTINCT FROM OLD.failure_class THEN
    RAISE EXCEPTION 'agent job attempt failure_class is immutable once recorded';
  END IF;
  -- cancel_deadline_at (ADR 0004 cancellation step 1): may only be set
  -- while the attempt is leased (already guaranteed above) and only once
  -- its job has recorded cancel_requested_at; once set it cannot move,
  -- forward or back. The authority-vs-deadline cap itself is the table
  -- CHECK above.
  IF OLD.cancel_deadline_at IS NOT NULL AND NEW.cancel_deadline_at IS DISTINCT FROM OLD.cancel_deadline_at THEN
    RAISE EXCEPTION 'agent job attempt cancel_deadline_at is immutable once set';
  END IF;
  IF OLD.cancel_deadline_at IS NULL AND NEW.cancel_deadline_at IS NOT NULL THEN
    IF NOT EXISTS (SELECT 1 FROM agent_jobs WHERE id = NEW.job_id AND cancel_requested_at IS NOT NULL) THEN
      RAISE EXCEPTION 'agent job attempt cancel_deadline_at requires job cancel_requested_at';
    END IF;
  END IF;
  RETURN NEW;
END; $$;
CREATE TRIGGER agent_job_attempts_validate_update BEFORE UPDATE ON agent_job_attempts FOR EACH ROW EXECUTE FUNCTION validate_agent_job_attempt_update();
CREATE FUNCTION reject_agent_job_attempt_delete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'agent job attempts are retained'; END; $$;
CREATE TRIGGER agent_job_attempts_no_delete BEFORE DELETE ON agent_job_attempts FOR EACH ROW EXECUTE FUNCTION reject_agent_job_attempt_delete();

-- Resolve the circular reference: the slot's current occupant and the
-- attempt awaiting reconciliation both point at agent_job_attempts.
ALTER TABLE environment_slots ADD COLUMN current_attempt_id BIGINT REFERENCES agent_job_attempts(id) ON DELETE RESTRICT;
ALTER TABLE environment_slots ADD COLUMN reconciliation_attempt_id BIGINT REFERENCES agent_job_attempts(id) ON DELETE RESTRICT;
-- Resolution of the ADR's fencing/reconciliation ambiguity: a slot may
-- remain "occupied" (current_attempt_id set) while reconciliation_required
-- is true, because ADR 0004 explicitly keeps the stale attempt's ownership
-- visible ("does not lease new work... until the agent reports the
-- operation outcome and reconciles") rather than clearing it immediately on
-- expiry/cancel-deadline. The enforceable invariant instead ties the flag
-- to the specific attempt under reconciliation: the flag is set if and only
-- if a reconciliation_attempt_id names the attempt it covers.
ALTER TABLE environment_slots ADD CONSTRAINT environment_slots_reconciliation_consistency_check CHECK (reconciliation_required = (reconciliation_attempt_id IS NOT NULL));

CREATE FUNCTION validate_environment_slot_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
  IF NEW.environment_id <> OLD.environment_id THEN
    RAISE EXCEPTION 'environment slot identity is immutable';
  END IF;
  IF NEW.fence < OLD.fence THEN
    RAISE EXCEPTION 'environment slot fence must not decrease';
  END IF;
  IF NEW.updated_at < OLD.updated_at THEN
    RAISE EXCEPTION 'environment slot updated_at must be monotonic';
  END IF;
  RETURN NEW;
END; $$;
CREATE TRIGGER environment_slots_validate_update BEFORE UPDATE ON environment_slots FOR EACH ROW EXECUTE FUNCTION validate_environment_slot_update();
CREATE FUNCTION reject_environment_slot_delete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'environment slots are retained'; END; $$;
CREATE TRIGGER environment_slots_no_delete BEFORE DELETE ON environment_slots FOR EACH ROW EXECUTE FUNCTION reject_environment_slot_delete();

-- Exact secret-version set frozen at claim time (ADR 0003/0004); empty
-- until the execution slice materializes secrets. Append-only.
CREATE TABLE frozen_secret_versions (
  attempt_id BIGINT NOT NULL REFERENCES agent_job_attempts(id) ON DELETE RESTRICT,
  secret_version_id TEXT NOT NULL REFERENCES secret_versions(id) ON DELETE RESTRICT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (attempt_id, secret_version_id)
);
CREATE FUNCTION reject_frozen_secret_version_mutation() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'frozen secret versions are append-only'; END; $$;
CREATE TRIGGER frozen_secret_versions_no_update BEFORE UPDATE ON frozen_secret_versions FOR EACH ROW EXECUTE FUNCTION reject_frozen_secret_version_mutation();
CREATE TRIGGER frozen_secret_versions_no_delete BEFORE DELETE ON frozen_secret_versions FOR EACH ROW EXECUTE FUNCTION reject_frozen_secret_version_mutation();

-- Widen agent_job_events (000010 allowed only the single initial 'enqueued'
-- event) to the full lease-lifecycle vocabulary, with an optional link to
-- the attempt an event concerns. Existing rows (sequence=1,
-- event_type='enqueued') remain valid; append-only triggers are unchanged.
ALTER TABLE agent_job_events ADD COLUMN attempt_id BIGINT REFERENCES agent_job_attempts(id) ON DELETE RESTRICT;
ALTER TABLE agent_job_events DROP CONSTRAINT agent_job_events_event_type_check;
ALTER TABLE agent_job_events ADD CONSTRAINT agent_job_events_event_type_check CHECK (event_type IN ('enqueued','claimed','cancel_requested','cancelled','expired','retry_scheduled','succeeded','failed','dead','reconciled'));
ALTER TABLE agent_job_events DROP CONSTRAINT agent_job_events_sequence_check;
ALTER TABLE agent_job_events ADD CONSTRAINT agent_job_events_sequence_check CHECK (sequence >= 1);
