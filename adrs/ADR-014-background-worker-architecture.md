# ADR-014: Background worker architecture

**Status:** Accepted
**Date:** 2026-04-03

## Context

Akashi runs numerous background tasks: conflict detection, integrity proof generation, search index synchronization, retention enforcement, rate limit cleanup, and more. These tasks have different cadences (1 second to 24 hours), different failure characteristics (some are critical, others are best-effort), and different shutdown requirements (some must drain buffered data, others can stop immediately). We need a lifecycle model that starts, runs, and stops these workers reliably without coupling their failure modes.

## Decision

Organize background work into four tiers based on lifecycle semantics, with a phased shutdown sequence that respects data dependencies between tiers.

### Tier 1: Infrastructure workers (started first, stopped last)

These workers provide foundational services consumed by other tiers:

| Worker | Purpose | Internal goroutine |
|--------|---------|-------------------|
| `trace.Buffer` | In-memory event buffer with periodic flush to PostgreSQL via COPY; optional WAL for crash durability | `flushLoop` |
| `search.OutboxWorker` | Polls `search_outbox` table, syncs changes to Qdrant (conditional — only if Qdrant configured) | `pollLoop` |
| `server.Broker` | SSE broker for PostgreSQL LISTEN/NOTIFY; fans out conflict/decision notifications to subscribers (conditional — only if notify connection available) | notification loop |

Infrastructure workers are started before the main loop goroutines and drained explicitly during shutdown, each with a dedicated timeout.

### Tier 2: Main loop goroutines (11 registered loops)

All main loops are registered to a shared `bgLoops` `sync.WaitGroup` and launched in `App.Run()`. Each loop runs on a configurable ticker interval and is wrapped by the `runLoop` helper, which provides panic recovery with stack trace logging — a single bad tick cannot kill the goroutine.

| Loop | Purpose | Default interval | Disableable |
|------|---------|-----------------|-------------|
| `conflictBackfillLoop` | One-shot backfill of conflict embeddings at startup; warms Ollama model | Once | No |
| `conflictRefreshLoop` | Polls for new conflicts, fires async `OnConflictDetected` hooks | Configurable | No |
| `integrityProofLoop` | Builds Merkle tree integrity proofs (ADR-013) | 5 min | No |
| `integrityAuditLoop` | Samples one org per tick, audits 10 recent proofs | 15 min | No |
| `integrityFullAuditLoop` | Exhaustive audit across all orgs | 24 hours | Yes (interval ≤ 0) |
| `idempotencyCleanupLoop` | Removes expired idempotency keys | Configurable | No |
| `hookCheckCleanupLoop` | Cleans up IDE hook check results | 10 min (hardcoded) | No |
| `retentionLoop` | Enforces per-org data retention policies | 24 hours | Yes (interval ≤ 0) |
| `claimEmbeddingRetryLoop` | Retries failed search index embedding generation | Configurable | Yes (interval ≤ 0) |
| `percentileRefreshLoop` | Recomputes citation percentile breakpoints for search normalization | Configurable | Yes (interval ≤ 0) |
| `autoResolveLoop` | Auto-resolves conflicts per org policies | Configurable | Yes (interval ≤ 0) |

### Tier 3: Cleanup goroutines (self-managed lifecycle)

These are background eviction loops owned by their parent structs, not registered to `bgLoops`:

| Component | Purpose | Interval | Shutdown |
|-----------|---------|----------|----------|
| `ratelimit.MemoryLimiter` | Evicts stale rate limit token buckets | 1 min | `Close()` |
| `authz.GrantCache` | Evicts expired RBAC grant cache entries | 1 min | `Close()` |
| `server.SignupLimiter` | Per-IP signup rate limiting (optional) | Internal | `CloseSignupLimiter()` |

These are closed explicitly during the cleanup phase of shutdown, after all other workers have drained.

### Tier 4: Async on-demand goroutines (spawned per-event)

| Type | Purpose | Tracking |
|------|---------|----------|
| Post-trace async work | Claim generation, conflict scoring, subscriber notification | Tracked via `decisions.Service.asyncWg`; drained explicitly during shutdown |
| `OnConflictDetected` hooks | Async hook invocations per detected conflict | Spawned with `context.Background()` and 10s timeout; not awaited during shutdown |

Post-trace async work is tracked in a dedicated `sync.WaitGroup` separate from `bgLoops` and drained before the event buffer, ensuring all database writes complete before the connection pool closes.

### Panic recovery

