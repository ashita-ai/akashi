# SPEC-002: Data Model

**Status:** Draft (partially implemented)
**Date:** 2026-02-03
**Depends on:** ADR-002 (Unified PostgreSQL), ADR-003 (Event-sourced bitemporal model)

> **Note:** This spec is a design document. The implemented schema differs in some details:
> - Embedding dimensions are 1024 (not 1536) per migration 013
> - `spans`, `run_params`, `run_metrics`, `run_tags` tables are not yet implemented
> - See `migrations/*.sql` for the actual schema

---

## Overview

Event-sourced architecture with bi-temporal modeling. The append-only event log (`agent_events`) is the single source of truth. Materialized views and derived tables provide query performance. Mutable entities use bi-temporal columns for full auditability.

## Schema Design

### Core Tables

#### `agent_runs`

Top-level execution context. Corresponds to an OTEL trace. Immutable once created.

```sql
CREATE TABLE agent_runs (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_id        TEXT NOT NULL,
    trace_id        TEXT,                          -- OTEL trace correlation
    parent_run_id   UUID REFERENCES agent_runs(id),-- nested/child runs
    status          TEXT NOT NULL DEFAULT 'running',-- running, completed, failed
    started_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at    TIMESTAMPTZ,
    metadata        JSONB NOT NULL DEFAULT '{}',   -- facet-based extensibility
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_agent_runs_agent_id ON agent_runs(agent_id);
CREATE INDEX idx_agent_runs_trace_id ON agent_runs(trace_id) WHERE trace_id IS NOT NULL;
CREATE INDEX idx_agent_runs_status ON agent_runs(status);
CREATE INDEX idx_agent_runs_started_at ON agent_runs(started_at DESC);
```

#### `agent_events`

Append-only event log. Source of truth. Never mutated or deleted. TimescaleDB hypertable for automatic partitioning and compression.

```sql
CREATE TABLE agent_events (
    id              UUID NOT NULL DEFAULT gen_random_uuid(),
    run_id          UUID NOT NULL,                 -- logical FK to agent_runs(id); not enforced (hypertable constraint)
    event_type      TEXT NOT NULL,
    sequence_num    BIGINT NOT NULL,               -- ordering within a run
    occurred_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    agent_id        TEXT NOT NULL,
    payload         JSONB NOT NULL DEFAULT '{}',   -- event-specific data (OpenLineage facet pattern)
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (id, occurred_at)                  -- TimescaleDB requires partitioning column in PK
);

-- Convert to TimescaleDB hypertable, partitioned by occurred_at
-- NOTE: must be called before creating indexes; FK constraints FROM hypertables
-- are not supported by TimescaleDB, so run_id integrity is enforced at the application layer
SELECT create_hypertable('agent_events', 'occurred_at');

CREATE INDEX idx_agent_events_run_id ON agent_events(run_id, sequence_num);
CREATE INDEX idx_agent_events_type ON agent_events(event_type, occurred_at DESC);
CREATE INDEX idx_agent_events_agent_id ON agent_events(agent_id, occurred_at DESC);
CREATE INDEX idx_agent_events_payload ON agent_events USING GIN (payload);
```

**Compression policy** (activate on chunks older than 7 days):
```sql
ALTER TABLE agent_events SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'agent_id,run_id',
    timescaledb.compress_orderby = 'occurred_at DESC'
);

SELECT add_compression_policy('agent_events', INTERVAL '7 days');
```

#### `decisions`

First-class decision entities. Bi-temporal: `valid_from`/`valid_to` (business time) + `transaction_time` (system time). Decisions are created from `DecisionMade` events and revised via `DecisionRevised` events.

```sql
CREATE TABLE decisions (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id              UUID NOT NULL REFERENCES agent_runs(id),
    agent_id            TEXT NOT NULL,
    decision_type       TEXT NOT NULL,             -- e.g., "tool_selection", "response_generation", "routing"
    outcome             TEXT NOT NULL,             -- what was decided
    confidence          REAL NOT NULL CHECK (confidence >= 0.0 AND confidence <= 1.0),
    reasoning           TEXT,                      -- step-by-step reasoning chain
    embedding           vector(1536),              -- for semantic search
    metadata            JSONB NOT NULL DEFAULT '{}',

    -- Bi-temporal columns
    valid_from          TIMESTAMPTZ NOT NULL DEFAULT now(),
    valid_to            TIMESTAMPTZ,               -- NULL = currently valid
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
```

#### `alternatives`

Alternatives considered for each decision. Immutable.

```sql
CREATE TABLE alternatives (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    decision_id     UUID NOT NULL REFERENCES decisions(id),
    label           TEXT NOT NULL,                 -- alternative name/description
    score           REAL,                          -- evaluation score (0.0-1.0)
    selected        BOOLEAN NOT NULL DEFAULT false,-- was this the chosen alternative?
    rejection_reason TEXT,                         -- why not selected (if not selected)
    metadata        JSONB NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_alternatives_decision_id ON alternatives(decision_id);
CREATE INDEX idx_alternatives_selected ON alternatives(decision_id) WHERE selected = true;
```

