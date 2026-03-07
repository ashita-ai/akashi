-- 063: Add 'external' to scoring_method constraint for enterprise pairwise scorer.
-- The WithPairwiseScorer path sets scoring_method = 'external' but the constraint
-- from migration 038 only allowed embedding, text, claim, llm, llm_v2.

ALTER TABLE scored_conflicts DROP CONSTRAINT IF EXISTS scored_conflicts_scoring_method_check;
ALTER TABLE scored_conflicts ADD CONSTRAINT scored_conflicts_scoring_method_check
    CHECK (scoring_method = ANY (ARRAY['embedding', 'text', 'claim', 'llm', 'llm_v2', 'external']));
