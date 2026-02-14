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
| `AKASHI_CORS_ALLOWED_ORIGINS` | _(empty)_ | Comma-separated allowed CORS origins. Empty = deny cross-origin browser requests unless same-origin |

## Database

| Variable | Default | Description |
|----------|---------|-------------|
| `DATABASE_URL` | `postgres://...@localhost:6432/akashi` | PgBouncer or direct Postgres URL for all queries and writes |
| `NOTIFY_URL` | `postgres://...@localhost:5432/akashi` | Direct Postgres URL for LISTEN/NOTIFY (SSE). Set `NOTIFY_URL=` to disable real-time push |
| `AKASHI_SKIP_EMBEDDED_MIGRATIONS` | `false` | Skip startup embedded migrations (use when an external system like Atlas owns migration execution) |

See [ADR-007](../adrs/ADR-007-dual-postgres-connections.md) for why two connections are needed.

## Authentication

| Variable | Default | Description |
|----------|---------|-------------|
| `AKASHI_ADMIN_API_KEY` | _(empty)_ | Bootstrap admin API key. If no agents exist and this is empty, startup fails to prevent admin lockout |
| `AKASHI_JWT_PRIVATE_KEY` | _(empty)_ | Path to Ed25519 private key PEM file. Empty = auto-generate ephemeral keys (dev only) |
| `AKASHI_JWT_PUBLIC_KEY` | _(empty)_ | Path to Ed25519 public key PEM file |
| `AKASHI_JWT_EXPIRATION` | `24h` | JWT token lifetime |

Key files must have `0600` permissions. See [ADR-005](../adrs/ADR-005-auth-rbac.md) for the auth architecture.

For `Authorization: ApiKey <agent_id>:<api_key>`, send `X-Akashi-Org-ID` when the same `agent_id` exists in multiple organizations. Ambiguous API key auth requests are rejected.

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
| `QDRANT_URL` | _(empty)_ | Qdrant URL. `:6334` (gRPC) is preferred; `:6333` (REST) is accepted and auto-mapped to `:6334`. Empty = text search fallback |
| `QDRANT_API_KEY` | _(empty)_ | Qdrant API key |
| `QDRANT_COLLECTION` | `akashi_decisions` | Qdrant collection name |
| `AKASHI_OUTBOX_POLL_INTERVAL` | `1s` | How often the outbox worker checks for pending syncs |
| `AKASHI_OUTBOX_BATCH_SIZE` | `100` | Max decisions synced to Qdrant per poll cycle |

Qdrant is optional. When not configured, search falls back to PostgreSQL full-text search (tsvector/tsquery) with ILIKE as secondary fallback. See [ADR-002](../adrs/ADR-002-unified-postgres-storage.md).

## Rate Limiting

| Variable | Default | Description |
|----------|---------|-------------|
| `AKASHI_RATE_LIMIT_ENABLED` | `true` | Enable rate limiting middleware |
| `AKASHI_RATE_LIMIT_RPS` | `100` | Sustained requests per second per key |
| `AKASHI_RATE_LIMIT_BURST` | `200` | Token bucket capacity (max burst size) per key |
| `AKASHI_TRUST_PROXY` | `false` | When true, use X-Forwarded-For for IP-based rate limits (e.g. behind load balancer) |

Keys are constructed as `org:<uuid>:agent:<id>` for authenticated requests. For unauthenticated paths (e.g. `/auth/token`), the key is `ip:<client_ip>`. Enable `AKASHI_TRUST_PROXY` only when behind a trusted reverse proxy; otherwise X-Forwarded-For can be spoofed.

The OSS distribution uses an in-memory token bucket. Enterprise deployments can substitute a Redis-backed implementation via the `ratelimit.Limiter` interface.

## Observability (OpenTelemetry)

| Variable | Default | Description |
|----------|---------|-------------|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | _(empty)_ | OTLP HTTP endpoint. Empty = OTEL disabled |
| `OTEL_EXPORTER_OTLP_INSECURE` | `false` | Use HTTP instead of HTTPS for OTLP |
| `OTEL_SERVICE_NAME` | `akashi` | Service name in OTEL spans and metrics |

## Conflict Detection

| Variable | Default | Description |
|----------|---------|-------------|
| `AKASHI_CONFLICT_SIGNIFICANCE_THRESHOLD` | `0.30` | Min significance (topic_sim × outcome_div) to store a conflict |
| `AKASHI_CONFLICT_REFRESH_INTERVAL` | `30s` | Interval for broker to poll new conflicts (SSE push). Conflicts are populated event-driven on trace. |

## Tuning

| Variable | Default | Description |
|----------|---------|-------------|
| `AKASHI_EVENT_BUFFER_SIZE` | `1000` | In-memory event buffer capacity before COPY flush |
| `AKASHI_EVENT_FLUSH_TIMEOUT` | `100ms` | Max time between buffer flushes |
| `AKASHI_INTEGRITY_PROOF_INTERVAL` | `5m` | How often Merkle tree proofs are built for new decisions |
| `AKASHI_ENABLE_DESTRUCTIVE_DELETE` | `false` | Enables irreversible `DELETE /v1/agents/{agent_id}`. Keep `false` in production unless explicitly needed for GDPR workflows |
| `AKASHI_SHUTDOWN_HTTP_TIMEOUT` | `10s` | HTTP shutdown grace timeout (`0` = wait indefinitely) |
| `AKASHI_SHUTDOWN_BUFFER_DRAIN_TIMEOUT` | `0` | Reserved. Akashi always waits indefinitely for event buffer drain to avoid data loss. |
| `AKASHI_SHUTDOWN_OUTBOX_DRAIN_TIMEOUT` | `0` | Outbox drain timeout (`0` = wait indefinitely) |

## Write Idempotency

For retry-safe write APIs (`POST /v1/trace`, `POST /v1/runs`, `POST /v1/runs/{run_id}/events`), clients can send:

- `Idempotency-Key: <unique-key>`

Behavior:

- Same key + same payload => server replays the original success response (no duplicate write).
- Same key + different payload => `409 CONFLICT`.
- Same key while the first request is still processing => `409 CONFLICT` (retry later).

Scope and matching rules:

- Keys are scoped by `(org_id, agent_id, endpoint, idempotency_key)`.
- For run events, `endpoint` includes the concrete run ID (for example `POST:/v1/runs/<run_id>/events`).
- Payload matching uses a server-side SHA-256 hash of the canonical JSON payload:
  - `POST /v1/trace`: request body plus header-derived context that changes write semantics.
  - `POST /v1/runs`: request body.
  - `POST /v1/runs/{run_id}/events`: request body only.
- Replayed responses preserve the original HTTP status code and response body.

Client guidance:

- Use a UUIDv4 (or similarly random) key per logical write attempt.
- Retry transient network failures with the same key and same payload.
- On `409` with "already in progress", back off and retry.
- Never reuse a key for a different payload.

Operational idempotency settings:

| Variable | Default | Description |
|----------|---------|-------------|
| `AKASHI_IDEMPOTENCY_IN_PROGRESS_TTL` | `5m` | In-progress key becomes reclaimable after this duration |
| `AKASHI_IDEMPOTENCY_CLEANUP_INTERVAL` | `1h` | Background cleanup cadence for idempotency records |
| `AKASHI_IDEMPOTENCY_COMPLETED_TTL` | `168h` (7d) | Retention for completed idempotency records |
| `AKASHI_IDEMPOTENCY_ABANDONED_TTL` | `24h` | Retention for abandoned in-progress idempotency records |
