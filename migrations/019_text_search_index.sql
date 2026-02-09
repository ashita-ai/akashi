-- 019_text_search_index.sql
-- Add trigram index for text search fallback (ILIKE queries).
-- pg_trgm enables efficient ILIKE/SIMILAR TO pattern matching without
-- full sequential scans. Used by SearchDecisionsByText when Qdrant is
-- unavailable or the embedding provider is noop.

CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- GIN trigram index on outcome (the primary text search target).
CREATE INDEX IF NOT EXISTS idx_decisions_outcome_trgm
    ON decisions USING GIN (outcome gin_trgm_ops);

-- GIN trigram index on decision_type for type-ahead/partial match.
CREATE INDEX IF NOT EXISTS idx_decisions_type_trgm
    ON decisions USING GIN (decision_type gin_trgm_ops);
