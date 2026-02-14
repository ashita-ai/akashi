# ADR-007: Dual Postgres connection strategy

**Status:** Accepted
**Date:** 2026-02-03

## Context

Akashi serves two fundamentally different Postgres workloads from a single process:

1. **Request-scoped queries and writes.** Every HTTP and MCP request executes short-lived SQL statements: reading decisions, inserting events via COPY, refreshing materialized views. These benefit from connection pooling because hundreds of concurrent requests can share a small pool of backend connections.

2. **Persistent LISTEN/NOTIFY subscriptions.** The SSE broker needs a long-lived connection that stays subscribed to the `akashi_decisions` and `akashi_conflicts` notification channels. Postgres delivers notifications only to the specific backend connection that executed LISTEN. This connection must remain open and dedicated for the lifetime of the process.

In production, PgBouncer sits in front of Postgres to manage connection pooling. PgBouncer operates in transaction-mode pooling by default, which means it reassigns backend connections across clients between transactions. This breaks LISTEN/NOTIFY: a client subscribes on backend connection A, but its next wait-for-notification call may land on backend connection B, which has no active subscriptions. The notification is delivered to connection A, which has no listener waiting.

This is not a bug in PgBouncer. It is an inherent limitation of multiplexing persistent, connection-scoped state through a stateless pooler.

## Decision

Akashi maintains two separate Postgres connection paths, configured via independent environment variables:

| Connection | Env var | Default port | Purpose |
|------------|---------|-------------|---------|
| Pooled | `DATABASE_URL` | 6432 (PgBouncer) | All reads, writes, COPY ingestion, migrations |
| Direct | `NOTIFY_URL` | 5432 (Postgres) | LISTEN/NOTIFY for real-time SSE |

The pooled connection uses `pgxpool.Pool` from the `jackc/pgx/v5` driver, which maintains a local pool of connections on top of PgBouncer's server-side pool. The direct connection uses a single `pgx.Conn` that bypasses PgBouncer entirely and connects straight to the Postgres backend.

The `storage.DB` struct owns both:

```go
type DB struct {
    pool           *pgxpool.Pool  // PgBouncer: all queries
    notifyConn     *pgx.Conn      // Direct: LISTEN/NOTIFY only
    notifyDSN      string
    notifyMu       sync.Mutex
    notifyGen      uint64         // incremented on every reconnect; protected by notifyMu
    listenChannels []string
    logger         *slog.Logger
}
```

The direct connection is optional. If `NOTIFY_URL` is empty, the SSE broker is disabled and the server operates without real-time push. This allows development and testing without a second connection path.

### Notification channels

Two channels are registered:

- `akashi_decisions` -- fired after a decision is created or revised.
- `akashi_conflicts` -- fired when the semantic conflict scorer inserts a new conflict.

Notification payloads are JSON containing at minimum an `org_id` field, which the SSE broker uses for tenant-scoped fan-out. Events with unparseable or missing `org_id` are dropped rather than broadcast, preventing cross-tenant data leakage.

### Reconnect strategy

The direct connection is a single persistent TCP session. If Postgres restarts, the network path breaks, or an idle timeout fires, the connection dies silently. The `WaitForNotification` call returns an error, triggering the reconnect path.

Reconnect uses exponential backoff with jitter:

- **Max retries:** 5
- **Base backoff:** 500ms
- **Backoff multiplier:** 2x per attempt (500ms, 1s, 2s, 4s, 8s before jitter)
- **Jitter:** uniform random in [0, backoff/2) added to each sleep
- **Channel re-subscription:** on successful reconnect, all previously tracked channels are re-LISTENed automatically

If all 5 attempts fail, the error propagates to the SSE broker, which logs a warning and retries on the next notification wait cycle. The broker does not crash the process; SSE subscribers simply stop receiving updates until the connection recovers.

