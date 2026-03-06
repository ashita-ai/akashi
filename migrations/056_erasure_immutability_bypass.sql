-- 056: Allow GDPR erasure to bypass immutability trigger.
--
-- The decisions_immutable_guard trigger (migration 036) correctly prevents
-- modification of core audit fields. However, GDPR right-to-erasure requires
-- scrubbing PII fields in-place while keeping the row. This migration
-- replaces the trigger function to allow updates where the new outcome is
-- exactly '[erased]' — the GDPR tombstone sentinel. All other immutability
-- rules remain unchanged.

CREATE OR REPLACE FUNCTION decisions_immutable_guard()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    -- GDPR erasure exemption: when outcome is being set to the erasure
    -- sentinel '[erased]', allow the update. The application layer
    -- (EraseDecision) enforces that this only happens through the proper
    -- erasure workflow with audit trail and role checks.
    IF NEW.outcome = '[erased]' AND OLD.outcome IS DISTINCT FROM '[erased]' THEN
        RETURN NEW;
    END IF;

    -- Standard immutability checks for all non-erasure updates.
    IF NEW.outcome IS DISTINCT FROM OLD.outcome THEN
        RAISE EXCEPTION 'decisions row is immutable: cannot modify outcome (decision_id=%)', OLD.id;
    END IF;
    IF NEW.reasoning IS DISTINCT FROM OLD.reasoning THEN
        RAISE EXCEPTION 'decisions row is immutable: cannot modify reasoning (decision_id=%)', OLD.id;
    END IF;
    IF NEW.confidence IS DISTINCT FROM OLD.confidence THEN
        RAISE EXCEPTION 'decisions row is immutable: cannot modify confidence (decision_id=%)', OLD.id;
    END IF;
    IF NEW.decision_type IS DISTINCT FROM OLD.decision_type THEN
        RAISE EXCEPTION 'decisions row is immutable: cannot modify decision_type (decision_id=%)', OLD.id;
    END IF;
    IF NEW.agent_id IS DISTINCT FROM OLD.agent_id THEN
        RAISE EXCEPTION 'decisions row is immutable: cannot modify agent_id (decision_id=%)', OLD.id;
    END IF;
    IF NEW.run_id IS DISTINCT FROM OLD.run_id THEN
        RAISE EXCEPTION 'decisions row is immutable: cannot modify run_id (decision_id=%)', OLD.id;
    END IF;
    IF NEW.org_id IS DISTINCT FROM OLD.org_id THEN
        RAISE EXCEPTION 'decisions row is immutable: cannot modify org_id (decision_id=%)', OLD.id;
    END IF;
    IF NEW.content_hash IS DISTINCT FROM OLD.content_hash THEN
        RAISE EXCEPTION 'decisions row is immutable: cannot modify content_hash (decision_id=%)', OLD.id;
    END IF;
    IF NEW.valid_from IS DISTINCT FROM OLD.valid_from THEN
        RAISE EXCEPTION 'decisions row is immutable: cannot modify valid_from (decision_id=%)', OLD.id;
    END IF;
    IF NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'decisions row is immutable: cannot modify created_at (decision_id=%)', OLD.id;
    END IF;
    IF NEW.transaction_time IS DISTINCT FROM OLD.transaction_time THEN
        RAISE EXCEPTION 'decisions row is immutable: cannot modify transaction_time (decision_id=%)', OLD.id;
    END IF;

    RETURN NEW;
END;
$$;
