# ADR-006: Standards alignment — OTEL, MCP, A2A

**Status:** Accepted
**Date:** 2026-02-03

## Context

Multiple standards are emerging for AI agent observability, communication, and interoperability. We need to decide how Akashi relates to each.

## Decision

```
Akashi position in the stack:
─────────────────────────────────────────
Observability:    OpenTelemetry  (complement, don't compete)
Communication:    A2A + MCP      (integrate with both)
Semantic Context: Akashi          (own this layer)
─────────────────────────────────────────
Linked via trace_id, conversation_id, context_id
```

### OpenTelemetry

- **Emit** OTEL telemetry from Akashi operations (traces and metrics).
- **Link** decision traces to OTEL traces via `trace_id`.
- **Propagate** `akashi.context_id` via OTEL baggage (identifier only, not sensitive data).
- OTEL GenAI semantic conventions are experimental. We participate in the SIG but don't depend on stabilization.

### MCP

- Akashi is an MCP server (ADR-004).
- MCP is the primary distribution channel.

### A2A

- Publish an A2A Agent Card when the protocol matures further (v0.3 stable, Linux Foundation governance, 150+ orgs).
- A2A task artifacts can reference Akashi context URIs.
- Not a day-1 priority.

### OpenLineage

- Borrow the facet-based extensibility pattern for JSONB event payloads.
- Do not implement the full OpenLineage spec.

## Rationale

**Why complement OTEL, not compete:**

- OTEL GenAI conventions define agent spans but have no provisions for decision rationale, evidence chains, confidence scores, or reasoning provenance.
- OTEL is for observability (latency, errors, token counts). Akashi is for semantic context (decisions, evidence, reasoning).
- Using OTEL IDs as foreign keys gives users a single trace that spans both systems.

**Why not wait for OTEL GenAI to stabilize:**

- All GenAI conventions are in Development status. No stable release date announced.
- Even when stable, they won't cover decision-specific semantics — that's not what OTEL is for.

**Security note:** Only propagate identifiers in OTEL baggage, never sensitive decision data. Baggage is visible in HTTP headers.

## Consequences

- Server emits OTEL spans and metrics for its own operations.
- Decision traces include optional `trace_id` field for OTEL correlation.
- Go implementation uses `go.opentelemetry.io/otel` SDK.
- A2A Agent Card is deferred but the API should be designed with it in mind.

## References

- Research: `ventures/specs/05-standards-landscape.md`
- OTEL GenAI conventions: opentelemetry.io/docs/specs/semconv/gen-ai/
- A2A protocol: a2a-protocol.org/latest/
- MCP spec: modelcontextprotocol.io/specification/2025-11-25
