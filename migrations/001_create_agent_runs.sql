-- 001_create_agent_runs.sql
-- Top-level execution context. Corresponds to an OTEL trace. Immutable once created.

CREATE TABLE agent_runs (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_id        TEXT NOT NULL,
    trace_id        TEXT,
    parent_run_id   UUID REFERENCES agent_runs(id),
    status          TEXT NOT NULL DEFAULT 'running'
                    CHECK (status IN ('running', 'completed', 'failed')),
    started_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at    TIMESTAMPTZ,
    metadata        JSONB NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_agent_runs_agent_id ON agent_runs(agent_id);
CREATE INDEX idx_agent_runs_trace_id ON agent_runs(trace_id) WHERE trace_id IS NOT NULL;
CREATE INDEX idx_agent_runs_status ON agent_runs(status);
CREATE INDEX idx_agent_runs_started_at ON agent_runs(started_at DESC);
