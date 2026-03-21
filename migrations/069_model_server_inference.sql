-- 069: Extend model generated column to include server-inferred values.
--
-- Background: the model field was only extracted from client-reported
-- agent_context ('client'->>'model' or flat ->>'model'). With server-side
-- inference (X-Model header for HTTP, tool-name mapping for MCP), the
-- inferred model is stored under 'server'->>'model'. The extraction chain
-- becomes: explicit client > server-inferred > legacy flat key.
--
-- Postgres requires DROP + ADD for generated column expression changes.

ALTER TABLE decisions DROP COLUMN IF EXISTS model;

ALTER TABLE decisions
  ADD COLUMN model TEXT GENERATED ALWAYS AS (
    COALESCE(
      agent_context->'client'->>'model',
      agent_context->'server'->>'model',
      agent_context->>'model'
    )
  ) STORED;

-- Recreate the index (dropped with the column).
CREATE INDEX IF NOT EXISTS idx_decisions_model ON decisions (model) WHERE model IS NOT NULL;
