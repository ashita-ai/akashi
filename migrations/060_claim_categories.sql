-- 060: Add category column to decision_claims for LLM-based claim extraction.
-- Categories: finding, recommendation, assessment, status.
-- Only 'finding' and 'assessment' claims participate in conflict scoring,
-- filtering out boilerplate status updates and non-actionable recommendations.

ALTER TABLE decision_claims ADD COLUMN IF NOT EXISTS category TEXT;

-- Index for efficient filtering during conflict scoring.
CREATE INDEX IF NOT EXISTS idx_decision_claims_category ON decision_claims(decision_id, category)
    WHERE category IN ('finding', 'assessment');
