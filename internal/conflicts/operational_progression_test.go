//go:build !lite

package conflicts

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/ashita-ai/akashi/internal/model"
)

// TestIsOperationalStateProgression exercises the operational sibling of
// isTemporalReassessment. The table covers each guardrail in isolation plus
// anchor cases drawn from the 2026-06-18 open-conflict queue:
//
//   - SalesPatriot (suppressed): codex "rolled kafka2pg back to its pre-rollout
//     digest" (operations, 06-18) vs claude "verified the connector ONLINE"
//     (deployment, 06-10) — eight days apart, same project, no reversal vocab.
//     A remediation step in an incident timeline, not a disagreement.
//   - Debezium fork (NOT suppressed): claude "recommend AGAINST replacing
//     pgstream with Debezium" vs codex "replace with Debezium" — architecture
//     types, so never eligible; the genuine direction-setting fork must reach
//     the validator.
func TestIsOperationalStateProgression(t *testing.T) {
	now := time.Date(2026, 6, 18, 18, 51, 0, 0, time.UTC)
	idA := uuid.New()
	idB := uuid.New()
	mono := "mono"
	other := "other"
	sess := uuid.New()

	// beyond is comfortably past the window; under is just inside it.
	beyond := operationalProgressionWindow + 24*time.Hour
	under := operationalProgressionWindow - time.Hour

	tests := []struct {
		name     string
		d        model.Decision
		cand     model.Decision
		expected bool
	}{
		{
			// SalesPatriot anchor: operations vs deployment, 8 days apart.
			name: "both operational, same project, far apart, no reversal vocab → suppressed",
			d: model.Decision{
				ID: idA, AgentID: "codex-mcp-client", Project: &mono,
				DecisionType: "operations",
				Outcome:      "restored SalesPatriot online by rolling kafka2pg back to its pre-rollout digest and scaling it to 1",
				ValidFrom:    now,
			},
			cand: model.Decision{
				ID: idB, AgentID: "claude-code", Project: &mono,
				DecisionType: "deployment",
				Outcome:      "verified SalesPatriot connector_c3fed173 is online and branchable on the deployed pgstream image",
				ValidFrom:    now.Add(-beyond),
			},
			expected: true,
		},
		{
			name: "decision_type case-insensitive → suppressed",
			d: model.Decision{
				ID: idA, Project: &mono, DecisionType: "Operations",
				Outcome: "scaled kafka2pg to 1 to resume the snapshot drain", ValidFrom: now,
			},
			cand: model.Decision{
				ID: idB, Project: &mono, DecisionType: "DEPLOYMENT",
				Outcome: "promoted the validated digest to the customer account", ValidFrom: now.Add(-beyond),
			},
			expected: true,
		},
		{
			name: "exactly at the window boundary → suppressed (>=)",
			d: model.Decision{
				ID: idA, Project: &mono, DecisionType: "operational",
				Outcome: "recovered the connector data plane on the latest image", ValidFrom: now,
			},
			cand: model.Decision{
				ID: idB, Project: &mono, DecisionType: "operations",
				Outcome: "paused the target writer pending validation", ValidFrom: now.Add(-operationalProgressionWindow),
			},
			expected: true,
		},
		{
			name: "reversed temporal order (cand newer) → still suppressed (abs delta)",
			d: model.Decision{
				ID: idA, Project: &mono, DecisionType: "deployment",
				Outcome: "completed the BYOC pgstream rollout to Ordo", ValidFrom: now.Add(-beyond),
			},
			cand: model.Decision{
				ID: idB, Project: &mono, DecisionType: "operations",
				Outcome: "rolled Ordo BYOC pgstream to the known-good digest", ValidFrom: now,
			},
			expected: true,
		},
		{
			name: "too close in time (just under window) → NOT suppressed",
			d: model.Decision{
				ID: idA, Project: &mono, DecisionType: "operations",
				Outcome: "scaled kafka2pg to 1", ValidFrom: now,
			},
			cand: model.Decision{
				ID: idB, Project: &mono, DecisionType: "operations",
				Outcome: "scaled kafka2pg to 0", ValidFrom: now.Add(-under),
			},
			expected: false,
		},
		{
			// Debezium-fork anchor: architecture is not an operationalType.
			name: "non-operational type (architecture) → NOT suppressed",
			d: model.Decision{
				ID: idA, Project: &mono, DecisionType: "architecture",
				Outcome: "ARD-1598 decision: use Debezium pgoutput for source capture", ValidFrom: now,
			},
			cand: model.Decision{
				ID: idB, Project: &mono, DecisionType: "operations",
				Outcome: "deployed the pgstream image to the fleet", ValidFrom: now.Add(-beyond),
			},
			expected: false,
		},
		{
			name: "one side trade_off → NOT suppressed (kept narrow to protect design forks)",
			d: model.Decision{
				ID: idA, Project: &mono, DecisionType: "trade_off",
				Outcome: "prepared Petra's BYOC image by copying the validated digest", ValidFrom: now,
			},
			cand: model.Decision{
				ID: idB, Project: &mono, DecisionType: "deployment",
				Outcome: "executed the stable rollout via terraform apply", ValidFrom: now.Add(-beyond),
			},
			expected: false,
		},
		{
			name: "different project → NOT suppressed",
			d: model.Decision{
				ID: idA, Project: &mono, DecisionType: "operations",
				Outcome: "rolled the fleet back to the known-good digest", ValidFrom: now,
			},
			cand: model.Decision{
				ID: idB, Project: &other, DecisionType: "deployment",
				Outcome: "deployed the new image to the fleet", ValidFrom: now.Add(-beyond),
			},
			expected: false,
		},
		{
			name: "both project untagged (nil) → NOT suppressed",
			d: model.Decision{
				ID: idA, DecisionType: "operations",
				Outcome: "rolled the fleet back to the known-good digest", ValidFrom: now,
			},
			cand: model.Decision{
				ID: idB, DecisionType: "deployment",
				Outcome: "deployed the new image to the fleet", ValidFrom: now.Add(-beyond),
			},
			expected: false,
		},
		{
			name: "precedent-linked → NOT suppressed (explicit link → let LLM decide)",
			d: model.Decision{
				ID: idA, Project: &mono, DecisionType: "operations",
				Outcome: "rolled the fleet back to the known-good digest", ValidFrom: now,
				PrecedentRef: &idB,
			},
			cand: model.Decision{
				ID: idB, Project: &mono, DecisionType: "deployment",
				Outcome: "deployed the new image to the fleet", ValidFrom: now.Add(-beyond),
			},
			expected: false,
		},
		{
			name: "same session → NOT suppressed",
			d: model.Decision{
				ID: idA, Project: &mono, DecisionType: "operations",
				Outcome: "rolled the fleet back to the known-good digest", ValidFrom: now,
				SessionID: &sess,
			},
			cand: model.Decision{
				ID: idB, Project: &mono, DecisionType: "deployment",
				Outcome: "deployed the new image to the fleet", ValidFrom: now.Add(-beyond),
				SessionID: &sess,
			},
			expected: false,
		},
		{
			name: "supersession keyword in later outcome → NOT suppressed (declared reversal)",
			d: model.Decision{
				ID: idA, Project: &mono, DecisionType: "operations",
				Outcome: "reverted last week's rollout because it caused target-side dataloss", ValidFrom: now,
			},
			cand: model.Decision{
				ID: idB, Project: &mono, DecisionType: "deployment",
				Outcome: "deployed the new image to the fleet", ValidFrom: now.Add(-beyond),
			},
			expected: false,
		},
		{
			name: "supersession keyword in earlier outcome → NOT suppressed (both-sides guard)",
			d: model.Decision{
				ID: idA, Project: &mono, DecisionType: "operations",
				Outcome: "deployed the new image to the fleet", ValidFrom: now,
			},
			cand: model.Decision{
				ID: idB, Project: &mono, DecisionType: "deployment",
				Outcome: "replaced the hosted writer with the BYOC writer", ValidFrom: now.Add(-beyond),
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, isOperationalStateProgression(tt.d, tt.cand),
				"isOperationalStateProgression(%q vs %q)", tt.d.DecisionType, tt.cand.DecisionType)
			// Symmetric: argument order must not change the verdict.
			assert.Equal(t, tt.expected, isOperationalStateProgression(tt.cand, tt.d),
				"isOperationalStateProgression should be symmetric for %q", tt.name)
		})
	}
}
