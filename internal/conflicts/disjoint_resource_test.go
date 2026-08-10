//go:build !lite

package conflicts

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/ashita-ai/akashi/internal/model"
)

// taskCtx builds an agent_context with a client-namespaced task, matching the
// structured layout nestedContextString reads.
func taskCtx(task string) map[string]any {
	return map[string]any{"client": map[string]any{"task": task}}
}

// TestExtractResourceRefs verifies the connector_/org_ extractor surfaces the
// structured infrastructure-resource tokens that dominate live operational
// traces, canonicalizes casing, reads the task field, and does NOT match method
// names, short fields, or bare hex runs that lack the prefix.
func TestExtractResourceRefs(t *testing.T) {
	tests := []struct {
		name string
		dec  model.Decision
		want []string
	}{
		{
			name: "connector token in outcome",
			dec:  model.Decision{Outcome: "Diagnosed Trellis connector_57a42328 pg2kafka restart_storm as unrecoverable"},
			want: []string{"CONNECTOR-57a42328"},
		},
		{
			name: "org token in outcome",
			dec:  model.Decision{Outcome: "Trellis connector_57a42328 (org_54b2e846) source slot invalidated"},
			want: []string{"CONNECTOR-57a42328", "ORG-54b2e846"},
		},
		{
			name: "multiple connectors, deduped",
			dec:  model.Decision{Outcome: "healthy reference 0307ac4b runs BOTH; connector_e1761afd idle; connector_e1761afd again"},
			want: []string{"CONNECTOR-e1761afd"},
		},
		{
			name: "connector named only in the task field",
			dec: model.Decision{
				Outcome:      "Executed connector reset recovery after confirming the slot was invalidated",
				AgentContext: taskCtx("Trellis recovery connector_9b38b47d restart_storm"),
			},
			want: []string{"CONNECTOR-9b38b47d"},
		},
		{
			name: "case-insensitive prefix and hex canonicalize to lower",
			dec:  model.Decision{Outcome: "CONNECTOR_AB12CD34 reset dispatched"},
			want: []string{"CONNECTOR-ab12cd34"},
		},
		{
			name: "method names and short fields are not matched",
			dec:  model.Decision{Outcome: "called connector_reset and updated org_id; POST /connectors/{id}/reset"},
			want: nil,
		},
		{
			name: "bare hex without the connector_/org_ prefix is ignored",
			dec:  model.Decision{Outcome: "Trellis 57a42328 restart_storm; commit 50fbd79b; op_90610865 progressing"},
			want: nil,
		},
		{
			name: "nothing extractable",
			dec:  model.Decision{Outcome: "raised Neon target endpoint to autoscale and resumed kafka2pg"},
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.ElementsMatch(t, tt.want, extractResourceRefs(tt.dec))
		})
	}
}

