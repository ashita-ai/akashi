// Package search provides vector search capabilities using external search indexes
// with transparent fallback to text-based search in Postgres.
package search

import (
	"context"
	"math"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/model"
)

// Result holds a decision ID and its raw similarity score from the search index.
// The caller hydrates full Decision objects from Postgres (source of truth).
type Result struct {
	DecisionID uuid.UUID
	Score      float32
}

// Searcher is the interface for vector search indexes.
// Implementations must be safe for concurrent use.
type Searcher interface {
	// Search returns decision IDs matching the query vector, filtered by org and optional filters.
	// Returns IDs + raw similarity scores; the caller hydrates from Postgres.
	Search(ctx context.Context, orgID uuid.UUID, embedding []float32, filters model.QueryFilters, limit int) ([]Result, error)

	// Healthy returns nil if the search index is reachable, or an error describing the problem.
	Healthy(ctx context.Context) error
}

// CandidateFinder performs ANN search for internal use (conflict detection, consensus scoring).
// Unlike Searcher (user-facing, with filter parameters), CandidateFinder is optimized for
// internal org-scoped ANN: minimal filters, excludes a single source decision by ID.
//
// QdrantIndex implements both Searcher and CandidateFinder; callers that hold a Searcher
// can type-assert to CandidateFinder when they need internal ANN access.
type CandidateFinder interface {
	// FindSimilar returns decision IDs similar to the given embedding within an org.
	// excludeID is removed from results (the source decision). repo, when non-nil,
	// restricts results to decisions with the same repo value or no repo.
	FindSimilar(ctx context.Context, orgID uuid.UUID, embedding []float32, excludeID uuid.UUID, repo *string, limit int) ([]Result, error)
}

// ReScore adjusts raw similarity scores with quality and recency weighting, sorts
// descending by adjusted score, and truncates to limit.
//
// Formula: relevance = similarity * (0.6 + 0.3 * quality_score) * (1.0 / (1.0 + age_days / 90.0))
func ReScore(results []Result, decisions map[uuid.UUID]model.Decision, limit int) []model.SearchResult {
	now := time.Now()
	scored := make([]model.SearchResult, 0, len(results))

	for _, r := range results {
		d, ok := decisions[r.DecisionID]
		if !ok {
			// Decision was deleted or invalidated between Qdrant search and Postgres hydration.
			continue
		}

		ageDays := math.Max(0, now.Sub(d.ValidFrom).Hours()/24.0)
		qualityBonus := 0.6 + 0.3*float64(d.QualityScore)
		recencyDecay := 1.0 / (1.0 + ageDays/90.0)
		relevance := float64(r.Score) * qualityBonus * recencyDecay

		scored = append(scored, model.SearchResult{
			Decision:        d,
			SimilarityScore: float32(math.Min(relevance, 1.0)),
		})
	}

	sort.Slice(scored, func(i, j int) bool {
		return scored[i].SimilarityScore > scored[j].SimilarityScore
	})

	if len(scored) > limit {
		scored = scored[:limit]
	}
	return scored
}
