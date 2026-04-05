-- 101: Rename indexes left over from quality_score → completeness_score rename
-- (migration 050), and drop redundant idx_scored_conflicts_org which is
-- subsumed by every other org-prefixed index on scored_conflicts.

ALTER INDEX idx_decisions_quality RENAME TO idx_decisions_completeness;
ALTER INDEX idx_decisions_type_quality RENAME TO idx_decisions_type_completeness;
DROP INDEX idx_scored_conflicts_org;
