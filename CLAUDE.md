# ashita-ai/akashi

Decision trace layer for multi-agent AI systems. Go 1.25, PostgreSQL 17 + pgvector + TimescaleDB.

## Before every commit

Run these checks. CI will reject the PR if any fail.

```sh
go mod tidy && git diff --exit-code go.mod go.sum
go build ./...
go vet ./...
golangci-lint run ./...
atlas migrate validate --dir file://migrations
```

If `go mod tidy` produces a diff, stage `go.mod` and `go.sum` in the same commit.
If `atlas migrate validate` fails, run `atlas migrate hash --dir file://migrations` and stage `migrations/atlas.sum`.
If `golangci-lint` is not on `$PATH`, it's at `~/go/bin/golangci-lint` (install: `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.8.0`).

Never commit without running these. `make ci` runs the full pipeline locally (tidy, build, lint, vet, security, test) and is the gold standard, but the five commands above are the minimum.

## Migrations

- Migration files live in `migrations/` as sequential SQL files (001, 002, ..., 017).
- Atlas manages checksums in `migrations/atlas.sum`. Any time a migration file is added or modified, rehash: `atlas migrate hash --dir file://migrations`
- Always validate before committing: `atlas migrate validate --dir file://migrations`

## Build

- `go build ./...` — build without UI
- `go build -tags ui ./...` — build with embedded React SPA (requires `cd ui && npm ci && npm run build` first)
- `make ci` — full local CI mirror

## Project structure

- `cmd/akashi/` — entrypoint
- `internal/server/` — HTTP handlers, middleware, MCP server
- `internal/storage/` — PostgreSQL storage layer
- `internal/service/` — business logic (decisions, billing)
- `internal/model/` — domain types
- `migrations/` — SQL migration files (Atlas-managed checksums)
- `adrs/` — technical architecture decision records
- `sdk/` — Go, Python, TypeScript client SDKs
- `ui/` — React 19 audit dashboard (embedded via go:embed with `ui` build tag)

## Conventions

- Do not add `Co-Authored-By` trailers to commits.
- Binary output goes to `bin/`. Root-level binaries are gitignored.
- `.env` files are gitignored. Never commit credentials.
- Specs that drive implementation live in the sibling `internal/` repo, not here.
