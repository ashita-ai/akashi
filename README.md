# Akashi

**Git blame for AI decisions.**

When multiple AI agents work together — a planner, a coder, a reviewer — their decisions are invisible. If something goes wrong, you can't answer: *who decided what, when, why, and what alternatives were considered?*

Akashi is the audit trail. Every agent decision gets recorded with its full reasoning, the alternatives that were weighed, the evidence that informed it, and the confidence level. When the CTO asks "why did the AI do that?", you have the answer.

## What it does

Akashi captures structured records of every AI agent decision and makes them queryable through an HTTP API, Model Context Protocol (MCP), and a built-in audit dashboard. Agent B can search Agent A's past decisions by semantic similarity, query by type and confidence threshold, or subscribe to a real-time feed of new decisions as they happen.

### What the audit trail captures

Every decision trace records:

- **The decision itself** -- what was chosen and the agent's confidence level
- **The reasoning chain** -- step-by-step logic explaining why
- **Rejected alternatives** -- what else was considered, with scores and rejection reasons
- **Supporting evidence** -- what information backed the decision, with provenance
- **Temporal context** -- when the decision was made and when it was valid
- **Conflicts** -- when two agents disagree on the same question, Akashi detects it

### What Akashi is not

Akashi is an audit system, not an orchestrator. It records, indexes, and queries decision traces but never directs agent behavior. It differs from:

- **Agent memory** (Mem0, etc.) -- Akashi stores structured decision records with typed fields, not unstructured memory blobs
- **Temporal knowledge graphs** (Zep, etc.) -- Akashi models decisions as first-class entities with alternatives and evidence, not graph relationships
- **Workflow engines** (LangGraph, Temporal) -- Akashi provides reactive coordination (subscriptions, conflict detection) without managing workflows
- **Observability dashboards** (Langfuse, etc.) -- Akashi captures structured decision records, not LLM call traces. It answers "why did the agent decide X?" not "how long did the API call take?"

## Audit Dashboard

Akashi ships with a built-in audit dashboard — a React SPA embedded in the Go binary via `go:embed`. No separate frontend deployment needed.

| Page | What it shows |
|------|---------------|
| **Dashboard** | Decision counts, active agents, open conflicts, usage gauge, recent activity |
| **Decisions** | Paginated table of all decisions with agent/type filters, click to drill down |
| **Decision Detail** | Full trace: reasoning, alternatives table with scores, evidence cards, timeline |
| **Agents** | Registered agents with RBAC roles, create/delete management (admin only) |
| **Conflicts** | Side-by-side view when two agents disagree on the same question |
| **Search** | Keyword and semantic search across all decisions |
| **Billing** | Plan limits, usage meters, Stripe checkout/portal (SaaS mode) |

The UI is optional — build with `go build -tags ui` to include it, or without for API-only deployments. In development, run `make dev-ui` for Vite's hot-reload dev server proxied to the Go backend.

## Architecture overview

```
     Browser (SPA)        MCP Clients              HTTP Clients
          |                    |                         |
          v                    v                         v
     +---------+         +---------+               +---------+
     |    /    |         |   /mcp  |               | /v1/... |
     +---------+         +---------+               +---------+
          |                    |                         |
          +----------- Akashi Server (Go, single binary) +
          |                                              |
+---------+----------+---------+---------+---------+-----+
|         |          |         |         |         |
Auth    Trace Buffer Query   Search   Conflict   Billing
Ed25519  (in-memory  (SQL    (pgvector  Detection  (Stripe)
 JWT    + COPY flush) WHERE)  HNSW)  (mat. view)
|         |          |         |         |
+----+----+----+-----+---------+---------+
     |              |
PgBouncer       Direct Conn
(port 6432)     (port 5432)
  queries     LISTEN/NOTIFY
     |              |
     +------+-------+
            |
     PostgreSQL 17
  pgvector + TimescaleDB
```

The server exposes the same capabilities through three interfaces:

- **Audit Dashboard** at `/` -- built-in React SPA for human reviewers, auditors, and operators
- **HTTP API** at `/v1/...` -- REST endpoints for trace ingestion, structured queries, semantic search, agent management, and real-time subscriptions (SSE)
- **MCP server** at `/mcp` -- StreamableHTTP transport with five tools (`akashi_check`, `akashi_trace`, `akashi_query`, `akashi_search`, `akashi_recent`), three resources (`akashi://session/current`, `akashi://decisions/recent`, `akashi://agent/{id}/history`), and three prompts (`before-decision`, `after-decision`, `agent-setup`), so any MCP-compatible agent can connect directly

