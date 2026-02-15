# ADR-004: MCP and framework integrations as primary distribution channels

**Status:** Accepted
**Date:** 2026-02-08

## Context

Akashi needs to reach three distinct audiences:

1. **AI-native users** working inside MCP-compatible environments (Claude, Cursor, Windsurf). They want decision tracing with zero code changes -- just add a server to their configuration.
2. **Agent framework developers** building with LangChain, CrewAI, AutoGen, or similar orchestration libraries. They want a callback or decorator that plugs into their existing agent lifecycle, not raw HTTP calls.
3. **Traditional programmatic integrators** who need full control over the API surface -- custom pipelines, batch ingestion, CI/CD hooks, or workflows that do not fit neatly into a framework.

All three audiences ultimately need the same backend operations: trace a decision, query the audit trail, search by semantic similarity, check for precedents. The question is how to package those operations for each audience.

The codebase already has the building blocks:

- An HTTP API at `/v1/*` with full CRUD: trace, query, search, check, subscribe (SSE), temporal queries, export, and agent management.
- An MCP server at `/mcp` (mark3labs/mcp-go, StreamableHTTP transport) co-hosted in the same binary, exposing Resources (`akashi://session/current`, `akashi://decisions/recent`, `akashi://agent/{id}/history`), Tools (`akashi_check`, `akashi_trace`, `akashi_query`, `akashi_search`, `akashi_recent`), and Prompts (`before-decision`, `after-decision`, `agent-setup`).
- A shared service layer (`internal/service/decisions/`) that both HTTP handlers and MCP handlers delegate to, ensuring consistent behavior for embedding generation, quality scoring, and transactional writes.
- SDKs for Go (`sdk/go/akashi/`), Python (`sdk/python/src/akashi/`), and TypeScript (`sdk/typescript/src/`) that wrap the HTTP API with typed clients, auth helpers, and middleware hooks.

## Decision

**MCP and framework integrations are the primary distribution channels. SDKs are glue -- the low-level HTTP wrappers that frameworks and MCP build on top of.**

The layering, from highest friction to lowest:

| Layer | Target audience | Integration effort | Status |
|-------|----------------|-------------------|--------|
| MCP server | AI-native users (Claude, Cursor, Windsurf) | One line of config | Shipped |
| Framework integrations | Agent builders (LangChain, CrewAI, AutoGen) | One import + decorator | Planned |
| SDKs (Go, Python, TypeScript) | Programmatic integrators | Import, instantiate, call | Shipped |
| HTTP API | Everything else | Raw HTTP | Shipped |

Framework integrations are thin adapters that use the SDKs internally. They translate framework-specific lifecycle hooks (LangChain callbacks, CrewAI task events, AutoGen message hooks) into Akashi SDK calls. The SDK handles auth, retries, connection management, and serialization. The framework integration handles only the mapping from framework concepts to Akashi concepts.

The MCP server is co-hosted in the Akashi binary and shares the same service layer as the HTTP API. It does not go through the SDK -- it calls the service layer directly, since it runs in-process. This avoids a network round-trip and means the MCP path has identical transactional guarantees to the HTTP path.

## Rationale

### Why MCP is the lowest-friction entry point

Adding Akashi to an MCP-compatible environment requires no code changes. The user adds a server entry to their MCP configuration, and the agent gains access to decision tracing tools, precedent checking, and the audit trail. The `agent-setup` prompt provides the behavioral contract (check before deciding, record after deciding) without any application-level wiring.

This matters because the initial adoption barrier for observability tools is high. Developers rarely instrument proactively. An MCP server that "just works" when added to the config lowers that barrier to near zero for the growing population of MCP-compatible AI environments.

### Why framework integrations are the highest-leverage channel

The Python and TypeScript AI agent ecosystems are organized around frameworks. A LangChain developer does not want to learn an HTTP API; they want `AkashiCallbackHandler` that they pass to their agent's constructor. A CrewAI developer wants `@akashi_trace` on their task functions. These integrations are small (each is likely under 500 lines), but they determine whether Akashi gets adopted by projects that already have a framework dependency.

Framework integrations use the SDK internally rather than calling the HTTP API directly. This keeps the integration code focused on the framework-specific lifecycle mapping and delegates connection management, auth, retries, and type safety to the SDK.

### Why SDKs remain essential despite not being the primary channel

SDKs serve three roles:

1. **Foundation for framework integrations.** Every framework integration imports the SDK. The SDK owns the HTTP client, auth flow, error handling, and typed models. Framework integrations are thin wrappers over SDK methods.
2. **Escape hatch for custom integrations.** Not every agent system uses LangChain or CrewAI. Custom orchestrators, batch pipelines, and CI/CD hooks need a typed client with proper error handling, not raw `curl` calls.
3. **Test surface.** SDK tests (Go: 26, Python: 33, TypeScript: 56) validate the HTTP API contract from the consumer's perspective. They catch breaking changes before framework integrations or MCP users encounter them.

### Why the MCP server is co-hosted, not a separate process

The MCP server runs in the same binary as the HTTP API and shares the `decisions.Service` layer. This is a deliberate architectural choice:

- **No extra deployment artifact.** Operators run one binary that serves HTTP, MCP, and the UI. No sidecar, no proxy, no second container.
- **Shared transactional guarantees.** MCP tools call `decisionSvc.Trace()` and `decisionSvc.Check()` directly. The same embedding generation, quality scoring, and single-transaction writes apply regardless of whether the request arrived via HTTP or MCP.
- **Shared auth.** The MCP StreamableHTTP transport is mounted behind the same `authMiddleware` and `requireRole(reader)` middleware chain as the HTTP API. No separate auth system to maintain.

## Consequences

- Framework integrations (LangChain, CrewAI, AutoGen) are planned but not yet built. Each will be a separate package in the SDK's language ecosystem (Python packages for LangChain/CrewAI, TypeScript package for Vercel AI SDK). Priority is gated by agent DX improvements (spec 33) and MCP tool ergonomics shipping first.
- Framework integrations import and depend on the corresponding SDK. They do not duplicate HTTP client logic.
- The MCP server continues to be co-hosted and uses the service layer directly. It does not depend on any SDK.
- SDKs must maintain backward compatibility, since framework integrations and direct users both depend on them. Breaking SDK changes require a major version bump and coordinated framework integration updates.
- The HTTP API is the contract. SDKs, MCP, and framework integrations are all projections of the same API surface. New capabilities are added to the HTTP API and service layer first, then exposed through MCP tools and SDK methods.
- Documentation should lead with MCP setup (lowest friction), then framework integration examples, then SDK usage. Raw HTTP API docs are the reference, not the tutorial.

## References

- ADR-001: Go for server, Go/Python/TypeScript for SDKs (language selection for server and SDK ecosystem)
- ADR-005: Ed25519 JWT authentication with Argon2id API keys and tiered RBAC (shared auth for HTTP and MCP paths)
- ADR-006: Embedding provider chain (service layer shared between HTTP handlers and MCP tools)
- Implementation: `internal/mcp/` (MCP server, resources, tools, prompts), `internal/service/decisions/` (shared service layer)
- SDKs: `sdk/go/akashi/`, `sdk/python/src/akashi/`, `sdk/typescript/src/`
- OpenAPI spec: `openapi.yaml` (embedded, served at GET /openapi.yaml)
