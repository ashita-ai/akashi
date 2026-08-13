//go:build !lite

package conflicts

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The 2,772-pair blind-labelled corpus, by gold class. Every published figure
// for this detector is projected onto these counts, so they are pinned here
// rather than recomputed.
var publishedCorpus = map[string]int{
	GoldContradiction: 93,
	GoldSupersession:  627,
	GoldRelated:       2017,
	GoldUnrelated:     35,
}

const publishedBaseRate = 93.0 / 2772.0 // 3.355%

// TestProjectCorpusPrecision_ReproducesPublishedGPT5Point is a characterization
// test. ADR-017 quotes gpt-5 at a 113-pair review queue, 41.5% precision and
// 12.4x lift over the base rate, and the operating point in production was
// chosen from those three numbers. The arithmetic that produces them used to
// live inline in package main where nothing could hold it still. It is pinned
// here so a refactor cannot move a number an operator already acted on.
//
// The per-class flag rates below reproduce the published aggregate: 50.5%
// recall on the contradiction class and a 2.5% flag rate on each of the two
// large negative classes, which weights to the published 2.44% overall FPR.
func TestProjectCorpusPrecision_ReproducesPublishedGPT5Point(t *testing.T) {
	conf := map[string]map[string]int{
		GoldContradiction: {GoldContradiction: 47, "complementary": 46},
		GoldRelated:       {GoldContradiction: 5, "complementary": 195},
		GoldSupersession:  {GoldContradiction: 5, GoldSupersession: 195},
		GoldUnrelated:     {GoldUnrelated: 35},
	}

	p := ProjectCorpusPrecision(conf, publishedCorpus)

	require.True(t, p.Measurable, "a run that sampled contradictions must be measurable")
	assert.Equal(t, 2772, p.CorpusSize)
	assert.InDelta(t, publishedBaseRate, p.BaseRate, 1e-6)

	assert.InDelta(t, 113, p.Flagged, 0.5, "ADR-017 quotes a 113-pair review queue")
	assert.InDelta(t, 0.415, p.Precision, 0.005, "ADR-017 quotes 41.5%% precision")
	assert.InDelta(t, 12.4, p.Lift, 0.05, "ADR-017 quotes 12.4x lift")

	// Recall falls out of the contradiction-class flag rate and is the third
	// leg of the published point.
	assert.InDelta(t, 0.505, p.TruePositives/float64(publishedCorpus[GoldContradiction]), 0.005)

	// The interval must bracket the point estimate and stay well inside (0,1):
	// 113 pairs is a small queue and the honest interval is wide.
	assert.Less(t, p.PrecisionLo, p.Precision)
	assert.Greater(t, p.PrecisionHi, p.Precision)
	assert.InDelta(t, 0.3293, p.PrecisionLo, 1e-3)
	assert.InDelta(t, 0.5081, p.PrecisionHi, 1e-3)
}

// A --gold-classes run that deliberately evaluates one negative class has no
// true positives to be precise about. Reporting "precision 0.0%" would read as
// a judge failure rather than as a question that was never asked, so the
// projection reports the false-positive rate instead.
func TestProjectCorpusPrecision_NegativeClassOnlyRunIsNotMeasurable(t *testing.T) {
	conf := map[string]map[string]int{
		GoldRelated: {GoldContradiction: 4, "complementary": 196},
	}

	p := ProjectCorpusPrecision(conf, publishedCorpus)

	assert.False(t, p.Measurable)
	assert.True(t, math.IsNaN(p.Precision), "precision must be NaN, not 0, when nothing was measured")
	assert.True(t, math.IsNaN(p.Lift))
	assert.InDelta(t, 0.02, p.SampleFalseFlagRate, 1e-9)
	assert.InDelta(t, 4, p.SampleFalseFlags, 1e-9)
	assert.InDelta(t, 200, p.SampleEvaluated, 1e-9)
}

