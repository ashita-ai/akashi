-- 068: Add ON DELETE CASCADE to alternatives and evidence FK constraints.
--
-- The original FKs in migration 001 default to RESTRICT, which blocks any
-- direct DELETE FROM decisions unless callers manually delete dependents first.
-- All existing deletion code paths (DeleteAgentData, deleteBatch) already
-- delete dependents explicitly, but RESTRICT is a landmine for any future
-- code path. CASCADE aligns the schema with the intended ownership semantics:
-- alternatives and evidence belong to their parent decision.

-- alternatives: drop the unnamed FK and recreate with CASCADE.
ALTER TABLE alternatives
    DROP CONSTRAINT alternatives_decision_id_fkey,
    ADD CONSTRAINT alternatives_decision_id_fkey
        FOREIGN KEY (decision_id) REFERENCES decisions(id) ON DELETE CASCADE;

-- evidence: drop the unnamed FK and recreate with CASCADE.
ALTER TABLE evidence
    DROP CONSTRAINT evidence_decision_id_fkey,
    ADD CONSTRAINT evidence_decision_id_fkey
        FOREIGN KEY (decision_id) REFERENCES decisions(id) ON DELETE CASCADE;
