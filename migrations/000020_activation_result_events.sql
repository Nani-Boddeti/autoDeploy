-- Slice 10, Step 10c-2a: event literals for POST /agent/v1/leases/activation-result.
-- 000019 deliberately left these two out ("10c-2's own 'activated'/
-- 'activation_reconciliation_required' literals are deliberately NOT added
-- here"); tx2 is the first writer, so they are added now, forward-only.
-- 'activated' is appended on a successful activation (before 'succeeded');
-- 'activation_reconciliation_required' names the failed/ambiguous path that
-- leaves the environment blocked. 'reconciled' is not reused for the latter:
-- it means "resolved", the opposite of this state. Every literal 000019
-- already permitted is kept. The constraint name was introspected from 000019's
-- own DROP/ADD pair (agent_job_events_event_type_check).
ALTER TABLE agent_job_events DROP CONSTRAINT agent_job_events_event_type_check;
ALTER TABLE agent_job_events ADD CONSTRAINT agent_job_events_event_type_check CHECK (event_type IN ('enqueued','claimed','cancel_requested','cancelled','expired','retry_scheduled','succeeded','failed','dead','reconciled','activation_authorized','activated','activation_reconciliation_required'));

-- route_heads.reconciliation_required must be settable (and clearable)
-- without a revision bump. route_revision is the CAS counter for the
-- *desired route state* (hostname/active release); marking or clearing a
-- reconciliation flag does not change desired state, so 000018's "every
-- UPDATE advances the revision by exactly one" was over-broad for it.
-- tx2's failure path sets the flag after the control plane already rendered
-- and handed the agent revision+1, so bumping the revision there would burn
-- a revision that was never applied; 10c-2b clears the flag again.
-- The function is replaced in full, semantically preserving all four 000018
-- rules (identity immutable; revision advances by exactly one -- rule 2,
-- restructured into the IF/ELSIF below; updated_at monotonic; hostname frozen
-- while a release is active) and adding one
-- narrow exemption: an UPDATE that leaves route_revision unchanged is
-- accepted only if reconciliation_required actually changes and every other
-- column (environment_id, hostname, active_release_id, created_at) is
-- identical -- only reconciliation_required and updated_at may differ. The
-- table's column list was read from the live database, not assumed.
CREATE OR REPLACE FUNCTION validate_route_head_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
  IF NEW.environment_id <> OLD.environment_id THEN
    RAISE EXCEPTION 'route head identity is immutable';
  END IF;
  IF NEW.route_revision = OLD.route_revision THEN
    IF NEW.reconciliation_required IS NOT DISTINCT FROM OLD.reconciliation_required
       OR NEW.hostname <> OLD.hostname
       OR NEW.active_release_id IS DISTINCT FROM OLD.active_release_id
       OR NEW.created_at <> OLD.created_at THEN
      RAISE EXCEPTION 'route head revision must advance by exactly one unless only reconciliation_required changes; expected % got %', OLD.route_revision + 1, NEW.route_revision;
    END IF;
  ELSIF NEW.route_revision <> OLD.route_revision + 1 THEN
    RAISE EXCEPTION 'route head revision must advance by exactly one; expected % got %', OLD.route_revision + 1, NEW.route_revision;
  END IF;
  IF NEW.updated_at < OLD.updated_at THEN
    RAISE EXCEPTION 'route head updated_at must be monotonic';
  END IF;
  IF OLD.active_release_id IS NOT NULL AND NEW.hostname <> OLD.hostname THEN
    RAISE EXCEPTION 'route head hostname cannot change while a release is active';
  END IF;
  RETURN NEW;
END; $$;
