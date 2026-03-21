package tracehealth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashita-ai/akashi/internal/storage"
)

// mockStore embeds storage.Store so we only need to override the methods
// used by tracehealth.Service.Compute. Calls to any other method will panic,
// which is acceptable for these unit tests.
type mockStore struct {
	storage.Store

	qualityStats           storage.DecisionQualityStats
	qualityStatsErr        error
	evidenceStats          storage.EvidenceCoverageStats
	evidenceStatsErr       error
	conflictCounts         storage.ConflictStatusCounts
	conflictErr            error
	outcomeSignals         storage.OutcomeSignalsSummary
	outcomeErr             error
	confidenceDist         storage.ConfidenceDistribution
	confidenceErr          error
	highConfOutcomeSignals storage.HighConfOutcomeSignals
	highConfOutcomeErr     error
	typeDist               []storage.DecisionTypeCount
	typeDistErr            error
}

func (m *mockStore) GetDecisionQualityStats(_ context.Context, _ uuid.UUID, _, _ *time.Time) (storage.DecisionQualityStats, error) {
	return m.qualityStats, m.qualityStatsErr
}

func (m *mockStore) GetEvidenceCoverageStats(_ context.Context, _ uuid.UUID, _, _ *time.Time) (storage.EvidenceCoverageStats, error) {
	return m.evidenceStats, m.evidenceStatsErr
}

func (m *mockStore) GetConflictStatusCounts(_ context.Context, _ uuid.UUID, _, _ *time.Time) (storage.ConflictStatusCounts, error) {
	return m.conflictCounts, m.conflictErr
}

func (m *mockStore) GetOutcomeSignalsSummary(_ context.Context, _ uuid.UUID, _, _ *time.Time) (storage.OutcomeSignalsSummary, error) {
	return m.outcomeSignals, m.outcomeErr
}

func (m *mockStore) GetConfidenceDistribution(_ context.Context, _ uuid.UUID, _, _ *time.Time) (storage.ConfidenceDistribution, error) {
	return m.confidenceDist, m.confidenceErr
}

func (m *mockStore) GetHighConfOutcomeSignals(_ context.Context, _ uuid.UUID, _, _ *time.Time) (storage.HighConfOutcomeSignals, error) {
	return m.highConfOutcomeSignals, m.highConfOutcomeErr
}

func (m *mockStore) GetDecisionTypeDistribution(_ context.Context, _ uuid.UUID) ([]storage.DecisionTypeCount, error) {
	return m.typeDist, m.typeDistErr
}

func TestComputeGaps_AllHealthy(t *testing.T) {
	qs := storage.DecisionQualityStats{
		Total: 100, AvgCompleteness: 0.8, BelowHalf: 2, BelowThird: 0, WithReasoning: 95,
	}
	gaps := computeGaps(qs, 5, 0, storage.OutcomeSignalsSummary{}, storage.ConfidenceDistribution{}, storage.HighConfOutcomeSignals{})

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
	gaps := computeGaps(qs, 0, 0, storage.OutcomeSignalsSummary{}, storage.ConfidenceDistribution{}, storage.HighConfOutcomeSignals{})

	assert.GreaterOrEqual(t, len(gaps), 1)
	assert.Contains(t, gaps[0], "Average completeness score")
}

func TestComputeGaps_UnresolvedConflicts(t *testing.T) {
	qs := storage.DecisionQualityStats{
		Total: 100, AvgCompleteness: 0.7, BelowHalf: 5, BelowThird: 0, WithReasoning: 90,
	}
	gaps := computeGaps(qs, 10, 7, storage.OutcomeSignalsSummary{}, storage.ConfidenceDistribution{}, storage.HighConfOutcomeSignals{})

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
	gaps := computeGaps(qs, 0, 0, storage.OutcomeSignalsSummary{}, storage.ConfidenceDistribution{}, storage.HighConfOutcomeSignals{})

	for _, g := range gaps {
		assert.NotContains(t, g, "evidence")
	}
}

