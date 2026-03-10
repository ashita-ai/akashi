-- 066: Add indexes to improve query performance for conflict filtering and analytics.

-- Covers conflict_kind filter in ListConflicts and ListConflictGroups queries.
-- The existing idx_scored_conflicts_org only covers (org_id); adding conflict_kind
-- avoids a filter scan when the UI filters by kind.
CREATE INDEX IF NOT EXISTS idx_scored_conflicts_org_kind
    ON scored_conflicts(org_id, conflict_kind);

-- Covers the analytics time-range queries that also filter by status for aggregation.
-- The existing idx_scored_conflicts_detected is (org_id, detected_at DESC) without status,
-- so GROUP BY status within a time window still requires a re-scan.
CREATE INDEX IF NOT EXISTS idx_scored_conflicts_org_detected_status
    ON scored_conflicts(org_id, detected_at DESC, status);

-- Covers resolved_at subquery in the analytics trend (daily resolved count).
-- Without this, the resolved-count CTE does a sequential scan filtered by resolved_at.
CREATE INDEX IF NOT EXISTS idx_scored_conflicts_org_resolved
    ON scored_conflicts(org_id, resolved_at DESC) WHERE resolved_at IS NOT NULL;
