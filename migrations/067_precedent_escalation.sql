-- 067: Precedent-aware conflict escalation.
-- When a new conflict contradicts the winning side of a prior resolved conflict,
-- link the reopened resolution and track how many times a group has been reopened.

ALTER TABLE scored_conflicts
    ADD COLUMN reopens_resolution_id UUID NULL REFERENCES scored_conflicts(id);

ALTER TABLE conflict_groups
    ADD COLUMN times_reopened INT NOT NULL DEFAULT 0;

CREATE INDEX idx_scored_conflicts_reopens ON scored_conflicts (reopens_resolution_id)
    WHERE reopens_resolution_id IS NOT NULL;
