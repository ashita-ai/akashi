# ADR-008: TimescaleDB for event ingestion

**Status:** Accepted
**Date:** 2026-02-03

## Context

Akashi's event-sourced architecture (ADR-003) produces a high-volume, append-only stream of agent events. Every decision, tool call, reasoning step, and handoff generates at least one row in `agent_events`. These events are the source of truth for the entire system -- they are never mutated or deleted, and every audit query, replay, and conflict detection operation reads from them.

This workload has three defining characteristics:

1. **Append-only writes.** Events are inserted and never updated. Write throughput matters more than random-access latency.
2. **Time-ordered reads.** Nearly all queries filter or sort by `occurred_at`: "show events for this run in the last hour", "replay agent state at time T", "stream events since sequence N".
3. **Cold data dominance.** Events older than a few days are read infrequently but must remain queryable for audit and compliance. Storage cost for cold data is a long-term concern.

Vanilla PostgreSQL handles this adequately at low volumes, but table scans degrade as the event table grows past tens of millions of rows. Partitioning by hand is possible but requires maintaining partition creation, routing, and cleanup logic in application code or custom DDL.

## Decision

The `agent_events` table is a **TimescaleDB hypertable**, partitioned automatically by `occurred_at`. TimescaleDB manages chunk creation, query routing, and compression transparently. The application issues standard SQL -- no TimescaleDB-specific syntax in queries.

### Hypertable creation (001_initial.sql)

All schema definitions below are consolidated in `migrations/001_initial.sql`. Earlier drafts of this ADR referenced separate migrations (002, 010, 013, 014) that were merged into the single initial migration before first release.

```sql
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
```

The primary key includes `occurred_at` because TimescaleDB requires the partitioning column in the primary key. The `org_id` column provides multi-tenant isolation and is required on all inserts and filtered on all queries.

### Chunk interval

```sql
SELECT set_chunk_time_interval('agent_events', INTERVAL '1 day');
```

One-day chunks are the starting configuration. For Akashi's current traffic volume (~1K-100K events/day), this may produce small, sparse chunks. Spec 06a recommends monitoring chunk sizes for 30 days and increasing to 7 days if chunks are consistently under 100MB. This is a live operation that only affects future chunks.

### Compression

```sql
ALTER TABLE agent_events SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'agent_id,run_id',
    timescaledb.compress_orderby = 'occurred_at DESC'
);

SELECT add_compression_policy('agent_events', INTERVAL '7 days');
```

Chunks older than 7 days are compressed automatically. Segmenting by `agent_id` and `run_id` means queries that filter on these columns can skip irrelevant compressed segments entirely. Ordering by `occurred_at DESC` optimizes the common pattern of reading the most recent events first within a segment. Typical compression ratio is ~90%, meaning 10GB of raw event data compresses to ~1GB while remaining fully queryable via standard SQL.

### Event ordering

```sql
CREATE SEQUENCE event_sequence_num_seq START WITH 1;
```

Event ordering uses a Postgres SEQUENCE rather than `SELECT MAX(sequence_num) + 1`. The sequence provides globally unique, monotonically increasing values under concurrent access. Gaps in the sequence are harmless -- they indicate concurrent callers, not lost events. The application pre-allocates a batch of sequence numbers via `generate_series` before bulk insertion.

### Batch insertion (COPY protocol)

The `InsertEvents` function uses pgx's `CopyFrom`, which maps to the PostgreSQL COPY protocol. This bypasses per-row overhead (parsing, planning, WAL amplification from individual INSERTs) and is the fastest path for bulk event ingestion. A single-row `InsertEvent` variant exists for low-volume operations where COPY overhead is not justified.

### No foreign keys from the hypertable

TimescaleDB does not support foreign key constraints originating from hypertables. The `run_id` column in `agent_events` references `agent_runs(id)` conceptually, but referential integrity is enforced at the application layer. This is documented in migration 002:

```sql
-- FK constraints FROM hypertables are not supported by TimescaleDB,
-- so run_id integrity is enforced at the application layer.
```

### No retention policy yet

