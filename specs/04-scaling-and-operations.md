# SPEC-004: Scaling Strategy and Operations

**Status:** Draft
**Date:** 2026-02-03
**Depends on:** SPEC-001 (System Overview), SPEC-002 (Data Model), ADR-002 (Unified PostgreSQL)

---

## Overview

Phased scaling strategy that starts simple and adds complexity only when concrete performance triggers are hit. Single PostgreSQL 17 is the starting point (ADR-002), not a permanent constraint. The architecture is designed so that each phase is a natural evolution, not a rewrite.

## Performance Targets

| Metric | Phase 1 | Phase 2 | Phase 3 |
|--------|---------|---------|---------|
| Concurrent agents | 100-500 | 500-1,000 | 1,000+ |
| Event ingestion | 10-50K events/sec | 50-100K events/sec | 100K+ events/sec |
| Query latency (p95) | <50ms | <20ms | <20ms |
| Storage | Single NVMe | Single NVMe + compression | Multi-node |

## Phase 1: Single Node (MVP)

### Architecture

```
┌─────────────────┐
│     Agents      │
│  (100-500 conns)│
└────────┬────────┘
         │
┌────────▼────────┐
│   PgBouncer     │  ← Transaction pooling mode
│  (50-100 DB     │     1000 client conns → 50-100 DB conns
│   connections)  │
└────────┬────────┘
         │
┌────────▼────────┐
│ PostgreSQL 17   │  ← Single instance, 32+ cores, NVMe storage
│ + TimescaleDB   │  ← Hypertables for agent_events
│ + pgvector      │  ← HNSW indexes for semantic search
└─────────────────┘
```

### Key Optimizations

#### Batch Ingestion via COPY Protocol

The Go server buffers incoming events in memory and flushes to PostgreSQL using the COPY protocol (via `pgx.CopyFrom`). This achieves 3-4x throughput over individual INSERTs.

```
Ingestion pipeline:
  Agent → HTTP/MCP → Go server buffer (in-memory, per-connection)
    → Flush trigger: 1000 events OR 100ms timeout (whichever comes first)
    → pgx.CopyFrom("agent_events", columns, rows)
```

Design constraints:
- Buffer size capped at 1000 events per flush batch
- Flush timeout of 100ms ensures low latency for small-volume agents
- Each Go server instance maintains its own buffer (no shared state)
- On server shutdown, flush remaining buffer before exit (graceful drain)

#### Connection Pooling

PgBouncer in **transaction pooling mode**:

```ini
[pgbouncer]
pool_mode = transaction
max_client_conn = 2000
default_pool_size = 50
min_pool_size = 10
reserve_pool_size = 10
server_idle_timeout = 300
```

This maps 1000+ agent connections to 50-100 actual database connections.

#### PostgreSQL Tuning

Target: write-heavy workload with concurrent reads.

```ini
# Memory
shared_buffers = 8GB                  # 25% of RAM for 32GB instance
effective_cache_size = 24GB           # 75% of RAM
work_mem = 64MB                       # per-sort/hash operation

# WAL (critical for write throughput)
wal_buffers = 256MB
max_wal_size = 8GB
min_wal_size = 2GB
checkpoint_timeout = 15min
checkpoint_completion_target = 0.9

# Write-ahead log
wal_level = replica                   # enables streaming replication for Phase 2
synchronous_commit = off              # trade durability for throughput on events
                                      # (events are append-only, loss of <1s is acceptable)

# Parallelism
max_parallel_workers_per_gather = 4
max_parallel_workers = 8
max_worker_processes = 16

# Autovacuum (aggressive for high-insert tables)
autovacuum_vacuum_scale_factor = 0.01
autovacuum_analyze_scale_factor = 0.005
autovacuum_naptime = 10s
```

**Note on `synchronous_commit = off`**: This trades up to ~600ms of durability for significantly higher write throughput. For an append-only event log where individual events are low-value and easily re-sent, this is an acceptable tradeoff. Decisions table writes use `synchronous_commit = on` (set per-transaction) since decisions are higher-value.

#### TimescaleDB Configuration

```sql
-- Chunk interval: 1 day (adjust based on ingestion rate)
SELECT set_chunk_time_interval('agent_events', INTERVAL '1 day');

-- Compression: activate on chunks older than 7 days
SELECT add_compression_policy('agent_events', INTERVAL '7 days');

-- Retention: no deletion (all data retained indefinitely)
-- Compression handles storage growth (~90% reduction on old chunks)
```

### Migration Triggers to Phase 2

