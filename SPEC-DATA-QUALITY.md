# Akashi Data Quality & Local Embeddings Spec

## Overview

This spec covers two complementary improvements:
1. **Local embeddings via Ollama** — Remove OpenAI dependency for POC/development
2. **Data quality mechanisms** — Ensure precedent lookup returns useful results

Both are designed for implementation in a single focused session.

---

## Part 1: Ollama Embedding Provider

### Goal

Add an `OllamaProvider` that implements the existing `embedding.Provider` interface, enabling local embedding generation without external API dependencies.

### Interface (existing)

```go
// Provider generates vector embeddings from text.
type Provider interface {
    Embed(ctx context.Context, text string) (pgvector.Vector, error)
    EmbedBatch(ctx context.Context, texts []string) ([]pgvector.Vector, error)
    Dimensions() int
}
```

### Implementation: `internal/service/embedding/ollama.go`

```go
package embedding

import (
    "bytes"
    "context"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "time"

    "github.com/pgvector/pgvector-go"
)

// OllamaProvider generates embeddings using a local Ollama server.
type OllamaProvider struct {
    baseURL    string
    model      string
    httpClient *http.Client
    dimensions int
    padTo      int // target dimension for compatibility (0 = no padding)
}

// NewOllamaProvider creates a provider that calls Ollama's embedding API.
// Model should be an embedding model like "mxbai-embed-large" or "nomic-embed-text".
// If padTo > dimensions, vectors are zero-padded for schema compatibility.
func NewOllamaProvider(baseURL, model string, dimensions, padTo int) *OllamaProvider {
    if baseURL == "" {
        baseURL = "http://localhost:11434"
    }
    return &OllamaProvider{
        baseURL: baseURL,
        model:   model,
        httpClient: &http.Client{
            Timeout: 30 * time.Second,
        },
        dimensions: dimensions,
        padTo:      padTo,
    }
}

// Dimensions returns the output vector size (after padding if configured).
func (p *OllamaProvider) Dimensions() int {
    if p.padTo > p.dimensions {
        return p.padTo
    }
    return p.dimensions
}

type ollamaEmbedRequest struct {
    Model  string `json:"model"`
    Prompt string `json:"prompt"`
}

type ollamaEmbedResponse struct {
    Embedding []float32 `json:"embedding"`
}

// Embed generates a single embedding.
func (p *OllamaProvider) Embed(ctx context.Context, text string) (pgvector.Vector, error) {
    reqBody, err := json.Marshal(ollamaEmbedRequest{
        Model:  p.model,
        Prompt: text,
    })
    if err != nil {
        return pgvector.Vector{}, fmt.Errorf("ollama: marshal request: %w", err)
    }

    req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/api/embeddings", bytes.NewReader(reqBody))
    if err != nil {
        return pgvector.Vector{}, fmt.Errorf("ollama: create request: %w", err)
    }
    req.Header.Set("Content-Type", "application/json")

    resp, err := p.httpClient.Do(req)
    if err != nil {
        return pgvector.Vector{}, fmt.Errorf("ollama: send request: %w", err)
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusOK {
        body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
        return pgvector.Vector{}, fmt.Errorf("ollama: status %d: %s", resp.StatusCode, string(body))
    }

    var result ollamaEmbedResponse
    if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
        return pgvector.Vector{}, fmt.Errorf("ollama: decode response: %w", err)
    }

    if len(result.Embedding) == 0 {
        return pgvector.Vector{}, fmt.Errorf("ollama: empty embedding returned")
    }

    // Pad if needed for schema compatibility
    vec := result.Embedding
    if p.padTo > len(vec) {
        padded := make([]float32, p.padTo)
        copy(padded, vec)
        vec = padded
    }

    return pgvector.NewVector(vec), nil
}

// EmbedBatch generates embeddings for multiple texts.
// Ollama doesn't have a native batch API, so we call sequentially.
// For high throughput, consider a worker pool.
func (p *OllamaProvider) EmbedBatch(ctx context.Context, texts []string) ([]pgvector.Vector, error) {
    if len(texts) == 0 {
        return nil, nil
    }

    vecs := make([]pgvector.Vector, len(texts))
    for i, text := range texts {
        vec, err := p.Embed(ctx, text)
        if err != nil {
            return nil, fmt.Errorf("ollama: batch item %d: %w", i, err)
        }
        vecs[i] = vec
    }
    return vecs, nil
}
```

