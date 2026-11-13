-- Step 20B: immutable server-side record of what the agent's predecessor drain
-- (stop after the switch) ended in. Visibility only (owner decision B-D1): a
-- stop failure never undoes or blocks anything and nothing reads this table to
-- decide an action. One row per activating attempt (the successor's); rows are
-- never updated or deleted. The agent reports with credential authentication
-- only (no lease: the attempt is closed by then); the store validates the
-- reported predecessor against the attempt's own succeeded activate operation
-- before inserting, so these columns are server-derived facts the agent merely
-- confirmed. outcome_summary has the same bounds as release_operations'.
CREATE TABLE release_drain_results (
  id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  attempt_id BIGINT NOT NULL UNIQUE REFERENCES agent_job_attempts(id) ON DELETE RESTRICT,
  environment_id TEXT NOT NULL REFERENCES environment_slots(environment_id) ON DELETE RESTRICT,
  release_id TEXT NOT NULL REFERENCES releases(id) ON DELETE RESTRICT,
  predecessor_release_id TEXT NOT NULL REFERENCES releases(id) ON DELETE RESTRICT,
  predecessor_runtime_instance_id TEXT NOT NULL CHECK (predecessor_runtime_instance_id ~ '^[1-9][0-9]{0,18}$'),
  outcome TEXT NOT NULL CHECK (outcome IN ('stopped','already_stopped','gone','stop_failed','route_moved','identity_error')),
  outcome_summary TEXT NOT NULL DEFAULT '' CHECK (octet_length(outcome_summary) <= 2048 AND outcome_summary !~ '[\x00-\x1f\x7f]'),
  reported_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  CHECK (release_id <> predecessor_release_id)
);
CREATE INDEX release_drain_results_environment_idx ON release_drain_results(environment_id, reported_at DESC);

CREATE FUNCTION reject_release_drain_result_mutation() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'release drain results are immutable'; END; $$;
CREATE TRIGGER release_drain_results_no_update BEFORE UPDATE ON release_drain_results FOR EACH ROW EXECUTE FUNCTION reject_release_drain_result_mutation();
CREATE TRIGGER release_drain_results_no_delete BEFORE DELETE ON release_drain_results FOR EACH ROW EXECUTE FUNCTION reject_release_drain_result_mutation();
