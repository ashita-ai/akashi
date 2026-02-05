# SPEC-001: Akashi System Overview

**Status:** Draft
**Date:** 2026-02-03
**Source:** Elenchus interrogation session `session-ml7g1tbe-6i_FszljgZ9l` (4 rounds, 46 premises, 0 unresolved critical contradictions)

---

## Problem Statement

Multi-agent AI systems lack a shared, persistent, queryable record of agent reasoning and decisions. When Agent A hands off to Agent B, context evaporates. When a human audits an agent's decision, the reasoning is gone. When the same situation arises again, past learnings are unavailable.

No existing product answers: "Why did the agent decide this, what alternatives were considered, what evidence supported it, and has this situation come up before?"

Akashi is the **decision trace layer** that fills this gap.

## System Identity

Akashi is a **smart store** — not an orchestrator. It stores, indexes, and queries decision traces. It provides reactive primitives (subscriptions, conflict detection views, handoff protocol) but never directs agent behavior or manages workflows. Orchestration is the responsibility of external systems (framework-native tools, durable execution libraries, etc.).

### Three Capability Pillars

| Pillar | Responsibility |
|--------|---------------|
| **Context Graph Engine** | Decision traces, evidence store, memory blocks, handoff protocol |
| **Query & Retrieval** | Semantic search, structured queries, temporal/point-in-time queries, precedent matching |
| **Governance & Access** | JWT + RBAC authentication, scoped visibility, audit logging, compliance exports |

### What Akashi Is NOT

