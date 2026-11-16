-- Slice 21E (gate 14, defence in depth): a route head flagged
-- reconciliation_required cannot change hostname.
-- The application already refuses this (AssignRouteHostname returns
-- ErrRouteHeadReconciliationRequired under FOR UPDATE), and 000018 already
-- refuses a hostname change while a release is active. The only unguarded
-- case was a flagged head with no active release (an ambiguous FIRST
-- activation); this closes it in the database too.
-- OLD-only by owner decision: the rule mirrors the app check exactly, so a
-- statement that sets the flag and changes hostname together is not newly
-- refused. No production UPDATE changes hostname on a flagged head:
-- AssignRouteHostname runs only when unflagged with no active release;
-- activation success changes active_release_id/revision/flag, not hostname;
-- the flag set and the flag clear touch the flag only.
-- The function is replaced in full: the body is the live 000020 definition
-- (introspected with pg_get_functiondef) plus one block; the trigger binding
-- from 000018 is unchanged.
CREATE OR REPLACE FUNCTION public.validate_route_head_update()
 RETURNS trigger
 LANGUAGE plpgsql
AS $$ BEGIN
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
  IF OLD.reconciliation_required AND NEW.hostname <> OLD.hostname THEN
    RAISE EXCEPTION 'route head hostname cannot change while reconciliation is required';
  END IF;
  RETURN NEW;
END; $$;
