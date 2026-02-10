# Akashi Operator's Runbook

Production operations guide for the Akashi decision trace server.

---

## 1. Health Checks

### Endpoint

```
GET /health
```

No authentication required. No rate limiting.

### Response

```json
{
  "status": "healthy",
  "version": "1.0.0",
  "postgres": "connected",
  "qdrant": "connected",
  "uptime_seconds": 86400
}
```

| Field      | Healthy Value   | Unhealthy Value   |
|------------|-----------------|-------------------|
| `status`   | `"healthy"`     | `"unhealthy"`     |
| `postgres` | `"connected"`   | `"disconnected"`  |
| `qdrant`   | `"connected"`   | `"disconnected"`  |

HTTP status is **200** when healthy, **503** when unhealthy. The endpoint returns 503 if and only if PostgreSQL is unreachable. Qdrant being down does NOT cause a 503 -- the system degrades to text search.

The `qdrant` field is omitted entirely when Qdrant is not configured (no `QDRANT_URL`).

### Kubernetes / Load Balancer Configuration

```yaml
# Kubernetes liveness probe
livenessProbe:
  httpGet:
    path: /health
    port: 8080
  initialDelaySeconds: 10
  periodSeconds: 15
  failureThreshold: 3

# Kubernetes readiness probe
readinessProbe:
  httpGet:
    path: /health
    port: 8080
  initialDelaySeconds: 5
  periodSeconds: 10
  failureThreshold: 2
```

For AWS ALB/NLB target groups, use `/health` with expected status 200.

---

## 2. Monitoring

### OTEL Metrics

Metrics are exported via OTLP/HTTP to the endpoint specified by `OTEL_EXPORTER_OTLP_ENDPOINT`. The metric reader flushes every 15 seconds. Traces are batched every 5 seconds. If the endpoint is not set, OTEL is disabled (no-op providers).

| Metric                         | Type      | Unit | Labels                                                    |
|--------------------------------|-----------|------|-----------------------------------------------------------|
| `http.server.request_count`    | Counter   | 1    | `http.method`, `http.route`, `http.status_code`, `akashi.agent_id` |
| `http.server.duration`         | Histogram | ms   | `http.method`, `http.route`, `http.status_code`, `akashi.agent_id` |

Trace spans include `http.method`, `http.url`, `http.request_id`, `http.status_code`, `akashi.agent_id`, and `akashi.role`.

### What to Alert On

| Condition                             | Query / Check                                                              | Suggested Threshold         | Severity |
|---------------------------------------|----------------------------------------------------------------------------|-----------------------------|----------|
| Request latency p99                   | `histogram_quantile(0.99, http.server.duration)`                           | > 2000 ms for 5 min        | Warning  |
| Request latency p99                   | `histogram_quantile(0.99, http.server.duration)`                           | > 5000 ms for 5 min        | Critical |
| 5xx error rate                        | `rate(http.server.request_count{http.status_code=~"5.."})`                 | > 1% of total for 5 min    | Warning  |
| 5xx error rate                        | `rate(http.server.request_count{http.status_code=~"5.."})`                 | > 5% of total for 2 min    | Critical |
| Health endpoint down                  | `GET /health` returns non-200                                              | 3 consecutive failures      | Critical |
| Outbox lag (stuck entries)            | `SELECT count(*) FROM search_outbox WHERE attempts > 0`                    | > 100 entries for 10 min   | Warning  |
| Outbox dead letters                   | `SELECT count(*) FROM search_outbox WHERE attempts >= 10`                  | > 0                         | Critical |
| Event buffer dropped events           | Log line `"trace: dropping events"` (structured field `dropped`)           | Any occurrence              | Critical |
| PostgreSQL pool exhaustion            | pgxpool metrics or connection wait time                                    | > 80% utilization           | Warning  |
| Qdrant health                         | `/health` response `qdrant: "disconnected"`                                | Sustained > 5 min           | Warning  |

### Log-Based Alerts

Akashi logs JSON to stdout. Key log messages to monitor:

