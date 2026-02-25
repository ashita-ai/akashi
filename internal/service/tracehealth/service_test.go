package tracehealth

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/ashita-ai/akashi/internal/storage"
)

func TestComputeGaps_AllHealthy(t *testing.T) {
	qs := storage.DecisionQualityStats{
		Total: 100, AvgCompleteness: 0.8, BelowHalf: 2, BelowThird: 0, WithReasoning: 95,
	}
	gaps := computeGaps(qs, 5, 0, storage.OutcomeSignalsSummary{})

	assert.LessOrEqual(t, len(gaps), 3)
	for _, g := range gaps {
		assert.NotContains(t, g, "Average completeness score")
		assert.NotContains(t, g, "evidence")
		assert.NotContains(t, g, "unresolved")
	}
}

func TestComputeGaps_LowQuality(t *testing.T) {
	qs := storage.DecisionQualityStats{
		Total: 50, AvgCompleteness: 0.2, BelowHalf: 30, BelowThird: 20, WithReasoning: 10,
	}
	gaps := computeGaps(qs, 0, 0, storage.OutcomeSignalsSummary{})

	assert.GreaterOrEqual(t, len(gaps), 1)
	assert.Contains(t, gaps[0], "Average completeness score")
}

func TestComputeGaps_UnresolvedConflicts(t *testing.T) {
	qs := storage.DecisionQualityStats{
		Total: 100, AvgCompleteness: 0.7, BelowHalf: 5, BelowThird: 0, WithReasoning: 90,
	}
	gaps := computeGaps(qs, 10, 7, storage.OutcomeSignalsSummary{})

	found := false
	for _, g := range gaps {
		if g == "7 of 10 conflicts are unresolved." {
			found = true
		}
	}
	assert.True(t, found, "expected unresolved conflicts gap")
}

// Evidence absence is never surfaced as a gap, regardless of coverage level.
// It's an opt-in field; near-0% is the expected state for most orgs.
func TestComputeGaps_EvidenceNeverSurfaces(t *testing.T) {
	qs := storage.DecisionQualityStats{
		Total: 100, AvgCompleteness: 0.7, BelowHalf: 5, BelowThird: 0, WithReasoning: 90,
	}
	gaps := computeGaps(qs, 0, 0, storage.OutcomeSignalsSummary{})

	for _, g := range gaps {
		assert.NotContains(t, g, "evidence")
	}
}

func TestComputeGaps_MaxThree(t *testing.T) {
	qs := storage.DecisionQualityStats{
		Total: 100, AvgCompleteness: 0.1, BelowHalf: 80, BelowThird: 60, WithReasoning: 10,
	}
	gaps := computeGaps(qs, 20, 15, storage.OutcomeSignalsSummary{})

	assert.LessOrEqual(t, len(gaps), 3, "should return at most 3 gaps")
}

func TestComputeStatus_Healthy(t *testing.T) {
	qs := storage.DecisionQualityStats{Total: 100, AvgCompleteness: 0.8}
	assert.Equal(t, "healthy", computeStatus(qs, 0))
}

func TestComputeStatus_NeedsAttention(t *testing.T) {
	qs := storage.DecisionQualityStats{Total: 100, AvgCompleteness: 0.2}
	assert.Equal(t, "needs_attention", computeStatus(qs, 5))
}

// A single genuine problem (low completeness) is enough to trigger needs_attention.
// We don't require two simultaneous failures before warning the user.
func TestComputeStatus_OneProblem(t *testing.T) {
	qs := storage.DecisionQualityStats{Total: 100, AvgCompleteness: 0.2}
	assert.Equal(t, "needs_attention", computeStatus(qs, 0))
}

// Evidence coverage alone — even very low — does not affect health status.
func TestComputeStatus_LowEvidenceAloneIsHealthy(t *testing.T) {
	qs := storage.DecisionQualityStats{Total: 100, AvgCompleteness: 0.8}
	assert.Equal(t, "healthy", computeStatus(qs, 0))
}
