-- 22-7 hardening: runtime DB role (F1), missing indexes (F7), retention purge (F13).

-- F1. Least-privilege runtime role. NOLOGIN here: the installer sets LOGIN and
-- a password. Roles are cluster-wide, so creation is idempotent. The migrating
-- role stays owner of every table; the runtime role gets DML only (no TRUNCATE,
-- no ownership, no CREATE, so it cannot DISABLE TRIGGER, ALTER TABLE, or set
-- session_replication_role).
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'autodeploy_app') THEN
    CREATE ROLE autodeploy_app NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
  END IF;
END $$;

DO $$
BEGIN
  EXECUTE format('GRANT CONNECT ON DATABASE %I TO autodeploy_app', current_database());
END $$;
GRANT USAGE ON SCHEMA public TO autodeploy_app;
REVOKE CREATE ON SCHEMA public FROM autodeploy_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO autodeploy_app;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO autodeploy_app;
-- The migration ledger is owned by the migrator alone.
REVOKE INSERT, UPDATE, DELETE ON schema_migrations FROM autodeploy_app;
-- Future tables/sequences created by the migrating role get the same grants.
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO autodeploy_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO autodeploy_app;

-- F7. release_operations is looked up by (attempt_id, fence) and by job_id
-- (FK parent of agent_job_attempts / agent_jobs); agent_jobs by rollback
-- target (cleanup NOT EXISTS) and by (owner_id, project_id) -> environment_id
-- (operations read model).
CREATE INDEX release_operations_attempt_fence_idx ON release_operations(attempt_id, fence);
CREATE INDEX release_operations_job_idx ON release_operations(job_id);
CREATE INDEX agent_jobs_rollback_target_idx ON agent_jobs(rollback_target_release_id) WHERE rollback_target_release_id IS NOT NULL;
CREATE INDEX agent_jobs_owner_project_environment_idx ON agent_jobs(owner_id, project_id, environment_id);

-- F13. Age-purge support indexes (webhook_deliveries is never purged).
CREATE INDEX agent_job_events_occurred_at_idx ON agent_job_events(occurred_at);
CREATE INDEX route_events_created_at_idx ON route_events(created_at);

-- F13. DELETE is allowed only in a transaction that ran
--   SELECT set_config('autodeploy.retention_purge','on',true)
-- (a distinct GUC from the log purge, so one cannot unlock the other). Accident
-- guard, not an access-control boundary. Structural guard remains in the
-- trigger: events of non-terminal jobs are never deletable (the job's next
-- event sequence is MAX(sequence)+1). webhook_deliveries and
-- webhook_lifecycle_repositories keep their 000010 delete-rejecting triggers:
-- deliveries are replay protection and are retained forever (owner decision).

CREATE FUNCTION reject_agent_job_event_delete() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF current_setting('autodeploy.retention_purge', true) IS DISTINCT FROM 'on' THEN
    RAISE EXCEPTION 'agent job events are append-only';
  END IF;
  IF EXISTS (SELECT 1 FROM agent_jobs j WHERE j.id = OLD.job_id AND j.state NOT IN ('succeeded','failed','cancelled','dead')) THEN
    RAISE EXCEPTION 'events of non-terminal jobs cannot be purged';
  END IF;
  RETURN OLD;
END;
$$;
DROP TRIGGER agent_job_events_no_delete ON agent_job_events;
CREATE TRIGGER agent_job_events_no_delete BEFORE DELETE ON agent_job_events FOR EACH ROW EXECUTE FUNCTION reject_agent_job_event_delete();

CREATE FUNCTION reject_route_event_delete() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF current_setting('autodeploy.retention_purge', true) IS DISTINCT FROM 'on' THEN
    RAISE EXCEPTION 'route events are append-only';
  END IF;
  RETURN OLD;
END;
$$;
DROP TRIGGER route_events_no_delete ON route_events;
CREATE TRIGGER route_events_no_delete BEFORE DELETE ON route_events FOR EACH ROW EXECUTE FUNCTION reject_route_event_delete();
