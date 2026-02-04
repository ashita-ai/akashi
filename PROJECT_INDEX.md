# Project Index: Kyoyu

Generated: 2026-02-04

Kyoyu is a decision-tracing service for AI agent coordination. Agents record decisions they make, check for precedents before deciding, and search decision history by semantic similarity. The server uses a bi-temporal data model (valid time + transaction time) with pgvector for embeddings.

## Project Structure

```
cmd/kyoyu/main.go              # Server entry point
internal/
  config/config.go              # Env-based configuration (Config struct, Load())
  auth/
    auth.go                     # JWTManager (Ed25519), IssueToken, ValidateToken, Claims
    hash.go                     # HashAPIKey, VerifyAPIKey (bcrypt)
  model/
    decision.go                 # Decision, Alternative, Evidence, SourceType, DecisionConflict
    run.go                      # Run, RunStatus
    event.go                    # Event, EventType
    span.go                     # Span
    agent.go                    # Agent, AgentRole, AccessGrant
    api.go                      # APIResponse/APIError envelopes, TraceRequest, auth types
    query.go                    # QueryRequest, QueryFilters, SearchRequest, CheckRequest/Response
  storage/
    pool.go                     # DB struct, NewDB (pgxpool), Ping, Close
    migrate.go                  # RunMigrations (embedded SQL)
    decisions.go                # CreateDecision, QueryDecisions, SearchDecisionsByEmbedding, GetDecisionsByAgent
    runs.go                     # CreateRun, GetRun, CompleteRun
    events.go                   # CreateEvent, GetEventsByRun
    alternatives.go             # CreateAlternativesBatch
    evidence.go                 # CreateEvidenceBatch
    agents.go                   # CreateAgent, GetAgentByAgentID, ListAgents, CountAgents
    grants.go                   # CreateGrant, DeleteGrant, CheckGrant
    conflicts.go                # ListConflicts (reads decision_conflicts materialized view)
    notify.go                   # Notify (pg_notify), ChannelDecisions constant
  service/
    embedding/embedding.go      # Provider interface, OpenAIProvider, NoopProvider
    trace/buffer.go             # Buffer (batched event writes with flush interval)
  server/
    server.go                   # Server struct, New (route registration), Start, Shutdown
    middleware.go               # requestID, logging, tracing (OTEL), auth, requireRole, writeJSON/writeError
    handlers.go                 # Handlers struct, NewHandlers, HandleAuthToken, HandleHealth, SeedAdmin, helpers
    handlers_decisions.go       # HandleTrace, HandleQuery, HandleTemporalQuery, HandleSearch, HandleCheck, HandleDecisionsRecent, HandleAgentHistory, HandleListConflicts
    handlers_runs.go            # HandleCreateRun, HandleAppendEvents, HandleCompleteRun, HandleGetRun
    handlers_admin.go           # HandleCreateAgent, HandleListAgents, HandleCreateGrant, HandleDeleteGrant
  mcp/
    mcp.go                      # MCP Server struct, New(), MCPServer() accessor
    tools.go                    # 5 MCP tools: kyoyu_check, kyoyu_trace, kyoyu_query, kyoyu_search, kyoyu_recent
    resources.go                # 3 MCP resources: session/current, decisions/recent, agents/{id}/history
    prompts.go                  # 3 MCP prompts: before-decision, after-decision, agent-setup
  telemetry/telemetry.go        # InitTracer, InitMeter (OTLP/HTTP exporters)
migrations/                     # 001-010 SQL files (agents, events, decisions, alternatives, evidence, spans, access control, materialized views)
docker/docker-compose.yml       # Postgres+pgvector, PgBouncer, Redis, Kyoyu server
sdk/
  go/kyoyu/                     # Go SDK (net/http, uuid only)
    client.go                   # Client, Config, NewClient, Check/Trace/Query/Search/Recent
    auth.go                     # tokenManager (sync.Mutex, auto-refresh)
    types.go                    # Decision, Alternative, Evidence, request/response types
    errors.go                   # Error type, IsNotFound/IsUnauthorized/IsForbidden
  python/src/kyoyu/             # Python SDK (httpx + pydantic v2)
    client.py                   # KyoyuClient (async), KyoyuSyncClient
    auth.py                     # TokenManager (asyncio.Lock, auto-refresh)
    types.py                    # Pydantic models
    middleware.py               # KyoyuMiddleware, KyoyuSyncMiddleware
    exceptions.py               # KyoyuError hierarchy
  typescript/src/               # TypeScript SDK (native fetch, zero deps)
    client.ts                   # KyoyuClient
    auth.ts                     # TokenManager
    types.ts                    # TypeScript interfaces
    middleware.ts               # withKyoyu<T> wrapper
    errors.ts                   # KyoyuError classes
prompts/                        # System prompt templates for agent builders
  generic.md                    # Framework-agnostic check-before/record-after instructions
  python.md                     # Python SDK usage examples
  typescript.md                 # TypeScript SDK usage examples
```

