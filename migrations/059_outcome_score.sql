-- 059: Add outcome_score column to decisions table.
--
-- outcome_score (0.0–1.0) reflects ground-truth assessment feedback:
-- how often assessors judged this decision correct. Computed from the
-- decision_assessments table and updated whenever a new assessment is recorded.
-- Separate from completeness_score, which measures trace form at write time.

ALTER TABLE decisions ADD COLUMN outcome_score REAL;
