# ADR-012: Transactional outbox for PostgreSQL → Qdrant synchronization

**Status:** Accepted
**Date:** 2026-04-03

## Context

Akashi maintains a vector search index in Qdrant alongside its authoritative PostgreSQL store. Keeping the two in sync requires solving a dual-write problem: a decision inserted into PostgreSQL must eventually appear in Qdrant, and a decision deleted from PostgreSQL must eventually disappear from Qdrant. Direct synchronous writes to both systems inside the HTTP request path would couple availability — a Qdrant outage would reject trace ingestion even though PostgreSQL is healthy.

## Decision

Use a **transactional outbox** pattern. Writers enqueue sync operations into a `search_outbox` table within the same PostgreSQL transaction that creates or deletes a decision. A background worker polls the outbox and applies changes to Qdrant asynchronously.

### Outbox table design

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
```

A unique partial index on `(decision_id, operation)` makes enqueue idempotent — re-inserting the same decision+operation resets `attempts` to 0 and clears `locked_until` via `ON CONFLICT ... DO UPDATE`.

### Entry points

Outbox entries are created by four producers:

1. **Decision creation** (`storage/decisions.go`): `queueSearchOutbox` called within the insert transaction.
2. **Agent context updates** (migration 074): a PostgreSQL trigger queues an `upsert` when the `agent_context` column changes on an existing decision, ensuring corrected metadata reaches Qdrant.
3. **Data deletion** (`storage/delete.go`): queues `delete` operations *before* removing decision rows, so the worker can reference the decision ID in the Qdrant delete call.
4. **Retention purge** (`storage/retention.go`): same pattern as data deletion, applied in batch during periodic retention enforcement.

### Worker processing

The outbox worker (`search/outbox.go`) runs a poll loop:

1. **SELECT & LOCK**: fetch up to `batchSize` entries ordered by `created_at ASC`, using `FOR UPDATE SKIP LOCKED` for distributed safety.
2. **Acquire lock**: set `locked_until = now() + 60s` on selected entries. The 60-second lock exceeds the 30-second batch timeout, preventing lock expiry mid-processing.
3. **Partition by operation**: separate entries into upserts and deletes.
4. **Process upserts**: fetch decision data from PostgreSQL, build Qdrant points, call `index.Upsert()` with `Wait: true`.
5. **Process deletes**: call `index.DeleteByIDs()` with `Wait: true`.
6. **On success**: delete processed entries from the outbox.

The worker is optional — it only starts when Qdrant is configured.

### Retry and failure handling

**Exponential backoff**: failed entries are deferred with `locked_until = now() + LEAST(2^(attempts+1), 300) seconds`, capping at 5 minutes.

**Embedding deferral**: decisions that lack embeddings (embedding backfill hasn't run yet) are deferred with a fixed 30-minute backoff rather than treated as failures. This avoids burning retry attempts while the embedding pipeline catches up.

**Dead-letter threshold**: entries exceeding 10 attempts are logged as dead-letter entries. A hourly cleanup job archives entries older than 7 days into `search_outbox_dead_letters` (an immutable audit table protected by triggers preventing UPDATE/DELETE). This preserves evidence for post-mortem investigation while clearing the active outbox.

**Dead-letter archival** uses an atomic CTE (`WITH candidates AS ... DELETE ... RETURNING`) to prevent race conditions between concurrent workers.

### Observability

An OTEL gauge (`akashi.outbox.depth`) exposes estimated queue depth via `pg_class.reltuples`, avoiding expensive `COUNT(*)` queries during Qdrant outages when the outbox may have many pending entries.

### Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `AKASHI_OUTBOX_POLL_INTERVAL` | 1s | How often the worker checks for pending entries |
| `AKASHI_OUTBOX_BATCH_SIZE` | 100 | Maximum entries processed per poll cycle |

## Rationale

**Why outbox over Change Data Capture (CDC)?**

CDC (e.g., Debezium) would capture WAL changes and stream them to Qdrant. This adds operational complexity (Kafka or equivalent, connector management, schema registry) that is disproportionate for a single sync target. The outbox table is self-contained within PostgreSQL, requires no external infrastructure, and gives explicit control over retry semantics and dead-lettering.

**Why polling over LISTEN/NOTIFY?**

LISTEN/NOTIFY would reduce latency but adds a failure mode: if the notification is missed (connection drop, worker restart), the entry silently stalls. Polling is self-healing — a restarted worker immediately picks up pending entries. The 1-second default poll interval is an acceptable latency trade-off for ingestion workloads.

**Why `FOR UPDATE SKIP LOCKED`?**

Multiple worker instances (typical in Kubernetes deployments) can safely run concurrently. `SKIP LOCKED` prevents contention — each worker takes the next available batch without blocking on entries held by another worker. The 60-second lock timeout acts as a lease: if a worker crashes, its entries become available after the lock expires.

## Consequences

- Trace ingestion remains available during Qdrant outages. Outbox entries accumulate in PostgreSQL and drain automatically on recovery.
- Search results may lag behind PostgreSQL by up to the poll interval (1s default) plus processing time. This is acceptable because search is advisory, not authoritative.
- The outbox table grows during extended Qdrant outages. The pending index (`WHERE attempts < 10`) keeps poll queries efficient regardless of table size.
- Manual re-sync is possible by inserting upsert entries for all current decisions, using the idempotent `ON CONFLICT` clause.
- Dead-letter entries require operator attention — they indicate either a persistent Qdrant issue or a data problem (e.g., malformed embedding) that retries cannot resolve.

## References

- ADR-002: Unified PostgreSQL storage (outbox lives in the same database as decisions)
- ADR-006: Embedding provider chain (embeddings consumed by upsert processing)
