-- 095: Fix workspace directory names that leaked into the project field.
--
-- Bug: inferProjectFromRootsWithGit fell back to filepath.Base(path) when
-- git detection failed (e.g. server running remotely). For Conductor
-- workspace paths like /Users/.../conductor/workspaces/akashi/riyadh-v1,
-- this returned "riyadh-v1" (the workspace name) instead of "akashi"
-- (the repo name). The server then:
--   1. Overrode the correct client-submitted project with the workspace name
--   2. Stored the original correct value in client.project_submitted
--   3. Auto-created poisoned alias mappings (correct_name → workspace_name)
--
-- This migration fixes both the decisions and the alias table.

-- Step 1: Delete all system:auto-alias entries. These were created by the
-- buggy code and cannot be trusted — some map correct names to workspace
-- names (inverted). The fixed code will recreate correct aliases naturally.
DELETE FROM project_links
WHERE link_type = 'alias'
  AND created_by = 'system:auto-alias';

-- Step 2: Fix decisions where project_submitted exists and is a known
-- canonical project name (appears in at least one decision without an
-- override). These are cases where the server wrongly overrode a correct
-- client value with a workspace name.
--
-- Strategy: if project_submitted appears as a "natural" project (i.e., in
-- decisions where no override happened), it's the real name. Swap it back.
-- The GENERATED project column auto-updates from agent_context.
WITH canonical_projects AS (
    -- Projects that appear in decisions without any project_submitted override
    -- are trustworthy — they were either server-verified from git or accepted
    -- without conflict.
    SELECT DISTINCT org_id, project
    FROM decisions
    WHERE valid_to IS NULL
      AND project IS NOT NULL
      AND agent_context->'client'->>'project_submitted' IS NULL
),
fixable AS (
    SELECT d.id, d.org_id,
           d.agent_context->'client'->>'project_submitted' AS correct_project,
           d.project AS wrong_project
    FROM decisions d
    JOIN canonical_projects cp
      ON cp.org_id = d.org_id
     AND cp.project = d.agent_context->'client'->>'project_submitted'
    WHERE d.valid_to IS NULL
      AND d.agent_context->'client'->>'project_submitted' IS NOT NULL
      -- Only fix rows where the current project differs from the submitted
      -- value (i.e., an override happened) AND the current project is NOT
      -- itself a canonical name (avoid breaking correct overrides).
      AND d.project != d.agent_context->'client'->>'project_submitted'
      AND NOT EXISTS (
          SELECT 1 FROM canonical_projects cp2
          WHERE cp2.org_id = d.org_id AND cp2.project = d.project
      )
)
UPDATE decisions d
SET agent_context = jsonb_set(
    -- Set client.project to the correct value
    jsonb_set(
        -- Preserve the wrong value under project_submitted for the audit trail
        -- (it's already there, just swap the semantics)
        d.agent_context,
        '{client,project}',
        to_jsonb(f.correct_project)
    ),
    -- Update project_submitted to record what was wrong (the workspace name)
    '{client,project_submitted}',
    to_jsonb(f.wrong_project)
)
FROM fixable f
WHERE d.id = f.id AND d.org_id = f.org_id;

-- Step 3: Clean server.project where it contains the workspace name that
-- was inferred from the basename fallback. Set it to the correct project
-- from client.project (which we just fixed above).
-- Only touch rows where server.project differs from client.project (mismatch
-- indicates the server had the wrong value).
UPDATE decisions d
SET agent_context = jsonb_set(
    d.agent_context,
    '{server,project}',
    to_jsonb(d.agent_context->'client'->>'project')
)
WHERE d.valid_to IS NULL
  AND d.agent_context->'server'->>'project' IS NOT NULL
  AND d.agent_context->'client'->>'project' IS NOT NULL
  AND d.agent_context->'server'->>'project' != d.agent_context->'client'->>'project'
  -- Only fix if server.project is NOT a canonical project (don't break
  -- rows where the server was correct and the client was wrong).
  AND NOT EXISTS (
      SELECT 1 FROM decisions d2
      WHERE d2.org_id = d.org_id
        AND d2.valid_to IS NULL
        AND d2.project = d.agent_context->'server'->>'project'
        AND d2.agent_context->'client'->>'project_submitted' IS NULL
  );
