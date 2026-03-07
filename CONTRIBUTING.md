# Contributing to Akashi

This guide covers local development setup, common workflows, and the architecture you'll encounter when contributing to Akashi.

## Local dev setup

### Prerequisites

- **Go 1.26+** ([install](https://go.dev/dl/))
- **Docker** (for testcontainers and the local stack)
- **Atlas CLI** ([install](https://atlasgo.io/getting-started#installation)) — database migration tool
- **golangci-lint v2.9.0** — `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.9.0`
- **Node.js 20+** (only if working on the UI)

### Starting the test database

Tests use [testcontainers-go](https://golang.testcontainers.org/) to spin up ephemeral TimescaleDB instances automatically — no manual database setup required for most work.

If you want a persistent local stack for manual testing or running the server:

```sh
docker compose -f docker-compose.complete.yml up -d postgres qdrant
```

This starts TimescaleDB (with pgvector) on port 5432 and Qdrant on port 6333.

For the complete stack (database + Qdrant + Ollama + Akashi server):

```sh
docker compose -f docker-compose.complete.yml up -d
```

First launch downloads Ollama models (~7 GB total) and takes 10–20 minutes. Track progress with:

```sh
docker compose -f docker-compose.complete.yml logs -f ollama-init
```

### Running tests

```sh
# All tests (recommended before pushing)
go test -race ./...

# A specific package
go test -race ./internal/server/...

# Verbose output
go test -race -v ./internal/storage/...
```

### Disabling Qdrant in tests

When `QDRANT_URL` is unset (the default), Qdrant is not used. Tests that exercise search fall back to PostgreSQL text search. This is the normal local development experience.

If you set `QDRANT_URL` (e.g., `http://localhost:6333`) and Qdrant is unreachable, search tests may fail. Either start Qdrant or unset the variable.

### Running SDK tests

Each SDK lives under `sdk/` and has its own test suite:

```sh
# Go SDK
cd sdk/go && go test ./...

# Python SDK
cd sdk/python && pip install -e '.[dev]' && pytest

# TypeScript SDK
cd sdk/typescript && npm ci && npm test
```

SDK integration tests that hit a live server require `AKASHI_URL` and `AKASHI_API_KEY` to be set.

## Writing a migration

1. Create `migrations/NNN_description.sql` where `NNN` is the next sequential number (check the `migrations/` directory for the current highest).
2. Start the file with a comment: `-- NNN: Brief description of what this migration does.`
3. Regenerate the checksum file:
   ```sh
   atlas migrate hash --dir file://migrations
   ```
4. Validate:
   ```sh
   atlas migrate validate --dir file://migrations
   ```
5. Stage both the `.sql` file and `migrations/atlas.sum` in your commit.

**Never edit an existing migration** — always create a new one. Applied migrations are immutable.

## Pre-commit checklist

Run these before every commit. CI rejects failures on all of them.

```sh
go mod tidy && git diff --exit-code go.mod go.sum
go build ./...
go vet ./...
golangci-lint run ./...
atlas migrate validate --dir file://migrations
```

If `go mod tidy` changes `go.mod` or `go.sum`, stage them in the commit.

If `atlas migrate validate` fails, regenerate the checksum with `atlas migrate hash --dir file://migrations` and stage `migrations/atlas.sum`.

Before pushing, also run:

```sh
go test -race -count=1 ./...
```

Or run the full CI mirror locally:

```sh
make ci
```

## Embedding provider note

Tests that exercise conflict detection or semantic search need an embedding provider. When none is configured (the default for local development), the noop embedder is used and vector similarity is disabled. Tests that assert on semantic results will produce low recall in this mode. **This is expected behavior, not a bug.**

To enable embeddings locally, set the appropriate environment variables (e.g., `AKASHI_EMBEDDING_PROVIDER=ollama`, `OLLAMA_URL=http://localhost:11434`). The complete docker-compose stack handles this automatically.

## Request path overview

Akashi follows a layered flow:

1. `cmd/akashi` wires dependencies and starts background loops.
2. `internal/server` handles HTTP concerns (auth, middleware, request parsing).
3. `internal/service` contains business logic shared by HTTP and MCP.
4. `internal/storage` handles SQL and transactional persistence.

Keep domain decisions in service/storage layers, not handlers.

## Core data model concepts

- **Bi-temporal decisions:**
  - `valid_from` / `valid_to` track business validity.
  - `transaction_time` tracks write-time history.
- **Revision chain:**
  - New revisions link via `supersedes_id`.
  - Historical rows remain queryable.
- **Evidence + alternatives:**
  - Stored separately but written atomically with decisions in trace paths.

## Search pipeline

- PostgreSQL is source of truth.
- Qdrant is an optional accelerator.
- `search_outbox` provides eventual-consistency sync:
  - Decision writes enqueue outbox rows in the same transaction.
  - Outbox worker upserts/deletes in Qdrant.
  - Text search fallback keeps queries functional if Qdrant is down.

## Multi-tenancy rules

- Every query must scope by `org_id`.
- Admin/platform actions should still preserve tenant isolation unless explicitly global.
- Cross-org behavior must be justified and tested.

## Operational safety expectations

- Avoid startup with partial schema state.
- Prefer bounded loops and context-aware shutdown.
- Treat audit durability regressions as high priority.
