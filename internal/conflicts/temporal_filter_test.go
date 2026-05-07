package conflicts

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/ashita-ai/akashi/internal/model"
)

// tStrPtr is a local helper that mirrors the integration-only strPtr.
// Defined here so this file builds under plain `go test` (no -tags=integration).
func tStrPtr(s string) *string { return &s }

func TestIsTemporalReassessment(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	sessionA := uuid.New()
	sessionB := uuid.New()
	declID := uuid.New()
	otherID := uuid.New()

	tests := []struct {
		name     string
		d        model.Decision
		cand     model.Decision
		expected bool
	}{
		{
			name: "two assessments, same project, 14 days apart → suppressed",
			d: model.Decision{
				ID:           declID,
				DecisionType: "assessment",
				Project:      tStrPtr("akashi"),
				ValidFrom:    now,
			},
			cand: model.Decision{
				ID:           otherID,
				DecisionType: "assessment",
				Project:      tStrPtr("akashi"),
				ValidFrom:    now.Add(-14 * 24 * time.Hour),
			},
			expected: true,
		},
		{
			name: "two code_reviews, same project, 30 days apart → suppressed",
			d: model.Decision{
				ID:           declID,
				DecisionType: "code_review",
				Project:      tStrPtr("akashi"),
				ValidFrom:    now,
			},
			cand: model.Decision{
				ID:           otherID,
				DecisionType: "code_review",
				Project:      tStrPtr("akashi"),
				ValidFrom:    now.Add(-30 * 24 * time.Hour),
			},
			expected: true,
		},
		{
			name: "audit + assessment, same project, 10 days apart → suppressed (mixed review types)",
			d: model.Decision{
				ID:           declID,
				DecisionType: "audit",
				Project:      tStrPtr("akashi"),
				ValidFrom:    now,
			},
			cand: model.Decision{
				ID:           otherID,
				DecisionType: "assessment",
				Project:      tStrPtr("akashi"),
				ValidFrom:    now.Add(-10 * 24 * time.Hour),
			},
			expected: true,
		},
		{
			name: "two assessments, same project, 2 days apart → not suppressed (window not crossed)",
			d: model.Decision{
				ID:           declID,
				DecisionType: "assessment",
				Project:      tStrPtr("akashi"),
				ValidFrom:    now,
			},
			cand: model.Decision{
				ID:           otherID,
				DecisionType: "assessment",
				Project:      tStrPtr("akashi"),
				ValidFrom:    now.Add(-2 * 24 * time.Hour),
			},
			expected: false,
		},
		{
			name: "two assessments, exactly 7 days apart → suppressed (boundary inclusive)",
			d: model.Decision{
				ID:           declID,
				DecisionType: "assessment",
				Project:      tStrPtr("akashi"),
				ValidFrom:    now,
			},
			cand: model.Decision{
				ID:           otherID,
				DecisionType: "assessment",
				Project:      tStrPtr("akashi"),
				ValidFrom:    now.Add(-7 * 24 * time.Hour),
			},
			expected: true,
		},
		{
			name: "two assessments, different projects → not suppressed",
			d: model.Decision{
				ID:           declID,
				DecisionType: "assessment",
				Project:      tStrPtr("akashi"),
				ValidFrom:    now,
			},
			cand: model.Decision{
				ID:           otherID,
				DecisionType: "assessment",
				Project:      tStrPtr("ardent-mono"),
				ValidFrom:    now.Add(-30 * 24 * time.Hour),
			},
			expected: false,
		},
		{
			name: "two assessments linked via precedent_ref → not suppressed (LLM decides)",
			d: model.Decision{
				ID:           declID,
				DecisionType: "assessment",
				Project:      tStrPtr("akashi"),
				ValidFrom:    now,
				PrecedentRef: &otherID,
			},
			cand: model.Decision{
				ID:           otherID,
				DecisionType: "assessment",
				Project:      tStrPtr("akashi"),
				ValidFrom:    now.Add(-30 * 24 * time.Hour),
			},
			expected: false,
		},
		{
			name: "two assessments in same session → not suppressed",
			d: model.Decision{
				ID:           declID,
				DecisionType: "assessment",
				Project:      tStrPtr("akashi"),
				ValidFrom:    now,
				SessionID:    &sessionA,
			},
			cand: model.Decision{
				ID:           otherID,
				DecisionType: "assessment",
				Project:      tStrPtr("akashi"),
				ValidFrom:    now.Add(-30 * 24 * time.Hour),
				SessionID:    &sessionA,
			},
			expected: false,
		},
		{
			name: "two assessments in different sessions, same project, 14d → suppressed",
			d: model.Decision{
				ID:           declID,
				DecisionType: "assessment",
				Project:      tStrPtr("akashi"),
				ValidFrom:    now,
				SessionID:    &sessionA,
			},
			cand: model.Decision{
				ID:           otherID,
				DecisionType: "assessment",
				Project:      tStrPtr("akashi"),
				ValidFrom:    now.Add(-14 * 24 * time.Hour),
				SessionID:    &sessionB,
			},
			expected: true,
		},
		{
			name: "assessment + architecture → not suppressed (architecture is not a review type)",
			d: model.Decision{
				ID:           declID,
				DecisionType: "assessment",
				Project:      tStrPtr("akashi"),
				ValidFrom:    now,
			},
			cand: model.Decision{
				ID:           otherID,
				DecisionType: "architecture",
				Project:      tStrPtr("akashi"),
				ValidFrom:    now.Add(-30 * 24 * time.Hour),
			},
			expected: false,
		},
		{
			name: "two architecture decisions → not suppressed (neither is a review type)",
			d: model.Decision{
				ID:           declID,
				DecisionType: "architecture",
				Project:      tStrPtr("akashi"),
				ValidFrom:    now,
			},
			cand: model.Decision{
				ID:           otherID,
				DecisionType: "architecture",
				Project:      tStrPtr("akashi"),
				ValidFrom:    now.Add(-30 * 24 * time.Hour),
			},
			expected: false,
		},
		{
			name: "case-insensitive type match (CODE_REVIEW)",
			d: model.Decision{
				ID:           declID,
				DecisionType: "CODE_REVIEW",
				Project:      tStrPtr("akashi"),
				ValidFrom:    now,
			},
			cand: model.Decision{
				ID:           otherID,
				DecisionType: "Assessment",
				Project:      tStrPtr("akashi"),
				ValidFrom:    now.Add(-30 * 24 * time.Hour),
			},
			expected: true,
		},
		{
			name: "both projects nil → suppressed when types match (same null scope)",
			d: model.Decision{
				ID:           declID,
				DecisionType: "assessment",
				ValidFrom:    now,
			},
			cand: model.Decision{
				ID:           otherID,
				DecisionType: "assessment",
				ValidFrom:    now.Add(-30 * 24 * time.Hour),
			},
			expected: true,
		},
		{
			name: "reverse temporal order (cand newer than d) → still suppressed",
			d: model.Decision{
				ID:           declID,
				DecisionType: "assessment",
				Project:      tStrPtr("akashi"),
				ValidFrom:    now.Add(-30 * 24 * time.Hour),
			},
			cand: model.Decision{
				ID:           otherID,
				DecisionType: "assessment",
				Project:      tStrPtr("akashi"),
				ValidFrom:    now,
			},
			expected: true,
		},
		{
			name: "precedent_ref on d points to cand → not suppressed",
			d: model.Decision{
				ID:           declID,
				DecisionType: "assessment",
				Project:      tStrPtr("akashi"),
				ValidFrom:    now,
				PrecedentRef: &otherID,
			},
			cand: model.Decision{
				ID:           otherID,
				DecisionType: "assessment",
				Project:      tStrPtr("akashi"),
				ValidFrom:    now.Add(-30 * 24 * time.Hour),
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, isTemporalReassessment(tt.d, tt.cand))
		})
	}
}

