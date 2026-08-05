CREATE TABLE deployments (
    id TEXT PRIMARY KEY CHECK (id <> ''),
    project_id TEXT NOT NULL CHECK (project_id <> ''),
    environment_id TEXT NOT NULL CHECK (environment_id <> ''),
    server_id TEXT NOT NULL CHECK (server_id <> ''),
    commit_sha TEXT NOT NULL CHECK (commit_sha <> ''),
    schema_version SMALLINT NOT NULL CHECK (schema_version > 0),
    status TEXT NOT NULL CHECK (status IN (
        'queued', 'assigned', 'fetching', 'building', 'starting', 'health_checking',
        'activating', 'healthy', 'failed', 'cancelled', 'superseded', 'rolled_back'
    )),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL CHECK (updated_at >= created_at),
    revision BIGINT NOT NULL CHECK (revision > 0)
);

CREATE INDEX deployments_environment_created_at_idx ON deployments (environment_id, created_at DESC);
CREATE INDEX deployments_server_status_idx ON deployments (server_id, status);

CREATE TABLE deployment_events (
    deployment_id TEXT NOT NULL REFERENCES deployments(id) ON DELETE RESTRICT,
    revision BIGINT NOT NULL CHECK (revision > 0),
    from_status TEXT NOT NULL CHECK (from_status = '' OR from_status IN (
        'queued', 'assigned', 'fetching', 'building', 'starting', 'health_checking',
        'activating', 'healthy', 'failed', 'cancelled', 'superseded', 'rolled_back'
    )),
    to_status TEXT NOT NULL CHECK (to_status IN (
        'queued', 'assigned', 'fetching', 'building', 'starting', 'health_checking',
        'activating', 'healthy', 'failed', 'cancelled', 'superseded', 'rolled_back'
    )),
    occurred_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (deployment_id, revision)
);

CREATE OR REPLACE FUNCTION validate_deployment_event() RETURNS trigger AS $$
DECLARE
    previous deployment_events%ROWTYPE;
    head deployments%ROWTYPE;
BEGIN
    SELECT * INTO head FROM deployments WHERE id = NEW.deployment_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'deployment % does not exist', NEW.deployment_id;
    END IF;
    IF NEW.revision = 1 THEN
        IF NEW.from_status <> '' OR NEW.to_status <> 'queued' OR NEW.occurred_at <> head.created_at THEN
            RAISE EXCEPTION 'invalid deployment creation event';
        END IF;
    ELSE
        SELECT * INTO previous FROM deployment_events
          WHERE deployment_id = NEW.deployment_id AND revision = NEW.revision - 1;
        IF NOT FOUND OR NEW.occurred_at < previous.occurred_at THEN
            RAISE EXCEPTION 'deployment event must follow its predecessor';
        END IF;
        IF NEW.from_status <> previous.to_status OR NOT (
            (NEW.from_status = 'queued' AND NEW.to_status IN ('assigned', 'cancelled', 'superseded')) OR
            (NEW.from_status = 'assigned' AND NEW.to_status IN ('fetching', 'cancelled', 'failed')) OR
            (NEW.from_status = 'fetching' AND NEW.to_status IN ('building', 'cancelled', 'failed')) OR
            (NEW.from_status = 'building' AND NEW.to_status IN ('starting', 'cancelled', 'failed')) OR
            (NEW.from_status = 'starting' AND NEW.to_status IN ('health_checking', 'cancelled', 'failed')) OR
            (NEW.from_status = 'health_checking' AND NEW.to_status IN ('activating', 'cancelled', 'failed')) OR
            (NEW.from_status = 'activating' AND NEW.to_status IN ('healthy', 'failed')) OR
            (NEW.from_status = 'healthy' AND NEW.to_status = 'rolled_back')
        ) THEN
            RAISE EXCEPTION 'invalid deployment transition % -> %', NEW.from_status, NEW.to_status;
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER deployment_events_validate_before_insert
BEFORE INSERT ON deployment_events
FOR EACH ROW EXECUTE FUNCTION validate_deployment_event();

CREATE OR REPLACE FUNCTION deployment_events_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'deployment events are append-only';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER deployment_events_reject_update
BEFORE UPDATE ON deployment_events FOR EACH ROW EXECUTE FUNCTION deployment_events_append_only();
CREATE TRIGGER deployment_events_reject_delete
BEFORE DELETE ON deployment_events FOR EACH ROW EXECUTE FUNCTION deployment_events_append_only();
