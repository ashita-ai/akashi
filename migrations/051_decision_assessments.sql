-- 051: decision_assessments — explicit outcome feedback for decisions (spec 29 / ADR-020 Tier 2).
-- Agents assess a prior decision after seeing the outcome: correct, incorrect, or partially_correct.
-- One assessor may assess each decision exactly once (unique index). Multiple agents can assess
-- the same decision, producing a majority-vote signal for ReScore and akashi_check.

CREATE TABLE decision_assessments (
    id                UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    decision_id       UUID        NOT NULL REFERENCES decisions(id) ON DELETE CASCADE,
    org_id            UUID        NOT NULL,
    assessor_agent_id TEXT        NOT NULL,
    outcome           TEXT        NOT NULL CHECK (outcome IN ('correct', 'incorrect', 'partially_correct')),
    notes             TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Fast lookup by decision (used by GET /v1/decisions/{id}/assessments and GET /v1/decisions/{id}).
CREATE INDEX idx_decision_assessments_decision_id
    ON decision_assessments (decision_id);

-- Org-scoped time-ordered listing (used by export, analytics).
CREATE INDEX idx_decision_assessments_org_created
    ON decision_assessments (org_id, created_at DESC);

-- One assessor per decision. Prevents duplicate assessments while allowing
-- updates via ON CONFLICT DO UPDATE (agents can revise their assessment).
CREATE UNIQUE INDEX idx_decision_assessments_unique_assessor
    ON decision_assessments (decision_id, assessor_agent_id);

-- Immutability trigger: prevent DELETE on decision_assessments.
-- Assessments are part of the audit trail — they can be superseded (upserted)
-- but never silently removed. Hard deletes require a cascade from decisions.
CREATE OR REPLACE FUNCTION prevent_assessment_delete()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'decision_assessments rows are immutable: use upsert to revise an assessment (decision_id=%, assessor=%)',
        OLD.decision_id, OLD.assessor_agent_id;
END;
$$;

CREATE TRIGGER trg_prevent_assessment_delete
    BEFORE DELETE ON decision_assessments
    FOR EACH ROW EXECUTE FUNCTION prevent_assessment_delete();
