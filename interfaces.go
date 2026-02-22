package akashi

import (
	"context"
	"net/http"

	"github.com/google/uuid"
)

// EmbeddingProvider generates vector embeddings from text.
// When provided via WithEmbeddingProvider, replaces auto-detected Ollama/OpenAI/noop.
// Uses []float32 (not pgvector.Vector) to avoid forcing the pgvector dependency on
// external consumers. App.New() wraps it in an adapter for internal use.
type EmbeddingProvider interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)
	Dimensions() int
}

// Searcher is a vector search index for decisions.
// When provided via WithSearcher, replaces the auto-detected Qdrant index.
// Returns decision IDs + scores; the caller hydrates full decisions from Postgres.
type Searcher interface {
	Search(ctx context.Context, orgID uuid.UUID, embedding []float32, filters SearchFilters, limit int) ([]SearchResult, error)
	Healthy(ctx context.Context) error
}

// ConflictScorer performs pairwise conflict scoring.
// When provided via WithConflictScorer, replaces the built-in embedding+LLM scorer
// for the pairwise confirmation step. Candidate finding via Qdrant still runs in OSS.
type ConflictScorer interface {
	Score(ctx context.Context, a, b Decision) (ConflictScore, error)
}

// EventHook receives async notifications when decision lifecycle events occur.
// Multiple hooks may be registered via multiple WithEventHook calls.
// Hook methods run in goroutines — they must not block indefinitely.
// Failures are logged but do not fail the originating request.
type EventHook interface {
	OnDecisionTraced(ctx context.Context, decision Decision) error
	OnConflictDetected(ctx context.Context, conflict Conflict) error
}

// PolicyEvaluator checks decisions against organizational rules.
// Advisory: called during akashi_check (surfaced as warnings).
// Enforcement: called during /v1/trace (returns HTTP 422 on violation if action="enforce").
// This interface is defined now to reserve the extension point; the enterprise
// policy engine implementation is a future spec.
type PolicyEvaluator interface {
	Evaluate(ctx context.Context, orgID uuid.UUID, decision Decision, action string) ([]Violation, error)
}

// RouteRegistrar registers additional routes on the shared HTTP mux.
// Enterprise routes share the mux, auth chain, and OTEL instrumentation with OSS routes.
// The function is called once during App.New() after all OSS routes are registered.
type RouteRegistrar func(mux *http.ServeMux, auth AuthHelper)

// AuthHelper provides RBAC middleware for use in RouteRegistrar.
// It wraps the server's requireRole function so enterprise routes use the same
// auth chain without depending on internal/server directly.
type AuthHelper interface {
	RequireRole(role Role) func(http.Handler) http.Handler
}

// Middleware wraps the root HTTP handler.
// Applied outermost (before routing), so it sees all requests including /health.
// Use for license enforcement, custom logging, or cross-cutting headers.
// Multiple middlewares are applied in registration order (first-registered = outermost).
type Middleware func(http.Handler) http.Handler
