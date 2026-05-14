package conflicts

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/ashita-ai/akashi/internal/model"
)

// Tests for the issue #717 structural pre-filters:
//   - supersession keyword expansion (walk-back vocabulary)
//   - extractTicketRefs (multi-ticket extraction)
//   - layer marker extraction
//   - isPRSeriesLayerRefinement (cross-agent same-ticket PR-series)
//   - isDisjointTicketReviewPair (cross-agent reviews on disjoint tickets)
//   - isSameBranchMechanicalHousekeeping (cross-agent same-branch housekeeping)

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

func TestExtractLayerMarker(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"Reviewed ARD-958 S1 PR (return consistent_point LSN)", "s1"},
		{"ARD-958 S2 implemented in branch", "s2"},
		{"Reviewed ARD-844 layer 3 PR", "layer 3"},
		{"ARD-844 layer-2 done", "layer 2"},
		{"phase 1 of the migration", "phase 1"},
		{"step-2 implementation", "step 2"},
		{"stage 4 complete", "stage 4"},
		{"PR-2 merged", "pr 2"},
		{"PR_2 review", "pr 2"},
		{"PR 2 of the series", "pr 2"},
		{"S-1 typo case", ""}, // hyphen between S and digit not matched
		{"prefix S1Z", ""},    // not a word boundary
		{"version 1.2.3", ""}, // unrelated digits
		{"refactor", ""},      // no marker
		{"", ""},              // empty
		{"step 12", ""},       // multi-digit deliberately rejected
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.expected, extractLayerMarker(tt.input))
		})
	}
}

func TestShareAny(t *testing.T) {
	assert.True(t, shareAny([]string{"A", "B"}, []string{"B", "C"}))
	assert.False(t, shareAny([]string{"A"}, []string{"B"}))
	assert.False(t, shareAny(nil, []string{"A"}))
	assert.False(t, shareAny([]string{"A"}, nil))
	assert.False(t, shareAny(nil, nil))
}

func TestTicketSetsDisjoint(t *testing.T) {
	assert.True(t, ticketSetsDisjoint([]string{"ARD-957"}, []string{"ARD-958"}))
	assert.False(t, ticketSetsDisjoint([]string{"ARD-958"}, []string{"ARD-958"}))
	assert.False(t, ticketSetsDisjoint([]string{"ARD-957", "ARD-958"}, []string{"ARD-958"}))
	assert.False(t, ticketSetsDisjoint(nil, []string{"ARD-957"}),
		"empty side treated as not-disjoint so callers don't suppress no-ticket pairs")
	assert.False(t, ticketSetsDisjoint([]string{"ARD-957"}, nil))
	assert.False(t, ticketSetsDisjoint(nil, nil))
}

