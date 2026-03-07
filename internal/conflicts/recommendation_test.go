package conflicts

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashita-ai/akashi/internal/model"
)

func baseConflict() model.DecisionConflict {
	return model.DecisionConflict{
		ID:            uuid.New(),
		DecisionAID:   uuid.New(),
		DecisionBID:   uuid.New(),
		OrgID:         uuid.New(),
		AgentA:        "agent-alpha",
		AgentB:        "agent-beta",
		DecisionType:  "code_review",
		DecisionTypeA: "code_review",
		DecisionTypeB: "code_review",
		ConfidenceA:   0.50,
		ConfidenceB:   0.50,
		DecidedAtA:    time.Now().Add(-24 * time.Hour),
		DecidedAtB:    time.Now().Add(-24 * time.Hour),
		Status:        "open",
	}
}

func TestRecommend_StrongConfidenceDelta(t *testing.T) {
	c := baseConflict()
	c.ConfidenceA = 0.30
	c.ConfidenceB = 0.95

	rec := Recommend(RecommendationInput{Conflict: c})

	require.NotNil(t, rec, "should produce recommendation for strong confidence delta")
	assert.Equal(t, c.DecisionBID, rec.SuggestedWinner)
	assert.Greater(t, rec.Confidence, 0.10)
	assert.NotEmpty(t, rec.Reasons)
	assert.Contains(t, rec.Reasons[0], "higher confidence")
	assert.Contains(t, rec.Reasons[0], "Decision B")
}

func TestRecommend_StrongConfidenceFavorsA(t *testing.T) {
	c := baseConflict()
	c.ConfidenceA = 0.95
	c.ConfidenceB = 0.30

	rec := Recommend(RecommendationInput{Conflict: c})

	require.NotNil(t, rec)
	assert.Equal(t, c.DecisionAID, rec.SuggestedWinner)
	assert.Contains(t, rec.Reasons[0], "Decision A")
}

func TestRecommend_EqualConfidenceStrongRecency(t *testing.T) {
	c := baseConflict()
	c.ConfidenceA = 0.80
	c.ConfidenceB = 0.80
	c.DecidedAtA = time.Now().Add(-7 * 24 * time.Hour) // 7 days ago
	c.DecidedAtB = time.Now().Add(-1 * time.Hour)      // 1 hour ago

	rec := Recommend(RecommendationInput{Conflict: c})

	require.NotNil(t, rec)
	assert.Equal(t, c.DecisionBID, rec.SuggestedWinner, "more recent decision should be favored")
	assert.NotEmpty(t, rec.Reasons)

	hasRecencyReason := false
	for _, r := range rec.Reasons {
		if assert.ObjectsAreEqual("", "") || len(r) > 0 {
			if containsString(r, "more recent") {
				hasRecencyReason = true
			}
		}
	}
	assert.True(t, hasRecencyReason, "should include recency reason")
}

func TestRecommend_AgentWinRateDominance(t *testing.T) {
	c := baseConflict()
	// Equal confidence and recency.
	c.ConfidenceA = 0.75
	c.ConfidenceB = 0.75
	c.DecidedAtA = time.Now().Add(-12 * time.Hour)
	c.DecidedAtB = time.Now().Add(-12 * time.Hour)

	rec := Recommend(RecommendationInput{
		Conflict:  c,
		WinRateA:  0.90,
		WinRateB:  0.20,
		WinCountA: 10,
		WinCountB: 5,
	})

	require.NotNil(t, rec)
	assert.Equal(t, c.DecisionAID, rec.SuggestedWinner, "agent with higher win rate should be favored")

	hasWinRateReason := false
	for _, r := range rec.Reasons {
		if containsString(r, "win rate") {
			hasWinRateReason = true
		}
	}
	assert.True(t, hasWinRateReason, "should include win rate reason")
}

func TestRecommend_WinRateSkippedWithInsufficientHistory(t *testing.T) {
	c := baseConflict()
	c.ConfidenceA = 0.50
	c.ConfidenceB = 0.50

	rec := Recommend(RecommendationInput{
		Conflict:  c,
		WinRateA:  1.0,
		WinRateB:  0.0,
		WinCountA: 2, // below minimum of 3
		WinCountB: 1, // below minimum of 3
	})

	// Without win rate signal, equal confidence + equal recency = nil recommendation.
	if rec != nil {
		for _, r := range rec.Reasons {
			assert.NotContains(t, r, "win rate", "win rate should be skipped with insufficient history")
		}
	}
}

func TestRecommend_RevisionDepthTiebreaker(t *testing.T) {
	c := baseConflict()
	c.ConfidenceA = 0.75
	c.ConfidenceB = 0.75
	c.DecidedAtA = time.Now().Add(-12 * time.Hour)
	c.DecidedAtB = time.Now().Add(-12 * time.Hour)

	rec := Recommend(RecommendationInput{
		Conflict:       c,
		RevisionDepthA: 0,
		RevisionDepthB: 3,
	})

	require.NotNil(t, rec)
	assert.Equal(t, c.DecisionBID, rec.SuggestedWinner, "decision with revision depth should be favored")

	hasRevisionReason := false
	for _, r := range rec.Reasons {
		if containsString(r, "revision") {
			hasRevisionReason = true
		}
	}
	assert.True(t, hasRevisionReason, "should include revision depth reason")
}

