-- Step 20D: rollback to a retained release. A rollback is its own job kind
-- ('rollback_execution') with its own deployment; it restarts the target's
-- retained container (owner decision D-R1), so it carries the target release
-- id. UNIQUE(deployment_id, job_kind) is unchanged: one job per deployment.
ALTER TABLE agent_jobs DROP CONSTRAINT agent_jobs_job_kind_check;
ALTER TABLE agent_jobs ADD CONSTRAINT agent_jobs_job_kind_check CHECK (job_kind IN ('deployment_execution','rollback_execution'));
ALTER TABLE agent_jobs ADD COLUMN rollback_target_release_id TEXT REFERENCES releases(id) ON DELETE RESTRICT;
ALTER TABLE agent_jobs ADD CONSTRAINT agent_jobs_rollback_target_check CHECK ((job_kind = 'rollback_execution') = (rollback_target_release_id IS NOT NULL));

-- 000013's identity trigger, plus rollback_target_release_id (immutable).
CREATE OR REPLACE FUNCTION validate_agent_job_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
  IF NEW.id <> OLD.id OR NEW.schema_version <> OLD.schema_version OR NEW.job_kind <> OLD.job_kind
     OR NEW.owner_id <> OLD.owner_id OR NEW.project_id <> OLD.project_id OR NEW.environment_id <> OLD.environment_id
     OR NEW.server_id <> OLD.server_id OR NEW.deployment_id <> OLD.deployment_id OR NEW.repository_id <> OLD.repository_id
     OR NEW.full_ref <> OLD.full_ref OR NEW.commit_sha <> OLD.commit_sha OR NEW.priority <> OLD.priority
     OR NEW.idempotency_key <> OLD.idempotency_key OR NEW.payload_digest <> OLD.payload_digest OR NEW.created_at <> OLD.created_at
     OR NEW.rollback_target_release_id IS DISTINCT FROM OLD.rollback_target_release_id THEN
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

-- Immutable record of an owner-initiated rollback; mirrors
-- manual_deployment_intents (000023). Never updated or deleted.
CREATE TABLE rollback_intents (
  id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  owner_id TEXT NOT NULL,
  actor_user_id TEXT NOT NULL,
  source_release_id TEXT NOT NULL REFERENCES releases(id) ON DELETE RESTRICT,
  target_release_id TEXT NOT NULL REFERENCES releases(id) ON DELETE RESTRICT,
  request_nonce TEXT NOT NULL CONSTRAINT rollback_intents_request_nonce_check CHECK (octet_length(request_nonce) BETWEEN 16 AND 128 AND request_nonce ~ '^[A-Za-z0-9_-]+$'),
  deployment_id TEXT NOT NULL UNIQUE REFERENCES deployments(id) DEFERRABLE INITIALLY DEFERRED CHECK (octet_length(deployment_id) BETWEEN 1 AND 128),
  created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  FOREIGN KEY (actor_user_id, owner_id) REFERENCES users(id, owner_id) ON DELETE RESTRICT,
  UNIQUE (owner_id, request_nonce),
  CHECK (source_release_id <> target_release_id)
);
CREATE INDEX rollback_intents_owner_created_idx ON rollback_intents(owner_id, created_at DESC);

CREATE FUNCTION reject_rollback_intent_mutation() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'rollback intents are immutable'; END; $$;
CREATE TRIGGER rollback_intents_no_update BEFORE UPDATE ON rollback_intents FOR EACH ROW EXECUTE FUNCTION reject_rollback_intent_mutation();
CREATE TRIGGER rollback_intents_no_delete BEFORE DELETE ON rollback_intents FOR EACH ROW EXECUTE FUNCTION reject_rollback_intent_mutation();
