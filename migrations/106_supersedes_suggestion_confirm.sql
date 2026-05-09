-- 106: Make migration 104's trigger retire matching latent suggestions when
-- an agent confirms a supersedes_id link, so the audit trail does not show
-- a stale 'suggested' row alongside the new confirmed one. Closes the
-- promotion gap noted in code review of #710.
--
-- The agent's confirmation flow is: detector writes
--   ('suggested', superseding_id=X, superseded_id=Y)
-- where X is an existing decision by the agent on the same ticket as Y.
-- The agent then traces a NEW decision X' with supersedes_id=Y. Because X'
-- gets a fresh UUID, the migration-104 ON CONFLICT path never fires on the
-- existing 'suggested' row — it inserts a parallel ('supersedes', X', Y).
-- Without this migration the original ('suggested', X, Y) row sits stale
-- forever, polluting akashi_check responses and the audit story.
--
-- This migration replaces the trigger function with a version that, after
-- writing the confirmed link, deletes any 'suggested' rows from the same
-- agent that proposed the now-explicitly-superseded predecessor. The match
-- is by (org_id, superseded_id, agent_id) — narrow enough that suggestions
-- from other agents are untouched.

CREATE OR REPLACE FUNCTION sync_decision_supersedes_primary()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.supersedes_id IS NULL THEN
        UPDATE decision_supersedes
        SET is_primary = FALSE
        WHERE superseding_id = NEW.id
          AND org_id = NEW.org_id
          AND is_primary;
        RETURN NEW;
    END IF;

    UPDATE decision_supersedes
    SET is_primary = FALSE
    WHERE superseding_id = NEW.id
      AND org_id = NEW.org_id
      AND superseded_id <> NEW.supersedes_id
      AND is_primary;

    INSERT INTO decision_supersedes (superseding_id, superseded_id, org_id, relationship, is_primary)
    VALUES (NEW.id, NEW.supersedes_id, NEW.org_id, 'supersedes', TRUE)
    ON CONFLICT (superseding_id, superseded_id) DO UPDATE
    SET is_primary = TRUE;

    -- Retire latent suggestions from the same agent that proposed the
    -- predecessor we just confirmed. The new confirmed row above is the
    -- canonical record — keeping the suggestion would surface a stale
    -- hint on every subsequent akashi_check call.
    DELETE FROM decision_supersedes
    WHERE relationship = 'suggested'
      AND org_id = NEW.org_id
      AND superseded_id = NEW.supersedes_id
      AND superseding_id IN (
          SELECT id FROM decisions
          WHERE org_id = NEW.org_id
            AND agent_id = NEW.agent_id
      );

    RETURN NEW;
END;
$$;
