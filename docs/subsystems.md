# Subsystem Reference

Internals of three key subsystems: embedding, rate limiting, and the Qdrant search pipeline. For configuration variables, see [configuration.md](configuration.md). For operational procedures, see [runbook.md](runbook.md).

---

## Embedding Provider

Akashi generates vector embeddings for every decision trace to enable semantic search ("find decisions similar to X"). The embedding provider is selected at startup and used throughout the server's lifetime.

### Provider Chain

```
AKASHI_EMBEDDING_PROVIDER=auto (default)
    │
    ├─ Try Ollama (GET /api/tags, 2s timeout)
    │  ├─ Reachable → OllamaProvider
    │  └─ Unreachable ↓
    │
    ├─ Check OPENAI_API_KEY
    │  ├─ Set → OpenAIProvider
    │  └─ Empty ↓
    │
    └─ NoopProvider (zero vectors, semantic search disabled)
```

Set `AKASHI_EMBEDDING_PROVIDER` to `ollama`, `openai`, or `noop` to skip auto-detection.

### Provider Implementations

| Provider | Model | Dimensions | Context Window | Data Residency |
|----------|-------|------------|----------------|----------------|
| `OllamaProvider` | `mxbai-embed-large` | 1024 | 512 tokens | On-premises |
| `OpenAIProvider` | `text-embedding-3-small` | 1024 | 8191 tokens | OpenAI servers |
| `NoopProvider` | N/A | configurable | N/A | N/A |

### Input Truncation (Ollama)

mxbai-embed-large has a 512-token context window. The Ollama provider truncates input in two layers:

1. **Client-side** (`truncateText`): Cuts text to 2000 characters at a word boundary before sending. At ~4 chars/token for English, this yields ~500 tokens — safely within the 512-token limit for prose. Code-heavy content tokenizes at ~2-3 chars/token, so some inputs may still exceed the limit.

2. **Server-side**: The `/api/embed` endpoint truncates at the token level as a safety net if the character-based estimate overshoots.

Decisions whose embedding text exceeds the limit are still stored in full — only the embedding input is truncated.

### Batch Support

`EmbedBatch` first tries Ollama's native batch API (`/api/embed` with an array input). If that fails (e.g., older Ollama versions), it falls back to concurrent single-text requests with a semaphore (max 4 concurrent).

OpenAI's `EmbedBatch` sends all texts in a single API call (native batch).

### Embedding Backfill

On startup, the server queries for decisions with `embedding IS NULL` and processes them in batches. This handles decisions that were created when the embedding provider was unavailable (e.g., Ollama was down) or before the provider was configured.

The backfill runs once per startup and logs progress:

```
{"level":"INFO","msg":"backfill: embedded decisions","count":5,"batch":1}
{"level":"INFO","msg":"embedding backfill complete","count":5}
```

### What Gets Embedded

The embedding text is constructed from decision fields (`internal/service/decisions/service.go`):

```
Type: {decision_type}
Outcome: {outcome}
Reasoning: {reasoning}
```

This is the input passed to `Embed()`. Alternatives and evidence are not included in the embedding text.

### Failure Behavior

If embedding fails for a decision (provider error, timeout, etc.), the decision is stored with `embedding = NULL`. It remains queryable via SQL filters and full-text search, but is invisible to semantic (vector) search. The backfill job on next restart will attempt to embed it again.

---

## Rate Limiting

### Interface

```go
type Limiter interface {
    Allow(ctx context.Context, key string) (bool, error)
    Close() error
}
```

Implementations must be safe for concurrent use. Errors are treated as fail-open: a broken limiter does not block traffic.

### OSS Implementation: MemoryLimiter

In-memory token bucket with per-key independent buckets.

- **Refill**: Tokens are added at `rate` per second (default 100).
- **Burst**: Bucket capacity (default 200). A new key starts with a full bucket.
- **Eviction**: A background goroutine evicts keys not accessed in 10 minutes (runs every minute).

### Key Construction

The middleware constructs keys as `org:<uuid>:agent:<id>`, giving each agent within each org an independent rate limit. Platform admins bypass rate limiting entirely. Unauthenticated paths (health, auth token) pass through.

### HTTP Behavior

When rate limited, the server returns `429 Too Many Requests` with a JSON error body:

```json
{"error": "rate limit exceeded"}
```

### Enterprise Extension

Enterprise deployments replace `MemoryLimiter` with a Redis-backed implementation for cross-instance coordination. The `Limiter` interface is the contract — the middleware is unaware of the backing store.

---

## Search Pipeline (Qdrant Sync)

Decisions are stored in PostgreSQL (source of truth) and indexed in Qdrant (derived search index). The outbox pattern ensures eventual consistency without distributed transactions.

### Data Flow

```
POST /v1/trace
    │
    ├─ 1. Decision written to PostgreSQL (with embedding)
    ├─ 2. Row inserted into search_outbox (same transaction)
    │
    └─ (async) OutboxWorker polls search_outbox
         │
         ├─ SELECT ... FOR UPDATE SKIP LOCKED (batch, max 100)
         ├─ Lock entries for 60s
         ├─ Fetch full decision data from PostgreSQL
         ├─ Upsert points to Qdrant (or delete)
         │
         ├─ Success → DELETE from search_outbox
         └─ Failure → INCREMENT attempts, exponential backoff
                       (2^attempts seconds, capped at 5 min)
```

### Qdrant Collection

Created automatically on startup if missing. Configuration:

| Property | Value |
|----------|-------|
| Collection name | `akashi_decisions` (configurable) |
| Vector size | 1024 (matches embedding dimensions) |
| Distance metric | Cosine similarity |
| HNSW M | 16 |
| HNSW ef_construct | 128 |

**Payload indexes** (for filtered search): `org_id`, `agent_id`, `decision_type` (keyword); `confidence`, `quality_score`, `valid_from_unix` (float).

Tenant isolation: every query includes `org_id` as a required filter.

### Retry and Dead Letters

| Behavior | Value |
|----------|-------|
| Max attempts | 10 |
| Backoff | Exponential: `2^attempts` seconds, capped at 5 minutes |
| Dead-letter cleanup | Entries with `attempts >= 10` and older than 7 days are deleted hourly |
| Lock duration | 60 seconds per batch (prevents double-processing) |

Dead-lettered entries remain in PostgreSQL and are queryable via SQL. They are not indexed in Qdrant until manually reset:

```sql
UPDATE search_outbox
SET attempts = 0, locked_until = NULL, last_error = NULL
WHERE attempts >= 10;
```

### Re-Scoring

Raw Qdrant similarity scores are adjusted before returning results:

```
relevance = similarity * (0.6 + 0.3 * quality_score) * (1.0 / (1.0 + age_days / 90.0))
```

- **Quality weight** (30%): Higher-quality decisions rank higher.
- **Recency decay**: Decisions lose relevance with a 90-day half-life.
- **Over-fetch**: Qdrant returns `limit * 3` results; re-scoring and truncation happen in Go.

### Graceful Shutdown

On shutdown, the outbox worker:

1. Cancels the poll loop.
2. Runs one final `processBatch` with the caller's drain context (respects deadline).
3. Signals completion via the `done` channel.

If the drain context expires before the final batch completes, the log emits `"search outbox: drain timed out"`. Remaining entries stay in the outbox and sync on next startup.

### Fallback: No Qdrant

When `QDRANT_URL` is empty, the outbox worker is not started and `POST /v1/search` falls back to PostgreSQL full-text search (`tsvector` with GIN index) plus ILIKE matching. Semantic similarity is unavailable; results are ranked by text relevance only.
