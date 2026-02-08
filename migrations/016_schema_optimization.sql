-- 016_schema_optimization.sql
-- Schema optimizations: evidence.org_id, mat view rewrite, missing indexes,
-- source_type constraint. Prerequisite for Qdrant vector search (spec 07).

-- =============================================================================
-- 1. Add org_id to evidence table
-- =============================================================================

-- Add column (nullable initially for backfill).
ALTER TABLE evidence
    ADD COLUMN IF NOT EXISTS org_id UUID;

-- Backfill org_id from the parent decision.
UPDATE evidence e
SET org_id = d.org_id
FROM decisions d
WHERE e.decision_id = d.id
  AND e.org_id IS NULL;

-- Make NOT NULL after backfill.
ALTER TABLE evidence ALTER COLUMN org_id SET NOT NULL;

-- Index for org-scoped queries.
CREATE INDEX IF NOT EXISTS idx_evidence_org
    ON evidence (org_id, decision_id);

-- RLS policy for evidence (matches pattern from migration 014).
ALTER TABLE evidence ENABLE ROW LEVEL SECURITY;

CREATE POLICY org_isolation_evidence ON evidence
    FOR ALL TO akashi_app
    USING (org_id = current_setting('app.org_id', true)::uuid)
    WITH CHECK (org_id = current_setting('app.org_id', true)::uuid);

-- =============================================================================
-- 2. Rewrite agent_current_state materialized view
-- =============================================================================

DROP MATERIALIZED VIEW IF EXISTS agent_current_state;

CREATE MATERIALIZED VIEW agent_current_state AS
WITH latest_runs AS (
    SELECT DISTINCT ON (agent_id, org_id)
        id, agent_id, org_id, status, started_at
    FROM agent_runs
    ORDER BY agent_id, org_id, started_at DESC
),
decision_counts AS (
    SELECT agent_id, org_id, COUNT(*) AS active_decisions
    FROM decisions
    WHERE valid_to IS NULL
    GROUP BY agent_id, org_id
),
event_stats AS (
    SELECT run_id, COUNT(id) AS event_count, MAX(occurred_at) AS last_activity
    FROM agent_events
    GROUP BY run_id
)
SELECT
    lr.agent_id,
    lr.org_id,
    lr.id AS latest_run_id,
    lr.status AS run_status,
    lr.started_at,
    COALESCE(es.event_count, 0) AS event_count,
    es.last_activity,
    COALESCE(dc.active_decisions, 0) AS active_decisions
FROM latest_runs lr
LEFT JOIN event_stats es ON es.run_id = lr.id
LEFT JOIN decision_counts dc ON dc.agent_id = lr.agent_id AND dc.org_id = lr.org_id
WITH DATA;

-- Unique index required for REFRESH MATERIALIZED VIEW CONCURRENTLY.
CREATE UNIQUE INDEX idx_agent_current_state_agent_org
    ON agent_current_state (agent_id, org_id);

-- =============================================================================
-- 3. Missing composite indexes
-- =============================================================================

-- 3a. decisions: org_id + decision_type without partial WHERE clause.
-- Covers queries like: SELECT COUNT(*) FROM decisions WHERE org_id = ? AND decision_type = ?
-- (without valid_to IS NULL filter). The existing idx_decisions_org_agent_current is
-- partial and only covers current decisions.
CREATE INDEX IF NOT EXISTS idx_decisions_org_type
    ON decisions (org_id, decision_type, valid_from DESC);

-- 3b. agent_events: org_id + event_type composite.
-- Covers queries filtering by org + event type (e.g., "find all tool_call events in org X").
CREATE INDEX IF NOT EXISTS idx_agent_events_org_type
    ON agent_events (org_id, event_type, occurred_at DESC);

-- 3c. access_grants: expires_at for cleanup queries.
-- Enables efficient deletion of expired grants (background job).
CREATE INDEX IF NOT EXISTS idx_access_grants_expires
    ON access_grants (expires_at)
    WHERE expires_at IS NOT NULL;

-- 3d. agent_runs: parent_run_id for tree traversal.
-- Enables efficient lookup of child runs (nested agent calls).
CREATE INDEX IF NOT EXISTS idx_agent_runs_parent_run
    ON agent_runs (parent_run_id)
    WHERE parent_run_id IS NOT NULL;

-- 3e. decisions: temporal companion for non-NULL valid_to.
-- The existing idx_decisions_temporal only covers WHERE valid_to IS NULL.
-- Bi-temporal queries that check both current and historical decisions
-- (valid_to IS NULL OR valid_to > $asOf) need this companion.
CREATE INDEX IF NOT EXISTS idx_decisions_temporal_historical
    ON decisions (org_id, transaction_time, valid_to)
    WHERE valid_to IS NOT NULL;

-- 3f. agents: metadata GIN index.
-- Enables queries like: SELECT * FROM agents WHERE metadata @> '{"team": "security"}'
CREATE INDEX IF NOT EXISTS idx_agents_metadata
    ON agents USING GIN (metadata);

-- =============================================================================
-- 4. Widen evidence.source_type CHECK constraint
-- =============================================================================

-- Drop the old restrictive CHECK constraint.
DO $$
DECLARE
    constraint_name text;
BEGIN
    SELECT conname INTO constraint_name
    FROM pg_constraint
    WHERE conrelid = 'evidence'::regclass
      AND contype = 'c'
      AND pg_get_constraintdef(oid) LIKE '%source_type%';

    IF constraint_name IS NOT NULL THEN
        EXECUTE format('ALTER TABLE evidence DROP CONSTRAINT %I', constraint_name);
    END IF;
END $$;

-- Add a more permissive CHECK. The constraint validates format (non-empty,
-- lowercase, underscore-separated) rather than enumerating values. New source
-- types can be added without migration.
ALTER TABLE evidence
    ADD CONSTRAINT evidence_source_type_format
    CHECK (source_type ~ '^[a-z][a-z0-9_]*$');
