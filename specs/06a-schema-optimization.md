# Schema Optimization

**Status:** Draft
**Author:** Akashi team
**Last Updated:** 2026-02-07

## 1. Overview

This spec fixes schema gaps discovered during a full audit of the PostgreSQL schema against actual query patterns in the storage layer.

### Priority

| Severity | Count | Summary |
|----------|-------|---------|
| Critical | 2 | Multi-tenancy data leak in evidence search; N+1 subquery in mat view |
| Medium | 6 | Missing composite indexes; restrictive CHECK constraint; non-concurrent mat view refresh |
| Low | 2 | Chunk interval tuning; missing retention policy |

---

## 2. Critical: Add `org_id` to `evidence` Table

### Problem

Migration 014 added `org_id` to `agents`, `agent_runs`, `decisions`, `agent_events`, and `access_grants` — but **skipped `evidence`**. The `SearchEvidenceByEmbedding` method (`evidence.go:134`) performs vector similarity search with no org scoping:

```sql
SELECT ... FROM evidence WHERE embedding IS NOT NULL ORDER BY embedding <=> $1 LIMIT $2
```

Any authenticated user can semantically search another organization's evidence. This is a **live multi-tenancy data leak**.

### Migration

**File: `migrations/016_schema_optimization.sql`** (section 1)

```sql
-- =============================================================================
-- 1. Add org_id to evidence table
-- =============================================================================

-- Add column with default for backfill.
ALTER TABLE evidence
    ADD COLUMN IF NOT EXISTS org_id UUID;

-- Backfill org_id from the parent decision.
UPDATE evidence e
SET org_id = d.org_id
FROM decisions d
WHERE e.decision_id = d.id
  AND e.org_id IS NULL;

-- Make NOT NULL after backfill.
ALTER TABLE evidence ALTER COLUMN org_id SET NOT NULL;

-- Index for org-scoped queries.
CREATE INDEX IF NOT EXISTS idx_evidence_org
    ON evidence (org_id, decision_id);
```

### Go Code Changes

**File: `internal/storage/evidence.go`**

#### `SearchEvidenceByEmbedding` — add org_id parameter and WHERE clause

```go
// SearchEvidenceByEmbedding performs semantic similarity search over evidence within an org.
func (db *DB) SearchEvidenceByEmbedding(ctx context.Context, orgID uuid.UUID, embedding pgvector.Vector, limit int) ([]model.Evidence, error) {
    if limit <= 0 {
        limit = 10
    }

    rows, err := db.pool.Query(ctx,
        `SELECT id, decision_id, org_id, source_type, source_uri, content,
         relevance_score, metadata, created_at
         FROM evidence
         WHERE org_id = $1 AND embedding IS NOT NULL
         ORDER BY embedding <=> $2
         LIMIT $3`, orgID, embedding, limit,
    )
    // ... scan with org_id field
}
```

**Signature change:** Add `orgID uuid.UUID` as the second parameter. Update all callers.

#### `CreateEvidence` / `CreateEvidenceBatch` — include org_id in INSERT

Add `org_id` to the INSERT column list and values. The `model.Evidence` struct needs an `OrgID` field (see Section 2.1 below).

#### `GetEvidenceByDecision` / `GetEvidenceByDecisions` — add org_id to SELECT

Update the SELECT list to include `org_id` and the scan call.

### Model Change

**File: `internal/model/decision.go`**

```go
type Evidence struct {
    ID             uuid.UUID        `json:"id"`
    DecisionID     uuid.UUID        `json:"decision_id"`
    OrgID          uuid.UUID        `json:"org_id"`        // NEW
    SourceType     SourceType       `json:"source_type"`
    SourceURI      *string          `json:"source_uri,omitempty"`
    Content        string           `json:"content"`
    RelevanceScore *float32         `json:"relevance_score,omitempty"`
    Embedding      *pgvector.Vector `json:"-"`
    Metadata       map[string]any   `json:"metadata"`
    CreatedAt      time.Time        `json:"created_at"`
}
```