| Log Message (substring)                        | Meaning                                              |
|-------------------------------------------------|------------------------------------------------------|
| `"trace: flush failed"`                         | Event buffer failed to write to PostgreSQL            |
| `"trace: dropping events"`                      | Events lost due to buffer overflow after flush fail   |
| `"search outbox: dead-letter entry"`            | Outbox entry exceeded 10 retry attempts               |
| `"search outbox: qdrant upsert"` + `error`      | Qdrant write failure (entries will retry)             |
| `"storage: notify reconnect attempt failed"`    | LISTEN/NOTIFY connection dropped, attempting recovery |
| `"ratelimit: redis error, allowing request"`    | Redis unreachable, rate limiting failed open          |
| `"conflict refresh failed"`                     | Materialized view refresh failed                      |

---

## 3. Common Failure Modes and Remediation

### Qdrant is Down

**Symptoms**: `/health` shows `qdrant: "disconnected"`. `POST /v1/search` returns degraded results (text fallback). Outbox entries accumulate.

**Impact**: Semantic (vector) search unavailable. Text-based search still works. No data loss -- new decisions continue to be written to PostgreSQL and queued in the `search_outbox` table.

**Remediation**:
1. Restore Qdrant.
2. Outbox worker will automatically sync accumulated entries on next poll cycle.
3. Verify: `SELECT count(*) FROM search_outbox WHERE attempts < 10;` should trend to 0.

**No operator intervention required** once Qdrant is restored.

---

### Redis is Down

**Symptoms**: Log line `"ratelimit: redis error, allowing request"`. Rate limit headers still present but limits are not enforced.

**Impact**: Rate limiting fails open -- all requests are allowed. The server continues to function normally otherwise.

**Remediation**:
1. Restore Redis.
2. Rate limiting resumes automatically on next request.

**No restart required.**

---

### PgBouncer is Down

**Symptoms**: `/health` returns 503. All API requests fail with 500.

**Impact**: Complete service outage. No queries or writes succeed.

**Remediation**:
1. Restore PgBouncer.
2. If PgBouncer cannot be restored quickly, update `DATABASE_URL` to point directly to PostgreSQL and restart Akashi. Be aware that direct connections bypass pooling -- monitor connection count.

---

### NOTIFY Connection Drops

**Symptoms**: SSE subscriptions (`GET /v1/subscribe`) stop receiving updates. Log lines: `"storage: notify reconnect attempt failed"` followed by `"storage: notify connection restored"` on success.

**Impact**: Real-time event streaming paused. All other functionality (API, ingestion, search) is unaffected.

**Automatic recovery**: The connection reconnects with exponential backoff (500ms base, doubling, up to 5 attempts with jitter). All previously subscribed channels (`akashi_decisions`, `akashi_conflicts`) are re-established on reconnect.

**Remediation** (if auto-reconnect fails after 5 attempts):
1. Check that the `NOTIFY_URL` PostgreSQL instance is reachable.
2. Restart the Akashi process to re-establish the connection.

---

### Event Buffer Full

**Symptoms**: `POST /v1/trace` and `POST /v1/runs/{run_id}/events` return errors. Log line: `"trace: buffer at capacity"`.

**Impact**: New event ingestion is rejected (backpressure). Decisions and queries are unaffected.

**Hard cap**: 100,000 events in memory regardless of `AKASHI_EVENT_BUFFER_SIZE`.

**Remediation**:
1. Check if PostgreSQL is accepting writes -- the buffer cannot flush if the database is down.
2. Check for log line `"trace: flush failed"` to identify the underlying cause.
3. If the load is legitimate, increase `AKASHI_EVENT_BUFFER_SIZE` (up to 100,000) and restart.
4. If flush failures persist, events will be dropped (log: `"trace: dropping events"`). This indicates data loss.

---

### Outbox Dead Letters (attempts >= 10)

**Symptoms**: `SELECT count(*) FROM search_outbox WHERE attempts >= 10;` returns non-zero. Log line: `"search outbox: dead-letter entry"`.

**Impact**: Those specific decisions are not indexed in Qdrant. They exist in PostgreSQL and are queryable via SQL, but not via semantic search.

