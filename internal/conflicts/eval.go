//go:build !lite

package conflicts

// eval.go provides precision/recall computation for conflict detection evaluation.
// It compares scorer predictions (detected conflicts) against ground truth labels.

// PrecisionRecall holds evaluation metrics for a conflict detection run.
type PrecisionRecall struct {
	TruePositives  int     // detected + labeled genuine
	FalsePositives int     // detected + labeled related_not_contradicting or unrelated_false_positive
	FalseNegatives int     // not detected + labeled genuine (missed conflicts)
	Precision      float64 // TP / (TP + FP)
	Recall         float64 // TP / (TP + FN)
	F1             float64 // 2 * (P * R) / (P + R)
}

// EvalResult pairs a decision pair with its expected and actual outcome.
type EvalResult struct {
	DecisionAOutcome string
	DecisionBOutcome string
	ExpectedLabel    string // "genuine", "related_not_contradicting", "unrelated_false_positive"
	Detected         bool   // whether the scorer produced a conflict for this pair
}

// ComputePrecisionRecall calculates precision, recall, and F1 from evaluation results.
// A "genuine" label is a positive; everything else is a negative.
func ComputePrecisionRecall(results []EvalResult) PrecisionRecall {
	var pr PrecisionRecall
	for _, r := range results {
		isPositive := r.ExpectedLabel == "genuine"
		switch {
		case r.Detected && isPositive:
			pr.TruePositives++
		case r.Detected && !isPositive:
			pr.FalsePositives++
		case !r.Detected && isPositive:
			pr.FalseNegatives++
		}
		// true negatives: !detected && !isPositive — not tracked, not needed for P/R
	}

	if pr.TruePositives+pr.FalsePositives > 0 {
		pr.Precision = float64(pr.TruePositives) / float64(pr.TruePositives+pr.FalsePositives)
	}
	if pr.TruePositives+pr.FalseNegatives > 0 {
		pr.Recall = float64(pr.TruePositives) / float64(pr.TruePositives+pr.FalseNegatives)
	}
	if pr.Precision+pr.Recall > 0 {
		pr.F1 = 2 * pr.Precision * pr.Recall / (pr.Precision + pr.Recall)
	}
	return pr
}
