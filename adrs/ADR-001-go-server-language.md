# ADR-001: Go for server, Python/TypeScript for SDKs

**Status:** Accepted
**Date:** 2026-02-03

## Context

Akashi is infrastructure: event ingestion, concurrent connections, query engine, MCP server. We need to choose a primary language for the server and for client SDKs.

## Decision

- **Go** for the server (this repo)
- **Python SDK** (separate repo: `akashi-python`)
- **TypeScript SDK** (separate repo: `akashi-typescript`)

## Rationale

**Why Go for the server:**

- Infrastructure DNA: the adjacent ecosystem (OTEL Collector, Prometheus, Milvus, CockroachDB) is Go for the same reasons we need it — high concurrency, low latency, small memory footprint.
- Single static binary deploys anywhere. No runtime dependencies.
- `pgx` is a best-in-class PostgreSQL driver with native pgvector support.
- `net/http` and gRPC are first-class. MCP server implementation is straightforward.
- Goroutines handle thousands of concurrent trace ingestion connections without thread pool tuning.
- Compile-time type safety catches schema drift early.

**Why not Python for the server:**

- GIL limits true parallelism for I/O multiplexing at scale.
- asyncio adds complexity (colored functions, ecosystem fragmentation).
- Deployment requires runtime, virtualenv, dependency management.
- Memory usage 5-10x higher per connection than Go.

**Why not Rust:**

- Steeper learning curve, slower iteration speed.
- Go's performance is sufficient — we're I/O bound (Postgres), not CPU bound.
- Smaller hiring pool for Rust.

**Why Python and TypeScript for SDKs:**

- Python: dominant in ML/AI agent frameworks (LangChain, CrewAI, AutoGen, OpenAI SDK).
- TypeScript: web agents, Node.js backends, Vercel AI SDK ecosystem.
- SDKs are thin HTTP clients — language choice should match the consumer ecosystem.

## Consequences

- Server development uses Go tooling (go test, golangci-lint, goimports).
- SDKs are maintained in separate repos with their own release cycles.
- API contract is HTTP/JSON, defined once, consumed by both SDKs.
- Need to maintain API compatibility across three codebases.
