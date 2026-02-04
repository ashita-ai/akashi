-- 003_create_decisions.sql
-- First-class decision entities with bi-temporal modeling.
-- Decisions are created from DecisionMade events and revised via DecisionRevised events.

CREATE TABLE decisions (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id              UUID NOT NULL REFERENCES agent_runs(id),
    agent_id            TEXT NOT NULL,
    decision_type       TEXT NOT NULL,
    outcome             TEXT NOT NULL,
    confidence          REAL NOT NULL CHECK (confidence >= 0.0 AND confidence <= 1.0),
    reasoning           TEXT,
    embedding           vector(1536),
    metadata            JSONB NOT NULL DEFAULT '{}',

    -- Bi-temporal columns
    valid_from          TIMESTAMPTZ NOT NULL DEFAULT now(),
    valid_to            TIMESTAMPTZ,
    transaction_time    TIMESTAMPTZ NOT NULL DEFAULT now(),

    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_decisions_agent_id ON decisions(agent_id, valid_from DESC);
CREATE INDEX idx_decisions_run_id ON decisions(run_id);
CREATE INDEX idx_decisions_type ON decisions(decision_type, valid_from DESC);
CREATE INDEX idx_decisions_confidence ON decisions(confidence DESC);
CREATE INDEX idx_decisions_embedding ON decisions USING hnsw (embedding vector_cosine_ops)
    WITH (m = 16, ef_construction = 64);
CREATE INDEX idx_decisions_metadata ON decisions USING GIN (metadata);

-- Bi-temporal: current decisions view
CREATE VIEW current_decisions AS
SELECT * FROM decisions
WHERE valid_to IS NULL
ORDER BY valid_from DESC;
