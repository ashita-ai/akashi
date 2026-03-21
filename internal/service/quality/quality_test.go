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
			{Label: "a", Selected: true},
			{Label: "b", Selected: false, RejectionReason: strPtr("not viable because of latency overhead issues")},
			{Label: "c", Selected: false, RejectionReason: strPtr("rejected due to licensing incompatibility")},
			{Label: "d", Selected: false, RejectionReason: strPtr("does not meet security requirements for production")},
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
			{Label: "a", Selected: true},
			{Label: "b", Selected: false, RejectionReason: strPtr("not viable because of latency overhead issues")},
			{Label: "c", Selected: false, RejectionReason: strPtr("rejected due to licensing incompatibility")},
			{Label: "d", Selected: false, RejectionReason: strPtr("does not meet security requirements for production")},
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
			{Label: "a", Selected: true},
			{Label: "b", Selected: false, RejectionReason: strPtr("this option was rejected for good reason")},
			{Label: "c", Selected: false, RejectionReason: strPtr("too slow for production usage patterns")},
			{Label: "d", Selected: false, RejectionReason: strPtr("licensing issues prevent adoption here")},
		},
	}
	assert.InDelta(t, float32(0.20), Score(d, false), 0.001)
}

func TestScore_Factor3_AlternativesWithoutRejections_NoCredit(t *testing.T) {
	// Three alternatives but no rejection reasons → no credit (anti-gaming).
	d := model.TraceDecision{
		Alternatives: []model.TraceAlternative{
			{Label: "a", Selected: true},
			{Label: "b", Selected: false},
			{Label: "c", Selected: false},
		},
	}
	assert.InDelta(t, float32(0.0), Score(d, false), 0.001,
		"alternatives without rejection reasons should not contribute to score")
}

