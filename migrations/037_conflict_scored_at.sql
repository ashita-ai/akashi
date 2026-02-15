-- 037: Track which decisions have completed conflict scoring.
--
-- Adds conflict_scored_at to decisions. NULL means the decision has not been
-- scored for conflicts yet. BackfillScoring uses this to skip already-scored
-- decisions, making server restarts near-instant instead of re-scoring the
-- entire corpus.
--
-- When transitioning from embedding-only to LLM-validated scoring, the
-- application resets conflict_scored_at to NULL so all decisions get re-scored
-- through the LLM validator.
--
-- This column is NOT protected by the trg_decisions_immutable trigger
-- (migration 036) because it is operational metadata, not audit content.

ALTER TABLE decisions ADD COLUMN IF NOT EXISTS conflict_scored_at TIMESTAMPTZ;

-- Partial index for the backfill query: find decisions with embeddings that
-- haven't been scored yet. This keeps the backfill query cheap even at scale.
-- Note: not using CONCURRENTLY because migrations run inside transactions.
CREATE INDEX IF NOT EXISTS idx_decisions_unscored_conflicts
    ON decisions (valid_from ASC)
    WHERE valid_to IS NULL
      AND embedding IS NOT NULL
      AND outcome_embedding IS NOT NULL
      AND conflict_scored_at IS NULL;