**Common causes**:
- Embedding dimension mismatch between Akashi config and Qdrant collection
- Qdrant collection deleted or renamed
- Persistent Qdrant connectivity issues

**Remediation**:
1. Inspect the error:
   ```sql
   SELECT id, decision_id, operation, attempts, last_error, created_at
   FROM search_outbox
   WHERE attempts >= 10
   ORDER BY created_at DESC
   LIMIT 20;
   ```
2. Fix the underlying issue (restore collection, fix dimensions, etc.).
3. Reset attempts to allow retry:
   ```sql
   UPDATE search_outbox
   SET attempts = 0, locked_until = NULL
   WHERE attempts >= 10;
   ```
4. The outbox worker will pick them up on the next poll cycle.

**Automatic cleanup**: Dead-letter entries older than 7 days are automatically deleted by the outbox worker (checked hourly).

---

## 4. JWT Key Rotation

Akashi uses Ed25519 (EdDSA) for JWT signing. Keys are loaded from PEM files at startup.

### Generate a New Key Pair

```sh
openssl genpkey -algorithm Ed25519 -out akashi-private.pem
openssl pkey -in akashi-private.pem -pubout -out akashi-public.pem
chmod 600 akashi-private.pem akashi-public.pem
```

Key files **must** have permissions `0600` or stricter. The server refuses to start if they are world-readable.

### Rotate Keys

1. Generate new key pair (see above).
2. Place the files where the server can read them.
3. Update environment variables:
   ```sh
   AKASHI_JWT_PRIVATE_KEY=/path/to/new/akashi-private.pem
   AKASHI_JWT_PUBLIC_KEY=/path/to/new/akashi-public.pem
   ```
4. Restart the Akashi process.

### Token Expiry Behavior

- Default token lifetime: 24 hours (`AKASHI_JWT_EXPIRATION`).
- After rotation, existing tokens signed with the old key will **fail validation immediately** because the server only holds one public key in memory.
- There is no token revocation list. To force all sessions to re-authenticate, rotate keys and restart.
- If you need zero-downtime rotation, coordinate with clients to re-authenticate within the restart window.

### Development Mode

If `AKASHI_JWT_PRIVATE_KEY` and `AKASHI_JWT_PUBLIC_KEY` are both unset, the server generates an ephemeral key pair in memory. Tokens are invalidated on every restart. **Never use this in production** -- a warning is logged at startup.

---

## 5. Database Operations

### Migrations

