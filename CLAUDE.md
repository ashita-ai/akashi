# ashita-ai/akashi

Decision trace layer for multi-agent AI systems ("git blame for AI decisions").

## Tech stack

- **Server:** Go 1.25, stdlib `net/http` (Go 1.22+ routing), no framework
- **Database:** PostgreSQL 18 + pgvector + TimescaleDB, Atlas for migrations
- **Auth:** Ed25519 JWT + Argon2id API keys, RBAC (platform_admin > org_owner > admin > agent > reader)
- **UI:** React 19, TypeScript, Vite, Tailwind CSS (embedded via `go:embed` with `ui` build tag)
- **SDKs:** Go, Python, TypeScript (in `sdk/`)
- **Testing:** `testing` + testify assertions, testcontainers-go for integration tests
- **Lint:** golangci-lint v2.8.0, Atlas migrate validate

## Commands

**Before every commit (mandatory, CI rejects failures):**
```sh
go mod tidy && git diff --exit-code go.mod go.sum
go build ./...
go vet ./...
golangci-lint run ./...
atlas migrate validate --dir file://migrations
```

**Before every push (mandatory, CI runs with `-race`):**
```sh
go test -race -count=1 ./...
```

**Build:**
```sh
go build ./...                           # without UI
cd ui && npm ci && npm run build && cd .. # build UI assets first
go build -tags ui ./...                  # with embedded React SPA
make ci                                  # full local CI mirror
```

**If go mod tidy changes go.mod/go.sum**, stage them in the commit.
**If atlas validate fails**, run `atlas migrate hash --dir file://migrations` and stage `migrations/atlas.sum`.
**golangci-lint location:** `~/go/bin/golangci-lint` (install: `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.8.0`).

## Project structure

```
cmd/akashi/          Server entrypoint. Config loading, dependency wiring, signal handling.
internal/
  server/            HTTP handlers (handlers*.go), middleware (middleware.go), SSE broker, MCP server.
  storage/           PostgreSQL queries. One file per entity (decisions.go, agents.go, events.go...).
  service/           Business logic. decisions/ (trace pipeline), embedding/, quality/, trace/ (event buffer, WAL).
  model/             Domain types. Decision, AgentEvent, Alternative, Evidence, Grant, etc.
  config/            Env var loading and validation.
  auth/              JWT issuing/verification, API key hashing (Argon2id).
  authz/             RBAC enforcement, grant cache, access filtering.
  conflicts/         Embedding-based conflict detection + LLM validation.
  integrity/         SHA-256 content hashes, Merkle tree proofs.
  search/            Qdrant vector search with PostgreSQL text fallback.
  ratelimit/         Pluggable token bucket rate limiter.
  telemetry/         OpenTelemetry setup (traces + metrics).
  testutil/          Shared test helpers (testcontainers, test DB, test logger).
migrations/          Sequential SQL files (001..037). Atlas-managed checksums.
adrs/                Technical architecture decision records (ADR-001 through ADR-008).
sdk/                 Go, Python, TypeScript client SDKs.
ui/                  React 19 SPA (audit dashboard). Embedded via go:embed when built with -tags ui.
docs/                Configuration reference, .env.example.
```

## Architecture patterns

**Multi-tenancy via org_id.** Every query MUST include `AND org_id = $N`. There are 84 org_id filters across the storage layer. Missing one is a data leak. When adding a new query, always scope by org_id.

**Bi-temporal model.** Decisions have `valid_from`/`valid_to` (business time) and `transaction_time` (system time). Active records have `valid_to IS NULL`. Always include this filter in queries that should return current state.

**Event-sourced ingestion.** `POST /v1/trace` creates a decision through events (`DecisionMade`, `DecisionRevised`). Events flow: HTTP handler -> idempotency check -> event buffer (WAL optional) -> COPY flush to Postgres -> embedding -> conflict scoring.

**Handler pattern.** All handlers are methods on the `Handlers` struct. Routes are registered in `server.go` using Go 1.22+ `METHOD /path` syntax with middleware wrappers (`adminOnly`, `writeRole`, `readRole`).

**RBAC enforcement.** The auth middleware extracts claims. Role-based middleware (`adminOnly`, `writeRole`, `readRole`) gates route access. Within handlers, `authz.FilterByAccess` post-filters query results for fine-grained grant checking.

## How to add a new API endpoint

1. Add the handler method to the appropriate `handlers_*.go` file:
```go
func (h *Handlers) HandleMyThing(w http.ResponseWriter, r *http.Request) {
    orgID := OrgIDFromContext(r.Context())        // always extract org
    agentID := AgentIDFromContext(r.Context())     // from auth middleware
    // ... business logic ...
    writeJSON(w, r, http.StatusOK, result)
}
```
2. Register the route in `server.go`:
```go
mux.Handle("GET /v1/my-thing", readRole(http.HandlerFunc(h.HandleMyThing)))
```
3. Add the storage query in the appropriate `internal/storage/*.go` file. Always filter by `org_id`.
4. Add the endpoint to `api/openapi.yaml`.
5. Run the full pre-commit checks.

## How to add a migration

1. Create `migrations/NNN_description.sql` where NNN is the next sequential number.
2. Start with a comment: `-- NNN: Brief description of what this migration does.`
3. Rehash: `atlas migrate hash --dir file://migrations`
4. Validate: `atlas migrate validate --dir file://migrations`
5. Stage both the `.sql` file and `migrations/atlas.sum`.

## Changing existing behavior

Before modifying any function's semantics (boundary conditions, error returns, nil behavior), **read the tests for that function first**. Tests often document intentional design choices via names and assertion messages (e.g. `"confidence == 0.05 is not > 0.05, so falls to edge tier"`). If a test contradicts your planned change, the test is probably right. Understand why before overriding it.

If you still believe the behavior should change, update the tests in the same commit.

## Boundaries

**Always:**
- Scope every storage query by `org_id`
- Include `valid_to IS NULL` when querying current decision state
- Run the 5 pre-commit checks + `go test -race` before pushing
- Use `require.NoError(t, err)` / `assert.*` from testify in tests
- Use `writeJSON(w, r, statusCode, payload)` for HTTP responses
- Use `slog` structured logging (never `fmt.Print` or `log.*`)

**Never:**
- Commit `.env` files, API keys, or credentials
- Add `Co-Authored-By` trailers to commits
- Skip pre-commit checks ("just this once" has caused two CI failures)
- Modify already-applied migrations (create a new one instead)
- Use `os.Exit` inside `run()` (it skips defers; return an error instead)
- Remove a failing test without understanding why it fails first

**Ask first:**
- Changing RBAC role requirements on an endpoint
- Adding new direct dependencies to go.mod
- Modifying the MCP server tool definitions
- Schema changes that widen access (e.g., removing org_id filters)

## Conventions

- Commit messages: imperative mood, concise first line, body explains "why"
- Branch names: `feature/*`, `fix/*` for PRs against `main`
- Binary output: `bin/` (gitignored)
- Specs that drive implementation live in the sibling `internal/` repo, not here
- Migration comments start with the migration number and a brief description
- Config env vars: `AKASHI_*` prefix (see `docs/configuration.md` for full reference)
- Test files use testcontainers for integration tests (`testutil.MustStartTimescaleDB()`)
