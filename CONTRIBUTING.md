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

## Running UI tests

The audit dashboard has unit tests (Vitest) and end-to-end tests (Playwright).

```sh
cd ui

# Unit tests
npm ci
npm test

# End-to-end tests (requires a running Akashi server)
npx playwright install   # first time only
npm run test:e2e
```

Vitest runs component-level tests against the React SPA. Playwright tests exercise the
full browser workflow against a live server. When adding new UI features, add at least a
Vitest unit test for any non-trivial logic.

## Running fuzz tests

Go native fuzz tests cover critical input parsing paths (content hashing, Merkle tree
construction, token validation, agent ID validation, JSON decoding):

```sh
# Run all fuzz targets for 30 seconds each
go test -fuzz=Fuzz -fuzztime=30s ./internal/integrity/...
go test -fuzz=Fuzz -fuzztime=30s ./internal/auth/...
go test -fuzz=Fuzz -fuzztime=30s ./internal/model/...
go test -fuzz=Fuzz -fuzztime=30s ./internal/server/...
```

Fuzz tests are not part of the normal `go test ./...` run — they require the `-fuzz` flag.
Run them when modifying parsing or validation logic.

## Embedding provider note

Tests that exercise conflict detection or semantic search need an embedding provider. When none is configured (the default for local development), the noop embedder is used and vector similarity is disabled. Tests that assert on semantic results will produce low recall in this mode. **This is expected behavior, not a bug.**

To enable embeddings locally, set the appropriate environment variables (e.g., `AKASHI_EMBEDDING_PROVIDER=ollama`, `OLLAMA_URL=http://localhost:11434`). The complete docker-compose stack handles this automatically.

## Architecture reference

For deeper context on the codebase, see:

- [decisions.md](docs/decisions.md) — Decision model, trace flow, bi-temporal data, embeddings
- [conflicts.md](docs/conflicts.md) — Conflict detection pipeline, scoring, resolution
- [subsystems.md](docs/subsystems.md) — Embedding providers, rate limiting, search pipeline
- [diagrams.md](docs/diagrams.md) — Mermaid diagrams of all major data flows
- [configuration.md](docs/configuration.md) — Full environment variable reference
