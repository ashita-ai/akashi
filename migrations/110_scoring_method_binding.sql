-- 110: Allow scoring_method = 'binding_v1'.
--
-- Binding conflicts are found by joining declared parameter bindings rather
-- than by scoring text (see migration 109). They need their own scoring_method
-- so they stay distinguishable from judge verdicts: a binding conflict has no
-- false-positive rate to estimate, and averaging the two together would flatter
-- the judge's measured precision.
--
-- Migration 062's constraint predates the binding path, so an insert from it
-- would fail the CHECK. That failure is loud rather than silent — the scorer
-- logs and skips — but it would mean the feature never produced a single
-- conflict while appearing to work.

ALTER TABLE scored_conflicts DROP CONSTRAINT IF EXISTS scored_conflicts_scoring_method_check;
ALTER TABLE scored_conflicts ADD CONSTRAINT scored_conflicts_scoring_method_check
    CHECK (scoring_method = ANY (ARRAY['embedding', 'text', 'claim', 'llm', 'llm_v2', 'external', 'binding_v1']));