// TestIsDisjointResource exercises the resource-identity sibling of the
// disjoint-work-item filter against the 2026-06-24 cross-connector
// restart-storm over-clustering: operational/investigation decisions about
// different connector_/org_ resources that share dense incident vocabulary.
func TestIsDisjointResource(t *testing.T) {
	idA := uuid.New()
	idB := uuid.New()
	mono := "mono"
	other := "other"

	tests := []struct {
		name     string
		d        model.Decision
		cand     model.Decision
		expected bool
	}{
		{
			// Anchor: investigation of one connector vs deployment recovery of
			// another, sharing restart_storm/kafka2pg/slot vocabulary.
			name: "investigation vs deployment, disjoint connectors → suppressed",
			d: model.Decision{
				ID: idA, Project: &mono, DecisionType: "investigation",
				Outcome: "Diagnosed connector_57a42328 pg2kafka restart_storm as unrecoverable source slot invalidation (wal_status=lost)",
			},
			cand: model.Decision{
				ID: idB, Project: &mono, DecisionType: "deployment",
				Outcome: "Recovered connector_9b38b47d kafka2pg restart_storm ONLINE, lag 0; raised Neon target I/O",
			},
			expected: true,
		},
		{
			name: "org-scoped disjoint → suppressed",
			d: model.Decision{
				ID: idA, Project: &mono, DecisionType: "operational_recovery",
				Outcome: "Cleared the orphaned pgstream slot for org_896c32e7 via a direct service-role update",
			},
			cand: model.Decision{
				ID: idB, Project: &mono, DecisionType: "investigation",
				Outcome: "Root-caused org_54b2e846 connector wedge: engine-setup never finalized",
			},
			expected: true,
		},
		{
			name: "decision_type case-insensitive → suppressed",
			d: model.Decision{
				ID: idA, Project: &mono, DecisionType: "Investigation",
				Outcome: "connector_57a42328 slot invalidated",
			},
			cand: model.Decision{
				ID: idB, Project: &mono, DecisionType: "DEPLOYMENT",
				Outcome: "reset connector_555f7beb after confirming source up",
			},
			expected: true,
		},
		{
			name: "same connector (overlap) → NOT suppressed",
			d: model.Decision{
				ID: idA, Project: &mono, DecisionType: "investigation",
				Outcome: "Diagnosed connector_57a42328 as unrecoverable; recommend full reset",
			},
			cand: model.Decision{
				ID: idB, Project: &mono, DecisionType: "deployment",
				Outcome: "Scaled up kafka2pg for connector_57a42328 instead of resetting — resume loses no data",
			},
			expected: false,
		},
		{
			// The data-safety guard was removed from this filter (2026-07-23):
			// provably-disjoint connectors are different data planes = different
			// incidents, so a silent-data-loss finding on connector_e1761afd cannot
			// be the same incident as, or contradict, a recovery of connector_9b38b47d.
			// Same-writer data-safety protection lives in isOperationalStateProgression.
			name: "data-loss vocabulary on one side, disjoint connectors → suppressed (different incidents)",
			d: model.Decision{
				ID: idA, Project: &mono, DecisionType: "investigation",
				Outcome: "connector_e1761afd: scaling kafka2pg would skip offsets = silent permanent data loss; must re-snapshot",
			},
			cand: model.Decision{
				ID: idB, Project: &mono, DecisionType: "deployment",
				Outcome: "Recovered connector_9b38b47d kafka2pg restart_storm ONLINE",
			},
			expected: true,
		},
		{
			// architecture is direction-setting — a cross-cutting design fork can
			// span resources and must reach the validator.
			name: "architecture vs investigation, disjoint → NOT suppressed (direction-setting type)",
			d: model.Decision{
				ID: idA, Project: &mono, DecisionType: "architecture",
				Outcome: "connector_57a42328 cutover contract should be fail-closed and exactly-once",
			},
			cand: model.Decision{
				ID: idB, Project: &mono, DecisionType: "investigation",
				Outcome: "connector_9b38b47d restart_storm root cause: pg_net feature",
			},
			expected: false,
		},
		{
			name: "one side has no extractable resource (customer name only) → NOT suppressed",
			d: model.Decision{
				ID: idA, Project: &mono, DecisionType: "deployment",
				Outcome: "recovered the Globex sink by rolling kafka2pg to stable, left pg2kafka unchanged",
			},
			cand: model.Decision{
				ID: idB, Project: &mono, DecisionType: "investigation",
				Outcome: "Diagnosed connector_57a42328 source slot invalidation",
			},
			expected: false,
		},
		{
			name: "different project → NOT suppressed",
			d: model.Decision{
				ID: idA, Project: &mono, DecisionType: "investigation",
				Outcome: "connector_57a42328 slot invalidated",
			},
			cand: model.Decision{
				ID: idB, Project: &other, DecisionType: "deployment",
				Outcome: "reset connector_9b38b47d",
			},
			expected: false,
		},
		{
			name: "both projects untagged (nil) → NOT suppressed",
			d: model.Decision{
				ID: idA, DecisionType: "investigation",
				Outcome: "connector_57a42328 slot invalidated",
			},
			cand: model.Decision{
				ID: idB, DecisionType: "deployment",
				Outcome: "reset connector_9b38b47d",
			},
			expected: false,
		},
		{
			name: "precedent-linked → NOT suppressed (explicit lineage → let LLM decide)",
			d: model.Decision{
				ID: idA, Project: &mono, DecisionType: "investigation",
				Outcome:      "connector_57a42328 slot invalidated",
				PrecedentRef: &idB,
			},
			cand: model.Decision{
				ID: idB, Project: &mono, DecisionType: "deployment",
				Outcome: "reset connector_9b38b47d",
			},
			expected: false,
		},
		{
			// A non-resource type pair (both code_review) is the work-item
			// filter's job, not this one.
			name: "non-resource types → NOT suppressed (out of scope for this filter)",
			d: model.Decision{
				ID: idA, Project: &mono, DecisionType: "code_review",
				Outcome: "Reviewed connector_57a42328 wiring",
			},
			cand: model.Decision{
				ID: idB, Project: &mono, DecisionType: "code_review",
				Outcome: "Reviewed connector_9b38b47d wiring",
			},
			expected: false,
		},
		{
			// Cross-namespace: one side names only a connector, the other only an
			// org. The connector may be owned by that org, so the sets are NOT
			// comparable and the pair must reach the validator. The pre-fix
			// flattened-overlap check wrongly suppressed this (no string in common).
			name: "connector-only vs owning-org-only → NOT suppressed (cross-namespace not comparable)",
			d: model.Decision{
				ID: idA, Project: &mono, DecisionType: "investigation",
				Outcome: "Diagnosed connector_57a42328 source slot invalidation (wal_status=lost)",
			},
			cand: model.Decision{
				ID: idB, Project: &mono, DecisionType: "operational_recovery",
				Outcome: "Cleared the orphaned pgstream slot for org_54b2e846 via a service-role update",
			},
			expected: false,
		},
		{
			// Asymmetric namespaces: connector-only vs connector+org. Even though
			// the connectors differ, the org named only on one side could own the
			// other side's connector, so the pair is not comparable → validator.
			name: "connector-only vs connector+org → NOT suppressed (asymmetric namespaces)",
			d: model.Decision{
				ID: idA, Project: &mono, DecisionType: "investigation",
				Outcome: "Diagnosed connector_57a42328 restart_storm",
			},
			cand: model.Decision{
				ID: idB, Project: &mono, DecisionType: "deployment",
				Outcome: "Recovered connector_9b38b47d (org_896c32e7) kafka2pg ONLINE",
			},
			expected: false,
		},
		{
			// Both sides populate the SAME namespace set {CONNECTOR, ORG} and differ
			// in every namespace → provably disjoint, still suppressed.
			name: "connector+org vs connector+org, disjoint in both namespaces → suppressed",
			d: model.Decision{
				ID: idA, Project: &mono, DecisionType: "investigation",
				Outcome: "Diagnosed connector_57a42328 (org_54b2e846) restart_storm as unrecoverable",
			},
			cand: model.Decision{
				ID: idB, Project: &mono, DecisionType: "deployment",
				Outcome: "Recovered connector_9b38b47d (org_896c32e7) kafka2pg ONLINE, lag 0",
			},
			expected: true,
		},
		{
			// Same org, different connectors: the shared org overlaps, so the pair
			// reaches the validator — a shared resource is where a real clash lives.
			name: "different connectors but same org → NOT suppressed (shared org overlaps)",
			d: model.Decision{
				ID: idA, Project: &mono, DecisionType: "investigation",
				Outcome: "Diagnosed connector_57a42328 (org_54b2e846) restart_storm",
			},
			cand: model.Decision{
				ID: idB, Project: &mono, DecisionType: "deployment",
				Outcome: "Recovered connector_9b38b47d (org_54b2e846) kafka2pg ONLINE",
			},
			expected: false,
		},
		{
			// The data-safety guard is gone from this filter (2026-07-23), so a
			// data-loss keyword — even one confined to the task field — no longer
			// blocks suppression of provably-disjoint connectors. Task-field
			// data-safety protection now lives solely in isOperationalStateProgression
			// (see its "guard scans task" case).
			name: "data-loss keyword only in task, disjoint connectors → suppressed",
			d: model.Decision{
				ID: idA, Project: &mono, DecisionType: "investigation",
				Outcome:      "Recovered connector_57a42328 kafka2pg restart_storm ONLINE",
				AgentContext: taskCtx("DATALOSS check: connector_57a42328 may have skipped offsets on resume"),
			},
			cand: model.Decision{
				ID: idB, Project: &mono, DecisionType: "deployment",
				Outcome: "Recovered connector_9b38b47d kafka2pg restart_storm ONLINE",
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, isDisjointResource(tt.d, tt.cand),
				"isDisjointResource(%q vs %q)", tt.d.DecisionType, tt.cand.DecisionType)
			// Symmetric: argument order must not change the verdict.
			assert.Equal(t, tt.expected, isDisjointResource(tt.cand, tt.d),
				"isDisjointResource should be symmetric for %q", tt.name)
		})
	}
}