- NOT a general-purpose agent memory (Mem0's space)
- NOT a temporal knowledge graph (Zep's space)
- NOT an orchestration engine (LangGraph, Temporal, etc.)
- NOT an observability dashboard (Langfuse/Phoenix's space)

Akashi **complements** these systems. It owns the semantic context layer — decisions, evidence, reasoning lineage.

## Scope

### Event Boundary

All agent actions are traceable events. Every LLM call, tool invocation, and handoff generates an event in the append-only log. **Decisions** are a subset of events with richer structure — alternatives, evidence, confidence, reasoning chain.

The event type hierarchy defines the boundary:

| Category | Event Types | Structure |
|----------|------------|-----------|
| **Run lifecycle** | `AgentRunStarted`, `AgentRunCompleted`, `AgentRunFailed` | Action events (lightweight) |
| **Decision events** | `DecisionStarted`, `AlternativeConsidered`, `EvidenceGathered`, `ReasoningStepCompleted`, `DecisionMade`, `DecisionRevised` | Decision events (full structure: alternatives, evidence, confidence) |
| **Tool events** | `ToolCallStarted`, `ToolCallCompleted` | Action events |
| **Coordination events** | `AgentHandoff`, `ConsensusRequested`, `ConflictDetected` | Coordination events |

### Coordination Model

Akashi provides **reactive coordination**, not directive orchestration:

- **Subscriptions**: Agents subscribe to new decisions matching criteria (real-time push via SSE or WebSocket)
- **Conflict detection**: Materialized views surface conflicting decisions automatically
- **Handoff protocol**: Structured context transfer between agents/humans — a data format and storage mechanism, not a workflow engine
- **Consensus views**: Read-only views showing agreement/disagreement state across agents on a topic

Agents query Akashi for context and write traces to it. Akashi never tells an agent what to do next.

## V1 Success Criteria

### Deliverables

| Component | Scope |
|-----------|-------|
| **Go server** | HTTP JSON API + MCP server, single static binary |
| **MCP server** | Resources (`akashi://session/current`, `akashi://decisions/recent`, `akashi://agent/{id}/history`) + Tools (`akashi_trace`, `akashi_query`, `akashi_search`) |
| **Python SDK** | Thin HTTP client (separate repo: `akashi-python`) |
| **TypeScript SDK** | Thin HTTP client (separate repo: `akashi-typescript`) |
| **Framework integration** | At least one of: LangChain callback handler, CrewAI hooks |
| **PostgreSQL schema** | Full migration set for all core tables |

### Functional Requirements

1. Agents can **record decision traces** with alternatives, evidence, confidence scores, and reasoning chains
2. Agents can **query past decisions** with structured filters (agent, time range, event type, confidence threshold)
3. Agents can **search semantically** over decision history using pgvector HNSW
4. Agents can **subscribe to new decisions** matching criteria in real-time
5. System **detects conflicts** between agent decisions and surfaces them in materialized views
6. **Bi-temporal queries**: "What did we know at time T?" and "When was this decision valid?"
7. **Context replay**: Reconstruct the exact context available at decision time

### Non-Functional Requirements

| Metric | Target |
|--------|--------|
| Concurrent agents | 1,000+ |
| Event ingestion | 100K+ events/sec |
| Query latency (p95) | <20ms |
| Data retention | Indefinite (TimescaleDB compression, 90%+ on old chunks) |
| Availability target | 99.9% |

### Auth & Access Control

- **Authentication**: JWT tokens on both HTTP API and MCP server
- **Authorization**: RBAC with three roles — `admin`, `agent`, `reader`
- **Visibility**: Scoped per-agent. Agents can only read traces they are explicitly granted access to
- **Cross-agent access**: Requires explicit grants

### Observability

- Akashi emits OTEL telemetry (traces and metrics) for its own operations
- Decision traces include optional `trace_id` for OTEL correlation
- `akashi.context_id` propagated via OTEL baggage (identifier only, never sensitive data)

### Target Adoption

15 design partners with real multi-agent workloads.

## Architecture Context

### Position in the Stack

```
Orchestration:    External           (durable execution, workflow management — not Akashi's concern)
Semantic Context: Akashi              (decision traces, evidence, reasoning)
Observability:    OpenTelemetry      (latency, errors, token counts)
─────────────────────────────────────────
Linked via trace_id, conversation_id, context_id
```

### Boundary: What Akashi Owns

| Akashi Owns | Akashi Does NOT Own |
|------------|-------------------|
| Decision trace storage and indexing | Workflow orchestration |
| Semantic search over decisions/evidence | Agent lifecycle management |
| Structured and bi-temporal queries | Task scheduling or durable execution |
| Conflict detection (materialized views) | Directing agent behavior |
| Real-time subscriptions (SSE) | Batch ETL or data pipelines |
| Handoff context (data format + storage) | Initiating or routing handoffs |
| Access control (JWT + RBAC + grants) | Identity provider (external) |

Akashi is framework-agnostic. Any orchestration tool (LangGraph, Temporal, DBOS, custom) can write traces to Akashi and query them. Akashi does not depend on or recommend any specific orchestration system.

## Resolved Contradictions

These tensions were identified during interrogation and resolved:

| Tension | Resolution |
|---------|-----------|
| Enterprise scale vs single Postgres (ADR-002) | Phased scaling: single Postgres with COPY batching + PgBouncer achieves 100K events/sec (proven by RudderStack). Read replicas for Phase 2. Citus for Phase 3 if needed. |
| Indefinite retention at 100K events/sec | TimescaleDB compression achieves 90-95% reduction in production. Storage growth is sustainable with monitoring. |
| "Not an orchestrator" vs active coordination | Smart store with reactive primitives. Subscriptions and conflict views inform agents but don't direct them. Orchestration is external. |
| <20ms queries under 100K events/sec write load | CQRS: writes to primary (optimized for throughput), reads to replicas (optimized for latency). TimescaleDB partitioning isolates read/write to different chunks. |
| Simplicity vs horizontal scaling | Phased: Phase 1 is simple (single node + PgBouncer). Complexity added only when concrete triggers are hit. |

## Implementation Order

Build in this sequence. Each phase produces a working, testable system.

### Phase A: Storage Foundation

1. **PostgreSQL schema** — Write all migration files per SPEC-002. Run against Docker Postgres with pgvector + TimescaleDB.
2. **Domain types** — Go structs in `internal/model/` matching every table and event type payload.
3. **Storage layer** — `internal/storage/` with `pgxpool` connection, COPY-based batch ingestion, and query methods. Table-driven integration tests against real Postgres (testcontainers-go).

**Exit criteria**: `make test` passes. Can insert events via COPY, query decisions, search by embedding vector.

### Phase B: HTTP API

4. **Server setup** — `internal/server/` with `net/http` router, middleware (auth, request ID, logging, OTEL tracing).
5. **Auth** — JWT issuance (`POST /auth/token`), middleware validation, RBAC enforcement, access grant checks.
6. **Trace endpoints** — `POST /v1/runs`, `POST /v1/runs/{id}/events`, `POST /v1/runs/{id}/complete`, `POST /v1/trace`.
7. **Query endpoints** — `POST /v1/query`, `POST /v1/query/temporal`, `GET /v1/runs/{id}`, `GET /v1/agents/{id}/history`.
8. **Search endpoint** — `POST /v1/search` with embedding generation and pgvector HNSW query.
9. **Conflict + subscription endpoints** — `GET /v1/conflicts`, `GET /v1/subscribe` (SSE), `POST /v1/grants`, `DELETE /v1/grants/{id}`.
10. **Health** — `GET /health`.

**Exit criteria**: Full HTTP API functional. `curl` tests pass for every endpoint. Integration tests cover auth, ingestion, query, search, conflicts.

### Phase C: MCP Server

11. **MCP implementation** — `internal/mcp/` exposing Resources and Tools per SPEC-003. Delegates to the same service layer as HTTP.

**Exit criteria**: MCP client can connect, read resources, call tools. Same data visible through both HTTP and MCP.

### Phase D: Observability + Hardening

12. **OTEL instrumentation** — Emit spans and metrics per SPEC-004. Structured slog logging.
13. **Rate limiting** — Per-role limits per SPEC-003.
14. **Configuration** — Environment variables, CLI flags, Docker Compose with PgBouncer.

**Exit criteria**: `make all` passes (format + lint + vet + test + build). Docker Compose brings up full stack. OTEL traces visible.

### Phase E: SDKs + Integration (separate repos)

15. **Python SDK** — Thin HTTP client in `akashi-python`.
16. **TypeScript SDK** — Thin HTTP client in `akashi-typescript`.
17. **Framework integration** — LangChain callback handler or CrewAI hooks wrapping the SDK.

**Exit criteria**: SDK can trace a decision, query it back, and search semantically.

## Open Questions for Detailed Specs

These are captured for follow-up specifications:

1. What constitutes a "conflict" between agent decisions? What detection logic identifies conflicts?
2. How do decision traces relate to OTEL spans when a decision crosses trace boundaries?
3. What defines a "session" in MCP resource URIs? How do sessions map to `agent_runs`?
4. What is the exact handoff protocol format? What data transfers between agents?
5. How does the consensus primitives layer work? What voting/agreement mechanisms exist?
6. What embedding model generates vectors for semantic search? Who runs it?

## References

- ADRs: `adrs/ADR-001` through `ADR-006`
- Ventures spec: `ventures/01-shared-agent-context-infrastructure.md`
- Elenchus session: `session-ml7g1tbe-6i_FszljgZ9l`
