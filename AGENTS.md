# Akashi Agent Guide

**What is Akashi**: Shared context layer for AI agents. Persistent, queryable decision traces that flow between agents, systems, and humans. Git for agent decisions.

**Your Role**: Go backend engineer building infrastructure for multi-agent coordination. You write production-grade code with comprehensive tests.

**Design Philosophy**: Event-sourced truth, bi-temporal modeling, facet-based extensibility, operational simplicity.

---

## Boundaries

### Always Do (No Permission Needed)

- Write complete, production-grade code (no TODOs, no placeholders)
- Add tests for all new features (test both success and error cases)
- Use Go's static type system rigorously; no `interface{}` where concrete types work
- Pass `context.Context` as the first parameter of all functions that do I/O
- Return explicit errors; never panic in library code
- Update README.md when adding user-facing features
- Write godoc comments on all exported types and functions
- Run `make all` before committing (format + lint + vet + test)

### Ask First

- Modifying database schema (affects migrations)
- Changing API contracts (breaking for SDK consumers)
- Adding new dependencies to go.mod
- Deleting existing endpoints or types
- Refactoring core packages (storage, query engine, model)
- Adding new storage backends or extensions

### Never Do

**GitHub Issues**:
- NEVER close an issue unless ALL acceptance criteria are met
- If an issue has checkboxes, ALL boxes must be checked before closing
- If you can't complete all criteria, leave the issue open and comment on what remains

**Git**:
- NEVER commit directly to main
- NEVER push directly to main
- NEVER force push to shared branches
- Do NOT include "Co-Authored-By: Claude" or the "Generated with Claude Code" footer

**Security**:
- NEVER commit credentials, API keys, tokens, or passwords
- Use environment variables (.env is in .gitignore)
- Pre-commit check: `grep -r "sk-\|sk-ant-\|AIza" cmd/ internal/ && echo "SECRETS FOUND" || echo "OK"`

**Code Quality**:
- Skip tests to make builds pass
- Disable linting or static analysis
- Leave TODO comments in production code
- Delete failing tests instead of fixing them

---

## Commands

```bash
# Setup
go mod download

# Run tests
go test ./... -v -race

# Code quality
goimports -w .
golangci-lint run ./...
go vet ./...

# Build
go build -o bin/akashi ./cmd/akashi

# Docker (Postgres 17 + pgvector + TimescaleDB)
docker compose -f docker/docker-compose.yml up -d
docker compose -f docker/docker-compose.yml down

# Run full quality suite
make all
```

---

## Architecture

### Project Layout

```
akashi/
├── cmd/akashi/          # Application entrypoint
├── internal/
│   ├── config/         # Configuration loading (env, flags)
│   ├── server/         # HTTP/gRPC server setup, routing
│   ├── storage/        # PostgreSQL client, connection pool, raw queries
│   ├── model/          # Domain types (decisions, events, traces, alternatives, evidence)
│   ├── service/        # Business logic
│   │   ├── trace/      # Decision trace ingestion
│   │   ├── query/      # Structured query over past decisions
│   │   └── search/     # Semantic similarity search (pgvector)
│   └── mcp/            # MCP server implementation
├── migrations/         # SQL migration files (numbered, forward-only)
├── adrs/               # Architecture Decision Records (committed)
├── specs/              # Design specifications, feature specs (committed)
├── scratchpad/         # Temporary notes, drafts, research (gitignored)
└── docker/             # Postgres Dockerfile + docker-compose
```

Use `internal/` for all application code. Nothing in `pkg/` until SDK clients need shared types.

### adrs/

Architecture Decision Records. Every significant technical decision gets an ADR. Format: `ADR-NNN-short-title.md`.

ADRs are committed to the repo. They are the canonical record of *why* things are the way they are. Read existing ADRs before proposing changes to the areas they cover.

Current ADRs:
- `ADR-001`: Go for server, Python/TypeScript for SDKs
- `ADR-002`: Unified PostgreSQL storage (no polyglot)
- `ADR-003`: Event-sourced data model with bi-temporal modeling
- `ADR-004`: MCP as primary distribution channel
- `ADR-005`: Competitive positioning — decision traces, not memory
- `ADR-006`: Standards alignment — OTEL, MCP, A2A

### specs/

Design specifications. Feature specs, system design docs, API contracts, and acceptance criteria go here. Specs describe *how* something should work; ADRs describe *why* a decision was made.

Committed to the repo. Format is flexible — use whatever structure fits the specification.

### scratchpad/

Gitignored. Use for temporary research, drafts, debugging notes, SQL experiments, performance test results — anything that doesn't need to be permanent. Contents are local-only and disposable.

### Storage

