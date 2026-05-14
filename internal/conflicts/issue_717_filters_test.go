package conflicts

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/ashita-ai/akashi/internal/model"
)

// Tests for the issue #717 structural updates:
//   - supersession keyword expansion (walk-back vocabulary)
//   - extractTicketRefs (multi-ticket extraction)
//   - isSameBranchMechanicalHousekeeping (narrow cross-agent same-branch housekeeping)

func TestContainsSupersessionKeyword_WalkbackVocabulary(t *testing.T) {
	// Coverage for the keyword extension added by #717. These are the exact
	// phrasings the FP audit found in self_contradiction / cross_agent FPs
	// that should have classified as supersession instead of contradiction.
	walkBacks := []string{
		"Shelved Observable Post-conditions draft (PR #28 closed without merge)",
		"Dropped the 2026-05-12 publish slot",
		"pivoting 2026-05-12 slot to a broader-audience essay",
		"Walked back over-critical take on ARD-720",
		"Rewrote ARD-952 ticket framing from scratch",
		"Scrapped the disk-spill buffer design (design A)",
		"Discarded the prior approach after empirical verification",
		"Retracted the recommendation in light of new evidence",
		"Tabled the migration until Q3 planning",
		"Parked the K8s RBAC change pending security review",
		"Deprecated this design in favour of B",
	}
	for _, outcome := range walkBacks {
		t.Run(outcome, func(t *testing.T) {
			assert.True(t, containsSupersessionKeyword(outcome),
				"#717 walk-back vocabulary should be recognised as supersession")
		})
	}

	// Negatives — none of these contain a walk-back signal.
	nonWalkBacks := []string{
		"Implemented the planned validator",
		"Reviewed PR #942 and approved",
		"Found one bug in the connection pool sizing",
		"",
	}
	for _, outcome := range nonWalkBacks {
		t.Run("not_walkback_"+outcome, func(t *testing.T) {
			assert.False(t, containsSupersessionKeyword(outcome),
				"non-reversal outcome should not trip supersession keyword check")
		})
	}
}

func TestExtractTicketRefs(t *testing.T) {
	tests := []struct {
		name     string
		decision model.Decision
		expected []string
	}{
		{
			name: "single ticket in task field",
			decision: model.Decision{
				AgentContext: map[string]any{"client": map[string]any{"task": "ARD-958 PR-1"}},
			},
			expected: []string{"ARD-958"},
		},
		{
			name: "multiple tickets across task and outcome are deduplicated and ordered",
			decision: model.Decision{
				AgentContext: map[string]any{"client": map[string]any{"task": "ARD-958 followup"}},
				Outcome:      "Implements ARD-958, also touches ARD-960 and ARD-958 again",
			},
			expected: []string{"ARD-958", "ARD-960"},
		},
		{
			name: "co-mention captured (regression for #717 first-match blindspot)",
			decision: model.Decision{
				Outcome: "Reviewed ARD-957 vs ARD-958 designs; both rejected in favour of design C",
			},
			expected: []string{"ARD-957", "ARD-958"},
		},
		{
			name: "branch + outcome with disjoint tickets returns both",
			decision: model.Decision{
				AgentContext: map[string]any{"client": map[string]any{"git_branch": "evanvolgas/ard-958-stream-wal"}},
				Outcome:      "Plumbing for ARD-960 verified",
			},
			expected: []string{"ARD-958", "ARD-960"},
		},
		{
			name: "lowercase canonicalised",
			decision: model.Decision{
				Outcome: "fixed ard-958 and ARD-960 in a single pass",
			},
			expected: []string{"ARD-958", "ARD-960"},
		},
		{
			name:     "no tickets extractable",
			decision: model.Decision{Outcome: "Refactored storage layer"},
			expected: nil,
		},
		{
			name:     "nil agent_context, empty outcome",
			decision: model.Decision{},
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, extractTicketRefs(tt.decision))
		})
	}
}

