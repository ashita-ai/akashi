-- 051: decision_assessments — explicit outcome feedback for decisions (spec 29 / ADR-020 Tier 2).
-- Agents assess a prior decision after seeing the outcome: correct, incorrect, or partially_correct.
-- Append-only: each assessment is a new row. An assessor changing their verdict later is itself
-- an auditable event — we never overwrite prior assessments. GetAssessmentSummary counts only
-- the latest assessment per assessor (DISTINCT ON) so summaries reflect current verdicts without
-- erasing history.

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
-- Covers DISTINCT ON queries ordered by (decision_id, assessor_agent_id, created_at DESC).
CREATE INDEX idx_decision_assessments_decision_id
    ON decision_assessments (decision_id, assessor_agent_id, created_at DESC);

-- Org-scoped time-ordered listing (used by export, analytics).
CREATE INDEX idx_decision_assessments_org_created
    ON decision_assessments (org_id, created_at DESC);

-- Immutability triggers: prevent DELETE and UPDATE on decision_assessments.
-- Assessments are append-only audit records. A revised assessment is a new row,
-- not an overwrite. Hard deletes are only permitted via ON DELETE CASCADE from decisions.
CREATE OR REPLACE FUNCTION prevent_assessment_mutation()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'decision_assessments rows are immutable: a revised assessment is a new row (decision_id=%, assessor=%)',
            OLD.decision_id, OLD.assessor_agent_id;
    ELSIF TG_OP = 'UPDATE' THEN
        RAISE EXCEPTION 'decision_assessments rows are immutable: a revised assessment is a new row (decision_id=%, assessor=%)',
            OLD.decision_id, OLD.assessor_agent_id;
    END IF;
    RETURN OLD;
END;
$$;

CREATE TRIGGER trg_prevent_assessment_delete
    BEFORE DELETE ON decision_assessments
    FOR EACH ROW EXECUTE FUNCTION prevent_assessment_mutation();

CREATE TRIGGER trg_prevent_assessment_update
    BEFORE UPDATE ON decision_assessments
    FOR EACH ROW EXECUTE FUNCTION prevent_assessment_mutation();
