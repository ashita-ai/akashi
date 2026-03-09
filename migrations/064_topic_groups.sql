-- 064: Topic-based conflict grouping — split coarse agent-pair groups by semantic topic.
--
-- Problem: the existing grouping key (org_id, agent_a, agent_b, conflict_kind, decision_type)
-- is too coarse. Two agents with multiple unrelated disagreements (e.g. "caching strategy" and
-- "API protocol") are lumped into one mega-group, hiding distinct topics.
--
-- Solution: allow multiple groups per agent-pair × decision-type by dropping the UNIQUE
-- constraint. Group assignment is now driven by outcome embedding similarity (>0.85 cosine)
-- in the application layer. A new group_topic column stores a human-readable label for UI
-- display, derived from the representative conflict's outcome text.

-- Add topic label column.
ALTER TABLE conflict_groups ADD COLUMN group_topic TEXT;

-- Drop the old UNIQUE constraint that enforced one group per agent-pair × decision-type.
-- PostgreSQL truncates auto-generated constraint names to 63 characters, so we use a
-- DO block to find and drop it by looking up the unique index on these columns.
DO $$
DECLARE
    constraint_name TEXT;
BEGIN
    SELECT tc.constraint_name INTO constraint_name
    FROM information_schema.table_constraints tc
    WHERE tc.table_name = 'conflict_groups'
      AND tc.constraint_type = 'UNIQUE'
      AND tc.constraint_name LIKE 'conflict_groups_org_id_agent_a_agent_b_conflict_kind_decisi%';
    IF constraint_name IS NOT NULL THEN
        EXECUTE format('ALTER TABLE conflict_groups DROP CONSTRAINT %I', constraint_name);
    END IF;
END $$;

-- Add a non-unique lookup index for the application-layer group finder.
CREATE INDEX idx_conflict_groups_lookup
    ON conflict_groups (org_id, agent_a, agent_b, conflict_kind, decision_type);

-- Backfill group_topic from the representative conflict's outcome_a (first 120 chars).
-- Uses a subquery with DISTINCT ON to pick the representative per group.
UPDATE conflict_groups cg
SET group_topic = LEFT(sub.outcome_a, 120)
FROM (
    SELECT DISTINCT ON (sc.group_id)
        sc.group_id,
        sc.outcome_a
    FROM scored_conflicts sc
    WHERE sc.group_id IS NOT NULL
    ORDER BY sc.group_id,
        CASE WHEN sc.status IN ('open', 'acknowledged') THEN 0 ELSE 1 END,
        sc.significance DESC NULLS LAST,
        sc.detected_at DESC
) sub
WHERE sub.group_id = cg.id;