func TestExtractTicketRef_WrapsExtractTicketRefs(t *testing.T) {
	// extractTicketRef must continue to return the first ticket (the prior
	// API contract) even after the refactor to multi-ticket extraction.
	d := model.Decision{Outcome: "Touches ARD-958 and ARD-960"}
	assert.Equal(t, "ARD-958", extractTicketRef(d))
}

func TestIsSameBranchMechanicalHousekeeping(t *testing.T) {
	branchCtx := func(branch string) map[string]any {
		return map[string]any{"client": map[string]any{"git_branch": branch}}
	}

	tests := []struct {
		name     string
		d        model.Decision
		cand     model.Decision
		expected bool
	}{
		{
			name: "cross-agent same branch, both mechanical → suppressed (the #717 same-branch FP shape)",
			d: model.Decision{
				AgentID: "claude-code", AgentContext: branchCtx("evanvolgas/fix-sse-heartbeat"),
				Outcome: "Merged origin/main into evanvolgas/fix-sse-heartbeat, renumbering migration 097→098",
			},
			cand: model.Decision{
				AgentID: "reviewer", AgentContext: branchCtx("evanvolgas/fix-sse-heartbeat"),
				Outcome: "Resolved 9 merge conflicts after rebasing onto main",
			},
			expected: true,
		},
		{
			name: "same agent same branch → not suppressed (self-correction filter owns this case)",
			d: model.Decision{
				AgentID: "claude-code", AgentContext: branchCtx("evanvolgas/fix-sse-heartbeat"),
				Outcome: "Renumbered migration 097→098",
			},
			cand: model.Decision{
				AgentID: "claude-code", AgentContext: branchCtx("evanvolgas/fix-sse-heartbeat"),
				Outcome: "Renumbered migration 098→099",
			},
			expected: false,
		},
		{
			name: "cross-agent different branches → not suppressed (cross-branch filter owns that case)",
			d: model.Decision{
				AgentID: "claude-code", AgentContext: branchCtx("branch-a"),
				Outcome: "Renumbered migration 097→098",
			},
			cand: model.Decision{
				AgentID: "reviewer", AgentContext: branchCtx("branch-b"),
				Outcome: "Renumbered migration 099→100",
			},
			expected: false,
		},
		{
			name: "cross-agent same branch, only one mechanical → not suppressed",
			d: model.Decision{
				AgentID: "claude-code", AgentContext: branchCtx("feature/x"),
				Outcome: "Renumbered migration 097→098",
			},
			cand: model.Decision{
				AgentID: "reviewer", AgentContext: branchCtx("feature/x"),
				Outcome: "Chose Redis with 5min TTL for session cache",
			},
			expected: false,
		},
		{
			name: "cross-agent same branch, neither mechanical → not suppressed",
			d: model.Decision{
				AgentID: "claude-code", AgentContext: branchCtx("feature/x"),
				Outcome: "Chose Redis for caching",
			},
			cand: model.Decision{
				AgentID: "reviewer", AgentContext: branchCtx("feature/x"),
				Outcome: "Chose Memcached for caching",
			},
			expected: false,
		},
		{
			name: "cross-agent same branch, migration strategy only → not suppressed",
			d: model.Decision{
				AgentID: "claude-code", AgentContext: branchCtx("feature/x"),
				Outcome: "Chose pg_dump migration strategy for ARD-958",
			},
			cand: model.Decision{
				AgentID: "reviewer", AgentContext: branchCtx("feature/x"),
				Outcome: "Rejected pg_dump migration strategy; use logical replication",
			},
			expected: false,
		},
		{
			name: "missing branch on one side → not suppressed",
			d: model.Decision{
				AgentID: "claude-code", AgentContext: branchCtx("feature/x"),
				Outcome: "Renumbered migration 097→098",
			},
			cand: model.Decision{
				AgentID: "reviewer", AgentContext: map[string]any{},
				Outcome: "Renumbered migration 099→100",
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, isSameBranchMechanicalHousekeeping(tt.d, tt.cand))
		})
	}
}
