# ADR-002: Unified PostgreSQL storage with Qdrant for vector search

**Status:** Accepted (revised 2026-02-19)
**Date:** 2026-02-03
**Revised:** 2026-02-08, 2026-02-19

## Context

Akashi needs vector search (semantic similarity over decisions and evidence), time-series ingestion (append-only events), graph traversal (decision chains), and structured queries (metadata filters). The typical approach uses 3-4 specialized databases.

Since the original ADR (2026-02-03), multi-tenancy and the OSS/cloud split introduced a new constraint: the self-hosted OSS version must work with minimal infrastructure, while the cloud version must handle filtered vector search across millions of decisions spanning hundreds of tenants.

As of 2026-02-19, a full audit confirmed that the `Searcher` interface has only one implementation (`QdrantIndex`) — no pgvector-backed `Searcher` has ever existed. All ANN queries for user-facing search go through Qdrant; when Qdrant is unavailable the service falls back to text search. Additionally, three pgvector HNSW indexes (`idx_decisions_embedding`, `idx_decisions_outcome_embedding`, `idx_evidence_embedding`) remain in the schema but serve only internal conflict detection queries, not user-facing search. These indexes impose ~8.5 GB RAM cost at 1M decisions without providing a search fallback. See issues #192 and #203.

## Decision

**PostgreSQL is the source of truth for all data.** Every record lives in Postgres first. No data exists only in an external index.

**Qdrant owns all ANN (approximate nearest neighbor) search.** Both cloud and self-hosted deployments use Qdrant for vector search. The OSS docker-compose bundles Qdrant. There is no pgvector HNSW search path for user-facing queries.

**pgvector stores embedding columns; HNSW indexes are dropped.** The `decisions.embedding` and `decisions.outcome_embedding` columns are retained as source of truth for Qdrant outbox sync and pairwise similarity computation on fetched candidate sets. The HNSW indexes on these columns are being removed (tracked in #192) — they impose large RAM costs for queries that should go to Qdrant.

**Text search is the explicit, documented fallback when Qdrant is unavailable.** This is not a bug. When Qdrant is down, `akashi_check` degrades to keyword search. A response header (`X-Search-Backend`) distinguishes degraded responses. Fallback events are logged as warnings.

The storage architecture:

| Concern | Technology | Deployment |
|---------|-----------|------------|
| Source of truth | PostgreSQL 18 | Always |
| Embeddings (source of truth) | pgvector column (no HNSW index) | Always |
| Vector search | Qdrant | Always (OSS docker-compose + cloud) |
| Fallback search | PostgreSQL full-text (tsvector/tsquery) | When Qdrant unavailable |
| Time-series events | TimescaleDB | Always |
| Flexible metadata | JSONB + GIN indexes | Always |
| Graph traversal | Recursive CTEs | Always |

## Rationale

### Why Qdrant for all ANN

- **No pgvector Searcher implementation exists.** The code has always fallen back to text search when Qdrant is unavailable. Formalizing Qdrant as the required ANN layer matches reality.
- **RAM economics.** pgvector HNSW at 1024 dimensions requires ~4.25 GB per 1M vectors *per index*. With two embedding columns on `decisions` plus `evidence`, this exceeds 8 GB at 1M decisions — forcing Postgres instance upsizing that costs more than running a Qdrant node.
- **Multi-tenant filtered search.** pgvector HNSW degrades for selective filters (small `org_id` populations) because the graph traversal backtracks to find enough neighbors inside the filter. Qdrant's payload-indexed filtering handles this without recall degradation.
- **Quantization.** Qdrant scalar quantization reduces memory 4x with <1% recall loss on 1024-dim vectors, bringing 1M decision index memory to ~1 GB.

### Why pgvector stays for storage

- Bi-temporal model (valid_from/valid_to + transaction_time) requires transactional guarantees that Qdrant doesn't provide.
- Evidence, alternatives, conflicts, and access grants all live in Postgres. Moving decisions out would break referential integrity.
- Qdrant is a search accelerator, not a database. If Qdrant loses data, it can be rebuilt from Postgres. The reverse is not true.
- Pairwise `outcome_embedding <=> d2.outcome_embedding` comparisons on fetched candidate sets (used by conflict scorer after Qdrant returns candidate IDs) happen in application code against vectors fetched from Postgres. No HNSW needed for this — it's at most 50 comparisons on rows already in memory.

### Why text search is an acceptable degraded fallback

`akashi_check` is a best-effort tool — it improves decision quality but does not gate decisions. A degraded response that misses some relevant precedents is better than no response. Text search handles the common case of keyword-heavy decision types and agent IDs well. The `X-Search-Backend` header allows callers to detect degraded mode and adjust their trust in results accordingly.

## Architecture

### Searcher interface

All vector search goes through the `Searcher` interface in `internal/search/search.go`:

- `QdrantIndex` (`internal/search/qdrant.go`) — primary ANN search. Required for production deployments.

Conflict detection ANN (candidate discovery for scorer) will migrate from `storage.FindSimilarDecisionsByEmbedding` to `QdrantIndex.Search` as part of #192. Until then, the conflict scorer still uses the pgvector `<=>` operator directly.

When Qdrant is not configured or is unhealthy, the service layer falls back to text search via `SearchDecisionsByText` in `internal/storage/decisions.go`.

### Qdrant sync

A search outbox table in Postgres captures decision mutations. A background worker reads the outbox and syncs to Qdrant. Delivery semantics are at-least-once with idempotent upserts. On Qdrant downtime, the outbox accumulates and the worker drains it on reconnect.

### Fallback behavior

If Qdrant returns an error, is unreachable, or is not configured, the service layer falls back to PostgreSQL full-text search (`tsvector/tsquery`). The response includes `X-Search-Backend: text` so callers can detect degraded mode.

## Open work

| Issue | Description |
|-------|-------------|
| #192 | Drop pgvector HNSW indexes; migrate conflict scorer ANN to Qdrant |
| #198 | GetConsensusScoresBatch O(N×50) ANN per page — becomes N Qdrant queries after #192 |
| #203 | Add Qdrant to OSS docker-compose (prerequisite for #192) |

## Migration triggers for further specialization

| Trigger | Threshold | Action |
|---------|-----------|--------|
| Event ingestion rate | >1M events/sec sustained | Evaluate ClickHouse for events |
| Graph query workload | >50% of queries, >10M nodes, deep traversal | Evaluate Neo4j |

## Consequences

- Core storage in `internal/storage/`, search abstraction in `internal/search/`.
- Migrations are forward-only SQL files in `migrations/`.
- The `Searcher` interface abstracts the vector search backend.
- Docker Compose for OSS will bundle Qdrant (tracked in #203). Until #203 is resolved, OSS users without Qdrant get text search.
- The `decisions.embedding` and `decisions.outcome_embedding` columns are retained. Their HNSW indexes will be dropped in a future migration (#192).

## References

- ADR-003: Event-sourced data model with bi-temporal modeling (data model built on this storage layer)
- ADR-007: Dual Postgres connection strategy (pooled queries + direct LISTEN/NOTIFY)
- ADR-008: TimescaleDB for event ingestion (time-series partition of this unified storage)
- Implementation: `internal/storage/` (queries), `internal/search/` (Qdrant + fallback)
- Issues: #192 (HNSW drop), #198 (consensus scoring O(N)), #203 (docker-compose Qdrant)
