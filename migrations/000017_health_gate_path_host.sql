-- Slice 10 ("health-gate configuration + blue/green activation"), Step 0.
-- See tasks/todo.md "Approved Slice 10" -> "Step 0 -- configured health-gate
-- path + Host": 9b (migration 000014) stops at health_checking with the
-- health gate probing a hard-coded path "/" and a synthesized Host header
-- (internal/agent/executor/executor.go's healthGatePathV1Default/
-- healthGateHost) because no schema column carried a configured value.
-- ADR 0005's health-gate contract requires "exact configured container
-- port, scheme, path, and Host header"; this migration adds the missing
-- path and Host columns so that contract can finally be satisfied end to
-- end, and Step 0's own code changes delete the hard-coded stand-ins.
--
-- Decision D2 (owner, approved 2026-09-22): health_gate_host is REQUIRED
-- (frozen NOT NULL, no default), not nullable-falls-back-to-ip:port. A
-- failed gate with the container still running classifies
-- permanent_configuration (never retried -- see docker.ClassifyHealthGateResult),
-- so an unconfigured Host would otherwise turn into a silent, permanent
-- deployment failure the first time an application actually branches on
-- Host; requiring it up front turns that into an admin-time form error
-- instead. health_gate_path keeps its long-standing default of "/" (ADR
-- 0005's own default), since "/" is a reasonable default health-check path
-- for many applications, unlike a hostname, for which there is no
-- sensible default at all.
--
-- Both ALTER TABLE ... ADD COLUMN ... NOT NULL statements below are safe
-- against an existing row exactly as 000016's comment on
-- environment_runtime_config_audit_events reasons for its own additive
-- change: environment_runtime_configs and agent_job_attempt_execution have
-- never had a production row (tasks/todo.md: "Safe additive: neither table
-- has ever had a production row (executorEnabled=false)"), so
-- health_gate_host's NOT NULL with no default cannot fail against any row
-- that already exists -- there are none. Existing raw-SQL test fixtures
-- that insert directly into these two tables are updated in the same
-- change that adds this migration, not left to violate the new constraint.
--
-- CHECK design (internal/healthgate/validate.go documents and mirrors the
-- same contract in Go, reused by the repository, the web admin page, and
-- the agent's own defense-in-depth re-validation):
--
--   health_gate_path: starts with exactly one '/' (never "//..."), at most
--   2048 bytes, and otherwise an ASCII-only allowlist of RFC 3986 path
--   characters (pchar, plus '/' for multiple segments) and %XX escapes, an
--   optional '?'-introduced query with the same rule (plus a literal '?'
--   also allowed inside the query), and nothing else. This replaced an
--   earlier denylist version (exclude whitespace/control/'#') after
--   independent review found the denylist alone let an already-percent-
--   encoded or non-ASCII path reach internal/runtime/docker/health.go's
--   probe, which net/url's url.URL.Path field (documented as the
--   *decoded* value) would then silently re-encode: "/health%2Fcheck" was
--   sent on the wire as "/health%252Fcheck", "/a%zz" as "/a%25zz", and a
--   raw non-ASCII byte like "/hé" as "/h%C3%A9" -- a different resource
--   than configured, violating ADR 0005's "exact configured ... path".
--   Restricting this CHECK to exactly the bytes RFC 3986 allows (literal
--   unreserved/sub-delims/":"/"@"/"/" characters, or a valid %XX triplet)
--   makes every accepted path unambiguous to decode, so
--   internal/runtime/docker/health.go's buildProbeURL can round-trip it
--   through net/url without ever needing to guess at a canonical
--   percent-encoding. A malformed %-escape (not exactly two hex digits,
--   including a truncated one at the end of the string) matches neither
--   alternative in the repeating group and is rejected, the same as a bare
--   '%', a space, a quote, or any other character outside the allowlist.
--
--   health_gate_host: a DNS hostname (strict [A-Za-z0-9.-] label charset,
--   each label 1-63 bytes, never starting or ending a label with a
--   hyphen) with an optional ":port" suffix (port 1-65535), and the
--   hostname portion (excluding any ":port" suffix) at most 253 bytes
--   (RFC 1035). This charset structurally excludes '@' (userinfo), '/' (a
--   path), and '[' ']' (an IPv6 literal, which this V1 does not support as
--   a Host header target) -- there is no character class alternative that
--   could ever match any of them.
ALTER TABLE environment_runtime_configs ADD COLUMN health_gate_path TEXT NOT NULL DEFAULT '/' CHECK (
  octet_length(health_gate_path) BETWEEN 1 AND 2048 AND
  health_gate_path ~ '^/(?:[A-Za-z0-9._~!$&''()*+,;=:@/-]|%[0-9A-Fa-f]{2})*(?:\?(?:[A-Za-z0-9._~!$&''()*+,;=:@/?-]|%[0-9A-Fa-f]{2})*)?$' AND
  health_gate_path !~ '^//'
);
ALTER TABLE environment_runtime_configs ADD COLUMN health_gate_host TEXT NOT NULL CHECK (
  health_gate_host ~ '^(?:[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)(?:\.(?:[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?))*(?::(?:6553[0-5]|655[0-2][0-9]|65[0-4][0-9]{2}|6[0-4][0-9]{3}|[1-5][0-9]{4}|[1-9][0-9]{0,3}))?$'
  AND octet_length(split_part(health_gate_host, ':', 1)) <= 253
);

-- agent_job_attempt_execution (000014) is the frozen, insert-once copy of
-- the execution configuration an attempt actually ran with. Both new
-- columns widen it the same way every other execution field already there
-- does: NOT NULL, no default at all (unlike health_gate_path's admin-side
-- default above, a frozen row must always carry the exact values that
-- attempt ran with; there is no such thing as "unspecified" once frozen).
-- reject_agent_job_attempt_execution_mutation (000014) unconditionally
-- raises on UPDATE/DELETE without inspecting any column, so it already
-- covers these two new columns correctly with no change.
ALTER TABLE agent_job_attempt_execution ADD COLUMN health_gate_path TEXT NOT NULL CHECK (
  octet_length(health_gate_path) BETWEEN 1 AND 2048 AND
  health_gate_path ~ '^/(?:[A-Za-z0-9._~!$&''()*+,;=:@/-]|%[0-9A-Fa-f]{2})*(?:\?(?:[A-Za-z0-9._~!$&''()*+,;=:@/?-]|%[0-9A-Fa-f]{2})*)?$' AND
  health_gate_path !~ '^//'
);
ALTER TABLE agent_job_attempt_execution ADD COLUMN health_gate_host TEXT NOT NULL CHECK (
  health_gate_host ~ '^(?:[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)(?:\.(?:[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?))*(?::(?:6553[0-5]|655[0-2][0-9]|65[0-4][0-9]{2}|6[0-4][0-9]{3}|[1-5][0-9]{4}|[1-9][0-9]{0,3}))?$'
  AND octet_length(split_part(health_gate_host, ':', 1)) <= 253
);
