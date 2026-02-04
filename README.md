# Kyoyu

Shared context layer for AI agents. Persistent, queryable decision traces that flow between agents, systems, and humans.

## What it does

Kyoyu captures structured decision traces from multi-agent systems: what was decided, what alternatives were considered, what evidence supported the decision, and how confident the agent was. This context is queryable by other agents, enabling coordination through shared understanding rather than opaque message passing.

## Architecture

- **Go server** exposing HTTP API and MCP server
- **PostgreSQL 17** with pgvector (semantic search) and TimescaleDB (time-series events)
- **Event-sourced** with bi-temporal modeling for full auditability
- **Python SDK** and **TypeScript SDK** as thin clients (separate repos)

## Quick start

```bash
# Start Postgres with extensions
make docker-up

# Build and run
make build
./bin/kyoyu

# Run tests
make test

# Full quality check (format + lint + vet + test + build)
make all
```

## Project structure

```
cmd/kyoyu/          Application entrypoint
internal/
  config/           Configuration loading
  server/           HTTP/gRPC server, routing
  storage/          PostgreSQL client, connection pool
  model/            Domain types
  service/
    trace/          Decision trace ingestion
    query/          Structured query engine
    search/         Semantic similarity search
  mcp/              MCP server implementation
migrations/         SQL migration files
docker/             Postgres Dockerfile + docker-compose
```

## Requirements

- Go 1.25+
- Docker (for Postgres)
- golangci-lint
- goimports

## License

Proprietary. See [LICENSE](LICENSE).
