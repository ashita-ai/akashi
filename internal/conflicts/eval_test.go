//go:build !lite

package conflicts

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestComputePrecisionRecall_Perfect(t *testing.T) {
	results := []EvalResult{
		{ExpectedLabel: "genuine", Detected: true},
		{ExpectedLabel: "genuine", Detected: true},
		{ExpectedLabel: "related_not_contradicting", Detected: false},
		{ExpectedLabel: "unrelated_false_positive", Detected: false},
	}
	pr := ComputePrecisionRecall(results)
	assert.Equal(t, 2, pr.TruePositives)
	assert.Equal(t, 0, pr.FalsePositives)
	assert.Equal(t, 0, pr.FalseNegatives)
	assert.InDelta(t, 1.0, pr.Precision, 1e-9)
	assert.InDelta(t, 1.0, pr.Recall, 1e-9)
	assert.InDelta(t, 1.0, pr.F1, 1e-9)
}

func TestComputePrecisionRecall_AllFalsePositives(t *testing.T) {
	results := []EvalResult{
		{ExpectedLabel: "related_not_contradicting", Detected: true},
		{ExpectedLabel: "unrelated_false_positive", Detected: true},
	}
	pr := ComputePrecisionRecall(results)
	assert.Equal(t, 0, pr.TruePositives)
	assert.Equal(t, 2, pr.FalsePositives)
	assert.InDelta(t, 0.0, pr.Precision, 1e-9)
	assert.InDelta(t, 0.0, pr.Recall, 1e-9) // no positives to recall
	assert.InDelta(t, 0.0, pr.F1, 1e-9)
}

func TestComputePrecisionRecall_MissedRecall(t *testing.T) {
	results := []EvalResult{
		{ExpectedLabel: "genuine", Detected: true},
		{ExpectedLabel: "genuine", Detected: false}, // missed
		{ExpectedLabel: "related_not_contradicting", Detected: false},
	}
	pr := ComputePrecisionRecall(results)
	assert.Equal(t, 1, pr.TruePositives)
	assert.Equal(t, 0, pr.FalsePositives)
	assert.Equal(t, 1, pr.FalseNegatives)
	assert.InDelta(t, 1.0, pr.Precision, 1e-9)
	assert.InDelta(t, 0.5, pr.Recall, 1e-9)
}

func TestComputePrecisionRecall_Empty(t *testing.T) {
	pr := ComputePrecisionRecall(nil)
	assert.InDelta(t, 0.0, pr.Precision, 1e-9)
	assert.InDelta(t, 0.0, pr.Recall, 1e-9)
	assert.InDelta(t, 0.0, pr.F1, 1e-9)
}

func TestComputePrecisionRecall_Mixed(t *testing.T) {
	// 8 TP, 2 FP, 1 FN -> precision = 8/10 = 0.80, recall = 8/9 ~= 0.889
	var results []EvalResult
	for i := 0; i < 8; i++ {
		results = append(results, EvalResult{ExpectedLabel: "genuine", Detected: true})
	}
	results = append(results, EvalResult{ExpectedLabel: "genuine", Detected: false})                  // FN
	results = append(results, EvalResult{ExpectedLabel: "related_not_contradicting", Detected: true}) // FP
	results = append(results, EvalResult{ExpectedLabel: "unrelated_false_positive", Detected: true})  // FP

	pr := ComputePrecisionRecall(results)
	assert.Equal(t, 8, pr.TruePositives)
	assert.Equal(t, 2, pr.FalsePositives)
	assert.Equal(t, 1, pr.FalseNegatives)
	assert.InDelta(t, 0.80, pr.Precision, 1e-9)
	assert.InDelta(t, 8.0/9.0, pr.Recall, 1e-9)
}
