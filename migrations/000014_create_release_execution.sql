-- Host execution slice 9a, step 1: schema for frozen execution configuration,
-- releases, and their runtime/operation records. See tasks/todo.md "Approved
-- Host Execution Slice 9a (control plane) and 9b (agent)" for the full slice,
-- docs/adr/0005-release-strategies.md ("Immutable identity, names, and
-- labels", "Health-gate contract", "Fenced activation and reconciliation",
-- "Cleanup and idempotency") for the release/runtime-instance/operation
-- model, and docs/adr/0003-secret-handling.md ("Immutable versions and
-- materialization freeze point") for why frozen_secret_versions must widen
-- rather than be replaced. This migration is schema-only: no claim/freeze,
-- materialize, progress, or executor logic ships here (later steps).

-- Desired (mutable, CAS'd) runtime configuration for an environment. No FK on
-- environment_id: this codebase has no projects/environments tables yet, so
-- environment_id is free TEXT everywhere it appears (agent_jobs.server_id is
-- the existing precedent for an identifier with no owning table). V1 hard-
-- codes ADR 0005's default HTTP health-gate shape, so health_policy_revision
-- is a pin recorded for provenance, not yet a selector; it can only be 1.
CREATE TABLE environment_runtime_configs (
  environment_id TEXT PRIMARY KEY CHECK (octet_length(environment_id) BETWEEN 1 AND 128),
  container_port INTEGER NOT NULL CHECK (container_port BETWEEN 1 AND 65535),
  container_scheme TEXT NOT NULL CHECK (container_scheme IN ('http','https')),
  release_strategy TEXT NOT NULL CHECK (release_strategy IN ('blue_green','stop_then_start')),
  drain_seconds SMALLINT NOT NULL CHECK (drain_seconds BETWEEN 1 AND 300),
  cpu_millis INTEGER NOT NULL CHECK (cpu_millis > 0 AND cpu_millis <= 256000),
  memory_bytes BIGINT NOT NULL CHECK (memory_bytes > 0 AND memory_bytes <= 549755813888),
  restart_policy TEXT NOT NULL CHECK (restart_policy IN ('unless-stopped','on-failure')),
  health_policy_revision SMALLINT NOT NULL CHECK (health_policy_revision = 1),
  revision BIGINT NOT NULL DEFAULT 0 CHECK (revision >= 0),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

-- Identity is immutable; every update must be a CAS that advances revision by
-- exactly one (the WHERE ... AND revision = $expected clause callers use is
-- what makes a stale-revision UPDATE affect zero rows and never even reach
-- this trigger; the arithmetic check here is a second guard against a caller
-- that matched a row but forgot to bump revision).
CREATE FUNCTION validate_environment_runtime_config_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
  IF NEW.environment_id <> OLD.environment_id THEN
    RAISE EXCEPTION 'environment runtime config identity is immutable';
  END IF;
  IF NEW.revision <> OLD.revision + 1 THEN
    RAISE EXCEPTION 'environment runtime config revision must advance by exactly one; expected % got %', OLD.revision + 1, NEW.revision;
  END IF;
  IF NEW.updated_at < OLD.updated_at THEN
    RAISE EXCEPTION 'environment runtime config updated_at must be monotonic';
  END IF;
  RETURN NEW;
END; $$;
CREATE TRIGGER environment_runtime_configs_validate_update BEFORE UPDATE ON environment_runtime_configs FOR EACH ROW EXECUTE FUNCTION validate_environment_runtime_config_update();
CREATE FUNCTION reject_environment_runtime_config_delete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'environment runtime configs are retained'; END; $$;
CREATE TRIGGER environment_runtime_configs_no_delete BEFORE DELETE ON environment_runtime_configs FOR EACH ROW EXECUTE FUNCTION reject_environment_runtime_config_delete();

-- Variable-name to value-source bindings for an environment. Freely
-- mutable/deletable by an administrator (step 3d admin pages); no
-- immutability trigger is required here, only the kind/head-presence CHECK.
CREATE TABLE environment_runtime_bindings (
  environment_id TEXT NOT NULL CHECK (octet_length(environment_id) BETWEEN 1 AND 128),
  variable_name TEXT NOT NULL CHECK (variable_name ~ '^[A-Za-z_][A-Za-z0-9_]*$' AND octet_length(variable_name) <= 128),
  binding_kind TEXT NOT NULL CHECK (binding_kind IN ('secret','literal_nonsecret')),
  secret_head_id TEXT REFERENCES secret_heads(id) ON DELETE RESTRICT,
  delivery_scope TEXT NOT NULL CHECK (delivery_scope IN ('build','runtime')),
  created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (environment_id, variable_name),
  CHECK ((binding_kind = 'secret') = (secret_head_id IS NOT NULL))
);

-- ADR 0003 names cross-environment/cross-project materialization as an
-- explicit threat. secret_head_id carries only a bare FK, so without this
-- trigger a binding in one environment could reference a secret head that
-- belongs to a different environment (or, transitively, a different
-- owner/project, since secret_heads scopes owner/project/environment
-- together) or a build-scoped head delivered at runtime (or vice versa).
-- Mirrors 000004's validate_secret_version_scope style. Also rejects a
-- 'provider'-purpose head: provider credentials are not deployable
-- variables. This is defense in depth at bind time -- it does NOT relieve
-- the later materialization step from re-checking scope immediately before
-- decryption.
CREATE FUNCTION validate_environment_runtime_binding_scope() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE h secret_heads%ROWTYPE;
BEGIN
  IF NEW.secret_head_id IS NULL THEN
    RETURN NEW;
  END IF;
  SELECT * INTO h FROM secret_heads WHERE id = NEW.secret_head_id;
  IF NOT FOUND OR h.environment_id <> NEW.environment_id OR h.delivery_scope <> NEW.delivery_scope THEN
    RAISE EXCEPTION 'environment runtime binding scope mismatch';
  END IF;
  IF h.purpose = 'provider' THEN
    RAISE EXCEPTION 'environment runtime binding cannot reference a provider-purpose secret head';
  END IF;
  RETURN NEW;
END; $$;
CREATE TRIGGER environment_runtime_bindings_scope_before_insert_update BEFORE INSERT OR UPDATE ON environment_runtime_bindings FOR EACH ROW EXECUTE FUNCTION validate_environment_runtime_binding_scope();

-- Widen frozen_secret_versions (000013) with the immutable variable_name and
-- delivery_scope a binding carried at freeze time, and change its PK to
-- (attempt_id, secret_version_id, variable_name): the same secret version can
-- now be bound to more than one variable for the same attempt. This is safe
-- schema surgery, not a data migration: freezeSecretVersions(ctx, tx,
-- attemptID, nil) (internal/store/postgres/agent_lease_repository.go) has
-- always been called with nil, so this table has never had a row in any
-- deployment, and ADD COLUMN ... NOT NULL below never has to satisfy an
-- existing row. The existing frozen_secret_versions_no_update/_no_delete
-- triggers both execute reject_frozen_secret_version_mutation(), which
-- unconditionally raises without referencing any column, so they remain
-- correct unchanged against the widened table.
ALTER TABLE frozen_secret_versions ADD COLUMN variable_name TEXT NOT NULL CHECK (variable_name ~ '^[A-Za-z_][A-Za-z0-9_]*$' AND octet_length(variable_name) <= 128);
ALTER TABLE frozen_secret_versions ADD COLUMN delivery_scope TEXT NOT NULL CHECK (delivery_scope IN ('build','runtime'));
ALTER TABLE frozen_secret_versions DROP CONSTRAINT frozen_secret_versions_pkey;
ALTER TABLE frozen_secret_versions ADD PRIMARY KEY (attempt_id, secret_version_id, variable_name);

-- Frozen (insert-once) copy of the execution configuration an attempt
-- actually ran with, plus the source config's revision so an operator can
-- prove what an attempt ran against. One row per attempt.
CREATE TABLE agent_job_attempt_execution (
  attempt_id BIGINT PRIMARY KEY REFERENCES agent_job_attempts(id) ON DELETE RESTRICT,
  container_port INTEGER NOT NULL CHECK (container_port BETWEEN 1 AND 65535),
  container_scheme TEXT NOT NULL CHECK (container_scheme IN ('http','https')),
  release_strategy TEXT NOT NULL CHECK (release_strategy IN ('blue_green','stop_then_start')),
  -- ADR 0005 ("Cleanup and idempotency"): the drain duration is
  -- environment-configurable and frozen into the operation, so the agent
  -- never has to re-read live environment_runtime_configs during drain.
  -- Same CHECK bounds as environment_runtime_configs.drain_seconds.
  drain_seconds SMALLINT NOT NULL CHECK (drain_seconds BETWEEN 1 AND 300),
  health_policy_revision SMALLINT NOT NULL CHECK (health_policy_revision = 1),
  cpu_millis INTEGER NOT NULL CHECK (cpu_millis > 0 AND cpu_millis <= 256000),
  memory_bytes BIGINT NOT NULL CHECK (memory_bytes > 0 AND memory_bytes <= 549755813888),
  restart_policy TEXT NOT NULL CHECK (restart_policy IN ('unless-stopped','on-failure')),
  environment_runtime_config_revision BIGINT NOT NULL CHECK (environment_runtime_config_revision >= 0),
  created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
CREATE FUNCTION reject_agent_job_attempt_execution_mutation() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'agent job attempt execution is insert-once'; END; $$;
CREATE TRIGGER agent_job_attempt_execution_no_update BEFORE UPDATE ON agent_job_attempt_execution FOR EACH ROW EXECUTE FUNCTION reject_agent_job_attempt_execution_mutation();
CREATE TRIGGER agent_job_attempt_execution_no_delete BEFORE DELETE ON agent_job_attempt_execution FOR EACH ROW EXECUTE FUNCTION reject_agent_job_attempt_execution_mutation();

-- One release per deployment (UNIQUE(deployment_id)). Identity, including the
-- frozen strategy, is immutable; state follows ADR 0005's minimal model
-- introduced for this slice: candidate->ready|failed|removing,
-- ready->failed|removing, removing->removed. active/retained do not exist
-- yet (the activation slice adds them) and are rejected two ways: they are
-- not in the state CHECK at all, and even if they were, the transition
-- trigger below only recognizes the five edges above.
CREATE TABLE releases (
  id TEXT PRIMARY KEY CHECK (octet_length(id) BETWEEN 1 AND 128),
  deployment_id TEXT NOT NULL UNIQUE REFERENCES deployments(id) ON DELETE RESTRICT,
  environment_id TEXT NOT NULL CHECK (octet_length(environment_id) BETWEEN 1 AND 128),
  -- The image digest does not exist yet when the release row is created: the
  -- release is opened before the build runs (its id must exist for the
  -- prepare operation to reference), and the digest only becomes known when
  -- BuildKit finishes. So it is nullable and write-once: the trigger below
  -- allows NULL -> value exactly once and never value -> anything, and a
  -- release cannot leave 'candidate' for 'ready' until it is set. config_digest
  -- is computed from the frozen execution configuration and is therefore known
  -- at insert time.
  image_digest BYTEA CHECK (image_digest IS NULL OR octet_length(image_digest) = 32),
  config_digest BYTEA NOT NULL CHECK (octet_length(config_digest) = 32),
  commit_sha TEXT NOT NULL CHECK (commit_sha ~ '^[0-9a-f]{40}$' AND commit_sha <> repeat('0', 40)),
  strategy TEXT NOT NULL CHECK (strategy IN ('blue_green','stop_then_start')),
  state TEXT NOT NULL CHECK (state IN ('candidate','ready','failed','removing','removed')),
  created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  CHECK (updated_at >= created_at)
);
CREATE INDEX releases_environment_state_idx ON releases(environment_id, state);

CREATE FUNCTION validate_release_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
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
  -- A same-state update is legitimate: the write-once image digest is set
  -- while the release stays 'candidate'. Only a genuine state change is
  -- checked against the legal edge list, and a terminal state permits no
  -- change at all.
  IF NEW.state <> OLD.state AND NOT (
    (OLD.state = 'candidate' AND NEW.state IN ('ready','failed','removing')) OR
    (OLD.state = 'ready' AND NEW.state IN ('failed','removing')) OR
    (OLD.state = 'removing' AND NEW.state = 'removed')
  ) THEN
    RAISE EXCEPTION 'invalid release transition % -> %', OLD.state, NEW.state;
  END IF;
  IF NEW.updated_at < OLD.updated_at THEN
    RAISE EXCEPTION 'release updated_at must be monotonic';
  END IF;
  RETURN NEW;
END; $$;
CREATE TRIGGER releases_validate_update BEFORE UPDATE ON releases FOR EACH ROW EXECUTE FUNCTION validate_release_update();
CREATE FUNCTION reject_release_delete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'releases are retained'; END; $$;
CREATE TRIGGER releases_no_delete BEFORE DELETE ON releases FOR EACH ROW EXECUTE FUNCTION reject_release_delete();

-- One row per attempt that actually ran a container for a release. Every
-- column but observed_state is immutable; observed_state may only change via
-- an UPDATE whose WHERE clause matches this row's own (immutable) fence, so
-- a write carrying a stale or foreign fence matches zero rows and never
-- reaches this trigger at all.
CREATE TABLE release_runtime_instances (
  id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  release_id TEXT NOT NULL REFERENCES releases(id) ON DELETE RESTRICT,
  job_id BIGINT NOT NULL REFERENCES agent_jobs(id) ON DELETE RESTRICT,
  attempt_id BIGINT NOT NULL UNIQUE REFERENCES agent_job_attempts(id) ON DELETE RESTRICT,
  fence BIGINT NOT NULL CHECK (fence > 0),
  container_name TEXT NOT NULL CHECK (container_name = btrim(container_name) AND octet_length(container_name) BETWEEN 1 AND 255 AND container_name !~ '[\x00-\x1f\x7f]'),
  observed_state TEXT NOT NULL CHECK (observed_state IN ('created','running','stopped','exited','missing')),
  created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX release_runtime_instances_release_idx ON release_runtime_instances(release_id);

-- Reject a fence or job_id that disagrees with the agent_job_attempts row
-- NEW.attempt_id actually names. Without this, a wrong fence would not fail
-- fast at insert; it would instead silently make the fence-gated
-- observed_state UPDATE (WHERE attempt_id=$1 AND fence=$2) match zero rows,
-- which is far harder to diagnose than an insert-time rejection.
--
-- No row lock is taken on agent_job_attempts here (unlike
-- validate_agent_job_attempt_insert's FOR UPDATE/FOR SHARE locks on
-- environment_slots/agent_jobs, which guard columns that ARE concurrently
-- mutated by the claim transaction). fence and job_id on agent_job_attempts
-- are identity columns: validate_agent_job_attempt_update (000013)
-- unconditionally rejects any UPDATE that changes either one, for the
-- entire lifetime of the row. So there is nothing for a lock to protect
-- against here -- a plain unlocked SELECT sees a value that can never
-- change again. Taking a lock would only add a new edge to the lock
-- hierarchy for no correctness benefit, risking an inversion of the
-- existing server/credential SHARE -> environment slot UPDATE -> job
-- UPDATE -> attempt order these inserts run inside.
CREATE FUNCTION validate_release_runtime_instance_insert() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  attempt_fence BIGINT;
  attempt_job_id BIGINT;
BEGIN
  SELECT fence, job_id INTO attempt_fence, attempt_job_id FROM agent_job_attempts WHERE id = NEW.attempt_id;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'release runtime instance attempt % does not exist', NEW.attempt_id;
  END IF;
  IF NEW.fence <> attempt_fence THEN
    RAISE EXCEPTION 'release runtime instance fence % does not match attempt % fence %', NEW.fence, NEW.attempt_id, attempt_fence;
  END IF;
  IF NEW.job_id <> attempt_job_id THEN
    RAISE EXCEPTION 'release runtime instance job_id % does not match attempt % job_id %', NEW.job_id, NEW.attempt_id, attempt_job_id;
  END IF;
  RETURN NEW;
END; $$;
CREATE TRIGGER release_runtime_instances_validate_insert BEFORE INSERT ON release_runtime_instances FOR EACH ROW EXECUTE FUNCTION validate_release_runtime_instance_insert();

CREATE FUNCTION validate_release_runtime_instance_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
  IF NEW.id <> OLD.id OR NEW.release_id <> OLD.release_id OR NEW.job_id <> OLD.job_id OR NEW.attempt_id <> OLD.attempt_id
     OR NEW.fence <> OLD.fence OR NEW.container_name <> OLD.container_name OR NEW.created_at <> OLD.created_at THEN
    RAISE EXCEPTION 'release runtime instance identity is immutable';
  END IF;
  RETURN NEW;
END; $$;
CREATE TRIGGER release_runtime_instances_validate_update BEFORE UPDATE ON release_runtime_instances FOR EACH ROW EXECUTE FUNCTION validate_release_runtime_instance_update();
CREATE FUNCTION reject_release_runtime_instance_delete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'release runtime instances are retained'; END; $$;
CREATE TRIGGER release_runtime_instances_no_delete BEFORE DELETE ON release_runtime_instances FOR EACH ROW EXECUTE FUNCTION reject_release_runtime_instance_delete();

-- Append-only operation log. Only 'prepare' is reachable in 9a/9b; the others
-- are named now so the activation/rollback/retire/cleanup slices need no
-- further migration. A row opens with state='started' and closed_at NULL,
-- and may close exactly once to 'succeeded' or 'failed' with closed_at set;
-- once closed it is fully frozen. outcome_summary must already be sanitized
-- (bounded length, no control characters) by the caller before it ever
-- reaches this column. It is written when the operation closes, because that
-- is when the outcome is known: it may change only on the single close
-- transition, and a closed row cannot be touched again.
CREATE TABLE release_operations (
  id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  release_id TEXT NOT NULL REFERENCES releases(id) ON DELETE RESTRICT,
  job_id BIGINT NOT NULL REFERENCES agent_jobs(id) ON DELETE RESTRICT,
  attempt_id BIGINT NOT NULL REFERENCES agent_job_attempts(id) ON DELETE RESTRICT,
  fence BIGINT NOT NULL CHECK (fence > 0),
  kind TEXT NOT NULL CHECK (kind IN ('prepare','activate','rollback','retire','cleanup')),
  digest BYTEA NOT NULL CHECK (octet_length(digest) = 32),
  state TEXT NOT NULL CHECK (state IN ('started','succeeded','failed')),
  outcome_summary TEXT NOT NULL DEFAULT '' CHECK (octet_length(outcome_summary) <= 2048 AND outcome_summary !~ '[\x00-\x1f\x7f]'),
  created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  closed_at TIMESTAMPTZ CHECK (closed_at IS NULL OR (isfinite(closed_at) AND closed_at >= created_at)),
  CHECK ((state = 'started') = (closed_at IS NULL))
);
CREATE INDEX release_operations_release_idx ON release_operations(release_id, created_at);

-- Same insert-time fence/job_id validation as
-- validate_release_runtime_instance_insert above, and the same reasoning for
-- taking no lock on agent_job_attempts: fence and job_id are immutable
-- identity columns on that table (enforced by validate_agent_job_attempt_update,
-- 000013), so an unlocked SELECT cannot observe a value that later changes
-- out from under this check.
CREATE FUNCTION validate_release_operation_insert() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  attempt_fence BIGINT;
  attempt_job_id BIGINT;
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
  RETURN NEW;
END; $$;
CREATE TRIGGER release_operations_validate_insert BEFORE INSERT ON release_operations FOR EACH ROW EXECUTE FUNCTION validate_release_operation_insert();

CREATE FUNCTION validate_release_operation_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
  IF NEW.id <> OLD.id OR NEW.release_id <> OLD.release_id OR NEW.job_id <> OLD.job_id OR NEW.attempt_id <> OLD.attempt_id
     OR NEW.fence <> OLD.fence OR NEW.kind <> OLD.kind OR NEW.digest <> OLD.digest
     OR NEW.created_at <> OLD.created_at THEN
    RAISE EXCEPTION 'release operation identity is immutable';
  END IF;
  IF OLD.state <> 'started' THEN
    RAISE EXCEPTION 'release operation % is closed', OLD.state;
  END IF;
  IF NEW.state NOT IN ('succeeded','failed') THEN
    RAISE EXCEPTION 'release operation must close to succeeded or failed';
  END IF;
  RETURN NEW;
END; $$;
CREATE TRIGGER release_operations_validate_update BEFORE UPDATE ON release_operations FOR EACH ROW EXECUTE FUNCTION validate_release_operation_update();
CREATE FUNCTION reject_release_operation_delete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'release operations are append-only'; END; $$;
CREATE TRIGGER release_operations_no_delete BEFORE DELETE ON release_operations FOR EACH ROW EXECUTE FUNCTION reject_release_operation_delete();
