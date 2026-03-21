package quality

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/ashita-ai/akashi/internal/model"
)

func strPtr(s string) *string { return &s }

// repeat returns a string of n copies of ch.
func repeat(ch byte, n int) string {
	return strings.Repeat(string(ch), n)
}

func TestScore_ZeroInput(t *testing.T) {
	d := model.TraceDecision{}
	assert.Equal(t, float32(0.0), Score(d, false), "empty decision should score 0")
}

func TestScore_MaximumScore(t *testing.T) {
	// Every factor at its maximum tier, including precedent_ref.
	d := model.TraceDecision{
		DecisionType: "architecture",                        // standard type → 0.10
		Outcome:      "chose Redis for session caching now", // >20 chars → 0.05
		Confidence:   0.85,                                  // mid-range → 0.15
		Reasoning:    strPtr(repeat('x', 101)),              // >100 chars → 0.25
		Alternatives: []model.TraceAlternative{
			{Label: "a"},
			{Label: "b", RejectionReason: strPtr("not viable because of latency overhead issues")},
			{Label: "c", RejectionReason: strPtr("rejected due to licensing incompatibility")},
			{Label: "d", RejectionReason: strPtr("does not meet security requirements for production")},
		}, // >=3 substantive rejections → 0.20
		Evidence: []model.TraceEvidence{
			{SourceType: "document", Content: "evidence one"},
			{SourceType: "api_response", Content: "evidence two"},
		}, // >=2 evidence → 0.15
	}
	assert.InDelta(t, float32(1.0), Score(d, true), 0.001, "fully populated decision with precedent_ref should score 1.0")
}

func TestScore_MaximumScoreWithoutPrecedent(t *testing.T) {
	// Every content factor at maximum, but no precedent_ref → 0.90.
	d := model.TraceDecision{
		DecisionType: "architecture",
		Outcome:      "chose Redis for session caching now",
		Confidence:   0.85,
		Reasoning:    strPtr(repeat('x', 101)),
		Alternatives: []model.TraceAlternative{
			{Label: "a"},
			{Label: "b", RejectionReason: strPtr("not viable because of latency overhead issues")},
			{Label: "c", RejectionReason: strPtr("rejected due to licensing incompatibility")},
			{Label: "d", RejectionReason: strPtr("does not meet security requirements for production")},
		},
		Evidence: []model.TraceEvidence{
			{SourceType: "document", Content: "evidence one"},
			{SourceType: "api_response", Content: "evidence two"},
		},
	}
	assert.InDelta(t, float32(0.90), Score(d, false), 0.001, "fully populated decision without precedent_ref should score 0.90")
}

// ---------------------------------------------------------------------------
// Factor isolation tests: set only one factor at a time, verify its
// contribution in isolation.
// ---------------------------------------------------------------------------

func TestScore_Factor1_ConfidenceMidRange(t *testing.T) {
	d := model.TraceDecision{Confidence: 0.50}
	assert.InDelta(t, float32(0.15), Score(d, false), 0.001)
}

func TestScore_Factor1_ConfidenceEdge(t *testing.T) {
	// Values at the boundary of mid-range fall into the edge tier.
	d := model.TraceDecision{Confidence: 0.05}
	assert.InDelta(t, float32(0.10), Score(d, false), 0.001,
		"confidence == 0.05 is not > 0.05, so falls to edge tier")
}

func TestScore_Factor2_ReasoningLong(t *testing.T) {
	d := model.TraceDecision{Reasoning: strPtr(repeat('a', 101))}
	assert.InDelta(t, float32(0.25), Score(d, false), 0.001)
}

func TestScore_Factor3_SubstantiveRejections(t *testing.T) {
	// Three non-selected alternatives with substantive rejection reasons (>20 chars).
	d := model.TraceDecision{
		Alternatives: []model.TraceAlternative{
			{Label: "a"},
			{Label: "b", RejectionReason: strPtr("this option was rejected for good reason")},
			{Label: "c", RejectionReason: strPtr("too slow for production usage patterns")},
			{Label: "d", RejectionReason: strPtr("licensing issues prevent adoption here")},
		},
	}
	assert.InDelta(t, float32(0.20), Score(d, false), 0.001)
}

