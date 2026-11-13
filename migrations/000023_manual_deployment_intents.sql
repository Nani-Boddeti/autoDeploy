-- Step 19a: immutable record of an owner-initiated manual deployment. Mirrors
-- automatic_deployment_intents (000010) but is keyed by an owner-scoped
-- request nonce (idempotent retries) instead of a webhook delivery, and records
-- the acting user. Rows are never updated or deleted.
CREATE TABLE manual_deployment_intents (
  id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  owner_id TEXT NOT NULL,
  actor_user_id TEXT NOT NULL,
  binding_project_id TEXT NOT NULL,
  repository_id BIGINT NOT NULL CHECK (repository_id > 0),
  full_ref TEXT NOT NULL CONSTRAINT manual_deployment_intents_full_ref_check CHECK (octet_length(full_ref) BETWEEN 12 AND 255 AND full_ref LIKE 'refs/heads/%' AND full_ref !~ '[[:space:]~^:?*\[\\]' AND full_ref !~ E'[\\x00-\\x20\\x7F]' AND full_ref !~ '//' AND full_ref !~ '/$' AND full_ref !~ '\.\.' AND full_ref !~ '@\{' AND full_ref !~ '(^|/)\.' AND full_ref !~ '\.lock($|/)' AND full_ref !~ '\.($|/)'),
  commit_sha TEXT NOT NULL CHECK (commit_sha ~ '^[0-9a-f]{40}$' AND commit_sha <> repeat('0',40)),
  request_nonce TEXT NOT NULL CONSTRAINT manual_deployment_intents_request_nonce_check CHECK (octet_length(request_nonce) BETWEEN 16 AND 128 AND request_nonce ~ '^[A-Za-z0-9_-]+$'),
  deployment_id TEXT NOT NULL UNIQUE REFERENCES deployments(id) DEFERRABLE INITIALLY DEFERRED CHECK (octet_length(deployment_id) BETWEEN 1 AND 128),
  created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  FOREIGN KEY (actor_user_id, owner_id) REFERENCES users(id, owner_id) ON DELETE RESTRICT,
  FOREIGN KEY (owner_id, binding_project_id) REFERENCES github_project_bindings(owner_id, project_id) ON DELETE RESTRICT,
  FOREIGN KEY (owner_id, repository_id) REFERENCES github_project_bindings(owner_id, repository_id) ON DELETE RESTRICT,
  UNIQUE (owner_id, request_nonce)
);
CREATE INDEX manual_deployment_intents_owner_created_idx ON manual_deployment_intents(owner_id, created_at DESC);

CREATE FUNCTION reject_manual_intent_mutation() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'manual deployment intents are immutable'; END; $$;
CREATE TRIGGER manual_deployment_intents_no_update BEFORE UPDATE ON manual_deployment_intents FOR EACH ROW EXECUTE FUNCTION reject_manual_intent_mutation();
CREATE TRIGGER manual_deployment_intents_no_delete BEFORE DELETE ON manual_deployment_intents FOR EACH ROW EXECUTE FUNCTION reject_manual_intent_mutation();
