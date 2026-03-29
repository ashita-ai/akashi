# Akashi Operator's Runbook

Production operations guide for the Akashi decision trace server.

For lifecycle-specific retention, archival, and reconciliation procedures, see:
`docs/operations/data-lifecycle.md`.

---

## 1. Health Checks

### Endpoint

```
GET /health
```

No authentication required.

### Response

```json
{
  "data": {
    "status": "healthy",
    "version": "1.0.0",
    "postgres": "connected",
    "qdrant": "connected",
    "buffer_depth": 0,
    "buffer_status": "ok",
    "sse_broker": "running",
    "uptime_seconds": 86400
  },
  "meta": {
    "request_id": "9a4c58db-8d9f-4cad-9ec8-c9476e4af9a6",
    "timestamp": "2026-02-14T04:21:00Z"
  }
}
```

| Field (under `data`) | Healthy Value   | Unhealthy Value   |
|----------------------|-----------------|-------------------|
| `status`             | `"healthy"`     | `"degraded"` / `"unhealthy"` |
| `postgres`           | `"connected"`   | `"disconnected"`  |
| `qdrant`             | `"connected"`   | `"disconnected"`  |
| `buffer_status`      | `"ok"`          | `"high"` / `"critical"` |

Three status values:
- **`healthy`** (HTTP 200) — Postgres connected, buffer below 75% capacity.
- **`degraded`** (HTTP 200) — Postgres connected, but buffer above 75% capacity (`buffer_status: "critical"`).
- **`unhealthy`** (HTTP 503) — PostgreSQL is unreachable.

The endpoint returns 503 if and only if PostgreSQL is unreachable. Qdrant being down does NOT cause a 503 -- the system degrades to text search.

The `qdrant` field is omitted entirely when Qdrant is not configured (no `QDRANT_URL`).
The `sse_broker` field is omitted when SSE/NOTIFY is disabled.

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
| `akashi.buffer.depth`          | Gauge     | 1    | _(none)_ |
| `akashi.buffer.dropped_total`  | Gauge     | 1    | _(none; ingress rejections due to capacity or shutdown drain)_ |
| `akashi.embedding.duration`    | Histogram | ms   | _(none)_ |
| `akashi.search.duration`       | Histogram | ms   | _(none)_ |
| `akashi.outbox.depth`          | Gauge     | 1    | _(none, via pg_class.reltuples estimate)_ |

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
| Event ingestion rejected              | `akashi.buffer.dropped_total` increasing OR log line `"trace: buffer at capacity"` | Any occurrence              | Critical |
| PostgreSQL pool exhaustion            | pgxpool metrics or connection wait time                                    | > 80% utilization           | Warning  |
| Qdrant health                         | `/health` response `qdrant: "disconnected"`                                | Sustained > 5 min           | Warning  |
| Rate limit 429s                       | `rate(http.server.request_count{http.status_code="429"})`                  | > 10/s sustained            | Warning  |

### Log-Based Alerts

Akashi logs JSON to stdout. Key log messages to monitor:

| Log Message (substring)                        | Meaning                                              |
|-------------------------------------------------|------------------------------------------------------|
| `"trace: flush failed"`                         | Event buffer failed to write to PostgreSQL            |
| `"trace: buffer at capacity"`                   | Event ingestion backpressure engaged (request rejected) |
| `"trace: buffer is draining"`                   | Node is shutting down; new event ingestion rejected      |
| `"search outbox: dead-letter entry"`            | Outbox entry exceeded 10 retry attempts               |
| `"search outbox: qdrant upsert"` + `error`      | Qdrant write failure (entries will retry)             |
| `"storage: notify reconnect attempt failed"`    | LISTEN/NOTIFY connection dropped, attempting recovery |
| `"conflict refresh failed"`                     | (Obsolete: conflicts are event-driven; RefreshConflicts is now a no-op) |
| `"rate limiter error, permitting request"`      | Limiter malfunction; request allowed (fail-open)      |
| `"rate limit exceeded"`                         | Agent hit rate limit; request rejected with 429       |

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
4. Requests rejected at capacity increment `akashi.buffer.dropped_total`; clients must retry to avoid event loss.

---

### Idempotency Key Conflicts (409)

