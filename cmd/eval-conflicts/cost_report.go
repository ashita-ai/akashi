package main

import (
	"fmt"
	"os"

	"github.com/ashita-ai/akashi/internal/conflicts"
)

// printCostReport adds the cost-relative view to an eval run.
//
// Precision and recall cannot say whether a detector is worth running: at a low
// base rate a detector can post respectable precision and still cost more than
// answering "no" every time. That comparison needs a stated miss:false-alarm
// ratio, which is a fact about the operator's world, so it is opt-in rather
// than defaulted to a number nobody chose.
func printCostReport(m conflicts.EvalMetrics, costRatio, prevalence float64) {
	if costRatio <= 0 && prevalence <= 0 {
		return
	}
	if costRatio <= 0 || prevalence <= 0 {
		fmt.Fprintln(os.Stderr, "--cost-ratio and --prevalence must be given together; skipping cost report")
		return
	}
	cm := conflicts.ComputeCostMetrics(m.TruePositives, m.FalsePositives, m.TrueNegatives, m.FalseNegatives,
		conflicts.CostModel{Prevalence: prevalence, MissCostRatio: costRatio})
	fmt.Print(conflicts.FormatCostMetrics(cm, []float64{1, costRatio, 3 * costRatio}))
}
