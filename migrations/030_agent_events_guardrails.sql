-- 030: Guardrails against agent_events/run orphaning.
--
-- TimescaleDB hypertables cannot always enforce conventional foreign keys with
-- desired performance characteristics. Add trigger-based integrity guardrails:
--   1) agent_events inserts must reference an existing run in the same org.
--   2) agent_runs cannot be deleted while events still exist for that run.

CREATE OR REPLACE FUNCTION check_agent_events_run_exists()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM agent_runs r
        WHERE r.id = NEW.run_id
          AND r.org_id = NEW.org_id
          AND r.agent_id = NEW.agent_id
    ) THEN
        RAISE EXCEPTION 'agent_events references missing run_id/org_id/agent_id (%/%/%)',
            NEW.run_id, NEW.org_id, NEW.agent_id;
    END IF;

    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_agent_events_run_exists ON agent_events;
CREATE TRIGGER trg_agent_events_run_exists
BEFORE INSERT ON agent_events
FOR EACH ROW
EXECUTE FUNCTION check_agent_events_run_exists();

CREATE OR REPLACE FUNCTION prevent_run_delete_with_events()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM agent_events e
        WHERE e.run_id = OLD.id
          AND e.org_id = OLD.org_id
    ) THEN
        RAISE EXCEPTION 'cannot delete run % while agent_events still exist', OLD.id;
    END IF;

    RETURN OLD;
END;
$$;

DROP TRIGGER IF EXISTS trg_prevent_run_delete_with_events ON agent_runs;
CREATE TRIGGER trg_prevent_run_delete_with_events
BEFORE DELETE ON agent_runs
FOR EACH ROW
EXECUTE FUNCTION prevent_run_delete_with_events();
