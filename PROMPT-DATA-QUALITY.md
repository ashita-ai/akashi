# Akashi Data Quality & Local Embeddings Implementation

You are implementing data quality improvements and local embedding support for the Akashi decision trace server. Read `SPEC-DATA-QUALITY.md` for the full specification. Your work persists between iterations — check git log and the codebase to see what you've already done.

## Implementation Order

Work in this exact order. Each step must compile and pass tests before moving to the next.

### Step 1: Ollama Embedding Provider

**Files to create/modify:**

1. Create `internal/service/embedding/ollama.go`:
   - `OllamaProvider` struct implementing the `Provider` interface
   - `NewOllamaProvider(baseURL, model string, dimensions, padTo int) *OllamaProvider`
   - `Embed(ctx, text) (pgvector.Vector, error)` — POST to `/api/embeddings`
   - `EmbedBatch(ctx, texts) ([]pgvector.Vector, error)` — sequential calls (Ollama has no batch API)
   - `Dimensions() int` — returns padTo if set, otherwise dimensions
   - Zero-pad vectors from native dimension (1024) to schema dimension (1536)

2. Create `internal/service/embedding/ollama_test.go`:
   - Mock HTTP server returning a 1024-dim vector
   - Test padding to 1536
   - Test error handling

3. Update `internal/config/config.go`:
   - Add `EmbeddingProvider string` (env: `AKASHI_EMBEDDING_PROVIDER`, default: `auto`)
   - Add `OllamaURL string` (env: `OLLAMA_URL`, default: `http://localhost:11434`)
   - Add `OllamaModel string` (env: `OLLAMA_MODEL`, default: `mxbai-embed-large`)

4. Update `cmd/akashi/main.go`:
   - Add `newEmbeddingProvider(cfg)` function with auto-detection logic:
     - `openai`: require OPENAI_API_KEY, use OpenAIProvider
     - `ollama`: use OllamaProvider with padding
     - `noop`: use NoopProvider
     - `auto`: try OpenAI if key present, else try Ollama ping, else noop with warning
   - Add `ollamaReachable(baseURL) bool` helper

5. Update `.env.example`:
   - Add the three new env vars with comments

**Exit criteria**: `go build ./...` passes. `go test ./internal/service/embedding/...` passes. Can start server with `AKASHI_EMBEDDING_PROVIDER=ollama`.

---

### Step 2: Quality Score Schema

**Files to create/modify:**

1. Create `migrations/011_add_quality_score.sql`:
```sql
ALTER TABLE decisions ADD COLUMN IF NOT EXISTS quality_score REAL DEFAULT 0.0;
CREATE INDEX IF NOT EXISTS idx_decisions_quality ON decisions (quality_score DESC);
CREATE INDEX IF NOT EXISTS idx_decisions_type_quality ON decisions (decision_type, quality_score DESC);
```

2. Update `internal/model/decision.go`:
   - Add `QualityScore float32 `json:"quality_score"`` to `Decision` struct

3. Update `internal/storage/decisions.go`:
   - Add `quality_score` to INSERT in `CreateDecision`
   - Add `quality_score` to SELECT list and Scan in `GetDecision`, `QueryDecisions`, `scanDecisions`
   - Add `quality_score` to the column list in `CreateDecision`

**Exit criteria**: `go build ./...` passes. Migration applies cleanly. Decision struct includes QualityScore.

---

### Step 3: Quality Scoring Service

**Files to create/modify:**

1. Create `internal/service/quality/quality.go`:
```go
package quality

// StandardDecisionTypes are the canonical types from prompt templates
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

// Score computes a quality score (0.0-1.0) for a trace decision
func Score(d model.TraceDecision) float32
```

Scoring factors (see SPEC-DATA-QUALITY.md for details):
- Confidence present and reasonable (0.05-0.95): 0.15
- Reasoning substantive (>50 chars): up to 0.25
- Alternatives provided (>=2): up to 0.20
- Rejection reason on any alternative: 0.10
- Evidence provided: up to 0.15
- Standard decision type: 0.10
- Outcome substantive (>20 chars): 0.05

2. Create `internal/service/quality/quality_test.go`:
   - Table-driven tests with various input combinations
   - Test edge cases: empty trace, perfect trace, partial trace

**Exit criteria**: `go test ./internal/service/quality/...` passes.

---

### Step 4: Integrate Quality Scoring

**Files to modify:**