| Trigger | Threshold | Measurement |
|---------|-----------|-------------|
| Query latency degradation | p95 > 50ms sustained over 1 hour | Prometheus `kyoyu_query_duration_seconds` |
| Connection pool saturation | > 80% pool utilization sustained | PgBouncer stats |
| CPU utilization | > 70% sustained on DB instance | Node exporter |
| Replication demand | Need for isolated read workloads | Manual assessment |

---

## Phase 2: Read Replicas (CQRS)

### Architecture

```
┌─────────────────┐
│     Agents      │
└────────┬────────┘
         │
┌────────▼────────┐
│     PgCat       │  ← Multi-threaded pooler (replaces PgBouncer)
│  (read/write    │     Automatic read/write splitting
│   splitting)    │
└────────┬────────┘
         │
    ┌────┴─────────────────┐
    │                      │
┌───▼──────────┐  ┌───────▼──────────┐
│   Primary    │  │   Replica(s)     │
│  (writes)    │  │  (reads)         │
│  ingestion,  │  │  queries,        │
│  decisions   │  │  search,         │
│              │  │  subscriptions   │
└──────────────┘  └──────────────────┘
```

### Why PgCat over PgBouncer

- **Multi-threaded**: handles 100+ concurrent connections without single-thread bottleneck
- **Read/write splitting**: automatically routes SELECT to replicas, writes to primary
- **Prepared statement support**: native support in all modes

### PgCat Configuration

```toml
[general]
host = "0.0.0.0"
port = 6432

[pools.kyoyu]
pool_mode = "transaction"

[pools.kyoyu.shards.0]
servers = [
    ["primary.db.internal", 5432, "primary"],
    ["replica1.db.internal", 5432, "replica"],
    ["replica2.db.internal", 5432, "replica"],
]
```

### Streaming Replication

```sql
-- On primary: already enabled by wal_level = replica (Phase 1)
-- On replicas:
-- primary_conninfo = 'host=primary.db.internal port=5432 user=replicator'
-- Typical replication lag: <100ms
```

### Materialized View Refresh Strategy

Materialized views (conflict detection, agent state) refresh on replicas to avoid write-load on primary:

```sql
-- Periodic refresh (every 30 seconds) via pg_cron on replica
SELECT cron.schedule('refresh-conflicts', '*/30 * * * * *',
    $$REFRESH MATERIALIZED VIEW CONCURRENTLY decision_conflicts$$);
```

### Migration Triggers to Phase 3

| Trigger | Threshold | Action |
|---------|-----------|--------|
| Write throughput ceiling | Primary sustaining > 80% of max COPY throughput | Consider Citus |
| Storage exceeds NVMe capacity | > 80% disk on primary | Consider Citus or Neon |
| Read replica count | > 4 replicas needed | Reassess architecture |

---

## Phase 3: Horizontal Scaling

Two options, chosen based on workload characteristics at the time.

### Option A: Citus (Distributed PostgreSQL)

Shard `agent_events` and `decisions` across multiple nodes.

```sql
-- Distribute agent_events by agent_id
SELECT create_distributed_table('agent_events', 'agent_id');

-- Distribute decisions by agent_id (co-located with events)
SELECT create_distributed_table('decisions', 'agent_id');

-- Co-locate related tables
SELECT create_distributed_table('alternatives', 'decision_id',
    colocate_with => 'decisions');
SELECT create_distributed_table('evidence', 'decision_id',
    colocate_with => 'decisions');
```

**When to choose**: sustained write throughput > 200K events/sec, need for horizontal write scaling.

### Option B: Neon (Serverless PostgreSQL)

Migrate to Neon for automatic compute scaling and storage separation.

**When to choose**: bursty workloads with unpredictable scaling needs, desire for operational simplicity, cost optimization for variable load.

---

## Observability

### Metrics (emitted via OTEL)

#### Ingestion Metrics

| Metric | Type | Labels |
|--------|------|--------|
| `kyoyu_events_ingested_total` | Counter | `agent_id`, `event_type` |
| `kyoyu_events_batch_size` | Histogram | |
| `kyoyu_events_batch_flush_duration_seconds` | Histogram | |
| `kyoyu_events_buffer_size` | Gauge | |
| `kyoyu_copy_errors_total` | Counter | `error_type` |

#### Query Metrics

| Metric | Type | Labels |
|--------|------|--------|
| `kyoyu_query_duration_seconds` | Histogram | `query_type` (`structured`, `temporal`, `search`) |
| `kyoyu_query_results_total` | Histogram | `query_type` |
| `kyoyu_search_similarity_score` | Histogram | |

#### Connection Metrics

| Metric | Type | Labels |
|--------|------|--------|
| `kyoyu_active_connections` | Gauge | `pool` |
| `kyoyu_pool_utilization_ratio` | Gauge | `pool` |
| `kyoyu_auth_failures_total` | Counter | `reason` |

