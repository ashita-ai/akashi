# Spec 07: Qdrant Vector Search Integration

## Context

Akashi uses pgvector (HNSW indexes) in PostgreSQL for semantic search over decision traces. pgvector won't scale with us — at 5-10M decisions with 1024-dim vectors the HNSW index alone is 30-60GB, filtered search quality degrades because post-filtering eats top-K results, and per-schema HNSW indexes under tenant isolation multiply the cost.

We're adding Qdrant Cloud as the primary vector search index. Postgres remains the authoritative data store. Qdrant is a derived, eventually-consistent search index populated via an outbox pattern. When Qdrant is unavailable, search falls back to the existing pgvector path transparently.

**Embedding generation is unchanged.** Ollama (`mxbai-embed-large`, 1024 dims) generates embeddings. Qdrant doesn't generate embeddings — it only stores and searches vectors. The `embedding.Provider` interface stays exactly as-is.

**Qdrant Go client:** `github.com/qdrant/go-client` (gRPC-based, port 6334). The user's Qdrant Cloud URL uses REST port 6333 — config parsing extracts the host and connects on gRPC port 6334 with TLS.

---

## Tenant Isolation: Shared Collection with `org_id` Payload Filter

**Decision: Shared collection, not collection-per-tenant.**

- Our practical schema-per-tenant ceiling is ~50 tenants. Both approaches work at this scale.
- Shared collection = simpler code (one collection name, one set of HNSW params, one outbox worker).
- `org_id` as an indexed keyword field. Qdrant applies payload filters before ANN traversal when filters are selective.
- GDPR deletion: `DeletePoints` with `org_id` filter. Immediately invisible to queries. Physical removal at compaction. Verify with `Count` query.
- Enterprise customers who need guaranteed physical deletion get their own Akashi deployment (per our architecture decision — enterprise = separate instance).

---

## Architecture

```
Write path:
  Service.Trace() → Postgres tx (decision + embedding + outbox entry)
                  → COMMIT
                  → outbox worker polls → Qdrant.Upsert (async, ~1-2s lag)

Search path:
  Service.Search() → Qdrant.Search (vector + org_id filter)
                   → get decision IDs + scores
                   → Postgres.GetDecisionsByIDs (full records + valid_to check)
                   → re-score with quality + recency weighting
                   → authz filter (existing)

Fallback:
  Qdrant unhealthy or error → pgvector (existing path, unchanged)
```

---

## 1. New Package: `internal/search/`

### `search.go` — Interface + types

```go
// Result is a search hit from the vector index (ID + score only).
type Result struct {
    DecisionID uuid.UUID
    Score      float32
}

// Searcher provides vector similarity search over an external index.
type Searcher interface {
    Search(ctx context.Context, orgID uuid.UUID, embedding []float32, filters model.QueryFilters, limit int) ([]Result, error)
    Healthy(ctx context.Context) error
}
```

Returns IDs + scores, not full Decision objects. Postgres is source of truth for record data. Accepts `[]float32` (not `pgvector.Vector`) to avoid Postgres-specific types in the search interface.

### `qdrant.go` — QdrantIndex (implements Searcher + write methods)

```go
type QdrantIndex struct { ... }

func NewQdrantIndex(cfg QdrantConfig, logger *slog.Logger) (*QdrantIndex, error)
func (q *QdrantIndex) EnsureCollection(ctx context.Context) error    // create collection + payload indexes if not exists
func (q *QdrantIndex) Search(ctx, orgID, embedding, filters, limit)  // Searcher interface
func (q *QdrantIndex) Upsert(ctx context.Context, points []Point) error
func (q *QdrantIndex) DeleteByOrg(ctx context.Context, orgID uuid.UUID) error
func (q *QdrantIndex) DeleteByIDs(ctx context.Context, ids []uuid.UUID) error
func (q *QdrantIndex) Healthy(ctx context.Context) error             // cached 5s health check
func (q *QdrantIndex) Close() error
```

### `outbox.go` — OutboxWorker (follows buffer.go pattern)

