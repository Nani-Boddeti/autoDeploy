-- Slice 10, Step 10c-2b: let an operator resolve an activation that tx2 left
-- at reconciliation_required, and name the two new job-event kinds this
-- needs. Forward-only: 000019/000020 are not edited. Every constraint,
-- trigger and index name below was introspected from a live PostgreSQL 18
-- database (pg_constraint / pg_trigger / pg_indexes / pg_proc), not assumed
-- -- this slice has already been bitten by a guessed name
-- (release_operations_closed_at_check was really release_operations_check1).

-- 1. validate_release_operation_update (trigger
-- release_operations_validate_update), replaced in full. The identity block
-- and the entire non-activate branch are byte-for-byte what 000019 shipped
-- (diffed against pg_proc.prosrc). Only the kind='activate' branch changes:
-- a row at OLD.state = 'reconciliation_required' may now move
--   reconciliation_required -> succeeded   (operator declared "activated")
--   reconciliation_required -> failed      (operator declared "not_activated")
--   reconciliation_required -> reconciliation_required, ONLY with closed_at
--       unchanged: a late-evidence write to outcome_summary. This one is
--       required, not optional -- step E records late reports that way.
-- closed_at is immutable once set on this edge: the instant the ambiguity was
-- recorded is forensic evidence; resolution time lives on the attempt and in
-- the job events. release_operations_check1 ((state IN ('started',
-- 'switch_authorized')) = (closed_at IS NULL)) and release_operations_check
-- (closed_at >= created_at) are different rules and do not stop a closed_at
-- rewrite between two closed states, hence the explicit trigger rule.
-- Everything else still raises exactly as before: any transition out of
-- succeeded/failed/started ("is closed"), and switch_authorized -> failed
-- (tx2 never reports a definite failure on this kind; ambiguity goes to
-- reconciliation_required). Rejected: letting the operator reopen a
-- closed row, or an unconditional rr -> rr, which would let closed_at drift.
CREATE OR REPLACE FUNCTION validate_release_operation_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
  IF NEW.id <> OLD.id OR NEW.release_id <> OLD.release_id OR NEW.job_id <> OLD.job_id OR NEW.attempt_id <> OLD.attempt_id
     OR NEW.fence <> OLD.fence OR NEW.kind <> OLD.kind OR NEW.digest <> OLD.digest
     OR NEW.created_at <> OLD.created_at
     OR NEW.environment_id IS DISTINCT FROM OLD.environment_id
     OR NEW.expected_active_release_id IS DISTINCT FROM OLD.expected_active_release_id
     OR NEW.expected_route_revision IS DISTINCT FROM OLD.expected_route_revision
     OR NEW.route_digest IS DISTINCT FROM OLD.route_digest THEN
    RAISE EXCEPTION 'release operation identity is immutable';
  END IF;
  IF OLD.kind = 'activate' THEN
    IF OLD.state = 'reconciliation_required' THEN
      IF NEW.state NOT IN ('succeeded','failed','reconciliation_required') THEN
        RAISE EXCEPTION 'reconciliation_required activate release operation may only move to succeeded, failed or stay reconciliation_required';
      END IF;
      IF NEW.closed_at IS DISTINCT FROM OLD.closed_at THEN
        RAISE EXCEPTION 'activate release operation closed_at is immutable once set';
      END IF;
    ELSE
      IF OLD.state <> 'switch_authorized' THEN
        RAISE EXCEPTION 'release operation % is closed', OLD.state;
      END IF;
      IF NEW.state NOT IN ('succeeded','reconciliation_required') THEN
        RAISE EXCEPTION 'activate release operation must close to succeeded or reconciliation_required';
      END IF;
    END IF;
  ELSE
    IF OLD.state <> 'started' THEN
      RAISE EXCEPTION 'release operation % is closed', OLD.state;
    END IF;
    IF NEW.state NOT IN ('succeeded','failed') THEN
      RAISE EXCEPTION 'release operation must close to succeeded or failed';
    END IF;
  END IF;
  RETURN NEW;
END; $$;

-- 2. agent_job_events: two new literals for the operator-resolution path
-- ('activation_not_activated' = operator declared the switch never took
-- effect; 'activation_late_report' = late agent evidence recorded against an
-- already-ambiguous operation). 'reconciled' is not reused: it is the
-- older, generic "resolved" literal and cannot distinguish the two verdicts.
-- Every literal 000020 permitted is kept. The constraint name
-- (agent_job_events_event_type_check) was introspected from pg_constraint.
ALTER TABLE agent_job_events DROP CONSTRAINT agent_job_events_event_type_check;
ALTER TABLE agent_job_events ADD CONSTRAINT agent_job_events_event_type_check CHECK (event_type IN ('enqueued','claimed','cancel_requested','cancelled','expired','retry_scheduled','succeeded','failed','dead','reconciled','activation_authorized','activated','activation_reconciliation_required','activation_not_activated','activation_late_report'));

-- 3. "One UNRESOLVED activation per environment" as a rule, not an
-- enumerated state. 000019's index covered only state = 'switch_authorized',
-- so an operation parked at reconciliation_required no longer counted and a
-- second Activate could insert a second operation beside it (reproduced
-- experimentally). Rejected: adding 'reconciliation_required' to the
-- enumeration -- the next unresolved state would silently escape again; a
-- NOT IN over the two RESOLVED states fails safe for any future state.
-- Same index name, recreated. Behaviour change approved by the owner: an
-- unresolved activation now hard-wedges new activations for that environment
-- until an operator resolves it.
-- This migration FAILS if any environment already holds two unresolved
-- activate operations. That is intended: it surfaces pre-existing corruption
-- instead of hiding it. Such pairs are exactly the bug fixed here, so they are
-- plausible in real data. A plain UPDATE cannot clear them: before this
-- migration the old trigger forbids reconciliation_required -> anything and
-- switch_authorized -> failed. The remedy is to ALTER TABLE release_operations
-- DISABLE TRIGGER release_operations_validate_update, fix the rows (or DELETE
-- the surplus ones), ENABLE the trigger again, and re-run. Detect them with:
--   SELECT environment_id, count(*) FROM release_operations
--   WHERE kind = 'activate' AND state NOT IN ('succeeded','failed')
--   GROUP BY environment_id HAVING count(*) > 1;
DROP INDEX release_operations_environment_open_activation_idx;
CREATE UNIQUE INDEX release_operations_environment_open_activation_idx ON release_operations(environment_id) WHERE kind = 'activate' AND state NOT IN ('succeeded','failed');