func TestIsPRSeriesLayerRefinement(t *testing.T) {
	now := time.Date(2026, 5, 8, 23, 0, 0, 0, time.UTC)
	idA := uuid.New()
	idB := uuid.New()

	taskCtx := func(task string) map[string]any {
		return map[string]any{"client": map[string]any{"task": task}}
	}

	tests := []struct {
		name     string
		d        model.Decision
		cand     model.Decision
		expected bool
	}{
		{
			name: "cross-agent same-ticket, S1 vs S2 → suppressed (the dominant #717 FP shape)",
			d: model.Decision{
				ID: idA, AgentID: "claude-code", AgentContext: taskCtx("ARD-958"),
				Outcome: "Reviewed ARD-958 S1 PR (return consistent_point LSN)", ValidFrom: now,
			},
			cand: model.Decision{
				ID: idB, AgentID: "reviewer", AgentContext: taskCtx("ARD-958"),
				Outcome: "Reviewed ARD-958 S2 PR (thread Init consistent_point)", ValidFrom: now.Add(time.Hour),
			},
			expected: true,
		},
		{
			name: "cross-agent same-ticket, only one side has layer marker → suppressed",
			d: model.Decision{
				ID: idA, AgentID: "claude-code", AgentContext: taskCtx("ARD-844"),
				Outcome: "Reviewed ARD-844 layer 3 PR (5 commits, ~1.2k lines)", ValidFrom: now,
			},
			cand: model.Decision{
				ID: idB, AgentID: "reviewer", AgentContext: taskCtx("ARD-844"),
				Outcome: "ARD-844 ticket assessed: structurally correct diagnosis", ValidFrom: now.Add(time.Hour),
			},
			expected: true,
		},
		{
			name: "same agent → not suppressed (#711 handles same-agent)",
			d: model.Decision{
				ID: idA, AgentID: "claude-code", AgentContext: taskCtx("ARD-958"),
				Outcome: "ARD-958 S1 implemented", ValidFrom: now,
			},
			cand: model.Decision{
				ID: idB, AgentID: "claude-code", AgentContext: taskCtx("ARD-958"),
				Outcome: "ARD-958 S2 implemented", ValidFrom: now.Add(time.Hour),
			},
			expected: false,
		},
		{
			name: "cross-agent same ticket but identical layer markers → not suppressed (duplicate review, not chain)",
			d: model.Decision{
				ID: idA, AgentID: "claude-code", AgentContext: taskCtx("ARD-958"),
				Outcome: "Reviewed ARD-958 S2 PR — approved", ValidFrom: now,
			},
			cand: model.Decision{
				ID: idB, AgentID: "reviewer", AgentContext: taskCtx("ARD-958"),
				Outcome: "Reviewed ARD-958 S2 PR — rejected", ValidFrom: now.Add(time.Hour),
			},
			expected: false,
		},
		{
			name: "cross-agent same ticket but no layer marker on either side → not suppressed",
			d: model.Decision{
				ID: idA, AgentID: "claude-code", AgentContext: taskCtx("ARD-958"),
				Outcome: "ARD-958 deployed", ValidFrom: now,
			},
			cand: model.Decision{
				ID: idB, AgentID: "reviewer", AgentContext: taskCtx("ARD-958"),
				Outcome: "ARD-958 rolled back", ValidFrom: now.Add(time.Hour),
			},
			expected: false,
		},
		{
			name: "cross-agent disjoint tickets → not suppressed (this filter is same-ticket)",
			d: model.Decision{
				ID: idA, AgentID: "claude-code", AgentContext: taskCtx("ARD-957"),
				Outcome: "Reviewed ARD-957 S1", ValidFrom: now,
			},
			cand: model.Decision{
				ID: idB, AgentID: "reviewer", AgentContext: taskCtx("ARD-958"),
				Outcome: "Reviewed ARD-958 S2", ValidFrom: now.Add(time.Hour),
			},
			expected: false,
		},
		{
			name: "cross-agent same ticket but later outcome reverses prior → not suppressed (let LLM judge)",
			d: model.Decision{
				ID: idA, AgentID: "claude-code", AgentContext: taskCtx("ARD-958"),
				Outcome: "Reviewed ARD-958 S1 — approved", ValidFrom: now,
			},
			cand: model.Decision{
				ID: idB, AgentID: "reviewer", AgentContext: taskCtx("ARD-958"),
				Outcome: "Walked back ARD-958 S1 approval; design B preferred", ValidFrom: now.Add(time.Hour),
			},
			expected: false,
		},
		{
			name: "cross-agent same ticket but precedent-linked → not suppressed",
			d: model.Decision{
				ID: idA, AgentID: "claude-code", AgentContext: taskCtx("ARD-958"),
				Outcome: "ARD-958 S2 implemented", ValidFrom: now, PrecedentRef: &idB,
			},
			cand: model.Decision{
				ID: idB, AgentID: "reviewer", AgentContext: taskCtx("ARD-958"),
				Outcome: "Reviewed ARD-958 S1", ValidFrom: now.Add(-time.Hour),
			},
			expected: false,
		},
		{
			name: "cross-agent missing ticket → not suppressed",
			d: model.Decision{
				ID: idA, AgentID: "claude-code",
				Outcome: "Reviewed S1 PR", ValidFrom: now,
			},
			cand: model.Decision{
				ID: idB, AgentID: "reviewer",
				Outcome: "Reviewed S2 PR", ValidFrom: now.Add(time.Hour),
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, isPRSeriesLayerRefinement(tt.d, tt.cand))
		})
	}
}

