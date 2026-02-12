# Configuration Reference

All configuration is via environment variables. See [`.env.example`](../.env.example) for a minimal starting point.

## Required

| Variable | Default | Description |
|----------|---------|-------------|
| `DATABASE_URL` | `postgres://...@localhost:6432/akashi` | Connection string for queries (typically through PgBouncer) |

## Server

| Variable | Default | Description |
|----------|---------|-------------|
| `AKASHI_PORT` | `8080` | HTTP listen port |
| `AKASHI_READ_TIMEOUT` | `30s` | HTTP read timeout |
| `AKASHI_WRITE_TIMEOUT` | `30s` | HTTP write timeout |
| `AKASHI_MAX_REQUEST_BODY_BYTES` | `1048576` | Max request body size (1 MB) |
| `AKASHI_LOG_LEVEL` | `info` | Log level: `debug`, `info`, `warn`, `error` |

## Database

| Variable | Default | Description |
|----------|---------|-------------|
| `DATABASE_URL` | `postgres://...@localhost:6432/akashi` | PgBouncer or direct Postgres URL for all queries and writes |
| `NOTIFY_URL` | `postgres://...@localhost:5432/akashi` | Direct Postgres URL for LISTEN/NOTIFY (SSE). Empty disables real-time push |

See [ADR-007](../adrs/ADR-007-dual-postgres-connections.md) for why two connections are needed.

## Authentication

| Variable | Default | Description |
|----------|---------|-------------|
| `AKASHI_ADMIN_API_KEY` | _(empty)_ | Bootstrap admin API key. On first startup with an empty `agents` table, creates an admin agent with this key |
| `AKASHI_JWT_PRIVATE_KEY` | _(empty)_ | Path to Ed25519 private key PEM file. Empty = auto-generate ephemeral keys (dev only) |
| `AKASHI_JWT_PUBLIC_KEY` | _(empty)_ | Path to Ed25519 public key PEM file |
| `AKASHI_JWT_EXPIRATION` | `24h` | JWT token lifetime |

Key files must have `0600` permissions. See [ADR-005](../adrs/ADR-005-auth-rbac.md) for the auth architecture.

## Embeddings

| Variable | Default | Description |
|----------|---------|-------------|
| `AKASHI_EMBEDDING_PROVIDER` | `auto` | Provider selection: `auto`, `ollama`, `openai`, `noop` |
| `AKASHI_EMBEDDING_DIMENSIONS` | `1024` | Vector dimensionality (must match the chosen model) |
| `OLLAMA_URL` | `http://localhost:11434` | Ollama server address |
| `OLLAMA_MODEL` | `mxbai-embed-large` | Ollama embedding model |
| `OPENAI_API_KEY` | _(empty)_ | OpenAI API key. Required when provider is `openai` |
| `AKASHI_EMBEDDING_MODEL` | `text-embedding-3-small` | OpenAI embedding model |

In `auto` mode: Ollama is tried first (health check with 2s timeout), then OpenAI if `OPENAI_API_KEY` is set, then noop (zero vectors, semantic search disabled). See [ADR-006](../adrs/ADR-006-embedding-provider-chain.md).

## Vector Search (Qdrant)

| Variable | Default | Description |
|----------|---------|-------------|
| `QDRANT_URL` | _(empty)_ | Qdrant gRPC URL (e.g. `https://xyz.cloud.qdrant.io:6334`). Empty = text search fallback |
| `QDRANT_API_KEY` | _(empty)_ | Qdrant API key |
| `QDRANT_COLLECTION` | `akashi_decisions` | Qdrant collection name |
| `AKASHI_OUTBOX_POLL_INTERVAL` | `1s` | How often the outbox worker checks for pending syncs |
| `AKASHI_OUTBOX_BATCH_SIZE` | `100` | Max decisions synced to Qdrant per poll cycle |

Qdrant is optional. When not configured, search falls back to ILIKE text matching. See [ADR-002](../adrs/ADR-002-unified-postgres-storage.md).

## Observability (OpenTelemetry)

| Variable | Default | Description |
|----------|---------|-------------|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | _(empty)_ | OTLP gRPC endpoint. Empty = OTEL disabled |
| `OTEL_EXPORTER_OTLP_INSECURE` | `false` | Use HTTP instead of HTTPS for OTLP |
| `OTEL_SERVICE_NAME` | `akashi` | Service name in OTEL spans and metrics |

## Tuning

| Variable | Default | Description |
|----------|---------|-------------|
| `AKASHI_EVENT_BUFFER_SIZE` | `1000` | In-memory event buffer capacity before COPY flush |
| `AKASHI_EVENT_FLUSH_TIMEOUT` | `100ms` | Max time between buffer flushes |
| `AKASHI_CONFLICT_REFRESH_INTERVAL` | `30s` | How often the conflict materialized view is refreshed |
| `AKASHI_INTEGRITY_PROOF_INTERVAL` | `5m` | How often Merkle tree proofs are built for new decisions |