func TestScore_Factor3_AlternativesWithoutRejections_NoCredit(t *testing.T) {
	// Three alternatives but no rejection reasons → no credit (anti-gaming).
	d := model.TraceDecision{
		Alternatives: []model.TraceAlternative{
			{Label: "a"},
			{Label: "b"},
			{Label: "c"},
		},
	}
	assert.InDelta(t, float32(0.0), Score(d, false), 0.001,
		"alternatives without rejection reasons should not contribute to score")
}

func TestScore_Factor3_TwoSubstantiveRejections(t *testing.T) {
	// Both alternatives have substantive rejection reasons (>20 chars).
	d := model.TraceDecision{
		Alternatives: []model.TraceAlternative{
			{Label: "a", RejectionReason: strPtr("this option was rejected for reason one here")},
			{Label: "b", RejectionReason: strPtr("this option was rejected for good reason")},
		},
	}
	// 2 substantive rejections → 0.10 + 0.05 = 0.15
	assert.InDelta(t, float32(0.15), Score(d, false), 0.001)
}

func TestScore_Factor4_TwoEvidence(t *testing.T) {
	d := model.TraceDecision{
		Evidence: []model.TraceEvidence{
			{SourceType: "document", Content: "a"},
			{SourceType: "api_response", Content: "b"},
		},
	}
	assert.InDelta(t, float32(0.15), Score(d, false), 0.001)
}

func TestScore_Factor5_StandardType(t *testing.T) {
	d := model.TraceDecision{DecisionType: "security"}
	assert.InDelta(t, float32(0.10), Score(d, false), 0.001)
}

func TestScore_Factor6_SubstantiveOutcome(t *testing.T) {
	d := model.TraceDecision{Outcome: "chose Redis for session caching now"} // >20 chars
	assert.InDelta(t, float32(0.05), Score(d, false), 0.001)
}

func TestScore_Factor7_PrecedentRef(t *testing.T) {
	d := model.TraceDecision{}
	assert.InDelta(t, float32(0.10), Score(d, true), 0.001)
}

func TestScore_Factor7_NoPrecedentRef(t *testing.T) {
	d := model.TraceDecision{}
	assert.InDelta(t, float32(0.0), Score(d, false), 0.001)
}

// ---------------------------------------------------------------------------
// Confidence boundary tests
// ---------------------------------------------------------------------------

func TestScore_ConfidenceBoundaries(t *testing.T) {
	tests := []struct {
		name       string
		confidence float32
		want       float32
	}{
		{"exactly 0", 0.0, 0.0},          // not > 0 → no credit
		{"exactly 1", 1.0, 0.0},          // not < 1 → no credit
		{"exactly 0.05", 0.05, 0.10},     // > 0 && < 1 but not > 0.05 → edge tier
		{"exactly 0.95", 0.95, 0.10},     // > 0 && < 1 but not < 0.95 → edge tier
		{"just above 0.05", 0.06, 0.15},  // > 0.05 && < 0.95 → mid-range
		{"just below 0.95", 0.94, 0.15},  // > 0.05 && < 0.95 → mid-range
		{"just above 0", 0.01, 0.10},     // edge tier
		{"just below 1", 0.99, 0.10},     // edge tier
		{"mid-range center", 0.50, 0.15}, // mid-range
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := model.TraceDecision{Confidence: tt.confidence}
			assert.InDelta(t, tt.want, Score(d, false), 0.001)
		})
	}
}

// ---------------------------------------------------------------------------
// Reasoning length boundary tests
// ---------------------------------------------------------------------------

func TestScore_ReasoningBoundaries(t *testing.T) {
	tests := []struct {
		name string
		len  int
		want float32
	}{
		{"empty string", 0, 0.0},
		{"1 char", 1, 0.0},
		{"exactly 20 chars", 20, 0.0},    // not > 20
		{"21 chars", 21, 0.10},           // > 20
		{"exactly 50 chars", 50, 0.10},   // not > 50
		{"51 chars", 51, 0.20},           // > 50
		{"exactly 100 chars", 100, 0.20}, // not > 100
		{"101 chars", 101, 0.25},         // > 100
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := repeat('x', tt.len)
			d := model.TraceDecision{Reasoning: &s}
			assert.InDelta(t, tt.want, Score(d, false), 0.001)
		})
	}
}

func TestScore_ReasoningNil(t *testing.T) {
	d := model.TraceDecision{Reasoning: nil}
	// No reasoning contribution.
	assert.InDelta(t, float32(0.0), Score(d, false), 0.001)
}

