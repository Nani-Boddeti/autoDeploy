-- Host execution slice 9a, step 2b: close the literal-binding silent no-op.
-- See tasks/todo.md "Approved Host Execution Slice 9a (control plane) and 9b
-- (agent)" -> "Step 2b -- literal binding values" for the bug this closes:
-- 000014 gave environment_runtime_bindings.binding_kind='literal_nonsecret'
-- no column to hold a value, and Step 2's freeze
-- (resolveRuntimeSecretBindings) only ever resolves binding_kind='secret'.
-- An administrator setting NODE_ENV=production therefore claims successfully
-- and the variable never exists -- exactly the silent-default failure this
-- design otherwise refuses everywhere else. This migration adds the missing
-- column, an append-only per-attempt frozen-literal table mirroring
-- frozen_secret_versions, and widens agent_lease_execution.go/tryClaim (Go
-- changes land alongside this migration, not here: schema first).
--
-- docs/adr/0003-secret-handling.md "Secret classification" places an
-- ordinary literal environment value (e.g. NODE_ENV=production) outside
-- every secret category it defines -- it is not a database-encrypted
-- recoverable value, not a one-way verifier, not transient material, and
-- not provider-held. It is exactly "non-secret protected metadata": a plain
-- configuration value with no confidentiality requirement. That is why it is
-- safe to store as bare TEXT here (unlike secret_versions' ciphertext
-- columns) and, in the Go layer, to carry across the wire on ClaimedLease --
-- never a secret value, by the same ADR.

-- literal_value bound: 4096 bytes. Chosen deliberately, not copied from
-- another column: this is a single free-form environment-variable value (a
-- JSON blob, a long allowlist, a connection string), not an identifier, so
-- the tight identifier-style bounds used elsewhere in this schema
-- (128/255 bytes) are the wrong precedent. 4096 bytes comfortably covers
-- every realistic env-var value while still keeping one binding row, one
-- frozen-literal row, and the config_digest encoding (which includes every
-- literal value) small and bounded -- an unbounded value would let a single
-- misconfigured binding blow up attempt-execution storage and digest cost
-- without limit. Control characters are rejected for the same reason
-- container_name/outcome_summary reject them elsewhere in this migration
-- family: a literal value can end up in shell/exec argument construction
-- (the agent's container env injection) and in operator-facing display, and
-- a stray NUL/ESC/newline there is an injection or log-corruption vector, not
-- a legitimate value. Newline-containing values (e.g. multi-line PEM-ish
-- blobs) are explicitly out of scope for a literal_nonsecret binding; that
-- shape belongs in a secret_versions "provider"/build-secret record instead.
ALTER TABLE environment_runtime_bindings ADD COLUMN literal_value TEXT
  CHECK (literal_value IS NULL OR (octet_length(literal_value) BETWEEN 1 AND 4096 AND literal_value !~ '[\x00-\x1f\x7f]'));
ALTER TABLE environment_runtime_bindings ADD CONSTRAINT environment_runtime_bindings_literal_presence_check
  CHECK ((binding_kind = 'literal_nonsecret') = (literal_value IS NOT NULL));

-- Frozen (append-only) copy of one attempt's literal binding values, mirroring
-- frozen_secret_versions' shape/trigger conventions exactly:
-- (attempt_id, variable_name) primary key (one literal value per variable per
-- attempt -- unlike frozen_secret_versions, a literal binding is always
-- exactly one row per variable, never fan-out across secret versions), FK to
-- agent_job_attempts ON DELETE RESTRICT, and dedicated no-update/no-delete
-- trigger functions specific to this table (not the generic
-- reject_frozen_secret_version_mutation, so a future change to one table's
-- append-only rule can never accidentally also change the other's).
CREATE TABLE frozen_literal_values (
  attempt_id BIGINT NOT NULL REFERENCES agent_job_attempts(id) ON DELETE RESTRICT,
  variable_name TEXT NOT NULL CHECK (variable_name ~ '^[A-Za-z_][A-Za-z0-9_]*$' AND octet_length(variable_name) <= 128),
  delivery_scope TEXT NOT NULL CHECK (delivery_scope IN ('build','runtime')),
  literal_value TEXT NOT NULL CHECK (octet_length(literal_value) BETWEEN 1 AND 4096 AND literal_value !~ '[\x00-\x1f\x7f]'),
  created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (attempt_id, variable_name)
);
CREATE FUNCTION reject_frozen_literal_value_mutation() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'frozen literal values are append-only'; END; $$;
CREATE TRIGGER frozen_literal_values_no_update BEFORE UPDATE ON frozen_literal_values FOR EACH ROW EXECUTE FUNCTION reject_frozen_literal_value_mutation();
CREATE TRIGGER frozen_literal_values_no_delete BEFORE DELETE ON frozen_literal_values FOR EACH ROW EXECUTE FUNCTION reject_frozen_literal_value_mutation();