**Symptoms**: `POST /v1/trace`, `POST /v1/runs`, or `POST /v1/runs/{run_id}/events` returns `409 CONFLICT` with a message about idempotency key mismatch or request already in progress.

**Impact**:
- No duplicate write is committed.
- A conflicting request is rejected until key/payload consistency is restored.

**Common causes**:
- Same `Idempotency-Key` reused for a different payload
- Client retries while original request is still processing
- Very long-running request exceeded in-progress reclaim window

**Remediation**:
1. Verify retries use the same payload bytes for the same key.
2. For "already in progress", use exponential backoff and retry.
3. Stale in-progress keys are cleared by the background cleanup job (`AKASHI_IDEMPOTENCY_ABANDONED_TTL`).
4. Ensure key generation is unique per logical write operation.

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

**Automatic cleanup**: Dead-letter entries older than 7 days are archived to `search_outbox_dead_letters`, then removed from `search_outbox` (checked hourly).

### Rate Limiting (429s)

**Symptoms**: Agents receiving `429 Too Many Requests` responses. Log line: `"rate limit exceeded"`.

**Default limits**: 100 requests/second sustained, 200 burst per agent per org.

**Tuning**:

```sh
AKASHI_RATE_LIMIT_RPS=200      # Double the sustained rate
AKASHI_RATE_LIMIT_BURST=500    # Allow larger bursts
```

**Disable entirely** (not recommended for production):

```sh
AKASHI_RATE_LIMIT_ENABLED=false
```

Platform admins are exempt from rate limiting. If a specific agent needs higher limits, either raise the global limit or promote the agent to platform_admin.

When Akashi runs behind a load balancer, set `AKASHI_TRUST_PROXY=true` so IP-based rate limits use the client IP from `X-Forwarded-For` instead of the proxy's address. Only enable when behind a trusted reverse proxy.

### Embedding Failures

**Symptoms**: Log line `"embedding: ... error"`. Decisions stored with `embedding = NULL`. Semantic search returns fewer results than expected.

**Common causes**:
- `AKASHI_EMBEDDING_PROVIDER=openai` but `OPENAI_API_KEY` is unset or invalid
- Ollama is down or unreachable (check `OLLAMA_URL` if using Ollama)
- Embedding dimension mismatch between `AKASHI_EMBEDDING_DIMENSIONS` and model output

**Recovery**: Fix the provider, then restart the server. The startup backfill job will embed any decisions that have `embedding IS NULL`.

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

**On startup**: by default, the server applies migrations from the embedded `migrations` package (built into the binary). Migration failure is fatal — the server will not start.

If you run Atlas externally in production, set:

```sh
AKASHI_SKIP_EMBEDDED_MIGRATIONS=true
```

This avoids startup migration races and keeps migration ownership with Atlas.

### Backup

Standard `pg_dump` works. Key tables by priority:

| Table                        | Notes                                                       |
|------------------------------|-------------------------------------------------------------|
| `organizations`              | Tenant configuration. Small. Always back up.                |
| `agents`                     | Auth identities. Small. Always back up.                     |
| `agent_runs`                 | Trace run metadata. Moderate size.                          |
| `agent_events`               | Append-only event log. Potentially very large. Consider partial or time-bounded backup. |
| `decisions`                  | Core decision data with embeddings. Can be large.           |
| `alternatives`               | Decision options. Required for complete decision reconstruction. |
| `evidence`                   | Decision evidence and citations.                            |
| `access_grants`              | RBAC grants. Small. Always back up.                         |
| `scored_conflicts`           | Conflict graph data used by conflict APIs.                  |
| `integrity_proofs`           | Merkle batch proofs for tamper/audit verification.          |
| `idempotency_keys`           | Replay safety records for write APIs.                       |
| `schema_migrations`          | Migration version tracking.                                 |
| `search_outbox`              | Pending sync queue for Qdrant.                              |
| `search_outbox_dead_letters` | Archived failed outbox entries (paper trail).               |
| `deletion_audit_log`         | Archived deleted records for destructive admin operations.   |
| `mutation_audit_log`         | Append-only ledger for API mutation paper trail.            |
| `agent_events_archive`       | Archived historical events moved out of the hot hypertable. |

