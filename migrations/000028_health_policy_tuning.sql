-- Step 22-1a (owner-approved 2026-11-16, tasks/22-plan.md "22-1 design"):
-- additive, tunable health policy. ADR 0005's HTTP gate stays mandatory for
-- every route; this migration only makes its numbers configurable per
-- environment and adds an optional TCP port for a defense-in-depth dial
-- check (agent behavior arrives in later slices). health_policy_revision
-- stays pinned to 1 on both tables: the defaults below ARE revision 1.
--
-- Columns (identical on environment_runtime_configs and the frozen
-- agent_job_attempt_execution), bounds enforced by explicitly named CHECKs
-- that internal/healthgate/policy.go mirrors in Go:
--   health_success_threshold  SMALLINT NOT NULL  1..10    (default 3)
--   health_interval_seconds   SMALLINT NOT NULL  1..30    (default 2)
--   health_timeout_seconds    SMALLINT NOT NULL  1..30    (default 2)
--   health_deadline_seconds   SMALLINT NOT NULL  10..600  (default 60)
--   health_tcp_port           INTEGER  NULL      1..65535 (NULL = no TCP check)
-- Cross CHECKs: timeout <= interval (a probe never outlives its round) and
-- deadline >= threshold * interval (the deadline must leave room for the
-- required consecutive successes).
--
-- Live table: plain ADD COLUMN ... DEFAULT, so every existing and future
-- INSERT that omits the columns gets ADR 0005's policy.
--
-- Frozen table: a frozen row must carry exactly what its attempt ran with,
-- so the end state has NO default (a missing value is an error, like every
-- other frozen column). Existing rows (real rows exist on the acer host) are
-- filled by ADD COLUMN ... DEFAULT, which PostgreSQL applies as a catalog-only
-- table-metadata fill without rewriting rows or firing row triggers, so
-- reject_agent_job_attempt_execution_mutation (000014, which rejects every
-- UPDATE) is never involved; DROP DEFAULT afterwards is metadata-only and
-- keeps the filled values. The 4 NOT NULL columns are added with their
-- CHECKs; the table-level cross CHECKs are added last, and every existing row
-- already satisfies them (3, 2, 2, 60: 2 <= 2 and 60 >= 6).
--
-- Digest compatibility: computeConfigDigest appends policy parts only when a
-- policy differs from these defaults, so every existing release/config_digest
-- is byte-identical after this migration.
ALTER TABLE environment_runtime_configs
  ADD COLUMN health_success_threshold SMALLINT NOT NULL DEFAULT 3
    CONSTRAINT environment_runtime_configs_health_success_threshold_check CHECK (health_success_threshold BETWEEN 1 AND 10),
  ADD COLUMN health_interval_seconds SMALLINT NOT NULL DEFAULT 2
    CONSTRAINT environment_runtime_configs_health_interval_seconds_check CHECK (health_interval_seconds BETWEEN 1 AND 30),
  ADD COLUMN health_timeout_seconds SMALLINT NOT NULL DEFAULT 2
    CONSTRAINT environment_runtime_configs_health_timeout_seconds_check CHECK (health_timeout_seconds BETWEEN 1 AND 30),
  ADD COLUMN health_deadline_seconds SMALLINT NOT NULL DEFAULT 60
    CONSTRAINT environment_runtime_configs_health_deadline_seconds_check CHECK (health_deadline_seconds BETWEEN 10 AND 600),
  ADD COLUMN health_tcp_port INTEGER
    CONSTRAINT environment_runtime_configs_health_tcp_port_check CHECK (health_tcp_port IS NULL OR health_tcp_port BETWEEN 1 AND 65535),
  ADD CONSTRAINT environment_runtime_configs_health_timeout_interval_check CHECK (health_timeout_seconds <= health_interval_seconds),
  ADD CONSTRAINT environment_runtime_configs_health_deadline_streak_check CHECK (health_deadline_seconds >= health_success_threshold * health_interval_seconds);

ALTER TABLE agent_job_attempt_execution
  ADD COLUMN health_success_threshold SMALLINT NOT NULL DEFAULT 3
    CONSTRAINT agent_job_attempt_execution_health_success_threshold_check CHECK (health_success_threshold BETWEEN 1 AND 10),
  ADD COLUMN health_interval_seconds SMALLINT NOT NULL DEFAULT 2
    CONSTRAINT agent_job_attempt_execution_health_interval_seconds_check CHECK (health_interval_seconds BETWEEN 1 AND 30),
  ADD COLUMN health_timeout_seconds SMALLINT NOT NULL DEFAULT 2
    CONSTRAINT agent_job_attempt_execution_health_timeout_seconds_check CHECK (health_timeout_seconds BETWEEN 1 AND 30),
  ADD COLUMN health_deadline_seconds SMALLINT NOT NULL DEFAULT 60
    CONSTRAINT agent_job_attempt_execution_health_deadline_seconds_check CHECK (health_deadline_seconds BETWEEN 10 AND 600),
  ADD COLUMN health_tcp_port INTEGER
    CONSTRAINT agent_job_attempt_execution_health_tcp_port_check CHECK (health_tcp_port IS NULL OR health_tcp_port BETWEEN 1 AND 65535),
  ADD CONSTRAINT agent_job_attempt_execution_health_timeout_interval_check CHECK (health_timeout_seconds <= health_interval_seconds),
  ADD CONSTRAINT agent_job_attempt_execution_health_deadline_streak_check CHECK (health_deadline_seconds >= health_success_threshold * health_interval_seconds);

ALTER TABLE agent_job_attempt_execution
  ALTER COLUMN health_success_threshold DROP DEFAULT,
  ALTER COLUMN health_interval_seconds DROP DEFAULT,
  ALTER COLUMN health_timeout_seconds DROP DEFAULT,
  ALTER COLUMN health_deadline_seconds DROP DEFAULT;
