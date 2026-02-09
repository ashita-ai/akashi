# ADR-002: Unified PostgreSQL storage with optional Qdrant acceleration

**Status:** Accepted (revised 2026-02-08)
**Date:** 2026-02-03
**Revised:** 2026-02-08

## Context

Akashi needs vector search (semantic similarity over decisions and evidence), time-series ingestion (append-only events), graph traversal (decision chains), and structured queries (metadata filters). The typical approach uses 3-4 specialized databases.

Since the original ADR (2026-02-03), multi-tenancy and the OSS/cloud split introduced a new constraint: the self-hosted OSS version must work with a single Postgres instance and zero additional infrastructure, while the cloud version must handle filtered vector search across millions of decisions spanning hundreds of tenants.

## Decision

**PostgreSQL is the source of truth for all data.** Every record lives in Postgres first. No data exists only in an external index.

**pgvector ships out of the box.** The OSS, single-tenant deployment uses pgvector with HNSW indexes for all vector search. This covers the common case: a team running 1-50 agents with up to hundreds of thousands of decisions. No additional infrastructure required.

**Qdrant is an optional, derived acceleration layer.** For multi-tenant cloud deployments where filtered vector search across millions of rows exceeds pgvector's practical limits, Qdrant provides a shared collection with org_id payload filtering. Qdrant is eventually consistent with Postgres via an outbox worker pattern. If Qdrant is unavailable, queries fall back to text search (ILIKE keyword matching) transparently.

The storage architecture:

| Concern | Technology | Deployment |
|---------|-----------|------------|
| Source of truth | PostgreSQL 17 | Always |
| Embeddings (source of truth) | pgvector column | Always (stored for Qdrant sync, not indexed for search) |
| Vector search | Qdrant | Optional (cloud, multi-tenant) |
| Time-series events | TimescaleDB | Always |
| Flexible metadata | JSONB + GIN indexes | Always |
| Graph traversal | Recursive CTEs | Always |

## Rationale

### Why pgvector out of the box

- Zero additional infrastructure for self-hosted users. `docker compose up` gives you vector search.
- pgvector with HNSW: 471 QPS at 99% recall on 50M vectors (pgvectorscale). Sufficient for single-tenant workloads well beyond MVP scale.
- Hybrid queries are the killer feature: vector similarity + metadata filter + time range + org isolation in a single SQL query. No cross-database joins.
- One database to monitor, back up, tune, and secure for the common case.

### Why Qdrant at scale

- Multi-tenant filtered search (WHERE org_id = X AND embedding <=> query) degrades in pgvector when the org filter is highly selective against a large shared index. Qdrant's payload-indexed filtering handles this natively.
- Qdrant's quantization (scalar/product) reduces memory footprint at 1M+ vectors with minimal recall loss.
- Horizontal scaling: Qdrant shards across nodes. pgvector is bound to a single Postgres instance.

### Why Postgres stays as source of truth

- Bi-temporal model (valid_from/valid_to + transaction_time) requires transactional guarantees that Qdrant doesn't provide.
- Evidence, alternatives, conflicts, and access grants all live in Postgres. Decisions reference them via foreign keys. Moving decisions out of Postgres would break referential integrity.
- Qdrant is a search accelerator, not a database. If Qdrant loses data, it can be rebuilt from Postgres. The reverse is not true.

## Architecture

### Searcher interface

All vector search goes through the `Searcher` interface in `internal/search/search.go`. The implementation:

- `QdrantIndex` (`internal/search/qdrant.go`) — queries Qdrant Cloud. Configured at startup when Qdrant credentials are present.

When Qdrant is not configured or is unhealthy, the service layer (`internal/service/decisions/service.go:Search()`) falls back to text search via `SearchDecisionsByText` in `internal/storage/decisions.go`, which uses ILIKE keyword matching. Application code calls the service layer and is unaware of which path serves results.

### Qdrant sync (cloud only)

A search outbox table in Postgres captures decision mutations. A background worker reads the outbox and syncs to Qdrant. Delivery semantics are at-least-once with idempotent upserts. On Qdrant downtime, the outbox accumulates and the worker drains it on reconnect. Meanwhile, text search handles all queries transparently.

### Fallback behavior

If Qdrant returns an error, is unreachable, or is not configured, the service layer falls back to text search (ILIKE keyword matching in `internal/storage/decisions.go:SearchDecisionsByText`). This is transparent to the caller. Fallback events are logged as warnings for monitoring.

## Migration triggers for further specialization

| Trigger | Threshold | Action |
|---------|-----------|--------|
| Event ingestion rate | >1M events/sec sustained | Evaluate ClickHouse for events |
| Graph query workload | >50% of queries, >10M nodes, deep traversal | Evaluate Neo4j |

The vector search trigger from the original ADR has been exercised — Qdrant was added for multi-tenant scale.

## Consequences

- Core storage in `internal/storage/`, search abstraction in `internal/search/`.
- Migrations are forward-only SQL files in `migrations/`.
- The `Searcher` interface abstracts vector search backend selection.
- Docker Compose for OSS bundles Postgres 17 + pgvector + TimescaleDB. No Qdrant.
- Cloud deployment adds Qdrant as a separate service with outbox-based sync.
- The pgvector HNSW index on `decisions.embedding` was dropped in migration 017 (Qdrant replaces it). The `evidence.embedding` HNSW index remains. The `decisions.embedding` column is retained as source of truth for Qdrant outbox sync.

## References

- Spec 07: Qdrant vector search (internal/specs/07-qdrant-vector-search.md)
- pgvector benchmarks: jkatz05.com/post/postgres/pgvector-performance-150x-speedup/
- TimescaleDB benchmarks: timescale.com/blog/timescaledb-vs-6a696248104e/
