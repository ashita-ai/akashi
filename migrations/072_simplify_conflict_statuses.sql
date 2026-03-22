-- 072: Simplify conflict statuses to open/resolved/false_positive.
--
-- Drops acknowledged (unused triage marker, identical to open in practice)
-- and wont_fix (conflated false positives with accepted divergence).
--
-- acknowledged → open (non-terminal, semantically equivalent).
-- wont_fix → false_positive (conflicts dismissed without resolution; mapping to
--   resolved would inflate ConflictsNoWinner since those rows have
--   winning_decision_id IS NULL and would match the resolved-no-winner filter).

UPDATE scored_conflicts SET status = 'open' WHERE status = 'acknowledged';
UPDATE scored_conflicts SET status = 'false_positive' WHERE status = 'wont_fix';

ALTER TABLE scored_conflicts DROP CONSTRAINT scored_conflicts_status_check;
ALTER TABLE scored_conflicts ADD CONSTRAINT scored_conflicts_status_check
    CHECK (status IN ('open', 'resolved', 'false_positive'));
