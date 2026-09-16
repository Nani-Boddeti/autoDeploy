-- Durable normalized webhook receipts and immutable initial enqueue records.
CREATE TABLE webhook_deliveries (
  id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  app_registration_id SMALLINT NOT NULL REFERENCES github_app_registrations(id) ON DELETE RESTRICT,
  delivery_id TEXT NOT NULL CHECK (octet_length(delivery_id) BETWEEN 1 AND 128),
  event_type TEXT NOT NULL CONSTRAINT webhook_deliveries_event_type_check CHECK (event_type IN ('push','installation','installation_repositories')),
  action TEXT NOT NULL DEFAULT '',
  body_digest BYTEA NOT NULL CHECK (octet_length(body_digest) = 32),
  verification_state TEXT NOT NULL CHECK (verification_state IN ('pending','verified','rejected')),
  processing_state TEXT NOT NULL CHECK (processing_state IN ('pending','processed','rejected')),
  failure_class TEXT NOT NULL DEFAULT '' CHECK (failure_class IN ('','signature','malformed','unsupported','unauthorized')),
  installation_id BIGINT CONSTRAINT webhook_deliveries_installation_id_check CHECK (installation_id IS NULL OR installation_id > 0),
  repository_id BIGINT CONSTRAINT webhook_deliveries_repository_id_check CHECK (repository_id IS NULL OR repository_id > 0),
  github_owner_id BIGINT CONSTRAINT webhook_deliveries_github_owner_id_check CHECK (github_owner_id IS NULL OR github_owner_id > 0),
  created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  processed_at TIMESTAMPTZ,
  UNIQUE (app_registration_id, delivery_id),
  CHECK ((verification_state = 'verified' AND processing_state IN ('pending','processed') AND failure_class = '')
      OR (verification_state = 'pending' AND processing_state = 'pending' AND failure_class = '')
      OR (verification_state = 'verified' AND processing_state = 'rejected' AND failure_class IN ('malformed','unsupported','unauthorized'))
      OR (verification_state = 'rejected' AND processing_state = 'rejected' AND failure_class = 'signature')),
  CHECK ((processing_state = 'processed') = (processed_at IS NOT NULL)),
  CHECK ((verification_state = 'verified' AND processing_state IN ('pending','processed') AND event_type = 'push' AND action = '' AND installation_id IS NOT NULL AND repository_id IS NOT NULL AND github_owner_id IS NOT NULL AND installation_id > 0 AND repository_id > 0 AND github_owner_id > 0)
      OR (verification_state = 'verified' AND processing_state IN ('pending','processed') AND event_type = 'installation' AND action IN ('created','deleted','suspend','unsuspend') AND installation_id IS NOT NULL AND repository_id IS NULL AND github_owner_id IS NOT NULL AND installation_id > 0 AND github_owner_id > 0)
      OR (verification_state = 'verified' AND processing_state IN ('pending','processed') AND event_type = 'installation_repositories' AND action IN ('added','removed') AND installation_id IS NOT NULL AND repository_id IS NOT NULL AND github_owner_id IS NOT NULL AND installation_id > 0 AND repository_id > 0 AND github_owner_id > 0)
      OR processing_state = 'rejected'),
  CONSTRAINT webhook_deliveries_terminal_action_check CHECK (processing_state <> 'rejected' OR action = '')
);
CREATE UNIQUE INDEX webhook_deliveries_verified_body_digest_uq ON webhook_deliveries(app_registration_id, body_digest) WHERE verification_state = 'verified';
CREATE INDEX webhook_deliveries_processing_idx ON webhook_deliveries(processing_state, created_at) WHERE processing_state = 'pending';

-- Canonical repository sets for installation_repositories receipts. The scalar
-- receipt repository_id remains the smallest ID for legacy audit/index paths.
CREATE TABLE webhook_lifecycle_repositories (
  delivery_id BIGINT NOT NULL REFERENCES webhook_deliveries(id) ON DELETE RESTRICT,
  repository_id BIGINT NOT NULL CHECK (repository_id > 0),
  PRIMARY KEY (delivery_id, repository_id)
);
CREATE FUNCTION seal_webhook_lifecycle_repository_insert() RETURNS trigger LANGUAGE plpgsql AS $$ DECLARE receipt_state TEXT; receipt_verification TEXT; BEGIN
  -- FOR SHARE conflicts with the receipt's processing-state UPDATE. A racing
  -- insert therefore either completes while the receipt remains pending or
  -- waits and observes the committed sealed state before it can append.
  SELECT verification_state,processing_state INTO receipt_verification,receipt_state FROM webhook_deliveries WHERE id = NEW.delivery_id FOR SHARE;
  IF receipt_verification <> 'verified' OR receipt_state <> 'pending' THEN RAISE EXCEPTION 'webhook lifecycle repository set is sealed'; END IF;
  RETURN NEW;
