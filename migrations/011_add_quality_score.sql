-- Add quality score column to decisions table.
-- Quality score (0.0-1.0) measures trace completeness: confidence, reasoning, alternatives, evidence.
ALTER TABLE decisions ADD COLUMN IF NOT EXISTS quality_score REAL DEFAULT 0.0;

-- Index for ordering by quality.
CREATE INDEX IF NOT EXISTS idx_decisions_quality ON decisions (quality_score DESC);

-- Composite index for quality-weighted queries by type.
CREATE INDEX IF NOT EXISTS idx_decisions_type_quality ON decisions (decision_type, quality_score DESC);
