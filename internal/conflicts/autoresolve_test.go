package conflicts

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashita-ai/akashi/internal/model"
)

func TestClassifyTier(t *testing.T) {
	t.Run("P0: previously resolved conflict", func(t *testing.T) {
		resolvedBy := "human"
		c := model.DecisionConflict{
			ResolvedBy: &resolvedBy,
			Status:     "open",
		}
		assert.Equal(t, TierP0, ClassifyTier(c))
	})

	t.Run("P1: high confidence both sides + critical severity", func(t *testing.T) {
		sev := "critical"
		c := model.DecisionConflict{
			ConfidenceA: 0.85,
			ConfidenceB: 0.90,
			Severity:    &sev,
			Status:      "open",
		}
		assert.Equal(t, TierP1, ClassifyTier(c))
	})

	t.Run("P1: high confidence both sides + high severity", func(t *testing.T) {
		sev := "high"
		c := model.DecisionConflict{
			ConfidenceA: 0.81,
			ConfidenceB: 0.95,
			Severity:    &sev,
			Status:      "open",
		}
		assert.Equal(t, TierP1, ClassifyTier(c))
	})

	t.Run("P3: low significance", func(t *testing.T) {
		sig := 0.20
		c := model.DecisionConflict{
			Significance: &sig,
			Status:       "open",
		}
		assert.Equal(t, TierP3, ClassifyTier(c))
	})

	t.Run("P3: supersession relationship", func(t *testing.T) {
		rel := "supersession"
		sig := 0.50
		c := model.DecisionConflict{
			Relationship: &rel,
			Significance: &sig,
			Status:       "open",
		}
		assert.Equal(t, TierP3, ClassifyTier(c))
	})

	t.Run("P2: standard conflict", func(t *testing.T) {
		sig := 0.50
		sev := "medium"
		c := model.DecisionConflict{
			ConfidenceA:  0.70,
			ConfidenceB:  0.60,
			Significance: &sig,
			Severity:     &sev,
			Status:       "open",
		}
		assert.Equal(t, TierP2, ClassifyTier(c))
	})

	t.Run("P2: high confidence but only medium severity", func(t *testing.T) {
		sig := 0.50
		sev := "medium"
		c := model.DecisionConflict{
			ConfidenceA:  0.90,
			ConfidenceB:  0.85,
			Significance: &sig,
			Severity:     &sev,
			Status:       "open",
		}
		assert.Equal(t, TierP2, ClassifyTier(c))
	})
}

func TestShouldAutoResolve(t *testing.T) {
	policy := model.ConflictResolutionPolicy{
		AutoResolveAfterDays:       14,
		AutoResolveWinner:          model.WinnerRecency,
		AutoResolveMaxSeverity:     "medium",
		NeverAutoResolveSeverities: []string{"critical"},
		ReopenedResolutionPolicy:   model.ReopenEscalate,
	}

	t.Run("P0 never resolves", func(t *testing.T) {
		resolvedBy := "human"
		c := model.DecisionConflict{ResolvedBy: &resolvedBy, Status: "open"}
		assert.False(t, ShouldAutoResolve(c, policy))
	})

	t.Run("P1 never resolves", func(t *testing.T) {
		sev := "high"
		c := model.DecisionConflict{
			ConfidenceA: 0.85,
			ConfidenceB: 0.90,
			Severity:    &sev,
			Status:      "open",
		}
		assert.False(t, ShouldAutoResolve(c, policy))
	})

	t.Run("P2 resolves when severity within max", func(t *testing.T) {
		sig := 0.50
		sev := "low"
		c := model.DecisionConflict{
			ConfidenceA:  0.5,
			ConfidenceB:  0.5,
			Significance: &sig,
			Severity:     &sev,
			Status:       "open",
		}
		assert.True(t, ShouldAutoResolve(c, policy))
	})

	t.Run("P2 blocked when severity exceeds max", func(t *testing.T) {
		sig := 0.50
		sev := "high"
		c := model.DecisionConflict{
			ConfidenceA:  0.5,
			ConfidenceB:  0.5,
			Significance: &sig,
			Severity:     &sev,
			Status:       "open",
		}
		assert.False(t, ShouldAutoResolve(c, policy))
	})

	t.Run("P2 blocked by never-resolve severity", func(t *testing.T) {
		sig := 0.50
		sev := "critical"
		// Override policy to allow critical max but never resolve it.
		p := policy
		p.AutoResolveMaxSeverity = "critical"
		c := model.DecisionConflict{
			ConfidenceA:  0.5,
			ConfidenceB:  0.5,
			Significance: &sig,
			Severity:     &sev,
			Status:       "open",
		}
		assert.False(t, ShouldAutoResolve(c, p))
	})

	t.Run("nil severity allows resolution", func(t *testing.T) {
		sig := 0.50
		c := model.DecisionConflict{
			ConfidenceA:  0.5,
			ConfidenceB:  0.5,
			Significance: &sig,
			Status:       "open",
		}
		assert.True(t, ShouldAutoResolve(c, policy))
	})
}

func TestDetermineWinner(t *testing.T) {
	idA := uuid.New()
	idB := uuid.New()
	now := time.Now()

	base := model.DecisionConflict{
		DecisionAID: idA,
		DecisionBID: idB,
		ConfidenceA: 0.60,
		ConfidenceB: 0.80,
		DecidedAtA:  now.Add(-48 * time.Hour),
		DecidedAtB:  now.Add(-1 * time.Hour),
	}

	t.Run("recency picks more recent", func(t *testing.T) {
		policy := model.ConflictResolutionPolicy{AutoResolveWinner: model.WinnerRecency}
		winner := DetermineWinner(base, policy, nil)
		require.NotNil(t, winner)
		assert.Equal(t, idB, *winner, "Decision B is more recent")
	})

	t.Run("confidence picks higher confidence", func(t *testing.T) {
		policy := model.ConflictResolutionPolicy{AutoResolveWinner: model.WinnerConfidence}
		winner := DetermineWinner(base, policy, nil)
		require.NotNil(t, winner)
		assert.Equal(t, idB, *winner, "Decision B has higher confidence")
	})

	t.Run("confidence tie falls back to recommendation", func(t *testing.T) {
		policy := model.ConflictResolutionPolicy{AutoResolveWinner: model.WinnerConfidence}
		tied := base
		tied.ConfidenceA = 0.80
		tied.ConfidenceB = 0.80
		rec := &model.Recommendation{SuggestedWinner: idA}
		winner := DetermineWinner(tied, policy, rec)
		require.NotNil(t, winner)
		assert.Equal(t, idA, *winner, "falls back to recommendation")
	})

	t.Run("consensus uses recommendation", func(t *testing.T) {
		policy := model.ConflictResolutionPolicy{AutoResolveWinner: model.WinnerConsensus}
		rec := &model.Recommendation{SuggestedWinner: idA}
		winner := DetermineWinner(base, policy, rec)
		require.NotNil(t, winner)
		assert.Equal(t, idA, *winner)
	})

	t.Run("consensus without recommendation falls back to recency", func(t *testing.T) {
		policy := model.ConflictResolutionPolicy{AutoResolveWinner: model.WinnerConsensus}
		winner := DetermineWinner(base, policy, nil)
		require.NotNil(t, winner)
		assert.Equal(t, idB, *winner)
	})
}

func TestTierString(t *testing.T) {
	assert.Equal(t, "P0", TierP0.String())
	assert.Equal(t, "P1", TierP1.String())
	assert.Equal(t, "P2", TierP2.String())
	assert.Equal(t, "P3", TierP3.String())
}
