package conflicts

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/ashita-ai/akashi/internal/model"
)

func TestIsMechanicalOperation(t *testing.T) {
	tests := []struct {
		name     string
		outcome  string
		expected bool
	}{
		{
			name:     "migration renumbering",
			outcome:  "Merged origin/main into evanvolgas/fix-akashi-projects, renumbering migration 097→098",
			expected: true,
		},
		{
			name:     "rebase with renumbering",
			outcome:  "Resolved 9 merge conflicts from main integration, renumbered migration 077→078",
			expected: true,
		},
		{
			name:     "merge conflicts resolution",
			outcome:  "Resolved merge conflicts after rebasing onto main",
			expected: true,
		},
		{
			name:     "version bump",
			outcome:  "Bumped version to v2.3.1 in package.json",
			expected: true,
		},
		{
			name:     "architecture decision — not mechanical",
			outcome:  "Chose Redis with 5min TTL for session cache to handle expected QPS",
			expected: false,
		},
		{
			name:     "code review — not mechanical",
			outcome:  "Found SQL injection vulnerability in user search endpoint",
			expected: false,
		},
		{
			name:     "empty outcome",
			outcome:  "",
			expected: false,
		},
		{
			name:     "case insensitive match",
			outcome:  "RENUMBERING migration files after REBASE",
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, isMechanicalOperation(tt.outcome))
		})
	}
}

func TestIsCrossBranchMechanical(t *testing.T) {
	tests := []struct {
		name     string
		d        model.Decision
		cand     model.Decision
		expected bool
	}{
		{
			name: "different branches, both mechanical → suppressed",
			d: model.Decision{
				AgentContext: map[string]any{
					"client": map[string]any{"git_branch": "evanvolgas/fix-akashi-projects"},
				},
				Outcome: "Merged origin/main, renumbering migration 097→098",
			},
			cand: model.Decision{
				AgentContext: map[string]any{
					"client": map[string]any{"git_branch": "evanvolgas/fix-sse-heartbeat"},
				},
				Outcome: "Merged main into branch, renumbered migration 083→084",
			},
			expected: true,
		},
		{
			name: "different branches, only one mechanical → not suppressed",
			d: model.Decision{
				AgentContext: map[string]any{
					"client": map[string]any{"git_branch": "evanvolgas/fix-akashi-projects"},
				},
				Outcome: "Merged origin/main, renumbering migration 097→098",
			},
			cand: model.Decision{
				AgentContext: map[string]any{
					"client": map[string]any{"git_branch": "evanvolgas/add-caching"},
				},
				Outcome: "Chose Redis with 5min TTL for session cache",
			},
			expected: false,
		},
		{
			name: "same branch, both mechanical → not suppressed (same-branch has different semantics)",
			d: model.Decision{
				AgentContext: map[string]any{
					"client": map[string]any{"git_branch": "evanvolgas/fix-akashi-projects"},
				},
				Outcome: "Renumbered migration 097→098",
			},
			cand: model.Decision{
				AgentContext: map[string]any{
					"client": map[string]any{"git_branch": "evanvolgas/fix-akashi-projects"},
				},
				Outcome: "Renumbered migration 098→099",
			},
			expected: false,
		},
		{
			name: "missing branch on one decision → not suppressed",
			d: model.Decision{
				AgentContext: map[string]any{
					"client": map[string]any{"git_branch": "evanvolgas/fix-akashi-projects"},
				},
				Outcome: "Renumbered migration 097→098",
			},
			cand: model.Decision{
				AgentContext: map[string]any{},
				Outcome:      "Renumbered migration 083→084",
			},
			expected: false,
		},
		{
			name: "both branches missing → not suppressed",
			d: model.Decision{
				AgentContext: map[string]any{},
				Outcome:      "Renumbered migration 097→098",
			},
			cand: model.Decision{
				AgentContext: map[string]any{},
				Outcome:      "Renumbered migration 083→084",
			},
			expected: false,
		},
		{
			name: "server namespace branch → works with nestedContextString",
			d: model.Decision{
				AgentContext: map[string]any{
					"server": map[string]any{"git_branch": "feature/a"},
				},
				Outcome: "Merged main, rebasing onto main",
			},
			cand: model.Decision{
				AgentContext: map[string]any{
					"server": map[string]any{"git_branch": "feature/b"},
				},
				Outcome: "Rebased onto main, renumbering migration 050→051",
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, isCrossBranchMechanical(tt.d, tt.cand))
		})
	}
}

