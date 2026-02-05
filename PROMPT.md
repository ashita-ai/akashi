# Akashi Implementation — Phases A through D

You are a principal Go engineer implementing the Akashi decision trace server. Read ALL specs in `specs/`, ALL ADRs in `adrs/`, and `AGENTS.md` before doing anything. Your work persists between iterations — check git log and the codebase to see what you've already done.

## Decisions (locked — do not revisit)

- **Embedding provider**: OpenAI `text-embedding-3-small` (1536 dims) behind an `EmbeddingProvider` interface. Configurable via `AKASHI_OPENAI_API_KEY` env var.
- **JWT signing**: EdDSA (Ed25519). Key pair loaded from env or file.
- **Migration tooling**: Atlas (`ariga.io/atlas`).
- **Agent bootstrapping**: Admin-only `POST /v1/agents` endpoint. First admin seeded on boot if `agents` table is empty (uses `AKASHI_ADMIN_API_KEY` env var).
- **Conflict detection**: Level 1 — materialized view per SPEC-002 (same decision_type, different agents, different outcomes, both valid, within 1 hour). Refresh every 30 seconds.
- **Connection pooling**: PgBouncer from Phase 1. App connects to PgBouncer (port 6432) for normal queries. One dedicated `pgx.Conn` directly to Postgres (port 5432) for LISTEN/NOTIFY.
- **Rate limiting**: Redis-based sliding window. `go-redis/redis_rate` or custom Lua script.
- **`sequence_num`**: Server-assigned in the Go ingestion buffer before COPY flush.
- **SSE subscriptions**: Postgres LISTEN/NOTIFY on dedicated direct connection, fan out to SSE subscribers in-process.

## Implementation Order

Work in this exact order. Each phase must compile, pass `make all`, and have tests before moving to the next.

### Phase A: Storage Foundation

1. **SQL migrations** — Write all 10 migration files per SPEC-002 in `migrations/`. Use Atlas format. Include extensions (pgvector, timescaledb), all tables, indexes, materialized views, hypertable setup, compression policies.
2. **Domain types** — Go structs in `internal/model/` for every table and every event type payload. Use strong typing (UUIDs, time.Time, enums). No `interface{}`.
3. **Storage layer** — `internal/storage/` with:
   - `pgxpool`-based connection pool (connects via PgBouncer)
   - Dedicated `pgx.Conn` for LISTEN/NOTIFY (direct to Postgres)
   - COPY-based batch ingestion for `agent_events`
   - In-memory buffer with flush trigger: 1000 events OR 100ms timeout
   - CRUD operations for all tables
   - Bi-temporal query helpers
   - Materialized view refresh
   - Table-driven integration tests using `testcontainers-go` against real Postgres with pgvector + TimescaleDB

**Exit criteria**: `make test` passes. Can insert events via COPY, query decisions, search by embedding vector.

### Phase B: HTTP API

4. **Configuration** — `internal/config/` loading from environment variables with sensible defaults. Struct-based, validated on startup.
5. **Server setup** — `internal/server/` with `net/http` router (use stdlib `http.ServeMux` — no framework). Middleware chain: request ID → structured logging → OTEL tracing → auth → rate limiting.
6. **Auth** — JWT issuance (`POST /auth/token`), EdDSA validation middleware, RBAC enforcement, access grant checks. `POST /v1/agents` (admin-only) for agent registration.
7. **Embedding service** — `internal/service/embedding/` with `EmbeddingProvider` interface. OpenAI implementation using their API. Batch support.
8. **Trace endpoints** — `POST /v1/runs`, `POST /v1/runs/{id}/events`, `POST /v1/runs/{id}/complete`, `POST /v1/trace`. Generate embeddings server-side on ingestion for decisions and evidence.
9. **Query endpoints** — `POST /v1/query`, `POST /v1/query/temporal`, `GET /v1/runs/{id}`, `GET /v1/agents/{id}/history`.
10. **Search endpoint** — `POST /v1/search` with embedding generation and pgvector HNSW query.
11. **Subscription endpoint** — `GET /v1/subscribe` (SSE) using LISTEN/NOTIFY fan-out.
12. **Access control endpoints** — `POST /v1/grants`, `DELETE /v1/grants/{id}`.
13. **Conflict endpoint** — `GET /v1/conflicts`.
14. **Health** — `GET /health` with Postgres connectivity check.
15. **Rate limiting middleware** — Redis-based sliding window, per-role limits per SPEC-003. Rate limit headers in responses.