## HTTP API Routes

| Method | Path | Auth | Handler |
|--------|------|------|---------|
| POST | /auth/token | None | HandleAuthToken |
| GET | /health | None | HandleHealth |
| POST | /v1/agents | Admin | HandleCreateAgent |
| GET | /v1/agents | Admin | HandleListAgents |
| POST | /v1/runs | Admin+Agent | HandleCreateRun |
| POST | /v1/runs/{run_id}/events | Admin+Agent | HandleAppendEvents |
| POST | /v1/runs/{run_id}/complete | Admin+Agent | HandleCompleteRun |
| POST | /v1/trace | Admin+Agent | HandleTrace |
| POST | /v1/check | All | HandleCheck |
| POST | /v1/query | All | HandleQuery |
| POST | /v1/query/temporal | All | HandleTemporalQuery |
| POST | /v1/search | All | HandleSearch |
| GET | /v1/decisions/recent | All | HandleDecisionsRecent |
| GET | /v1/runs/{run_id} | All | HandleGetRun |
| GET | /v1/agents/{agent_id}/history | All | HandleAgentHistory |
| GET | /v1/subscribe | All | HandleSubscribe (SSE) |
| POST | /v1/grants | Admin+Agent | HandleCreateGrant |
| DELETE | /v1/grants/{grant_id} | Admin+Agent | HandleDeleteGrant |
| GET | /v1/conflicts | All | HandleListConflicts |
| * | /mcp | All | MCP StreamableHTTP |

## Key Dependencies

| Dependency | Purpose |
|------------|---------|
| pgx/v5 | PostgreSQL driver (connection pooling via pgxpool) |
| pgvector-go | Vector similarity search (cosine distance) |
| mcp-go v0.43.2 | Model Context Protocol server (tools, resources, prompts) |
| golang-jwt/v5 | JWT token issuance and validation (Ed25519) |
| x/crypto | bcrypt for API key hashing |
| OTEL SDK | Distributed tracing and metrics (OTLP/HTTP export) |
| testcontainers-go | Integration tests with real Postgres+pgvector |

## Configuration

All via environment variables (see `.env.example`):
- `DATABASE_URL` / `NOTIFY_URL` — Postgres (PgBouncer for queries, direct for LISTEN/NOTIFY)
- `KYOYU_ADMIN_API_KEY` — Bootstrap admin agent
- `OPENAI_API_KEY` / `KYOYU_EMBEDDING_MODEL` — Embeddings (falls back to NoopProvider)
- `KYOYU_PORT`, `KYOYU_LOG_LEVEL` — Server tuning

## Tests

- 3 test files, 42 Go source files total
- `internal/auth/auth_test.go` — JWT + API key hashing unit tests
- `internal/storage/storage_test.go` — Storage layer integration tests (testcontainers)
- `internal/server/server_test.go` — HTTP + MCP integration tests (testcontainers)
- Run: `make test` or `go test -race ./... -v`

## Quick Start

```bash
cp .env.example .env            # Configure secrets
make docker-up                  # Start Postgres, PgBouncer, Redis, Kyoyu
curl http://localhost:8080/health
```

## Architecture Notes

- **Bi-temporal model**: Every decision has `valid_from`/`valid_to` (business time) and `transaction_time` (system time). Supports temporal queries ("what did we know at time T?").
- **Materialized view** `decision_conflicts` detects contradictory decisions (same type, different outcome, overlapping validity). Refreshed periodically.
- **Event sourcing**: Runs contain ordered events. The `/v1/trace` convenience endpoint creates a run + decision + alternatives + evidence in one call.
- **Middleware chain**: requestID → OTEL tracing → structured logging → JWT auth → role-based access.
- **MCP transport**: StreamableHTTP at `/mcp`. Tools have rich descriptions that teach agents the check-before/record-after workflow.
- **Embedding**: OpenAI `text-embedding-3-small` by default. Decisions and evidence get embeddings for semantic search. Falls back to noop if no API key.
