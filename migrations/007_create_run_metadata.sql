-- 007_create_run_metadata.sql
-- MLflow-inspired metadata tables for runs.

-- Immutable key-value pairs set at run start
CREATE TABLE run_params (
    id      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id  UUID NOT NULL REFERENCES agent_runs(id),
    key     TEXT NOT NULL,
    value   TEXT NOT NULL,
    UNIQUE(run_id, key)
);

-- Append-only numeric metrics (e.g., token counts, latency measurements)
CREATE TABLE run_metrics (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id      UUID NOT NULL REFERENCES agent_runs(id),
    key         TEXT NOT NULL,
    value       DOUBLE PRECISION NOT NULL,
    step        BIGINT NOT NULL DEFAULT 0,
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Mutable tags for categorization (upsert semantics)
CREATE TABLE run_tags (
    id      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id  UUID NOT NULL REFERENCES agent_runs(id),
    key     TEXT NOT NULL,
    value   TEXT NOT NULL,
    UNIQUE(run_id, key)
);

CREATE INDEX idx_run_params_run_id ON run_params(run_id);
CREATE INDEX idx_run_metrics_run_id ON run_metrics(run_id, key);
CREATE INDEX idx_run_tags_run_id ON run_tags(run_id);
