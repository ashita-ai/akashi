-- 027: Semantic conflict detection (Option B: outcome-only embedding).
--
-- outcome_embedding: Embedding of outcome text only, for precise outcome comparison.
-- Same dimensions as embedding. Populated at trace time and backfilled.
--
-- scored_conflicts: Semantically scored conflicts. No time or type constraints.
-- significance = topic_similarity * outcome_divergence. Event-driven population.
--
-- decision_conflicts mat view: Dropped. Replaced by scored_conflicts.

-- Add outcome embedding column.
ALTER TABLE decisions ADD COLUMN IF NOT EXISTS outcome_embedding vector(1024);

-- HNSW index for outcome similarity (used by conflict scorer).
CREATE INDEX IF NOT EXISTS idx_decisions_outcome_embedding
    ON decisions USING hnsw (outcome_embedding vector_cosine_ops)
    WITH (m = 16, ef_construction = 64)
    WHERE outcome_embedding IS NOT NULL;

-- scored_conflicts: semantic conflict pairs with scores.
CREATE TABLE IF NOT EXISTS scored_conflicts (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    decision_a_id      UUID NOT NULL REFERENCES decisions(id),
    decision_b_id      UUID NOT NULL REFERENCES decisions(id),
    org_id             UUID NOT NULL REFERENCES organizations(id),
    conflict_kind      TEXT NOT NULL CHECK (conflict_kind IN ('cross_agent', 'self_contradiction')),
    agent_a           TEXT NOT NULL,
    agent_b           TEXT NOT NULL,
    decision_type_a    TEXT NOT NULL,
    decision_type_b    TEXT NOT NULL,
    outcome_a          TEXT NOT NULL,
    outcome_b          TEXT NOT NULL,
    topic_similarity   REAL NOT NULL,
    outcome_divergence REAL NOT NULL,
    significance       REAL NOT NULL,
    scoring_method     TEXT NOT NULL DEFAULT 'embedding' CHECK (scoring_method IN ('embedding', 'text')),
    detected_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(decision_a_id, decision_b_id)
);

CREATE INDEX IF NOT EXISTS idx_scored_conflicts_org_sig ON scored_conflicts(org_id, significance DESC);
CREATE INDEX IF NOT EXISTS idx_scored_conflicts_detected ON scored_conflicts(org_id, detected_at DESC);
CREATE INDEX IF NOT EXISTS idx_scored_conflicts_org ON scored_conflicts(org_id);

-- Drop the lexical mat view; scored_conflicts is the source of truth.
DROP MATERIALIZED VIEW IF EXISTS decision_conflicts;