### Callers to Update

| File | Method | Change |
|------|--------|--------|
| `internal/storage/evidence.go` | `SearchEvidenceByEmbedding` | Add `orgID` param + WHERE clause |
| `internal/storage/evidence.go` | `CreateEvidence` | Add `org_id` to INSERT |
| `internal/storage/evidence.go` | `CreateEvidenceBatch` | Add `org_id` to COPY columns |
| `internal/storage/evidence.go` | `GetEvidenceByDecision` | Add `org_id` to SELECT + scan |
| `internal/storage/evidence.go` | `GetEvidenceByDecisions` | Add `org_id` to SELECT + scan |
| `internal/storage/trace.go` | `CreateTraceTx` | Set `ev.OrgID = params.OrgID` before COPY; add `org_id` to COPY columns list |
| `internal/storage/delete.go` | `DeleteAgentData` | No change needed (deletes via `decision_id IN (...)`, still works) |
| `internal/service/decisions/service.go` | `Trace` | Set `evs[i].OrgID = orgID` when building evidence slice (currently not set — newly added field) |
| Any MCP/handler that calls `SearchEvidenceByEmbedding` | Pass `orgID` from auth context |

---

## 3. Critical: Fix `agent_current_state` Materialized View

### Problem

The current mat view definition has a correlated subquery that runs per-row:

```sql
(SELECT COUNT(*) FROM decisions d WHERE d.agent_id = ar.agent_id AND d.org_id = ar.org_id AND d.valid_to IS NULL) AS active_decisions
```

With 1000 agents, each REFRESH executes 1000 separate COUNT queries. This scales O(n) in agent count.

### Migration

**File: `migrations/016_schema_optimization.sql`** (section 2)

```sql
-- =============================================================================
-- 2. Rewrite agent_current_state materialized view
-- =============================================================================

DROP MATERIALIZED VIEW IF EXISTS agent_current_state;

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

-- Unique index required for REFRESH MATERIALIZED VIEW CONCURRENTLY.
CREATE UNIQUE INDEX idx_agent_current_state_agent_org
    ON agent_current_state (agent_id, org_id);
```

### Why This Is Better

- **`latest_runs` CTE** uses `DISTINCT ON` — Postgres picks the first row per `(agent_id, org_id)` after ordering by `started_at DESC`. One pass over `agent_runs`, leveraging `idx_agent_runs_started_at`.
- **`decision_counts` CTE** aggregates once across all agents, not per-row. One sequential scan of `decisions WHERE valid_to IS NULL`, leveraging the partial index.
- **`event_stats` CTE** aggregates once across all runs. One sequential scan of `agent_events`.
- No correlated subqueries. Total: 3 scans + 2 hash joins instead of N+1 queries.

### Go Code Change

**File: `internal/storage/conflicts.go`**

Change `RefreshAgentState` to use CONCURRENTLY (now possible with the unique index):

```go
func (db *DB) RefreshAgentState(ctx context.Context) error {
    _, err := db.pool.Exec(ctx, `REFRESH MATERIALIZED VIEW CONCURRENTLY agent_current_state`)
    if err != nil {
        return fmt.Errorf("storage: refresh agent state: %w", err)
    }
    return nil
}
```

---

## 4. Medium: Missing Composite Indexes

### Migration

**File: `migrations/016_schema_optimization.sql`** (section 3)

