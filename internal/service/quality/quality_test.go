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
	assert.Equal(t, float32(0.0), Score(d), "empty decision should score 0")
}

func TestScore_MaximumScore(t *testing.T) {
	// Every factor at its maximum tier.
	d := model.TraceDecision{
		DecisionType: "architecture",                        // standard type → 0.10
		Outcome:      "chose Redis for session caching now", // >20 chars → 0.05
		Confidence:   0.85,                                  // mid-range → 0.15
		Reasoning:    strPtr(repeat('x', 101)),              // >100 chars → 0.25
		Alternatives: []model.TraceAlternative{
			{Label: "a", Selected: true},
			{Label: "b", Selected: false, RejectionReason: strPtr("not viable because of latency overhead")}, // >10 chars → 0.10
			{Label: "c", Selected: false},
		}, // >=3 alts → 0.20
		Evidence: []model.TraceEvidence{
			{SourceType: "document", Content: "evidence one"},
			{SourceType: "api_response", Content: "evidence two"},
		}, // >=2 evidence → 0.15
	}
	assert.InDelta(t, float32(1.0), Score(d), 0.001, "fully populated decision should score 1.0")
}

// ---------------------------------------------------------------------------
// Factor isolation tests: set only one factor at a time, verify its
// contribution in isolation.
// ---------------------------------------------------------------------------

func TestScore_Factor1_ConfidenceMidRange(t *testing.T) {
	d := model.TraceDecision{Confidence: 0.50}
	assert.InDelta(t, float32(0.15), Score(d), 0.001)
}

func TestScore_Factor1_ConfidenceEdge(t *testing.T) {
	// Values at the boundary of mid-range fall into the edge tier.
	d := model.TraceDecision{Confidence: 0.05}
	assert.InDelta(t, float32(0.10), Score(d), 0.001,
		"confidence == 0.05 is not > 0.05, so falls to edge tier")
}

func TestScore_Factor2_ReasoningLong(t *testing.T) {
	d := model.TraceDecision{Reasoning: strPtr(repeat('a', 101))}
	assert.InDelta(t, float32(0.25), Score(d), 0.001)
}

func TestScore_Factor3_ThreeAlternatives(t *testing.T) {
	d := model.TraceDecision{
		Alternatives: []model.TraceAlternative{
			{Label: "a"}, {Label: "b"}, {Label: "c"},
		},
	}
	assert.InDelta(t, float32(0.20), Score(d), 0.001)
}

func TestScore_Factor4_RejectionReason(t *testing.T) {
	// One alternative with a substantive (>10 char) rejection reason.
	// Also includes 1 alternative → factor 3 contributes 0.05.
	d := model.TraceDecision{
		Alternatives: []model.TraceAlternative{
			{Label: "x", RejectionReason: strPtr("this option is too slow for production use")},
		},
	}
	// factor 3: 1 alt → 0.05, factor 4: rejection reason → 0.10
	assert.InDelta(t, float32(0.15), Score(d), 0.001)
}

func TestScore_Factor5_TwoEvidence(t *testing.T) {
	d := model.TraceDecision{
		Evidence: []model.TraceEvidence{
			{SourceType: "document", Content: "a"},
			{SourceType: "api_response", Content: "b"},
		},
	}
	assert.InDelta(t, float32(0.15), Score(d), 0.001)
}

func TestScore_Factor6_StandardType(t *testing.T) {
	d := model.TraceDecision{DecisionType: "security"}
	assert.InDelta(t, float32(0.10), Score(d), 0.001)
}

func TestScore_Factor7_SubstantiveOutcome(t *testing.T) {
	d := model.TraceDecision{Outcome: "chose Redis for session caching now"} // >20 chars
	assert.InDelta(t, float32(0.05), Score(d), 0.001)
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
			assert.InDelta(t, tt.want, Score(d), 0.001)
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
			assert.InDelta(t, tt.want, Score(d), 0.001)
		})
	}
}

func TestScore_ReasoningNil(t *testing.T) {
	d := model.TraceDecision{Reasoning: nil}
	// No reasoning contribution.
	assert.InDelta(t, float32(0.0), Score(d), 0.001)
}

func TestScore_ReasoningWhitespaceOnly(t *testing.T) {
	// 30 spaces, but after TrimSpace the length is 0 → no credit.
	spaces := strings.Repeat(" ", 30)
	d := model.TraceDecision{Reasoning: &spaces}
	assert.InDelta(t, float32(0.0), Score(d), 0.001)
}

// ---------------------------------------------------------------------------
// Alternatives count boundary tests
// ---------------------------------------------------------------------------