func TestScore_ReasoningWhitespaceOnly(t *testing.T) {
	// 30 spaces, but after TrimSpace the length is 0 → no credit.
	spaces := strings.Repeat(" ", 30)
	d := model.TraceDecision{Reasoning: &spaces}
	assert.InDelta(t, float32(0.0), Score(d, false), 0.001)
}

// ---------------------------------------------------------------------------
// Substantive rejection tests (replaces old alternatives count tests)
// ---------------------------------------------------------------------------

func TestScore_SubstantiveRejectionCount(t *testing.T) {
	makeAltsWithRejections := func(n int) []model.TraceAlternative {
		alts := []model.TraceAlternative{{Label: "selected"}}
		for i := range n {
			alts = append(alts, model.TraceAlternative{
				Label:           repeat('a'+byte(i%26), 1),
				RejectionReason: strPtr("this alternative was rejected because of reason " + repeat('x', 10)),
			})
		}
		return alts
	}

	tests := []struct {
		name  string
		count int
		want  float32
	}{
		{"0 substantive rejections", 0, 0.0},
		{"1 substantive rejection", 1, 0.10},
		{"2 substantive rejections", 2, 0.15},
		{"3 substantive rejections", 3, 0.20},
		{"5 substantive rejections", 5, 0.20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := model.TraceDecision{Alternatives: makeAltsWithRejections(tt.count)}
			assert.InDelta(t, tt.want, Score(d, false), 0.001)
		})
	}
}

// ---------------------------------------------------------------------------
// Rejection reason edge cases
// ---------------------------------------------------------------------------

func TestScore_RejectionReasonTooShort(t *testing.T) {
	d := model.TraceDecision{
		Alternatives: []model.TraceAlternative{
			{Label: "a", RejectionReason: strPtr("too short")}, // 9 chars, not > 20
		},
	}
	// rejection too short → 0
	assert.InDelta(t, float32(0.0), Score(d, false), 0.001)
}

func TestScore_RejectionReasonExactly20(t *testing.T) {
	d := model.TraceDecision{
		Alternatives: []model.TraceAlternative{
			{Label: "a", RejectionReason: strPtr(repeat('x', 20))}, // 20 chars, not > 20
		},
	}
	assert.InDelta(t, float32(0.0), Score(d, false), 0.001)
}

func TestScore_RejectionReasonExactly21(t *testing.T) {
	d := model.TraceDecision{
		Alternatives: []model.TraceAlternative{
			{Label: "a", RejectionReason: strPtr(repeat('x', 21))}, // 21 chars, > 20 → credit
		},
	}
	// 1 substantive rejection → 0.10
	assert.InDelta(t, float32(0.10), Score(d, false), 0.001)
}

func TestScore_RejectionReasonNil(t *testing.T) {
	d := model.TraceDecision{
		Alternatives: []model.TraceAlternative{
			{Label: "a", RejectionReason: nil},
		},
	}
	assert.InDelta(t, float32(0.0), Score(d, false), 0.001)
}

func TestScore_RejectionReasonWhitespace(t *testing.T) {
	// 25 chars of whitespace trims to 0 → no credit.
	ws := strings.Repeat(" ", 25)
	d := model.TraceDecision{
		Alternatives: []model.TraceAlternative{
			{Label: "a", RejectionReason: &ws},
		},
	}
	assert.InDelta(t, float32(0.0), Score(d, false), 0.001)
}

func TestScore_RejectionReasonMixed(t *testing.T) {
	// Two non-selected alternatives: one without rejection, one with substantive rejection.
	d := model.TraceDecision{
		Alternatives: []model.TraceAlternative{
			{Label: "a", RejectionReason: nil},
			{Label: "b", RejectionReason: strPtr("this option was rejected for good reason")},
		},
	}
	// 1 substantive rejection → 0.10
	assert.InDelta(t, float32(0.10), Score(d, false), 0.001)
}

// ---------------------------------------------------------------------------
// Evidence count boundary tests
// ---------------------------------------------------------------------------

func TestScore_EvidenceCount(t *testing.T) {
	makeEvidence := func(n int) []model.TraceEvidence {
		ev := make([]model.TraceEvidence, n)
		for i := range ev {
			ev[i] = model.TraceEvidence{SourceType: "document", Content: "content"}
		}
		return ev
	}

	tests := []struct {
		name  string
		count int
		want  float32
	}{
		{"0 evidence", 0, 0.0},
		{"1 evidence", 1, 0.10},
		{"2 evidence", 2, 0.15},
		{"5 evidence", 5, 0.15},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := model.TraceDecision{Evidence: makeEvidence(tt.count)}
			assert.InDelta(t, tt.want, Score(d, false), 0.001)
		})
	}
}