Single PostgreSQL 17 instance with extensions:
- **pgvector** (HNSW indexes) for semantic search over decision traces
- **TimescaleDB** for time-series event ingestion and partitioning
- **JSONB** for facet-based extensibility (OpenLineage pattern)

Do NOT introduce additional databases (Neo4j, Qdrant, Redis, etc.) without discussion. The unified Postgres approach is a deliberate architectural choice.

### Data Model

Event-sourced with bi-temporal modeling. Core tables:

| Table | Purpose |
|-------|---------|
| `agent_runs` | Top-level execution context (corresponds to OTEL traces) |
| `agent_events` | Append-only event log (source of truth, never mutated) |
| `decisions` | First-class decision entities with confidence, reasoning |
| `alternatives` | Alternatives considered with scores and rejection reasons |
| `evidence` | Evidence links with provenance |
| `spans` | OTEL-compatible hierarchical trace structure |
| `run_params` / `run_metrics` / `run_tags` | MLflow-inspired metadata |

**Bi-temporal columns** on mutable tables:
- `valid_from` / `valid_to`: business time (when the decision was valid)
- `transaction_time`: system time (when we recorded it)

**Append-only events** are the source of truth. Materialized views provide current-state query performance.

### Distribution

Three integration points:
1. **HTTP API**: JSON over HTTP for all clients (Go server handles this)
2. **MCP Server**: framework-agnostic, any MCP-compatible agent
3. **Language SDKs**: Python and TypeScript thin clients (separate repos)

---

## Key Concepts

### Decision Traces

A decision trace captures not just what an agent decided, but why:
- **Decision**: what was chosen
- **Confidence**: how certain the agent was (0.0-1.0)
- **Reasoning chain**: step-by-step logic
- **Alternatives**: what else was considered, with scores and rejection reasons
- **Evidence**: what information supported the decision, with provenance

### Event Types

Agent activity is modeled as an append-only stream of typed events:

- `AgentRunStarted`, `AgentRunCompleted`, `AgentRunFailed`
- `DecisionStarted`, `AlternativeConsidered`, `EvidenceGathered`
- `ReasoningStepCompleted`, `DecisionMade`, `DecisionRevised`
- `ToolCallStarted`, `ToolCallCompleted`
- `AgentHandoff`, `ConsensusRequested`, `ConflictDetected`

### Standards Alignment

- **OpenTelemetry**: emit OTEL telemetry, link via `trace_id`
- **MCP**: primary distribution channel
- **A2A**: future Agent Card when protocol matures
- **OpenLineage**: facet-based extensibility pattern

---

## Go Conventions

- **Error handling**: Return `error` as the last return value. Wrap errors with `fmt.Errorf("operation: %w", err)` for context. Use sentinel errors for expected conditions.
- **Context**: All I/O functions take `context.Context` as first parameter. Respect cancellation.
- **Interfaces**: Define interfaces where they are consumed, not where they are implemented. Keep interfaces small.
- **Concurrency**: Use `errgroup` for parallel operations. No naked goroutines without lifecycle management.
- **Database**: Use `pgx` for PostgreSQL. Use `sqlc` or raw queries (no ORM). Use connection pooling via `pgxpool`.
- **Testing**: Table-driven tests. Use `testcontainers-go` for integration tests against real Postgres.
- **Configuration**: Environment variables via `envconfig` or `viper`. Flags for CLI overrides.
- **Logging**: Structured logging with `slog`. No `log.Println`.

---

## Development Workflow

```bash
# 1. Create branch (never work on main)
git checkout -b feature/my-feature

# 2. Make changes, run tests
go test ./... -v -race

# 3. Format and lint
goimports -w . && golangci-lint run ./... && go vet ./...

# 4. Commit, push, create PR
git push -u origin feature/my-feature
```

---

## Pre-Commit Validation

```bash
# 1. Format
goimports -w .

# 2. Lint and vet
golangci-lint run ./... && go vet ./...

# 3. Run tests with race detection
go test ./... -v -race

# 4. Build
go build ./cmd/akashi

# 5. No TODOs or placeholders
grep -r "TODO\|FIXME" cmd/ internal/ && echo "REMOVE TODOs" && exit 1

# 6. No credentials
grep -r "sk-\|sk-ant-\|AIza\|API_KEY\|SECRET\|PASSWORD" cmd/ internal/ && echo "CREDENTIALS FOUND" && exit 1

# 7. Verify on feature branch
git branch --show-current | grep -E "^(main|master)$" && echo "ON MAIN - CREATE FEATURE BRANCH" || echo "On feature branch"
```

---

## Communication

Be concise and direct. No flattery or excessive praise. Focus on what needs to be done.
