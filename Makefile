.PHONY: all build build-local build-ui build-with-ui test lint fmt vet clean docker-up docker-down ci security tidy \
       dev-ui migrate-apply migrate-lint migrate-hash migrate-diff migrate-status migrate-validate new-migration \
       check-doc-consistency verify-restore reconcile-qdrant reconcile-qdrant-repair \
       archive-events-dry-run archive-events verify-exit-criteria install-hooks coverage

BINARY := bin/akashi
GO := go
GOFLAGS := -race
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -ldflags "-X main.version=$(VERSION)"

all: fmt lint vet test build

# Run the full CI pipeline locally (mirrors .github/workflows/ci.yml)
ci: tidy check-doc-consistency build lint vet security test migrate-validate
	@echo "CI passed"

check-doc-consistency:
	python3 scripts/check_doc_config_consistency.py

build:
	$(GO) build $(LDFLAGS) -o $(BINARY) ./cmd/akashi

build-local: ## Build the zero-infra SQLite MCP server (ADR-009)
	$(GO) build -tags lite $(LDFLAGS) -o bin/akashi-local ./cmd/akashi-local

# Build the frontend (produces ui/dist/).
build-ui:
	cd ui && npm ci && npm run build

# Build the Go binary with the embedded UI.
build-with-ui: build-ui
	$(GO) build -tags ui $(LDFLAGS) -o $(BINARY) ./cmd/akashi

# Run the Vite dev server with API proxy to the Go server.
dev-ui:
	cd ui && npm run dev

test:
	$(GO) test $(GOFLAGS) ./... -v

coverage: ## Run tests with coverage and enforce 50% threshold
	$(GO) test $(GOFLAGS) -coverprofile=coverage.out ./...
	bash scripts/check_coverage.sh coverage.out 50

# NOTE: CI uses golangci-lint v2.8.0. Install locally with:
#   go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.8.0
lint:
	golangci-lint run ./...

fmt:
	goimports -w .
	gofmt -s -w .

vet:
	$(GO) vet ./...

security:
	govulncheck ./...

tidy:
	$(GO) mod tidy
	@git diff --quiet go.mod go.sum || (echo "go.mod/go.sum not tidy" && exit 1)

clean:
	rm -rf bin/ ui/dist/ ui/node_modules/
	$(GO) clean -testcache

docker-up:
	docker compose up -d

docker-down:
	docker compose down

docker-rebuild:
	docker compose up -d --build

# Atlas migration targets.
# Requires: atlas CLI (https://atlasgo.io/getting-started#installation)
# Environment variables:
#   DATABASE_URL   - target database (default: local PgBouncer)
#   ATLAS_DEV_URL  - disposable Postgres for diffing/linting (default: local direct)
ATLAS_DEV_URL ?= postgres://akashi:akashi@localhost:5432/akashi?sslmode=disable&search_path=public
ATLAS ?= atlas

migrate-apply: ## Apply pending migrations
	$(ATLAS) migrate apply --env local

migrate-lint: ## Lint migration files for safety issues
	$(ATLAS) migrate lint --env ci --latest 1

migrate-hash: ## Regenerate atlas.sum after editing migration files
	$(ATLAS) migrate hash --dir file://migrations

migrate-diff: ## Generate a new migration from schema changes (usage: make migrate-diff name=add_foo)
	@test -n "$(name)" || (echo "usage: make migrate-diff name=<migration_name>" && exit 1)
	$(ATLAS) migrate diff $(name) --env local

migrate-status: ## Show migration status
	$(ATLAS) migrate status --env local

migrate-validate: ## Validate migration file integrity (checksums + SQL)
	$(ATLAS) migrate validate --dir file://migrations

new-migration: ## Create a new migration file (usage: make new-migration name=add_foo_index)
	@test -n "$(name)" || (echo "usage: make new-migration name=<migration_name>" && exit 1)
	@LAST=$$(ls migrations/*.sql 2>/dev/null | sed 's|.*/||' | grep -o '^[0-9]*' | sort -n | tail -1); \
	LAST=$${LAST:-0}; \
	NEXT=$$(printf '%03d' $$((10#$$LAST + 1))); \
	DESC=$$(echo "$(name)" | tr '_' ' '); \
	FILE="migrations/$${NEXT}_$(name).sql"; \
	echo "-- $${NEXT}: $${DESC}" > "$$FILE"; \
	echo "Created $$FILE"; \
	$(ATLAS) migrate hash --dir file://migrations

verify-restore: ## Run post-restore DB verification checks (requires DATABASE_URL)
	bash scripts/verify_restore.sh

reconcile-qdrant: ## Detect Postgres vs Qdrant drift (requires DATABASE_URL, QDRANT_URL)
	python3 scripts/reconcile_qdrant.py

reconcile-qdrant-repair: ## Enqueue missing Postgres decisions into outbox for Qdrant repair
	python3 scripts/reconcile_qdrant.py --repair

archive-events-dry-run: ## Archive candidate agent_events window without purging
	DRY_RUN=true ENABLE_PURGE=false bash scripts/archive_agent_events.sh

archive-events: ## Archive and purge one retention window (requires explicit flags)
	DRY_RUN=false ENABLE_PURGE=true bash scripts/archive_agent_events.sh

verify-exit-criteria: ## Evaluate durability exit criteria (JSON output; non-zero on failure)
	python3 scripts/verify_exit_criteria.py

install-hooks: ## Install IDE hooks for Claude Code and Cursor
	@bash scripts/install-ide-hooks.sh
