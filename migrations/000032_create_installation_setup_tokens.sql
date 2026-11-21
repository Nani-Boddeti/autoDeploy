-- 24-C: browser first-run admin setup (ADR 0006 amendment 2026-11-21).
-- One-time, short-lived setup codes minted by the host operator
-- (`admin setup-token`) and consumed atomically with the bootstrap
-- transaction. Only the domain-separated digest is stored. Rows are retained
-- (no delete) as the record that a code existed.
CREATE TABLE installation_setup_tokens (
  id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  digest BYTEA NOT NULL UNIQUE CHECK (octet_length(digest) = 32),
  issued_at TIMESTAMPTZ NOT NULL,
  expires_at TIMESTAMPTZ NOT NULL CHECK (expires_at > issued_at),
  consumed_at TIMESTAMPTZ CHECK (consumed_at IS NULL OR (consumed_at >= issued_at AND consumed_at <= expires_at))
);

-- A consumed row never changes. An unconsumed row may be consumed (issuance
-- untouched) or retired early by shortening expires_at (minting a newer code
-- retires older ones); its lifetime may never be extended.
CREATE FUNCTION validate_installation_setup_token_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
  IF NEW.id <> OLD.id OR NEW.digest <> OLD.digest OR NEW.issued_at <> OLD.issued_at THEN
    RAISE EXCEPTION 'setup token issuance is immutable';
  END IF;
  IF OLD.consumed_at IS NOT NULL THEN
    RAISE EXCEPTION 'consumed setup token cannot change';
  END IF;
  IF NEW.expires_at > OLD.expires_at THEN
    RAISE EXCEPTION 'setup token lifetime cannot be extended';
  END IF;
  RETURN NEW;
END; $$;
CREATE TRIGGER installation_setup_tokens_validate_update BEFORE UPDATE ON installation_setup_tokens FOR EACH ROW EXECUTE FUNCTION validate_installation_setup_token_update();
CREATE FUNCTION reject_installation_setup_token_delete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'setup tokens are retained'; END; $$;
CREATE TRIGGER installation_setup_tokens_no_delete BEFORE DELETE ON installation_setup_tokens FOR EACH ROW EXECUTE FUNCTION reject_installation_setup_token_delete();

-- The web first-run path records its own audit action.
ALTER TABLE auth_audit_events DROP CONSTRAINT auth_audit_events_action_check;
ALTER TABLE auth_audit_events ADD CONSTRAINT auth_audit_events_action_check CHECK (action IN ('bootstrap', 'bootstrap_web', 'login_success', 'logout', 'session_revoked', 'password_reset'));

-- Runtime role (000030). Its default privileges would hand a new table full
-- DML, so withdraw that and grant only what the code needs: read, insert, and
-- update of the two lifecycle columns (consume; retire by shortening expiry).
-- No DELETE, and digest/issued_at stay unwritable even by the app role.
REVOKE ALL ON installation_setup_tokens FROM autodeploy_app;
GRANT SELECT, INSERT ON installation_setup_tokens TO autodeploy_app;
GRANT UPDATE (consumed_at, expires_at) ON installation_setup_tokens TO autodeploy_app;
