-- 032: Durable archive table for agent_events retention workflows.
--
-- This enables archive-before-purge lifecycle management for Timescale hypertable
-- data so retention can be applied without losing paper trail records.

CREATE TABLE IF NOT EXISTS agent_events_archive (
    id           UUID NOT NULL,
    run_id       UUID NOT NULL,
    event_type   TEXT NOT NULL,
    sequence_num BIGINT NOT NULL,
    occurred_at  TIMESTAMPTZ NOT NULL,
    agent_id     TEXT NOT NULL,
    payload      JSONB NOT NULL DEFAULT '{}',
    org_id       UUID NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL,
    archived_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (id, occurred_at)
);

CREATE INDEX IF NOT EXISTS idx_agent_events_archive_org_time
    ON agent_events_archive (org_id, occurred_at DESC);

CREATE INDEX IF NOT EXISTS idx_agent_events_archive_run_seq
    ON agent_events_archive (run_id, sequence_num);
