-- Durable deployment-agent server identity, scoped bearer credentials,
-- single-use enrollment tokens, and an append-only audit trail. See
-- docs/adr/0001-control-plane-agent-separation.md (agent identity/authority)
-- and docs/adr/0003-secret-handling.md (one-way verifiers only; no raw
-- token/digest ever leaves this boundary into audit or logs).

CREATE TABLE servers (
  id TEXT PRIMARY KEY CHECK (octet_length(id) BETWEEN 1 AND 128),
  owner_id TEXT NOT NULL REFERENCES owners(id) ON DELETE RESTRICT,
  name TEXT NOT NULL CHECK (octet_length(name) BETWEEN 1 AND 128 AND name = btrim(name) AND name !~ '[\x00-\x1f\x7f]'),
  protocol_version SMALLINT NOT NULL CHECK (protocol_version = 1),
  status TEXT NOT NULL CHECK (status IN ('active','revoked')),
  created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  revoked_at TIMESTAMPTZ,
  last_heartbeat_at TIMESTAMPTZ,
  CHECK ((status = 'revoked') = (revoked_at IS NOT NULL)),
  CHECK (revoked_at IS NULL OR revoked_at >= created_at),
  CHECK (last_heartbeat_at IS NULL OR last_heartbeat_at >= created_at)
);
CREATE INDEX servers_owner_idx ON servers(owner_id);

-- Identity (id/owner_id/name/protocol_version/created_at) never changes.
-- The only legal transitions are active->revoked (revoked_at set, exactly
-- once) and a monotonic last_heartbeat_at advance; both may be combined
-- with an unrelated heartbeat update in the same statement only if the
-- heartbeat value itself still does not decrease.
CREATE FUNCTION validate_server_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
  IF NEW.id <> OLD.id OR NEW.owner_id <> OLD.owner_id OR NEW.name <> OLD.name OR NEW.protocol_version <> OLD.protocol_version OR NEW.created_at <> OLD.created_at THEN
    RAISE EXCEPTION 'server identity is immutable';
  END IF;
  IF OLD.last_heartbeat_at IS NOT NULL AND NEW.last_heartbeat_at IS NOT NULL AND NEW.last_heartbeat_at < OLD.last_heartbeat_at THEN
    RAISE EXCEPTION 'server heartbeat must be monotonic';
  END IF;
  IF OLD.last_heartbeat_at IS NOT NULL AND NEW.last_heartbeat_at IS NULL THEN
    RAISE EXCEPTION 'server heartbeat must be monotonic';
  END IF;
  IF NEW.status = OLD.status THEN
    IF NEW.status <> 'active' OR NEW.revoked_at IS NOT NULL THEN
      RAISE EXCEPTION 'invalid server transition';
    END IF;
    RETURN NEW;
  END IF;
  IF OLD.status = 'active' AND NEW.status = 'revoked' AND NEW.revoked_at IS NOT NULL THEN
    RETURN NEW;
  END IF;
  RAISE EXCEPTION 'invalid server transition';
END; $$;
CREATE TRIGGER servers_validate_update BEFORE UPDATE ON servers FOR EACH ROW EXECUTE FUNCTION validate_server_update();
CREATE FUNCTION reject_server_delete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'servers are retained'; END; $$;
CREATE TRIGGER servers_no_delete BEFORE DELETE ON servers FOR EACH ROW EXECUTE FUNCTION reject_server_delete();

-- Only the digest of a bearer credential is ever stored. Exactly one active
-- credential per server at a time; rotation revokes the active row and
-- inserts version+1 in the same transaction.
CREATE TABLE server_credentials (
  id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  server_id TEXT NOT NULL REFERENCES servers(id) ON DELETE RESTRICT,
  version INTEGER NOT NULL CHECK (version > 0),
  digest BYTEA NOT NULL CHECK (octet_length(digest) = 32),
  status TEXT NOT NULL CHECK (status IN ('active','revoked')),
  issued_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  revoked_at TIMESTAMPTZ,
  CHECK ((status = 'revoked') = (revoked_at IS NOT NULL)),
  CHECK (revoked_at IS NULL OR revoked_at >= issued_at),
  UNIQUE (server_id, version)
);
CREATE UNIQUE INDEX server_credentials_one_active_uq ON server_credentials(server_id) WHERE status = 'active';