END; $$;
CREATE TRIGGER webhook_lifecycle_repositories_seal_insert BEFORE INSERT ON webhook_lifecycle_repositories FOR EACH ROW EXECUTE FUNCTION seal_webhook_lifecycle_repository_insert();
CREATE FUNCTION reject_webhook_lifecycle_repository_mutation() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'webhook lifecycle repositories are immutable'; END; $$;
CREATE TRIGGER webhook_lifecycle_repositories_no_update BEFORE UPDATE ON webhook_lifecycle_repositories FOR EACH ROW EXECUTE FUNCTION reject_webhook_lifecycle_repository_mutation();
CREATE TRIGGER webhook_lifecycle_repositories_no_delete BEFORE DELETE ON webhook_lifecycle_repositories FOR EACH ROW EXECUTE FUNCTION reject_webhook_lifecycle_repository_mutation();

CREATE TABLE automatic_deployment_intents (
  id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  delivery_id BIGINT NOT NULL UNIQUE REFERENCES webhook_deliveries(id) ON DELETE RESTRICT,
  binding_owner_id TEXT NOT NULL,
  binding_project_id TEXT NOT NULL,
  repository_id BIGINT NOT NULL CHECK (repository_id > 0),
  full_ref TEXT NOT NULL CONSTRAINT automatic_deployment_intents_full_ref_check CHECK (octet_length(full_ref) BETWEEN 12 AND 255 AND full_ref LIKE 'refs/heads/%' AND full_ref !~ '[[:space:]~^:?*\[\\]' AND full_ref !~ E'[\\x00-\\x20\\x7F]' AND full_ref !~ '//' AND full_ref !~ '/$' AND full_ref !~ '\.\.' AND full_ref !~ '@\{' AND full_ref !~ '(^|/)\.' AND full_ref !~ '\.lock($|/)' AND full_ref !~ '\.($|/)'),
  commit_sha TEXT NOT NULL CHECK (commit_sha ~ '^[0-9a-f]{40}$' AND commit_sha <> repeat('0',40)),
  deployment_id TEXT NOT NULL UNIQUE REFERENCES deployments(id) DEFERRABLE INITIALLY DEFERRED CHECK (octet_length(deployment_id) BETWEEN 1 AND 128),
  created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  FOREIGN KEY (binding_owner_id, binding_project_id) REFERENCES github_project_bindings(owner_id, project_id) ON DELETE RESTRICT,
  UNIQUE (binding_owner_id, binding_project_id, repository_id, full_ref, commit_sha)
);

CREATE TABLE agent_jobs (
  id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  schema_version SMALLINT NOT NULL CHECK (schema_version = 1),
  job_kind TEXT NOT NULL CHECK (job_kind = 'deployment_execution'),
  owner_id TEXT NOT NULL CHECK (octet_length(owner_id) BETWEEN 1 AND 128),
  project_id TEXT NOT NULL CHECK (octet_length(project_id) BETWEEN 1 AND 128),
  environment_id TEXT NOT NULL CHECK (octet_length(environment_id) BETWEEN 1 AND 128),
  server_id TEXT NOT NULL CHECK (octet_length(server_id) BETWEEN 1 AND 128),
  deployment_id TEXT NOT NULL REFERENCES deployments(id) ON DELETE RESTRICT DEFERRABLE INITIALLY DEFERRED CHECK (octet_length(deployment_id) BETWEEN 1 AND 128),
  repository_id BIGINT NOT NULL CHECK (repository_id > 0),
  full_ref TEXT NOT NULL CHECK (octet_length(full_ref) BETWEEN 12 AND 255 AND full_ref LIKE 'refs/heads/%' AND full_ref !~ '[[:space:]~^:?*\[\\]' AND full_ref !~ E'[\\x00-\\x20\\x7F]' AND full_ref !~ '//' AND full_ref !~ '/$' AND full_ref !~ '\.\.' AND full_ref !~ '@\{' AND full_ref !~ '(^|/)\.' AND full_ref !~ '\.lock($|/)' AND full_ref !~ '\.($|/)'),
  commit_sha TEXT NOT NULL CHECK (commit_sha ~ '^[0-9a-f]{40}$' AND commit_sha <> repeat('0',40)),
  priority SMALLINT NOT NULL CHECK (priority BETWEEN -100 AND 100),
  available_at TIMESTAMPTZ NOT NULL CONSTRAINT agent_jobs_available_at_finite_check CHECK (isfinite(available_at) AND available_at = date_trunc('microseconds', available_at)),
  idempotency_key TEXT NOT NULL CHECK (octet_length(idempotency_key) BETWEEN 1 AND 128),
  payload_digest BYTEA NOT NULL CHECK (octet_length(payload_digest) = 32),
  state TEXT NOT NULL CHECK (state = 'queued'),
  created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  UNIQUE (deployment_id, job_kind),
  UNIQUE (owner_id, idempotency_key)
);
CREATE INDEX agent_jobs_runnable_idx ON agent_jobs(server_id, priority DESC, available_at, id) WHERE state = 'queued';
CREATE INDEX agent_jobs_idempotency_idx ON agent_jobs(owner_id, idempotency_key);

