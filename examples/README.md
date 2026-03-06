# Akashi SDK Examples

Working examples demonstrating the Akashi Python, TypeScript, and Go SDKs.

## Prerequisites

1. Start the local stack:

```sh
docker compose -f docker-compose.complete.yml up -d
```

2. Wait for the server to be ready:

```sh
curl http://localhost:8080/health
```

3. For conflict detection, wait for Ollama model downloads to finish (10-20 min on first launch):

```sh
docker compose -f docker-compose.complete.yml logs -f ollama-init
```

## Environment variables

| Variable | Default | Description |
|---|---|---|
| `AKASHI_URL` | `http://localhost:8080` | Server base URL |
| `AKASHI_ADMIN_API_KEY` | `admin` | Admin API key (matches docker-compose.complete.yml) |

## Examples

| File | Language | What it demonstrates | Requires full stack? |
|---|---|---|---|
| `python/quickstart.py` | Python | Check, trace, query, recent | No (Postgres only) |
| `typescript/quickstart.ts` | TypeScript | Check, trace, query, recent | No (Postgres only) |
| `go/quickstart/main.go` | Go | Check, trace, query, recent | No (Postgres only) |
| `python/middleware.py` | Python | `AkashiSyncMiddleware.wrap()` — automatic check/trace | No (Postgres only) |
| `typescript/middleware.ts` | TypeScript | `withAkashi()` — automatic check/trace | No (Postgres only) |
| `python/multi_agent_conflicts.py` | Python | Two agents, conflicting decisions, conflict detection | Yes (Qdrant + Ollama) |

## Setup by language

### Python

```sh
pip install -e sdk/python       # from repo root
python examples/python/quickstart.py
```

### TypeScript

```sh
cd examples/typescript
npm install
npm run quickstart              # or: npm run middleware
```

### Go

```sh
cd examples/go
go mod tidy
go run ./quickstart
```

## Framework integrations

For CrewAI, LangChain, and Vercel AI SDK integrations, see `sdk/integrations/`.
The existing `examples/crewai/` directory demonstrates CrewAI with live LLM agents.