func TestIsDisjointTicketReviewPair(t *testing.T) {
	now := time.Date(2026, 5, 8, 23, 0, 0, 0, time.UTC)
	idA := uuid.New()
	idB := uuid.New()

	taskCtx := func(task string) map[string]any {
		return map[string]any{"client": map[string]any{"task": task}}
	}

	tests := []struct {
		name     string
		d        model.Decision
		cand     model.Decision
		expected bool
	}{
		{
			name: "cross-agent code_review on different tickets → suppressed (10/71 FP signature)",
			d: model.Decision{
				ID: idA, AgentID: "claude-code", DecisionType: "code_review",
				AgentContext: taskCtx("ARD-950"),
				Outcome:      "Reviewed ARD-950 (statement_timeout killing slot creation)", ValidFrom: now,
			},
			cand: model.Decision{
				ID: idB, AgentID: "reviewer", DecisionType: "code_review",
				AgentContext: taskCtx("ARD-958"),
				Outcome:      "Reviewed ARD-958 S1 PR (consistent_point LSN)", ValidFrom: now.Add(time.Hour),
			},
			expected: true,
		},
		{
			name: "cross-agent assessment vs investigation, disjoint tickets → suppressed",
			d: model.Decision{
				ID: idA, AgentID: "claude-code", DecisionType: "investigation",
				AgentContext: taskCtx("ARD-870"),
				Outcome:      "ARD-870 homework verified", ValidFrom: now,
			},
			cand: model.Decision{
				ID: idB, AgentID: "reviewer", DecisionType: "assessment",
				AgentContext: taskCtx("ARD-964"),
				Outcome:      "Reviewed ARD-964 (SP BYOC pg_dump fast-path)", ValidFrom: now.Add(time.Hour),
			},
			expected: true,
		},
		{
			name: "shared ticket → not suppressed (other filters handle same-ticket)",
			d: model.Decision{
				ID: idA, AgentID: "claude-code", DecisionType: "code_review",
				AgentContext: taskCtx("ARD-958"),
				Outcome:      "Reviewed ARD-958 design", ValidFrom: now,
			},
			cand: model.Decision{
				ID: idB, AgentID: "reviewer", DecisionType: "code_review",
				AgentContext: taskCtx("ARD-958"),
				Outcome:      "Reviewed ARD-958 S2", ValidFrom: now.Add(time.Hour),
			},
			expected: false,
		},
		{
			name: "same agent, disjoint tickets → not suppressed (deliberate cross-ticket analysis preserved)",
			d: model.Decision{
				ID: idA, AgentID: "claude-code", DecisionType: "code_review",
				AgentContext: taskCtx("ARD-957"),
				Outcome:      "Reviewed ARD-957 design", ValidFrom: now,
			},
			cand: model.Decision{
				ID: idB, AgentID: "claude-code", DecisionType: "code_review",
				AgentContext: taskCtx("ARD-958"),
				Outcome:      "Reviewed ARD-958 design", ValidFrom: now.Add(time.Hour),
			},
			expected: false,
		},
		{
			name: "cross-agent but non-review types → not suppressed (architecture can span tickets)",
			d: model.Decision{
				ID: idA, AgentID: "claude-code", DecisionType: "architecture",
				AgentContext: taskCtx("ARD-957"),
				Outcome:      "Adopted shadow Neon mirror for ARD-957", ValidFrom: now,
			},
			cand: model.Decision{
				ID: idB, AgentID: "design-architect", DecisionType: "architecture",
				AgentContext: taskCtx("ARD-958"),
				Outcome:      "Migration flow design for ARD-958", ValidFrom: now.Add(time.Hour),
			},
			expected: false,
		},
		{
			name: "cross-agent precedent-linked → not suppressed",
			d: model.Decision{
				ID: idA, AgentID: "claude-code", DecisionType: "code_review",
				AgentContext: taskCtx("ARD-957"),
				Outcome:      "Reviewed ARD-957", ValidFrom: now, PrecedentRef: &idB,
			},
			cand: model.Decision{
				ID: idB, AgentID: "reviewer", DecisionType: "code_review",
				AgentContext: taskCtx("ARD-958"),
				Outcome:      "Reviewed ARD-958", ValidFrom: now.Add(-time.Hour),
			},
			expected: false,
		},
		{
			name: "cross-agent review, one side has no ticket → not suppressed (no join key)",
			d: model.Decision{
				ID: idA, AgentID: "claude-code", DecisionType: "code_review",
				AgentContext: taskCtx("ARD-957"),
				Outcome:      "Reviewed ARD-957", ValidFrom: now,
			},
			cand: model.Decision{
				ID: idB, AgentID: "reviewer", DecisionType: "code_review",
				Outcome: "Reviewed the storage layer refactor", ValidFrom: now.Add(time.Hour),
			},
			expected: false,
		},
		{
			name: "case-insensitive review type match",
			d: model.Decision{
				ID: idA, AgentID: "claude-code", DecisionType: "CODE_REVIEW",
				AgentContext: taskCtx("ARD-950"), Outcome: "Reviewed ARD-950", ValidFrom: now,
			},
			cand: model.Decision{
				ID: idB, AgentID: "reviewer", DecisionType: "Assessment",
				AgentContext: taskCtx("ARD-958"), Outcome: "Assessed ARD-958", ValidFrom: now.Add(time.Hour),
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, isDisjointTicketReviewPair(tt.d, tt.cand))
		})
	}
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
