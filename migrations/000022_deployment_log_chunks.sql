-- Step 15g-1: durable deployment log channel (agent -> control plane -> UI).
--
-- deployment_log_chunks is append-only. attempt_id deliberately has NO foreign
-- key to agent_job_attempts: an FK insert takes FOR KEY SHARE on the attempt
-- row, which conflicts with the plain FOR UPDATE that Complete, Activate,
-- Renew and the sweeper take on attempts (ADR 0004 lock hierarchy). Log
-- appends must never wait on, or hold up, those transactions. The FK would
-- only guard against deleting attempts, and agent_job_attempts already
-- rejects DELETE (agent_job_attempts_no_delete, 000013), so integrity is
-- enforced by that trigger plus the authorization read in AppendLogChunks.
CREATE TABLE deployment_log_chunks (
  id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  attempt_id BIGINT NOT NULL CHECK (attempt_id > 0),
  stream TEXT NOT NULL CHECK (stream IN ('build','phase','health','activation','drain','system')),
  -- writer_epoch identifies one agent process start so a restarted agent
  -- cannot collide with its earlier seq values. 0 is reserved for rows the
  -- control plane itself writes (the truncation marker); the store rejects
  -- agent input with epoch <= 0.
  writer_epoch BIGINT NOT NULL CHECK (writer_epoch >= 0),
  seq BIGINT NOT NULL CHECK (seq > 0),
  -- text guarantees valid UTF-8 (and no NUL); the byte bound is the 4 KiB cap.
  body TEXT NOT NULL CHECK (octet_length(body) BETWEEN 1 AND 4096),
  created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  -- Idempotent-retry key.
  UNIQUE (attempt_id, stream, writer_epoch, seq)
);
-- Reads: one attempt's chunks by id; the keyset UI cursor and the purge both
-- walk this index.
CREATE INDEX deployment_log_chunks_attempt_id_idx ON deployment_log_chunks(attempt_id, id);

-- Per-attempt counters used to enforce the caps race-safely. The only
-- mutable log table; UPDATE is allowed here, DELETE only by the purge.
CREATE TABLE deployment_log_attempt_totals (
  attempt_id BIGINT PRIMARY KEY CHECK (attempt_id > 0),
  bytes BIGINT NOT NULL DEFAULT 0 CHECK (bytes >= 0),
  chunks INTEGER NOT NULL DEFAULT 0 CHECK (chunks >= 0),
  truncated BOOLEAN NOT NULL DEFAULT false,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

CREATE FUNCTION deployment_log_chunks_reject_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'deployment log chunks are append-only';
END;
$$;

-- DELETE is allowed only inside a transaction that set the purge GUC with
-- set_config('autodeploy.log_purge','on',true) (transaction-local). This is
-- an accident guard for the retention purge, not an access-control boundary.
CREATE FUNCTION deployment_log_reject_delete() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF current_setting('autodeploy.log_purge', true) IS DISTINCT FROM 'on' THEN
    RAISE EXCEPTION 'deployment log rows may only be deleted by the retention purge';
  END IF;
  RETURN OLD;
END;
$$;

CREATE TRIGGER deployment_log_chunks_reject_update
BEFORE UPDATE ON deployment_log_chunks FOR EACH ROW EXECUTE FUNCTION deployment_log_chunks_reject_update();
CREATE TRIGGER deployment_log_chunks_reject_delete
BEFORE DELETE ON deployment_log_chunks FOR EACH ROW EXECUTE FUNCTION deployment_log_reject_delete();
CREATE TRIGGER deployment_log_attempt_totals_reject_delete
BEFORE DELETE ON deployment_log_attempt_totals FOR EACH ROW EXECUTE FUNCTION deployment_log_reject_delete();

-- Purge support: newest-N-per-environment lookup walks attempts by
-- (environment_id, id DESC).
CREATE INDEX agent_job_attempts_environment_id_idx ON agent_job_attempts(environment_id, id DESC);
