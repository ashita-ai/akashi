.PHONY: all build test lint fmt vet clean docker-up docker-down migrate ci security tidy

BINARY := bin/akashi
GO := go
GOFLAGS := -race

all: fmt lint vet test build

# Run the full CI pipeline locally (mirrors .github/workflows/ci.yml)
ci: tidy build lint vet security test
	@echo "CI passed"

build:
	$(GO) build -o $(BINARY) ./cmd/akashi

test:
	$(GO) test $(GOFLAGS) ./... -v

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
	rm -rf bin/
	$(GO) clean -testcache

docker-up:
	docker compose -f docker/docker-compose.yml up -d

docker-down:
	docker compose -f docker/docker-compose.yml down

docker-rebuild:
	docker compose -f docker/docker-compose.yml up -d --build

migrate:
	@echo "Run migrations against the database"
	@echo "TODO: integrate golang-migrate or atlas"