func TestScore_Factor3_SelectedAlternativeIgnored(t *testing.T) {
	// Selected alternatives are ignored — only non-selected with rejections count.
	d := model.TraceDecision{
		Alternatives: []model.TraceAlternative{
			{Label: "a", Selected: true, RejectionReason: strPtr("this was selected so rejection is irrelevant")},
			{Label: "b", Selected: false, RejectionReason: strPtr("this option was rejected for good reason")},
		},
	}
	// 1 substantive rejection → 0.10
	assert.InDelta(t, float32(0.10), Score(d, false), 0.001)
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
		alts := []model.TraceAlternative{{Label: "selected", Selected: true}}
		for i := range n {
			alts = append(alts, model.TraceAlternative{
				Label:           repeat('a'+byte(i%26), 1),
				Selected:        false,
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
			{Label: "a", Selected: false, RejectionReason: strPtr("too short")}, // 9 chars, not > 20
		},
	}
	// rejection too short → 0
	assert.InDelta(t, float32(0.0), Score(d, false), 0.001)
}

func TestScore_RejectionReasonExactly20(t *testing.T) {
	d := model.TraceDecision{
		Alternatives: []model.TraceAlternative{
			{Label: "a", Selected: false, RejectionReason: strPtr(repeat('x', 20))}, // 20 chars, not > 20
		},
	}
	assert.InDelta(t, float32(0.0), Score(d, false), 0.001)
}

func TestScore_RejectionReasonExactly21(t *testing.T) {
	d := model.TraceDecision{
		Alternatives: []model.TraceAlternative{
			{Label: "a", Selected: false, RejectionReason: strPtr(repeat('x', 21))}, // 21 chars, > 20 → credit
		},
	}
	// 1 substantive rejection → 0.10
	assert.InDelta(t, float32(0.10), Score(d, false), 0.001)
}

func TestScore_RejectionReasonNil(t *testing.T) {
	d := model.TraceDecision{
		Alternatives: []model.TraceAlternative{
			{Label: "a", Selected: false, RejectionReason: nil},
		},
	}
	assert.InDelta(t, float32(0.0), Score(d, false), 0.001)
}

func TestScore_RejectionReasonWhitespace(t *testing.T) {
	// 25 chars of whitespace trims to 0 → no credit.
	ws := strings.Repeat(" ", 25)
	d := model.TraceDecision{
		Alternatives: []model.TraceAlternative{
			{Label: "a", Selected: false, RejectionReason: &ws},
		},
	}
	assert.InDelta(t, float32(0.0), Score(d, false), 0.001)
}

func TestScore_RejectionReasonMixed(t *testing.T) {
	// Two non-selected alternatives: one without rejection, one with substantive rejection.
	d := model.TraceDecision{
		Alternatives: []model.TraceAlternative{
			{Label: "a", Selected: false, RejectionReason: nil},
			{Label: "b", Selected: false, RejectionReason: strPtr("this option was rejected for good reason")},
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
// Completeness profile tests
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
// Profile-aware scoring: investigation (no alts, no evidence expected)
// ---------------------------------------------------------------------------

func TestScore_Investigation_ReasoningWeightRedistributed(t *testing.T) {
	// Investigation profile: alts_expected=false, min_evidence=0.
	// Reasoning weight = 0.25 + 0.20 + 0.15 = 0.60.
	// Well-reasoned investigation should score high without alts/evidence.
	d := model.TraceDecision{
		DecisionType: "investigation",
		Outcome:      "root cause is a race condition in the event buffer flush path",
		Confidence:   0.70,
		Reasoning:    strPtr(repeat('r', 101)),
	}
	// Confidence mid-range: 0.15
	// Reasoning >100 chars @ 0.60 weight: 0.60
	// Standard type: 0.10
	// Outcome >20 chars: 0.05
	// Total: 0.90
	assert.InDelta(t, float32(0.90), Score(d, false), 0.001,
		"investigation with good reasoning should reach 0.90 without alternatives or evidence")
}

func TestScore_Investigation_MaxWithPrecedent(t *testing.T) {
	d := model.TraceDecision{
		DecisionType: "investigation",
		Outcome:      "root cause is a race condition in the event buffer flush path",
		Confidence:   0.70,
		Reasoning:    strPtr(repeat('r', 101)),
	}
	assert.InDelta(t, float32(1.0), Score(d, true), 0.001,
		"investigation with reasoning + precedent should reach 1.0")
}

func TestScore_Investigation_EmptyStillScoresLow(t *testing.T) {
	// An empty investigation should not get free credit — redistribution only
	// helps when reasoning is actually provided.
	d := model.TraceDecision{DecisionType: "investigation"}
	assert.InDelta(t, float32(0.10), Score(d, false), 0.001,
		"empty investigation should score only the type factor")
}

func TestScore_Investigation_HighConfidenceNoEvidence_Penalized(t *testing.T) {
	// Investigation max_confidence_no_evidence = 0.90.
	// Confidence 0.92 with no evidence should be capped at edge tier (0.10).
	d := model.TraceDecision{
		DecisionType: "investigation",
		Confidence:   0.92,
	}
	// Confidence: 0.92 > 0.90 and no evidence → capped to 0.10
	// Type: 0.10
	assert.InDelta(t, float32(0.20), Score(d, false), 0.001)
}

func TestScore_Investigation_HighConfidenceWithEvidence_NoPenalty(t *testing.T) {
	// Even though confidence > max_confidence_no_evidence, having evidence
	// disables the penalty.
	d := model.TraceDecision{
		DecisionType: "investigation",
		Confidence:   0.92,
		Evidence: []model.TraceEvidence{
			{SourceType: "tool_output", Content: "stack trace"},
		},
	}
	// Confidence: 0.92 with evidence → mid-range 0.15 (no penalty)
	// Evidence: investigation has min_evidence=0, so evidence factor skipped
	// Type: 0.10
	assert.InDelta(t, float32(0.25), Score(d, false), 0.001)
}

// ---------------------------------------------------------------------------
// Profile-aware scoring: security (strict requirements)
// ---------------------------------------------------------------------------

func TestScore_Security_HighConfidenceNoEvidence_Penalized(t *testing.T) {
	// Security max_confidence_no_evidence = 0.75.
	// Confidence 0.80 with no evidence should be capped at edge tier.
	d := model.TraceDecision{
		DecisionType: "security",
		Confidence:   0.80,
		Reasoning:    strPtr(repeat('r', 101)),
	}
	// Confidence: 0.80 > 0.75 and no evidence → capped to 0.10
	// Reasoning: 0.25 (base weight, alts and evidence both expected)
	// Type: 0.10
	// Total: 0.45
	assert.InDelta(t, float32(0.45), Score(d, false), 0.001)
}

func TestScore_Security_HighConfidenceWithEvidence_NoPenalty(t *testing.T) {
	d := model.TraceDecision{
		DecisionType: "security",
		Confidence:   0.80,
		Reasoning:    strPtr(repeat('r', 101)),
		Evidence: []model.TraceEvidence{
			{SourceType: "document", Content: "OWASP guideline"},
			{SourceType: "tool_output", Content: "security scan passed"},
		},
	}
	// Confidence: 0.80 with evidence → mid-range 0.15
	// Reasoning: 0.25
	// Evidence: 2 items → 0.15
	// Type: 0.10
	// Total: 0.65
	assert.InDelta(t, float32(0.65), Score(d, false), 0.001)
}

func TestScore_Security_MaxScore(t *testing.T) {
	d := model.TraceDecision{
		DecisionType: "security",
		Outcome:      "enforced Argon2id for all API key hashing with 64MB memory cost",
		Confidence:   0.75, // exactly at threshold, not above
		Reasoning:    strPtr(repeat('r', 101)),
		Alternatives: []model.TraceAlternative{
			{Label: "selected", Selected: true},
			{Label: "a", Selected: false, RejectionReason: strPtr("insufficient for production workloads")},
			{Label: "b", Selected: false, RejectionReason: strPtr("deprecated algorithm with known weaknesses")},
			{Label: "c", Selected: false, RejectionReason: strPtr("excessive memory cost for containerized deploys")},
		},
		Evidence: []model.TraceEvidence{
			{SourceType: "document", Content: "OWASP password storage cheat sheet"},
			{SourceType: "tool_output", Content: "benchmark: 250ms per hash at 64MB"},
		},
	}
	// Confidence 0.75 > 0.05 && < 0.95 → mid-range 0.15.
	// 0.75 is NOT > 0.75 (strict inequality), so no penalty.
	assert.InDelta(t, float32(1.0), Score(d, true), 0.001,
		"fully documented security decision with precedent should reach 1.0")
}

// ---------------------------------------------------------------------------
// Profile-aware scoring: planning (no alts, no evidence expected)
// ---------------------------------------------------------------------------

func TestScore_Planning_ReasoningHeavy(t *testing.T) {
	// Planning profile: same redistribution as investigation.
	d := model.TraceDecision{
		DecisionType: "planning",
		Outcome:      "split migration into three phases to reduce blast radius",
		Confidence:   0.60,
		Reasoning:    strPtr(repeat('r', 101)),
	}
	// Reasoning weight = 0.25 + 0.20 + 0.15 = 0.60
	// Confidence mid-range: 0.15
	// Reasoning >100: 0.60
	// Type: 0.10
	// Outcome >20: 0.05
	// Total: 0.90
	assert.InDelta(t, float32(0.90), Score(d, false), 0.001)
}

// ---------------------------------------------------------------------------
// Profile-aware scoring: code_review (alts expected, min_evidence=1)
// ---------------------------------------------------------------------------

func TestScore_CodeReview_MatchesDefaultForAltsAndEvidence(t *testing.T) {
	// code_review profile: min_evidence=1, alts_expected=true.
	// Same base weights as default (no redistribution).
	d := model.TraceDecision{
		DecisionType: "code_review",
		Outcome:      "approved with minor nits on error handling",
		Confidence:   0.70,
		Reasoning:    strPtr(repeat('r', 101)),
		Alternatives: []model.TraceAlternative{
			{Label: "selected", Selected: true},
			{Label: "a", Selected: false, RejectionReason: strPtr("refactor is too large for this PR cycle")},
		},
		Evidence: []model.TraceEvidence{
			{SourceType: "tool_output", Content: "tests pass, coverage at 82%"},
		},
	}
	// Confidence: 0.15, Reasoning: 0.25, Alts (1): 0.10, Evidence (1): 0.10, Type: 0.10, Outcome: 0.05
	assert.InDelta(t, float32(0.75), Score(d, false), 0.001)
}

func TestScore_CodeReview_HighConfidenceNoEvidence_Penalized(t *testing.T) {
	// code_review max_confidence_no_evidence = 0.85.
	d := model.TraceDecision{
		DecisionType: "code_review",
		Confidence:   0.90,
	}
	// 0.90 > 0.85 and no evidence → capped to 0.10
	// Type: 0.10
	assert.InDelta(t, float32(0.20), Score(d, false), 0.001)
}

// ---------------------------------------------------------------------------
// ScoreWithProfile: direct profile usage
// ---------------------------------------------------------------------------

func TestScoreWithProfile_CustomProfile(t *testing.T) {
	// Custom profile: no alternatives, no evidence, no confidence penalty.
	profile := CompletenessProfile{
		MinEvidence:             0,
		AlternativesExpected:    false,
		MaxConfidenceNoEvidence: 1.0,
	}
	d := model.TraceDecision{
		DecisionType: "custom_type",
		Outcome:      "made a decision about something important",
		Confidence:   0.60,
		Reasoning:    strPtr(repeat('r', 101)),
	}
	// reasoningMax = 0.25 + 0.20 + 0.15 = 0.60
	// Confidence: 0.15, Reasoning: 0.60, Type: 0.00 (custom), Outcome: 0.05
	// Total: 0.80
	assert.InDelta(t, float32(0.80), ScoreWithProfile(d, false, profile), 0.001)
}

// ---------------------------------------------------------------------------
// Default profile preserves backward compatibility for non-profiled types
// ---------------------------------------------------------------------------

func TestScore_UnprofiledType_MatchesOldBehavior(t *testing.T) {
	// "trade_off" has no specific profile → DefaultProfile.
	// Behavior should be identical to the pre-profile scoring.
	d := model.TraceDecision{
		DecisionType: "trade_off",
		Outcome:      "chose latency over throughput for user-facing endpoint",
		Confidence:   0.70,
		Reasoning:    strPtr(repeat('r', 101)),
		Alternatives: []model.TraceAlternative{
			{Label: "selected", Selected: true},
			{Label: "a", Selected: false, RejectionReason: strPtr("throughput optimization hurts p99 latency")},
			{Label: "b", Selected: false, RejectionReason: strPtr("hybrid approach adds too much complexity")},
			{Label: "c", Selected: false, RejectionReason: strPtr("caching layer introduces consistency problems")},
		},
		Evidence: []model.TraceEvidence{
			{SourceType: "tool_output", Content: "load test results"},
			{SourceType: "document", Content: "SLA requirements doc"},
		},
	}
	// All factors at max: 0.15 + 0.25 + 0.20 + 0.15 + 0.10 + 0.05 = 0.90
	assert.InDelta(t, float32(0.90), Score(d, false), 0.001)
	assert.InDelta(t, float32(1.0), Score(d, true), 0.001)
}

// ---------------------------------------------------------------------------
// Reasoning tier scaling with redistributed weight
// ---------------------------------------------------------------------------

func TestScore_Investigation_ReasoningTiers(t *testing.T) {
	// Investigation reasoningMax = 0.60.
	// Verify each tier scales correctly.
	tests := []struct {
		name string
		len  int
		want float32 // just reasoning contribution
	}{
		{"empty", 0, 0.0},
		{"20 chars (not > 20)", 20, 0.0},
		{"21 chars (> 20, 40% tier)", 21, 0.24}, // 0.60 * 0.40
		{"51 chars (> 50, 80% tier)", 51, 0.48}, // 0.60 * 0.80
		{"101 chars (> 100, full)", 101, 0.60},  // 0.60 * 1.00
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := repeat('x', tt.len)
			d := model.TraceDecision{
				DecisionType: "investigation",
				Reasoning:    &s,
			}
			// Score = reasoning + type(0.10). Subtract type to isolate reasoning.
			got := Score(d, false) - 0.10
			assert.InDelta(t, tt.want, got, 0.001)
		})
	}
}