All three interfaces share the same storage layer, auth, and embedding provider.

## How data flows

### Recording a decision

An agent records a decision to the black box either through the HTTP convenience endpoint or the MCP `akashi_trace` tool:

1. A run (execution context) is created in the `agent_runs` table
2. The decision text is embedded via the configured embedding provider (auto-detected: Ollama if available, then OpenAI if `OPENAI_API_KEY` is set, then a noop zero-vector fallback)
3. The decision is stored with its embedding in the `decisions` table (1024-dimensional vector column with an HNSW index)
4. Alternatives and evidence are batch-inserted using the PostgreSQL COPY protocol for throughput
5. A `NOTIFY` is sent on the `akashi_decisions` channel so SSE subscribers learn about it immediately
6. The run is marked complete

For high-throughput event ingestion, agents can use the step-by-step path (`POST /v1/runs`, then `POST /v1/runs/{id}/events`). Events accumulate in an in-memory buffer that flushes via `COPY` when it hits 1000 events or 100ms, whichever comes first. Sequence numbers are assigned server-side to guarantee ordering.

### Querying the audit trail

Three query modes, all available through both HTTP and MCP:

- **Structured query** -- filter by agent ID, decision type, outcome, confidence threshold, and time range. Dynamic WHERE clause construction with pagination.
- **Semantic search** -- embed the query text, then find the most similar decisions using pgvector's HNSW index with cosine distance. Returns results ranked by similarity score.
- **Temporal query** -- point-in-time query using bi-temporal modeling. Ask "what did the system believe at time T?" by filtering on both business time (`valid_from`/`valid_to`) and system time (`transaction_time`).

### Conflict detection

A materialized view (`decision_conflicts`) automatically identifies when two agents made different decisions on the same type within a one-hour window. The view is refreshed every 30 seconds. Query it via `GET /v1/conflicts`.

## Data model

The schema uses PostgreSQL 17 with two extensions:

- **pgvector** for HNSW vector indexes on decision and evidence embeddings
- **TimescaleDB** for time-partitioned event storage with automatic compression

Core tables:

| Table | Purpose |
|-------|---------|
| `agent_runs` | Top-level execution context (one per agent invocation) |
| `agent_events` | Append-only event log (TimescaleDB hypertable, daily chunks, 7-day compression) |
| `decisions` | First-class decision entities with vector(1024) embeddings, bi-temporal columns |
| `alternatives` | Options the agent considered, with scores and rejection reasons |
| `evidence` | Supporting evidence with provenance, vector(1024) embeddings |
| `agents` | Registered agents with roles (admin/agent/reader) and Argon2id API key hashes |
| `access_grants` | Fine-grained, time-limited cross-agent visibility |

Decisions use **bi-temporal modeling**: `valid_from`/`valid_to` track business time (when the decision was in effect), while `transaction_time` tracks when it was recorded. Revising a decision closes the old row's `valid_to` and inserts a new row, preserving full history.

## Authentication and authorization

- **API keys** are hashed with Argon2id (64MB memory, constant-time comparison) and stored in the `agents` table
- **JWTs** are signed with Ed25519 (EdDSA). Keys load from PEM files in production or are auto-generated for development
- Three roles: `admin` (full access), `agent` (read/write own traces, query granted traces), `reader` (read-only)
- On first startup, if the `agents` table is empty and `AKASHI_ADMIN_API_KEY` is set, an admin agent is bootstrapped automatically

Auth flow:
```
POST /auth/token  {"agent_id": "...", "api_key": "..."}
  -> {"data": {"token": "<jwt>", "expires_at": "..."}}

# Then use the JWT:
Authorization: Bearer <jwt>
```

## Quick start

```bash
# Copy and edit the environment file
cp .env.example .env

# Start the full local stack (Postgres + PgBouncer + Redis + Akashi)
docker compose -f docker/docker-compose.local.yml up -d

# Or use the Makefile shorthand
make docker-up
make build                    # API-only binary
AKASHI_ADMIN_API_KEY=dev-admin-key ./bin/akashi

# Build with the audit dashboard embedded
make build-with-ui            # Builds UI + Go binary with -tags ui
AKASHI_ADMIN_API_KEY=dev-admin-key ./bin/akashi
# Open http://localhost:8080 to see the dashboard

# UI development (hot reload)
make dev-ui                   # Vite dev server at :5173, proxies API to :8080

# Run the test suite (requires Docker for testcontainers)
make test
```

### Record a decision

