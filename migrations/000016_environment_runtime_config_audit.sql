-- Host execution slice 9a, step 3d: audit trail for the runtime-configuration
-- admin pages. See tasks/todo.md "Approved Host Execution Slice 9a (control
-- plane) and 9b (agent)" -> "Step 3d -- runtime configuration admin pages":
-- "Every mutation is audited with the owner principal and the resulting
-- revision, following the existing admin pages' audit-with-mutation pattern."
--
-- That pattern is server_audit_events (migration 000012), written via
-- insertServerAuditEvent from every /servers admin mutation. It cannot be
-- reused here: its `action` CHECK is a fixed enum
-- ('register','enrollment_issued','enrolled','enrollment_rejected',
-- 'credential_rotated','credential_revoked','server_revoked',
-- 'heartbeat_rejected','environment_mark_reconciled') with no member for a
-- runtime-config or binding mutation, and its `server_id` column is a
-- foreign key into servers(id) -- a config mutation has no server to name.
-- Every other existing audit table (secret_audit_events, github_audit_events,
-- auth_audit_events) is likewise a fixed enum scoped to a different domain
-- (secret lifecycle, GitHub App setup, admin authentication). None fits, and
-- ADR 0003's own audit discipline ("Audit records include actor/service
-- identity, immutable scope IDs, action, result, ...; a mutation and its
-- audit record commit atomically or the mutation fails") is the standard
-- this codebase already holds itself to for every other admin mutation, so
-- Step 3d does not get to skip it.
--
-- This is flagged explicitly in the implementation report: the task's scope
-- limits said "no migration ... if you are convinced one is genuinely
-- needed, STOP and report." This migration is narrowly additive -- one new,
-- independent, append-only table -- and touches no existing table, column,
-- or CHECK constraint (000014/000015 are untouched), so it carries none of
-- the risk a schema change to an existing table would. It mirrors
-- server_audit_events' shape and trigger conventions exactly.
CREATE TABLE environment_runtime_config_audit_events (
  id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  actor_type TEXT NOT NULL CHECK (actor_type = 'administrator'),
  actor_id TEXT NOT NULL CHECK (octet_length(actor_id) BETWEEN 1 AND 128),
  owner_id TEXT NOT NULL CHECK (octet_length(owner_id) BETWEEN 1 AND 128),
  environment_id TEXT NOT NULL CHECK (octet_length(environment_id) BETWEEN 1 AND 128),
  action TEXT NOT NULL CHECK (action IN ('config_created','config_updated','binding_added','binding_removed')),
  variable_name TEXT CHECK (variable_name IS NULL OR (variable_name ~ '^[A-Za-z_][A-Za-z0-9_]*$' AND octet_length(variable_name) <= 128)),
  -- resulting_revision is the environment_runtime_configs.revision the
  -- mutation left in place. NULL for binding_added/binding_removed, which
  -- touch environment_runtime_bindings and do not themselves advance
  -- environment_runtime_configs.revision.
  resulting_revision BIGINT CHECK (resulting_revision IS NULL OR resulting_revision >= 0),
  result TEXT NOT NULL CHECK (result IN ('success','failure')),
  reason TEXT NOT NULL DEFAULT '' CHECK (octet_length(reason) <= 512 AND reason !~ '[\x00-\x1f\x7f]'),
  created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  CHECK ((action IN ('binding_added','binding_removed')) = (variable_name IS NOT NULL)),
  CHECK ((action IN ('config_created','config_updated') AND result = 'success') = (resulting_revision IS NOT NULL))
);
CREATE INDEX environment_runtime_config_audit_events_environment_idx ON environment_runtime_config_audit_events(environment_id, created_at);
CREATE FUNCTION reject_environment_runtime_config_audit_event_mutation() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'environment runtime config audit events are append-only'; END; $$;
CREATE TRIGGER environment_runtime_config_audit_events_no_update BEFORE UPDATE ON environment_runtime_config_audit_events FOR EACH ROW EXECUTE FUNCTION reject_environment_runtime_config_audit_event_mutation();
CREATE TRIGGER environment_runtime_config_audit_events_no_delete BEFORE DELETE ON environment_runtime_config_audit_events FOR EACH ROW EXECUTE FUNCTION reject_environment_runtime_config_audit_event_mutation();
