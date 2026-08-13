-- GitHub capabilities must remain tied to the exact session/user/owner tuple.
ALTER TABLE admin_sessions
  ADD CONSTRAINT admin_sessions_id_user_owner_key UNIQUE (id, user_id, owner_id);

ALTER TABLE github_setup_states
  DROP CONSTRAINT github_setup_states_session_id_fkey,
  ADD CONSTRAINT github_setup_states_session_owner_user_fkey
    FOREIGN KEY (session_id, user_id, owner_id)
    REFERENCES admin_sessions(id, user_id, owner_id);

ALTER TABLE github_pending_handoffs
  DROP CONSTRAINT github_pending_handoffs_session_id_fkey,
  ADD CONSTRAINT github_pending_handoffs_session_owner_user_fkey
    FOREIGN KEY (session_id, user_id, owner_id)
    REFERENCES admin_sessions(id, user_id, owner_id);

-- Audit admission is serialized per owner. Ordinary events may use capacity
-- only after reserving one future audit per active installation and repository.
CREATE TABLE github_audit_admission (
  owner_id TEXT PRIMARY KEY REFERENCES owners(id),
  event_count INTEGER NOT NULL CHECK (event_count BETWEEN 0 AND 100000)
);
INSERT INTO github_audit_admission(owner_id,event_count)
  SELECT owner_id,count(*)::integer FROM github_audit_events GROUP BY owner_id;

CREATE FUNCTION admit_github_audit_event() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE next_count INTEGER; protected_count INTEGER;
BEGIN
  INSERT INTO github_audit_admission(owner_id,event_count) VALUES (NEW.owner_id,0)
    ON CONFLICT (owner_id) DO NOTHING;
  -- The counter row is the per-owner serialization point. At READ COMMITTED,
  -- this query observes authority changes committed before this insert acquires
  -- that row lock; own transaction changes are visible as well.
  SELECT (SELECT count(*) FROM github_installations WHERE owner_id=NEW.owner_id AND active)
       + (SELECT count(*) FROM github_repositories WHERE owner_id=NEW.owner_id AND active)
    INTO protected_count;
  UPDATE github_audit_admission
    SET event_count=event_count+1
    WHERE owner_id=NEW.owner_id
      AND event_count < 100000
      AND (NEW.action IN ('installation_disabled','repository_disabled')
        OR event_count + 1 + protected_count <= 100000)
    RETURNING event_count INTO next_count;
  IF next_count IS NULL THEN
    RAISE EXCEPTION 'github audit capacity reached' USING ERRCODE='54000';
  END IF;
  RETURN NEW;
END; $$;
CREATE TRIGGER github_audit_events_admit BEFORE INSERT ON github_audit_events
  FOR EACH ROW EXECUTE FUNCTION admit_github_audit_event();