#### `evidence`

Evidence that supported a decision. Includes provenance tracking. Immutable.

```sql
CREATE TABLE evidence (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    decision_id     UUID NOT NULL REFERENCES decisions(id),
    source_type     TEXT NOT NULL,                 -- "document", "api_response", "agent_output", "user_input", "search_result"
    source_uri      TEXT,                          -- provenance: where did this evidence come from?
    content         TEXT NOT NULL,                 -- the evidence itself
    relevance_score REAL,                          -- how relevant to the decision (0.0-1.0)
    embedding       vector(1536),                  -- for semantic search over evidence
    metadata        JSONB NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_evidence_decision_id ON evidence(decision_id);
CREATE INDEX idx_evidence_source_type ON evidence(source_type);
CREATE INDEX idx_evidence_embedding ON evidence USING hnsw (embedding vector_cosine_ops)
    WITH (m = 16, ef_construction = 64);
```

#### `spans`

OTEL-compatible hierarchical trace structure. Immutable.

```sql
CREATE TABLE spans (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id          UUID NOT NULL REFERENCES agent_runs(id),
    parent_span_id  UUID REFERENCES spans(id),
    trace_id        TEXT,                          -- OTEL trace_id correlation
    span_id         TEXT,                          -- OTEL span_id
    name            TEXT NOT NULL,
    kind            TEXT NOT NULL DEFAULT 'internal',-- internal, client, server, producer, consumer
    started_at      TIMESTAMPTZ NOT NULL,
    ended_at        TIMESTAMPTZ,
    status          TEXT NOT NULL DEFAULT 'ok',    -- ok, error, unset
    attributes      JSONB NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_spans_run_id ON spans(run_id);
CREATE INDEX idx_spans_trace_id ON spans(trace_id) WHERE trace_id IS NOT NULL;
CREATE INDEX idx_spans_parent ON spans(parent_span_id) WHERE parent_span_id IS NOT NULL;
```

#### `run_params`, `run_metrics`, `run_tags`

MLflow-inspired metadata tables.

```sql
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

-- Mutable tags for categorization
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
```

### Access Control Tables

```sql
-- Agent identity and role assignment
CREATE TABLE agents (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_id    TEXT NOT NULL UNIQUE,             -- external agent identifier
    name        TEXT NOT NULL,
    role        TEXT NOT NULL DEFAULT 'agent',    -- admin, agent, reader
    api_key_hash TEXT,                            -- hashed API key for JWT issuance
    metadata    JSONB NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Fine-grained access grants (scoped visibility)
CREATE TABLE access_grants (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    grantor_id      UUID NOT NULL REFERENCES agents(id),
    grantee_id      UUID NOT NULL REFERENCES agents(id),
    resource_type   TEXT NOT NULL,                -- "agent_traces", "decision", "run"
    resource_id     TEXT,                         -- specific resource ID, or NULL for all of type
    permission      TEXT NOT NULL,                -- "read", "write"
    granted_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at      TIMESTAMPTZ,                  -- optional expiration
    UNIQUE(grantee_id, resource_type, resource_id, permission)
);

CREATE INDEX idx_access_grants_grantee ON access_grants(grantee_id, resource_type);
CREATE INDEX idx_agents_agent_id ON agents(agent_id);
```

## Event Type Payloads

Each event type has a defined JSONB payload structure. The `event_type` field determines the expected payload shape.

### Run Lifecycle Events

```json
// AgentRunStarted
{
  "agent_id": "underwriting-agent",
  "agent_version": "1.2.0",
  "trigger": "api_call",
  "input_summary": "Evaluate loan application #12345",
  "config": { "model": "gpt-4o", "temperature": 0.1 }
}

// AgentRunCompleted
{
  "output_summary": "Loan approved with conditions",
  "duration_ms": 4523,
  "token_usage": { "input": 12000, "output": 3400 }
}

// AgentRunFailed
{
  "error_type": "timeout",
  "error_message": "LLM call exceeded 30s timeout",
  "partial_output": null,
  "retryable": true
}
```

### Decision Events

```json
// DecisionStarted
{
  "decision_type": "loan_approval",
  "context_summary": "Evaluating application DTI=42%, credit=720"
}

// AlternativeConsidered
{
  "decision_id": "uuid",
  "label": "Approve with standard terms",
  "score": 0.82,
  "evaluation_criteria": { "risk_score": 0.3, "revenue_potential": 0.9 }
}

// EvidenceGathered
{
  "decision_id": "uuid",
  "source_type": "api_response",
  "source_uri": "credit-bureau/report/12345",
  "content_summary": "Credit score 720, no delinquencies",
  "relevance_score": 0.95
}

// ReasoningStepCompleted
{
  "decision_id": "uuid",
  "step_number": 3,
  "description": "Compared DTI against policy threshold of 45%",
  "conclusion": "DTI 42% is within acceptable range"
}

// DecisionMade
{
  "decision_id": "uuid",
  "outcome": "approve_with_conditions",
  "confidence": 0.87,
  "conditions": ["Require 6 months bank statements"],
  "reasoning_summary": "DTI within threshold, strong credit history, minor employment gap"
}

// DecisionRevised
{
  "original_decision_id": "uuid",
  "revised_decision_id": "uuid",
  "revision_reason": "New evidence: employer verification failed",
  "previous_outcome": "approve_with_conditions",
  "new_outcome": "deny",
  "new_confidence": 0.92
}
```