func TestRecommend_ConflictingSignals(t *testing.T) {
	c := baseConflict()
	// A has higher confidence, but B is much more recent and has better win rate.
	c.ConfidenceA = 0.85
	c.ConfidenceB = 0.70
	c.DecidedAtA = time.Now().Add(-14 * 24 * time.Hour) // 2 weeks ago
	c.DecidedAtB = time.Now().Add(-1 * time.Hour)       // 1 hour ago

	rec := Recommend(RecommendationInput{
		Conflict:  c,
		WinRateA:  0.30,
		WinRateB:  0.85,
		WinCountA: 10,
		WinCountB: 10,
	})

	require.NotNil(t, rec)
	// Recency (0.25 * ~1.0 = 0.25 for B) + Win rate (0.25 * 0.55 = 0.14 for B)
	// outweigh Confidence (0.35 * -0.15 = -0.05 for A).
	assert.Equal(t, c.DecisionBID, rec.SuggestedWinner,
		"recency + win rate should outweigh moderate confidence advantage")
}

func TestRecommend_InsufficientSignalReturnsNil(t *testing.T) {
	c := baseConflict()
	// Everything nearly equal.
	c.ConfidenceA = 0.500
	c.ConfidenceB = 0.505 // delta 0.005, below 0.01 threshold
	c.DecidedAtA = time.Now().Add(-12 * time.Hour)
	c.DecidedAtB = time.Now().Add(-12*time.Hour - 30*time.Minute) // ~30min, below 1h threshold

	rec := Recommend(RecommendationInput{Conflict: c})

	assert.Nil(t, rec, "should return nil when signals are too weak")
}

func TestRecommend_ResolvedConflictReturnsNil(t *testing.T) {
	c := baseConflict()
	c.Status = "resolved"
	c.ConfidenceA = 0.30
	c.ConfidenceB = 0.95

	rec := Recommend(RecommendationInput{Conflict: c})

	assert.Nil(t, rec, "should return nil for resolved conflicts")
}

func TestRecommend_WontFixConflictReturnsNil(t *testing.T) {
	c := baseConflict()
	c.Status = "wont_fix"

	rec := Recommend(RecommendationInput{Conflict: c})

	assert.Nil(t, rec, "should return nil for wont_fix conflicts")
}

func TestRecommend_SelfContradictionSkipsWinRate(t *testing.T) {
	c := baseConflict()
	c.AgentA = "agent-same"
	c.AgentB = "agent-same"
	c.ConflictKind = model.ConflictKindSelfContradiction
	c.ConfidenceA = 0.40
	c.ConfidenceB = 0.90

	rec := Recommend(RecommendationInput{
		Conflict:  c,
		WinRateA:  0.10,
		WinRateB:  0.90,
		WinCountA: 10,
		WinCountB: 10,
	})

	require.NotNil(t, rec)
	for _, r := range rec.Reasons {
		assert.NotContains(t, r, "win rate", "win rate should be skipped for self-contradiction")
	}
}

func TestRecommend_ConfidenceCappedAt099(t *testing.T) {
	c := baseConflict()
	c.ConfidenceA = 0.0
	c.ConfidenceB = 1.0

	rec := Recommend(RecommendationInput{
		Conflict:       c,
		WinRateA:       0.0,
		WinRateB:       1.0,
		WinCountA:      10,
		WinCountB:      10,
		RevisionDepthA: 0,
		RevisionDepthB: 5,
	})

	require.NotNil(t, rec)
	assert.LessOrEqual(t, rec.Confidence, 0.99, "confidence should be capped at 0.99")
}

func TestRecommend_AcknowledgedStatusGetsRecommendation(t *testing.T) {
	c := baseConflict()
	c.Status = "acknowledged"
	c.ConfidenceA = 0.30
	c.ConfidenceB = 0.95

	rec := Recommend(RecommendationInput{Conflict: c})

	require.NotNil(t, rec, "acknowledged conflicts should still get recommendations")
}

func TestRecommend_ReasonsOrderedByContribution(t *testing.T) {
	c := baseConflict()
	c.ConfidenceA = 0.20
	c.ConfidenceB = 0.90 // strong confidence signal
	c.DecidedAtA = time.Now().Add(-7 * 24 * time.Hour)
	c.DecidedAtB = time.Now().Add(-1 * time.Hour) // moderate recency signal

	rec := Recommend(RecommendationInput{Conflict: c})

	require.NotNil(t, rec)
	require.GreaterOrEqual(t, len(rec.Reasons), 2, "should have at least two reasons")
	// First reason should be confidence (weight 0.35 * 0.70 delta = 0.245, strongest).
	assert.Contains(t, rec.Reasons[0], "confidence", "strongest signal should be listed first")
}

func TestRecommend_ZeroConfidenceBothSides(t *testing.T) {
	c := baseConflict()
	c.ConfidenceA = 0.0
	c.ConfidenceB = 0.0
	c.DecidedAtA = time.Now().Add(-12 * time.Hour)
	c.DecidedAtB = time.Now().Add(-12 * time.Hour)

	rec := Recommend(RecommendationInput{Conflict: c})

	assert.Nil(t, rec, "should return nil when both have zero confidence and equal timestamps")
}

func TestFormatAge(t *testing.T) {
	tests := []struct {
		name     string
		t        time.Time
		contains string
	}{
		{"just now", time.Now(), "just now"},
		{"minutes ago", time.Now().Add(-30 * time.Minute), "m ago"},
		{"hours ago", time.Now().Add(-5 * time.Hour), "h ago"},
		{"days ago", time.Now().Add(-3 * 24 * time.Hour), "d ago"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatAge(tt.t)
			assert.Contains(t, result, tt.contains)
		})
	}
}

func containsString(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && findSubstring(s, substr))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
