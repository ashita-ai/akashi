-- 050: Rename quality_score to completeness_score.
-- quality_score measured field completeness at trace time, not decision correctness.
-- The rename makes this explicit. Values are unchanged; any existing indexes are
-- automatically renamed by PostgreSQL when the column is renamed.
ALTER TABLE decisions RENAME COLUMN quality_score TO completeness_score;