// ---------------------------------------------------------------------------
// Decision type tests
// ---------------------------------------------------------------------------

func TestScore_NonStandardDecisionType(t *testing.T) {
	d := model.TraceDecision{DecisionType: "custom_workflow"}
	assert.InDelta(t, float32(0.0), Score(d, false), 0.001)
}

func TestScore_AllStandardDecisionTypes(t *testing.T) {
	for dt := range StandardDecisionTypes {
		t.Run(dt, func(t *testing.T) {
			d := model.TraceDecision{DecisionType: dt}
			assert.InDelta(t, float32(0.10), Score(d, false), 0.001)
		})
	}
}

// ---------------------------------------------------------------------------
// Outcome boundary tests
// ---------------------------------------------------------------------------

func TestScore_OutcomeBoundaries(t *testing.T) {
	tests := []struct {
		name    string
		outcome string
		want    float32
	}{
		{"empty", "", 0.0},
		{"exactly 20 chars", repeat('x', 20), 0.0},
		{"21 chars", repeat('x', 21), 0.05},
		{"whitespace padded to 25 but trimmed to 15", "   " + repeat('x', 15) + "       ", 0.0},
		{"whitespace padded to 30 with 21 content", "    " + repeat('x', 21) + "     ", 0.05},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := model.TraceDecision{Outcome: tt.outcome}
			assert.InDelta(t, tt.want, Score(d, false), 0.001)
		})
	}
}

// ---------------------------------------------------------------------------
// Composite scoring: verify additive behavior of multiple factors.
// ---------------------------------------------------------------------------

func TestScore_TwoFactorsCombined(t *testing.T) {
	// Confidence mid-range (0.15) + standard type (0.10) = 0.25
	d := model.TraceDecision{
		DecisionType: "trade_off",
		Confidence:   0.70,
	}
	assert.InDelta(t, float32(0.25), Score(d, false), 0.001)
}

func TestScore_ThreeFactorsCombined(t *testing.T) {
	// Confidence mid-range (0.15) + reasoning >100 (0.25) + 2 evidence (0.15) = 0.55
	d := model.TraceDecision{
		Confidence: 0.60,
		Reasoning:  strPtr(repeat('r', 101)),
		Evidence: []model.TraceEvidence{
			{SourceType: "document", Content: "a"},
			{SourceType: "document", Content: "b"},
		},
	}
	assert.InDelta(t, float32(0.55), Score(d, false), 0.001)
}

// ---------------------------------------------------------------------------
// Uniform scoring: score is independent of decision type.
// This is a critical invariant — changing the formula per type would break
// aggregate comparability and create stored score discontinuities.
// ---------------------------------------------------------------------------

func TestScore_UniformAcrossTypes(t *testing.T) {
	// Same content, different types → same score.
	d := model.TraceDecision{
		Outcome:    "root cause is a race condition in the event buffer flush",
		Confidence: 0.70,
		Reasoning:  strPtr(repeat('r', 101)),
	}

	types := []string{"investigation", "security", "architecture", "planning", "code_review"}
	scores := make(map[string]float32)
	for _, dt := range types {
		d.DecisionType = dt
		scores[dt] = Score(d, false)
	}

	// All standard types get type credit (0.10), so all should be equal.
	first := scores[types[0]]
	for _, dt := range types[1:] {
		assert.InDelta(t, first, scores[dt], 0.001,
			"%s and %s should have the same score with identical content", types[0], dt)
	}
}

// ---------------------------------------------------------------------------
// StandardDecisionTypes map completeness
// ---------------------------------------------------------------------------

func TestStandardDecisionTypes_Contains(t *testing.T) {
	expected := []string{
		"model_selection", "architecture", "data_source", "error_handling",
		"feature_scope", "trade_off", "deployment", "security",
		"code_review", "investigation", "planning", "assessment",
	}
	assert.Equal(t, len(expected), len(StandardDecisionTypes),
		"StandardDecisionTypes should have exactly %d entries", len(expected))
	for _, dt := range expected {
		assert.True(t, StandardDecisionTypes[dt], "%q should be a standard decision type", dt)
	}
}

