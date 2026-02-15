package tracehealth

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/ashita-ai/akashi/internal/storage"
)

func TestComputeGaps_AllHealthy(t *testing.T) {
	qs := storage.DecisionQualityStats{
		Total: 100, AvgQuality: 0.8, BelowHalf: 2, BelowThird: 0, WithReasoning: 95,
	}
	es := storage.EvidenceCoverageStats{
		TotalDecisions: 100, WithEvidence: 80, WithoutEvidenceCount: 20, CoveragePercent: 80,
	}
	gaps := computeGaps(qs, es, 5, 0)

	// No quality or coverage gaps. No open conflicts. Only "20 decisions lack evidence."
	assert.LessOrEqual(t, len(gaps), 3)
	for _, g := range gaps {
		assert.NotContains(t, g, "Average decision quality")
		assert.NotContains(t, g, "Less than half")
		assert.NotContains(t, g, "unresolved")
	}
}

func TestComputeGaps_LowQuality(t *testing.T) {
	qs := storage.DecisionQualityStats{
		Total: 50, AvgQuality: 0.2, BelowHalf: 30, BelowThird: 20, WithReasoning: 10,
	}
	es := storage.EvidenceCoverageStats{
		TotalDecisions: 50, WithEvidence: 30, WithoutEvidenceCount: 20, CoveragePercent: 60,
	}
	gaps := computeGaps(qs, es, 0, 0)

	assert.GreaterOrEqual(t, len(gaps), 1)
	assert.Contains(t, gaps[0], "Average decision quality")
}

func TestComputeGaps_LowEvidence(t *testing.T) {
	qs := storage.DecisionQualityStats{
		Total: 100, AvgQuality: 0.7, BelowHalf: 5, BelowThird: 0, WithReasoning: 90,
	}
	es := storage.EvidenceCoverageStats{
		TotalDecisions: 100, WithEvidence: 30, WithoutEvidenceCount: 70, CoveragePercent: 30,
	}
	gaps := computeGaps(qs, es, 0, 0)

	found := false
	for _, g := range gaps {
		if g == "Less than half of decisions have supporting evidence." {
			found = true
		}
	}
	assert.True(t, found, "expected low evidence gap")
}

func TestComputeGaps_UnresolvedConflicts(t *testing.T) {
	qs := storage.DecisionQualityStats{
		Total: 100, AvgQuality: 0.7, BelowHalf: 5, BelowThird: 0, WithReasoning: 90,
	}
	es := storage.EvidenceCoverageStats{
		TotalDecisions: 100, WithEvidence: 80, WithoutEvidenceCount: 20, CoveragePercent: 80,
	}
	gaps := computeGaps(qs, es, 10, 7)

	found := false
	for _, g := range gaps {
		if g == "7 of 10 conflicts are unresolved." {
			found = true
		}
	}
	assert.True(t, found, "expected unresolved conflicts gap")
}

func TestComputeGaps_MaxThree(t *testing.T) {
	qs := storage.DecisionQualityStats{
		Total: 100, AvgQuality: 0.1, BelowHalf: 80, BelowThird: 60, WithReasoning: 10,
	}
	es := storage.EvidenceCoverageStats{
		TotalDecisions: 100, WithEvidence: 10, WithoutEvidenceCount: 90, CoveragePercent: 10,
	}
	gaps := computeGaps(qs, es, 20, 15)

	assert.LessOrEqual(t, len(gaps), 3, "should return at most 3 gaps")
}

func TestComputeStatus_Healthy(t *testing.T) {
	qs := storage.DecisionQualityStats{Total: 100, AvgQuality: 0.8}
	es := storage.EvidenceCoverageStats{CoveragePercent: 80}
	assert.Equal(t, "healthy", computeStatus(qs, es, 0))
}

func TestComputeStatus_NeedsAttention(t *testing.T) {
	qs := storage.DecisionQualityStats{Total: 100, AvgQuality: 0.2}
	es := storage.EvidenceCoverageStats{CoveragePercent: 30}
	assert.Equal(t, "needs_attention", computeStatus(qs, es, 5))
}

func TestComputeStatus_OneProblem(t *testing.T) {
	qs := storage.DecisionQualityStats{Total: 100, AvgQuality: 0.2}
	es := storage.EvidenceCoverageStats{CoveragePercent: 80}
	assert.Equal(t, "healthy", computeStatus(qs, es, 0))
}