### Configuration

Add to `internal/config/config.go`:

```go
type Config struct {
    // ... existing fields

    // Embedding provider selection
    EmbeddingProvider string `env:"AKASHI_EMBEDDING_PROVIDER" envDefault:"auto"`

    // Ollama settings (used when provider is "ollama" or "auto" with no OpenAI key)
    OllamaURL   string `env:"OLLAMA_URL" envDefault:"http://localhost:11434"`
    OllamaModel string `env:"OLLAMA_MODEL" envDefault:"mxbai-embed-large"`
}
```

Provider selection logic in `cmd/akashi/main.go`:

```go
func newEmbeddingProvider(cfg *config.Config) embedding.Provider {
    switch cfg.EmbeddingProvider {
    case "openai":
        if cfg.OpenAIAPIKey == "" {
            log.Fatal("OPENAI_API_KEY required when AKASHI_EMBEDDING_PROVIDER=openai")
        }
        return embedding.NewOpenAIProvider(cfg.OpenAIAPIKey, cfg.EmbeddingModel)

    case "ollama":
        return embedding.NewOllamaProvider(cfg.OllamaURL, cfg.OllamaModel, 1024, 1536)

    case "noop":
        return embedding.NewNoopProvider(1536)

    case "auto":
        fallthrough
    default:
        // Auto-detect: prefer OpenAI if key present, then Ollama if reachable, else noop
        if cfg.OpenAIAPIKey != "" {
            return embedding.NewOpenAIProvider(cfg.OpenAIAPIKey, cfg.EmbeddingModel)
        }
        // Try to ping Ollama
        if ollamaReachable(cfg.OllamaURL) {
            return embedding.NewOllamaProvider(cfg.OllamaURL, cfg.OllamaModel, 1024, 1536)
        }
        log.Warn("no embedding provider available, using noop (semantic search disabled)")
        return embedding.NewNoopProvider(1536)
    }
}

func ollamaReachable(baseURL string) bool {
    ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
    defer cancel()
    req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/tags", nil)
    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        return false
    }
    resp.Body.Close()
    return resp.StatusCode == http.StatusOK
}
```

### Environment Variables

Update `.env.example`:

```bash
# Embedding provider: "auto" (default), "openai", "ollama", or "noop"
AKASHI_EMBEDDING_PROVIDER=auto

# OpenAI (used when provider is "openai" or "auto" with key present)
OPENAI_API_KEY=
AKASHI_EMBEDDING_MODEL=text-embedding-3-small

# Ollama (used when provider is "ollama" or "auto" without OpenAI key)
OLLAMA_URL=http://localhost:11434
OLLAMA_MODEL=mxbai-embed-large
```

### Testing

Add `internal/service/embedding/ollama_test.go`:

```go
package embedding

import (
    "context"
    "net/http"
    "net/http/httptest"
    "testing"
)

func TestOllamaProvider(t *testing.T) {
    // Mock Ollama server
    server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if r.URL.Path != "/api/embeddings" {
            t.Errorf("unexpected path: %s", r.URL.Path)
        }
        // Return a mock 1024-dim embedding
        vec := make([]float32, 1024)
        for i := range vec {
            vec[i] = float32(i) * 0.001
        }
        json.NewEncoder(w).Encode(ollamaEmbedResponse{Embedding: vec})
    }))
    defer server.Close()

    p := NewOllamaProvider(server.URL, "test-model", 1024, 1536)

    t.Run("dimensions with padding", func(t *testing.T) {
        if p.Dimensions() != 1536 {
            t.Errorf("expected 1536, got %d", p.Dimensions())
        }
    })

    t.Run("embed single", func(t *testing.T) {
        vec, err := p.Embed(context.Background(), "test text")
        if err != nil {
            t.Fatal(err)
        }
        if len(vec.Slice()) != 1536 {
            t.Errorf("expected 1536-dim vector, got %d", len(vec.Slice()))
        }
    })

    t.Run("embed batch", func(t *testing.T) {
        vecs, err := p.EmbedBatch(context.Background(), []string{"a", "b", "c"})
        if err != nil {
            t.Fatal(err)
        }
        if len(vecs) != 3 {
            t.Errorf("expected 3 vectors, got %d", len(vecs))
        }
    })
}
```

---

## Part 2: Trace Quality Scoring

### Goal

Compute a quality score (0.0-1.0) for each decision trace at ingestion time. Use this score to rank precedent results so higher-quality decisions surface first.

### Schema Change

Add migration `migrations/011_add_quality_score.sql`:

```sql
-- Add quality score column to decisions table
ALTER TABLE decisions ADD COLUMN IF NOT EXISTS quality_score REAL DEFAULT 0.0;

-- Add index for ordering by quality
CREATE INDEX IF NOT EXISTS idx_decisions_quality ON decisions (quality_score DESC);

-- Composite index for quality-weighted search results
CREATE INDEX IF NOT EXISTS idx_decisions_type_quality ON decisions (decision_type, quality_score DESC);
```

### Quality Calculation

Add `internal/service/quality/quality.go`:

```go
package quality

import (
    "strings"

    "github.com/ashita-ai/akashi/internal/model"
)

// StandardDecisionTypes are the canonical types from the prompt templates.
var StandardDecisionTypes = map[string]bool{
    "model_selection": true,
    "architecture":    true,
    "data_source":     true,
    "error_handling":  true,
    "feature_scope":   true,
    "trade_off":       true,
    "deployment":      true,
    "security":        true,
}

// Score computes a quality score for a decision trace.
// Returns a value between 0.0 and 1.0.
func Score(d model.TraceDecision) float32 {
    var score float32

    // Factor 1: Confidence is present and reasonable (0.0-1.0 range, not exactly 0 or 1)
    // Weight: 0.15
    if d.Confidence > 0.05 && d.Confidence < 0.95 {
        score += 0.15
    } else if d.Confidence > 0 && d.Confidence < 1 {
        score += 0.10 // partial credit for any non-extreme value
    }

    // Factor 2: Reasoning is substantive (>50 chars after trimming)
    // Weight: 0.25
    if d.Reasoning != nil {
        reasoningLen := len(strings.TrimSpace(*d.Reasoning))
        if reasoningLen > 100 {
            score += 0.25
        } else if reasoningLen > 50 {
            score += 0.20
        } else if reasoningLen > 20 {
            score += 0.10
        }
    }

    // Factor 3: Alternatives provided (at least 2)
    // Weight: 0.20
    if len(d.Alternatives) >= 3 {
        score += 0.20
    } else if len(d.Alternatives) >= 2 {
        score += 0.15
    } else if len(d.Alternatives) >= 1 {
        score += 0.05
    }

    // Factor 4: At least one alternative has a rejection reason
    // Weight: 0.10
    for _, alt := range d.Alternatives {
        if alt.RejectionReason != nil && len(*alt.RejectionReason) > 10 {
            score += 0.10
            break
        }
    }

    // Factor 5: Evidence provided
    // Weight: 0.15
    if len(d.Evidence) >= 2 {
        score += 0.15
    } else if len(d.Evidence) >= 1 {
        score += 0.10
    }

    // Factor 6: Decision type is from standard taxonomy
    // Weight: 0.10
    if StandardDecisionTypes[d.DecisionType] {
        score += 0.10
    }

    // Factor 7: Outcome is substantive (>20 chars)
    // Weight: 0.05
    if len(strings.TrimSpace(d.Outcome)) > 20 {
        score += 0.05
    }

    return score
}
```