func TestFormatPrompt_TemporalReassessmentHint(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	t.Run("two assessments 14 days apart → hint present", func(t *testing.T) {
		prompt := formatPrompt(ValidateInput{
			OutcomeA: "FP rate measured at 76% on rolling 30 days.",
			OutcomeB: "FP rate measured at 51% during prior review.",
			TypeA:    "assessment",
			TypeB:    "assessment",
			AgentA:   "claude-code",
			AgentB:   "senior-engineer",
			CreatedA: now,
			CreatedB: now.Add(-14 * 24 * time.Hour),
		})
		assert.Contains(t, prompt, "TEMPORAL RE-MEASUREMENT",
			"hint should fire for review-type pairs past the temporal window")
		assert.Contains(t, prompt, "time-bound",
			"hint body should explain the time-bound nature of metrics")
	})

	t.Run("two assessments 2 days apart → no hint", func(t *testing.T) {
		prompt := formatPrompt(ValidateInput{
			OutcomeA: "FP rate at 60%.",
			OutcomeB: "FP rate at 58%.",
			TypeA:    "assessment",
			TypeB:    "assessment",
			AgentA:   "agent-a",
			AgentB:   "agent-b",
			CreatedA: now,
			CreatedB: now.Add(-2 * 24 * time.Hour),
		})
		assert.NotContains(t, prompt, "TEMPORAL RE-MEASUREMENT",
			"hint should not fire when within the temporal window")
	})

	t.Run("assessment + architecture 30 days apart → no hint", func(t *testing.T) {
		prompt := formatPrompt(ValidateInput{
			OutcomeA: "Audited the validator.",
			OutcomeB: "Chose Redis for caching.",
			TypeA:    "assessment",
			TypeB:    "architecture",
			AgentA:   "agent-a",
			AgentB:   "agent-b",
			CreatedA: now,
			CreatedB: now.Add(-30 * 24 * time.Hour),
		})
		assert.NotContains(t, prompt, "TEMPORAL RE-MEASUREMENT",
			"hint should not fire when one decision is not a review type")
	})

	t.Run("case-insensitive on decision type", func(t *testing.T) {
		prompt := formatPrompt(ValidateInput{
			OutcomeA: "review of system.",
			OutcomeB: "review of system, later.",
			TypeA:    "CODE_REVIEW",
			TypeB:    "Assessment",
			AgentA:   "agent-a",
			AgentB:   "agent-b",
			CreatedA: now,
			CreatedB: now.Add(-14 * 24 * time.Hour),
		})
		// Hint relies on canonical lowercased lookup in reviewTypes.
		if !strings.Contains(prompt, "TEMPORAL RE-MEASUREMENT") {
			t.Fatalf("hint missing for case-insensitive review-type match; prompt:\n%s", prompt)
		}
	})
}
