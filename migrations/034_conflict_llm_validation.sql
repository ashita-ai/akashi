-- 034: LLM-validated conflict detection.
--
-- Adds an explanation column for LLM-generated conflict explanations and widens
-- the scoring_method constraint to allow 'llm' as a method. When an LLM validator
-- is configured, embedding/claim candidates are confirmed by the LLM before insertion,
-- and the explanation records why the LLM judged the pair contradictory.

ALTER TABLE scored_conflicts ADD COLUMN IF NOT EXISTS explanation TEXT;

ALTER TABLE scored_conflicts DROP CONSTRAINT IF EXISTS scored_conflicts_scoring_method_check;
ALTER TABLE scored_conflicts ADD CONSTRAINT scored_conflicts_scoring_method_check
    CHECK (scoring_method = ANY (ARRAY['embedding', 'text', 'claim', 'llm']));