CREATE FUNCTION validate_server_credential_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
  IF NEW.id <> OLD.id OR NEW.server_id <> OLD.server_id OR NEW.version <> OLD.version OR NEW.digest <> OLD.digest OR NEW.issued_at <> OLD.issued_at THEN
    RAISE EXCEPTION 'server credential identity is immutable';
  END IF;
  IF OLD.status <> 'active' OR NEW.status <> 'revoked' OR NEW.revoked_at IS NULL THEN
    RAISE EXCEPTION 'invalid server credential transition';
  END IF;
  RETURN NEW;
END; $$;
CREATE TRIGGER server_credentials_validate_update BEFORE UPDATE ON server_credentials FOR EACH ROW EXECUTE FUNCTION validate_server_credential_update();
CREATE FUNCTION reject_server_credential_delete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'server credentials are retained'; END; $$;
CREATE TRIGGER server_credentials_no_delete BEFORE DELETE ON server_credentials FOR EACH ROW EXECUTE FUNCTION reject_server_credential_delete();

-- One outstanding enrollment token per server. Reissue overwrites digest and
-- lifetime and invalidates the prior unconsumed token; a consumed token can
-- never be reissued or altered again (enforced below), so an already
-- enrolled server can never receive a fresh enrollment token.
CREATE TABLE server_enrollment_tokens (
  server_id TEXT PRIMARY KEY REFERENCES servers(id) ON DELETE RESTRICT,
  digest BYTEA NOT NULL CHECK (octet_length(digest) = 32),
  issued_at TIMESTAMPTZ NOT NULL,
  expires_at TIMESTAMPTZ NOT NULL CHECK (expires_at > issued_at),
  consumed_at TIMESTAMPTZ CHECK (consumed_at IS NULL OR (consumed_at >= issued_at AND consumed_at <= expires_at))
);

CREATE FUNCTION validate_server_enrollment_token_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
  IF NEW.server_id <> OLD.server_id THEN
    RAISE EXCEPTION 'enrollment token server is immutable';
  END IF;
  IF OLD.consumed_at IS NOT NULL THEN
    RAISE EXCEPTION 'consumed enrollment token cannot change';
  END IF;
  IF NEW.consumed_at IS NOT NULL THEN
    IF NEW.digest <> OLD.digest OR NEW.issued_at <> OLD.issued_at OR NEW.expires_at <> OLD.expires_at THEN
      RAISE EXCEPTION 'enrollment token consumption must not alter issuance';
    END IF;
  END IF;
  RETURN NEW;
END; $$;
CREATE TRIGGER server_enrollment_tokens_validate_update BEFORE UPDATE ON server_enrollment_tokens FOR EACH ROW EXECUTE FUNCTION validate_server_enrollment_token_update();
CREATE FUNCTION reject_server_enrollment_token_delete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'server enrollment tokens are retained'; END; $$;
CREATE TRIGGER server_enrollment_tokens_no_delete BEFORE DELETE ON server_enrollment_tokens FOR EACH ROW EXECUTE FUNCTION reject_server_enrollment_token_delete();

-- Append-only. Never contains a raw token, digest, or other secret value.
CREATE TABLE server_audit_events (
  id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  actor_type TEXT NOT NULL CHECK (actor_type IN ('administrator','agent','system')),
  actor_id TEXT NOT NULL DEFAULT '' CHECK (octet_length(actor_id) <= 128),
  action TEXT NOT NULL CHECK (action IN ('register','enrollment_issued','enrolled','enrollment_rejected','credential_rotated','credential_revoked','server_revoked','heartbeat_rejected','environment_mark_reconciled')),
  server_id TEXT REFERENCES servers(id) ON DELETE RESTRICT,
  credential_version INTEGER CHECK (credential_version IS NULL OR credential_version > 0),
  result TEXT NOT NULL CHECK (result IN ('success','failure')),
  reason TEXT NOT NULL DEFAULT '' CHECK (octet_length(reason) <= 512 AND reason !~ '[\x00-\x1f\x7f]'),
  created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX server_audit_events_server_idx ON server_audit_events(server_id, created_at);
CREATE FUNCTION reject_server_audit_event_mutation() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'server audit events are append-only'; END; $$;
CREATE TRIGGER server_audit_events_no_update BEFORE UPDATE ON server_audit_events FOR EACH ROW EXECUTE FUNCTION reject_server_audit_event_mutation();
CREATE TRIGGER server_audit_events_no_delete BEFORE DELETE ON server_audit_events FOR EACH ROW EXECUTE FUNCTION reject_server_audit_event_mutation();
