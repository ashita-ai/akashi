-- Enable extensions
CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS timescaledb;

-- Verify extensions loaded
DO $$
BEGIN
    RAISE NOTICE 'pgvector version: %', (SELECT extversion FROM pg_extension WHERE extname = 'vector');
    RAISE NOTICE 'timescaledb version: %', (SELECT extversion FROM pg_extension WHERE extname = 'timescaledb');
END
$$;