```bash
# Get a token
TOKEN=$(curl -s -X POST http://localhost:8080/auth/token \
  -H 'Content-Type: application/json' \
  -d '{"agent_id": "admin", "api_key": "dev-admin-key"}' \
  | jq -r '.data.token')

# Record a decision trace
curl -X POST http://localhost:8080/v1/trace \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "agent_id": "admin",
    "decision": {
      "decision_type": "model_selection",
      "outcome": "chose gpt-4o for summarization",
      "confidence": 0.92,
      "reasoning": "gpt-4o balances quality and cost for this task",
      "alternatives": [
        {"label": "gpt-4o", "selected": true, "score": 0.92},
        {"label": "claude-3-haiku", "selected": false, "score": 0.78}
      ],
      "evidence": [
        {"source_type": "benchmark", "content": "gpt-4o scored 94% on summarization eval"}
      ]
    }
  }'

# Search for similar decisions
curl -X POST http://localhost:8080/v1/search \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"query": "which model to use for text tasks", "limit": 5}'
```

## SDKs

Client SDKs are available for Go, Python, and TypeScript. Each provides the same five operations: `Check`, `Trace`, `Query`, `Search`, and `Recent`.

| Language | Path | Dependencies | Docs |
|----------|------|--------------|------|
| Go | [`sdk/go/`](sdk/go/) | `net/http` + `google/uuid` | [README](sdk/go/README.md) |
| Python | [`sdk/python/`](sdk/python/) | `httpx` + `pydantic v2` | [README](sdk/python/README.md) |
| TypeScript | [`sdk/typescript/`](sdk/typescript/) | Native `fetch` (zero deps) | [README](sdk/typescript/README.md) |

All SDKs handle JWT token acquisition and auto-refresh transparently. Python and TypeScript include middleware that enforces the check-before/record-after pattern automatically. See `prompts/` for system prompt templates that teach agents when to use these tools.

## Project structure

```
cmd/akashi/                 Application entrypoint, wires all components
internal/
  auth/                    Ed25519 JWT + Argon2id API key hashing
  billing/                 Stripe billing integration with quota enforcement
  config/                  Environment variable configuration
  mcp/                     MCP server (5 tools, 3 resources, 3 prompts)
  model/                   Domain types (runs, events, decisions, agents, queries)
  ratelimit/               Redis-backed sliding window rate limiter
  server/                  HTTP server, handlers, middleware, SPA handler
  service/
    decisions/             Shared decision service (trace, check, search, query)
    embedding/             Embedding provider interface (OpenAI, Ollama, noop)
    quality/               Decision quality scoring
    trace/                 In-memory event buffer with COPY-based batch flush
  signup/                  Self-serve signup with email verification
  storage/                 PostgreSQL storage layer (pgxpool + pgx for NOTIFY)
  telemetry/               OpenTelemetry tracer and meter initialization
migrations/                21 forward-only SQL migration files
ui/                        Audit dashboard (React 19, TypeScript, Vite, Tailwind)
  ui.go                    go:embed with build tag (ui)
  ui_noop.go               nil FS fallback without build tag
  src/
    pages/                 Dashboard, Decisions, DecisionDetail, Agents, Conflicts, Billing, Search, Login
    components/            Layout, ErrorBoundary, shadcn/ui primitives
    lib/                   API client, auth context, SSE hook, utilities
docker/
  docker-compose.local.yml Self-contained local stack (Postgres, PgBouncer, Redis, Akashi)
  docker-compose.cloud.yml Cloud stack (PgBouncer → TimescaleDB Cloud, Qdrant Cloud)
  init.sql                 Extension initialization (pgvector + TimescaleDB)
  env.local.example        Example env for local mode
  env.cloud.example        Example env for cloud mode
sdk/
  go/                      Go SDK (separate go.mod, no server dependencies)
  python/                  Python SDK (pydantic v2 + httpx)
  typescript/              TypeScript SDK (native fetch, zero runtime dependencies)
prompts/                   System prompt templates for agent builders
Dockerfile                 3-stage build: Node (UI) → Go (binary) → Alpine (runtime)
```

## Configuration

All configuration is via environment variables. See [`.env.example`](.env.example) for the full list.

