//go:build !lite && integration

package conflicts

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The measured operating point for the akashi corpus: 3.35% prevalence
// (93 contradictions in 2,772 blind-labelled pairs), 50.5% recall and a 2.44%
// weighted false-positive rate for gpt-5 as judge. These numbers anchor the
// tests because the property that matters is not "the formula computes
// something" but "the formula reproduces the verdict we acted on".
const (
	corpusPrevalence = 0.0335
	corpusSens       = 0.505
	corpusFPR        = 0.0244
)

func countsFor(prevalence, sens, fpr float64, n int) (tp, fp, tn, fn int) {
	pos := int(math.Round(prevalence * float64(n)))
	neg := n - pos
	tp = int(math.Round(sens * float64(pos)))
	fn = pos - tp
	fp = int(math.Round(fpr * float64(neg)))
	tn = neg - fp
	return
}

// At equal error costs this detector is worse than never flagging anything.
// Precision and recall cannot express that, which is the reason this file
// exists; if this test ever starts passing at NEC < 1 without the underlying
// numbers changing, the cost model has been broken.
func TestComputeCostMetrics_LosesToTrivialSystemAtEqualCosts(t *testing.T) {
	tp, fp, tn, fn := countsFor(corpusPrevalence, corpusSens, corpusFPR, 1_000_000)
	m := ComputeCostMetrics(tp, fp, tn, fn, CostModel{Prevalence: corpusPrevalence, MissCostRatio: 1})

	assert.InDelta(t, corpusSens, m.Sensitivity, 0.002)
	assert.InDelta(t, corpusFPR, m.FalsePosRate, 0.002)
	assert.InDelta(t, 1-corpusFPR, m.Specificity, 0.002)
	assert.Greater(t, m.NormalizedExpectedCost, 1.0,
		"at a 1:1 cost ratio the detector must be reported as worse than the trivial system")
	assert.InDelta(t, 1.198, m.NormalizedExpectedCost, 0.02)
}

func TestComputeCostMetrics_BreakEvenMatchesDecisionCurveIdentity(t *testing.T) {
	tp, fp, tn, fn := countsFor(corpusPrevalence, corpusSens, corpusFPR, 1_000_000)
	m := ComputeCostMetrics(tp, fp, tn, fn, CostModel{Prevalence: corpusPrevalence, MissCostRatio: 1})

	assert.InDelta(t, 1.39, m.BreakEvenCostRatio, 0.05)

	// Decision curve analysis says net benefit is positive exactly when
	// precision exceeds the threshold probability, i.e. the break-even odds are
	// (1-precision)/precision. The two derivations must agree, because they are
	// the same statement about the same trade.
	precision := float64(tp) / float64(tp+fp)
	assert.InDelta(t, (1-precision)/precision, m.BreakEvenCostRatio, 0.02,
		"cost model and decision curve analysis must give the same break-even")
}

// Crossing the break-even ratio must flip the verdict, in both directions.
func TestComputeCostMetrics_VerdictFlipsAtBreakEven(t *testing.T) {
	tp, fp, tn, fn := countsFor(corpusPrevalence, corpusSens, corpusFPR, 1_000_000)
	base := ComputeCostMetrics(tp, fp, tn, fn, CostModel{Prevalence: corpusPrevalence, MissCostRatio: 1})
	be := base.BreakEvenCostRatio
	require.False(t, math.IsNaN(be))

	below := ComputeCostMetrics(tp, fp, tn, fn, CostModel{Prevalence: corpusPrevalence, MissCostRatio: be * 0.9})
	above := ComputeCostMetrics(tp, fp, tn, fn, CostModel{Prevalence: corpusPrevalence, MissCostRatio: be * 1.1})
	assert.Greater(t, below.NormalizedExpectedCost, 1.0, "below break-even the detector must lose")
	assert.Less(t, above.NormalizedExpectedCost, 1.0, "above break-even the detector must win")
}

// A perfect-specificity detector is worth running at any positive cost ratio,
// and a zero-sensitivity detector is worth running at none. These are the
// boundaries where a naive implementation divides by zero.
func TestComputeCostMetrics_Boundaries(t *testing.T) {
	noFalseAlarms := ComputeCostMetrics(50, 0, 950, 50, CostModel{Prevalence: 0.1, MissCostRatio: 1})
	assert.Equal(t, 0.0, noFalseAlarms.BreakEvenCostRatio)
	assert.Less(t, noFalseAlarms.NormalizedExpectedCost, 1.0)

	blind := ComputeCostMetrics(0, 100, 900, 100, CostModel{Prevalence: 0.1, MissCostRatio: 1})
	assert.True(t, math.IsNaN(blind.BreakEvenCostRatio), "a detector that finds nothing never pays off")

	// An unusable cost model must not produce a confident-looking number.
	bad := ComputeCostMetrics(10, 10, 10, 10, CostModel{Prevalence: 0, MissCostRatio: 1})
	assert.True(t, math.IsNaN(bad.NormalizedExpectedCost))
	bad = ComputeCostMetrics(10, 10, 10, 10, CostModel{Prevalence: 0.1, MissCostRatio: 0})
	assert.True(t, math.IsNaN(bad.NormalizedExpectedCost))
}

// Precision moves with prevalence and sensitivity/specificity do not. This is
// why a stratified eval sample must never supply the prevalence.
func TestComputeCostMetrics_PrevalenceInvariance(t *testing.T) {
	tp, fp, tn, fn := countsFor(0.5, corpusSens, corpusFPR, 100_000) // stratified sample
	rare := ComputeCostMetrics(tp, fp, tn, fn, CostModel{Prevalence: corpusPrevalence, MissCostRatio: 1})
	balanced := ComputeCostMetrics(tp, fp, tn, fn, CostModel{Prevalence: 0.5, MissCostRatio: 1})

	assert.InDelta(t, rare.Sensitivity, balanced.Sensitivity, 1e-9)
	assert.InDelta(t, rare.FalsePosRate, balanced.FalsePosRate, 1e-9)
	assert.Greater(t, math.Abs(rare.NormalizedExpectedCost-balanced.NormalizedExpectedCost), 0.1,
		"the same detector must be judged differently at different prevalences")
}

func TestFormatCostMetrics(t *testing.T) {
	tp, fp, tn, fn := countsFor(corpusPrevalence, corpusSens, corpusFPR, 1_000_000)
	m := ComputeCostMetrics(tp, fp, tn, fn, CostModel{Prevalence: corpusPrevalence, MissCostRatio: 1})
	out := FormatCostMetrics(m, []float64{1, 3, 10})

	assert.Contains(t, out, "break-even")
	assert.Contains(t, out, "prevalence-invariant")
	assert.Contains(t, out, "worse than the best trivial system")
	assert.Contains(t, out, "worth running")
}