func TestScore_AlternativesCount(t *testing.T) {
	makeAlts := func(n int) []model.TraceAlternative {
		alts := make([]model.TraceAlternative, n)
		for i := range alts {
			alts[i] = model.TraceAlternative{Label: repeat('a'+byte(i%26), 1)}
		}
		return alts
	}

	tests := []struct {
		name  string
		count int
		want  float32
	}{
		{"0 alternatives", 0, 0.0},
		{"1 alternative", 1, 0.05},
		{"2 alternatives", 2, 0.15},
		{"3 alternatives", 3, 0.20},
		{"5 alternatives", 5, 0.20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := model.TraceDecision{Alternatives: makeAlts(tt.count)}
			assert.InDelta(t, tt.want, Score(d), 0.001)
		})
	}
}

// ---------------------------------------------------------------------------
// Rejection reason edge cases
// ---------------------------------------------------------------------------

func TestScore_RejectionReasonTooShort(t *testing.T) {
	d := model.TraceDecision{
		Alternatives: []model.TraceAlternative{
			{Label: "a", RejectionReason: strPtr("too short")}, // 9 chars, not > 10
		},
	}
	// factor 3: 1 alt → 0.05, factor 4: rejection too short → 0
	assert.InDelta(t, float32(0.05), Score(d), 0.001)
}

func TestScore_RejectionReasonExactly10(t *testing.T) {
	d := model.TraceDecision{
		Alternatives: []model.TraceAlternative{
			{Label: "a", RejectionReason: strPtr("exactly 10")}, // 10 chars, not > 10
		},
	}
	// factor 3: 1 alt → 0.05, factor 4: len == 10 not > 10 → 0
	assert.InDelta(t, float32(0.05), Score(d), 0.001)
}

func TestScore_RejectionReasonExactly11(t *testing.T) {
	d := model.TraceDecision{
		Alternatives: []model.TraceAlternative{
			{Label: "a", RejectionReason: strPtr("exactly 11!")}, // 11 chars, > 10 → credit
		},
	}
	// factor 3: 1 alt → 0.05, factor 4: rejection → 0.10
	assert.InDelta(t, float32(0.15), Score(d), 0.001)
}

func TestScore_RejectionReasonNil(t *testing.T) {
	d := model.TraceDecision{
		Alternatives: []model.TraceAlternative{
			{Label: "a", RejectionReason: nil},
		},
	}
	// factor 3: 1 alt → 0.05, factor 4: nil → 0
	assert.InDelta(t, float32(0.05), Score(d), 0.001)
}

func TestScore_RejectionReasonWhitespace(t *testing.T) {
	// 15 chars of whitespace trims to 0 → no credit.
	ws := strings.Repeat(" ", 15)
	d := model.TraceDecision{
		Alternatives: []model.TraceAlternative{
			{Label: "a", RejectionReason: &ws},
		},
	}
	// factor 3: 1 alt → 0.05, factor 4: trimmed len 0 → 0
	assert.InDelta(t, float32(0.05), Score(d), 0.001)
}

func TestScore_RejectionReasonOnlyOneNeeded(t *testing.T) {
	// Two alternatives: one without rejection, one with. Factor 4 should fire once.
	d := model.TraceDecision{
		Alternatives: []model.TraceAlternative{
			{Label: "a", RejectionReason: nil},
			{Label: "b", RejectionReason: strPtr("this option was rejected for good reason")},
		},
	}
	// factor 3: 2 alts → 0.15, factor 4: rejection → 0.10
	assert.InDelta(t, float32(0.25), Score(d), 0.001)
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
			assert.InDelta(t, tt.want, Score(d), 0.001)
		})
	}
}

// ---------------------------------------------------------------------------
// Decision type tests
// ---------------------------------------------------------------------------

func TestScore_NonStandardDecisionType(t *testing.T) {
	d := model.TraceDecision{DecisionType: "custom_workflow"}
	assert.InDelta(t, float32(0.0), Score(d), 0.001)
}

func TestScore_AllStandardDecisionTypes(t *testing.T) {
	for dt := range StandardDecisionTypes {
		t.Run(dt, func(t *testing.T) {
			d := model.TraceDecision{DecisionType: dt}
			assert.InDelta(t, float32(0.10), Score(d), 0.001)
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
			assert.InDelta(t, tt.want, Score(d), 0.001)
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
	assert.InDelta(t, float32(0.25), Score(d), 0.001)
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
	assert.InDelta(t, float32(0.55), Score(d), 0.001)
}

// ---------------------------------------------------------------------------
// StandardDecisionTypes map completeness
// ---------------------------------------------------------------------------

func TestStandardDecisionTypes_Contains(t *testing.T) {
	expected := []string{
		"model_selection", "architecture", "data_source", "error_handling",
		"feature_scope", "trade_off", "deployment", "security",
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
