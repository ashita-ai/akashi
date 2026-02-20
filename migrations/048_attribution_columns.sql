-- 048: Promote tool, model, repo to first-class generated columns.
--
-- Background: three expression indexes existed on flat agent_context keys
-- (agent_context->>'tool' etc.) but queries used nested paths
-- (agent_context->'server'->>'tool'). The indexes were never hit — every
-- tool/model/repo filter was a full table scan.
--
-- Fix: add GENERATED ALWAYS AS STORED columns that extract the values from
-- agent_context using the correct COALESCE fallback chain. Postgres computes
-- them at INSERT/UPDATE time and backfills all existing rows during the ALTER.
-- Drop the three broken expression indexes and create proper column indexes.
--
-- No write-path changes required: the columns are computed automatically.

-- Add generated columns.
ALTER TABLE decisions
  ADD COLUMN IF NOT EXISTS tool  TEXT GENERATED ALWAYS AS (
    COALESCE(
      agent_context->'server'->>'tool',
      agent_context->>'tool'
    )
  ) STORED,
  ADD COLUMN IF NOT EXISTS model TEXT GENERATED ALWAYS AS (
    COALESCE(
      agent_context->'client'->>'model',
      agent_context->>'model'
    )
  ) STORED,
  ADD COLUMN IF NOT EXISTS repo  TEXT GENERATED ALWAYS AS (
    COALESCE(
      agent_context->'server'->>'repo',
      agent_context->'server'->>'project',
      agent_context->'client'->>'repo',
      agent_context->>'repo'
    )
  ) STORED;

-- Drop the broken expression indexes.
DROP INDEX IF EXISTS idx_decisions_context_tool;
DROP INDEX IF EXISTS idx_decisions_context_model;
DROP INDEX IF EXISTS idx_decisions_context_repo;

-- Create proper column indexes.
CREATE INDEX IF NOT EXISTS idx_decisions_tool  ON decisions (tool)  WHERE tool  IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_decisions_model ON decisions (model) WHERE model IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_decisions_repo  ON decisions (repo)  WHERE repo  IS NOT NULL;
