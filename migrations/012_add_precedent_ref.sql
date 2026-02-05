-- Add precedent reference column to track decision influence chains.
-- Links a decision to a prior decision that influenced it.
ALTER TABLE decisions ADD COLUMN IF NOT EXISTS precedent_ref UUID REFERENCES decisions(id);

-- Index for finding decisions that reference a given precedent.
CREATE INDEX IF NOT EXISTS idx_decisions_precedent_ref ON decisions (precedent_ref) WHERE precedent_ref IS NOT NULL;