The reconnect logic holds `notifyMu` for the duration of the retry sequence. This serializes reconnect attempts and prevents the broker from reading a half-initialized connection.

### Sending notifications

Outbound `pg_notify()` calls go through the pooled connection (PgBouncer), not the direct connection. This is correct because `pg_notify` is a regular SQL function call, not a session-scoped subscription. It executes in a single round-trip and does not require connection affinity.

```go
func (db *DB) Notify(ctx context.Context, channel, payload string) error {
    _, err := db.pool.Exec(ctx, "SELECT pg_notify($1, $2)", channel, payload)
    return err
}
```

## Rationale

### Why not a single direct connection for everything

Connection pooling is essential at scale. A direct Postgres connection per HTTP request (or even a small local pool without PgBouncer) exhausts Postgres's `max_connections` quickly. PgBouncer allows hundreds of application-level connections to share a small number of backend connections (typically 20-50), which is critical for Postgres, where each backend is a separate OS process consuming non-trivial memory.

### Why not run LISTEN/NOTIFY through PgBouncer

PgBouncer's transaction-mode pooling reassigns backend connections between transactions. LISTEN is a session-level command: the subscription is bound to the backend connection that executed it, not to the PgBouncer client connection. After a LISTEN, PgBouncer may route the next statement to a different backend, making `WaitForNotification` hang indefinitely on a connection with no subscriptions.

PgBouncer's session-mode pooling would fix this, but it defeats the purpose of pooling: each client holds a dedicated backend connection for its entire session, giving the same connection-per-client behavior as no pooler at all.

### Why not polling instead of LISTEN/NOTIFY

Polling the decisions table on a timer introduces latency (proportional to the poll interval), wastes database resources on empty queries, and scales poorly with the number of SSE subscribers. LISTEN/NOTIFY delivers sub-millisecond push notifications with zero database load when there are no events.

### Why one direct connection, not a pool

Only the SSE broker uses LISTEN/NOTIFY. A single goroutine calls `WaitForNotification` in a loop and fans out to all SSE subscribers via in-process channels. There is no concurrency on the listen side, so a pool of direct connections would be waste. The cost is exactly one additional Postgres backend process.

### Why the connection is optional

Development environments and test suites often run without PgBouncer. In that case, `DATABASE_URL` points directly to Postgres and can handle LISTEN/NOTIFY itself. Setting `NOTIFY_URL` to empty disables the dedicated connection and the SSE broker, which is acceptable for non-production use.

## Consequences

- **Operational:** production deployments require two Postgres connection strings. One pointing to PgBouncer (port 6432 by convention), one pointing directly to Postgres (port 5432). Monitoring should alert on the direct connection's health independently.
- **Resource cost:** one additional persistent Postgres backend connection per Akashi process. Negligible in practice.
- **Failure isolation:** a PgBouncer outage affects queries but not notifications. A direct-connection failure affects SSE but not queries. The two failure domains are independent.
- **Testing:** unit tests that do not need SSE can pass `""` for `notifyDSN` and skip the direct connection entirely.
- **Multi-tenancy:** notification payloads must include `org_id`. The broker enforces org-scoped delivery and drops events with missing tenant context, preventing cross-tenant leakage through the real-time channel.

## References

- ADR-002: Unified PostgreSQL storage (this decision implements the connection layer for that storage architecture)
- ADR-008: TimescaleDB for event ingestion (COPY-based writes go through the pooled connection)
- `internal/storage/pool.go` -- `DB` struct, `New()` constructor, `reconnectNotify()` backoff logic
- `internal/storage/notify.go` -- `Listen()`, `WaitForNotification()`, `Notify()` methods
- `internal/server/broker.go` -- SSE broker that consumes notifications and fans out to HTTP subscribers
- `internal/config/config.go` -- `DATABASE_URL` and `NOTIFY_URL` environment variable definitions
- PgBouncer documentation on LISTEN/NOTIFY limitations: pgbouncer.github.io/faq.html