**Exit criteria**: Full HTTP API functional. Integration tests cover auth, ingestion, query, search, conflicts, subscriptions. `make all` passes.

### Phase C: MCP Server

16. **MCP implementation** — `internal/mcp/` using `mark3labs/mcp-go`. Expose resources (`akashi://session/current`, `akashi://decisions/recent`, `akashi://agent/{id}/history`) and tools (`akashi_trace`, `akashi_query`, `akashi_search`). JWT auth on MCP transport. Delegates to the same service layer as HTTP.

**Exit criteria**: MCP client can connect, authenticate, read resources, call tools. Same data visible through both HTTP and MCP.

### Phase D: Observability + Hardening

17. **OTEL instrumentation** — Emit spans and metrics per SPEC-004. Every API request gets a span. Ingestion, query, and search metrics as defined. Use `go.opentelemetry.io/otel` SDK.
18. **Structured logging** — slog with JSON handler. Request-scoped fields (request_id, agent_id, trace_id). Log levels per SPEC-004.
19. **Graceful shutdown** — Drain in-memory event buffer, close SSE connections, close DB pools.
20. **Docker Compose** — Full stack: Postgres 17 (TimescaleDB + pgvector), PgBouncer, Redis 7, Akashi server. Init script enables extensions. Health checks on all services.
21. **Configuration hardening** — All secrets from env vars. Validation on startup. Sensible defaults for dev, explicit required vars for prod.

**Exit criteria**: `make all` passes. `docker compose up` brings up the full stack. OTEL traces emitted. Structured logs on all requests. Graceful shutdown works.

## Code Standards (from AGENTS.md — mandatory)

- Complete production code. No TODOs, no placeholders, no stubs.
- Tests for all features (success and error cases). Table-driven tests.
- `context.Context` as first parameter on all I/O functions.
- Return explicit errors. Wrap with `fmt.Errorf("operation: %w", err)`.
- Godoc comments on all exported types and functions.
- Use `internal/` for all application code.
- Structured logging via `slog`. No `log.Println`.
- `errgroup` for parallel operations. No naked goroutines.
- No `interface{}` where concrete types work.
- Run `make all` before considering a phase complete.
- Do NOT include "Co-Authored-By" or "Generated with Claude Code" in commits.

## What to Do Each Iteration

1. Check git log and the current state of the codebase to understand what's been done.
2. Identify the next incomplete item from the implementation order above.
3. Implement it fully with tests.
4. Run `make all` (or at minimum `go build ./...` and `go vet ./...` if tests require Docker).
5. Commit your work to the feature branch with a clear commit message.
6. If the phase you just completed is fully done (all exit criteria met), note it.
7. If ALL phases A-D are complete and passing, output: <promise>PHASES A-D COMPLETE</promise>

## Important

- Read existing code before modifying it. Don't duplicate what exists.
- If a dependency isn't in go.mod yet, `go get` it.
- If `make all` fails, fix it before moving on.
- Keep commits granular — one logical unit per commit.
- The docker-compose.yml in `docker/` needs to be updated to include PgBouncer, Redis, and the Akashi service.
- Update `specs/04-scaling-and-operations.md` to reconcile the docker-compose section with the actual `docker/docker-compose.yml` (remove PgBouncer deferral, add Redis, match reality).