// A judge that flags nothing produces an empty queue, not a division by zero.
func TestProjectCorpusPrecision_EmptyQueueIsNotMeasurable(t *testing.T) {
	conf := map[string]map[string]int{
		GoldContradiction: {"complementary": 93},
		GoldRelated:       {"complementary": 200},
	}

	p := ProjectCorpusPrecision(conf, publishedCorpus)

	assert.False(t, p.Measurable)
	assert.Zero(t, p.Flagged)
	assert.True(t, math.IsNaN(p.Precision))
}

// TestCorrectedBaseRate_AkashiLabellerIsInadmissible is the finding this file
// exists for. Akashi's own re-rate reliability figures put the labeller's
// false-flag rate at 5.7% against a 3.355% observed base rate. Rogan-Gladen
// then returns a negative prevalence, and the caller must be told so rather
// than handed a floor of zero — the corrected estimate is not "about zero
// contradictions", it is "this protocol cannot resolve a prevalence this low".
func TestCorrectedBaseRate_AkashiLabellerIsInadmissible(t *testing.T) {
	_, err := CorrectedBaseRate(publishedBaseRate, LabelCalibration{
		Sensitivity: 0.817,
		Specificity: 0.943,
		N:           200,
	})

	require.Error(t, err, "a negative Rogan-Gladen estimate must be an error, never a clamped zero")
	assert.Contains(t, err.Error(), "positivity condition",
		"the operator has to be told which condition failed")
	assert.Contains(t, err.Error(), "(1 - specificity) < observed base rate")
	assert.Contains(t, err.Error(), "5.700%", "the labeller's false-flag rate belongs in the message")
	assert.Contains(t, err.Error(), "3.355%", "the observed base rate belongs in the message")
	assert.Contains(t, err.Error(), "cannot resolve a prevalence this low",
		"the message must rule out the 'there are no contradictions' misreading")
}

// The sensitivity sweep over the labeller's false-flag rate, holding its
// sensitivity at the reported 0.817. Break-even sits exactly at the observed
// base rate, which is the positivity condition stated as a number.
func TestCorrectedBaseRate_FalseFlagSweep(t *testing.T) {
	cases := []struct {
		name      string
		falseFlag float64
		want      float64
	}{
		{"false-flag 2.0%", 0.020, 0.01700},
		{"false-flag 1.0%", 0.010, 0.02918},
		{"false-flag 0.5%", 0.005, 0.03516},
		{"perfect specificity", 0.000, 0.04106},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CorrectedBaseRate(publishedBaseRate, LabelCalibration{
				Sensitivity: 0.817,
				Specificity: 1 - tc.falseFlag,
				N:           200,
			})
			require.NoError(t, err)
			assert.InDelta(t, tc.want, got, 1e-3)
		})
	}
}

// The positivity condition is a knife edge at the observed base rate itself:
// a labeller whose false-flag rate sits a whisker above 3.355% is inadmissible,
// and one a whisker below yields an admissible but useless estimate near zero.
// Both sides are pinned because the whole finding is that Akashi's labeller is
// on the wrong side of this line by a factor of 1.7.
func TestCorrectedBaseRate_PositivityKnifeEdge(t *testing.T) {
	_, err := CorrectedBaseRate(publishedBaseRate, LabelCalibration{
		Sensitivity: 0.817, Specificity: 1 - 0.0340, N: 200,
	})
	require.Error(t, err, "false-flag rate just above the base rate must be inadmissible")
	assert.Contains(t, err.Error(), "positivity condition")

	got, err := CorrectedBaseRate(publishedBaseRate, LabelCalibration{
		Sensitivity: 0.817, Specificity: 1 - 0.0330, N: 200,
	})
	require.NoError(t, err, "false-flag rate just below the base rate is admissible")
	assert.InDelta(t, 0.0007, got, 1e-4, "and worth almost nothing: 0.07%% against an observed 3.355%%")
}

