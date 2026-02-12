# ADR-003: Event-sourced data model with bi-temporal modeling

**Status:** Accepted
**Date:** 2026-02-03
**Revised:** 2026-02-08

## Context

Akashi captures decision traces from AI agents. We need a data model that supports auditability (what happened and when), replay (reconstruct state at any point), and extensibility (new event types without schema changes).

## Decision

Event-sourced architecture with bi-temporal columns:

- **Append-only event log** (`agent_events`) is the source of truth. Events are never mutated or deleted.
- **Materialized views** provide current-state query performance.
- **Bi-temporal columns** on mutable tables: `valid_from`/`valid_to` (business time) + `transaction_time` (system time).
- **JSONB facets** for extensible event payloads (OpenLineage pattern).

### Core Tables

| Table | Purpose | Mutable? |
|-------|---------|----------|
| `agent_runs` | Top-level execution context | Yes (status transitions: running → completed/failed) |
| `agent_events` | Append-only event log (TimescaleDB hypertable) | No (append only) |
| `decisions` | Decision entities with embeddings | Bi-temporal |
| `alternatives` | Alternatives considered with scores | No |
| `evidence` | Evidence links with embeddings and provenance | No |
| `agents` | Registered agents with roles and API key hashes | Yes |
| `access_grants` | Fine-grained inter-agent permissions | Yes |
| `organizations` | Multi-tenant org registration | Yes |
| `search_outbox` | Qdrant sync queue (outbox pattern) | Yes (transient) |
| `integrity_proofs` | Merkle tree batch hashes for tamper detection | No (append only) |

### Event Types

```
AgentRunStarted, AgentRunCompleted, AgentRunFailed
DecisionStarted, AlternativeConsidered, EvidenceGathered
ReasoningStepCompleted, DecisionMade, DecisionRevised
ToolCallStarted, ToolCallCompleted
AgentHandoff, ConsensusRequested, ConflictDetected
```

## Rationale

**Why event sourcing:**

- Full audit trail without explicit audit logging.
- Replay: reconstruct any agent's state at any point in time.
- Debugging: "what did the agent know when it made this decision?"
- Derived views can be rebuilt from events if requirements change.
- Natural fit for agent activity, which is inherently a stream of actions.

**Why bi-temporal:**

- Business time vs system time distinction is critical for auditable AI systems.
- "What did we know at time T?" requires `transaction_time`.
- "When was this decision valid?" requires `valid_from`/`valid_to`.
- Corrections don't destroy history — the corrected record coexists with the original.

**Why JSONB facets:**

- New event types and metadata fields without schema migrations.
- OpenLineage pattern: core schema is fixed, extension points are JSONB.
- GIN indexes on JSONB provide fast filtering.

## Consequences

- Events are immutable. No UPDATE or DELETE on `agent_events`.
- Current state requires materialized views or derived tables.
- Materialized views need refresh strategy (on-demand or periodic).
- Bi-temporal queries are more complex than single-timeline queries.
- Storage grows monotonically — need retention/compression policies.

## References

- ADR-002: Unified PostgreSQL storage (storage engine this data model runs on)
- ADR-008: TimescaleDB for event ingestion (`agent_events` hypertable configuration)
- Implementation: `internal/model/` (domain types), `internal/storage/` (query layer), `migrations/*.sql` (schema)
- Martin Fowler: martinfowler.com/articles/bitemporal-history.html
- OpenLineage object model: openlineage.io/docs/spec/object-model/
