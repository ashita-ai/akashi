-- 010_create_hypertable_and_compression.sql
-- TimescaleDB-specific configuration for agent_events.
-- Note: create_hypertable is called in 002 during table creation.
-- This migration configures chunk interval and compression policies.

-- Set chunk interval to 1 day (adjust based on ingestion rate).
SELECT set_chunk_time_interval('agent_events', INTERVAL '1 day');

-- Enable compression on agent_events.
ALTER TABLE agent_events SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'agent_id,run_id',
    timescaledb.compress_orderby = 'occurred_at DESC'
);

-- Compress chunks older than 7 days.
SELECT add_compression_policy('agent_events', INTERVAL '7 days');