#### Storage Metrics

| Metric | Type | Labels |
|--------|------|--------|
| `kyoyu_db_table_size_bytes` | Gauge | `table` |
| `kyoyu_compression_ratio` | Gauge | `table` |
| `kyoyu_chunk_count` | Gauge | `state` (`compressed`, `uncompressed`) |

### Traces (emitted via OTEL)

Each API request generates an OTEL span:

```
kyoyu.api.trace         (POST /v1/trace)
  ├── kyoyu.validate    (input validation)
  ├── kyoyu.embed       (embedding generation)
  ├── kyoyu.ingest      (COPY to PostgreSQL)
  └── kyoyu.notify      (SSE subscription fanout)

kyoyu.api.query         (POST /v1/query)
  ├── kyoyu.authz       (permission check)
  └── kyoyu.db.query    (PostgreSQL query execution)

kyoyu.api.search        (POST /v1/search)
  ├── kyoyu.embed       (query embedding)
  ├── kyoyu.authz       (permission check)
  └── kyoyu.db.search   (pgvector HNSW search)
```

### Logging (structured, via slog)

```json
{
  "level": "info",
  "msg": "batch flushed",
  "component": "ingestion",
  "batch_size": 847,
  "flush_duration_ms": 12,
  "agent_id": "underwriting-agent",
  "trace_id": "abc123"
}
```

Log levels:
- `error`: failures requiring attention (DB connection loss, auth failures)
- `warn`: degraded performance, approaching thresholds
- `info`: normal operations (batch flushes, queries served)
- `debug`: detailed execution (individual event processing, query plans)

### Alerting Thresholds

| Alert | Condition | Severity |
|-------|-----------|----------|
| High query latency | p95 > 50ms for 5 minutes | Warning |
| Critical query latency | p95 > 100ms for 2 minutes | Critical |
| Ingestion backlog | Buffer size > 5000 events for 30 seconds | Warning |
| Connection pool saturation | Utilization > 80% for 5 minutes | Warning |
| Storage growth anomaly | Daily growth > 2x 7-day average | Warning |
| Compression stalled | Uncompressed chunks > 14 days old | Warning |
| Auth failures spike | > 100 failures in 1 minute | Critical |

---

## Deployment

### Docker Compose (Development)

```yaml
services:
  postgres:
    image: timescale/timescaledb:latest-pg17
    environment:
      POSTGRES_DB: kyoyu
      POSTGRES_USER: kyoyu
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}
    ports:
      - "5432:5432"
    volumes:
      - pgdata:/var/lib/postgresql/data
      - ./docker/init.sql:/docker-entrypoint-initdb.d/init.sql
    command: >
      postgres
        -c shared_buffers=2GB
        -c effective_cache_size=6GB
        -c wal_buffers=64MB
        -c max_wal_size=4GB
        -c synchronous_commit=off
        -c max_connections=200

  pgbouncer:
    image: edoburu/pgbouncer:latest
    environment:
      DATABASE_URL: postgres://kyoyu:${POSTGRES_PASSWORD}@postgres:5432/kyoyu
      POOL_MODE: transaction
      MAX_CLIENT_CONN: 1000
      DEFAULT_POOL_SIZE: 50
    ports:
      - "6432:6432"

  kyoyu:
    build: .
    environment:
      DATABASE_URL: postgres://kyoyu:${POSTGRES_PASSWORD}@pgbouncer:6432/kyoyu
      KYOYU_PORT: 8080
      KYOYU_JWT_SECRET: ${JWT_SECRET}
      OTEL_EXPORTER_OTLP_ENDPOINT: http://otel-collector:4317
    ports:
      - "8080:8080"

volumes:
  pgdata:
```

### Production Checklist

- [ ] NVMe storage for PostgreSQL data directory
- [ ] `wal_level = replica` enabled (for future read replicas)
- [ ] PgBouncer deployed between application and database
- [ ] TimescaleDB compression policy active
- [ ] OTEL collector configured for metrics and traces
- [ ] JWT secret stored in secrets manager (not environment variable)
- [ ] Database credentials rotated, stored in secrets manager
- [ ] Backup strategy: WAL archiving + periodic base backups
- [ ] Monitoring dashboards for all metrics above
- [ ] Alerting configured for all thresholds above

## References

- ADR-002: Unified PostgreSQL storage
- SPEC-001: System Overview
- SPEC-002: Data Model
- RudderStack: 100K events/sec on single PostgreSQL (rudderstack.com/blog/scaling-postgres-queue/)
- TimescaleDB compression: 90-95% reduction in production
- PgCat vs PgBouncer: pganalyze.com/blog/5mins-postgres-pgcat-vs-pgbouncer
