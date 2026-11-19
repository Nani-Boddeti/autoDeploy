-- Re-enrollment of an already-enrolled server (slice 22-8). After a restore the
-- agent can hold a credential the database does not know; migration 000012
-- allowed exactly one token row per server and froze it once consumed, so such
-- a server could never receive a new credential. A server may now hold one
-- additional single-use token with purpose 'reenroll', issued by an
-- administrator for an active, already-enrolled server. See
-- docs/adr/0001-control-plane-agent-separation.md (re-enrollment amendment).
--
-- Rules kept from 000012: a consumed token is frozen forever, rows are never
-- deleted, consumption never alters issuance, and an enrollment-purpose token
-- can never be issued to a server that already consumed one.
-- Rules added: surrogate id, purpose, at most one UNCONSUMED token per server,
-- at most one 'enroll' token per server ever, and a 'reenroll' token can be
-- inserted only for an active server that already consumed its enroll token.

ALTER TABLE server_enrollment_tokens DROP CONSTRAINT server_enrollment_tokens_pkey;
ALTER TABLE server_enrollment_tokens ADD COLUMN id BIGINT GENERATED ALWAYS AS IDENTITY;
ALTER TABLE server_enrollment_tokens ADD CONSTRAINT server_enrollment_tokens_pkey PRIMARY KEY (id);
-- Existing rows predate re-enrollment: they are all first-enrollment tokens.
ALTER TABLE server_enrollment_tokens ADD COLUMN purpose TEXT NOT NULL DEFAULT 'enroll';
ALTER TABLE server_enrollment_tokens ALTER COLUMN purpose DROP DEFAULT;
ALTER TABLE server_enrollment_tokens ADD CONSTRAINT server_enrollment_tokens_purpose_check CHECK (purpose IN ('enroll','reenroll'));

CREATE INDEX server_enrollment_tokens_server_idx ON server_enrollment_tokens(server_id, issued_at);
CREATE UNIQUE INDEX server_enrollment_tokens_one_unconsumed_uq ON server_enrollment_tokens(server_id) WHERE consumed_at IS NULL;
CREATE UNIQUE INDEX server_enrollment_tokens_one_enroll_uq ON server_enrollment_tokens(server_id) WHERE purpose = 'enroll';

CREATE OR REPLACE FUNCTION validate_server_enrollment_token_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
  IF NEW.server_id <> OLD.server_id THEN
    RAISE EXCEPTION 'enrollment token server is immutable';
  END IF;
  IF NEW.purpose <> OLD.purpose THEN
    RAISE EXCEPTION 'enrollment token purpose is immutable';
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

-- A re-enrollment token is only meaningful for a live, already-enrolled
-- server, and is always born unconsumed.
CREATE FUNCTION validate_server_enrollment_token_insert() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
  IF NEW.purpose = 'reenroll' THEN
    IF NEW.consumed_at IS NOT NULL THEN
      RAISE EXCEPTION 're-enrollment token must be inserted unconsumed';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM servers WHERE id = NEW.server_id AND status = 'active') THEN
      RAISE EXCEPTION 're-enrollment token requires an active server';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM server_enrollment_tokens WHERE server_id = NEW.server_id AND purpose = 'enroll' AND consumed_at IS NOT NULL) THEN
      RAISE EXCEPTION 're-enrollment token requires an already enrolled server';
    END IF;
  END IF;
  RETURN NEW;
END; $$;
CREATE TRIGGER server_enrollment_tokens_validate_insert BEFORE INSERT ON server_enrollment_tokens FOR EACH ROW EXECUTE FUNCTION validate_server_enrollment_token_insert();

ALTER TABLE server_audit_events DROP CONSTRAINT server_audit_events_action_check;
ALTER TABLE server_audit_events ADD CONSTRAINT server_audit_events_action_check CHECK (action IN ('register','enrollment_issued','reenrollment_issued','enrolled','enrollment_rejected','credential_rotated','credential_revoked','server_revoked','heartbeat_rejected','environment_mark_reconciled'));
