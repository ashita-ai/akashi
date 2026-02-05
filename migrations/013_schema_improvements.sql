-- 013_schema_improvements.sql
-- Schema improvements: embedding dimensions, temporal indexes, sequence safety.

-- 1. Update embedding columns from vector(1536) to vector(1024) to match the
--    Ollama default. Idempotent: checks current type before altering.

-- Drop HNSW indexes first (they depend on the fixed dimension).
DROP INDEX IF EXISTS idx_decisions_embedding;
DROP INDEX IF EXISTS idx_evidence_embedding;

DO $$
DECLARE
    current_type text;
BEGIN
    -- Check decisions.embedding type.
    SELECT format_type(atttypid, atttypmod) INTO current_type
    FROM pg_attribute
    WHERE attrelid = 'decisions'::regclass
      AND attname = 'embedding'
      AND NOT attisdropped;

    IF current_type IS DISTINCT FROM 'vector(1024)' THEN
        -- Drop views that depend on the decisions table before altering columns.
        DROP VIEW IF EXISTS current_decisions;
        DROP MATERIALIZED VIEW IF EXISTS decision_conflicts;
        DROP MATERIALIZED VIEW IF EXISTS agent_current_state;

        -- Clear embeddings that won't fit the new dimension (pre-1.0, safe).
        UPDATE decisions SET embedding = NULL WHERE embedding IS NOT NULL;
        ALTER TABLE decisions ALTER COLUMN embedding TYPE vector(1024);
        RAISE NOTICE 'decisions.embedding changed to vector(1024)';

        -- Recreate views.
        CREATE VIEW current_decisions AS
        SELECT * FROM decisions
        WHERE valid_to IS NULL
        ORDER BY valid_from DESC;

        CREATE MATERIALIZED VIEW decision_conflicts AS
        SELECT
            d1.id AS decision_a_id,
            d2.id AS decision_b_id,
            d1.agent_id AS agent_a,
            d2.agent_id AS agent_b,
            d1.run_id AS run_a,
            d2.run_id AS run_b,
            d1.decision_type,
            d1.outcome AS outcome_a,
            d2.outcome AS outcome_b,
            d1.confidence AS confidence_a,
            d2.confidence AS confidence_b,
            d1.valid_from AS decided_at_a,
            d2.valid_from AS decided_at_b,
            GREATEST(d1.valid_from, d2.valid_from) AS detected_at
        FROM decisions d1
        JOIN decisions d2
            ON d1.decision_type = d2.decision_type
            AND d1.agent_id != d2.agent_id
            AND d1.outcome != d2.outcome
            AND d1.valid_to IS NULL
            AND d2.valid_to IS NULL
            AND d1.id < d2.id
            AND ABS(EXTRACT(EPOCH FROM (d1.valid_from - d2.valid_from))) < 3600
        WITH DATA;
        CREATE UNIQUE INDEX idx_decision_conflicts_pair ON decision_conflicts(decision_a_id, decision_b_id);

        CREATE MATERIALIZED VIEW agent_current_state AS
        SELECT
            ar.agent_id,
            ar.id AS latest_run_id,
            ar.status AS run_status,
            ar.started_at,
            COUNT(ae.id) AS event_count,
            MAX(ae.occurred_at) AS last_activity,
            (SELECT COUNT(*) FROM decisions d WHERE d.agent_id = ar.agent_id AND d.valid_to IS NULL) AS active_decisions
        FROM agent_runs ar
        LEFT JOIN agent_events ae ON ae.run_id = ar.id
        WHERE ar.started_at = (
            SELECT MAX(started_at) FROM agent_runs WHERE agent_id = ar.agent_id
        )
        GROUP BY ar.agent_id, ar.id, ar.status, ar.started_at
        WITH DATA;
    END IF;

    -- Check evidence.embedding type.
    SELECT format_type(atttypid, atttypmod) INTO current_type
    FROM pg_attribute
    WHERE attrelid = 'evidence'::regclass
      AND attname = 'embedding'
      AND NOT attisdropped;

    IF current_type IS DISTINCT FROM 'vector(1024)' THEN
        UPDATE evidence SET embedding = NULL WHERE embedding IS NOT NULL;
        ALTER TABLE evidence ALTER COLUMN embedding TYPE vector(1024);
        RAISE NOTICE 'evidence.embedding changed to vector(1024)';
    END IF;
END $$;

-- Recreate HNSW indexes with the correct dimension.
CREATE INDEX IF NOT EXISTS idx_decisions_embedding
    ON decisions USING hnsw (embedding vector_cosine_ops)
    WITH (m = 16, ef_construction = 64);

CREATE INDEX IF NOT EXISTS idx_evidence_embedding
    ON evidence USING hnsw (embedding vector_cosine_ops)
    WITH (m = 16, ef_construction = 64);

-- 2. Composite temporal query index.
-- Accelerates bi-temporal point-in-time queries:
--   WHERE transaction_time <= $1 AND (valid_to IS NULL OR valid_to > $2)
CREATE INDEX IF NOT EXISTS idx_decisions_temporal
    ON decisions(transaction_time, valid_from DESC)
    WHERE valid_to IS NULL;

-- 3. Composite index for precedent lookup ordering.
-- Used by HandleCheck: ORDER BY valid_from DESC, quality_score DESC.
CREATE INDEX IF NOT EXISTS idx_decisions_check_order
    ON decisions(decision_type, valid_from DESC, quality_score DESC)
    WHERE valid_to IS NULL;

-- 4. Unique constraint on event sequences within a run.
-- Detects sequence number collisions under horizontal scaling.
-- Includes occurred_at as required by TimescaleDB unique indexes on hypertables.
CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_events_run_seq_unique
    ON agent_events(run_id, sequence_num, occurred_at);

-- 5. Global event sequence.
-- Replaces SELECT MAX(sequence_num)+1 (race condition under concurrent writes)
-- with a Postgres SEQUENCE. Each event gets a globally unique, monotonically
-- increasing sequence_num. Within a run, events are still ordered by their
-- sequence_num, which increases monotonically since the buffer assigns them
-- in order.
DO $$
DECLARE
    max_seq bigint;
BEGIN
    -- Only create if the sequence doesn't exist yet.
    IF NOT EXISTS (
        SELECT 1 FROM pg_sequences WHERE schemaname = 'public' AND sequencename = 'event_sequence_num_seq'
    ) THEN
        SELECT COALESCE(MAX(sequence_num), 0) INTO max_seq FROM agent_events;
        EXECUTE format('CREATE SEQUENCE event_sequence_num_seq START WITH %s', max_seq + 1);
        RAISE NOTICE 'event_sequence_num_seq created, starting at %', max_seq + 1;
    END IF;
END $$;