```sh
# Full backup
pg_dump "$DATABASE_URL" -Fc -f akashi-backup-$(date +%Y%m%d).dump

# Data-only backup of core tables (skip events)
pg_dump "$DATABASE_URL" -Fc --table=organizations --table=agents \
  --table=decisions --table=agent_runs --table=access_grants \
  -f akashi-core-$(date +%Y%m%d).dump
```

### Restore (Durability-Critical Procedure)

1. Stop all Akashi instances so no new writes arrive during restore.
2. Restore PostgreSQL from a known-good dump:
   ```sh
   pg_restore --clean --if-exists --no-owner --no-privileges \
     -d "$DATABASE_URL" akashi-backup-YYYYMMDD.dump
   ```
3. Start Akashi and verify health:
   ```sh
   curl -sf http://localhost:8080/health | jq .data
   ```
4. Run post-restore checks:
   ```sql
   SELECT count(*) FROM decisions WHERE valid_to IS NULL;
   SELECT count(*) FROM agent_runs;
   SELECT count(*) FROM agent_events;
   SELECT count(*) FROM search_outbox WHERE attempts < 10;
   ```
5. If Qdrant index state is stale or missing, repopulate outbox from current decisions, then allow worker replay:
   ```sql
   INSERT INTO search_outbox (decision_id, org_id, operation)
   SELECT id, org_id, 'upsert'
   FROM decisions
   WHERE valid_to IS NULL AND embedding IS NOT NULL
   ON CONFLICT (decision_id, operation) DO UPDATE
      SET created_at = now(), attempts = 0, locked_until = NULL;
   ```

PostgreSQL is the source of truth. `search_outbox` is transient and cannot by itself reconstruct all historical sync intent after restore.

Automated verification helper:

```sh
# Verify restore invariants and table integrity checks
DATABASE_URL=postgres://... make verify-restore

# Optionally repopulate outbox from current decisions during drill recovery
DATABASE_URL=postgres://... REBUILD_OUTBOX=true make verify-restore
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

### Postgres/Qdrant Reconciliation

Use reconciliation to detect and repair drift between PostgreSQL source-of-truth decisions and Qdrant indexed points.

```sh
# Detect drift (exit non-zero if mismatch exists)
DATABASE_URL=postgres://... QDRANT_URL=https://...:6333 make reconcile-qdrant

# Repair missing Qdrant points by queueing outbox upserts
DATABASE_URL=postgres://... QDRANT_URL=https://...:6333 make reconcile-qdrant-repair
```

`reconcile-qdrant-repair` only queues missing entries into `search_outbox`; it does not delete extra Qdrant points automatically.

### Exit Criteria Verification

Use a single verifier to evaluate durability/consistency gates with structured JSON output:

```sh
DATABASE_URL=postgres://... make verify-exit-criteria
```

Optional thresholds:

- `MAX_DEAD_LETTERS` (default `0`)
- `MAX_OUTBOX_OLDEST_SECONDS` (default `1800`)
- `STRICT_RETENTION_CHECK` (default `false`)
- `RETAIN_DAYS` (default `90`, only used when strict retention is enabled)

When `QDRANT_URL` is set, the verifier also checks Postgres/Qdrant drift by running reconciliation in read-only mode.

### Branch Protection Policy

For protected branches (for example `main`), configure GitHub branch protection to require this status check before merge:

- `Verify Exit Criteria`

Recommended minimum required checks:

- `CI`
- `Build with UI`
- `Verify Exit Criteria`

### GDPR / Right-to-Erasure (Delete Agent Data)

Use the admin-only endpoint:

```http
DELETE /v1/agents/{agent_id}
Authorization: Bearer <admin-jwt>
X-Akashi-Org-Id: <org-uuid>
```

This performs a transactional delete of the agent and related records (runs, events, decisions, access grants), and clears supersedes links that point at deleted decisions.
The endpoint is disabled by default; set `AKASHI_ENABLE_DESTRUCTIVE_DELETE=true` to allow execution.

### Event Retention and Archival (TimescaleDB)

`agent_events` is a Timescale hypertable and can grow quickly. Use archive-before-purge to preserve paper trail while controlling storage.

```sh
# Preview one archival window (safe default, no purge)
DATABASE_URL=postgres://... make archive-events-dry-run