```go
type OutboxWorker struct { ... }

func NewOutboxWorker(pool *pgxpool.Pool, index *QdrantIndex, logger *slog.Logger, opts ...Option) *OutboxWorker
func (w *OutboxWorker) Start(ctx context.Context)
func (w *OutboxWorker) Drain(ctx context.Context)
```

---

## 2. Qdrant Collection Schema

**Collection:** `akashi_decisions` (1024 dims, cosine distance, HNSW m=16 ef_construct=128)

**Point ID:** Decision UUID (string)

| Payload field    | Type    | Indexed | Purpose                     |
|------------------|---------|---------|-----------------------------|
| `org_id`         | keyword | Yes     | Tenant isolation (every query) |
| `agent_id`       | keyword | Yes     | Agent filter                |
| `decision_type`  | keyword | Yes     | Type filter                 |
| `confidence`     | float   | Yes     | Min confidence filter       |
| `quality_score`  | float   | Yes     | Used in re-scoring          |
| `valid_from`     | integer | Yes     | Unix timestamp for time range + recency |
| `run_id`         | keyword | No      | Stored for optional filter  |
| `outcome`        | keyword | No      | Stored for optional filter  |

**Quality + recency re-scoring** — applied in Go after Qdrant returns raw cosine scores:
```
relevance = similarity * (0.6 + 0.3 * quality_score) * (1.0 / (1.0 + age_days / 90.0))
```
Over-fetch `limit * 3` from Qdrant, re-score, sort, truncate to `limit`.

---

## 3. Outbox Table

**Migration: `migrations/017_search_outbox.sql`**

```sql
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
    WHERE locked_until IS NULL OR locked_until < now();

CREATE UNIQUE INDEX idx_search_outbox_decision_op
    ON search_outbox (decision_id, operation);

CREATE INDEX idx_search_outbox_org
    ON search_outbox (org_id);
```

- **Outbox insert is inside `CreateTraceTx` transaction** — if decision commits, outbox entry commits. No orphans.
- **Unique index on `(decision_id, operation)`** — deduplication via `ON CONFLICT DO NOTHING`.
- **`locked_until`** — pessimistic lock for worker. Expires if worker crashes.
- **`attempts` + `last_error`** — dead-letter after 10 retries.

### Outbox Worker Poll Loop

```
1. FOR UPDATE SKIP LOCKED — claim batch of N unlocked rows
2. SET locked_until = now() + 30s
3. For upsert: fetch decision + embedding from Postgres, build Point, call Qdrant.Upsert
4. For delete: call Qdrant.DeleteByIDs
5. On success: DELETE outbox row
6. On failure: increment attempts, clear lock, log error
```

Batch optimization: collect all upsert IDs, fetch decisions in one query, single Qdrant Upsert RPC with all points.

---

## 4. Search Path Changes

### `decisions.Service` gains optional `Searcher`

```go
type Service struct {
    db         *storage.DB
    embedder   embedding.Provider
    searcher   search.Searcher     // nil = Qdrant disabled, pgvector only
    billingSvc *billing.Service
    logger     *slog.Logger
}
```

### Modified `Search()` method

```
1. Generate embedding from query (Ollama/OpenAI — unchanged)
2. If embedding is non-zero AND searcher != nil AND searcher.Healthy():
   a. Query Qdrant (org_id filter + structured filters, over-fetch limit*3)
   b. Fetch full records from Postgres via GetDecisionsByIDs (with valid_to IS NULL)
   c. Re-score with quality + recency weighting
   d. On any Qdrant error: log warning, fall back to pgvector
3. Else: fall back to pgvector (existing SearchDecisionsByEmbedding or SearchDecisionsByText)
```

Same pattern for `Check()`.

### New storage method: `GetDecisionsByIDs`

```go
func (db *DB) GetDecisionsByIDs(ctx context.Context, orgID uuid.UUID, ids []uuid.UUID) ([]model.Decision, error)
```

Batch fetch by `id = ANY($2)` with `org_id` and `valid_to IS NULL` filters. Uses existing `scanDecision` helper.

