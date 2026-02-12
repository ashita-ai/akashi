-- 025_review_fixes.sql
-- Performance indexes identified in 8th comprehensive code review.

-- Common-path index: decisions filtered by org + time, used by recent/query/check.
-- Covers the most frequent query pattern (active decisions for an org, newest first).
CREATE INDEX IF NOT EXISTS idx_decisions_org_current_time
    ON decisions (org_id, valid_from DESC) WHERE valid_to IS NULL;

-- Export cursor-based keyset pagination index.
-- Supports the (valid_from, id) > ($cursor) pattern used by ExportDecisionsCursor.
CREATE INDEX IF NOT EXISTS idx_decisions_org_export
    ON decisions (org_id, created_at, id) WHERE valid_to IS NULL;
