-- 086: Change decision_assessments FK from CASCADE to RESTRICT.
--
-- Problem: ON DELETE CASCADE on decision_assessments.decision_id contradicts
-- the BEFORE DELETE immutability trigger (trg_prevent_assessment_delete) added
-- in migration 051. In PostgreSQL, cascaded deletes fire row-level triggers,
-- so the cascade raises an exception and blocks decision deletion entirely.
-- Even if it didn't, CASCADE would silently destroy feedback-loop data.
--
-- Fix: switch to RESTRICT so the database enforces that assessments must be
-- explicitly archived and deleted before their parent decision can be removed.
-- The application-layer deletion code (DeleteAgentData, deleteBatch) already
-- archives assessments to deletion_audit_log; this migration makes the
-- explicit DELETE mandatory rather than relying on a broken cascade.

ALTER TABLE decision_assessments
    DROP CONSTRAINT decision_assessments_decision_id_fkey,
    ADD CONSTRAINT decision_assessments_decision_id_fkey
        FOREIGN KEY (decision_id) REFERENCES decisions(id) ON DELETE RESTRICT;

-- Replace the immutability trigger to allow application-layer deletes that
-- set the session flag 'akashi.allow_assessment_delete'. This preserves
-- protection against accidental/unauthorized deletes while letting the
-- archive-then-delete code path work. The flag is transaction-scoped
-- (SET LOCAL), so it cannot leak across connections.
CREATE OR REPLACE FUNCTION prevent_assessment_mutation()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF current_setting('akashi.allow_assessment_delete', true) = 'true' THEN
            RETURN OLD;
        END IF;
        RAISE EXCEPTION 'decision_assessments rows are immutable: a revised assessment is a new row (decision_id=%, assessor=%)',
            OLD.decision_id, OLD.assessor_agent_id;
    ELSIF TG_OP = 'UPDATE' THEN
        RAISE EXCEPTION 'decision_assessments rows are immutable: a revised assessment is a new row (decision_id=%, assessor=%)',
            OLD.decision_id, OLD.assessor_agent_id;
    END IF;
    RETURN OLD;
END;
$$;