# Archive then purge one window (explicit destructive mode)
DATABASE_URL=postgres://... DRY_RUN=false ENABLE_PURGE=true make archive-events
```

Optional knobs:

- `RETAIN_DAYS` (default `90`) - keep recent events in primary hypertable
- `BATCH_DAYS` (default `1`) - process one bounded time window per run to reduce lock pressure

Archive destination:

- `agent_events_archive` holds immutable historical rows moved out of the hot hypertable.

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
| JWT validation          | Stateless. All instances must have the same public key.                            |

### Bottlenecks

1. **PostgreSQL** is the primary bottleneck. Scale read replicas for query load. Consider connection pooling (PgBouncer) tuning.
2. **Qdrant** for vector search at scale. Monitor query latency via `http.server.duration` on `/v1/search`.

### SSE Limitation

SSE subscriptions are bound to the instance the client connects to. With multiple instances behind a load balancer, a client only receives events produced by its connected instance. For full coverage, clients should use polling (`GET /v1/decisions/recent`) or ensure sticky sessions.

---

## 7. Configuration Reference

See [configuration.md](configuration.md) for the full environment variable reference.

---

## 8. Graceful Shutdown

On `SIGTERM` or `SIGINT`, the server shuts down in this order:

```
1. HTTP server drains           -- stops accepting new requests, completes in-flight (`AKASHI_SHUTDOWN_HTTP_TIMEOUT`)
2. Async post-trace drain       -- waits for in-flight claim generation and conflict scoring (`AKASHI_SHUTDOWN_ASYNC_DRAIN_TIMEOUT`)
3. Event buffer drains          -- final flush to PostgreSQL (`AKASHI_SHUTDOWN_BUFFER_DRAIN_TIMEOUT`)
4. Outbox worker drains         -- syncs remaining entries to Qdrant (`AKASHI_SHUTDOWN_OUTBOX_DRAIN_TIMEOUT`)
5. Cleanup                      -- grant cache, rate limiter, Qdrant client closed
6. OTEL flushes                 -- final trace/metric export
7. Database pool closes         -- PgBouncer pool + NOTIFY connection
```

There is no single shared shutdown timeout. Each phase has its own timeout, and setting a timeout to `0` waits indefinitely.

### Warnings

- **DO NOT** send `kill -9` during shutdown. If the event buffer is mid-flush, events in memory will be lost.
- Buffer drain is durability-critical and runs without a timeout. Do not force-stop the process while draining.
- The outbox worker drain timeout (log: `"search outbox: drain timed out"`) means some outbox entries were not synced to Qdrant. They remain in PostgreSQL and will sync on next startup.

### Pre-Deployment Checklist

1. Ensure load balancer has stopped sending traffic (remove from target group or mark unhealthy).
2. Send `SIGTERM`.
3. Wait for exit (can be indefinite when drain timeouts are set to `0`).
4. Verify clean shutdown: log line `"akashi stopped"` with exit code 0.

---

## 9. Quick Diagnostic Commands

```sh
# Is the server running?
curl -sf http://localhost:8080/health | jq .data.status

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

---

## 10. Conflict Detection Evaluation

Akashi detects conflicts between agent decisions using an embedding-based scorer followed by an optional LLM validator. Two evaluation modes let you measure detection quality against ground truth.

### Concepts

| Term | Meaning |
|------|---------|
| **Scorer** | Embedding similarity pipeline that flags candidate conflicts. Fast, cheap, always on. |
| **Validator** | LLM-based second pass that confirms or rejects scorer candidates. Slower, costs API tokens. |
| **Ground truth label** | Human judgment on a detected conflict: was it real? |

### Label Types

Every detected conflict (in `scored_conflicts`) can be labeled with one of three values:

| Label | Meaning | Scorer eval role |
|-------|---------|-----------------|
| `genuine` | Real conflict — the decisions truly contradict each other | True positive |
| `related_not_contradicting` | Same topic but not actually contradictory (e.g. paraphrases) | False positive |
| `unrelated_false_positive` | Different topics entirely — should not have been flagged | False positive |

### Labeling Conflicts via API

All label endpoints require admin authentication.