### Coordination Events

```json
// AgentHandoff
{
  "from_agent_id": "underwriting-agent",
  "to_agent_id": "compliance-agent",
  "handoff_reason": "Requires compliance review for high-value loan",
  "context_snapshot": { "decision_ids": ["uuid1", "uuid2"] },
  "priority": "high"
}

// ConsensusRequested
{
  "topic": "loan_approval_12345",
  "requesting_agent_id": "supervisor-agent",
  "participant_agent_ids": ["underwriting-agent", "risk-agent", "compliance-agent"],
  "deadline": "2026-02-03T12:00:00Z"
}

// ConflictDetected
{
  "topic": "loan_approval_12345",
  "conflicting_decisions": [
    { "agent_id": "underwriting-agent", "decision_id": "uuid1", "outcome": "approve" },
    { "agent_id": "risk-agent", "decision_id": "uuid2", "outcome": "deny" }
  ],
  "conflict_type": "opposing_outcomes",
  "severity": "high"
}
```

## Materialized Views

### Conflict Detection View

```sql
CREATE MATERIALIZED VIEW decision_conflicts AS
SELECT
    d1.id AS decision_a_id,
    d2.id AS decision_b_id,
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
    AND d1.agent_id != d2.agent_id
    AND d1.outcome != d2.outcome
    AND d1.valid_to IS NULL
    AND d2.valid_to IS NULL
    AND d1.id < d2.id  -- avoid duplicates
    AND ABS(EXTRACT(EPOCH FROM (d1.valid_from - d2.valid_from))) < 3600  -- within 1 hour
WITH DATA;

CREATE UNIQUE INDEX idx_decision_conflicts_pair ON decision_conflicts(decision_a_id, decision_b_id);

-- Refresh strategy: on-demand after decision writes, or periodic (every 30 seconds)
```

### Current Agent State View

```sql
CREATE MATERIALIZED VIEW agent_current_state AS
SELECT
    ar.agent_id,
    ar.id AS latest_run_id,
    ar.status AS run_status,
    ar.started_at,
    COUNT(ae.id) AS event_count,
    MAX(ae.occurred_at) AS last_activity,
    (SELECT COUNT(*) FROM decisions d WHERE d.agent_id = ar.agent_id AND d.valid_to IS NULL) AS active_decisions
FROM agent_runs ar
JOIN agent_events ae ON ae.run_id = ar.id
WHERE ar.started_at = (
    SELECT MAX(started_at) FROM agent_runs WHERE agent_id = ar.agent_id
)
GROUP BY ar.agent_id, ar.id, ar.status, ar.started_at
WITH DATA;
```

## Bi-Temporal Query Patterns

### "What did we know at time T?" (transaction_time query)

```sql
-- All decisions recorded before a specific point in time
SELECT * FROM decisions
WHERE transaction_time <= '2026-02-03T10:00:00Z'
AND (valid_to IS NULL OR valid_to > '2026-02-03T10:00:00Z')
ORDER BY valid_from DESC;
```

### "When was this decision valid?" (business time query)

```sql
-- Decision validity timeline
SELECT id, outcome, confidence, valid_from, valid_to, transaction_time
FROM decisions
WHERE decision_type = 'loan_approval'
AND run_id = 'uuid'
ORDER BY transaction_time ASC;
```

### "Context replay" — reconstruct state at decision time

```sql
-- All events that occurred before a specific decision
SELECT ae.* FROM agent_events ae
JOIN decisions d ON ae.run_id = d.run_id
WHERE d.id = 'decision-uuid'
AND ae.occurred_at <= d.valid_from
ORDER BY ae.sequence_num ASC;
```

## Embedding Strategy

Decisions and evidence get vector embeddings for semantic search.

- **Embedding dimension**: 1536 (OpenAI `text-embedding-3-small` compatible; configurable)
- **Index type**: HNSW with `vector_cosine_ops`
- **Index parameters**: `m = 16`, `ef_construction = 64` (tunable per deployment)
- **Embedding generation**: Server-side. Akashi generates embeddings on ingestion using a configurable embedding provider. Clients can also supply pre-computed embeddings.

## Migration Strategy

Migrations are forward-only SQL files in `migrations/`, numbered sequentially:

```
migrations/
  001_create_agent_runs.sql
  002_create_agent_events.sql
  003_create_decisions.sql
  004_create_alternatives.sql
  005_create_evidence.sql
  006_create_spans.sql
  007_create_run_metadata.sql
  008_create_access_control.sql
  009_create_materialized_views.sql
  010_create_hypertable_and_compression.sql
```

No rollback files. If a migration fails, fix forward.

## References

- ADR-002: Unified PostgreSQL storage
- ADR-003: Event-sourced data model with bi-temporal modeling
- SPEC-001: System Overview
