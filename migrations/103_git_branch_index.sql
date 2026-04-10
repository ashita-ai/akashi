-- 103: Add GIN index on agent_context->'client'->'git_branch' for branch-aware
-- conflict suppression queries. The git_branch field is stored in the existing
-- agent_context JSONB column (no schema change needed). This index accelerates
-- the cross-branch mechanical operation filter in the conflict scorer.

CREATE INDEX IF NOT EXISTS idx_decisions_git_branch
    ON decisions ((agent_context->'client'->>'git_branch'))
    WHERE agent_context->'client'->>'git_branch' IS NOT NULL;
