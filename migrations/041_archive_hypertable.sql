-- 041: Convert agent_events_archive to a TimescaleDB hypertable with compression
-- and retention policies so the archive table doesn't grow without bound.
--
-- Compression: chunks older than 30 days (archived data is rarely queried).
-- Retention: drop chunks older than 365 days. Operators can adjust via:
--   SELECT remove_retention_policy('agent_events_archive');
--   SELECT add_retention_policy('agent_events_archive', INTERVAL '730 days');

SELECT create_hypertable('agent_events_archive', 'occurred_at',
    chunk_time_interval => INTERVAL '7 days',
    if_not_exists => TRUE,
    migrate_data => TRUE
);

ALTER TABLE agent_events_archive SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'org_id',
    timescaledb.compress_orderby = 'occurred_at DESC, sequence_num'
);

SELECT add_compression_policy('agent_events_archive', INTERVAL '30 days', if_not_exists => TRUE);

SELECT add_retention_policy('agent_events_archive', INTERVAL '365 days', if_not_exists => TRUE);
