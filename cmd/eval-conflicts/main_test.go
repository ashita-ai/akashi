package main

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashita-ai/akashi/internal/conflicts"
)

// akashiCorpus is the labelled gold corpus as it stands: 93 contradictions in
// 2772 pairs, an observed base rate of 3.355%. The label-noise correction is
// inadmissible against it for any labeller whose false-flag rate exceeds that.
func akashiCorpus() map[string]int {
	return map[string]int{
		conflicts.GoldRelated:       2017,
		"supersession":              627,
		conflicts.GoldContradiction: 93,
		"unrelated":                 35,
	}
}

// goldResults is a minimal stratified sample: both a contradiction row and a
// negative row, so ProjectCorpusPrecision has something to project.
func goldResults() []conflicts.EvalResult {
	return []conflicts.EvalResult{
		{ExpectedRelationship: conflicts.GoldContradiction, ActualRelationship: conflicts.GoldContradiction},
		{ExpectedRelationship: conflicts.GoldContradiction, ActualRelationship: "complementary"},
		{ExpectedRelationship: conflicts.GoldRelated, ActualRelationship: "complementary"},
		{ExpectedRelationship: conflicts.GoldRelated, ActualRelationship: conflicts.GoldContradiction},
	}
}

// captureStdout runs fn with os.Stdout redirected and returns what it printed.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()

	fn()
	require.NoError(t, w.Close())
	return <-done
}

// The behaviour docs/conflict-detection.md promises operators: an inadmissible
// correction fails the run. Before this, reportGold printed a WARNING and
// returned nothing, so a CI job checking $? saw a green run on a base rate the
// tool had just declared unestablished.
func TestReportGold_InadmissibleCorrectionReturnsError(t *testing.T) {
	// Akashi's own 200-pair re-rate: false-flag rate 5.7% against a 3.355%
	// observed base rate, so the positivity condition fails.
	cal := &conflicts.LabelCalibration{Sensitivity: 0.817, Specificity: 0.943, N: 200}

	var err error
	out := captureStdout(t, func() {
		err = reportGold(goldResults(), "gpt-5", akashiCorpus(), cal)
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not established")
	assert.NotZero(t, exitBaseRateUnestablished, "the error must map to a non-zero exit code")

	// The message names the positivity condition in the operator's terms.
	assert.Contains(t, out, "Positivity condition")
	assert.Contains(t, out, "false-flag rate")

	// The operator paid for the run: the full report prints anyway, and the
	// confusion matrix comes after the warning rather than being suppressed
	// by it.
	warn := strings.Index(out, "WARNING")
	matrix := strings.Index(out, "gold (row)")
	require.NotEqual(t, -1, warn)
	require.NotEqual(t, -1, matrix)
	assert.Less(t, warn, matrix, "exit code signals distrust; it must not truncate the report")
}

// The queue line is the one that gets screenshotted, grepped and pasted into a
// dashboard. A caveat printed below it does not travel with it, so the
// disqualification has to be on the same line as the number it disqualifies —
// which means the correction must be resolved before that line is printed.
func TestReportGold_InadmissibleCorrectionMarksTheQueueLineItself(t *testing.T) {
	cal := &conflicts.LabelCalibration{Sensitivity: 0.817, Specificity: 0.943, N: 200}

	out := captureStdout(t, func() {
		_ = reportGold(goldResults(), "gpt-5", akashiCorpus(), cal)
	})

	var queueLine string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "corpus-projected queue") {
			queueLine = line
			break
		}
	}
	require.NotEmpty(t, queueLine, "the projection headline must still print")
	assert.Contains(t, queueLine, "BASE RATE UNESTABLISHED",
		"the headline carries the precision figure, so it must carry the disqualification too")

	// And the marker must appear before the correction block, not only inside
	// it — otherwise it has not travelled with the number.
	assert.Less(t, strings.Index(out, "BASE RATE UNESTABLISHED"), strings.Index(out, "label-noise correction"))
}

// The converse: an admissible correction must leave the headline clean, or the
// marker becomes noise operators learn to ignore.
func TestReportGold_AdmissibleCorrectionLeavesTheQueueLineUnmarked(t *testing.T) {
	cal := &conflicts.LabelCalibration{Sensitivity: 0.90, Specificity: 0.995, N: 200}

	out := captureStdout(t, func() {
		_ = reportGold(goldResults(), "gpt-5", akashiCorpus(), cal)
	})

	assert.NotContains(t, out, "BASE RATE UNESTABLISHED")
}

func TestReportGold_AdmissibleCorrectionReturnsNil(t *testing.T) {
	// A labeller whose false-flag rate (0.5%) sits below the 3.355% base rate.
	cal := &conflicts.LabelCalibration{Sensitivity: 0.90, Specificity: 0.995, N: 200}

	var err error
	out := captureStdout(t, func() {
		err = reportGold(goldResults(), "gpt-5", akashiCorpus(), cal)
	})

	require.NoError(t, err)
	assert.Contains(t, out, "corrected")
	assert.NotContains(t, out, "WARNING")
}

func TestReportGold_NoCalibrationReturnsNil(t *testing.T) {
	var err error
	out := captureStdout(t, func() {
		err = reportGold(goldResults(), "gpt-5", akashiCorpus(), nil)
	})

	require.NoError(t, err)
	assert.NotContains(t, out, "label-noise correction")
}

// A corpus with no contradiction-labelled rows has no observed base rate to
// correct. That is not a failure, but silence would let an operator who passed
// the flags believe the figures were corrected.
func TestReportGold_ZeroBaseRateExplainsTheSkip(t *testing.T) {
	corpus := map[string]int{conflicts.GoldRelated: 400, "supersession": 100}
	cal := &conflicts.LabelCalibration{Sensitivity: 0.817, Specificity: 0.943, N: 200}

	var err error
	out := captureStdout(t, func() {
		err = reportGold(goldResults(), "gpt-5", corpus, cal)
	})

	require.NoError(t, err, "an absent base rate is not an inadmissible one")
	assert.Contains(t, out, "label-noise correction: SKIPPED")
	assert.Contains(t, out, "none are labelled")
	assert.Contains(t, out, "500 pairs")
}
