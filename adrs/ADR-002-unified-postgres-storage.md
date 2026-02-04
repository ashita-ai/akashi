# ADR-002: Unified PostgreSQL storage

**Status:** Accepted
**Date:** 2026-02-03

## Context

Kyoyu needs vector search (semantic similarity over decisions), time-series ingestion (append-only events), graph traversal (decision chains), and structured queries (metadata filters). The typical approach uses 3-4 specialized databases. We need to decide whether to go polyglot or unified.

## Decision

Single PostgreSQL 17 instance with extensions:
- **pgvector** (HNSW indexes) for semantic search
- **TimescaleDB** for time-series event ingestion and partitioning
- **JSONB** for facet-based extensibility
- **Recursive CTEs** for shallow graph traversal (1-3 hops)

Do NOT introduce Neo4j, Qdrant, Redis, ClickHouse, or any additional database without hitting a documented migration trigger.

## Rationale

**Performance is sufficient for MVP through early scale:**

- pgvector with HNSW: 471 QPS at 99% recall on 50M vectors (pgvectorscale). Matches Qdrant throughput, 75-79% cheaper than Pinecone.
- TimescaleDB: 20x faster inserts, 14,000x faster time-ordered queries vs vanilla Postgres. 90% compression on old chunks.
- Recursive CTEs: <10ms for 1-3 hop traversals on 100K nodes. Sufficient for decision chain queries.

**Operational simplicity:**

- One database to monitor, back up, tune, and secure.
- One connection pool. One migration system. One failure domain.
- Cost: ~$294/month unified vs $369-1,350/month for equivalent polyglot stack.

**Hybrid queries are the killer feature:**

- Vector similarity + metadata filter + time range in a single SQL query.
- No cross-database joins, no eventual consistency between systems.

## Migration Triggers

Split out a specialized database ONLY when concrete performance data shows:

| Trigger | Threshold | Action |
|---------|-----------|--------|
| Vector search latency | p95 >500ms with >100M vectors | Add dedicated vector DB |
| Event ingestion rate | >1M events/sec sustained | Add ClickHouse |
| Graph query workload | >50% of queries, >10M nodes, deep traversal | Add Neo4j |

## Consequences

- All storage code lives in `internal/storage/`.
- Migrations are forward-only SQL files in `migrations/`.
- Docker setup bundles Postgres 17 + pgvector + TimescaleDB.
- Must monitor query latency and ingestion throughput to know when triggers are hit.

## References

- Research: `ventures/specs/03-postgres-unified-storage.md`
- pgvector benchmarks: jkatz05.com/post/postgres/pgvector-performance-150x-speedup/
- TimescaleDB benchmarks: timescale.com/blog/timescaledb-vs-6a696248104e/
