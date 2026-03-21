-- 071: Add precedent_reason column to decisions for explaining why a precedent was cited.
-- When an agent references a prior decision via precedent_ref, precedent_reason captures
-- the human-readable explanation of why that precedent applies, making attribution chains
-- auditable without re-reading both decisions.

ALTER TABLE decisions ADD COLUMN precedent_reason TEXT;

COMMENT ON COLUMN decisions.precedent_reason IS 'Free-text explanation of why the precedent_ref decision was cited. NULL when no precedent is set or the agent did not provide a reason.';
