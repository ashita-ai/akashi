# ADR-004: MCP as primary distribution channel

**Status:** Accepted
**Date:** 2026-02-03

## Context

Akashi needs to be accessible to AI agents built on diverse frameworks. We need to decide the primary integration mechanism.

## Decision

Build Akashi as an **MCP (Model Context Protocol) server** as the primary distribution channel. HTTP API is the underlying transport. Framework-specific integrations (LangChain callback, CrewAI hooks) are secondary.

### MCP Surface

**Resources** (read-only context):
- `akashi://session/current` — current session context
- `akashi://decisions/recent` — recent decisions
- `akashi://agent/{id}/history` — agent decision history

**Tools** (executable operations):
- `akashi_trace` — record a decision trace
- `akashi_query` — structured query over past decisions
- `akashi_search` — semantic similarity search

## Rationale

**Why MCP first:**

- Framework-agnostic: any MCP-compatible agent (Claude, ChatGPT, custom) gets Akashi for free.
- Two-way protocol: agents both read context (Resources) and write traces (Tools).
- Industry convergence: Anthropic, OpenAI, Google, AWS on MCP steering committee.
- Solves M*N integration problem with M+N adapters.
- Stateful across interactions.

**Why HTTP API underneath:**

- MCP tools call the same HTTP endpoints that SDKs call.
- One API surface, multiple access patterns.
- SDKs are thin HTTP clients — no custom protocol needed.

**Framework integrations are secondary because:**

- LangChain callback handler captures events automatically but is framework-locked.
- CrewAI hooks, AutoGen middleware, OpenAI SDK context wrappers — each is a small adapter.
- These are built on top of the HTTP API, not as separate integration paths.

## Integration Priority

| Priority | Integration | Effort |
|----------|-----------|--------|
| 1 | MCP Server | Medium |
| 2 | HTTP API (JSON) | Medium (built alongside MCP) |
| 3 | Python SDK | Low (thin HTTP client) |
| 4 | TypeScript SDK | Low (thin HTTP client) |
| 5 | LangChain/LangGraph callback | Low (wraps SDK) |
| 6 | CrewAI hooks | Low |
| 7 | OpenAI Agents SDK | Low |

## Consequences

- MCP server implementation lives in `internal/mcp/`.
- HTTP API lives in `internal/server/`.
- MCP tools delegate to the same service layer as HTTP endpoints.
- Must keep MCP tool schemas and HTTP API schemas in sync.

## References

- Research: `ventures/specs/04-framework-integration.md`
- MCP specification: modelcontextprotocol.io/specification/2025-11-25
