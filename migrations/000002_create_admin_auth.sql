-- Administrator authentication.  This migration is intentionally extension-free.
CREATE TABLE owners (
  id TEXT PRIMARY KEY CHECK (octet_length(id) BETWEEN 1 AND 128),
  created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE installation_auth (
  id SMALLINT PRIMARY KEY CHECK (id = 1),
  owner_id TEXT REFERENCES owners(id),
  password_policy_revision BIGINT,
  password_memory_kib INTEGER,
  password_iterations INTEGER,
  password_lanes SMALLINT,
  bootstrapped_at TIMESTAMPTZ,
  CHECK ((owner_id IS NULL) = (password_policy_revision IS NULL)),
  CHECK ((owner_id IS NULL) = (password_memory_kib IS NULL)),
  CHECK ((owner_id IS NULL) = (password_iterations IS NULL)),
  CHECK ((owner_id IS NULL) = (password_lanes IS NULL)),
  CHECK ((owner_id IS NULL) = (bootstrapped_at IS NULL)),
  CHECK (password_policy_revision IS NULL OR password_policy_revision > 0),
  CHECK (password_memory_kib IS NULL OR password_memory_kib BETWEEN 19456 AND 65536),
  CHECK (password_iterations IS NULL OR password_iterations BETWEEN 2 AND 5),
  CHECK (password_lanes IS NULL OR password_lanes = 1)
);
INSERT INTO installation_auth (id) VALUES (1);

CREATE TABLE users (
  id TEXT PRIMARY KEY CHECK (octet_length(id) BETWEEN 1 AND 128),
  owner_id TEXT NOT NULL REFERENCES owners(id),
  username TEXT NOT NULL UNIQUE CHECK (username ~ '^[a-z0-9][a-z0-9._-]{2,63}$'),
  role TEXT NOT NULL CHECK (role = 'owner'),
  password_phc TEXT NOT NULL CHECK (octet_length(password_phc) BETWEEN 68 AND 128),
  password_policy_revision BIGINT NOT NULL CHECK (password_policy_revision > 0),
  auth_revision BIGINT NOT NULL DEFAULT 1 CHECK (auth_revision > 0),
  status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
  created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
  ,UNIQUE (id, owner_id)
  ,CHECK (updated_at >= created_at)
);
CREATE INDEX users_owner_id_idx ON users(owner_id);

CREATE TABLE admin_sessions (
  id TEXT PRIMARY KEY CHECK (octet_length(id) BETWEEN 1 AND 128),
  token_digest BYTEA NOT NULL UNIQUE CHECK (octet_length(token_digest) = 32),
  user_id TEXT NOT NULL,
  owner_id TEXT NOT NULL REFERENCES owners(id),
  auth_revision BIGINT NOT NULL CHECK (auth_revision > 0),
  created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  last_seen_at TIMESTAMPTZ NOT NULL,
  idle_expires_at TIMESTAMPTZ NOT NULL,
  absolute_expires_at TIMESTAMPTZ NOT NULL,
  revoked_at TIMESTAMPTZ,
  revoked_reason TEXT CHECK (revoked_reason IS NULL OR revoked_reason IN ('logout', 'session_limit', 'password_reset')),
  CHECK (last_seen_at >= created_at AND idle_expires_at >= last_seen_at AND absolute_expires_at >= idle_expires_at),
  CHECK ((revoked_at IS NULL) = (revoked_reason IS NULL)),
  CHECK (revoked_at IS NULL OR revoked_at >= created_at),
  FOREIGN KEY (user_id, owner_id) REFERENCES users(id, owner_id)
);
CREATE INDEX admin_sessions_active_user_idx ON admin_sessions(user_id, created_at) WHERE revoked_at IS NULL;
CREATE INDEX admin_sessions_active_expiry_idx ON admin_sessions(idle_expires_at, absolute_expires_at) WHERE revoked_at IS NULL;

CREATE TABLE auth_throttle_buckets (
  kind TEXT NOT NULL CHECK (kind IN ('pair', 'ip', 'invalid_forward', 'pair_overflow', 'ip_overflow', 'invalid_forward_overflow')),
  key_version TEXT NOT NULL CHECK (octet_length(key_version) BETWEEN 1 AND 32),
  identifier_digest BYTEA NOT NULL CHECK (octet_length(identifier_digest) = 32),
  window_started_at TIMESTAMPTZ NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  failures INTEGER NOT NULL DEFAULT 0 CHECK (failures >= 0 AND failures <= attempts),
  blocked_until TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY(kind, key_version, identifier_digest, window_started_at),
  CHECK (date_trunc('minute', window_started_at) = window_started_at),
  CHECK (mod(extract(minute FROM window_started_at)::integer, 15) = 0),
  CHECK (updated_at >= created_at),
  CHECK (blocked_until IS NULL OR (blocked_until >= created_at AND blocked_until <= created_at + interval '15 minutes'))
);
CREATE INDEX auth_throttle_buckets_cleanup_idx ON auth_throttle_buckets(window_started_at);

CREATE TABLE auth_username_throttles (
  key_version TEXT NOT NULL CHECK (octet_length(key_version) BETWEEN 1 AND 32),
  identifier_digest BYTEA NOT NULL CHECK (octet_length(identifier_digest) = 32),
  failures SMALLINT NOT NULL DEFAULT 0 CHECK (failures BETWEEN 0 AND 15),
  blocked_until TIMESTAMPTZ,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (key_version, identifier_digest),
  CHECK ((key_version = 'overflow') = (identifier_digest = decode(repeat('00', 32), 'hex'))),
  CHECK (blocked_until IS NULL OR (blocked_until >= updated_at AND blocked_until <= updated_at + interval '15 minutes'))
);
CREATE INDEX auth_username_throttles_cleanup_idx ON auth_username_throttles(updated_at);

-- Persist the installation-wide throttle cardinality chosen by the first
-- replica.  Capacity is reserved for one overflow row per throttle kind.
CREATE TABLE auth_throttle_state (
  id SMALLINT PRIMARY KEY CHECK (id = 1),
  max_rows INTEGER CHECK (max_rows IS NULL OR max_rows BETWEEN 8 AND 100000)
);
INSERT INTO auth_throttle_state(id) VALUES (1);

CREATE TABLE auth_audit_events (
  id TEXT PRIMARY KEY CHECK (octet_length(id) BETWEEN 1 AND 128),
  actor_user_id TEXT,
  owner_id TEXT NOT NULL REFERENCES owners(id),
  action TEXT NOT NULL CHECK (action IN ('bootstrap', 'login_success', 'logout', 'session_revoked', 'password_reset')),
  result TEXT NOT NULL CHECK (result IN ('success', 'already_revoked')),
  metadata JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (octet_length(metadata::text) <= 1024 AND jsonb_typeof(metadata) = 'object'),
  occurred_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  FOREIGN KEY (actor_user_id, owner_id) REFERENCES users(id, owner_id)
);
CREATE INDEX auth_audit_events_owner_occurred_idx ON auth_audit_events(owner_id, occurred_at DESC);

CREATE FUNCTION reject_auth_audit_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'auth audit events are append-only'; END;
$$;
CREATE TRIGGER auth_audit_events_no_update BEFORE UPDATE ON auth_audit_events FOR EACH ROW EXECUTE FUNCTION reject_auth_audit_mutation();
CREATE TRIGGER auth_audit_events_no_delete BEFORE DELETE ON auth_audit_events FOR EACH ROW EXECUTE FUNCTION reject_auth_audit_mutation();
