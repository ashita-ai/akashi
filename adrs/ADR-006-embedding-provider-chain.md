# ADR-006: Embedding provider chain with graceful degradation

**Status:** Accepted
**Date:** 2026-02-03

## Context

Akashi stores vector embeddings alongside decisions and evidence to power semantic similarity search ("find decisions similar to X"). Generating these embeddings requires an external model, and the choice of model provider varies dramatically across deployment environments:

- **Self-hosted / air-gapped deployments** cannot call external APIs and need embeddings generated locally.
- **Cloud deployments** may prefer a managed API for convenience or quality.
- **Minimal / evaluation setups** may have no embedding infrastructure at all and should not fail to start.

We need an embedding subsystem that works across all three scenarios without requiring the operator to configure anything in the common case.

## Decision

Implement a provider chain with three concrete implementations behind a single `Provider` interface:

```
Provider interface {
    Embed(ctx context.Context, text string) (pgvector.Vector, error)
    EmbedBatch(ctx context.Context, texts []string) ([]pgvector.Vector, error)
    Dimensions() int
}
```

The three providers, in priority order:

| Priority | Provider | Model | Dimensions | When selected |
|----------|----------|-------|------------|---------------|
| 1 | `OllamaProvider` | mxbai-embed-large | 1024 | Ollama server responds to health check |
| 2 | `OpenAIProvider` | text-embedding-3-small | 1024 (truncated from native 1536 via API `dimensions` parameter) | `OPENAI_API_KEY` is set |
| 3 | `NoopProvider` | n/a | matches configured dimensions | Always available (terminal fallback) |

### Auto-detection (default mode)

When `AKASHI_EMBEDDING_PROVIDER` is `"auto"` (the default), the `newEmbeddingProvider` function at startup:

1. Sends a GET to `{OLLAMA_URL}/api/tags` with a 2-second timeout. If the response is 200 OK, selects `OllamaProvider`.
2. If Ollama is unreachable, checks whether `OPENAI_API_KEY` is non-empty. If set, selects `OpenAIProvider`.
3. If neither is available, selects `NoopProvider` and logs a warning.

This is a startup-time decision. The provider does not change while the process is running.

### Explicit override

Operators can bypass auto-detection by setting `AKASHI_EMBEDDING_PROVIDER` to `"ollama"`, `"openai"`, or `"noop"`. When set to `"openai"` without a valid `OPENAI_API_KEY`, the system logs an error and falls back to noop rather than crashing.

### Configuration

| Environment variable | Default | Purpose |
|---------------------|---------|---------|
| `AKASHI_EMBEDDING_PROVIDER` | `"auto"` | Provider selection: `"auto"`, `"ollama"`, `"openai"`, `"noop"` |
| `AKASHI_EMBEDDING_DIMENSIONS` | `1024` | Vector dimensionality (must match chosen model) |
| `OLLAMA_URL` | `http://localhost:11434` | Ollama server address |
| `OLLAMA_MODEL` | `mxbai-embed-large` | Ollama embedding model name |
| `OPENAI_API_KEY` | (empty) | OpenAI API key |
| `AKASHI_EMBEDDING_MODEL` | `text-embedding-3-small` | OpenAI embedding model name |

## Rationale

### Why Ollama first

- **Zero-configuration vector search for OSS users.** If someone runs `ollama pull mxbai-embed-large` and starts Akashi, semantic search works immediately. No API keys, no accounts, no cost.
- **Data sovereignty.** Embeddings are generated on the same machine or local network. Decision text never leaves the operator's infrastructure. This matters for regulated industries and air-gapped environments.
- **No external dependency.** Ollama runs on commodity hardware with no API keys or accounts required, which removes a barrier to embedding every decision and evidence record.
- **Offline operation.** Development, CI, and air-gapped production environments all work without internet access.

### Why OpenAI as second choice

- **Convenience for cloud-native deployments.** Teams already using OpenAI for their agents can reuse the same API key. No additional infrastructure to deploy.
- **Higher throughput at scale.** OpenAI's batch endpoint processes multiple texts in a single HTTP round-trip. The `EmbedBatch` implementation sends all texts in one API call, unlike Ollama which calls sequentially (Ollama lacks a native batch API).
- **Quality.** OpenAI's embedding models are well-benchmarked and widely understood. For use cases where embedding quality directly affects retrieval precision, this may be preferred.

### Why noop as terminal fallback

- **Never fail to start.** An Akashi instance with no embedding provider should still function for all non-semantic operations: trace ingestion, decision recording, event queries, the audit UI, data export, and text-based search. Crashing on a missing optional dependency is hostile to operators.
- **Graceful degradation.** The `NoopProvider` returns zero vectors (`[]float32` of the configured dimension, all zeros). When pgvector computes cosine similarity against a zero vector, the result is undefined/zero for all candidates, which means semantic search returns no meaningful results. The system naturally degrades to metadata and text-based filtering without a special code path.
- **Explicit signal.** A startup log line (`no embedding provider available, using noop (semantic search disabled)`) tells operators exactly what happened and what to do about it.

### Why a single interface, not a chain-of-responsibility at query time

The provider is selected once at startup, not per-request. This is deliberate:

- **Consistency.** All embeddings in the database should come from the same model. Mixing embeddings from different models (e.g., Ollama's mxbai-embed-large and OpenAI's text-embedding-3-small) in the same vector column produces meaningless similarity scores. Cosine similarity between vectors from different embedding spaces is not semantically valid.
- **Simplicity.** A single `Provider` field on the service struct eliminates retry/fallback complexity in the hot path.
- **Predictability.** Operators can reason about which provider is active by reading a single log line at startup.

### Why 1024 dimensions as the default

The default dimension is 1024, matching Ollama's `mxbai-embed-large` model. This is smaller than OpenAI's native 1536 for `text-embedding-3-small`, but OpenAI supports a `dimensions` parameter to truncate output. 1024 dimensions provide a good balance between recall quality and storage/index cost in pgvector. The dimension is configurable via `AKASHI_EMBEDDING_DIMENSIONS` for operators who need a different value.

## Consequences

- The `Provider` interface is the sole abstraction for embedding generation. All consumers (the decisions service, future Qdrant sync) depend on this interface, never on a concrete provider.
- Adding a new provider (e.g., Cohere, a local ONNX runtime) requires implementing three methods and adding a case to `newEmbeddingProvider`. No changes to consumers.
- Operators who switch providers on an existing deployment must re-embed all stored vectors, since vectors from different models are not comparable. There is no migration tooling for this yet.
- The Ollama `EmbedBatch` implementation calls `Embed` sequentially in a loop. This is correct but slower than OpenAI's single-call batch. If Ollama adds a native batch endpoint in the future, `EmbedBatch` should be updated to use it.
- The health check at startup is a point-in-time probe. If Ollama becomes unreachable after startup, embedding calls will fail with errors. The system does not fall back to OpenAI at runtime (see rationale above for why this is intentional).

## References

- ADR-002: Unified PostgreSQL storage (pgvector stores embeddings; Qdrant is derived index)
- Implementation: `internal/service/embedding/embedding.go`, `internal/service/embedding/ollama.go`
- Provider initialization: `cmd/akashi/main.go` (`newEmbeddingProvider`, `ollamaReachable`)
- Configuration: `internal/config/config.go`
- Consumer: `internal/service/decisions/service.go`
- mxbai-embed-large model card: mixedbread.ai/docs/models/mxbai-embed-large-v1
