//go:build !lite

package conflicts

import (
	"fmt"
	"math"
	"strings"
)

// Cost-aware evaluation for a rare-event detector.
//
// Conflict detection is screening, not classification: genuine contradictions
// are roughly 3% of the pairs that reach the judge. At that prevalence,
// precision is governed almost entirely by the false-positive rate on the
// majority class — at a 3.35% base rate, an FPR of 5.7% yields 23% precision,
// 1% yields 63%, and 0.1% yields 95%, while raising recall from 30% to 80% at a
// fixed 1% FPR moves precision only from 51% to 74%.
//
// Two consequences drive this file.
//
// First, class-averaged scores actively mislead here. Measured on the blind
// gold set, gpt-5-mini had the best sample F1 (0.704) and the worst product
// outcome (17.3% corpus precision), because F1 cannot see a fourfold difference
// in majority-class FPR. Sensitivity and specificity are the transportable
// quantities; precision is not, because it moves with prevalence.
//
// Second, a detector that looks good on precision can still be worth less than
// not running it. Normalized expected cost compares against the best trivial
// system — always-flag or never-flag — so it answers the question precision
// cannot: is this worth a reviewer's attention at all? At the operating point
// measured for this corpus (3.35% prevalence, 50.5% recall, 2.44% FPR) the
// answer at equal error costs is no: NEC is 1.198, and the detector only earns
// its keep once a missed contradiction is judged ~1.4x worse than a false alarm.
//
// The arithmetic follows decision curve analysis (Vickers & Elkin, Medical
// Decision Making 2006;26(6):565-574): net benefit is positive exactly when
// precision exceeds the threshold probability, which is the same break-even
// this file computes from the cost ratio.

// CostModel describes the operating environment a detector is judged in.
type CostModel struct {
	// Prevalence is the fraction of evaluated pairs that are genuine
	// contradictions. It must come from the population the detector runs on,
	// not from a stratified eval sample, which oversamples positives by design.
	Prevalence float64

	// MissCostRatio is the cost of a missed contradiction relative to one false
	// alarm. 1.0 means they are equally bad. There is no defensible default:
	// it is a statement about the user's world, so callers must supply it.
	MissCostRatio float64
}

// CostMetrics reports prevalence-invariant detector characteristics alongside
// the cost-relative verdict.
type CostMetrics struct {
	Prevalence    float64 `json:"prevalence"`
	Sensitivity   float64 `json:"sensitivity"`
	Specificity   float64 `json:"specificity"`
	FalsePosRate  float64 `json:"false_positive_rate"`
	MCC           float64 `json:"mcc"`
	MissCostRatio float64 `json:"miss_cost_ratio"`

	// NormalizedExpectedCost is the detector's expected cost divided by the
	// cost of the better trivial system. Below 1.0 the detector is worth
	// running; at or above 1.0 a fixed answer is cheaper.
	NormalizedExpectedCost float64 `json:"normalized_expected_cost"`

	// BreakEvenCostRatio is the lowest miss:false-alarm ratio at which the
	// detector stops losing to flagging nothing. NaN when no ratio works.
	BreakEvenCostRatio float64 `json:"break_even_cost_ratio"`

	// UpperCostRatio is the ratio above which flagging EVERYTHING becomes
	// cheaper than running the detector. A detector is only worth running for
	// ratios strictly between BreakEvenCostRatio and this. The upper bound is
	// easy to forget because it is counter-intuitive: once a miss is
	// catastrophic enough, a review queue that filters anything at all costs
	// more than reviewing everything. NaN when no ratio works.
	UpperCostRatio float64 `json:"upper_cost_ratio"`
}

// ComputeCostMetrics derives cost-relative metrics from a confusion matrix.
//
// Counts may come from a stratified sample, but prevalence must not: pass the
// population prevalence in the model and let sensitivity and specificity carry
// the sample's information. Mixing a sample's prevalence into this calculation
// is how a stratified eval flatters a detector.
func ComputeCostMetrics(tp, fp, tn, fn int, model CostModel) CostMetrics {
	m := CostMetrics{
		Prevalence:             model.Prevalence,
		MissCostRatio:          model.MissCostRatio,
		NormalizedExpectedCost: math.NaN(),
		BreakEvenCostRatio:     math.NaN(),
		UpperCostRatio:         math.NaN(),
	}
	if tp+fn > 0 {
		m.Sensitivity = float64(tp) / float64(tp+fn)
	}
	if tn+fp > 0 {
		m.Specificity = float64(tn) / float64(tn+fp)
		m.FalsePosRate = float64(fp) / float64(tn+fp)
	}
	m.MCC = matthews(tp, fp, tn, fn)

	if model.Prevalence <= 0 || model.Prevalence >= 1 || model.MissCostRatio <= 0 {
		return m
	}
	m.NormalizedExpectedCost = normalizedExpectedCost(m.Sensitivity, m.FalsePosRate, model)
	m.BreakEvenCostRatio, m.UpperCostRatio = worthwhileCostRatios(m.Sensitivity, m.FalsePosRate, model.Prevalence)
	return m
}