```sql
-- =============================================================================
-- 3. Missing composite indexes
-- =============================================================================

-- 3a. decisions: org_id + decision_type without partial WHERE clause.
-- Covers queries like: SELECT COUNT(*) FROM decisions WHERE org_id = ? AND decision_type = ?
-- (without valid_to IS NULL filter). The existing idx_decisions_org_agent_current is
-- partial and only covers current decisions.
CREATE INDEX IF NOT EXISTS idx_decisions_org_type
    ON decisions (org_id, decision_type, valid_from DESC);

-- 3b. agent_events: org_id + event_type composite.
-- Covers queries filtering by org + event type (e.g., "find all tool_call events in org X").
-- Without this, Postgres must merge idx_agent_events_type and idx_agent_events_org
-- or fall back to sequential scan.
CREATE INDEX IF NOT EXISTS idx_agent_events_org_type
    ON agent_events (org_id, event_type, occurred_at DESC);

-- 3c. access_grants: expires_at for cleanup queries.
-- Enables efficient deletion of expired grants (background job).
CREATE INDEX IF NOT EXISTS idx_access_grants_expires
    ON access_grants (expires_at)
    WHERE expires_at IS NOT NULL;

-- 3d. agent_runs: parent_run_id for tree traversal.
-- Enables efficient lookup of child runs (nested agent calls).
CREATE INDEX IF NOT EXISTS idx_agent_runs_parent_run
    ON agent_runs (parent_run_id)
    WHERE parent_run_id IS NOT NULL;

-- 3e. decisions: temporal companion for non-NULL valid_to.
-- The existing idx_decisions_temporal only covers WHERE valid_to IS NULL.
-- Bi-temporal queries that check both current and historical decisions
-- (valid_to IS NULL OR valid_to > $asOf) need this companion.
CREATE INDEX IF NOT EXISTS idx_decisions_temporal_historical
    ON decisions (org_id, transaction_time, valid_to)
    WHERE valid_to IS NOT NULL;

-- 3f. agents: metadata GIN index.
-- Enables queries like: SELECT * FROM agents WHERE metadata @> '{"team": "security"}'
CREATE INDEX IF NOT EXISTS idx_agents_metadata
    ON agents USING GIN (metadata);
```

### No Go Code Changes

These indexes are transparent to the query planner. Existing queries benefit automatically.

---

## 5. Medium: Widen `evidence.source_type` CHECK Constraint

### Problem

The CHECK constraint on `evidence.source_type` allows only 5 values: `'document'`, `'api_response'`, `'agent_output'`, `'user_input'`, `'search_result'`. When application code or an SDK sends a new source type (e.g., `'tool_output'`, `'database_query'`, `'memory'`), the INSERT fails with a CHECK violation that surfaces as a silent 500 error.

This is listed in MEMORY.md as known technical debt.

### Migration

**File: `migrations/016_schema_optimization.sql`** (section 4)

```sql
-- =============================================================================
-- 4. Widen evidence.source_type CHECK constraint
-- =============================================================================

-- Drop the old restrictive CHECK constraint.
DO $$
DECLARE
    constraint_name text;
BEGIN
    SELECT conname INTO constraint_name
    FROM pg_constraint
    WHERE conrelid = 'evidence'::regclass
      AND contype = 'c'
      AND pg_get_constraintdef(oid) LIKE '%source_type%';

    IF constraint_name IS NOT NULL THEN
        EXECUTE format('ALTER TABLE evidence DROP CONSTRAINT %I', constraint_name);
    END IF;
END $$;

-- Add a more permissive CHECK. The constraint validates format (non-empty,
-- lowercase, underscore-separated) rather than enumerating values. New source
-- types can be added without migration.
ALTER TABLE evidence
    ADD CONSTRAINT evidence_source_type_format
    CHECK (source_type ~ '^[a-z][a-z0-9_]*$');
```

### Go Code Change

**File: `internal/model/decision.go`**

Add the new source types as constants (but the CHECK constraint no longer rejects unknown types):

```go
const (
    SourceDocument     SourceType = "document"
    SourceAPIResponse  SourceType = "api_response"
    SourceAgentOutput  SourceType = "agent_output"
    SourceUserInput    SourceType = "user_input"
    SourceSearchResult SourceType = "search_result"
    SourceToolOutput   SourceType = "tool_output"
    SourceMemory       SourceType = "memory"
    SourceDatabaseQuery SourceType = "database_query"
)
```

