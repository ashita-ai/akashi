package conflicts

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDecisionTypeTierOf(t *testing.T) {
	tests := []struct {
		decisionType string
		want         int
	}{
		{"security", 4},
		{"Security", 4}, // case-insensitive
		{"SECURITY", 4}, // case-insensitive
		{"architecture", 3},
		{"deployment", 3},
		{"trade_off", 2},
		{"model_selection", 2},
		{"data_source", 2},
		{"error_handling", 2},
		{"code_review", 1},
		{"feature_scope", 1},
		{"investigation", 1},
		{"planning", 1},
		{"assessment", 1},
		{"custom_type", 1}, // unknown defaults to tier 1
		{"", 1},            // empty defaults to tier 1
	}
	for _, tt := range tests {
		t.Run(tt.decisionType, func(t *testing.T) {
			assert.Equal(t, tt.want, DecisionTypeTierOf(tt.decisionType))
		})
	}
}

func TestComputeSeverity(t *testing.T) {
	tests := []struct {
		name  string
		input SeverityInput
		want  string
	}{
		// --- Type tier drives base severity ---
		{
			name:  "security type yields high",
			input: SeverityInput{DecisionTypeA: "security", DecisionTypeB: "code_review", ConfidenceA: 0.5, ConfidenceB: 0.5},
			want:  "high",
		},
		{
			name:  "architecture type yields medium",
			input: SeverityInput{DecisionTypeA: "architecture", DecisionTypeB: "architecture", ConfidenceA: 0.5, ConfidenceB: 0.5},
			want:  "medium",
		},
		{
			name:  "deployment type yields medium",
			input: SeverityInput{DecisionTypeA: "deployment", DecisionTypeB: "code_review", ConfidenceA: 0.5, ConfidenceB: 0.5},
			want:  "medium",
		},
		{
			name:  "trade_off type yields medium",
			input: SeverityInput{DecisionTypeA: "trade_off", DecisionTypeB: "trade_off", ConfidenceA: 0.5, ConfidenceB: 0.5},
			want:  "medium",
		},
		{
			name:  "code_review type yields low",
			input: SeverityInput{DecisionTypeA: "code_review", DecisionTypeB: "code_review", ConfidenceA: 0.5, ConfidenceB: 0.5},
			want:  "low",
		},
		{
			name:  "investigation type yields low",
			input: SeverityInput{DecisionTypeA: "investigation", DecisionTypeB: "planning", ConfidenceA: 0.5, ConfidenceB: 0.5},
			want:  "low",
		},
		{
			name:  "unknown custom type yields low",
			input: SeverityInput{DecisionTypeA: "custom_type", DecisionTypeB: "custom_type", ConfidenceA: 0.5, ConfidenceB: 0.5},
			want:  "low",
		},

		// --- Independence: high significance does NOT inflate low-tier severity ---
		{
			name:  "high confidence on investigation stays low",
			input: SeverityInput{DecisionTypeA: "investigation", DecisionTypeB: "investigation", ConfidenceA: 0.9, ConfidenceB: 0.9},
			want:  "low",
		},
		{
			name:  "high confidence on code_review stays low",
			input: SeverityInput{DecisionTypeA: "code_review", DecisionTypeB: "code_review", ConfidenceA: 0.95, ConfidenceB: 0.95},
			want:  "low",
		},

		// --- Both-high-confidence promotion (tier >= 3 only) ---
		{
			name:  "architecture + both high confidence promotes to high",
			input: SeverityInput{DecisionTypeA: "architecture", DecisionTypeB: "architecture", ConfidenceA: 0.8, ConfidenceB: 0.8},
			want:  "high",
		},
		{
			name:  "deployment + both at 0.7 threshold promotes to high",
			input: SeverityInput{DecisionTypeA: "deployment", DecisionTypeB: "deployment", ConfidenceA: 0.7, ConfidenceB: 0.7},
			want:  "high",
		},
		{
			name:  "trade_off tier 2 not promoted even with high confidence",
			input: SeverityInput{DecisionTypeA: "trade_off", DecisionTypeB: "trade_off", ConfidenceA: 0.9, ConfidenceB: 0.9},
			want:  "medium",
		},

		// --- Low-confidence demotion ---
		{
			name:  "security demoted to medium when one side low confidence",
			input: SeverityInput{DecisionTypeA: "security", DecisionTypeB: "security", ConfidenceA: 0.9, ConfidenceB: 0.2},
			want:  "medium",
		},
		{
			name:  "architecture demoted to low when one side low confidence",
			input: SeverityInput{DecisionTypeA: "architecture", DecisionTypeB: "architecture", ConfidenceA: 0.3, ConfidenceB: 0.5},
			want:  "low",
		},
		{
			name:  "code_review already low cannot demote further",
			input: SeverityInput{DecisionTypeA: "code_review", DecisionTypeB: "code_review", ConfidenceA: 0.2, ConfidenceB: 0.5},
			want:  "low",
		},

		// --- Factual category boost ---
		{
			name:  "factual trade_off promotes medium to high",
			input: SeverityInput{DecisionTypeA: "trade_off", DecisionTypeB: "trade_off", ConfidenceA: 0.5, ConfidenceB: 0.5, Category: "factual"},
			want:  "high",
		},
		{
			name:  "factual code_review stays low (tier 1 not eligible)",
			input: SeverityInput{DecisionTypeA: "code_review", DecisionTypeB: "code_review", ConfidenceA: 0.5, ConfidenceB: 0.5, Category: "factual"},
			want:  "low",
		},
		{
			name:  "assessment category has no boost",
			input: SeverityInput{DecisionTypeA: "code_review", DecisionTypeB: "code_review", ConfidenceA: 0.5, ConfidenceB: 0.5, Category: "assessment"},
			want:  "low",
		},

		// --- Zero confidence treated as unknown (not low) ---
		{
			name:  "security with zero confidence stays high (unknown, not low)",
			input: SeverityInput{DecisionTypeA: "security", DecisionTypeB: "security", ConfidenceA: 0, ConfidenceB: 0},
			want:  "high",
		},
		{
			name:  "architecture with zero confidence stays medium",
			input: SeverityInput{DecisionTypeA: "architecture", DecisionTypeB: "architecture", ConfidenceA: 0, ConfidenceB: 0},
			want:  "medium",
		},

		// --- Max-of-pair semantics ---
		{
			name:  "security vs investigation uses security tier",
			input: SeverityInput{DecisionTypeA: "security", DecisionTypeB: "investigation", ConfidenceA: 0.5, ConfidenceB: 0.5},
			want:  "high",
		},
		{
			name:  "architecture vs code_review uses architecture tier",
			input: SeverityInput{DecisionTypeA: "architecture", DecisionTypeB: "code_review", ConfidenceA: 0.5, ConfidenceB: 0.5},
			want:  "medium",
		},

		// --- Empty types ---
		{
			name:  "both empty types returns empty",
			input: SeverityInput{DecisionTypeA: "", DecisionTypeB: ""},
			want:  "",
		},
		{
			name:  "one empty type uses the other",
			input: SeverityInput{DecisionTypeA: "security", DecisionTypeB: "", ConfidenceA: 0.5, ConfidenceB: 0.5},
			want:  "high",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ComputeSeverity(tt.input)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestComputeSeverity_NeverReturnsCritical(t *testing.T) {
	// Sweep all tiers with extreme inputs — critical must never be returned.
	types := []string{"security", "architecture", "deployment", "trade_off",
		"model_selection", "code_review", "investigation"}
	categories := []string{"", "factual", "strategic", "assessment", "temporal"}

	for _, dt := range types {
		for _, cat := range categories {
			input := SeverityInput{
				DecisionTypeA: dt,
				DecisionTypeB: dt,
				ConfidenceA:   1.0,
				ConfidenceB:   1.0,
				Category:      cat,
			}
			got := ComputeSeverity(input)
			assert.NotEqual(t, "critical", got,
				"ComputeSeverity must never return critical (type=%s, category=%s)", dt, cat)
		}
	}
}

func TestComputeSeverity_PromotionAndDemotionInteraction(t *testing.T) {
	// Both-high-confidence promotion + low-confidence demotion cannot both apply
	// (mutually exclusive conditions), but factual boost + demotion can.
	t.Run("factual boost then low-confidence demotion", func(t *testing.T) {
		// architecture: base medium → factual promotes to high → low conf demotes to medium
		got := ComputeSeverity(SeverityInput{
			DecisionTypeA: "architecture",
			DecisionTypeB: "architecture",
			ConfidenceA:   0.2,
			ConfidenceB:   0.5,
			Category:      "factual",
		})
		assert.Equal(t, "medium", got)
	})

	t.Run("factual boost on tier 2 with normal confidence", func(t *testing.T) {
		// error_handling: base medium → factual promotes to high
		got := ComputeSeverity(SeverityInput{
			DecisionTypeA: "error_handling",
			DecisionTypeB: "error_handling",
			ConfidenceA:   0.5,
			ConfidenceB:   0.5,
			Category:      "factual",
		})
		assert.Equal(t, "high", got)
	})

	t.Run("both high confidence on security stays high", func(t *testing.T) {
		// security: base high → both high conf on tier 4 >= 3: promote but already high
		got := ComputeSeverity(SeverityInput{
			DecisionTypeA: "security",
			DecisionTypeB: "security",
			ConfidenceA:   0.9,
			ConfidenceB:   0.9,
		})
		assert.Equal(t, "high", got)
	})

	t.Run("at most one promotion applies — high-conf factual tier 3", func(t *testing.T) {
		// architecture: base medium. Both promotion conditions are true
		// (high confidence + tier >= 3, and factual + tier >= 2), but
		// ADR-015 mandates at most one promotion. Result: high, not beyond.
		got := ComputeSeverity(SeverityInput{
			DecisionTypeA: "architecture",
			DecisionTypeB: "architecture",
			ConfidenceA:   0.8,
			ConfidenceB:   0.8,
			Category:      "factual",
		})
		assert.Equal(t, "high", got)
	})
}