1. Update `internal/server/handlers_decisions.go` in `HandleTrace`:
   - Import `github.com/ashita-ai/akashi/internal/service/quality`
   - After validation, before CreateDecision: `qualityScore := quality.Score(req.Decision)`
   - Pass `QualityScore: qualityScore` to CreateDecision

**Exit criteria**: `go build ./...` passes. New traces have quality_score populated.

---

### Step 5: Quality-Weighted Search

**Files to modify:**

1. Update `internal/storage/decisions.go` in `SearchDecisionsByEmbedding`:
   - Change SELECT to include `quality_score`
   - Change relevance formula to: `(1 - (embedding <=> $1)) * (0.7 + 0.3 * quality_score)`
   - Order by `relevance DESC`

2. Update `internal/server/handlers_decisions.go` in `HandleCheck`:
   - In the structured query path, change `OrderBy` from `valid_from` to `quality_score`

**Exit criteria**: `go build ./...` passes. Search results ordered by quality-weighted relevance.

---

### Step 6: Temporal Decay

**Files to modify:**

1. Update `internal/storage/decisions.go` in `SearchDecisionsByEmbedding`:
   - Extend relevance formula to include recency factor:
   ```sql
   (1 - (embedding <=> $1))
     * (0.6 + 0.3 * quality_score)
     * (1.0 / (1.0 + EXTRACT(EPOCH FROM (NOW() - valid_from)) / 86400.0 / 90.0))
   AS relevance
   ```

**Exit criteria**: `go build ./...` passes. Recent decisions rank higher than old ones at equal similarity/quality.

---

### Step 7: Precedent Reference Tracking

**Files to create/modify:**

1. Create `migrations/012_add_precedent_ref.sql`:
```sql
ALTER TABLE decisions ADD COLUMN IF NOT EXISTS precedent_ref UUID REFERENCES decisions(id);
CREATE INDEX IF NOT EXISTS idx_decisions_precedent_ref ON decisions (precedent_ref) WHERE precedent_ref IS NOT NULL;
```

2. Update `internal/model/decision.go`:
   - Add `PrecedentRef *uuid.UUID `json:"precedent_ref,omitempty"`` to `Decision` struct

3. Update `internal/model/api.go`:
   - Add `PrecedentRef *uuid.UUID `json:"precedent_ref,omitempty"`` to `TraceRequest` struct

4. Update `internal/storage/decisions.go`:
   - Add `precedent_ref` to INSERT in `CreateDecision`
   - Add `precedent_ref` to SELECT list and Scan

5. Update `internal/server/handlers_decisions.go` in `HandleTrace`:
   - Pass `PrecedentRef: req.PrecedentRef` to CreateDecision

**Exit criteria**: `go build ./...` passes. Can create trace with `precedent_ref` field.

---

### Step 8: Tests and Verification

**Files to create/modify:**

1. Add integration tests in `internal/server/server_test.go`:
   - `TestTraceQualityScore` — create trace with full details, verify quality_score > 0.7
   - `TestTraceQualityScoreLow` — create minimal trace, verify quality_score < 0.3
   - `TestSearchQualityOrdering` — create high and low quality traces, verify high ranks first
   - `TestTracePrecedentRef` — create trace referencing prior decision

2. Verify all existing tests still pass

**Exit criteria**: `go test ./...` passes. All new functionality covered.

---

## What to Do Each Iteration

1. Check git log and the current state of the codebase to understand what's been done.
2. Identify the next incomplete step from the implementation order above.
3. Implement it fully with tests.
4. Run `go build ./...` and `go vet ./...` to verify compilation.
5. Run `go test ./...` if tests are ready (some require Docker).
6. Commit your work with a clear commit message.
7. If all 8 steps are complete and tests pass, output: `<promise>DATA QUALITY COMPLETE</promise>`

## Code Standards

- Complete production code. No TODOs, no placeholders, no stubs.
- Tests for all features (success and error cases). Table-driven tests.
- `context.Context` as first parameter on all I/O functions.
- Return explicit errors. Wrap with `fmt.Errorf("operation: %w", err)`.
- Godoc comments on all exported types and functions.
- Structured logging via `slog`. No `log.Println`.
- No `interface{}` where concrete types work.
- Do NOT include "Co-Authored-By" trailers in commits.

## Important Notes

- The schema uses `vector(1536)`. Ollama's mxbai-embed-large produces 1024-dim vectors. Pad with zeros.
- The Provider interface is already abstracted — add OllamaProvider alongside OpenAIProvider and NoopProvider.
- Quality scoring is computed at trace time and stored. Don't recompute on every query.
- Read existing code before modifying. The patterns are established.
