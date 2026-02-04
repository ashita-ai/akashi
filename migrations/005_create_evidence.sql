-- 005_create_evidence.sql
-- Evidence that supported a decision. Includes provenance tracking. Immutable.

CREATE TABLE evidence (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    decision_id     UUID NOT NULL REFERENCES decisions(id),
    source_type     TEXT NOT NULL
                    CHECK (source_type IN ('document', 'api_response', 'agent_output', 'user_input', 'search_result')),
    source_uri      TEXT,
    content         TEXT NOT NULL,
    relevance_score REAL,
    embedding       vector(1536),
    metadata        JSONB NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_evidence_decision_id ON evidence(decision_id);
CREATE INDEX idx_evidence_source_type ON evidence(source_type);
CREATE INDEX idx_evidence_embedding ON evidence USING hnsw (embedding vector_cosine_ops)
    WITH (m = 16, ef_construction = 64);
