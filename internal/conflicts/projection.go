//go:build !lite

package conflicts

import (
	"fmt"
	"math"
)

// Projecting an eval sample onto the corpus, and correcting the corpus base
// rate for the noise in the reference labels.
//
// Two things happen here, and they are separate on purpose.
//
// First, projection. A gold-set run oversamples contradictions by design —
// there are only 93 of them in 2,772 pairs, so a bounded run that sampled
// uniformly would measure almost nothing about recall. Sample precision from
// such a run is meaningless: it describes a population that does not exist.
// ProjectCorpusPrecision re-weights each class's measured flag rate by that
// class's true size in the corpus, which is the only precision figure that
// predicts what an operator's review queue will look like.
//
// Second, correction. Every one of those corpus labels came from a single
// blind LLM rater, and the projection above silently treats them as truth.
// They are not truth; they are a noisy measurement, and at a 3.35% base rate
// the noise floor can exceed the signal. Rogan-Gladen (Am J Public Health
// 1978;68(1):71-76) corrects an observed prevalence for a reference test of
// known sensitivity and specificity:
//
//	theta = (p + Sp - 1) / (Se + Sp - 1)
//
// The correction has a positivity condition that is easy to miss and brutal
// when violated: theta > 0 requires (1 - Sp) < p, i.e. the labeller's
// false-flag rate must be below the base rate being measured. Akashi's own
// re-rate reliability figures put the labeller's false-flag rate at 5.7%
// against a 3.355% observed base rate, which yields theta = -3.09%. That is
// not "there are no contradictions". It is "this labelling protocol cannot
// resolve a prevalence this low, so the base rate every downstream number
// rests on is not established".
//
// CorrectedBaseRate therefore returns an error rather than a floor of zero
// when the estimate is inadmissible. Clamping would hand the caller a number
// that looks like a measurement and is not one, and every precision, lift and
// cost-ratio figure downstream inherits that lie. The operator has to see it.
//
// The calibration constants are inputs, never defaults. Akashi's own
// 0.817/0.943 pair is prose-derived from a 200-pair re-rate whose per-pair
// agreement rows were never published, so it is a sensitivity-analysis input,
// not a measurement. Nothing in this package supplies it.

// CorpusProjection is a gold-set eval sample re-weighted onto the true corpus.
//
// The counts are fractional because they are projections, not observations:
// Flagged is "how many pairs this judge would put in the queue if it ran on
// the whole corpus", derived from per-class flag rates measured on a sample
// that deliberately does not match the corpus mix.
type CorpusProjection struct {
	// Flagged is the projected review-queue size over the whole corpus.
	Flagged float64 `json:"flagged"`

	// TruePositives is the share of Flagged drawn from the contradiction class.
	TruePositives float64 `json:"true_positives"`

	// Precision is TruePositives/Flagged: the fraction of the projected queue
	// an operator would find worth reading. NaN when Measurable is false.
	Precision float64 `json:"precision"`

	// PrecisionLo and PrecisionHi are a Wilson 95% interval computed by
	// treating the rounded projection as a binomial sample. It captures only
	// the queue-size sampling term; the per-class flag rates are themselves
	// estimates, so the true interval is wider than this one. NaN when
	// Measurable is false.
	PrecisionLo float64 `json:"precision_lo"`
	PrecisionHi float64 `json:"precision_hi"`

	// BaseRate is the contradiction share of the corpus as labelled. It is an
	// observed rate against a noisy reference — see CorrectedBaseRate.
	BaseRate float64 `json:"base_rate"`

	// Lift is Precision/BaseRate: how much better than random draw the queue
	// is. NaN when Measurable is false.
	Lift float64 `json:"lift"`

	// CorpusSize is the total labelled corpus the projection re-weights onto.
	CorpusSize int `json:"corpus_size"`

	// Measurable is false when the sample contained no contradiction-class
	// pairs — the intended shape of a run that deliberately evaluates one
	// negative class to measure its false-positive rate. Such a run has no
	// true positives to be precise about, and reporting "precision 0.0%" would
	// read as a judge failure rather than as a question that was not asked.
	Measurable bool `json:"measurable"`

	// SampleFalseFlags, SampleEvaluated and SampleFalseFlagRate describe what a
	// non-measurable run does measure. These are raw sample counts over the
	// non-contradiction classes, not corpus projections.
	SampleFalseFlags    float64 `json:"sample_false_flags"`
	SampleEvaluated     float64 `json:"sample_evaluated"`
	SampleFalseFlagRate float64 `json:"sample_false_flag_rate"`
}