### Integration

Update `internal/server/handlers_decisions.go` in `HandleTrace`:

```go
import "github.com/ashita-ai/akashi/internal/service/quality"

// In HandleTrace, after validation, before CreateDecision:
qualityScore := quality.Score(req.Decision)

// Update CreateDecision call to include quality score:
decision, err := h.db.CreateDecision(r.Context(), model.Decision{
    RunID:        run.ID,
    AgentID:      req.AgentID,
    DecisionType: req.Decision.DecisionType,
    Outcome:      req.Decision.Outcome,
    Confidence:   req.Decision.Confidence,
    Reasoning:    req.Decision.Reasoning,
    Embedding:    decisionEmb,
    QualityScore: qualityScore, // new field
})
```

Update `internal/model/decision.go`:

```go
type Decision struct {
    // ... existing fields
    QualityScore float32 `json:"quality_score"`
}
```

Update `internal/storage/decisions.go` to include `quality_score` in INSERT and SELECT.

### Quality-Weighted Search

Update `SearchDecisionsByEmbedding` in `internal/storage/decisions.go`:

```go
// Change ORDER BY to weight similarity by quality
query := fmt.Sprintf(
    `SELECT id, run_id, agent_id, decision_type, outcome, confidence, reasoning,
     metadata, valid_from, valid_to, transaction_time, created_at, quality_score,
     (1 - (embedding <=> $1)) * (0.7 + 0.3 * quality_score) AS relevance
     FROM decisions%s
     ORDER BY relevance DESC
     LIMIT %d`, where, limit,
)
```

This formula:
- Base similarity contributes 70% of relevance
- Quality score contributes up to 30% bonus
- A perfect quality score (1.0) boosts relevance by 30%
- A zero quality score provides no bonus but doesn't penalize

### Quality-Weighted Check

Update `HandleCheck` to order by quality:

```go
// In the structured query path:
queried, _, err := h.db.QueryDecisions(r.Context(), model.QueryRequest{
    Filters:  filters,
    Include:  []string{"alternatives"},
    OrderBy:  "quality_score", // Changed from valid_from
    OrderDir: "desc",
    Limit:    limit,
})
```

---

## Part 3: Temporal Decay

### Goal

Recent decisions are more relevant than old ones. Apply a time-based decay factor to search results.

### Implementation

Update the relevance formula in `SearchDecisionsByEmbedding`:

```go
// Decay factor: halves every 90 days
// Formula: 1 / (1 + days_old / 90)
query := fmt.Sprintf(
    `SELECT id, run_id, agent_id, decision_type, outcome, confidence, reasoning,
     metadata, valid_from, valid_to, transaction_time, created_at, quality_score,
     (1 - (embedding <=> $1))
       * (0.6 + 0.3 * quality_score)
       * (1.0 / (1.0 + EXTRACT(EPOCH FROM (NOW() - valid_from)) / 86400.0 / 90.0))
       AS relevance
     FROM decisions%s
     ORDER BY relevance DESC
     LIMIT %d`, where, limit,
)
```

Breakdown:
- Semantic similarity: 60% base weight
- Quality score: up to 30% bonus
- Recency: multiplier from 1.0 (today) to ~0.5 (90 days old) to ~0.25 (270 days old)

---

## Part 4: Precedent Reference Tracking

### Goal

Track when a decision was influenced by a prior precedent. This creates a feedback signal for which decisions are actually useful.

### Schema Change

Add migration `migrations/012_add_precedent_ref.sql`:

