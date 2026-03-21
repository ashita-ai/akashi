-- 071: Drop unused score and selected columns from the alternatives table.
-- These columns were never populated by the MCP trace workflow and are always
-- NULL / false. The selected option is captured in the decision's outcome field;
-- alternatives are inherently rejected options that don't need a score.

DROP INDEX IF EXISTS idx_alternatives_selected;
ALTER TABLE alternatives DROP COLUMN IF EXISTS score;
ALTER TABLE alternatives DROP COLUMN IF EXISTS selected;