The `runLoop` helper (`akashi.go`) wraps every main loop tick in `defer recover()`:

```go
func runLoop(ctx context.Context, name string, logger *slog.Logger, fn func(context.Context)) {
    defer func() {
        if r := recover(); r != nil {
            logger.Error("background loop panic", "loop", name,
                "panic", r, "stack", string(debug.Stack()))
        }
    }()
    fn(ctx)
}
```

A panic in one tick is logged with a full stack trace and the loop continues on the next tick. This prevents a transient failure (e.g., a nil pointer from unexpected database state) from permanently disabling a background subsystem.

### Shutdown sequence

Shutdown proceeds in five ordered phases, each with a configurable timeout:

```
Phase 1: HTTP drain
  └─ Stop accepting new requests; wait for in-flight requests to complete
      Timeout: ShutdownHTTPTimeout

Phase 2: Async work drain
  └─ Wait for decisions.Service.asyncWg (claim generation, conflict scoring)
      Timeout: ShutdownAsyncDrainTimeout
      Why before buffer: async work may enqueue events into the buffer

Phase 3: Event buffer drain
  └─ Flush buffered events to PostgreSQL via COPY
      Timeout: ShutdownBufferDrainTimeout
      Why before outbox: flushed events may generate outbox entries

Phase 4: Search outbox drain
  └─ Process remaining Qdrant sync entries
      Timeout: ShutdownOutboxDrainTimeout

Phase 5: Background loop drain
  └─ Wait for bgLoops WaitGroup (all 11 loops + broker)
      Timeout: ShutdownLoopDrainTimeout
      Loops have already received cancellation; this waits for graceful exit

Phase 6: Cleanup
  └─ Close grant cache, rate limiter, signup limiter, Qdrant index, OTEL, database pool
      No timeout — these are fast, non-blocking closes
```

The ordering is intentional: each phase produces work consumed by the next. Async work (Phase 2) may buffer events. The buffer flush (Phase 3) may create outbox entries. The outbox drain (Phase 4) syncs those entries to Qdrant. Reversing this order would lose data.

## Rationale

**Why a shared WaitGroup over individual goroutine management?**

A WaitGroup provides a single synchronization point for "all loops have exited" without requiring individual goroutine handles or channels. The loops are homogeneous in lifecycle (all start together, all stop on context cancellation), so per-goroutine management would add complexity without benefit.

**Why phased shutdown over simultaneous cancellation?**

Simultaneous cancellation creates a race between producers and consumers. If the event buffer closes before async work finishes, in-flight events are lost. If the outbox worker stops before the buffer flushes, new outbox entries aren't synced. The phased approach respects the producer → consumer dependency chain.

**Why panic recovery instead of letting the goroutine die?**

A dead background goroutine is silent. The system continues to accept requests, but a critical subsystem (e.g., integrity proofs, conflict detection) stops functioning with no user-visible signal other than stale data. Recovery with logging keeps the subsystem alive and produces actionable log entries. The trade-off is that a persistently panicking loop will generate log noise, but this is preferable to silent degradation.

**Why separate WaitGroups for async work and background loops?**

Post-trace async work has a different shutdown constraint: it must complete *before* the event buffer drains, because it may enqueue events. Background loops have no such dependency — they can drain after the buffer. Using the same WaitGroup for both would prevent the phased ordering.

## Consequences

- Adding a new background loop requires: implementing the loop function, adding it to the `bgLoops` WaitGroup, and launching it in `App.Run()`. The `runLoop` helper provides panic recovery automatically.
- Disableable loops (interval ≤ 0) allow operators to turn off non-essential background work in resource-constrained deployments without code changes.
- Shutdown timeouts are independently configurable per phase, allowing operators to tune drain behavior for their deployment (e.g., longer outbox drain in environments with slow Qdrant).
- The `OnConflictDetected` hook goroutines are not awaited during shutdown — they use `context.Background()` with a 10-second timeout and may still execute briefly after the database pool closes. This is a known trade-off: tracking every hook invocation would add synchronization overhead to the conflict refresh hot path.
- Per-request HTTP goroutines are bounded by the HTTP drain timeout in Phase 1. Long-running requests that exceed this timeout are abandoned.

## References

- ADR-003: Event-sourced bi-temporal model (event buffer and flush pipeline)
- ADR-012: Outbox sync pattern (outbox worker lifecycle and drain)
- ADR-013: Merkle integrity proofs (integrity proof and audit loops)