// ProjectCorpusPrecision re-weights a sampled confusion matrix onto the corpus.
//
// conf is indexed conf[expectedRelationship][actualRelationship]; corpusSizes
// gives the true number of pairs per expected relationship in the full labelled
// corpus. Classes present in conf but absent from corpusSizes contribute
// nothing, which is correct: a class the corpus does not contain cannot put
// pairs in the queue.
//
// Only GoldContradiction is a reserved key. The two maps must agree on how the
// other classes are spelled — gold vocabulary or validator vocabulary, either
// works, but not one of each.
func ProjectCorpusPrecision(conf map[string]map[string]int, corpusSizes map[string]int) CorpusProjection {
	p := CorpusProjection{
		Precision:   math.NaN(),
		PrecisionLo: math.NaN(),
		PrecisionHi: math.NaN(),
		Lift:        math.NaN(),
	}
	for _, n := range corpusSizes {
		p.CorpusSize += n
	}
	if p.CorpusSize > 0 {
		p.BaseRate = float64(corpusSizes[GoldContradiction]) / float64(p.CorpusSize)
	}

	for expected, acts := range conf {
		total := 0
		for _, n := range acts {
			total += n
		}
		if total == 0 {
			continue
		}
		projected := float64(corpusSizes[expected]) * float64(acts[GoldContradiction]) / float64(total)
		p.Flagged += projected
		if expected == GoldContradiction {
			p.TruePositives = projected
			continue
		}
		p.SampleEvaluated += float64(total)
		p.SampleFalseFlags += float64(acts[GoldContradiction])
	}
	if p.SampleEvaluated > 0 {
		p.SampleFalseFlagRate = p.SampleFalseFlags / p.SampleEvaluated
	}

	// The guard is on the SAMPLE, not the corpus: a run that evaluated no
	// contradiction-class pairs has no true positives to be precise about,
	// however many the corpus holds.
	_, sampledContradictions := conf[GoldContradiction]
	p.Measurable = sampledContradictions && corpusSizes[GoldContradiction] > 0 && p.Flagged > 0
	if !p.Measurable {
		return p
	}

	p.Precision = p.TruePositives / p.Flagged
	if p.BaseRate > 0 {
		p.Lift = p.Precision / p.BaseRate
	}
	// Round the projection to integers for the interval. A fractional queue has
	// no binomial interpretation, and rounding is the honest way to say "an
	// interval this construction can only approximate".
	if lo, hi, err := WilsonInterval(int(math.Round(p.TruePositives)), int(math.Round(p.Flagged)), 1.96); err == nil {
		p.PrecisionLo, p.PrecisionHi = lo, hi
	}
	return p
}

// LabelCalibration is the measured reliability of the reference labeller whose
// output the corpus base rate was computed from.
//
// There is no default and there must not be one. Akashi's own figures are
// derived from a 200-pair re-rate whose per-pair agreement rows were never
// published; treating them as constants would launder a sensitivity-analysis
// input into a measurement.
type LabelCalibration struct {
	// Sensitivity is the labeller's true-positive rate: the share of genuine
	// contradictions it labels as contradictions.
	Sensitivity float64

	// Specificity is the labeller's true-negative rate. Its complement,
	// 1 - Specificity, is the false-flag rate that drives the positivity
	// condition below.
	Specificity float64

	// N is the size of the re-rate sample the two constants came from. It does
	// not enter the arithmetic; it is carried so a report can state the
	// provenance of numbers that otherwise look authoritative.
	N int
}

// VarianceAmplification is the factor by which Rogan-Gladen widens the interval
// around the corrected estimate relative to the observed one: 1/(Se+Sp-1)^2.
// A weak reference labeller does not merely shift the estimate, it destroys the
// precision of it.
func (c LabelCalibration) VarianceAmplification() float64 {
	d := c.Sensitivity + c.Specificity - 1
	if d <= 0 {
		return math.Inf(1)
	}
	return 1 / (d * d)
}