---

## 5. Write Path Changes

### `CreateTraceTx` (storage/trace.go)

Add inside the existing transaction, after decision insert, before commit:

```go
if d.Embedding != nil {
    tx.Exec(ctx,
        `INSERT INTO search_outbox (decision_id, org_id, operation)
         VALUES ($1, $2, 'upsert') ON CONFLICT (decision_id, operation) DO NOTHING`,
        d.ID, params.OrgID)
}
```

### `ReviseDecision` — add two outbox entries (delete old + upsert new)

### `DeleteAgentData` — insert outbox `delete` entries for each decision ID being deleted in the transaction + `DELETE FROM search_outbox WHERE decision_id IN (SELECT id FROM decisions WHERE org_id = $1 AND agent_id = $2)`

Note: `DeleteByOrg` is reserved for full org deletion (GDPR). Agent-level deletion targets specific decision IDs only.

---

## 6. Evidence Search: Stays in pgvector

Evidence search (`SearchEvidenceByEmbedding`) stays in pgvector. It's lightly used, evidence records are children of decisions (not independently searched at scale), and adding a second Qdrant collection adds complexity for little benefit. Can be revisited later.

---

## 7. Config Additions

**`internal/config/config.go`:**

| Env Variable | Default | Description |
|-------------|---------|-------------|
| `QDRANT_URL` | `""` | Qdrant cluster URL (e.g. `https://xyz.cloud.qdrant.io:6333`). Empty = disabled. |
| `QDRANT_API_KEY` | `""` | API key for Qdrant Cloud |
| `QDRANT_COLLECTION` | `akashi_decisions` | Collection name |
| `AKASHI_OUTBOX_POLL_INTERVAL` | `1s` | Outbox worker poll interval |
| `AKASHI_OUTBOX_BATCH_SIZE` | `100` | Max outbox entries per poll |

Config parsing extracts host from `QDRANT_URL`, infers TLS from `https://` scheme, uses gRPC port 6334 (ignoring REST port in URL).

---

## 8. Initialization in main.go

After embedding provider, before decision service:

```go
var searcher search.Searcher
if cfg.QdrantURL != "" {
    idx, err := search.NewQdrantIndex(parsedConfig, logger)
    idx.EnsureCollection(ctx)
    searcher = idx
    outboxWorker = search.NewOutboxWorker(db.Pool(), idx, logger, ...)
    outboxWorker.Start(ctx)
}
decisionSvc := decisions.New(db, embedder, searcher, billingSvc, logger)
```

Shutdown: drain outbox worker before event buffer.

Health endpoint: report Qdrant status (`"connected"`, `"disconnected"`, or omitted if not configured).

---

## 9. Backfill Existing Decisions

One-time SQL to seed the outbox with all existing decisions:

```sql
INSERT INTO search_outbox (decision_id, org_id, operation)
SELECT id, org_id, 'upsert' FROM decisions
WHERE embedding IS NOT NULL AND valid_to IS NULL
ON CONFLICT (decision_id, operation) DO NOTHING;
```

The outbox worker processes these normally. No special backfill code.

---

## 10. Consistency Model

- **Postgres queries** (`/v1/query`, `/v1/recent`): Always consistent.
- **Semantic search** (`/v1/search`, `/v1/check` with query): Eventually consistent. ~1-2s lag after trace.
- **Text search** (`/v1/search` with `semantic: false`): Always consistent (stays in Postgres).
- **GetDecisionsByIDs applies `valid_to IS NULL`**: Catches decisions revised between Qdrant index and Postgres fetch.

---

## Implementation Order

### Phase 1: Foundation
1. `go get github.com/qdrant/go-client`
2. Add Qdrant config fields to `internal/config/config.go`
3. Write `migrations/017_search_outbox.sql`
4. Create `internal/search/search.go` (Searcher interface, Result type)
5. Create `internal/search/qdrant.go` (QdrantIndex + filter translation + re-scoring)
6. Tests: TestBuildQdrantFilter, TestReScore