There is no `add_retention_policy` call. GDPR-compliant data deletion is planned but not yet scheduled. When implemented, it will use TimescaleDB's `drop_chunks` to remove data older than a configurable threshold. Until then, compression keeps storage costs manageable.

## Rationale

### Why TimescaleDB over vanilla Postgres partitioning

- **Automatic chunk management.** TimescaleDB creates, names, and routes to partitions without DDL triggers or cron jobs. Vanilla Postgres requires `CREATE TABLE ... PARTITION OF` for each new range, plus cleanup of old partitions.
- **Transparent compression.** Columnar compression on cold chunks reduces storage ~90% while keeping data queryable. Vanilla Postgres has no built-in columnar compression for partitions.
- **Query planning.** TimescaleDB's chunk exclusion prunes irrelevant partitions during planning. Vanilla Postgres partition pruning works but requires careful constraint configuration.
- **Performance.** TimescaleDB benchmarks demonstrate 20x faster inserts and orders-of-magnitude faster time-range queries compared to vanilla Postgres for time-series workloads, primarily due to chunk exclusion and compressed scan optimizations.

### Why TimescaleDB over a separate time-series database (ClickHouse, InfluxDB)

- **Unified stack.** ADR-002 establishes PostgreSQL as the single source of truth. TimescaleDB is a Postgres extension, not a separate database. It shares the same connection, transaction context, and backup strategy. Events can JOIN directly against `decisions`, `agent_runs`, and other tables without cross-database synchronization.
- **Operational simplicity.** One database to monitor, back up, failover, and secure. No ETL pipeline to maintain between an event store and a query database.
- **Transactional consistency.** Event insertion can participate in the same transaction as decision and run creation (used in `CreateTraceTx`). A separate time-series database would require distributed transactions or eventual consistency with compensating logic.
- **Sufficient scale.** TimescaleDB handles millions of events per day on a single node. ADR-002 identifies >1M events/sec sustained as the threshold for evaluating ClickHouse. Akashi is far below this threshold.

### Why segment compression by agent_id and run_id

The two most common query patterns are:

1. "Get all events for run X" -- filters on `run_id`.
2. "Get recent events for agent Y" -- filters on `agent_id` with a time range.

Segmenting compression by these columns means the decompressor can skip entire segments that don't match the filter predicate, avoiding full-chunk decompression for selective queries.

### Why COPY protocol over prepared INSERT statements

For batch ingestion of N events:

- **INSERT** (even batched): N round-trips of parse/bind/execute, or a single large multi-row INSERT that still requires per-row WAL entries.
- **COPY**: single protocol-level command, binary encoding, minimal WAL amplification. For batches of 50-500 events (typical trace size), COPY is 2-5x faster.

The tradeoff is that COPY does not return per-row conflict information (no `ON CONFLICT` clause), but since events are append-only with UUID primary keys, conflicts do not occur in practice.

## Consequences

- `agent_events` must not have foreign key constraints to other tables. Application code must enforce referential integrity for `run_id` and other cross-table references.
- The primary key must include `occurred_at` (the partitioning column). Lookups by `id` alone require a composite index or must also specify `occurred_at`.
- Indexes on hypertables are per-chunk, not global. Index creation is fast (only indexes the latest chunk), but total index count grows with chunk count.
- Compressed chunks are read-only. If a future requirement needs to mutate historical events (contradicting the append-only model), those chunks must be decompressed first.
- The chunk interval (currently 1 day) is a tuning knob that should be revisited after observing production traffic patterns.
- Retention policy implementation is deferred. Storage grows monotonically until GDPR deletion is implemented.

## References

- ADR-002: Unified PostgreSQL storage (parent architecture: Postgres as single source of truth)
- ADR-003: Event-sourced data model with bi-temporal modeling (data model that defines the event schema)
- ADR-007: Dual Postgres connection strategy (COPY writes use the pooled connection path)
- Migration: `migrations/001_initial.sql` (complete schema: tables, hypertable, compression, sequence, multi-tenancy)
- Implementation: `internal/storage/events.go` (COPY insertion, sequence reservation)
- TimescaleDB compression docs: docs.timescale.com/use-timescale/latest/compression/
