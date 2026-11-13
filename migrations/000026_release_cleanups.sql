-- Step 20E: retention cleanup of retained releases.
--
-- (1) State machine: 000018 deliberately left 'retained' -> 'removing' out
-- ("deferred alongside the cleanup logic"). This migration adds exactly that
-- edge, and nothing else: it is 000018's validate_release_update with one more
-- alternative in the transition list. 'removing' -> 'removed' already exists;
-- 'active' -> 'removing' stays illegal; 'failed' and 'removed' stay terminal.
-- The edge is reachable only through the cleanup claim transaction, which
-- re-checks eligibility under the environment slot lock (ADR 0004 amendment
-- 2026-11-13, "Retention cleanup").
CREATE OR REPLACE FUNCTION validate_release_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
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
  IF NEW.state <> OLD.state AND NOT (
    (OLD.state = 'candidate' AND NEW.state IN ('ready','failed','removing')) OR
    (OLD.state = 'ready' AND NEW.state IN ('failed','removing','active')) OR
    (OLD.state = 'removing' AND NEW.state = 'removed') OR
    (OLD.state = 'active' AND NEW.state = 'retained') OR
    (OLD.state = 'retained' AND NEW.state IN ('active','removing'))
  ) THEN
    RAISE EXCEPTION 'invalid release transition % -> %', OLD.state, NEW.state;
  END IF;
  IF NEW.updated_at < OLD.updated_at THEN
    RAISE EXCEPTION 'release updated_at must be monotonic';
  END IF;
  RETURN NEW;
END; $$;

-- (2) One row per cleanup claim. A claim moves a retained release to
-- 'removing' and inserts the row in the same transaction; the agent's later
-- report is the one-time write of result/error_class/image_kept/outcome_summary/
-- completed_at. A failed or skipped result leaves the release in 'removing'
-- (operator visible); a later claim may open a new row for it after a backoff.
-- Rows are never deleted and never updated except that single write.
CREATE TABLE release_cleanups (
  id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  release_id TEXT NOT NULL REFERENCES releases(id) ON DELETE RESTRICT,
  server_id TEXT NOT NULL REFERENCES servers(id) ON DELETE RESTRICT,
  environment_id TEXT NOT NULL REFERENCES environment_slots(environment_id) ON DELETE RESTRICT,
  claimed_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  result TEXT CHECK (result IN ('removed','failed','skipped')),
  error_class TEXT CHECK (error_class IN ('engine','identity','stop','other')),
  image_kept BOOLEAN NOT NULL DEFAULT false,
  outcome_summary TEXT NOT NULL DEFAULT '' CHECK (octet_length(outcome_summary) <= 2048 AND outcome_summary !~ '[\x00-\x1f\x7f]'),
  completed_at TIMESTAMPTZ,
  CHECK ((result IS NULL) = (completed_at IS NULL)),
  CHECK (completed_at IS NULL OR (isfinite(completed_at) AND completed_at >= claimed_at)),
  CHECK ((result IN ('failed','skipped')) = (error_class IS NOT NULL)),
  CHECK (result IS NOT NULL OR (NOT image_kept AND outcome_summary = '')),
  CHECK (result = 'removed' OR NOT image_kept)
);
-- At most one open (unreported) cleanup per release, and a release is removed once.
CREATE UNIQUE INDEX release_cleanups_open_idx ON release_cleanups(release_id) WHERE completed_at IS NULL;
CREATE UNIQUE INDEX release_cleanups_removed_idx ON release_cleanups(release_id) WHERE result = 'removed';
CREATE INDEX release_cleanups_release_idx ON release_cleanups(release_id, id DESC);
CREATE INDEX release_cleanups_server_open_idx ON release_cleanups(server_id) WHERE completed_at IS NULL;

-- A cleanup can only be opened for a release that is already 'removing' in the
-- same environment (the claim transaction moves it first).
CREATE FUNCTION validate_release_cleanup_insert() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  rel_state TEXT;
  rel_env TEXT;
BEGIN
  IF NEW.result IS NOT NULL OR NEW.completed_at IS NOT NULL THEN
    RAISE EXCEPTION 'release cleanup must be inserted open';
  END IF;
  SELECT state, environment_id INTO rel_state, rel_env FROM releases WHERE id = NEW.release_id;
  IF NOT FOUND OR rel_state <> 'removing' OR rel_env <> NEW.environment_id THEN
    RAISE EXCEPTION 'release cleanup requires a removing release in the same environment';
  END IF;
  RETURN NEW;
END; $$;
CREATE TRIGGER release_cleanups_validate_insert BEFORE INSERT ON release_cleanups FOR EACH ROW EXECUTE FUNCTION validate_release_cleanup_insert();

CREATE FUNCTION validate_release_cleanup_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
  IF NEW.id <> OLD.id OR NEW.release_id <> OLD.release_id OR NEW.server_id <> OLD.server_id
     OR NEW.environment_id <> OLD.environment_id OR NEW.claimed_at <> OLD.claimed_at THEN
    RAISE EXCEPTION 'release cleanup identity is immutable';
  END IF;
  IF OLD.result IS NOT NULL OR OLD.completed_at IS NOT NULL THEN
    RAISE EXCEPTION 'release cleanup result is write-once';
  END IF;
  IF NEW.result IS NULL OR NEW.completed_at IS NULL THEN
    RAISE EXCEPTION 'release cleanup update must record its result';
  END IF;
  RETURN NEW;
END; $$;
CREATE TRIGGER release_cleanups_validate_update BEFORE UPDATE ON release_cleanups FOR EACH ROW EXECUTE FUNCTION validate_release_cleanup_update();

CREATE FUNCTION reject_release_cleanup_delete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'release cleanups are never deleted'; END; $$;
CREATE TRIGGER release_cleanups_no_delete BEFORE DELETE ON release_cleanups FOR EACH ROW EXECUTE FUNCTION reject_release_cleanup_delete();
