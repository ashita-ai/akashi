-- 094: Add partial unique index to prevent duplicate auto-assessments.
--
-- The auto-assessor checks HasAssessmentFromSource before inserting, but that
-- check-then-act pattern has a TOCTOU race when concurrent goroutines assess
-- the same decision from the same source. This index makes the DB the
-- authoritative idempotency guard for non-manual assessment sources.
-- Manual assessments remain append-only (an assessor revising their verdict
-- is itself an auditable event).

CREATE UNIQUE INDEX IF NOT EXISTS idx_assessments_auto_unique
    ON decision_assessments (decision_id, org_id, source)
    WHERE source != 'manual';
