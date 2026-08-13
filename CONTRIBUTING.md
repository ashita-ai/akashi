# Contributing to Akashi

This guide covers local development setup, common workflows, and the architecture you'll encounter when contributing to Akashi.

[AGENTS.md](AGENTS.md) is normative for conventions and project structure. This file is the
human-facing subset; where the two disagree, AGENTS.md wins.

## Architecture invariants

Two rules are load-bearing enough that a PR violating either will be sent back, and neither is
visible from the code you are editing. Quoting AGENTS.md:

> **Multi-tenancy via org_id.** Every query MUST include `AND org_id = $N`. There are 400+
> org_id references across the storage layer. Missing one is a data leak. When adding a new
> query, always scope by org_id.

> **Bi-temporal model.** Decisions have `valid_from`/`valid_to` (business time) and
> `transaction_time` (system time). Active records have `valid_to IS NULL`. Always include this
> filter in queries that should return current state.

Nothing mechanical enforces either one — `go vet`, the linter, and the type system are all blind
to a dropped `WHERE` clause. Review is the only check, so make it easy: keep storage queries in
the one-file-per-entity layout `internal/storage/` already uses.

Some changes need a maintainer conversation before the code, not after. From AGENTS.md's
"Ask first" list: changing the RBAC role required by an endpoint, adding a direct dependency to
`go.mod`, modifying the MCP tool definitions, and any schema change that widens access.

Finally, a house rule that surprises people: **every PR description must end with a blockquote
about Marvel, DC, Harry Potter, Star Wars, Star Trek, or Tolkien** — an argument for one universe
over another, or an original haiku. See AGENTS.md for the format. This is not a joke entry; PRs
without it get sent back.

## Local dev setup

### Prerequisites

- **Go 1.26+** ([install](https://go.dev/dl/))
- **Docker** (only for integration tests and the local stack — unit tests need none)
- **Atlas CLI** ([install](https://atlasgo.io/getting-started#installation)) — database migration tool
- **Python 3** — `make preflight` runs `scripts/check_doc_config_consistency.py`
- **golangci-lint v2.11.0** — `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.11.0`
- **goimports v0.42.0** — `go install golang.org/x/tools/cmd/goimports@v0.42.0` (used by `make fmt`)
- **govulncheck v1.1.4** — `go install golang.org/x/vuln/cmd/govulncheck@v1.1.4` (used by `make ci`)
- **Node.js 22** (only if working on the UI — matches CI and the Dockerfile)

The three `go install` tools land in `$(go env GOPATH)/bin`, usually `~/go/bin`. Put it on your
`PATH` or `make lint` will report the tool as missing rather than the code as clean. The pinned
versions are the ones CI installs (`.github/workflows/ci.yml`); a different golangci-lint minor
can report findings CI does not, and vice versa.

### Starting the test database

Tests use [testcontainers-go](https://golang.testcontainers.org/) to spin up ephemeral TimescaleDB instances automatically — no manual database setup required for most work.

If you want a persistent local stack for manual testing or running the server:

```sh
docker compose -f docker-compose.complete.yml up -d postgres qdrant
```

This starts TimescaleDB (with pgvector) on port 5432 and Qdrant on port 6333, both bound to
loopback only. Verify before going further — the containers report healthy whether or not the
ports reached your host:

```sh
psql postgres://akashi:akashi@localhost:5432/akashi -c 'select 1'
```

If you already run Postgres or Qdrant locally, the bind fails loudly. Set `POSTGRES_PORT` or
`QDRANT_PORT` to move the published port, and adjust `DATABASE_URL` to match.

For the complete stack (database + Qdrant + Ollama + Akashi server):

```sh
docker compose -f docker-compose.complete.yml up -d
```

First launch downloads Ollama models (~7 GB total) and takes 15–25 minutes. Track progress with:

```sh
docker compose -f docker-compose.complete.yml logs -f ollama-init
```

### Running the server from source

Once the database is reachable, point a locally built binary at it. Akashi reads a `.env` from
the working directory on startup (`akashi.go:112`).

```sh
cp .env.example .env
```

Then edit `.env` for a host run — `.env.example` is written for the container:

- `AKASHI_JWT_PRIVATE_KEY` and `AKASHI_JWT_PUBLIC_KEY` point at `/data/*.pem`, which does not
  exist on your host. **Comment both out.** When both are empty, Akashi generates an ephemeral
  Ed25519 keypair in memory (`internal/auth/auth.go:58`) — fine for development, and it means
  tokens do not survive a restart.
- `AKASHI_EMBEDDING_PROVIDER=noop` keeps the server off Ollama and OpenAI. Semantic search and
  conflict detection then run at low recall by design — see [Embedding provider note](#embedding-provider-note).

```sh
make build
./bin/akashi
curl -s http://localhost:8080/health | jq .
```

Configuration errors are fatal and accumulate: the server reports every invalid variable at
once, each naming its remedy, rather than starting in a degraded state.

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

```sh
make new-migration name=add_foo_index
```

That picks the next sequential number, writes the `-- NNN: description.` header, and rehashes
`atlas.sum` — the three steps most easily got wrong by hand. Write your SQL into the created
file, then:

```sh
make migrate-validate
```

Stage both the `.sql` file and `migrations/atlas.sum`. If validation fails after you edit the
file, `make migrate-hash` regenerates the checksum.

**Never edit an existing migration** — always create a new one. Applied migrations are immutable.

Two mechanisms apply migrations, and they are not the same one. The server runs a simple
forward-only runner over the embedded files at startup, tracking what it has applied in
`schema_migrations` (`internal/storage/migrate.go`); set `AKASHI_SKIP_EMBEDDED_MIGRATIONS=true`
to suppress it. Atlas owns checksum integrity, linting, and production application — `make
migrate-apply` is the operator path, not part of local development.

## Pre-commit checklist

```sh
make preflight
```

That is the whole gate, and it is the same list CI runs in its build job — tidy plus a
`go.mod`/`go.sum` diff, doc/config consistency, Atlas migration validation, `go build`, the
lite build, lint, and vet. It needs no Docker, no database, and no API keys.

The target is the single definition on purpose. This file used to carry a hand-copied
five-command version that had drifted: it omitted `scripts/check_doc_config_consistency.py`
and the `-tags lite` build, both of which CI enforces on every push. If you want the raw
commands, they are kept as a comment directly above the `preflight` target in the `Makefile`.

If `go mod tidy` changes `go.mod` or `go.sum`, stage them in the commit.

If migration validation fails, regenerate the checksum with `make migrate-hash` and stage
`migrations/atlas.sum`.

Before pushing:

```sh
make test-unit    # fast, no containers
make test         # full suite, requires Docker
```

Or the full CI mirror, which is `preflight` plus govulncheck and the test suite:

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
- [faq.md](docs/faq.md) — Concepts, auth, integrity; includes the five-level RBAC role table
