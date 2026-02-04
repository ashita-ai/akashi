.PHONY: all build test lint fmt vet clean docker-up docker-down migrate

BINARY := bin/kyoyu
GO := go
GOFLAGS := -race

all: fmt lint vet test build

build:
	$(GO) build -o $(BINARY) ./cmd/kyoyu

test:
	$(GO) test $(GOFLAGS) ./... -v

lint:
	golangci-lint run ./...

fmt:
	goimports -w .
	gofmt -s -w .

vet:
	$(GO) vet ./...

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
