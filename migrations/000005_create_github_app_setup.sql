-- GitHub App setup capabilities and explicit, owner-scoped repository bindings.
CREATE TABLE github_app_registrations (
  id SMALLINT PRIMARY KEY CHECK (id = 1),
  app_id BIGINT NOT NULL UNIQUE CHECK (app_id > 0),
  client_id TEXT NOT NULL CHECK (octet_length(client_id) BETWEEN 1 AND 255),
  slug TEXT NOT NULL CHECK (octet_length(slug) BETWEEN 1 AND 100),
  owner_account_id BIGINT NOT NULL CHECK (owner_account_id > 0),
  owner_type TEXT NOT NULL CHECK (owner_type IN ('User','Organization')),
  active BOOLEAN NOT NULL DEFAULT true,
  created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  UNIQUE (id, owner_account_id)
);

CREATE TABLE github_setup_states (
  digest BYTEA PRIMARY KEY CHECK (octet_length(digest) = 32),
  owner_id TEXT NOT NULL REFERENCES owners(id),
  user_id TEXT NOT NULL,
  session_id TEXT NOT NULL,
  auth_revision BIGINT NOT NULL CHECK (auth_revision > 0),
  expires_at TIMESTAMPTZ NOT NULL,
  consumed_at TIMESTAMPTZ,
  recoverable_failure_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  FOREIGN KEY (user_id, owner_id) REFERENCES users(id, owner_id),
  FOREIGN KEY (session_id) REFERENCES admin_sessions(id),
  CHECK (expires_at > created_at),
  CHECK (consumed_at IS NULL OR consumed_at >= created_at)
);
CREATE UNIQUE INDEX github_setup_states_one_active_session_idx ON github_setup_states(session_id) WHERE consumed_at IS NULL;
CREATE INDEX github_setup_states_cleanup_idx ON github_setup_states(expires_at) WHERE consumed_at IS NULL;
CREATE TABLE github_setup_state_limits (
  id SMALLINT PRIMARY KEY CHECK (id = 1),
  max_active INTEGER NOT NULL CHECK (max_active BETWEEN 8 AND 100000)
);
INSERT INTO github_setup_state_limits(id,max_active) VALUES (1,10000);

CREATE TABLE github_pending_handoffs (
  digest BYTEA PRIMARY KEY CHECK (octet_length(digest) = 32),
  state_digest BYTEA NOT NULL UNIQUE REFERENCES github_setup_states(digest),
  owner_id TEXT NOT NULL REFERENCES owners(id),
  user_id TEXT NOT NULL,
  session_id TEXT NOT NULL,
  auth_revision BIGINT NOT NULL CHECK (auth_revision > 0),
  installation_id BIGINT NOT NULL CHECK (installation_id > 0),
  account_id BIGINT NOT NULL CHECK (account_id > 0),
  account_type TEXT NOT NULL CHECK (account_type IN ('User','Organization')),
  expires_at TIMESTAMPTZ NOT NULL,
  consumed_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  FOREIGN KEY (user_id, owner_id) REFERENCES users(id, owner_id),
  FOREIGN KEY (session_id) REFERENCES admin_sessions(id),
  CHECK (expires_at > created_at)
);
CREATE INDEX github_pending_handoffs_cleanup_idx ON github_pending_handoffs(expires_at) WHERE consumed_at IS NULL;

CREATE TABLE github_installations (
  installation_id BIGINT PRIMARY KEY CHECK (installation_id > 0),
  owner_id TEXT NOT NULL REFERENCES owners(id),
  account_id BIGINT NOT NULL CHECK (account_id > 0),
  account_type TEXT NOT NULL CHECK (account_type IN ('User','Organization')),
  active BOOLEAN NOT NULL DEFAULT true,
  suspended BOOLEAN NOT NULL DEFAULT false,
  authorization_revision BIGINT NOT NULL DEFAULT 1 CHECK (authorization_revision > 0),
  created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  UNIQUE (installation_id, owner_id),
  UNIQUE (owner_id, account_id)
);

CREATE TABLE github_repositories (
  repository_id BIGINT PRIMARY KEY CHECK (repository_id > 0),
  installation_id BIGINT NOT NULL,
  owner_id TEXT NOT NULL,
  node_id TEXT CHECK (node_id IS NULL OR octet_length(node_id) BETWEEN 1 AND 255),
  github_owner_id BIGINT NOT NULL CHECK (github_owner_id > 0),
  name TEXT NOT NULL CHECK (octet_length(name) BETWEEN 1 AND 255),
  full_name TEXT NOT NULL CHECK (octet_length(full_name) BETWEEN 3 AND 512),
  default_branch TEXT CHECK (default_branch IS NULL OR octet_length(default_branch) BETWEEN 1 AND 255),
  active BOOLEAN NOT NULL DEFAULT true,
  authorization_revision BIGINT NOT NULL DEFAULT 1 CHECK (authorization_revision > 0),
  created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  FOREIGN KEY (installation_id, owner_id) REFERENCES github_installations(installation_id, owner_id),
  UNIQUE (repository_id, owner_id)
);
CREATE INDEX github_repositories_installation_idx ON github_repositories(installation_id, owner_id) WHERE active;

CREATE TABLE github_project_bindings (
  owner_id TEXT NOT NULL REFERENCES owners(id),
  project_id TEXT NOT NULL CHECK (octet_length(project_id) BETWEEN 1 AND 128),
  repository_id BIGINT NOT NULL,
  installation_id BIGINT NOT NULL,
  active BOOLEAN NOT NULL DEFAULT true,
  authorization_revision BIGINT NOT NULL DEFAULT 1 CHECK (authorization_revision > 0),
  created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (owner_id, project_id),
  FOREIGN KEY (repository_id, owner_id) REFERENCES github_repositories(repository_id, owner_id),
  FOREIGN KEY (installation_id, owner_id) REFERENCES github_installations(installation_id, owner_id),
  UNIQUE (owner_id, repository_id)
);

CREATE TABLE github_audit_events (
  id TEXT PRIMARY KEY CHECK (octet_length(id) BETWEEN 1 AND 128),
  owner_id TEXT NOT NULL REFERENCES owners(id),
  actor_user_id TEXT,
  action TEXT NOT NULL CHECK (action IN ('setup_started','callback_verified','setup_completed','repository_bound','installation_disabled','repository_disabled')),
  result TEXT NOT NULL CHECK (result IN ('success','denied','failed')),
  installation_id BIGINT,
  repository_id BIGINT,
  project_id TEXT CHECK (project_id IS NULL OR octet_length(project_id) <= 128),
  metadata JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(metadata) = 'object' AND octet_length(metadata::text) <= 1024),
  occurred_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  FOREIGN KEY (actor_user_id, owner_id) REFERENCES users(id, owner_id)
);
CREATE INDEX github_audit_events_owner_time_idx ON github_audit_events(owner_id, occurred_at DESC);
CREATE FUNCTION reject_github_audit_mutation() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'github audit events are append-only'; END; $$;
CREATE TRIGGER github_audit_events_no_update BEFORE UPDATE ON github_audit_events FOR EACH ROW EXECUTE FUNCTION reject_github_audit_mutation();
CREATE TRIGGER github_audit_events_no_delete BEFORE DELETE ON github_audit_events FOR EACH ROW EXECUTE FUNCTION reject_github_audit_mutation();