func TestComputeGaps_MaxThree(t *testing.T) {
	qs := storage.DecisionQualityStats{
		Total: 100, AvgCompleteness: 0.1, BelowHalf: 80, BelowThird: 60, WithReasoning: 10,
	}
	gaps := computeGaps(qs, 20, 15, storage.OutcomeSignalsSummary{}, storage.ConfidenceDistribution{}, storage.HighConfOutcomeSignals{})

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

	m, err := svc.Compute(context.Background(), uuid.New(), nil, nil)
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
		typeDist: []storage.DecisionTypeCount{
			{DecisionType: "architecture", Count: 50},
			{DecisionType: "trade_off", Count: 30},
			{DecisionType: "security", Count: 20},
		},
	}
	svc := New(ms)

	m, err := svc.Compute(context.Background(), uuid.New(), nil, nil)
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

	// Confidence distribution populated when total > 0.
	require.NotNil(t, m.ConfidenceDistribution)

	// Decision type distribution populated.
	require.Len(t, m.DecisionTypeDistribution, 3)
	assert.Equal(t, "architecture", m.DecisionTypeDistribution[0].DecisionType)
	assert.Equal(t, 50, m.DecisionTypeDistribution[0].Count)
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

	m, err := svc.Compute(context.Background(), uuid.New(), nil, nil)
	require.NoError(t, err)
	assert.Nil(t, m.Conflicts, "conflicts should be nil when total == 0")
}

func TestCompute_QualityStatsError(t *testing.T) {
	ms := &mockStore{
		qualityStatsErr: errors.New("db timeout"),
	}
	svc := New(ms)

	_, err := svc.Compute(context.Background(), uuid.New(), nil, nil)
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

	_, err := svc.Compute(context.Background(), uuid.New(), nil, nil)
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

	_, err := svc.Compute(context.Background(), uuid.New(), nil, nil)
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

	_, err := svc.Compute(context.Background(), uuid.New(), nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "outcome signals")
}

func TestCompute_ConfidenceDistributionError(t *testing.T) {
	ms := &mockStore{
		qualityStats:   storage.DecisionQualityStats{Total: 5, AvgCompleteness: 0.5},
		evidenceStats:  storage.EvidenceCoverageStats{TotalDecisions: 5},
		conflictCounts: storage.ConflictStatusCounts{},
		outcomeSignals: storage.OutcomeSignalsSummary{DecisionsTotal: 5},
		confidenceErr:  errors.New("connection reset"),
	}
	svc := New(ms)

	_, err := svc.Compute(context.Background(), uuid.New(), nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "confidence distribution")
}

