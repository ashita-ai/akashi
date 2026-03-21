-- 074: Queue Qdrant re-sync when agent_context is updated on existing decisions.
--
-- In normal operation agent_context is write-once (revisions create new rows).
-- However, manual corrections (e.g. fixing workspace names to canonical project
-- names, as done for issue #450) update agent_context on existing rows. Without
-- this trigger, the corrected project name reaches Postgres (via the generated
-- column) but Qdrant retains the stale value, causing conflict detection to
-- miss cross-project comparisons.
--
-- The trigger inserts an upsert entry into search_outbox so the outbox worker
-- picks up the change and re-syncs the decision's Qdrant point.

CREATE OR REPLACE FUNCTION queue_search_outbox_on_context_update()
RETURNS trigger AS $$
BEGIN
  IF OLD.agent_context IS DISTINCT FROM NEW.agent_context THEN
    INSERT INTO search_outbox (decision_id, org_id, operation, created_at)
    VALUES (NEW.id, NEW.org_id, 'upsert', now())
    ON CONFLICT (decision_id, operation) DO UPDATE SET
      created_at = EXCLUDED.created_at,
      attempts = 0,
      last_error = NULL,
      locked_until = NULL;
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_decisions_context_update_outbox
  AFTER UPDATE OF agent_context ON decisions
  FOR EACH ROW
  EXECUTE FUNCTION queue_search_outbox_on_context_update();