// normalizedExpectedCost divides the detector's expected cost by the cost of
// the cheaper of the two trivial systems (flag everything / flag nothing).
func normalizedExpectedCost(sens, fpr float64, model CostModel) float64 {
	pi, r := model.Prevalence, model.MissCostRatio
	detector := pi*(1-sens)*r + (1-pi)*fpr
	// Flagging nothing misses every positive; flagging everything raises a
	// false alarm on every negative.
	trivial := math.Min(pi*r, 1-pi)
	if trivial <= 0 {
		return math.NaN()
	}
	return detector / trivial
}

// worthwhileCostRatios returns the open interval of miss:false-alarm ratios for
// which the detector beats BOTH trivial systems.
//
// There are two baselines, and a detector must beat whichever is cheaper.
// Against flag-nothing (cost pi*r):
//
//	pi*(1-sens)*r + (1-pi)*fpr < pi*r   =>   r > (1-pi)*fpr / (pi*sens)
//
// which is the decision-curve identity — the detector pays off once the cost
// odds exceed (1-precision)/precision. Against flag-everything (cost 1-pi):
//
//	pi*(1-sens)*r + (1-pi)*fpr < 1-pi   =>   r < (1-pi)*(1-fpr) / (pi*(1-sens))
//
// The upper bound is the one that gets forgotten, and it is real: once a miss
// is catastrophic enough relative to a false alarm, any filtering at all costs
// more than reviewing every pair. Reporting only the lower bound tells an
// operator with an extreme cost ratio to run a detector they should not.
//
// The interval is non-empty exactly when fpr < sens, i.e. when the detector is
// better than chance. Both bounds are NaN otherwise.
func worthwhileCostRatios(sens, fpr, pi float64) (low, high float64) {
	if sens <= fpr {
		// At or below chance no cost ratio makes the detector worth running.
		return math.NaN(), math.NaN()
	}
	high = ((1 - pi) * (1 - fpr)) / (pi * (1 - sens))
	if sens >= 1 {
		high = math.Inf(1) // never misses, so flag-everything never wins
	}
	if fpr <= 0 {
		// No false alarms: beats flag-nothing at any positive cost ratio.
		return 0, high
	}
	return ((1 - pi) * fpr) / (pi * sens), high
}

// FormatCostMetrics renders a short cost report, including a sweep over
// plausible cost ratios so the reader sees where the operating point flips
// rather than being handed a single number to trust.
func FormatCostMetrics(m CostMetrics, sweep []float64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n=== cost-aware metrics (prevalence %.2f%%) ===\n", m.Prevalence*100)
	fmt.Fprintf(&b, "sensitivity %.1f%%   specificity %.2f%%   FPR %.2f%%   MCC %.3f\n",
		m.Sensitivity*100, m.Specificity*100, m.FalsePosRate*100, m.MCC)
	fmt.Fprintf(&b, "(sensitivity, specificity and FPR are prevalence-invariant; precision is not)\n")

	if math.IsNaN(m.BreakEvenCostRatio) {
		fmt.Fprintf(&b, "worthwhile at no cost ratio — sensitivity does not exceed the false-positive rate\n")
		return b.String()
	}
	if math.IsInf(m.UpperCostRatio, 1) {
		fmt.Fprintf(&b, "worth running for cost ratios above %.2f:1\n", m.BreakEvenCostRatio)
	} else {
		fmt.Fprintf(&b, "worth running for cost ratios between %.2f:1 and %.2f:1\n",
			m.BreakEvenCostRatio, m.UpperCostRatio)
		fmt.Fprintf(&b, "(above %.2f:1 a miss is costly enough that reviewing every pair is cheaper)\n",
			m.UpperCostRatio)
	}

	for _, r := range sweep {
		nec := normalizedExpectedCost(m.Sensitivity, m.FalsePosRate, CostModel{Prevalence: m.Prevalence, MissCostRatio: r})
		verdict := "worse than the best trivial system"
		if nec < 1 {
			verdict = "worth running"
		}
		fmt.Fprintf(&b, "  cost ratio %6.1f:1  NEC %.3f  %s\n", r, nec, verdict)
	}
	return b.String()
}

func matthews(tp, fp, tn, fn int) float64 {
	num := float64(tp)*float64(tn) - float64(fp)*float64(fn)
	den := math.Sqrt(float64(tp+fp) * float64(tp+fn) * float64(tn+fp) * float64(tn+fn))
	if den == 0 {
		return 0
	}
	return num / den
}