CREATE TABLE agent_job_events (
  job_id BIGINT NOT NULL REFERENCES agent_jobs(id) ON DELETE RESTRICT,
  sequence SMALLINT NOT NULL CHECK (sequence = 1),
  event_type TEXT NOT NULL CHECK (event_type = 'enqueued'),
  occurred_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (job_id, sequence)
);
CREATE FUNCTION reject_agent_job_mutation() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'agent jobs are immutable before lease support'; END; $$;
CREATE TRIGGER agent_jobs_no_update BEFORE UPDATE ON agent_jobs FOR EACH ROW EXECUTE FUNCTION reject_agent_job_mutation();
CREATE TRIGGER agent_jobs_no_delete BEFORE DELETE ON agent_jobs FOR EACH ROW EXECUTE FUNCTION reject_agent_job_mutation();
CREATE FUNCTION reject_agent_job_event_mutation() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'agent job events are append-only'; END; $$;
CREATE TRIGGER agent_job_events_no_update BEFORE UPDATE ON agent_job_events FOR EACH ROW EXECUTE FUNCTION reject_agent_job_event_mutation();
CREATE TRIGGER agent_job_events_no_delete BEFORE DELETE ON agent_job_events FOR EACH ROW EXECUTE FUNCTION reject_agent_job_event_mutation();
CREATE FUNCTION reject_automatic_intent_mutation() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'automatic deployment intents are immutable'; END; $$;
CREATE TRIGGER automatic_deployment_intents_no_update BEFORE UPDATE ON automatic_deployment_intents FOR EACH ROW EXECUTE FUNCTION reject_automatic_intent_mutation();
CREATE TRIGGER automatic_deployment_intents_no_delete BEFORE DELETE ON automatic_deployment_intents FOR EACH ROW EXECUTE FUNCTION reject_automatic_intent_mutation();
CREATE FUNCTION validate_webhook_delivery_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
  IF NEW.app_registration_id <> OLD.app_registration_id OR NEW.delivery_id <> OLD.delivery_id OR NEW.event_type <> OLD.event_type OR NEW.action <> OLD.action OR NEW.body_digest <> OLD.body_digest OR NEW.installation_id IS DISTINCT FROM OLD.installation_id OR NEW.repository_id IS DISTINCT FROM OLD.repository_id OR NEW.github_owner_id IS DISTINCT FROM OLD.github_owner_id OR NEW.verification_state <> OLD.verification_state OR NEW.created_at <> OLD.created_at THEN RAISE EXCEPTION 'webhook delivery identity is immutable'; END IF;
  IF OLD.processing_state <> 'pending' OR NEW.processing_state NOT IN ('processed','rejected') THEN RAISE EXCEPTION 'invalid webhook delivery transition'; END IF;
  RETURN NEW;
END; $$;
CREATE TRIGGER webhook_deliveries_validate_update BEFORE UPDATE ON webhook_deliveries FOR EACH ROW EXECUTE FUNCTION validate_webhook_delivery_update();
CREATE FUNCTION reject_webhook_delivery_delete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'webhook deliveries are retained'; END; $$;
CREATE TRIGGER webhook_deliveries_no_delete BEFORE DELETE ON webhook_deliveries FOR EACH ROW EXECUTE FUNCTION reject_webhook_delivery_delete();