func TestStandardDecisionTypes_ExcludesUnknown(t *testing.T) {
	bogus := []string{"", "unknown", "custom", "MODEL_SELECTION", "Architecture"}
	for _, dt := range bogus {
		assert.False(t, StandardDecisionTypes[dt], "%q should not be a standard decision type", dt)
	}
}

// ---------------------------------------------------------------------------
// ComputeOutcomeScore tests
// ---------------------------------------------------------------------------

func TestComputeOutcomeScore_NoAssessments(t *testing.T) {
	s := model.AssessmentSummary{Total: 0}
	assert.Nil(t, ComputeOutcomeScore(s), "no assessments should return nil")
}

func TestComputeOutcomeScore_AllCorrect(t *testing.T) {
	s := model.AssessmentSummary{Total: 3, Correct: 3}
	score := ComputeOutcomeScore(s)
	assert.NotNil(t, score)
	assert.InDelta(t, float32(1.0), *score, 0.001)
}

func TestComputeOutcomeScore_AllIncorrect(t *testing.T) {
	s := model.AssessmentSummary{Total: 2, Incorrect: 2}
	score := ComputeOutcomeScore(s)
	assert.NotNil(t, score)
	assert.InDelta(t, float32(0.0), *score, 0.001)
}

func TestComputeOutcomeScore_Mixed(t *testing.T) {
	// 1 correct + 1 partial + 1 incorrect = (1 + 0.5) / 3 = 0.5
	s := model.AssessmentSummary{Total: 3, Correct: 1, PartiallyCorrect: 1, Incorrect: 1}
	score := ComputeOutcomeScore(s)
	assert.NotNil(t, score)
	assert.InDelta(t, float32(0.5), *score, 0.001)
}

func TestComputeOutcomeScore_AllPartial(t *testing.T) {
	// 4 partial = (0 + 0.5*4) / 4 = 0.5
	s := model.AssessmentSummary{Total: 4, PartiallyCorrect: 4}
	score := ComputeOutcomeScore(s)
	assert.NotNil(t, score)
	assert.InDelta(t, float32(0.5), *score, 0.001)
}

// ---------------------------------------------------------------------------
// Completeness profile tests (profiles drive tips, not scoring)
// ---------------------------------------------------------------------------

func TestProfileFor_DefaultOverride(t *testing.T) {
	overrides := map[string]CompletenessProfile{
		"architecture": {MinEvidence: 3, AlternativesExpected: true, MaxConfidenceNoEvidence: 0.70},
	}
	p := ProfileFor("architecture", overrides)
	assert.Equal(t, 3, p.MinEvidence, "override should take precedence")

	// Non-overridden type falls to built-in default.
	p2 := ProfileFor("investigation", overrides)
	assert.Equal(t, 0, p2.MinEvidence, "investigation should use built-in default")

	// Unknown type falls to DefaultProfile.
	p3 := ProfileFor("custom_thing", overrides)
	assert.Equal(t, DefaultProfile, p3)
}

func TestProfileFor_NilOverrides(t *testing.T) {
	p := ProfileFor("security", nil)
	assert.Equal(t, DefaultProfiles["security"], p)

	p2 := ProfileFor("unknown_type", nil)
	assert.Equal(t, DefaultProfile, p2)
}

// ---------------------------------------------------------------------------
// Type expectation tests
// ---------------------------------------------------------------------------

func TestExpectationFor_KnownTypes(t *testing.T) {
	tests := []struct {
		decisionType string
		wantMin      float64
	}{
		{"investigation", 0.30},
		{"planning", 0.30},
		{"assessment", 0.30},
		{"code_review", 0.45},
		{"architecture", 0.55},
		{"trade_off", 0.55},
		{"security", 0.60},
	}
	for _, tt := range tests {
		t.Run(tt.decisionType, func(t *testing.T) {
			e := ExpectationFor(tt.decisionType)
			assert.InDelta(t, tt.wantMin, e.ExpectedMin, 0.001)
		})
	}
}

func TestExpectationFor_UnknownType(t *testing.T) {
	e := ExpectationFor("custom_workflow")
	assert.InDelta(t, 0.40, e.ExpectedMin, 0.001, "unknown types should get default expectation of 0.40")
}

func TestExpectationFor_AllStandardTypesHaveExpectations(t *testing.T) {
	// Verify that every type with an expectation is a standard type.
	for dt := range DefaultExpectations {
		assert.True(t, StandardDecisionTypes[dt],
			"type %q has an expectation but is not a standard decision type", dt)
	}
}