```sh
# Authenticate
TOKEN=$(curl -s http://localhost:8081/auth/token \
  -d '{"agent_id":"admin","api_key":"ak_..."}' | jq -r .token)

# List detected conflicts to find IDs to label
curl -s http://localhost:8081/v1/conflicts \
  -H "Authorization: Bearer $TOKEN" | jq '.conflicts[:5]'

# Label a conflict as genuine
curl -X PUT http://localhost:8081/v1/admin/conflicts/{id}/label \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"label": "genuine", "notes": "clearly opposite caching strategies"}'

# Label a conflict as false positive
curl -X PUT http://localhost:8081/v1/admin/conflicts/{id}/label \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"label": "related_not_contradicting", "notes": "same decision, different wording"}'

# View a label
curl -s http://localhost:8081/v1/admin/conflicts/{id}/label \
  -H "Authorization: Bearer $TOKEN" | jq .

# List all labels with counts
curl -s http://localhost:8081/v1/admin/conflict-labels \
  -H "Authorization: Bearer $TOKEN" | jq .

# Delete a label (to re-label)
curl -X DELETE http://localhost:8081/v1/admin/conflicts/{id}/label \
  -H "Authorization: Bearer $TOKEN"
```

### Running Scorer Eval (Precision from Labels)

Once you have labeled conflicts, compute scorer precision. This measures what fraction of the scorer's detections are genuine conflicts.

**Precision = genuine / (genuine + related_not_contradicting + unrelated_false_positive)**

Note: recall cannot be computed from labels alone because labels only cover *detected* conflicts. Measuring recall requires a separate dataset of known conflicts that should have been detected.

```sh
# Via CLI (recommended)
export AKASHI_URL=http://localhost:8081
export AKASHI_AGENT_ID=admin
export AKASHI_API_KEY=ak_...

go run ./cmd/eval-conflicts --mode=scorer

# Save results to ./eval-results/
go run ./cmd/eval-conflicts --mode=scorer --save

# Via API directly
curl -X POST http://localhost:8081/v1/admin/scorer-eval \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{}' | jq .
```

Example output:

```
Scorer Precision: 85.7% (6 TP, 1 FP, 7 labeled)
```

### Running Validator Eval (LLM Accuracy)

The validator eval runs a hardcoded dataset of 27 decision pairs through the LLM validator and measures precision and recall of the LLM's conflict/no-conflict judgments.

```sh
export AKASHI_URL=http://localhost:8081
export AKASHI_AGENT_ID=admin
export AKASHI_API_KEY=ak_...

# Run validator eval (requires LLM API access — costs tokens)
go run ./cmd/eval-conflicts --mode=validator

# Save results
go run ./cmd/eval-conflicts --mode=validator --save
```

The validator eval requires the akashi server to have a working embedding/LLM provider configured.

### Saving and Reviewing Results

The `--save` flag writes JSON results to `./eval-results/`:

```
eval-results/
  scorer_2026-03-07T14-30-00.json
  validator_2026-03-07T14-35-00.json
```

This directory is gitignored. Results accumulate locally so you can track precision over time as you tune scorer thresholds or retrain embeddings.

### Synthetic Benchmark (Development Only)

A 300-pair synthetic dataset tests the scorer's embedding math in isolation (not real detection quality). This is gated behind an environment variable and requires a running TimescaleDB with testcontainers:

```sh
AKASHI_BENCH=1 go test -run TestScorerPrecisionRecall ./internal/conflicts/ -v
```

This is useful for verifying that threshold changes don't break the scorer's ability to distinguish orthogonal embeddings. It does not test real-world detection quality — use the label-based eval for that.

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `AKASHI_URL` | `http://localhost:8081` | Base URL of the akashi instance to evaluate |
| `AKASHI_AGENT_ID` | (required) | Agent ID for authentication |
| `AKASHI_API_KEY` | (required) | API key for admin authentication |

### Recommended Workflow

1. Run your akashi instance locally on port 8081.
2. Exercise the system — trace decisions, let the scorer detect conflicts.
3. Review detected conflicts via the UI or `GET /v1/conflicts`.
4. Label 20+ conflicts across all three categories for a meaningful precision measurement.
5. Run `go run ./cmd/eval-conflicts --mode=scorer --save` to compute precision.
6. Tune scorer thresholds (`AKASHI_CONFLICT_SIGNIFICANCE_THRESHOLD`, early exit floor) and re-evaluate.
7. Repeat after model or embedding changes to catch regressions.
