-- 054: Conflict grouping — collapse N×M pairwise conflicts into one logical group.
--
-- Problem: conflict detection runs pairwise across all decisions. When agents A and B
-- each make N and M decisions respectively on the same topic, up to N×M conflict rows
-- are created — all expressing the same disagreement. The UI and MCP tool surface every
-- pair as a distinct, equally-notable conflict, making the signal indistinguishable from
-- noise (e.g. 11 conflicts from one demo run, 9 of which are the same two agents
-- disagreeing about architecture).
--
-- Solution: conflict_groups — one row per (org, agent-pair, conflict-kind, decision-type).
-- scored_conflicts.group_id links each pairwise conflict to its canonical group.
-- InsertScoredConflict upserts the group atomically (CTE) before inserting the pair.
-- The group collapses display noise: the UI and MCP show one entry per logical disagreement
-- instead of N×M entries.
--
-- Key design choices:
--   - agent_a = LEAST(agent_a, agent_b), agent_b = GREATEST(agent_a, agent_b) — normalized
--     agent pair ordering mirrors the decision ordering in scored_conflicts.
--   - conflict_kind is part of the key: self_contradiction (same agent) groups separately
--     from cross_agent conflicts even for the same decision_type.
--   - decision_type is the final dimension: architecture vs security conflicts stay separate.
--   - group_id is nullable: existing rows without a group are valid (pre-migration state).
--     All new inserts via InsertScoredConflict always set group_id.
--   - The representative conflict (highest significance) is derived at query time via
--     LATERAL JOIN — no mutable pointer to maintain.

CREATE TABLE conflict_groups (
    id                UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id            UUID        NOT NULL REFERENCES organizations(id),
    -- Normalized agent pair: agent_a = LEAST, agent_b = GREATEST.
    -- For self-contradictions, both are the same agent ID.
    agent_a           TEXT        NOT NULL,
    agent_b           TEXT        NOT NULL,
    conflict_kind     TEXT        NOT NULL CHECK (conflict_kind IN ('cross_agent', 'self_contradiction')),
    decision_type     TEXT        NOT NULL,
    first_detected_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_detected_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(org_id, agent_a, agent_b, conflict_kind, decision_type)
);

CREATE INDEX idx_conflict_groups_org ON conflict_groups(org_id, last_detected_at DESC);

-- Wire group_id onto scored_conflicts.
ALTER TABLE scored_conflicts
    ADD COLUMN group_id UUID REFERENCES conflict_groups(id) ON DELETE SET NULL;

CREATE INDEX idx_scored_conflicts_group ON scored_conflicts(group_id)
    WHERE group_id IS NOT NULL;

-- Backfill: create one conflict_groups row per (org, agent-pair, conflict-kind, decision-type)
-- from the existing scored_conflicts rows. Uses the same normalization (LEAST/GREATEST)
-- that InsertScoredConflict applies going forward.
INSERT INTO conflict_groups
    (org_id, agent_a, agent_b, conflict_kind, decision_type, first_detected_at, last_detected_at)
SELECT
    org_id,
    LEAST(agent_a, agent_b),
    GREATEST(agent_a, agent_b),
    conflict_kind,
    decision_type_a,
    MIN(detected_at),
    MAX(detected_at)
FROM scored_conflicts
GROUP BY org_id, LEAST(agent_a, agent_b), GREATEST(agent_a, agent_b), conflict_kind, decision_type_a
ON CONFLICT DO NOTHING;

-- Backfill: set group_id on all existing scored_conflicts rows.
UPDATE scored_conflicts sc
SET group_id = cg.id
FROM conflict_groups cg
WHERE sc.org_id            = cg.org_id
  AND LEAST(sc.agent_a, sc.agent_b)    = cg.agent_a
  AND GREATEST(sc.agent_a, sc.agent_b) = cg.agent_b
  AND sc.conflict_kind     = cg.conflict_kind
  AND sc.decision_type_a   = cg.decision_type;