func TestCorrectedBaseRate_RejectsDegenerateCalibration(t *testing.T) {
	cases := []struct {
		name    string
		obs     float64
		cal     LabelCalibration
		wantMsg string
	}{
		{
			name:    "coin-flip labeller",
			obs:     0.5,
			cal:     LabelCalibration{Sensitivity: 0.5, Specificity: 0.5},
			wantMsg: "degenerate",
		},
		{
			name:    "denominator just under the floor",
			obs:     0.5,
			cal:     LabelCalibration{Sensitivity: 0.90, Specificity: 0.14},
			wantMsg: "degenerate",
		},
		{
			name:    "observed rate is not a probability",
			obs:     1.4,
			cal:     LabelCalibration{Sensitivity: 0.9, Specificity: 0.9},
			wantMsg: "not a probability",
		},
		{
			name:    "sensitivity is not a probability",
			obs:     0.03,
			cal:     LabelCalibration{Sensitivity: 1.2, Specificity: 0.9},
			wantMsg: "sensitivity",
		},
		{
			name:    "specificity is not a probability",
			obs:     0.03,
			cal:     LabelCalibration{Sensitivity: 0.9, Specificity: -0.1},
			wantMsg: "specificity",
		},
		{
			name:    "observed rate above the labeller's sensitivity",
			obs:     0.95,
			cal:     LabelCalibration{Sensitivity: 0.80, Specificity: 0.99},
			wantMsg: "> 100%",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CorrectedBaseRate(tc.obs, tc.cal)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantMsg)
		})
	}
}

// A perfect labeller changes nothing, which is the identity that says the
// correction is applied in the right direction.
func TestCorrectedBaseRate_PerfectLabellerIsIdentity(t *testing.T) {
	got, err := CorrectedBaseRate(publishedBaseRate, LabelCalibration{Sensitivity: 1, Specificity: 1, N: 200})
	require.NoError(t, err)
	assert.InDelta(t, publishedBaseRate, got, 1e-12)
}

// The reason the corrected estimate cannot simply be quoted with the observed
// interval: Akashi's own calibration widens it by 1.73x.
func TestLabelCalibration_VarianceAmplification(t *testing.T) {
	cal := LabelCalibration{Sensitivity: 0.817, Specificity: 0.943, N: 200}
	assert.InDelta(t, 1.73, cal.VarianceAmplification(), 0.01)
	assert.Equal(t, 1.0, LabelCalibration{Sensitivity: 1, Specificity: 1}.VarianceAmplification())
	assert.True(t, math.IsInf(LabelCalibration{Sensitivity: 0.5, Specificity: 0.5}.VarianceAmplification(), 1))
}

func TestWilsonInterval(t *testing.T) {
	cases := []struct {
		name   string
		k, n   int
		wantLo float64
		wantHi float64
	}{
		// Boundary cases are the reason for Wilson over the normal
		// approximation: the interval stays inside [0,1] and is exact at the
		// ends.
		{"zero successes", 0, 100, 0, 0.0370},
		{"all successes", 100, 100, 0.9630, 1},
		{"single trial, zero", 0, 1, 0, 0.7935},
		{"single trial, one", 1, 1, 0.2065, 1},
		// Textbook midpoint: 50/100 is symmetric about 0.5.
		{"half", 50, 100, 0.4038, 0.5962},
		// The published gold-set base rate, as an observed proportion.
		{"corpus base rate", 93, 2772, 0.0275, 0.0409},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lo, hi, err := WilsonInterval(tc.k, tc.n, 1.96)
			require.NoError(t, err)
			assert.InDelta(t, tc.wantLo, lo, 1e-3)
			assert.InDelta(t, tc.wantHi, hi, 1e-3)
			assert.LessOrEqual(t, lo, hi)
		})
	}
}

func TestWilsonInterval_Rejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		k, n int
		z    float64
	}{
		{"no trials", 0, 0, 1.96},
		{"negative successes", -1, 10, 1.96},
		{"more successes than trials", 11, 10, 1.96},
		{"non-positive z", 5, 10, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := WilsonInterval(tc.k, tc.n, tc.z)
			require.Error(t, err)
		})
	}
}
