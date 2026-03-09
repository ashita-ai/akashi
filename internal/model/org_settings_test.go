package model

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestConflictResolutionPolicy_Validate(t *testing.T) {
	valid := ConflictResolutionPolicy{
		AutoResolveAfterDays:       14,
		AutoResolveWinner:          WinnerRecency,
		AutoResolveMaxSeverity:     "medium",
		NeverAutoResolveSeverities: []string{"critical"},
		ReopenedResolutionPolicy:   ReopenEscalate,
	}
	assert.NoError(t, valid.Validate())

	t.Run("after_days < 1", func(t *testing.T) {
		p := valid
		p.AutoResolveAfterDays = 0
		assert.Error(t, p.Validate())
	})

	t.Run("invalid winner strategy", func(t *testing.T) {
		p := valid
		p.AutoResolveWinner = "random"
		assert.Error(t, p.Validate())
	})

	t.Run("invalid max severity", func(t *testing.T) {
		p := valid
		p.AutoResolveMaxSeverity = "extreme"
		assert.Error(t, p.Validate())
	})

	t.Run("invalid never-resolve severity", func(t *testing.T) {
		p := valid
		p.NeverAutoResolveSeverities = []string{"critical", "mega"}
		assert.Error(t, p.Validate())
	})

	t.Run("invalid reopened policy", func(t *testing.T) {
		p := valid
		p.ReopenedResolutionPolicy = "auto_resolve"
		assert.Error(t, p.Validate())
	})

	t.Run("all winner strategies", func(t *testing.T) {
		for _, w := range []AutoResolveWinner{WinnerRecency, WinnerConfidence, WinnerConsensus} {
			p := valid
			p.AutoResolveWinner = w
			assert.NoError(t, p.Validate(), "strategy %s should be valid", w)
		}
	})

	t.Run("all valid severities", func(t *testing.T) {
		for _, s := range []string{"low", "medium", "high", "critical"} {
			p := valid
			p.AutoResolveMaxSeverity = s
			assert.NoError(t, p.Validate(), "severity %s should be valid", s)
		}
	})
}

func TestSeverityRank(t *testing.T) {
	assert.Equal(t, 1, SeverityRank("low"))
	assert.Equal(t, 2, SeverityRank("medium"))
	assert.Equal(t, 3, SeverityRank("high"))
	assert.Equal(t, 4, SeverityRank("critical"))
	assert.Equal(t, 0, SeverityRank("unknown"))
	assert.Equal(t, 0, SeverityRank(""))
}
