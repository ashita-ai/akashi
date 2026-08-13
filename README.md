[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8.svg)](https://go.dev/)
[![PostgreSQL](https://img.shields.io/badge/PostgreSQL-18-336791.svg)](https://www.postgresql.org/)

**Version control for AI decisions.**

Agents in multi-agent systems have no shared memory. They contradict each other, relitigate settled questions, and leave no trail when things go wrong. Akashi fixes this: every agent checks for precedents before deciding and records its reasoning after. Conflicts are detected semantically, not by keyword — so when two agents quietly disagree about the same thing, you find out before production does.

## How it works

Two primitives: **check before deciding, trace after deciding.**

```
Before deciding                   After deciding
─────────────────────────         ───────────────────────
akashi_check                      akashi_trace
  → precedents found?               → decision stored
  → conflicts flagged?               → embeddings computed
                                     → conflicts detected
```

Decisions are written atomically with reasoning, alternatives, and evidence. Conflict detection runs asynchronously against the org's full history. Over time, `akashi_assess` lets agents record whether past decisions turned out to be correct — and that feedback reshapes which precedents surface first.

See [Conflict Detection](docs/conflicts.md) and [Subsystems](docs/subsystems.md) for internals.

## Try it in 60 seconds

No Docker, no database, no API keys. `akashi-local` is a single binary that speaks MCP over
stdio and stores everything in SQLite (see [ADR-009](adrs/)):

```bash
make build-local
./bin/akashi-local          # database created at ~/.akashi/local.db
```

Point an MCP client at it — for Claude Code:

```bash
claude mcp add akashi -- /absolute/path/to/bin/akashi-local
```

`akashi_check` and `akashi_trace` work immediately. Conflict detection in this mode is
text-based rather than semantic; see [Conflict Detection](docs/conflicts.md).

## Quick start (full stack)

```bash
docker compose -f docker-compose.complete.yml up -d
```

Everything runs locally — TimescaleDB, Qdrant, Ollama, and the Akashi server. No API keys, no external accounts. First launch downloads models (~15–25 min); subsequent starts take seconds.

```bash
curl http://localhost:8080/health       # verify
open http://localhost:8080              # audit dashboard
```

Other deployment options: [Self-Hosting Guide](docs/self-hosting.md). All env vars: [Configuration](docs/configuration.md).

## MCP integration

Your agent gains decision tracing with zero code changes. **`ApiKey` auth is recommended** — it never expires and survives restarts.

### Claude Code

```bash
claude mcp add --transport http --scope user akashi http://localhost:8080/mcp \
  --header "Authorization: ApiKey admin:${AKASHI_ADMIN_API_KEY:-admin}"
```

### Cursor, Windsurf, and other MCP clients

```json
{
  "mcpServers": {
    "akashi": {
      "url": "http://localhost:8080/mcp",
      "headers": {
        "Authorization": "ApiKey admin:<your-api-key>"
      }
    }
  }
}
```

### Tools

| Tool | Purpose |
|------|---------|
| `akashi_check` | Find precedents and conflicts before deciding |
| `akashi_trace` | Record a decision with reasoning and confidence |
| `akashi_assess` | Record whether a past decision was correct |
| `akashi_query` | Search decisions by filters or semantics |
| `akashi_conflicts` | List open conflicts between agents |
| `akashi_stats` | Decision trail health metrics |

## SDKs

| Language | Install |
|----------|---------|
| [Go](sdk/go/) | `go get github.com/ashita-ai/akashi/sdk/go/akashi` |
| [Python](sdk/python/) | `pip install git+https://github.com/ashita-ai/akashi.git#subdirectory=sdk/python` |
| [TypeScript](sdk/typescript/) | `npm install github:ashita-ai/akashi#path:sdk/typescript` |

## Docs

| | |
|---|---|
| [FAQ](docs/faq.md) | Setup, concepts, auth, integrity, SDKs, architecture |
| [Self-Hosting](docs/self-hosting.md) | Postgres-only through full stack with Qdrant and Ollama |
| [Configuration](docs/configuration.md) | Every environment variable |
| [Conflict Detection](docs/conflicts.md) | Scoring, LLM validation, resolution |
| [Detector Internals](docs/conflict-detection.md) | Operating point, judge models, evaluation, rejected alternatives |
| [Quality Scoring](docs/quality-scoring.md) | Completeness scores and anti-gaming |
| [GDPR Erasure](docs/erasure.md) | Tombstone erasure for right-to-be-forgotten |
| [IDE Hooks](docs/hooks.md) | Claude Code and Cursor integration |
| [Subsystems](docs/subsystems.md) | Embeddings, rate limiting, Qdrant pipeline |
| [Runbook](docs/runbook.md) | Health checks, monitoring, troubleshooting |
| [Diagrams](docs/diagrams.md) | Write path, read path, auth flow, schema |
| [ADRs](adrs/) | Architecture decision records |
| [OpenAPI](api/openapi.yaml) | Full API spec (also at `GET /openapi.yaml`) |
| [Contributing](CONTRIBUTING.md) | Local setup, the pre-commit gate, and the invariants a PR is judged against |

## Contributing

Contributions are welcome. The fast path needs no Docker, no database, and no API keys:

```bash
go build ./...
go test -race ./...     # unit tests only; every container-backed test is behind a build tag
```

That is the whole loop for most changes, and it takes seconds. Read
[CONTRIBUTING.md](CONTRIBUTING.md) before opening a PR — it names the two invariants
(`org_id` scoping and `valid_to IS NULL`) that a reviewer will check first, and points at
[AGENTS.md](AGENTS.md), which is normative for conventions.

## License

Apache 2.0. See [LICENSE](LICENSE).
