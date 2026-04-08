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
	calibration            storage.ConfidenceCalibration
	calibrationErr         error
	typeDist               []storage.DecisionTypeCount
	typeDistErr            error
	completenessByType     []storage.DecisionTypeCompleteness
	completenessByTypeErr  error
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

func (m *mockStore) GetConfidenceCalibration(_ context.Context, _ uuid.UUID, _, _ *time.Time) (storage.ConfidenceCalibration, error) {
	return m.calibration, m.calibrationErr
}

func (m *mockStore) GetDecisionTypeDistribution(_ context.Context, _ uuid.UUID, _, _ *time.Time) ([]storage.DecisionTypeCount, error) {
	return m.typeDist, m.typeDistErr
}

func (m *mockStore) GetCompletenessByDecisionType(_ context.Context, _ uuid.UUID, _, _ *time.Time) ([]storage.DecisionTypeCompleteness, error) {
	return m.completenessByType, m.completenessByTypeErr
}

// emptyCal is a convenience zero-value calibration for tests that don't care about it.
var emptyCal = storage.ConfidenceCalibration{}

func TestComputeGaps_AllHealthy(t *testing.T) {
	qs := storage.DecisionQualityStats{
		Total: 100, AvgCompleteness: 0.8, BelowHalf: 2, BelowThird: 0, WithReasoning: 95,
	}
	gaps := computeGaps(qs, 5, 0, storage.OutcomeSignalsSummary{}, storage.ConfidenceDistribution{}, emptyCal)

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
	gaps := computeGaps(qs, 0, 0, storage.OutcomeSignalsSummary{}, storage.ConfidenceDistribution{}, emptyCal)

	assert.GreaterOrEqual(t, len(gaps), 1)
	assert.Contains(t, gaps[0], "Average completeness score")
}

func TestComputeGaps_UnresolvedConflicts(t *testing.T) {
	qs := storage.DecisionQualityStats{
		Total: 100, AvgCompleteness: 0.7, BelowHalf: 5, BelowThird: 0, WithReasoning: 90,
	}
	gaps := computeGaps(qs, 10, 7, storage.OutcomeSignalsSummary{}, storage.ConfidenceDistribution{}, emptyCal)

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
	gaps := computeGaps(qs, 0, 0, storage.OutcomeSignalsSummary{}, storage.ConfidenceDistribution{}, emptyCal)

	for _, g := range gaps {
		assert.NotContains(t, g, "evidence")
	}
}

func TestComputeGaps_MaxThree(t *testing.T) {
	qs := storage.DecisionQualityStats{
		Total: 100, AvgCompleteness: 0.1, BelowHalf: 80, BelowThird: 60, WithReasoning: 10,
	}
	gaps := computeGaps(qs, 20, 15, storage.OutcomeSignalsSummary{}, storage.ConfidenceDistribution{}, emptyCal)

	assert.LessOrEqual(t, len(gaps), 3, "should return at most 3 gaps")
}

// When confidence is well-calibrated, no gap is generated.
func TestComputeGaps_CalibratedConfidenceNoGap(t *testing.T) {
	qs := storage.DecisionQualityStats{
		Total: 100, AvgCompleteness: 0.9,
	}
	cd := storage.ConfidenceDistribution{TotalDecisions: 100, AvgConfidence: 0.65, OverconfidentPct: 20}
	gaps := computeGaps(qs, 0, 0, storage.OutcomeSignalsSummary{}, cd, emptyCal)

	for _, g := range gaps {
		assert.NotContains(t, g, "confidence")
	}
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
			Total: 10, Open: 1, Resolved: 6, FalsePositive: 3,
			TotalGroups: 4, OpenGroups: 1,
		},
		outcomeSignals: storage.OutcomeSignalsSummary{
			DecisionsTotal:   100,
			NeverSuperseded:  90,
			RevisedWithin48h: 2,
			NeverCited:       30,
			CitedAtLeastOnce: 70,
		},
		highConfOutcomeSignals: storage.HighConfOutcomeSignals{
			Total:            30,
			RevisedWithin48h: 2,
			ConflictsLost:    1,
			AssessedCount:    8,
			AvgOutcomeScore:  0.85,
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
	assert.Equal(t, 4, m.Conflicts.TotalGroups)
	assert.Equal(t, 1, m.Conflicts.OpenGroups)
	assert.Equal(t, 10, m.Conflicts.TotalIndividual)
	assert.Equal(t, 1, m.Conflicts.OpenIndividual)
	assert.Equal(t, 6, m.Conflicts.Resolved)
	assert.Equal(t, 3, m.Conflicts.FalsePositive)
	assert.InDelta(t, 60.0, m.Conflicts.ResolvedPct, 0.1)

	// Outcome signals populated when total > 0.
	require.NotNil(t, m.OutcomeSignals)
	assert.Equal(t, 100, m.OutcomeSignals.DecisionsTotal)

	// Confidence distribution populated when total > 0.
	require.NotNil(t, m.ConfidenceDistribution)

	// High-confidence outcome signals surfaced as data.
	require.NotNil(t, m.HighConfOutcomeSignals)
	assert.Equal(t, 30, m.HighConfOutcomeSignals.Total)
	assert.Equal(t, 2, m.HighConfOutcomeSignals.RevisedWithin48h)
	assert.Equal(t, 1, m.HighConfOutcomeSignals.ConflictsLost)
	assert.Equal(t, 8, m.HighConfOutcomeSignals.AssessedCount)
	assert.InDelta(t, 0.85, m.HighConfOutcomeSignals.AvgOutcomeScore, 0.001)

	// Confidence calibration populated when total > 0.
	require.NotNil(t, m.ConfidenceCalibration)

	// Decision type distribution populated.
	require.Len(t, m.DecisionTypeDistribution, 3)
	assert.Equal(t, "architecture", m.DecisionTypeDistribution[0].DecisionType)
	assert.Equal(t, 50, m.DecisionTypeDistribution[0].Count)
}