func TestIsSameBranchSelfCorrection(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name     string
		d        model.Decision
		cand     model.Decision
		expected bool
	}{
		{
			name: "same agent, same branch → self-correction",
			d: model.Decision{
				AgentID: "admin",
				AgentContext: map[string]any{
					"client": map[string]any{"git_branch": "feature/rate-limiting"},
				},
				ValidFrom: now,
			},
			cand: model.Decision{
				AgentID: "admin",
				AgentContext: map[string]any{
					"client": map[string]any{"git_branch": "feature/rate-limiting"},
				},
				ValidFrom: now.Add(time.Hour),
			},
			expected: true,
		},
		{
			name: "same agent, different branches → not self-correction",
			d: model.Decision{
				AgentID: "admin",
				AgentContext: map[string]any{
					"client": map[string]any{"git_branch": "feature/rate-limiting"},
				},
				ValidFrom: now,
			},
			cand: model.Decision{
				AgentID: "admin",
				AgentContext: map[string]any{
					"client": map[string]any{"git_branch": "feature/caching"},
				},
				ValidFrom: now.Add(time.Hour),
			},
			expected: false,
		},
		{
			name: "different agents, same branch → not self-correction",
			d: model.Decision{
				AgentID: "admin",
				AgentContext: map[string]any{
					"client": map[string]any{"git_branch": "feature/rate-limiting"},
				},
				ValidFrom: now,
			},
			cand: model.Decision{
				AgentID: "reviewer",
				AgentContext: map[string]any{
					"client": map[string]any{"git_branch": "feature/rate-limiting"},
				},
				ValidFrom: now.Add(time.Hour),
			},
			expected: false,
		},
		{
			name: "same agent, no branch → not self-correction",
			d: model.Decision{
				AgentID:      "admin",
				AgentContext: map[string]any{},
				ValidFrom:    now,
			},
			cand: model.Decision{
				AgentID:      "admin",
				AgentContext: map[string]any{},
				ValidFrom:    now.Add(time.Hour),
			},
			expected: false,
		},
		{
			name: "same agent, one branch missing → not self-correction",
			d: model.Decision{
				AgentID: "admin",
				AgentContext: map[string]any{
					"client": map[string]any{"git_branch": "feature/rate-limiting"},
				},
				ValidFrom: now,
			},
			cand: model.Decision{
				AgentID:      "admin",
				AgentContext: map[string]any{},
				ValidFrom:    now.Add(time.Hour),
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, isSameBranchSelfCorrection(tt.d, tt.cand))
		})
	}
}

func TestFormatPrompt_DifferentBranches(t *testing.T) {
	now := time.Now()
	prompt := formatPrompt(ValidateInput{
		OutcomeA: "renumbered migration 097→098",
		OutcomeB: "renumbered migration 083→084",
		TypeA:    "bug_fix",
		TypeB:    "bug_fix",
		AgentA:   "claude-code",
		AgentB:   "senior-engineer",
		CreatedA: now,
		CreatedB: now.Add(time.Hour),
		BranchA:  "evanvolgas/fix-akashi-projects",
		BranchB:  "evanvolgas/fix-sse-heartbeat",
	})
	assert.Contains(t, prompt, "DIFFERENT BRANCHES")
	assert.Contains(t, prompt, "evanvolgas/fix-akashi-projects")
	assert.Contains(t, prompt, "evanvolgas/fix-sse-heartbeat")
	assert.Contains(t, prompt, "parallel work")
}

func TestFormatPrompt_SameBranch(t *testing.T) {
	now := time.Now()
	prompt := formatPrompt(ValidateInput{
		OutcomeA: "chose Redis",
		OutcomeB: "switched to Memcached",
		TypeA:    "architecture",
		TypeB:    "architecture",
		AgentA:   "claude-code",
		AgentB:   "claude-code",
		CreatedA: now,
		CreatedB: now.Add(time.Hour),
		BranchA:  "feature/caching",
		BranchB:  "feature/caching",
	})
	assert.Contains(t, prompt, "Same branch: feature/caching")
	assert.NotContains(t, prompt, "DIFFERENT BRANCHES")
}

func TestFormatPrompt_NoBranches(t *testing.T) {
	now := time.Now()
	prompt := formatPrompt(ValidateInput{
		OutcomeA: "chose Redis",
		OutcomeB: "chose Memcached",
		TypeA:    "architecture",
		TypeB:    "architecture",
		AgentA:   "claude-code",
		AgentB:   "reviewer",
		CreatedA: now,
		CreatedB: now.Add(time.Hour),
	})
	assert.True(t, !strings.Contains(prompt, "DIFFERENT BRANCHES") && !strings.Contains(prompt, "Same branch"),
		"no branch context should appear when both branches are empty")
}

func TestFormatPrompt_OneBranchOnly(t *testing.T) {
	now := time.Now()
	prompt := formatPrompt(ValidateInput{
		OutcomeA: "chose Redis",
		OutcomeB: "chose Memcached",
		TypeA:    "architecture",
		TypeB:    "architecture",
		AgentA:   "claude-code",
		AgentB:   "reviewer",
		CreatedA: now,
		CreatedB: now.Add(time.Hour),
		BranchA:  "feature/caching",
	})
	assert.Contains(t, prompt, "one decision was on branch")
	assert.Contains(t, prompt, "feature/caching")
}
