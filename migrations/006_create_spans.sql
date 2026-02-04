-- 006_create_spans.sql
-- OTEL-compatible hierarchical trace structure. Immutable.

CREATE TABLE spans (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id          UUID NOT NULL REFERENCES agent_runs(id),
    parent_span_id  UUID REFERENCES spans(id),
    trace_id        TEXT,
    span_id         TEXT,
    name            TEXT NOT NULL,
    kind            TEXT NOT NULL DEFAULT 'internal'
                    CHECK (kind IN ('internal', 'client', 'server', 'producer', 'consumer')),
    started_at      TIMESTAMPTZ NOT NULL,
    ended_at        TIMESTAMPTZ,
    status          TEXT NOT NULL DEFAULT 'ok'
                    CHECK (status IN ('ok', 'error', 'unset')),
    attributes      JSONB NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_spans_run_id ON spans(run_id);
CREATE INDEX idx_spans_trace_id ON spans(trace_id) WHERE trace_id IS NOT NULL;
CREATE INDEX idx_spans_parent ON spans(parent_span_id) WHERE parent_span_id IS NOT NULL;
