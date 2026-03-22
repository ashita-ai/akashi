package conflicts

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/model"
)

// Heuristic weights for the composite recommendation score.
const (
	weightConfidence    = 0.35
	weightRecency       = 0.25
	weightWinRate       = 0.25
	weightRevisionDepth = 0.15

	// Minimum composite magnitude to produce a recommendation.
	minCompositeThreshold = 0.10

	// Minimum individual score magnitude to include in reasons.
	minReasonThreshold = 0.05

	// Minimum resolved conflicts per agent before win rate is considered.
	minWinRateHistory = 3

	// Recency normalization: hours in one week. tanh(hoursDelta/168) saturates
	// at ~1.0 for deltas well beyond a week, preventing unbounded influence.
	recencyNormHours = 168.0

	// Revision depth normalization. tanh((depthB-depthA)/3) saturates at ~1.0
	// for depth differences of 3+.
	revisionNormDepth = 3.0
)

// RecommendationInput holds the signals needed to compute a recommendation.
// All fields are pre-fetched by the caller; the scoring logic has no DB deps.
type RecommendationInput struct {
	Conflict       model.DecisionConflict
	WinRateA       float64 // Agent A's win rate for this decision type [0,1].
	WinRateB       float64 // Agent B's win rate for this decision type [0,1].
	WinCountA      int     // Total resolved conflicts involving Agent A on this type.
	WinCountB      int     // Total resolved conflicts involving Agent B on this type.
	RevisionDepthA int     // Length of supersedes_id chain for decision A.
	RevisionDepthB int     // Length of supersedes_id chain for decision B.
}

// scoredReason pairs a directional score with a human-readable reason.
type scoredReason struct {
	score  float64 // weighted contribution (negative = favors A, positive = favors B)
	reason string
}

// Recommend computes a resolution recommendation from input signals.
// Returns nil when the conflict is already resolved or when composite signal
// strength is below the minimum threshold.
func Recommend(input RecommendationInput) *model.Recommendation {
	if input.Conflict.Status == "resolved" || input.Conflict.Status == "false_positive" {
		return nil
	}

	var reasons []scoredReason
	var composite float64

	// 1. Confidence delta.
	if r, ok := scoreConfidence(input.Conflict); ok {
		composite += r.score
		reasons = append(reasons, r)
	}

	// 2. Recency.
	if r, ok := scoreRecency(input.Conflict); ok {
		composite += r.score
		reasons = append(reasons, r)
	}

	// 3. Agent win rate (skip for self-contradictions).
	if input.Conflict.AgentA != input.Conflict.AgentB {
		if r, ok := scoreWinRate(input); ok {
			composite += r.score
			reasons = append(reasons, r)
		}
	}

	// 4. Revision depth.
	if r, ok := scoreRevisionDepth(input); ok {
		composite += r.score
		reasons = append(reasons, r)
	}

	if math.Abs(composite) < minCompositeThreshold {
		return nil
	}

	var winner uuid.UUID
	side := "B"
	if composite < 0 {
		winner = input.Conflict.DecisionAID
		side = "A"
	} else {
		winner = input.Conflict.DecisionBID
	}
	_ = side // used indirectly via reason text

	// Build reason strings, filtered and sorted by contribution magnitude.
	filtered := filterAndSortReasons(reasons)

	return &model.Recommendation{
		SuggestedWinner: winner,
		Reasons:         filtered,
		Confidence:      math.Min(0.99, math.Abs(composite)),
	}
}

// scoreConfidence produces a directional score based on confidence delta.
func scoreConfidence(c model.DecisionConflict) (scoredReason, bool) {
	delta := float64(c.ConfidenceB - c.ConfidenceA)
	if math.Abs(delta) < 0.01 {
		return scoredReason{}, false
	}

	weighted := weightConfidence * delta

	var reason string
	if delta > 0 {
		reason = fmt.Sprintf("Decision B has higher confidence (%.2f vs %.2f)",
			c.ConfidenceB, c.ConfidenceA)
	} else {
		reason = fmt.Sprintf("Decision A has higher confidence (%.2f vs %.2f)",
			c.ConfidenceA, c.ConfidenceB)
	}

	return scoredReason{score: weighted, reason: reason}, true
}

