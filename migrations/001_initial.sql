-- 001_initial.sql
-- Akashi decision trace layer: complete initial schema.
-- PostgreSQL 18+ with pgvector, TimescaleDB, and pg_trgm extensions.

-- ============================================================================
-- Extensions
-- ============================================================================

CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS timescaledb;
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- ============================================================================
-- Organizations
-- ============================================================================

CREATE TABLE organizations (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL,
    slug        TEXT NOT NULL UNIQUE,
    plan        TEXT NOT NULL DEFAULT 'oss',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_organizations_slug ON organizations (slug);

-- Default organization for single-tenant or bootstrap use.
INSERT INTO organizations (id, name, slug, plan, created_at, updated_at)
VALUES (
    '00000000-0000-0000-0000-000000000000',
    'Default',
    'default',
    'oss',
    now(),
    now()
) ON CONFLICT DO NOTHING;

-- ============================================================================
-- Agents
-- ============================================================================

CREATE TABLE agents (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_id     TEXT NOT NULL,
    name         TEXT NOT NULL,
    role         TEXT NOT NULL DEFAULT 'agent'
                 CHECK (role IN ('platform_admin', 'org_owner', 'admin', 'agent', 'reader')),
    api_key_hash TEXT,
    metadata     JSONB NOT NULL DEFAULT '{}',
    org_id       UUID NOT NULL REFERENCES organizations(id),
    tags         TEXT[] NOT NULL DEFAULT '{}',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX idx_agents_org_agent ON agents (org_id, agent_id);
CREATE INDEX idx_agents_agent_id_global ON agents (agent_id);
CREATE INDEX idx_agents_metadata ON agents USING GIN (metadata);
CREATE INDEX idx_agents_tags ON agents USING GIN (tags);

-- ============================================================================
-- Agent Runs
-- ============================================================================

CREATE TABLE agent_runs (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_id      TEXT NOT NULL,
    trace_id      TEXT,
    parent_run_id UUID REFERENCES agent_runs(id),
    status        TEXT NOT NULL DEFAULT 'running'
                  CHECK (status IN ('running', 'completed', 'failed')),
    started_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at  TIMESTAMPTZ,
    metadata      JSONB NOT NULL DEFAULT '{}',
    org_id        UUID NOT NULL REFERENCES organizations(id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_agent_runs_agent_id ON agent_runs(agent_id);
CREATE INDEX idx_agent_runs_trace_id ON agent_runs(trace_id) WHERE trace_id IS NOT NULL;
CREATE INDEX idx_agent_runs_status ON agent_runs(status);
CREATE INDEX idx_agent_runs_started_at ON agent_runs(started_at DESC);
CREATE INDEX idx_agent_runs_org ON agent_runs (org_id, id);
CREATE INDEX idx_agent_runs_parent_run ON agent_runs (parent_run_id) WHERE parent_run_id IS NOT NULL;

-- ============================================================================
-- Decisions (bi-temporal)
-- ============================================================================

CREATE TABLE decisions (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id           UUID NOT NULL REFERENCES agent_runs(id),
    agent_id         TEXT NOT NULL,
    decision_type    TEXT NOT NULL,
    outcome          TEXT NOT NULL,
    confidence       REAL NOT NULL CHECK (confidence >= 0.0 AND confidence <= 1.0),
    reasoning        TEXT,
    embedding        vector(1024),
    metadata         JSONB NOT NULL DEFAULT '{}',
    valid_from       TIMESTAMPTZ NOT NULL DEFAULT now(),
    valid_to         TIMESTAMPTZ,
    transaction_time TIMESTAMPTZ NOT NULL DEFAULT now(),
    quality_score    REAL DEFAULT 0.0,
    precedent_ref    UUID REFERENCES decisions(id),
    supersedes_id    UUID REFERENCES decisions(id),
    content_hash     TEXT,
    org_id           UUID NOT NULL REFERENCES organizations(id),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_decisions_agent_id ON decisions(agent_id, valid_from DESC);
CREATE INDEX idx_decisions_run_id ON decisions(run_id);
CREATE INDEX idx_decisions_type ON decisions(decision_type, valid_from DESC);
CREATE INDEX idx_decisions_confidence ON decisions(confidence DESC);
CREATE INDEX idx_decisions_metadata ON decisions USING GIN (metadata);
CREATE INDEX idx_decisions_quality ON decisions (quality_score DESC);
CREATE INDEX idx_decisions_type_quality ON decisions (decision_type, quality_score DESC);
CREATE INDEX idx_decisions_precedent_ref ON decisions (precedent_ref) WHERE precedent_ref IS NOT NULL;
CREATE INDEX idx_decisions_temporal
    ON decisions(transaction_time, valid_from DESC)
    WHERE valid_to IS NULL;
CREATE INDEX idx_decisions_check_order
    ON decisions(decision_type, valid_from DESC, quality_score DESC)
    WHERE valid_to IS NULL;
CREATE INDEX idx_decisions_org_agent_current
    ON decisions (org_id, agent_id, valid_from DESC)
    WHERE valid_to IS NULL;
CREATE INDEX idx_decisions_org_type
    ON decisions (org_id, decision_type, valid_from DESC);
CREATE INDEX idx_decisions_temporal_historical
    ON decisions (org_id, transaction_time, valid_to)
    WHERE valid_to IS NOT NULL;
CREATE INDEX idx_decisions_supersedes ON decisions(supersedes_id) WHERE supersedes_id IS NOT NULL;
CREATE INDEX idx_decisions_content_hash ON decisions(content_hash) WHERE content_hash IS NOT NULL;
CREATE INDEX idx_decisions_embedding
    ON decisions USING hnsw (embedding vector_cosine_ops)
    WITH (m = 16, ef_construction = 64);
CREATE INDEX idx_decisions_outcome_trgm
    ON decisions USING GIN (outcome gin_trgm_ops);
CREATE INDEX idx_decisions_type_trgm
    ON decisions USING GIN (decision_type gin_trgm_ops);

-- ============================================================================
-- Alternatives
-- ============================================================================

CREATE TABLE alternatives (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    decision_id      UUID NOT NULL REFERENCES decisions(id),
    label            TEXT NOT NULL,
    score            REAL,
    selected         BOOLEAN NOT NULL DEFAULT false,
    rejection_reason TEXT,
    metadata         JSONB NOT NULL DEFAULT '{}',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_alternatives_decision_id ON alternatives(decision_id);
CREATE INDEX idx_alternatives_selected ON alternatives(decision_id) WHERE selected = true;

-- ============================================================================
-- Evidence
-- ============================================================================

CREATE TABLE evidence (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    decision_id     UUID NOT NULL REFERENCES decisions(id),
    source_type     TEXT NOT NULL
                    CHECK (source_type ~ '^[a-z][a-z0-9_]*$'),
    source_uri      TEXT,
    content         TEXT NOT NULL,
    relevance_score REAL,
    embedding       vector(1024),
    metadata        JSONB NOT NULL DEFAULT '{}',
    org_id          UUID NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_evidence_decision_id ON evidence(decision_id);
CREATE INDEX idx_evidence_source_type ON evidence(source_type);
CREATE INDEX idx_evidence_embedding
    ON evidence USING hnsw (embedding vector_cosine_ops)
    WITH (m = 16, ef_construction = 64);
CREATE INDEX idx_evidence_org ON evidence (org_id, decision_id);

-- ============================================================================
-- Agent Events (TimescaleDB hypertable)
-- ============================================================================

CREATE TABLE agent_events (
    id           UUID NOT NULL DEFAULT gen_random_uuid(),
    run_id       UUID NOT NULL,
    event_type   TEXT NOT NULL,
    sequence_num BIGINT NOT NULL,
    occurred_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    agent_id     TEXT NOT NULL,
    payload      JSONB NOT NULL DEFAULT '{}',
    org_id       UUID NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (id, occurred_at)
);

SELECT create_hypertable('agent_events', 'occurred_at', if_not_exists => TRUE);
SELECT set_chunk_time_interval('agent_events', INTERVAL '1 day');

ALTER TABLE agent_events SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'agent_id,run_id',
    timescaledb.compress_orderby = 'occurred_at DESC'
);

SELECT add_compression_policy('agent_events', INTERVAL '7 days');

CREATE INDEX idx_agent_events_run_id ON agent_events(run_id, sequence_num);
CREATE INDEX idx_agent_events_type ON agent_events(event_type, occurred_at DESC);
CREATE INDEX idx_agent_events_agent_id ON agent_events(agent_id, occurred_at DESC);
CREATE INDEX idx_agent_events_payload ON agent_events USING GIN (payload);
CREATE INDEX idx_agent_events_org ON agent_events (org_id);
CREATE INDEX idx_agent_events_org_type ON agent_events (org_id, event_type, occurred_at DESC);
CREATE UNIQUE INDEX idx_agent_events_run_seq_unique
    ON agent_events(run_id, sequence_num, occurred_at);

-- Global monotonic sequence for event ordering.
CREATE SEQUENCE event_sequence_num_seq START WITH 1;

-- ============================================================================
-- Access Grants
-- ============================================================================

CREATE TABLE access_grants (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    grantor_id    UUID NOT NULL REFERENCES agents(id),
    grantee_id    UUID NOT NULL REFERENCES agents(id),
    resource_type TEXT NOT NULL
                  CHECK (resource_type IN ('agent_traces', 'decision', 'run')),
    resource_id   TEXT,
    permission    TEXT NOT NULL
                  CHECK (permission IN ('read', 'write')),
    granted_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at    TIMESTAMPTZ,
    org_id        UUID NOT NULL REFERENCES organizations(id),
    UNIQUE(grantee_id, resource_type, resource_id, permission)
);

CREATE INDEX idx_access_grants_grantee ON access_grants(grantee_id, resource_type);
CREATE INDEX idx_access_grants_org ON access_grants (org_id, grantee_id);
CREATE INDEX idx_access_grants_expires ON access_grants (expires_at) WHERE expires_at IS NOT NULL;

-- ============================================================================
-- Search Outbox (async Qdrant sync)
-- ============================================================================

CREATE TABLE search_outbox (
    id           BIGSERIAL PRIMARY KEY,
    decision_id  UUID NOT NULL,
    org_id       UUID NOT NULL,
    operation    TEXT NOT NULL CHECK (operation IN ('upsert', 'delete')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    attempts     INT NOT NULL DEFAULT 0,
    last_error   TEXT,
    locked_until TIMESTAMPTZ
);

CREATE INDEX idx_search_outbox_pending
    ON search_outbox (created_at ASC)
    WHERE locked_until IS NULL;
CREATE UNIQUE INDEX idx_search_outbox_decision_op
    ON search_outbox (decision_id, operation);
CREATE INDEX idx_search_outbox_org ON search_outbox (org_id);

-- ============================================================================
-- Integrity Proofs (Merkle tree batch hashes)
-- ============================================================================

CREATE TABLE integrity_proofs (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id         UUID NOT NULL REFERENCES organizations(id),
    batch_start    TIMESTAMPTZ NOT NULL,
    batch_end      TIMESTAMPTZ NOT NULL,
    decision_count INTEGER NOT NULL,
    root_hash      TEXT NOT NULL,
    previous_root  TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_integrity_proofs_org_time ON integrity_proofs(org_id, created_at DESC);

-- ============================================================================
-- Views
-- ============================================================================

-- Current (non-superseded) decisions.
CREATE VIEW current_decisions AS
SELECT * FROM decisions
WHERE valid_to IS NULL
ORDER BY valid_from DESC;

-- Conflict detection: same decision_type, different agents, different outcomes,
-- both currently valid, within 1 hour of each other, scoped to same org.
CREATE MATERIALIZED VIEW decision_conflicts AS
SELECT
    d1.id AS decision_a_id,
    d2.id AS decision_b_id,
    d1.org_id,
    d1.agent_id AS agent_a,
    d2.agent_id AS agent_b,
    d1.run_id AS run_a,
    d2.run_id AS run_b,
    d1.decision_type,
    d1.outcome AS outcome_a,
    d2.outcome AS outcome_b,
    d1.confidence AS confidence_a,
    d2.confidence AS confidence_b,
    d1.valid_from AS decided_at_a,
    d2.valid_from AS decided_at_b,
    GREATEST(d1.valid_from, d2.valid_from) AS detected_at
FROM decisions d1
JOIN decisions d2
    ON d1.decision_type = d2.decision_type
    AND d1.org_id = d2.org_id
    AND d1.agent_id != d2.agent_id
    AND d1.outcome != d2.outcome
    AND d1.valid_to IS NULL
    AND d2.valid_to IS NULL
    AND d1.id < d2.id
    AND ABS(EXTRACT(EPOCH FROM (d1.valid_from - d2.valid_from))) < 3600
WITH DATA;

CREATE UNIQUE INDEX idx_decision_conflicts_pair
    ON decision_conflicts(decision_a_id, decision_b_id);

-- Current agent state: latest run and activity summary per agent per org.
CREATE MATERIALIZED VIEW agent_current_state AS
WITH latest_runs AS (
    SELECT DISTINCT ON (agent_id, org_id)
        id, agent_id, org_id, status, started_at
    FROM agent_runs
    ORDER BY agent_id, org_id, started_at DESC
),
decision_counts AS (
    SELECT agent_id, org_id, COUNT(*) AS active_decisions
    FROM decisions
    WHERE valid_to IS NULL
    GROUP BY agent_id, org_id
),
event_stats AS (
    SELECT run_id, COUNT(id) AS event_count, MAX(occurred_at) AS last_activity
    FROM agent_events
    GROUP BY run_id
)
SELECT
    lr.agent_id,
    lr.org_id,
    lr.id AS latest_run_id,
    lr.status AS run_status,
    lr.started_at,
    COALESCE(es.event_count, 0) AS event_count,
    es.last_activity,
    COALESCE(dc.active_decisions, 0) AS active_decisions
FROM latest_runs lr
LEFT JOIN event_stats es ON es.run_id = lr.id
LEFT JOIN decision_counts dc ON dc.agent_id = lr.agent_id AND dc.org_id = lr.org_id
WITH DATA;

CREATE UNIQUE INDEX idx_agent_current_state_agent_org
    ON agent_current_state (agent_id, org_id);