---

## 6. Medium: Non-Concurrent `agent_current_state` Refresh

Addressed by Section 3 above. The rewritten mat view includes a unique index (`idx_agent_current_state_agent_org`), enabling `REFRESH MATERIALIZED VIEW CONCURRENTLY`. The Go code change is in Section 3.

---

## 7. Low: TimescaleDB Chunk Interval

### Current State

`agent_events` uses a 1-day chunk interval (migration 010). This is optimal for high-volume workloads (>1M events/day). For Akashi's current traffic (~1K-100K events/day), 1-day chunks create many small, sparse chunks that increase metadata overhead.

### Recommendation

**Do not change now.** Monitor chunk sizes for 30 days after tenant isolation. If average chunk is under 100MB:

```sql
-- Check chunk sizes (run manually, not in migration).
SELECT
    hypertable_name,
    chunk_name,
    pg_size_pretty(total_bytes) AS total_size
FROM timescaledb_information.chunks
WHERE hypertable_name = 'agent_events'
ORDER BY range_start DESC
LIMIT 20;
```

If chunks are consistently under 100MB, increase interval:

```sql
SELECT set_chunk_time_interval('agent_events', INTERVAL '7 days');
```

This is a live operation that only affects future chunks. Existing chunks remain unchanged.

---

## 8. Low: Retention Policy

### Current State

`agent_events` compresses chunks older than 7 days but never drops them. Storage grows indefinitely. GDPR compliance requires an ability to delete old data.

### Recommendation

**Add a retention policy after tenant isolation is complete** (spec 06, Phase 6). The retention interval should be configurable per-tenant:

- Free: 90 days
- Pro: 1 year
- Enterprise: unlimited (or customer-defined)

For now, document the command for manual use:

```sql
-- Drop chunks older than 90 days (example, not in migration).
SELECT drop_chunks('agent_events', INTERVAL '90 days');
```

The `DeprovisionSchema` method in spec 06 handles GDPR full-deletion via `DROP SCHEMA CASCADE`.

---

## 9. Complete Migration

**File: `migrations/016_schema_optimization.sql`**

Combines all sections above into a single idempotent migration:

```sql
-- 016_schema_optimization.sql
-- Schema optimizations: evidence.org_id, mat view rewrite, missing indexes,
-- source_type constraint. Prerequisite for tenant isolation (spec 06).

-- =============================================================================
-- 1. Add org_id to evidence table
-- =============================================================================

ALTER TABLE evidence
    ADD COLUMN IF NOT EXISTS org_id UUID;

UPDATE evidence e
SET org_id = d.org_id
FROM decisions d
WHERE e.decision_id = d.id
  AND e.org_id IS NULL;

ALTER TABLE evidence ALTER COLUMN org_id SET NOT NULL;

CREATE INDEX IF NOT EXISTS idx_evidence_org
    ON evidence (org_id, decision_id);

-- =============================================================================
-- 2. Rewrite agent_current_state materialized view
-- =============================================================================

DROP MATERIALIZED VIEW IF EXISTS agent_current_state;

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

-- =============================================================================
-- 3. Missing composite indexes
-- =============================================================================

CREATE INDEX IF NOT EXISTS idx_decisions_org_type
    ON decisions (org_id, decision_type, valid_from DESC);

CREATE INDEX IF NOT EXISTS idx_agent_events_org_type
    ON agent_events (org_id, event_type, occurred_at DESC);

CREATE INDEX IF NOT EXISTS idx_access_grants_expires
    ON access_grants (expires_at)
    WHERE expires_at IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_agent_runs_parent_run
    ON agent_runs (parent_run_id)
    WHERE parent_run_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_decisions_temporal_historical
    ON decisions (org_id, transaction_time, valid_to)
    WHERE valid_to IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_agents_metadata
    ON agents USING GIN (metadata);

-- =============================================================================
-- 4. Widen evidence.source_type CHECK constraint
-- =============================================================================

DO $$
DECLARE
    constraint_name text;
BEGIN
    SELECT conname INTO constraint_name
    FROM pg_constraint
    WHERE conrelid = 'evidence'::regclass
      AND contype = 'c'
      AND pg_get_constraintdef(oid) LIKE '%source_type%';

    IF constraint_name IS NOT NULL THEN
        EXECUTE format('ALTER TABLE evidence DROP CONSTRAINT %I', constraint_name);
    END IF;
END $$;

ALTER TABLE evidence
    ADD CONSTRAINT evidence_source_type_format
    CHECK (source_type ~ '^[a-z][a-z0-9_]*$');
```