// scoreRecency produces a directional score based on decision timestamps.
func scoreRecency(c model.DecisionConflict) (scoredReason, bool) {
	hoursDelta := c.DecidedAtB.Sub(c.DecidedAtA).Hours()
	if math.Abs(hoursDelta) < 1 {
		return scoredReason{}, false
	}

	raw := math.Tanh(hoursDelta / recencyNormHours)
	weighted := weightRecency * raw

	var reason string
	if hoursDelta > 0 {
		reason = fmt.Sprintf("Decision B is more recent (%s vs %s)",
			formatAge(c.DecidedAtB), formatAge(c.DecidedAtA))
	} else {
		reason = fmt.Sprintf("Decision A is more recent (%s vs %s)",
			formatAge(c.DecidedAtA), formatAge(c.DecidedAtB))
	}

	return scoredReason{score: weighted, reason: reason}, true
}

// scoreWinRate produces a directional score based on agent resolution history.
func scoreWinRate(input RecommendationInput) (scoredReason, bool) {
	if input.WinCountA < minWinRateHistory || input.WinCountB < minWinRateHistory {
		return scoredReason{}, false
	}

	delta := input.WinRateB - input.WinRateA
	if math.Abs(delta) < 0.01 {
		return scoredReason{}, false
	}

	weighted := weightWinRate * delta

	c := input.Conflict
	var reason string
	if delta > 0 {
		reason = fmt.Sprintf("Agent %s has %.0f%% resolution win rate on %s decisions (vs %.0f%%)",
			c.AgentB, input.WinRateB*100, c.DecisionType, input.WinRateA*100)
	} else {
		reason = fmt.Sprintf("Agent %s has %.0f%% resolution win rate on %s decisions (vs %.0f%%)",
			c.AgentA, input.WinRateA*100, c.DecisionType, input.WinRateB*100)
	}

	return scoredReason{score: weighted, reason: reason}, true
}

// scoreRevisionDepth produces a directional score based on revision chain length.
func scoreRevisionDepth(input RecommendationInput) (scoredReason, bool) {
	if input.RevisionDepthA == 0 && input.RevisionDepthB == 0 {
		return scoredReason{}, false
	}

	depthDelta := float64(input.RevisionDepthB - input.RevisionDepthA)
	if math.Abs(depthDelta) < 0.5 {
		return scoredReason{}, false
	}

	raw := math.Tanh(depthDelta / revisionNormDepth)
	weighted := weightRevisionDepth * raw

	var reason string
	if depthDelta > 0 {
		reason = fmt.Sprintf("Decision B is a revision (depth %d, reflecting more deliberation)",
			input.RevisionDepthB)
	} else {
		reason = fmt.Sprintf("Decision A is a revision (depth %d, reflecting more deliberation)",
			input.RevisionDepthA)
	}

	return scoredReason{score: weighted, reason: reason}, true
}

// filterAndSortReasons returns reason strings for heuristics that contributed
// meaningfully, sorted by absolute contribution (strongest first).
func filterAndSortReasons(reasons []scoredReason) []string {
	// Filter by minimum contribution threshold.
	var significant []scoredReason
	for _, r := range reasons {
		if math.Abs(r.score) >= minReasonThreshold {
			significant = append(significant, r)
		}
	}

	// Sort by absolute contribution, strongest first.
	sort.Slice(significant, func(i, j int) bool {
		return math.Abs(significant[i].score) > math.Abs(significant[j].score)
	})

	result := make([]string, len(significant))
	for i, r := range significant {
		result[i] = r.reason
	}
	return result
}

// formatAge returns a human-readable relative time string.
func formatAge(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%.0fm ago", d.Minutes())
	case d < 24*time.Hour:
		return fmt.Sprintf("%.0fh ago", d.Hours())
	default:
		days := d.Hours() / 24
		return fmt.Sprintf("%.0fd ago", days)
	}
}