// minCorrectionDenominator is the smallest Se+Sp-1 this package will divide by.
// Below it the correction is arithmetic noise amplification: at 0.05 the
// variance is already 400x the uncorrected variance, so the point estimate is
// meaningless whatever sign it carries.
const minCorrectionDenominator = 0.05

// CorrectedBaseRate applies the Rogan-Gladen misclassification correction to an
// observed prevalence measured against a noisy reference labeller:
//
//	theta = (p + Sp - 1) / (Se + Sp - 1)
//
// It returns an error, never a clamped or defaulted value, when the estimate is
// inadmissible. A negative theta is a real result and the caller must see it:
// it says the observed flag rate is below what the labeller's own false-flag
// rate alone would produce on a corpus containing zero positives, so the
// protocol cannot resolve a prevalence this low.
func CorrectedBaseRate(observed float64, cal LabelCalibration) (float64, error) {
	if math.IsNaN(observed) || observed < 0 || observed > 1 {
		return 0, fmt.Errorf("observed base rate %v is not a probability", observed)
	}
	if cal.Sensitivity < 0 || cal.Sensitivity > 1 {
		return 0, fmt.Errorf("label sensitivity %v is not a probability", cal.Sensitivity)
	}
	if cal.Specificity < 0 || cal.Specificity > 1 {
		return 0, fmt.Errorf("label specificity %v is not a probability", cal.Specificity)
	}
	if cal.N < 0 {
		return 0, fmt.Errorf("label calibration sample size %d is negative", cal.N)
	}

	denom := cal.Sensitivity + cal.Specificity - 1
	if denom <= minCorrectionDenominator {
		return 0, fmt.Errorf(
			"label calibration is degenerate: sensitivity %.3f + specificity %.3f - 1 = %.3f, at or below the %.2f floor; "+
				"a labeller this close to chance carries no recoverable signal (variance amplification %.0fx)",
			cal.Sensitivity, cal.Specificity, denom, minCorrectionDenominator, cal.VarianceAmplification())
	}

	theta := (observed + cal.Specificity - 1) / denom
	if theta <= 0 {
		return 0, fmt.Errorf(
			"Rogan-Gladen correction is inadmissible: theta = %.4f%% <= 0. "+
				"The positivity condition is (1 - specificity) < observed base rate: the labeller's false-flag rate "+
				"of %.3f%% must be below the observed base rate of %.3f%%, and it is not. "+
				"This does not mean the true rate is zero — it means this labelling protocol cannot resolve a "+
				"prevalence this low, so the observed base rate is not established",
			theta*100, (1-cal.Specificity)*100, observed*100)
	}
	if theta > 1 {
		return 0, fmt.Errorf(
			"Rogan-Gladen correction is inadmissible: theta = %.4f%% > 100%%. "+
				"The observed base rate of %.3f%% exceeds the labeller's sensitivity of %.3f%%, which no prevalence can produce",
			theta*100, observed*100, cal.Sensitivity*100)
	}
	return theta, nil
}

// WilsonInterval returns the Wilson score interval for k successes in n trials
// at the given z. Wilson rather than the normal approximation because the rates
// here sit near zero, where the normal interval runs below it and stops meaning
// anything.
//
// The interval is exact at the boundaries: k=0 gives a lower bound of exactly 0
// and k=n an upper bound of exactly 1, which is the behaviour the boundary
// tests pin.
func WilsonInterval(k, n int, z float64) (lo, hi float64, err error) {
	if n <= 0 {
		return 0, 0, fmt.Errorf("wilson interval needs at least one trial, got n=%d", n)
	}
	if k < 0 || k > n {
		return 0, 0, fmt.Errorf("wilson interval needs 0 <= k <= n, got k=%d n=%d", k, n)
	}
	if z <= 0 {
		return 0, 0, fmt.Errorf("wilson interval needs a positive z, got %v", z)
	}
	fn := float64(n)
	p := float64(k) / fn
	z2 := z * z
	denom := 1 + z2/fn
	center := (p + z2/(2*fn)) / denom
	half := z / denom * math.Sqrt(p*(1-p)/fn+z2/(4*fn*fn))
	return math.Max(0, center-half), math.Min(1, center+half), nil
}