func TestCompute_DecisionTypeDistributionError(t *testing.T) {
	ms := &mockStore{
		qualityStats:   storage.DecisionQualityStats{Total: 5, AvgCompleteness: 0.5},
		evidenceStats:  storage.EvidenceCoverageStats{TotalDecisions: 5},
		conflictCounts: storage.ConflictStatusCounts{},
		outcomeSignals: storage.OutcomeSignalsSummary{DecisionsTotal: 5},
		typeDistErr:    errors.New("table missing"),
	}
	svc := New(ms)

	_, err := svc.Compute(context.Background(), uuid.New(), nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decision type distribution")
}

func TestComputeGaps_OutcomeSignals_RevisedWithin48h(t *testing.T) {
	qs := storage.DecisionQualityStats{
		Total: 100, AvgCompleteness: 0.9,
	}
	os := storage.OutcomeSignalsSummary{
		DecisionsTotal:   100,
		RevisedWithin48h: 15, // 15% > 10% threshold
	}
	gaps := computeGaps(qs, 0, 0, os, storage.ConfidenceDistribution{}, storage.HighConfOutcomeSignals{})

	found := false
	for _, g := range gaps {
		if g == "15 decisions (15%) were revised within 48 hours." {
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
	gaps := computeGaps(qs, 0, 0, os, storage.ConfidenceDistribution{}, storage.HighConfOutcomeSignals{})

	found := false
	for _, g := range gaps {
		if g == "80 decisions (80%) have never been cited as a precedent. Set precedent_ref when tracing to build the attribution graph." {
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
	gaps := computeGaps(qs, 0, 0, os, storage.ConfidenceDistribution{}, storage.HighConfOutcomeSignals{})

	for _, g := range gaps {
		assert.NotContains(t, g, "revised within 48 hours")
		assert.NotContains(t, g, "never been cited")
	}
}

// Unsupported high avg confidence triggers the gap.
func TestComputeGaps_HighAvgConfidence_LowCompleteness(t *testing.T) {
	qs := storage.DecisionQualityStats{
		Total: 100, AvgCompleteness: 0.9,
	}
	cd := storage.ConfidenceDistribution{
		TotalDecisions:          100,
		AvgConfidence:           0.89,
		OverconfidentPct:        75,
		HighConfAvgCompleteness: 0.30, // unsupported
	}
	gaps := computeGaps(qs, 0, 0, storage.OutcomeSignalsSummary{}, cd, storage.HighConfOutcomeSignals{})

	found := false
	for _, g := range gaps {
		if g == "Avg confidence is 0.89 but high-confidence decisions average only 30% completeness. Add reasoning, alternatives, or evidence to support high confidence scores." {
			found = true
		}
	}
	assert.True(t, found, "expected confidence calibration gap, got: %v", gaps)
}

// High avg confidence with strong completeness does NOT trigger a gap.
func TestComputeGaps_HighAvgConfidence_HighCompleteness(t *testing.T) {
	qs := storage.DecisionQualityStats{
		Total: 100, AvgCompleteness: 0.9,
	}
	cd := storage.ConfidenceDistribution{
		TotalDecisions:          100,
		AvgConfidence:           0.89,
		OverconfidentPct:        75,
		HighConfAvgCompleteness: 0.72, // well-supported — earned confidence
	}
	gaps := computeGaps(qs, 0, 0, storage.OutcomeSignalsSummary{}, cd, storage.HighConfOutcomeSignals{})

	for _, g := range gaps {
		assert.NotContains(t, g, "confidence", "earned high confidence should not trigger a gap")
	}
}

// Unsupported high overconfident pct triggers the gap when avg is in range.
func TestComputeGaps_HighOverconfidentPct_LowCompleteness(t *testing.T) {
	qs := storage.DecisionQualityStats{
		Total: 100, AvgCompleteness: 0.9,
	}
	cd := storage.ConfidenceDistribution{
		TotalDecisions:          100,
		AvgConfidence:           0.75,
		OverconfidentPct:        65,
		HighConfAvgCompleteness: 0.25, // unsupported
	}
	gaps := computeGaps(qs, 0, 0, storage.OutcomeSignalsSummary{}, cd, storage.HighConfOutcomeSignals{})

	found := false
	for _, g := range gaps {
		if g == "65% of decisions have confidence >= 0.85 but average only 25% completeness. Add reasoning, alternatives, or evidence to support high confidence scores." {
			found = true
		}
	}
	assert.True(t, found, "expected overconfident pct gap, got: %v", gaps)
}

// Confidence within the healthy range should not trigger a gap.
func TestComputeGaps_ConfidenceWithinRange(t *testing.T) {
	qs := storage.DecisionQualityStats{
		Total: 100, AvgCompleteness: 0.9,
	}
	cd := storage.ConfidenceDistribution{
		TotalDecisions:          100,
		AvgConfidence:           0.70,
		OverconfidentPct:        40,
		HighConfAvgCompleteness: 0.30, // low completeness but confidence is in range — no gap
	}
	gaps := computeGaps(qs, 0, 0, storage.OutcomeSignalsSummary{}, cd, storage.HighConfOutcomeSignals{})

	for _, g := range gaps {
		assert.NotContains(t, g, "confidence")
	}
}

// When both conditions are true, the avg-based message takes priority.
func TestComputeGaps_BothConfidenceTriggersPicksAvg(t *testing.T) {
	qs := storage.DecisionQualityStats{
		Total: 100, AvgCompleteness: 0.9,
	}
	cd := storage.ConfidenceDistribution{
		TotalDecisions:          100,
		AvgConfidence:           0.88,
		OverconfidentPct:        70,
		HighConfAvgCompleteness: 0.20, // unsupported
	}
	gaps := computeGaps(qs, 0, 0, storage.OutcomeSignalsSummary{}, cd, storage.HighConfOutcomeSignals{})

	found := false
	for _, g := range gaps {
		if g == "Avg confidence is 0.88 but high-confidence decisions average only 20% completeness. Add reasoning, alternatives, or evidence to support high confidence scores." {
			found = true
		}
	}
	assert.True(t, found, "expected avg-based message when both triggers fire, got: %v", gaps)

	// Should NOT also contain the pct-based message.
	for _, g := range gaps {
		assert.NotContains(t, g, "% of decisions have confidence")
	}
}

// Exactly at the completeness threshold (0.6) should NOT trigger.
func TestComputeGaps_ConfidenceAtCompletenessThreshold(t *testing.T) {
	qs := storage.DecisionQualityStats{
		Total: 100, AvgCompleteness: 0.9,
	}
	cd := storage.ConfidenceDistribution{
		TotalDecisions:          100,
		AvgConfidence:           0.89,
		OverconfidentPct:        75,
		HighConfAvgCompleteness: 0.60, // exactly at threshold — should not fire
	}
	gaps := computeGaps(qs, 0, 0, storage.OutcomeSignalsSummary{}, cd, storage.HighConfOutcomeSignals{})

	for _, g := range gaps {
		assert.NotContains(t, g, "confidence", "completeness == 0.6 is not < 0.6, so no gap")
	}
}

func TestCompute_HighConfOutcomeSignalsError(t *testing.T) {
	ms := &mockStore{
		qualityStats:       storage.DecisionQualityStats{Total: 5, AvgCompleteness: 0.5},
		evidenceStats:      storage.EvidenceCoverageStats{TotalDecisions: 5},
		conflictCounts:     storage.ConflictStatusCounts{},
		outcomeSignals:     storage.OutcomeSignalsSummary{DecisionsTotal: 5},
		highConfOutcomeErr: errors.New("index missing"),
	}
	svc := New(ms)
	_, err := svc.Compute(context.Background(), uuid.New(), nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "high-conf outcome signals")
}

// --- confidenceCalibrationGap tier tests ---

// Tier 1: low outcome score with enough assessments fires.
func TestConfidenceCalibrationGap_Tier1_LowScore(t *testing.T) {
	hcos := storage.HighConfOutcomeSignals{
		Total: 20, AssessedCount: 10, AvgOutcomeScore: 0.55,
	}
	g := confidenceCalibrationGap(hcos, storage.ConfidenceDistribution{})
	assert.Contains(t, g, "55% correctness")
	assert.Contains(t, g, "miscalibrated")
}

// Tier 1: exactly at threshold (0.70) does NOT fire.
func TestConfidenceCalibrationGap_Tier1_AtThreshold(t *testing.T) {
	hcos := storage.HighConfOutcomeSignals{
		Total: 20, AssessedCount: 5, AvgOutcomeScore: 0.70,
	}
	g := confidenceCalibrationGap(hcos, storage.ConfidenceDistribution{})
	assert.Empty(t, g, "0.70 is not < 0.70, so no gap")
}

// Tier 1: insufficient assessment data (<5) falls through to lower tiers.
func TestConfidenceCalibrationGap_Tier1_InsufficientData(t *testing.T) {
	hcos := storage.HighConfOutcomeSignals{
		Total: 20, AssessedCount: 4, AvgOutcomeScore: 0.50,
	}
	g := confidenceCalibrationGap(hcos, storage.ConfidenceDistribution{})
	// With Total=20 but no revisions or conflicts, falls through all tiers.
	assert.Empty(t, g)
}

// Tier 2a: high revision rate fires.
func TestConfidenceCalibrationGap_Tier2_HighRevisionRate(t *testing.T) {
	hcos := storage.HighConfOutcomeSignals{
		Total: 20, RevisedWithin48h: 6, // 30% > 25%
	}
	g := confidenceCalibrationGap(hcos, storage.ConfidenceDistribution{})
	assert.Contains(t, g, "30% of high-confidence decisions were revised")
}

// Tier 2a: exactly at revision threshold (25%) does NOT fire.
func TestConfidenceCalibrationGap_Tier2_AtRevisionThreshold(t *testing.T) {
	hcos := storage.HighConfOutcomeSignals{
		Total: 4, RevisedWithin48h: 1, // 25% is not > 25%
	}
	g := confidenceCalibrationGap(hcos, storage.ConfidenceDistribution{})
	assert.Empty(t, g, "25%% is not > 25%%, so no gap")
}

// Tier 2b: high conflict loss rate fires.
func TestConfidenceCalibrationGap_Tier2_HighConflictLoss(t *testing.T) {
	hcos := storage.HighConfOutcomeSignals{
		Total: 20, ConflictsLost: 4, // 20% > 15%
	}
	g := confidenceCalibrationGap(hcos, storage.ConfidenceDistribution{})
	assert.Contains(t, g, "20% of high-confidence decisions lost conflicts")
}

// Tier 2b: exactly at conflict loss threshold (15%) does NOT fire.
func TestConfidenceCalibrationGap_Tier2_AtConflictThreshold(t *testing.T) {
	hcos := storage.HighConfOutcomeSignals{
		Total: 20, ConflictsLost: 3, // 15% is not > 15%
	}
	g := confidenceCalibrationGap(hcos, storage.ConfidenceDistribution{})
	assert.Empty(t, g, "15%% is not > 15%%, so no gap")
}

// Tier 1 takes priority over tier 2 when both would fire.
func TestConfidenceCalibrationGap_Tier1PrecedesTier2(t *testing.T) {
	hcos := storage.HighConfOutcomeSignals{
		Total:            20,
		AssessedCount:    10,
		AvgOutcomeScore:  0.50,
		RevisedWithin48h: 10, // 50% revision rate would fire tier 2
	}
	g := confidenceCalibrationGap(hcos, storage.ConfidenceDistribution{})
	assert.Contains(t, g, "correctness from assessments", "tier 1 should fire, not tier 2")
	assert.NotContains(t, g, "revised")
}

// All healthy: no gap returned.
func TestConfidenceCalibrationGap_AllHealthy(t *testing.T) {
	hcos := storage.HighConfOutcomeSignals{
		Total:           20,
		AssessedCount:   10,
		AvgOutcomeScore: 0.85, // well-calibrated
	}
	cd := storage.ConfidenceDistribution{
		TotalDecisions:          100,
		AvgConfidence:           0.75,
		HighConfAvgCompleteness: 0.80,
	}
	g := confidenceCalibrationGap(hcos, cd)
	assert.Empty(t, g)
}

// Tier 3 fires as fallback when hcos has no data (zero-value struct).
func TestConfidenceCalibrationGap_Tier3_Fallback(t *testing.T) {
	hcos := storage.HighConfOutcomeSignals{} // zero value — no behavioral data
	cd := storage.ConfidenceDistribution{
		TotalDecisions:          100,
		AvgConfidence:           0.89,
		HighConfAvgCompleteness: 0.30,
	}
	g := confidenceCalibrationGap(hcos, cd)
	assert.Contains(t, g, "Avg confidence is 0.89")
	assert.Contains(t, g, "30% completeness")
}
