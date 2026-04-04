-- 093: Add source column to decision_assessments to distinguish automatic
-- assessments (from supersession, conflict resolution, citation signals) from
-- manual assessments recorded by agents via akashi_assess.

ALTER TABLE decision_assessments
    ADD COLUMN source TEXT NOT NULL DEFAULT 'manual';

COMMENT ON COLUMN decision_assessments.source IS
    'Origin of the assessment: manual (agent-submitted), supersession (decision was superseded), '
    'conflict (conflict resolution winner/loser), citation (precedent cited 3+ times).';
