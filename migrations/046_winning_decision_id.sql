-- 046: Add winning_decision_id to scored_conflicts for structured conflict resolution.
-- Tracks which decision prevailed when a conflict is resolved. Distinct from
-- resolution_decision_id (the narrative trace created to document the resolution).
ALTER TABLE scored_conflicts
    ADD COLUMN winning_decision_id UUID REFERENCES decisions(id) ON DELETE SET NULL;

CREATE INDEX idx_scored_conflicts_winner
    ON scored_conflicts(winning_decision_id)
    WHERE winning_decision_id IS NOT NULL;
