-- 24-E1: GitHub App creation through the manifest flow (tasks/24-E-plan.md).
-- Two retained, append-mostly tables plus two audit actions. Secrets (App
-- private key, webhook secret) are stored only as crypto.Envelope blobs.

-- One-time create capabilities. Only the domain-separated digest of the state
-- is stored. session_id deliberately has NO foreign key to admin_sessions:
-- these rows are retained (never deleted) and session cleanup deletes
-- sessions, so a foreign key would make cleanup fail. The repository checks
-- session liveness instead.
CREATE TABLE github_manifest_states (
  digest BYTEA PRIMARY KEY CONSTRAINT github_manifest_states_digest_len CHECK (octet_length(digest) = 32),
  owner_id TEXT NOT NULL REFERENCES owners(id),
  user_id TEXT NOT NULL,
  session_id TEXT NOT NULL CHECK (octet_length(session_id) BETWEEN 1 AND 128),
  auth_revision BIGINT NOT NULL CHECK (auth_revision > 0),
  created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  expires_at TIMESTAMPTZ NOT NULL,
  consumed_at TIMESTAMPTZ,
  FOREIGN KEY (user_id, owner_id) REFERENCES users(id, owner_id),
  CONSTRAINT github_manifest_states_expiry_after_issue CHECK (expires_at > created_at),
  CONSTRAINT github_manifest_states_ttl_cap CHECK (expires_at <= created_at + interval '1 hour'),
  CONSTRAINT github_manifest_states_consumed_not_before_issue CHECK (consumed_at IS NULL OR consumed_at >= created_at),
  CONSTRAINT github_manifest_states_consumed_before_expiry CHECK (consumed_at IS NULL OR consumed_at <= expires_at)
);
-- One live capability per session: issuing a new one retires the old by
-- shortening its expiry (rows are never deleted).
CREATE INDEX github_manifest_states_session_idx ON github_manifest_states(session_id, expires_at);
CREATE INDEX github_manifest_states_consumed_idx ON github_manifest_states(consumed_at) WHERE consumed_at IS NOT NULL;

-- An unconsumed row may be consumed (issuance untouched) or retired early by
-- shortening expires_at; a consumed row never changes; lifetime never grows.
CREATE FUNCTION validate_github_manifest_state_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
  IF NEW.digest <> OLD.digest OR NEW.owner_id <> OLD.owner_id OR NEW.user_id <> OLD.user_id
     OR NEW.session_id <> OLD.session_id OR NEW.auth_revision <> OLD.auth_revision OR NEW.created_at <> OLD.created_at THEN
    RAISE EXCEPTION 'manifest state issuance is immutable';
  END IF;
  IF OLD.consumed_at IS NOT NULL THEN
    RAISE EXCEPTION 'consumed manifest state cannot change';
  END IF;
  -- Consumption is stamped now: the runtime role cannot backdate or forward-date it
  -- (the in-flight window logic depends on consumed_at being real).
  IF NEW.consumed_at IS NOT NULL AND abs(extract(epoch FROM (NEW.consumed_at - clock_timestamp()))) > 5 THEN
    RAISE EXCEPTION 'manifest state consumed_at must be current';
  END IF;
  IF NEW.expires_at > OLD.expires_at THEN
    RAISE EXCEPTION 'manifest state lifetime cannot be extended';
  END IF;
  RETURN NEW;
END; $$;
CREATE TRIGGER github_manifest_states_validate_update BEFORE UPDATE ON github_manifest_states FOR EACH ROW EXECUTE FUNCTION validate_github_manifest_state_update();
CREATE FUNCTION reject_github_manifest_state_delete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'manifest states are retained'; END; $$;
CREATE TRIGGER github_manifest_states_no_delete BEFORE DELETE ON github_manifest_states FOR EACH ROW EXECUTE FUNCTION reject_github_manifest_state_delete();

-- The App created through the manifest flow. Not github_app_registrations:
-- that table (000005) is the env-configured App row that webhook and checkout
-- tables reference. At most one row; replacing it is break-glass (out of scope).
-- id is the crypto SecretVersionID of both envelopes.
CREATE TABLE github_manifest_apps (
  singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CONSTRAINT github_manifest_apps_singleton CHECK (singleton),
  id TEXT NOT NULL UNIQUE CHECK (octet_length(id) BETWEEN 1 AND 128),
  app_id BIGINT NOT NULL UNIQUE CONSTRAINT github_manifest_apps_app_id_positive CHECK (app_id > 0),
  slug TEXT NOT NULL CHECK (octet_length(slug) BETWEEN 1 AND 100),
  client_id TEXT NOT NULL CHECK (octet_length(client_id) BETWEEN 1 AND 255),
  github_owner_id BIGINT NOT NULL CHECK (github_owner_id > 0),
  github_owner_type TEXT NOT NULL CONSTRAINT github_manifest_apps_owner_type CHECK (github_owner_type IN ('User','Organization')),
  github_owner_login TEXT NOT NULL CHECK (octet_length(github_owner_login) BETWEEN 1 AND 100),
  html_url TEXT NOT NULL CHECK (octet_length(html_url) BETWEEN 1 AND 512),
  private_key_envelope BYTEA NOT NULL CONSTRAINT github_manifest_apps_key_envelope_len CHECK (octet_length(private_key_envelope) BETWEEN 64 AND 70000),
  webhook_secret_envelope BYTEA NOT NULL CONSTRAINT github_manifest_apps_secret_envelope_len CHECK (octet_length(webhook_secret_envelope) BETWEEN 64 AND 70000),
  -- KEK ids the envelopes were wrapped with, copied from the envelopes by the
  -- repository: the KEK-retirement and ledger checks must see this reference.
  private_key_kek_id TEXT NOT NULL CHECK (octet_length(private_key_kek_id) BETWEEN 1 AND 64),
  webhook_secret_kek_id TEXT NOT NULL CHECK (octet_length(webhook_secret_kek_id) BETWEEN 1 AND 64),
  owner_id TEXT NOT NULL REFERENCES owners(id),
  created_by_user_id TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  FOREIGN KEY (created_by_user_id, owner_id) REFERENCES users(id, owner_id)
);
CREATE FUNCTION reject_github_manifest_app_change() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'manifest GitHub App is immutable'; END; $$;
CREATE TRIGGER github_manifest_apps_no_update BEFORE UPDATE ON github_manifest_apps FOR EACH ROW EXECUTE FUNCTION reject_github_manifest_app_change();
CREATE TRIGGER github_manifest_apps_no_delete BEFORE DELETE ON github_manifest_apps FOR EACH ROW EXECUTE FUNCTION reject_github_manifest_app_change();

ALTER TABLE github_audit_events DROP CONSTRAINT github_audit_events_action_check;
ALTER TABLE github_audit_events ADD CONSTRAINT github_audit_events_action_check CHECK (action IN ('setup_started','callback_verified','setup_completed','repository_bound','installation_disabled','repository_disabled','app_create_started','app_created'));

-- Runtime role (000030): withdraw the default full DML, grant only what the
-- code needs. States: read, insert, consume, retire early. Apps: read, insert.
REVOKE ALL ON github_manifest_states FROM autodeploy_app;
GRANT SELECT, INSERT ON github_manifest_states TO autodeploy_app;
GRANT UPDATE (consumed_at, expires_at) ON github_manifest_states TO autodeploy_app;
REVOKE ALL ON github_manifest_apps FROM autodeploy_app;
GRANT SELECT, INSERT ON github_manifest_apps TO autodeploy_app;