Migrations are managed by [Atlas](https://atlasgo.io/). Files live in `migrations/` as sequential numbered SQL files.

```sh
# Apply pending migrations
atlas migrate apply --dir file://migrations --url "$DATABASE_URL"

# Validate migration integrity (checksums)
atlas migrate validate --dir file://migrations

# Rehash after modifying migration files
atlas migrate hash --dir file://migrations
```

**On startup** (development only): the server attempts to apply migrations from the embedded `migrations/` directory. In production, run migrations explicitly before deploying a new version.

### Backup

Standard `pg_dump` works. Key tables by priority:

| Table             | Notes                                                       |
|-------------------|-------------------------------------------------------------|
| `organizations`   | Tenant configuration. Small. Always back up.                |
| `agents`          | Auth identities. Small. Always back up.                     |
| `decisions`       | Core decision data with embeddings. Can be large.           |
| `runs`            | Trace run metadata. Moderate size.                          |
| `agent_events`    | Append-only event log. Potentially very large. Consider partial or time-bounded backup. |
| `search_outbox`   | Transient sync queue. Only back up if investigating issues. |
| `access_grants`   | RBAC grants. Small. Always back up.                         |

```sh
# Full backup
pg_dump "$DATABASE_URL" -Fc -f akashi-backup-$(date +%Y%m%d).dump

# Data-only backup of core tables (skip events)
pg_dump "$DATABASE_URL" -Fc --table=organizations --table=agents \
  --table=decisions --table=runs --table=access_grants \
  -f akashi-core-$(date +%Y%m%d).dump
```

### Outbox Health Check

```sql
-- Overall sync status
SELECT count(*) AS pending,
       count(*) FILTER (WHERE attempts > 0) AS retrying,
       count(*) FILTER (WHERE attempts >= 10) AS dead_letter,
       max(attempts) AS max_attempts,
       min(created_at) AS oldest_entry
FROM search_outbox;

-- Recent errors
SELECT decision_id, operation, attempts, last_error, created_at
FROM search_outbox
WHERE last_error IS NOT NULL
ORDER BY created_at DESC
LIMIT 10;
```

### Connection Pool Monitoring

```sql
-- Active connections (run against PostgreSQL directly, not PgBouncer)
SELECT count(*) AS total,
       count(*) FILTER (WHERE state = 'active') AS active,
       count(*) FILTER (WHERE state = 'idle') AS idle
FROM pg_stat_activity
WHERE datname = 'akashi';
```

---

## 6. Scaling Guidelines

### Single Instance Capacity

A single Akashi binary handles approximately 1,000 req/s on modest hardware (4 vCPU, 8 GB RAM). The event buffer and COPY-based batch writes amortize database round trips.

### Horizontal Scaling

Run multiple Akashi instances behind a load balancer.

| Component               | Scaling Behavior                                                                  |
|-------------------------|-----------------------------------------------------------------------------------|
| HTTP API                | Stateless. Any instance can serve any request.                                    |
| Event buffer            | Per-instance. Each instance flushes its own buffer to PostgreSQL.                 |
| Outbox worker           | Per-instance. Uses `FOR UPDATE SKIP LOCKED` -- multiple workers safely share work.|
| LISTEN/NOTIFY           | Per-instance. Each instance maintains its own direct PostgreSQL connection.        |
| SSE broker              | Per-instance. Clients receive events only from the instance they are connected to.|
| Rate limiting           | Shared via Redis. Limits are enforced consistently across all instances.           |
| JWT validation          | Stateless. All instances must have the same public key.                            |

### Bottlenecks

1. **PostgreSQL** is the primary bottleneck. Scale read replicas for query load. Consider connection pooling (PgBouncer) tuning.
2. **Qdrant** for vector search at scale. Monitor query latency via `http.server.duration` on `/v1/search`.
3. **Redis** is lightweight (rate limiting only). A single Redis instance handles millions of operations per second.

### SSE Limitation

SSE subscriptions are bound to the instance the client connects to. With multiple instances behind a load balancer, a client only receives events produced by its connected instance. For full coverage, clients should use polling (`GET /v1/decisions/recent`) or ensure sticky sessions.

---

## 7. Configuration Reference

All configuration is via environment variables. No config files.

### Server

| Variable                        | Default                  | Description                              |
|---------------------------------|--------------------------|------------------------------------------|
| `AKASHI_PORT`                   | `8080`                   | HTTP listen port                         |
| `AKASHI_READ_TIMEOUT`           | `30s`                    | HTTP read timeout (Go duration)          |
| `AKASHI_WRITE_TIMEOUT`          | `30s`                    | HTTP write timeout (Go duration)         |
| `AKASHI_LOG_LEVEL`              | `info`                   | Log level (`debug` or `info`)            |
| `AKASHI_MAX_REQUEST_BODY_BYTES` | `1048576` (1 MB)         | Max request body size in bytes           |

### Database

| Variable        | Default (development)                                          | Description                                   |
|-----------------|----------------------------------------------------------------|-----------------------------------------------|
| `DATABASE_URL`  | `postgres://akashi:akashi@localhost:6432/akashi?sslmode=verify-full` | PgBouncer / pooled connection URL (queries)   |
| `NOTIFY_URL`    | `postgres://akashi:akashi@localhost:5432/akashi?sslmode=verify-full` | Direct PostgreSQL URL (LISTEN/NOTIFY, SSE)    |

### Redis

| Variable    | Default | Description                                   |
|-------------|---------|-----------------------------------------------|
| `REDIS_URL` | (empty) | Redis URL for rate limiting. Empty = disabled. |

### Authentication

| Variable                 | Default   | Description                                        |
|--------------------------|-----------|----------------------------------------------------|
| `AKASHI_JWT_PRIVATE_KEY` | (empty)   | Path to Ed25519 private key PEM. Empty = ephemeral.|
| `AKASHI_JWT_PUBLIC_KEY`  | (empty)   | Path to Ed25519 public key PEM. Empty = ephemeral. |
| `AKASHI_JWT_EXPIRATION`  | `24h`     | JWT token lifetime (Go duration)                   |
| `AKASHI_ADMIN_API_KEY`   | (empty)   | Bootstrap API key for initial admin agent           |

### Embeddings

| Variable                       | Default                    | Description                                    |
|--------------------------------|----------------------------|------------------------------------------------|
| `AKASHI_EMBEDDING_PROVIDER`    | `auto`                     | `auto`, `openai`, `ollama`, or `noop`          |
| `OPENAI_API_KEY`               | (empty)                    | OpenAI API key (required if provider=openai)   |
| `AKASHI_EMBEDDING_MODEL`       | `text-embedding-3-small`   | OpenAI model name                              |
| `AKASHI_EMBEDDING_DIMENSIONS`  | `1024`                     | Vector dimensions. Must match model output.    |
| `OLLAMA_URL`                   | `http://localhost:11434`   | Ollama server URL                              |
| `OLLAMA_MODEL`                 | `mxbai-embed-large`        | Ollama embedding model                         |

Provider auto-detection order: Ollama (if reachable) > OpenAI (if key set) > noop.

### Qdrant (Vector Search)

| Variable                       | Default              | Description                                |
|--------------------------------|----------------------|--------------------------------------------|
| `QDRANT_URL`                   | (empty)              | Qdrant gRPC URL. Empty = vector search disabled. |
| `QDRANT_API_KEY`               | (empty)              | Qdrant API key                             |
| `QDRANT_COLLECTION`            | `akashi_decisions`   | Qdrant collection name                     |
| `AKASHI_OUTBOX_POLL_INTERVAL`  | `1s`                 | Outbox worker poll frequency (Go duration) |
| `AKASHI_OUTBOX_BATCH_SIZE`     | `100`                | Max outbox entries per poll cycle           |

### OpenTelemetry

| Variable                          | Default   | Description                               |
|-----------------------------------|-----------|-------------------------------------------|
| `OTEL_EXPORTER_OTLP_ENDPOINT`    | (empty)   | OTLP HTTP endpoint. Empty = OTEL disabled.|
| `OTEL_EXPORTER_OTLP_INSECURE`    | `false`   | Use HTTP instead of HTTPS for OTLP        |
| `OTEL_SERVICE_NAME`              | `akashi`  | Service name in traces/metrics            |

### Stripe Billing

| Variable                | Default | Description                                   |
|-------------------------|---------|-----------------------------------------------|
| `STRIPE_SECRET_KEY`     | (empty) | Stripe secret key. Empty = billing disabled.  |
| `STRIPE_WEBHOOK_SECRET` | (empty) | Stripe webhook signing secret (required if billing enabled) |
| `STRIPE_PRO_PRICE_ID`   | (empty) | Stripe Price ID for the Pro plan (required if billing enabled) |

### SMTP (Email Verification)

| Variable              | Default              | Description                        |
|-----------------------|----------------------|------------------------------------|
| `AKASHI_SMTP_HOST`   | (empty)              | SMTP host. Empty = dev mode (logs URL instead). |
| `AKASHI_SMTP_PORT`   | `587`                | SMTP port (STARTTLS required)      |
| `AKASHI_SMTP_USER`   | (empty)              | SMTP username                      |
| `AKASHI_SMTP_PASSWORD`| (empty)             | SMTP password                      |
| `AKASHI_SMTP_FROM`   | `noreply@akashi.dev` | Sender address for verification emails |
| `AKASHI_BASE_URL`    | `http://localhost:8080` | Public base URL for verification links |

### Operational

| Variable                            | Default  | Description                                         |
|-------------------------------------|----------|-----------------------------------------------------|
| `AKASHI_EVENT_BUFFER_SIZE`          | `1000`   | Flush threshold (events). Hard cap: 100,000.        |
| `AKASHI_EVENT_FLUSH_TIMEOUT`        | `100ms`  | Max time between flushes (Go duration)              |
| `AKASHI_CONFLICT_REFRESH_INTERVAL`  | `30s`    | How often to refresh the conflicts materialized view |

---

## 8. Graceful Shutdown

On `SIGTERM` or `SIGINT`, the server shuts down in this order:

```
1. Outbox worker stops         -- cancels poll loop, drains remaining batch (10s timeout)
2. Event buffer drains          -- final flush to PostgreSQL (10s timeout)
3. HTTP server drains           -- completes in-flight requests (15s total shutdown budget)
4. Database pools close         -- PgBouncer pool + NOTIFY connection
5. OTEL flushes                 -- final trace/metric export
```

The overall shutdown context has a **15-second** timeout. Each subsystem has its own internal timeout within that budget.

### Warnings

- **DO NOT** send `kill -9` during shutdown. If the event buffer is mid-flush, events in memory will be lost.
- If the buffer drain times out (log: `"trace: drain timed out waiting for flush loop"`), events in the buffer at that moment are lost.
- The outbox worker drain timeout (log: `"search outbox: drain timed out"`) means some outbox entries were not synced to Qdrant. They remain in PostgreSQL and will sync on next startup.

### Pre-Deployment Checklist

1. Ensure load balancer has stopped sending traffic (remove from target group or mark unhealthy).
2. Send `SIGTERM`.
3. Wait for exit (up to 15 seconds).
4. Verify clean shutdown: log line `"akashi stopped"` with exit code 0.

---

## 9. Rate Limit Rules

These are the rate limit rules enforced when Redis is configured:

| Prefix   | Limit           | Window   | Key           | Applies To                                     |
|----------|-----------------|----------|---------------|-------------------------------------------------|
| `auth`   | 20 req/min      | 1 min    | Client IP     | `POST /auth/token`, `/auth/refresh`, `/auth/signup`, `/auth/verify` |
| `ingest` | 300 req/min     | 1 min    | Agent ID      | `POST /v1/trace`, `/v1/runs`, `/v1/runs/*/events`, `/v1/runs/*/complete` |
| `query`  | 300 req/min     | 1 min    | Agent ID      | `POST /v1/query`, `/v1/query/temporal`, `GET /v1/runs/*`, etc. |
| `search` | 100 req/min     | 1 min    | Agent ID      | `POST /v1/search`                               |

Agents with `admin` role or higher are **exempt** from ingest, query, and search rate limits. Auth rate limits apply to everyone (keyed by IP).

Response headers on rate-limited requests:
- `X-RateLimit-Limit` -- max requests per window
- `X-RateLimit-Remaining` -- requests remaining
- `X-RateLimit-Reset` -- Unix timestamp when the window resets

---

## 10. Quick Diagnostic Commands

```sh
# Is the server running?
curl -sf http://localhost:8080/health | jq .

# Outbox sync status
psql "$DATABASE_URL" -c "
  SELECT count(*) AS pending,
         count(*) FILTER (WHERE attempts > 0) AS retrying,
         count(*) FILTER (WHERE attempts >= 10) AS dead_letter
  FROM search_outbox;
"

# Recent outbox errors
psql "$DATABASE_URL" -c "
  SELECT decision_id, attempts, last_error, created_at
  FROM search_outbox
  WHERE last_error IS NOT NULL
  ORDER BY created_at DESC LIMIT 5;
"

# Decision count per org (capacity check)
psql "$DATABASE_URL" -c "
  SELECT o.name, count(d.id) AS decisions
  FROM organizations o
  LEFT JOIN decisions d ON d.org_id = o.id AND d.valid_to IS NULL
  GROUP BY o.name
  ORDER BY decisions DESC;
"

# Active PostgreSQL connections
psql "$NOTIFY_URL" -c "
  SELECT count(*) AS total,
         count(*) FILTER (WHERE state = 'active') AS active
  FROM pg_stat_activity
  WHERE datname = 'akashi';
"

# Reset dead-letter outbox entries after fixing root cause
psql "$DATABASE_URL" -c "
  UPDATE search_outbox SET attempts = 0, locked_until = NULL WHERE attempts >= 10;
"
```