### Phase 2: Outbox worker
1. Create `internal/search/outbox.go` (OutboxWorker with Start/Drain/poll loop)
2. Add outbox INSERT inside `CreateTraceTx` in `storage/trace.go`
3. Add `GetDecisionsByIDs` to `storage/decisions.go`
4. Tests: TestOutboxWorkerProcessesUpsert, TestOutboxDeduplication

### Phase 3: Search path integration
1. Add `Searcher` field to `decisions.Service`, modify constructor
2. Modify `Service.Search()` — Qdrant first, pgvector fallback
3. Modify `Service.Check()` — same pattern
4. Wire in `cmd/akashi/main.go` (init, start worker, shutdown)
5. Update health endpoint
6. Run backfill SQL
7. Tests: TestSearchFallbackToPgvector, TestSearchViaQdrant

### Phase 4: Deletion
1. Add `QdrantIndex.DeleteByOrg` to GDPR deletion flow in `storage/delete.go`
2. Add outbox entries for `ReviseDecision`
3. Tests: TestGDPRDeletion

---

## File Manifest

### New Files

| File | Phase | Description |
|------|-------|-------------|
| `internal/search/search.go` | 1 | Searcher interface, Result type |
| `internal/search/qdrant.go` | 1 | QdrantIndex implementation |
| `internal/search/qdrant_test.go` | 1 | Unit tests |
| `internal/search/outbox.go` | 2 | OutboxWorker |
| `internal/search/outbox_test.go` | 2 | Unit tests |
| `migrations/017_search_outbox.sql` | 1 | Outbox table |

### Modified Files

| File | Phase | Changes |
|------|-------|---------|
| `internal/config/config.go` | 1 | Add Qdrant + outbox config fields |
| `go.mod` / `go.sum` | 1 | Add `github.com/qdrant/go-client` |
| `.env` / `.env.example` | 1 | Already done — Qdrant URL + API key |
| `internal/storage/trace.go` | 2 | Add outbox INSERT inside `CreateTraceTx` |
| `internal/storage/decisions.go` | 2 | Add `GetDecisionsByIDs`; outbox INSERTs in `ReviseDecision` |
| `internal/service/decisions/service.go` | 3 | Add `Searcher` field; modify `Search()` and `Check()` |
| `cmd/akashi/main.go` | 3 | Init Qdrant, outbox worker, wire into decisionSvc, shutdown |
| `internal/server/handlers.go` | 3 | Health endpoint reports Qdrant status |
| `internal/storage/delete.go` | 4 | GDPR deletion calls Qdrant DeleteByOrg |

---

## Invariants

1. **Postgres is always the source of truth.** Qdrant is a derived index. If they disagree, Postgres wins.
2. **Every decision with a non-null embedding gets an outbox entry** inside the same transaction.
3. **The outbox worker is idempotent.** Qdrant upserts and deletes are idempotent.
4. **Search falls back to pgvector on any Qdrant failure.** Callers never see Qdrant errors.
5. **`org_id` is always the first filter in every Qdrant query.** No code path queries without it.
6. **`valid_to IS NULL` applied in Postgres hydration** catches stale Qdrant results.
7. **Outbox table stays in public schema** (not per-tenant). One worker polls one table.
8. **Dead-letter entries (attempts >= max) are kept** for inspection + alerting.

## Verification

1. `go build -tags ui ./cmd/akashi` — compiles with Qdrant client
2. `go test ./internal/search/...` — unit tests pass
3. `go test ./... -race` — all tests pass
4. Start server with `QDRANT_URL` set — logs `qdrant: enabled`
5. Start server without `QDRANT_URL` — logs `qdrant: disabled`, search uses pgvector
6. POST `/v1/trace` — decision appears in Qdrant within ~2s (check outbox drains)
7. POST `/v1/search` with `semantic: true` — results come from Qdrant (verify with debug logging)
8. Kill Qdrant → search falls back to pgvector transparently (check logs for fallback warning)
9. Backfill SQL → existing decisions appear in Qdrant search
10. GDPR delete → `Count` query returns 0 for deleted org_id