// When no high-confidence decisions exist, the field is omitted.
func TestCompute_NoHighConfOmitsSignals(t *testing.T) {
	ms := &mockStore{
		qualityStats: storage.DecisionQualityStats{
			Total: 10, AvgCompleteness: 0.9, WithReasoning: 10,
		},
		evidenceStats:          storage.EvidenceCoverageStats{TotalDecisions: 10},
		conflictCounts:         storage.ConflictStatusCounts{Total: 0},
		outcomeSignals:         storage.OutcomeSignalsSummary{DecisionsTotal: 10},
		highConfOutcomeSignals: storage.HighConfOutcomeSignals{Total: 0}, // no high-conf decisions
	}
	svc := New(ms)

	m, err := svc.Compute(context.Background(), uuid.New(), nil, nil)
	require.NoError(t, err)
	assert.Nil(t, m.HighConfOutcomeSignals, "should be nil when Total == 0")
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

func TestCompute_ConfidenceCalibrationError(t *testing.T) {
	ms := &mockStore{
		qualityStats:   storage.DecisionQualityStats{Total: 5, AvgCompleteness: 0.5},
		evidenceStats:  storage.EvidenceCoverageStats{TotalDecisions: 5},
		conflictCounts: storage.ConflictStatusCounts{},
		outcomeSignals: storage.OutcomeSignalsSummary{DecisionsTotal: 5},
		calibrationErr: errors.New("query failed"),
	}
	svc := New(ms)

	_, err := svc.Compute(context.Background(), uuid.New(), nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "confidence calibration")
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

// ---------------------------------------------------------------------------
// enrichCompletenessWithExpectations tests
// ---------------------------------------------------------------------------

func TestEnrichCompletenessWithExpectations_HealthyType(t *testing.T) {
	rows := []storage.DecisionTypeCompleteness{
		{DecisionType: "investigation", Count: 20, AvgCompleteness: 0.45},
	}
	enrichCompletenessWithExpectations(rows)

	assert.InDelta(t, 0.30, rows[0].ExpectedMin, 0.001, "investigation expected_min should be 0.30")
	assert.Equal(t, "healthy", rows[0].Status, "0.45 >= 0.30 should be healthy")
}

func TestEnrichCompletenessWithExpectations_NeedsAttention(t *testing.T) {
	rows := []storage.DecisionTypeCompleteness{
		{DecisionType: "security", Count: 10, AvgCompleteness: 0.35},
	}
	enrichCompletenessWithExpectations(rows)

	assert.InDelta(t, 0.60, rows[0].ExpectedMin, 0.001, "security expected_min should be 0.60")
	assert.Equal(t, "needs_attention", rows[0].Status, "0.35 < 0.60 should be needs_attention")
}

func TestEnrichCompletenessWithExpectations_ExactlyAtThreshold(t *testing.T) {
	rows := []storage.DecisionTypeCompleteness{
		{DecisionType: "architecture", Count: 15, AvgCompleteness: 0.55},
	}
	enrichCompletenessWithExpectations(rows)

	assert.Equal(t, "healthy", rows[0].Status, "exactly at expected_min should be healthy")
}

func TestEnrichCompletenessWithExpectations_UnknownType(t *testing.T) {
	rows := []storage.DecisionTypeCompleteness{
		{DecisionType: "custom_workflow", Count: 5, AvgCompleteness: 0.50},
	}
	enrichCompletenessWithExpectations(rows)

	assert.InDelta(t, 0.40, rows[0].ExpectedMin, 0.001, "unknown type should get default 0.40")
	assert.Equal(t, "healthy", rows[0].Status, "0.50 >= 0.40 should be healthy")
}

func TestEnrichCompletenessWithExpectations_MultipleTypes(t *testing.T) {
	rows := []storage.DecisionTypeCompleteness{
		{DecisionType: "investigation", Count: 20, AvgCompleteness: 0.40},
		{DecisionType: "security", Count: 10, AvgCompleteness: 0.35},
		{DecisionType: "architecture", Count: 15, AvgCompleteness: 0.60},
	}
	enrichCompletenessWithExpectations(rows)

	assert.Equal(t, "healthy", rows[0].Status, "investigation 0.40 >= 0.30")
	assert.Equal(t, "needs_attention", rows[1].Status, "security 0.35 < 0.60")
	assert.Equal(t, "healthy", rows[2].Status, "architecture 0.60 >= 0.55")
}

func TestCompute_CompletenessByTypeEnriched(t *testing.T) {
	ms := &mockStore{
		qualityStats: storage.DecisionQualityStats{
			Total: 50, AvgCompleteness: 0.6, WithReasoning: 40,
		},
		evidenceStats:  storage.EvidenceCoverageStats{TotalDecisions: 50},
		conflictCounts: storage.ConflictStatusCounts{},
		outcomeSignals: storage.OutcomeSignalsSummary{DecisionsTotal: 50},
		completenessByType: []storage.DecisionTypeCompleteness{
			{DecisionType: "investigation", Count: 20, AvgCompleteness: 0.40},
			{DecisionType: "security", Count: 10, AvgCompleteness: 0.35},
		},
	}
	svc := New(ms)

	m, err := svc.Compute(context.Background(), uuid.New(), nil, nil)
	require.NoError(t, err)
	require.Len(t, m.CompletenessByType, 2)

	// Verify enrichment was applied.
	assert.Equal(t, "healthy", m.CompletenessByType[0].Status)
	assert.InDelta(t, 0.30, m.CompletenessByType[0].ExpectedMin, 0.001)

	assert.Equal(t, "needs_attention", m.CompletenessByType[1].Status)
	assert.InDelta(t, 0.60, m.CompletenessByType[1].ExpectedMin, 0.001)
}

func TestComputeGaps_OutcomeSignals_RevisedWithin48h(t *testing.T) {
	qs := storage.DecisionQualityStats{
		Total: 100, AvgCompleteness: 0.9,
	}
	os := storage.OutcomeSignalsSummary{
		DecisionsTotal:   100,
		RevisedWithin48h: 15, // 15% > 10% threshold
	}
	gaps := computeGaps(qs, 0, 0, os, storage.ConfidenceDistribution{}, emptyCal)

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
	gaps := computeGaps(qs, 0, 0, os, storage.ConfidenceDistribution{}, emptyCal)

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
	gaps := computeGaps(qs, 0, 0, os, storage.ConfidenceDistribution{}, emptyCal)

	for _, g := range gaps {
		assert.NotContains(t, g, "revised within 48 hours")
		assert.NotContains(t, g, "never been cited")
	}
}

// ---------------------------------------------------------------------------
// Confidence calibration gap tests — three tiers of evidence
// ---------------------------------------------------------------------------

func ptrFloat(f float64) *float64 { return &f }

// Tier 1: Assessment-based calibration detects miscalibration.
// When high-confidence decisions have lower outcome scores than mid-range,
// the gap message should cite the actual outcome scores.
func TestConfidenceCalibrationGap_OutcomeBased_Miscalibrated(t *testing.T) {
	cal := storage.ConfidenceCalibration{
		HasOutcomeData: true,
		Tiers: []storage.ConfidenceTier{
			{Tier: "high", Total: 40, AssessedCount: 10, AvgOutcome: ptrFloat(0.55), RevisionRate: 5},
			{Tier: "mid", Total: 50, AssessedCount: 15, AvgOutcome: ptrFloat(0.72), RevisionRate: 3},
			{Tier: "low", Total: 10, AssessedCount: 3, AvgOutcome: ptrFloat(0.60), RevisionRate: 10},
		},
	}
	g := confidenceCalibrationGap(cal, storage.ConfidenceDistribution{})

	assert.Contains(t, g, "avg outcome score 0.55")
	assert.Contains(t, g, "0.72 for mid-range")
	assert.Contains(t, g, "confidence is not predicting outcomes")
}

// Tier 1: Assessment-based calibration passes when high >= mid.
func TestConfidenceCalibrationGap_OutcomeBased_Calibrated(t *testing.T) {
	cal := storage.ConfidenceCalibration{
		HasOutcomeData: true,
		Tiers: []storage.ConfidenceTier{
			{Tier: "high", Total: 40, AssessedCount: 10, AvgOutcome: ptrFloat(0.80), RevisionRate: 5},
			{Tier: "mid", Total: 50, AssessedCount: 15, AvgOutcome: ptrFloat(0.72), RevisionRate: 3},
		},
	}
	g := confidenceCalibrationGap(cal, storage.ConfidenceDistribution{})

	assert.Empty(t, g, "calibrated by outcome data — no gap expected")
}

// Tier 1: Requires minimum 3 assessed decisions per tier to avoid noise.
func TestConfidenceCalibrationGap_OutcomeBased_InsufficientAssessments(t *testing.T) {
	cal := storage.ConfidenceCalibration{
		HasOutcomeData: true,
		Tiers: []storage.ConfidenceTier{
			{Tier: "high", Total: 40, AssessedCount: 2, AvgOutcome: ptrFloat(0.30), RevisionRate: 20},
			{Tier: "mid", Total: 50, AssessedCount: 15, AvgOutcome: ptrFloat(0.72), RevisionRate: 3},
		},
	}
	// Should fall through to tier 2 (revision rate) because high has < 3 assessments.
	g := confidenceCalibrationGap(cal, storage.ConfidenceDistribution{})

	// With 20% vs 3% revision rate and enough samples, tier 2 should trigger.
	assert.Contains(t, g, "revised within 48h")
}

// Tier 2: Revision-rate proxy detects miscalibration without assessments.
func TestConfidenceCalibrationGap_RevisionRate_Miscalibrated(t *testing.T) {
	cal := storage.ConfidenceCalibration{
		HasOutcomeData: false,
		Tiers: []storage.ConfidenceTier{
			{Tier: "high", Total: 30, RevisionRate: 18},
			{Tier: "mid", Total: 50, RevisionRate: 4},
			{Tier: "low", Total: 20, RevisionRate: 8},
		},
	}
	g := confidenceCalibrationGap(cal, storage.ConfidenceDistribution{})

	assert.Contains(t, g, "revised within 48h at 18%")
	assert.Contains(t, g, "4% for mid-range")
	assert.Contains(t, g, "over-committing")
}

// Tier 2: Revision rate passes when high <= mid.
func TestConfidenceCalibrationGap_RevisionRate_Calibrated(t *testing.T) {
	cal := storage.ConfidenceCalibration{
		HasOutcomeData: false,
		Tiers: []storage.ConfidenceTier{
			{Tier: "high", Total: 30, RevisionRate: 3},
			{Tier: "mid", Total: 50, RevisionRate: 5},
		},
	}
	g := confidenceCalibrationGap(cal, storage.ConfidenceDistribution{})

	assert.Empty(t, g, "calibrated by revision rate — no gap expected")
}

// Tier 2: Requires >= 5 decisions per tier to avoid noise.
func TestConfidenceCalibrationGap_RevisionRate_InsufficientData(t *testing.T) {
	cal := storage.ConfidenceCalibration{
		HasOutcomeData: false,
		Tiers: []storage.ConfidenceTier{
			{Tier: "high", Total: 3, RevisionRate: 33}, // too few decisions
			{Tier: "mid", Total: 50, RevisionRate: 2},
		},
	}
	// Should fall through to tier 3 (distribution shape).
	g := confidenceCalibrationGap(cal, storage.ConfidenceDistribution{TotalDecisions: 53, AvgConfidence: 0.85, OverconfidentPct: 70})

	assert.Contains(t, g, "Avg confidence is 0.85")
}

// Tier 2: High revision rate must exceed 5% absolute to trigger.
// This prevents noise when both tiers have low revision rates (e.g. 2% vs 1%).
func TestConfidenceCalibrationGap_RevisionRate_BelowAbsoluteThreshold(t *testing.T) {
	cal := storage.ConfidenceCalibration{
		HasOutcomeData: false,
		Tiers: []storage.ConfidenceTier{
			{Tier: "high", Total: 30, RevisionRate: 4}, // higher than mid but below 5% absolute
			{Tier: "mid", Total: 50, RevisionRate: 2},
		},
	}
	g := confidenceCalibrationGap(cal, storage.ConfidenceDistribution{})

	assert.Empty(t, g, "revision rates too low overall to be meaningful")
}

// Tier 3: Distribution shape fallback — high avg confidence.
func TestConfidenceCalibrationGap_Fallback_HighAvg(t *testing.T) {
	cal := emptyCal // no tiers at all
	cd := storage.ConfidenceDistribution{
		TotalDecisions:   100,
		AvgConfidence:    0.89,
		OverconfidentPct: 50,
	}
	g := confidenceCalibrationGap(cal, cd)

	assert.Contains(t, g, "Avg confidence is 0.89")
	assert.Contains(t, g, "above the recommended 0.4")
}

// Tier 3: Distribution shape fallback — high overconfident percentage.
func TestConfidenceCalibrationGap_Fallback_HighPct(t *testing.T) {
	cal := emptyCal
	cd := storage.ConfidenceDistribution{
		TotalDecisions:   100,
		AvgConfidence:    0.78,
		OverconfidentPct: 65,
	}
	g := confidenceCalibrationGap(cal, cd)

	assert.Contains(t, g, "65% of decisions have confidence >= 0.85")
}

// Tier 3: Below both thresholds — no gap.
func TestConfidenceCalibrationGap_Fallback_BelowThresholds(t *testing.T) {
	cal := emptyCal
	cd := storage.ConfidenceDistribution{
		TotalDecisions:   100,
		AvgConfidence:    0.72,
		OverconfidentPct: 40,
	}
	g := confidenceCalibrationGap(cal, cd)

	assert.Empty(t, g)
}

// Tier 3: Exact boundary values — strict inequality (> not >=).
func TestConfidenceCalibrationGap_Fallback_ExactThresholds(t *testing.T) {
	cal := emptyCal
	cd := storage.ConfidenceDistribution{
		TotalDecisions:   100,
		AvgConfidence:    0.82, // exactly at threshold — should NOT trigger
		OverconfidentPct: 60,   // exactly at threshold — should NOT trigger
	}
	g := confidenceCalibrationGap(cal, cd)

	assert.Empty(t, g, "exactly-at-threshold should not trigger")
}

// Zero confidence distribution decisions — no gap.
func TestConfidenceCalibrationGap_Fallback_ZeroDecisions(t *testing.T) {
	cal := emptyCal
	cd := storage.ConfidenceDistribution{
		TotalDecisions: 0,
		AvgConfidence:  0.95,
	}
	g := confidenceCalibrationGap(cal, cd)

	assert.Empty(t, g, "should not trigger with zero decisions")
}

// Tier priority: outcome data takes precedence over revision rate,
// even if revision rate would trigger.
func TestConfidenceCalibrationGap_OutcomeTakesPrecedenceOverRevision(t *testing.T) {
	cal := storage.ConfidenceCalibration{
		HasOutcomeData: true,
		Tiers: []storage.ConfidenceTier{
			{Tier: "high", Total: 30, AssessedCount: 5, AvgOutcome: ptrFloat(0.80), RevisionRate: 25},
			{Tier: "mid", Total: 50, AssessedCount: 10, AvgOutcome: ptrFloat(0.70), RevisionRate: 2},
		},
	}
	g := confidenceCalibrationGap(cal, storage.ConfidenceDistribution{})

	// Outcome says calibrated (0.80 >= 0.70), even though revision rate is terrible.
	// Outcome data is ground truth and should win.
	assert.Empty(t, g)
}

// Missing "mid" tier — not enough data to compare, should not flag.
func TestConfidenceCalibrationGap_MissingMidTier(t *testing.T) {
	cal := storage.ConfidenceCalibration{
		HasOutcomeData: false,
		Tiers: []storage.ConfidenceTier{
			{Tier: "high", Total: 30, RevisionRate: 25},
			{Tier: "low", Total: 50, RevisionRate: 2},
		},
	}
	g := confidenceCalibrationGap(cal, storage.ConfidenceDistribution{})

	assert.Empty(t, g, "cannot calibrate without mid tier")
}
