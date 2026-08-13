-- KEK selection belongs to the external provider. This table records only
-- non-reusable invocations for a caller-selected KEK ID.
CREATE TABLE secret_wrap_invocations (
    kek_id TEXT PRIMARY KEY CHECK (kek_id <> '' AND length(kek_id) <= 64),
    reserved_invocations BIGINT NOT NULL DEFAULT 0 CHECK (reserved_invocations >= 0 AND reserved_invocations <= 2147483648),
    database_generation_id TEXT NOT NULL CHECK (database_generation_id <> '' AND length(database_generation_id) <= 128),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- This is a non-secret binding, not a monotonic restore proof. A restore must
-- use a fresh active KEK ID, initialized explicitly against this exact value.
CREATE TABLE secret_database_generation (
    singleton BOOLEAN PRIMARY KEY DEFAULT true CHECK (singleton),
    id TEXT NOT NULL CHECK (length(id) BETWEEN 16 AND 128),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO secret_database_generation(singleton,id)
VALUES (true, md5(random()::text || clock_timestamp()::text || txid_current()::text));

CREATE TABLE secret_heads (
    id TEXT PRIMARY KEY CHECK (id <> '' AND length(id) <= 128),
    owner_id TEXT NOT NULL CHECK (owner_id <> '' AND length(owner_id) <= 128),
    project_id TEXT NOT NULL CHECK (project_id <> '' AND length(project_id) <= 128),
    environment_id TEXT NOT NULL CHECK (environment_id <> '' AND length(environment_id) <= 128),
    purpose TEXT NOT NULL CHECK (purpose IN ('build', 'runtime', 'provider')),
    delivery_scope TEXT NOT NULL CHECK (delivery_scope IN ('build', 'runtime')),
    current_version_id TEXT,
    revision BIGINT NOT NULL DEFAULT 0 CHECK (revision >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX secret_heads_scope_idx ON secret_heads(owner_id, project_id, environment_id);

CREATE TABLE secret_versions (
    id TEXT PRIMARY KEY CHECK (id <> '' AND length(id) <= 128),
    secret_id TEXT NOT NULL REFERENCES secret_heads(id) ON DELETE RESTRICT,
    owner_id TEXT NOT NULL CHECK (owner_id <> '' AND length(owner_id) <= 128),
    project_id TEXT NOT NULL CHECK (project_id <> '' AND length(project_id) <= 128),
    environment_id TEXT NOT NULL CHECK (environment_id <> '' AND length(environment_id) <= 128),
    version_number BIGINT NOT NULL CHECK (version_number > 0),
    state TEXT NOT NULL CHECK (state IN ('active', 'superseded', 'revoked')),
    suite TEXT NOT NULL CHECK (suite = 'AES-256-GCM-RANDOMNONCE-v1'),
    aad_version TEXT NOT NULL CHECK (aad_version = 'v1'),
    kek_id TEXT NOT NULL CHECK (kek_id <> '' AND length(kek_id) <= 64),
    ciphertext BYTEA NOT NULL CHECK (octet_length(ciphertext) BETWEEN 28 AND 65564),
    wrapped_dek BYTEA NOT NULL CHECK (octet_length(wrapped_dek) = 60),
    envelope_revision BIGINT NOT NULL DEFAULT 1 CHECK (envelope_revision > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(secret_id, version_number)
);
CREATE UNIQUE INDEX secret_versions_one_active_idx ON secret_versions(secret_id) WHERE state = 'active';
ALTER TABLE secret_heads ADD CONSTRAINT secret_heads_current_version_fk FOREIGN KEY (current_version_id) REFERENCES secret_versions(id) ON DELETE RESTRICT;
CREATE INDEX secret_versions_scope_idx ON secret_versions(owner_id, project_id, environment_id, secret_id, version_number DESC);

CREATE TABLE secret_audit_events (
    id TEXT PRIMARY KEY CHECK (id <> '' AND length(id) <= 128),
    owner_id TEXT NOT NULL CHECK (owner_id <> '' AND length(owner_id) <= 128),
    secret_id TEXT NOT NULL REFERENCES secret_heads(id) ON DELETE RESTRICT,
    secret_version_id TEXT REFERENCES secret_versions(id) ON DELETE RESTRICT,
    action TEXT NOT NULL CHECK (action IN ('created', 'replaced', 'replaced_compromised', 'materialized', 'revoked')),
    actor_id TEXT NOT NULL CHECK (length(actor_id) <= 128),
    service_id TEXT NOT NULL CHECK (length(service_id) <= 128),
    result TEXT NOT NULL CHECK (result IN ('success', 'denied', 'failed')),
    old_version_id TEXT CHECK (old_version_id IS NULL OR (old_version_id <> '' AND length(old_version_id) <= 128)),
    kek_id TEXT NOT NULL CHECK (kek_id <> '' AND length(kek_id) <= 64),
    project_id TEXT NOT NULL CHECK (project_id <> '' AND length(project_id) <= 128),
    environment_id TEXT NOT NULL CHECK (environment_id <> '' AND length(environment_id) <= 128),
    deployment_id TEXT NOT NULL DEFAULT '' CHECK (length(deployment_id) <= 128),
    lease_id TEXT NOT NULL DEFAULT '' CHECK (length(lease_id) <= 128),
    CHECK ((actor_id <> '') <> (service_id <> '')),
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX secret_audit_events_secret_idx ON secret_audit_events(secret_id, occurred_at);

CREATE OR REPLACE FUNCTION validate_secret_version_scope() RETURNS trigger AS $$
DECLARE h secret_heads%ROWTYPE;
BEGIN SELECT * INTO h FROM secret_heads WHERE id=NEW.secret_id;
 IF NOT FOUND OR NEW.owner_id<>h.owner_id OR NEW.project_id<>h.project_id OR NEW.environment_id<>h.environment_id THEN RAISE EXCEPTION 'secret version scope mismatch'; END IF;
 RETURN NEW; END; $$ LANGUAGE plpgsql;
CREATE TRIGGER secret_versions_scope_before_insert BEFORE INSERT ON secret_versions FOR EACH ROW EXECUTE FUNCTION validate_secret_version_scope();

CREATE OR REPLACE FUNCTION validate_secret_head_current() RETURNS trigger AS $$
DECLARE v secret_versions%ROWTYPE;
BEGIN IF NEW.current_version_id IS NULL THEN RETURN NEW; END IF;
 SELECT * INTO v FROM secret_versions WHERE id=NEW.current_version_id;
 IF NOT FOUND OR v.secret_id<>NEW.id OR v.state<>'active' THEN RAISE EXCEPTION 'invalid current secret version'; END IF;
 RETURN NEW; END; $$ LANGUAGE plpgsql;
CREATE TRIGGER secret_heads_current_before_insert_update BEFORE INSERT OR UPDATE OF current_version_id ON secret_heads FOR EACH ROW EXECUTE FUNCTION validate_secret_head_current();
CREATE OR REPLACE FUNCTION secret_heads_immutable() RETURNS trigger AS $$
BEGIN
 IF NEW.owner_id<>OLD.owner_id OR NEW.project_id<>OLD.project_id OR NEW.environment_id<>OLD.environment_id OR NEW.purpose<>OLD.purpose OR NEW.delivery_scope<>OLD.delivery_scope THEN RAISE EXCEPTION 'secret head scope immutable'; END IF;
 IF NEW.current_version_id IS DISTINCT FROM OLD.current_version_id AND NEW.revision<>OLD.revision+1 THEN RAISE EXCEPTION 'secret head revision conflict'; END IF;
 IF NEW.current_version_id IS NOT DISTINCT FROM OLD.current_version_id AND NEW.revision<>OLD.revision THEN RAISE EXCEPTION 'secret head revision immutable'; END IF;
 RETURN NEW; END; $$ LANGUAGE plpgsql;
CREATE TRIGGER secret_heads_immutable_before_update BEFORE UPDATE ON secret_heads FOR EACH ROW EXECUTE FUNCTION secret_heads_immutable();

CREATE OR REPLACE FUNCTION validate_current_secret_version_state() RETURNS trigger AS $$
BEGIN
 IF NEW.state <> 'active' AND EXISTS (SELECT 1 FROM secret_heads WHERE current_version_id=NEW.id) THEN RAISE EXCEPTION 'current secret version must be active'; END IF;
 RETURN NEW; END; $$ LANGUAGE plpgsql;
CREATE CONSTRAINT TRIGGER secret_versions_current_state_after_update
AFTER UPDATE OF state ON secret_versions DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION validate_current_secret_version_state();

CREATE OR REPLACE FUNCTION validate_secret_audit_scope() RETURNS trigger AS $$
DECLARE h secret_heads%ROWTYPE; v secret_versions%ROWTYPE;
BEGIN SELECT * INTO h FROM secret_heads WHERE id=NEW.secret_id;
 IF NOT FOUND OR NEW.owner_id<>h.owner_id OR (NEW.project_id<>'' AND NEW.project_id<>h.project_id) OR (NEW.environment_id<>'' AND NEW.environment_id<>h.environment_id) THEN RAISE EXCEPTION 'secret audit scope mismatch'; END IF;
 IF NEW.secret_version_id IS NOT NULL THEN SELECT * INTO v FROM secret_versions WHERE id=NEW.secret_version_id; IF NOT FOUND OR v.secret_id<>NEW.secret_id OR v.owner_id<>NEW.owner_id THEN RAISE EXCEPTION 'secret audit version mismatch'; END IF; END IF;
 IF NEW.old_version_id IS NOT NULL THEN SELECT * INTO v FROM secret_versions WHERE id=NEW.old_version_id; IF NOT FOUND OR v.secret_id<>NEW.secret_id OR v.owner_id<>NEW.owner_id THEN RAISE EXCEPTION 'secret audit old version mismatch'; END IF; END IF;
 IF NEW.action='created' AND NEW.old_version_id IS NOT NULL THEN RAISE EXCEPTION 'created audit cannot have old version'; END IF;
 IF NEW.action='replaced' AND (NEW.old_version_id IS NULL OR (SELECT state FROM secret_versions WHERE id=NEW.old_version_id) <> 'superseded') THEN RAISE EXCEPTION 'invalid replacement audit linkage'; END IF;
 IF NEW.action='replaced_compromised' AND (NEW.old_version_id IS NULL OR (SELECT state FROM secret_versions WHERE id=NEW.old_version_id) <> 'revoked') THEN RAISE EXCEPTION 'invalid compromise replacement audit linkage'; END IF;
 RETURN NEW; END; $$ LANGUAGE plpgsql;
CREATE TRIGGER secret_audit_scope_before_insert BEFORE INSERT ON secret_audit_events FOR EACH ROW EXECUTE FUNCTION validate_secret_audit_scope();

CREATE OR REPLACE FUNCTION secret_versions_immutable() RETURNS trigger AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id OR NEW.secret_id IS DISTINCT FROM OLD.secret_id
       OR NEW.owner_id IS DISTINCT FROM OLD.owner_id OR NEW.project_id IS DISTINCT FROM OLD.project_id
       OR NEW.environment_id IS DISTINCT FROM OLD.environment_id OR NEW.version_number IS DISTINCT FROM OLD.version_number
       OR NEW.suite IS DISTINCT FROM OLD.suite OR NEW.aad_version IS DISTINCT FROM OLD.aad_version
       OR NEW.ciphertext IS DISTINCT FROM OLD.ciphertext OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'secret versions are immutable';
    END IF;
 IF NEW.kek_id IS DISTINCT FROM OLD.kek_id OR NEW.wrapped_dek IS DISTINCT FROM OLD.wrapped_dek OR NEW.envelope_revision IS DISTINCT FROM OLD.envelope_revision THEN
      IF OLD.state='revoked' OR NEW.state IS DISTINCT FROM OLD.state OR NEW.envelope_revision <> OLD.envelope_revision + 1 THEN RAISE EXCEPTION 'invalid secret rewrap'; END IF;
    ELSIF NOT ((OLD.state = 'active' AND NEW.state IN ('superseded', 'revoked')) OR (OLD.state = 'superseded' AND NEW.state = 'revoked')) THEN RAISE EXCEPTION 'secret versions are immutable'; END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER secret_versions_reject_update BEFORE UPDATE ON secret_versions FOR EACH ROW EXECUTE FUNCTION secret_versions_immutable();
CREATE OR REPLACE FUNCTION secret_versions_reject_delete() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'secret versions are immutable'; END; $$ LANGUAGE plpgsql;
CREATE TRIGGER secret_versions_reject_delete BEFORE DELETE ON secret_versions FOR EACH ROW EXECUTE FUNCTION secret_versions_reject_delete();

CREATE OR REPLACE FUNCTION secret_audit_events_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'secret audit events are append-only';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER secret_audit_events_reject_update BEFORE UPDATE ON secret_audit_events FOR EACH ROW EXECUTE FUNCTION secret_audit_events_append_only();
CREATE TRIGGER secret_audit_events_reject_delete BEFORE DELETE ON secret_audit_events FOR EACH ROW EXECUTE FUNCTION secret_audit_events_append_only();
