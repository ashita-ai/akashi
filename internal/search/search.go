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
	// excludeID is removed from results (the source decision). project, when non-nil,
	// restricts results to decisions with the same project value or no project.
	FindSimilar(ctx context.Context, orgID uuid.UUID, embedding []float32, excludeID uuid.UUID, project *string, limit int) ([]Result, error)
}

// ReScore adjusts raw similarity scores with outcome signals and recency weighting,
// sorts descending by adjusted score, and truncates to limit.
//
// Formula (issue #235 redesign):
//
//	outcome_weight =
//	    0.40 * assessment_score    // (correct + 0.5*partial) / total; ONLY when assessed
//	    0.25 * citation_score      // log1p(citations) / log(6); logarithmic, saturates at 5
//	    0.15 * stability_score     // 1.0 unless superseded within 48h (0.0)
//	    0.10 * agreement_score     // min(AgreementCount / 3.0, 1.0)
//	    0.10 * conflict_win_rate   // wins/(wins+losses); ONLY when conflict history exists
//
//	relevance = similarity * (0.5 + 0.5*outcome_weight) * recency_decay
//
// Key design choices vs prior formula:
//   - Assessment is the primary signal (0.40) because it's the only explicit correctness feedback.
//     It contributes 0 when no assessments exist — no phantom neutral score.
//   - Citations use a logarithmic cap so the first citation is worth more than later ones.
//   - Conflict win rate contributes 0 when no conflict history exists, separating "never contested"
//     from "won all conflicts". A decision that lost all conflicts scores 0 on this signal, same as
//     one that was never contested — neither is penalized for the absence of conflict history.
//   - Completeness is removed from the relevance formula. Field-filling quality (evidence,
//     alternatives, reasoning present) does not imply decision correctness. Completeness remains
//     as a display metric but no longer inflates ranking.
//
// Cold-start (new decision, stable, no other signals): outcome_weight = 0.15, multiplier = 0.575.
// Maximum (all signals perfect): outcome_weight = 1.0, multiplier = 1.0 (capped to 1.0 by caller).
// The caller is responsible for populating outcome signal fields on model.Decision before calling
// (see hydrateAndReScore which calls GetDecisionOutcomeSignalsBatch and GetAssessmentSummaryBatch).
func ReScore(results []Result, decisions map[uuid.UUID]model.Decision, limit int) []model.SearchResult {
	now := time.Now()
	scored := make([]model.SearchResult, 0, len(results))

	for _, r := range results {
		d, ok := decisions[r.DecisionID]
		if !ok {
			// Decision was deleted or invalidated between Qdrant search and Postgres hydration.
			continue
		}

		// Citation: logarithmic cap — first citations matter more than later ones.
		// Saturates at 5 citations (log(6)/log(6) = 1.0).
		citationScore := math.Min(math.Log1p(float64(d.PrecedentCitationCount))/math.Log(6), 1.0)

		// Agreement: linear cap (unchanged).
		agreementScore := math.Min(float64(d.AgreementCount)/3.0, 1.0)

		// Stability: decisions superseded within 48h of creation were likely wrong.
		stabilityScore := 1.0
		if d.SupersessionVelocityHours != nil && *d.SupersessionVelocityHours < 48 {
			stabilityScore = 0.0
		}

		// Conflict win rate: only contributes when conflict history exists.
		// "Never contested" (no history) is neutral — it contributes 0 like a 50% win rate would.
		// A decision that won adjudicated conflicts earns a boost; one that lost doesn't.
		conflictContrib := 0.0
		wins := float64(d.ConflictFate.Won)
		losses := float64(d.ConflictFate.Lost)
		if wins+losses > 0 {
			conflictContrib = (wins / (wins + losses)) * 0.10
		}

		// Assessment: primary signal — explicit correctness feedback from agents.
		// Partially-correct counts as half a correct.
		// Contributes 0 when no assessments exist: no phantom neutral boost for unreviewed decisions.
		assessmentContrib := 0.0
		if d.AssessmentSummary != nil && d.AssessmentSummary.Total > 0 {
			a := d.AssessmentSummary
			assessmentContrib = ((float64(a.Correct) + 0.5*float64(a.PartiallyCorrect)) / float64(a.Total)) * 0.40
		}

		outcomeWeight := assessmentContrib + 0.25*citationScore + 0.15*stabilityScore + 0.10*agreementScore + conflictContrib

		ageDays := math.Max(0, now.Sub(d.ValidFrom).Hours()/24.0)
		recencyDecay := 1.0 / (1.0 + ageDays/90.0)
		// Completeness removed: field-filling quality ≠ decision correctness.
		relevance := float64(r.Score) * (0.5 + 0.5*outcomeWeight) * recencyDecay

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
