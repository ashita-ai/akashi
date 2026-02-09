# ADR-001: Go for server, Go/Python/TypeScript for SDKs

**Status:** Accepted
**Date:** 2026-02-03
**Revised:** 2026-02-08

## Context

Akashi is infrastructure: event ingestion, concurrent connections, query engine, MCP server. We need to choose a primary language for the server and for client SDKs.

## Decision

- **Go** for the server (this repo)
- **Go SDK** (`sdk/go/akashi/`)
- **Python SDK** (`sdk/python/src/akashi/`)
- **TypeScript SDK** (`sdk/typescript/src/`)

All three SDKs live in the same repo under `sdk/`. API and SDK changes are committed together in a single release cycle.

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

**Why Go, Python, and TypeScript for SDKs:**

- Go: provides a native client for Go-based agent systems and serves as the reference SDK implementation.
- Python: dominant in ML/AI agent frameworks (LangChain, CrewAI, AutoGen, OpenAI SDK).
- TypeScript: web agents, Node.js backends, Vercel AI SDK ecosystem.
- SDKs are thin HTTP clients — language choice should match the consumer ecosystem.

## Consequences

- Server development uses Go tooling (go test, golangci-lint, goimports).
- SDKs live in `sdk/` within the same repo. API and SDK changes are committed together, eliminating version drift.
- API contract is HTTP/JSON, validated by all three SDK test suites in a single CI pipeline.
