package tracehealth

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashita-ai/akashi/internal/storage"
)

// mockStore embeds storage.Store so we only need to override the 4 methods
// used by tracehealth.Service.Compute. Calls to any other method will panic,
// which is acceptable for these unit tests.
type mockStore struct {
	storage.Store

	qualityStats     storage.DecisionQualityStats
	qualityStatsErr  error
	evidenceStats    storage.EvidenceCoverageStats
	evidenceStatsErr error
	conflictCounts   storage.ConflictStatusCounts
	conflictErr      error
	outcomeSignals   storage.OutcomeSignalsSummary
	outcomeErr       error
}

func (m *mockStore) GetDecisionQualityStats(_ context.Context, _ uuid.UUID) (storage.DecisionQualityStats, error) {
	return m.qualityStats, m.qualityStatsErr
}

func (m *mockStore) GetEvidenceCoverageStats(_ context.Context, _ uuid.UUID) (storage.EvidenceCoverageStats, error) {
	return m.evidenceStats, m.evidenceStatsErr
}

func (m *mockStore) GetConflictStatusCounts(_ context.Context, _ uuid.UUID) (storage.ConflictStatusCounts, error) {
	return m.conflictCounts, m.conflictErr
}

func (m *mockStore) GetOutcomeSignalsSummary(_ context.Context, _ uuid.UUID) (storage.OutcomeSignalsSummary, error) {
	return m.outcomeSignals, m.outcomeErr
}

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

// computeStatus only triggers needs_attention for >3 open conflicts, not exactly 3.
func TestComputeStatus_ExactlyThreeOpenConflictsIsHealthy(t *testing.T) {
	qs := storage.DecisionQualityStats{Total: 100, AvgCompleteness: 0.8}
	assert.Equal(t, "healthy", computeStatus(qs, 3))
}

func TestComputeStatus_FourOpenConflictsNeedsAttention(t *testing.T) {
	qs := storage.DecisionQualityStats{Total: 100, AvgCompleteness: 0.8}
	assert.Equal(t, "needs_attention", computeStatus(qs, 4))
}

func TestNew(t *testing.T) {
	ms := &mockStore{}
	svc := New(ms)
	require.NotNil(t, svc)
	assert.Equal(t, ms, svc.db)
}

func TestCompute_InsufficientData(t *testing.T) {
	ms := &mockStore{
		qualityStats: storage.DecisionQualityStats{Total: 0},
	}
	svc := New(ms)

	m, err := svc.Compute(context.Background(), uuid.New())
	require.NoError(t, err)
	assert.Equal(t, "insufficient_data", m.Status)
	assert.NotNil(t, m.Completeness)
	assert.NotNil(t, m.Evidence)
	assert.Nil(t, m.Conflicts)
	assert.Nil(t, m.OutcomeSignals)
	require.Len(t, m.Gaps, 1)
	assert.Contains(t, m.Gaps[0], "No decisions recorded yet")
}

func TestCompute_HealthyOrg(t *testing.T) {
	ms := &mockStore{
		qualityStats: storage.DecisionQualityStats{
			Total:            100,
			AvgCompleteness:  0.85,
			BelowHalf:        0,
			BelowThird:       0,
			WithReasoning:    90,
			WithAlternatives: 60,
		},
		evidenceStats: storage.EvidenceCoverageStats{
			TotalDecisions:       100,
			WithEvidence:         80,
			WithoutEvidenceCount: 20,
			CoveragePercent:      80.0,
			TotalRecords:         240,
			AvgPerDecision:       2.4,
		},
		conflictCounts: storage.ConflictStatusCounts{
			Total: 10, Open: 1, Acknowledged: 2, Resolved: 6, WontFix: 1,
		},
		outcomeSignals: storage.OutcomeSignalsSummary{
			DecisionsTotal:   100,
			NeverSuperseded:  90,
			RevisedWithin48h: 2,
			NeverCited:       30,
			CitedAtLeastOnce: 70,
		},
	}
	svc := New(ms)

	m, err := svc.Compute(context.Background(), uuid.New())
	require.NoError(t, err)
	assert.Equal(t, "healthy", m.Status)

	// Completeness metrics mapped correctly.
	assert.Equal(t, 100, m.Completeness.TotalDecisions)
	assert.InDelta(t, 0.85, m.Completeness.AvgCompleteness, 0.001)
	assert.Equal(t, 90, m.Completeness.WithReasoning)
	assert.InDelta(t, 90.0, m.Completeness.ReasoningPct, 0.1)
	assert.Equal(t, 60, m.Completeness.WithAlternatives)
	assert.InDelta(t, 60.0, m.Completeness.AlternativesPct, 0.1)

	// Evidence metrics mapped correctly.
	assert.Equal(t, 100, m.Evidence.TotalDecisions)
	assert.Equal(t, 240, m.Evidence.TotalRecords)
	assert.InDelta(t, 2.4, m.Evidence.AvgPerDecision, 0.001)
	assert.Equal(t, 80, m.Evidence.WithEvidence)
	assert.Equal(t, 20, m.Evidence.WithoutEvidence)
	assert.InDelta(t, 80.0, m.Evidence.CoveragePct, 0.1)

	// Conflicts populated when total > 0.
	require.NotNil(t, m.Conflicts)
	assert.Equal(t, 10, m.Conflicts.Total)
	assert.Equal(t, 1, m.Conflicts.Open)
	assert.Equal(t, 2, m.Conflicts.Acknowledged)
	assert.Equal(t, 6, m.Conflicts.Resolved)
	assert.Equal(t, 1, m.Conflicts.WontFix)
	assert.InDelta(t, 60.0, m.Conflicts.ResolvedPct, 0.1)

	// Outcome signals populated when total > 0.
	require.NotNil(t, m.OutcomeSignals)
	assert.Equal(t, 100, m.OutcomeSignals.DecisionsTotal)
}

