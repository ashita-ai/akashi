<p align="center">
  <img src="docs/assets/logo.png" alt="Akashi" width="400" />
</p>

[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.25-00ADD8.svg)](https://go.dev/)
[![PostgreSQL](https://img.shields.io/badge/PostgreSQL-18-336791.svg)](https://www.postgresql.org/)

**Git blame for AI decisions.**

Multi-agent AI systems are moving from demos to production, but their decisions are invisible. When something goes wrong, nobody can answer: *who decided what, when, why, and what alternatives were considered?*

Akashi is the decision audit trail. Every agent decision gets recorded with its full reasoning chain, the alternatives that were weighed, the evidence that informed it, and the confidence level. When the CTO asks "why did the AI do that?" or an auditor asks for proof of decision traceability, you have the answer.

## Quick start

Two modes. Pick one.

### Complete local stack (recommended for trying Akashi)

Everything runs in Docker — TimescaleDB, Qdrant, Ollama, and the Akashi server. No API keys, no external accounts.

```bash
docker compose -f docker-compose.complete.yml up -d
```

**First launch builds the server image from source and downloads the Ollama embedding model (~670MB).** Expect 5–15 minutes on first run depending on your machine and network. Subsequent launches start in seconds.

Watch akashi come up:

```bash
docker compose -f docker-compose.complete.yml logs -f akashi
```

The server is ready when you see a `listening` log line. The Ollama model download continues in the background — embeddings activate automatically once it completes.

```bash
curl http://localhost:8080/health
# Open http://localhost:8080 for the audit dashboard
```

If port 8080 is already in use, set `AKASHI_PORT` before starting:

```bash
echo "AKASHI_PORT=8081" > .env
docker compose -f docker-compose.complete.yml up -d
# Open http://localhost:8081
```

### Binary only (bring your own infrastructure)

Just the Akashi server container. You provide TimescaleDB, Qdrant, and an embedding API key.

```bash
cp docker/env.example .env
# Edit .env: set DATABASE_URL, AKASHI_ADMIN_API_KEY, and optionally QDRANT_URL / OPENAI_API_KEY
docker compose up -d
```

First run builds the server image from source (~3–5 minutes). After that, `docker compose up -d` starts in seconds. To force a rebuild after updating the source: `docker compose up -d --build`.

Required variables:

| Variable | Description |
|----------|-------------|
| `DATABASE_URL` | TimescaleDB connection string (pgvector + TimescaleDB extensions required) |
| `AKASHI_ADMIN_API_KEY` | Bootstrap API key for the admin agent |

Optional (server starts without them — search falls back to text):

| Variable | Description |
|----------|-------------|
| `QDRANT_URL` | Qdrant endpoint for vector search |
| `OPENAI_API_KEY` | Enables OpenAI embeddings and LLM conflict validation |
| `OLLAMA_URL` | Ollama endpoint for local embeddings |

See [Configuration](docs/configuration.md) for all variables.

### Record your first decision

```bash
# Get a token (default admin key for local dev is "admin")
TOKEN=$(curl -s -X POST http://localhost:8080/auth/token \
  -H 'Content-Type: application/json' \
  -d '{"agent_id": "admin", "api_key": "admin"}' \
  | jq -r '.data.token')

# Record a decision with reasoning, alternatives, and evidence
curl -X POST http://localhost:8080/v1/trace \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "agent_id": "admin",
    "decision": {
      "decision_type": "architecture",
      "outcome": "use microservices for the payment system",
      "confidence": 0.85,
      "reasoning": "Payment processing needs independent scaling and deployment. A monolith couples payment latency to unrelated features.",
      "alternatives": [
        {"label": "microservices", "selected": true, "score": 0.85,
         "rationale": "Independent scaling, isolated failures, team autonomy"},
        {"label": "monolith", "selected": false, "score": 0.65,
         "rationale": "Simpler deployment but couples all domains"}
      ],
      "evidence": [
        {"source_type": "analysis", "content": "Payment traffic spikes 10x during promotions while other services stay flat"}
      ]
    }
  }'
```

### Check for precedents before deciding

```bash
# Before making a decision, check if similar ones exist
curl -X POST http://localhost:8080/v1/check \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"decision_type": "architecture", "query": "microservices vs monolith"}'
```

### Search the audit trail

```bash
curl -X POST http://localhost:8080/v1/search \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"query": "scaling decisions for high-traffic services", "limit": 5}'
```

## MCP integration

The fastest way to use Akashi is through MCP. Your agent gains decision tracing with zero code changes.

### Claude Code (simplest)

```bash
# Get a token first
TOKEN=$(curl -s -X POST http://localhost:8080/auth/token \
  -H 'Content-Type: application/json' \
  -d '{"agent_id": "admin", "api_key": "admin"}' | jq -r '.data.token')

# Add globally (all projects on this machine)
claude mcp add --transport http --scope user akashi http://localhost:8080/mcp \
  --header "Authorization: Bearer $TOKEN"

# Or scope it to just the current project
claude mcp add --transport http --scope project akashi http://localhost:8080/mcp \
  --header "Authorization: Bearer $TOKEN"
```

### Cursor, Windsurf, and other MCP clients

Add to your MCP configuration file (`~/.cursor/mcp.json`, `~/.windsurf/mcp.json`, etc.):

```json
{
  "mcpServers": {
    "akashi": {
      "url": "http://localhost:8080/mcp",
      "headers": {
        "Authorization": "Bearer <your-jwt-token>"
      }
    }
  }
}
```

### Available tools

| Tool | Purpose |
|------|---------|
| `akashi_check` | Look for precedents before making a decision |
| `akashi_trace` | Record a decision with reasoning and confidence |
| `akashi_query` | Structured query with filters (type, agent, confidence) |
| `akashi_search` | Semantic similarity search over past decisions |
| `akashi_recent` | Quick overview of recent decisions |

Three prompts guide the workflow: `agent-setup` (system prompt with the check-before/record-after pattern), `before-decision` (precedent lookup guidance), and `after-decision` (recording reminder).

### What this looks like in practice

A planner agent decides to use microservices for a payment system and records it via `akashi_trace`. Later, a coder agent is about to choose a monolith for the same system. It calls `akashi_check` first and discovers the planner already made a conflicting decision with different reasoning. The coder sees the conflict, reviews the planner's evidence, and either aligns or records a competing decision with its own rationale. Either way, the disagreement is visible and auditable.

## Three interfaces, one service

| Interface | Endpoint | Audience |
|-----------|----------|----------|
| **HTTP API** | `/v1/...` | Programmatic integrators, SDKs, CI/CD |
| **MCP server** | `/mcp` | AI agents in Claude, Cursor, Windsurf |
| **Audit dashboard** | `/` | Human reviewers, auditors, operators |

All three share the same storage, auth, and embedding provider.

## What the audit trail captures

Every decision trace records:

- **The decision** -- what was chosen and the agent's confidence level
- **Reasoning** -- step-by-step logic explaining why
- **Rejected alternatives** -- what else was considered, with scores and rejection reasons
- **Supporting evidence** -- what information backed the decision, with provenance
- **Temporal context** -- when it was made, when it was valid (bi-temporal model)
- **Integrity proof** -- SHA-256 content hash and Merkle tree batch verification
- **Conflicts** -- when two agents disagree on the same question

## SDKs

| Language | Path | Install |
|----------|------|---------|
| Go | [`sdk/go/`](sdk/go/) | `go get github.com/ashita-ai/akashi/sdk/go/akashi` |
| Python | [`sdk/python/`](sdk/python/) | `pip install git+https://github.com/ashita-ai/akashi.git#subdirectory=sdk/python` |
| TypeScript | [`sdk/typescript/`](sdk/typescript/) | `npm install github:ashita-ai/akashi#path:sdk/typescript` |

All SDKs provide: `Check`, `Trace`, `Query`, `Search`, `Recent`. Auth token management is automatic.

## Architecture

```mermaid
flowchart TD
    C1["Audit Dashboard"] -->|"/"| AUTH
    C2["MCP Clients<br/>Claude, Cursor, Windsurf"] -->|"/mcp"| AUTH
    C3["HTTP Clients<br/>SDKs, CI/CD, scripts"] -->|"/v1/"| AUTH

    AUTH["Auth + Middleware<br/>Ed25519 JWT, RBAC, tracing"]

    AUTH --> TRACE["Trace Buffer<br/>in-memory batch + COPY flush"]
    AUTH --> QUERY["Query Engine<br/>SQL filters + bi-temporal"]
    AUTH --> SEARCH["Semantic Search<br/>Qdrant / pgvector fallback"]
    AUTH --> CONFLICT["Conflict Detection<br/>semantic scoring"]

    TRACE --> PG
    QUERY --> PG
    SEARCH --> PG
    CONFLICT --> PG

    PG[("PostgreSQL 18<br/>pgvector + TimescaleDB")]

    style C1 fill:#f0f4ff,stroke:#4a6fa5
    style C2 fill:#f0f4ff,stroke:#4a6fa5
    style C3 fill:#f0f4ff,stroke:#4a6fa5
    style AUTH fill:#fff3e0,stroke:#e65100
    style TRACE fill:#f9f9f9,stroke:#999
    style QUERY fill:#f9f9f9,stroke:#999
    style SEARCH fill:#f9f9f9,stroke:#999
    style CONFLICT fill:#f9f9f9,stroke:#999
    style PG fill:#e8f5e9,stroke:#2e7d32
```

## Documentation

| Document | Description |
|----------|-------------|
| [Configuration](docs/configuration.md) | All environment variables with defaults and descriptions |
| [Subsystems](docs/subsystems.md) | Embedding provider, rate limiting, and Qdrant search pipeline internals |
| [Technical Deep Dive](docs/technical-deep-dive.md) | Architecture walkthrough, data model, code organization |
| [Runbook](docs/runbook.md) | Production operations: health checks, monitoring, troubleshooting |
| [Diagrams](docs/diagrams.md) | Mermaid diagrams of write path, read path, auth flow, schema |
| [ADRs](adrs/) | Architecture decision records (8 technical decisions) |
| [OpenAPI Spec](api/openapi.yaml) | Full API specification (also served at `GET /openapi.yaml`) |

## Building from source

```bash
# Without UI
make build
DATABASE_URL=postgres://... AKASHI_ADMIN_API_KEY=admin ./bin/akashi

# With the embedded audit dashboard
make build-with-ui
DATABASE_URL=postgres://... AKASHI_ADMIN_API_KEY=admin ./bin/akashi
# Open http://localhost:8080
```

The binary requires a PostgreSQL 18 database with the `pgvector` and `timescaledb` extensions pre-installed (see `docker/init.sql`). Qdrant and an embedding provider are optional — the server starts without them and falls back to text search.

For a local database during development:

```bash
# Start just the database and Qdrant (no Akashi binary — run that from source)
docker run -d --name akashi-pg \
  -e POSTGRES_USER=akashi -e POSTGRES_PASSWORD=akashi -e POSTGRES_DB=akashi \
  -v "$(pwd)/docker/init.sql:/docker-entrypoint-initdb.d/01-init.sql:ro" \
  -p 5432:5432 timescale/timescaledb-ha:pg18

docker run -d --name akashi-qdrant -p 6333:6333 qdrant/qdrant:v1.13.6

DATABASE_URL=postgres://akashi:akashi@localhost:5432/akashi?sslmode=disable \
QDRANT_URL=http://localhost:6333 \
AKASHI_ADMIN_API_KEY=admin \
./bin/akashi
```

## Testing

Tests use [testcontainers-go](https://golang.testcontainers.org/) for real TimescaleDB + pgvector instances. No mocks for the storage layer.

```bash
make test              # Full suite (requires Docker)
go test -race ./...    # Go tests with race detection
```

## Requirements

- Go 1.25+
- Docker (for testcontainers and local stack)

## License

Apache 2.0. See [LICENSE](LICENSE).
