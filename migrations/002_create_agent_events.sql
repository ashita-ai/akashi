-- 002_create_agent_events.sql
-- Append-only event log. Source of truth. Never mutated or deleted.
-- TimescaleDB hypertable for automatic partitioning and compression.

CREATE TABLE agent_events (
    id              UUID NOT NULL DEFAULT gen_random_uuid(),
    run_id          UUID NOT NULL,
    event_type      TEXT NOT NULL,
    sequence_num    BIGINT NOT NULL,
    occurred_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    agent_id        TEXT NOT NULL,
    payload         JSONB NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (id, occurred_at)
);

-- Convert to TimescaleDB hypertable partitioned by occurred_at.
-- FK constraints FROM hypertables are not supported by TimescaleDB,
-- so run_id integrity is enforced at the application layer.
SELECT create_hypertable('agent_events', 'occurred_at');

CREATE INDEX idx_agent_events_run_id ON agent_events(run_id, sequence_num);
CREATE INDEX idx_agent_events_type ON agent_events(event_type, occurred_at DESC);
CREATE INDEX idx_agent_events_agent_id ON agent_events(agent_id, occurred_at DESC);
CREATE INDEX idx_agent_events_payload ON agent_events USING GIN (payload);