```sql
-- Add precedent reference column
ALTER TABLE decisions ADD COLUMN IF NOT EXISTS precedent_ref UUID REFERENCES decisions(id);

-- Index for finding decisions that reference a given precedent
CREATE INDEX IF NOT EXISTS idx_decisions_precedent_ref ON decisions (precedent_ref) WHERE precedent_ref IS NOT NULL;

-- Materialized view for precedent usefulness scores
CREATE MATERIALIZED VIEW IF NOT EXISTS precedent_usefulness AS
SELECT
    precedent_ref,
    COUNT(*) AS reference_count,
    AVG(quality_score) AS avg_referrer_quality
FROM decisions
WHERE precedent_ref IS NOT NULL
GROUP BY precedent_ref;

CREATE UNIQUE INDEX IF NOT EXISTS idx_precedent_usefulness_id ON precedent_usefulness (precedent_ref);

-- Refresh function (call periodically or via trigger)
-- For now, refresh every 5 minutes via application-level scheduler
```

### API Update

Update `internal/model/api.go`:

```go
type TraceRequest struct {
    AgentID      string        `json:"agent_id"`
    Decision     TraceDecision `json:"decision"`
    TraceID      *string       `json:"trace_id,omitempty"`
    Metadata     any           `json:"metadata,omitempty"`
    PrecedentRef *uuid.UUID    `json:"precedent_ref,omitempty"` // decision that influenced this one
}
```

Update `HandleTrace` to pass through:

```go
decision, err := h.db.CreateDecision(r.Context(), model.Decision{
    // ... existing fields
    PrecedentRef: req.PrecedentRef,
})
```

### Future: Usefulness-Boosted Search

Once precedent references accumulate, boost decisions that are frequently referenced:

```go
// Join with precedent_usefulness to boost frequently-referenced decisions
query := `
SELECT d.*,
    (1 - (d.embedding <=> $1))
    * (0.5 + 0.25 * d.quality_score + 0.25 * COALESCE(pu.reference_count, 0) / 10.0)
    * (1.0 / (1.0 + EXTRACT(EPOCH FROM (NOW() - d.valid_from)) / 86400.0 / 90.0))
    AS relevance
FROM decisions d
LEFT JOIN precedent_usefulness pu ON pu.precedent_ref = d.id
WHERE ...
ORDER BY relevance DESC
`
```

---

## Implementation Order

1. **Ollama provider** (~30 min)
   - Create `internal/service/embedding/ollama.go`
   - Update config with new env vars
   - Update main.go provider selection
   - Add test

2. **Quality scoring** (~45 min)
   - Create migration 011
   - Create `internal/service/quality/quality.go`
   - Update Decision model
   - Update storage layer
   - Update HandleTrace
   - Add test

3. **Quality-weighted search** (~15 min)
   - Update SearchDecisionsByEmbedding query
   - Update HandleCheck ordering

4. **Temporal decay** (~10 min)
   - Update relevance formula in search

5. **Precedent reference** (~30 min)
   - Create migration 012
   - Update TraceRequest model
   - Update HandleTrace
   - Update storage layer

Total: ~2-2.5 hours

---

## Verification

After implementation:

1. `go build ./...` passes
2. `go test ./...` passes
3. Start server with `AKASHI_EMBEDDING_PROVIDER=ollama` (requires local Ollama with mxbai-embed-large)
4. Create a trace with full details (reasoning, alternatives, evidence) — verify quality_score > 0.7
5. Create a trace with minimal details — verify quality_score < 0.3
6. Search for related decisions — verify high-quality results rank first
7. Create a trace with `precedent_ref` pointing to a prior decision — verify it's stored

---

## Notes

- Padding Ollama vectors to 1536 is a pragmatic hack. A proper fix would make the dimension configurable and handle migrations. For POC, padding works.
- The quality weights are initial guesses. They should be tuned based on real usage data.
- Temporal decay rate (90 days to 50%) is also a guess. Different domains may want different rates.
- The precedent_usefulness view refresh is not automated in this spec. Add a goroutine or cron job to refresh it periodically.
