-- 038: Conflict precision: relationship classification, confidence weight, temporal decay, resolution-by-decision.
ALTER TABLE scored_conflicts ADD COLUMN IF NOT EXISTS relationship TEXT;
ALTER TABLE scored_conflicts ADD COLUMN IF NOT EXISTS confidence_weight DOUBLE PRECISION;
ALTER TABLE scored_conflicts ADD COLUMN IF NOT EXISTS temporal_decay DOUBLE PRECISION;
ALTER TABLE scored_conflicts ADD COLUMN IF NOT EXISTS resolution_decision_id UUID REFERENCES decisions(id);

-- Widen scoring_method constraint to include llm_v2.
ALTER TABLE scored_conflicts DROP CONSTRAINT IF EXISTS scored_conflicts_scoring_method_check;
ALTER TABLE scored_conflicts ADD CONSTRAINT scored_conflicts_scoring_method_check
    CHECK (scoring_method = ANY (ARRAY['embedding', 'text', 'claim', 'llm', 'llm_v2']));
