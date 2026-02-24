-- 052: Rename generated column repo → project on decisions.
--
-- "repo" was too narrow — the field scopes decisions to a logical project or
-- application, not necessarily a git repository. LangChain agents, CrewAI crews,
-- and batch pipelines don't have repos in the traditional sense. "project" is
-- the correct semantic.
--
-- The new generated column prefers agent_context["client"]["project"] and
-- agent_context["server"]["project"] (new write path), then falls back to the
-- old "repo" key variants so existing decisions are not lost.
--
-- No write-path migration required: the column is computed automatically from
-- agent_context at INSERT time and backfilled for all existing rows during ALTER.

-- Add the new generated column.
ALTER TABLE decisions
  ADD COLUMN IF NOT EXISTS project TEXT GENERATED ALWAYS AS (
    COALESCE(
      agent_context->'client'->>'project',
      agent_context->'server'->>'project',
      agent_context->>'project',
      agent_context->'server'->>'repo',
      agent_context->'client'->>'repo',
      agent_context->>'repo'
    )
  ) STORED;

-- Create the new column index.
CREATE INDEX IF NOT EXISTS idx_decisions_project ON decisions (project) WHERE project IS NOT NULL;

-- Drop the old column and its index (safe: project column covers all old values).
DROP INDEX IF EXISTS idx_decisions_repo;
ALTER TABLE decisions DROP COLUMN IF EXISTS repo;
