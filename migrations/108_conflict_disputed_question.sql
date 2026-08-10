-- 108: Persist the disputed question a contradiction verdict names.
--
-- The validator prompt requires a CONTRADICTION verdict to state, in one clause,
-- the single question the two decisions answer differently, and the parser
-- downgrades any verdict that cannot name one. That clause is the most useful
-- artifact the judge produces: it is what a reviewer needs in order to decide,
-- and it is what makes the verdict auditable rather than an assertion. Until
-- now it was parsed and discarded.
--
-- NULL for every relationship other than 'contradiction' — the parser clears it
-- otherwise — so a non-null value is a positive signal that a dispute was
-- actually identified, not merely that a pair scored highly.

ALTER TABLE scored_conflicts
    ADD COLUMN IF NOT EXISTS disputed_question TEXT;

COMMENT ON COLUMN scored_conflicts.disputed_question IS
    'The single question both decisions answer differently, as named by the validator. Non-null only for contradictions.';