---

## 10. Go Code Changes Summary

| File | Change | Lines Affected |
|------|--------|---------------|
| `internal/model/decision.go` | Add `OrgID uuid.UUID` to `Evidence` struct; add new `SourceType` constants | ~5 lines |
| `internal/storage/evidence.go` | Add `orgID` param to `SearchEvidenceByEmbedding`; add `org_id` to all INSERT/SELECT/COPY operations; update all scan calls | ~30 lines |
| `internal/storage/trace.go` | Add `org_id` to evidence COPY columns in `CreateTraceTx`; populate `OrgID` on evidence rows | ~5 lines |
| `internal/storage/conflicts.go` | Change `RefreshAgentState` to use `CONCURRENTLY` | 1 line |
| `internal/service/decisions/service.go` | Set `evs[i].OrgID = orgID` when building evidence for `Trace`; pass `orgID` to any `SearchEvidenceByEmbedding` calls | ~3 lines |
| Any handler/MCP calling `SearchEvidenceByEmbedding` | Pass `orgID` from auth context | ~1 line per call site |

No new files are created. No new dependencies.

---

## 11. Testing

| Test | Description |
|------|-------------|
| `TestEvidenceOrgIsolation` | Create evidence in org A, search by embedding in org B, verify zero results |
| `TestEvidenceOrgIDBackfill` | Run migration against DB with existing evidence, verify all rows have org_id |
| `TestAgentCurrentStateRefresh` | Refresh mat view, verify counts match direct queries, verify CONCURRENTLY doesn't block |
| `TestSourceTypeFlexibility` | Insert evidence with `source_type = 'tool_output'`, verify success (was 500 before) |
| `TestCompositeIndexUsage` | EXPLAIN ANALYZE key queries, verify new indexes are used |
| `TestDecisionTemporalQuery` | Query with both current and historical decisions, verify idx_decisions_temporal_historical is used |

---

## 12. Execution Order

1. Write migration `016_schema_optimization.sql`
2. Run migration against TimescaleDB Cloud
3. Update `model.Evidence` — add `OrgID` field
4. Update `storage/evidence.go` — add org_id to all methods
5. Update `storage/trace.go` — add org_id to evidence COPY
6. Update `storage/conflicts.go` — CONCURRENTLY refresh
7. Update `service/decisions/service.go` — set evidence OrgID
8. Update callers of `SearchEvidenceByEmbedding`
9. Run `go test ./... -v -race`
10. Verify with `make all`

---

## 13. Migration Numbering

This spec claims **migration number 016**. Spec 07 (Qdrant vector search) follows at migration 017.

---

## 14. Invariants

1. **Every `evidence` row must have a non-NULL `org_id`** matching its parent decision's `org_id`. Enforced by NOT NULL constraint + application code.

2. **`SearchEvidenceByEmbedding` must always filter by `org_id`.** No cross-org evidence search is permitted.

3. **`evidence.source_type` must match `^[a-z][a-z0-9_]*$`.** New source types require only a Go constant, not a migration.

4. **`agent_current_state` must always have a unique index on `(agent_id, org_id)`** to support `REFRESH CONCURRENTLY`.

5. **All new indexes use `IF NOT EXISTS`** to ensure migration idempotency.