func TestCompute_NoConflictsOmitsConflictMetrics(t *testing.T) {
	ms := &mockStore{
		qualityStats: storage.DecisionQualityStats{
			Total: 10, AvgCompleteness: 0.9, WithReasoning: 10,
		},
		evidenceStats:  storage.EvidenceCoverageStats{TotalDecisions: 10},
		conflictCounts: storage.ConflictStatusCounts{Total: 0},
		outcomeSignals: storage.OutcomeSignalsSummary{DecisionsTotal: 10},
	}
	svc := New(ms)

	m, err := svc.Compute(context.Background(), uuid.New())
	require.NoError(t, err)
	assert.Nil(t, m.Conflicts, "conflicts should be nil when total == 0")
}

func TestCompute_QualityStatsError(t *testing.T) {
	ms := &mockStore{
		qualityStatsErr: errors.New("db timeout"),
	}
	svc := New(ms)

	_, err := svc.Compute(context.Background(), uuid.New())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "quality stats")
	assert.Contains(t, err.Error(), "db timeout")
}

func TestCompute_EvidenceStatsError(t *testing.T) {
	ms := &mockStore{
		qualityStats:     storage.DecisionQualityStats{Total: 5, AvgCompleteness: 0.5},
		evidenceStatsErr: errors.New("connection refused"),
	}
	svc := New(ms)

	_, err := svc.Compute(context.Background(), uuid.New())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "evidence stats")
}

func TestCompute_ConflictCountsError(t *testing.T) {
	ms := &mockStore{
		qualityStats:  storage.DecisionQualityStats{Total: 5, AvgCompleteness: 0.5},
		evidenceStats: storage.EvidenceCoverageStats{TotalDecisions: 5},
		conflictErr:   errors.New("table missing"),
	}
	svc := New(ms)

	_, err := svc.Compute(context.Background(), uuid.New())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "conflict status counts")
}

func TestCompute_OutcomeSignalsError(t *testing.T) {
	ms := &mockStore{
		qualityStats:   storage.DecisionQualityStats{Total: 5, AvgCompleteness: 0.5},
		evidenceStats:  storage.EvidenceCoverageStats{TotalDecisions: 5},
		conflictCounts: storage.ConflictStatusCounts{},
		outcomeErr:     errors.New("query failed"),
	}
	svc := New(ms)

	_, err := svc.Compute(context.Background(), uuid.New())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "outcome signals")
}

func TestComputeGaps_OutcomeSignals_RevisedWithin48h(t *testing.T) {
	qs := storage.DecisionQualityStats{
		Total: 100, AvgCompleteness: 0.9,
	}
	os := storage.OutcomeSignalsSummary{
		DecisionsTotal:   100,
		RevisedWithin48h: 15, // 15% > 10% threshold
	}
	gaps := computeGaps(qs, 0, 0, os)

	found := false
	for _, g := range gaps {
		if assert.ObjectsAreEqual("15 decisions (15%) were revised within 48 hours.", g) {
			found = true
		}
	}
	assert.True(t, found, "expected revised-within-48h gap, got: %v", gaps)
}

func TestComputeGaps_OutcomeSignals_NeverCited(t *testing.T) {
	qs := storage.DecisionQualityStats{
		Total: 100, AvgCompleteness: 0.9,
	}
	os := storage.OutcomeSignalsSummary{
		DecisionsTotal: 100,
		NeverCited:     80, // 80% > 70% threshold
	}
	gaps := computeGaps(qs, 0, 0, os)

	found := false
	for _, g := range gaps {
		if assert.ObjectsAreEqual("80 decisions (80%) have never been cited as a precedent. Set precedent_ref when tracing to build the attribution graph.", g) {
			found = true
		}
	}
	assert.True(t, found, "expected never-cited gap, got: %v", gaps)
}

func TestComputeGaps_OutcomeSignals_BelowThresholds(t *testing.T) {
	qs := storage.DecisionQualityStats{
		Total: 100, AvgCompleteness: 0.9,
	}
	os := storage.OutcomeSignalsSummary{
		DecisionsTotal:   100,
		RevisedWithin48h: 5,  // 5% <= 10% threshold
		NeverCited:       60, // 60% <= 70% threshold
	}
	gaps := computeGaps(qs, 0, 0, os)

	for _, g := range gaps {
		assert.NotContains(t, g, "revised within 48 hours")
		assert.NotContains(t, g, "never been cited")
	}
}
