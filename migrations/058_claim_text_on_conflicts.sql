-- 058: Add claim fragment columns to scored_conflicts for claim-level audit trail.
-- When claim-level scoring produces a higher significance than full-outcome scoring,
-- the specific claim texts that were compared are stored here. NULL when the winning
-- scoring method was not "claim".
ALTER TABLE scored_conflicts
  ADD COLUMN claim_text_a TEXT,
  ADD COLUMN claim_text_b TEXT;
