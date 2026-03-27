-- 089: Fix conflict_resolutions immutability trigger and proof_leaves duplicate FK.
--
-- Two issues:
--
-- 1. conflict_resolutions has an immutability trigger that blocks DELETE, but
--    DeleteAgentData needs to remove resolutions before their parent
--    scored_conflicts (which use ON DELETE RESTRICT). Add a session-variable
--    bypass matching the pattern from migration 086 (decision_assessments).
--
-- 2. proof_leaves (migration 084) declares two foreign keys on org_id:
--    an inline REFERENCES and a named CONSTRAINT. Drop the redundant one.

-- Part 1: Replace the immutability trigger to allow application-layer deletes
-- that set the session flag 'akashi.allow_conflict_resolution_delete'.

CREATE OR REPLACE FUNCTION prevent_conflict_resolutions_modify()
RETURNS TRIGGER AS $$
BEGIN
  -- UPDATE is never allowed — resolutions are append-only audit artifacts.
  IF TG_OP = 'UPDATE' THEN
    RAISE EXCEPTION 'conflict_resolutions is append-only';
  END IF;

  -- DELETE is allowed only when the caller has set the session flag within
  -- a transaction (SET LOCAL). This restricts deletes to explicit
  -- application-layer operations like DeleteAgentData, preventing
  -- accidental or ad-hoc removal.
  IF TG_OP = 'DELETE' THEN
    IF current_setting('akashi.allow_conflict_resolution_delete', true) = 'true' THEN
      RETURN OLD;
    END IF;
    RAISE EXCEPTION 'conflict_resolutions is append-only';
  END IF;

  RETURN NULL;
END;
$$ LANGUAGE plpgsql;

-- Part 2: Drop the duplicate named FK constraint on proof_leaves.org_id.
-- The inline REFERENCES on the column definition already enforces the same
-- relationship; the explicit CONSTRAINT fk_proof_leaves_org is redundant.

ALTER TABLE proof_leaves DROP CONSTRAINT IF EXISTS fk_proof_leaves_org;
