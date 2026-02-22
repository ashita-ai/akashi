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

// ReScore adjusts raw similarity scores with outcome signals, completeness, and recency
// weighting, sorts descending by adjusted score, and truncates to limit.
//
// Formula (spec 36):
//
//	outcome_weight =
//	    0.4 * min(PrecedentCitationCount / 5.0, 1.0)   // citation_score
//	    0.3 * wins / (wins + losses), default 0.5       // conflict_win_rate
//	    0.2 * min(AgreementCount / 3.0, 1.0)            // agreement_score
//	    0.1 * stability_score                           // 1.0 if not superseded within 48h, else 0.0
//
//	relevance = similarity * (0.5 + 0.3*outcome_weight + 0.2*completeness_score) * recency_decay
//
// Cold-start (new decision, all signals zero): outcome_weight = 0.25, relevance multiplier ≈ 0.665.
// The caller is responsible for populating outcome signal fields on model.Decision before calling
// (see hydrateAndReScore which calls GetDecisionOutcomeSignalsBatch first).
func ReScore(results []Result, decisions map[uuid.UUID]model.Decision, limit int) []model.SearchResult {
	now := time.Now()
	scored := make([]model.SearchResult, 0, len(results))

	for _, r := range results {
		d, ok := decisions[r.DecisionID]
		if !ok {
			// Decision was deleted or invalidated between Qdrant search and Postgres hydration.
			continue
		}

		// Outcome signals.
		citationScore := math.Min(float64(d.PrecedentCitationCount)/5.0, 1.0)
		wins := float64(d.ConflictFate.Won)
		losses := float64(d.ConflictFate.Lost)
		var conflictWinRate float64
		if wins+losses > 0 {
			conflictWinRate = wins / (wins + losses)
		} else {
			conflictWinRate = 0.5 // neutral: no resolved conflicts yet
		}
		agreementScore := math.Min(float64(d.AgreementCount)/3.0, 1.0)
		stabilityScore := 1.0
		if d.SupersessionVelocityHours != nil && *d.SupersessionVelocityHours < 48 {
			stabilityScore = 0.0
		}
		outcomeWeight := 0.4*citationScore + 0.3*conflictWinRate + 0.2*agreementScore + 0.1*stabilityScore

		ageDays := math.Max(0, now.Sub(d.ValidFrom).Hours()/24.0)
		recencyDecay := 1.0 / (1.0 + ageDays/90.0)
		relevance := float64(r.Score) * (0.5 + 0.3*outcomeWeight + 0.2*float64(d.CompletenessScore)) * recencyDecay

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
