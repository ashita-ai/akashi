-- 071: Add typed metrics column to evidence for structured quantitative data.
--
-- source_type = 'metrics' entries store key→float pairs in this column,
-- allowing the server to query and compare numeric evidence natively
-- instead of losing fidelity in TEXT serialization.

ALTER TABLE evidence
    ADD COLUMN metrics JSONB;

-- Helper function: returns true when every value in a JSONB object is numeric.
-- Used by the CHECK constraint below. PostgreSQL disallows subqueries in CHECK
-- constraints, so we wrap the validation in an immutable function instead.
CREATE OR REPLACE FUNCTION jsonb_values_all_numeric(obj JSONB) RETURNS BOOLEAN
    LANGUAGE sql IMMUTABLE STRICT AS $$
    SELECT NOT EXISTS (
        SELECT 1
        FROM jsonb_each(obj) AS kv
        WHERE jsonb_typeof(kv.value) <> 'number'
    );
$$;

-- Ensure every value in the metrics object is numeric (integer or float).
-- The constraint fires only when metrics IS NOT NULL.
ALTER TABLE evidence
    ADD CONSTRAINT evidence_metrics_values_numeric
    CHECK (metrics IS NULL OR jsonb_values_all_numeric(metrics));

-- Partial index for efficient lookups on evidence with metrics.
CREATE INDEX idx_evidence_metrics ON evidence USING gin (metrics)
    WHERE metrics IS NOT NULL;
