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

	// BreakEvenCostRatio is the miss:false-alarm ratio at which the detector
	// stops losing to the trivial system. NaN when no ratio makes it worthwhile.
	BreakEvenCostRatio float64 `json:"break_even_cost_ratio"`
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
	m.BreakEvenCostRatio = breakEvenCostRatio(m.Sensitivity, m.FalsePosRate, model.Prevalence)
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

// breakEvenCostRatio solves normalizedExpectedCost(r) = 1 for r.
//
// Against the flag-nothing baseline the condition is linear in r:
//
//	pi*(1-sens)*r + (1-pi)*fpr < pi*r      =>      r > (1-pi)*fpr / (pi*sens)
//
// which is exactly the decision-curve identity: the detector pays off once the
// odds of the cost ratio exceed (1-precision)/precision. Returns NaN when the
// detector never beats the trivial system, which happens when it has no
// sensitivity at all.
func breakEvenCostRatio(sens, fpr, pi float64) float64 {
	if sens <= 0 {
		return math.NaN()
	}
	if fpr <= 0 {
		// A detector with no false alarms beats flag-nothing at any positive cost.
		return 0
	}
	return ((1 - pi) * fpr) / (pi * sens)
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
		fmt.Fprintf(&b, "break-even: never — the detector has no sensitivity\n")
		return b.String()
	}
	fmt.Fprintf(&b, "break-even miss:false-alarm cost ratio: %.2f:1\n", m.BreakEvenCostRatio)

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