| Variable | Default | Description |
|----------|---------|-------------|
| `DATABASE_URL` | `postgres://...@localhost:6432/akashi` | Connection string (through PgBouncer) |
| `NOTIFY_URL` | `postgres://...@localhost:5432/akashi` | Direct connection for LISTEN/NOTIFY |
| `REDIS_URL` | `redis://localhost:6379/0` | Redis for rate limiting (noop if unreachable) |
| `AKASHI_PORT` | `8080` | HTTP server port |
| `AKASHI_ADMIN_API_KEY` | _(empty)_ | Bootstrap admin API key |
| `OPENAI_API_KEY` | _(empty)_ | OpenAI key for embeddings (falls back to noop) |
| `AKASHI_EMBEDDING_PROVIDER` | `auto` | Embedding provider: `auto`, `openai`, `ollama`, `noop` |
| `AKASHI_EMBEDDING_MODEL` | `text-embedding-3-small` | OpenAI embedding model |
| `OLLAMA_URL` | `http://localhost:11434` | Ollama server URL (preferred in auto mode) |
| `AKASHI_JWT_PRIVATE_KEY` | _(empty)_ | Path to Ed25519 private key PEM (auto-generated if empty) |
| `AKASHI_JWT_PUBLIC_KEY` | _(empty)_ | Path to Ed25519 public key PEM |
| `AKASHI_JWT_EXPIRATION` | `24h` | JWT token lifetime |
| `STRIPE_SECRET_KEY` | _(empty)_ | Stripe secret key (billing disabled if empty) |
| `STRIPE_WEBHOOK_SECRET` | _(empty)_ | Stripe webhook signing secret |
| `AKASHI_SMTP_HOST` | _(empty)_ | SMTP host for email verification (logs URL if empty) |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | _(empty)_ | OTLP endpoint (OTEL disabled if empty) |
| `AKASHI_LOG_LEVEL` | `info` | Log level (debug, info, warn, error) |

## Observability

When `OTEL_EXPORTER_OTLP_ENDPOINT` is set, Akashi exports:

- **Traces**: every HTTP request gets a span with method, path, status code, agent ID, and role attributes
- **Metrics**: `http.server.request_count` (counter) and `http.server.duration` (histogram in ms), both tagged by method, route, status code, and agent ID

All request logs are structured JSON via `slog` and include `request_id`, `trace_id` (when OTEL is active), and `agent_id` (when authenticated).

## Docker Compose

Two compose variants are provided:

**Local** (`docker/docker-compose.local.yml`) — fully self-contained, no cloud dependencies. Runs Postgres (pgvector + TimescaleDB), PgBouncer, Redis, and Akashi. Semantic search uses pgvector. Embeddings default to noop.

```bash
docker compose -f docker/docker-compose.local.yml up -d
```

**Cloud** (`docker/docker-compose.cloud.yml`) — connects to TimescaleDB Cloud and Qdrant Cloud. Runs only PgBouncer (local pooling to cloud DB), Redis, and Akashi.

```bash
cp docker/env.cloud.example docker/.env.cloud
# Edit docker/.env.cloud with your cloud credentials
docker compose -f docker/docker-compose.cloud.yml --env-file docker/.env.cloud up -d
```

Both variants share the same service architecture:

| Service | Port | Purpose |
|---------|------|---------|
| postgres | 5432 | Primary datastore (local only) |
| pgbouncer | 6432 | Connection pooling (transaction mode, 50 pool / 1000 max) |
| redis | 6379 | Rate limiting (sliding window, per-agent quotas) |
| akashi | 8080 | Application server |

Akashi connects to PgBouncer for all queries and maintains a separate direct connection to Postgres for `LISTEN/NOTIFY` (which PgBouncer does not support in transaction pooling mode).

## Testing

Tests use [testcontainers-go](https://golang.testcontainers.org/) to spin up real TimescaleDB + pgvector instances. No mocks for the storage layer.

```bash
# Run all tests (requires Docker)
make test

# Run with verbose output
go test ./... -v -count=1
```

100+ tests across Go, Python, and TypeScript:

**Go integration tests** (real TimescaleDB + pgvector via testcontainers, no mocks):
- `internal/server/` -- 27 tests: health, auth, RBAC, ingestion, trace, query, search, export, GDPR deletion, signup/verification, billing, access grants, SSE, MCP tools/resources/prompts
- `internal/storage/` -- 17 tests: runs, events, decisions, alternatives, evidence, agents, grants, conflicts, notifications, temporal queries, embedding search
- `internal/ratelimit/` -- 7 tests: sliding window, concurrency, noop mode
- `internal/auth/` -- 2 tests: API key hashing, JWT issuance/validation
- `internal/service/` -- 4 tests: embedding providers, quality scoring
- `internal/signup/`, `internal/billing/` -- validation and quota logic

**SDK tests:**
- TypeScript: 23 tests (client methods, error mapping, middleware)
- Python: 12 tests (async/sync clients, error mapping, middleware)
- Go: 5 tests (client methods, token refresh, error types, timeouts)

## Requirements

- Go 1.25+
- Docker (for testcontainers and docker-compose)

## License

Apache 2.0. See [LICENSE](LICENSE).
