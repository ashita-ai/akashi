//go:build !lite

package conflicts

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestComputeMetrics_PerfectScore(t *testing.T) {
	results := []EvalResult{
		{ConflictExpected: true, ConflictActual: true, Correct: true},
		{ConflictExpected: true, ConflictActual: true, Correct: true},
		{ConflictExpected: false, ConflictActual: false, Correct: true},
		{ConflictExpected: false, ConflictActual: false, Correct: true},
	}
	m := ComputeMetrics(results)
	assert.Equal(t, 4, m.TotalPairs)
	assert.Equal(t, 0, m.Errors)
	assert.InDelta(t, 1.0, m.ConflictPrec, 1e-6)
	assert.InDelta(t, 1.0, m.ConflictRecall, 1e-6)
	assert.InDelta(t, 1.0, m.ConflictF1, 1e-6)
	assert.InDelta(t, 1.0, m.RelationshipAcc, 1e-6)
	assert.Equal(t, 2, m.TruePositives)
	assert.Equal(t, 0, m.FalsePositives)
	assert.Equal(t, 2, m.TrueNegatives)
	assert.Equal(t, 0, m.FalseNegatives)
}

func TestComputeMetrics_WithFalsePositives(t *testing.T) {
	results := []EvalResult{
		{ConflictExpected: true, ConflictActual: true, Correct: true},   // TP
		{ConflictExpected: false, ConflictActual: true, Correct: false}, // FP
		{ConflictExpected: false, ConflictActual: false, Correct: true}, // TN
		{ConflictExpected: true, ConflictActual: false, Correct: false}, // FN
	}
	m := ComputeMetrics(results)
	assert.Equal(t, 1, m.TruePositives)
	assert.Equal(t, 1, m.FalsePositives)
	assert.Equal(t, 1, m.TrueNegatives)
	assert.Equal(t, 1, m.FalseNegatives)
	assert.InDelta(t, 0.5, m.ConflictPrec, 1e-6)
	assert.InDelta(t, 0.5, m.ConflictRecall, 1e-6)
	assert.InDelta(t, 0.5, m.ConflictF1, 1e-6)
	assert.InDelta(t, 0.5, m.RelationshipAcc, 1e-6)
}

func TestComputeMetrics_WithErrors(t *testing.T) {
	results := []EvalResult{
		{ConflictExpected: true, ConflictActual: true, Correct: true},
		{Error: "timeout"},
	}
	m := ComputeMetrics(results)
	assert.Equal(t, 2, m.TotalPairs)
	assert.Equal(t, 1, m.Errors)
	assert.Equal(t, 1, m.TruePositives)
	assert.InDelta(t, 1.0, m.RelationshipAcc, 1e-6) // 1/1 (1 error excluded)
}

func TestComputeMetrics_Empty(t *testing.T) {
	m := ComputeMetrics(nil)
	assert.Equal(t, 0, m.TotalPairs)
	assert.InDelta(t, 0.0, m.ConflictPrec, 1e-6)
	assert.InDelta(t, 0.0, m.ConflictRecall, 1e-6)
}

func TestDefaultEvalDataset_NotEmpty(t *testing.T) {
	ds := DefaultEvalDataset()
	assert.GreaterOrEqual(t, len(ds), 16, "dataset should have at least 16 labeled pairs")

	// Verify all expected relationships are valid.
	for _, p := range ds {
		assert.Contains(t, []string{"contradiction", "supersession", "complementary", "refinement", "unrelated"},
			p.ExpectedRelationship, "pair %q has invalid expected relationship", p.Label)
		assert.NotEmpty(t, p.Label, "pair must have a label")
		assert.NotEmpty(t, p.Input.OutcomeA, "pair %q must have outcome_a", p.Label)
		assert.NotEmpty(t, p.Input.OutcomeB, "pair %q must have outcome_b", p.Label)
	}

	// Verify dataset has both positive and negative examples.
	var conflicts, nonConflicts int
	for _, p := range ds {
		if p.ExpectedRelationship == "contradiction" || p.ExpectedRelationship == "supersession" {
			conflicts++
		} else {
			nonConflicts++
		}
	}
	assert.Greater(t, conflicts, 0, "dataset needs genuine conflict examples")
	assert.Greater(t, nonConflicts, 0, "dataset needs non-conflict examples")
}

func TestFormatMetrics(t *testing.T) {
	results := []EvalResult{
		{Label: "good", ConflictExpected: true, ConflictActual: true, Correct: true, ExpectedRelationship: "contradiction", ActualRelationship: "contradiction"},
		{Label: "bad", ConflictExpected: false, ConflictActual: true, Correct: false, ExpectedRelationship: "complementary", ActualRelationship: "contradiction", Explanation: "wrong"},
	}
	m := ComputeMetrics(results)
	s := FormatMetrics(m, results)
	assert.Contains(t, s, "Precision")
	assert.Contains(t, s, "Recall")
	assert.Contains(t, s, "Misclassifications")
	assert.Contains(t, s, "[bad]")
}
