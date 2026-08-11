//go:build !lite

package storage

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClampPagination(t *testing.T) {
	t.Run("uses defaults for zero limit", func(t *testing.T) {
		limit, offset := clampPagination(0, 0, 50, 1000)
		assert.Equal(t, 50, limit)
		assert.Equal(t, 0, offset)
	})

	t.Run("uses defaults for negative limit", func(t *testing.T) {
		limit, offset := clampPagination(-1, 0, 50, 1000)
		assert.Equal(t, 50, limit)
		assert.Equal(t, 0, offset)
	})

	t.Run("caps limit at max", func(t *testing.T) {
		limit, offset := clampPagination(5000, 0, 50, 1000)
		assert.Equal(t, 1000, limit)
		assert.Equal(t, 0, offset)
	})

	t.Run("passes through valid limit", func(t *testing.T) {
		limit, offset := clampPagination(200, 10, 50, 1000)
		assert.Equal(t, 200, limit)
		assert.Equal(t, 10, offset)
	})

	t.Run("clamps negative offset to zero", func(t *testing.T) {
		limit, offset := clampPagination(50, -5, 50, 1000)
		assert.Equal(t, 50, limit)
		assert.Equal(t, 0, offset)
	})

	t.Run("limit exactly at max is kept", func(t *testing.T) {
		limit, _ := clampPagination(1000, 0, 50, 1000)
		assert.Equal(t, 1000, limit)
	})

	t.Run("limit of 1 is valid", func(t *testing.T) {
		limit, offset := clampPagination(1, 0, 50, 1000)
		assert.Equal(t, 1, limit)
		assert.Equal(t, 0, offset)
	})

	t.Run("respects different defaults", func(t *testing.T) {
		limit, offset := clampPagination(0, 0, 200, 500)
		assert.Equal(t, 200, limit)
		assert.Equal(t, 0, offset)

		limit, offset = clampPagination(600, 0, 200, 500)
		assert.Equal(t, 500, limit)
		assert.Equal(t, 0, offset)
	})
}

// ComputeCalibrated must distinguish "shown" from "unknown". Collapsing them is
// what let the API answer calibrated=true on 19 assessed decisions out of 968.
func TestComputeCalibrated(t *testing.T) {
	tier := func(assessed int, outcome float64, total int, revision float64) *ConfidenceTier {
		o := outcome
		return &ConfidenceTier{AssessedCount: assessed, AvgOutcome: &o, Total: total, RevisionRate: revision}
	}

	tests := []struct {
		name      string
		tiers     map[string]*ConfidenceTier
		hasData   bool
		wantCal   bool
		wantBasis string
	}{
		{
			name:      "demonstrated on a sample that carries it",
			tiers:     map[string]*ConfidenceTier{"high": tier(40, 0.90, 90, 0), "mid": tier(50, 0.70, 100, 0)},
			hasData:   true,
			wantCal:   true,
			wantBasis: CalibrationBasisOutcome,
		},
		{
			name:      "right ordering but too few assessments is not a yes",
			tiers:     map[string]*ConfidenceTier{"high": tier(19, 0.95, 968, 0), "mid": tier(205, 0.87, 2991, 0)},
			hasData:   true,
			wantCal:   false,
			wantBasis: CalibrationBasisInsufficient,
		},
		{
			name:      "high underperforms mid",
			tiers:     map[string]*ConfidenceTier{"high": tier(40, 0.55, 90, 0), "mid": tier(50, 0.72, 100, 0)},
			hasData:   true,
			wantCal:   false,
			wantBasis: CalibrationBasisOutcome,
		},
		{
			name: "low beats mid — ordering carries no information",
			tiers: map[string]*ConfidenceTier{
				"high": tier(40, 0.95, 90, 0), "mid": tier(50, 0.87, 100, 0), "low": tier(30, 0.91, 40, 0),
			},
			hasData:   true,
			wantCal:   false,
			wantBasis: CalibrationBasisNonMonotonic,
		},
		{
			name:      "uniform zero revision rate is absence of signal, not proof",
			tiers:     map[string]*ConfidenceTier{"high": tier(0, 0, 968, 0), "mid": tier(0, 0, 2991, 0)},
			hasData:   false,
			wantCal:   false,
			wantBasis: CalibrationBasisInsufficient,
		},
		{
			name:      "revision proxy fires when revisions actually occur",
			tiers:     map[string]*ConfidenceTier{"high": tier(0, 0, 90, 2), "mid": tier(0, 0, 100, 8)},
			hasData:   false,
			wantCal:   true,
			wantBasis: CalibrationBasisRevisionRate,
		},
		{
			name:      "missing mid tier is unknown, not calibrated",
			tiers:     map[string]*ConfidenceTier{"high": tier(40, 0.90, 90, 0)},
			hasData:   true,
			wantCal:   false,
			wantBasis: CalibrationBasisInsufficient,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotCal, gotBasis := ComputeCalibrated(tc.tiers, tc.hasData)
			assert.Equal(t, tc.wantCal, gotCal)
			assert.Equal(t, tc.wantBasis, gotBasis)
		})
	}
}
